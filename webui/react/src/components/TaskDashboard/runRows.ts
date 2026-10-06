import { V1SlotsFilter } from 'services/api-ts-sdk';
import {
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

/** The order of the kind chips, which also orders runs that started at the same time. */
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

/* GPU or CPU-only */

export const SlotsFilter = {
  CpuOnly: 'cpu-only',
  Gpu: 'gpu',
} as const;

export type SlotsFilter = ValueOf<typeof SlotsFilter>;

export const slotsFilterLabel: Record<SlotsFilter, string> = {
  [SlotsFilter.CpuOnly]: 'CPU-only',
  [SlotsFilter.Gpu]: 'GPU',
};

export const toApiSlotsFilter = (filter?: SlotsFilter): V1SlotsFilter | undefined => {
  switch (filter) {
    case SlotsFilter.Gpu:
      return V1SlotsFilter.HASSLOTS;
    case SlotsFilter.CpuOnly:
      return V1SlotsFilter.ZEROSLOTS;
    default:
      return undefined;
  }
};

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

/**
 * Whether a run that asks for these slots passes the filter: GPU is one slot or more, CPU-only no
 * slots. A run whose slots are unknown passes neither.
 */
export const matchesSlots = (slots: number | undefined, filter?: SlotsFilter): boolean => {
  if (!filter) return true;
  if (slots === undefined) return false;
  return filter === SlotsFilter.Gpu ? slots > 0 : slots <= 0;
};

/* Rows */

interface RunRowBase {
  endTime?: string;
  /** The ID as text: a task ID, or an experiment's number. */
  id: string;
  /** Unique across kinds: `${kind}:${id}`. */
  key: string;
  name: string;
  resourcePool: string;
  /** The slots the run asks for, per trial for an experiment; unset if unknown. */
  slots?: number;
  startTime: string;
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

export const commandRow = (task: CommandTask): CommandRunRow => ({
  id: task.id,
  key: runRowKey(task.type, task.id),
  kind: task.type,
  name: task.name,
  resourcePool: task.resourcePool,
  slots: task.slots,
  startTime: task.startTime,
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
  resourcePool: task.resourcePool,
  slots: task.slots,
  startTime: task.startTime,
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
    resourcePool: item.resourcePool,
    slots: experimentSlots(item),
    startTime: item.startTime,
    userId: item.userId,
    workspaceId: experiment.workspaceId,
  };
};

/* Order and paging */

const startMs = (row: RunRow): number => {
  const ms = Date.parse(row.startTime);
  return Number.isNaN(ms) ? -Infinity : ms;
};

/** Newest start first, then by kind. The order within a kind is its source's. */
export const compareRuns = (a: RunRow, b: RunRow): number => {
  const startA = startMs(a);
  const startB = startMs(b);
  if (startA !== startB) return startA > startB ? -1 : 1;
  return RUN_KINDS.indexOf(a.kind) - RUN_KINDS.indexOf(b.kind);
};

/** Sorts the notebooks, shells, commands and TensorBoards, which the master lists unsorted. */
export const sortCommandRows = (rows: CommandRunRow[]): CommandRunRow[] =>
  [...rows].sort((a, b) => compareRuns(a, b) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));

/**
 * Merges lists that are each in the dashboard's order into one. Each list keeps its own order, so
 * that ties within a kind stay as its source sorted them (experiments by ID, newest first; generic
 * tasks by task ID).
 */
export const mergeRuns = (lists: RunRow[][]): RunRow[] => {
  const next = lists.map(() => 0);
  const merged: RunRow[] = [];
  for (;;) {
    let best = -1;
    lists.forEach((list, i) => {
      if (next[i] >= list.length) return;
      if (best < 0 || compareRuns(list[next[i]], lists[best][next[best]]) < 0) best = i;
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
export const pageOfRuns = (lists: RunRow[][], offset: number, limit: number): RunRow[] =>
  mergeRuns(lists).slice(offset, offset + limit);

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
  slots?: SlotsFilter;
  states?: StateGroup[];
}

export const filterCommandRows = (
  rows: CommandRunRow[],
  { kinds, search, slots, states }: CommandFilter,
): CommandRunRow[] =>
  rows.filter(
    (row) =>
      kinds.includes(row.kind) &&
      (!states?.length || states.includes(commandStateGroup(row.task.state))) &&
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
