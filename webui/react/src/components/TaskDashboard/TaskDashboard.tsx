import { ColumnFilterItem, FilterDropdownProps, SortOrder } from 'antd/es/table/interface';
import Alert from 'hew/Alert';
import Button from 'hew/Button';
import Icon, { IconName } from 'hew/Icon';
import Input from 'hew/Input';
import { useModal } from 'hew/Modal';
import { Loadable } from 'hew/utils/loadable';
import _ from 'lodash';
import { useObservable } from 'micro-observables';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';

import Badge, { BadgeType } from 'components/Badge';
import BatchActionConfirmModalComponent from 'components/BatchActionConfirmModal';
import ExperimentActionDropdown from 'components/ExperimentActionDropdown';
import FilterCounter from 'components/FilterCounter';
import GenericTaskActionDropdown from 'components/GenericTaskActionDropdown';
import GenericTaskIdLink from 'components/GenericTaskIdLink';
import GenericTaskStateBadge from 'components/GenericTaskStateBadge';
import JupyterLabButton from 'components/JupyterLabButton';
import Link from 'components/Link';
import ShellButton from 'components/ShellButton';
import InteractiveTable, {
  ColumnDef,
  ContextMenuProps,
  onRightClickableCell,
} from 'components/Table/InteractiveTable';
import {
  defaultRowClassName,
  experimentNameRenderer,
  getFullPaginationConfig,
  relativeTimeRenderer,
  taskIdRenderer,
  taskNameRenderer,
  userRenderer,
} from 'components/Table/Table';
import TableBatch from 'components/Table/TableBatch';
import TableFilterDropdown from 'components/Table/TableFilterDropdown';
import TaskActionDropdown from 'components/TaskActionDropdown';
import TensorBoardSourcesModalComponent, {
  TensorBoardSource,
} from 'components/TensorBoardSourcesModal';
import useFeature from 'hooks/useFeature';
import {
  GenericTaskActionStateContext,
  useGenericTaskActionState,
} from 'hooks/useGenericTaskActions';
import { useLaunchAgain } from 'hooks/useLaunchAgain';
import usePermissions from 'hooks/usePermissions';
import usePolling from 'hooks/usePolling';
import { settingsToQuery, useSettings } from 'hooks/useSettings';
import { paths } from 'routes/utils';
import { killExperiment, killGenericTask, killTask } from 'services/api';
import clusterStore from 'stores/cluster';
import projectStore from 'stores/projects';
import userStore from 'stores/users';
import workspaceStore from 'stores/workspaces';
import { CommandTask, CommandType, DetailedUser, ExperimentAction, Workspace } from 'types';
import handleError, { ErrorLevel, ErrorType, isDetError } from 'utils/error';
import { getActionsForExperiment } from 'utils/experiment';
import { alphaNumericSorter, numericSorter } from 'utils/sort';
import { pluralizer } from 'utils/string';
import { canKillGenericTask, isTaskKillable } from 'utils/task';
import { compareText } from 'utils/textOrder';
import { getDisplayName } from 'utils/user';

import { fetchRunPage, RunPage, RunQuery } from './fetchRuns';
import {
  DashboardScope,
  DEFAULT_SORT,
  kindsOf,
  mostAgentSlots,
  MULTI_NODE,
  RUN_KINDS,
  RunKind,
  runKindLabel,
  runKindPluralLabel,
  RunRow,
  RunSort,
  savedSlots,
  slotsOptions,
  slotsQuery,
  SORT_KEYS,
  STATE_GROUPS,
  StateGroup,
  stateGroupLabel,
  tickedSlots,
} from './runRows';
import css from './TaskDashboard.module.scss';
import settingsConfig, {
  DEFAULT_COLUMN_WIDTHS,
  DEFAULT_COLUMNS,
  DEFAULT_PAGE_SIZE,
  FILTER_KEYS,
  MAX_PAGE_SIZE,
  MIN_COLUMN_WIDTH,
  MIN_SORT_FILTER_WIDTHS,
  NO_FILTERS,
  normalizedLayout,
  readFilters,
  Settings,
  TaskDashboardColumnName,
  urlView,
} from './TaskDashboard.settings';

interface Props {
  /** A project's dashboard: its experiments and generic tasks. */
  projectId?: number;
  /** The tasks-only view: notebooks, shells, commands, TensorBoards and generic tasks. */
  tasksOnly?: boolean;
  /** A workspace's dashboard, which launches notebooks and shells in it. */
  workspace?: Workspace;
}

const runKindIcon: Record<RunKind, IconName> = {
  [RunKind.Command]: 'command',
  [RunKind.Experiment]: 'experiment',
  [RunKind.GenericTask]: 'tasks',
  [RunKind.JupyterLab]: 'jupyter-lab',
  [RunKind.Shell]: 'shell',
  [RunKind.TensorBoard]: 'tensor-board',
};

const runKindSingularLabel: Record<RunKind, string> = {
  [RunKind.Command]: 'command',
  [RunKind.Experiment]: 'experiment',
  [RunKind.GenericTask]: 'generic task',
  [RunKind.JupyterLab]: 'JupyterLab',
  [RunKind.Shell]: 'shell',
  [RunKind.TensorBoard]: 'TensorBoard',
};

/** "2 experiments and 1 shell" */
export const describeKindCounts = (counts: Partial<Record<RunKind, number>>): string => {
  const parts = RUN_KINDS.filter((kind) => counts[kind]).map((kind) => {
    const count = counts[kind] ?? 0;
    return `${count} ${count === 1 ? runKindSingularLabel[kind] : runKindPluralLabel[kind]}`;
  });
  if (parts.length <= 1) return parts.join('');
  return `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}`;
};

const errorMessage = (error: unknown): string | undefined => {
  if (isDetError(error)) return error.publicMessage || error.message;
  if (error instanceof Error) return error.message;
  return undefined;
};

/* A generic task's Kill kills its descendants too, as its menu says. */
const GENERIC_TASK_KILL_NOTE = 'Each generic task is killed together with all its descendants.';

const PAGE_SIZE_OPTIONS = [10, 20, 50, MAX_PAGE_SIZE];
const SEARCH_DELAY_MS = 400;

/*
 * Three directions, so that every click flips the sort: with two, antd clears the sort on the third
 * click, and the column then keeps its direction.
 */
const DESCEND_FIRST: SortOrder[] = ['descend', 'ascend', 'descend'];
const ASCEND_FIRST: SortOrder[] = ['ascend', 'descend', 'ascend'];

const ExperimentEntityCopyMap = { Experiment: 'Experiment', Trial: 'Trial' } as const;
const RunEntityCopyMap = { Experiment: 'Search', Trial: 'Run' } as const;

/** Where a run is: its workspace and project, as far as the page does not already say. */
const RunLocation: React.FC<{
  row: RunRow;
  showWorkspace: boolean;
  workspaces: Workspace[];
}> = ({ row, showWorkspace, workspaces }) => {
  const genericProjectId = row.kind === RunKind.GenericTask ? row.task.projectId : undefined;
  const genericProject = Loadable.getOrElse(
    undefined,
    useObservable(projectStore.getProject(genericProjectId)),
  );
  const workspaceName =
    row.kind === RunKind.Experiment
      ? row.experiment.workspaceName
      : workspaces.find((ws) => ws.id === row.workspaceId)?.name;
  let project: React.ReactNode = '—';
  let projectName = '—';
  if (row.kind === RunKind.Experiment) {
    projectName = row.experiment.projectName ?? '';
    project = <Link path={paths.projectDetails(row.experiment.projectId)}>{projectName}</Link>;
  } else if (genericProjectId !== undefined) {
    projectName = genericProject?.name ?? `Project ${genericProjectId}`;
    project = <Link path={paths.projectDetails(genericProjectId)}>{projectName}</Link>;
  }
  // The cell cuts a long location short; its title shows the whole of it.
  if (!showWorkspace) return <span title={projectName}>{project}</span>;
  return (
    <span title={`${workspaceName ?? '—'} › ${projectName}`}>
      {workspaceName ? (
        <Link path={paths.workspaceDetails(row.workspaceId)}>{workspaceName}</Link>
      ) : (
        '—'
      )}
      <span className={css.separator}>›</span>
      {project}
    </span>
  );
};

/**
 * A column's funnel: a button that Enter or Space opens without sorting the column. It is pressed
 * while the column is filtered.
 */
const FilterButton: React.FC<{ filtered: boolean; label: string }> = ({ filtered, label }) => (
  <span
    aria-haspopup="listbox"
    aria-label={label}
    aria-pressed={filtered}
    className={css.funnel}
    role="button"
    tabIndex={0}
    onKeyDown={(e) => {
      if (e.key !== 'Enter' && e.key !== ' ') return;
      // The column header sorts on Enter.
      e.preventDefault();
      e.stopPropagation();
      e.currentTarget.click();
    }}>
    <Icon decorative name="filter" />
  </span>
);

/** The signed-in user first, then everyone else A to Z, as the Owner sort orders names. */
const ownersMeFirst = (users: DetailedUser[], me?: DetailedUser): DetailedUser[] => [
  ...(me ? [me] : []),
  ...users
    .filter((user) => user.id !== me?.id)
    .sort((a, b) => compareText(getDisplayName(a), getDisplayName(b))),
];

interface ChecklistFilter {
  /** Whether the column's filter is on. */
  filtered: boolean;
  /** What the column filters by: "Owner". */
  name: string;
  onFilter: (keys: string[]) => void;
  options: ColumnFilterItem[];
  searchable?: boolean;
  ticked: string[];
  width: number;
}

/** A column's tick list filter. */
const checklistFilter = ({
  filtered,
  name,
  onFilter,
  options,
  searchable,
  ticked,
  width,
}: ChecklistFilter): Pick<
  ColumnDef<RunRow>,
  'filterDropdown' | 'filterIcon' | 'filters' | 'isFiltered'
> => ({
  filterDropdown: (filterProps: FilterDropdownProps) => (
    <TableFilterDropdown
      {...filterProps}
      checklist
      label={name}
      multiple
      searchable={searchable}
      values={ticked}
      width={width}
      onFilter={onFilter}
    />
  ),
  filterIcon: <FilterButton filtered={filtered} label={`Filter by ${name.toLowerCase()}`} />,
  filters: options,
  isFiltered: () => filtered,
});

/**
 * One table of the runs of every kind: experiments, generic tasks, notebooks (JupyterLab), shells,
 * commands and TensorBoards, newest first by default.
 * - All workspaces (no props), a workspace, or a project (experiments and generic tasks only).
 * - The column headers sort and filter: kind, state, owner, slots and, on the global page,
 *   workspaces; the toolbar searches. Each dashboard stores them on its own, and a URL with any of
 *   them sets them all.
 * - Each row has its kind's action menu, in the actions column and on a right click; Kill works on
 *   a selection of any kinds.
 */
const TaskDashboard: React.FC<Props> = ({ projectId, tasksOnly = false, workspace }: Props) => {
  const workspaceId = workspace?.id;
  const scope: DashboardScope = useMemo(() => {
    if (projectId !== undefined) return { projectId, type: 'project' };
    if (workspaceId !== undefined) return { type: 'workspace', workspaceId };
    return { type: 'global' };
  }, [projectId, workspaceId]);
  const experiments = !tasksOnly;
  const pageKinds = useMemo(() => kindsOf(scope, experiments), [experiments, scope]);
  const config = useMemo(() => settingsConfig(scope, experiments), [experiments, scope]);
  const { isLoading, settings, updateSettings } = useSettings<Settings>(config);
  /*
   * The updates of the settings since the settings last changed. The settings store takes an update
   * a tick after the update writes the URL, from the store and the update: each update goes with
   * those before it, so that its URL has them too. An update that changes nothing is dropped.
   */
  const written = useRef<Partial<Settings>>({});
  useEffect(() => {
    written.current = {};
  }, [settings]);
  const writeSettings = useCallback(
    (update: Partial<Settings>) => {
      const view: Partial<Settings> = { ...settings, ...written.current };
      const changes = Object.keys(update).some(
        (key) => !_.isEqual(update[key as keyof Settings], view[key as keyof Settings]),
      );
      if (!changes) return;
      written.current = { ...written.current, ...update };
      updateSettings(written.current);
    },
    [settings, updateSettings],
  );

  const currentUser = Loadable.getOrElse(undefined, useObservable(userStore.currentUser));
  const users = Loadable.getOrElse([], useObservable(userStore.getUsers()));
  const workspaces = Loadable.getOrElse([], useObservable(workspaceStore.workspaces));
  const agents = Loadable.getOrElse([], useObservable(clusterStore.agents));
  const permissions = usePermissions();
  const { canCreateNSC, canCreateWorkspaceNSC, canModifyWorkspaceNSC } = permissions;
  const f_flat_runs = useFeature().isOn('flat_runs');
  const entityCopyMap = f_flat_runs ? RunEntityCopyMap : ExperimentEntityCopyMap;
  /*
   * Each row has two menus, in the actions column and on a right click; for a generic task they
   * share its running action and failed unpause, so that both offer the same.
   */
  const actionState = useGenericTaskActionState();
  const BatchActionConfirmModal = useModal(BatchActionConfirmModalComponent);
  const TensorBoardSourcesModal = useModal(TensorBoardSourcesModalComponent);
  const [tensorBoardSources, setTensorBoardSources] = useState<TensorBoardSource[]>();
  const [page, setPage] = useState<RunPage>();
  const [selected, setSelected] = useState<ReadonlyMap<string, RunRow>>(() => new Map());
  const containerRef = useRef<HTMLDivElement>(null);
  const canceler = useRef<AbortController>();
  const requestedProjects = useRef(new Set<number>());
  const appliedSearch = useRef<string>();
  const location = useLocation();
  const navigate = useNavigate();
  const currentUserId = currentUser?.id;

  // The filters, cleaned of what 0.41.0 saved before the first fetch.
  const read = useMemo(() => readFilters(settings, currentUserId), [currentUserId, settings]);
  const { cleanup, waitsForUser } = read;
  // The same filters keep their object, so that a change of another setting fetches nothing.
  const filtersRef = useRef(read.filters);
  const filters = useMemo(
    () => (_.isEqual(read.filters, filtersRef.current) ? filtersRef.current : read.filters),
    [read.filters],
  );
  useEffect(() => {
    filtersRef.current = filters;
  }, [filters]);

  /*
   * A URL with any filter, sort or page key sets the whole view, once: on the first load, and after
   * an in-app link or redirect such as /tasks/generic, which the settings read only on the first
   * page load. The URL's view and the clean-up of the saved filters go in one update. Each update
   * writes the URL, whose view is then the one of the settings. A URL without any of the keys, as
   * the app's links to the page are, opens the saved view and then shows it, the default view with
   * its sort key, so that a copied link shows the same rows.
   */
  useEffect(() => {
    // Settings that are still loading would drop the update; "Mine" needs the signed-in user.
    if (isLoading || currentUserId === undefined) return;
    let wanted: Partial<Settings> = { ...cleanup };
    if (location.search !== appliedSearch.current) {
      appliedSearch.current = location.search;
      const view = urlView(location.search, currentUserId);
      if (view) {
        wanted = { ...wanted, ...view };
      } else {
        const query = settingsToQuery(config, { ...settings, ...written.current, ...wanted });
        const search = query ? `?${query}` : '';
        if (search !== location.search) {
          appliedSearch.current = search;
          navigate({ search }, { replace: true });
        }
      }
    }
    writeSettings(wanted);
  }, [
    cleanup,
    config,
    currentUserId,
    isLoading,
    location.search,
    navigate,
    settings,
    writeSettings,
  ]);

  // Stored columns and widths, also from before the Slots column, get one width for each column.
  const layoutUpdate = useMemo(
    () =>
      isLoading
        ? undefined
        : normalizedLayout({ columns: settings.columns, columnWidths: settings.columnWidths }),
    [isLoading, settings.columns, settings.columnWidths],
  );
  useEffect(() => {
    if (layoutUpdate) writeSettings(layoutUpdate);
  }, [layoutUpdate, writeSettings]);
  /*
   * The table takes the widths it mounts with, and later ones only when their count changes. It
   * mounts again once the stored layout has loaded and has one width for each column, so that it
   * shows (and a resize keeps) the stored widths, not the default ones. It shows no rows before,
   * as a click on one could land on a row about to be replaced.
   */
  const layoutReady = !isLoading && !layoutUpdate;

  const selectedKinds = useMemo(
    () => (filters.type ?? []).filter((kind) => pageKinds.includes(kind)),
    [filters.type, pageKinds],
  );
  // Kinds of other pages, as a link can have, filter nothing here, and neither do all of the page's.
  const kindFiltered = selectedKinds.length > 0 && selectedKinds.length < pageKinds.length;
  const kinds = kindFiltered ? selectedKinds : pageKinds;
  const limit = Math.min(Math.max(settings.tableLimit || DEFAULT_PAGE_SIZE, 1), MAX_PAGE_SIZE);
  const offset = Math.max(settings.tableOffset || 0, 0);
  const sortKey = SORT_KEYS.includes(settings.sortKey) ? settings.sortKey : DEFAULT_SORT.key;
  const sortDesc = typeof settings.sortDesc === 'boolean' ? settings.sortDesc : DEFAULT_SORT.desc;
  /*
   * The Slots filter's N: the most slots of any agent, or the N that Multi-node was saved with if
   * larger, so that an agent that leaves, or agents still loading, never widen a saved filter.
   */
  const maxSlots = useMemo(
    () => Math.max(mostAgentSlots(agents), slotsQuery(filters.slots)?.above ?? 0),
    [agents, filters.slots],
  );

  // The settings the table shows: its sort arrows are those of the sort the page fetches.
  const tableSettings = useMemo(
    () => ({ ...settings, sortDesc, sortKey }),
    [settings, sortDesc, sortKey],
  );
  /*
   * The table stores only the widths on a resize. The columns they belong to are stored with them,
   * so that the widths still find their columns once the default columns change. A new sort goes to
   * the first page; a page click sends the sort too, unchanged.
   */
  const updateTableSettings = useCallback(
    (update: Partial<Settings>) => {
      const next = { ...update };
      if (next.columnWidths && !next.columns) next.columns = [...settings.columns];
      const sortChanged =
        ('sortKey' in next && next.sortKey !== sortKey) ||
        ('sortDesc' in next && next.sortDesc !== sortDesc);
      if (sortChanged) next.tableOffset = 0;
      writeSettings(next);
    },
    [settings.columns, sortDesc, sortKey, writeSettings],
  );

  const query: RunQuery | undefined = useMemo(() => {
    // The saved filters first; "Mine" waits for the signed-in user instead of listing everyone's.
    if (isLoading || waitsForUser) return undefined;
    const sort: RunSort = { desc: sortDesc, key: sortKey };
    return {
      kinds,
      limit,
      offset,
      scope,
      search: filters.search,
      slots: slotsQuery(filters.slots),
      sort,
      states: filters.state,
      userIds: filters.user,
      workspaceIds: scope.type === 'global' ? filters.workspace : undefined,
    };
  }, [filters, isLoading, kinds, limit, offset, scope, sortDesc, sortKey, waitsForUser]);

  const fetchRuns = useCallback(async () => {
    if (!query) return;
    canceler.current?.abort();
    const controller = new AbortController();
    canceler.current = controller;
    try {
      const result = await fetchRunPage(query, controller.signal);
      // A later fetch (new filters, a new page) or an unmount aborted this one: its reply is stale.
      if (controller.signal.aborted) return;
      setPage((prev) => (_.isEqual(prev, result) ? prev : result));
    } catch (e) {
      if (controller.signal.aborted) return;
      handleError(e, {
        publicSubject: 'Unable to fetch jobs.',
        silent: true,
        type: ErrorType.Api,
      });
    }
  }, [query]);

  // Every 5 seconds, paused while the browser tab is hidden.
  usePolling(fetchRuns, { rerunOnNewFn: true });

  useEffect(() => () => canceler.current?.abort(), []);

  useEffect(() => workspaceStore.fetch(), []);

  // The project names of the generic tasks shown, from their workspaces' projects.
  useEffect(() => {
    page?.rows.forEach((row) => {
      if (row.kind !== RunKind.GenericTask || requestedProjects.current.has(row.workspaceId)) {
        return;
      }
      requestedProjects.current.add(row.workspaceId);
      projectStore.fetch(row.workspaceId);
    });
  }, [page?.rows]);

  useEffect(() => {
    if (tensorBoardSources) TensorBoardSourcesModal.open();
  }, [TensorBoardSourcesModal, tensorBoardSources]);

  const { launchAgain, launchAgainModals } = useLaunchAgain({ onLaunched: fetchRuns, workspace });

  /* Filters */

  // The filters that the page applies: the workspaces only on the global page.
  const workspaceFiltered = scope.type === 'global' && filters.workspace !== undefined;
  const filterCount = FILTER_KEYS.filter((key) => {
    if (key === 'type') return kindFiltered;
    if (key === 'workspace') return workspaceFiltered;
    return filters[key] !== undefined;
  }).length;

  const clearFilters = useCallback(() => {
    // Not a reset of the settings, which would also drop the columns, their widths and the sort.
    writeSettings({ ...NO_FILTERS, tableOffset: 0 });
    setSelected(new Map());
  }, [writeSettings]);

  const [searchInput, setSearchInput] = useState(filters.search ?? '');
  useEffect(() => setSearchInput(filters.search ?? ''), [filters.search]);
  useEffect(() => {
    const search = searchInput || undefined;
    if (search === filters.search) return;
    const timer = setTimeout(() => writeSettings({ search, tableOffset: 0 }), SEARCH_DELAY_MS);
    return () => clearTimeout(timer);
  }, [filters.search, searchInput, writeSettings]);

  const owners = useMemo(() => ownersMeFirst(users, currentUser), [currentUser, users]);

  const columnFilters = useMemo(() => {
    const apply = (update: Partial<Settings>) => writeSettings({ ...update, tableOffset: 0 });
    const ids = (keys: string[]) => (keys.length > 0 ? keys.map(Number) : undefined);
    return {
      kind: checklistFilter({
        filtered: kindFiltered,
        name: 'Kind',
        onFilter: (keys) => apply({ type: keys.length > 0 ? (keys as RunKind[]) : undefined }),
        options: pageKinds.map((kind) => ({
          text: (
            <span className={css.kindOption}>
              <Icon decorative name={runKindIcon[kind]} size="small" />
              {runKindLabel[kind]}
            </span>
          ),
          value: kind,
        })),
        ticked: selectedKinds,
        width: 180,
      }),
      location: checklistFilter({
        filtered: workspaceFiltered,
        name: 'Workspace',
        onFilter: (keys) => apply({ workspace: ids(keys) }),
        options: workspaces.map((ws) => ({ text: ws.name, value: String(ws.id) })),
        searchable: true,
        ticked: (filters.workspace ?? []).map(String),
        width: 240,
      }),
      slots: checklistFilter({
        filtered: filters.slots !== undefined,
        name: 'Slots',
        onFilter: (keys) =>
          apply({ slots: keys.length > 0 ? savedSlots(maxSlots, keys) : undefined }),
        options: slotsOptions(maxSlots, filters.slots).map((option) => ({
          text: option === MULTI_NODE ? 'Multi-node' : option,
          value: option,
        })),
        ticked: tickedSlots(maxSlots, filters.slots),
        width: 160,
      }),
      state: checklistFilter({
        filtered: filters.state !== undefined,
        name: 'State',
        onFilter: (keys) => apply({ state: keys.length > 0 ? (keys as StateGroup[]) : undefined }),
        options: STATE_GROUPS.map((group) => ({ text: stateGroupLabel[group], value: group })),
        ticked: filters.state ?? [],
        width: 160,
      }),
      user: checklistFilter({
        filtered: filters.user !== undefined,
        name: 'Owner',
        onFilter: (keys) => apply({ user: ids(keys) }),
        options: owners.map((user) => ({ text: getDisplayName(user), value: String(user.id) })),
        searchable: true,
        ticked: (filters.user ?? []).map(String),
        width: 220,
      }),
    };
  }, [
    filters,
    kindFiltered,
    maxSlots,
    owners,
    pageKinds,
    selectedKinds,
    workspaceFiltered,
    workspaces,
    writeSettings,
  ]);

  /* Actions */

  const canKill = useCallback(
    (row: RunRow): boolean => {
      const canControl = canModifyWorkspaceNSC({
        userId: row.userId,
        workspace: { id: row.workspaceId },
      });
      switch (row.kind) {
        case RunKind.Experiment:
          return getActionsForExperiment(
            row.experiment,
            [ExperimentAction.Kill],
            permissions,
          ).includes(ExperimentAction.Kill);
        case RunKind.GenericTask:
          return canKillGenericTask(row.task, canControl);
        default:
          return isTaskKillable(row.task, canControl);
      }
    },
    [canModifyWorkspaceNSC, permissions],
  );

  // The selection as it is now: a row on the page has its latest state.
  const selectedRows = useMemo(
    () =>
      [...selected.keys()].map(
        (key) => page?.rows.find((row) => row.key === key) ?? (selected.get(key) as RunRow),
      ),
    [page?.rows, selected],
  );
  const hasKillable = useMemo(() => selectedRows.some(canKill), [canKill, selectedRows]);
  const killsGenericTasks = useMemo(
    () => selectedRows.some((row) => row.kind === RunKind.GenericTask && canKill(row)),
    [canKill, selectedRows],
  );

  const handleBatchKill = useCallback(async () => {
    const targets = selectedRows.filter(canKill);
    const failures: Partial<Record<RunKind, number>> = {};
    const kill = async (row: RunRow) => {
      try {
        switch (row.kind) {
          case RunKind.Experiment:
            await killExperiment({ experimentId: row.experiment.id });
            break;
          case RunKind.GenericTask:
            await killGenericTask({ taskId: row.task.taskId });
            break;
          default:
            await killTask(row.task);
        }
      } catch {
        failures[row.kind] = (failures[row.kind] ?? 0) + 1;
      }
    };
    /*
     * The master refuses a generic task kill while another one runs (409), so the generic tasks
     * are killed one after another, alongside the other kinds.
     */
    const genericTasks = targets.filter((row) => row.kind === RunKind.GenericTask);
    const killGenericTasksInTurn = async () => {
      for (const row of genericTasks) await kill(row);
    };
    await Promise.all([
      ...targets.filter((row) => row.kind !== RunKind.GenericTask).map(kill),
      killGenericTasksInTurn(),
    ]);
    // The killed rows may no longer pass the filters.
    setSelected(new Map());
    fetchRuns();
    if (Object.keys(failures).length > 0) {
      handleError(new Error(`Unable to kill ${describeKindCounts(failures)}.`), {
        level: ErrorLevel.Error,
        publicMessage: `Could not kill ${describeKindCounts(failures)}. Please try again later.`,
        publicSubject: 'Unable to Kill Selected Jobs',
        silent: false,
        type: ErrorType.Server,
      });
    }
  }, [canKill, fetchRuns, selectedRows]);

  const handleRowSelect = useCallback((keys: React.Key[], rows: RunRow[]) => {
    setSelected((prev) => {
      const next = new Map<string, RunRow>();
      keys.forEach((key) => {
        const row = rows.find((r) => r?.key === key) ?? prev.get(String(key));
        if (row) next.set(String(key), row);
      });
      return next;
    });
  }, []);

  const handleExperimentComplete = useCallback(() => {
    fetchRuns();
  }, [fetchRuns]);

  const renderMenu = useCallback(
    (row: RunRow, contextMenu?: Omit<ContextMenuProps<RunRow>, 'record'>) => {
      if (row.kind === RunKind.Experiment) {
        return (
          <ExperimentActionDropdown
            experiment={row.experiment}
            isContextMenu={!!contextMenu}
            onComplete={handleExperimentComplete}
            onVisibleChange={contextMenu?.onVisibleChange}>
            {contextMenu?.children}
          </ExperimentActionDropdown>
        );
      }
      if (row.kind === RunKind.GenericTask) {
        return (
          <GenericTaskActionDropdown task={row.task} onComplete={fetchRuns}>
            {contextMenu?.children}
          </GenericTaskActionDropdown>
        );
      }
      return (
        <TaskActionDropdown task={row.task} onComplete={fetchRuns} onLaunchAgain={launchAgain}>
          {contextMenu?.children}
        </TaskActionDropdown>
      );
    },
    [fetchRuns, handleExperimentComplete, launchAgain],
  );

  const RowContextMenu = useCallback(
    ({ record, ...contextMenu }: ContextMenuProps<RunRow>) => renderMenu(record, contextMenu),
    [renderMenu],
  );

  /* Columns */

  const columns = useMemo(() => {
    const timeRenderer = (time?: string): React.ReactNode =>
      time ? relativeTimeRenderer(new Date(time)) : '—';

    const sourcesOf = (task: CommandTask): TensorBoardSource[] =>
      [
        ...(task.misc?.experimentIds ?? []).map((id) => ({
          id,
          path: paths.experimentDetails(id),
          type: entityCopyMap.Experiment,
        })),
        ...(task.misc?.trialIds ?? []).map((id) => ({
          id,
          path: paths.trialDetails(id),
          type: entityCopyMap.Trial,
        })),
      ].sort((a, b) =>
        a.type !== b.type ? alphaNumericSorter(a.type, b.type) : numericSorter(a.id, b.id),
      );

    const cols: Array<ColumnDef<RunRow> | false> = [
      {
        dataIndex: 'kind',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.kind,
        ...columnFilters.kind,
        // The sort key of the old task list's Type column.
        key: 'type',
        render: (_: unknown, row: RunRow) => (
          <div className={css.kind}>
            <Icon name={runKindIcon[row.kind]} title={runKindLabel[row.kind]} />
          </div>
        ),
        sortDirections: ASCEND_FIRST,
        sorter: true,
        title: 'Kind',
      },
      {
        dataIndex: 'id',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.id,
        key: 'id',
        onCell: () => ({ 'data-testid': 'taskID' }),
        render: (_: unknown, row: RunRow, index: number) => {
          if (row.kind === RunKind.Experiment) {
            return (
              <Link path={paths.experimentDetails(row.experiment.id)}>
                <Badge type={BadgeType.Id}>{row.experiment.id}</Badge>
              </Link>
            );
          }
          if (row.kind === RunKind.GenericTask)
            return <GenericTaskIdLink taskId={row.task.taskId} />;
          return taskIdRenderer(row.id, row.task, index);
        },
        responsive: ['md'],
        title: 'ID',
      },
      {
        dataIndex: 'name',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.name,
        // A long name is cut short with an ellipsis, with the whole name in its title or tooltip.
        ellipsis: true,
        key: 'name',
        render: (_: unknown, row: RunRow, index: number) => {
          if (row.kind === RunKind.Experiment)
            return experimentNameRenderer(row.name, row.experiment);
          if (row.kind === RunKind.GenericTask) {
            return <Link path={paths.genericTaskDetails(row.task.taskId)}>{row.name}</Link>;
          }
          const name = (
            <div className={css.name} title={row.name}>
              {taskNameRenderer(row.name, row.task, index)}
            </div>
          );
          if (row.task.type !== CommandType.TensorBoard || !row.task.misc) return name;
          const sources = sourcesOf(row.task);
          return (
            <div className={css.sourceName}>
              {name}
              <Button type="text" onClick={() => setTensorBoardSources(sources)}>
                Show {sources.length} {pluralizer(sources.length, 'Source')}
              </Button>
            </div>
          );
        },
        sortDirections: ASCEND_FIRST,
        sorter: true,
        title: 'Name',
      },
      {
        dataIndex: 'state',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.state,
        ...columnFilters.state,
        key: 'state',
        onCell: () => ({ 'data-testid': 'state' }),
        render: (_: unknown, row: RunRow) => {
          if (row.kind === RunKind.Experiment) {
            return <Badge state={row.experiment.state} type={BadgeType.State} />;
          }
          if (row.kind === RunKind.GenericTask)
            return <GenericTaskStateBadge state={row.task.state} />;
          return <Badge state={row.task.state} type={BadgeType.State} />;
        },
        sortDirections: ASCEND_FIRST,
        sorter: true,
        title: 'State',
      },
      {
        dataIndex: 'user',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.user,
        ellipsis: true,
        ...columnFilters.user,
        key: 'user',
        render: (_: unknown, row: RunRow) => {
          const user = users.find((u) => u.id === row.userId);
          if (user) return userRenderer(user);
          return row.ownerName ?? '—';
        },
        sortDirections: ASCEND_FIRST,
        sorter: true,
        title: 'Owner',
      },
      scope.type !== 'project' && {
        dataIndex: 'location',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.location,
        ellipsis: true,
        ...(scope.type === 'global' ? columnFilters.location : {}),
        key: 'location',
        render: (_: unknown, row: RunRow) => (
          <RunLocation row={row} showWorkspace={scope.type === 'global'} workspaces={workspaces} />
        ),
        title: scope.type === 'global' ? 'Workspace › Project' : 'Project',
      },
      {
        dataIndex: 'resourcePool',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.resourcePool,
        ellipsis: true,
        key: 'resourcePool',
        responsive: ['md'],
        sortDirections: ASCEND_FIRST,
        sorter: true,
        title: 'Resource Pool',
      },
      {
        align: 'right',
        dataIndex: 'slots',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.slots,
        ...columnFilters.slots,
        key: 'slots',
        onCell: () => ({ 'data-testid': 'slots-cell' }),
        render: (_: unknown, row: RunRow) => {
          if (row.slots === undefined) return '—';
          if (row.kind !== RunKind.Experiment) return row.slots;
          return <span title="Slots per trial">{row.slots}</span>;
        },
        sortDirections: DESCEND_FIRST,
        sorter: true,
        title: 'Slots',
      },
      {
        dataIndex: 'startTime',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.startTime,
        key: 'startTime',
        render: (_: unknown, row: RunRow) => timeRenderer(row.startTime),
        sortDirections: DESCEND_FIRST,
        sorter: true,
        title: 'Started',
      },
      {
        dataIndex: 'endTime',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.endTime,
        key: 'endTime',
        // Notebooks, shells, commands and TensorBoards have no end time in their API.
        render: (_: unknown, row: RunRow) => timeRenderer(row.endTime),
        responsive: ['md'],
        sortDirections: DESCEND_FIRST,
        sorter: true,
        title: 'Ended',
      },
      {
        align: 'right',
        className: 'fullCell',
        dataIndex: 'action',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.action,
        fixed: 'right',
        key: 'action',
        onCell: () => ({ ...onRightClickableCell(), 'data-testid': 'actions' }),
        render: (_: unknown, row: RunRow) => renderMenu(row),
        title: '',
      },
    ];
    return cols
      .filter((col): col is ColumnDef<RunRow> => !!col)
      .map((col) => ({
        ...col,
        minWidth:
          (col.sorter || col.filterDropdown
            ? MIN_SORT_FILTER_WIDTHS[col.dataIndex as TaskDashboardColumnName]
            : undefined) ?? Math.min(col.defaultWidth, MIN_COLUMN_WIDTH),
      }));
  }, [columnFilters, entityCopyMap, renderMenu, scope.type, users, workspaces]);

  /* Layout */

  const launchEnabled = workspace ? canCreateWorkspaceNSC({ workspace }) : canCreateNSC;
  const showLaunch = scope.type !== 'project';
  const itemName = experiments ? 'job' : 'task';

  return (
    <div className={css.base} ref={containerRef}>
      <div className={css.toolbar}>
        <div className={css.filters}>
          <Input
            allowClear
            placeholder="Search name or ID"
            prefix={<Icon name="search" size="tiny" title="Search" />}
            value={searchInput}
            width={200}
            onChange={(e) => setSearchInput(e.target.value)}
          />
          <FilterCounter activeFilterCount={filterCount} onReset={clearFilters} />
        </div>
        {showLaunch && (
          <div className={css.launch}>
            <JupyterLabButton enabled={launchEnabled} workspace={workspace} />
            <ShellButton enabled={launchEnabled} workspace={workspace} />
          </div>
        )}
      </div>
      {RUN_KINDS.filter((kind) => page?.errors[kind] !== undefined).map((kind) => (
        <Alert
          description={errorMessage(page?.errors[kind])}
          key={kind}
          message={`Unable to load ${runKindPluralLabel[kind]}. They are left out of the list.`}
          showIcon
          type="error"
        />
      ))}
      <TableBatch
        actions={[
          { disabled: !hasKillable, label: ExperimentAction.Kill, value: ExperimentAction.Kill },
        ]}
        selectedRowCount={selected.size}
        onAction={() => BatchActionConfirmModal.open()}
        onClear={() => setSelected(new Map())}
      />
      <div className={css.table}>
        <GenericTaskActionStateContext.Provider value={actionState}>
          <InteractiveTable<RunRow, Settings>
            columns={columns}
            containerRef={containerRef}
            ContextMenu={RowContextMenu}
            dataSource={page?.rows}
            defaultColumns={DEFAULT_COLUMNS}
            key={layoutReady ? 'layout-ready' : 'layout-pending'}
            loading={page === undefined || !layoutReady}
            pagination={{
              ...getFullPaginationConfig({ limit, offset }, page?.total ?? 0),
              pageSizeOptions: PAGE_SIZE_OPTIONS,
            }}
            rowClassName={defaultRowClassName({ clickable: false })}
            rowKey="key"
            rowSelection={{
              onChange: handleRowSelect,
              preserveSelectedRowKeys: true,
              selectedRowKeys: [...selected.keys()],
            }}
            // A definite width keeps the table's fixed layout: with 'max-content' the columns grew
            // to their longest content.
            scroll={{ x: '100%' }}
            settings={tableSettings}
            showSorterTooltip={false}
            size="small"
            updateSettings={updateTableSettings}
          />
        </GenericTaskActionStateContext.Provider>
      </div>
      <BatchActionConfirmModal.Component
        batchAction={ExperimentAction.Kill}
        itemName={itemName}
        note={killsGenericTasks ? GENERIC_TASK_KILL_NOTE : undefined}
        onConfirm={handleBatchKill}
      />
      <TensorBoardSourcesModal.Component
        sources={tensorBoardSources ?? []}
        onClose={() => setTensorBoardSources(undefined)}
      />
      {launchAgainModals}
    </div>
  );
};

export default TaskDashboard;
