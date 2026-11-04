import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { MemoryRouter } from 'react-router-dom';

import userStore from 'stores/users';
import { CommandState, CommandTask, CommandType, DetailedUser } from 'types';
import { isDangerMenuItem, menuLabels } from 'utils/tests/menu';

import TaskActionDropdown from './TaskActionDropdown';

const mocks = vi.hoisted(() => ({ canCreateWorkspaceNSC: true }));

const resources = vi.hoisted(() => ({ enabled: false }));
vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => resources.enabled }));
vi.mock('hooks/usePermissions', () => ({
  default: () => ({
    canCreateWorkspaceNSC: () => mocks.canCreateWorkspaceNSC,
    canModifyWorkspaceNSC: ({ userId }: { userId?: number }) => userId === 101,
  }),
}));
vi.mock('routes/utils', () => ({
  paths: {
    shellTerminal: (id: string) => `/shells/${id}/terminal`,
    taskLogs: () => '/logs',
    taskResources: (id: string) => `/tasks/${id}/resources`,
  },
  serverAddress: () => 'http://localhost',
}));
vi.mock('services/api', () => ({ killTask: vi.fn() }));
const feature = vi.hoisted(() => ({ on: true }));
vi.mock('hooks/useFeature', () => ({ default: () => ({ isOn: () => feature.on }) }));

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

/** Opens the menu with Launch Again handled, for the given signed-in user. */
const openLaunchAgainMenu = async (
  taskOverrides: Partial<CommandTask>,
  signedIn: DetailedUser,
  contextMenu = false,
) => {
  userStore.updateCurrentUser(signedIn);
  const onLaunchAgain = vi.fn();
  const props = {
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

/** An ended notebook record: the master keeps its label, logs and resources, and nothing else. */
const notebook: Partial<CommandTask> = {
  id: 'nb-1',
  state: CommandState.Terminated,
  type: CommandType.JupyterLab,
};

const openContextMenu = (ownerId: number, overrides: Partial<CommandTask> = {}) => {
  render(
    <MemoryRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ConfirmationProvider>
          <TaskActionDropdown task={{ ...task, userId: ownerId, ...overrides }}>
            <div>task row</div>
          </TaskActionDropdown>
        </ConfirmationProvider>
      </UIProvider>
    </MemoryRouter>,
  );
  fireEvent.contextMenu(screen.getByText('task row'));
};

const runningShell: Partial<CommandTask> = { state: CommandState.Running, type: CommandType.Shell };

const setCurrentUser = (id: number, isAdmin = false) =>
  userStore.updateCurrentUser({ id, isActive: true, isAdmin, username: `user-${id}` });

describe('TaskActionDropdown', () => {
  beforeEach(() => {
    mocks.canCreateWorkspaceNSC = true;
    resources.enabled = false;
    setCurrentUser(101);
  });

  it('hides Kill for another user’s task', async () => {
    await openMenu(102);
    expect(screen.queryByText('Kill')).not.toBeInTheDocument();
  });

  it('shows Kill for the user’s own task', async () => {
    await openMenu(101);
    expect(screen.getByText('Kill')).toBeInTheDocument();
  });

  it.each([
    ['an ended', CommandState.Terminated],
    ['a running', CommandState.Running],
  ])(
    'offers only View Logs, View Resources and Copy Task ID on %s notebook record',
    async (_, state) => {
      resources.enabled = true;
      const onLaunchAgain = await openLaunchAgainMenu({ ...notebook, state }, owner);
      await screen.findByText('View Logs');
      expect(menuLabels()).toEqual(['View Logs', 'View Resources', 'Copy Task ID']);
      expect(onLaunchAgain).not.toHaveBeenCalled();
    },
  );

  describe('Connect', () => {
    const shellLabel = 'Connect via CLI';
    const exactly = (label: string) => screen.queryByText(label, { exact: true });

    it('is offered on the user’s own running shell', async () => {
      await openMenu(101, runningShell);
      expect(exactly(shellLabel)).toBeInTheDocument();
    });

    it('is not offered on another user’s running shell', async () => {
      await openMenu(102, runningShell);
      await screen.findByText('View Logs');
      expect(exactly(shellLabel)).not.toBeInTheDocument();
    });

    it('is not offered on another user’s running shell in the right-click menu', async () => {
      openContextMenu(102, runningShell);
      await screen.findByText('View Logs');
      expect(exactly(shellLabel)).not.toBeInTheDocument();
    });

    it('is offered to an admin on another user’s running shell', async () => {
      setCurrentUser(1, true);
      await openMenu(102, runningShell);
      expect(exactly(shellLabel)).toBeInTheDocument();
    });

    it('is not offered on a task that is not running', async () => {
      await openMenu(101, { state: CommandState.Queued, type: CommandType.Shell });
      await screen.findByText('View Logs');
      expect(exactly(shellLabel)).not.toBeInTheDocument();
    });

    it('is not offered before the current user is known', async () => {
      userStore.reset();
      await openMenu(101, runningShell);
      await screen.findByText('View Logs');
      expect(exactly(shellLabel)).not.toBeInTheDocument();
    });
  });

  describe('Open Terminal', () => {
    beforeEach(() => {
      feature.on = true;
      setCurrentUser(101);
    });

    it('opens the terminal of the user’s own running shell in a tab', async () => {
      const open = vi.spyOn(window, 'open').mockReturnValue(null);
      await openMenu(101, runningShell);
      await userEvent.click(screen.getByText('Open Terminal'));
      expect(open).toHaveBeenCalledWith('/shells/task-1/terminal', 'shell-terminal-task-1');
      // The CLI command is still offered.
      expect(screen.getByText('Connect via CLI')).toBeInTheDocument();
      open.mockRestore();
    });

    it('is in the context menu, too', async () => {
      openContextMenu(101, runningShell);
      expect(await screen.findByText('Open Terminal')).toBeInTheDocument();
    });

    it('is hidden for another user’s shell', async () => {
      await openMenu(102, runningShell);
      expect(screen.queryByText('Open Terminal')).not.toBeInTheDocument();
    });

    it('is shown to admins for any running shell', async () => {
      setCurrentUser(1, true);
      await openMenu(102, runningShell);
      expect(screen.getByText('Open Terminal')).toBeInTheDocument();
    });

    it('is hidden for shells that are not running and for other tasks', async () => {
      await openMenu(101, { ...runningShell, state: CommandState.Queued });
      expect(screen.queryByText('Open Terminal')).not.toBeInTheDocument();
    });

    it('is hidden for commands', async () => {
      await openMenu(101, { state: CommandState.Running, type: CommandType.Command });
      expect(screen.queryByText('Open Terminal')).not.toBeInTheDocument();
    });

    it('is hidden when the master turns the terminal off', async () => {
      feature.on = false;
      await openMenu(101, runningShell);
      expect(screen.queryByText('Open Terminal')).not.toBeInTheDocument();
    });
  });

  describe('Launch Again', () => {
    it.each([
      [CommandType.Shell, CommandState.Running],
      [CommandType.Shell, CommandState.Terminated],
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

    it('follows the signed-in user, as Connect and Open Terminal do', async () => {
      // Another user's running shell: none of the owner's actions, which all ask who is signed in.
      await openLaunchAgainMenu(runningShell, other);
      await screen.findByText('View Logs');
      ['Connect via CLI', 'Open Terminal', 'Launch Again'].forEach((label) =>
        expect(menuLabels()).not.toContain(label),
      );
    });

    it('offers the owner’s actions together to an admin signed in', async () => {
      feature.on = true;
      await openLaunchAgainMenu({ ...runningShell, userId: 102 }, admin);
      await screen.findByText('Launch Again');
      ['Connect via CLI', 'Open Terminal', 'Launch Again'].forEach((label) =>
        expect(menuLabels()).toContain(label),
      );
    });

    it('is not offered before the signed-in user is known', async () => {
      userStore.reset();
      render(
        <MemoryRouter>
          <UIProvider theme={DefaultTheme.Light}>
            <ConfirmationProvider>
              <TaskActionDropdown task={{ ...task, ...runningShell }} onLaunchAgain={vi.fn()} />
            </ConfirmationProvider>
          </UIProvider>
        </MemoryRouter>,
      );
      await userEvent.click(screen.getByRole('button'));
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
              <TaskActionDropdown task={{ ...task, type: CommandType.Shell }} />
            </ConfirmationProvider>
          </UIProvider>
        </MemoryRouter>,
      );
      await userEvent.click(screen.getByRole('button'));
      await screen.findByText('View Logs');
      expect(screen.queryByText('Launch Again')).not.toBeInTheDocument();
    });
  });
  describe('order', () => {
    beforeEach(() => {
      feature.on = true;
      resources.enabled = true;
      setCurrentUser(101);
    });

    const FULL_SHELL_MENU = [
      'View Logs',
      'View Resources',
      'Copy Task ID',
      'Connect via CLI',
      'Open Terminal',
      'Launch Again',
      'Kill',
    ];

    it('lists every action of the owner’s running shell in the fixed order', async () => {
      await openLaunchAgainMenu(runningShell, owner);
      await screen.findByText('Launch Again');
      expect(menuLabels()).toEqual(FULL_SHELL_MENU);
    });

    it.each([
      ['action menu', false],
      ['right-click menu', true],
    ])('shows Kill in red and the other actions not, in the %s', async (_, contextMenu) => {
      await openLaunchAgainMenu(runningShell, owner, contextMenu);
      await screen.findByText('Launch Again');
      expect(isDangerMenuItem('Kill')).toBe(true);
      FULL_SHELL_MENU.filter((label) => label !== 'Kill').forEach((label) =>
        expect(isDangerMenuItem(label)).toBe(false),
      );
    });

    it('uses the same order in the right-click menu', async () => {
      await openLaunchAgainMenu(runningShell, owner, true);
      await screen.findByText('Launch Again');
      expect(menuLabels()).toEqual(FULL_SHELL_MENU);
    });

    it('leaves out what does not apply and keeps the order of the rest', async () => {
      setCurrentUser(102);
      await openLaunchAgainMenu({ ...runningShell, userId: 103 }, other);
      await screen.findByText('View Logs');
      // Another user's shell: no Connect via CLI, Open Terminal, Launch Again or Kill.
      expect(menuLabels()).toEqual(['View Logs', 'View Resources', 'Copy Task ID']);
    });

    it('leaves out View Resources when the master does not offer it', async () => {
      resources.enabled = false;
      await openLaunchAgainMenu(runningShell, owner);
      await screen.findByText('Launch Again');
      expect(menuLabels()).toEqual(FULL_SHELL_MENU.filter((label) => label !== 'View Resources'));
    });

    it('puts Manage Job after Launch Again and before Kill when offered', async () => {
      const onManageJob = vi.fn();
      render(
        <MemoryRouter>
          <UIProvider theme={DefaultTheme.Light}>
            <ConfirmationProvider>
              <TaskActionDropdown
                task={{ ...task, ...runningShell }}
                onLaunchAgain={vi.fn()}
                onManageJob={onManageJob}
              />
            </ConfirmationProvider>
          </UIProvider>
        </MemoryRouter>,
      );
      await userEvent.click(screen.getByRole('button'));
      await screen.findByText('Manage Job');
      expect(menuLabels()).toEqual([
        'View Logs',
        'View Resources',
        'Copy Task ID',
        'Connect via CLI',
        'Open Terminal',
        'Launch Again',
        'Manage Job',
        'Kill',
      ]);
      await userEvent.click(screen.getByText('Manage Job'));
      expect(onManageJob).toHaveBeenCalledTimes(1);
    });
  });

  it('shows how to connect from the right-click menu', async () => {
    setCurrentUser(101);
    openContextMenu(101, runningShell);
    await userEvent.click(await screen.findByText('Connect via CLI'));
    expect(await screen.findByText('det shell open task-1')).toBeInTheDocument();
  });
});
