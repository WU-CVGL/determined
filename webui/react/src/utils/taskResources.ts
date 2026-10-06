import { AlignedData } from 'uplot';

export const RESOURCE_METRICS = [
  { key: 'cpu_cores', title: 'CPU', unit: 'cores' },
  { key: 'memory_working_set_bytes', title: 'Memory working set', unit: 'bytes' },
  { key: 'memory_rss_bytes', title: 'Memory RSS', unit: 'bytes' },
  { key: 'gpu_utilization_percent', title: 'Assigned GPU utilization', unit: '%' },
  { key: 'gpu_memory_used_bytes', title: 'Assigned GPU memory', unit: 'bytes' },
  { key: 'gpu_power_watts', title: 'Assigned GPU power', unit: 'W' },
  { key: 'gpu_temperature_celsius', title: 'Assigned GPU temperature', unit: '°C' },
  { key: 'allocation_active', title: 'Allocation lifecycle', unit: 'active' },
] as const;

export type ResourceMetric = (typeof RESOURCE_METRICS)[number];
export interface ResourceSeries {
  metric: ResourceMetric['key'];
  labels: { allocation_id?: string; node?: string; gpu_uuid?: string };
  samples: [number, number | null][];
}
export interface TaskResourcesResponse {
  enabled: boolean;
  series: ResourceSeries[];
  warnings: { code: string; message: string }[];
}
export interface ResourceRange {
  start: number;
  end: number;
  step: number;
}

// The longest range the master answers in one query, in seconds.
export const RESOURCE_MAX_SPAN = 7 * 86400;

export const resourceRange = (start: number, end: number): ResourceRange => {
  const from = Math.floor(start);
  const to = Math.floor(end);
  // Include both endpoints within the backend's 1,440-point limit.
  return { end: to, start: from, step: Math.max(15, Math.ceil((to - from) / 1439)) };
};

export interface ResourceAllocation {
  allocationId: string;
  // When the allocation got its resources, in Unix seconds; absent while it is queued.
  containerStart?: number;
  // When the allocation released its resources; absent while it still holds them.
  end?: number;
}

const unixSeconds = (value: unknown): number | undefined => {
  if (typeof value !== 'string') return undefined;
  const ms = Date.parse(value);
  return Number.isFinite(ms) ? Math.floor(ms / 1000) : undefined;
};

// Anything that is not an allocation list, such as an older master's answer, yields undefined.
export const parseResourceAllocations = (data: unknown): ResourceAllocation[] | undefined => {
  const items = (data as { allocations?: unknown } | null)?.allocations;
  if (!Array.isArray(items)) return undefined;
  const allocations: ResourceAllocation[] = [];
  for (const item of items as {
    allocation_id?: unknown;
    container_start?: unknown;
    end?: unknown;
  }[]) {
    if (typeof item?.allocation_id !== 'string') return undefined;
    allocations.push({
      allocationId: item.allocation_id,
      containerStart: unixSeconds(item.container_start),
      end: unixSeconds(item.end),
    });
  }
  return allocations;
};

export interface SinceStartBounds {
  start: number;
  end?: number;
  // No container start is recorded for the selection, so the range begins at the task start.
  noContainerStart?: boolean;
}

// "Since start" covers the earliest container start of all allocations, or one allocation from
// its container start to its end. Without an allocation list it begins at the task start.
export const sinceStartBounds = (
  allocations: ResourceAllocation[] | null,
  allocationId: string,
  taskStart: number,
): SinceStartBounds => {
  if (!allocations) return { start: taskStart };
  if (!allocationId) {
    const starts = allocations
      .map((item) => item.containerStart)
      .filter((start): start is number => start !== undefined);
    return starts.length
      ? { start: Math.min(...starts) }
      : { noContainerStart: true, start: taskStart };
  }
  const item = allocations.find((candidate) => candidate.allocationId === allocationId);
  if (!item) return { start: taskStart };
  if (item.containerStart === undefined) return { noContainerStart: true, start: taskStart };
  return { end: item.end, start: item.containerStart };
};

// Limits a "Since start" range to the most recent RESOURCE_MAX_SPAN seconds before `now`.
export const sinceStartWindow = (
  bounds: SinceStartBounds,
  now: number,
): { start: number; end: number; clamped: boolean } => {
  const end = Math.floor(bounds.end === undefined ? now : Math.min(bounds.end, now));
  const earliest = end - RESOURCE_MAX_SPAN;
  return {
    clamped: bounds.start < earliest,
    end,
    start: Math.min(Math.max(Math.floor(bounds.start), earliest), end - 1),
  };
};

// A full sampling grid preserves missing scrapes as nulls. Never interpolate
// absent samples or convert them to zero, including between allocation runs.
export const alignResourceSeries = (
  series: ResourceSeries[],
  range: ResourceRange,
): AlignedData => {
  const timestamps: number[] = [];
  for (let t = range.start; t <= range.end; t += range.step) timestamps.push(t);
  return [
    timestamps,
    ...series.map((item) => {
      const samples = new Map(item.samples);
      return timestamps.map((t) => {
        const value = samples.get(t);
        return value != null && Number.isFinite(value) ? value : null;
      });
    }),
  ];
};

export const resourceSeriesName = ({ labels }: ResourceSeries): string =>
  [labels.allocation_id, labels.node, labels.gpu_uuid].filter(Boolean).join(' · ') || 'Task';
