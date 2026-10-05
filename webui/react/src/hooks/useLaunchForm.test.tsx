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
import { CommandState, CommandTask, CommandType, WorkspaceState } from 'types';

const mocks = vi.hoisted(() => ({
  getJupyterLabConfig: vi.fn(),
  getShellConfig: vi.fn(),
  launchJupyterLab: vi.fn(),
  launchShell: vi.fn(),
  onShellLaunched: vi.fn(),
  openCommandResponse: vi.fn(),
}));

vi.mock('services/api', () => ({
  getAvailableResourcePools: () => Promise.resolve([]),
  getCurrentUser: () => Promise.resolve({ id: 1, isActive: true, isAdmin: false, username: 'me' }),
  getJupyterLabConfig: mocks.getJupyterLabConfig,
  getJupyterLabs: () => Promise.resolve([]),
  getResourcePools: () => Promise.resolve([]),
  getShellConfig: mocks.getShellConfig,
  getShells: () => Promise.resolve([]),
  getTaskTemplates: () => Promise.resolve([]),
  getUsers: () => Promise.resolve({ users: [] }),
  getUserSetting: () => Promise.resolve({ settings: [] }),
  getWorkspaces: () => Promise.resolve({ workspaces: [] }),
  launchJupyterLab: mocks.launchJupyterLab,
  launchShell: mocks.launchShell,
  previewJupyterLab: () => Promise.resolve({}),
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

const NOTEBOOK = task(CommandType.JupyterLab, 'nb-old');
const SHELL = task(CommandType.Shell, 'shell-old');
const COMMAND = task(CommandType.Command, 'cmd-old');

const launchedShell = {
  command: { ...task(CommandType.Shell, 'shell-new'), name: 'Shell (lively-calm-fox)' },
  config: { description: 'Shell (lively-calm-fox)' },
  warnings: [],
};

const launchedNotebook = {
  command: task(CommandType.JupyterLab, 'nb-new'),
  config: { description: 'JupyterLab (kindly-quick-heron)' },
  warnings: [],
};

/** The entry points: Launch JupyterLab, Launch Shell and Launch Again. */
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
        <Button onClick={() => openLaunchForm(CommandType.JupyterLab)}>Open JupyterLab</Button>
        <Button onClick={() => openLaunchForm(CommandType.Shell)}>Open Shell</Button>
        {[NOTEBOOK, SHELL, COMMAND].map((item) => (
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

const typeRadio = (label: 'JupyterLab' | 'Shell') => screen.getByRole('radio', { name: label });

describe('useLaunchForm', () => {
  beforeEach(() => {
    window.localStorage.clear();
    mocks.getJupyterLabConfig.mockReset().mockResolvedValue({
      description: 'old notebook',
      resources: { resource_pool: 'gpu', slots: 2 },
    });
    mocks.getShellConfig.mockReset().mockResolvedValue({
      description: 'old shell',
      resources: { resource_pool: 'gpu', slots: 1 },
    });
    mocks.launchJupyterLab.mockReset().mockResolvedValue(launchedNotebook);
    mocks.launchShell.mockReset().mockResolvedValue(launchedShell);
    mocks.onShellLaunched.mockReset();
    mocks.openCommandResponse.mockReset();
    userStore.updateCurrentUser({ id: USER_ID, isActive: true, isAdmin: false, username: 'me' });
  });

  it.each([
    ['Open JupyterLab', 'Launch JupyterLab', 'JupyterLab'],
    ['Open Shell', 'Launch Shell', 'Shell'],
  ] as const)('"%s" opens the form with its type selected', async (entry, title, selected) => {
    const user = setup();
    await open(user, entry);
    expect(screen.getByText(title)).toBeInTheDocument();
    expect(typeRadio(selected)).toBeChecked();
  });

  it('shows the shell’s launch result after switching a JupyterLab entry to Shell', async () => {
    const user = setup();
    await open(user, 'Open JupyterLab');
    await user.click(typeRadio('Shell'));
    await user.click(screen.getByRole('button', { name: 'Launch' }));

    expect(await screen.findByText('Shell Launched')).toBeInTheDocument();
    expect(screen.getByText('det shell open shell-new')).toBeInTheDocument();
    expect(mocks.launchShell).toHaveBeenCalled();
    expect(mocks.launchJupyterLab).not.toHaveBeenCalled();
    expect(mocks.openCommandResponse).not.toHaveBeenCalled();
    expect(mocks.onShellLaunched).toHaveBeenCalledTimes(1);
  });

  it('opens the notebook’s wait page after switching a Shell entry to JupyterLab', async () => {
    const user = setup();
    await open(user, 'Open Shell');
    await user.click(typeRadio('JupyterLab'));
    await user.click(screen.getByRole('button', { name: 'Launch' }));

    await waitFor(() => expect(mocks.openCommandResponse).toHaveBeenCalled());
    expect(mocks.launchJupyterLab).toHaveBeenCalled();
    expect(mocks.launchShell).not.toHaveBeenCalled();
    expect(mocks.onShellLaunched).not.toHaveBeenCalled();
    expect(screen.queryByText('Shell Launched')).not.toBeInTheDocument();
  });

  describe('Launch Again', () => {
    it.each([
      ['Again nb-old', 'Launch JupyterLab', 'JupyterLab', 'old notebook'],
      ['Again shell-old', 'Launch Shell', 'Shell', 'old shell'],
    ] as const)(
      '"%s" selects the task’s type and loads its config',
      async (entry, title, selected, name) => {
        const user = setup();
        await open(user, entry);
        expect(screen.getByText(title)).toBeInTheDocument();
        expect(typeRadio(selected)).toBeChecked();
        await waitFor(() =>
          expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue(name),
        );
        if (selected === 'Shell') {
          expect(mocks.getShellConfig).toHaveBeenCalledWith({ commandId: 'shell-old' });
          expect(mocks.getJupyterLabConfig).not.toHaveBeenCalled();
        } else {
          expect(mocks.getJupyterLabConfig).toHaveBeenCalledWith({ commandId: 'nb-old' });
          expect(mocks.getShellConfig).not.toHaveBeenCalled();
        }
      },
    );

    it('launches the notebook again as a shell when the type is switched', async () => {
      const user = setup();
      await open(user, 'Again nb-old');
      await waitFor(() =>
        expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue('old notebook'),
      );
      await user.click(typeRadio('Shell'));
      await user.click(screen.getByRole('button', { name: 'Launch' }));

      expect(await screen.findByText('Shell Launched')).toBeInTheDocument();
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: { description: 'old notebook', resources: { resource_pool: 'gpu', slots: 2 } },
        workspaceId: WORKSPACE.id,
      });
      expect(mocks.onShellLaunched).toHaveBeenCalledTimes(1);
    });

    it('opens nothing for other task types', async () => {
      const user = setup();
      await user.click(screen.getByRole('button', { name: 'Again cmd-old' }));
      expect(screen.queryByText('Start from')).not.toBeInTheDocument();
    });
  });
});
