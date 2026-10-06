import useConfirm from 'hew/useConfirm';
import { createContext, useCallback, useContext, useMemo, useState } from 'react';

import { killGenericTask, pauseGenericTask, unpauseGenericTask } from 'services/api';
import { GenericTaskState } from 'types';
import handleError, { ErrorLevel, ErrorType } from 'utils/error';
import { canKillGenericTask, canPauseGenericTask, canUnpauseGenericTask } from 'utils/task';

export interface GenericTaskActionTarget {
  name: string;
  noPause?: boolean;
  parentId?: string;
  state?: GenericTaskState;
  taskId: string;
}

/**
 * Which tasks have an action running and whose last unpause failed, by task ID. The generic task
 * list keeps one for all its rows (GenericTaskActionStateContext), so that the two menus of a row,
 * in the actions column and on a right click, show the same retry and the same running action.
 */
export interface GenericTaskActionState {
  busyTaskIds: ReadonlySet<string>;
  failedUnpauseTaskIds: ReadonlySet<string>;
  setBusy: (taskId: string, busy: boolean) => void;
  setUnpauseFailed: (taskId: string, failed: boolean) => void;
}

const withMember = (
  set: ReadonlySet<string>,
  member: string,
  isMember: boolean,
): ReadonlySet<string> => {
  if (set.has(member) === isMember) return set;
  const next = new Set(set);
  if (isMember) next.add(member);
  else next.delete(member);
  return next;
};

export const useGenericTaskActionState = (): GenericTaskActionState => {
  const [busyTaskIds, setBusyTaskIds] = useState<ReadonlySet<string>>(() => new Set());
  const [failedUnpauseTaskIds, setFailedUnpauseTaskIds] = useState<ReadonlySet<string>>(
    () => new Set(),
  );
  const setBusy = useCallback(
    (taskId: string, busy: boolean) => setBusyTaskIds((prev) => withMember(prev, taskId, busy)),
    [],
  );
  const setUnpauseFailed = useCallback(
    (taskId: string, failed: boolean) =>
      setFailedUnpauseTaskIds((prev) => withMember(prev, taskId, failed)),
    [],
  );
  return useMemo(
    () => ({ busyTaskIds, failedUnpauseTaskIds, setBusy, setUnpauseFailed }),
    [busyTaskIds, failedUnpauseTaskIds, setBusy, setUnpauseFailed],
  );
};

/**
 * The action state shared by the menus under it. Without a provider, each user of
 * useGenericTaskActions keeps its own, as the task's page does.
 */
export const GenericTaskActionStateContext = createContext<GenericTaskActionState | undefined>(
  undefined,
);

interface Options {
  /** Whether the user may control the task: its owner or an admin. */
  canControl: boolean;
  /** Called after each action, whether it succeeded or not, to refresh the task. */
  onComplete?: () => void;
  task: GenericTaskActionTarget;
}

export interface GenericTaskActions {
  canKill: boolean;
  canPause: boolean;
  canUnpause: boolean;
  /** An action on this task is running. */
  isBusy: boolean;
  /** The last unpause of this task failed, so unpause is offered as a retry. */
  isUnpauseRetry: boolean;
  /* Each action asks for confirmation first and runs only once it is confirmed. */
  kill: () => void;
  killTree: () => void;
  pause: () => void;
  unpause: () => void;
}

/*
 * Shows the master's reason (for example the HTTP 400 or 409 message) instead of the generic
 * message that the confirmation modal would set.
 */
const handleActionError = (subject: string) => (e: unknown) =>
  handleError(e, {
    level: ErrorLevel.Error,
    publicSubject: subject,
    silent: false,
    type: ErrorType.Server,
  });

/**
 * Pause, unpause and kill of a generic task, with their confirmations, for the task's page and
 * the generic task menu, so that both ask the same and do the same. Whether each action is offered
 * follows canPauseGenericTask, canUnpauseGenericTask and canKillGenericTask. Under a
 * GenericTaskActionStateContext provider, the running actions and failed unpauses are the
 * provider's; otherwise this hook keeps its own.
 */
const useGenericTaskActions = ({ canControl, onComplete, task }: Options): GenericTaskActions => {
  const confirm = useConfirm();
  const localState = useGenericTaskActionState();
  const { busyTaskIds, failedUnpauseTaskIds, setBusy, setUnpauseFailed } =
    useContext(GenericTaskActionStateContext) ?? localState;
  const isBusy = busyTaskIds.has(task.taskId);
  /*
   * The last unpause of this task from here failed. Its retry stays offered, also once the task
   * reads active, until an unpause or kill of it succeeds or the page is left.
   */
  const isUnpauseRetry = failedUnpauseTaskIds.has(task.taskId);

  const run = useCallback(
    async (taskId: string, action: () => Promise<void>) => {
      setBusy(taskId, true);
      try {
        await action();
      } finally {
        setBusy(taskId, false);
        onComplete?.();
      }
    },
    [onComplete, setBusy],
  );

  const pause = useCallback(() => {
    confirm({
      content:
        'Pause this task and its pausable descendants? Their containers are stopped; ' +
        'unpausing runs their entrypoints again from the start.',
      okText: 'Pause',
      onConfirm: () => run(task.taskId, () => pauseGenericTask({ taskId: task.taskId })),
      onError: handleActionError('Unable to pause task'),
      title: `Pause ${task.name}`,
    });
  }, [confirm, run, task.name, task.taskId]);

  const unpause = useCallback(() => {
    const taskId = task.taskId;
    confirm({
      content: isUnpauseRetry
        ? 'Retry the unpause of this task? The master resumes the members of its tree that ' +
          'the failed unpause did not start, or refuses if there is nothing left to resume.'
        : 'Unpause this task and its paused descendants? Their entrypoints run again from the ' +
          'start in new containers.',
      okText: isUnpauseRetry ? 'Retry Unpause' : 'Unpause',
      onConfirm: () =>
        run(taskId, async () => {
          try {
            await unpauseGenericTask({ taskId });
          } catch (e) {
            setUnpauseFailed(taskId, true);
            throw e;
          }
          setUnpauseFailed(taskId, false);
        }),
      onError: handleActionError('Unable to unpause task'),
      title: `${isUnpauseRetry ? 'Retry unpause of' : 'Unpause'} ${task.name}`,
    });
  }, [confirm, isUnpauseRetry, run, setUnpauseFailed, task.name, task.taskId]);

  const killTask = useCallback(
    async (killFromRoot: boolean) => {
      await killGenericTask({ killFromRoot, taskId: task.taskId });
      // A kill ends any unpause of the tree that is still pending.
      setUnpauseFailed(task.taskId, false);
    },
    [setUnpauseFailed, task.taskId],
  );

  const kill = useCallback(() => {
    confirm({
      content: 'Kill this task and all its descendants?',
      danger: true,
      okText: 'Kill',
      onConfirm: () => run(task.taskId, () => killTask(false)),
      onError: handleActionError('Unable to kill task'),
      title: `Kill ${task.name}`,
    });
  }, [confirm, killTask, run, task.name, task.taskId]);

  const killTree = useCallback(() => {
    confirm({
      content:
        'Kill the whole tree of this task, from its root task down, including tasks that ' +
        'are not descendants of this one?',
      danger: true,
      okText: 'Kill Tree',
      onConfirm: () => run(task.taskId, () => killTask(true)),
      onError: handleActionError('Unable to kill task tree'),
      title: `Kill the tree of ${task.name}`,
    });
  }, [confirm, killTask, run, task.name, task.taskId]);

  return {
    canKill: canKillGenericTask(task, canControl),
    canPause: canPauseGenericTask(task, canControl),
    canUnpause: canUnpauseGenericTask(task, canControl, isUnpauseRetry),
    isBusy,
    isUnpauseRetry,
    kill,
    killTree,
    pause,
    unpause,
  };
};

export default useGenericTaskActions;
