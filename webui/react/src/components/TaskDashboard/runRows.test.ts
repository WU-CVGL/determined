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
  experimentApiSort,
  experimentRow,
  experimentSearch,
  experimentSlots,
  experimentStateGroup,
  experimentStates,
  filterCommandRows,
  genericTaskApiSort,
  genericTaskRow,
  genericTaskStateGroup,
  genericTaskStates,
  kindsOf,
  matchesSlots,
  mergeRuns,
  MULTI_NODE,
  pageOfRuns,
  RunKind,
  RunRow,
  savedSlots,
  slotsOptions,
  slotsQuery,
  sortCommandRows,
  SortKey,
  StateGroup,
  tickedSlots,
  toApiSlots,
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

  describe('state groups for the sort', () => {
    it('groups experiments as the master sorts them: queued and running are active', () => {
      [RunState.Running, RunState.Queued, RunState.Pulling, RunState.StoppingKilled].forEach(
        (state) => expect(experimentStateGroup(state)).toBe(StateGroup.Active),
      );
      expect(experimentStateGroup(RunState.Paused)).toBe(StateGroup.Paused);
      [RunState.Completed, RunState.Canceled, RunState.Error, RunState.DeleteFailed].forEach(
        (state) => expect(experimentStateGroup(state)).toBe(StateGroup.Ended),
      );
    });

    it('leaves a generic task without a state out of every group', () => {
      expect(genericTaskStateGroup(GenericTaskState.Unspecified)).toBeUndefined();
      expect(genericTaskStateGroup(GenericTaskState.StoppingPaused)).toBe(StateGroup.Active);
      expect(genericTaskStateGroup(GenericTaskState.Paused)).toBe(StateGroup.Paused);
      expect(genericTaskStateGroup(GenericTaskState.Error)).toBe(StateGroup.Ended);
    });
  });

  describe('slots', () => {
    it('reads saved counts and Multi-node with its N, and drops anything else', () => {
      expect(slotsQuery(undefined)).toBeUndefined();
      expect(slotsQuery([])).toBeUndefined();
      expect(slotsQuery(['gpu', 'multi:x'])).toBeUndefined();
      expect(slotsQuery(['2', '0', '2'])).toEqual({ above: undefined, counts: [0, 2] });
      expect(slotsQuery(['1', 'multi:8'])).toEqual({ above: 8, counts: [1] });
    });

    it('sends counts and slots_above, either one alone', () => {
      expect(toApiSlots(undefined)).toEqual({});
      expect(toApiSlots(slotsQuery(['0']))).toEqual({ slots: [0] });
      expect(toApiSlots(slotsQuery(['multi:0']))).toEqual({ slotsAbove: 0 });
      expect(toApiSlots(slotsQuery(['0', '1', 'multi:8']))).toEqual({
        slots: [0, 1],
        slotsAbove: 8,
      });
    });

    it('matches a count or more than Multi-node, never unknown slots', () => {
      const query = slotsQuery(['0', 'multi:8']);
      expect(matchesSlots(0, query)).toBe(true);
      expect(matchesSlots(4, query)).toBe(false);
      expect(matchesSlots(8, query)).toBe(false);
      expect(matchesSlots(16, query)).toBe(true);
      expect(matchesSlots(undefined, query)).toBe(false);
      expect(matchesSlots(undefined, undefined)).toBe(true);
    });

    it('lists 0 to N, saved counts above N, then Multi-node', () => {
      expect(slotsOptions(2)).toEqual(['0', '1', '2', MULTI_NODE]);
      expect(slotsOptions(2, ['16', '1', 'multi:8'])).toEqual(['0', '1', '2', '16', MULTI_NODE]);
      expect(slotsOptions(0)).toEqual(['0', MULTI_NODE]);
    });

    it('ticks the counts between the saved N and N, so that a new agent changes nothing', () => {
      expect(tickedSlots(8, undefined)).toEqual([]);
      expect(tickedSlots(8, ['0', 'multi:8'])).toEqual(['0', MULTI_NODE]);
      expect(tickedSlots(10, ['0', 'multi:8'])).toEqual(['0', '9', '10', MULTI_NODE]);
      // GPU in 0.41.0: more than 0 slots.
      expect(tickedSlots(2, ['multi:0'])).toEqual(['1', '2', MULTI_NODE]);
    });

    it('saves Multi-node with N', () => {
      expect(savedSlots(8, ['0', MULTI_NODE])).toEqual(['0', 'multi:8']);
    });
  });

  describe('filterCommandRows', () => {
    const rows = [
      commandRow(
        command('gpu-shell', '2026-01-03T00:00:00Z', {
          slots: 1,
          type: CommandType.Shell,
          workspaceId: 3,
        }),
      ),
      commandRow(command('cpu-cmd', '2026-01-02T00:00:00Z', { name: 'Preprocess' })),
      commandRow(
        command('old-nb', '2026-01-01T00:00:00Z', {
          slots: 4,
          state: CommandState.Terminated,
          type: CommandType.JupyterLab,
          userId: 2,
        }),
      ),
    ];
    const all = [CommandType.Shell, CommandType.Command, CommandType.JupyterLab];
    const ids = (filtered: CommandRunRow[]) => filtered.map((row) => row.id);

    it('filters by kind, state, search, slots, owner and workspace', () => {
      expect(ids(filterCommandRows(rows, { kinds: [CommandType.Shell] }))).toEqual(['gpu-shell']);
      expect(ids(filterCommandRows(rows, { kinds: all, states: [StateGroup.Ended] }))).toEqual([
        'old-nb',
      ]);
      expect(ids(filterCommandRows(rows, { kinds: all, states: [StateGroup.Paused] }))).toEqual([]);
      expect(ids(filterCommandRows(rows, { kinds: all, search: 'PREPRO' }))).toEqual(['cpu-cmd']);
      expect(ids(filterCommandRows(rows, { kinds: all, search: 'old-' }))).toEqual(['old-nb']);
      expect(ids(filterCommandRows(rows, { kinds: all, slots: slotsQuery(['multi:0']) }))).toEqual([
        'gpu-shell',
        'old-nb',
      ]);
      expect(ids(filterCommandRows(rows, { kinds: all, slots: slotsQuery(['0', '4']) }))).toEqual([
        'cpu-cmd',
        'old-nb',
      ]);
      expect(ids(filterCommandRows(rows, { kinds: all, userIds: [2] }))).toEqual(['old-nb']);
      expect(ids(filterCommandRows(rows, { kinds: all, workspaceIds: [3, 4] }))).toEqual([
        'gpu-shell',
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

    it('sorts by a key, with missing values last both ways and ties newest first', () => {
      const rows = [
        commandRow(command('c1', '2026-01-01T00:00:00Z', { resourcePool: '' })),
        commandRow(command('c2', '2026-01-03T00:00:00Z', { resourcePool: 'b' })),
        commandRow(command('c3', '2026-01-02T00:00:00Z', { resourcePool: 'B' })),
        commandRow(command('c4', '2026-01-04T00:00:00Z', { resourcePool: 'b' })),
      ];
      const pool = (desc: boolean) =>
        sortCommandRows(rows, { desc, key: SortKey.ResourcePool }).map((row) => row.id);
      expect(pool(false)).toEqual(['c3', 'c4', 'c2', 'c1']);
      expect(pool(true)).toEqual(['c4', 'c2', 'c3', 'c1']);
    });

    it('merges a RUNNING experiment among paused and ended runs by state group', () => {
      const running = experimentRow({ ...experiment(5, '2026-01-01T00:00:00Z') });
      const paused = genericTaskRow({
        ...generic('g-paused', '2026-01-03T00:00:00Z'),
        state: GenericTaskState.Paused,
      });
      const stateless = genericTaskRow({
        ...generic('g-none', '2026-01-05T00:00:00Z'),
        state: GenericTaskState.Unspecified,
      });
      const ended = commandRow(
        command('c-ended', '2026-01-04T00:00:00Z', { state: CommandState.Terminated }),
      );
      const active = commandRow(command('c-active', '2026-01-02T00:00:00Z'));
      const sort = { desc: false, key: SortKey.State };
      expect(keys(mergeRuns([[active, ended], [paused, stateless], [running]], sort))).toEqual([
        'command:c-active',
        'experiment:5',
        'generic-task:g-paused',
        'command:c-ended',
        'generic-task:g-none',
      ]);
    });

    it('sorts by kind, Experiment first, newest first within a kind', () => {
      const merged = mergeRuns(
        [
          [commandRow(command('c', '2026-01-03T00:00:00Z'))],
          [genericTaskRow(generic('g', '2026-01-04T00:00:00Z'))],
          [
            experimentRow(experiment(2, '2026-01-02T00:00:00Z')),
            experimentRow(experiment(1, '2026-01-01T00:00:00Z')),
          ],
        ],
        { desc: false, key: SortKey.Kind },
      );
      expect(keys(merged)).toEqual(['experiment:2', 'experiment:1', 'generic-task:g', 'command:c']);
    });

    it('asks the master for the same sort, newest first by kind', () => {
      expect(experimentApiSort({ desc: false, key: SortKey.Name })).toEqual({
        orderBy: 'ORDER_BY_ASC',
        sortBy: 'SORT_BY_NAME',
      });
      expect(genericTaskApiSort({ desc: true, key: SortKey.Slots })).toEqual({
        orderBy: 'ORDER_BY_DESC',
        sortBy: 'SORT_BY_SLOTS',
      });
      expect(experimentApiSort({ desc: false, key: SortKey.Kind })).toEqual({
        orderBy: 'ORDER_BY_DESC',
        sortBy: 'SORT_BY_START_TIME',
      });
      expect(genericTaskApiSort({ desc: true, key: SortKey.State }).sortBy).toBe(
        'SORT_BY_STATE_GROUP',
      );
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

  describe('owner names', () => {
    it('are the display name, else the username, as each list API sends them', () => {
      expect(
        experimentRow({ ...experiment(1, '2026-01-01T00:00:00Z'), displayName: '', username: 'bo' })
          .ownerName,
      ).toBe('bo');
      expect(
        genericTaskRow({ ...generic('g', '2026-01-01T00:00:00Z'), displayName: 'Al' }).ownerName,
      ).toBe('Al');
      expect(
        genericTaskRow({ ...generic('g', '2026-01-01T00:00:00Z'), username: '' }).ownerName,
      ).toBeUndefined();
      expect(commandRow(command('c', '2026-01-01T00:00:00Z', { username: 'cy' })).ownerName).toBe(
        'cy',
      );
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
