import dayjs, { Dayjs } from 'dayjs';
import utc from 'dayjs/plugin/utc';

import { GroupBy } from 'pages/Cluster/ClusterHistoricalUsage.settings';
import {
  UserPoolResources,
  UserResourceCounts,
  UserResources,
  UserUsageEntry,
  ValueOf,
} from 'types';

dayjs.extend(utc);

export const DAY_FORMAT = 'YYYY-MM-DD';
export const MONTH_FORMAT = 'YYYY-MM';

/** The weeks of the calendar, the last one the current UTC week. */
export const CALENDAR_WEEKS = 53;
/** The days of the Day view, today included. */
export const USAGE_DAYS = 31;
/** The months of the Month view: twelve whole months and the current one. */
export const USAGE_MONTHS = 13;

const HOUR_SECONDS = 3600;
/** The active hours above which a day reaches calendar levels 2 and 3. */
const LEVEL_HOURS = [4, 12];

/** The resources the Active days section shows: GPUs, Aux containers, or both. */
export const ResourceMode = {
  Aux: 'aux',
  Both: 'both',
  Gpu: 'gpu',
} as const;

export type ResourceMode = ValueOf<typeof ResourceMode>;

export type ActivityLevel = 0 | 1 | 2 | 3;

export interface CalendarDay {
  /** The UTC day, YYYY-MM-DD. */
  date: string;
  /** The day's usage; undefined for a day without records. */
  entry?: UserUsageEntry;
  level: ActivityLevel;
}

export interface CalendarWeek {
  /** The days from Monday, up to today in the current week. */
  days: CalendarDay[];
  /** The short name of the month whose first day is in this week. */
  monthLabel?: string;
}

export const todayUtc = (now: number = Date.now()): Dayjs => dayjs.utc(now).startOf('day');

/** The Monday of the UTC week of a UTC day. */
const mondayOf = (day: Dayjs): Dayjs => day.subtract((day.day() + 6) % 7, 'day');

/** The first day of the calendar: the Monday 52 weeks before the current UTC week. */
export const calendarStart = (now: number = Date.now()): Dayjs =>
  mondayOf(todayUtc(now)).subtract(CALENDAR_WEEKS - 1, 'week');

/**
 * The days of the one usage request: from the earlier of the first day of the month twelve
 * months back and the first day of the calendar, to today (UTC).
 */
export const usageRange = (now: number = Date.now()): { endDate: string; startDate: string } => {
  const today = todayUtc(now);
  const firstMonth = today.subtract(USAGE_MONTHS - 1, 'month').startOf('month');
  const gridStart = calendarStart(now);
  const start = firstMonth.isBefore(gridStart) ? firstMonth : gridStart;
  return { endDate: today.format(DAY_FORMAT), startDate: start.format(DAY_FORMAT) };
};

export const activityLevel = (activeSeconds: number): ActivityLevel => {
  if (activeSeconds <= 0) return 0;
  const hours = activeSeconds / HOUR_SECONDS;
  if (hours > LEVEL_HOURS[1]) return 3;
  if (hours > LEVEL_HOURS[0]) return 2;
  return 1;
};

/** The weeks of the calendar, Monday first, ending with today, levelled by active seconds. */
export const calendarWeeks = (
  entries: UserUsageEntry[],
  activeSeconds: (entry: UserUsageEntry) => number,
  now: number = Date.now(),
): CalendarWeek[] => {
  const byDate = new Map(entries.map((entry) => [entry.periodStart, entry]));
  const today = todayUtc(now);
  const start = calendarStart(now);
  const weeks: CalendarWeek[] = [];
  for (let week = 0; week < CALENDAR_WEEKS; week++) {
    const monday = start.add(week, 'week');
    const days: CalendarDay[] = [];
    let monthLabel: string | undefined;
    for (let weekday = 0; weekday < 7; weekday++) {
      const day = monday.add(weekday, 'day');
      if (day.date() === 1) monthLabel = day.format('MMM');
      if (day.isAfter(today)) continue;
      const date = day.format(DAY_FORMAT);
      const entry = byDate.get(date);
      days.push({ date, entry, level: activityLevel(entry ? activeSeconds(entry) : 0) });
    }
    weeks.push({ days, monthLabel });
  }
  return weeks;
};

const addEntries = (a: UserUsageEntry, b: UserUsageEntry): UserUsageEntry => ({
  auxActiveDays: a.auxActiveDays + b.auxActiveDays,
  auxActiveSeconds: a.auxActiveSeconds + b.auxActiveSeconds,
  auxContainerSeconds: a.auxContainerSeconds + b.auxContainerSeconds,
  cpuSlotSeconds: a.cpuSlotSeconds + b.cpuSlotSeconds,
  gpuActiveDays: a.gpuActiveDays + b.gpuActiveDays,
  gpuActiveSeconds: a.gpuActiveSeconds + b.gpuActiveSeconds,
  gpuOrAuxActiveDays: a.gpuOrAuxActiveDays + b.gpuOrAuxActiveDays,
  gpuOrAuxActiveSeconds: a.gpuOrAuxActiveSeconds + b.gpuOrAuxActiveSeconds,
  gpuSeconds: a.gpuSeconds + b.gpuSeconds,
  periodStart: a.periodStart,
  unclassifiedSlotSeconds: a.unclassifiedSlotSeconds + b.unclassifiedSlotSeconds,
});

/** The months of daily entries, each the sum of its days. */
export const monthlyUsage = (entries: UserUsageEntry[]): UserUsageEntry[] => {
  const months = new Map<string, UserUsageEntry>();
  entries.forEach((entry) => {
    const month = entry.periodStart.slice(0, MONTH_FORMAT.length);
    const sum = months.get(month);
    months.set(month, sum ? addEntries(sum, entry) : { ...entry, periodStart: month });
  });
  return [...months.values()].sort((a, b) => a.periodStart.localeCompare(b.periodStart));
};

/** The entries of the last 31 days, today included. */
export const lastDays = (entries: UserUsageEntry[], now: number = Date.now()): UserUsageEntry[] => {
  const first = todayUtc(now)
    .subtract(USAGE_DAYS - 1, 'day')
    .format(DAY_FORMAT);
  return entries.filter((entry) => entry.periodStart >= first);
};

/** The months of the last 13 UTC months, the current one included. */
export const lastMonths = (
  entries: UserUsageEntry[],
  now: number = Date.now(),
): UserUsageEntry[] => {
  const first = todayUtc(now)
    .subtract(USAGE_MONTHS - 1, 'month')
    .format(MONTH_FORMAT);
  return monthlyUsage(entries).filter((entry) => entry.periodStart >= first);
};

export const toHours = (seconds: number): number => seconds / HOUR_SECONDS;

export const formatHours = (seconds: number): string => toHours(seconds).toFixed(1);

/** The counts of all pools. */
export const totalResources = (pools: UserPoolResources[]): UserResourceCounts =>
  pools.reduce<UserResourceCounts>(
    (total, { counts }) => ({
      auxContainers: total.auxContainers + counts.auxContainers,
      cpuSlots: total.cpuSlots + counts.cpuSlots,
      gpus: total.gpus + counts.gpus,
    }),
    { auxContainers: 0, cpuSlots: 0, gpus: 0 },
  );

/* The projection: what each block of a user page shows, from the daily entries and the resources. */

type Kind = 'aux' | 'gpu';

interface KindView {
  /** The title of the current-resource card. */
  card: string;
  count: (counts: UserResourceCounts) => number;
  /** The title of the table column and of the day value. */
  hours: string;
  seconds: (entry: UserUsageEntry) => number;
  /** The label of the chart series. */
  series: string;
}

const KINDS: Record<Kind, KindView> = {
  aux: {
    card: 'Aux containers',
    count: (counts) => counts.auxContainers,
    hours: 'Aux container hours',
    seconds: (entry) => entry.auxContainerSeconds,
    series: 'Aux',
  },
  gpu: {
    card: 'GPUs',
    count: (counts) => counts.gpus,
    hours: 'GPU hours',
    seconds: (entry) => entry.gpuSeconds,
    series: 'GPU',
  },
};

const MODE_KINDS: Record<ResourceMode, Kind[]> = {
  [ResourceMode.Aux]: ['aux'],
  [ResourceMode.Both]: ['gpu', 'aux'],
  [ResourceMode.Gpu]: ['gpu'],
};

interface ActiveView {
  days: (entry: UserUsageEntry) => number;
  seconds: (entry: UserUsageEntry) => number;
}

const MODE_ACTIVE: Record<ResourceMode, ActiveView> = {
  [ResourceMode.Aux]: {
    days: (entry) => entry.auxActiveDays,
    seconds: (entry) => entry.auxActiveSeconds,
  },
  [ResourceMode.Both]: {
    days: (entry) => entry.gpuOrAuxActiveDays,
    seconds: (entry) => entry.gpuOrAuxActiveSeconds,
  },
  [ResourceMode.Gpu]: {
    days: (entry) => entry.gpuActiveDays,
    seconds: (entry) => entry.gpuActiveSeconds,
  },
};

const MODE_OPTIONS: ModeOption[] = [
  { id: ResourceMode.Gpu, label: 'GPU' },
  { id: ResourceMode.Aux, label: 'Aux' },
  { id: ResourceMode.Both, label: 'GPU+Aux' },
];

export interface ModeOption {
  id: ResourceMode;
  label: string;
}

export interface ResourceCard {
  key: string;
  title: string;
  value: number;
}

export interface UsageValue {
  title: string;
  value: string;
}

export interface UsageColumn {
  key: string;
  numeric: boolean;
  title: string;
}

/** A row of the usage table: the value shown in each column, by column key. */
export type UsageRow = Record<string, string>;

export interface UsageView {
  columns: UsageColumn[];
  /** The hours of each series, oldest first. */
  hoursByLabel: Record<string, number[]>;
  /** The rows, newest first. */
  rows: UsageRow[];
  /** The days (YYYY-MM-DD) or months (YYYY-MM) of the series, oldest first. */
  time: string[];
}

export interface CalendarView {
  /** The days of the grid with active time. */
  activeDays: number;
  weeks: CalendarWeek[];
}

/** The Active days section: the calendar, its count and the selected day, in one mode. */
export interface ActiveDaysProjection {
  calendar: (entries: UserUsageEntry[], now?: number) => CalendarView;
  /** The values of a calendar day; none for a day without records. */
  day: (day: CalendarDay) => UsageValue[];
  /** The mode shown. */
  mode: ResourceMode;
  /** The modes to choose from; none where only one is shown. */
  modes: ModeOption[];
}

/** The Current resources section. */
export interface CurrentResourcesProjection {
  cards: (resources: UserResources) => ResourceCard[];
}

/** The Usage section: the chart and the table by day or by month. */
export interface UsageSectionProjection {
  view: (groupBy: GroupBy, entries: UserUsageEntry[], now?: number) => UsageView;
}

export interface UsageProjection {
  activeDays: ActiveDaysProjection;
  currentResources: CurrentResourcesProjection;
  usage: UsageSectionProjection;
}

export const PERIOD_COLUMN = 'periodStart';
const ACTIVE_DAYS_COLUMN = 'activeDays';

const kindViews = (mode: ResourceMode) =>
  MODE_KINDS[mode].map((kind) => ({ key: kind, ...KINDS[kind] }));

const activeDaysProjection = (mode: ResourceMode, modes: ModeOption[]): ActiveDaysProjection => {
  const kinds = kindViews(mode);
  const active = MODE_ACTIVE[mode];
  return {
    calendar: (entries, now = Date.now()) => {
      const weeks = calendarWeeks(entries, active.seconds, now);
      const activeDays = weeks.reduce(
        (count, week) => count + week.days.filter((day) => day.level > 0).length,
        0,
      );
      return { activeDays, weeks };
    },
    day: ({ entry }) =>
      entry
        ? kinds.map((kind) => ({ title: kind.hours, value: formatHours(kind.seconds(entry)) }))
        : [],
    mode,
    modes,
  };
};

const currentResourcesProjection = (mode: ResourceMode): CurrentResourcesProjection => {
  const kinds = kindViews(mode);
  return {
    cards: ({ pools }) => {
      const total = totalResources(pools);
      return kinds.map((kind) => ({ key: kind.key, title: kind.card, value: kind.count(total) }));
    },
  };
};

const usageSectionProjection = (mode: ResourceMode): UsageSectionProjection => {
  const kinds = kindViews(mode);
  const active = MODE_ACTIVE[mode];
  return {
    view: (groupBy, entries, now = Date.now()) => {
      const byMonth = groupBy === GroupBy.Month;
      const shown = byMonth ? lastMonths(entries, now) : lastDays(entries, now);
      const columns: UsageColumn[] = [
        { key: PERIOD_COLUMN, numeric: false, title: byMonth ? 'Month' : 'Date' },
        ...kinds.map((kind) => ({ key: kind.key, numeric: true, title: kind.hours })),
      ];
      if (byMonth) columns.push({ key: ACTIVE_DAYS_COLUMN, numeric: true, title: 'Active days' });
      const rows = shown
        .map((entry) => {
          const row: UsageRow = { [PERIOD_COLUMN]: entry.periodStart };
          kinds.forEach((kind) => (row[kind.key] = formatHours(kind.seconds(entry))));
          if (byMonth) row[ACTIVE_DAYS_COLUMN] = String(active.days(entry));
          return row;
        })
        .reverse();
      return {
        columns,
        hoursByLabel: Object.fromEntries(
          kinds.map((kind) => [kind.series, shown.map((entry) => toHours(kind.seconds(entry)))]),
        ),
        rows,
        time: shown.map((entry) => entry.periodStart),
      };
    },
  };
};

/*
 * Built once: the parts of a section are the same objects whatever the other sections show, so a
 * section whose part does not change does not render again.
 */
const ACTIVE_DAYS: Record<ResourceMode, ActiveDaysProjection> = {
  [ResourceMode.Aux]: activeDaysProjection(ResourceMode.Aux, MODE_OPTIONS),
  [ResourceMode.Both]: activeDaysProjection(ResourceMode.Both, MODE_OPTIONS),
  [ResourceMode.Gpu]: activeDaysProjection(ResourceMode.Gpu, MODE_OPTIONS),
};
const GPU_ACTIVE_DAYS = activeDaysProjection(ResourceMode.Gpu, []);
const WITH_AUX = {
  currentResources: currentResourcesProjection(ResourceMode.Both),
  usage: usageSectionProjection(ResourceMode.Both),
};
const GPU_ONLY = {
  currentResources: currentResourcesProjection(ResourceMode.Gpu),
  usage: usageSectionProjection(ResourceMode.Gpu),
};

/**
 * What each section of a user page shows. With Aux shown, the Active days section shows the mode
 * chosen there and the other sections show GPUs and Aux containers both; without, every section
 * shows GPUs only and there is no mode to choose.
 */
export const usageProjection = (mode: ResourceMode, auxShown: boolean): UsageProjection =>
  auxShown
    ? { activeDays: ACTIVE_DAYS[mode], ...WITH_AUX }
    : { activeDays: GPU_ACTIVE_DAYS, ...GPU_ONLY };
