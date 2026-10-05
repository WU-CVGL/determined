import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import Button from 'hew/Button';
import { useModal } from 'hew/Modal';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { Loadable, NotLoaded } from 'hew/utils/loadable';
import React, { useEffect } from 'react';
import { BrowserRouter } from 'react-router-dom';

import NtscLaunchModalComponent, { Props } from 'components/NtscLaunchModal';
import { ThemeProvider } from 'components/ThemeProvider';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import authStore from 'stores/auth';
import userStore from 'stores/users';
import userSettings from 'stores/userSettings';
import { CommandState, CommandTask, CommandType, RawJson, WorkspaceState } from 'types';
import { listLaunchHistory, recordLaunch } from 'utils/launchHistory';

const mocks = vi.hoisted(() => ({
  canCreateTemplateWorkspace: true,
  createTaskTemplate: vi.fn(),
  getJupyterLabConfig: vi.fn(),
  getJupyterLabs: vi.fn(),
  getShellConfig: vi.fn(),
  getShells: vi.fn(),
  getTaskTemplates: vi.fn(),
  getUserSetting: vi.fn(),
  launchJupyterLab: vi.fn(),
  launchShell: vi.fn(),
  makeToast: vi.fn(),
  openCommandResponse: vi.fn(),
  previewJupyterLab: vi.fn(),
  taskTemplatesOn: true,
  updateTaskTemplate: vi.fn(),
}));

vi.mock('services/api', () => ({
  createTaskTemplate: mocks.createTaskTemplate,
  getAvailableResourcePools: () => Promise.resolve([]),
  getCurrentUser: () => Promise.resolve({ id: 1, isActive: true, isAdmin: false, username: 'me' }),
  getJupyterLabConfig: mocks.getJupyterLabConfig,
  getJupyterLabs: mocks.getJupyterLabs,
  getResourcePools: () => Promise.resolve([]),
  getShellConfig: mocks.getShellConfig,
  getShells: mocks.getShells,
  getTaskTemplates: mocks.getTaskTemplates,
  getUsers: () => Promise.resolve({ users: [] }),
  getUserSetting: mocks.getUserSetting,
  getWorkspaces: () => Promise.resolve({ workspaces: [] }),
  launchJupyterLab: mocks.launchJupyterLab,
  launchShell: mocks.launchShell,
  previewJupyterLab: mocks.previewJupyterLab,
  updateTaskTemplate: mocks.updateTaskTemplate,
  updateTaskTemplateName: vi.fn(),
  updateUserSetting: () => Promise.resolve(),
}));

// Error toasts render outside the test's UIProvider; capture them instead.
vi.mock('hew/Toast', async (importOriginal) => ({
  ...(await importOriginal<typeof import('hew/Toast')>()),
  makeToast: mocks.makeToast,
}));

vi.mock('hooks/usePermissions', () => ({
  default: () => ({
    canCreateTemplateWorkspace: () => mocks.canCreateTemplateWorkspace,
    canCreateWorkspaceNSC: () => true,
  }),
}));

vi.mock('hooks/useFeature', () => ({
  default: () => ({
    isOn: (feature: string) => (feature === 'task_templates' ? mocks.taskTemplatesOn : false),
  }),
}));

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

const task = (type: CommandType, overrides: Partial<CommandTask> = {}): CommandTask => ({
  id: `${type}-old`,
  name: 'cluster task',
  resourcePool: 'gpu',
  startTime: '2026-01-01T10:00:00Z',
  state: CommandState.Terminated,
  type,
  userId: USER_ID,
  workspaceId: WORKSPACE.id,
  ...overrides,
});

/** A merged config as GET /api/v1/shells/{id} returns it. */
const clusterShellConfig = {
  bind_mounts: [{ container_path: '/data', host_path: '/mnt/data' }],
  description: 'old shell',
  entrypoint: ['/run/determined/ssh/shell-entrypoint.sh', '-p', '3201'],
  environment: { image: { cuda: 'custom:1' } },
  idle_timeout: null,
  resources: { priority: 42, resource_pool: 'gpu', slots: 2 },
};

const GPU_TEMPLATE = {
  config: { resources: { resource_pool: 'gpu', slots: 4 } },
  name: 'gpu-template',
  workspaceId: WORKSPACE.id,
};

/** What the master's notebook preview returns for a request config. */
const previewFor = (params: { config?: RawJson; templateName?: string }) => {
  const config = params.config ?? {};
  const resources = config.resources ?? {};
  const custom = config.description === 'tpl source';
  return Promise.resolve({
    description: config.description ?? 'JupyterLab (kindly-quick-heron)',
    entrypoint: null,
    environment: { image: { cuda: custom ? 'custom:2' : 'default:1' } },
    idle_timeout: '30m',
    notebook_idle_type: 'kernels_or_terminals',
    resources: {
      priority: 42,
      resource_pool: resources.resource_pool ?? 'default',
      slots: resources.slots ?? 1,
    },
  });
};

const launchedShell = {
  command: task(CommandType.Shell, {
    id: 'shell-new',
    name: 'Shell (lively-calm-fox)',
    state: CommandState.Queued,
  }),
  config: { description: 'Shell (lively-calm-fox)', resources: { resource_pool: 'gpu', slots: 1 } },
  warnings: [],
};

const ModalTrigger: React.FC<Props> = (props) => {
  const LaunchModal = useModal(NtscLaunchModalComponent);

  useEffect(() => {
    authStore.setAuth({ isAuthenticated: true });
    authStore.setAuthChecked();
  }, []);

  return (
    <SettingsProvider>
      <>
        <Button onClick={LaunchModal.open}>Open</Button>
        <LaunchModal.Component {...props} />
      </>
    </SettingsProvider>
  );
};

const setup = async (props: Partial<Props> = {}) => {
  const user = userEvent.setup();
  const onLaunched = vi.fn();
  render(
    <BrowserRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <ModalTrigger
            initialType={CommandType.Shell}
            workspace={WORKSPACE}
            onShellLaunched={onLaunched}
            {...props}
          />
        </ThemeProvider>
      </UIProvider>
    </BrowserRouter>,
  );
  await user.click(await screen.findByRole('button', { name: 'Open' }));
  await screen.findByText('Start from');
  return { onLaunched, user };
};

const openStartFrom = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(await screen.findByLabelText('Start from'));
};

const launchButton = () => screen.getByRole('button', { name: 'Launch' });

const typeRadio = (label: 'JupyterLab' | 'Shell') => screen.getByRole('radio', { name: label });

const selectType = async (
  user: ReturnType<typeof userEvent.setup>,
  label: 'JupyterLab' | 'Shell',
) => await user.click(typeRadio(label));

const launch = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(launchButton());
};

/** A promise that the test resolves by hand. */
function deferred<T>() {
  let resolve: (value: T) => void = () => undefined;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

describe('NtscLaunchModal', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.localStorage.clear();
    mocks.canCreateTemplateWorkspace = true;
    mocks.taskTemplatesOn = true;
    mocks.createTaskTemplate.mockReset().mockResolvedValue({});
    mocks.updateTaskTemplate.mockReset();
    mocks.getJupyterLabConfig.mockReset();
    mocks.getJupyterLabs.mockReset().mockResolvedValue([]);
    mocks.getShellConfig.mockReset().mockResolvedValue(clusterShellConfig);
    mocks.getShells.mockReset().mockResolvedValue([]);
    mocks.getTaskTemplates.mockReset().mockResolvedValue([GPU_TEMPLATE]);
    mocks.launchJupyterLab.mockReset();
    mocks.launchShell.mockReset().mockResolvedValue(launchedShell);
    mocks.makeToast.mockReset();
    mocks.getUserSetting.mockReset().mockResolvedValue({ settings: [] });
    mocks.openCommandResponse.mockReset();
    mocks.previewJupyterLab.mockReset().mockImplementation(previewFor);
    userStore.updateCurrentUser({ id: USER_ID, isActive: true, isAdmin: false, username: 'me' });
  });

  it.each([
    [CommandType.JupyterLab, 'Launch JupyterLab', 'JupyterLab', 'Shell'],
    [CommandType.Shell, 'Launch Shell', 'Shell', 'JupyterLab'],
  ] as const)(
    'renders the shared form for %s with its type selected',
    async (type, title, selected, other) => {
      await setup({ initialType: type });
      expect(screen.getByText(title)).toBeInTheDocument();
      expect(typeRadio(selected)).toBeChecked();
      expect(typeRadio(other)).not.toBeChecked();
      expect(screen.getByText('Start from')).toBeInTheDocument();
      expect(screen.getByPlaceholderText('Name (optional)')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Show Full Config' })).toBeInTheDocument();
    },
  );

  describe('shell', () => {
    it('launches from the simple form, reports the shell and records it in this browser', async () => {
      const { onLaunched, user } = await setup();
      await user.type(screen.getByPlaceholderText('Name (optional)'), 'debug box');
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalledWith(launchedShell));
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: { description: 'debug box', resources: { resource_pool: undefined, slots: 1 } },
        templateName: undefined,
        workspaceId: WORKSPACE.id,
      });
      expect(mocks.launchJupyterLab).not.toHaveBeenCalled();
      expect(mocks.openCommandResponse).not.toHaveBeenCalled();
      expect(listLaunchHistory(USER_ID, CommandType.Shell)).toHaveLength(1);
    });

    it('keeps the modal open and does not report a failed launch', async () => {
      mocks.launchShell.mockRejectedValue(new Error('invalid config'));
      const { onLaunched, user } = await setup();
      await launch(user);
      await waitFor(() =>
        expect(mocks.makeToast).toHaveBeenCalledWith(
          expect.objectContaining({ title: 'Could not submit form' }),
        ),
      );
      expect(onLaunched).not.toHaveBeenCalled();
      expect(screen.getByText('Launch Shell')).toBeInTheDocument();
    });

    it('previews the full config through the JupyterLab preview without notebook-only keys', async () => {
      const { onLaunched, user } = await setup();
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));

      await waitFor(() =>
        expect(screen.getByTestId('code-editor')).toHaveTextContent('resource_pool: default'),
      );
      expect(mocks.previewJupyterLab).toHaveBeenCalledWith(
        expect.objectContaining({ preview: true, workspaceId: WORKSPACE.id }),
      );
      const yamlText = screen.getByTestId('code-editor').textContent ?? '';
      expect(yamlText).not.toContain('idle_timeout');
      expect(yamlText).not.toContain('notebook_idle_type');
      expect(yamlText).not.toContain('JupyterLab (');
      expect(yamlText).not.toContain('entrypoint');

      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      const { config } = mocks.launchShell.mock.calls[0][0];
      expect(config).not.toHaveProperty('idle_timeout');
      expect(config.resources).toEqual({ priority: 42, resource_pool: 'default', slots: 1 });
    });

    it('lists recent tasks on the cluster, this browser’s history and templates', async () => {
      const now = vi.spyOn(Date, 'now');
      now.mockReturnValue(Date.parse('2026-01-01T09:00:00Z'));
      recordLaunch(USER_ID, CommandType.Shell, {
        config: { description: 'from browser', resources: { resource_pool: 'cpu', slots: 0 } },
        workspaceId: WORKSPACE.id,
      });
      now.mockReturnValue(Date.parse('2026-01-01T09:30:00Z'));
      recordLaunch(USER_ID, CommandType.JupyterLab, {
        config: { description: 'a notebook' },
        workspaceId: WORKSPACE.id,
      });
      now.mockRestore();
      mocks.getShells.mockResolvedValue([task(CommandType.Shell)]);
      mocks.getJupyterLabs.mockResolvedValue([
        task(CommandType.JupyterLab, { name: 'nb', startTime: '2026-01-01T11:00:00Z' }),
      ]);
      const { user } = await setup();

      // Both types, whichever type the form launches.
      const params = {
        limit: 20,
        orderBy: 'ORDER_BY_DESC',
        sortBy: 'SORT_BY_START_TIME',
        users: [String(USER_ID)],
        workspaceId: WORKSPACE.id,
      };
      await waitFor(() => expect(mocks.getShells).toHaveBeenCalledWith(params));
      expect(mocks.getJupyterLabs).toHaveBeenCalledWith(params);

      await openStartFrom(user);
      expect(await screen.findByText('Recent on cluster')).toBeInTheDocument();
      expect(screen.getByText('Recently launched in this browser')).toBeInTheDocument();
      expect(screen.getByText('Templates')).toBeInTheDocument();
      // Each item is labelled with its type, newest first.
      const recent = await screen.findByText(/^JupyterLab · nb · Terminated ·/);
      const recentShell = screen.getByText(/^Shell · cluster task · Terminated ·/);
      expect(
        recent.compareDocumentPosition(recentShell) & Node.DOCUMENT_POSITION_FOLLOWING,
      ).toBeTruthy();
      const browserNotebook = screen.getByText(/^JupyterLab · a notebook · default pool, 1 slot ·/);
      const browserShell = screen.getByText(/^Shell · from browser · cpu, 0 slots ·/);
      expect(
        browserNotebook.compareDocumentPosition(browserShell) & Node.DOCUMENT_POSITION_FOLLOWING,
      ).toBeTruthy();
      expect(screen.getByText('gpu-template')).toBeInTheDocument();
      // A config is only fetched once an item is picked.
      expect(mocks.getShellConfig).not.toHaveBeenCalled();
      expect(mocks.getJupyterLabConfig).not.toHaveBeenCalled();
    });

    it('still lists one type when the other fails to load', async () => {
      mocks.getShells.mockRejectedValue(new Error('503'));
      mocks.getJupyterLabs.mockResolvedValue([task(CommandType.JupyterLab, { name: 'nb' })]);
      const { user } = await setup();
      await openStartFrom(user);
      expect(await screen.findByText(/^JupyterLab · nb ·/)).toBeInTheDocument();
    });

    it('starts from a recent task: fetches its config, fills the form and launches the merged config', async () => {
      mocks.getShells.mockResolvedValue([task(CommandType.Shell)]);
      const { onLaunched, user } = await setup();
      await openStartFrom(user);
      await user.click(await screen.findByText(/^Shell · cluster task ·/));

      await waitFor(() =>
        expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue('old shell'),
      );
      expect(mocks.getShellConfig).toHaveBeenCalledWith({ commandId: 'shell-old' });

      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: {
          bind_mounts: clusterShellConfig.bind_mounts,
          description: 'old shell',
          environment: { image: { cuda: 'custom:1' } },
          idle_timeout: null,
          resources: { resource_pool: 'gpu', slots: 2 },
        },
        workspaceId: WORKSPACE.id,
      });
    });

    it('drops a recent task the master no longer knows', async () => {
      mocks.getShells.mockResolvedValue([task(CommandType.Shell)]);
      mocks.getShellConfig.mockRejectedValue(new Error('shell not found'));
      const { user } = await setup();
      await openStartFrom(user);
      await user.click(await screen.findByText(/^Shell · cluster task ·/));

      await waitFor(() =>
        expect(mocks.makeToast).toHaveBeenCalledWith(
          expect.objectContaining({
            severity: 'Warning',
            title: 'Unable to load the config of cluster task.',
          }),
        ),
      );
      expect(launchButton()).toBeEnabled();
      await openStartFrom(user);
      await waitFor(() =>
        expect(screen.queryByText(/^Shell · cluster task ·/)).not.toBeInTheDocument(),
      );
    });

    it('starts from this browser’s history', async () => {
      recordLaunch(USER_ID, CommandType.Shell, {
        config: {
          description: 'from browser',
          environment: { environment_variables: ['HF_TOKEN=abc', 'LANG=C.UTF-8'] },
          resources: { resource_pool: 'cpu', slots: 0 },
        },
        workspaceId: WORKSPACE.id,
      });
      const { onLaunched, user } = await setup();
      await openStartFrom(user);
      await user.click(await screen.findByText(/^Shell · from browser ·/));

      await waitFor(() =>
        expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue('from browser'),
      );
      expect(
        screen.getByText(/Environment variables that looked like credentials were not saved/),
      ).toBeInTheDocument();
      expect(screen.getByText(/HF_TOKEN/)).toBeInTheDocument();

      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: {
          description: 'from browser',
          environment: { environment_variables: ['LANG=C.UTF-8'] },
          resources: { resource_pool: 'cpu', slots: 0 },
        },
        workspaceId: WORKSPACE.id,
      });
    });

    it('starts from a template and copies its pool and slots into the form', async () => {
      const { onLaunched, user } = await setup();
      await openStartFrom(user);
      await user.click(await screen.findByText('gpu-template'));
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: { description: undefined, resources: { resource_pool: 'gpu', slots: 4 } },
        templateName: 'gpu-template',
        workspaceId: WORKSPACE.id,
      });
    });

    it('starts from the given task for "Launch Again"', async () => {
      const { onLaunched, user } = await setup({ initialTask: task(CommandType.Shell) });

      await waitFor(() =>
        expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue('old shell'),
      );
      expect(mocks.getShellConfig).toHaveBeenCalledWith({ commandId: 'shell-old' });
      expect(screen.getByText(/^Shell · cluster task ·/)).toBeInTheDocument();

      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell.mock.calls[0][0].config).toMatchObject({
        description: 'old shell',
        resources: { resource_pool: 'gpu', slots: 2 },
      });
      expect(mocks.launchShell.mock.calls[0][0].templateName).toBeUndefined();
    });

    it('still works when local storage throws', async () => {
      vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
        throw new DOMException('blocked', 'SecurityError');
      });
      vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
        throw new DOMException('full', 'QuotaExceededError');
      });
      const { onLaunched, user } = await setup();
      await openStartFrom(user);
      expect(await screen.findByText('gpu-template')).toBeInTheDocument();
      expect(screen.queryByText('Recently launched in this browser')).not.toBeInTheDocument();

      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalledWith(launchedShell));
    });
  });

  describe('while a recent task’s config loads', () => {
    /** Merged configs as the master returns them, each with its own image and resources. */
    const configA = {
      description: 'config a',
      environment: { image: { cuda: 'image-a:1' } },
      resources: { priority: 42, resource_pool: 'pool-a', slots: 1 },
    };
    const configB = {
      bind_mounts: [{ container_path: '/scratch', host_path: '/mnt/scratch' }],
      description: 'config b',
      environment: { image: { cuda: 'image-b:1' } },
      resources: { priority: 42, resource_pool: 'pool-b', slots: 4 },
    };
    const launchedConfigB = {
      bind_mounts: configB.bind_mounts,
      description: 'config b',
      environment: { image: { cuda: 'image-b:1' } },
      resources: { resource_pool: 'pool-b', slots: 4 },
    };
    const taskA = task(CommandType.Shell, { id: 'shell-a', name: 'task a' });
    const taskB = task(CommandType.Shell, { id: 'shell-b', name: 'task b' });

    const pick = async (user: ReturnType<typeof userEvent.setup>, label: RegExp) => {
      await openStartFrom(user);
      await user.click(await screen.findByText(label));
    };

    it.each([CommandType.Shell, CommandType.JupyterLab])(
      'keeps Launch and the full config disabled for "Launch Again" (%s) until the config arrives',
      async (type) => {
        const getConfig =
          type === CommandType.Shell ? mocks.getShellConfig : mocks.getJupyterLabConfig;
        const launchApi = type === CommandType.Shell ? mocks.launchShell : mocks.launchJupyterLab;
        mocks.launchJupyterLab.mockResolvedValue({
          command: task(CommandType.JupyterLab),
          config: {},
          warnings: [],
        });
        const config = deferred<RawJson>();
        getConfig.mockReturnValue(config.promise);
        const { user } = await setup({ initialTask: task(type), initialType: type });

        await waitFor(() => expect(getConfig).toHaveBeenCalledWith({ commandId: `${type}-old` }));
        // The task's type is selected.
        expect(typeRadio(type === CommandType.Shell ? 'Shell' : 'JupyterLab')).toBeChecked();
        expect(launchButton()).toBeDisabled();
        expect(screen.getByRole('button', { name: 'Show Full Config' })).toBeDisabled();

        config.resolve(configB);
        await waitFor(() => expect(launchButton()).toBeEnabled());
        expect(screen.getByRole('button', { name: 'Show Full Config' })).toBeEnabled();
        await launch(user);

        await waitFor(() => expect(launchApi).toHaveBeenCalled());
        expect(launchApi).toHaveBeenCalledWith({
          config: launchedConfigB,
          workspaceId: WORKSPACE.id,
        });
      },
    );

    it('waits for the second config after switching from a loaded task', async () => {
      mocks.getShells.mockResolvedValue([taskA, taskB]);
      const secondConfig = deferred<RawJson>();
      mocks.getShellConfig.mockImplementation(({ commandId }: { commandId: string }) =>
        commandId === taskA.id ? Promise.resolve(configA) : secondConfig.promise,
      );
      const { onLaunched, user } = await setup();

      await pick(user, /^Shell · task a ·/);
      await waitFor(() =>
        expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue('config a'),
      );
      expect(launchButton()).toBeEnabled();

      await pick(user, /^Shell · task b ·/);
      await waitFor(() =>
        expect(mocks.getShellConfig).toHaveBeenCalledWith({ commandId: taskB.id }),
      );
      expect(launchButton()).toBeDisabled();

      secondConfig.resolve(configB);
      await waitFor(() => expect(launchButton()).toBeEnabled());
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledTimes(1);
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: launchedConfigB,
        workspaceId: WORKSPACE.id,
      });
    });
  });

  it('restores the last template and slots without applying the template’s resources', async () => {
    mocks.getUserSetting.mockResolvedValue({
      settings: [
        { key: 'template', storagePath: 'shell-launch', value: JSON.stringify('gpu-template') },
        { key: 'slots', storagePath: 'shell-launch', value: JSON.stringify(2) },
      ],
    });
    const stopPolling = userSettings.startPolling();
    try {
      await waitFor(() => expect(Loadable.isLoaded(userSettings.getAll().get())).toBe(true));
      const { onLaunched, user } = await setup();
      expect(await screen.findByTitle('gpu-template')).toBeInTheDocument();
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: { description: undefined, resources: { resource_pool: undefined, slots: 2 } },
        templateName: 'gpu-template',
        workspaceId: WORKSPACE.id,
      });
    } finally {
      stopPolling();
      // Do not leak loaded settings into the other tests.
      userSettings._forUseSettingsOnly().set(NotLoaded);
    }
  });

  describe('JupyterLab', () => {
    it('keeps the existing launch: template name with the simple fields, then opens the notebook', async () => {
      mocks.launchJupyterLab.mockResolvedValue({
        command: task(CommandType.JupyterLab),
        warnings: [],
      });
      const { user } = await setup({ initialType: CommandType.JupyterLab });
      await openStartFrom(user);
      await user.click(await screen.findByText('gpu-template'));
      await launch(user);

      await waitFor(() => expect(mocks.openCommandResponse).toHaveBeenCalled());
      expect(mocks.launchJupyterLab).toHaveBeenCalledWith({
        config: { description: undefined, resources: { resource_pool: 'gpu', slots: 4 } },
        templateName: 'gpu-template',
        workspaceId: WORKSPACE.id,
      });
      expect(mocks.launchShell).not.toHaveBeenCalled();
    });

    it('starts from a recent notebook and previews its config as the base', async () => {
      mocks.getJupyterLabs.mockResolvedValue([task(CommandType.JupyterLab, { name: 'nb' })]);
      mocks.getJupyterLabConfig.mockResolvedValue({
        description: 'JupyterLab (kindly-quick-heron)',
        entrypoint: ['/run/determined/jupyter/notebook-entrypoint.sh'],
        environment: { image: { cuda: 'custom:1' } },
        idle_timeout: '1h',
        resources: { priority: 42, resource_pool: 'gpu', slots: 1 },
      });
      const { user } = await setup({ initialType: CommandType.JupyterLab });
      await openStartFrom(user);
      await user.click(await screen.findByText(/^JupyterLab · nb ·/));
      await waitFor(() =>
        expect(mocks.getJupyterLabConfig).toHaveBeenCalledWith({ commandId: 'jupyter-lab-old' }),
      );
      expect(mocks.getShellConfig).not.toHaveBeenCalled();

      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() =>
        expect(mocks.previewJupyterLab).toHaveBeenCalledWith({
          config: {
            environment: { image: { cuda: 'custom:1' } },
            idle_timeout: '1h',
            resources: { resource_pool: 'gpu', slots: 1 },
          },
          preview: true,
          templateName: undefined,
          workspaceId: WORKSPACE.id,
        }),
      );
    });
  });

  describe('task type', () => {
    const launchedNotebook = {
      command: task(CommandType.JupyterLab, { id: 'nb-new', state: CommandState.Queued }),
      config: { description: 'JupyterLab (kindly-quick-heron)' },
      warnings: [],
    };
    /** A notebook's merged config as GET /api/v1/notebooks/{id} returns it. */
    const clusterNotebookConfig = {
      description: 'JupyterLab (kindly-quick-heron)',
      entrypoint: ['/run/determined/jupyter/notebook-entrypoint.sh'],
      environment: { image: { cuda: 'custom:1' } },
      idle_timeout: '1h',
      resources: { priority: 42, resource_pool: 'gpu', slots: 1 },
    };
    const launchedNotebookConfig = {
      environment: { image: { cuda: 'custom:1' } },
      idle_timeout: '1h',
      resources: { resource_pool: 'gpu', slots: 1 },
    };

    beforeEach(() => {
      mocks.launchJupyterLab.mockResolvedValue(launchedNotebook);
      mocks.getJupyterLabConfig.mockResolvedValue(clusterNotebookConfig);
    });

    it('launches a JupyterLab and opens its wait page after switching from Shell', async () => {
      const { onLaunched, user } = await setup();
      await user.type(screen.getByPlaceholderText('Name (optional)'), 'my notebook');
      await selectType(user, 'JupyterLab');

      expect(screen.getByText('Launch JupyterLab')).toBeInTheDocument();
      expect(screen.queryByText('Launch Shell')).not.toBeInTheDocument();
      expect(typeRadio('JupyterLab')).toBeChecked();
      await launch(user);

      await waitFor(() => expect(mocks.openCommandResponse).toHaveBeenCalled());
      expect(mocks.launchJupyterLab).toHaveBeenCalledWith({
        config: { description: 'my notebook', resources: { resource_pool: undefined, slots: 1 } },
        templateName: undefined,
        workspaceId: WORKSPACE.id,
      });
      expect(mocks.launchShell).not.toHaveBeenCalled();
      expect(onLaunched).not.toHaveBeenCalled();
      expect(listLaunchHistory(USER_ID, CommandType.JupyterLab)).toHaveLength(1);
      expect(listLaunchHistory(USER_ID, CommandType.Shell)).toHaveLength(0);
    });

    it('launches a shell and reports it after switching from JupyterLab', async () => {
      const { onLaunched, user } = await setup({ initialType: CommandType.JupyterLab });
      await user.type(screen.getByPlaceholderText('Name (optional)'), 'my box');
      await selectType(user, 'Shell');

      expect(screen.getByText('Launch Shell')).toBeInTheDocument();
      expect(typeRadio('Shell')).toBeChecked();
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalledWith(launchedShell));
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: { description: 'my box', resources: { resource_pool: undefined, slots: 1 } },
        templateName: undefined,
        workspaceId: WORKSPACE.id,
      });
      expect(mocks.launchJupyterLab).not.toHaveBeenCalled();
      expect(mocks.openCommandResponse).not.toHaveBeenCalled();
      expect(listLaunchHistory(USER_ID, CommandType.Shell)).toHaveLength(1);
    });

    it('keeps the entered fields and the picked template when the type is switched', async () => {
      const { onLaunched, user } = await setup();
      await user.type(screen.getByPlaceholderText('Name (optional)'), 'kept');
      await openStartFrom(user);
      await user.click(await screen.findByText('gpu-template'));

      await selectType(user, 'JupyterLab');
      expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue('kept');
      await selectType(user, 'Shell');
      expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue('kept');
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: { description: 'kept', resources: { resource_pool: 'gpu', slots: 4 } },
        templateName: 'gpu-template',
        workspaceId: WORKSPACE.id,
      });
    });

    it('starts from a recent task of the other type without changing the selected type', async () => {
      mocks.getJupyterLabs.mockResolvedValue([task(CommandType.JupyterLab, { name: 'nb' })]);
      const { onLaunched, user } = await setup();
      await openStartFrom(user);
      await user.click(await screen.findByText(/^JupyterLab · nb ·/));

      // The config comes from the notebook's own API.
      await waitFor(() =>
        expect(mocks.getJupyterLabConfig).toHaveBeenCalledWith({ commandId: 'jupyter-lab-old' }),
      );
      expect(mocks.getShellConfig).not.toHaveBeenCalled();
      expect(screen.getByText('Launch Shell')).toBeInTheDocument();
      expect(typeRadio('Shell')).toBeChecked();
      await waitFor(() => expect(launchButton()).toBeEnabled());
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: launchedNotebookConfig,
        workspaceId: WORKSPACE.id,
      });
      expect(mocks.launchJupyterLab).not.toHaveBeenCalled();
    });

    it('starts from a browser history entry of the other type without changing the selected type', async () => {
      recordLaunch(USER_ID, CommandType.Shell, {
        config: {
          description: 'shell from browser',
          resources: { resource_pool: 'cpu', slots: 0 },
        },
        workspaceId: WORKSPACE.id,
      });
      const { user } = await setup({ initialType: CommandType.JupyterLab });
      await openStartFrom(user);
      await user.click(await screen.findByText(/^Shell · shell from browser ·/));
      await waitFor(() =>
        expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue('shell from browser'),
      );
      expect(typeRadio('JupyterLab')).toBeChecked();
      await launch(user);

      await waitFor(() => expect(mocks.openCommandResponse).toHaveBeenCalled());
      expect(mocks.launchJupyterLab).toHaveBeenCalledWith({
        config: {
          description: 'shell from browser',
          resources: { resource_pool: 'cpu', slots: 0 },
        },
        workspaceId: WORKSPACE.id,
      });
      expect(mocks.launchShell).not.toHaveBeenCalled();
    });

    it('clears the browser history of both types', async () => {
      recordLaunch(USER_ID, CommandType.Shell, {
        config: { description: 'a shell' },
        workspaceId: WORKSPACE.id,
      });
      recordLaunch(USER_ID, CommandType.JupyterLab, {
        config: { description: 'a notebook' },
        workspaceId: WORKSPACE.id,
      });
      const { user } = await setup();
      await user.click(screen.getByRole('button', { name: 'Clear browser history' }));
      expect(listLaunchHistory(USER_ID, CommandType.Shell)).toEqual([]);
      expect(listLaunchHistory(USER_ID, CommandType.JupyterLab)).toEqual([]);
      expect(
        screen.queryByRole('button', { name: 'Clear browser history' }),
      ).not.toBeInTheDocument();
    });

    it('keeps the full config when the type is switched and lets the master name the task after its type', async () => {
      const { onLaunched, user } = await setup({ initialType: CommandType.JupyterLab });
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() =>
        expect(screen.getByTestId('code-editor')).toHaveTextContent(
          'JupyterLab (kindly-quick-heron)',
        ),
      );
      const yamlText = screen.getByTestId('code-editor').textContent;
      expect(mocks.previewJupyterLab).toHaveBeenCalledTimes(1);

      await selectType(user, 'Shell');
      expect(screen.getByText('Launch Shell')).toBeInTheDocument();
      // Not previewed again: the YAML stays as it was.
      expect(mocks.previewJupyterLab).toHaveBeenCalledTimes(1);
      expect(screen.getByTestId('code-editor').textContent).toBe(yamlText);
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      const { config } = mocks.launchShell.mock.calls[0][0];
      // The JupyterLab preview's name is not used for the shell.
      expect(config).not.toHaveProperty('description');
      expect(config.environment).toEqual({ image: { cuda: 'default:1' } });
      expect(config.resources).toEqual({ priority: 42, resource_pool: 'default', slots: 1 });
      expect(mocks.launchJupyterLab).not.toHaveBeenCalled();
    });
  });

  describe('Save as Template', () => {
    it('opens a prefilled new template with only the non-default settings and creates it', async () => {
      const { user } = await setup({ initialType: CommandType.JupyterLab });
      await user.type(screen.getByPlaceholderText('Name (optional)'), 'tpl source');
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() => expect(screen.getByTestId('code-editor')).toHaveTextContent('custom:2'));

      await user.click(screen.getByRole('button', { name: 'Save as Template' }));
      expect(await screen.findByText('New Template')).toBeInTheDocument();
      expect(screen.getByText(/Other users can read templates/)).toBeInTheDocument();
      const editors = screen.getAllByTestId('code-editor');
      const draft = editors[editors.length - 1].textContent ?? '';
      expect(draft).toContain('custom:2');
      expect(draft).toContain('resource_pool: default');
      expect(draft).not.toContain('tpl source');
      expect(draft).not.toContain('idle_timeout');
      expect(draft).not.toContain('priority');

      await user.type(screen.getByLabelText('Name'), 'my-template');
      await user.click(screen.getByRole('button', { name: 'Create Template' }));
      await waitFor(() =>
        expect(mocks.createTaskTemplate).toHaveBeenCalledWith({
          config: {
            environment: { image: { cuda: 'custom:2' } },
            resources: { resource_pool: 'default', slots: 1 },
          },
          name: 'my-template',
          workspaceId: WORKSPACE.id,
        }),
      );
      expect(mocks.updateTaskTemplate).not.toHaveBeenCalled();
    });

    it('opens no template when the cluster defaults fail to load, and keeps a null work_dir on retry', async () => {
      // A task launched with work_dir: null, which clears the cluster default.
      mocks.getShellConfig.mockResolvedValue({ ...clusterShellConfig, work_dir: null });
      mocks.previewJupyterLab.mockImplementation(async (params: { config?: RawJson }) => ({
        ...(await previewFor(params)),
        work_dir:
          params.config && 'work_dir' in params.config ? params.config.work_dir : '/cluster/work',
      }));
      const { user } = await setup({ initialTask: task(CommandType.Shell) });
      await waitFor(() =>
        expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue('old shell'),
      );
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() =>
        expect(screen.getByTestId('code-editor')).toHaveTextContent('work_dir: null'),
      );
      const loadedConfig = screen.getByTestId('code-editor').textContent;

      mocks.previewJupyterLab.mockRejectedValueOnce(new Error('503 Service Unavailable'));
      await user.click(screen.getByRole('button', { name: 'Save as Template' }));
      await waitFor(() =>
        expect(mocks.makeToast).toHaveBeenCalledWith(
          expect.objectContaining({
            severity: 'Error',
            title: 'Unable to load the cluster defaults. Try Save as Template again.',
          }),
        ),
      );
      expect(screen.queryByText('New Template')).not.toBeInTheDocument();
      expect(screen.getByText('Launch Shell')).toBeInTheDocument();
      expect(screen.getAllByTestId('code-editor')).toHaveLength(1);
      expect(screen.getByTestId('code-editor').textContent).toBe(loadedConfig);

      await user.click(screen.getByRole('button', { name: 'Save as Template' }));
      expect(await screen.findByText('New Template')).toBeInTheDocument();
      const editors = screen.getAllByTestId('code-editor');
      const draft = editors[editors.length - 1].textContent ?? '';
      expect(draft).toContain('work_dir: null');
      expect(draft).not.toContain('/cluster/work');
    });

    it('is hidden without permission to create templates', async () => {
      mocks.canCreateTemplateWorkspace = false;
      const { user } = await setup();
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await screen.findByRole('button', { name: 'Show Simple Config' });
      expect(screen.queryByRole('button', { name: 'Save as Template' })).not.toBeInTheDocument();
    });

    it('is hidden when templates are turned off', async () => {
      mocks.taskTemplatesOn = false;
      const { user } = await setup();
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await screen.findByRole('button', { name: 'Show Simple Config' });
      expect(screen.queryByRole('button', { name: 'Save as Template' })).not.toBeInTheDocument();
    });
  });
});
