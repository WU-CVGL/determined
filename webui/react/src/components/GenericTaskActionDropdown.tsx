import Button from 'hew/Button';
import Dropdown, { MenuItem } from 'hew/Dropdown';
import Icon from 'hew/Icon';
import { useToast } from 'hew/Toast';
import React, { useMemo } from 'react';
import { useNavigate } from 'react-router-dom';

import css from 'components/ActionDropdown/ActionDropdown.module.scss';
import useGenericTaskActions from 'hooks/useGenericTaskActions';
import usePermissions from 'hooks/usePermissions';
import useTaskResourcesEnabled from 'hooks/useTaskResourcesEnabled';
import { paths } from 'routes/utils';
import { GenericTask, ValueOf } from 'types';
import { copyToClipboard } from 'utils/dom';
import handleError, { ErrorLevel, ErrorType } from 'utils/error';

export const GenericTaskAction = {
  CopyTaskID: 'Copy Task ID',
  Kill: 'Kill',
  Pause: 'Pause',
  Unpause: 'Unpause',
  ViewLogs: 'View Logs',
  ViewResources: 'View Resources',
} as const;

export type GenericTaskAction = ValueOf<typeof GenericTaskAction>;

interface Props {
  /** The row the right-click menu opens on; without it, the menu opens from a button. */
  children?: React.ReactNode;
  /** Called after an action, to refresh the list. */
  onComplete?: () => void;
  task: GenericTask;
}

/**
 * The menu of a generic task in the generic task list, in the order of the task menu: viewing
 * first, then Copy Task ID, Pause or Unpause, and the destructive Kill last, in red. Pause, Unpause
 * and Kill are offered as the task's page offers them, to the task's owner or an admin, and ask the
 * same confirmations (useGenericTaskActions). Kill kills the task and its descendants, as the
 * page's Kill does. In the generic task list, the two menus of a row share the task's running
 * action and failed unpause (GenericTaskActionStateContext).
 */
const GenericTaskActionDropdown: React.FC<Props> = ({ children, onComplete, task }: Props) => {
  const { canModifyWorkspaceNSC } = usePermissions();
  const resourcesEnabled = useTaskResourcesEnabled();
  const navigate = useNavigate();
  const { openToast } = useToast();
  const canControl = canModifyWorkspaceNSC({
    userId: task.userId,
    workspace: { id: task.workspaceId },
  });
  const { canKill, canPause, canUnpause, isBusy, isUnpauseRetry, kill, pause, unpause } =
    useGenericTaskActions({
      canControl,
      onComplete,
      task: {
        name: task.name,
        noPause: task.noPause,
        parentId: task.parentId,
        state: task.state,
        taskId: task.taskId,
      },
    });

  const menuItems: MenuItem[] = useMemo(() => {
    const items: MenuItem[] = [{ key: GenericTaskAction.ViewLogs, label: 'View Logs' }];
    if (resourcesEnabled) {
      items.push({ key: GenericTaskAction.ViewResources, label: 'View Resources' });
    }
    items.push({ key: GenericTaskAction.CopyTaskID, label: 'Copy Task ID' });
    if (canPause) items.push({ disabled: isBusy, key: GenericTaskAction.Pause, label: 'Pause' });
    if (canUnpause) {
      items.push({
        disabled: isBusy,
        key: GenericTaskAction.Unpause,
        label: isUnpauseRetry ? 'Retry Unpause' : 'Unpause',
      });
    }
    if (canKill) {
      items.push({ danger: true, disabled: isBusy, key: GenericTaskAction.Kill, label: 'Kill' });
    }
    return items;
  }, [canKill, canPause, canUnpause, isBusy, isUnpauseRetry, resourcesEnabled]);

  const handleDropdown = async (key: string) => {
    try {
      switch (key) {
        case GenericTaskAction.ViewLogs:
          navigate(paths.genericTaskDetails(task.taskId, 'logs'));
          break;
        case GenericTaskAction.ViewResources:
          navigate(paths.genericTaskDetails(task.taskId, 'resources'));
          break;
        case GenericTaskAction.CopyTaskID:
          await copyToClipboard(task.taskId);
          openToast({ severity: 'Confirm', title: 'Task ID has been copied to clipboard.' });
          break;
        case GenericTaskAction.Pause:
          pause();
          break;
        case GenericTaskAction.Unpause:
          unpause();
          break;
        case GenericTaskAction.Kill:
          kill();
          break;
      }
    } catch (e) {
      handleError(e, {
        level: ErrorLevel.Error,
        publicSubject: `${key} failed.`,
        silent: false,
        type: ErrorType.Ui,
      });
    }
  };

  return children ? (
    <Dropdown isContextMenu menu={menuItems} onClick={handleDropdown}>
      {children}
    </Dropdown>
  ) : (
    <div className={css.base} title="Open actions menu">
      <Dropdown menu={menuItems} placement="bottomRight" onClick={handleDropdown}>
        <Button
          icon={<Icon name="overflow-vertical" size="small" title="Action menu" />}
          type="text"
        />
      </Dropdown>
    </div>
  );
};

export default GenericTaskActionDropdown;
