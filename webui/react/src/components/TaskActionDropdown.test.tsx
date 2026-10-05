import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { MemoryRouter } from 'react-router-dom';

import { CommandState, CommandTask, CommandType, DetailedUser } from 'types';
import { NOTEBOOK_ACCESS_DENIED } from 'utils/wait';

import TaskActionDropdown from './TaskActionDropdown';

const mocks = vi.hoisted(() => ({ canCreateWorkspaceNSC: true, getJupyterLab: vi.fn() }));

vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => false }));
vi.mock('hooks/usePermissions', () => ({
  default: () => ({
    canCreateWorkspaceNSC: () => mocks.canCreateWorkspaceNSC,
    canModifyWorkspaceNSC: ({ userId }: { userId?: number }) => userId === 101,
  }),
}));
vi.mock('routes/utils', () => ({
  paths: { taskLogs: () => '/logs' },
  serverAddress: () => 'http://localhost',
}));
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

const owner: DetailedUser = { id: 101, isActive: true, isAdmin: false, username: 'owner' };
const other: DetailedUser = { id: 102, isActive: true, isAdmin: false, username: 'other' };
const admin: DetailedUser = { id: 1, isActive: true, isAdmin: true, username: 'admin' };

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

const openLaunchAgainMenu = async (
  taskOverrides: Partial<CommandTask>,
  curUser: DetailedUser,
  contextMenu = false,
) => {
  const onLaunchAgain = vi.fn();
  const props = {
    curUser,
    onLaunchAgain,
    task: { ...task, ...taskOverrides },
  };
  render(
    <MemoryRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ConfirmationProvider>
          {contextMenu ? (
            <TaskActionDropdown {...props}>
              <div>row</div>
            </TaskActionDropdown>
          ) : (
            <TaskActionDropdown {...props} />
          )}
        </ConfirmationProvider>
      </UIProvider>
    </MemoryRouter>,
  );
  if (contextMenu) {
    await userEvent.pointer({ keys: '[MouseRight]', target: screen.getByText('row') });
  } else {
    await userEvent.click(screen.getByRole('button'));
  }
  return onLaunchAgain;
};

const notebook: Partial<CommandTask> = {
  id: 'nb-1',
  serviceAddress: '/proxy/nb-1/',
  type: CommandType.JupyterLab,
};

describe('TaskActionDropdown', () => {
  beforeEach(() => {
    mocks.canCreateWorkspaceNSC = true;
  });

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

  describe('Launch Again', () => {
    it.each([
      [CommandType.Shell, CommandState.Running],
      [CommandType.Shell, CommandState.Terminated],
      [CommandType.JupyterLab, CommandState.Running],
      [CommandType.JupyterLab, CommandState.Terminated],
    ])('is offered on the user’s own %s (%s) and passes the task on', async (type, state) => {
      const onLaunchAgain = await openLaunchAgainMenu({ state, type }, owner);
      await userEvent.click(await screen.findByText('Launch Again'));
      expect(onLaunchAgain).toHaveBeenCalledWith(expect.objectContaining({ id: 'task-1', type }));
    });

    it('is offered to an admin on another user’s shell', async () => {
      await openLaunchAgainMenu({ type: CommandType.Shell }, admin);
      expect(await screen.findByText('Launch Again')).toBeInTheDocument();
    });

    it('works from the right-click menu', async () => {
      const onLaunchAgain = await openLaunchAgainMenu({ type: CommandType.Shell }, owner, true);
      await userEvent.click(await screen.findByText('Launch Again'));
      expect(onLaunchAgain).toHaveBeenCalledTimes(1);
    });

    it('is not offered on another user’s shell', async () => {
      await openLaunchAgainMenu({ type: CommandType.Shell }, other);
      await screen.findByText('View Logs');
      expect(screen.queryByText('Launch Again')).not.toBeInTheDocument();
    });

    it.each([CommandType.Command, CommandType.TensorBoard])(
      'is not offered on a %s',
      async (type) => {
        await openLaunchAgainMenu({ type }, owner);
        await screen.findByText('View Logs');
        expect(screen.queryByText('Launch Again')).not.toBeInTheDocument();
      },
    );

    it('is not offered without permission to launch in the workspace', async () => {
      mocks.canCreateWorkspaceNSC = false;
      await openLaunchAgainMenu({ type: CommandType.Shell }, owner);
      await screen.findByText('View Logs');
      expect(screen.queryByText('Launch Again')).not.toBeInTheDocument();
    });

    it('is not offered when the list does not handle it', async () => {
      render(
        <MemoryRouter>
          <UIProvider theme={DefaultTheme.Light}>
            <ConfirmationProvider>
              <TaskActionDropdown curUser={owner} task={{ ...task, type: CommandType.Shell }} />
            </ConfirmationProvider>
          </UIProvider>
        </MemoryRouter>,
      );
      await userEvent.click(screen.getByRole('button'));
      await screen.findByText('View Logs');
      expect(screen.queryByText('Launch Again')).not.toBeInTheDocument();
    });
  });
});
