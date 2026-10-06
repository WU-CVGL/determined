import dayjs from 'dayjs';
import Alert from 'hew/Alert';
import Button from 'hew/Button';
import DatePicker from 'hew/DatePicker';
import { SyncProvider } from 'hew/LineChart/SyncProvider';
import Select from 'hew/Select';
import Spinner from 'hew/Spinner';
import React, { useEffect, useMemo, useState } from 'react';

import TaskResourceChart from 'components/TaskResourceChart';
import useTaskResourcesEnabled from 'hooks/useTaskResourcesEnabled';
import { serverAddress } from 'routes/utils';
import {
  parseResourceAllocations,
  RESOURCE_MAX_SPAN,
  RESOURCE_METRICS,
  ResourceAllocation,
  ResourceRange,
  resourceRange,
  sinceStartBounds,
  sinceStartWindow,
  TaskResourcesResponse,
} from 'utils/taskResources';

import css from './TaskResourcesPanel.module.scss';

interface Props {
  taskId: string;
  startTime: string;
  endTime?: string;
  initialAllocationId?: string;
}

// From when the container got its resources; Custom range is 0 and the others count seconds.
const SINCE_START = -1;

const PERIODS = [
  { label: 'Since start', value: SINCE_START },
  { label: 'Last 15 minutes', value: 900 },
  { label: 'Last hour', value: 3600 },
  { label: 'Last 6 hours', value: 21600 },
  { label: 'Last 24 hours', value: 86400 },
  { label: 'Last 7 days', value: RESOURCE_MAX_SPAN },
  { label: 'Custom range', value: 0 },
];

const responseError = (status: number): string => {
  if (status === 401 || status === 403)
    return 'You do not have permission to view this task’s resources.';
  if (status === 404) return 'This task or its resource monitoring is unavailable.';
  if (status === 429 || status === 503) return 'Resource monitoring is busy. Please retry shortly.';
  return 'Resource metrics could not be loaded. Please retry.';
};

const TaskResourcesPanel: React.FC<Props> = ({
  taskId,
  startTime,
  endTime,
  initialAllocationId,
}) => {
  const enabled = useTaskResourcesEnabled();
  const [period, setPeriod] = useState(SINCE_START);
  const [allocation, setAllocation] = useState(initialAllocationId || '');
  const [refresh, setRefresh] = useState(Date.now);
  const [customStart, setCustomStart] = useState(() => dayjs().subtract(1, 'hour'));
  const [customEnd, setCustomEnd] = useState(() => dayjs());
  const [appliedCustom, setAppliedCustom] = useState<ResourceRange>();
  const [payload, setPayload] = useState<{
    data: TaskResourcesResponse;
    range: ResourceRange;
    updated: Date;
  }>();
  const [error, setError] = useState<string>();
  const [loading, setLoading] = useState(false);
  // An unavailable list (null) falls back to the task start.
  const [allocationList, setAllocationList] = useState<{
    taskId: string;
    items: ResourceAllocation[] | null;
  }>();
  const customValid =
    customEnd.isAfter(customStart) &&
    customEnd.diff(customStart, 'second') <= RESOURCE_MAX_SPAN &&
    customEnd.valueOf() <= Date.now();

  const taskStart = Math.floor(Date.parse(startTime) / 1000);
  const allocations = allocationList?.taskId === taskId ? allocationList.items : undefined;
  const bounds =
    allocations === undefined ? undefined : sinceStartBounds(allocations, allocation, taskStart);
  // Primitive bounds keep the range stable when a refreshed list has not changed.
  const boundStart = bounds?.start;
  const boundEnd = bounds?.end;

  const timeWindow = useMemo(() => {
    if (!period) return appliedCustom && { clamped: false, range: appliedCustom };
    const end = Math.floor(Math.min(endTime ? Date.parse(endTime) : refresh, refresh) / 1000);
    if (period === SINCE_START) {
      if (boundStart === undefined) return undefined;
      const since = sinceStartWindow({ end: boundEnd, start: boundStart }, end);
      if (!Number.isFinite(since.start) || !Number.isFinite(since.end)) return undefined;
      return { clamped: since.clamped, range: resourceRange(since.start, since.end) };
    }
    const start = Math.max(taskStart, end - period);
    if (!Number.isFinite(start) || !Number.isFinite(end)) return undefined;
    return { clamped: false, range: resourceRange(Math.min(start, end - 1), end) };
  }, [appliedCustom, boundEnd, boundStart, endTime, period, refresh, taskStart]);
  const range = timeWindow?.range;
  const listPending = period === SINCE_START && allocations === undefined;

  useEffect(() => {
    setAllocation(initialAllocationId || '');
  }, [taskId, initialAllocationId]);

  useEffect(() => setPayload(undefined), [taskId]);

  // Every refresh, manual or the 30-second one of a running task, reads the list again: a queued
  // allocation gets its container start later, and an earlier read may have failed.
  useEffect(() => {
    if (!enabled || !taskId) return;
    const controller = new AbortController();
    fetch(serverAddress(`/ui/task-resources/${encodeURIComponent(taskId)}/allocations`), {
      credentials: 'include',
      signal: controller.signal,
    })
      .then(async (response) =>
        response.ok ? parseResourceAllocations(await response.json()) : undefined,
      )
      .catch(() => undefined)
      .then((items) => {
        if (controller.signal.aborted) return;
        // A failed refresh keeps the last list; without one the range begins at the task start.
        setAllocationList((previous) =>
          items
            ? { items, taskId }
            : previous?.taskId === taskId && previous.items
              ? previous
              : { items: null, taskId },
        );
      });
    return () => controller.abort();
  }, [enabled, refresh, taskId]);

  useEffect(() => {
    if (!enabled || !range || !taskId) return;
    const controller = new AbortController();
    setLoading(true);
    setError(undefined);
    const params = new URLSearchParams({
      end: String(range.end),
      start: String(range.start),
      step: String(range.step),
    });
    fetch(serverAddress(`/ui/task-resources/${encodeURIComponent(taskId)}?${params}`), {
      credentials: 'include',
      signal: controller.signal,
    })
      .then(async (response) => {
        if (!response.ok) throw new Error(responseError(response.status));
        const data: TaskResourcesResponse = await response.json();
        if (!Array.isArray(data.series) || !Array.isArray(data.warnings))
          throw new Error('Unexpected resource monitoring response.');
        if (!controller.signal.aborted) setPayload({ data, range, updated: new Date() });
      })
      .catch((e: Error) => {
        if (!controller.signal.aborted)
          setError(e.message || 'Resource metrics could not be loaded.');
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [enabled, range, refresh, taskId]);

  useEffect(() => {
    if (!enabled || endTime || !period) return;
    const timer = window.setInterval(() => {
      if (!document.hidden) setRefresh(Date.now());
    }, 30000);
    return () => window.clearInterval(timer);
  }, [enabled, endTime, period]);

  const allocationOptions = useMemo(() => {
    const ids = new Set(
      payload?.data.series
        .map((item) => item.labels.allocation_id)
        .filter((id): id is string => !!id),
    );
    allocations?.forEach((item) => ids.add(item.allocationId));
    if (allocation) ids.add(allocation);
    return [
      { label: 'All allocations', value: '' },
      ...Array.from(ids)
        .sort()
        .map((id) => ({ label: id, value: id })),
    ];
  }, [allocation, allocations, payload]);

  if (enabled === undefined) return <Spinner center spinning />;
  if (!enabled)
    return (
      <Alert message="Native resource monitoring is not enabled for this cluster." type="info" />
    );

  return (
    <div className={css.base}>
      <div className={css.filters}>
        <Select
          label={endTime && period !== SINCE_START ? 'Window before task end' : 'Time range'}
          options={PERIODS}
          value={period}
          width={190}
          onChange={(value) => {
            setPeriod(Number(value));
            setPayload(undefined);
          }}
        />
        <Select
          label="Allocation"
          options={allocationOptions}
          searchable
          value={allocation}
          width={320}
          onChange={(value) => setAllocation(String(value))}
        />
        <Button disabled={loading || !range} onClick={() => setRefresh(Date.now())}>
          Refresh
        </Button>
        <span className={css.status}>
          {loading || listPending
            ? 'Loading…'
            : payload
              ? `Updated ${payload.updated.toLocaleTimeString()}`
              : ''}
          {!endTime && period ? ' · live, every 30s' : ''}
        </span>
      </div>
      {!period && (
        <div className={css.filters}>
          <DatePicker
            allowClear={false}
            label="From"
            showTime
            value={customStart}
            onChange={(value) => value && setCustomStart(value)}
          />
          <DatePicker
            allowClear={false}
            label="To"
            showTime
            value={customEnd}
            onChange={(value) => value && setCustomEnd(value)}
          />
          <Button
            disabled={!customValid}
            onClick={() => {
              setPayload(undefined);
              setAppliedCustom(resourceRange(customStart.unix(), customEnd.unix()));
            }}>
            Apply range
          </Button>
          {!customValid && <span>Choose a past time range of up to 7 days.</span>}
        </div>
      )}
      {period === SINCE_START && timeWindow?.clamped && (
        <p className={css.explanation}>
          The time since start is longer than the 7-day query limit, so the most recent 7 days are
          shown.
        </p>
      )}
      {period === SINCE_START && bounds?.noContainerStart && (
        <p className={css.explanation}>
          No container start is recorded{endTime ? '' : ' yet'}, so the range begins when the task
          was submitted.
        </p>
      )}
      <p className={css.explanation}>
        Each allocation is shown separately. Gaps mean no confirmed samples, not zero use. GPU
        readings describe the entire assigned device and may include other processes. Child tasks
        are not included.
      </p>
      {error && (
        <Alert
          description={
            payload ? 'The charts below retain the last successful response.' : undefined
          }
          message={error}
          type="error"
        />
      )}
      {payload?.data.warnings.map((warning) => (
        <Alert key={warning.code} message={warning.message} type="warning" />
      ))}
      {(loading || listPending) && !payload && <Spinner center spinning />}
      {payload && (
        <SyncProvider key={`${payload.range.start}:${payload.range.end}`}>
          <div className={css.grid}>
            {RESOURCE_METRICS.map((metric) => (
              <TaskResourceChart
                key={metric.key}
                metric={metric}
                range={payload.range}
                series={payload.data.series.filter(
                  (item) =>
                    item.metric === metric.key &&
                    (!allocation || item.labels.allocation_id === allocation),
                )}
              />
            ))}
          </div>
        </SyncProvider>
      )}
    </div>
  );
};

export default TaskResourcesPanel;
