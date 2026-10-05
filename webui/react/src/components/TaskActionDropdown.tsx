import Button from 'hew/Button';
import Dropdown, { MenuItem } from 'hew/Dropdown';
import Icon from 'hew/Icon';
import { useModal } from 'hew/Modal';
import { useToast } from 'hew/Toast';
import useConfirm from 'hew/useConfirm';
import { Loadable } from 'hew/utils/loadable';
import { useObservable } from 'micro-observables';
import React, { useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';

import css from 'components/ActionDropdown/ActionDropdown.module.scss';
import TaskConnectModalComponent, { TaskConnectField } from 'components/TaskConnectModal';
import useFeature from 'hooks/useFeature';
import usePermissions from 'hooks/usePermissions';
import useTaskResourcesEnabled from 'hooks/useTaskResourcesEnabled';
import { paths, serverAddress } from 'routes/utils';
import { killTask } from 'services/api';
import { openShellTerminalTab } from 'services/shellTerminal';
import userStore from 'stores/users';
import { TaskAction as Action, CommandState, CommandTask, CommandType, DetailedUser } from 'types';
import { copyToClipboard } from 'utils/dom';
import handleError, { ErrorLevel, ErrorType } from 'utils/error';
import { capitalize } from 'utils/string';
import { canOpenShellTerminal, isTaskKillable } from 'utils/task';
import { getJupyterLabAddress, NOTEBOOK_ACCESS_DENIED } from 'utils/wait';

interface Props {
  children?: React.ReactNode;
  curUser?: DetailedUser;
  onComplete?: (action?: Action) => void;
  /** Offers "Launch Again" on the user's own shells and JupyterLabs when given. */
  onLaunchAgain?: (task: CommandTask) => void;
  /** Offers "Manage Job" (the job queue's priority, weight and pool) when given. */
  onManageJob?: () => void;
  onVisibleChange?: (visible: boolean) => void;
  task: CommandTask;
}

const relaunchableTaskTypes: CommandType[] = [CommandType.JupyterLab, CommandType.Shell];

const TaskActionDropdown: React.FC<Props> = ({
  task,
  curUser,
  onComplete,
  onLaunchAgain,
  onManageJob,
  children,
}: Props) => {
  const { canCreateWorkspaceNSC, canModifyWorkspaceNSC } = usePermissions();
  const resourcesEnabled = useTaskResourcesEnabled();
  const terminalEnabled = useFeature().isOn('shell_terminal');
  const currentUser = Loadable.getOrElse(undefined, useObservable(userStore.currentUser));
  const { openToast } = useToast();
  const TaskConnectModal = useModal(TaskConnectModalComponent);
  // Listings never include a notebook's Jupyter token, so it is fetched when connecting.
  const [jupyterLabAddress, setJupyterLabAddress] = useState<string>();

  const isConnectable = (task: CommandTask): boolean => {
    const connectableTaskTypes: CommandType[] = [CommandType.JupyterLab, CommandType.Shell];
    return connectableTaskTypes.includes(task.type) && task.state === CommandState.Running;
  };

  const confirm = useConfirm();

  const taskConnectFields: TaskConnectField[] = useMemo(() => {
    switch (task.type) {
      case CommandType.JupyterLab:
        return [
          {
            label: 'Connect to notebook in VSCode using the remote Jupyter server address:',
            value: `${serverAddress()}${jupyterLabAddress ?? task.serviceAddress}`,
          },
        ];
      case CommandType.Shell:
        return [
          {
            label: 'Start an interactive SSH session in the terminal:',
            value: `det shell open ${task.id}`,
          },
        ];
      default:
        return [];
    }
  }, [task, jupyterLabAddress]);

  // One order everywhere the menu appears: viewing first, then connecting, then launching and
  // managing, with the destructive Kill last. Items that do not apply are left out.
  const menuItems: MenuItem[] = useMemo(() => {
    const items: MenuItem[] = [{ key: Action.ViewLogs, label: 'View Logs' }];
    if (resourcesEnabled) items.push({ key: Action.ViewResources, label: 'View Resources' });
    items.push({ key: Action.CopyTaskID, label: 'Copy Task ID' });
    if (isConnectable(task)) {
      items.push({
        key: Action.Connect,
        label: task.type === CommandType.Shell ? 'Connect via CLI' : 'Connect',
      });
    }
    if (terminalEnabled && canOpenShellTerminal(task, currentUser)) {
      items.push({ key: Action.OpenTerminal, label: 'Open Terminal' });
    }
    // A UI rule only: the API lets anyone who can view a task read its config.
    if (
      onLaunchAgain &&
      relaunchableTaskTypes.includes(task.type) &&
      !!curUser &&
      (curUser.id === task.userId || curUser.isAdmin) &&
      canCreateWorkspaceNSC({ workspace: { id: task.workspaceId } })
    ) {
      items.push({ key: Action.LaunchAgain, label: 'Launch Again' });
    }
    if (onManageJob) items.push({ key: Action.ManageJob, label: 'Manage Job' });
    if (
      isTaskKillable(
        task,
        canModifyWorkspaceNSC({ userId: task.userId, workspace: { id: task.workspaceId } }),
      )
    ) {
      items.push({ key: Action.Kill, label: 'Kill' });
    }
    return items;
  }, [
    task,
    canCreateWorkspaceNSC,
    canModifyWorkspaceNSC,
    curUser,
    currentUser,
    onLaunchAgain,
    onManageJob,
    resourcesEnabled,
    terminalEnabled,
  ]);

  const navigate = useNavigate();

  const handleDropdown = async (key: string) => {
    try {
      switch (key) {
        case Action.Connect:
          if (task.type === CommandType.JupyterLab) {
            const address = await getJupyterLabAddress(task.id);
            if (!address) {
              openToast({ severity: 'Error', title: NOTEBOOK_ACCESS_DENIED });
              break;
            }
            setJupyterLabAddress(address);
          }
          TaskConnectModal.open();
          break;
        case Action.OpenTerminal:
          openShellTerminalTab(task.id);
          onComplete?.(key);
          break;
        case Action.LaunchAgain:
          onLaunchAgain?.(task);
          break;
        case Action.ManageJob:
          onManageJob?.();
          break;
        case Action.Kill:
          confirm({
            content: 'Are you sure you want to kill this task?',
            danger: true,
            okText: 'Kill',
            onConfirm: async () => {
              await killTask(task);
              onComplete?.(key);
            },
            onError: handleError,
            title: 'Confirm Task Kill',
          });
          break;
        case Action.ViewResources:
          navigate(paths.taskResources(task.id));
          break;
        case Action.ViewLogs:
          onComplete?.(key);
          navigate(paths.taskLogs(task));
          break;
        case Action.CopyTaskID:
          await copyToClipboard(task.id);
          openToast({
            severity: 'Confirm',
            title: 'Task ID has been copied to clipboard.',
          });
          break;
      }
    } catch (e) {
      handleError(e, {
        level: ErrorLevel.Error,
        publicMessage: `Unable to ${key} task ${task.id}.`,
        publicSubject: `${capitalize(key)} failed.`,
        silent: false,
        type: ErrorType.Server,
      });
    }
    // TODO show loading indicator when we have a button component that supports it.
  };
  // The connect modal is rendered for the right-click menu, too, so that Connect works from it.
  const taskConnectModal = (
    <TaskConnectModal.Component fields={taskConnectFields} title={`Connect to ${task.name}`} />
  );
  return children ? (
    <>
      <Dropdown isContextMenu menu={menuItems} onClick={handleDropdown}>
        {children}
      </Dropdown>
      {taskConnectModal}
    </>
  ) : (
    <div className={css.base} title="Open actions menu">
      <Dropdown menu={menuItems} placement="bottomRight" onClick={handleDropdown}>
        <Button
          icon={<Icon name="overflow-vertical" size="small" title="Action menu" />}
          type="text"
        />
      </Dropdown>
      {taskConnectModal}
    </div>
  );
};

export default TaskActionDropdown;
