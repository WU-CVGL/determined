import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { MemoryRouter } from 'react-router-dom';

import { CommandState, CommandTask, CommandType } from 'types';
import { isDangerMenuItem, isDisabledMenuItem, menuLabels } from 'utils/tests/menu';

import TaskBar from './TaskBar';

const tensorBoard: CommandTask = {
  id: 'tb-1',
  name: 'TensorBoard (lively-calm-fox)',
  resourcePool: 'default',
  startTime: '2026-01-01T00:00:00Z',
  state: CommandState.Running,
  type: CommandType.TensorBoard,
  userId: 7,
  workspaceId: 1,
};

const api = vi.hoisted(() => ({
  getCommand: vi.fn(),
  getShell: vi.fn(),
  getTensorBoard: vi.fn(),
  killTask: vi.fn(),
}));
vi.mock('services/api', () => api);
vi.mock('hooks/usePermissions', () => ({
  default: () => ({ canModifyWorkspaceNSC: () => true }),
}));

const renderBar = (task: CommandTask, handleViewLogsClick = vi.fn()) =>
  render(
    <MemoryRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ConfirmationProvider>
          <TaskBar
            handleViewLogsClick={handleViewLogsClick}
            id={task.id}
            name={task.name}
            resourcePool={task.resourcePool}
            type={task.type}
          />
        </ConfirmationProvider>
      </UIProvider>
    </MemoryRouter>,
  );

describe('TaskBar', () => {
  beforeEach(() => {
    Object.values(api).forEach((fn) => fn.mockReset());
    api.getTensorBoard.mockResolvedValue(tensorBoard);
  });

  it('lists View Logs first and Kill last, in red, like the task action menu', async () => {
    const handleViewLogsClick = vi.fn();
    renderBar(tensorBoard, handleViewLogsClick);
    await userEvent.click(screen.getByTestId('task-action-dropdown-trigger'));
    await screen.findByText('Kill');
    expect(menuLabels()).toEqual(['View Logs', 'Kill']);
    expect(isDangerMenuItem('Kill')).toBe(true);
    expect(isDangerMenuItem('View Logs')).toBe(false);
    await userEvent.click(screen.getByText('View Logs'));
    expect(handleViewLogsClick).toHaveBeenCalledTimes(1);
  });

  it('keeps View Logs and leaves Kill disabled for a notebook record, which has no lookup', async () => {
    const handleViewLogsClick = vi.fn();
    renderBar({ ...tensorBoard, id: 'nb-1', type: CommandType.JupyterLab }, handleViewLogsClick);
    await userEvent.click(screen.getByTestId('task-action-dropdown-trigger'));
    await screen.findByText('Kill');
    expect(menuLabels()).toEqual(['View Logs', 'Kill']);
    expect(isDisabledMenuItem('Kill')).toBe(true);
    expect(api.getCommand).not.toHaveBeenCalled();
    expect(api.getShell).not.toHaveBeenCalled();
    expect(api.getTensorBoard).not.toHaveBeenCalled();
    await userEvent.click(screen.getByText('View Logs'));
    expect(handleViewLogsClick).toHaveBeenCalledTimes(1);
  });
});
