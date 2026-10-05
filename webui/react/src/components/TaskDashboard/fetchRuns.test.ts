import {
  getCommands,
  getExperiments,
  getGenericTasks,
  getJupyterLabs,
  getShells,
  getTensorBoards,
} from 'services/api';
import { V1SlotsFilter } from 'services/api-ts-sdk';
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
import { kindsOf, RunKind, SlotsFilter, StateGroup } from './runRows';

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

/** The calls of a paged source: the page's list, then the active count (limit 1). */
type PagedList<P> = (params: P, options?: FetchOptions) => Promise<unknown>;
const listCall = <P extends { limit?: number }>(fn: PagedList<P>): P | undefined =>
  vi.mocked(fn).mock.calls.find(([params]) => params.limit !== 1)?.[0];
const countCall = <P extends { limit?: number }>(fn: PagedList<P>): P | undefined =>
  vi.mocked(fn).mock.calls.find(([params]) => params.limit === 1)?.[0];

const globalJobs = (overrides: Partial<RunQuery> = {}): RunQuery => {
  const pageKinds = kindsOf({ type: 'global' }, true);
  return {
    kinds: pageKinds,
    limit: 20,
    offset: 0,
    pageKinds,
    scope: { type: 'global' },
    ...overrides,
  };
};

describe('fetchRunPage', () => {
  beforeEach(() => {
    vi.mocked(getCommands).mockResolvedValue([
      command('cmd-gpu', CommandType.Command, '2026-01-05T00:00:00Z', { slots: 2 }),
    ]);
    vi.mocked(getJupyterLabs).mockResolvedValue([
      command('nb-cpu', CommandType.JupyterLab, '2026-01-03T00:00:00Z'),
      command('nb-ended', CommandType.JupyterLab, '2026-01-01T00:00:00Z', {
        state: CommandState.Terminated,
      }),
    ]);
    vi.mocked(getShells).mockResolvedValue([]);
    vi.mocked(getTensorBoards).mockResolvedValue([]);
    vi.mocked(getGenericTasks).mockImplementation((params) =>
      Promise.resolve(
        params.limit === 1
          ? genericPage([], 3)
          : genericPage([generic('g1', '2026-01-04T00:00:00Z')], 7),
      ),
    );
    vi.mocked(getExperiments).mockImplementation((params) =>
      Promise.resolve(
        params.limit === 1
          ? experimentPage([], 2)
          : experimentPage([experiment(5, '2026-01-02T00:00:00Z')], 11),
      ),
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
    expect(page.activeCounts).toEqual({
      [RunKind.Command]: 1,
      [RunKind.Experiment]: 2,
      [RunKind.GenericTask]: 3,
      [RunKind.JupyterLab]: 1,
      [RunKind.Shell]: 0,
      [RunKind.TensorBoard]: 0,
    });
  });

  it('asks the paged sources for their first offset + limit runs, newest first', async () => {
    await fetchRunPage(globalJobs({ limit: 20, offset: 40 }));

    expect(listCall(getGenericTasks)).toMatchObject({ limit: 60, offset: 0 });
    expect(listCall(getExperiments)).toMatchObject({
      archived: false,
      limit: 60,
      offset: 0,
      orderBy: 'ORDER_BY_DESC',
      sortBy: 'SORT_BY_START_TIME',
    });
    // Without a state filter, deleted experiments stay out.
    expect(listCall(getExperiments)?.states).not.toContain('STATE_DELETED');
  });

  it('counts the active runs of the paged sources with one light call each', async () => {
    await fetchRunPage(globalJobs({ states: [StateGroup.Ended] }));

    expect(countCall(getGenericTasks)).toMatchObject({
      limit: 1,
      offset: 0,
      states: [
        GenericTaskState.Active,
        GenericTaskState.StoppingPaused,
        GenericTaskState.StoppingCompleted,
        GenericTaskState.StoppingCanceled,
        GenericTaskState.StoppingError,
      ],
    });
    expect(countCall(getExperiments)?.states).toContain('STATE_ACTIVE');
    expect(listCall(getGenericTasks)?.states).toEqual([
      GenericTaskState.Completed,
      GenericTaskState.Canceled,
      GenericTaskState.Error,
    ]);
  });

  it('sends the user for Mine', async () => {
    await fetchRunPage(globalJobs({ userId: 3 }));

    expect(listCall(getGenericTasks)?.userIds).toEqual([3]);
    expect(listCall(getExperiments)?.userIds).toEqual([3]);
    expect(vi.mocked(getCommands).mock.calls[0][0]).toMatchObject({ users: ['3'] });
    expect(vi.mocked(getJupyterLabs).mock.calls[0][0]).toMatchObject({ users: ['3'] });
  });

  it('lists all users without Mine', async () => {
    await fetchRunPage(globalJobs());

    expect(listCall(getGenericTasks)?.userIds).toBeUndefined();
    expect(listCall(getExperiments)?.userIds).toBeUndefined();
    expect(vi.mocked(getShells).mock.calls[0][0].users).toBeUndefined();
  });

  it('skips the paged sources that the kind filter leaves out', async () => {
    const page = await fetchRunPage(globalJobs({ kinds: [RunKind.JupyterLab] }));

    expect(listCall(getGenericTasks)).toBeUndefined();
    expect(listCall(getExperiments)).toBeUndefined();
    expect(page.rows.map((row) => row.key)).toEqual(['jupyter-lab:nb-cpu', 'jupyter-lab:nb-ended']);
    expect(page.total).toBe(2);
    // The chips still count every kind.
    expect(page.activeCounts[RunKind.Experiment]).toBe(2);
    expect(page.activeCounts[RunKind.Command]).toBe(1);
  });

  describe('GPU and CPU-only', () => {
    it('filters experiments and generic tasks on the master', async () => {
      await fetchRunPage(globalJobs({ slots: SlotsFilter.Gpu }));

      expect(listCall(getGenericTasks)?.slotsFilter).toBe(V1SlotsFilter.GPU);
      expect(countCall(getGenericTasks)?.slotsFilter).toBe(V1SlotsFilter.GPU);
      expect(listCall(getExperiments)?.slotsFilter).toBe(V1SlotsFilter.GPU);
      expect(countCall(getExperiments)?.slotsFilter).toBe(V1SlotsFilter.GPU);

      vi.clearAllMocks();
      await fetchRunPage(globalJobs({ slots: SlotsFilter.CpuOnly }));
      expect(listCall(getGenericTasks)?.slotsFilter).toBe(V1SlotsFilter.CPUONLY);
      expect(listCall(getExperiments)?.slotsFilter).toBe(V1SlotsFilter.CPUONLY);
    });

    it('sends no filter by default', async () => {
      await fetchRunPage(globalJobs());

      expect(listCall(getGenericTasks)?.slotsFilter).toBeUndefined();
      expect(listCall(getExperiments)?.slotsFilter).toBeUndefined();
    });

    it('filters notebooks, shells, commands and TensorBoards by their slots here', async () => {
      const gpu = await fetchRunPage(
        globalJobs({
          kinds: [...[CommandType.Command, CommandType.JupyterLab]],
          slots: SlotsFilter.Gpu,
        }),
      );
      expect(gpu.rows.map((row) => row.key)).toEqual(['command:cmd-gpu']);
      expect(gpu.activeCounts[RunKind.JupyterLab]).toBe(0);

      const cpu = await fetchRunPage(
        globalJobs({
          kinds: [CommandType.Command, CommandType.JupyterLab],
          slots: SlotsFilter.CpuOnly,
        }),
      );
      expect(cpu.rows.map((row) => row.key)).toEqual([
        'jupyter-lab:nb-cpu',
        'jupyter-lab:nb-ended',
      ]);
      expect(cpu.activeCounts[RunKind.Command]).toBe(0);
      // The task lists themselves are not filtered by the master.
      expect(vi.mocked(getCommands).mock.calls[0][0]).not.toHaveProperty('slotsFilter');
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
    const pageKinds = kindsOf(scope, true);
    await fetchRunPage({ kinds: pageKinds, limit: 20, offset: 0, pageKinds, scope });

    expect(listCall(getGenericTasks)).toMatchObject({ workspaceId: 7 });
    expect(listCall(getGenericTasks)?.projectId).toBeUndefined();
    expect(listCall(getExperiments)).toMatchObject({ workspaceId: 7 });
    expect(vi.mocked(getTensorBoards).mock.calls[0][0]).toMatchObject({ workspaceId: 7 });
  });

  it('filters the global page by one workspace', async () => {
    await fetchRunPage(globalJobs({ workspaceId: 4 }));

    expect(listCall(getGenericTasks)).toMatchObject({ workspaceId: 4 });
    expect(listCall(getExperiments)).toMatchObject({ workspaceId: 4 });
    expect(vi.mocked(getCommands).mock.calls[0][0]).toMatchObject({ workspaceId: 4 });
  });

  it("lists a project's experiments and generic tasks only", async () => {
    const scope = { projectId: 1, type: 'project' as const };
    const pageKinds = kindsOf(scope, true);
    const page = await fetchRunPage({ kinds: pageKinds, limit: 20, offset: 0, pageKinds, scope });

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
    const pageKinds = kindsOf({ type: 'global' }, false);
    const page = await fetchRunPage(globalJobs({ kinds: pageKinds, pageKinds }));

    expect(getExperiments).not.toHaveBeenCalled();
    expect(page.rows.some((row) => row.kind === RunKind.Experiment)).toBe(false);
    expect(page.activeCounts[RunKind.Experiment]).toBeUndefined();
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
    expect(page.activeCounts[RunKind.Experiment]).toBeUndefined();
    expect(page.activeCounts[RunKind.Shell]).toBeUndefined();
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
