import {
  getCommands,
  getExperiments,
  getGenericTasks,
  getJupyterLabs,
  getShells,
  getTensorBoards,
} from 'services/api';
import {
  BulkExperimentItem,
  CommandState,
  CommandTask,
  CommandType,
  FetchOptions,
  GenericTask,
  GenericTaskState,
  RunState,
} from 'types';

import { fetchRunPage, RunQuery } from './fetchRuns';
import { kindsOf, RunKind, slotsQuery, SortKey, StateGroup } from './runRows';

vi.mock('services/api', () => ({
  getCommands: vi.fn(),
  getExperiments: vi.fn(),
  getGenericTasks: vi.fn(),
  getJupyterLabs: vi.fn(),
  getShells: vi.fn(),
  getTensorBoards: vi.fn(),
}));

const command = (
  id: string,
  type: CommandType,
  startTime: string,
  overrides: Partial<CommandTask> = {},
): CommandTask => ({
  id,
  name: id,
  resourcePool: 'default',
  slots: 0,
  startTime,
  state: CommandState.Running,
  type,
  userId: 1,
  workspaceId: 1,
  ...overrides,
});

const generic = (taskId: string, startTime: string): GenericTask => ({
  description: '',
  jobId: `job-${taskId}`,
  name: taskId,
  noPause: false,
  projectId: 1,
  resourcePool: 'default',
  slots: 1,
  startTime,
  state: GenericTaskState.Active,
  taskId,
  userId: 1,
  username: 'alice',
  workspaceId: 1,
});

const experiment = (id: number, startTime: string): BulkExperimentItem => ({
  archived: false,
  hyperparameters: {},
  id,
  jobId: `job-${id}`,
  labels: [],
  name: `exp ${id}`,
  numTrials: 1,
  projectId: 1,
  resourcePool: 'default',
  searcherType: 'single',
  startTime,
  state: RunState.Active,
  userId: 1,
});

const genericPage = (tasks: GenericTask[], total = tasks.length) => ({
  pagination: { limit: 0, offset: 0, total },
  tasks,
});
const experimentPage = (experiments: BulkExperimentItem[], total = experiments.length) => ({
  experiments,
  pagination: { limit: 0, offset: 0, total },
});

type PagedList<P> = (params: P, options?: FetchOptions) => Promise<unknown>;
/** The parameters of a paged source's one call. */
const listCall = <P>(fn: PagedList<P>): P | undefined => vi.mocked(fn).mock.calls[0]?.[0];

const globalJobs = (overrides: Partial<RunQuery> = {}): RunQuery => ({
  kinds: kindsOf({ type: 'global' }, true),
  limit: 20,
  offset: 0,
  scope: { type: 'global' },
  ...overrides,
});

describe('fetchRunPage', () => {
  beforeEach(() => {
    vi.mocked(getCommands).mockResolvedValue([
      command('cmd-gpu', CommandType.Command, '2026-01-05T00:00:00Z', { slots: 2 }),
    ]);
    vi.mocked(getJupyterLabs).mockResolvedValue([
      command('nb-cpu', CommandType.JupyterLab, '2026-01-03T00:00:00Z'),
      command('nb-ended', CommandType.JupyterLab, '2026-01-01T00:00:00Z', {
        state: CommandState.Terminated,
        userId: 2,
        workspaceId: 4,
      }),
    ]);
    vi.mocked(getShells).mockResolvedValue([]);
    vi.mocked(getTensorBoards).mockResolvedValue([]);
    vi.mocked(getGenericTasks).mockResolvedValue(
      genericPage([generic('g1', '2026-01-04T00:00:00Z')], 7),
    );
    vi.mocked(getExperiments).mockResolvedValue(
      experimentPage([experiment(5, '2026-01-02T00:00:00Z')], 11),
    );
  });

  afterEach(() => vi.clearAllMocks());

  it('lists every kind on the global Jobs page, merged newest first', async () => {
    const page = await fetchRunPage(globalJobs());

    expect(page.rows.map((row) => row.key)).toEqual([
      'command:cmd-gpu',
      'generic-task:g1',
      'jupyter-lab:nb-cpu',
      'experiment:5',
      'jupyter-lab:nb-ended',
    ]);
    // 3 tasks listed whole, and the totals of the two paged sources.
    expect(page.total).toBe(3 + 7 + 11);
    expect(page.errors).toEqual({});
  });

  it('asks the paged sources for their first offset + limit runs, newest first', async () => {
    await fetchRunPage(globalJobs({ limit: 20, offset: 40 }));

    expect(listCall(getGenericTasks)).toMatchObject({
      limit: 60,
      offset: 0,
      orderBy: 'ORDER_BY_DESC',
      sortBy: 'SORT_BY_START_TIME',
    });
    expect(listCall(getExperiments)).toMatchObject({
      archived: false,
      limit: 60,
      offset: 0,
      orderBy: 'ORDER_BY_DESC',
      sortBy: 'SORT_BY_START_TIME',
    });
    // Without a state filter, deleted experiments stay out.
    expect(listCall(getExperiments)?.states).not.toContain('STATE_DELETED');
    // One call each: the kind chips and their counts are gone.
    expect(getGenericTasks).toHaveBeenCalledTimes(1);
    expect(getExperiments).toHaveBeenCalledTimes(1);
  });

  it('asks the paged sources for the sort, and sorts the others the same way here', async () => {
    const page = await fetchRunPage(globalJobs({ sort: { desc: false, key: SortKey.Name } }));

    expect(listCall(getGenericTasks)).toMatchObject({
      orderBy: 'ORDER_BY_ASC',
      sortBy: 'SORT_BY_NAME',
    });
    expect(listCall(getExperiments)).toMatchObject({
      orderBy: 'ORDER_BY_ASC',
      sortBy: 'SORT_BY_NAME',
    });
    // "cmd-gpu" < "exp 5" < "g1" < "nb-cpu" < "nb-ended"
    expect(page.rows.map((row) => row.key)).toEqual([
      'command:cmd-gpu',
      'experiment:5',
      'generic-task:g1',
      'jupyter-lab:nb-cpu',
      'jupyter-lab:nb-ended',
    ]);
  });

  it('asks each paged source for newest first when sorted by kind', async () => {
    await fetchRunPage(globalJobs({ sort: { desc: false, key: SortKey.Kind } }));

    expect(listCall(getExperiments)).toMatchObject({
      orderBy: 'ORDER_BY_DESC',
      sortBy: 'SORT_BY_START_TIME',
    });
    expect(listCall(getGenericTasks)).toMatchObject({
      orderBy: 'ORDER_BY_DESC',
      sortBy: 'SORT_BY_START_TIME',
    });
  });

  it('asks for the state groups of the filter', async () => {
    await fetchRunPage(globalJobs({ states: [StateGroup.Ended] }));

    expect(listCall(getGenericTasks)?.states).toEqual([
      GenericTaskState.Completed,
      GenericTaskState.Canceled,
      GenericTaskState.Error,
    ]);
    expect(listCall(getExperiments)?.states).toContain('STATE_DELETE_FAILED');
  });

  it('sends the owners, and filters the other tasks by them here', async () => {
    const page = await fetchRunPage(globalJobs({ userIds: [3, 2] }));

    expect(listCall(getGenericTasks)?.userIds).toEqual([3, 2]);
    expect(listCall(getExperiments)?.userIds).toEqual([3, 2]);
    expect(vi.mocked(getCommands).mock.calls[0][0]).toMatchObject({ users: ['3', '2'] });
    expect(vi.mocked(getJupyterLabs).mock.calls[0][0]).toMatchObject({ users: ['3', '2'] });
    // The mock lists every user's tasks; only the owners' are shown.
    expect(page.rows.filter((row) => row.kind === RunKind.JupyterLab).map((row) => row.id)).toEqual(
      ['nb-ended'],
    );
  });

  it('lists all users without an owner filter', async () => {
    await fetchRunPage(globalJobs());

    expect(listCall(getGenericTasks)?.userIds).toBeUndefined();
    expect(listCall(getExperiments)?.userIds).toBeUndefined();
    expect(vi.mocked(getShells).mock.calls[0][0].users).toBeUndefined();
  });

  it('asks for all the notebooks, shells, commands and TensorBoards (limit 0)', async () => {
    await fetchRunPage(globalJobs());

    [getCommands, getJupyterLabs, getShells, getTensorBoards].forEach((fn) =>
      expect(vi.mocked(fn).mock.calls[0][0].limit).toBe(0),
    );
  });

  it('fetches only the kinds of the filter', async () => {
    const page = await fetchRunPage(globalJobs({ kinds: [RunKind.JupyterLab] }));

    expect(getGenericTasks).not.toHaveBeenCalled();
    expect(getExperiments).not.toHaveBeenCalled();
    expect(getCommands).not.toHaveBeenCalled();
    expect(getShells).not.toHaveBeenCalled();
    expect(page.rows.map((row) => row.key)).toEqual(['jupyter-lab:nb-cpu', 'jupyter-lab:nb-ended']);
    expect(page.total).toBe(2);
  });

  describe('slots', () => {
    it('filters experiments and generic tasks on the master by counts and Multi-node', async () => {
      await fetchRunPage(globalJobs({ slots: slotsQuery(['0', '1', 'multi:8']) }));

      for (const call of [listCall(getGenericTasks), listCall(getExperiments)]) {
        expect(call?.slots).toEqual([0, 1]);
        expect(call?.slotsAbove).toBe(8);
      }

      vi.clearAllMocks();
      await fetchRunPage(globalJobs({ slots: slotsQuery(['multi:0']) }));
      for (const call of [listCall(getGenericTasks), listCall(getExperiments)]) {
        expect(call?.slots).toBeUndefined();
        expect(call?.slotsAbove).toBe(0);
      }
    });

    it('sends no filter by default', async () => {
      await fetchRunPage(globalJobs());

      for (const call of [listCall(getGenericTasks), listCall(getExperiments)]) {
        expect(call).not.toHaveProperty('slots');
        expect(call).not.toHaveProperty('slotsAbove');
      }
    });

    it('filters notebooks, shells, commands and TensorBoards by their slots here', async () => {
      const kinds = [CommandType.Command, CommandType.JupyterLab];
      const many = await fetchRunPage(globalJobs({ kinds, slots: slotsQuery(['multi:1']) }));
      expect(many.rows.map((row) => row.key)).toEqual(['command:cmd-gpu']);

      const none = await fetchRunPage(globalJobs({ kinds, slots: slotsQuery(['0']) }));
      expect(none.rows.map((row) => row.key)).toEqual([
        'jupyter-lab:nb-cpu',
        'jupyter-lab:nb-ended',
      ]);
      // The task lists themselves are not filtered by the master.
      expect(vi.mocked(getCommands).mock.calls[0][0]).not.toHaveProperty('slots');
      expect(vi.mocked(getCommands).mock.calls[0][0]).not.toHaveProperty('slotsAbove');
    });
  });

  it('searches experiment names, or IDs for a number, and generic tasks on the master', async () => {
    await fetchRunPage(globalJobs({ search: ' bert ' }));
    expect(listCall(getExperiments)).toMatchObject({ name: 'bert' });
    expect(listCall(getGenericTasks)).toMatchObject({ search: 'bert' });

    vi.clearAllMocks();
    await fetchRunPage(globalJobs({ search: '42' }));
    expect(listCall(getExperiments)).toMatchObject({ experimentIdFilter: { incl: [42] } });
    expect(listCall(getExperiments)?.name).toBeUndefined();
  });

  it("lists a workspace's runs in the workspace", async () => {
    const scope = { type: 'workspace' as const, workspaceId: 7 };
    await fetchRunPage({ kinds: kindsOf(scope, true), limit: 20, offset: 0, scope });

    expect(listCall(getGenericTasks)).toMatchObject({ workspaceId: 7 });
    expect(listCall(getGenericTasks)?.projectId).toBeUndefined();
    expect(listCall(getExperiments)).toMatchObject({ workspaceId: 7 });
    expect(vi.mocked(getTensorBoards).mock.calls[0][0]).toMatchObject({ workspaceId: 7 });
  });

  it('filters the global page by workspaces, the other tasks here', async () => {
    const page = await fetchRunPage(globalJobs({ workspaceIds: [4, 9] }));

    expect(listCall(getGenericTasks)).toMatchObject({ workspaceIds: [4, 9] });
    expect(listCall(getExperiments)).toMatchObject({ workspaceIds: [4, 9] });
    expect(listCall(getExperiments)?.workspaceId).toBeUndefined();
    expect(vi.mocked(getCommands).mock.calls[0][0].workspaceId).toBeUndefined();
    expect(
      page.rows.filter(
        (row) => row.kind !== RunKind.Experiment && row.kind !== RunKind.GenericTask,
      ),
    ).toEqual([expect.objectContaining({ id: 'nb-ended' })]);
  });

  it("lists a project's experiments and generic tasks only", async () => {
    const scope = { projectId: 1, type: 'project' as const };
    const page = await fetchRunPage({ kinds: kindsOf(scope, true), limit: 20, offset: 0, scope });

    expect(getCommands).not.toHaveBeenCalled();
    expect(getJupyterLabs).not.toHaveBeenCalled();
    expect(getShells).not.toHaveBeenCalled();
    expect(getTensorBoards).not.toHaveBeenCalled();
    expect(listCall(getGenericTasks)).toMatchObject({ projectId: 1 });
    expect(listCall(getGenericTasks)?.workspaceId).toBeUndefined();
    expect(listCall(getExperiments)).toMatchObject({ projectId: 1 });
    expect(page.rows.map((row) => row.key)).toEqual(['generic-task:g1', 'experiment:5']);
  });

  it('leaves experiments out of the tasks-only view', async () => {
    const page = await fetchRunPage(globalJobs({ kinds: kindsOf({ type: 'global' }, false) }));

    expect(getExperiments).not.toHaveBeenCalled();
    expect(page.rows.some((row) => row.kind === RunKind.Experiment)).toBe(false);
  });

  it('reports a failed source and leaves it out of the rows and the total', async () => {
    const error = new Error('experiments are down');
    vi.mocked(getExperiments).mockRejectedValue(error);
    vi.mocked(getShells).mockRejectedValue(new Error('shells are down'));

    const page = await fetchRunPage(globalJobs());

    expect(page.errors[RunKind.Experiment]).toBe(error);
    expect(page.errors[RunKind.Shell]).toBeInstanceOf(Error);
    expect(Object.keys(page.errors).sort()).toEqual([RunKind.Experiment, RunKind.Shell].sort());
    expect(page.rows.some((row) => row.kind === RunKind.Experiment)).toBe(false);
    expect(page.total).toBe(3 + 7);
  });

  it('forwards the abort signal to every call', async () => {
    const signal = new AbortController().signal;
    await fetchRunPage(globalJobs(), signal);

    [
      getCommands,
      getJupyterLabs,
      getShells,
      getTensorBoards,
      getGenericTasks,
      getExperiments,
    ].forEach((fn) =>
      vi.mocked(fn).mock.calls.forEach((call) => expect(call[1]).toEqual({ signal })),
    );
  });
});
