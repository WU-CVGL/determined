import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { MemoryRouter } from 'react-router-dom';

import { CommandState, CommandTask, CommandType } from 'types';
import { NOTEBOOK_ACCESS_DENIED } from 'utils/wait';

import TaskActionDropdown from './TaskActionDropdown';

vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => false }));
vi.mock('hooks/usePermissions', () => ({
  default: () => ({
    canModifyWorkspaceNSC: ({ userId }: { userId?: number }) => userId === 101,
  }),
}));
vi.mock('routes/utils', () => ({
  paths: { taskLogs: () => '/logs' },
  serverAddress: () => 'http://localhost',
}));
const mocks = vi.hoisted(() => ({ getJupyterLab: vi.fn() }));
vi.mock('services/api', () => ({ getJupyterLab: mocks.getJupyterLab, killTask: vi.fn() }));

const task: CommandTask = {
  id: 'task-1',
  name: 'Task',
  resourcePool: 'default',
  startTime: '2024-01-01T00:00:00Z',
  state: CommandState.Running,
  type: CommandType.Command,
  userId: 101,
  workspaceId: 10,
};

const openMenu = async (ownerId: number, overrides: Partial<CommandTask> = {}) => {
  render(
    <MemoryRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ConfirmationProvider>
          <TaskActionDropdown task={{ ...task, userId: ownerId, ...overrides }} />
        </ConfirmationProvider>
      </UIProvider>
    </MemoryRouter>,
  );
  await userEvent.click(screen.getByRole('button'));
};

const notebook: Partial<CommandTask> = {
  id: 'nb-1',
  serviceAddress: '/proxy/nb-1/',
  type: CommandType.JupyterLab,
};

describe('TaskActionDropdown', () => {
  it('hides Kill for another user’s task', async () => {
    await openMenu(102);
    expect(screen.queryByText('Kill')).not.toBeInTheDocument();
  });

  it('shows Kill for the user’s own task', async () => {
    await openMenu(101);
    expect(screen.getByText('Kill')).toBeInTheDocument();
  });

  it('connects to a notebook with the token that the master returns to its owner', async () => {
    mocks.getJupyterLab.mockResolvedValue({ serviceAddress: '/proxy/nb-1/?token=tok' });
    await openMenu(101, notebook);
    await userEvent.click(screen.getByText('Connect'));
    expect(await screen.findByText('http://localhost/proxy/nb-1/?token=tok')).toBeInTheDocument();
    expect(mocks.getJupyterLab).toHaveBeenCalledWith({ commandId: 'nb-1' });
  });

  it('refuses to connect to a notebook without its token', async () => {
    mocks.getJupyterLab.mockResolvedValue({ serviceAddress: '/proxy/nb-1/' });
    await openMenu(102, notebook);
    await userEvent.click(screen.getByText('Connect'));
    expect(await screen.findByText(NOTEBOOK_ACCESS_DENIED)).toBeInTheDocument();
    expect(screen.queryByText('http://localhost/proxy/nb-1/')).not.toBeInTheDocument();
  });
});
