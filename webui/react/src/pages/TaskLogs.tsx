import LogViewer, { FetchConfig, FetchDirection, FetchType } from 'hew/LogViewer/LogViewer';
import LogViewerSelect, { Filters } from 'hew/LogViewer/LogViewerSelect';
import { Settings, settingsConfigForTask } from 'hew/LogViewer/LogViewerSelect.settings';
import Message from 'hew/Message';
import Spinner from 'hew/Spinner';
import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { useParams } from 'react-router-dom';

import Page from 'components/Page';
import TaskResourcesLink from 'components/TaskResourcesLink';
import { commandTypeToLabel } from 'constants/states';
import usePageReturns from 'hooks/usePageReturns';
import useRefusal from 'hooks/useRefusal';
import { ResetSettings, UpdateSettings, useSettings } from 'hooks/useSettings';
import { DateString, decode, optional } from 'ioTypes';
import { paths, serverAddress } from 'routes/utils';
import { getTask } from 'services/api';
import { detApi } from 'services/apiConfig';
import { mapV1LogsResponse } from 'services/decoder';
import { readStream } from 'services/utils';
import { CommandType, JobCapabilities, TaskItem } from 'types';
import handleError from 'utils/error';
import { isLogsNotShared, LOGS_NOT_SHARED, logsHidden } from 'utils/jobPrivacy';
import { lookupLegacyCapabilities } from 'utils/taskLookup';

import css from './TaskLogs.module.scss';

type Params = {
  taskId: string;
  taskType: string;
};

interface Props {
  headerComponent?: React.ReactNode;
  onCloseLogs?: () => void;
  taskId: string;
  taskType: string;
}
type OrderBy = 'ORDER_BY_UNSPECIFIED' | 'ORDER_BY_ASC' | 'ORDER_BY_DESC';

export const TaskLogsWrapper: React.FC = () => {
  const { taskId, taskType } = useParams<Params>();
  return <TaskLogs taskId={taskId ?? ''} taskType={taskType ?? ''} />;
};
interface ViewerProps {
  /** The task's logs are not shared with the viewer, as its capabilities say. */
  logsHidden?: boolean;
  onCloseLogs?: () => void;
  resetSettings: ResetSettings;
  settings: Settings;
  taskId: string;
  updateSettings: UpdateSettings<Settings>;
}

/*
 * The log viewer of a task with its filters, without a page around it. The caller owns the log
 * settings so that it can also read them, e.g. for the selected allocation. Logs that are not
 * shared with the viewer, by the task's capabilities or by the master's refusal, are not read
 * (see useRefusal for when a refusal ends).
 */
export const TaskLogsViewer: React.FC<ViewerProps> = ({
  logsHidden,
  onCloseLogs,
  resetSettings,
  settings,
  taskId,
  updateSettings,
}: ViewerProps) => {
  const [filterOptions, setFilterOptions] = useState<Filters>({});
  const { notShared, refuse } = useRefusal(taskId, !!logsHidden);

  const handleLogsError = useCallback(
    (e: unknown) => {
      if (isLogsNotShared(e)) {
        refuse();
        return;
      }
      handleError(e);
    },
    [refuse],
  );

  const filterValues: Filters = useMemo(
    () => ({
      agentIds: settings.agentId,
      containerIds: settings.containerId,
      levels: settings.level,
      rankIds: settings.rankId,
      searchText: settings.searchText,
    }),
    [settings],
  );

  const handleFilterChange = useCallback(
    (filters: Filters) => {
      updateSettings({
        agentId: filters.agentIds,
        allocationId: filters.allocationIds,
        containerId: filters.containerIds,
        level: filters.levels,
        rankId: filters.rankIds,
        searchText: filters.searchText,
      });
    },
    [updateSettings],
  );

  const handleFilterReset = useCallback(() => resetSettings(), [resetSettings]);

  const handleFetch = useCallback(
    (config: FetchConfig, type: FetchType) => {
      const options = {
        follow: false,
        limit: config.limit,
        orderBy: 'ORDER_BY_UNSPECIFIED',
        timestampAfter: undefined as Date | string | undefined,
        timestampBefore: undefined as Date | string | undefined,
      };

      if (type === FetchType.Initial) {
        options.orderBy =
          config.fetchDirection === FetchDirection.Older ? 'ORDER_BY_DESC' : 'ORDER_BY_ASC';
      } else if (type === FetchType.Newer) {
        options.orderBy = 'ORDER_BY_ASC';
        if (config.offsetLog?.time) options.timestampAfter = config.offsetLog.time;
      } else if (type === FetchType.Older) {
        options.orderBy = 'ORDER_BY_DESC';
        if (config.offsetLog?.time) options.timestampBefore = config.offsetLog.time;
      } else if (type === FetchType.Stream) {
        options.follow = true;
        options.limit = 0;
        options.orderBy = 'ORDER_BY_ASC';
        options.timestampAfter = new Date().toISOString();
      }

      return detApi.StreamingJobs.taskLogs(
        taskId,
        options.limit,
        options.follow,
        settings.allocationId,
        settings.agentId,
        settings.containerId,
        settings.rankId,
        settings.level,
        undefined,
        undefined,
        decode(optional(DateString), options.timestampBefore),
        decode(optional(DateString), options.timestampAfter),
        options.orderBy as OrderBy,
        settings.searchText,
        false,
        { signal: config.canceler.signal },
      );
    },
    [settings, taskId],
  );

  useEffect(() => {
    if (notShared) return;
    const canceler = new AbortController();

    readStream(
      detApi.StreamingJobs.taskLogsFields(taskId, true, { signal: canceler.signal }),
      (event) => setFilterOptions(event as Filters),
      (e) => {
        if (isLogsNotShared(e)) refuse();
      },
    );

    return () => canceler.abort();
  }, [notShared, refuse, taskId]);

  if (notShared) return <Message title={LOGS_NOT_SHARED} />;

  const logFilters = (
    <div className={css.filters}>
      <LogViewerSelect
        options={filterOptions}
        showSearch={true}
        values={filterValues}
        onChange={handleFilterChange}
        onReset={handleFilterReset}
      />
    </div>
  );

  return (
    <LogViewer
      decoder={mapV1LogsResponse}
      handleCloseLogs={onCloseLogs}
      serverAddress={serverAddress}
      title={logFilters}
      onError={handleLogsError}
      onFetch={handleFetch}
    />
  );
};

const TaskLogs: React.FC<Props> = ({ taskId, taskType, onCloseLogs, headerComponent }: Props) => {
  const [task, setTask] = useState<TaskItem>();
  // The task's current capabilities, once a read of the task has answered: from the task, or from
  // the record of its type for an older master. Without any, the logs are read and the master
  // refuses them if they are not shared.
  const [access, setAccess] = useState<{ capabilities?: JobCapabilities; taskId: string }>();

  const taskTypeLabel =
    taskType === 'generic' ? 'Generic Task' : commandTypeToLabel[taskType as CommandType];
  const title = `${taskTypeLabel} ${taskId.substring(0, 8)} Logs`;

  const taskSettingsConfig = useMemo(() => settingsConfigForTask(taskId), [taskId]);
  const { resetSettings, settings, updateSettings } = useSettings<Settings>(taskSettingsConfig);
  const selectedAllocation =
    settings.allocationId?.length === 1 ? settings.allocationId[0] : undefined;

  // The task is read again when the user comes back to the page, for sharing changed meanwhile.
  const pageReturns = usePageReturns();

  useEffect(() => {
    let active = true;
    // A read that fails keeps the capabilities of the last one for this task.
    const keepLast = () => {
      if (active) setAccess((last) => (last?.taskId === taskId ? last : { taskId }));
    };
    const read = async () => {
      let value: TaskItem | undefined;
      try {
        value = await getTask({ taskId });
      } catch {
        return keepLast();
      }
      if (!active) return;
      if (!value) return keepLast();
      setTask(value);
      if (value.capabilities) {
        setAccess({ capabilities: value.capabilities, taskId });
        return;
      }
      try {
        const capabilities = await lookupLegacyCapabilities(taskId, taskType);
        if (active) setAccess({ capabilities, taskId });
      } catch {
        keepLast();
      }
    };
    void read();
    return () => {
      active = false;
    };
  }, [pageReturns, taskId, taskType]);

  return (
    <Page
      bodyNoPadding
      breadcrumb={[
        { breadcrumbName: 'Tasks', path: paths.taskList() },
        {
          breadcrumbName: `${taskTypeLabel} ${taskId.substring(0, 8)}`,
          path:
            taskType === 'generic'
              ? paths.genericTaskDetails(taskId)
              : paths.taskLogs({ id: taskId, type: taskType as CommandType }),
        },
      ]}
      headerComponent={headerComponent}
      id="task-logs"
      options={
        task?.taskId === taskId && (
          <TaskResourcesLink
            nativeUrl={paths.taskResources(task.taskId, selectedAllocation)}
            target={{
              allocationId: selectedAllocation,
              endTime: task.endTime,
              startTime: task.startTime,
              taskId: task.taskId,
            }}
          />
        )
      }
      title={title}>
      {access?.taskId === taskId ? (
        <TaskLogsViewer
          logsHidden={logsHidden(access.capabilities)}
          resetSettings={resetSettings}
          settings={settings}
          taskId={taskId}
          updateSettings={updateSettings}
          onCloseLogs={onCloseLogs}
        />
      ) : (
        <Spinner center spinning />
      )}
    </Page>
  );
};

export default TaskLogs;
