import dayjs, { Dayjs } from 'dayjs';
import utc from 'dayjs/plugin/utc';

import { UserPoolResources, UserResourceCounts, UserUsageEntry } from 'types';

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
/** The active hours at which a day reaches calendar levels 2 and 3. */
const LEVEL_HOURS = [4, 12];

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
  if (hours >= LEVEL_HOURS[1]) return 3;
  if (hours >= LEVEL_HOURS[0]) return 2;
  return 1;
};

/** The weeks of the calendar, Monday first, ending with today. */
export const calendarWeeks = (
  entries: UserUsageEntry[],
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
      days.push({ date, entry, level: activityLevel(entry?.activeSeconds ?? 0) });
    }
    weeks.push({ days, monthLabel });
  }
  return weeks;
};

const sumOptional = (a?: number, b?: number): number | undefined =>
  a === undefined && b === undefined ? undefined : (a ?? 0) + (b ?? 0);

/** The months of daily entries, each the sum of its days. */
export const monthlyUsage = (entries: UserUsageEntry[]): UserUsageEntry[] => {
  const months = new Map<string, UserUsageEntry>();
  entries.forEach((entry) => {
    const month = entry.periodStart.slice(0, MONTH_FORMAT.length);
    const sum = months.get(month);
    months.set(
      month,
      sum
        ? {
            activeDays: sum.activeDays + entry.activeDays,
            activeSeconds: sum.activeSeconds + entry.activeSeconds,
            auxContainerSeconds: sumOptional(sum.auxContainerSeconds, entry.auxContainerSeconds),
            cpuSlotSeconds: sum.cpuSlotSeconds + entry.cpuSlotSeconds,
            gpuSeconds: sum.gpuSeconds + entry.gpuSeconds,
            periodStart: month,
          }
        : { ...entry, periodStart: month },
    );
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

/** The counts of all pools; Aux undefined when no pool reports it. */
export const totalResources = (pools: UserPoolResources[]): UserResourceCounts =>
  pools.reduce<UserResourceCounts>(
    (total, { counts }) => ({
      auxContainers: sumOptional(total.auxContainers, counts.auxContainers),
      cpuSlots: total.cpuSlots + counts.cpuSlots,
      gpus: total.gpus + counts.gpus,
    }),
    { auxContainers: undefined, cpuSlots: 0, gpus: 0 },
  );
