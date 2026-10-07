import fixture from 'fixtures/jobsSortOrder.json';
import * as Sdk from 'services/api-ts-sdk';
import { mapV1Command, mapV1ExperimentList, mapV1GenericTasksResponse } from 'services/decoder';

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
 * (experiments) and Go (generic tasks): the browser must merge in the same order. The rows go
 * through the JSON the master sends and the decoders the page uses.
 */

interface FixtureRow {
  end: string | null;
  genericTaskOnly?: boolean;
  id: string;
  name: string;
  owner: string | null;
  pool: string | null;
  slots: number;
  slotsDefault?: boolean;
  start: string;
  state: string | null;
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

/* A time as the master's JSON writes it: no fraction, or 3, 6 or 9 digits. */
const protoTime = (time: string): string =>
  time.replace(/\.(\d+)Z$/, (_, fraction: string) => {
    const digits = fraction.padEnd(9, '0');
    if (/^0+$/.test(digits)) return 'Z';
    if (digits.endsWith('000000')) return `.${digits.slice(0, 3)}Z`;
    if (digits.endsWith('000')) return `.${digits.slice(0, 6)}Z`;
    return `.${digits}Z`;
  });

/*
 * How the master sends the owner whose name the row sorts by, by turns: as the display name; as
 * the username of a user whose display name is empty; and as the username of a user without a
 * display name, where an experiment's display name falls back to the username.
 */
const owner = (row: FixtureRow, i: number, experiment: boolean) => {
  if (row.owner === null) return { displayName: '', userId: 0, username: '' };
  switch (i % 3) {
    case 0:
      return { displayName: row.owner, userId: 1, username: `user-${i}` };
    case 1:
      return { displayName: '', userId: 1, username: row.owner };
    default:
      return { displayName: experiment ? row.owner : '', userId: 1, username: row.owner };
  }
};

/* The list shows the finer state of an active experiment, which the master fills in after it sorts. */
const ACTIVE_STATES: Sdk.Experimentv1State[] = [
  Sdk.Experimentv1State.RUNNING,
  Sdk.Experimentv1State.QUEUED,
  Sdk.Experimentv1State.PULLING,
  Sdk.Experimentv1State.STARTING,
  Sdk.Experimentv1State.ACTIVE,
];

/* The source's rows and the fixture's ID of each. */
interface Source {
  ids: Map<RunRow, string>;
  rows: RunRow[];
}

const source = (fixtureRows: FixtureRow[], toRow: (row: FixtureRow, i: number) => RunRow) => {
  const ids = new Map<RunRow, string>();
  const list = fixtureRows.map((row, i) => {
    const run = toRow(row, i);
    ids.set(run, row.id);
    return run;
  });
  return { ids, rows: list };
};

const sharedRows = rows.filter((row) => !row.genericTaskOnly);

/* Experiments: a row listed earlier has a higher ID, which wins a tie. */
const experiments: Source = source(sharedRows, (row, i) => {
  const wire = {
    archived: false,
    config: {
      name: row.name,
      resources: {
        ...(row.pool === null ? {} : { resource_pool: row.pool }),
        ...(row.slotsDefault ? {} : { slots_per_trial: row.slots }),
      },
    },
    endTime: row.end === null ? null : protoTime(row.end),
    id: 1000 - i,
    jobId: `job-${i}`,
    name: row.name,
    numTrials: 1,
    originalConfig: '',
    projectId: 1,
    projectOwnerId: 1,
    resourcePool: row.pool ?? '',
    searcherType: 'single',
    startTime: protoTime(row.start),
    state:
      row.state === 'ACTIVE'
        ? ACTIVE_STATES[i % ACTIVE_STATES.length]
        : (`STATE_${row.state}` as Sdk.Experimentv1State),
    workspaceId: 1,
    ...owner(row, i, true),
  } as unknown as Sdk.V1Experiment;
  return experimentRow(mapV1ExperimentList([wire])[0]);
});

/* Generic tasks: a row listed earlier has a lower task ID, which wins a tie. */
const taskId = (i: number) => `task-${String(i).padStart(2, '0')}`;
const genericTasks: Source = source(rows, (row, i) => {
  const wire = {
    description: '',
    endTime: row.end === null ? null : protoTime(row.end),
    jobId: `job-${i}`,
    name: row.name,
    noPause: false,
    projectId: 1,
    resourcePool: row.pool ?? '',
    slots: row.slots,
    startTime: protoTime(row.start),
    state: (row.state === null
      ? Sdk.V1GenericTaskState.UNSPECIFIED
      : `GENERIC_TASK_STATE_${row.state}`) as Sdk.V1GenericTaskState,
    taskId: taskId(i),
    workspaceId: 1,
    ...owner(row, i, false),
  } as unknown as Sdk.V1GenericTask;
  return genericTaskRow(mapV1GenericTasksResponse({ pagination: {}, tasks: [wire] }).tasks[0]);
});

/* Commands have no end time and never pause, so they take the other keys. */
const commands: Source = source(sharedRows, (row, i) => {
  const wire = {
    description: row.name,
    id: taskId(i),
    jobId: `job-${i}`,
    resourcePool: row.pool ?? '',
    slots: row.slots,
    startTime: protoTime(row.start),
    state: Sdk.Taskv1State.RUNNING,
    workspaceId: 1,
    ...owner(row, i, false),
  } as unknown as Sdk.V1Command;
  return commandRow(mapV1Command(wire));
});

/* The fixture's order of a source: without the rows the source does not have. */
const orderOf = (src: Source, fixtureKey: string, direction: 'ascend' | 'descend') => {
  const has = new Set(src.ids.values());
  return orders[fixtureKey][direction].filter((id) => has.has(id));
};

const sorted = (src: Source, key: SortKey, desc: boolean) =>
  [...src.rows].sort(runComparator({ desc, key })).map((row) => src.ids.get(row));

describe('the Jobs page order', () => {
  it('decodes what the master sends', () => {
    // A time to the microsecond, an empty display name, no slots_per_trial, no state, no owner.
    expect(protoTime('2026-01-01T00:03:20.000001Z')).toBe('2026-01-01T00:03:20.000001Z');
    expect(protoTime('2026-01-01T00:00:10.000000Z')).toBe('2026-01-01T00:00:10Z');
    const byId = (src: Source, id: string) =>
      src.rows.find((row) => src.ids.get(row) === id) as RunRow;
    expect(timeKey(byId(experiments, 'r22').startTime)).not.toEqual(
      timeKey(byId(experiments, 'r21').startTime),
    );
    expect(byId(experiments, 'r02').ownerName).toBe('Alice');
    expect(byId(genericTasks, 'r02').ownerName).toBe('Alice');
    expect(byId(experiments, 'r09').slots).toBe(1);
    expect(byId(genericTasks, 'r23').ownerName).toBeUndefined();
    expect(byId(genericTasks, 'r24').stateGroup).toBeUndefined();
  });

  describe.each(Object.entries(fixtureKeys))('by %s', (fixtureKey, key) => {
    it.each([
      ['ascend', false],
      ['descend', true],
    ] as const)('%s, as the master lists experiments and generic tasks', (direction, desc) => {
      expect(sorted(experiments, key, desc)).toEqual(orderOf(experiments, fixtureKey, direction));
      expect(sorted(genericTasks, key, desc)).toEqual(orderOf(genericTasks, fixtureKey, direction));
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
      expect(sorted(commands, key, desc)).toEqual(orderOf(commands, fixtureKey, direction));
    });
  });

  it.each(
    Object.values(fixtureKeys).flatMap((key) => [true, false].map((desc) => ({ desc, key }))),
  )('pages the merge of the three sources with no run twice and none left out: %o', (sort) => {
    const compare = runComparator(sort);
    const lists = [commands, genericTasks, experiments].map((src) => [...src.rows].sort(compare));
    const whole = mergeRuns(lists, sort);
    expect(whole).toHaveLength(
      commands.rows.length + genericTasks.rows.length + experiments.rows.length,
    );
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
