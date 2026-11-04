import Dropdown, { MenuItem } from 'hew/Dropdown';
import Icon from 'hew/Icon';
import useConfirm from 'hew/useConfirm';
import React, { useCallback, useMemo } from 'react';

import usePermissions from 'hooks/usePermissions';
import { paths } from 'routes/utils';
import { killTask } from 'services/api';
import { CommandType } from 'types';
import handleError from 'utils/error';
import { routeToReactUrl } from 'utils/routes';

import css from './TaskBar.module.scss';

interface Props {
  handleViewLogsClick: () => void;
  id: string;
  name: string;
  /** The task's owner and workspace, which decide who may kill it; without them, nobody can. */
  owner?: { userId: number; workspaceId: number };
  resourcePool: string;
  /** The task's type, for the kill API; a notebook, the label of ended records, has none. */
  type?: CommandType;
}

const MenuKey = {
  Kill: 'kill',
  ViewLogs: 'viewLogs',
} as const;

export const TaskBar: React.FC<Props> = ({
  handleViewLogsClick,
  id,
  name,
  owner,
  resourcePool,
  type,
}: Props) => {
  const { canModifyWorkspaceNSC } = usePermissions();
  const confirm = useConfirm();

  const canKill =
    !!type &&
    type !== CommandType.JupyterLab &&
    !!owner &&
    canModifyWorkspaceNSC({ userId: owner.userId, workspace: { id: owner.workspaceId } });

  const deleteTask = useCallback(() => {
    if (!type) return;
    confirm({
      content: 'Are you sure you want to kill this task?',
      danger: true,
      okText: 'Kill',
      onConfirm: async () => {
        try {
          await killTask({ id, type });
          routeToReactUrl(paths.taskList());
        } catch (e) {
          handleError(e, {
            publicMessage: `Unable to kill task ${id}.`,
            publicSubject: 'Kill failed.',
            silent: false,
          });
        }
      },
      onError: handleError,
      title: 'Confirm Task Kill',
    });
  }, [confirm, id, type]);

  // The task action menu's order: View Logs first, the destructive Kill last.
  const menuItems: MenuItem[] = useMemo(
    () => [
      { key: MenuKey.ViewLogs, label: 'View Logs' },
      { danger: true, disabled: !canKill, key: MenuKey.Kill, label: 'Kill' },
    ],
    [canKill],
  );

  const handleDropdown = (key: string) => {
    switch (key) {
      case MenuKey.Kill:
        deleteTask();
        break;
      case MenuKey.ViewLogs:
        handleViewLogsClick();
        break;
    }
  };

  return (
    <div className={css.base}>
      <div className={css.barContent}>
        <span>{name}</span>
        {resourcePool && <span>&#8212;</span>}
        <Dropdown menu={menuItems} placement="bottomRight" onClick={handleDropdown}>
          <div className={css.dropdownTrigger} data-testid="task-action-dropdown-trigger">
            {resourcePool && <span className={css.dropdownTrigger}>{resourcePool}</span>}
            <Icon name="arrow-down" title="Action menu" />
          </div>
        </Dropdown>
      </div>
    </div>
  );
};

export default TaskBar;
