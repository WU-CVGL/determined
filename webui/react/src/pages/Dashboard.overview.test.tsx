import { render, screen } from '@testing-library/react';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { HelmetProvider } from 'react-helmet-async';

import { ThemeProvider } from 'components/ThemeProvider';
import {
  getExperiments,
  getProjectsByUserActivity,
  getUserResources,
  getUserUsage,
} from 'services/api';
import userStore from 'stores/users';
import { DetailedUser } from 'types';

import Dashboard from './Dashboard';

// The projects and submissions never answer.
vi.mock('services/api', () => ({
  getCommands: vi.fn(() => new Promise(() => {})),
  getExperiments: vi.fn(() => new Promise(() => {})),
  getProjectsByUserActivity: vi.fn(() => new Promise(() => {})),
  getShells: vi.fn(() => new Promise(() => {})),
  getUserResources: vi.fn(),
  getUserUsage: vi.fn(),
}));

vi.mock('hooks/usePermissions', () => ({ default: () => ({ canCreateNSC: false }) }));

vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: vi.fn(),
}));

/* The chart draws on a canvas. */
vi.mock('pages/Cluster/ClusterHistoricalUsageChart', () => ({
  default: () => <div data-testid="chart" />,
}));

const ME: DetailedUser = { id: 3, isActive: true, isAdmin: false, username: 'me' };

describe('Home', () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.mocked(getUserResources).mockResolvedValue({
      auxMetered: true,
      pools: [{ counts: { auxContainers: 0, cpuSlots: 0, gpus: 2 }, resourcePool: 'default' }],
    });
    vi.mocked(getUserUsage).mockResolvedValue({ coverageStart: '2025-11-01', entries: [] });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.clearAllMocks();
    userStore.reset();
  });

  it('loads the overview while the projects and submissions are pending', async () => {
    userStore.updateCurrentUser(ME);
    render(
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <HelmetProvider>
            <Dashboard />
          </HelmetProvider>
        </ThemeProvider>
      </UIProvider>,
    );

    expect(await screen.findByText('GPUs')).toBeInTheDocument();
    expect(screen.getByTestId('user-overview')).toBeInTheDocument();
    expect(getUserResources).toHaveBeenCalledWith({ userId: 3 }, expect.anything());
    expect(getUserUsage).toHaveBeenCalledWith(
      expect.objectContaining({ userId: 3 }),
      expect.anything(),
    );
    expect(getProjectsByUserActivity).toHaveBeenCalled();
    expect(getExperiments).toHaveBeenCalled();
    expect(screen.getByText('Your Recent Submissions')).toBeInTheDocument();
    expect(screen.queryByText('No submissions')).not.toBeInTheDocument();
  });
});
