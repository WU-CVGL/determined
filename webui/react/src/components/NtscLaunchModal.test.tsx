import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import Button from 'hew/Button';
import { useModal } from 'hew/Modal';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { Loadable, NotLoaded } from 'hew/utils/loadable';
import yaml from 'js-yaml';
import React, { useEffect } from 'react';
import { BrowserRouter } from 'react-router-dom';

import NtscLaunchModalComponent, { Props } from 'components/NtscLaunchModal';
import { ThemeProvider } from 'components/ThemeProvider';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import authStore from 'stores/auth';
import clusterStore from 'stores/cluster';
import userStore from 'stores/users';
import userSettings from 'stores/userSettings';
import { CommandState, CommandTask, CommandType, RawJson, WorkspaceState } from 'types';
import { listLaunchHistory, recordLaunch } from 'utils/launchHistory';
import { processApiError } from 'utils/service';

const mocks = vi.hoisted(() => ({
  availablePools: [] as string[],
  canCreateTemplateWorkspace: true,
  codeServer: true,
  createTaskTemplate: vi.fn(),
  getShellConfig: vi.fn(),
  getShells: vi.fn(),
  getTaskTemplates: vi.fn(),
  getUserSetting: vi.fn(),
  launchShell: vi.fn(),
  makeToast: vi.fn(),
  openCommandResponse: vi.fn(),
  pools: [] as unknown[],
  previewShell: vi.fn(),
  taskTemplatesOn: true,
  updateTaskTemplate: vi.fn(),
}));

vi.mock('services/api', () => ({
  createTaskTemplate: mocks.createTaskTemplate,
  getAvailableResourcePools: () => Promise.resolve(mocks.availablePools),
  getCurrentUser: () => Promise.resolve({ id: 1, isActive: true, isAdmin: false, username: 'me' }),
  getResourcePools: () => Promise.resolve(mocks.pools),
  getShellConfig: mocks.getShellConfig,
  getShells: mocks.getShells,
  getTaskTemplates: mocks.getTaskTemplates,
  getUsers: () => Promise.resolve({ users: [] }),
  getUserSetting: mocks.getUserSetting,
  getWorkspaces: () => Promise.resolve({ workspaces: [] }),
  launchShell: mocks.launchShell,
  previewShell: mocks.previewShell,
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
  openInteractiveTask: () => null,
  waitPageUrl: () => '',
}));

/**
 * An editable stand-in for the CodeMirror editor. Like the real one, it shows `file` (again
 * whenever `file` changes), keeps what is typed and reports each edit through onChange, which
 * Form.Item passes to it.
 */
vi.mock('hew/CodeEditor', async () => {
  const { useEffect, useState } = await import('react');
  const { Loadable } = await import('hew/utils/loadable');
  const CodeEditorStandIn = ({
    file,
    onChange,
  }: {
    file: string | Loadable<string>;
    onChange?: (fileContent: string) => void;
  }) => {
    const text = typeof file === 'string' ? file : Loadable.getOrElse('', file);
    const [value, setValue] = useState(text);
    useEffect(() => setValue(text), [text]);
    return (
      <textarea
        data-testid="code-editor"
        value={value}
        onChange={(e) => {
          setValue(e.target.value);
          onChange?.(e.target.value);
        }}
      />
    );
  };
  return { __esModule: true, default: CodeEditorStandIn };
});

/** The text of the last code editor shown: the launch form's, or a template draft's above it. */
const editorText = (): string =>
  (screen.getAllByTestId('code-editor').at(-1) as HTMLTextAreaElement).value;

/** Edits the full config's YAML as a user would, through the editor. */
const editConfig = (edit: (config: RawJson) => RawJson) => {
  const editor = screen.getByTestId('code-editor');
  const edited = yaml.dump(edit(yaml.load((editor as HTMLTextAreaElement).value) as RawJson));
  fireEvent.change(editor, { target: { value: edited } });
};

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

const NO_JUPYTER_TEMPLATE = {
  config: { services: { jupyter: { enabled: false } } },
  name: 'no-jupyter',
  workspaceId: WORKSPACE.id,
};

/** The services that the master runs by default: jupyter, and vscode when it serves code-server. */
const DEFAULT_SERVICES = { jupyter: {}, vscode: {} };

/**
 * What the master's shell preview returns for a request config: no description, entrypoint or
 * idle_timeout of its own, and the services it runs: its defaults, under the template's entries,
 * under the request's.
 */
const previewFor = (params: { config?: RawJson; templateName?: string }) => {
  const config = params.config ?? {};
  const resources = config.resources ?? {};
  const custom = config.description === 'tpl source';
  const template = [GPU_TEMPLATE, NO_JUPYTER_TEMPLATE].find(
    (item) => item.name === params.templateName,
  )?.config as RawJson | undefined;
  return Promise.resolve({
    ...(config.description === undefined ? {} : { description: config.description }),
    environment: { image: { cuda: custom ? 'custom:2' : 'default:1' } },
    ...(config.idle_timeout === undefined ? {} : { idle_timeout: config.idle_timeout }),
    resources: {
      priority: 42,
      resource_pool: resources.resource_pool ?? 'default',
      slots: resources.slots ?? 1,
    },
    services: {
      jupyter: {},
      ...(mocks.codeServer ? { vscode: {} } : {}),
      ...template?.services,
      ...config.services,
    },
  });
};

/** A refusal of the master as the services layer throws it, with the master's message. */
const masterRefusal = (status: number, message: string) =>
  processApiError(
    'previewShell',
    new Response(JSON.stringify({ error: { code: 3, error: message } }), { status }),
  );

/** The checkbox of a service by its config key, as the form labels it. */
const SERVICE_LABELS: Record<string, string> = { jupyter: 'JupyterLab', vscode: 'VS Code' };
const serviceBox = (name: string) => screen.getByRole('checkbox', { name: SERVICE_LABELS[name] });

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

/**
 * Renders and opens the form. Unless `settled` is false, waits for the master's preview of the
 * start-from to check jupyter, which the master runs by default.
 */
const setup = async (props: Partial<Props> = {}, { settled = true } = {}) => {
  const user = userEvent.setup();
  const onLaunched = vi.fn();
  render(
    <BrowserRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <ModalTrigger workspace={WORKSPACE} onShellLaunched={onLaunched} {...props} />
        </ThemeProvider>
      </UIProvider>
    </BrowserRouter>,
  );
  await user.click(await screen.findByRole('button', { name: 'Open' }));
  await screen.findByText('Start from');
  if (settled) await waitFor(() => expect(serviceBox('jupyter')).toBeChecked());
  return { onLaunched, user };
};

const openStartFrom = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(await screen.findByLabelText('Start from'));
};

const launchButton = () => screen.getByRole('button', { name: 'Launch' });

/** Clicks Launch once it is enabled, as a user does. */
const launch = async (user: ReturnType<typeof userEvent.setup>) => {
  await waitFor(() => expect(launchButton()).toBeEnabled());
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

/** getUserSetting's response for these per-user settings, by storage path. */
const userSettingsResponse = (byPath: Record<string, Record<string, unknown>>) => ({
  settings: Object.entries(byPath).flatMap(([storagePath, values]) =>
    Object.entries(values).map(([key, value]) => ({
      key,
      storagePath,
      value: JSON.stringify(value),
    })),
  ),
});

/** Runs the test body with the user settings loaded, as the app does after login. */
const withLoadedSettings = async (body: () => Promise<void>) => {
  const stopPolling = userSettings.startPolling();
  try {
    await waitFor(() => expect(Loadable.isLoaded(userSettings.getAll().get())).toBe(true));
    await body();
  } finally {
    stopPolling();
    // useSettings saves on a timer: let the last save (on launch or cancel)
    // land before the reset, so that it does not reach the next test's settings.
    await new Promise((resolve) => setTimeout(resolve, 0));
    // Do not leak loaded settings into the other tests.
    userSettings._forUseSettingsOnly().set(NotLoaded);
  }
};

/** The per-user settings stored under a storage path (last-used form values). */
const savedSettings = (storagePath: string) =>
  Loadable.getOrElse(undefined, userSettings.getAll().get())?.get(storagePath);

describe('NtscLaunchModal', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.localStorage.clear();
    mocks.canCreateTemplateWorkspace = true;
    mocks.codeServer = true;
    mocks.taskTemplatesOn = true;
    mocks.createTaskTemplate.mockReset().mockResolvedValue({});
    mocks.updateTaskTemplate.mockReset();
    mocks.getShellConfig.mockReset().mockResolvedValue(clusterShellConfig);
    mocks.getShells.mockReset().mockResolvedValue([]);
    mocks.getTaskTemplates.mockReset().mockResolvedValue([GPU_TEMPLATE]);
    mocks.launchShell.mockReset().mockResolvedValue(launchedShell);
    mocks.makeToast.mockReset();
    mocks.getUserSetting.mockReset().mockResolvedValue({ settings: [] });
    mocks.openCommandResponse.mockReset();
    mocks.previewShell.mockReset().mockImplementation(previewFor);
    userStore.updateCurrentUser({ id: USER_ID, isActive: true, isAdmin: false, username: 'me' });
  });

  it('renders the shell form, without a task type to choose', async () => {
    await setup();
    expect(screen.getByText('Launch Interactive Task')).toBeInTheDocument();
    expect(screen.queryByRole('radio')).not.toBeInTheDocument();
    // JupyterLab is a service of the shell, not a task type.
    expect(screen.getByText('JupyterLab').closest('label')).toBe(
      serviceBox('jupyter').closest('label'),
    );
    expect(screen.getByText('Start from')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Name (optional)')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Show Full Config' })).toBeInTheDocument();
  });

  describe('shell', () => {
    it('launches from the simple form, reports the shell and records it in this browser', async () => {
      const { onLaunched, user } = await setup();
      await user.type(screen.getByPlaceholderText('Name (optional)'), 'debug box');
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalledWith(launchedShell));
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: {
          description: 'debug box',
          resources: { resource_pool: undefined, slots: 1 },
          services: DEFAULT_SERVICES,
        },
        templateName: undefined,
        workspaceId: WORKSPACE.id,
      });
      expect(mocks.openCommandResponse).not.toHaveBeenCalled();
      expect(listLaunchHistory(USER_ID)).toHaveLength(1);
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
      expect(screen.getByText('Launch Interactive Task')).toBeInTheDocument();
    });

    it('previews the full config through the shell preview and launches the YAML', async () => {
      const { onLaunched, user } = await setup();
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));

      await waitFor(() => expect(editorText()).toContain('resource_pool: default'));
      // The start-from was previewed on open without services; the full config with the
      // checkboxes.
      expect(mocks.previewShell).toHaveBeenNthCalledWith(1, {
        config: { description: undefined, resources: { resource_pool: undefined, slots: 1 } },
        templateName: undefined,
        workspaceId: WORKSPACE.id,
      });
      expect(mocks.previewShell).toHaveBeenLastCalledWith({
        config: {
          description: undefined,
          resources: { resource_pool: undefined, slots: 1 },
          services: DEFAULT_SERVICES,
        },
        templateName: undefined,
        workspaceId: WORKSPACE.id,
      });
      const yamlText = editorText();
      expect(yamlText).toContain('jupyter: {}');
      // The master names the shell at launch; the preview has no name of its own.
      expect(yamlText).not.toContain('description');
      expect(yamlText).not.toContain('idle_timeout');

      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: {
          environment: { image: { cuda: 'default:1' } },
          resources: { priority: 42, resource_pool: 'default', slots: 1 },
          services: DEFAULT_SERVICES,
        },
        templateName: undefined,
        workspaceId: WORKSPACE.id,
      });
    });

    it('lists recent tasks on the cluster, this browser’s history and templates', async () => {
      const now = vi.spyOn(Date, 'now');
      now.mockReturnValue(Date.parse('2026-01-01T09:00:00Z'));
      recordLaunch(USER_ID, {
        config: { description: 'from browser', resources: { resource_pool: 'cpu', slots: 0 } },
        workspaceId: WORKSPACE.id,
      });
      now.mockRestore();
      mocks.getShells.mockResolvedValue([
        task(CommandType.Shell),
        task(CommandType.Shell, {
          id: 'shell-newer',
          name: 'newer',
          startTime: '2026-01-01T11:00:00Z',
        }),
      ]);
      const { user } = await setup();

      const params = {
        limit: 20,
        orderBy: 'ORDER_BY_DESC',
        sortBy: 'SORT_BY_START_TIME',
        users: [String(USER_ID)],
        workspaceId: WORKSPACE.id,
      };
      await waitFor(() => expect(mocks.getShells).toHaveBeenCalledWith(params));

      await openStartFrom(user);
      expect(await screen.findByText('Recent on cluster')).toBeInTheDocument();
      expect(screen.getByText('Recently launched in this browser')).toBeInTheDocument();
      expect(screen.getByText('Templates')).toBeInTheDocument();
      // Newest first.
      const recent = await screen.findByText(/^Shell · newer · Terminated ·/);
      const older = screen.getByText(/^Shell · cluster task · Terminated ·/);
      expect(recent.compareDocumentPosition(older) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
      expect(screen.getByText(/^Shell · from browser · cpu, 0 slots ·/)).toBeInTheDocument();
      expect(screen.getByText('gpu-template')).toBeInTheDocument();
      // A config is only fetched once an item is picked.
      expect(mocks.getShellConfig).not.toHaveBeenCalled();
    });

    it('still lists this browser’s history and the templates when the recent shells fail to load', async () => {
      recordLaunch(USER_ID, { config: { description: 'from browser' }, workspaceId: WORKSPACE.id });
      mocks.getShells.mockRejectedValue(new Error('503'));
      const { user } = await setup();
      await openStartFrom(user);
      expect(await screen.findByText(/^Shell · from browser ·/)).toBeInTheDocument();
      expect(screen.getByText('gpu-template')).toBeInTheDocument();
      expect(screen.queryByText('Recent on cluster')).not.toBeInTheDocument();
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
          services: DEFAULT_SERVICES,
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
      recordLaunch(USER_ID, {
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
          services: DEFAULT_SERVICES,
        },
        workspaceId: WORKSPACE.id,
      });
    });

    it('picks the first pool that the user may use and lists the others disabled', async () => {
      const pool = { auxContainerCapacityPerAgent: 1, slotsAvailable: 0, slotsPerAgent: 0 };
      mocks.pools = [
        { ...pool, name: 'a-locked', usable: false },
        { ...pool, name: 'b-open', usable: true },
      ];
      mocks.availablePools = ['a-locked', 'b-open'];
      clusterStore.fetchResourcePools();
      await waitFor(() =>
        expect(Loadable.getOrElse([], clusterStore.resourcePools.get())).toHaveLength(2),
      );
      try {
        const { user } = await setup();
        const picker = screen.getByRole('combobox', { name: 'Resource Pool' });
        await waitFor(() => expect(picker.closest('.ant-select')).toHaveTextContent('b-open'));

        await user.click(picker);
        const locked = (
          await screen.findByText('a-locked', { selector: '.ant-select-item-option-content *' })
        ).closest('.ant-select-item-option');
        expect(locked).toHaveClass('ant-select-item-option-disabled');
        expect(locked).toHaveTextContent('Restricted');
      } finally {
        mocks.pools = [];
        mocks.availablePools = [];
        clusterStore.fetchResourcePools();
        await waitFor(() =>
          expect(Loadable.getOrElse([], clusterStore.resourcePools.get())).toHaveLength(0),
        );
      }
    });

    it('starts from a template and copies its pool and slots into the form', async () => {
      const { onLaunched, user } = await setup();
      await openStartFrom(user);
      await user.click(await screen.findByText('gpu-template'));
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: {
          description: undefined,
          resources: { resource_pool: 'gpu', slots: 4 },
          services: DEFAULT_SERVICES,
        },
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
      services: DEFAULT_SERVICES,
    };
    const taskA = task(CommandType.Shell, { id: 'shell-a', name: 'task a' });
    const taskB = task(CommandType.Shell, { id: 'shell-b', name: 'task b' });

    const pick = async (user: ReturnType<typeof userEvent.setup>, label: RegExp) => {
      await openStartFrom(user);
      await user.click(await screen.findByText(label));
    };

    it('keeps Launch and the full config disabled for "Launch Again" until the config arrives', async () => {
      const config = deferred<RawJson>();
      mocks.getShellConfig.mockReturnValue(config.promise);
      const { user } = await setup({ initialTask: task(CommandType.Shell) }, { settled: false });

      await waitFor(() =>
        expect(mocks.getShellConfig).toHaveBeenCalledWith({ commandId: 'shell-old' }),
      );
      expect(launchButton()).toBeDisabled();
      expect(screen.getByRole('button', { name: 'Show Full Config' })).toBeDisabled();

      config.resolve(configB);
      await waitFor(() => expect(launchButton()).toBeEnabled());
      expect(screen.getByRole('button', { name: 'Show Full Config' })).toBeEnabled();
      await launch(user);

      await waitFor(() => expect(mocks.launchShell).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: launchedConfigB,
        workspaceId: WORKSPACE.id,
      });
    });

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
      await waitFor(() => expect(launchButton()).toBeEnabled());

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
    mocks.getUserSetting.mockResolvedValue(
      userSettingsResponse({ 'shell-launch': { slots: 2, template: 'gpu-template' } }),
    );
    await withLoadedSettings(async () => {
      const { onLaunched, user } = await setup();
      expect(await screen.findByTitle('gpu-template')).toBeInTheDocument();
      // The services of the restored template, previewed once it is selected.
      await waitFor(() =>
        expect(mocks.previewShell).toHaveBeenLastCalledWith(
          expect.objectContaining({ templateName: 'gpu-template' }),
        ),
      );
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: {
          description: undefined,
          resources: { resource_pool: undefined, slots: 2 },
          services: DEFAULT_SERVICES,
        },
        templateName: 'gpu-template',
        workspaceId: WORKSPACE.id,
      });
    });
  });

  describe('the selected Start from item', () => {
    /** The open dropdown's rows (group headers and items), top to bottom. */
    const startFromRows = () =>
      Array.from(
        document.querySelectorAll(
          '.ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item',
        ),
        (row) => row.textContent ?? '',
      );

    it('is listed first, above the recent tasks, when it is the last template of many', async () => {
      // More templates than the dropdown shows at once (8 rows); the last one was used last.
      mocks.getTaskTemplates.mockResolvedValue(
        Array.from({ length: 12 }, (_, index) => ({
          config: {},
          name: `template-${index + 1}`,
          workspaceId: WORKSPACE.id,
        })),
      );
      mocks.getShells.mockResolvedValue([task(CommandType.Shell)]);
      mocks.getUserSetting.mockResolvedValue(
        userSettingsResponse({ 'shell-launch': { template: 'template-12' } }),
      );
      await withLoadedSettings(async () => {
        const { onLaunched, user } = await setup();
        expect(await screen.findByTitle('template-12')).toBeInTheDocument();
        await openStartFrom(user);
        await screen.findByText('Recent on cluster');

        const rows = startFromRows();
        // The selected row comes first, so the list opens at the top.
        expect(rows.slice(0, 2)).toEqual(['Selected', 'template-12']);
        expect(rows.indexOf('Recent on cluster')).toBeGreaterThan(1);
        expect(rows.indexOf('Recent on cluster')).toBeLessThan(rows.indexOf('Templates'));
        // The Templates group follows from its first template. Only the first
        // rows of the virtual list render, so template-12's place at the end of
        // that group is checked by the short-list case below.
        expect(rows[rows.indexOf('Templates') + 1]).toBe('template-1');

        // The value is unchanged: the launch still names the template.
        await user.keyboard('{Escape}');
        await launch(user);
        await waitFor(() => expect(onLaunched).toHaveBeenCalled());
        expect(mocks.launchShell).toHaveBeenCalledWith(
          expect.objectContaining({ templateName: 'template-12' }),
        );
      });
    });

    it('is left out of the Templates group, which lists the other templates', async () => {
      // Few enough rows that the whole list renders.
      mocks.getTaskTemplates.mockResolvedValue(
        ['template-1', 'template-2', 'template-3'].map((name) => ({
          config: {},
          name,
          workspaceId: WORKSPACE.id,
        })),
      );
      mocks.getShells.mockResolvedValue([task(CommandType.Shell)]);
      mocks.getUserSetting.mockResolvedValue(
        userSettingsResponse({ 'shell-launch': { template: 'template-2' } }),
      );
      await withLoadedSettings(async () => {
        const { user } = await setup();
        expect(await screen.findByTitle('template-2')).toBeInTheDocument();
        await openStartFrom(user);
        await screen.findByText('Recent on cluster');

        expect(startFromRows()).toEqual([
          'Selected',
          'template-2',
          'Recent on cluster',
          expect.stringMatching(/^Shell · /),
          'Templates',
          'template-1',
          'template-3',
        ]);
      });
    });

    it('is listed first when it is a recent task, the others stay in their groups', async () => {
      mocks.getShells.mockResolvedValue([
        task(CommandType.Shell, { id: 'shell-newer', name: 'newer' }),
        task(CommandType.Shell, { name: 'older', startTime: '2026-01-01T08:00:00Z' }),
      ]);
      const { user } = await setup();
      await openStartFrom(user);
      await user.click(await screen.findByText(/^Shell · older ·/));
      await waitFor(() =>
        expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue('old shell'),
      );

      await openStartFrom(user);
      await screen.findByText('Selected');
      const rows = startFromRows();
      expect(rows[0]).toBe('Selected');
      expect(rows[1]).toMatch(/^Shell · older ·/);
      expect(rows[2]).toBe('Recent on cluster');
      expect(rows[3]).toMatch(/^Shell · newer ·/);
      expect(rows.filter((row) => row.startsWith('Shell · older'))).toHaveLength(1);
      expect(rows.slice(4)).toEqual(['Templates', 'gpu-template']);
    });
  });

  describe('last-used values', () => {
    const SEEDED = {
      'shell-launch': { name: 'shell-name', slots: 2 },
    };

    beforeEach(() => {
      mocks.getUserSetting.mockResolvedValue(userSettingsResponse(SEEDED));
    });

    it('opens with the last shell values and saves them on launch', async () => {
      await withLoadedSettings(async () => {
        const { user } = await setup();
        const name = screen.getByPlaceholderText('Name (optional)');
        expect(name).toHaveValue('shell-name');

        await user.clear(name);
        await user.type(name, 'launched');
        await launch(user);

        await waitFor(() =>
          expect(savedSettings('shell-launch')).toEqual({
            name: 'launched',
            slots: 2,
            workspaceId: WORKSPACE.id,
          }),
        );
      });
    });

    it('saves the values on Cancel', async () => {
      await withLoadedSettings(async () => {
        const { user } = await setup();
        const name = screen.getByPlaceholderText('Name (optional)');
        await user.clear(name);
        await user.type(name, 'draft');
        await user.click(screen.getByRole('button', { name: 'Cancel' }));

        await waitFor(() =>
          expect(savedSettings('shell-launch')).toEqual({
            name: 'draft',
            slots: 2,
            workspaceId: WORKSPACE.id,
          }),
        );
        expect(mocks.launchShell).not.toHaveBeenCalled();
      });
    });

    it('ignores the services of older saved values: the checkboxes follow the master', async () => {
      mocks.getUserSetting.mockResolvedValue(
        userSettingsResponse({
          'shell-launch': { ...SEEDED['shell-launch'], services: ['vscode'] },
        }),
      );
      await withLoadedSettings(async () => {
        const { user } = await setup();
        expect(serviceBox('vscode')).toBeChecked();
        expect(serviceBox('jupyter')).toBeChecked();

        await user.click(serviceBox('jupyter'));
        await launch(user);

        await waitFor(() =>
          expect(savedSettings('shell-launch')).toMatchObject({ name: 'shell-name', slots: 2 }),
        );
        expect(mocks.launchShell.mock.calls[0][0].config.services).toEqual({
          jupyter: { enabled: false },
          vscode: {},
        });
      });
    });
  });

  describe('services', () => {
    it('checks the services the master runs by default: jupyter, and vscode with code-server', async () => {
      await setup();
      const services = screen.getByTitle('Services').closest('.ant-form-item') as HTMLElement;
      expect(
        within(services)
          .getAllByRole('checkbox')
          .map((box) => box.closest('label')?.textContent),
      ).toEqual(['JupyterLab', 'VS Code']);
      expect(screen.getAllByRole('checkbox')).toHaveLength(2);
      expect(serviceBox('jupyter')).toBeChecked();
      expect(serviceBox('vscode')).toBeChecked();
      // The start-from is previewed without services, so that the master fills its defaults.
      expect(mocks.previewShell).toHaveBeenCalledTimes(1);
      expect(mocks.previewShell).toHaveBeenCalledWith({
        config: { description: undefined, resources: { resource_pool: undefined, slots: 1 } },
        templateName: undefined,
        workspaceId: WORKSPACE.id,
      });
    });

    it('leaves vscode unchecked and out of the request without code-server on the master', async () => {
      mocks.codeServer = false;
      const { onLaunched, user } = await setup();
      expect(serviceBox('jupyter')).toBeChecked();
      expect(serviceBox('vscode')).not.toBeChecked();

      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell.mock.calls[0][0].config.services).toEqual({ jupyter: {} });
    });

    it('keeps a box checked while the preview on open is on its way', async () => {
      mocks.codeServer = false;
      const pending = deferred<void>();
      mocks.previewShell.mockImplementationOnce((params: { config?: RawJson }) =>
        pending.promise.then(() => previewFor(params)),
      );
      const { onLaunched, user } = await setup({}, { settled: false });
      await waitFor(() => expect(mocks.previewShell).toHaveBeenCalledTimes(1));
      await user.click(serviceBox('vscode'));
      pending.resolve();

      await waitFor(() => expect(serviceBox('jupyter')).toBeChecked());
      expect(serviceBox('vscode')).toBeChecked();
      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell.mock.calls[0][0].config.services).toEqual({
        jupyter: {},
        vscode: {},
      });
    });

    it('keeps a box unchecked while the preview of a picked config is on its way', async () => {
      mocks.getShellConfig.mockResolvedValue({ ...clusterShellConfig, services: { jupyter: {} } });
      const pending = deferred<void>();
      mocks.previewShell.mockImplementationOnce((params: { config?: RawJson }) =>
        pending.promise.then(() => previewFor(params)),
      );
      const { onLaunched, user } = await setup(
        { initialTask: task(CommandType.Shell) },
        { settled: false },
      );
      await waitFor(() => expect(serviceBox('jupyter')).toBeChecked());
      await waitFor(() => expect(mocks.previewShell).toHaveBeenCalledTimes(1));
      await user.click(serviceBox('jupyter'));
      pending.resolve();

      await waitFor(() => expect(serviceBox('vscode')).toBeChecked());
      expect(serviceBox('jupyter')).not.toBeChecked();
      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell.mock.calls[0][0].config.services).toEqual({
        jupyter: { enabled: false },
        vscode: {},
      });
    });

    it('previews and launches exactly the same request, with an unchecked service disabled', async () => {
      const { onLaunched, user } = await setup();
      await user.type(screen.getByPlaceholderText('Name (optional)'), 'lab');
      await user.click(serviceBox('jupyter'));
      expect(serviceBox('jupyter')).not.toBeChecked();
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));

      await waitFor(() => expect(editorText()).toContain('enabled: false'));
      const request = {
        config: {
          description: 'lab',
          resources: { resource_pool: undefined, slots: 1 },
          services: { jupyter: { enabled: false }, vscode: {} },
        },
        templateName: undefined,
        workspaceId: WORKSPACE.id,
      };
      expect(mocks.previewShell).toHaveBeenLastCalledWith(request);
      expect(yaml.load(editorText())).toMatchObject({
        services: { jupyter: { enabled: false }, vscode: {} },
      });

      await user.click(screen.getByRole('button', { name: 'Show Simple Config' }));
      expect(serviceBox('jupyter')).not.toBeChecked();
      expect(serviceBox('vscode')).toBeChecked();
      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith(request);
      expect(mocks.launchShell.mock.calls[0][0]).toEqual(mocks.previewShell.mock.lastCall?.[0]);
    });

    it('launches the services of the full config’s YAML, where enabled: false round-trips', async () => {
      const { onLaunched, user } = await setup();
      await user.click(serviceBox('vscode'));
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() => expect(editorText()).toContain('enabled: false'));
      expect(yaml.load(editorText())).toMatchObject({
        services: { jupyter: {}, vscode: { enabled: false } },
      });
      editConfig((config) => ({
        ...config,
        services: { jupyter: { enabled: false }, vscode: {} },
      }));
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell.mock.calls[0][0].config.services).toEqual({
        jupyter: { enabled: false },
        vscode: {},
      });
    });

    it('checks the services of a recent shell as the master runs them, and sends an unchecked one disabled', async () => {
      mocks.getShells.mockResolvedValue([task(CommandType.Shell)]);
      mocks.getShellConfig.mockResolvedValue({
        ...clusterShellConfig,
        services: { jupyter: { enabled: false }, vscode: {} },
      });
      const { onLaunched, user } = await setup();
      await openStartFrom(user);
      await user.click(await screen.findByText(/^Shell · cluster task ·/));
      await waitFor(() => expect(serviceBox('jupyter')).not.toBeChecked());
      expect(serviceBox('vscode')).toBeChecked();
      // The picked config is previewed with its services as they are.
      expect(mocks.previewShell).toHaveBeenLastCalledWith(
        expect.objectContaining({
          config: expect.objectContaining({
            services: { jupyter: { enabled: false }, vscode: {} },
          }),
        }),
      );

      await user.click(serviceBox('vscode'));
      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell.mock.calls[0][0].config.services).toEqual({
        jupyter: { enabled: false },
        vscode: { enabled: false },
      });
    });

    it('follows a template that turns a service off, and sends the checkboxes with the template', async () => {
      mocks.getTaskTemplates.mockResolvedValue([GPU_TEMPLATE, NO_JUPYTER_TEMPLATE]);
      const { onLaunched, user } = await setup();
      await openStartFrom(user);
      await user.click(await screen.findByText('no-jupyter'));
      await waitFor(() => expect(serviceBox('vscode')).toBeChecked());
      expect(serviceBox('jupyter')).not.toBeChecked();

      await user.click(serviceBox('jupyter'));
      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: {
          description: undefined,
          resources: { resource_pool: undefined, slots: 1 },
          services: DEFAULT_SERVICES,
        },
        templateName: 'no-jupyter',
        workspaceId: WORKSPACE.id,
      });
    });

    it('waits for the preview of a newly picked template before Launch and the full config', async () => {
      mocks.getTaskTemplates.mockResolvedValue([GPU_TEMPLATE, NO_JUPYTER_TEMPLATE]);
      const pending = deferred<void>();
      mocks.previewShell.mockImplementation(
        (params: { config?: RawJson; templateName?: string }) =>
          params.templateName === 'no-jupyter'
            ? pending.promise.then(() => previewFor(params))
            : previewFor(params),
      );
      const { onLaunched, user } = await setup();
      expect(serviceBox('jupyter')).toBeChecked();
      await openStartFrom(user);
      await user.click(await screen.findByText('no-jupyter'));
      await waitFor(() => expect(mocks.previewShell).toHaveBeenCalledTimes(2));

      // The checkboxes of the defaults are cleared, and nothing is sent with them.
      expect(serviceBox('jupyter')).not.toBeChecked();
      expect(serviceBox('vscode')).not.toBeChecked();
      expect(launchButton()).toBeDisabled();
      const fullConfigButton = screen.getByRole('button', { name: 'Show Full Config' });
      expect(fullConfigButton).toBeDisabled();
      fireEvent.click(launchButton());
      fireEvent.click(fullConfigButton);
      expect(mocks.launchShell).not.toHaveBeenCalled();
      expect(mocks.previewShell).toHaveBeenCalledTimes(2);
      expect(mocks.previewShell.mock.lastCall?.[0]).toEqual({
        config: { description: undefined, resources: { resource_pool: undefined, slots: 1 } },
        templateName: 'no-jupyter',
        workspaceId: WORKSPACE.id,
      });

      pending.resolve();
      await waitFor(() => expect(launchButton()).toBeEnabled());
      expect(serviceBox('jupyter')).not.toBeChecked();
      expect(serviceBox('vscode')).toBeChecked();
      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledTimes(1);
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: {
          description: undefined,
          resources: { resource_pool: undefined, slots: 1 },
          services: { jupyter: { enabled: false }, vscode: {} },
        },
        templateName: 'no-jupyter',
        workspaceId: WORKSPACE.id,
      });
    });

    it('waits for the preview on its way after switching back to a template, and drops a late answer', async () => {
      mocks.getTaskTemplates.mockResolvedValue([GPU_TEMPLATE, NO_JUPYTER_TEMPLATE]);
      const noJupyter = deferred<void>();
      const gpuAgain = deferred<void>();
      let gpuPreviews = 0;
      mocks.previewShell.mockImplementation(
        (params: { config?: RawJson; templateName?: string }) => {
          if (params.templateName === 'no-jupyter') {
            return noJupyter.promise.then(() => previewFor(params));
          }
          if (params.templateName === 'gpu-template' && ++gpuPreviews > 1) {
            return gpuAgain.promise.then(() => previewFor(params));
          }
          return previewFor(params);
        },
      );
      const { onLaunched, user } = await setup();
      await openStartFrom(user);
      await user.click(await screen.findByText('gpu-template'));
      await waitFor(() => expect(gpuPreviews).toBe(1));
      await waitFor(() => expect(launchButton()).toBeEnabled());

      await openStartFrom(user);
      await user.click(await screen.findByText('no-jupyter'));
      await waitFor(() => expect(launchButton()).toBeDisabled());
      await openStartFrom(user);
      await user.click(await screen.findByText('gpu-template'));
      await waitFor(() => expect(gpuPreviews).toBe(2));
      const previews = mocks.previewShell.mock.calls.length;

      // The template's earlier answer does not settle the preview on its way.
      expect(launchButton()).toBeDisabled();
      const fullConfigButton = screen.getByRole('button', { name: 'Show Full Config' });
      expect(fullConfigButton).toBeDisabled();
      fireEvent.click(launchButton());
      fireEvent.click(fullConfigButton);
      expect(mocks.launchShell).not.toHaveBeenCalled();
      expect(mocks.previewShell).toHaveBeenCalledTimes(previews);

      // no-jupyter's late answer is dropped.
      noJupyter.resolve();
      await noJupyter.promise;
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(launchButton()).toBeDisabled();
      expect(serviceBox('jupyter')).not.toBeChecked();
      expect(serviceBox('vscode')).not.toBeChecked();

      gpuAgain.resolve();
      await waitFor(() => expect(serviceBox('jupyter')).toBeChecked());
      expect(serviceBox('vscode')).toBeChecked();
      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledTimes(1);
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: {
          description: undefined,
          resources: { resource_pool: 'gpu', slots: 4 },
          services: DEFAULT_SERVICES,
        },
        templateName: 'gpu-template',
        workspaceId: WORKSPACE.id,
      });
    });

    it('drops the late answer of the preview on open after a template was picked', async () => {
      mocks.getTaskTemplates.mockResolvedValue([GPU_TEMPLATE, NO_JUPYTER_TEMPLATE]);
      const onOpen = deferred<void>();
      mocks.previewShell.mockImplementationOnce((params: { config?: RawJson }) =>
        onOpen.promise.then(() => previewFor(params)),
      );
      const { onLaunched, user } = await setup({}, { settled: false });
      await waitFor(() => expect(mocks.previewShell).toHaveBeenCalledTimes(1));
      await openStartFrom(user);
      await user.click(await screen.findByText('no-jupyter'));
      await waitFor(() => expect(serviceBox('vscode')).toBeChecked());
      expect(serviceBox('jupyter')).not.toBeChecked();

      onOpen.resolve();
      await onOpen.promise;
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(serviceBox('jupyter')).not.toBeChecked();
      expect(serviceBox('vscode')).toBeChecked();
      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: {
          description: undefined,
          resources: { resource_pool: undefined, slots: 1 },
          services: { jupyter: { enabled: false }, vscode: {} },
        },
        templateName: 'no-jupyter',
        workspaceId: WORKSPACE.id,
      });
    });

    it('keeps a box checked while the preview of a picked template is on its way', async () => {
      mocks.getTaskTemplates.mockResolvedValue([GPU_TEMPLATE, NO_JUPYTER_TEMPLATE]);
      const pending = deferred<void>();
      mocks.previewShell.mockImplementation(
        (params: { config?: RawJson; templateName?: string }) =>
          params.templateName === 'no-jupyter'
            ? pending.promise.then(() => previewFor(params))
            : previewFor(params),
      );
      const { onLaunched, user } = await setup();
      await openStartFrom(user);
      await user.click(await screen.findByText('no-jupyter'));
      await waitFor(() => expect(serviceBox('jupyter')).not.toBeChecked());
      await user.click(serviceBox('jupyter'));
      pending.resolve();

      await waitFor(() => expect(serviceBox('vscode')).toBeChecked());
      expect(serviceBox('jupyter')).toBeChecked();
      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledWith(
        expect.objectContaining({
          config: expect.objectContaining({ services: DEFAULT_SERVICES }),
          templateName: 'no-jupyter',
        }),
      );
    });

    it('shows a refused preview of the services with Retry, which sends it again', async () => {
      const message = 'user may not use the resource pool gpu';
      const retried = deferred<void>();
      mocks.previewShell
        .mockImplementationOnce(async () => {
          throw await masterRefusal(403, message);
        })
        .mockImplementationOnce((params: { config?: RawJson }) =>
          retried.promise.then(() => previewFor(params)),
        );
      const { onLaunched, user } = await setup({}, { settled: false });
      expect(await screen.findByText(message)).toBeInTheDocument();
      expect(serviceBox('jupyter')).not.toBeChecked();
      expect(launchButton()).toBeDisabled();
      expect(screen.getByRole('button', { name: 'Show Full Config' })).toBeDisabled();

      await user.click(screen.getByRole('button', { name: 'Retry' }));
      await waitFor(() => expect(mocks.previewShell).toHaveBeenCalledTimes(2));
      // While the retry is on its way, the refusal is gone and Launch waits.
      expect(screen.queryByText(message)).not.toBeInTheDocument();
      expect(launchButton()).toBeDisabled();
      expect(screen.getByRole('button', { name: 'Show Full Config' })).toBeDisabled();

      retried.resolve();
      await waitFor(() => expect(serviceBox('jupyter')).toBeChecked());
      expect(mocks.previewShell).toHaveBeenCalledTimes(2);
      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell.mock.calls[0][0].config.services).toEqual(DEFAULT_SERVICES);
    });

    it('drops a refused preview’s message when another start-from is picked', async () => {
      mocks.getTaskTemplates.mockResolvedValue([GPU_TEMPLATE, NO_JUPYTER_TEMPLATE]);
      const message = 'user may not use the resource pool gpu';
      const pending = deferred<void>();
      mocks.previewShell
        .mockImplementationOnce(async () => {
          throw await masterRefusal(403, message);
        })
        .mockImplementationOnce((params: { config?: RawJson; templateName?: string }) =>
          pending.promise.then(() => previewFor(params)),
        );
      const { user } = await setup({}, { settled: false });
      expect(await screen.findByText(message)).toBeInTheDocument();

      await openStartFrom(user);
      await user.click(await screen.findByText('no-jupyter'));
      await waitFor(() => expect(mocks.previewShell).toHaveBeenCalledTimes(2));
      expect(screen.queryByText(message)).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
      expect(launchButton()).toBeDisabled();

      pending.resolve();
      await waitFor(() => expect(serviceBox('vscode')).toBeChecked());
      expect(serviceBox('jupyter')).not.toBeChecked();
      expect(launchButton()).toBeEnabled();
    });

    it('sends nothing after a refused preview of a picked template, until a retry succeeds', async () => {
      mocks.getTaskTemplates.mockResolvedValue([GPU_TEMPLATE, NO_JUPYTER_TEMPLATE]);
      let refuse = true;
      mocks.previewShell.mockImplementation(
        async (params: { config?: RawJson; templateName?: string }) => {
          if (params.templateName === 'no-jupyter' && refuse) {
            throw await masterRefusal(503, 'master is restarting');
          }
          return previewFor(params);
        },
      );
      const { onLaunched, user } = await setup();
      await openStartFrom(user);
      await user.click(await screen.findByText('no-jupyter'));
      await screen.findByRole('button', { name: 'Retry' });
      const previews = mocks.previewShell.mock.calls.length;

      // The template's preview failed, so vscode is not one that the master offered for it.
      await user.click(serviceBox('vscode'));
      await user.click(serviceBox('vscode'));
      expect(serviceBox('vscode')).not.toBeChecked();
      expect(launchButton()).toBeDisabled();
      const fullConfigButton = screen.getByRole('button', { name: 'Show Full Config' });
      expect(fullConfigButton).toBeDisabled();
      fireEvent.click(launchButton());
      fireEvent.click(fullConfigButton);
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(mocks.launchShell).not.toHaveBeenCalled();
      expect(mocks.previewShell).toHaveBeenCalledTimes(previews);
      expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();

      // The retry's answer keeps the box that the user changed while the refusal showed.
      refuse = false;
      await user.click(screen.getByRole('button', { name: 'Retry' }));
      await waitFor(() => expect(launchButton()).toBeEnabled());
      expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
      expect(serviceBox('jupyter')).not.toBeChecked();
      expect(serviceBox('vscode')).not.toBeChecked();
      await launch(user);
      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell).toHaveBeenCalledTimes(1);
      expect(mocks.launchShell).toHaveBeenCalledWith({
        config: {
          description: undefined,
          resources: { resource_pool: undefined, slots: 1 },
          services: { jupyter: { enabled: false }, vscode: { enabled: false } },
        },
        templateName: 'no-jupyter',
        workspaceId: WORKSPACE.id,
      });
    });

    it('previews the start-from again after a refusal when the pool changes', async () => {
      const pool = { auxContainerCapacityPerAgent: 1, slotsAvailable: 0, slotsPerAgent: 0 };
      mocks.pools = [
        { ...pool, name: 'a-pool', usable: true },
        { ...pool, name: 'b-pool', usable: true },
      ];
      mocks.availablePools = ['a-pool', 'b-pool'];
      clusterStore.fetchResourcePools();
      await waitFor(() =>
        expect(Loadable.getOrElse([], clusterStore.resourcePools.get())).toHaveLength(2),
      );
      let refuse = true;
      mocks.previewShell.mockImplementation(async (params: { config?: RawJson }) => {
        if (refuse) throw await masterRefusal(403, 'user may not use the resource pool a-pool');
        return previewFor(params);
      });
      try {
        const { onLaunched, user } = await setup({}, { settled: false });
        const picker = screen.getByRole('combobox', { name: 'Resource Pool' });
        await waitFor(() => expect(picker.closest('.ant-select')).toHaveTextContent('a-pool'));
        await screen.findByRole('button', { name: 'Retry' });
        expect(serviceBox('jupyter')).not.toBeChecked();
        expect(serviceBox('vscode')).not.toBeChecked();
        expect(launchButton()).toBeDisabled();

        refuse = false;
        await user.click(picker);
        await user.click(await screen.findByTitle('b-pool'));
        await waitFor(() => expect(serviceBox('jupyter')).toBeChecked());
        expect(serviceBox('vscode')).toBeChecked();

        await launch(user);
        await waitFor(() => expect(onLaunched).toHaveBeenCalled());
        expect(mocks.launchShell.mock.calls[0][0].config.services).toEqual(DEFAULT_SERVICES);
      } finally {
        mocks.pools = [];
        mocks.availablePools = [];
        clusterStore.fetchResourcePools();
        await waitFor(() =>
          expect(Loadable.getOrElse([], clusterStore.resourcePools.get())).toHaveLength(0),
        );
      }
    });
  });

  describe('a refused preview', () => {
    /**
     * Opens the form, whose preview on open is refused with status and message: Launch and the full
     * config stay disabled until Retry succeeds. Then opens the full config, whose preview is
     * refused the same way.
     */
    const openRefusedFullConfig = async (status: number, message: string) => {
      const refuse = async () => {
        throw await masterRefusal(status, message);
      };
      // The preview on open, the retry, then the full config's.
      mocks.previewShell
        .mockImplementationOnce(refuse)
        .mockImplementationOnce(previewFor)
        .mockImplementationOnce(refuse);
      const { user } = await setup({}, { settled: false });
      const retry = await screen.findByRole('button', { name: 'Retry' });
      expect(launchButton()).toBeDisabled();
      const fullConfigButton = screen.getByRole('button', { name: 'Show Full Config' });
      expect(fullConfigButton).toBeDisabled();
      fireEvent.click(fullConfigButton);
      expect(mocks.previewShell).toHaveBeenCalledTimes(1);

      await user.click(retry);
      await waitFor(() => expect(fullConfigButton).toBeEnabled());
      await user.click(fullConfigButton);
      await waitFor(() => expect(mocks.previewShell).toHaveBeenCalledTimes(3));
      return { user };
    };

    it.each([
      [400, 'services: unknown service "x"; known services: jupyter, ssh, vscode'],
      [403, 'user may not use the resource pool gpu'],
    ])('shows the master’s message for a %s', async (status, message) => {
      await openRefusedFullConfig(status, message);
      expect(await screen.findByText(message)).toBeInTheDocument();
      expect(screen.queryByText('Unable to fetch shell config.')).not.toBeInTheDocument();
    });

    it('shows the generic message for another failure', async () => {
      await openRefusedFullConfig(503, 'master is restarting');
      expect(await screen.findByText('Unable to fetch shell config.')).toBeInTheDocument();
      expect(screen.queryByText('master is restarting')).not.toBeInTheDocument();
    });

    it('drops the refusal when the next preview succeeds', async () => {
      const message = 'user may not use the resource pool gpu';
      const { user } = await openRefusedFullConfig(403, message);
      expect(await screen.findByText(message)).toBeInTheDocument();

      await user.click(screen.getByRole('button', { name: 'Show Simple Config' }));
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() => expect(editorText()).toContain('resource_pool: default'));
      expect(screen.queryByText(message)).not.toBeInTheDocument();
    });
  });

  describe('browser history and the full config', () => {
    it('clears the browser history', async () => {
      recordLaunch(USER_ID, { config: { description: 'a shell' }, workspaceId: WORKSPACE.id });
      const { user } = await setup();
      await user.click(screen.getByRole('button', { name: 'Clear browser history' }));
      expect(listLaunchHistory(USER_ID)).toEqual([]);
      expect(
        screen.queryByRole('button', { name: 'Clear browser history' }),
      ).not.toBeInTheDocument();
    });

    it('launches the edited full config, previewed once more, and lets the master name the shell', async () => {
      const { onLaunched, user } = await setup();
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() => expect(editorText()).toContain('default:1'));
      editConfig((config) => ({
        ...config,
        environment: {
          ...config.environment,
          environment_variables: ['EDITED=1'],
          image: { cuda: 'edited:7' },
        },
      }));
      expect(editorText()).toContain('edited:7');
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      // Once for the start-from on open, once for the full config.
      expect(mocks.previewShell).toHaveBeenCalledTimes(2);
      const { config } = mocks.launchShell.mock.calls[0][0];
      expect(config).not.toHaveProperty('description');
      expect(config.environment).toEqual({
        environment_variables: ['EDITED=1'],
        image: { cuda: 'edited:7' },
      });
      expect(config.resources).toEqual({ priority: 42, resource_pool: 'default', slots: 1 });
    });

    it('drops the late answer of an earlier full config', async () => {
      const { onLaunched, user } = await setup();
      const first = deferred<void>();
      mocks.previewShell.mockImplementationOnce(async (params: { config?: RawJson }) => {
        await first.promise;
        return previewFor(params);
      });
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() => expect(mocks.previewShell).toHaveBeenCalledTimes(2));
      await user.click(screen.getByRole('button', { name: 'Show Simple Config' }));
      await user.click(serviceBox('vscode'));
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() => expect(editorText()).toContain('enabled: false'));

      first.resolve();
      await waitFor(() => expect(mocks.previewShell.mock.results[1].value).resolves.toBeDefined());
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(yaml.load(editorText())).toMatchObject({
        services: { jupyter: {}, vscode: { enabled: false } },
      });
      await launch(user);

      await waitFor(() => expect(onLaunched).toHaveBeenCalled());
      expect(mocks.launchShell.mock.calls[0][0].config.services).toEqual({
        jupyter: {},
        vscode: { enabled: false },
      });
    });
  });

  describe('Save as Template', () => {
    it('opens a prefilled new template with only the non-default settings and creates it', async () => {
      const { user } = await setup();
      await user.type(screen.getByPlaceholderText('Name (optional)'), 'tpl source');
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() => expect(editorText()).toContain('custom:2'));

      await user.click(screen.getByRole('button', { name: 'Save as Template' }));
      expect(await screen.findByText('New Template')).toBeInTheDocument();
      expect(screen.getByText(/Other users can read templates/)).toBeInTheDocument();
      expect(screen.getAllByTestId('code-editor')).toHaveLength(2);
      const draft = editorText();
      expect(draft).toContain('custom:2');
      expect(draft).toContain('resource_pool: default');
      expect(draft).not.toContain('tpl source');
      expect(draft).not.toContain('idle_timeout');
      expect(draft).not.toContain('priority');
      // The master's default services are not a template's setting.
      expect(draft).not.toContain('services');

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

    it('keeps no services without code-server on the master', async () => {
      mocks.codeServer = false;
      const { user } = await setup();
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() => expect(editorText()).toContain('default:1'));

      await user.click(screen.getByRole('button', { name: 'Save as Template' }));
      expect(await screen.findByText('New Template')).toBeInTheDocument();
      expect(editorText()).not.toContain('services');
    });

    it('keeps a service turned off that the cluster does not run now', async () => {
      mocks.codeServer = false;
      mocks.getShellConfig.mockResolvedValue({
        ...clusterShellConfig,
        services: { jupyter: {}, vscode: { enabled: false } },
      });
      const { user } = await setup({ initialTask: task(CommandType.Shell) });
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() => expect(editorText()).toContain('enabled: false'));

      await user.click(screen.getByRole('button', { name: 'Save as Template' }));
      expect(await screen.findByText('New Template')).toBeInTheDocument();
      expect(yaml.load(editorText())).toMatchObject({ services: { vscode: { enabled: false } } });
      expect(yaml.load(editorText())).not.toHaveProperty('services.jupyter');
    });

    it('opens no template when the cluster defaults fail to load, and keeps a null work_dir on retry', async () => {
      // A task launched with work_dir: null, which clears the cluster default.
      mocks.getShellConfig.mockResolvedValue({ ...clusterShellConfig, work_dir: null });
      mocks.previewShell.mockImplementation(async (params: { config?: RawJson }) => ({
        ...(await previewFor(params)),
        work_dir:
          params.config && 'work_dir' in params.config ? params.config.work_dir : '/cluster/work',
      }));
      const { user } = await setup({ initialTask: task(CommandType.Shell) });
      await waitFor(() =>
        expect(screen.getByPlaceholderText('Name (optional)')).toHaveValue('old shell'),
      );
      await user.click(screen.getByRole('button', { name: 'Show Full Config' }));
      await waitFor(() => expect(editorText()).toContain('work_dir: null'));
      const loadedConfig = editorText();

      mocks.previewShell.mockRejectedValueOnce(new Error('503 Service Unavailable'));
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
      expect(screen.getByText('Launch Interactive Task')).toBeInTheDocument();
      expect(screen.getAllByTestId('code-editor')).toHaveLength(1);
      expect(editorText()).toBe(loadedConfig);

      await user.click(screen.getByRole('button', { name: 'Save as Template' }));
      expect(await screen.findByText('New Template')).toBeInTheDocument();
      expect(screen.getAllByTestId('code-editor')).toHaveLength(2);
      const draft = editorText();
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
