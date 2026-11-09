import { UserUsageEntry } from 'types';

import {
  activityLevel,
  CALENDAR_WEEKS,
  calendarWeeks,
  formatHours,
  lastDays,
  lastMonths,
  monthlyUsage,
  totalResources,
  usageRange,
} from './usage';

// A Tuesday: the current UTC week starts on Monday 2026-11-09.
const NOW = Date.parse('2026-11-10T15:00:00Z');
const HOUR = 3600;

const day = (periodStart: string, hours: Partial<Record<string, number>> = {}): UserUsageEntry => ({
  activeDays: (hours.active ?? 0) > 0 ? 1 : 0,
  activeSeconds: (hours.active ?? 0) * HOUR,
  auxContainerSeconds: hours.aux === undefined ? undefined : hours.aux * HOUR,
  cpuSlotSeconds: (hours.cpu ?? 0) * HOUR,
  gpuSeconds: (hours.gpu ?? 0) * HOUR,
  periodStart,
});

describe('user usage', () => {
  it('requests from the first day of the month twelve months back to today', () => {
    // The calendar starts on Monday 2025-11-10, after 2025-11-01.
    expect(usageRange(NOW)).toEqual({ endDate: '2026-11-10', startDate: '2025-11-01' });
  });

  it('requests from the earlier of the first month and the first day of the calendar', () => {
    // On Monday 2026-11-30 the calendar starts on Monday 2025-12-01, the first month on 2025-11-01.
    expect(usageRange(Date.parse('2026-11-30T00:00:00Z'))).toEqual({
      endDate: '2026-11-30',
      startDate: '2025-11-01',
    });
    // On Sunday 2026-11-01 the week started on Monday 2026-10-26: the calendar starts on
    // Monday 2025-10-27, before the first month, 2025-11-01.
    expect(usageRange(Date.parse('2026-11-01T23:59:59Z'))).toEqual({
      endDate: '2026-11-01',
      startDate: '2025-10-27',
    });
    // On Wednesday 2026-12-02 the calendar starts on Monday 2025-12-01, the month on 2025-12-01.
    expect(usageRange(Date.parse('2026-12-02T12:00:00Z')).startDate).toBe('2025-12-01');
  });

  it('levels a day by its active hours at 4 h and 12 h', () => {
    expect(activityLevel(0)).toBe(0);
    expect(activityLevel(1)).toBe(1);
    expect(activityLevel(4 * HOUR)).toBe(1);
    expect(activityLevel(4 * HOUR + 1)).toBe(2);
    expect(activityLevel(12 * HOUR)).toBe(2);
    expect(activityLevel(12 * HOUR + 1)).toBe(3);
    expect(activityLevel(24 * HOUR)).toBe(3);
  });

  it('lays out 53 weeks from Monday, ending with today', () => {
    const weeks = calendarWeeks([day('2026-11-10', { active: 13 }), day('2025-11-10')], NOW);
    expect(weeks).toHaveLength(CALENDAR_WEEKS);
    expect(weeks[0].days.map((d) => d.date)).toEqual([
      '2025-11-10',
      '2025-11-11',
      '2025-11-12',
      '2025-11-13',
      '2025-11-14',
      '2025-11-15',
      '2025-11-16',
    ]);
    weeks.slice(0, -1).forEach((week) => expect(week.days).toHaveLength(7));
    // No days after today.
    expect(weeks.at(-1)?.days.map((d) => d.date)).toEqual(['2026-11-09', '2026-11-10']);
    expect(weeks.at(-1)?.days[1]).toMatchObject({ level: 3 });
    expect(weeks[0].days[0]).toMatchObject({ entry: { periodStart: '2025-11-10' }, level: 0 });
    // A day without an entry is not covered.
    expect(weeks[0].days[1].entry).toBeUndefined();
  });

  it('labels the weeks with the first day of a month', () => {
    const labels = calendarWeeks([], NOW)
      .map((week, index) => [index, week.monthLabel])
      .filter(([, label]) => label);
    // 2025-12-01 is the Monday of the fourth week; 2026-11-01 is the Sunday of the 51st.
    expect(labels[0]).toEqual([3, 'Dec']);
    expect(labels.at(-1)).toEqual([50, 'Nov']);
    expect(labels).toHaveLength(12);
  });

  it('sums the days of each month', () => {
    const months = monthlyUsage([
      day('2028-02-28', { active: 2, aux: 1, cpu: 1, gpu: 4 }),
      day('2028-02-29', { active: 3, gpu: 2 }),
      day('2028-03-01'),
      day('2028-03-31', { active: 1, gpu: 1 }),
    ]);
    expect(months).toEqual([
      {
        activeDays: 2,
        activeSeconds: 5 * HOUR,
        auxContainerSeconds: 1 * HOUR,
        cpuSlotSeconds: 1 * HOUR,
        gpuSeconds: 6 * HOUR,
        periodStart: '2028-02',
      },
      {
        activeDays: 1,
        activeSeconds: 1 * HOUR,
        auxContainerSeconds: undefined,
        cpuSlotSeconds: 0,
        gpuSeconds: 1 * HOUR,
        periodStart: '2028-03',
      },
    ]);
  });

  it('keeps the last 31 days and the last 13 months', () => {
    const entries = [
      day('2025-10-31', { gpu: 1 }),
      day('2025-11-01', { gpu: 2 }),
      day('2026-10-10', { gpu: 3 }),
      day('2026-10-11', { gpu: 4 }),
      day('2026-11-10', { gpu: 5 }),
    ];
    expect(lastDays(entries, NOW).map((e) => e.periodStart)).toEqual(['2026-10-11', '2026-11-10']);
    expect(lastMonths(entries, NOW).map((e) => [e.periodStart, e.gpuSeconds / HOUR])).toEqual([
      ['2025-11', 2],
      ['2026-10', 7],
      ['2026-11', 5],
    ]);
  });

  it('formats hours with one decimal', () => {
    expect(formatHours(0)).toBe('0.0');
    expect(formatHours(5400)).toBe('1.5');
  });

  it('sums the pools, Aux undefined when no pool reports it', () => {
    expect(
      totalResources([
        { counts: { auxContainers: 1, cpuSlots: 1, gpus: 2 }, resourcePool: 'a' },
        { counts: { cpuSlots: 0, gpus: 4 }, resourcePool: 'k8s' },
      ]),
    ).toEqual({ auxContainers: 1, cpuSlots: 1, gpus: 6 });
    expect(totalResources([{ counts: { cpuSlots: 0, gpus: 8 }, resourcePool: 'k8s' }])).toEqual({
      auxContainers: undefined,
      cpuSlots: 0,
      gpus: 8,
    });
    expect(totalResources([])).toEqual({ auxContainers: undefined, cpuSlots: 0, gpus: 0 });
  });
});
