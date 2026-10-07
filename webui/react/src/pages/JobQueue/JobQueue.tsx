import {
  FilterValue,
  SorterResult,
  TableCurrentDataSource,
  TablePaginationConfig,
} from 'antd/es/table/interface';
import Icon from 'hew/Icon';
import { useModal } from 'hew/Modal';
import { DetError } from 'hew/utils/error';
import { Loadable } from 'hew/utils/loadable';
import _ from 'lodash';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import ActionDropdown, { Triggers } from 'components/ActionDropdown';
import Section from 'components/Section';
import InteractiveTable, { ColumnDef } from 'components/Table/InteractiveTable';
import SkeletonTable from 'components/Table/SkeletonTable';
import {
  checkmarkRenderer,
  createOmitableRenderer,
  defaultRowClassName,
  getFullPaginationConfig,
  userRenderer,
} from 'components/Table/Table';
import TaskActionDropdown from 'components/TaskActionDropdown';
import { V1SchedulerTypeToLabel } from 'constants/states';
import useFeature from 'hooks/useFeature';
import { useLaunchAgain } from 'hooks/useLaunchAgain';
import usePermissions from 'hooks/usePermissions';
import usePolling from 'hooks/usePolling';
import { useSettings } from 'hooks/useSettings';
import useTaskResourcesEnabled from 'hooks/useTaskResourcesEnabled';
import { columns as columnsFunc, SCHEDULING_VAL_KEY } from 'pages/JobQueue/JobQueue.table';
import { paths } from 'routes/utils';
import {
  cancelExperiment,
  getJobQ,
  getTask,
  killExperiment,
  killGenericTask,
  killTask,
} from 'services/api';
import * as Api from 'services/api-ts-sdk';
import userStore from 'stores/users';
import {
  CommandState,
  CommandTask,
  CommandType,
  DetailedUser,
  FullJob,
  Job,
  JobAction,
  JobState,
  JobType,
  ResourcePool,
  TaskItem,
} from 'types';
import handleError, { ErrorLevel, ErrorType } from 'utils/error';
import { canManageJob, jobTypeToCommandType, orderedSchedulers } from 'utils/job';
import { useObservable } from 'utils/observable';
import { routeToReactUrl } from 'utils/routes';
import { numericSorter } from 'utils/sort';
import { capitalize } from 'utils/string';
import { getDisplayName } from 'utils/user';

import css from './JobQueue.module.scss';
import settingsConfig, { Settings } from './JobQueue.settings';
import { ACTIVE_SORTS, isActiveSortKey, QUEUE_ORDER, sortJobs } from './JobQueue.sort';
import ManageJobModalComponent from './ManageJob';

interface Props {
  jobState: JobState;
  rpStats: Api.V1RPQueueStat[];
  selectedRp: ResourcePool;
}

/** The largest limit the API takes, to list all jobs: the master lists 100 for a limit of 0. */
const ALL_JOBS = 2 ** 31 - 1;

/**
 * The task of a shell, JupyterLab, command or TensorBoard job, as the task action menu takes it:
 * the job has the task's ID, name, owner, workspace and pool, and the task's record has its state,
 * the state of its latest allocation (the master lists the open allocation first).
 */
const commandTaskFromJob = (job: FullJob, type: CommandType, task: TaskItem): CommandTask => ({
  id: job.entityId,
  name: job.name,
  resourcePool: job.resourcePool,
  startTime: task.startTime,
  state: task.allocations[0]?.state ?? CommandState.Queued,
  type,
  userId: job.userId ?? 0,
  workspaceId: job.workspaceId,
});

const JobQueue: React.FC<Props> = ({ rpStats, selectedRp, jobState }) => {
  const resourcesEnabled = useTaskResourcesEnabled();
  const { canModifyExperiment, canModifyWorkspaceNSC } = usePermissions();
  const loadableUsers = useObservable(useMemo(() => userStore.getUsers(), []));
  const users = useMemo(() => Loadable.getOrElse([], loadableUsers), [loadableUsers]);
  const [managingJob, setManagingJob] = useState<Job>();
  const [jobs, setJobs] = useState<Job[]>([]);
  // The shells, JupyterLabs, commands and TensorBoards among the jobs on the page, by task ID.
  const [commandTasks, setCommandTasks] = useState<Record<string, CommandTask>>({});
  // The task IDs on the page, and those being looked up: one lookup per task at a time.
  const pageTaskIds = useRef<Set<string>>(new Set());
  const taskLookups = useRef<Set<string>>(new Set());
  const isMounted = useRef(true);
  const [total, setTotal] = useState(0);
  const [canceler] = useState(new AbortController());
  const [pageState, setPageState] = useState<{ isLoading: boolean }>({ isLoading: true });
  const manageJobModal = useModal(ManageJobModalComponent);
  const pageRef = useRef<HTMLElement>(null);
  const f_flat_runs = useFeature().isOn('flat_runs');

  const defaultColumns = useMemo(() => columnsFunc(f_flat_runs), [f_flat_runs]);

  useEffect(() => {
    if (managingJob) {
      manageJobModal.open();
    }
  }, [managingJob, manageJobModal]);
  const { settings, updateSettings } = useSettings<Settings>(
    useMemo(() => settingsConfig(jobState), [jobState]),
  );
  const settingsColumns = useMemo(() => [...settings.columns], [settings.columns]);

  const isJobOrderAvailable = orderedSchedulers.has(selectedRp.schedulerType);

  // The Active tab sorts all its jobs in the browser by a column. Without a column sort both tabs
  // show the queue order, also for a sort key the tab has no column for, such as from a link.
  const hasColumnSorts = jobState === JobState.SCHEDULED;
  const sortedInBrowser = hasColumnSorts && isActiveSortKey(settings.sortKey);
  const knownSortKey = sortedInBrowser || settings.sortKey === QUEUE_ORDER.sortKey;
  const sortKey = knownSortKey ? settings.sortKey : QUEUE_ORDER.sortKey;
  const sortDesc = knownSortKey ? settings.sortDesc : QUEUE_ORDER.sortDesc;
  // A sort in the browser fetches all jobs and pages them here, so a page click fetches nothing.
  const fetchLimit = sortedInBrowser ? ALL_JOBS : settings.tableLimit;
  const fetchOffset = sortedInBrowser ? 0 : settings.tableOffset;
  // The page the table shows, for the check that a sorted list still reaches it.
  const tableOffset = useRef(settings.tableOffset);
  useEffect(() => {
    tableOffset.current = settings.tableOffset;
  }, [settings.tableOffset]);
  // The latest fetch of the jobs: a reply to an earlier one, for another sort or page, is stale.
  const latestFetch = useRef(0);

  useEffect(() => {
    isMounted.current = true;
    return () => {
      isMounted.current = false;
    };
  }, []);

  /**
   * The task action menu of a shell, JupyterLab, command or TensorBoard needs the task's state,
   * which its job does not have. Each such job on the page is looked up by its task ID on every
   * poll (unless its last lookup is still running), on its own: a failed lookup leaves the other
   * rows alone, and the jobs table does not wait for the lookups. A job whose task has not been
   * loaded yet, or whose lookup failed, keeps the job menu; a failed refresh keeps the copy
   * loaded before.
   *
   * The lookup reads the task's record (GET /api/v1/tasks/{id}), which has no credentials. The
   * shell and notebook APIs return the shell's private key and the notebook's token to its owner
   * or an admin, and log an admin's read, so they are not polled for a menu.
   */
  const refreshCommandTasks = useCallback((jobs: Job[]) => {
    const lookups = jobs.flatMap((job) => {
      if (!('entityId' in job) || !job.entityId) return [];
      const type = jobTypeToCommandType(job.type);
      return type ? [{ job, type }] : [];
    });
    const ids = new Set(lookups.map(({ job }) => job.entityId));
    pageTaskIds.current = ids;
    // Forget the tasks of jobs that left the page.
    setCommandTasks((prev) => {
      const kept = _.pickBy(prev, (_task, id) => ids.has(id));
      return _.size(kept) === _.size(prev) ? prev : kept;
    });
    lookups.forEach(({ job, type }) => {
      const id = job.entityId;
      if (taskLookups.current.has(id)) return;
      taskLookups.current.add(id);
      getTask({ taskId: id })
        .then((item) => {
          if (!item || !isMounted.current || !pageTaskIds.current.has(id)) return;
          const task = commandTaskFromJob(job, type, item);
          setCommandTasks((prev) => (_.isEqual(prev[id], task) ? prev : { ...prev, [id]: task }));
        })
        .catch((e) => {
          handleError(e, {
            publicSubject: 'Unable to fetch task.',
            silent: true,
            type: ErrorType.Api,
          });
        })
        .finally(() => taskLookups.current.delete(id));
    });
  }, []);

  const fetchJobsTable = useCallback(async () => {
    const fetchId = ++latestFetch.current;
    try {
      const orderBy = !sortedInBrowser && sortDesc ? 'ORDER_BY_DESC' : 'ORDER_BY_ASC';
      const jobs = await getJobQ(
        {
          limit: fetchLimit,
          offset: fetchOffset,
          orderBy,
          resourcePool: selectedRp.name,
          states: jobState ? [jobState] : undefined,
        },
        { signal: canceler.signal },
      );
      if (fetchId !== latestFetch.current) return;

      const newJobs = jobState ? jobs.jobs.filter((j) => j.summary.state === jobState) : jobs.jobs;
      setJobs(newJobs);
      if (sortedInBrowser) {
        setTotal(newJobs.length);
        if (tableOffset.current > 0 && tableOffset.current >= newJobs.length) {
          updateSettings({ tableOffset: 0 });
        }
      } else if (jobs.pagination.total !== undefined) {
        setTotal(jobs.pagination.total);
      }
    } catch (e) {
      if (fetchId !== latestFetch.current) return;
      if ((e as DetError)?.publicMessage === 'offset out of bounds' && fetchOffset !== 0) {
        updateSettings({ tableOffset: 0 });
        return;
      }
      handleError(e, {
        level: ErrorLevel.Error,
        publicSubject: 'Unable to fetch job queue and stats.',
        silent: false,
        type: ErrorType.Server,
      });
    } finally {
      if (fetchId === latestFetch.current) setPageState((cur) => ({ ...cur, isLoading: false }));
    }
  }, [
    canceler.signal,
    fetchLimit,
    fetchOffset,
    selectedRp.name,
    jobState,
    updateSettings,
    sortedInBrowser,
    sortDesc,
  ]);

  usePolling(fetchJobsTable, { rerunOnNewFn: true });

  // The jobs on the page. A column sort orders all jobs of the tab; jobs with equal values keep
  // their queue order. Users sort by the name their avatar shows.
  const pageJobs = useMemo(() => {
    if (!sortedInBrowser) return jobs;
    const ownerName = (job: Job) => {
      if (!('username' in job)) return undefined;
      const user = users.find((u) => u.id === job.userId);
      return user ? getDisplayName(user) : job.username;
    };
    const { tableLimit, tableOffset } = settings;
    return sortJobs(jobs, sortKey, sortDesc, ownerName).slice(
      tableOffset,
      tableOffset + tableLimit,
    );
  }, [jobs, settings, sortDesc, sortKey, sortedInBrowser, users]);

  useEffect(() => refreshCommandTasks(pageJobs), [pageJobs, refreshCommandTasks]);

  /**
   * In place of InteractiveTable's own change handler, which the props it passes on to the table
   * override: a third click on a sorted column goes back to the queue order, and a new sort starts
   * on the first page.
   */
  const handleTableChange = useCallback(
    (
      pagination: TablePaginationConfig,
      _filters: Record<string, FilterValue | null>,
      sorter: SorterResult<Job> | SorterResult<Job>[],
      { action }: TableCurrentDataSource<Job>,
    ) => {
      const tableLimit = pagination.pageSize ?? settings.tableLimit;
      const updates: Partial<Settings> = {
        tableLimit,
        tableOffset: ((pagination.current ?? 1) - 1) * tableLimit,
      };
      if (action === 'sort' && !Array.isArray(sorter)) {
        const key = String(sorter.columnKey);
        const sortable = key === QUEUE_ORDER.sortKey || (hasColumnSorts && isActiveSortKey(key));
        const sort =
          sorter.order && sortable
            ? { sortDesc: sorter.order === 'descend', sortKey: key }
            : QUEUE_ORDER;
        if (sort.sortKey !== settings.sortKey || sort.sortDesc !== settings.sortDesc) {
          Object.assign(updates, sort, { tableOffset: 0 });
        }
      }
      updateSettings(updates);
    },
    [hasColumnSorts, settings, updateSettings],
  );

  const { launchAgain, launchAgainModals } = useLaunchAgain({ onLaunched: fetchJobsTable });

  const canControlJob = useCallback(
    (job: FullJob) =>
      job.type === JobType.EXPERIMENT
        ? canModifyExperiment({ userId: job.userId, workspace: { id: job.workspaceId } })
        : canModifyWorkspaceNSC({ userId: job.userId, workspace: { id: job.workspaceId } }),
    [canModifyExperiment, canModifyWorkspaceNSC],
  );

  // Whether to offer Manage Job, in the job menu and the task menu alike.
  const canManage = useCallback(
    (job: FullJob) => canControlJob(job) && canManageJob(job, selectedRp),
    [canControlJob, selectedRp],
  );

  const dropDownOnTrigger = useCallback(
    (job: Job) => {
      if (!('entityId' in job) || !job.entityId) return {};
      const triggers: Triggers<JobAction> = {};
      const commandType = jobTypeToCommandType(job.type);
      const canControl = canControlJob(job);

      if (commandType) {
        if (canControl) {
          triggers[JobAction.Kill] = () => killTask({ id: job.entityId, type: commandType });
        }
        triggers[JobAction.ViewLog] = () => {
          routeToReactUrl(paths.taskLogs({ id: job.entityId, name: job.name, type: commandType }));
        };
      }

      if (job.type === JobType.GENERIC) {
        if (canControl) {
          triggers[JobAction.Kill] = () => killGenericTask({ taskId: job.entityId });
        }
        triggers[JobAction.ViewLog] = () => {
          routeToReactUrl(paths.genericTaskDetails(job.entityId, 'logs'));
        };
      }

      // if job is an experiment type add action to kill it
      if (job.type === JobType.EXPERIMENT && canControl) {
        triggers[JobAction.Cancel] = async () => {
          await cancelExperiment({ experimentId: parseInt(job.entityId, 10) });
        };
        triggers[JobAction.Kill] = async () => {
          await killExperiment({ experimentId: parseInt(job.entityId, 10) });
        };
      }

      if (resourcesEnabled) {
        if (job.type === JobType.EXPERIMENT) {
          triggers[JobAction.ViewResources] = () =>
            routeToReactUrl(paths.experimentResources(job.entityId));
        } else if (commandType || job.type === JobType.GENERIC) {
          triggers[JobAction.ViewResources] = () =>
            routeToReactUrl(paths.taskResources(job.entityId));
        }
      }

      if (canManage(job)) {
        triggers[JobAction.ManageJob] = () => setManagingJob(job);
      }

      Object.keys(triggers).forEach((key) => {
        const action = key as JobAction;
        const fn = triggers[action];
        if (!fn) return;
        triggers[action] = async () => {
          await fn();
          await fetchJobsTable();
        };
      });
      return triggers;
    },
    [fetchJobsTable, resourcesEnabled, canControlJob, canManage],
  );

  const onModalClose = useCallback(() => {
    setManagingJob(undefined);
    fetchJobsTable();
  }, [fetchJobsTable]);

  useEffect(() => {
    if (!managingJob) return;
    const job = jobs.find((j) => j.jobId === managingJob.jobId);
    if (!job) {
      setManagingJob(undefined);
    } else if (!_.isEqual(job, managingJob)) {
      setManagingJob(job);
    }
  }, [jobs, managingJob]);

  useEffect(() => {
    const col = defaultColumns.find(({ key }) => key === SCHEDULING_VAL_KEY);
    if (col) {
      const replaceIndex = settingsColumns.findIndex((column) =>
        ['priority', 'weight', 'resourcePool'].includes(column),
      );
      const newColumns = [...settingsColumns];
      if (replaceIndex !== -1) newColumns[replaceIndex] = col.dataIndex;
      if (!_.isEqual(newColumns, settings.columns)) updateSettings({ columns: newColumns });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [settings.columns, settingsColumns]);

  const columns = useMemo(() => {
    return defaultColumns
      .map<ColumnDef<Job>>((col) => {
        switch (col.key) {
          case 'actions':
            return {
              ...col,
              render: createOmitableRenderer<Job, FullJob>(
                'entityId',
                (_, record) => {
                  // Shells, JupyterLabs, commands and TensorBoards get the Tasks page's menu.
                  const task = jobTypeToCommandType(record.type)
                    ? commandTasks[record.entityId]
                    : undefined;
                  if (task) {
                    return (
                      <div>
                        <TaskActionDropdown
                          task={task}
                          onComplete={fetchJobsTable}
                          onLaunchAgain={launchAgain}
                          onManageJob={canManage(record) ? () => setManagingJob(record) : undefined}
                        />
                      </div>
                    );
                  }
                  // The same order as the task menu: Manage Job after the viewing actions, and
                  // Cancel and Kill last.
                  return (
                    <div>
                      <ActionDropdown<JobAction>
                        actionOrder={[
                          JobAction.ViewLog,
                          JobAction.ViewResources,
                          JobAction.ManageJob,
                          JobAction.Cancel,
                          JobAction.Kill,
                        ]}
                        confirmations={{
                          [JobAction.Cancel]: { cancelText: 'Abort', onError: handleError },
                          [JobAction.Kill]: { danger: true, onError: handleError },
                        }}
                        danger={{ [JobAction.Kill]: true }}
                        id={record.name}
                        kind="job"
                        onError={handleError}
                        onTrigger={dropDownOnTrigger(record)}
                      />
                    </div>
                  );
                },
                null,
              ),
            };
          case SCHEDULING_VAL_KEY: {
            if (!settingsColumns) return col;

            switch (selectedRp.schedulerType) {
              case Api.V1SchedulerType.SLURM:
                return {
                  ...col,
                  dataIndex: 'resourcePool',
                  title: 'Partition',
                };
              case Api.V1SchedulerType.PBS:
                return {
                  ...col,
                  dataIndex: 'resourcePool',
                  title: 'Queue',
                };
              case Api.V1SchedulerType.PRIORITY:
              case Api.V1SchedulerType.KUBERNETES:
                return {
                  ...col,
                  dataIndex: 'priority',
                  title: 'Priority',
                };
              case Api.V1SchedulerType.FAIRSHARE:
                return {
                  ...col,
                  align: 'right',
                  dataIndex: 'weight',
                  title: 'Weight',
                };
              case Api.V1SchedulerType.UNSPECIFIED:
              case Api.V1SchedulerType.ROUNDROBIN:
                return col;
            }
          }
          case 'jobsAhead':
            if (!isJobOrderAvailable) {
              return {
                ...col,
                render: (_, record) => (
                  <div className={`${css.centerVertically} ${css.centerHorizontally}`}>
                    {checkmarkRenderer(record.isPreemptible)}
                  </div>
                ),
                title: 'Preemptible',
              };
            } else {
              return {
                ...col,
                render: (_: unknown, record) => (
                  <div className={css.centerVertically}>
                    {record.summary.jobsAhead}
                    {!record.isPreemptible && <Icon name="lock" title="Not Preemptible" />}
                  </div>
                ),
                sorter: (a, b) => numericSorter(a.summary.jobsAhead, b.summary.jobsAhead),
                title: '#',
              };
            }
          case 'user':
            return {
              ...col,
              render: createOmitableRenderer<Job, FullJob>('entityId', (_, r) => {
                let user = users.find((u) => u.id === r.userId);
                if (!user) {
                  // This is an external user. Create a new DetailedUser instance.
                  const externalUser: DetailedUser = {
                    // external users do not have a user id. Indicate that with a value of -1.
                    id: -1,
                    isActive: true,
                    isAdmin: false,
                    username: r.username,
                  };
                  user = externalUser;
                }
                return userRenderer(user);
              }),
            };
          default:
            return col;
        }
      })
      .map<ColumnDef<Job>>((column) => {
        const key = String(column.key);
        return {
          ...column,
          ...(hasColumnSorts && isActiveSortKey(key)
            ? { sortDirections: ACTIVE_SORTS[key].directions, sorter: true }
            : {}),
          sortOrder: key === sortKey ? (sortDesc ? 'descend' : 'ascend') : null,
        };
      });
  }, [
    defaultColumns,
    isJobOrderAvailable,
    dropDownOnTrigger,
    settingsColumns,
    hasColumnSorts,
    sortKey,
    sortDesc,
    selectedRp.schedulerType,
    canManage,
    commandTasks,
    fetchJobsTable,
    launchAgain,
    users,
  ]);

  // table title using selectedRp and schedulerType from list of resource pools
  const tableTitle = useMemo(() => {
    if (!selectedRp) return '';
    const schedulerType = V1SchedulerTypeToLabel[selectedRp.schedulerType];
    return (
      <div>
        {`${capitalize(selectedRp.name)} (${schedulerType.toLowerCase()}) `}
        <Icon name="info" title={`Job Queue for resource pool "${selectedRp.name}"`} />
      </div>
    );
  }, [selectedRp]);

  return (
    <div className={css.base} id="jobs">
      <Section hideTitle={!!selectedRp} title={tableTitle}>
        {settings ? (
          <InteractiveTable<Job, Settings>
            columns={columns}
            containerRef={pageRef}
            dataSource={pageJobs}
            loading={pageState.isLoading}
            pagination={getFullPaginationConfig(
              {
                limit: settings.tableLimit,
                offset: settings.tableOffset,
              },
              total,
            )}
            rowClassName={defaultRowClassName({ clickable: false })}
            rowKey="jobId"
            scroll={{ x: 1000 }}
            settings={settings}
            showSorterTooltip={false}
            size="small"
            updateSettings={updateSettings}
            onChange={handleTableChange}
          />
        ) : (
          <SkeletonTable columns={columns.length} />
        )}
      </Section>
      {!!managingJob && (
        <manageJobModal.Component
          initialPool={selectedRp.name}
          job={managingJob}
          rpStats={rpStats}
          schedulerType={selectedRp.schedulerType}
          onFinish={onModalClose}
        />
      )}
      {launchAgainModals}
    </div>
  );
};

export default JobQueue;
