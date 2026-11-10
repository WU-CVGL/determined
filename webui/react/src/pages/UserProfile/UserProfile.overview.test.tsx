import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { HelmetProvider } from 'react-helmet-async';
import { createMemoryRouter, RouterProvider } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { getUser, getUserResources, getUserUsage } from 'services/api';
import userStore from 'stores/users';
import { DetailedUser, UserUsageEntry } from 'types';

import UserProfile from './UserProfile';

vi.mock('services/api', () => ({
  getUser: vi.fn(),
  getUserResources: vi.fn(),
  getUsers: vi.fn(),
  getUserUsage: vi.fn(),
}));

vi.mock('hooks/usePermissions', () => ({ default: () => ({ loading: false }) }));

vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: vi.fn(),
}));

/* The chart draws on a canvas; the stub shows its series. */
vi.mock('pages/Cluster/ClusterHistoricalUsageChart', () => ({
  default: (props: { hoursByLabel: Record<string, number[]> }) => (
    <div data-testid="chart-series">{Object.keys(props.hoursByLabel).join(',')}</div>
  ),
}));

// A Tuesday: today is 2026-11-10 (UTC).
const NOW = new Date('2026-11-10T15:00:00Z');
const HOUR = 3600;

const ME: DetailedUser = { id: 3, isActive: true, isAdmin: false, username: 'me' };
const ALICE: DetailedUser = { id: 4, isActive: true, isAdmin: false, username: 'alice' };

const day = (periodStart: string, gpuHours: number, auxHours = 0): UserUsageEntry => ({
  auxActiveDays: auxHours > 0 ? 1 : 0,
  auxActiveSeconds: auxHours * HOUR,
  auxContainerSeconds: auxHours * HOUR,
  cpuSlotSeconds: 0,
  gpuActiveDays: gpuHours > 0 ? 1 : 0,
  gpuActiveSeconds: gpuHours * HOUR,
  gpuOrAuxActiveDays: gpuHours > 0 || auxHours > 0 ? 1 : 0,
  gpuOrAuxActiveSeconds: Math.max(gpuHours, auxHours) * HOUR,
  gpuSeconds: gpuHours * HOUR,
  periodStart,
  unclassifiedSlotSeconds: 0,
});

// GPUs on two days, Aux containers on one.
const ENTRIES = [day('2026-11-07', 2), day('2026-11-08', 0, 13), day('2026-11-09', 6)];

const setup = () => {
  userStore.updateCurrentUser(ME);
  const router = createMemoryRouter([{ element: <UserProfile />, path: '/users/:userId' }], {
    initialEntries: ['/users/4'],
  });
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <HelmetProvider>
          <RouterProvider router={router} />
        </HelmetProvider>
      </ThemeProvider>
    </UIProvider>,
  );
};

const section = (id: string) => document.querySelector(`section#${id}`) as HTMLElement;
const modes = () => within(section('active-days')).queryAllByRole('radio');
const cardTitles = () =>
  ['GPUs', 'Aux containers'].filter((title) =>
    within(section('current-resources')).queryByText(title),
  );

describe("another user's page", () => {
  beforeEach(() => {
    vi.useFakeTimers({ now: NOW, shouldAdvanceTime: true });
    vi.mocked(getUser).mockResolvedValue(ALICE);
    vi.mocked(getUserUsage).mockResolvedValue({ coverageStart: '2025-11-01', entries: ENTRIES });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.clearAllMocks();
    userStore.reset();
  });

  it('chooses the mode of the active days only, in the Active days section', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    vi.mocked(getUserResources).mockResolvedValue({
      auxMetered: true,
      pools: [{ counts: { auxContainers: 1, cpuSlots: 0, gpus: 2 }, resourcePool: 'default' }],
    });
    setup();
    expect(await screen.findByText('Active days · 3')).toBeInTheDocument();
    await waitFor(() => expect(cardTitles()).toEqual(['GPUs', 'Aux containers']));
    expect(getUserUsage).toHaveBeenCalledWith(
      expect.objectContaining({ userId: 4 }),
      expect.anything(),
    );
    expect(modes().map((radio) => radio.closest('label')?.textContent)).toEqual([
      'GPU',
      'Aux',
      'GPU+Aux',
    ]);
    expect(screen.getByTestId('chart-series')).toHaveTextContent(/^GPU,Aux$/);

    await act(() => user.click(within(section('active-days')).getByRole('radio', { name: 'GPU' })));
    expect(screen.getByText('Active days · 2')).toBeInTheDocument();
    expect(screen.getByLabelText('2026-11-08')).toHaveClass('level0');
    expect(cardTitles()).toEqual(['GPUs', 'Aux containers']);
    expect(screen.getByTestId('chart-series')).toHaveTextContent(/^GPU,Aux$/);
  });

  it('shows GPUs only, without the mode control, where Aux is known absent', async () => {
    vi.mocked(getUserResources).mockResolvedValue({
      auxMetered: false,
      pools: [{ counts: { auxContainers: 0, cpuSlots: 0, gpus: 8 }, resourcePool: 'k8s' }],
    });
    vi.mocked(getUserUsage).mockResolvedValue({
      coverageStart: '2025-11-01',
      entries: [day('2026-11-07', 2), day('2026-11-09', 6)],
    });
    setup();
    await waitFor(() => expect(screen.getByTestId('chart-series')).toHaveTextContent(/^GPU$/));
    await waitFor(() => expect(cardTitles()).toEqual(['GPUs']));
    expect(screen.getByText('Active days · 2')).toBeInTheDocument();
    expect(modes()).toHaveLength(0);
  });
});
