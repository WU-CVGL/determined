import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { MemoryRouter } from 'react-router-dom';

import { getTaskServices } from 'services/api';
import userStore from 'stores/users';
import { CommandState, CommandTask, CommandType, DetailedUser, TaskAction } from 'types';
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
    interactiveTask: (id: string, params: { tab?: string } = {}) =>
      `/tasks/${id}/interactive${params.tab ? `?tab=${params.tab}` : ''}`,
    taskLogs: () => '/logs',
    taskResources: (id: string) => `/tasks/${id}/resources`,
  },
  serverAddress: () => 'http://localhost',
}));
/** The task's services as the master reports them when the menu opens: a shell's SSH by default. */
const services = vi.hoisted(() => ({ list: [{ mayConnect: true, name: 'ssh' }] }));
vi.mock('services/api', () => ({
  getTaskServices: vi.fn(() => Promise.resolve({ services: services.list })),
  killTask: vi.fn(),
}));
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

const openMenu = async (
  ownerId: number,
  overrides: Partial<CommandTask> = {},
  onComplete?: (action?: TaskAction) => void,
) => {
  render(
    <MemoryRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ConfirmationProvider>
          <TaskActionDropdown
            task={{ ...task, userId: ownerId, ...overrides }}
            onComplete={onComplete}
          />
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
    services.list = [{ mayConnect: true, name: 'ssh' }];
    vi.mocked(getTaskServices).mockClear();
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
    ['an ended', 'nothing else', CommandState.Terminated, []],
    ['a running', 'Open JupyterLab', CommandState.Running, ['Open JupyterLab']],
  ])(
    'on %s notebook record offers View Logs, View Resources, Copy Task ID and %s',
    async (_, __, state, fromServices) => {
      resources.enabled = true;
      // A notebook record's one service is JupyterLab, and the master lets its owner connect to
      // every service alike. The entries follow the services, as the task page's tabs do, so a
      // notebook that still runs (the master ends restored ones at start) offers Open JupyterLab.
      services.list = [{ mayConnect: true, name: 'jupyter' }];
      const onLaunchAgain = await openLaunchAgainMenu({ ...notebook, state }, owner);
      await screen.findByText('View Logs');
      if (state === CommandState.Running) {
        await screen.findByText('Open JupyterLab');
        expect(getTaskServices).toHaveBeenCalledTimes(1);
      }
      expect(menuLabels()).toEqual([
        'View Logs',
        'View Resources',
        'Copy Task ID',
        ...fromServices,
      ]);
      expect(onLaunchAgain).not.toHaveBeenCalled();
    },
  );

  describe('Connect', () => {
    const shellLabel = 'Connect via CLI';
    const exactly = (label: string) => screen.queryByText(label, { exact: true });

    it('is offered on a running shell the master lets the user connect to', async () => {
      await openMenu(102, runningShell);
      await waitFor(() => expect(exactly(shellLabel)).toBeInTheDocument());
      expect(getTaskServices).toHaveBeenCalledWith({ taskId: 'task-1' });
    });

    it('is not offered on a shell the master does not let the user connect to', async () => {
      services.list = [{ mayConnect: false, name: 'ssh' }];
      await openMenu(102, runningShell);
      await screen.findByText('View Logs');
      await waitFor(() => expect(getTaskServices).toHaveBeenCalledTimes(1));
      expect(exactly(shellLabel)).not.toBeInTheDocument();
    });

    it('is not offered in the right-click menu either', async () => {
      services.list = [{ mayConnect: false, name: 'ssh' }];
      openContextMenu(102, runningShell);
      await screen.findByText('View Logs');
      await waitFor(() => expect(getTaskServices).toHaveBeenCalledTimes(1));
      expect(exactly(shellLabel)).not.toBeInTheDocument();
    });

    it('is offered in the right-click menu', async () => {
      openContextMenu(102, runningShell);
      await waitFor(() => expect(exactly(shellLabel)).toBeInTheDocument());
    });

    it('is offered on a shell that is still queued, which the CLI waits for', async () => {
      await openMenu(101, { state: CommandState.Queued, type: CommandType.Shell });
      await waitFor(() => expect(exactly(shellLabel)).toBeInTheDocument());
    });

    it('is not offered on a task that has ended, whose services are not read', async () => {
      await openMenu(101, { state: CommandState.Terminated, type: CommandType.Shell });
      await screen.findByText('View Logs');
      expect(exactly(shellLabel)).not.toBeInTheDocument();
      expect(getTaskServices).not.toHaveBeenCalled();
    });

    it('is not offered on a task without an SSH service', async () => {
      services.list = [];
      await openMenu(101, { state: CommandState.Running, type: CommandType.Command });
      await screen.findByText('View Logs');
      await waitFor(() => expect(getTaskServices).toHaveBeenCalledTimes(1));
      expect(exactly(shellLabel)).not.toBeInTheDocument();
    });

    it('reads the services once per menu', async () => {
      await openMenu(101, runningShell);
      await waitFor(() => expect(exactly(shellLabel)).toBeInTheDocument());
      await userEvent.keyboard('{Escape}');
      await userEvent.click(screen.getByRole('button'));
      await waitFor(() => expect(exactly(shellLabel)).toBeInTheDocument());
      expect(getTaskServices).toHaveBeenCalledTimes(1);
    });

    it('reads the services again when the menu opens after a failed read', async () => {
      vi.mocked(getTaskServices).mockRejectedValueOnce(new Error('network'));
      await openMenu(101, runningShell);
      await screen.findByText('View Logs');
      await waitFor(() => expect(getTaskServices).toHaveBeenCalledTimes(1));
      expect(exactly(shellLabel)).not.toBeInTheDocument();
      expect(exactly('Open Terminal')).not.toBeInTheDocument();
      // Closed and opened again from its button.
      await userEvent.click(screen.getByRole('button'));
      await waitFor(() => expect(document.querySelector('.ant-dropdown-hidden')).not.toBeNull());
      await userEvent.click(screen.getByRole('button'));
      await waitFor(() => expect(exactly(shellLabel)).toBeInTheDocument());
      expect(exactly('Open Terminal')).toBeInTheDocument();
      expect(getTaskServices).toHaveBeenCalledTimes(2);
    });
  });

  describe('Open Terminal', () => {
    beforeEach(() => {
      feature.on = true;
      setCurrentUser(101);
    });

    it('opens the Terminal tab of the task page in a tab named after the task', async () => {
      const open = vi.spyOn(window, 'open').mockReturnValue(null);
      const onComplete = vi.fn();
      await openMenu(101, runningShell, onComplete);
      await userEvent.click(await screen.findByText('Open Terminal'));
      expect(open).toHaveBeenCalledWith('/tasks/task-1/interactive?tab=terminal', 'task-1', '');
      expect(onComplete).toHaveBeenCalledWith(TaskAction.OpenTerminal);
      // The CLI command is still offered.
      expect(screen.getByText('Connect via CLI')).toBeInTheDocument();
      open.mockRestore();
    });

    it('is in the context menu, too', async () => {
      openContextMenu(101, runningShell);
      expect(await screen.findByText('Open Terminal')).toBeInTheDocument();
    });

    it('is hidden for a shell the master does not let the user connect to', async () => {
      services.list = [{ mayConnect: false, name: 'ssh' }];
      await openMenu(102, runningShell);
      await waitFor(() => expect(getTaskServices).toHaveBeenCalledTimes(1));
      expect(screen.queryByText('Open Terminal')).not.toBeInTheDocument();
    });

    it('is hidden for shells that have ended', async () => {
      await openMenu(101, { ...runningShell, state: CommandState.Terminated });
      expect(screen.queryByText('Open Terminal')).not.toBeInTheDocument();
    });

    it('is hidden for tasks without an SSH service', async () => {
      services.list = [];
      await openMenu(101, { state: CommandState.Running, type: CommandType.Command });
      await waitFor(() => expect(getTaskServices).toHaveBeenCalledTimes(1));
      expect(screen.queryByText('Open Terminal')).not.toBeInTheDocument();
    });

    it('is hidden when the master turns the terminal off', async () => {
      feature.on = false;
      await openMenu(101, runningShell);
      await waitFor(() => expect(screen.getByText('Connect via CLI')).toBeInTheDocument());
      expect(screen.queryByText('Open Terminal')).not.toBeInTheDocument();
    });
  });

  describe('Open JupyterLab and Open VS Code', () => {
    const allServices = [
      { mayConnect: true, name: 'ssh' },
      { mayConnect: true, name: 'jupyter' },
      { mayConnect: true, name: 'vscode' },
    ];

    beforeEach(() => {
      feature.on = true;
      setCurrentUser(101);
    });

    it('follow Open Terminal, one per service of the task', async () => {
      services.list = allServices;
      await openMenu(101, runningShell);
      await screen.findByText('Open VS Code');
      expect(menuLabels()).toEqual([
        'View Logs',
        'Copy Task ID',
        'Connect via CLI',
        'Open Terminal',
        'Open JupyterLab',
        'Open VS Code',
        'Kill',
      ]);
    });

    it.each([
      [TaskAction.OpenJupyterLab, 'jupyter'],
      [TaskAction.OpenVSCode, 'vscode'],
    ])(
      '"%s" opens the %s tab of the task page in a tab named after the task',
      async (action, tab) => {
        const open = vi.spyOn(window, 'open').mockReturnValue(null);
        const onComplete = vi.fn();
        services.list = allServices;
        await openMenu(101, runningShell, onComplete);
        await userEvent.click(await screen.findByText(action));
        expect(open).toHaveBeenCalledWith(`/tasks/task-1/interactive?tab=${tab}`, 'task-1', '');
        expect(onComplete).toHaveBeenCalledWith(action);
        open.mockRestore();
      },
    );

    it('are in the context menu, too', async () => {
      services.list = allServices;
      openContextMenu(101, runningShell);
      expect(await screen.findByText('Open JupyterLab')).toBeInTheDocument();
      expect(screen.getByText('Open VS Code')).toBeInTheDocument();
    });

    it('are offered without the terminal when the master turns it off', async () => {
      feature.on = false;
      services.list = allServices;
      await openMenu(101, runningShell);
      await screen.findByText('Open VS Code');
      expect(menuLabels()).toEqual([
        'View Logs',
        'Copy Task ID',
        'Connect via CLI',
        'Open JupyterLab',
        'Open VS Code',
        'Kill',
      ]);
    });

    it('are offered one at a time, for the service the task runs', async () => {
      services.list = [
        { mayConnect: true, name: 'ssh' },
        { mayConnect: true, name: 'vscode' },
      ];
      await openMenu(101, runningShell);
      await screen.findByText('Open VS Code');
      expect(menuLabels()).not.toContain('Open JupyterLab');
    });

    it('are hidden for a shell the master does not let the user connect to', async () => {
      services.list = allServices.map((service) => ({ ...service, mayConnect: false }));
      await openMenu(102, runningShell);
      await waitFor(() => expect(getTaskServices).toHaveBeenCalledTimes(1));
      expect(screen.queryByText('Open JupyterLab')).not.toBeInTheDocument();
      expect(screen.queryByText('Open VS Code')).not.toBeInTheDocument();
    });

    it('are hidden for shells that have ended', async () => {
      services.list = allServices;
      await openMenu(101, { ...runningShell, state: CommandState.Terminated });
      await screen.findByText('View Logs');
      expect(screen.queryByText('Open JupyterLab')).not.toBeInTheDocument();
      expect(screen.queryByText('Open VS Code')).not.toBeInTheDocument();
      expect(getTaskServices).not.toHaveBeenCalled();
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

    it('is left out with Connect and Open Terminal on another user’s shell', async () => {
      // Another user's running shell: the master lets no one but its owner and admins connect, and
      // Launch Again asks who is signed in.
      services.list = [{ mayConnect: false, name: 'ssh' }];
      await openLaunchAgainMenu(runningShell, other);
      await screen.findByText('View Logs');
      await waitFor(() => expect(getTaskServices).toHaveBeenCalledTimes(1));
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
      await waitFor(() => expect(menuLabels()).toEqual(FULL_SHELL_MENU));
    });

    it.each([
      ['action menu', false],
      ['right-click menu', true],
    ])('shows Kill in red and the other actions not, in the %s', async (_, contextMenu) => {
      await openLaunchAgainMenu(runningShell, owner, contextMenu);
      await waitFor(() => expect(menuLabels()).toEqual(FULL_SHELL_MENU));
      expect(isDangerMenuItem('Kill')).toBe(true);
      FULL_SHELL_MENU.filter((label) => label !== 'Kill').forEach((label) =>
        expect(isDangerMenuItem(label)).toBe(false),
      );
    });

    it('uses the same order in the right-click menu', async () => {
      await openLaunchAgainMenu(runningShell, owner, true);
      await waitFor(() => expect(menuLabels()).toEqual(FULL_SHELL_MENU));
    });

    it('leaves out what does not apply and keeps the order of the rest', async () => {
      services.list = [{ mayConnect: false, name: 'ssh' }];
      setCurrentUser(102);
      await openLaunchAgainMenu({ ...runningShell, userId: 103 }, other);
      await screen.findByText('View Logs');
      await waitFor(() => expect(getTaskServices).toHaveBeenCalledTimes(1));
      // Another user's shell: no Connect via CLI, Open Terminal, Launch Again or Kill.
      expect(menuLabels()).toEqual(['View Logs', 'View Resources', 'Copy Task ID']);
    });

    it('leaves out View Resources when the master does not offer it', async () => {
      resources.enabled = false;
      await openLaunchAgainMenu(runningShell, owner);
      await waitFor(() =>
        expect(menuLabels()).toEqual(FULL_SHELL_MENU.filter((label) => label !== 'View Resources')),
      );
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
