import fixture from 'fixtures/jobsSortOrder.json';
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
  compareJobsText,
  experimentRow,
  genericTaskRow,
  mergeRuns,
  pageOfRuns,
  runComparator,
  RunRow,
  SortKey,
  timeKey,
} from './runRows';

/*
 * The shared fixture of the Jobs page's order, which the master's tests check against Postgres
 * (experiments) and Go (generic tasks): the browser must merge in the same order.
 */

interface FixtureRow {
  end: string | null;
  id: string;
  name: string;
  owner: string;
  pool: string | null;
  slots: number;
  start: string;
  state: string;
}

const rows = fixture.rows as FixtureRow[];
const orders = fixture.orders as Record<string, { ascend: string[]; descend: string[] }>;

const fixtureKeys: Record<string, SortKey> = {
  end: SortKey.EndTime,
  name: SortKey.Name,
  owner: SortKey.User,
  pool: SortKey.ResourcePool,
  slots: SortKey.Slots,
  start: SortKey.StartTime,
  stateGroup: SortKey.State,
};

/* The list shows the finer state of an active experiment, which the master fills in after it sorts. */
const ACTIVE_STATES = [RunState.Running, RunState.Queued, RunState.Pulling, RunState.Starting];

/* Experiments: a row listed earlier has a higher ID, which wins a tie. */
const experiments: RunRow[] = rows.map((row, i) => {
  const item: BulkExperimentItem = {
    archived: false,
    config: {
      resources: { slots_per_trial: row.slots },
    } as unknown as BulkExperimentItem['config'],
    displayName: row.owner,
    endTime: row.end ?? undefined,
    hyperparameters: {},
    id: 1000 - i,
    jobId: `job-${i}`,
    labels: [],
    name: row.name,
    numTrials: 1,
    projectId: 1,
    resourcePool: row.pool ?? '',
    searcherType: 'single',
    startTime: row.start,
    state:
      row.state === RunState.Active
        ? ACTIVE_STATES[i % ACTIVE_STATES.length]
        : (row.state as RunState),
    userId: 1,
    username: `user-${i}`,
  };
  return experimentRow(item);
});

/* Generic tasks: a row listed earlier has a lower task ID, which wins a tie. */
const taskId = (i: number) => `task-${String(i).padStart(2, '0')}`;
const genericTasks: RunRow[] = rows.map((row, i) => {
  const task: GenericTask = {
    description: '',
    displayName: row.owner,
    endTime: row.end ?? undefined,
    jobId: `job-${i}`,
    name: row.name,
    noPause: false,
    projectId: 1,
    resourcePool: row.pool ?? '',
    slots: row.slots,
    startTime: row.start,
    state: row.state as GenericTaskState,
    taskId: taskId(i),
    userId: 1,
    username: `user-${i}`,
    workspaceId: 1,
  };
  return genericTaskRow(task);
});

/* Commands have no end time and never pause, so they take the other keys. */
const commands: RunRow[] = rows.map((row, i) => {
  const task: CommandTask = {
    displayName: row.owner,
    id: taskId(i),
    name: row.name,
    resourcePool: row.pool ?? '',
    slots: row.slots,
    startTime: row.start,
    state: CommandState.Running,
    type: CommandType.Command,
    userId: 1,
    username: `user-${i}`,
    workspaceId: 1,
  };
  return commandRow(task);
});

const label = (list: RunRow[], source: RunRow[]) => list.map((row) => rows[source.indexOf(row)].id);

const sorted = (source: RunRow[], key: SortKey, desc: boolean) =>
  label([...source].sort(runComparator({ desc, key })), source);

describe('the Jobs page order', () => {
  describe.each(Object.entries(fixtureKeys))('by %s', (fixtureKey, key) => {
    it.each([
      ['ascend', false],
      ['descend', true],
    ] as const)('%s, as the master lists experiments and generic tasks', (direction, desc) => {
      const want = orders[fixtureKey][direction];
      expect(sorted(experiments, key, desc)).toEqual(want);
      expect(sorted(genericTasks, key, desc)).toEqual(want);
    });
  });

  describe.each(
    Object.entries(fixtureKeys).filter(
      ([fixtureKey]) => !['end', 'stateGroup'].includes(fixtureKey),
    ),
  )('notebooks, shells, commands and TensorBoards by %s', (fixtureKey, key) => {
    it.each([
      ['ascend', false],
      ['descend', true],
    ] as const)('%s, as the generic tasks', (direction, desc) => {
      expect(sorted(commands, key, desc)).toEqual(orders[fixtureKey][direction]);
    });
  });

  it.each(
    Object.values(fixtureKeys).flatMap((key) => [true, false].map((desc) => ({ desc, key }))),
  )('pages the merge of the three sources with no run twice and none left out: %o', (sort) => {
    const compare = runComparator(sort);
    const lists = [commands, genericTasks, experiments].map((list) => [...list].sort(compare));
    const whole = mergeRuns(lists, sort);
    expect(whole).toHaveLength(rows.length * 3);
    const pages: RunRow[] = [];
    for (let offset = 0; offset < whole.length; offset += 5) {
      // Each source sends its first offset + limit runs.
      const window = lists.map((list) => list.slice(0, offset + 5));
      pages.push(...pageOfRuns(window, offset, 5, sort));
    }
    expect(pages).toEqual(whole);
    expect(new Set(pages.map((row) => row.key)).size).toBe(whole.length);
    // The merge is the order of the comparator, across kinds too.
    expect(whole).toEqual([...whole].sort(compare));
  });
});

describe('compareJobsText', () => {
  const order = (texts: string[]) => [...texts].sort(compareJobsText);

  it('folds A-Z only, then compares as is', () => {
    expect(order(['beta', 'Beta', 'alpha'])).toEqual(['alpha', 'Beta', 'beta']);
    expect(order(['é', 'É', 'e', 'f'])).toEqual(['e', 'f', 'É', 'é']);
    // KELVIN SIGN lowercases to k in JavaScript, but not under Postgres's C collation.
    expect(order(['K', 'l', 'k'])).toEqual(['k', 'l', 'K']);
  });

  it('compares by code point, not by UTF-16 unit', () => {
    // 😀 (U+1F600) is D83D DE00 in UTF-16, before ｱ (U+FF71) by unit.
    expect('😀' < 'ｱ').toBe(true);
    expect(order(['😀', 'ｱ'])).toEqual(['ｱ', '😀']);
  });

  it('puts underscores, digits and spaces where their code points are', () => {
    expect(order(['ab', 'a_b', 'a-b', '_x', '48c', '128c', ' lead'])).toEqual([
      ' lead',
      '128c',
      '48c',
      '_x',
      'a-b',
      'a_b',
      'ab',
    ]);
  });
});

describe('timeKey', () => {
  it('compares times to the nanosecond, whichever way an API writes them', () => {
    expect(timeKey('2026-01-01T00:00:10Z')).toEqual(timeKey('2026-01-01T00:00:10.000000Z'));
    expect(timeKey('2026-01-01T09:00:10+09:00')).toEqual(timeKey('2026-01-01T00:00:10.000Z'));
    const [ms1, ns1] = timeKey('2026-01-01T00:03:20.000001Z') as [number, number];
    const [ms2, ns2] = timeKey('2026-01-01T00:03:20.000002Z') as [number, number];
    expect(ms1).toBe(ms2);
    expect(ns2 - ns1).toBe(1000);
    expect(timeKey(undefined)).toBeUndefined();
    expect(timeKey('')).toBeUndefined();
  });
});
