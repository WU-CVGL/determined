import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import Button from 'hew/Button';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { Loadable } from 'hew/utils/loadable';
import React, { useEffect } from 'react';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { useLaunchAgain } from 'hooks/useLaunchAgain';
import { useLaunchForm } from 'hooks/useLaunchForm';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import authStore from 'stores/auth';
import userStore from 'stores/users';
import { CommandState, CommandTask, CommandType, RawJson, WorkspaceState } from 'types';

const mocks = vi.hoisted(() => ({
  getShellConfig: vi.fn(),
  getTaskServices: vi.fn(),
  launchShell: vi.fn(),
  onShellLaunched: vi.fn(),
  openCommandResponse: vi.fn(),
}));

vi.mock('services/api', () => ({
  getAvailableResourcePools: () => Promise.resolve([]),
  getCurrentUser: () => Promise.resolve({ id: 1, isActive: true, isAdmin: false, username: 'me' }),
  getResourcePools: () => Promise.resolve([]),
  getShellConfig: mocks.getShellConfig,
  getShells: () => Promise.resolve([]),
  getTaskServices: mocks.getTaskServices,
  getTaskTemplates: () => Promise.resolve([]),
  getUsers: () => Promise.resolve({ users: [] }),
  getUserSetting: () => Promise.resolve({ settings: [] }),
  getWorkspaces: () => Promise.resolve({ workspaces: [] }),
  launchShell: mocks.launchShell,
  // A master without code-server: it runs jupyter by default.
  previewShell: (params: { config?: RawJson }) =>
    Promise.resolve({ ...params.config, services: { jupyter: {}, ...params.config?.services } }),
  updateUserSetting: () => Promise.resolve(),
}));

vi.mock('hooks/usePermissions', () => ({
  default: () => ({
    canCreateTemplateWorkspace: () => false,
    canCreateWorkspaceNSC: () => true,
  }),
}));

vi.mock('hooks/useFeature', () => ({ default: () => ({ isOn: () => true }) }));
vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => true }));

vi.mock('stores/cluster', async (importOriginal) => {
  const loadable = await import('hew/utils/loadable');
  const observable = await import('utils/observable');

  const store = { resourcePools: observable.observable(loadable.Loaded([])) };
  return {
    __esModule: true,
    ...(await importOriginal<typeof import('stores/cluster')>()),
    clusterStore: store,
  };
});

vi.mock('utils/wait', () => ({
  openCommand: () => null,
  openCommandResponse: mocks.openCommandResponse,
  openInteractiveTask: () => null,
  waitPageUrl: () => '',
}));

vi.mock('hew/CodeEditor', () => ({
  __esModule: true,
  default: ({ file }: { file: string | Loadable<string> }) => (
    <pre data-testid="code-editor">
      {typeof file === 'string' ? file : Loadable.getOrElse('', file)}
    </pre>
  ),
}));

const USER_ID = 1;

const WORKSPACE = {
  archived: false,
  id: 1,
  immutable: false,
  name: 'Uncategorized',
  numExperiments: 0,
  numProjects: 0,
  pinned: false,
  state: WorkspaceState.Unspecified,
  userId: USER_ID,
};

const task = (type: CommandType, id: string): CommandTask => ({
  id,
  name: `${type} task`,
  resourcePool: 'gpu',
  startTime: '2026-01-01T10:00:00Z',
  state: CommandState.Terminated,
  type,
  userId: USER_ID,
  workspaceId: WORKSPACE.id,
});

const SHELL = task(CommandType.Shell, 'shell-old');
const COMMAND = task(CommandType.Command, 'cmd-old');

const launchedShell = {
  command: { ...task(CommandType.Shell, 'shell-new'), name: 'Shell (lively-calm-fox)' },
  config: { description: 'Shell (lively-calm-fox)' },
  warnings: [],
};

/** The entry points: Launch Interactive Task and Launch Again. */
const Entries: React.FC = () => {
  const { launchFormModals, openLaunchForm } = useLaunchForm({
    onShellLaunched: mocks.onShellLaunched,
    workspace: WORKSPACE,
  });
  const { launchAgain, launchAgainModals } = useLaunchAgain({
    onLaunched: mocks.onShellLaunched,
    workspace: WORKSPACE,
  });

  useEffect(() => {
    authStore.setAuth({ isAuthenticated: true });
    authStore.setAuthChecked();
  }, []);

  return (
    <SettingsProvider>
      <>
        <Button onClick={() => openLaunchForm()}>Open Shell</Button>
        {[SHELL, COMMAND].map((item) => (
          <Button key={item.id} onClick={() => launchAgain(item)}>
            Again {item.id}
          </Button>
        ))}
        {launchFormModals}
        {launchAgainModals}
      </>
    </SettingsProvider>
  );
};

const setup = () => {
  const user = userEvent.setup();
  render(
    <BrowserRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <Entries />
        </ThemeProvider>
      </UIProvider>
    </BrowserRouter>,
  );
  return user;
};

const open = async (user: ReturnType<typeof userEvent.setup>, entry: string) => {
  await user.click(screen.getByRole('button', { name: entry }));
  await screen.findByText('Start from');
};

describe('useLaunchForm', () => {
  beforeEach(() => {
    window.localStorage.clear();
    mocks.getShellConfig.mockReset().mockResolvedValue({
      description: 'old shell',
      resources: { resource_pool: 'gpu', slots: 1 },
    });
    mocks.getTaskServices
      .mockReset()
      .mockResolvedValue({ services: [{ mayConnect: true, name: 'ssh' }] });
    mocks.launchShell.mockReset().mockResolvedValue(launchedShell);
    mocks.onShellLaunched.mockReset();
    mocks.openCommandResponse.mockReset();
    userStore.updateCurrentUser({ id: USER_ID, isActive: true, isAdmin: false, username: 'me' });
  });

  it('"Open Shell" opens the shell launch form and shows the shell’s launch result', async () => {
    const user = setup();
    await open(user, 'Open Shell');
    expect(screen.getByText('Launch Interactive Task')).toBeInTheDocument();
    expect(screen.queryByRole('radio')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Launch' }));

    expect(await screen.findByText('Interactive Task Launched')).toBeInTheDocument();
    expect(screen.getByText('det shell open shell-new')).toBeInTheDocument();
    expect(mocks.launchShell).toHaveBeenCalled();
    expect(mocks.openCommandResponse).not.toHaveBeenCalled();
    expect(mocks.onShellLaunched).toHaveBeenCalledTimes(1);
  });

  describe('the launched shell’s services', () => {
    const launch = async () => {
      const user = setup();
      await open(user, 'Open Shell');
      await user.click(screen.getByRole('button', { name: 'Launch' }));
      await screen.findByText('Interactive Task Launched');
      return user;
    };

    it('are read once per launch, not again when the dialog closes', async () => {
      const user = await launch();
      expect(mocks.getTaskServices).toHaveBeenCalledTimes(1);
      expect(mocks.getTaskServices).toHaveBeenCalledWith({ taskId: 'shell-new' });
      await user.click(screen.getByRole('button', { name: 'OK' }));
      await waitFor(() =>
        expect(screen.queryByText('Interactive Task Launched')).not.toBeInTheDocument(),
      );
      expect(mocks.getTaskServices).toHaveBeenCalledTimes(1);
    });

    it('give the dialog its Open JupyterLab and Open VS Code buttons', async () => {
      mocks.getTaskServices.mockResolvedValue({
        services: [
          { mayConnect: true, name: 'ssh' },
          { mayConnect: true, name: 'jupyter' },
          { mayConnect: true, name: 'vscode' },
        ],
      });
      await launch();
      expect(await screen.findByRole('button', { name: 'Open JupyterLab' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Open VS Code' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Open Terminal' })).toBeInTheDocument();
    });

    it('leave the dialog without service buttons when the read fails', async () => {
      mocks.getTaskServices.mockRejectedValueOnce(new Error('services unavailable'));
      await launch();
      await waitFor(() => expect(mocks.getTaskServices).toHaveBeenCalledTimes(1));
      expect(screen.getByRole('button', { name: 'Open Terminal' })).toBeInTheDocument();
      expect(screen.getByText('det shell open shell-new')).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Open JupyterLab' })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Open VS Code' })).not.toBeInTheDocument();
    });
  });

  describe('Launch Again', () => {
    it('loads a shell’s config and launches it again', async () => {
      const user = setup();
      await open(user, 'Again shell-old');
      expect(screen.getByText('Launch Interactive Task')).toBeInTheDocument();
      await waitFor(() =>
        expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue('old shell'),
      );
      expect(mocks.getShellConfig).toHaveBeenCalledWith({ commandId: 'shell-old' });
      await waitFor(() => expect(screen.getByRole('checkbox', { name: 'jupyter' })).toBeChecked());
      await user.click(screen.getByRole('button', { name: 'Launch' }));

      expect(await screen.findByText('Interactive Task Launched')).toBeInTheDocument();
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: {
          description: 'old shell',
          resources: { resource_pool: 'gpu', slots: 1 },
          services: { jupyter: {} },
        },
        workspaceId: WORKSPACE.id,
      });
      expect(mocks.onShellLaunched).toHaveBeenCalledTimes(1);
    });

    it.each(['Again cmd-old'])('"%s" opens nothing', async (entry) => {
      const user = setup();
      await user.click(screen.getByRole('button', { name: entry }));
      expect(screen.queryByText('Start from')).not.toBeInTheDocument();
      expect(mocks.getShellConfig).not.toHaveBeenCalled();
    });
  });
});
