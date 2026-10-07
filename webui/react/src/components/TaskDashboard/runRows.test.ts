import {
  BulkExperimentItem,
  CommandState,
  CommandTask,
  CommandType,
  GenericTask,
  GenericTaskState,
  RunState,
} from 'types';

import {
  commandRow,
  CommandRunRow,
  commandStateGroup,
  experimentRow,
  experimentSearch,
  experimentSlots,
  experimentStates,
  filterCommandRows,
  genericTaskRow,
  genericTaskStates,
  kindsOf,
  matchesSlots,
  mergeRuns,
  pageOfRuns,
  RunKind,
  RunRow,
  SlotsFilter,
  sortCommandRows,
  StateGroup,
} from './runRows';

const command = (
  id: string,
  startTime: string,
  overrides: Partial<CommandTask> = {},
): CommandTask => ({
  id,
  name: `task ${id}`,
  resourcePool: 'default',
  slots: 0,
  startTime,
  state: CommandState.Running,
  type: CommandType.Command,
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
  parentArchived: true,
  projectId: 3,
  projectName: 'vision',
  projectOwnerId: 5,
  resourcePool: 'default',
  searcherType: 'single',
  startTime,
  state: RunState.Running,
  userId: 1,
  workspaceId: 2,
  workspaceName: 'lab',
});

const keys = (rows: RunRow[]) => rows.map((row) => row.key);

describe('runRows', () => {
  describe('state groups', () => {
    it('maps notebooks, shells, commands and TensorBoards: terminated is ended, never paused', () => {
      expect(commandStateGroup(CommandState.Terminated)).toBe(StateGroup.Ended);
      [
        CommandState.Queued,
        CommandState.Pulling,
        CommandState.Starting,
        CommandState.Running,
        CommandState.Terminating,
        CommandState.Waiting,
      ].forEach((state) => expect(commandStateGroup(state)).toBe(StateGroup.Active));
    });

    it('maps generic task states, and asks for all states without a filter', () => {
      expect(genericTaskStates(undefined)).toBeUndefined();
      expect(genericTaskStates([])).toBeUndefined();
      expect(genericTaskStates([StateGroup.Active])).toEqual([
        GenericTaskState.Active,
        GenericTaskState.StoppingPaused,
        GenericTaskState.StoppingCompleted,
        GenericTaskState.StoppingCanceled,
        GenericTaskState.StoppingError,
      ]);
      expect(genericTaskStates([StateGroup.Paused])).toEqual([GenericTaskState.Paused]);
      expect(genericTaskStates([StateGroup.Ended])).toEqual([
        GenericTaskState.Completed,
        GenericTaskState.Canceled,
        GenericTaskState.Error,
      ]);
    });

    it('maps experiment states by the database, and never asks for deleted ones', () => {
      expect(experimentStates([StateGroup.Active])).toEqual([
        'STATE_ACTIVE',
        'STATE_STOPPING_COMPLETED',
        'STATE_STOPPING_CANCELED',
        'STATE_STOPPING_ERROR',
        'STATE_STOPPING_KILLED',
      ]);
      expect(experimentStates([StateGroup.Paused])).toEqual(['STATE_PAUSED']);
      expect(experimentStates([StateGroup.Ended])).toEqual([
        'STATE_COMPLETED',
        'STATE_CANCELED',
        'STATE_ERROR',
        'STATE_DELETE_FAILED',
      ]);
      const all = experimentStates(undefined);
      expect(all).toEqual(
        experimentStates([StateGroup.Active, StateGroup.Paused, StateGroup.Ended]),
      );
      expect(all).not.toContain('STATE_DELETING');
      expect(all).not.toContain('STATE_DELETED');
    });
  });

  describe('kinds', () => {
    it('lists every kind on the Jobs pages, no experiments in the tasks-only view', () => {
      expect(kindsOf({ type: 'global' }, true)).toContain(RunKind.Experiment);
      expect(kindsOf({ type: 'global' }, true)).toHaveLength(6);
      expect(kindsOf({ type: 'workspace', workspaceId: 2 }, false)).not.toContain(
        RunKind.Experiment,
      );
      expect(kindsOf({ type: 'workspace', workspaceId: 2 }, false)).toHaveLength(5);
    });

    it('lists only experiments and generic tasks in a project', () => {
      expect(kindsOf({ projectId: 1, type: 'project' }, true)).toEqual([
        RunKind.Experiment,
        RunKind.GenericTask,
      ]);
    });
  });

  describe('GPU and CPU-only', () => {
    it('counts one slot or more as GPU and no slots as CPU-only', () => {
      expect(matchesSlots(2, SlotsFilter.Gpu)).toBe(true);
      expect(matchesSlots(0, SlotsFilter.Gpu)).toBe(false);
      expect(matchesSlots(0, SlotsFilter.CpuOnly)).toBe(true);
      expect(matchesSlots(1, SlotsFilter.CpuOnly)).toBe(false);
      expect(matchesSlots(undefined, undefined)).toBe(true);
    });

    it('leaves a task with unknown slots out of both', () => {
      expect(matchesSlots(undefined, SlotsFilter.Gpu)).toBe(false);
      expect(matchesSlots(undefined, SlotsFilter.CpuOnly)).toBe(false);
    });
  });

  describe('filterCommandRows', () => {
    const rows = [
      commandRow(
        command('gpu-shell', '2026-01-03T00:00:00Z', { slots: 1, type: CommandType.Shell }),
      ),
      commandRow(command('cpu-cmd', '2026-01-02T00:00:00Z', { name: 'Preprocess' })),
      commandRow(
        command('old-nb', '2026-01-01T00:00:00Z', {
          slots: 4,
          state: CommandState.Terminated,
          type: CommandType.JupyterLab,
        }),
      ),
    ];
    const all = [CommandType.Shell, CommandType.Command, CommandType.JupyterLab];
    const ids = (filtered: CommandRunRow[]) => filtered.map((row) => row.id);

    it('filters by kind, state, search and slots', () => {
      expect(ids(filterCommandRows(rows, { kinds: [CommandType.Shell] }))).toEqual(['gpu-shell']);
      expect(ids(filterCommandRows(rows, { kinds: all, states: [StateGroup.Ended] }))).toEqual([
        'old-nb',
      ]);
      expect(ids(filterCommandRows(rows, { kinds: all, states: [StateGroup.Paused] }))).toEqual([]);
      expect(ids(filterCommandRows(rows, { kinds: all, search: 'PREPRO' }))).toEqual(['cpu-cmd']);
      expect(ids(filterCommandRows(rows, { kinds: all, search: 'old-' }))).toEqual(['old-nb']);
      expect(ids(filterCommandRows(rows, { kinds: all, slots: SlotsFilter.Gpu }))).toEqual([
        'gpu-shell',
        'old-nb',
      ]);
      expect(ids(filterCommandRows(rows, { kinds: all, slots: SlotsFilter.CpuOnly }))).toEqual([
        'cpu-cmd',
      ]);
    });
  });

  describe('order', () => {
    it('sorts tasks newest first, then by kind, then by ID', () => {
      const same = '2026-01-01T00:00:00Z';
      const sorted = sortCommandRows([
        commandRow(command('b', same)),
        commandRow(command('z', same, { type: CommandType.JupyterLab })),
        commandRow(command('a', same)),
        commandRow(command('new', '2026-01-02T00:00:00Z')),
      ]);
      expect(keys(sorted)).toEqual(['command:new', 'jupyter-lab:z', 'command:a', 'command:b']);
    });

    it('merges newest first, breaks ties by kind and keeps each source order', () => {
      const same = '2026-01-02T00:00:00.000Z';
      // The master sorts experiments by ID, newest first, on the same start time.
      const experiments = [
        experimentRow(experiment(9, same)),
        experimentRow(experiment(8, same)),
        experimentRow(experiment(1, '2026-01-01T00:00:00Z')),
      ];
      const generics = [genericTaskRow(generic('g1', '2026-01-02T00:00:00Z'))];
      const commands = [
        commandRow(command('c1', '2026-01-03T00:00:00Z')),
        commandRow(command('c2', same)),
      ];
      expect(keys(mergeRuns([commands, generics, experiments]))).toEqual([
        'command:c1',
        'experiment:9',
        'experiment:8',
        'generic-task:g1',
        'command:c2',
        'experiment:1',
      ]);
    });

    it('compares times, not the way each API writes them', () => {
      const merged = mergeRuns([
        [commandRow(command('c', '2026-01-01T10:00:00+09:00'))],
        [genericTaskRow(generic('g', '2026-01-01T02:00:00.5Z'))],
      ]);
      expect(keys(merged)).toEqual(['generic-task:g', 'command:c']);
    });

    it('cuts a deep page out of the first offset + limit runs of each source', () => {
      const day = (n: number) => `2026-01-${String(n).padStart(2, '0')}T00:00:00Z`;
      // Odd days are experiments, even days generic tasks; each source returns its first 6.
      const experiments = [29, 27, 25, 23, 21, 19].map((n) => experimentRow(experiment(n, day(n))));
      const generics = [28, 26, 24, 22, 20, 18].map((n) =>
        genericTaskRow(generic(`g${n}`, day(n))),
      );
      const page = pageOfRuns([[], generics, experiments], 4, 2);
      expect(keys(page)).toEqual(['experiment:25', 'generic-task:g24']);
    });
  });

  describe('experiment rows', () => {
    it("keep the experiment's project and workspace for its menu", () => {
      const row = experimentRow(experiment(4, '2026-01-01T00:00:00Z'));
      expect(row.key).toBe('experiment:4');
      expect(row.id).toBe('4');
      expect(row.experiment).toMatchObject({
        parentArchived: true,
        projectId: 3,
        projectName: 'vision',
        projectOwnerId: 5,
        workspaceId: 2,
        workspaceName: 'lab',
      });
    });

    it("carry the slots per trial from the experiment's stored config, 1 when it has none", () => {
      const withConfig = (config: unknown): BulkExperimentItem => ({
        ...experiment(4, '2026-01-01T00:00:00Z'),
        config: config as BulkExperimentItem['config'],
      });
      expect(experimentSlots(withConfig({ resources: { slots_per_trial: 4 } }))).toBe(4);
      expect(experimentSlots(withConfig({ resources: { slots_per_trial: 0 } }))).toBe(0);
      expect(experimentSlots(withConfig({ resources: {} }))).toBe(1);
      expect(experimentSlots(withConfig({}))).toBe(1);
      expect(experimentSlots(experiment(4, '2026-01-01T00:00:00Z'))).toBeUndefined();
      expect(experimentRow(withConfig({ resources: { slots_per_trial: 2 } })).slots).toBe(2);
    });
  });

  describe('experimentSearch', () => {
    it('searches names, and IDs for a number', () => {
      expect(experimentSearch(undefined)).toEqual({});
      expect(experimentSearch('  ')).toEqual({});
      expect(experimentSearch(' bert ')).toEqual({ name: 'bert' });
      expect(experimentSearch('42')).toEqual({ experimentIdFilter: { incl: [42] } });
    });
  });
});
