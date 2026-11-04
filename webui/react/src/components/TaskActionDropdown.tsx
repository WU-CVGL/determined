import Button from 'hew/Button';
import Dropdown, { MenuItem } from 'hew/Dropdown';
import Icon from 'hew/Icon';
import { useModal } from 'hew/Modal';
import { useToast } from 'hew/Toast';
import useConfirm from 'hew/useConfirm';
import { Loadable } from 'hew/utils/loadable';
import { useObservable } from 'micro-observables';
import React, { useCallback, useMemo, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';

import css from 'components/ActionDropdown/ActionDropdown.module.scss';
import TaskConnectModalComponent, { TaskConnectField } from 'components/TaskConnectModal';
import useFeature from 'hooks/useFeature';
import usePermissions from 'hooks/usePermissions';
import useTaskResourcesEnabled from 'hooks/useTaskResourcesEnabled';
import { paths } from 'routes/utils';
import { getTaskServices, killTask } from 'services/api';
import { V1TaskService } from 'services/api-ts-sdk';
import userStore from 'stores/users';
import { TaskAction as Action, CommandTask, CommandType, InteractiveTaskTab } from 'types';
import { copyToClipboard } from 'utils/dom';
import handleError, { ErrorLevel, ErrorType } from 'utils/error';
import { capitalize } from 'utils/string';
import {
  canBeOpened,
  canConnectToTask,
  canOpenShellTerminal,
  canOpenTaskService,
  isTaskKillable,
} from 'utils/task';
import { openInteractiveTask } from 'utils/wait';

interface Props {
  children?: React.ReactNode;
  onComplete?: (action?: Action) => void;
  /** Offers "Launch Again" on the user's own shells when given. */
  onLaunchAgain?: (task: CommandTask) => void;
  /** Offers "Manage Job" (the job queue's priority, weight and pool) when given. */
  onManageJob?: () => void;
  task: CommandTask;
}

const TaskActionDropdown: React.FC<Props> = ({
  task,
  onComplete,
  onLaunchAgain,
  onManageJob,
  children,
}: Props) => {
  const { canCreateWorkspaceNSC, canModifyWorkspaceNSC } = usePermissions();
  const resourcesEnabled = useTaskResourcesEnabled();
  const terminalEnabled = useFeature().isOn('shell_terminal');
  // The signed-in user, for every rule of the menu that depends on who is asking.
  const currentUser = Loadable.getOrElse(undefined, useObservable(userStore.currentUser));
  const { openToast } = useToast();
  const TaskConnectModal = useModal(TaskConnectModalComponent);

  const confirm = useConfirm();

  // The task's services, read when the menu opens: they say whether the user may connect. Only an
  // answer is kept; after a failed read, the next opening reads again.
  const [services, setServices] = useState<V1TaskService[]>();
  const loadingServices = useRef(false);
  const loadServices = useCallback(
    (open: boolean) => {
      if (!open || services !== undefined || loadingServices.current || !canBeOpened(task)) return;
      loadingServices.current = true;
      getTaskServices({ taskId: task.id })
        .then((response) => setServices(response.services))
        .catch((e) => {
          handleError(e, {
            publicMessage: 'Unable to load the services of the task.',
            silent: true,
          });
        })
        .finally(() => {
          loadingServices.current = false;
        });
    },
    [services, task],
  );

  const taskConnectFields: TaskConnectField[] = useMemo(() => {
    switch (task.type) {
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
  }, [task]);

  // One order everywhere the menu appears: viewing first, then connecting, then launching and
  // managing, with the destructive Kill last. Items that do not apply are left out.
  const menuItems: MenuItem[] = useMemo(() => {
    const items: MenuItem[] = [{ key: Action.ViewLogs, label: 'View Logs' }];
    if (resourcesEnabled) items.push({ key: Action.ViewResources, label: 'View Resources' });
    items.push({ key: Action.CopyTaskID, label: 'Copy Task ID' });
    // Only the owner or an admin gets the shell's key from the master; a task that has ended
    // has no session to connect to.
    if (canBeOpened(task) && canConnectToTask(services ?? [])) {
      items.push({ key: Action.Connect, label: 'Connect via CLI' });
    }
    if (terminalEnabled && canBeOpened(task) && canOpenShellTerminal(services ?? [])) {
      items.push({ key: Action.OpenTerminal, label: 'Open Terminal' });
    }
    // A service's tab opens it once it is ready, so the entries follow the services alone.
    if (canBeOpened(task) && canOpenTaskService(services ?? [], 'jupyter')) {
      items.push({ key: Action.OpenJupyterLab, label: 'Open JupyterLab' });
    }
    if (canBeOpened(task) && canOpenTaskService(services ?? [], 'vscode')) {
      items.push({ key: Action.OpenVSCode, label: 'Open VS Code' });
    }
    // A UI rule only: the API lets anyone who can view a task read its config.
    if (
      onLaunchAgain &&
      task.type === CommandType.Shell &&
      !!currentUser &&
      (currentUser.id === task.userId || currentUser.isAdmin) &&
      canCreateWorkspaceNSC({ workspace: { id: task.workspaceId } })
    ) {
      items.push({ key: Action.LaunchAgain, label: 'Launch Again' });
    }
    if (onManageJob) items.push({ key: Action.ManageJob, label: 'Manage Job' });
    // The master has no API to kill a notebook, the label of ended records.
    if (
      task.type !== CommandType.JupyterLab &&
      isTaskKillable(
        task,
        canModifyWorkspaceNSC({ userId: task.userId, workspace: { id: task.workspaceId } }),
      )
    ) {
      items.push({ danger: true, key: Action.Kill, label: 'Kill' });
    }
    return items;
  }, [
    task,
    canCreateWorkspaceNSC,
    canModifyWorkspaceNSC,
    currentUser,
    onLaunchAgain,
    onManageJob,
    resourcesEnabled,
    services,
    terminalEnabled,
  ]);

  const navigate = useNavigate();

  const handleDropdown = async (key: string) => {
    try {
      switch (key) {
        case Action.Connect:
          TaskConnectModal.open();
          break;
        case Action.OpenTerminal:
          openInteractiveTask(task.id, { tab: InteractiveTaskTab.Terminal });
          onComplete?.(key);
          break;
        case Action.OpenJupyterLab:
          openInteractiveTask(task.id, { tab: InteractiveTaskTab.JupyterLab });
          onComplete?.(key);
          break;
        case Action.OpenVSCode:
          openInteractiveTask(task.id, { tab: InteractiveTaskTab.VSCode });
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
      <Dropdown isContextMenu menu={menuItems} onClick={handleDropdown} onOpenChange={loadServices}>
        {children}
      </Dropdown>
      {taskConnectModal}
    </>
  ) : (
    <div className={css.base} title="Open actions menu">
      <Dropdown
        menu={menuItems}
        placement="bottomRight"
        onClick={handleDropdown}
        onOpenChange={loadServices}>
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
