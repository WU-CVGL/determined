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

const MODES = [ResourceMode.Gpu, ResourceMode.Aux, ResourceMode.Both];

/** The projection with Aux shown, or without. */
const projection = (mode: ResourceMode, auxShown = true) => usageProjection(mode, auxShown);

/** The level of each day of the fixture in the calendar. */
const levels = (mode: ResourceMode, auxShown = true) =>
  Object.fromEntries(
    projection(mode, auxShown)
      .activeDays.calendar(ENTRIES, NOW)
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

describe('the projection', () => {
  const BOTH_CARDS = [
    { key: 'gpu', title: 'GPUs', value: 3 },
    { key: 'aux', title: 'Aux containers', value: 1 },
  ];
  const DAYS = ['2026-10-20', '2026-11-03', '2026-11-04', '2026-11-05', '2026-11-06', '2026-11-07'];

  it('shows the current GPUs and Aux containers in every mode, and GPUs only without Aux', () => {
    MODES.forEach((mode) => {
      expect(projection(mode).currentResources.cards(RESOURCES)).toEqual(BOTH_CARDS);
      expect(projection(mode, false).currentResources.cards(RESOURCES)).toEqual([
        { key: 'gpu', title: 'GPUs', value: 3 },
      ]);
    });
    expect(
      projection(ResourceMode.Gpu).currentResources.cards({ auxMetered: true, pools: [] }),
    ).toEqual([
      { key: 'gpu', title: 'GPUs', value: 0 },
      { key: 'aux', title: 'Aux containers', value: 0 },
    ]);
  });

  it('shows the GPU and Aux hours of the days in the chart and the table in every mode', () => {
    MODES.forEach((mode) => {
      const view = projection(mode).usage.view(GroupBy.Day, ENTRIES, NOW);
      expect(view.time).toEqual(DAYS);
      expect(Object.keys(view.hoursByLabel)).toEqual(['GPU', 'Aux']);
      expect(view.hoursByLabel).toEqual({ Aux: [2, 0, 10, 3, 0, 0], GPU: [1, 26, 0, 3, 0, 0] });
      expect(view.columns).toEqual([
        { key: 'periodStart', numeric: false, title: 'Date' },
        { key: 'gpu', numeric: true, title: 'GPU hours' },
        { key: 'aux', numeric: true, title: 'Aux container hours' },
      ]);
      expect(view.rows).toEqual([
        { aux: '0.0', gpu: '0.0', periodStart: '2026-11-07' },
        { aux: '0.0', gpu: '0.0', periodStart: '2026-11-06' },
        { aux: '3.0', gpu: '3.0', periodStart: '2026-11-05' },
        { aux: '10.0', gpu: '0.0', periodStart: '2026-11-04' },
        { aux: '0.0', gpu: '26.0', periodStart: '2026-11-03' },
        { aux: '2.0', gpu: '1.0', periodStart: '2026-10-20' },
      ]);
    });
  });

  it('shows the GPU hours of the days only without Aux', () => {
    MODES.forEach((mode) => {
      const view = projection(mode, false).usage.view(GroupBy.Day, ENTRIES, NOW);
      expect(view.time).toEqual(DAYS);
      expect(view.hoursByLabel).toEqual({ GPU: [1, 26, 0, 3, 0, 0] });
      expect(view.columns.map((column) => column.title)).toEqual(['Date', 'GPU hours']);
      expect(view.rows.map((row) => Object.keys(row))).toEqual(
        Array(6).fill(['periodStart', 'gpu']),
      );
      expect(view.rows.map((row) => row.gpu)).toEqual(['0.0', '0.0', '3.0', '0.0', '26.0', '1.0']);
    });
  });

  it("sums the months and counts each month's days with GPUs or Aux containers in every mode", () => {
    MODES.forEach((mode) => {
      const view = projection(mode).usage.view(GroupBy.Month, ENTRIES, NOW);
      expect(view.time).toEqual(['2025-11', '2026-10', '2026-11']);
      expect(view.hoursByLabel).toEqual({ Aux: [0, 2, 13], GPU: [4, 1, 29] });
      expect(view.columns.map((column) => [column.key, column.title])).toEqual([
        ['periodStart', 'Month'],
        ['gpu', 'GPU hours'],
        ['aux', 'Aux container hours'],
        ['activeDays', 'Active days'],
      ]);
      // November 2026: the GPU day, the Aux day and the day with both, not 2 + 2.
      expect(view.rows).toEqual([
        { activeDays: '3', aux: '13.0', gpu: '29.0', periodStart: '2026-11' },
        { activeDays: '1', aux: '2.0', gpu: '1.0', periodStart: '2026-10' },
        { activeDays: '1', aux: '0.0', gpu: '4.0', periodStart: '2025-11' },
      ]);
    });
  });

  it("counts each month's GPU days without Aux", () => {
    MODES.forEach((mode) => {
      const view = projection(mode, false).usage.view(GroupBy.Month, ENTRIES, NOW);
      expect(view.columns.map((column) => column.title)).toEqual([
        'Month',
        'GPU hours',
        'Active days',
      ]);
      expect(view.rows).toEqual([
        { activeDays: '2', gpu: '29.0', periodStart: '2026-11' },
        { activeDays: '1', gpu: '1.0', periodStart: '2026-10' },
        { activeDays: '1', gpu: '4.0', periodStart: '2025-11' },
      ]);
    });
  });

  it('counts the active days of a month with either kind, not with the mode', () => {
    // 3 GPU days, 2 Aux days and 4 days with either.
    const month = [
      entry('2026-10-01', { ...active(2, 0, 2), gpuSeconds: 2 }),
      entry('2026-10-02', { ...active(2, 0, 2), gpuSeconds: 2 }),
      entry('2026-10-03', { ...active(1, 1, 1), auxContainerSeconds: 1, gpuSeconds: 1 }),
      entry('2026-10-04', { ...active(0, 3, 3), auxContainerSeconds: 3 }),
    ];
    const activeDays = (mode: ResourceMode, auxShown = true) =>
      projection(mode, auxShown).usage.view(GroupBy.Month, month, NOW).rows[0].activeDays;
    MODES.forEach((mode) => {
      expect(activeDays(mode)).toBe('4');
      expect(activeDays(mode, false)).toBe('3');
    });
    expect(projection(ResourceMode.Gpu).activeDays.calendar(month, NOW).activeDays).toBe(3);
    expect(projection(ResourceMode.Aux).activeDays.calendar(month, NOW).activeDays).toBe(2);
    expect(projection(ResourceMode.Both).activeDays.calendar(month, NOW).activeDays).toBe(4);
  });

  it('levels the calendar by the active time of the mode, never by CPU slots', () => {
    const gpu = {
      '2026-10-20': 1,
      '2026-11-03': 3,
      '2026-11-04': 0,
      '2026-11-05': 1,
      '2026-11-06': 0,
      '2026-11-07': 0,
    };
    expect(levels(ResourceMode.Gpu)).toEqual(gpu);
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
    // Without Aux, the calendar is GPU in every mode.
    MODES.forEach((mode) => expect(levels(mode, false)).toEqual(gpu));
  });

  it('counts the active days of the shown grid only', () => {
    // 2025-11-05 is in the request, before the grid.
    expect(projection(ResourceMode.Gpu).activeDays.calendar(ENTRIES, NOW).activeDays).toBe(3);
    expect(projection(ResourceMode.Aux).activeDays.calendar(ENTRIES, NOW).activeDays).toBe(3);
    expect(projection(ResourceMode.Both).activeDays.calendar(ENTRIES, NOW).activeDays).toBe(4);
    expect(projection(ResourceMode.Both).activeDays.calendar([], NOW).activeDays).toBe(0);
    expect(projection(ResourceMode.Aux, false).activeDays.calendar(ENTRIES, NOW).activeDays).toBe(
      3,
    );
  });

  it("shows a day's values of the mode", () => {
    const day = { date: '2026-11-05', entry: ENTRIES[4], level: 1 as const };
    expect(projection(ResourceMode.Both).activeDays.day(day)).toEqual([
      { title: 'GPU hours', value: '3.0' },
      { title: 'Aux container hours', value: '3.0' },
    ]);
    expect(projection(ResourceMode.Gpu).activeDays.day(day)).toEqual([
      { title: 'GPU hours', value: '3.0' },
    ]);
    expect(projection(ResourceMode.Aux).activeDays.day(day)).toEqual([
      { title: 'Aux container hours', value: '3.0' },
    ]);
    expect(projection(ResourceMode.Both, false).activeDays.day(day)).toEqual([
      { title: 'GPU hours', value: '3.0' },
    ]);
    // The CPU day shows no CPU slots.
    expect(
      projection(ResourceMode.Both).activeDays.day({
        date: '2026-11-06',
        entry: ENTRIES[5],
        level: 0,
      }),
    ).toEqual([
      { title: 'GPU hours', value: '0.0' },
      { title: 'Aux container hours', value: '0.0' },
    ]);
    // A day without records.
    expect(projection(ResourceMode.Both).activeDays.day({ date: '2026-10-31', level: 0 })).toEqual(
      [],
    );
  });

  it('offers the three modes with Aux, and none without', () => {
    MODES.forEach((mode) => {
      expect(projection(mode).activeDays.mode).toBe(mode);
      expect(projection(mode).activeDays.modes).toEqual([
        { id: ResourceMode.Gpu, label: 'GPU' },
        { id: ResourceMode.Aux, label: 'Aux' },
        { id: ResourceMode.Both, label: 'GPU+Aux' },
      ]);
      expect(projection(mode, false).activeDays.mode).toBe(ResourceMode.Gpu);
      expect(projection(mode, false).activeDays.modes).toEqual([]);
    });
  });

  it('changes the Active days section only with the mode', () => {
    const [gpu, aux, both] = MODES.map((mode) => projection(mode));
    expect(aux.currentResources).toBe(gpu.currentResources);
    expect(both.currentResources).toBe(gpu.currentResources);
    expect(aux.usage).toBe(gpu.usage);
    expect(both.usage).toBe(gpu.usage);
    expect(new Set([gpu.activeDays, aux.activeDays, both.activeDays]).size).toBe(3);
    // The same parts every time.
    expect(projection(ResourceMode.Aux)).toEqual(aux);
    expect(projection(ResourceMode.Aux).activeDays).toBe(aux.activeDays);
    const [gpuOnly, auxOnly] = [ResourceMode.Gpu, ResourceMode.Aux].map((mode) =>
      projection(mode, false),
    );
    expect(auxOnly.activeDays).toBe(gpuOnly.activeDays);
    expect(auxOnly.usage).toBe(gpuOnly.usage);
    expect(gpuOnly.usage).not.toBe(gpu.usage);
  });

  it('shows the Aux hours whatever the cluster meters now', () => {
    const p = projection(ResourceMode.Both);
    const day = entry('2026-11-09', { ...active(0, 2, 2), auxContainerSeconds: 2 });
    expect(day.auxContainerSeconds).toBe(7200);
    expect(
      p.currentResources
        .cards({ auxMetered: false, pools: RESOURCES.pools })
        .map((card) => card.title),
    ).toEqual(['GPUs', 'Aux containers']);
    expect(p.activeDays.day({ date: '2026-11-09', entry: day, level: 1 })).toEqual([
      { title: 'GPU hours', value: '0.0' },
      { title: 'Aux container hours', value: '2.0' },
    ]);
  });
});
