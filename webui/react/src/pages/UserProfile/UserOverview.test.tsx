import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import React from 'react';
import { MemoryRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { getUserResources, getUserUsage } from 'services/api';
import { UserResources, UserUsage, UserUsageEntry } from 'types';
import handleError from 'utils/error';

import UserOverview, { RESOURCES_POLL_INTERVAL } from './UserOverview';
import { USAGE_REFRESH_INTERVAL, USAGE_STALE_AFTER } from './useUserUsage';

vi.mock('services/api', () => ({
  getUserResources: vi.fn(),
  getUserUsage: vi.fn(),
}));

vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: vi.fn(),
}));

/** The renders of the cards and of the history blocks. */
const renders = vi.hoisted(() => ({ calendar: 0, cards: 0, chart: 0, table: 0 }));

vi.mock('components/OverviewStats', async (importOriginal) => {
  const { default: OverviewStats } =
    await importOriginal<typeof import('components/OverviewStats')>();
  return {
    default: (props: React.ComponentProps<typeof OverviewStats>) => {
      renders.cards++;
      return <OverviewStats {...props} />;
    },
  };
});

/* The chart draws on a canvas; the stub shows what it was given. */
vi.mock('pages/Cluster/ClusterHistoricalUsageChart', () => ({
  default: (props: { hoursByLabel: Record<string, number[]>; label?: string; time: string[] }) => {
    renders.chart++;
    return (
      <div data-testid="usage-chart">
        <span data-testid="chart-series">{Object.keys(props.hoursByLabel).join(',')}</span>
        <span data-testid="chart-time">{props.time.join(',')}</span>
        <span data-testid="chart-label">{props.label}</span>
        <span data-testid="chart-values">{JSON.stringify(props.hoursByLabel)}</span>
      </div>
    );
  },
}));

vi.mock('./UsageCalendar', async (importOriginal) => {
  const { default: UsageCalendar } = await importOriginal<typeof import('./UsageCalendar')>();
  return {
    default: (props: React.ComponentProps<typeof UsageCalendar>) => {
      renders.calendar++;
      return <UsageCalendar {...props} />;
    },
  };
});

vi.mock('components/Table/ResponsiveTable', async (importOriginal) => {
  const { default: ResponsiveTable } =
    await importOriginal<typeof import('components/Table/ResponsiveTable')>();
  return {
    default: (props: React.ComponentProps<typeof ResponsiveTable>) => {
      renders.table++;
      return <ResponsiveTable {...props} />;
    },
  };
});

// A Tuesday: today is 2026-11-10 (UTC).
const NOW = new Date('2026-11-10T15:00:00Z');
const HOUR = 3600;

const RESOURCES: UserResources = {
  auxMetered: true,
  pools: [
    { counts: { auxContainers: 1, cpuSlots: 0, gpus: 2 }, resourcePool: 'default' },
    { counts: { auxContainers: 0, cpuSlots: 0, gpus: 1 }, resourcePool: 'other' },
  ],
};

/**
 * A day with GPU and Aux hours, the two at the same time, and CPU slots on days without either.
 */
const day = (periodStart: string, gpuHours: number, auxHours = 0): UserUsageEntry => ({
  auxActiveDays: auxHours > 0 ? 1 : 0,
  auxActiveSeconds: auxHours * HOUR,
  auxContainerSeconds: auxHours * HOUR,
  cpuSlotSeconds: gpuHours > 0 || auxHours > 0 ? 0 : 13 * HOUR,
  gpuActiveDays: gpuHours > 0 ? 1 : 0,
  gpuActiveSeconds: gpuHours * HOUR,
  gpuOrAuxActiveDays: gpuHours > 0 || auxHours > 0 ? 1 : 0,
  gpuOrAuxActiveSeconds: Math.max(gpuHours, auxHours) * HOUR,
  gpuSeconds: gpuHours * HOUR,
  periodStart,
  unclassifiedSlotSeconds: 0,
});

/** Covered daily from 2025-11-01 to a day, with the GPU and Aux hours of each day. */
const coveredUsage = (
  hours: (date: string) => [number, number?],
  last: string = '2026-11-10',
): UserUsage => {
  const entries: UserUsageEntry[] = [];
  for (let t = Date.parse('2025-11-01T00:00:00Z'); t <= Date.parse(last); t += 24 * HOUR * 1000) {
    const date = new Date(t).toISOString().slice(0, 10);
    entries.push(day(date, ...hours(date)));
  }
  return { coverageStart: '2025-11-01', entries };
};

/**
 * In November: GPUs on 11-09, 11-07 and 11-06, Aux containers on 11-08 and 11-07: 3 GPU days, 2
 * Aux days and 4 days with either.
 */
const MIXED = coveredUsage((date) =>
  date === '2026-11-09'
    ? [6]
    : date === '2026-11-08'
      ? [0, 13]
      : date === '2026-11-07'
        ? [2, 5]
        : date === '2026-11-06'
          ? [1]
          : [0],
);

/** GPU hours on two days. */
const USAGE = coveredUsage((date) => [date === '2026-11-09' ? 6 : date === '2026-10-02' ? 2 : 0]);

const Wrapper: React.FC<{ children?: React.ReactNode }> = ({ children }) => (
  <UIProvider theme={DefaultTheme.Light}>
    <ThemeProvider>
      <MemoryRouter>{children}</MemoryRouter>
    </ThemeProvider>
  </UIProvider>
);

const setup = (userId = 4) => render(<UserOverview userId={userId} />, { wrapper: Wrapper });

const section = (id: string) => document.querySelector(`section#${id}`) as HTMLElement;
/** The mode control of the Active days section. */
const modes = () => within(section('active-days')).queryAllByRole('radio');
const mode = (name: string) => within(section('active-days')).getByRole('radio', { name });
const stat = (title: string) => screen.getByText(title).parentElement?.parentElement as HTMLElement;
const columns = () => screen.getAllByRole('columnheader').map((header) => header.textContent);
const series = () => screen.getByTestId('chart-series').textContent;
const cardTitles = () =>
  ['GPUs', 'Aux containers'].filter((title) =>
    within(section('current-resources')).queryByText(title),
  );
const level = (date: string) => screen.getByLabelText(date).className.match(/level\d/)?.[0];
const dayLine = () => screen.getByTestId('usage-calendar-day');
/** The control of the table under the chart. */
const details = () =>
  within(document.querySelector('section#usage') as HTMLElement).getByRole('button', {
    name: /Details/,
  });
const openDetails = async (user: ReturnType<typeof userEvent.setup>) => {
  await act(() => user.click(details()));
  expect(details()).toHaveAttribute('aria-expanded', 'true');
};

let hidden = false;

const setHidden = (value: boolean) => {
  hidden = value;
  act(() => {
    document.dispatchEvent(new Event('visibilitychange'));
  });
};

function deferred<T>() {
  let resolve: (value: T) => void = () => undefined;
  let reject: (e: Error) => void = () => undefined;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, reject, resolve };
}

describe('UserOverview', () => {
  let visibility: { mockRestore: () => void };

  beforeEach(() => {
    vi.useFakeTimers({ now: NOW, shouldAdvanceTime: true });
    hidden = false;
    visibility = vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
    vi.mocked(getUserResources).mockResolvedValue(RESOURCES);
    vi.mocked(getUserUsage).mockResolvedValue(USAGE);
  });

  afterEach(() => {
    Object.assign(renders, { calendar: 0, cards: 0, chart: 0, table: 0 });
    visibility.mockRestore();
    vi.useRealTimers();
    vi.clearAllMocks();
  });

  it('shows the GPUs and the Aux containers the user holds now', async () => {
    setup();
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('3'));
    expect(stat('Aux containers')).toHaveTextContent('1');
    expect(screen.queryByText('CPU slots')).not.toBeInTheDocument();
    expect(getUserResources).toHaveBeenCalledWith(
      { userId: 4 },
      { signal: expect.any(AbortSignal) },
    );
    expect(mode('GPU+Aux')).toBeChecked();
  });

  it('puts the mode control in the Active days section, GPU, Aux and GPU+Aux', async () => {
    setup();
    expect(await screen.findByText('Active days · 2')).toBeInTheDocument();
    expect(modes().map((radio) => radio.closest('label')?.textContent)).toEqual([
      'GPU',
      'Aux',
      'GPU+Aux',
    ]);
    expect(mode('GPU+Aux')).toBeChecked();
    // Day and Month are the other radios.
    expect(
      within(section('current-resources')).queryByRole('radio', { name: 'GPU' }),
    ).not.toBeInTheDocument();
    expect(within(section('usage')).getAllByRole('radio')).toHaveLength(2);
  });

  it('changes the calendar, its count and the day line with the mode, and not the cards, the chart or the table', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    vi.mocked(getUserUsage).mockResolvedValue(MIXED);
    setup();
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('3'));
    expect(await screen.findByText('Active days · 4')).toBeInTheDocument();
    await act(() => user.click(screen.getByLabelText('2026-11-07')));
    await openDetails(user);
    const days = ['2026-11-09', '2026-11-08', '2026-11-07', '2026-11-06'];
    const others = () => ({
      aux: stat('Aux containers').textContent,
      cards: cardTitles(),
      columns: columns(),
      gpus: stat('GPUs').textContent,
      rows: screen.getAllByRole('row').map((row) => row.textContent),
      series: series(),
      values: screen.getByTestId('chart-values').textContent,
    });

    expect(days.map(level)).toEqual(['level2', 'level3', 'level2', 'level1']);
    expect(dayLine()).toHaveTextContent(/^2026-11-07 \(UTC\)GPU hours 2.0Aux container hours 5.0$/);
    const shown = others();
    expect(shown.cards).toEqual(['GPUs', 'Aux containers']);
    expect(shown.series).toBe('GPU,Aux');
    expect(shown.columns).toEqual(['Date', 'GPU hours', 'Aux container hours']);

    await act(() => user.click(mode('GPU')));
    expect(mode('GPU')).toBeChecked();
    expect(screen.getByText('Active days · 3')).toBeInTheDocument();
    expect(days.map(level)).toEqual(['level2', 'level0', 'level1', 'level1']);
    expect(dayLine()).toHaveTextContent(/^2026-11-07 \(UTC\)GPU hours 2.0$/);
    expect(others()).toEqual(shown);

    await act(() => user.click(mode('Aux')));
    expect(screen.getByText('Active days · 2')).toBeInTheDocument();
    expect(days.map(level)).toEqual(['level0', 'level3', 'level2', 'level0']);
    expect(dayLine()).toHaveTextContent(/^2026-11-07 \(UTC\)Aux container hours 5.0$/);
    expect(others()).toEqual(shown);
    expect(details()).toHaveAttribute('aria-expanded', 'true');

    await act(() => user.click(mode('GPU+Aux')));
    expect(screen.getByText('Active days · 4')).toBeInTheDocument();
    expect(others()).toEqual(shown);
    expect(getUserUsage).toHaveBeenCalledTimes(1);
  });

  it("counts each month's days with GPUs or Aux containers in every mode", async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    vi.mocked(getUserUsage).mockResolvedValue(MIXED);
    setup();
    expect(await screen.findByText('Active days · 4')).toBeInTheDocument();
    await user.click(screen.getByRole('radio', { name: 'Month' }));
    await openDetails(user);
    expect(columns()).toEqual(['Month', 'GPU hours', 'Aux container hours', 'Active days']);
    const november = () =>
      within(screen.getAllByRole('row')[1])
        .getAllByRole('cell')
        .map((cell) => cell.textContent);
    expect(november()).toEqual(['2026-11', '9.0', '18.0', '4']);
    for (const [name, count] of [
      ['GPU', 3],
      ['Aux', 2],
    ] as const) {
      await act(() => user.click(mode(name)));
      expect(screen.getByText(`Active days · ${count}`)).toBeInTheDocument();
      expect(november()).toEqual(['2026-11', '9.0', '18.0', '4']);
    }
  });

  it('shows GPUs only, without the mode control, where the cluster does not meter Aux and the usage has none', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    vi.mocked(getUserResources).mockResolvedValue({
      auxMetered: false,
      pools: [{ counts: { auxContainers: 0, cpuSlots: 0, gpus: 8 }, resourcePool: 'k8s' }],
    });
    setup();
    await waitFor(() => expect(screen.getByTestId('chart-series')).toHaveTextContent(/^GPU$/));
    expect(stat('GPUs')).toHaveTextContent('8');
    expect(cardTitles()).toEqual(['GPUs']);
    expect(screen.queryAllByRole('radio', { name: /GPU|Aux/ })).toHaveLength(0);
    expect(modes()).toHaveLength(0);
    expect(screen.getByText('Active days · 2')).toBeInTheDocument();
    await openDetails(user);
    expect(columns()).toEqual(['Date', 'GPU hours']);
    await act(() => user.click(screen.getByLabelText('2026-11-09')));
    expect(dayLine()).toHaveTextContent(/^2026-11-09 \(UTC\)GPU hours 6.0$/);
    await user.click(screen.getByRole('radio', { name: 'Month' }));
    await waitFor(() => expect(columns()).toEqual(['Month', 'GPU hours', 'Active days']));
    expect(within(screen.getAllByRole('row')[1]).getAllByRole('cell')[2]).toHaveTextContent(/^1$/);
  });

  it('selects GPU once Aux is known to be absent, and keeps it when Aux comes back', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    vi.mocked(getUserResources).mockResolvedValue({
      auxMetered: false,
      pools: [{ counts: { auxContainers: 0, cpuSlots: 0, gpus: 8 }, resourcePool: 'k8s' }],
    });
    const usage = deferred<UserUsage>();
    vi.mocked(getUserUsage).mockReturnValue(usage.promise);
    setup();
    // Unknown while the usage loads: both kinds and the control.
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('8'));
    expect(cardTitles()).toEqual(['GPUs', 'Aux containers']);
    await act(() => user.click(mode('Aux')));
    expect(mode('Aux')).toBeChecked();

    await act(() => {
      usage.resolve(USAGE);
      return Promise.resolve();
    });
    expect(await screen.findByTestId('chart-series')).toHaveTextContent(/^GPU$/);
    expect(modes()).toHaveLength(0);
    expect(cardTitles()).toEqual(['GPUs']);
    expect(screen.getByText('Active days · 2')).toBeInTheDocument();
    await openDetails(user);

    vi.mocked(getUserResources).mockResolvedValue({
      auxMetered: true,
      pools: [{ counts: { auxContainers: 0, cpuSlots: 0, gpus: 8 }, resourcePool: 'default' }],
    });
    await act(() => vi.advanceTimersByTimeAsync(RESOURCES_POLL_INTERVAL));
    // The control comes back in GPU, not in the Aux mode chosen before.
    await waitFor(() => expect(mode('GPU')).toBeChecked());
    expect(mode('Aux')).not.toBeChecked();
    await act(() => user.click(screen.getByLabelText('2026-11-09')));
    expect(dayLine()).toHaveTextContent(/^2026-11-09 \(UTC\)GPU hours 6.0$/);
    expect(cardTitles()).toEqual(['GPUs', 'Aux containers']);
    expect(series()).toBe('GPU,Aux');
    expect(columns()).toEqual(['Date', 'GPU hours', 'Aux container hours']);
    expect(details()).toHaveAttribute('aria-expanded', 'true');
  });

  it('shows the Aux containers the user holds in a pool the viewer may not see', async () => {
    vi.mocked(getUserResources).mockResolvedValue({
      auxMetered: true,
      pools: [{ counts: { auxContainers: 2, cpuSlots: 0, gpus: 0 }, resourcePool: 'hidden' }],
    });
    setup();
    await waitFor(() => expect(stat('Aux containers')).toHaveTextContent('2'));
  });

  it('shows the usage in GPU+Aux before the current resources answer', async () => {
    vi.mocked(getUserResources).mockReturnValue(new Promise(() => undefined));
    setup();
    expect(await screen.findByTestId('chart-series')).toHaveTextContent('GPU,Aux');
    expect(screen.getByTestId('usage-calendar')).toBeInTheDocument();
    expect(mode('GPU+Aux')).toBeChecked();
    expect(screen.queryByText('GPUs')).not.toBeInTheDocument();
  });

  it('shows the Aux usage and the mode control when the current resources fail', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    vi.mocked(getUserResources).mockRejectedValue(new Error('offline'));
    vi.mocked(getUserUsage).mockResolvedValue(
      coveredUsage((date) => (date === '2026-11-09' ? [1, 3] : [0])),
    );
    setup();
    expect(await screen.findByTestId('chart-series')).toHaveTextContent('GPU,Aux');
    expect(screen.queryByText('GPUs')).not.toBeInTheDocument();
    expect(screen.queryByText('Aux containers')).not.toBeInTheDocument();
    await act(() => user.click(mode('Aux')));
    expect(series()).toBe('GPU,Aux');
    expect(screen.getByText('Active days · 1')).toBeInTheDocument();
    await act(() => user.click(screen.getByLabelText('2026-11-09')));
    expect(dayLine()).toHaveTextContent(/^2026-11-09 \(UTC\)Aux container hours 3.0$/);
  });

  it('keeps the mode control when the current resources fail and the usage has no Aux', async () => {
    vi.mocked(getUserResources).mockRejectedValue(new Error('offline'));
    setup();
    expect(await screen.findByTestId('chart-series')).toHaveTextContent('GPU,Aux');
    await waitFor(() => expect(getUserResources).toHaveBeenCalled());
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(mode('GPU+Aux')).toBeChecked();
    expect(series()).toBe('GPU,Aux');
  });

  it('keeps the Aux modes for the Aux usage where the cluster no longer meters Aux', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    vi.mocked(getUserResources).mockResolvedValue({
      auxMetered: false,
      pools: [{ counts: { auxContainers: 0, cpuSlots: 0, gpus: 8 }, resourcePool: 'k8s' }],
    });
    // 7200 Aux container seconds on 11-09.
    vi.mocked(getUserUsage).mockResolvedValue(
      coveredUsage((date) => (date === '2026-11-09' ? [0, 2] : [0])),
    );
    setup();
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('8'));
    expect(stat('Aux containers')).toHaveTextContent('0');
    expect(mode('GPU+Aux')).toBeChecked();
    expect(await screen.findByTestId('chart-series')).toHaveTextContent('GPU,Aux');
    await act(() => user.click(screen.getByLabelText('2026-11-09')));
    expect(screen.getByTestId('usage-calendar-day')).toHaveTextContent(
      /^2026-11-09 \(UTC\)GPU hours 0.0Aux container hours 2.0$/,
    );
    expect(screen.getByText('Active days · 1')).toBeInTheDocument();
  });

  it('reads the usage daily, from the first month twelve months back to today', async () => {
    setup();
    expect(await screen.findByTestId('usage-calendar')).toBeInTheDocument();
    expect(getUserUsage).toHaveBeenCalledTimes(1);
    expect(vi.mocked(getUserUsage).mock.calls[0][0]).toEqual({
      endDate: '2026-11-10',
      period: 'RESOURCE_ALLOCATION_AGGREGATION_PERIOD_DAILY',
      startDate: '2025-11-01',
      userId: 4,
    });
    expect(screen.getByLabelText('2026-11-09')).toHaveClass('level2');
    // The other days hold CPU slots only.
    expect(screen.getByLabelText('2026-11-08')).toHaveClass('level0');
    expect(screen.getByText('Active days · 2')).toBeInTheDocument();
  });

  it('switches between days and months without another request', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    setup();
    expect(await screen.findByTestId('chart-series')).toHaveTextContent('GPU,Aux');
    expect(screen.getByTestId('chart-label')).toHaveTextContent('Hours');
    const days = screen.getByTestId('chart-time').textContent?.split(',');
    expect(days).toHaveLength(31);
    expect(days?.[0]).toBe('2026-10-11');
    expect(days?.at(-1)).toBe('2026-11-10');
    await openDetails(user);
    expect(screen.getByRole('columnheader', { name: 'Date' })).toBeInTheDocument();
    // The table lists the newest day first.
    const firstRow = () => screen.getAllByRole('row')[1];
    expect(within(firstRow()).getAllByRole('cell')[0]).toHaveTextContent('2026-11-10');
    expect(within(screen.getAllByRole('row')[2]).getAllByRole('cell')[1]).toHaveTextContent('6.0');

    await user.click(screen.getByRole('radio', { name: 'Month' }));
    await waitFor(() =>
      expect(screen.getByRole('columnheader', { name: 'Month' })).toBeInTheDocument(),
    );
    const months = screen.getByTestId('chart-time').textContent?.split(',');
    expect(months).toEqual([
      '2025-11',
      '2025-12',
      '2026-01',
      '2026-02',
      '2026-03',
      '2026-04',
      '2026-05',
      '2026-06',
      '2026-07',
      '2026-08',
      '2026-09',
      '2026-10',
      '2026-11',
    ]);
    expect(within(firstRow()).getAllByRole('cell')[0]).toHaveTextContent('2026-11');
    expect(within(firstRow()).getAllByRole('cell')[1]).toHaveTextContent('6.0');
    expect(within(screen.getAllByRole('row')[2]).getAllByRole('cell')[1]).toHaveTextContent('2.0');
    expect(screen.getByRole('columnheader', { name: 'Active days' })).toBeInTheDocument();
    expect(within(firstRow()).getAllByRole('cell')[3]).toHaveTextContent(/^1$/);

    await user.click(screen.getByRole('radio', { name: 'Day' }));
    await waitFor(() =>
      expect(screen.getByRole('columnheader', { name: 'Date' })).toBeInTheDocument(),
    );
    expect(getUserUsage).toHaveBeenCalledTimes(1);
  });

  it('shows zeros and flat lines without any usage', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    vi.mocked(getUserResources).mockResolvedValue({ auxMetered: true, pools: [] });
    vi.mocked(getUserUsage).mockResolvedValue(coveredUsage(() => [0]));
    setup();
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('0'));
    expect(stat('Aux containers')).toHaveTextContent('0');
    expect(await screen.findByTestId('usage-calendar')).toBeInTheDocument();
    expect(screen.getByLabelText('2026-11-10')).toHaveClass('level0');
    expect(screen.getByLabelText('2026-11-10')).not.toHaveClass('notCovered');
    expect(screen.getByLabelText('2025-11-10')).toHaveClass('level0');
    expect(screen.getByText('Active days · 0')).toBeInTheDocument();
    expect(screen.getByTestId('chart-time').textContent?.split(',')).toHaveLength(31);
    const values = JSON.parse(screen.getByTestId('chart-values').textContent ?? '{}');
    expect(values.GPU).toEqual(Array(31).fill(0));
    expect(values.Aux).toEqual(Array(31).fill(0));
    await openDetails(user);
    expect(within(screen.getAllByRole('row')[1]).getAllByRole('cell')[1]).toHaveTextContent('0.0');
  });

  it('outlines the calendar when the cluster has no records', async () => {
    vi.mocked(getUserUsage).mockResolvedValue({ coverageStart: undefined, entries: [] });
    setup();
    expect(await screen.findByTestId('usage-calendar')).toBeInTheDocument();
    expect(screen.getByLabelText('2026-11-10')).toHaveClass('notCovered');
    expect(screen.getByTestId('chart-series')).toHaveTextContent('GPU');
  });

  describe('Details', () => {
    it('is closed at first, without the table', async () => {
      setup();
      expect(await screen.findByTestId('usage-chart')).toBeInTheDocument();
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(details()).toHaveAttribute('aria-expanded', 'false');
      expect(details()).toHaveTextContent(/^Details$/);
      expect(screen.queryByRole('table')).not.toBeInTheDocument();
      expect(screen.queryAllByRole('row')).toHaveLength(0);
      expect(renders.table).toBe(0);
    });

    it('lists the hours of the view when opened, and drops them when closed', async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      setup();
      expect(await screen.findByTestId('usage-chart')).toBeInTheDocument();
      await act(() => user.click(mode('GPU')));
      await user.click(screen.getByRole('radio', { name: 'Month' }));
      await waitFor(() => expect(screen.getByTestId('chart-time')).toHaveTextContent(/^2025-11,/));

      await openDetails(user);
      expect(columns()).toEqual(['Month', 'GPU hours', 'Aux container hours', 'Active days']);
      // The header and the 13 months, the newest first.
      expect(screen.getAllByRole('row')).toHaveLength(14);
      expect(within(screen.getAllByRole('row')[1]).getAllByRole('cell')[0]).toHaveTextContent(
        '2026-11',
      );

      await act(() => user.click(details()));
      expect(details()).toHaveAttribute('aria-expanded', 'false');
      await waitFor(() => expect(screen.queryAllByRole('row')).toHaveLength(0));
    });

    it('opens and closes from the keyboard', async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      setup();
      expect(await screen.findByTestId('usage-chart')).toBeInTheDocument();
      act(() => details().focus());
      await act(() => user.keyboard('{Enter}'));
      expect(details()).toHaveAttribute('aria-expanded', 'true');
      // The header and the 31 days.
      expect(screen.getAllByRole('row')).toHaveLength(32);
      await act(() => user.keyboard('{Enter}'));
      expect(details()).toHaveAttribute('aria-expanded', 'false');
      await waitFor(() => expect(screen.queryAllByRole('row')).toHaveLength(0));
    });

    it('stays open across the views, the modes and the usage refreshes', async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      setup();
      expect(await screen.findByTestId('usage-chart')).toBeInTheDocument();
      await openDetails(user);
      expect(columns()).toEqual(['Date', 'GPU hours', 'Aux container hours']);

      await user.click(screen.getByRole('radio', { name: 'Month' }));
      await waitFor(() =>
        expect(columns()).toEqual(['Month', 'GPU hours', 'Aux container hours', 'Active days']),
      );
      expect(details()).toHaveAttribute('aria-expanded', 'true');

      await act(() => user.click(mode('GPU')));
      expect(columns()).toEqual(['Month', 'GPU hours', 'Aux container hours', 'Active days']);
      expect(details()).toHaveAttribute('aria-expanded', 'true');

      vi.mocked(getUserUsage).mockResolvedValue(
        coveredUsage((date) => [date === '2026-11-09' ? 9 : 0]),
      );
      await act(() => vi.advanceTimersByTimeAsync(USAGE_REFRESH_INTERVAL));
      expect(getUserUsage).toHaveBeenCalledTimes(2);
      await waitFor(() =>
        expect(within(screen.getAllByRole('row')[1]).getAllByRole('cell')[1]).toHaveTextContent(
          '9.0',
        ),
      );
      expect(details()).toHaveAttribute('aria-expanded', 'true');

      await user.click(screen.getByRole('radio', { name: 'Day' }));
      await waitFor(() => expect(columns()).toEqual(['Date', 'GPU hours', 'Aux container hours']));
      expect(within(screen.getAllByRole('row')[2]).getAllByRole('cell')[1]).toHaveTextContent(
        '9.0',
      );
      expect(details()).toHaveAttribute('aria-expanded', 'true');
    });

    it('stays closed across the views, the modes and the usage refreshes', async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      setup();
      expect(await screen.findByTestId('usage-chart')).toBeInTheDocument();
      await user.click(screen.getByRole('radio', { name: 'Month' }));
      await waitFor(() => expect(screen.getByTestId('chart-time')).toHaveTextContent(/^2025-11,/));
      await act(() => user.click(mode('Aux')));
      expect(mode('Aux')).toBeChecked();
      await act(() => vi.advanceTimersByTimeAsync(USAGE_REFRESH_INTERVAL));
      expect(getUserUsage).toHaveBeenCalledTimes(2);
      expect(details()).toHaveAttribute('aria-expanded', 'false');
      expect(screen.queryAllByRole('row')).toHaveLength(0);
      expect(renders.table).toBe(0);
    });
  });

  it('reads the current resources every 15 s, and not while the page is hidden', async () => {
    setup();
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('3'));
    expect(getUserResources).toHaveBeenCalledTimes(1);

    vi.mocked(getUserResources).mockResolvedValue({
      auxMetered: true,
      pools: [{ counts: { auxContainers: 0, cpuSlots: 0, gpus: 0 }, resourcePool: 'default' }],
    });
    await act(() => vi.advanceTimersByTimeAsync(RESOURCES_POLL_INTERVAL));
    expect(getUserResources).toHaveBeenCalledTimes(2);
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('0'));

    setHidden(true);
    await act(() => vi.advanceTimersByTimeAsync(4 * RESOURCES_POLL_INTERVAL));
    expect(getUserResources).toHaveBeenCalledTimes(2);

    setHidden(false);
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(getUserResources).toHaveBeenCalledTimes(3);
    await act(() => vi.advanceTimersByTimeAsync(RESOURCES_POLL_INTERVAL));
    expect(getUserResources).toHaveBeenCalledTimes(4);
  });

  it('renders the calendar, the chart and the table no more for the resources polls', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    const pools = (gpus: number) => ({
      auxMetered: true,
      pools: [{ counts: { auxContainers: 0, cpuSlots: 0, gpus }, resourcePool: 'default' }],
    });
    const poll = async (counts: number[]) => {
      for (const gpus of counts) {
        vi.mocked(getUserResources).mockResolvedValue(pools(gpus));
        await act(() => vi.advanceTimersByTimeAsync(RESOURCES_POLL_INTERVAL));
        await waitFor(() => expect(stat('GPUs')).toHaveTextContent(String(gpus)));
      }
    };
    const history = () => ({
      calendar: renders.calendar,
      chart: renders.chart,
      table: renders.table,
    });
    setup();
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('3'));
    expect(await screen.findByTestId('usage-chart')).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(0));

    // With the table closed.
    const closed = history();
    expect(closed.calendar).toBeGreaterThan(0);
    expect(closed.chart).toBeGreaterThan(0);
    expect(closed.table).toBe(0);
    await poll([4, 5, 6]);
    expect(history()).toEqual(closed);

    // With the table open.
    await openDetails(user);
    await act(() => vi.advanceTimersByTimeAsync(0));
    const open = history();
    expect(open.table).toBeGreaterThan(0);
    await poll([7, 8, 9]);
    expect(getUserResources).toHaveBeenCalledTimes(7);
    expect(getUserUsage).toHaveBeenCalledTimes(1);
    expect(history()).toEqual(open);
  });

  it('renders the Active days section only for a mode switch', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    setup();
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('3'));
    expect(await screen.findByText('Active days · 2')).toBeInTheDocument();
    await openDetails(user);
    await act(() => vi.advanceTimersByTimeAsync(0));
    const before = { ...renders };
    expect(before.cards).toBeGreaterThan(0);
    expect(before.table).toBeGreaterThan(0);

    for (const name of ['GPU', 'Aux', 'GPU+Aux']) {
      const calendar = renders.calendar;
      await act(() => user.click(mode(name)));
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(mode(name)).toBeChecked();
      expect(renders.calendar).toBeGreaterThan(calendar);
    }
    expect(renders).toEqual({ ...before, calendar: renders.calendar });
    expect(getUserResources).toHaveBeenCalledTimes(1);
  });

  it('cancels the requests in flight when the overview goes away', async () => {
    vi.mocked(getUserResources).mockReturnValue(new Promise(() => undefined));
    vi.mocked(getUserUsage).mockReturnValue(new Promise(() => undefined));
    const { unmount } = setup();
    await waitFor(() => expect(getUserUsage).toHaveBeenCalled());
    await waitFor(() => expect(getUserResources).toHaveBeenCalled());
    const resourcesSignal = vi.mocked(getUserResources).mock.calls[0][1]?.signal;
    const usageSignal = vi.mocked(getUserUsage).mock.calls[0][1]?.signal;
    expect(resourcesSignal?.aborted).toBe(false);
    expect(usageSignal?.aborted).toBe(false);
    unmount();
    expect(resourcesSignal?.aborted).toBe(true);
    expect(usageSignal?.aborted).toBe(true);
    expect(handleError).not.toHaveBeenCalled();
  });

  it('keeps the last counts when a refresh fails', async () => {
    setup();
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('3'));
    vi.mocked(getUserResources).mockRejectedValue(new Error('offline'));
    await act(() => vi.advanceTimersByTimeAsync(RESOURCES_POLL_INTERVAL));
    expect(getUserResources).toHaveBeenCalledTimes(2);
    expect(stat('GPUs')).toHaveTextContent('3');
    // Without a toast every 15 s.
    expect(handleError).toHaveBeenCalledWith(
      expect.any(Error),
      expect.objectContaining({ silent: true }),
    );
  });

  it('shows one toast while the current resources keep failing', async () => {
    vi.mocked(getUserResources).mockRejectedValue(new Error('offline'));
    setup();
    await waitFor(() => expect(getUserResources).toHaveBeenCalledTimes(1));
    await act(() => vi.advanceTimersByTimeAsync(4 * RESOURCES_POLL_INTERVAL));
    expect(getUserResources).toHaveBeenCalledTimes(5);
    const toasts = vi
      .mocked(handleError)
      .mock.calls.filter(([, options]) => options?.publicSubject?.includes('current resources'));
    expect(toasts).toHaveLength(5);
    expect(toasts.filter(([, options]) => !options?.silent)).toHaveLength(1);
  });

  it('reads no current resources while hidden from the start, and reads when shown', async () => {
    hidden = true;
    setup();
    await act(() => vi.advanceTimersByTimeAsync(4 * RESOURCES_POLL_INTERVAL));
    expect(getUserResources).not.toHaveBeenCalled();
    setHidden(false);
    await waitFor(() => expect(getUserResources).toHaveBeenCalledTimes(1));
    await act(() => vi.advanceTimersByTimeAsync(RESOURCES_POLL_INTERVAL));
    expect(getUserResources).toHaveBeenCalledTimes(2);
  });

  it('drops a late answer to a request sent before the page was hidden', async () => {
    const first = deferred<UserResources>();
    vi.mocked(getUserResources)
      .mockReturnValueOnce(first.promise)
      .mockResolvedValue({
        auxMetered: true,
        pools: [{ counts: { auxContainers: 0, cpuSlots: 0, gpus: 7 }, resourcePool: 'default' }],
      });
    setup();
    await waitFor(() => expect(getUserResources).toHaveBeenCalledTimes(1));
    setHidden(true);
    // Hiding the page cancels the request.
    expect(vi.mocked(getUserResources).mock.calls[0][1]?.signal?.aborted).toBe(true);
    setHidden(false);
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('7'));
    await act(() => {
      first.resolve(RESOURCES);
      return Promise.resolve();
    });
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(stat('GPUs')).toHaveTextContent('7');
  });

  it('shows empty blocks when the requests fail', async () => {
    vi.mocked(getUserResources).mockRejectedValue(new Error('offline'));
    vi.mocked(getUserUsage).mockRejectedValue(new Error('offline'));
    setup();
    await waitFor(() => expect(getUserUsage).toHaveBeenCalled());
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByText('Current resources')).toBeInTheDocument();
    expect(screen.queryByText('GPUs')).not.toBeInTheDocument();
    expect(screen.queryByTestId('usage-calendar')).not.toBeInTheDocument();
    expect(screen.queryByTestId('usage-chart')).not.toBeInTheDocument();
    expect(handleError).toHaveBeenCalledTimes(2);
    vi.mocked(handleError).mock.calls.forEach(([, options]) => expect(options?.silent).toBeFalsy());
  });

  describe('usage refresh', () => {
    it('reads the usage again every five minutes while the page is visible', async () => {
      setup();
      expect(await screen.findByTestId('usage-calendar')).toBeInTheDocument();
      expect(getUserUsage).toHaveBeenCalledTimes(1);
      await act(() => vi.advanceTimersByTimeAsync(USAGE_REFRESH_INTERVAL - 1000));
      expect(getUserUsage).toHaveBeenCalledTimes(1);
      await act(() => vi.advanceTimersByTimeAsync(1000));
      expect(getUserUsage).toHaveBeenCalledTimes(2);
      await act(() => vi.advanceTimersByTimeAsync(USAGE_REFRESH_INTERVAL));
      expect(getUserUsage).toHaveBeenCalledTimes(3);
    });

    it('reads nothing while the page is hidden', async () => {
      setup();
      expect(await screen.findByTestId('usage-calendar')).toBeInTheDocument();
      setHidden(true);
      await act(() => vi.advanceTimersByTimeAsync(3 * USAGE_REFRESH_INTERVAL));
      expect(getUserUsage).toHaveBeenCalledTimes(1);
    });

    it('reads nothing while hidden from the start, and reads when shown', async () => {
      hidden = true;
      setup();
      await act(() => vi.advanceTimersByTimeAsync(USAGE_REFRESH_INTERVAL));
      expect(getUserUsage).not.toHaveBeenCalled();
      setHidden(false);
      await waitFor(() => expect(getUserUsage).toHaveBeenCalledTimes(1));
    });

    it('reads the usage when shown again only after a minute', async () => {
      setup();
      expect(await screen.findByTestId('usage-calendar')).toBeInTheDocument();
      setHidden(true);
      await act(() => vi.advanceTimersByTimeAsync(USAGE_STALE_AFTER - 10_000));
      setHidden(false);
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(getUserUsage).toHaveBeenCalledTimes(1);

      setHidden(true);
      await act(() => vi.advanceTimersByTimeAsync(20_000));
      setHidden(false);
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(getUserUsage).toHaveBeenCalledTimes(2);
      // The five minutes count from that load.
      await act(() => vi.advanceTimersByTimeAsync(USAGE_REFRESH_INTERVAL - 1000));
      expect(getUserUsage).toHaveBeenCalledTimes(2);
      await act(() => vi.advanceTimersByTimeAsync(1000));
      expect(getUserUsage).toHaveBeenCalledTimes(3);
    });

    it('moves the range and the calendar to the new day at UTC midnight', async () => {
      vi.setSystemTime(new Date('2026-11-10T23:59:50Z'));
      vi.mocked(getUserUsage).mockResolvedValueOnce(USAGE);
      setup();
      expect(await screen.findByTestId('usage-calendar')).toBeInTheDocument();
      expect(screen.queryByLabelText('2026-11-11')).not.toBeInTheDocument();
      vi.mocked(getUserUsage).mockReturnValue(new Promise(() => undefined));

      await act(() => vi.advanceTimersByTimeAsync(15_000));
      expect(getUserUsage).toHaveBeenCalledTimes(2);
      expect(vi.mocked(getUserUsage).mock.calls[1][0]).toEqual(
        expect.objectContaining({ endDate: '2026-11-11', startDate: '2025-11-01' }),
      );
      // The grid moves while the new day loads, with the data shown so far.
      expect(screen.getByLabelText('2026-11-11')).toHaveClass('notCovered');
      expect(screen.getByLabelText('2026-11-09')).toHaveClass('level2');
      expect(screen.getByTestId('chart-time').textContent?.split(',')[0]).toBe('2026-10-12');
    });

    it("cancels the previous day's load at UTC midnight", async () => {
      vi.setSystemTime(new Date('2026-11-10T23:59:50Z'));
      const first = deferred<UserUsage>();
      vi.mocked(getUserUsage).mockReturnValueOnce(first.promise);
      setup();
      await waitFor(() => expect(getUserUsage).toHaveBeenCalledTimes(1));
      await act(() => vi.advanceTimersByTimeAsync(15_000));
      expect(getUserUsage).toHaveBeenCalledTimes(2);
      const signal = (call: number) => vi.mocked(getUserUsage).mock.calls[call][1]?.signal;
      expect(signal(0)?.aborted).toBe(true);
      expect(signal(1)?.aborted).toBe(false);
      await act(() => {
        first.resolve(coveredUsage(() => [13]));
        return Promise.resolve();
      });
      await act(() => vi.advanceTimersByTimeAsync(0));
      // The new day's answer, not the late one.
      expect(screen.getByLabelText('2026-11-11')).toBeInTheDocument();
      expect(screen.getByLabelText('2026-11-09')).toHaveClass('level2');
      expect(screen.getByLabelText('2026-11-08')).toHaveClass('level0');
    });

    it('keeps the usage shown, without a loading state or a toast, while a refresh loads and when it fails', async () => {
      setup();
      expect(await screen.findByText('Active days · 2')).toBeInTheDocument();
      const refresh = deferred<UserUsage>();
      vi.mocked(getUserUsage).mockReturnValue(refresh.promise);
      await act(() => vi.advanceTimersByTimeAsync(USAGE_REFRESH_INTERVAL));
      expect(getUserUsage).toHaveBeenCalledTimes(2);
      expect(screen.getByText('Active days · 2')).toBeInTheDocument();
      expect(screen.getByTestId('usage-chart')).toBeInTheDocument();
      expect(document.querySelector('#active-days .ant-spin-spinning')).toBeNull();
      expect(document.querySelector('#usage .ant-spin-spinning')).toBeNull();

      await act(() => {
        refresh.reject(new Error('offline'));
        return Promise.resolve();
      });
      expect(screen.getByText('Active days · 2')).toBeInTheDocument();
      expect(screen.getByLabelText('2026-11-09')).toHaveClass('level2');
      expect(handleError).toHaveBeenCalledWith(
        expect.any(Error),
        expect.objectContaining({ silent: true }),
      );
    });

    it("drops the late answers for the previous user's overview", async () => {
      const resources = deferred<UserResources>();
      const usage = deferred<UserUsage>();
      vi.mocked(getUserResources).mockImplementation(({ userId }) =>
        userId === 4
          ? resources.promise
          : Promise.resolve({
              auxMetered: true,
              pools: [{ counts: { auxContainers: 0, cpuSlots: 0, gpus: 7 }, resourcePool: 'a' }],
            }),
      );
      vi.mocked(getUserUsage).mockImplementation(({ userId }) =>
        userId === 4 ? usage.promise : Promise.resolve(coveredUsage(() => [0])),
      );
      const { rerender } = setup(4);
      await waitFor(() =>
        expect(getUserUsage).toHaveBeenCalledWith(
          expect.objectContaining({ userId: 4 }),
          expect.anything(),
        ),
      );
      rerender(<UserOverview userId={5} />);
      await waitFor(() => expect(stat('GPUs')).toHaveTextContent('7'));
      expect(await screen.findByText('Active days · 0')).toBeInTheDocument();

      await act(() => {
        resources.resolve(RESOURCES);
        usage.resolve(USAGE);
        return Promise.resolve();
      });
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(stat('GPUs')).toHaveTextContent('7');
      expect(screen.getByText('Active days · 0')).toBeInTheDocument();
      expect(screen.getByLabelText('2026-11-09')).toHaveClass('level0');
    });
  });
});
