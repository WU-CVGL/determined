import { calendarWeeks, lastDays, usageRange } from './usage';

// Local time is America/Los_Angeles in this file. vite.config.mts runs *.tz.test.ts in the forks
// pool, where Node applies the change.
const savedTZ = process.env.TZ;
process.env.TZ = 'America/Los_Angeles';
beforeAll(() => {
  expect(new Date('2026-11-10T03:00:00Z').getTimezoneOffset()).toBe(480);
});
afterAll(() => {
  if (savedTZ === undefined) delete process.env.TZ;
  else process.env.TZ = savedTZ;
});

describe('user usage in another time zone', () => {
  it('counts the days in UTC, not in local time', () => {
    // 2026-11-09 19:00 in Los Angeles, Tuesday 2026-11-10 in UTC.
    const now = Date.parse('2026-11-10T03:00:00Z');
    expect(usageRange(now)).toEqual({ endDate: '2026-11-10', startDate: '2025-11-01' });
    const weeks = calendarWeeks([], now);
    expect(weeks[0].days[0].date).toBe('2025-11-10');
    expect(weeks.at(-1)?.days.map((day) => day.date)).toEqual(['2026-11-09', '2026-11-10']);
    expect(
      lastDays(
        [
          {
            activeDays: 0,
            activeSeconds: 0,
            cpuSlotSeconds: 0,
            gpuSeconds: 0,
            periodStart: '2026-10-10',
          },
        ],
        now,
      ),
    ).toEqual([]);
  });

  it('starts the week on the UTC Monday', () => {
    // Sunday 2026-11-15 18:00 in Los Angeles, Monday 2026-11-16 in UTC: a new week.
    const weeks = calendarWeeks([], Date.parse('2026-11-16T02:00:00Z'));
    expect(weeks.at(-1)?.days.map((day) => day.date)).toEqual(['2026-11-16']);
    expect(weeks[0].days[0].date).toBe('2025-11-17');
  });
});
