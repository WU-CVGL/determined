import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { Loadable } from 'hew/utils/loadable';
import React, { useEffect } from 'react';
import { DndProvider } from 'react-dnd';
import { HTML5Backend } from 'react-dnd-html5-backend';
import { BrowserRouter, MemoryRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import {
  getExperiments,
  getGenericTasks,
  getJupyterLabs,
  getShells,
  getTensorBoards,
  getUserSetting,
  killExperiment,
  killGenericTask,
  killTask,
  unpauseGenericTask,
  updateUserSetting,
} from 'services/api';
import { V1SlotsFilter } from 'services/api-ts-sdk';
import authStore from 'stores/auth';
import userStore from 'stores/users';
import userSettings from 'stores/userSettings';
import {
  BulkExperimentItem,
  CommandState,
  CommandTask,
  CommandType,
  FetchOptions,
  GenericTask,
  GenericTaskState,
  RunState,
  Workspace,
  WorkspaceState,
} from 'types';
import handleError from 'utils/error';
import { isDangerMenuItem, isDisabledMenuItem, menuLabels, openMenuItem } from 'utils/tests/menu';

import { fetchRunPage } from './fetchRuns';
import TaskDashboard from './TaskDashboard';
import { MIN_COLUMN_WIDTH } from './TaskDashboard.settings';

// The menu and bulk Kill tests take many steps, which can outlast 5 s when the whole suite runs.
vi.setConfig({ testTimeout: 15_000 });

const CURRENT_USER_ID = 3;

/* The states the list APIs report for the generic task and the experiment. */
const listed = vi.hoisted(() => ({ experimentState: 'ACTIVE', genericState: 'ACTIVE' }));

/* With a user ID, the user may control only their own notebooks, shells, commands and TensorBoards. */
const access = vi.hoisted(() => ({ onlyOwnRunsOf: undefined as number | undefined }));

/* The real fetchRunPage, which a test may replace for some queries. */
const real = vi.hoisted(() => ({ fetchRunPage: undefined as unknown as typeof fetchRunPage }));

const SHELL: CommandTask = {
  id: 'shell-1',
  name: 'gpu-shell',
  resourcePool: 'default',
  slots: 1,
  startTime: '2026-01-04T00:00:00Z',
  state: CommandState.Running,
  type: CommandType.Shell,
  userId: CURRENT_USER_ID,
  workspaceId: 1,
};

const NOTEBOOK: CommandTask = {
  id: 'nb-1',
  name: 'cpu-notebook',
  resourcePool: 'default',
  slots: 0,
  startTime: '2026-01-02T00:00:00Z',
  state: CommandState.Running,
  type: CommandType.JupyterLab,
  userId: 4,
  workspaceId: 1,
};

const GENERIC: GenericTask = {
  description: '',
  jobId: 'job-1',
  name: 'eval-sweep',
  noPause: false,
  projectId: 1,
  resourcePool: 'default',
  slots: 1,
  startTime: '2026-01-03T00:00:00Z',
  state: GenericTaskState.Active,
  taskId: 'task-1',
  userId: 4,
  username: 'bob',
  workspaceId: 1,
};

const EXPERIMENT: BulkExperimentItem = {
  archived: false,
  hyperparameters: {},
  id: 42,
  jobId: 'job-42',
  labels: [],
  name: 'bert-finetune',
  numTrials: 1,
  parentArchived: false,
  projectId: 1,
  projectName: 'Uncategorized',
  projectOwnerId: 1,
  resourcePool: 'default',
  searcherType: 'single',
  startTime: '2026-01-01T00:00:00Z',
  state: RunState.Running,
  userId: 4,
  workspaceId: 1,
  workspaceName: 'Uncategorized',
};

const WORKSPACE: Workspace = {
  archived: false,
  id: 7,
  immutable: false,
  name: 'vision',
  numExperiments: 0,
  numProjects: 0,
  pinned: false,
  state: WorkspaceState.Unspecified,
  userId: 1,
};

vi.mock('services/api', () => ({
  getCommands: vi.fn(() => Promise.resolve([])),
  getCurrentUser: () => Promise.resolve({ id: 3, isActive: true, isAdmin: false, username: 'me' }),
  getExperiments: vi.fn(),
  getGenericTasks: vi.fn(),
  getJupyterLabs: vi.fn(),
  getShells: vi.fn(),
  getTensorBoards: vi.fn(() => Promise.resolve([])),
  getUsers: () => Promise.resolve({ users: [] }),
  getUserSetting: vi.fn(() => Promise.resolve({ settings: [] })),
  getWorkspaceProjects: () => Promise.resolve({ pagination: { total: 0 }, projects: [] }),
  getWorkspaces: () => Promise.resolve({ pagination: { total: 0 }, workspaces: [] }),
  killExperiment: vi.fn(() => Promise.resolve()),
  killGenericTask: vi.fn(() => Promise.resolve()),
  killTask: vi.fn(() => Promise.resolve()),
  pauseGenericTask: vi.fn(() => Promise.resolve()),
  resetUserSetting: () => Promise.resolve(),
  unpauseGenericTask: vi.fn(() => Promise.resolve()),
  updateUserSetting: vi.fn(() => Promise.resolve()),
}));

// Every permission granted, unless a test limits it: the menus' own rules are tested with each menu.
vi.mock('hooks/usePermissions', () => ({
  default: () =>
    new Proxy(
      {},
      {
        get: (_target, key) => {
          if (key === 'loading') return false;
          if (key === 'canCreateNSC') return true;
          if (key === 'canModifyWorkspaceNSC' && access.onlyOwnRunsOf !== undefined) {
            return ({ userId }: { userId?: number }) => userId === access.onlyOwnRunsOf;
          }
          return () => true;
        },
      },
    ),
}));
vi.mock('./fetchRuns', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./fetchRuns')>();
  real.fetchRunPage = actual.fetchRunPage;
  return { ...actual, fetchRunPage: vi.fn(actual.fetchRunPage) };
});
vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => true }));
vi.mock('components/JupyterLabButton', () => ({
  default: () => <div data-testid="jupyter-lab-button" />,
}));
vi.mock('components/ShellButton', () => ({ default: () => <div data-testid="shell-button" /> }));
vi.mock('hew/Tooltip');
vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: vi.fn(),
}));

const Container: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  useEffect(() => {
    authStore.setAuth({ isAuthenticated: true });
    authStore.setAuthChecked();
    userStore.updateCurrentUser({
      id: CURRENT_USER_ID,
      isActive: true,
      isAdmin: false,
      username: 'me',
    });
    return userSettings.startPolling();
  }, []);
  return <SettingsProvider>{children}</SettingsProvider>;
};

/** With `browser`, at this URL of the browser itself, which the settings read on the first load. */
const setup = (
  props: React.ComponentProps<typeof TaskDashboard> = {},
  url = '/jobs',
  { browser = false } = {},
) => {
  const page = (
    <Container>
      <ConfirmationProvider>
        <TaskDashboard {...props} />
      </ConfirmationProvider>
    </Container>
  );
  if (browser) window.history.replaceState(null, '', url);
  return render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <DndProvider backend={HTML5Backend}>
          {browser ? (
            <BrowserRouter>{page}</BrowserRouter>
          ) : (
            <MemoryRouter initialEntries={[url]}>{page}</MemoryRouter>
          )}
        </DndProvider>
      </ThemeProvider>
    </UIProvider>,
  );
};

const JOBS_SETTINGS = 'jobs-dashboard-global';

/** Settings stored before the page loads, which the settings store then reads from the master. */
const storeBeforeLoad = (settings: Record<string, unknown>, storagePath = JOBS_SETTINGS) => {
  userSettings.reset();
  vi.mocked(getUserSetting).mockResolvedValueOnce({
    settings: Object.entries(settings).map(([key, value]) => ({
      key,
      storagePath,
      value: JSON.stringify(value),
    })),
  });
};

/* For a page that first loads stored settings, which takes seconds in a full test run. */
const AFTER_LOAD = { timeout: 10_000 };

/** The settings in the store, as the page last updated them. */
const stored = (storagePath = JOBS_SETTINGS): Record<string, unknown> =>
  (Loadable.getOrElse(undefined, userSettings.getAll().get())?.get(storagePath) ?? {}) as Record<
    string,
    unknown
  >;

/** The last value of this Jobs page setting that the page sent to the master. */
const saved = (key: string): unknown => {
  const value = vi
    .mocked(updateUserSetting)
    .mock.calls.flatMap(([params]) => params.settings ?? [])
    .filter((setting) => setting.storagePath === JOBS_SETTINGS && setting.key === key)
    .at(-1)?.value;
  return value === undefined ? undefined : JSON.parse(value);
};

/* The columns and widths the Jobs page stored before its Slots column. */
const OLD_COLUMNS = [
  'kind',
  'id',
  'name',
  'state',
  'user',
  'location',
  'resourcePool',
  'startTime',
  'endTime',
];
const OLD_WIDTHS = [301, 302, 303, 304, 305, 306, 307, 308, 309];

/* The same with the Slots column, as this version stores them. */
const NEW_COLUMNS = [...OLD_COLUMNS.slice(0, 7), 'slots', ...OLD_COLUMNS.slice(7)];
const NEW_WIDTHS = [...OLD_WIDTHS.slice(0, 7), 72, ...OLD_WIDTHS.slice(7)];

/* The width of each stored column: the table binds the stored widths to the columns by place. */
const storedWidths = () => {
  const { columns, columnWidths } = stored() as { columns: string[]; columnWidths: number[] };
  expect(columnWidths).toHaveLength(columns.length);
  return Object.fromEntries(columns.map((col, i) => [col, columnWidths[i]]));
};

/** The width of each column the table shows, by its title. */
const shownWidths = () => {
  const table = screen.getAllByRole('table')[0];
  const titles = within(table)
    .getAllByRole('columnheader')
    .map((th) => th.textContent?.trim());
  const widths = [...table.querySelectorAll('colgroup > col')].map(
    (col) => (col as HTMLElement).style.width,
  );
  expect(widths).toHaveLength(titles.length);
  return Object.fromEntries(titles.map((title, i) => [title, widths[i]]));
};

const header = (title: string) =>
  screen.getAllByRole('columnheader').find((th) => th.textContent?.trim() === title) as HTMLElement;

/** Drags the right edge of the column with this title to x, as a user resizes it. */
const resize = async (title: string, x: number) => {
  const edge = header(title).querySelector('[class*="columnResizeHandle"]') as HTMLElement;
  fireEvent.mouseDown(edge, { button: 0, clientX: 0 });
  fireEvent.mouseMove(document, { clientX: x });
  // The table takes a new width after a short throttle.
  await act(() => new Promise((resolve) => setTimeout(resolve, 100)));
  fireEvent.mouseUp(document, { clientX: x });
};

/** Drags the column with this title onto the next one to its right, as a user moves it. */
const moveRight = async (title: string, onto: string) => {
  const data: Record<string, string> = {};
  const dataTransfer = {
    dropEffect: 'move',
    effectAllowed: 'all',
    files: [],
    getData: (key: string) => data[key],
    items: [],
    setData: (key: string, value: string) => (data[key] = value),
    setDragImage: () => undefined,
    types: [],
  };
  const source = header(title).querySelector('[draggable="true"]') as HTMLElement;
  const target = header(onto).querySelector('[class*="dropTarget"]') as HTMLElement;
  fireEvent.dragStart(source, { clientX: 100, dataTransfer });
  // The drag starts a tick after its event.
  await act(() => new Promise((resolve) => setTimeout(resolve, 20)));
  for (const drag of [fireEvent.dragEnter, fireEvent.dragOver, fireEvent.drop]) {
    drag(target, { clientX: 300, dataTransfer });
  }
  fireEvent.dragEnd(source, { clientX: 300, dataTransfer });
};

const user = userEvent.setup();

/** The parameters of the latest list call of a paged source (not its count call, limit 1). */
const lastListCall = <P extends { limit?: number }>(
  fn: (params: P, options?: FetchOptions) => Promise<unknown>,
): P | undefined =>
  vi
    .mocked(fn)
    .mock.calls.filter(([params]) => params.limit !== 1)
    .at(-1)?.[0];

const row = async (name: string) => (await screen.findByText(name)).closest('tr') as HTMLElement;

/** Waits for the stored settings, which drop an update while they load. */
const settingsLoaded = () =>
  waitFor(() => expect(Loadable.isLoaded(userSettings.getAll().get())).toBe(true));

const STALE_EXPERIMENT: BulkExperimentItem = { ...EXPERIMENT, id: 41, name: 'stale-run' };

/** Picks an option of the Select with this test ID. */
const choose = async (testId: string, label: string) => {
  await user.click(within(screen.getByTestId(testId)).getByRole('combobox'));
  const options = (await screen.findAllByTitle(label)).filter(
    (option) => !option.closest('.ant-select-dropdown-hidden'),
  );
  await user.click(options[options.length - 1]);
};

describe('TaskDashboard', () => {
  beforeEach(() => {
    access.onlyOwnRunsOf = undefined;
    vi.mocked(fetchRunPage).mockImplementation(real.fetchRunPage);
    listed.experimentState = RunState.Running;
    listed.genericState = GenericTaskState.Active;
    vi.mocked(getShells).mockResolvedValue([SHELL]);
    vi.mocked(getJupyterLabs).mockResolvedValue([NOTEBOOK]);
    vi.mocked(getGenericTasks).mockImplementation((params) =>
      Promise.resolve({
        pagination: { limit: 0, offset: 0, total: params.limit === 1 ? 5 : 1 },
        tasks: [{ ...GENERIC, state: listed.genericState as GenericTaskState }],
      }),
    );
    vi.mocked(getExperiments).mockImplementation((params) =>
      Promise.resolve({
        experiments: [{ ...EXPERIMENT, state: listed.experimentState as RunState }],
        pagination: { limit: 0, offset: 0, total: params.limit === 1 ? 2 : 1 },
      }),
    );
  });

  afterEach(async () => {
    // The settings store outlives a test; each test starts from the default filters.
    await userSettings.clear();
    vi.clearAllMocks();
    window.history.replaceState(null, '', '/');
  });

  it('lists every kind on the Jobs page, newest first, with the launch buttons', async () => {
    setup();

    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    const names = screen
      .getAllByRole('row')
      .map((tr) => tr.textContent ?? '')
      .filter((text) => /gpu-shell|eval-sweep|cpu-notebook|bert-finetune/.test(text))
      .map((text) => text.match(/gpu-shell|eval-sweep|cpu-notebook|bert-finetune/)?.[0]);
    expect(names).toEqual(['gpu-shell', 'eval-sweep', 'cpu-notebook', 'bert-finetune']);
    expect(screen.getByTestId('jupyter-lab-button')).toBeInTheDocument();
    expect(screen.getByTestId('shell-button')).toBeInTheDocument();
    expect(screen.getByText(/listed for 24 hours after they end/)).toBeInTheDocument();
  });

  it('counts the active runs of each kind on its chip', async () => {
    setup();

    await waitFor(() =>
      expect(screen.getByTestId('kind-experiment')).toHaveTextContent('Experiment2'),
    );
    expect(screen.getByTestId('kind-generic-task')).toHaveTextContent('Generic Task5');
    expect(screen.getByTestId('kind-shell')).toHaveTextContent('Shell1');
    expect(screen.getByTestId('kind-command')).toHaveTextContent('Command0');
  });

  it('leaves experiments out of the tasks-only view', async () => {
    setup({ tasksOnly: true }, '/tasks');

    expect(await screen.findByText('eval-sweep')).toBeInTheDocument();
    expect(getExperiments).not.toHaveBeenCalled();
    expect(screen.queryByTestId('kind-experiment')).not.toBeInTheDocument();
    expect(screen.queryByText('bert-finetune')).not.toBeInTheDocument();
  });

  it("lists a workspace's runs, with the launch buttons", async () => {
    setup({ workspace: WORKSPACE }, '/workspaces/7/jobs');

    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    expect(lastListCall(getExperiments)).toMatchObject({ workspaceId: 7 });
    expect(lastListCall(getGenericTasks)).toMatchObject({ workspaceId: 7 });
    expect(vi.mocked(getShells).mock.calls[0][0]).toMatchObject({ workspaceId: 7 });
    expect(screen.getByTestId('shell-button')).toBeInTheDocument();
  });

  it("lists a project's experiments and generic tasks only, without launch buttons", async () => {
    setup({ projectId: 1 }, '/projects/1/jobs');

    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    expect(screen.getByText('eval-sweep')).toBeInTheDocument();
    expect(lastListCall(getExperiments)).toMatchObject({ projectId: 1 });
    expect(lastListCall(getGenericTasks)).toMatchObject({ projectId: 1 });
    expect(getShells).not.toHaveBeenCalled();
    expect(getJupyterLabs).not.toHaveBeenCalled();
    expect(screen.queryByText('gpu-shell')).not.toBeInTheDocument();
    expect(screen.queryByTestId('kind-shell')).not.toBeInTheDocument();
    expect(screen.queryByTestId('shell-button')).not.toBeInTheDocument();
    expect(screen.queryByText(/listed for 24 hours after they end/)).not.toBeInTheDocument();
  });

  it('lists only the kind of a chip after a click on it', async () => {
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();

    const before = vi.mocked(getExperiments).mock.calls.length;
    await user.click(screen.getByTestId('kind-generic-task'));

    await waitFor(() => expect(screen.queryByText('bert-finetune')).not.toBeInTheDocument());
    expect(screen.getByText('eval-sweep')).toBeInTheDocument();
    expect(screen.queryByText('gpu-shell')).not.toBeInTheDocument();
    expect(screen.getByTestId('kind-generic-task')).toHaveAttribute('aria-pressed', 'true');
    // The chips still count experiments, but no experiment list is asked for.
    const after = vi.mocked(getExperiments).mock.calls.slice(before);
    expect(after.length).toBeGreaterThan(0);
    expect(after.every(([params]) => params.limit === 1)).toBe(true);
  });

  it("applies the URL's kind filter, as /tasks/generic redirects with it", async () => {
    setup({ tasksOnly: true }, '/tasks?type=generic-task');

    expect(await screen.findByText('eval-sweep')).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByTestId('kind-generic-task')).toHaveAttribute('aria-pressed', 'true'),
    );
    await waitFor(() => expect(screen.queryByText('gpu-shell')).not.toBeInTheDocument());
    expect(screen.queryByText('cpu-notebook')).not.toBeInTheDocument();
  });

  it("keeps the URL's kinds over the stored ones while it moves the stored columns", async () => {
    storeBeforeLoad({ columns: OLD_COLUMNS, columnWidths: OLD_WIDTHS, type: ['experiment'] });
    setup({}, '/jobs?type=shell', { browser: true });

    await waitFor(() => expect(stored().columns).toContain('slots'), AFTER_LOAD);
    await waitFor(() => expect(stored().type).toEqual(['shell']), AFTER_LOAD);
    // Long enough for the URL and the settings to undo each other, as they did.
    await act(() => new Promise((resolve) => setTimeout(resolve, 300)));
    expect(new URLSearchParams(window.location.search).getAll('type')).toEqual(['shell']);
    expect(stored().type).toEqual(['shell']);
    expect(screen.getByTestId('kind-shell')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('kind-experiment')).toHaveAttribute('aria-pressed', 'false');
    await waitFor(
      () => expect(screen.queryByText('bert-finetune')).not.toBeInTheDocument(),
      AFTER_LOAD,
    );
    expect(screen.getByText('gpu-shell')).toBeInTheDocument();
  }, 30_000);

  it('keeps old ?type values of the task list working', async () => {
    setup({ tasksOnly: true }, '/tasks?type=jupyter-lab');

    await waitFor(() =>
      expect(screen.getByTestId('kind-jupyter-lab')).toHaveAttribute('aria-pressed', 'true'),
    );
    await waitFor(() => expect(screen.queryByText('gpu-shell')).not.toBeInTheDocument());
    expect(screen.getByText('cpu-notebook')).toBeInTheDocument();
  });

  it("asks for the current user's runs with Mine, everyone's by default", async () => {
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    expect(lastListCall(getGenericTasks)?.userIds).toBeUndefined();

    await choose('owner', 'Mine');

    await waitFor(() => expect(lastListCall(getGenericTasks)?.userIds).toEqual([CURRENT_USER_ID]));
    expect(lastListCall(getExperiments)?.userIds).toEqual([CURRENT_USER_ID]);
    expect(vi.mocked(getShells).mock.calls.at(-1)?.[0]).toMatchObject({
      users: [String(CURRENT_USER_ID)],
    });
  });

  it('filters GPU runs on the master for experiments and generic tasks, here for tasks', async () => {
    setup();
    expect(await screen.findByText('cpu-notebook')).toBeInTheDocument();

    await choose('slots', 'GPU');

    await waitFor(() =>
      expect(lastListCall(getGenericTasks)?.slotsFilter).toBe(V1SlotsFilter.HASSLOTS),
    );
    expect(lastListCall(getExperiments)?.slotsFilter).toBe(V1SlotsFilter.HASSLOTS);
    await waitFor(() => expect(screen.queryByText('cpu-notebook')).not.toBeInTheDocument());
    expect(screen.getByText('gpu-shell')).toBeInTheDocument();

    await choose('slots', 'CPU-only');

    await waitFor(() =>
      expect(lastListCall(getGenericTasks)?.slotsFilter).toBe(V1SlotsFilter.ZEROSLOTS),
    );
    await waitFor(() => expect(screen.queryByText('gpu-shell')).not.toBeInTheDocument());
    expect(screen.getByText('cpu-notebook')).toBeInTheDocument();
  });

  it('explains GPU and CPU-only in a tooltip on an icon, never over the open options', async () => {
    const hint = 'GPU: asks for at least one slot. CPU-only: asks for none.';
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();

    await user.click(within(screen.getByTestId('slots')).getByRole('combobox'));
    expect((await screen.findAllByTitle('CPU-only')).length).toBeGreaterThan(0);
    // Longer than the tooltip's 0.1 s open delay.
    await act(() => new Promise((resolve) => setTimeout(resolve, 300)));
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();

    await user.hover(screen.getByLabelText(hint));
    expect(await screen.findByRole('tooltip')).toHaveTextContent(hint);
  });

  it('shows the slots each run asks for between Resource Pool and Started', async () => {
    const configOf = (config: unknown) => config as BulkExperimentItem['config'];
    vi.mocked(getShells).mockResolvedValue([
      SHELL,
      { ...SHELL, id: 'shell-2', name: 'old-shell', slots: undefined },
    ]);
    vi.mocked(getExperiments).mockImplementation(() =>
      Promise.resolve({
        experiments: [
          { ...EXPERIMENT, config: configOf({ resources: { slots_per_trial: 4 } }) },
          { ...STALE_EXPERIMENT, config: configOf({ resources: {} }) },
        ],
        pagination: { limit: 0, offset: 0, total: 2 },
      }),
    );
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();

    // Resource Pool is hidden below the md breakpoint, as in tests; Slots shows at every width.
    const headers = screen.getAllByRole('columnheader').map((h) => h.textContent?.trim());
    expect(headers.indexOf('Slots')).toBe(headers.indexOf('Started') - 1);

    const slots = async (name: string) => within(await row(name)).getByTestId('slots-cell');
    expect(await slots('bert-finetune')).toHaveTextContent(/^4$/);
    expect(within(await slots('bert-finetune')).getByTitle('Slots per trial')).toBeInTheDocument();
    expect(await slots('stale-run')).toHaveTextContent(/^1$/);
    expect(await slots('gpu-shell')).toHaveTextContent(/^1$/);
    expect(await slots('old-shell')).toHaveTextContent(/^—$/);
    expect(await slots('cpu-notebook')).toHaveTextContent(/^0$/);
    expect(await slots('eval-sweep')).toHaveTextContent(/^1$/);
  });

  describe('with widths stored before the Slots column', () => {
    it.each([
      ['only the widths, as a resize stores them', { columnWidths: OLD_WIDTHS }],
      ['the columns and their widths', { columns: OLD_COLUMNS, columnWidths: OLD_WIDTHS }],
    ])(
      'gives each column its own width when they were %s',
      async (_, settings) => {
        storeBeforeLoad(settings);
        setup();
        expect(await screen.findByText('bert-finetune', {}, AFTER_LOAD)).toBeInTheDocument();

        await waitFor(() => expect(stored().columnWidths).toHaveLength(10), AFTER_LOAD);
        expect(storedWidths()).toEqual({
          endTime: 309,
          id: 302,
          kind: 301,
          location: 306,
          name: 303,
          resourcePool: 307,
          slots: 72,
          startTime: 308,
          state: 304,
          user: 305,
        });
        // ID, User, Location, Resource Pool and Ended are hidden below the md breakpoint, as in
        // tests. The table takes the new widths a render later, which takes seconds in a full run.
        await waitFor(() => expect(shownWidths()).toMatchObject({ Slots: '72px' }), AFTER_LOAD);
        expect(shownWidths()).toMatchObject({
          Kind: '301px',
          Name: '303px',
          Started: '308px',
          State: '304px',
        });
      },
      30_000,
    );

    it('gives columns stored in another order without their widths their own widths', async () => {
      const reordered = [
        'name',
        'kind',
        ...OLD_COLUMNS.filter((c) => !['name', 'kind'].includes(c)),
      ];
      storeBeforeLoad({ columns: reordered });
      setup();
      expect(await screen.findByText('bert-finetune', {}, AFTER_LOAD)).toBeInTheDocument();

      await waitFor(() => expect(stored().columns).toContain('slots'), AFTER_LOAD);
      expect(storedWidths()).toMatchObject({ id: 100, kind: 64, name: 240, slots: 72 });
      // The widths keep their count, which the table alone would not take up.
      await waitFor(() => expect(shownWidths()).toMatchObject({ Kind: '64px' }), AFTER_LOAD);
      expect(shownWidths()).toMatchObject({ Name: '240px', Slots: '72px', State: '120px' });
    }, 30_000);
  });

  describe('with the columns and widths of this version stored', () => {
    it.each([
      ['the columns and their widths', { columns: NEW_COLUMNS, columnWidths: NEW_WIDTHS }],
      ['only the widths, as a resize stores them', { columnWidths: NEW_WIDTHS }],
    ])(
      'shows each column at its stored width on a first load when they were %s',
      async (_, settings) => {
        storeBeforeLoad(settings);
        setup();
        expect(await screen.findByText('bert-finetune', {}, AFTER_LOAD)).toBeInTheDocument();

        // The table mounted while the settings loaded; it mounts again once they have.
        await waitFor(() => expect(shownWidths()).toMatchObject({ Kind: '301px' }), AFTER_LOAD);
        expect(shownWidths()).toMatchObject({
          Name: '303px',
          Slots: '72px',
          Started: '308px',
          State: '304px',
        });
      },
      30_000,
    );

    it('stores a resize after a first load', async () => {
      storeBeforeLoad({ columns: NEW_COLUMNS, columnWidths: NEW_WIDTHS });
      setup();
      expect(await screen.findByText('bert-finetune', {}, AFTER_LOAD)).toBeInTheDocument();
      await waitFor(() => expect(shownWidths()).toMatchObject({ Kind: '301px' }), AFTER_LOAD);

      await resize('Name', 450);
      // The resize changes the table's own widths in place, not the stored ones it started from,
      // so the settings see a change and store it.
      await waitFor(() =>
        expect(saved('columnWidths')).toEqual([301, 302, 450, 304, 305, 306, 307, 72, 308, 309]),
      );
      expect(shownWidths()).toMatchObject({ Kind: '301px', Name: '450px' });
    }, 30_000);

    it('stores a resize after a column move', async () => {
      storeBeforeLoad({ columns: NEW_COLUMNS, columnWidths: NEW_WIDTHS });
      setup();
      expect(await screen.findByText('bert-finetune', {}, AFTER_LOAD)).toBeInTheDocument();
      await waitFor(() => expect(shownWidths()).toMatchObject({ Kind: '301px' }), AFTER_LOAD);

      await moveRight('Name', 'State');
      await waitFor(() =>
        expect(saved('columns')).toEqual(['kind', 'id', 'state', 'name', ...NEW_COLUMNS.slice(4)]),
      );
      expect(saved('columnWidths')).toEqual([301, 302, 304, 303, 305, 306, 307, 72, 308, 309]);

      // The move gives the table and the settings one array of widths; the resize changes the
      // table's own.
      await resize('Kind', 500);
      await waitFor(() =>
        expect(saved('columnWidths')).toEqual([500, 302, 304, 303, 305, 306, 307, 72, 308, 309]),
      );
      expect(shownWidths()).toMatchObject({ Kind: '500px', Name: '303px', State: '304px' });
    }, 30_000);
  });

  it('shows the rows once the stored settings have loaded', async () => {
    let load: (response: { settings: [] }) => void = () => undefined;
    userSettings.reset();
    vi.mocked(getUserSetting).mockReturnValueOnce(new Promise((resolve) => (load = resolve)));
    setup();
    await waitFor(() => expect(getExperiments).toHaveBeenCalled());
    await act(() => new Promise((resolve) => setTimeout(resolve, 300)));
    // The table mounts again once they have, which would replace a row being clicked.
    expect(screen.queryByText('bert-finetune')).not.toBeInTheDocument();

    load({ settings: [] });
    expect(await screen.findByText('bert-finetune', {}, AFTER_LOAD)).toBeInTheDocument();
  });

  it('stores the columns with the widths of a resize', async () => {
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();

    await resize('Name', 450);
    // Widths stored alone would be bound by place to the default columns of a later version.
    await waitFor(() =>
      expect(saved('columnWidths')).toEqual([64, 100, 450, 120, 85, 230, 128, 72, 117, 117]),
    );
    expect(saved('columns')).toEqual(NEW_COLUMNS);
    expect(storedWidths()).toMatchObject({ kind: 64, name: 450, slots: 72 });
  });

  it('resizes a column narrower than its default width, down to the minimum', async () => {
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();

    await resize('Name', 120);
    await waitFor(() => expect(storedWidths()).toMatchObject({ name: 120 }));
    expect(shownWidths()).toMatchObject({ Name: '120px' });

    await resize('Name', 10);
    await waitFor(() => expect(storedWidths()).toMatchObject({ name: MIN_COLUMN_WIDTH }));
    expect(shownWidths()).toMatchObject({ Name: `${MIN_COLUMN_WIDTH}px` });
  });

  it('shows a stored width narrower than the default', async () => {
    storeBeforeLoad({
      columns: NEW_COLUMNS,
      columnWidths: [...NEW_WIDTHS.slice(0, 2), 90, ...NEW_WIDTHS.slice(3)],
    });
    setup();
    expect(await screen.findByText('bert-finetune', {}, AFTER_LOAD)).toBeInTheDocument();
    await waitFor(() => expect(shownWidths()).toMatchObject({ Kind: '301px' }), AFTER_LOAD);
    expect(shownWidths()).toMatchObject({ Name: '90px' });
  });

  it('cuts a long name short in its cell, with the whole name in a title or tooltip', async () => {
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();

    // A definite table width keeps the columns at their own widths, which 'max-content' would
    // widen to their longest content.
    expect(screen.getAllByRole('table')[0].style.width).toBe('100%');
    const names = ['bert-finetune', 'eval-sweep', 'gpu-shell', 'cpu-notebook'];
    for (const name of names) {
      expect(screen.getByText(name).closest('td')).toHaveClass('ant-table-cell-ellipsis');
    }
    // The experiment's name shows its own tooltip when cut short, so it has no title.
    expect(screen.getByText('bert-finetune').closest('a')).toHaveAttribute(
      'href',
      '/experiments/42',
    );
    expect(screen.getByText('bert-finetune').closest('[title]')).toBeNull();
    expect(screen.getByText('eval-sweep').closest('a')).toHaveAttribute(
      'href',
      '/generic-tasks/task-1',
    );
    for (const name of names.slice(1)) {
      expect(screen.getByText(name).closest('[title]')).toHaveAttribute('title', name);
    }
  });

  it('keeps the filters of the Jobs page and of the tasks-only view apart', async () => {
    const jobs = setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();
    await choose('owner', 'Mine');
    await waitFor(() => expect(lastListCall(getGenericTasks)?.userIds).toEqual([CURRENT_USER_ID]));
    jobs.unmount();

    vi.mocked(getGenericTasks).mockClear();
    const tasks = setup({ tasksOnly: true }, '/tasks');
    expect(await screen.findByText('eval-sweep')).toBeInTheDocument();
    expect(lastListCall(getGenericTasks)?.userIds).toBeUndefined();
    expect(screen.getByTestId('owner')).toHaveTextContent('All users');
    tasks.unmount();

    // The Jobs page kept its own filter.
    vi.mocked(getGenericTasks).mockClear();
    setup();
    await waitFor(() => expect(lastListCall(getGenericTasks)?.userIds).toEqual([CURRENT_USER_ID]));
    expect(screen.getByTestId('owner')).toHaveTextContent('Mine');
  });

  it('drops the reply of a fetch that a newer one replaced, and aborts it', async () => {
    // Everyone's experiments are held until the list of the user's own (Mine) is shown.
    const held: { release: () => void; signal?: AbortSignal }[] = [];
    vi.mocked(getExperiments).mockImplementation((params, options) => {
      const page = (experiment: BulkExperimentItem) => ({
        experiments: [experiment],
        pagination: { limit: 0, offset: 0, total: 1 },
      });
      if (params.userIds) return Promise.resolve(page(EXPERIMENT));
      return new Promise((resolve) =>
        held.push({ release: () => resolve(page(STALE_EXPERIMENT)), signal: options?.signal }),
      );
    });
    setup();
    await waitFor(() => expect(held.length).toBeGreaterThan(0));
    await settingsLoaded();

    await choose('owner', 'Mine');

    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    expect(held.every(({ signal }) => signal?.aborted)).toBe(true);
    await act(async () => {
      held.forEach(({ release }) => release());
      await new Promise((resolve) => setTimeout(resolve, 50));
    });
    expect(screen.queryByText('stale-run')).not.toBeInTheDocument();
    expect(screen.getByText('bert-finetune')).toBeInTheDocument();
    expect(screen.queryByText(/Unable to load/)).not.toBeInTheDocument();
  });

  it('reports no error for a fetch that fails after a newer one replaced it', async () => {
    // Everyone's runs are held until the list of the user's own (Mine) is shown, then fail.
    const held: { fail: () => void; signal?: AbortSignal }[] = [];
    vi.mocked(fetchRunPage).mockImplementation((query, signal) => {
      if (query.userId !== undefined) return real.fetchRunPage(query, signal);
      return new Promise((_resolve, reject) =>
        held.push({ fail: () => reject(new Error('late failure')), signal }),
      );
    });
    setup();
    await waitFor(() => expect(held.length).toBeGreaterThan(0));
    await settingsLoaded();

    await choose('owner', 'Mine');

    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    expect(held.every(({ signal }) => signal?.aborted)).toBe(true);
    await act(async () => {
      held.forEach(({ fail }) => fail());
      await new Promise((resolve) => setTimeout(resolve, 50));
    });
    expect(handleError).not.toHaveBeenCalled();
    expect(screen.getByText('bert-finetune')).toBeInTheDocument();
  });

  it('shows the sources of a TensorBoard: with flat runs, its searches and runs', async () => {
    vi.mocked(getTensorBoards).mockResolvedValue([
      {
        ...SHELL,
        id: 'tb-1',
        misc: { experimentIds: [5, 2], trialIds: [7] },
        name: 'loss-curves',
        type: CommandType.TensorBoard,
      },
    ]);
    setup();

    await user.click(await screen.findByRole('button', { name: 'Show 3 Sources' }));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('3 TensorBoard Sources')).toBeInTheDocument();
    expect(
      within(dialog)
        .getAllByRole('link')
        .map((link) => link.textContent),
    ).toEqual(['Run 7', 'Search 2', 'Search 5']);
  });

  it('searches after typing stops', async () => {
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();

    await user.type(screen.getByPlaceholderText('Search name or ID'), 'sweep');

    await waitFor(() => expect(lastListCall(getGenericTasks)?.search).toBe('sweep'));
    expect(lastListCall(getExperiments)?.name).toBe('sweep');
    await waitFor(() => expect(screen.queryByText('gpu-shell')).not.toBeInTheDocument());
  });

  it('shows an alert for a list that failed and lists the others', async () => {
    vi.mocked(getShells).mockRejectedValue(new Error('shells are down'));
    setup();

    expect(await screen.findByText(/Unable to load shells/)).toBeInTheDocument();
    expect(screen.getByText('bert-finetune')).toBeInTheDocument();
    expect(screen.queryByText('gpu-shell')).not.toBeInTheDocument();
  });

  it('shows no alert for a failed list of a kind that the chips leave out', async () => {
    vi.mocked(getShells).mockRejectedValue(new Error('shells are down'));
    setup();
    expect(await screen.findByText(/Unable to load shells/)).toBeInTheDocument();
    await settingsLoaded();

    await user.click(screen.getByTestId('kind-generic-task'));

    await waitFor(() =>
      expect(screen.queryByText(/Unable to load shells/)).not.toBeInTheDocument(),
    );
    expect(screen.getByText('eval-sweep')).toBeInTheDocument();
    // Its chip goes without a count.
    expect(screen.getByTestId('kind-shell')).toHaveTextContent(/^Shell$/);
  });

  describe('row menus', () => {
    it("are each kind's menu", async () => {
      setup();

      await user.click(
        within(await row('gpu-shell'))
          .getByTitle('Open actions menu')
          .querySelector('button') as HTMLElement,
      );
      await waitFor(() => expect(menuLabels()).toContain('Connect via CLI'));
      expect(menuLabels()[0]).toBe('View Logs');
      expect(isDangerMenuItem('Kill')).toBe(true);
      await user.keyboard('{Escape}');

      fireEvent.contextMenu(await row('bert-finetune'));
      await waitFor(() => expect(menuLabels()).toContain('Copy Experiment ID'));
    });

    it('of a generic task are in the order of the task menu, also on a right click', async () => {
      setup();
      fireEvent.contextMenu(await row('eval-sweep'));
      await screen.findByText('Copy Task ID');
      expect(menuLabels()).toEqual([
        'View Logs',
        'View Resources',
        'Copy Task ID',
        'Pause',
        'Kill',
      ]);
      expect(isDangerMenuItem('Kill')).toBe(true);
    });

    it('of a generic task share a failed unpause, to retry from the other menu', async () => {
      listed.genericState = GenericTaskState.Paused;
      vi.mocked(unpauseGenericTask).mockRejectedValueOnce(new Error('resume failed'));
      setup();
      await user.click(
        within(await row('eval-sweep'))
          .getByTitle('Open actions menu')
          .querySelector('button') as HTMLElement,
      );
      await user.click(await screen.findByText('Unpause'));
      // The failed unpause started the root, so the refreshed list reads it active.
      listed.genericState = GenericTaskState.Active;
      await user.click(await screen.findByRole('button', { name: 'Unpause' }));
      await waitFor(() => expect(handleError).toHaveBeenCalled());
      await user.click(screen.getByRole('button', { name: 'Cancel' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

      fireEvent.contextMenu(await row('eval-sweep'));
      await waitFor(() => expect(menuLabels()).toContain('Retry Unpause'));
      await user.click(openMenuItem('Retry Unpause'));
      const dialog = await screen.findByRole('dialog');
      await user.click(within(dialog).getByRole('button', { name: 'Retry Unpause' }));
      await waitFor(() => expect(unpauseGenericTask).toHaveBeenCalledTimes(2));
    });

    it('of a generic task disable Kill in one menu while a kill from the other runs', async () => {
      let finishKill = () => {};
      vi.mocked(killGenericTask).mockImplementationOnce(
        () => new Promise<void>((resolve) => (finishKill = resolve)),
      );
      setup();
      await user.click(
        within(await row('eval-sweep'))
          .getByTitle('Open actions menu')
          .querySelector('button') as HTMLElement,
      );
      await user.click(await screen.findByText('Kill'));
      await user.click(await screen.findByRole('button', { name: 'Kill' }));
      await waitFor(() => expect(killGenericTask).toHaveBeenCalled());

      fireEvent.contextMenu(await row('eval-sweep'));
      await waitFor(() => expect(isDisabledMenuItem('Kill')).toBe(true));

      finishKill();
      await waitFor(() => expect(isDisabledMenuItem('Kill')).toBe(false));
    });
  });

  describe('bulk Kill', () => {
    const selectRow = async (name: string) =>
      await user.click(within(await row(name)).getByRole('checkbox'));

    const openKillConfirmation = async () => {
      await user.click(screen.getByText('Select an action...'));
      const options = (await screen.findAllByTitle('Kill')).filter(
        (option) => !option.closest('.ant-select-dropdown-hidden'),
      );
      await user.click(options[options.length - 1]);
      return await screen.findByRole('dialog');
    };

    const killSelected = async () => {
      const dialog = await openKillConfirmation();
      await user.click(within(dialog).getByRole('button', { name: 'Kill' }));
    };

    const DESCENDANTS_NOTE = /generic task is killed together with all its descendants/;

    it('says that generic tasks are killed with their descendants', async () => {
      setup();
      await selectRow('gpu-shell');
      await selectRow('eval-sweep');

      const dialog = await openKillConfirmation();

      expect(within(dialog).getByText(DESCENDANTS_NOTE)).toBeInTheDocument();
    });

    it('says nothing of descendants without a generic task to kill', async () => {
      setup();
      await selectRow('gpu-shell');

      const dialog = await openKillConfirmation();

      expect(within(dialog).getByText(/Are you sure/)).toBeInTheDocument();
      expect(within(dialog).queryByText(DESCENDANTS_NOTE)).not.toBeInTheDocument();
    });

    it('kills the selected runs of every kind with their own API', async () => {
      setup();
      await selectRow('gpu-shell');
      await selectRow('eval-sweep');
      await selectRow('bert-finetune');

      await killSelected();

      await waitFor(() => expect(killExperiment).toHaveBeenCalledWith({ experimentId: 42 }));
      expect(killGenericTask).toHaveBeenCalledWith({ taskId: 'task-1' });
      expect(killTask).toHaveBeenCalledWith(
        expect.objectContaining({ id: 'shell-1', type: 'shell' }),
      );
      expect(handleError).not.toHaveBeenCalled();
    });

    it('kills only the selected runs that the user may kill', async () => {
      access.onlyOwnRunsOf = CURRENT_USER_ID;
      listed.experimentState = RunState.Completed;
      setup();
      await settingsLoaded();
      // Another user's notebook and an experiment that has ended.
      await selectRow('cpu-notebook');
      await selectRow('bert-finetune');

      await user.click(screen.getByText('Select an action...'));
      const options = (await screen.findAllByTitle('Kill')).filter(
        (option) => !option.closest('.ant-select-dropdown-hidden'),
      );
      expect(options[options.length - 1]).toHaveClass('ant-select-item-option-disabled');
      await user.keyboard('{Escape}');

      // The user's own running shell.
      await selectRow('gpu-shell');
      await killSelected();

      await waitFor(() => expect(killTask).toHaveBeenCalledTimes(1));
      expect(killTask).toHaveBeenCalledWith(expect.objectContaining({ id: 'shell-1' }));
      expect(killExperiment).not.toHaveBeenCalled();
      expect(killGenericTask).not.toHaveBeenCalled();
    });

    describe('of generic tasks', () => {
      const OTHER_GENERIC: GenericTask = {
        ...GENERIC,
        jobId: 'job-2',
        name: 'data-prep',
        taskId: 'task-2',
      };

      beforeEach(() => {
        vi.mocked(getGenericTasks).mockImplementation((params) =>
          Promise.resolve({
            pagination: { limit: 0, offset: 0, total: params.limit === 1 ? 5 : 2 },
            tasks: [GENERIC, OTHER_GENERIC],
          }),
        );
      });

      const killedTaskIds = () =>
        vi.mocked(killGenericTask).mock.calls.map(([params]) => params.taskId);

      it('kills them one after another, as the master takes one kill at a time', async () => {
        let finishFirstKill = () => {};
        vi.mocked(killGenericTask).mockImplementationOnce(
          () => new Promise<void>((resolve) => (finishFirstKill = resolve)),
        );
        setup();
        await selectRow('eval-sweep');
        await selectRow('data-prep');
        await selectRow('gpu-shell');

        await killSelected();

        // The other kinds do not wait.
        await waitFor(() => expect(killTask).toHaveBeenCalledTimes(1));
        expect(killedTaskIds()).toEqual(['task-1']);
        await act(() => new Promise((resolve) => setTimeout(resolve, 50)));
        expect(killedTaskIds()).toEqual(['task-1']);

        finishFirstKill();

        await waitFor(() => expect(killedTaskIds()).toEqual(['task-1', 'task-2']));
        await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
        expect(handleError).not.toHaveBeenCalled();
      });

      it('goes on after a kill that fails, and reports it', async () => {
        vi.mocked(killGenericTask).mockRejectedValueOnce(new Error('in progress'));
        setup();
        await selectRow('eval-sweep');
        await selectRow('data-prep');

        await killSelected();

        await waitFor(() => expect(handleError).toHaveBeenCalled());
        expect(killedTaskIds()).toEqual(['task-1', 'task-2']);
        const [, options] = vi.mocked(handleError).mock.calls[0];
        expect(options?.publicMessage).toBe(
          'Could not kill 1 generic task. Please try again later.',
        );
      });
    });

    it('reports the failures by kind', async () => {
      vi.mocked(killExperiment).mockRejectedValueOnce(new Error('no'));
      vi.mocked(killTask).mockRejectedValueOnce(new Error('no'));
      setup();
      await selectRow('gpu-shell');
      await selectRow('eval-sweep');
      await selectRow('bert-finetune');

      await killSelected();

      await waitFor(() => expect(handleError).toHaveBeenCalled());
      const [, options] = vi.mocked(handleError).mock.calls[0];
      expect(options?.publicMessage).toMatch(/1 experiment and 1 shell/);
      expect(killGenericTask).toHaveBeenCalled();
    });
  });
});
