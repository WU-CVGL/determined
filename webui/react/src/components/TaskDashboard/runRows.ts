import { V1GetExperimentsRequestSortBy, V1GetGenericTasksRequestSortBy } from 'services/api-ts-sdk';
import {
  Agent,
  BulkExperimentItem,
  CommandState,
  CommandTask,
  CommandType,
  GenericTask,
  GenericTaskState,
  ProjectExperiment,
  RawJson,
  RunState,
  ValueOf,
} from 'types';
import { compareCodePoints, compareText } from 'utils/textOrder';

/*
 * The rows of the Jobs dashboard and how they are filtered, merged and paged. Kept free of React so
 * that the order and the paging can be tested on their own.
 */

/**
 * The kinds of runs the dashboard lists. The values are those of the URL's `type` key; the four
 * task types keep the values of the old task list, so that its bookmarks still filter.
 */
export const RunKind = {
  Command: CommandType.Command,
  Experiment: 'experiment',
  GenericTask: 'generic-task',
  JupyterLab: CommandType.JupyterLab,
  Shell: CommandType.Shell,
  TensorBoard: CommandType.TensorBoard,
} as const;

export type RunKind = ValueOf<typeof RunKind>;

/** The order of the kinds: the Kind filter's and sort's, and of runs that started at the same time. */
export const RUN_KINDS: RunKind[] = [
  RunKind.Experiment,
  RunKind.GenericTask,
  RunKind.JupyterLab,
  RunKind.Shell,
  RunKind.Command,
  RunKind.TensorBoard,
];

/** Notebooks, shells, commands and TensorBoards: the kinds the master lists whole, from memory. */
export const COMMAND_KINDS: CommandType[] = [
  RunKind.JupyterLab,
  RunKind.Shell,
  RunKind.Command,
  RunKind.TensorBoard,
];

export const isCommandKind = (kind: RunKind): kind is CommandType =>
  (COMMAND_KINDS as RunKind[]).includes(kind);

export const runKindLabel: Record<RunKind, string> = {
  [RunKind.Command]: 'Command',
  [RunKind.Experiment]: 'Experiment',
  [RunKind.GenericTask]: 'Generic Task',
  [RunKind.JupyterLab]: 'JupyterLab',
  [RunKind.Shell]: 'Shell',
  [RunKind.TensorBoard]: 'TensorBoard',
};

export const runKindPluralLabel: Record<RunKind, string> = {
  [RunKind.Command]: 'commands',
  [RunKind.Experiment]: 'experiments',
  [RunKind.GenericTask]: 'generic tasks',
  [RunKind.JupyterLab]: 'JupyterLabs',
  [RunKind.Shell]: 'shells',
  [RunKind.TensorBoard]: 'TensorBoards',
};

/** Which page a dashboard is and where: the kinds it can list. */
export type DashboardScope =
  | { type: 'global' }
  | { type: 'workspace'; workspaceId: number }
  | { type: 'project'; projectId: number };

/**
 * The kinds a dashboard lists. A project has only experiments and generic tasks, since notebooks,
 * shells, commands and TensorBoards belong to a workspace and not to a project. The tasks-only view
 * leaves experiments out.
 */
export const kindsOf = (scope: DashboardScope, experiments: boolean): RunKind[] => {
  if (scope.type === 'project') return [RunKind.Experiment, RunKind.GenericTask];
  return experiments ? RUN_KINDS : RUN_KINDS.filter((kind) => kind !== RunKind.Experiment);
};

/* State */

export const StateGroup = {
  Active: 'active',
  Ended: 'ended',
  Paused: 'paused',
} as const;

export type StateGroup = ValueOf<typeof StateGroup>;

export const STATE_GROUPS: StateGroup[] = [StateGroup.Active, StateGroup.Paused, StateGroup.Ended];

export const stateGroupLabel: Record<StateGroup, string> = {
  [StateGroup.Active]: 'Active',
  [StateGroup.Ended]: 'Ended',
  [StateGroup.Paused]: 'Paused',
};

const genericTaskStatesByGroup: Record<StateGroup, GenericTaskState[]> = {
  [StateGroup.Active]: [
    GenericTaskState.Active,
    GenericTaskState.StoppingPaused,
    GenericTaskState.StoppingCompleted,
    GenericTaskState.StoppingCanceled,
    GenericTaskState.StoppingError,
  ],
  [StateGroup.Ended]: [
    GenericTaskState.Completed,
    GenericTaskState.Canceled,
    GenericTaskState.Error,
  ],
  [StateGroup.Paused]: [GenericTaskState.Paused],
};

/*
 * By the experiment's state in the database: queued and running experiments are ACTIVE there. The
 * list shows the finer state the master reports for an active experiment.
 */
const experimentStatesByGroup: Record<StateGroup, RunState[]> = {
  [StateGroup.Active]: [
    RunState.Active,
    RunState.StoppingCompleted,
    RunState.StoppingCanceled,
    RunState.StoppingError,
    RunState.StoppingKilled,
  ],
  [StateGroup.Ended]: [
    RunState.Completed,
    RunState.Canceled,
    RunState.Error,
    RunState.DeleteFailed,
  ],
  [StateGroup.Paused]: [RunState.Paused],
};

/** The generic task states to ask for; none (all states) without a state filter. */
export const genericTaskStates = (groups?: StateGroup[]): GenericTaskState[] | undefined =>
  groups?.length ? groups.flatMap((group) => genericTaskStatesByGroup[group]) : undefined;

/**
 * The experiment states to ask for. Without a state filter, all three groups, so that experiments
 * being deleted or deleted are never listed.
 */
export const experimentStates = (groups?: StateGroup[]): `STATE_${RunState}`[] =>
  (groups?.length ? groups : STATE_GROUPS)
    .flatMap((group) => experimentStatesByGroup[group])
    .map((state) => `STATE_${state}` as const);

/** A notebook, shell, command or TensorBoard is active until it is terminated; it never pauses. */
export const commandStateGroup = (state: CommandState): StateGroup =>
  state === CommandState.Terminated ? StateGroup.Ended : StateGroup.Active;

const ENDED_EXPERIMENT_STATES: string[] = [
  RunState.Completed,
  RunState.Canceled,
  RunState.Error,
  RunState.DeleteFailed,
];

/**
 * An experiment's state group as the master sorts by it: paused, ended, or else active, which
 * takes in the queued and running states that the list shows for an active experiment.
 */
export const experimentStateGroup = (state: string): StateGroup => {
  if (state === RunState.Paused) return StateGroup.Paused;
  return ENDED_EXPERIMENT_STATES.includes(state) ? StateGroup.Ended : StateGroup.Active;
};

/** A generic task's state group as the master sorts by it; none for a task without a state. */
export const genericTaskStateGroup = (state: GenericTaskState): StateGroup | undefined => {
  if (state === GenericTaskState.Unspecified) return undefined;
  if (state === GenericTaskState.Paused) return StateGroup.Paused;
  return genericTaskStatesByGroup[StateGroup.Ended].includes(state)
    ? StateGroup.Ended
    : StateGroup.Active;
};

/* Slots */

/** The Slots filter's option for "Multi-node": more slots than any agent has. */
export const MULTI_NODE = 'multi';

/** The saved value of "Multi-node": more slots than N, the most of any agent when it was saved. */
const MULTI_NODE_SAVED = /^multi:(\d+)$/;
const COUNT = /^\d+$/;

/** A Slots filter: the runs that ask for one of the counts, or for more slots than `above`. */
export interface SlotsQuery {
  above?: number;
  counts: number[];
}

/**
 * The Slots filter of the saved values: counts ('0', '1', ...) and Multi-node with its N
 * ('multi:8'). Undefined for none; other values are left out.
 */
export const slotsQuery = (values?: string[]): SlotsQuery | undefined => {
  const counts = new Set<number>();
  let above: number | undefined;
  (values ?? []).forEach((value) => {
    if (COUNT.test(value)) counts.add(Number(value));
    const multi = MULTI_NODE_SAVED.exec(value);
    if (multi) above = Math.min(above ?? Infinity, Number(multi[1]));
  });
  if (counts.size === 0 && above === undefined) return undefined;
  return { above, counts: [...counts].sort((a, b) => a - b) };
};

/** The list APIs' slot filter. */
export const toApiSlots = (query?: SlotsQuery): { slots?: number[]; slotsAbove?: number } => ({
  ...(query?.counts.length ? { slots: query.counts } : {}),
  ...(query?.above !== undefined ? { slotsAbove: query.above } : {}),
});

/** Whether a run that asks for these slots passes the filter; one with unknown slots never does. */
export const matchesSlots = (slots: number | undefined, query?: SlotsQuery): boolean => {
  if (!query) return true;
  if (slots === undefined) return false;
  return query.counts.includes(slots) || (query.above !== undefined && slots > query.above);
};

/**
 * The options of the Slots filter where the most slots of any agent is `n`: 0 to n, the counts above
 * n that the saved filter has, then Multi-node.
 */
export const slotsOptions = (n: number, saved?: string[]): string[] => {
  const above = (slotsQuery(saved)?.counts ?? []).filter((count) => count > n);
  return [...Array.from({ length: n + 1 }, (_, i) => i), ...above].map(String).concat(MULTI_NODE);
};

/**
 * The options that the saved filter ticks where the most slots of any agent is `n`: its counts,
 * and for Multi-node saved with a smaller N, the counts up to n and Multi-node.
 */
export const tickedSlots = (n: number, saved?: string[]): string[] => {
  const query = slotsQuery(saved);
  if (!query) return [];
  const ticked = query.counts.map(String);
  if (query.above === undefined) return ticked;
  for (let count = query.above + 1; count <= n; count++) ticked.push(String(count));
  return [...new Set(ticked), MULTI_NODE];
};

/** The most slots of any agent, by its slot counts of each device type. */
export const mostAgentSlots = (agents: Agent[]): number =>
  agents.reduce(
    (most, agent) =>
      Math.max(
        most,
        Object.values(agent.slotStats?.typeStats ?? {}).reduce(
          (total, stats) => total + (stats?.total ?? 0),
          0,
        ),
      ),
    0,
  );

/** The values to save for the ticked options, Multi-node with `n`. */
export const savedSlots = (n: number, ticked: string[]): string[] =>
  ticked.map((option) => (option === MULTI_NODE ? `${MULTI_NODE}:${n}` : option));

/**
 * The slots each trial of an experiment asks for: `resources.slots_per_trial`, 1 when the config
 * leaves it out, as the master's slot filter counts it. Unknown without a config.
 */
export const experimentSlots = (item: BulkExperimentItem): number | undefined => {
  // The list API returns the stored config as is, with the master's snake_case keys.
  const config = item.config as RawJson | undefined;
  if (!config) return undefined;
  const slots = config.resources?.slots_per_trial;
  return typeof slots === 'number' ? slots : 1;
};

/* Rows */

interface RunRowBase {
  endTime?: string;
  /** The ID as text: a task ID, or an experiment's number. */
  id: string;
  /** Unique across kinds: `${kind}:${id}`. */
  key: string;
  name: string;
  /**
   * The owner's display name, or the username without one, as the list API sends it: the value
   * that the master sorts by. Unset without either.
   */
  ownerName?: string;
  resourcePool: string;
  /** The slots the run asks for, per trial for an experiment; unset if unknown. */
  slots?: number;
  startTime: string;
  /** The state group the Jobs page sorts by; unset for a generic task without a state. */
  stateGroup?: StateGroup;
  userId: number;
  workspaceId: number;
}

export type CommandRunRow = RunRowBase & { kind: CommandType; task: CommandTask };
export type GenericTaskRunRow = RunRowBase & {
  kind: typeof RunKind.GenericTask;
  task: GenericTask;
};
export type ExperimentRunRow = RunRowBase & {
  experiment: ProjectExperiment;
  kind: typeof RunKind.Experiment;
};

export type RunRow = CommandRunRow | GenericTaskRunRow | ExperimentRunRow;

export const runRowKey = (kind: RunKind, id: string | number): string => `${kind}:${id}`;

const ownerName = (displayName?: string, username?: string): string | undefined =>
  displayName || username || undefined;

export const commandRow = (task: CommandTask): CommandRunRow => ({
  id: task.id,
  key: runRowKey(task.type, task.id),
  kind: task.type,
  name: task.name,
  ownerName: ownerName(task.displayName, task.username),
  resourcePool: task.resourcePool,
  slots: task.slots,
  startTime: task.startTime,
  stateGroup: commandStateGroup(task.state),
  task,
  userId: task.userId,
  workspaceId: task.workspaceId,
});

export const genericTaskRow = (task: GenericTask): GenericTaskRunRow => ({
  endTime: task.endTime,
  id: task.taskId,
  key: runRowKey(RunKind.GenericTask, task.taskId),
  kind: RunKind.GenericTask,
  name: task.name,
  ownerName: ownerName(task.displayName, task.username),
  resourcePool: task.resourcePool,
  slots: task.slots,
  startTime: task.startTime,
  stateGroup: genericTaskStateGroup(task.state),
  task,
  userId: task.userId,
  workspaceId: task.workspaceId,
});

/** The list API returns each experiment's project and workspace, which its menu needs. */
export const experimentRow = (item: BulkExperimentItem): ExperimentRunRow => {
  const experiment: ProjectExperiment = {
    ...item,
    parentArchived: !!item.parentArchived,
    projectName: item.projectName ?? '',
    projectOwnerId: item.projectOwnerId ?? 0,
    workspaceId: item.workspaceId ?? 0,
    workspaceName: item.workspaceName ?? '',
  };
  return {
    endTime: item.endTime,
    experiment,
    id: String(item.id),
    key: runRowKey(RunKind.Experiment, item.id),
    kind: RunKind.Experiment,
    name: item.name,
    ownerName: ownerName(item.displayName, item.username),
    resourcePool: item.resourcePool,
    slots: experimentSlots(item),
    startTime: item.startTime,
    stateGroup: experimentStateGroup(item.state),
    userId: item.userId,
    workspaceId: experiment.workspaceId,
  };
};

/* Order */

/**
 * The keys the Jobs page sorts by, the URL's `sortKey` values. Kind's is `type`, as on the old task
 * list.
 */
export const SortKey = {
  EndTime: 'endTime',
  Kind: 'type',
  Name: 'name',
  ResourcePool: 'resourcePool',
  Slots: 'slots',
  StartTime: 'startTime',
  State: 'state',
  User: 'user',
} as const;

export type SortKey = ValueOf<typeof SortKey>;

export const SORT_KEYS: SortKey[] = Object.values(SortKey);

export interface RunSort {
  desc: boolean;
  key: SortKey;
}

/** Newest first. */
export const DEFAULT_SORT: RunSort = { desc: true, key: SortKey.StartTime };

const TIME = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:?\d{2})?$/i;

/**
 * A time as its milliseconds since the epoch to the second and its nanoseconds within the second:
 * the APIs write times to the microsecond or nanosecond, which Date.parse cuts to the millisecond.
 * Undefined for no time.
 */
export const timeKey = (time?: string): [number, number] | undefined => {
  if (!time) return undefined;
  const parts = TIME.exec(time);
  if (!parts) {
    const ms = Date.parse(time);
    return Number.isNaN(ms) ? undefined : [ms, 0];
  }
  const [, seconds, fraction = '', zone = 'Z'] = parts;
  const ms = Date.parse(`${seconds}${zone}`);
  if (Number.isNaN(ms)) return undefined;
  return [ms, Number(fraction.padEnd(9, '0').slice(0, 9))];
};

const compareNumbers = (a: number, b: number): number => (a < b ? -1 : a > b ? 1 : 0);

const compareTimeKeys = (a: [number, number], b: [number, number]): number =>
  compareNumbers(a[0], b[0]) || compareNumbers(a[1], b[1]);

const STATE_GROUP_ORDER: Record<StateGroup, number> = {
  [StateGroup.Active]: 0,
  [StateGroup.Paused]: 1,
  [StateGroup.Ended]: 2,
};

type SortValue = number | string | [number, number];

/** A row's value of the key; undefined for a missing one, which sorts last both ways. */
const sortValue = (row: RunRow, key: SortKey): SortValue | undefined => {
  switch (key) {
    case SortKey.Kind:
      return RUN_KINDS.indexOf(row.kind);
    case SortKey.Name:
      return row.name;
    case SortKey.State:
      return row.stateGroup === undefined ? undefined : STATE_GROUP_ORDER[row.stateGroup];
    case SortKey.User:
      return row.ownerName || undefined;
    case SortKey.ResourcePool:
      return row.resourcePool || undefined;
    case SortKey.Slots:
      return row.slots;
    case SortKey.StartTime:
      return timeKey(row.startTime);
    case SortKey.EndTime:
      return timeKey(row.endTime);
  }
};

const compareSortValues = (a: SortValue, b: SortValue): number => {
  if (typeof a === 'string' && typeof b === 'string') return compareText(a, b);
  if (Array.isArray(a) && Array.isArray(b)) return compareTimeKeys(a, b);
  return compareNumbers(a as number, b as number);
};

const NO_TIME: [number, number] = [-Infinity, 0];

/** Ties: the newest start first, then the kind, then the ID as the kind's source orders it. */
const compareTies = (a: RunRow, b: RunRow): number => {
  const start = compareTimeKeys(timeKey(b.startTime) ?? NO_TIME, timeKey(a.startTime) ?? NO_TIME);
  if (start !== 0) return start;
  const kind = RUN_KINDS.indexOf(a.kind) - RUN_KINDS.indexOf(b.kind);
  if (kind !== 0) return Math.sign(kind);
  // Experiments by number, highest first; generic tasks and the other tasks by ID, A to Z.
  if (a.kind === RunKind.Experiment && b.kind === RunKind.Experiment) {
    return compareNumbers(b.experiment.id, a.experiment.id);
  }
  return compareCodePoints(a.id, b.id);
};

/**
 * The order of the Jobs page: the key in the sort's direction, with a missing key last either way,
 * then the ties. It is the order in which the master lists experiments and generic tasks, so that
 * the lists merge into one.
 */
export const runComparator =
  ({ desc, key }: RunSort = DEFAULT_SORT) =>
  (a: RunRow, b: RunRow): number => {
    const valueA = sortValue(a, key);
    const valueB = sortValue(b, key);
    if (valueA === undefined || valueB === undefined) {
      if (valueA !== valueB) return valueA === undefined ? 1 : -1;
    } else {
      const cmp = compareSortValues(valueA, valueB);
      if (cmp !== 0) return desc ? -cmp : cmp;
    }
    return compareTies(a, b);
  };

/** Sorts the notebooks, shells, commands and TensorBoards, which the master lists unsorted. */
export const sortCommandRows = (rows: CommandRunRow[], sort?: RunSort): CommandRunRow[] =>
  [...rows].sort(runComparator(sort));

/** Merges lists that are each in the sort's order into one. */
export const mergeRuns = (lists: RunRow[][], sort?: RunSort): RunRow[] => {
  const compare = runComparator(sort);
  const next = lists.map(() => 0);
  const merged: RunRow[] = [];
  for (;;) {
    let best = -1;
    lists.forEach((list, i) => {
      if (next[i] >= list.length) return;
      if (best < 0 || compare(list[next[i]], lists[best][next[best]]) < 0) best = i;
    });
    if (best < 0) return merged;
    merged.push(lists[best][next[best]]);
    next[best] += 1;
  }
};

/**
 * The rows of one page. Each paged source is fetched from its start up to the end of the page
 * (offset 0, limit offset + limit), so the first offset + limit runs of the merge are exact.
 */
export const pageOfRuns = (
  lists: RunRow[][],
  offset: number,
  limit: number,
  sort?: RunSort,
): RunRow[] => mergeRuns(lists, sort).slice(offset, offset + limit);

const experimentSortBy: Record<SortKey, V1GetExperimentsRequestSortBy> = {
  [SortKey.EndTime]: V1GetExperimentsRequestSortBy.ENDTIME,
  [SortKey.Kind]: V1GetExperimentsRequestSortBy.STARTTIME,
  [SortKey.Name]: V1GetExperimentsRequestSortBy.NAME,
  [SortKey.ResourcePool]: V1GetExperimentsRequestSortBy.RESOURCEPOOL,
  [SortKey.Slots]: V1GetExperimentsRequestSortBy.SLOTS,
  [SortKey.StartTime]: V1GetExperimentsRequestSortBy.STARTTIME,
  [SortKey.State]: V1GetExperimentsRequestSortBy.STATEGROUP,
  [SortKey.User]: V1GetExperimentsRequestSortBy.USER,
};

const genericTaskSortBy: Record<SortKey, V1GetGenericTasksRequestSortBy> = {
  [SortKey.EndTime]: V1GetGenericTasksRequestSortBy.ENDTIME,
  [SortKey.Kind]: V1GetGenericTasksRequestSortBy.STARTTIME,
  [SortKey.Name]: V1GetGenericTasksRequestSortBy.NAME,
  [SortKey.ResourcePool]: V1GetGenericTasksRequestSortBy.RESOURCEPOOL,
  [SortKey.Slots]: V1GetGenericTasksRequestSortBy.SLOTS,
  [SortKey.StartTime]: V1GetGenericTasksRequestSortBy.STARTTIME,
  [SortKey.State]: V1GetGenericTasksRequestSortBy.STATEGROUP,
  [SortKey.User]: V1GetGenericTasksRequestSortBy.USER,
};

type OrderBy = 'ORDER_BY_ASC' | 'ORDER_BY_DESC';

/* A source lists one kind, so by kind it lists in the order of the ties: the newest start first. */
const orderBy = ({ desc, key }: RunSort): OrderBy =>
  key === SortKey.Kind || desc ? 'ORDER_BY_DESC' : 'ORDER_BY_ASC';

/** The experiment list's sort for the Jobs page's. */
export const experimentApiSort = (
  sort: RunSort,
): { orderBy: OrderBy; sortBy: V1GetExperimentsRequestSortBy } => ({
  orderBy: orderBy(sort),
  sortBy: experimentSortBy[sort.key],
});

/** The generic task list's sort for the Jobs page's. */
export const genericTaskApiSort = (
  sort: RunSort,
): { orderBy: OrderBy; sortBy: V1GetGenericTasksRequestSortBy } => ({
  orderBy: orderBy(sort),
  sortBy: genericTaskSortBy[sort.key],
});

/* Filters run in the browser, for notebooks, shells, commands and TensorBoards */

export const matchesSearch = (row: Pick<RunRow, 'id' | 'name'>, search?: string): boolean => {
  const term = search?.trim().toLowerCase();
  if (!term) return true;
  return row.name.toLowerCase().includes(term) || row.id.toLowerCase().includes(term);
};

export interface CommandFilter {
  /** The task types to list. */
  kinds: RunKind[];
  search?: string;
  slots?: SlotsQuery;
  states?: StateGroup[];
  /** The owners to list; any without. */
  userIds?: number[];
  /** The workspaces to list; any without. */
  workspaceIds?: number[];
}

export const filterCommandRows = (
  rows: CommandRunRow[],
  { kinds, search, slots, states, userIds, workspaceIds }: CommandFilter,
): CommandRunRow[] =>
  rows.filter(
    (row) =>
      kinds.includes(row.kind) &&
      (!states?.length || states.includes(commandStateGroup(row.task.state))) &&
      (!userIds?.length || userIds.includes(row.userId)) &&
      (!workspaceIds?.length || workspaceIds.includes(row.workspaceId)) &&
      matchesSearch(row, search) &&
      matchesSlots(row.task.slots, slots),
  );

/**
 * The experiments' search: a number is an experiment ID, anything else a part of the name, as the
 * list API matches it.
 */
export const experimentSearch = (
  search?: string,
): { experimentIdFilter?: { incl: number[] }; name?: string } => {
  const term = search?.trim();
  if (!term) return {};
  if (/^\d+$/.test(term)) return { experimentIdFilter: { incl: [Number(term)] } };
  return { name: term };
};
