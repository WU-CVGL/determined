import { GridCell } from '@glideapps/glide-data-grid';
import Button from 'hew/Button';
import { ContextMenuCompleteHandlerProps } from 'hew/DataGrid/contextMenu';
import Dropdown, { DropdownEvent, MenuItem } from 'hew/Dropdown';
import Icon from 'hew/Icon';
import { useModal } from 'hew/Modal';
import { useToast } from 'hew/Toast';
import useConfirm from 'hew/useConfirm';
import { copyToClipboard } from 'hew/utils/functions';
import { Failed, Loadable, Loaded, NotLoaded } from 'hew/utils/loadable';
import { isString } from 'lodash';
import React, { useCallback, useMemo, useRef, useState } from 'react';

import css from 'components/ActionDropdown/ActionDropdown.module.scss';
import ExperimentEditModalComponent from 'components/ExperimentEditModal';
import ExperimentMoveModalComponent from 'components/ExperimentMoveModal';
import ExperimentRetainLogsModalComponent from 'components/ExperimentRetainLogsModal';
import HyperparameterSearchModalComponent from 'components/HyperparameterSearchModal';
import InterstitialModalComponent, {
  type onInterstitialCloseActionType,
} from 'components/InterstitialModalComponent';
import useFeature from 'hooks/useFeature';
import usePermissions from 'hooks/usePermissions';
import useTaskResourcesEnabled from 'hooks/useTaskResourcesEnabled';
import { handlePath, paths } from 'routes/utils';
import {
  activateExperiment,
  archiveExperiment,
  cancelExperiment,
  deleteExperiment,
  getExperiment,
  killExperiment,
  openOrCreateTensorBoard,
  pauseExperiment,
  unarchiveExperiment,
} from 'services/api';
import {
  BulkExperimentItem,
  ExperimentAction,
  ExperimentSearcherName,
  FullExperimentItem,
  ProjectExperiment,
  ValueOf,
} from 'types';
import handleError, { ErrorLevel, ErrorType } from 'utils/error';
import { getActionsForExperiment, isSingleTrialExperiment } from 'utils/experiment';
import { capitalize } from 'utils/string';
import { openCommandResponse } from 'utils/wait';

interface Props {
  children?: React.ReactNode;
  cell?: GridCell;
  experiment: ProjectExperiment;
  isContextMenu?: boolean;
  link?: string;
  makeOpen?: boolean;
  onComplete?: ContextMenuCompleteHandlerProps<ExperimentAction, BulkExperimentItem>;
  onLink?: () => void;
  onVisibleChange?: (visible: boolean) => void;
  workspaceId?: number;
}

export const Action = {
  Copy: 'Copy Value',
  CopyExperimentID: 'Copy Experiment ID',
  NewTab: 'Open Link in New Tab',
  NewWindow: 'Open Link in New Window',
  ...ExperimentAction,
};

type Action = ValueOf<typeof Action>;

// One order wherever the menu appears, after the link and Copy Value items: viewing first, then
// Copy Experiment ID, then changing the experiment, with Stop, Kill and Delete last. Items that do
// not apply are left out.
const viewActions = [Action.ViewLogs, Action.ViewResources, Action.OpenTensorBoard];
const manageActions = [
  Action.Activate,
  Action.Pause,
  Action.Edit,
  Action.Move,
  Action.HyperparameterSearch,
  Action.RetainLogs,
  Action.Archive,
  Action.Unarchive,
  Action.Cancel,
  Action.Kill,
  Action.Delete,
];
const dangerActions: Action[] = [Action.Kill, Action.Delete];

/**
 * Where View Logs leads. Experiment list rows have numTrials and the config, but no trial IDs (the
 * list routes leave them out for speed); only an experiment fetched on its own has trialIds.
 * - One trial with a known ID: that trial's logs page.
 * - A single-trial experiment (by its config, as the experiment page decides; by the searcher type
 *   if the row has no config): the Logs tab of its page, which shows that trial's logs.
 * - Otherwise: the Trials tab of the experiment page, to choose a trial.
 * An experiment without trials has no logs yet, so View Logs is left out (see experimentCheckers).
 */
const experimentLogsPath = (experiment: ProjectExperiment): string => {
  const trialId = experiment.numTrials === 1 ? experiment.trialIds?.[0] : undefined;
  if (trialId !== undefined) return paths.trialLogs(trialId, experiment.id);
  if (
    isSingleTrialExperiment(experiment) ||
    experiment.searcherType === ExperimentSearcherName.Single
  ) {
    return `${paths.experimentDetails(experiment.id)}/logs`;
  }
  return `${paths.experimentDetails(experiment.id)}/trials`;
};

const ExperimentActionDropdown: React.FC<Props> = ({
  experiment,
  cell,
  isContextMenu,
  link,
  makeOpen,
  onComplete,
  onLink,
  onVisibleChange,
  children,
}: Props) => {
  const ExperimentEditModal = useModal(ExperimentEditModalComponent);
  const ExperimentMoveModal = useModal(ExperimentMoveModalComponent);
  const ExperimentRetainLogsModal = useModal(ExperimentRetainLogsModalComponent);
  const {
    Component: HyperparameterSearchModal,
    open: hyperparameterSearchModalOpen,
    close: hyperparameterSearchModalClose,
  } = useModal(HyperparameterSearchModalComponent);
  const {
    Component: InterstitialModal,
    open: interstitialModalOpen,
    close: interstitialModalClose,
  } = useModal(InterstitialModalComponent);
  const [experimentItem, setExperimentItem] = useState<Loadable<FullExperimentItem>>(NotLoaded);
  const canceler = useRef<AbortController>(new AbortController());
  const confirm = useConfirm();
  const { openToast } = useToast();
  const f_flat_runs = useFeature().isOn('flat_runs');
  const taskResourcesEnabled = useTaskResourcesEnabled();

  const entityName = f_flat_runs ? 'search' : 'experiment';
  const trialsName = f_flat_runs ? 'runs' : 'trials';

  // this is required when experiment does not contain `config`.
  // since we removed config. See #8765 on GitHub
  const fetchedExperimentItem = useCallback(async () => {
    try {
      setExperimentItem(NotLoaded);
      const response: FullExperimentItem = await getExperiment(
        { id: experiment.id },
        { signal: canceler.current.signal },
      );
      setExperimentItem(Loaded(response));
    } catch (e) {
      handleError(e, { publicSubject: `Unable to fetch ${entityName} data.` });
      setExperimentItem(Failed(new Error('experiment data failure')));
    }
  }, [entityName, experiment.id]);

  const onInterstitalClose: onInterstitialCloseActionType = useCallback(
    (reason) => {
      switch (reason) {
        case 'ok':
          hyperparameterSearchModalOpen();
          break;
        case 'failed':
          break;
        case 'close':
          canceler.current.abort();
          canceler.current = new AbortController();
          break;
      }
      interstitialModalClose(reason);
    },
    [hyperparameterSearchModalOpen, interstitialModalClose],
  );

  const handleEditComplete = useCallback(
    (data: Partial<BulkExperimentItem>) => {
      onComplete?.(ExperimentAction.Edit, experiment.id, data);
    },
    [experiment.id, onComplete],
  );

  const handleMoveComplete = useCallback(() => {
    onComplete?.(ExperimentAction.Move, experiment.id);
  }, [experiment.id, onComplete]);

  const handleRetainLogsComplete = useCallback(() => {
    onComplete?.(ExperimentAction.RetainLogs, experiment.id);
  }, [experiment.id, onComplete]);

  const permissions = usePermissions();
  const menuItems: MenuItem[] = useMemo(() => {
    const allowedItems = (actions: ExperimentAction[]): MenuItem[] =>
      getActionsForExperiment(experiment, actions, permissions)
        .filter((action) => action !== Action.ViewResources || taskResourcesEnabled === true)
        .map((action) => ({ danger: dangerActions.includes(action), key: action, label: action }));
    return [
      ...allowedItems(viewActions),
      { key: Action.CopyExperimentID, label: Action.CopyExperimentID },
      ...allowedItems(manageActions),
    ];
  }, [experiment, permissions, taskResourcesEnabled]);
  const logsPath = experimentLogsPath(experiment);

  const cellCopyData = useMemo(() => {
    if (cell && 'displayData' in cell && isString(cell.displayData)) return cell.displayData;
    if (cell?.copyData && cell.copyData !== '-') return cell.copyData;
    return undefined;
  }, [cell]);

  const dropdownMenu = useMemo(() => {
    const items: MenuItem[] = [];
    if (link) {
      items.push(
        { key: Action.NewTab, label: Action.NewTab },
        { key: Action.NewWindow, label: Action.NewWindow },
        { type: 'divider' },
      );
    }
    if (cellCopyData) {
      items.push({ key: Action.Copy, label: Action.Copy });
    }
    items.push(...menuItems);
    return items;
  }, [link, menuItems, cellCopyData]);

  const handleDropdown = useCallback(
    async (action: string, e: DropdownEvent) => {
      try {
        switch (action) {
          case Action.NewTab:
            handlePath(e, { path: link, popout: 'tab' });
            await onLink?.();
            break;
          case Action.NewWindow:
            handlePath(e, { path: link, popout: 'window' });
            await onLink?.();
            break;
          case Action.Activate:
            await activateExperiment({ experimentId: experiment.id });
            await onComplete?.(action, experiment.id);
            break;
          case Action.Archive:
            await archiveExperiment({ experimentId: experiment.id });
            await onComplete?.(action, experiment.id);
            break;
          case Action.Cancel:
            // Not red: Stop ends the trials gracefully, unlike Kill.
            confirm({
              content: `Stop ${entityName} ${experiment.id}? Its ${trialsName} are asked to save a checkpoint and exit.`,
              okText: 'Stop',
              onConfirm: async () => {
                await cancelExperiment({ experimentId: experiment.id });
                await onComplete?.(action, experiment.id);
              },
              onError: handleError,
              title: `Confirm ${capitalize(entityName)} Stop`,
            });
            break;
          case Action.CopyExperimentID:
            await copyToClipboard(String(experiment.id));
            openToast({
              severity: 'Confirm',
              title: 'Experiment ID has been copied to clipboard.',
            });
            break;
          case Action.OpenTensorBoard: {
            const commandResponse = await openOrCreateTensorBoard({
              experimentIds: [experiment.id],
              workspaceId: experiment.workspaceId,
            });
            openCommandResponse(commandResponse);
            break;
          }
          case Action.ViewLogs:
            handlePath(e, { path: logsPath });
            break;
          case Action.ViewResources:
            handlePath(e, { path: paths.experimentResources(experiment.id) });
            break;
          case Action.SwitchPin: {
            // TODO: leaving old code behind for when we want to enable this for our current experiment list.
            // const newPinned = { ...(settings?.pinned ?? {}) };
            // const pinSet = new Set(newPinned[experiment.projectId]);
            // if (pinSet.has(id)) {
            //   pinSet.delete(id);
            // } else {
            //   if (pinSet.size >= 5) {
            //     notification.warning({
            //       description: 'Up to 5 pinned items',
            //       message: 'Unable to pin this item',
            //     });
            //     break;
            //   }
            //   pinSet.add(id);
            // }
            // newPinned[experiment.projectId] = Array.from(pinSet);
            // updateSettings?.({ pinned: newPinned });
            // await onComplete?.(action, id);
            break;
          }
          case Action.Kill:
            confirm({
              content: `Are you sure you want to kill ${entityName} ${experiment.id}?`,
              danger: true,
              okText: 'Kill',
              onConfirm: async () => {
                await killExperiment({ experimentId: experiment.id });
                await onComplete?.(action, experiment.id);
              },
              onError: handleError,
              title: `Confirm ${capitalize(entityName)} Kill`,
            });
            break;
          case Action.Pause:
            await pauseExperiment({ experimentId: experiment.id });
            await onComplete?.(action, experiment.id);
            break;
          case Action.Unarchive:
            await unarchiveExperiment({ experimentId: experiment.id });
            await onComplete?.(action, experiment.id);
            break;
          case Action.Delete:
            confirm({
              content: `Are you sure you want to delete ${entityName} ${experiment.id}?`,
              danger: true,
              okText: 'Delete',
              onConfirm: async () => {
                await deleteExperiment({ experimentId: experiment.id });
                await onComplete?.(action, experiment.id);
              },
              onError: handleError,
              title: `Confirm ${capitalize(entityName)} Deletion`,
            });
            break;
          case Action.Edit:
            ExperimentEditModal.open();
            break;
          case Action.Move:
            ExperimentMoveModal.open();
            break;
          case Action.RetainLogs:
            ExperimentRetainLogsModal.open();
            break;
          case Action.HyperparameterSearch:
            interstitialModalOpen();
            fetchedExperimentItem();
            break;
          case Action.Copy:
            await copyToClipboard(cellCopyData ?? '');
            openToast({
              severity: 'Confirm',
              title: 'Value has been copied to clipboard.',
            });
            break;
        }
      } catch (e) {
        handleError(e, {
          level: ErrorLevel.Error,
          publicMessage: `Unable to ${action} ${entityName} ${experiment.id}.`,
          publicSubject: `${capitalize(action)} failed.`,
          silent: false,
          type: ErrorType.Server,
        });
      } finally {
        onVisibleChange?.(false);
      }
    },
    [
      entityName,
      trialsName,
      link,
      onLink,
      logsPath,
      experiment.id,
      onComplete,
      confirm,
      ExperimentEditModal,
      ExperimentMoveModal,
      ExperimentRetainLogsModal,
      interstitialModalOpen,
      fetchedExperimentItem,
      cellCopyData,
      openToast,
      experiment.workspaceId,
      onVisibleChange,
    ],
  );

  const shared = (
    <>
      <ExperimentEditModal.Component
        description={experiment.description ?? ''}
        experimentId={experiment.id}
        experimentName={experiment.name}
        onEditComplete={handleEditComplete}
      />
      <ExperimentMoveModal.Component
        experimentIds={[experiment.id]}
        sourceProjectId={experiment.projectId}
        sourceWorkspaceId={experiment.workspaceId}
        onSubmit={handleMoveComplete}
      />
      <ExperimentRetainLogsModal.Component
        experimentIds={[experiment.id]}
        projectId={experiment.projectId}
        onSubmit={handleRetainLogsComplete}
      />
      {experimentItem.isLoaded && (
        <HyperparameterSearchModal
          closeModal={hyperparameterSearchModalClose}
          experiment={experimentItem.data}
        />
      )}
      <InterstitialModal loadableData={experimentItem} onCloseAction={onInterstitalClose} />
    </>
  );

  return children ? (
    <>
      <Dropdown
        isContextMenu={isContextMenu}
        menu={dropdownMenu}
        open={makeOpen}
        onClick={handleDropdown}>
        {children}
      </Dropdown>
      {shared}
    </>
  ) : (
    <div className={css.base} title="Open actions menu">
      <Dropdown menu={dropdownMenu} placement="bottomRight" onClick={handleDropdown}>
        <Button icon={<Icon name="overflow-vertical" size="small" title="Action menu" />} />
      </Dropdown>
      {shared}
    </div>
  );
};

export default ExperimentActionDropdown;
