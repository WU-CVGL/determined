import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { MemoryRouter } from 'react-router-dom';

import { CommandState, CommandTask, CommandType } from 'types';
import { isDangerMenuItem, menuLabels } from 'utils/tests/menu';

import TaskBar from './TaskBar';

const notebook: CommandTask = {
  id: 'nb-1',
  name: 'JupyterLab (lively-calm-fox)',
  resourcePool: 'default',
  startTime: '2026-01-01T00:00:00Z',
  state: CommandState.Running,
  type: CommandType.JupyterLab,
  userId: 7,
  workspaceId: 1,
};

vi.mock('services/api', () => ({
  getCommand: vi.fn(),
  getJupyterLab: vi.fn(() => Promise.resolve(notebook)),
  getShell: vi.fn(),
  getTensorBoard: vi.fn(),
  killTask: vi.fn(),
}));
vi.mock('hooks/usePermissions', () => ({
  default: () => ({ canModifyWorkspaceNSC: () => true }),
}));

describe('TaskBar', () => {
  it('lists View Logs first and Kill last, in red, like the task action menu', async () => {
    const handleViewLogsClick = vi.fn();
    render(
      <MemoryRouter>
        <UIProvider theme={DefaultTheme.Light}>
          <ConfirmationProvider>
            <TaskBar
              handleViewLogsClick={handleViewLogsClick}
              id={notebook.id}
              name={notebook.name}
              resourcePool={notebook.resourcePool}
              type={CommandType.JupyterLab}
            />
          </ConfirmationProvider>
        </UIProvider>
      </MemoryRouter>,
    );
    await userEvent.click(screen.getByTestId('task-action-dropdown-trigger'));
    await screen.findByText('Kill');
    expect(menuLabels()).toEqual(['View Logs', 'Kill']);
    expect(isDangerMenuItem('Kill')).toBe(true);
    expect(isDangerMenuItem('View Logs')).toBe(false);
    await userEvent.click(screen.getByText('View Logs'));
    expect(handleViewLogsClick).toHaveBeenCalledTimes(1);
  });
});
