import { GroupBy } from 'pages/Cluster/ClusterHistoricalUsage.settings';
import { UserResources, UserUsageEntry } from 'types';

import {
  activityLevel,
  CALENDAR_WEEKS,
  calendarWeeks,
  formatHours,
  lastDays,
  lastMonths,
  monthlyUsage,
  ResourceMode,
  totalResources,
  usageProjection,
  usageRange,
} from './usage';

// A Tuesday: the current UTC week starts on Monday 2026-11-09.
const NOW = Date.parse('2026-11-10T15:00:00Z');
const HOUR = 3600;

type Values = Partial<Omit<Record<keyof UserUsageEntry, number>, 'periodStart'>>;

/** An entry with the given hours (the seconds fields) and day counts (the days fields). */
const entry = (periodStart: string, values: Values = {}): UserUsageEntry => {
  const seconds = (key: keyof Values) => (values[key] ?? 0) * HOUR;
  return {
    auxActiveDays: values.auxActiveDays ?? 0,
    auxActiveSeconds: seconds('auxActiveSeconds'),
    auxContainerSeconds: seconds('auxContainerSeconds'),
    cpuSlotSeconds: seconds('cpuSlotSeconds'),
    gpuActiveDays: values.gpuActiveDays ?? 0,
    gpuActiveSeconds: seconds('gpuActiveSeconds'),
    gpuOrAuxActiveDays: values.gpuOrAuxActiveDays ?? 0,
    gpuOrAuxActiveSeconds: seconds('gpuOrAuxActiveSeconds'),
    gpuSeconds: seconds('gpuSeconds'),
    periodStart,
    unclassifiedSlotSeconds: seconds('unclassifiedSlotSeconds'),
  };
};

/** The active hours of a day with GPUs, with Aux containers and with either, and its days. */
const active = (gpu: number, aux: number, either: number): Values => ({
  auxActiveDays: aux > 0 ? 1 : 0,
  auxActiveSeconds: aux,
  gpuActiveDays: gpu > 0 ? 1 : 0,
  gpuActiveSeconds: gpu,
  gpuOrAuxActiveDays: either > 0 ? 1 : 0,
  gpuOrAuxActiveSeconds: either,
});

const ENTRIES: UserUsageEntry[] = [
  // GPUs only, in the first month but before the first day of the calendar, Monday 2025-11-10.
  entry('2025-11-05', { ...active(4, 0, 4), gpuSeconds: 4 }),
  entry('2026-10-20', { ...active(1, 2, 2), auxContainerSeconds: 2, gpuSeconds: 1 }),
  // GPUs only: 2 GPUs for 13 h.
  entry('2026-11-03', { ...active(13, 0, 13), gpuSeconds: 26 }),
  // Aux only: 2 containers for 5 h.
  entry('2026-11-04', { ...active(0, 5, 5), auxContainerSeconds: 10 }),
  // Both, overlapping: 3 h each, 3.5 h with either.
  entry('2026-11-05', { ...active(3, 3, 3.5), auxContainerSeconds: 3, gpuSeconds: 3 }),
  // CPU slots and slots of an unknown kind only.
  entry('2026-11-06', { cpuSlotSeconds: 16, unclassifiedSlotSeconds: 2 }),
  entry('2026-11-07'),
];

const RESOURCES: UserResources = {
  auxMetered: true,
  pools: [
    { counts: { auxContainers: 1, cpuSlots: 4, gpus: 2 }, resourcePool: 'default' },
    { counts: { auxContainers: 0, cpuSlots: 0, gpus: 1 }, resourcePool: 'k8s' },
  ],
};

const projection = (mode: ResourceMode) => usageProjection(mode, true);

/** The level of each day of the fixture in the calendar. */
const levels = (mode: ResourceMode) =>
  Object.fromEntries(
    projection(mode)
      .calendar(ENTRIES, NOW)
      .weeks.flatMap((week) => week.days)
      .filter((day) => day.entry)
      .map((day) => [day.date, day.level]),
  );

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
    const weeks = calendarWeeks(
      [entry('2026-11-10', { gpuActiveSeconds: 13 }), entry('2025-11-10')],
      (e) => e.gpuActiveSeconds,
      NOW,
    );
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
    const labels = calendarWeeks([], () => 0, NOW)
      .map((week, index) => [index, week.monthLabel])
      .filter(([, label]) => label);
    // 2025-12-01 is the Monday of the fourth week; 2026-11-01 is the Sunday of the 51st.
    expect(labels[0]).toEqual([3, 'Dec']);
    expect(labels.at(-1)).toEqual([50, 'Nov']);
    expect(labels).toHaveLength(12);
  });

  it('sums every field of the days of each month', () => {
    const months = monthlyUsage([
      entry('2028-02-28', {
        ...active(2, 1, 2.5),
        auxContainerSeconds: 1,
        cpuSlotSeconds: 1,
        gpuSeconds: 4,
        unclassifiedSlotSeconds: 0.5,
      }),
      entry('2028-02-29', { ...active(3, 0, 3), gpuSeconds: 2 }),
      entry('2028-03-01'),
      entry('2028-03-31', { ...active(0, 1, 1), auxContainerSeconds: 1 }),
    ]);
    expect(months).toEqual([
      entry('2028-02', {
        auxActiveDays: 1,
        auxActiveSeconds: 1,
        auxContainerSeconds: 1,
        cpuSlotSeconds: 1,
        gpuActiveDays: 2,
        gpuActiveSeconds: 5,
        gpuOrAuxActiveDays: 2,
        gpuOrAuxActiveSeconds: 5.5,
        gpuSeconds: 6,
        unclassifiedSlotSeconds: 0.5,
      }),
      entry('2028-03', { ...active(0, 1, 1), auxContainerSeconds: 1 }),
    ]);
  });

  it('keeps the last 31 days and the last 13 months', () => {
    const entries = [
      entry('2025-10-31', { gpuSeconds: 1 }),
      entry('2025-11-01', { gpuSeconds: 2 }),
      entry('2026-10-10', { gpuSeconds: 3 }),
      entry('2026-10-11', { gpuSeconds: 4 }),
      entry('2026-11-10', { gpuSeconds: 5 }),
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

  it('sums the pools', () => {
    expect(totalResources(RESOURCES.pools)).toEqual({ auxContainers: 1, cpuSlots: 4, gpus: 3 });
    expect(totalResources([])).toEqual({ auxContainers: 0, cpuSlots: 0, gpus: 0 });
  });
});

describe('the projection of a resource mode', () => {
  it('shows the current GPUs, Aux containers or both', () => {
    expect(projection(ResourceMode.Both).cards(RESOURCES)).toEqual([
      { key: 'gpu', title: 'GPUs', value: 3 },
      { key: 'aux', title: 'Aux containers', value: 1 },
    ]);
    expect(projection(ResourceMode.Gpu).cards(RESOURCES)).toEqual([
      { key: 'gpu', title: 'GPUs', value: 3 },
    ]);
    expect(projection(ResourceMode.Aux).cards(RESOURCES)).toEqual([
      { key: 'aux', title: 'Aux containers', value: 1 },
    ]);
    expect(projection(ResourceMode.Both).cards({ auxMetered: true, pools: [] })).toEqual([
      { key: 'gpu', title: 'GPUs', value: 0 },
      { key: 'aux', title: 'Aux containers', value: 0 },
    ]);
  });

  it('shows the days of the mode in the chart and the table', () => {
    const time = [
      '2026-10-20',
      '2026-11-03',
      '2026-11-04',
      '2026-11-05',
      '2026-11-06',
      '2026-11-07',
    ];
    const both = projection(ResourceMode.Both).usage(GroupBy.Day, ENTRIES, NOW);
    expect(both.time).toEqual(time);
    expect(Object.keys(both.hoursByLabel)).toEqual(['GPU', 'Aux']);
    expect(both.hoursByLabel).toEqual({ Aux: [2, 0, 10, 3, 0, 0], GPU: [1, 26, 0, 3, 0, 0] });
    expect(both.columns).toEqual([
      { key: 'periodStart', numeric: false, title: 'Date' },
      { key: 'gpu', numeric: true, title: 'GPU hours' },
      { key: 'aux', numeric: true, title: 'Aux container hours' },
    ]);
    expect(both.rows).toEqual([
      { aux: '0.0', gpu: '0.0', periodStart: '2026-11-07' },
      { aux: '0.0', gpu: '0.0', periodStart: '2026-11-06' },
      { aux: '3.0', gpu: '3.0', periodStart: '2026-11-05' },
      { aux: '10.0', gpu: '0.0', periodStart: '2026-11-04' },
      { aux: '0.0', gpu: '26.0', periodStart: '2026-11-03' },
      { aux: '2.0', gpu: '1.0', periodStart: '2026-10-20' },
    ]);

    const gpu = projection(ResourceMode.Gpu).usage(GroupBy.Day, ENTRIES, NOW);
    expect(gpu.time).toEqual(time);
    expect(gpu.hoursByLabel).toEqual({ GPU: [1, 26, 0, 3, 0, 0] });
    expect(gpu.columns.map((column) => column.title)).toEqual(['Date', 'GPU hours']);
    expect(gpu.rows.map((row) => Object.keys(row))).toEqual(Array(6).fill(['periodStart', 'gpu']));
    expect(gpu.rows.map((row) => row.gpu)).toEqual(['0.0', '0.0', '3.0', '0.0', '26.0', '1.0']);

    const aux = projection(ResourceMode.Aux).usage(GroupBy.Day, ENTRIES, NOW);
    expect(aux.time).toEqual(time);
    expect(aux.hoursByLabel).toEqual({ Aux: [2, 0, 10, 3, 0, 0] });
    expect(aux.columns.map((column) => column.title)).toEqual(['Date', 'Aux container hours']);
    expect(aux.rows.map((row) => Object.keys(row))).toEqual(Array(6).fill(['periodStart', 'aux']));
    expect(aux.rows.map((row) => row.aux)).toEqual(['0.0', '0.0', '3.0', '10.0', '0.0', '2.0']);
  });

  it("sums the months and counts each month's active days of the mode", () => {
    const both = projection(ResourceMode.Both).usage(GroupBy.Month, ENTRIES, NOW);
    expect(both.time).toEqual(['2025-11', '2026-10', '2026-11']);
    expect(both.hoursByLabel).toEqual({ Aux: [0, 2, 13], GPU: [4, 1, 29] });
    expect(both.columns.map((column) => [column.key, column.title])).toEqual([
      ['periodStart', 'Month'],
      ['gpu', 'GPU hours'],
      ['aux', 'Aux container hours'],
      ['activeDays', 'Active days'],
    ]);
    // November 2026: the GPU day, the Aux day and the day with both, not 2 + 2.
    expect(both.rows).toEqual([
      { activeDays: '3', aux: '13.0', gpu: '29.0', periodStart: '2026-11' },
      { activeDays: '1', aux: '2.0', gpu: '1.0', periodStart: '2026-10' },
      { activeDays: '1', aux: '0.0', gpu: '4.0', periodStart: '2025-11' },
    ]);
    const gpu = projection(ResourceMode.Gpu).usage(GroupBy.Month, ENTRIES, NOW);
    expect(gpu.columns.map((column) => column.title)).toEqual([
      'Month',
      'GPU hours',
      'Active days',
    ]);
    expect(gpu.rows).toEqual([
      { activeDays: '2', gpu: '29.0', periodStart: '2026-11' },
      { activeDays: '1', gpu: '1.0', periodStart: '2026-10' },
      { activeDays: '1', gpu: '4.0', periodStart: '2025-11' },
    ]);
    const aux = projection(ResourceMode.Aux).usage(GroupBy.Month, ENTRIES, NOW);
    expect(aux.columns.map((column) => column.title)).toEqual([
      'Month',
      'Aux container hours',
      'Active days',
    ]);
    expect(aux.rows).toEqual([
      { activeDays: '2', aux: '13.0', periodStart: '2026-11' },
      { activeDays: '1', aux: '2.0', periodStart: '2026-10' },
      { activeDays: '0', aux: '0.0', periodStart: '2025-11' },
    ]);
  });

  it('levels the calendar by the active time of the mode, never by CPU slots', () => {
    expect(levels(ResourceMode.Gpu)).toEqual({
      '2026-10-20': 1,
      '2026-11-03': 3,
      '2026-11-04': 0,
      '2026-11-05': 1,
      '2026-11-06': 0,
      '2026-11-07': 0,
    });
    expect(levels(ResourceMode.Aux)).toEqual({
      '2026-10-20': 1,
      '2026-11-03': 0,
      '2026-11-04': 2,
      '2026-11-05': 1,
      '2026-11-06': 0,
      '2026-11-07': 0,
    });
    // The day with both has 3.5 h with either, not 6 h.
    expect(levels(ResourceMode.Both)).toEqual({
      '2026-10-20': 1,
      '2026-11-03': 3,
      '2026-11-04': 2,
      '2026-11-05': 1,
      '2026-11-06': 0,
      '2026-11-07': 0,
    });
  });

  it('counts the active days of the shown grid only', () => {
    // 2025-11-05 is in the request, before the grid.
    expect(projection(ResourceMode.Gpu).calendar(ENTRIES, NOW).activeDays).toBe(3);
    expect(projection(ResourceMode.Aux).calendar(ENTRIES, NOW).activeDays).toBe(3);
    expect(projection(ResourceMode.Both).calendar(ENTRIES, NOW).activeDays).toBe(4);
    expect(projection(ResourceMode.Both).calendar([], NOW).activeDays).toBe(0);
  });

  it("shows a day's values of the mode", () => {
    const day = { date: '2026-11-05', entry: ENTRIES[4], level: 1 as const };
    expect(projection(ResourceMode.Both).day(day)).toEqual([
      { title: 'GPU hours', value: '3.0' },
      { title: 'Aux container hours', value: '3.0' },
    ]);
    expect(projection(ResourceMode.Gpu).day(day)).toEqual([{ title: 'GPU hours', value: '3.0' }]);
    expect(projection(ResourceMode.Aux).day(day)).toEqual([
      { title: 'Aux container hours', value: '3.0' },
    ]);
    // The CPU day shows no CPU slots.
    expect(
      projection(ResourceMode.Both).day({ date: '2026-11-06', entry: ENTRIES[5], level: 0 }),
    ).toEqual([
      { title: 'GPU hours', value: '0.0' },
      { title: 'Aux container hours', value: '0.0' },
    ]);
    // A day without records.
    expect(projection(ResourceMode.Both).day({ date: '2026-10-31', level: 0 })).toEqual([]);
  });

  it('changes the cards, the chart, the table, the calendar and the day together', () => {
    const view = (mode: ResourceMode) => {
      const p = projection(mode);
      const usage = p.usage(GroupBy.Day, ENTRIES, NOW);
      const calendar = p.calendar(ENTRIES, NOW);
      const day = calendar.weeks.flatMap((week) => week.days).find((d) => d.date === '2026-11-04');
      return {
        activeDays: calendar.activeDays,
        cards: p.cards(RESOURCES).map((card) => card.title),
        columns: usage.columns.slice(1).map((column) => column.title),
        day: day && p.day(day).map((value) => value.title),
        level: day?.level,
        series: Object.keys(usage.hoursByLabel),
      };
    };
    expect(view(ResourceMode.Gpu)).toEqual({
      activeDays: 3,
      cards: ['GPUs'],
      columns: ['GPU hours'],
      day: ['GPU hours'],
      level: 0,
      series: ['GPU'],
    });
    expect(view(ResourceMode.Aux)).toEqual({
      activeDays: 3,
      cards: ['Aux containers'],
      columns: ['Aux container hours'],
      day: ['Aux container hours'],
      level: 2,
      series: ['Aux'],
    });
    expect(view(ResourceMode.Both)).toEqual({
      activeDays: 4,
      cards: ['GPUs', 'Aux containers'],
      columns: ['GPU hours', 'Aux container hours'],
      day: ['GPU hours', 'Aux container hours'],
      level: 2,
      series: ['GPU', 'Aux'],
    });
  });

  it('shows GPUs only where the cluster does not meter Aux', () => {
    const p = usageProjection(ResourceMode.Both, false);
    expect(p.mode).toBe(ResourceMode.Gpu);
    expect(p.cards({ auxMetered: false, pools: RESOURCES.pools })).toEqual([
      { key: 'gpu', title: 'GPUs', value: 3 },
    ]);
    expect(Object.keys(p.usage(GroupBy.Day, ENTRIES, NOW).hoursByLabel)).toEqual(['GPU']);
    expect(p.calendar(ENTRIES, NOW).activeDays).toBe(3);
    expect(usageProjection(ResourceMode.Aux, false).mode).toBe(ResourceMode.Gpu);
    expect(usageProjection(ResourceMode.Aux, true).mode).toBe(ResourceMode.Aux);
  });
});
