import { describe, expect, it } from 'vitest';

import {
  alignResourceSeries,
  parseResourceAllocations,
  RESOURCE_MAX_SPAN,
  resourceRange,
  ResourceSeries,
  sinceStartBounds,
  sinceStartWindow,
} from './taskResources';

describe('task resource sampling', () => {
  it('keeps missing samples and non-finite values as gaps, while preserving true zero', () => {
    const series: ResourceSeries[] = [
      {
        labels: { allocation_id: 'retry.1' },
        metric: 'cpu_cores',
        samples: [
          [100, 0],
          [130, 2],
          [145, null],
          [160, NaN],
          [175, Infinity],
        ],
      },
    ];
    expect(alignResourceSeries(series, { end: 175, start: 100, step: 15 })).toEqual([
      [100, 115, 130, 145, 160, 175],
      [0, null, 2, null, null, null],
    ]);
  });

  it('does not connect separate allocation lifetimes', () => {
    const series: ResourceSeries[] = [
      { labels: { allocation_id: 'run.1' }, metric: 'cpu_cores', samples: [[100, 1]] },
      { labels: { allocation_id: 'run.2' }, metric: 'cpu_cores', samples: [[130, 3]] },
    ];
    expect(alignResourceSeries(series, { end: 130, start: 100, step: 15 })).toEqual([
      [100, 115, 130],
      [1, null, null],
      [null, null, 3],
    ]);
  });

  it('keeps seven-day requests within the server point budget including both endpoints', () => {
    const range = resourceRange(100, 100 + 7 * 86400);
    expect(Math.floor((range.end - range.start) / range.step) + 1).toBeLessThanOrEqual(1440);
    expect(resourceRange(100, 101).step).toBe(15);
  });

  it('meets the server step and point limits for every span up to 7 days', () => {
    // Mirrors parseTaskResourceRange: 15 <= step <= 86400 and (end - start) / step + 1 <= 1440.
    const end = 2_000_000_000;
    for (let span = 1; span <= RESOURCE_MAX_SPAN; span++) {
      const range = resourceRange(end - span, end);
      const ok =
        range.end - range.start === span &&
        range.step >= 15 &&
        range.step <= 86400 &&
        Math.floor((range.end - range.start) / range.step) + 1 <= 1440;
      if (!ok) throw new Error(`span ${span} gives step ${range.step}`);
    }
    expect(resourceRange(end - 1439 * 15, end).step).toBe(15);
    expect(resourceRange(end - 1439 * 15 - 1, end).step).toBe(16);
    expect(resourceRange(end - RESOURCE_MAX_SPAN, end).step).toBe(421);
    // Fractional bounds are floored before the step is chosen.
    const fractional = resourceRange(end - RESOURCE_MAX_SPAN + 0.5, end + 0.9);
    expect(fractional.end - fractional.start).toBe(RESOURCE_MAX_SPAN);
    expect(fractional.step).toBe(421);
    expect(
      Math.floor((fractional.end - fractional.start) / fractional.step) + 1,
    ).toBeLessThanOrEqual(1440);
  });
});

describe('since start', () => {
  it('reads the allocation list and rejects other shapes', () => {
    expect(
      parseResourceAllocations({
        allocations: [
          { allocation_id: 'a', container_start: '2026-10-01T08:00:00Z', end: null },
          { allocation_id: 'b', container_start: null, end: null },
          { allocation_id: 'c', container_start: 'not a time', end: '2026-10-01T09:00:00.5Z' },
        ],
      }),
    ).toEqual([
      { allocationId: 'a', containerStart: 1790841600, end: undefined },
      { allocationId: 'b', containerStart: undefined, end: undefined },
      { allocationId: 'c', containerStart: undefined, end: 1790845200 },
    ]);
    expect(parseResourceAllocations({ allocations: [] })).toEqual([]);
    for (const invalid of [undefined, null, '<html>', {}, { allocations: [{}] }])
      expect(parseResourceAllocations(invalid)).toBeUndefined();
  });

  it('starts at the earliest container start, or one allocation, or the task start', () => {
    const list = [
      { allocationId: 'b', containerStart: 300 },
      { allocationId: 'a', containerStart: 200, end: 250 },
      { allocationId: 'c' },
    ];
    expect(sinceStartBounds(list, '', 100)).toEqual({ start: 200 });
    expect(sinceStartBounds(list, 'a', 100)).toEqual({ end: 250, start: 200 });
    expect(sinceStartBounds(list, 'b', 100)).toEqual({ end: undefined, start: 300 });
    expect(sinceStartBounds(list, 'c', 100)).toEqual({ noContainerStart: true, start: 100 });
    expect(sinceStartBounds([{ allocationId: 'c' }], '', 100)).toEqual({
      noContainerStart: true,
      start: 100,
    });
    expect(sinceStartBounds(list, 'unknown', 100)).toEqual({ start: 100 });
    expect(sinceStartBounds(null, 'a', 100)).toEqual({ start: 100 });
  });

  it('ends at the allocation end or now and keeps the most recent 7 days', () => {
    expect(sinceStartWindow({ end: 250, start: 200 }, 1000)).toEqual({
      clamped: false,
      end: 250,
      start: 200,
    });
    expect(sinceStartWindow({ start: 200 }, 1000)).toEqual({
      clamped: false,
      end: 1000,
      start: 200,
    });
    const now = 10 * 86400;
    expect(sinceStartWindow({ start: now - RESOURCE_MAX_SPAN }, now)).toEqual({
      clamped: false,
      end: now,
      start: now - RESOURCE_MAX_SPAN,
    });
    expect(sinceStartWindow({ start: now - RESOURCE_MAX_SPAN - 1 }, now)).toEqual({
      clamped: true,
      end: now,
      start: now - RESOURCE_MAX_SPAN,
    });
    // A container that started this second still yields a valid one-second range.
    expect(sinceStartWindow({ start: now }, now)).toEqual({
      clamped: false,
      end: now,
      start: now - 1,
    });
  });
});
