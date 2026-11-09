import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { HelmetProvider } from 'react-helmet-async';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import usePageVisibility from 'hooks/usePageVisibility';
import { getResourcePools, getUserResources, getUsers, getUserUsage } from 'services/api';
import { V1ResourcePoolType } from 'services/api-ts-sdk';
import clusterStore from 'stores/cluster';
import userStore from 'stores/users';
import {
  DetailedUser,
  ResourcePool,
  ResourceType,
  UserResources,
  UserUsage,
  UserUsageEntry,
} from 'types';
import handleError from 'utils/error';

import UserProfile, { RESOURCES_POLL_INTERVAL } from './UserProfile';

vi.mock('services/api', () => ({
  getResourcePools: vi.fn(),
  getUserResources: vi.fn(),
  getUsers: vi.fn(),
  getUserUsage: vi.fn(),
}));

vi.mock('hooks/usePermissions', () => ({ default: () => ({ loading: false }) }));

vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: vi.fn(),
}));

/* The chart draws on a canvas; the stub shows what it was given. */
vi.mock('pages/Cluster/ClusterHistoricalUsageChart', () => ({
  default: (props: { hoursByLabel: Record<string, number[]>; label?: string; time: string[] }) => (
    <div data-testid="usage-chart">
      <span data-testid="chart-series">{Object.keys(props.hoursByLabel).join(',')}</span>
      <span data-testid="chart-time">{props.time.join(',')}</span>
      <span data-testid="chart-label">{props.label}</span>
      <span data-testid="chart-values">{JSON.stringify(props.hoursByLabel)}</span>
    </div>
  ),
}));

// A Tuesday: today is 2026-11-10 (UTC).
const NOW = new Date('2026-11-10T15:00:00Z');
const HOUR = 3600;

const ALICE: DetailedUser = {
  displayName: 'Alice Liddell',
  id: 4,
  isActive: true,
  isAdmin: false,
  username: 'alice',
};

const BOB: DetailedUser = { id: 5, isActive: false, isAdmin: false, username: 'bob' };

const pool = (name: string, type: V1ResourcePoolType, slotType: ResourceType): ResourcePool =>
  ({ name, slotType, type, usable: true }) as unknown as ResourcePool;

const GPU_POOL = pool('default', V1ResourcePoolType.STATIC, ResourceType.CUDA);
const CPU_POOL = pool('cpu', V1ResourcePoolType.STATIC, ResourceType.CPU);
const K8S_POOL = pool('k8s', V1ResourcePoolType.K8S, ResourceType.CUDA);

const RESOURCES: UserResources = {
  pools: [
    { counts: { auxContainers: 1, cpuSlots: 0, gpus: 2 }, resourcePool: 'default' },
    { counts: { auxContainers: 0, cpuSlots: 0, gpus: 1 }, resourcePool: 'other' },
  ],
};

const day = (periodStart: string, gpuHours: number, auxHours?: number): UserUsageEntry => ({
  activeDays: gpuHours > 0 ? 1 : 0,
  activeSeconds: gpuHours * HOUR,
  auxContainerSeconds: auxHours === undefined ? undefined : auxHours * HOUR,
  cpuSlotSeconds: 0,
  gpuSeconds: gpuHours * HOUR,
  periodStart,
});

/** Covered daily from 2025-11-01 to today, with the GPU hours of each day. */
const coveredUsage = (gpuHours: (date: string) => number): UserUsage => {
  const entries: UserUsageEntry[] = [];
  for (let t = Date.parse('2025-11-01T00:00:00Z'); t <= NOW.getTime(); t += 24 * HOUR * 1000) {
    const date = new Date(t).toISOString().slice(0, 10);
    entries.push(day(date, gpuHours(date), 0));
  }
  return { coverageStart: '2025-11-01', entries };
};

/** GPU hours on two days. */
const USAGE = coveredUsage((date) => (date === '2026-11-09' ? 6 : date === '2026-10-02' ? 2 : 0));

/* App keeps the page's visibility in the UI store; the page alone does not. */
const PageVisibility = () => {
  usePageVisibility();
  return null;
};

const setup = async (userId: string, pools: ResourcePool[] = [GPU_POOL]) => {
  vi.mocked(getResourcePools).mockResolvedValue(pools);
  clusterStore.fetchResourcePools();
  userStore.fetchUsers();
  await waitFor(() => expect(getUsers).toHaveBeenCalled());
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <HelmetProvider>
          <PageVisibility />
          <MemoryRouter initialEntries={[`/users/${userId}`]}>
            <Routes>
              <Route element={<UserProfile />} path="/users/:userId" />
            </Routes>
          </MemoryRouter>
        </HelmetProvider>
      </ThemeProvider>
    </UIProvider>,
  );
};

const stat = (title: string) => screen.getByText(title).parentElement?.parentElement as HTMLElement;

describe('UserProfile', () => {
  beforeEach(() => {
    vi.useFakeTimers({ now: NOW, shouldAdvanceTime: true });
    vi.mocked(getUsers).mockResolvedValue({
      pagination: { limit: 0, offset: 0, total: 2 },
      users: [ALICE, BOB],
    });
    vi.mocked(getUserResources).mockResolvedValue(RESOURCES);
    vi.mocked(getUserUsage).mockResolvedValue(USAGE);
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.clearAllMocks();
    userStore.reset();
  });

  it("shows the user's badge, current resources and a link to the user's jobs", async () => {
    await setup('4');
    expect(await screen.findByText('Alice Liddell')).toBeInTheDocument();
    expect(screen.getByText('alice')).toBeInTheDocument();
    expect(screen.queryByText('Deactivated')).not.toBeInTheDocument();
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('3'));
    expect(stat('Aux containers')).toHaveTextContent('1');
    // No CPU pool and no CPU slots held.
    expect(screen.queryByText('CPU slots')).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Jobs' })).toHaveAttribute('href', '/jobs?user=4');
    expect(getUserResources).toHaveBeenCalledWith({ userId: 4 });
  });

  it('marks a deactivated user', async () => {
    await setup('5');
    expect(await screen.findByText('Deactivated')).toBeInTheDocument();
  });

  it('shows CPU slots with a CPU pool', async () => {
    await setup('4', [GPU_POOL, CPU_POOL]);
    await waitFor(() => expect(stat('CPU slots')).toHaveTextContent('0'));
  });

  it('shows Aux as not available when every pool is Kubernetes', async () => {
    vi.mocked(getUserResources).mockResolvedValue({
      pools: [{ counts: { cpuSlots: 0, gpus: 8 }, resourcePool: 'k8s' }],
    });
    await setup('4', [K8S_POOL]);
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('8'));
    expect(stat('Aux containers')).toHaveTextContent('Not available');
  });

  it('shows CPU slots the user holds without a CPU pool', async () => {
    vi.mocked(getUserResources).mockResolvedValue({
      pools: [{ counts: { auxContainers: 0, cpuSlots: 2, gpus: 0 }, resourcePool: 'default' }],
    });
    await setup('4');
    await waitFor(() => expect(stat('CPU slots')).toHaveTextContent('2'));
  });

  it.each(['99', 'alice', '4x'])('says "User not found" for /users/%s', async (userId) => {
    await setup(userId);
    expect(await screen.findByText('User not found')).toBeInTheDocument();
    expect(getUserResources).not.toHaveBeenCalled();
    expect(getUserUsage).not.toHaveBeenCalled();
  });

  it('reads the usage once, daily, from the first month twelve months back to today', async () => {
    await setup('4');
    expect(await screen.findByTestId('usage-calendar')).toBeInTheDocument();
    expect(getUserUsage).toHaveBeenCalledTimes(1);
    expect(vi.mocked(getUserUsage).mock.calls[0][0]).toEqual({
      endDate: '2026-11-10',
      period: 'RESOURCE_ALLOCATION_AGGREGATION_PERIOD_DAILY',
      startDate: '2025-11-01',
      userId: 4,
    });
    expect(screen.getByLabelText('2026-11-09')).toHaveClass('level2');
    expect(screen.getByText('Active days (UTC)')).toBeInTheDocument();
  });

  it('switches between days and months without another request', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    await setup('4');
    const series = await screen.findByTestId('chart-series');
    expect(series).toHaveTextContent('GPU,Aux');
    expect(screen.getByTestId('chart-label')).toHaveTextContent('Hours');
    const days = screen.getByTestId('chart-time').textContent?.split(',');
    expect(days).toHaveLength(31);
    expect(days?.[0]).toBe('2026-10-11');
    expect(days?.at(-1)).toBe('2026-11-10');
    expect(screen.getByRole('columnheader', { name: 'Date' })).toBeInTheDocument();
    // The table lists the newest day first.
    const firstRow = () => screen.getAllByRole('row')[1];
    expect(within(firstRow()).getAllByRole('cell')[0]).toHaveTextContent('2026-11-10');
    expect(within(screen.getAllByRole('row')[2]).getAllByRole('cell')[1]).toHaveTextContent('6.0');

    await user.click(screen.getByText('Month'));
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

    await user.click(screen.getByText('Day'));
    await waitFor(() =>
      expect(screen.getByRole('columnheader', { name: 'Date' })).toBeInTheDocument(),
    );
    expect(getUserUsage).toHaveBeenCalledTimes(1);
  });

  it('shows zeros and flat lines without any usage', async () => {
    vi.mocked(getUserResources).mockResolvedValue({ pools: [] });
    vi.mocked(getUserUsage).mockResolvedValue(coveredUsage(() => 0));
    await setup('4');
    await waitFor(() => expect(stat('GPUs')).toHaveTextContent('0'));
    expect(stat('Aux containers')).toHaveTextContent('0');
    expect(await screen.findByTestId('usage-calendar')).toBeInTheDocument();
    expect(screen.getByLabelText('2026-11-10')).toHaveClass('level0');
    expect(screen.getByLabelText('2026-11-10')).not.toHaveClass('notCovered');
    expect(screen.getByLabelText('2025-11-10')).toHaveClass('level0');
    expect(screen.getByTestId('chart-time').textContent?.split(',')).toHaveLength(31);
    const values = JSON.parse(screen.getByTestId('chart-values').textContent ?? '{}');
    expect(values.GPU).toEqual(Array(31).fill(0));
    expect(values.Aux).toEqual(Array(31).fill(0));
    expect(within(screen.getAllByRole('row')[1]).getAllByRole('cell')[1]).toHaveTextContent('0.0');
  });

  it('outlines the calendar when the cluster has no records', async () => {
    vi.mocked(getUserUsage).mockResolvedValue({ coverageStart: undefined, entries: [] });
    await setup('4');
    expect(await screen.findByTestId('usage-calendar')).toBeInTheDocument();
    expect(screen.getByLabelText('2026-11-10')).toHaveClass('notCovered');
    expect(screen.getByTestId('chart-series')).toHaveTextContent('GPU');
  });

  it('reads the current resources every 15 s, never the usage, and not while the page is hidden', async () => {
    let hidden = false;
    const visibility = vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
    try {
      await setup('4');
      await waitFor(() => expect(stat('GPUs')).toHaveTextContent('3'));
      expect(getUserResources).toHaveBeenCalledTimes(1);

      vi.mocked(getUserResources).mockResolvedValue({
        pools: [{ counts: { auxContainers: 0, cpuSlots: 0, gpus: 0 }, resourcePool: 'default' }],
      });
      await act(() => vi.advanceTimersByTimeAsync(RESOURCES_POLL_INTERVAL));
      expect(getUserResources).toHaveBeenCalledTimes(2);
      await waitFor(() => expect(stat('GPUs')).toHaveTextContent('0'));

      hidden = true;
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'));
      });
      await act(() => vi.advanceTimersByTimeAsync(4 * RESOURCES_POLL_INTERVAL));
      expect(getUserResources).toHaveBeenCalledTimes(2);

      hidden = false;
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'));
      });
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(getUserResources).toHaveBeenCalledTimes(3);
      await act(() => vi.advanceTimersByTimeAsync(RESOURCES_POLL_INTERVAL));
      expect(getUserResources).toHaveBeenCalledTimes(4);
      expect(getUserUsage).toHaveBeenCalledTimes(1);
    } finally {
      visibility.mockRestore();
    }
  });

  it('keeps the last counts when a refresh fails', async () => {
    await setup('4');
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

  it('shows empty blocks when the requests fail', async () => {
    vi.mocked(getUserResources).mockRejectedValue(new Error('offline'));
    vi.mocked(getUserUsage).mockRejectedValue(new Error('offline'));
    await setup('4');
    expect(await screen.findByText('Alice Liddell')).toBeInTheDocument();
    await waitFor(() => expect(getUserUsage).toHaveBeenCalled());
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByText('Current resources')).toBeInTheDocument();
    expect(screen.queryByText('GPUs')).not.toBeInTheDocument();
    expect(screen.queryByTestId('usage-calendar')).not.toBeInTheDocument();
    expect(screen.queryByTestId('usage-chart')).not.toBeInTheDocument();
    expect(handleError).toHaveBeenCalledTimes(2);
    vi.mocked(handleError).mock.calls.forEach(([, options]) => expect(options?.silent).toBeFalsy());
  });
});
