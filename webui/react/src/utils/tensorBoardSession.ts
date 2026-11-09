import { Loadable } from 'hew/utils/loadable';
import { isEqual } from 'lodash';

import { terminalCommandStates } from 'constants/states';
import { getCurrentUser, getShells, getTaskServices, launchShell } from 'services/api';
import { V1GetTaskServicesResponse, V1TaskServiceState } from 'services/api-ts-sdk';
import userStore from 'stores/users';
import { CommandResponse, CommandTask, InteractiveTaskTab, RawJson } from 'types';
import handleError, { ErrorLevel } from 'utils/error';
import { openInteractiveTask } from 'utils/wait';

/** The idle timeout of a session started from the pages: the one `det tensorboard start` sends. */
export const TENSORBOARD_SESSION_IDLE_TIMEOUT = '5m';

export interface TensorBoardSelection {
  experimentIds?: number[];
  trialIds?: number[];
  /** The workspace of the selection; without one there is no reuse (the omnibar). */
  workspaceId?: number;
}

/** Sorted numerically and deduplicated; the input is not changed. */
export const canonicalTensorBoardIds = (ids?: readonly number[]): number[] =>
  Array.from(new Set(ids ?? [])).sort((a, b) => a - b);

/** The LaunchShell config of a viewing session of the selection. */
export const tensorBoardSessionConfig = (s: TensorBoardSelection): RawJson => {
  const experimentIds = canonicalTensorBoardIds(s.experimentIds);
  const trialIds = canonicalTensorBoardIds(s.trialIds);
  const tensorboard: RawJson = {};
  if (experimentIds.length > 0) tensorboard.experiment_ids = experimentIds;
  if (trialIds.length > 0) tensorboard.trial_ids = trialIds;
  return {
    idle_timeout: TENSORBOARD_SESSION_IDLE_TIMEOUT,
    resources: { slots: 0 },
    services: { ssh: { enabled: false }, tensorboard },
  };
};

/** Whether a task's sources equal the canonical lists (both lists; a task without misc has none). */
export const sameTensorBoardSources = (
  task: CommandTask,
  exps: number[],
  trials: number[],
): boolean =>
  isEqual(canonicalTensorBoardIds(task.misc?.experimentIds), exps) &&
  isEqual(canonicalTensorBoardIds(task.misc?.trialIds), trials);

/**
 * Whether a task is a viewing session whose TensorBoard can be opened: TensorBoard is its primary
 * service (not one attached to a development shell), it has not exited and can run (starting or
 * not reported yet counts, so that a session still queued or starting is found again), and the
 * viewer may connect to it.
 */
export const isOpenableTensorBoardSession = (r: V1GetTaskServicesResponse): boolean =>
  r.primary === 'tensorboard' &&
  r.services.some(
    (service) =>
      service.name === 'tensorboard' &&
      service.mayConnect &&
      service.state !== V1TaskServiceState.EXITED &&
      service.state !== V1TaskServiceState.UNAVAILABLE,
  );

/**
 * The newest of the viewer's own unended shells in the selection's workspace with the same sources
 * that is a viewing session whose TensorBoard can be opened, else a new viewing session. Only the
 * shells with the same sources have their services read, newest first, until one qualifies; one
 * whose services cannot be read is skipped.
 */
export const openOrCreateTensorBoard = async (
  s: TensorBoardSelection,
): Promise<CommandResponse> => {
  const experimentIds = canonicalTensorBoardIds(s.experimentIds);
  const trialIds = canonicalTensorBoardIds(s.trialIds);
  const { workspaceId } = s;
  // An empty selection would match any shell without sources; the master refuses its create.
  if (workspaceId !== undefined && (experimentIds.length > 0 || trialIds.length > 0)) {
    const me =
      Loadable.getOrElse(undefined, userStore.currentUser.get()) ?? (await getCurrentUser({}));
    const shells = await getShells({
      orderBy: 'ORDER_BY_DESC',
      sortBy: 'SORT_BY_START_TIME',
      users: [String(me.id)],
      workspaceId,
    });
    const candidates = shells.filter(
      (shell) =>
        shell.userId === me.id &&
        shell.workspaceId === workspaceId &&
        !terminalCommandStates.has(shell.state) &&
        sameTensorBoardSources(shell, experimentIds, trialIds),
    );
    for (const candidate of candidates) {
      try {
        // One at a time: the first that qualifies ends the reads.
        const services = await getTaskServices({ taskId: candidate.id });
        if (isOpenableTensorBoardSession(services)) return { command: candidate, warnings: [] };
      } catch (e) {
        handleError(e, { level: ErrorLevel.Warn, silent: true });
      }
    }
  }
  return launchShell({
    config: tensorBoardSessionConfig({ experimentIds, trialIds }),
    workspaceId,
  });
};

/** openOrCreateTensorBoard, then the task page's TensorBoard tab. */
export const openTensorBoardSession = async (s: TensorBoardSelection): Promise<CommandResponse> => {
  const response = await openOrCreateTensorBoard(s);
  openInteractiveTask(response.command.id, { tab: InteractiveTaskTab.TensorBoard });
  return response;
};

/** Shows a refused or failed open with the master's text under the given subject. */
export const handleTensorBoardError = (e: unknown, publicSubject: string): void => {
  handleError(e, { isUserTriggered: true, level: ErrorLevel.Error, publicSubject, silent: false });
};
