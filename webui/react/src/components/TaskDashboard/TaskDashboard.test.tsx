import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { Loadable } from 'hew/utils/loadable';
import React, { useEffect } from 'react';
import { DndProvider } from 'react-dnd';
import { HTML5Backend } from 'react-dnd-html5-backend';
import { BrowserRouter, MemoryRouter } from 'react-router-dom';

import { ARIA_LABEL_CONTAINER } from 'components/Table/TableFilterDropdown';
import { ThemeProvider } from 'components/ThemeProvider';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import {
  getAgents,
  getExperiments,
  getGenericTasks,
  getJupyterLabs,
  getShells,
  getTensorBoards,
  getUsers,
  getUserSetting,
  killExperiment,
  killGenericTask,
  killTask,
  unpauseGenericTask,
  updateUserSetting,
} from 'services/api';
import authStore from 'stores/auth';
import clusterStore from 'stores/cluster';
import userStore from 'stores/users';
import userSettings from 'stores/userSettings';
import {
  Agent,
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
import { MIN_SORT_FILTER_WIDTHS } from './TaskDashboard.settings';

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
  getAgents: vi.fn(() => Promise.resolve([])),
  getCommands: vi.fn(() => Promise.resolve([])),
  getCurrentUser: () => Promise.resolve({ id: 3, isActive: true, isAdmin: false, username: 'me' }),
  getExperiments: vi.fn(),
  getGenericTasks: vi.fn(),
  getJupyterLabs: vi.fn(),
  getShells: vi.fn(),
  getTensorBoards: vi.fn(() => Promise.resolve([])),
  getUsers: vi.fn(() => Promise.resolve({ users: [] })),
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
const NEW_WIDTHS = [...OLD_WIDTHS.slice(0, 7), 92, ...OLD_WIDTHS.slice(7)];

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

/** The parameters of the latest list call of a paged source. */
const lastListCall = <P extends object>(
  fn: (params: P, options?: FetchOptions) => Promise<unknown>,
): P | undefined => vi.mocked(fn).mock.calls.at(-1)?.[0];

const row = async (name: string) => (await screen.findByText(name)).closest('tr') as HTMLElement;

/** Waits for the stored settings, which drop an update while they load. */
const settingsLoaded = () =>
  waitFor(() => expect(Loadable.isLoaded(userSettings.getAll().get())).toBe(true));

const STALE_EXPERIMENT: BulkExperimentItem = { ...EXPERIMENT, id: 41, name: 'stale-run' };

/** The open filter dropdown, shown at once instead of after its animation. */
const openDropdown = async (): Promise<HTMLElement> => {
  const container = await waitFor(() => {
    const open = screen
      .getAllByLabelText(ARIA_LABEL_CONTAINER)
      .find((el) => !el.closest('.ant-dropdown-hidden'));
    expect(open).toBeDefined();
    return open as HTMLElement;
  });
  const dropdown = container.closest('.ant-dropdown') as HTMLElement;
  dropdown.style.removeProperty('opacity');
  dropdown.style.removeProperty('pointer-events');
  return container;
};

/** Opens the filter of a column by its funnel. */
const openFilter = async (label: string): Promise<HTMLElement> => {
  await user.click(screen.getByRole('button', { name: label }));
  return await openDropdown();
};

const options = (dropdown: HTMLElement) =>
  within(dropdown)
    .getAllByRole('option')
    .map((option) => option.textContent);

const ticked = (dropdown: HTMLElement) =>
  within(dropdown)
    .getAllByRole('option')
    .filter((option) => option.getAttribute('aria-selected') === 'true')
    .map((option) => option.textContent);

/** Ticks or unticks options of the open filter by their text, then applies it with OK. */
const filterBy = async (label: string, texts: string[]) => {
  const dropdown = await openFilter(label);
  for (const text of texts) {
    await user.click(within(dropdown).getByRole('option', { name: text }));
  }
  await user.click(within(dropdown).getByRole('button', { name: 'OK' }));
};

const AGENT = (id: string, slots: number): Agent => ({
  id,
  registeredTime: 0,
  resourcePools: ['default'],
  resources: [],
  slotStats: {
    brandStats: {},
    typeStats: { TYPE_CUDA: { disabled: 0, draining: 0, states: {}, total: slots } },
  },
});

/** The keys of the Jobs page's view in the URL of the browser. */
const urlParams = () => new URLSearchParams(window.location.search);

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
    // The toolbar has the search and the launch buttons only.
    for (const testId of ['owner', 'slots', 'kind-experiment']) {
      expect(screen.queryByTestId(testId)).not.toBeInTheDocument();
    }
    expect(screen.queryByText('Clear Filters', { exact: false })).not.toBeInTheDocument();
    expect(screen.queryByText(/listed for 24 hours/)).not.toBeInTheDocument();
  });

  it('sorts from the column headers, newest first by default, each click flipping the sort', async () => {
    setup({}, '/jobs', { browser: true });
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();
    expect(lastListCall(getExperiments)).toMatchObject({
      orderBy: 'ORDER_BY_DESC',
      sortBy: 'SORT_BY_START_TIME',
    });
    expect(header('Started')).toHaveAttribute('aria-sort', 'descending');

    for (const [desc, orderBy] of [
      [false, 'ORDER_BY_ASC'],
      [true, 'ORDER_BY_DESC'],
      [false, 'ORDER_BY_ASC'],
    ] as const) {
      await user.click(within(header('Name')).getByText('Name'));
      await waitFor(() =>
        expect(lastListCall(getExperiments)).toMatchObject({ orderBy, sortBy: 'SORT_BY_NAME' }),
      );
      expect(lastListCall(getGenericTasks)).toMatchObject({ orderBy, sortBy: 'SORT_BY_NAME' });
      expect(urlParams().get('sortKey')).toBe('name');
      // A default is left out of the URL.
      expect(urlParams().get('sortDesc') ?? 'true').toBe(String(desc));
    }
    expect(header('Name')).toHaveAttribute('aria-sort', 'ascending');
    expect(header('Started')).not.toHaveAttribute('aria-sort');

    // Slots sorts most first; the state sort is by state group.
    await user.click(within(header('Slots')).getByText('Slots'));
    await waitFor(() =>
      expect(lastListCall(getExperiments)).toMatchObject({
        orderBy: 'ORDER_BY_DESC',
        sortBy: 'SORT_BY_SLOTS',
      }),
    );
    await user.click(within(header('State')).getByText('State'));
    await waitFor(() =>
      expect(lastListCall(getGenericTasks)).toMatchObject({
        orderBy: 'ORDER_BY_ASC',
        sortBy: 'SORT_BY_STATE_GROUP',
      }),
    );
  }, 30_000);

  it('keeps a page under a non-default sort, and goes to the first page on a new sort', async () => {
    storeBeforeLoad({ sortDesc: false, sortKey: 'name', tableLimit: 2 });
    setup({}, '/jobs', { browser: true });
    expect(await screen.findByText('bert-finetune', {}, AFTER_LOAD)).toBeInTheDocument();
    await waitFor(
      () =>
        expect(lastListCall(getExperiments)).toMatchObject({ limit: 2, sortBy: 'SORT_BY_NAME' }),
      AFTER_LOAD,
    );
    // By name: bert-finetune, cpu-notebook | eval-sweep, gpu-shell.
    expect(screen.queryByText('eval-sweep')).not.toBeInTheDocument();

    await user.click(screen.getByTitle('2'));

    await waitFor(() => expect(screen.getByText('eval-sweep')).toBeInTheDocument());
    expect(screen.getByText('gpu-shell')).toBeInTheDocument();
    expect(screen.queryByText('bert-finetune')).not.toBeInTheDocument();
    // Each paged source sends its first offset + limit runs in the sort's order.
    expect(lastListCall(getExperiments)).toMatchObject({
      limit: 4,
      offset: 0,
      orderBy: 'ORDER_BY_ASC',
      sortBy: 'SORT_BY_NAME',
    });
    expect(stored()).toMatchObject({ sortKey: 'name', tableOffset: 2 });

    await user.click(within(header('Name')).getByText('Name'));
    await waitFor(() => expect(stored()).toMatchObject({ sortDesc: true, tableOffset: 0 }));
  }, 30_000);

  it('leaves experiments out of the tasks-only view', async () => {
    setup({ tasksOnly: true }, '/tasks');

    expect(await screen.findByText('eval-sweep')).toBeInTheDocument();
    expect(getExperiments).not.toHaveBeenCalled();
    expect(screen.queryByText('bert-finetune')).not.toBeInTheDocument();
    expect(options(await openFilter('Filter by kind'))).toEqual([
      'Generic Task',
      'JupyterLab',
      'Shell',
      'Command',
      'TensorBoard',
    ]);
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
    expect(screen.queryByTestId('shell-button')).not.toBeInTheDocument();
    expect(options(await openFilter('Filter by kind'))).toEqual(['Experiment', 'Generic Task']);
    expect(screen.queryByRole('button', { name: 'Filter by workspace' })).not.toBeInTheDocument();
  });

  it('lists only the kinds ticked in the Kind filter, and fetches only those', async () => {
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();

    await filterBy('Filter by kind', ['Generic Task']);

    await waitFor(() => expect(screen.queryByText('bert-finetune')).not.toBeInTheDocument());
    expect(screen.getByText('eval-sweep')).toBeInTheDocument();
    expect(screen.queryByText('gpu-shell')).not.toBeInTheDocument();
    expect(header('Kind').className).toMatch(/headerFilterOn/);
    const experimentCalls = vi.mocked(getExperiments).mock.calls.length;
    const shellCalls = vi.mocked(getShells).mock.calls.length;
    await act(() => new Promise((resolve) => setTimeout(resolve, 100)));
    expect(vi.mocked(getExperiments).mock.calls).toHaveLength(experimentCalls);
    expect(vi.mocked(getShells).mock.calls).toHaveLength(shellCalls);
    expect(screen.getByRole('button', { name: 'Clear Filters (1)' })).toBeInTheDocument();

    // Every kind ticked is no filter, so that no kind is ever left out.
    const dropdown = await openFilter('Filter by kind');
    await user.click(within(dropdown).getByRole('button', { name: 'All' }));
    await user.click(within(dropdown).getByRole('button', { name: 'OK' }));
    await waitFor(() => expect(screen.getByText('bert-finetune')).toBeInTheDocument());
    await waitFor(() => expect(stored().type).toBeUndefined());
    await waitFor(() =>
      expect(screen.queryByText('Clear Filters', { exact: false })).not.toBeInTheDocument(),
    );
  });

  it("applies the URL's kind filter, as /tasks/generic redirects with it", async () => {
    setup({ tasksOnly: true }, '/tasks?type=generic-task');

    expect(await screen.findByText('eval-sweep')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText('gpu-shell')).not.toBeInTheDocument());
    expect(screen.queryByText('cpu-notebook')).not.toBeInTheDocument();
    expect(ticked(await openFilter('Filter by kind'))).toEqual(['Generic Task']);
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
    await waitFor(
      () => expect(screen.queryByText('bert-finetune')).not.toBeInTheDocument(),
      AFTER_LOAD,
    );
    expect(screen.getByText('gpu-shell')).toBeInTheDocument();
  }, 30_000);

  it('keeps old ?type values of the task list working', async () => {
    setup({ tasksOnly: true }, '/tasks?type=jupyter-lab');

    expect(await screen.findByText('cpu-notebook', {}, AFTER_LOAD)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText('gpu-shell')).not.toBeInTheDocument());
  });

  it('sets the whole view from a URL with any of its keys: no filter or the default sort where it has none', async () => {
    storeBeforeLoad({ sortDesc: false, sortKey: 'name', type: ['shell'], user: [4] });
    setup({}, '/jobs?state=paused', { browser: true });

    await waitFor(() => expect(stored().state).toEqual(['paused']), AFTER_LOAD);
    expect(stored()).toMatchObject({ sortDesc: true, sortKey: 'startTime' });
    expect(stored().type).toBeUndefined();
    expect(stored().user).toBeUndefined();
    await waitFor(() =>
      expect(lastListCall(getExperiments)).toMatchObject({
        sortBy: 'SORT_BY_START_TIME',
        states: ['STATE_PAUSED'],
      }),
    );
    expect(lastListCall(getExperiments)?.userIds).toBeUndefined();
    expect(urlParams().getAll('state')).toEqual(['paused']);
  }, 30_000);

  it('opens the saved view at a URL without its keys', async () => {
    storeBeforeLoad({ sortDesc: false, sortKey: 'name', state: ['ended'] });
    setup({}, '/jobs', { browser: true });

    await waitFor(
      () =>
        expect(lastListCall(getExperiments)).toMatchObject({
          orderBy: 'ORDER_BY_ASC',
          sortBy: 'SORT_BY_NAME',
          states: ['STATE_COMPLETED', 'STATE_CANCELED', 'STATE_ERROR', 'STATE_DELETE_FAILED'],
        }),
      AFTER_LOAD,
    );
    expect(stored()).toMatchObject({ sortKey: 'name', state: ['ended'] });
  }, 30_000);

  it('turns what 0.41.0 saved into the filters of now, before the first fetch, and saves them once', async () => {
    storeBeforeLoad({ owner: 'mine', slots: 'gpu', workspace: 7 });
    setup();

    await waitFor(() => expect(getExperiments).toHaveBeenCalled(), AFTER_LOAD);
    await waitFor(() => expect(saved('slots')).toEqual(['multi:0']), AFTER_LOAD);
    for (const [params] of vi.mocked(getExperiments).mock.calls) {
      expect(params).toMatchObject({
        slotsAbove: 0,
        userIds: [CURRENT_USER_ID],
        workspaceIds: [7],
      });
    }
    expect(saved('user')).toEqual([CURRENT_USER_ID]);
    expect(saved('workspace')).toEqual([7]);
    expect(saved('owner')).toBeUndefined();
    expect(stored().owner).toBeUndefined();
    expect(screen.getByRole('button', { name: 'Clear Filters (3)' })).toBeInTheDocument();
    // Once: a later load reads the saved values as they are.
    const updates = vi.mocked(updateUserSetting).mock.calls.length;
    await act(() => new Promise((resolve) => setTimeout(resolve, 300)));
    expect(vi.mocked(updateUserSetting).mock.calls).toHaveLength(updates);
  }, 30_000);

  it('filters by owner, the signed-in user first, on the master and here for tasks', async () => {
    vi.mocked(getUsers).mockResolvedValue({
      pagination: { total: 3 },
      // Users of no listed run, whose owners stay without a user record.
      users: [
        { displayName: 'Zoe', id: 9, isActive: true, isAdmin: false, username: 'zoe' },
        { displayName: 'alice', id: 11, isActive: true, isAdmin: false, username: 'al' },
      ],
    });
    userStore.fetchUsers();
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();
    expect(lastListCall(getGenericTasks)?.userIds).toBeUndefined();

    const dropdown = await openFilter('Filter by owner');
    await waitFor(() => expect(options(dropdown)).toEqual(['me', 'alice', 'Zoe']));
    await user.click(within(dropdown).getByRole('option', { name: 'me' }));
    await user.click(within(dropdown).getByRole('button', { name: 'OK' }));

    await waitFor(() => expect(lastListCall(getGenericTasks)?.userIds).toEqual([CURRENT_USER_ID]));
    expect(lastListCall(getExperiments)?.userIds).toEqual([CURRENT_USER_ID]);
    expect(vi.mocked(getShells).mock.calls.at(-1)?.[0]).toMatchObject({
      users: [String(CURRENT_USER_ID)],
    });
    expect(header('Owner').className).toMatch(/headerFilterOn/);
  });

  it('filters by slot counts and Multi-node, listing 0 to the most slots of any agent', async () => {
    vi.mocked(getAgents).mockResolvedValue([AGENT('a', 8), AGENT('b', 2)]);
    clusterStore.fetchAgents();
    setup();
    expect(await screen.findByText('cpu-notebook')).toBeInTheDocument();
    await settingsLoaded();

    const dropdown = await openFilter('Filter by slots');
    await waitFor(() =>
      expect(options(dropdown)).toEqual([
        '0',
        '1',
        '2',
        '3',
        '4',
        '5',
        '6',
        '7',
        '8',
        'Multi-node',
      ]),
    );
    await user.click(within(dropdown).getByRole('option', { name: '0' }));
    await user.click(within(dropdown).getByRole('option', { name: 'Multi-node' }));
    await user.click(within(dropdown).getByRole('button', { name: 'OK' }));

    await waitFor(() =>
      expect(lastListCall(getGenericTasks)).toMatchObject({ slots: [0], slotsAbove: 8 }),
    );
    expect(lastListCall(getExperiments)).toMatchObject({ slots: [0], slotsAbove: 8 });
    await waitFor(() => expect(screen.queryByText('gpu-shell')).not.toBeInTheDocument());
    expect(screen.getByText('cpu-notebook')).toBeInTheDocument();
    await waitFor(() => expect(stored().slots).toEqual(['0', 'multi:8']));
  });

  it('opens a funnel with Enter without sorting, and goes round the filter with Tab', async () => {
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();

    screen.getByRole('button', { name: 'Filter by state' }).focus();
    await user.keyboard('{Enter}');
    const dropdown = await openDropdown();
    const list = within(dropdown).getByRole('listbox');
    await waitFor(() => expect(list).toHaveFocus());
    // The header sorts on Enter, but not from its funnel.
    expect(stored().sortKey).toBeUndefined();
    expect(
      vi
        .mocked(getExperiments)
        .mock.calls.every(([params]) => params.sortBy === 'SORT_BY_START_TIME'),
    ).toBe(true);

    // Active, Paused, Ended: Down to Paused, Space ticks it.
    await user.keyboard('{ArrowDown}{ }');
    expect(ticked(dropdown)).toEqual(['Paused']);
    await user.keyboard('{Tab}');
    expect(within(dropdown).getByRole('button', { name: 'All' })).toHaveFocus();
    await user.keyboard('{Tab}');
    expect(within(dropdown).getByRole('button', { name: 'None' })).toHaveFocus();
    await user.keyboard('{Tab}');
    expect(within(dropdown).getByRole('button', { name: 'OK' })).toHaveFocus();
    await user.keyboard('{Tab}');
    expect(list).toHaveFocus();
    await user.keyboard('{Shift>}{Tab}{/Shift}');
    expect(within(dropdown).getByRole('button', { name: 'OK' })).toHaveFocus();
    await user.keyboard('{Enter}');

    await waitFor(() => expect(lastListCall(getExperiments)?.states).toEqual(['STATE_PAUSED']));
    expect(stored().state).toEqual(['paused']);
  });

  it('drops the ticks on Escape, and ticks all but one on a Ctrl+click', async () => {
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();

    let dropdown = await openFilter('Filter by state');
    await user.click(within(dropdown).getByRole('option', { name: 'Ended' }));
    expect(ticked(dropdown)).toEqual(['Ended']);
    await user.keyboard('{Escape}');
    await act(() => new Promise((resolve) => setTimeout(resolve, 50)));
    expect(stored().state).toBeUndefined();

    dropdown = await openFilter('Filter by state');
    expect(ticked(dropdown)).toEqual([]);
    await user.keyboard('{Control>}');
    await user.click(within(dropdown).getByRole('option', { name: 'Ended' }));
    await user.keyboard('{/Control}');
    expect(ticked(dropdown)).toEqual(['Active', 'Paused']);
    await user.click(within(dropdown).getByRole('button', { name: 'None' }));
    expect(ticked(dropdown)).toEqual([]);
  });

  it('clears the filters, and only them, with Clear Filters', async () => {
    storeBeforeLoad({
      columns: NEW_COLUMNS,
      columnWidths: NEW_WIDTHS,
      sortKey: 'name',
      state: ['active'],
      type: ['shell', 'experiment'],
    });
    setup();
    const clear = await screen.findByRole('button', { name: 'Clear Filters (2)' }, AFTER_LOAD);

    await user.click(clear);

    await waitFor(() => expect(stored().state).toBeUndefined());
    expect(stored().type).toBeUndefined();
    expect(stored()).toMatchObject({ columnWidths: NEW_WIDTHS, sortKey: 'name' });
    await waitFor(() =>
      expect(screen.queryByText('Clear Filters', { exact: false })).not.toBeInTheDocument(),
    );
  }, 30_000);

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
          slots: 90,
          startTime: 308,
          state: 304,
          user: 305,
        });
        // ID, Resource Pool and Ended are hidden below the md breakpoint, as in tests. The table
        // takes the new widths a render later, which takes seconds in a full run.
        await waitFor(() => expect(shownWidths()).toMatchObject({ Slots: '90px' }), AFTER_LOAD);
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
      expect(storedWidths()).toMatchObject({ id: 100, kind: 84, name: 240, slots: 90 });
      // The widths keep their count, which the table alone would not take up.
      await waitFor(() => expect(shownWidths()).toMatchObject({ Kind: '84px' }), AFTER_LOAD);
      expect(shownWidths()).toMatchObject({ Name: '240px', Slots: '90px', State: '120px' });
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
          Slots: '92px',
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
        expect(saved('columnWidths')).toEqual([301, 302, 450, 304, 305, 306, 307, 92, 308, 309]),
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
      expect(saved('columnWidths')).toEqual([301, 302, 304, 303, 305, 306, 307, 92, 308, 309]);

      // The move gives the table and the settings one array of widths; the resize changes the
      // table's own.
      await resize('Kind', 500);
      await waitFor(() =>
        expect(saved('columnWidths')).toEqual([500, 302, 304, 303, 305, 306, 307, 92, 308, 309]),
      );
      expect(shownWidths()).toMatchObject({ Kind: '500px', Name: '303px', State: '304px' });
    }, 30_000);
  });

  it('shows the rows once the stored settings have loaded', async () => {
    let load: (response: { settings: [] }) => void = () => undefined;
    userSettings.reset();
    vi.mocked(getUserSetting).mockReturnValueOnce(new Promise((resolve) => (load = resolve)));
    setup();
    await act(() => new Promise((resolve) => setTimeout(resolve, 300)));
    // Nothing is fetched before the saved filters are known.
    expect(getExperiments).not.toHaveBeenCalled();
    expect(screen.queryByText('bert-finetune')).not.toBeInTheDocument();

    load({ settings: [] });
    expect(await screen.findByText('bert-finetune', {}, AFTER_LOAD)).toBeInTheDocument();
  });

  it('fetches nothing on a column resize', async () => {
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();
    const calls = vi.mocked(getExperiments).mock.calls.length;

    await resize('Name', 450);
    await waitFor(() => expect(storedWidths()).toMatchObject({ name: 450 }));
    await act(() => new Promise((resolve) => setTimeout(resolve, 200)));

    expect(vi.mocked(getExperiments).mock.calls).toHaveLength(calls);
  });

  it('stores the columns with the widths of a resize', async () => {
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();

    await resize('Name', 450);
    // Widths stored alone would be bound by place to the default columns of a later version.
    await waitFor(() =>
      expect(saved('columnWidths')).toEqual([84, 100, 450, 120, 115, 230, 130, 90, 117, 117]),
    );
    expect(saved('columns')).toEqual(NEW_COLUMNS);
    expect(storedWidths()).toMatchObject({ kind: 84, name: 450, slots: 90 });
  });

  it('resizes a column narrower than its default width, down to the minimum', async () => {
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();

    await resize('Name', 120);
    await waitFor(() => expect(storedWidths()).toMatchObject({ name: 120 }));
    expect(shownWidths()).toMatchObject({ Name: '120px' });

    // Wide enough for the title and its sort arrows.
    await resize('Name', 10);
    await waitFor(() =>
      expect(storedWidths()).toMatchObject({ name: MIN_SORT_FILTER_WIDTHS.name }),
    );
    expect(shownWidths()).toMatchObject({ Name: `${MIN_SORT_FILTER_WIDTHS.name}px` });
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

  it('cuts a long owner, location or pool short, with the whole of it in a title', async () => {
    // A medium screen or wider, which shows these columns.
    vi.stubGlobal('matchMedia', (media: string) => ({
      addEventListener: vi.fn(),
      addListener: vi.fn(),
      dispatchEvent: vi.fn(),
      matches: true,
      media,
      onchange: null,
      removeEventListener: vi.fn(),
      removeListener: vi.fn(),
    }));
    try {
      setup();
      expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
      const cell = (name: string, title: string) => {
        const at = screen
          .getAllByRole('columnheader')
          .findIndex((th) => th.textContent?.trim().startsWith(title));
        expect(at).toBeGreaterThan(-1);
        return screen.getByText(name).closest('tr')?.children[at] as HTMLElement;
      };
      // The table shows these columns once it has seen the screen's width.
      await waitFor(
        () => expect(cell('eval-sweep', 'Resource Pool')).toHaveTextContent('default'),
        AFTER_LOAD,
      );

      for (const title of ['Owner', 'Workspace › Project', 'Resource Pool']) {
        expect(cell('bert-finetune', title)).toHaveClass('ant-table-cell-ellipsis');
      }
      const location = cell('bert-finetune', 'Workspace › Project');
      expect(within(location).getByTitle('Uncategorized › Uncategorized')).toBeInTheDocument();
      expect(cell('eval-sweep', 'Workspace › Project').querySelector('[title]')).toHaveAttribute(
        'title',
        '— › Project 1',
      );
      expect(cell('bert-finetune', 'Resource Pool')).toHaveAttribute('title', 'default');
      // A generic task's owner without a user record is its username.
      expect(cell('eval-sweep', 'Owner')).toHaveAttribute('title', 'bob');
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('keeps the filters of the Jobs page and of the tasks-only view apart', async () => {
    const jobs = setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();
    await filterBy('Filter by owner', ['me']);
    await waitFor(() => expect(lastListCall(getGenericTasks)?.userIds).toEqual([CURRENT_USER_ID]));
    jobs.unmount();

    vi.mocked(getGenericTasks).mockClear();
    const tasks = setup({ tasksOnly: true }, '/tasks');
    expect(await screen.findByText('eval-sweep', {}, AFTER_LOAD)).toBeInTheDocument();
    expect(lastListCall(getGenericTasks)?.userIds).toBeUndefined();
    expect(header('Owner').className).not.toMatch(/headerFilterOn/);
    tasks.unmount();

    // The Jobs page kept its own filter.
    vi.mocked(getGenericTasks).mockClear();
    setup();
    await waitFor(() => expect(lastListCall(getGenericTasks)?.userIds).toEqual([CURRENT_USER_ID]));
    expect(await screen.findByText('eval-sweep', {}, AFTER_LOAD)).toBeInTheDocument();
    expect(header('Owner').className).toMatch(/headerFilterOn/);
  });

  it('drops the reply of a fetch that a newer one replaced, and aborts it', async () => {
    // After the first list, everyone's experiments are held until the user's own are shown.
    const held: { release: () => void; signal?: AbortSignal }[] = [];
    let hold = false;
    vi.mocked(getExperiments).mockImplementation((params, options) => {
      const page = (experiment: BulkExperimentItem) => ({
        experiments: [experiment],
        pagination: { limit: 0, offset: 0, total: 1 },
      });
      if (params.userIds) return Promise.resolve(page(EXPERIMENT));
      if (!hold) return Promise.resolve(page({ ...EXPERIMENT, id: 40, name: 'first-run' }));
      return new Promise((resolve) =>
        held.push({ release: () => resolve(page(STALE_EXPERIMENT)), signal: options?.signal }),
      );
    });
    setup();
    expect(await screen.findByText('first-run')).toBeInTheDocument();
    await settingsLoaded();
    hold = true;
    await user.type(screen.getByPlaceholderText('Search name or ID'), 'run');
    await waitFor(() => expect(held.length).toBeGreaterThan(0));

    await filterBy('Filter by owner', ['me']);

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
    // After the first list, everyone's runs are held until the user's own are shown, then fail.
    const held: { fail: () => void; signal?: AbortSignal }[] = [];
    let hold = false;
    vi.mocked(fetchRunPage).mockImplementation((query, signal) => {
      if (query.userIds !== undefined || !hold) return real.fetchRunPage(query, signal);
      return new Promise((_resolve, reject) =>
        held.push({ fail: () => reject(new Error('late failure')), signal }),
      );
    });
    setup();
    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await settingsLoaded();
    hold = true;
    await user.type(screen.getByPlaceholderText('Search name or ID'), 'bert');
    await waitFor(() => expect(held.length).toBeGreaterThan(0));

    await filterBy('Filter by owner', ['me']);

    expect(await screen.findByText('bert-finetune')).toBeInTheDocument();
    await waitFor(() => expect(held.every(({ signal }) => signal?.aborted)).toBe(true));
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

  it('shows no alert for a failed list of a kind that the Kind filter leaves out', async () => {
    vi.mocked(getShells).mockRejectedValue(new Error('shells are down'));
    setup();
    expect(await screen.findByText(/Unable to load shells/)).toBeInTheDocument();
    await settingsLoaded();

    await filterBy('Filter by kind', ['Generic Task']);

    await waitFor(() =>
      expect(screen.queryByText(/Unable to load shells/)).not.toBeInTheDocument(),
    );
    expect(screen.getByText('eval-sweep')).toBeInTheDocument();
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
