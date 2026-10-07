import { SortOrder } from 'antd/es/table/interface';

import { Job } from 'types';

/** The queue order: no column sort. */
export const QUEUE_ORDER = { sortDesc: false, sortKey: 'jobsAhead' } as const;

/** A row's sort value. A missing value sorts last in both directions. */
type SortValue = string | number[] | undefined;

interface ColumnSort {
  /** The first click sorts in the first direction, the second in the other, the third clears. */
  directions: SortOrder[];
  value: (job: Job, ownerName: (job: Job) => string | undefined) => SortValue;
}

const A_TO_Z_FIRST: SortOrder[] = ['ascend', 'descend'];
const MOST_FIRST: SortOrder[] = ['descend', 'ascend'];

/** The columns of the Active tab that sort, by column key. */
export const ACTIVE_SORTS: Record<string, ColumnSort> = {
  name: { directions: A_TO_Z_FIRST, value: (job) => ('name' in job ? job.name : undefined) },
  slots: { directions: MOST_FIRST, value: (job) => [job.allocatedSlots, job.requestedSlots] },
  submitted: {
    directions: MOST_FIRST,
    value: (job) => {
      if (!('submissionTime' in job) || !job.submissionTime) return undefined;
      const time = new Date(job.submissionTime).getTime();
      return Number.isNaN(time) ? undefined : [time];
    },
  },
  user: { directions: A_TO_Z_FIRST, value: (job, ownerName) => ownerName(job) },
};

export const isActiveSortKey = (key: unknown): key is string =>
  typeof key === 'string' && Object.keys(ACTIVE_SORTS).includes(key);

const compareCodePoints = (a: string, b: string): number => {
  const [x, y] = [
    Array.from(a, (c) => c.codePointAt(0) ?? 0),
    Array.from(b, (c) => c.codePointAt(0) ?? 0),
  ];
  for (let i = 0; i < Math.min(x.length, y.length); i++) {
    if (x[i] !== y[i]) return x[i] - y[i];
  }
  return x.length - y.length;
};

const foldAToZ = (text: string): string => text.replace(/[A-Z]+/g, (s) => s.toLowerCase());

/**
 * The Jobs page's text order: A-Z folded to a-z only, by code point, then the text as is; as
 * Postgres orders `lower(x COLLATE "C"), x COLLATE "C"`.
 */
export const compareText = (a: string, b: string): number =>
  compareCodePoints(foldAToZ(a), foldAToZ(b)) || compareCodePoints(a, b);

const compareValues = (a: string | number[], b: string | number[]): number => {
  if (typeof a === 'string' || typeof b === 'string') {
    return typeof a === 'string' && typeof b === 'string' ? compareText(a, b) : 0;
  }
  for (let i = 0; i < Math.min(a.length, b.length); i++) {
    if (a[i] !== b[i]) return a[i] - b[i];
  }
  return a.length - b.length;
};

/**
 * The jobs, given in queue order, sorted by a column of the Active tab. Missing values go last in
 * both directions, and jobs with equal values keep their queue order.
 */
export const sortJobs = (
  jobs: Job[],
  key: string,
  desc: boolean,
  ownerName: (job: Job) => string | undefined,
): Job[] => {
  const sort = ACTIVE_SORTS[key];
  if (!sort) return jobs;
  return jobs
    .map((job, index) => ({ index, job, value: sort.value(job, ownerName) }))
    .sort((a, b) => {
      if (a.value === undefined || b.value === undefined) {
        return Number(a.value === undefined) - Number(b.value === undefined) || a.index - b.index;
      }
      const order = compareValues(a.value, b.value);
      return (desc ? -order : order) || a.index - b.index;
    })
    .map(({ job }) => job);
};
