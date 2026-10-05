import {
  getCommands,
  getExperiments,
  getGenericTasks,
  getJupyterLabs,
  getShells,
  getTensorBoards,
} from 'services/api';
import { CommandTask, CommandType, ExperimentPagination, GenericTaskPagination } from 'types';

import {
  COMMAND_KINDS,
  commandRow,
  CommandRunRow,
  commandStateGroup,
  DashboardScope,
  experimentRow,
  experimentSearch,
  experimentStates,
  filterCommandRows,
  genericTaskRow,
  genericTaskStates,
  pageOfRuns,
  RunKind,
  RunRow,
  SlotsFilter,
  sortCommandRows,
  StateGroup,
  toApiSlotsFilter,
} from './runRows';

/** What one page of the dashboard shows. */
export interface RunQuery {
  /** The kinds to list, a part of `pageKinds`. */
  kinds: RunKind[];
  limit: number;
  offset: number;
  /** All the kinds the page lists, whose active runs the kind chips count. */
  pageKinds: RunKind[];
  scope: DashboardScope;
  search?: string;
  slots?: SlotsFilter;
  states?: StateGroup[];
  /** Only this user's runs ("Mine"). */
  userId?: number;
  /** On the global page, the runs of this workspace only. */
  workspaceId?: number;
}

export interface RunPage {
  /** By kind, the runs in the Active state group that pass the filters other than kind and state. */
  activeCounts: Partial<Record<RunKind, number>>;
  /** The kinds whose list failed, with the error; they are left out of the rows and the total. */
  errors: Partial<Record<RunKind, unknown>>;
  rows: RunRow[];
  total: number;
}

const commandListers: Record<CommandType, typeof getCommands> = {
  [RunKind.Command]: getCommands,
  [RunKind.JupyterLab]: getJupyterLabs,
  [RunKind.Shell]: getShells,
  [RunKind.TensorBoard]: getTensorBoards,
};

const ACTIVE: StateGroup[] = [StateGroup.Active];

/**
 * Fetches one page of the dashboard.
 * - Experiments and generic tasks are paged by the master, newest first: each is fetched with
 *   offset 0 and limit offset + limit, filtered on the master.
 * - Notebooks, shells, commands and TensorBoards are listed whole by the master and filtered here.
 *   They are fetched for the counts of the kind chips even when the kind filter leaves them out.
 * - The sorted lists are merged and the page's rows cut out; the total is the sum of the three.
 * - For the chips, one light call each counts active experiments and generic tasks.
 * Each list that fails is reported in `errors` and left out.
 */
export const fetchRunPage = async (query: RunQuery, signal?: AbortSignal): Promise<RunPage> => {
  const { kinds, limit, offset, pageKinds, scope, search, slots, states, userId } = query;
  const options = { signal };
  const workspaceId = scope.type === 'workspace' ? scope.workspaceId : query.workspaceId;
  const projectId = scope.type === 'project' ? scope.projectId : undefined;
  const term = search?.trim() || undefined;
  const slotsFilter = toApiSlotsFilter(slots);
  const userIds = userId !== undefined ? [userId] : undefined;
  const window = { limit: offset + limit, offset: 0 };

  const commandKinds = COMMAND_KINDS.filter((kind) => pageKinds.includes(kind));
  const commandLists = commandKinds.map((kind) =>
    commandListers[kind](
      { users: userId !== undefined ? [String(userId)] : undefined, workspaceId },
      options,
    ),
  );

  const genericParams = { projectId, search: term, slotsFilter, userIds, workspaceId };
  const withGeneric = pageKinds.includes(RunKind.GenericTask);
  const genericList: Promise<GenericTaskPagination | undefined> =
    withGeneric && kinds.includes(RunKind.GenericTask)
      ? getGenericTasks({ ...genericParams, ...window, states: genericTaskStates(states) }, options)
      : Promise.resolve(undefined);
  const genericCount: Promise<GenericTaskPagination | undefined> = withGeneric
    ? getGenericTasks(
        { ...genericParams, limit: 1, offset: 0, states: genericTaskStates(ACTIVE) },
        options,
      )
    : Promise.resolve(undefined);

  const experimentParams = {
    archived: false,
    orderBy: 'ORDER_BY_DESC' as const,
    projectId,
    slotsFilter,
    sortBy: 'SORT_BY_START_TIME' as const,
    userIds,
    workspaceId,
    ...experimentSearch(term),
  };
  const withExperiments = pageKinds.includes(RunKind.Experiment);
  const experimentList: Promise<ExperimentPagination | undefined> =
    withExperiments && kinds.includes(RunKind.Experiment)
      ? getExperiments(
          { ...experimentParams, ...window, states: experimentStates(states) },
          options,
        )
      : Promise.resolve(undefined);
  const experimentCount: Promise<ExperimentPagination | undefined> = withExperiments
    ? getExperiments(
        { ...experimentParams, limit: 1, offset: 0, states: experimentStates(ACTIVE) },
        options,
      )
    : Promise.resolve(undefined);

  const [commandResults, genericResults, experimentResults] = await Promise.all([
    Promise.allSettled(commandLists),
    Promise.allSettled([genericList, genericCount]),
    Promise.allSettled([experimentList, experimentCount]),
  ]);

  const errors: RunPage['errors'] = {};
  const activeCounts: RunPage['activeCounts'] = {};
  let total = 0;

  // Notebooks, shells, commands and TensorBoards.
  const commandTasks: CommandTask[] = [];
  commandResults.forEach((result, i) => {
    const kind = commandKinds[i];
    if (result.status === 'rejected') {
      errors[kind] = result.reason;
      return;
    }
    commandTasks.push(...result.value);
    activeCounts[kind] = 0;
  });
  const allCommandRows = commandTasks.map(commandRow);
  filterCommandRows(allCommandRows, { kinds: commandKinds, search: term, slots }).forEach((row) => {
    if (commandStateGroup(row.task.state) === StateGroup.Active) {
      activeCounts[row.kind] = (activeCounts[row.kind] ?? 0) + 1;
    }
  });
  const commandRows: CommandRunRow[] = sortCommandRows(
    filterCommandRows(allCommandRows, { kinds, search: term, slots, states }),
  );
  total += commandRows.length;

  // Generic tasks.
  const [genericListResult, genericCountResult] = genericResults;
  let genericRows: RunRow[] = [];
  if (genericListResult.status === 'rejected') {
    errors[RunKind.GenericTask] = genericListResult.reason;
  } else if (genericListResult.value) {
    genericRows = genericListResult.value.tasks.map(genericTaskRow);
    total += genericListResult.value.pagination.total ?? genericRows.length;
  }
  if (genericCountResult.status === 'fulfilled' && genericCountResult.value) {
    activeCounts[RunKind.GenericTask] = genericCountResult.value.pagination.total ?? 0;
  }

  // Experiments.
  const [experimentListResult, experimentCountResult] = experimentResults;
  let experimentRows: RunRow[] = [];
  if (experimentListResult.status === 'rejected') {
    errors[RunKind.Experiment] = experimentListResult.reason;
  } else if (experimentListResult.value) {
    experimentRows = experimentListResult.value.experiments.map(experimentRow);
    total += experimentListResult.value.pagination.total ?? experimentRows.length;
  }
  if (experimentCountResult.status === 'fulfilled' && experimentCountResult.value) {
    activeCounts[RunKind.Experiment] = experimentCountResult.value.pagination.total ?? 0;
  }

  return {
    activeCounts,
    errors,
    rows: pageOfRuns([commandRows, genericRows, experimentRows], offset, limit),
    total,
  };
};
