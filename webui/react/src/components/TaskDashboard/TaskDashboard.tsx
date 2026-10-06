import { FilterDropdownProps } from 'antd/es/table/interface';
import Alert from 'hew/Alert';
import Button from 'hew/Button';
import Icon, { IconName } from 'hew/Icon';
import Input from 'hew/Input';
import { useModal } from 'hew/Modal';
import Select, { Option, SelectValue } from 'hew/Select';
import Tooltip from 'hew/Tooltip';
import { Loadable } from 'hew/utils/loadable';
import _ from 'lodash';
import { useObservable } from 'micro-observables';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useLocation } from 'react-router-dom';

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
import TaskListModalComponent, { SourceInfo } from 'components/TaskListModalComponent';
import WorkspaceFilter from 'components/WorkspaceFilter';
import useFeature from 'hooks/useFeature';
import {
  GenericTaskActionStateContext,
  useGenericTaskActionState,
} from 'hooks/useGenericTaskActions';
import { useLaunchAgain } from 'hooks/useLaunchAgain';
import usePermissions from 'hooks/usePermissions';
import usePolling from 'hooks/usePolling';
import { useSettings } from 'hooks/useSettings';
import { paths } from 'routes/utils';
import { killExperiment, killGenericTask, killTask } from 'services/api';
import projectStore from 'stores/projects';
import userStore from 'stores/users';
import workspaceStore from 'stores/workspaces';
import { CommandTask, CommandType, ExperimentAction, Workspace } from 'types';
import handleError, { ErrorLevel, ErrorType, isDetError } from 'utils/error';
import { getActionsForExperiment } from 'utils/experiment';
import { alphaNumericSorter, numericSorter } from 'utils/sort';
import { canKillGenericTask, isTaskKillable } from 'utils/task';

import { fetchRunPage, RunPage, RunQuery } from './fetchRuns';
import {
  DashboardScope,
  isCommandKind,
  kindsOf,
  RUN_KINDS,
  RunKind,
  runKindLabel,
  runKindPluralLabel,
  RunRow,
  SlotsFilter,
  slotsFilterLabel,
  STATE_GROUPS,
  StateGroup,
  stateGroupLabel,
} from './runRows';
import css from './TaskDashboard.module.scss';
import settingsConfig, {
  DEFAULT_COLUMN_WIDTHS,
  DEFAULT_COLUMNS,
  DEFAULT_PAGE_SIZE,
  FILTER_KEYS,
  MAX_PAGE_SIZE,
  Owner,
  Settings,
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

const ANY_SLOTS = 'any';
const SLOTS_TOOLTIP = 'GPU: asks for at least one slot. CPU-only: asks for none.';
const PAGE_SIZE_OPTIONS = [10, 20, 50, MAX_PAGE_SIZE];
const SEARCH_DELAY_MS = 400;

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
  if (row.kind === RunKind.Experiment) {
    project = (
      <Link path={paths.projectDetails(row.experiment.projectId)}>
        {row.experiment.projectName}
      </Link>
    );
  } else if (genericProjectId !== undefined) {
    project = (
      <Link path={paths.projectDetails(genericProjectId)}>
        {genericProject?.name ?? `Project ${genericProjectId}`}
      </Link>
    );
  }
  if (!showWorkspace) return <span className={css.location}>{project}</span>;
  return (
    <span className={css.location}>
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
 * One table of the runs of every kind: experiments, generic tasks, notebooks (JupyterLab), shells,
 * commands and TensorBoards, newest first.
 * - All workspaces (no props), a workspace, or a project (experiments and generic tasks only).
 * - Filters: kind chips that count the active runs of each kind, search, owner, state, GPU or
 *   CPU-only, and on the global page one workspace. Each dashboard stores them on its own.
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
  const listsCommands = pageKinds.some(isCommandKind);
  const config = useMemo(() => settingsConfig(scope, experiments), [experiments, scope]);
  const { activeSettings, isLoading, resetSettings, settings, updateSettings } =
    useSettings<Settings>(config);

  const currentUser = Loadable.getOrElse(undefined, useObservable(userStore.currentUser));
  const users = Loadable.getOrElse([], useObservable(userStore.getUsers()));
  const workspaces = Loadable.getOrElse([], useObservable(workspaceStore.workspaces));
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
  const taskListModal = useModal(TaskListModalComponent);
  const [sourcesModal, setSourcesModal] = useState<SourceInfo>();
  const [page, setPage] = useState<RunPage>();
  const [selected, setSelected] = useState<ReadonlyMap<string, RunRow>>(() => new Map());
  const containerRef = useRef<HTMLDivElement>(null);
  const fetchSeq = useRef(0);
  const canceler = useRef<AbortController>();
  const requestedProjects = useRef(new Set<number>());
  const location = useLocation();

  /*
   * The URL's kinds win over the stored ones, also after an in-app redirect such as /tasks/generic,
   * which the settings read only on the first page load.
   */
  const urlKinds = useMemo(
    () =>
      new URLSearchParams(location.search)
        .getAll('type')
        .filter((value): value is RunKind => (RUN_KINDS as string[]).includes(value)),
    [location.search],
  );
  useEffect(() => {
    // Settings that are still loading would drop the update.
    if (isLoading || urlKinds.length === 0 || _.isEqual(urlKinds, settings.type)) return;
    updateSettings({ tableOffset: 0, type: urlKinds });
  }, [isLoading, settings.type, updateSettings, urlKinds]);

  const selectedKinds = useMemo(
    () => (settings.type ?? []).filter((kind) => pageKinds.includes(kind)),
    [pageKinds, settings.type],
  );
  const kinds = selectedKinds.length > 0 ? selectedKinds : pageKinds;
  const limit = Math.min(Math.max(settings.tableLimit || DEFAULT_PAGE_SIZE, 1), MAX_PAGE_SIZE);
  const offset = Math.max(settings.tableOffset || 0, 0);
  const mine = settings.owner === Owner.Mine;
  const currentUserId = currentUser?.id;

  const query: RunQuery | undefined = useMemo(() => {
    // "Mine" waits for the signed-in user instead of listing everyone's runs.
    if (mine && currentUserId === undefined) return undefined;
    return {
      kinds,
      limit,
      offset,
      pageKinds,
      scope,
      search: settings.search,
      slots: settings.slots,
      states: settings.state,
      userId: mine ? currentUserId : undefined,
      workspaceId: scope.type === 'global' ? settings.workspace : undefined,
    };
  }, [
    currentUserId,
    kinds,
    limit,
    mine,
    offset,
    pageKinds,
    scope,
    settings.search,
    settings.slots,
    settings.state,
    settings.workspace,
  ]);

  const fetchRuns = useCallback(async () => {
    if (!query) return;
    canceler.current?.abort();
    const controller = new AbortController();
    canceler.current = controller;
    const seq = ++fetchSeq.current;
    try {
      const result = await fetchRunPage(query, controller.signal);
      // A later fetch (new filters, a new page) replaces this one.
      if (seq !== fetchSeq.current) return;
      setPage((prev) => (_.isEqual(prev, result) ? prev : result));
    } catch (e) {
      if (seq !== fetchSeq.current) return;
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
    if (sourcesModal) taskListModal.open();
  }, [taskListModal, sourcesModal]);

  const { launchAgain, launchAgainModals } = useLaunchAgain({ onLaunched: fetchRuns, workspace });

  /* Filters */

  const filterCount = useMemo(() => activeSettings(FILTER_KEYS).length, [activeSettings]);

  const resetFilters = useCallback(() => {
    resetSettings([...FILTER_KEYS, 'tableOffset']);
    setSelected(new Map());
  }, [resetSettings]);

  const handleKindToggle = useCallback(
    (kind: RunKind) => {
      const next = selectedKinds.includes(kind)
        ? selectedKinds.filter((k) => k !== kind)
        : pageKinds.filter((k) => k === kind || selectedKinds.includes(k));
      updateSettings({
        tableOffset: 0,
        type: next.length === 0 || next.length === pageKinds.length ? undefined : next,
      });
    },
    [pageKinds, selectedKinds, updateSettings],
  );

  const [searchInput, setSearchInput] = useState(settings.search ?? '');
  useEffect(() => setSearchInput(settings.search ?? ''), [settings.search]);
  useEffect(() => {
    const search = searchInput || undefined;
    if (search === (settings.search || undefined)) return;
    const timer = setTimeout(() => updateSettings({ search, tableOffset: 0 }), SEARCH_DELAY_MS);
    return () => clearTimeout(timer);
  }, [searchInput, settings.search, updateSettings]);

  const handleOwnerChange = useCallback(
    (value: SelectValue) => updateSettings({ owner: value as Owner, tableOffset: 0 }),
    [updateSettings],
  );

  const handleStateChange = useCallback(
    (value: SelectValue) => {
      const states = (Array.isArray(value) ? value : []) as StateGroup[];
      updateSettings({ state: states.length ? states : undefined, tableOffset: 0 });
    },
    [updateSettings],
  );

  const handleSlotsChange = useCallback(
    (value: SelectValue) =>
      updateSettings({
        slots: value === ANY_SLOTS ? undefined : (value as SlotsFilter),
        tableOffset: 0,
      }),
    [updateSettings],
  );

  const workspaceFilterDropdown = useCallback(
    (filterProps: FilterDropdownProps) => (
      <TableFilterDropdown
        {...filterProps}
        values={settings.workspace !== undefined ? [String(settings.workspace)] : []}
        width={220}
        onFilter={(values: string[]) =>
          updateSettings({
            tableOffset: 0,
            workspace: values.length ? Number(values[values.length - 1]) : undefined,
          })
        }
        onReset={() => updateSettings({ tableOffset: 0, workspace: undefined })}
      />
    ),
    [settings.workspace, updateSettings],
  );

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
        <TaskActionDropdown
          task={row.task}
          onComplete={fetchRuns}
          onLaunchAgain={launchAgain}
          onVisibleChange={contextMenu?.onVisibleChange}>
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

    const tensorBoardSources = (task: CommandTask): SourceInfo => {
      const info: SourceInfo = { path: '', plural: '', sources: [] };
      task.misc?.experimentIds.forEach((id) => {
        info.sources.push({
          id,
          path: paths.experimentDetails(id),
          type: entityCopyMap.Experiment,
        });
      });
      task.misc?.trialIds.forEach((id) => {
        info.sources.push({ id, path: paths.trialDetails(id), type: entityCopyMap.Trial });
      });
      if (info.sources.length > 1) info.plural = 's';
      info.sources.sort((a, b) =>
        a.type !== b.type ? alphaNumericSorter(a.type, b.type) : numericSorter(a.id, b.id),
      );
      return info;
    };

    const cols: Array<ColumnDef<RunRow> | false> = [
      {
        dataIndex: 'kind',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.kind,
        key: 'kind',
        render: (_: unknown, row: RunRow) => (
          <div className={css.kind}>
            <Icon name={runKindIcon[row.kind]} title={runKindLabel[row.kind]} />
          </div>
        ),
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
        key: 'name',
        render: (_: unknown, row: RunRow, index: number) => {
          if (row.kind === RunKind.Experiment)
            return experimentNameRenderer(row.name, row.experiment);
          if (row.kind === RunKind.GenericTask) {
            return <Link path={paths.genericTaskDetails(row.task.taskId)}>{row.name}</Link>;
          }
          const name = taskNameRenderer(row.name, row.task, index);
          if (row.task.type !== CommandType.TensorBoard || !row.task.misc) return name;
          const info = tensorBoardSources(row.task);
          return (
            <div className={css.sourceName}>
              {name}
              <Button type="text" onClick={() => setSourcesModal(info)}>
                Show {info.sources.length} Source{info.plural}
              </Button>
            </div>
          );
        },
        title: 'Name',
      },
      {
        dataIndex: 'state',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.state,
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
        title: 'State',
      },
      {
        dataIndex: 'user',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.user,
        key: 'user',
        render: (_: unknown, row: RunRow) => {
          const user = users.find((u) => u.id === row.userId);
          if (user) return userRenderer(user);
          return row.kind === RunKind.GenericTask ? row.task.username : '—';
        },
        responsive: ['md'],
        title: 'Owner',
      },
      scope.type !== 'project' && {
        dataIndex: 'location',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.location,
        ...(scope.type === 'global'
          ? {
              filterDropdown: workspaceFilterDropdown,
              filterIcon: <Icon name="filter" title="Filter by workspace" />,
              filters: workspaces.map((ws) => ({
                text: <WorkspaceFilter workspace={ws} />,
                value: ws.id,
              })),
              isFiltered: (s: unknown) => (s as Settings).workspace !== undefined,
            }
          : {}),
        key: 'location',
        render: (_: unknown, row: RunRow) => (
          <RunLocation row={row} showWorkspace={scope.type === 'global'} workspaces={workspaces} />
        ),
        responsive: ['md'],
        title: scope.type === 'global' ? 'Workspace › Project' : 'Project',
      },
      {
        dataIndex: 'resourcePool',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.resourcePool,
        key: 'resourcePool',
        responsive: ['md'],
        title: 'Resource Pool',
      },
      {
        dataIndex: 'startTime',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.startTime,
        key: 'startTime',
        render: (_: unknown, row: RunRow) => timeRenderer(row.startTime),
        title: 'Started',
      },
      {
        dataIndex: 'endTime',
        defaultWidth: DEFAULT_COLUMN_WIDTHS.endTime,
        key: 'endTime',
        // Notebooks, shells, commands and TensorBoards have no end time in their API.
        render: (_: unknown, row: RunRow) => timeRenderer(row.endTime),
        responsive: ['md'],
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
    return cols.filter((col): col is ColumnDef<RunRow> => !!col);
  }, [entityCopyMap, renderMenu, scope.type, users, workspaceFilterDropdown, workspaces]);

  /* Layout */

  const launchEnabled = workspace ? canCreateWorkspaceNSC({ workspace }) : canCreateNSC;
  const showLaunch = scope.type !== 'project';
  const itemName = experiments ? 'job' : 'task';

  return (
    <div className={css.base} ref={containerRef}>
      <div className={css.toolbar}>
        <div aria-label="Kinds" className={css.kinds} role="group">
          {pageKinds.map((kind) => {
            const count = page?.activeCounts[kind];
            const isSelected = selectedKinds.includes(kind);
            return (
              <Button
                aria-pressed={isSelected}
                data-testid={`kind-${kind}`}
                icon={<Icon decorative name={runKindIcon[kind]} size="small" />}
                key={kind}
                selected={isSelected}
                size="small"
                tooltip={count !== undefined ? `${count} active` : undefined}
                onClick={() => handleKindToggle(kind)}>
                {runKindLabel[kind]}
                {count !== undefined && <span className={css.count}>{count}</span>}
              </Button>
            );
          })}
        </div>
        <div className={css.filters}>
          <Input
            allowClear
            placeholder="Search name or ID"
            prefix={<Icon name="search" size="tiny" title="Search" />}
            value={searchInput}
            width={200}
            onChange={(e) => setSearchInput(e.target.value)}
          />
          <Select
            data-testid="owner"
            searchable={false}
            value={settings.owner}
            width={120}
            onChange={handleOwnerChange}>
            <Option value={Owner.All}>All users</Option>
            <Option value={Owner.Mine}>Mine</Option>
          </Select>
          <Select
            data-testid="state"
            mode="multiple"
            placeholder="All states"
            searchable={false}
            value={settings.state ?? []}
            width={200}
            onChange={handleStateChange}>
            {STATE_GROUPS.map((group) => (
              <Option key={group} value={group}>
                {stateGroupLabel[group]}
              </Option>
            ))}
          </Select>
          <Tooltip content={SLOTS_TOOLTIP}>
            <div>
              <Select
                data-testid="slots"
                searchable={false}
                value={settings.slots ?? ANY_SLOTS}
                width={150}
                onChange={handleSlotsChange}>
                <Option value={ANY_SLOTS}>GPU and CPU</Option>
                <Option value={SlotsFilter.Gpu}>{slotsFilterLabel[SlotsFilter.Gpu]}</Option>
                <Option value={SlotsFilter.CpuOnly}>{slotsFilterLabel[SlotsFilter.CpuOnly]}</Option>
              </Select>
            </div>
          </Tooltip>
          <FilterCounter activeFilterCount={filterCount} onReset={resetFilters} />
          {showLaunch && (
            <>
              <JupyterLabButton enabled={launchEnabled} workspace={workspace} />
              <ShellButton enabled={launchEnabled} workspace={workspace} />
            </>
          )}
        </div>
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
            loading={page === undefined}
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
            scroll={{ x: 'max-content' }}
            settings={settings}
            showSorterTooltip={false}
            size="small"
            updateSettings={updateSettings}
          />
        </GenericTaskActionStateContext.Provider>
      </div>
      {listsCommands && (
        <p className={css.note}>
          Notebooks, shells, commands and TensorBoards are listed for 24 hours after they end, and
          not after the master restarts.
        </p>
      )}
      <BatchActionConfirmModal.Component
        batchAction={ExperimentAction.Kill}
        itemName={itemName}
        onConfirm={handleBatchKill}
      />
      <taskListModal.Component
        sourcesModal={sourcesModal}
        title={`${sourcesModal?.sources.length} TensorBoard Source${sourcesModal?.plural}`}
        onClose={() => setSourcesModal(undefined)}
      />
      {launchAgainModals}
    </div>
  );
};

export default TaskDashboard;
