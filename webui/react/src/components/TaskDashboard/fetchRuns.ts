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
  DashboardScope,
  DEFAULT_SORT,
  experimentApiSort,
  experimentRow,
  experimentSearch,
  experimentStates,
  filterCommandRows,
  genericTaskApiSort,
  genericTaskRow,
  genericTaskStates,
  pageOfRuns,
  RunKind,
  RunRow,
  RunSort,
  SlotsQuery,
  sortCommandRows,
  StateGroup,
  toApiSlots,
} from './runRows';

/** What one page of the dashboard shows. */
export interface RunQuery {
  /** The kinds to list. */
  kinds: RunKind[];
  limit: number;
  offset: number;
  scope: DashboardScope;
  search?: string;
  slots?: SlotsQuery;
  sort?: RunSort;
  states?: StateGroup[];
  /** The owners whose runs to list; everyone's without. */
  userIds?: number[];
  /** On the global page, the runs of these workspaces only. */
  workspaceIds?: number[];
}

export interface RunPage {
  /** The listed kinds whose list failed, with the error; they are left out of the rows and total. */
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

/**
 * Fetches one page of the dashboard, of the kinds asked for only.
 * - Experiments and generic tasks are sorted, filtered and paged by the master: each is fetched
 *   with offset 0 and limit offset + limit.
 * - Notebooks, shells, commands and TensorBoards are listed whole by the master, and filtered and
 *   sorted here in the same order.
 * - The sorted lists are merged and the page's rows cut out; the total is the sum of the three.
 * Each list that fails is left out and reported in `errors`.
 */
export const fetchRunPage = async (query: RunQuery, signal?: AbortSignal): Promise<RunPage> => {
  const { kinds, limit, offset, scope, search, slots, states, userIds } = query;
  const sort = query.sort ?? DEFAULT_SORT;
  const options = { signal };
  const workspaceId = scope.type === 'workspace' ? scope.workspaceId : undefined;
  const workspaceIds =
    scope.type === 'global' && query.workspaceIds?.length ? query.workspaceIds : undefined;
  const projectId = scope.type === 'project' ? scope.projectId : undefined;
  const term = search?.trim() || undefined;
  const apiSlots = toApiSlots(slots);
  const owners = userIds?.length ? userIds : undefined;
  const window = { limit: offset + limit, offset: 0 };

  const commandKinds = COMMAND_KINDS.filter((kind) => kinds.includes(kind));
  // Limit 0: all of them. Without it the API wrapper asks for the first 1000 by ID, not the newest.
  const commandLists = commandKinds.map((kind) =>
    commandListers[kind](
      { limit: 0, users: owners?.map((id) => String(id)), workspaceId },
      options,
    ),
  );

  const genericList: Promise<GenericTaskPagination | undefined> = kinds.includes(
    RunKind.GenericTask,
  )
    ? getGenericTasks(
        {
          ...window,
          ...apiSlots,
          ...genericTaskApiSort(sort),
          projectId,
          search: term,
          states: genericTaskStates(states),
          userIds: owners,
          workspaceId,
          workspaceIds,
        },
        options,
      )
    : Promise.resolve(undefined);

  const experimentList: Promise<ExperimentPagination | undefined> = kinds.includes(
    RunKind.Experiment,
  )
    ? getExperiments(
        {
          ...window,
          ...apiSlots,
          ...experimentApiSort(sort),
          ...experimentSearch(term),
          archived: false,
          projectId,
          states: experimentStates(states),
          userIds: owners,
          workspaceId,
          workspaceIds,
        },
        options,
      )
    : Promise.resolve(undefined);

  const [commandResults, [genericResult, experimentResult]] = await Promise.all([
    Promise.allSettled(commandLists),
    Promise.allSettled([genericList, experimentList]),
  ]);

  const errors: RunPage['errors'] = {};
  let total = 0;

  // Notebooks, shells, commands and TensorBoards.
  const commandTasks: CommandTask[] = [];
  commandResults.forEach((result, i) => {
    if (result.status === 'rejected') errors[commandKinds[i]] = result.reason;
    else commandTasks.push(...result.value);
  });
  const commandRows: CommandRunRow[] = sortCommandRows(
    filterCommandRows(commandTasks.map(commandRow), {
      kinds,
      search: term,
      slots,
      states,
      userIds: owners,
      workspaceIds,
    }),
    sort,
  );
  total += commandRows.length;

  // Generic tasks.
  let genericRows: RunRow[] = [];
  if (genericResult.status === 'rejected') {
    errors[RunKind.GenericTask] = genericResult.reason;
  } else if (genericResult.value) {
    genericRows = genericResult.value.tasks.map(genericTaskRow);
    total += genericResult.value.pagination.total ?? genericRows.length;
  }

  // Experiments.
  let experimentRows: RunRow[] = [];
  if (experimentResult.status === 'rejected') {
    errors[RunKind.Experiment] = experimentResult.reason;
  } else if (experimentResult.value) {
    experimentRows = experimentResult.value.experiments.map(experimentRow);
    total += experimentResult.value.pagination.total ?? experimentRows.length;
  }

  return {
    errors,
    rows: pageOfRuns([commandRows, genericRows, experimentRows], offset, limit, sort),
    total,
  };
};
