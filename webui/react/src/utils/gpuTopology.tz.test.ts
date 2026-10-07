import { V1GpuHealth, V1GpuInfo, V1GpuXid } from 'services/api-ts-sdk';

import { recentXidTexts, xidWindowText } from './gpuTopology';

// Local time is America/New_York in this file. vite.config.mts runs *.tz.test.ts in the forks pool,
// where Node applies the change.
const savedTZ = process.env.TZ;
process.env.TZ = 'America/New_York';
beforeAll(() => {
  // Daylight saving time ends at 06:00 UTC on 2026-11-01: 01:00 to 02:00 happens twice.
  expect(new Date('2026-11-01T05:30:00Z').getTimezoneOffset()).toBe(240);
  expect(new Date('2026-11-01T06:30:00Z').getTimezoneOffset()).toBe(300);
});
afterAll(() => {
  if (savedTZ === undefined) delete process.env.TZ;
  else process.env.TZ = savedTZ;
});

const gpu = (...recentXids: V1GpuXid[]): V1GpuInfo => ({
  deviceId: 0,
  excluded: false,
  health: V1GpuHealth.ERROR,
  numaNode: 0,
  nvmlError: '',
  pciBusId: '0000:81:00.0',
  pcieLinkGen: 4,
  pcieLinkGenMax: 4,
  pcieLinkWidth: 16,
  pcieLinkWidthMax: 16,
  recentXids,
  uuid: 'GPU-a',
});

describe('recent critical XIDs in a fixed time zone', () => {
  it('keeps the windows of the repeated hour apart at the end of daylight saving time', () => {
    // 01:25–01:30 EDT and 01:25–01:30 EST are an hour apart: never one window.
    expect(
      recentXidTexts(
        gpu({
          firstObserved: '2026-11-01T05:30:00Z',
          lastObserved: '2026-11-01T06:30:00Z',
          xid: 79,
        }),
      ),
    ).toEqual(['79 (2026-11-01, 01:25–01:30 -04:00 to 2026-11-01, 01:25–01:30 -05:00)']);
    // A window across the change shows both offsets, never "01:55–01:00".
    expect(xidWindowText('2026-11-01T06:00:00Z')).toBe('2026-11-01, 01:55 -04:00–01:00 -05:00');
    expect(
      recentXidTexts(
        gpu({
          firstObserved: '2026-11-01T06:00:00Z',
          lastObserved: '2026-11-01T06:30:00Z',
          xid: 48,
        }),
      ),
    ).toEqual(['48 (2026-11-01, 01:55 -04:00–01:00 -05:00 to 2026-11-01, 01:25–01:30 -05:00)']);
    // Away from the change, no offset.
    expect(
      recentXidTexts(
        gpu({
          firstObserved: '2026-11-01T08:00:00Z',
          lastObserved: '2026-11-01T08:10:00Z',
          xid: 94,
        }),
      ),
    ).toEqual(['94 (2026-11-01, 02:55–03:00 to 2026-11-01, 03:05–03:10)']);
  });

  it('shows the date of a window end on another date', () => {
    // 2026-10-08T04:00:00Z is midnight in New York.
    expect(xidWindowText('2026-10-08T04:00:00Z')).toBe('2026-10-07, 23:55–2026-10-08, 00:00');
    expect(
      recentXidTexts(
        gpu({
          firstObserved: '2026-10-08T04:00:00Z',
          lastObserved: '2026-10-08T04:10:00Z',
          xid: 79,
        }),
      ),
    ).toEqual(['79 (2026-10-07, 23:55–2026-10-08, 00:00 to 2026-10-08, 00:05–00:10)']);
    expect(xidWindowText('2026-10-08T04:05:00Z')).toBe('2026-10-08, 00:00–00:05');
  });
});
