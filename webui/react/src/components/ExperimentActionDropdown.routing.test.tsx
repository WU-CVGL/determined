import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { useInitApi } from 'hew/Toast';
import { ConfirmationProvider } from 'hew/useConfirm';
import React from 'react';
import { HelmetProvider } from 'react-helmet-async';
import { createMemoryRouter, RouterProvider, useParams } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { FEATURE_SETTINGS_PATH, FeatureSettingsConfig } from 'hooks/useFeature';
import ExperimentDetails from 'pages/ExperimentDetails';
import TrialDetails from 'pages/TrialDetails';
import router from 'router';
import appRoutes from 'routes/routes';
import { getExperimentDetails, getExpTrials, getTrialDetails } from 'services/api';
import userSettings from 'stores/userSettings';
import { ExperimentSearcherName, ProjectExperiment, TrialItem } from 'types';
import { generateTestExperimentData } from 'utils/tests/generateTestData';

import ExperimentActionDropdown, { Action } from './ExperimentActionDropdown';
import { experiment as listExperiment } from './ExperimentActionDropdown.test.mock';

/**
 * View Logs through the app's router, with the app's route paths and flat runs on (the default,
 * from the real useFeature): handlePath, routeAll and the router are the real ones, the experiment
 * page is the real one (with flat runs and user settings loaded it redirects every experiment page
 * path to the search page), and so is the trial page, with its Logs tab content replaced.
 */

vi.mock('services/api', () => ({
  getExperimentDetails: vi.fn(),
  getExpTrials: vi.fn(),
  getTrialDetails: vi.fn(),
  getTrialRemainingLogRetentionDays: vi.fn(() =>
    Promise.resolve({ remainingLogRetentionDays: -1 }),
  ),
  getTrialWorkloads: vi.fn(() => new Promise(() => {})),
  getWorkspaces: vi.fn(() => Promise.resolve({ workspaces: [] })),
  resetUserSetting: vi.fn(() => Promise.resolve()),
  updateUserSetting: vi.fn(() => Promise.resolve()),
}));

// The logs viewer streams from the master; the test only needs to see which trial it is given.
vi.mock('pages/TrialDetails/TrialDetailsLogs', () => ({
  default: ({ trial }: { trial?: { id: number } }) => <div>Logs of trial {trial?.id}</div>,
}));

const user = userEvent.setup();

const EXPERIMENT_ID = listExperiment.id;
const TRIAL_ID = 7;

// A search with searcher random and max_trials 1, as a list row has it: the raw config, one trial,
// and no trial IDs (the list APIs leave them out).
const searchRow: ProjectExperiment = {
  ...listExperiment,
  config: {
    searcher: { max_trials: 1, metric: 'loss', name: 'random', smaller_is_better: true },
  } as unknown as ProjectExperiment['config'],
  numTrials: 1,
  searcherType: 'random',
  trialIds: [],
};

const testData = generateTestExperimentData();
const trialDetails = { ...testData.trial, experimentId: EXPERIMENT_ID, id: TRIAL_ID };
const experimentDetails = { ...testData.experiment, id: EXPERIMENT_ID, trialIds: [TRIAL_ID] };
const singleTrialExperimentDetails = {
  ...experimentDetails,
  config: {
    ...experimentDetails.config,
    searcher: { ...experimentDetails.config.searcher, name: ExperimentSearcherName.Single },
  },
};

const SearchPage = () => {
  const { searchId, tab } = useParams();
  return (
    <div>
      Search {searchId} {tab ?? 'runs'}
    </div>
  );
};

const ListPage = () => {
  useInitApi();
  return (
    <ExperimentActionDropdown experiment={searchRow} isContextMenu makeOpen>
      <div />
    </ExperimentActionDropdown>
  );
};

const pages: Record<string, React.ReactNode> = {
  experimentDetails: <ExperimentDetails />,
  searchDetails: <SearchPage />,
  trialDetails: <TrialDetails />,
};

const setup = (initialEntry = '/') => {
  const memoryRouter = createMemoryRouter(
    [
      { element: <ListPage />, path: '/' },
      ...appRoutes
        .filter((route) => route.id in pages)
        .map((route) => ({ element: pages[route.id], path: route.path })),
    ],
    { initialEntries: [initialEntry] },
  );
  // routeToReactUrl, which handlePath uses, navigates the app router.
  router.routerInstance = memoryRouter;
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <HelmetProvider>
          <ConfirmationProvider>
            <RouterProvider router={memoryRouter} />
          </ConfirmationProvider>
        </HelmetProvider>
      </ThemeProvider>
    </UIProvider>,
  );
  return memoryRouter;
};

const pathname = (memoryRouter: ReturnType<typeof createMemoryRouter>) =>
  memoryRouter.state.location.pathname;

const mockTrialPage = () => {
  vi.mocked(getExpTrials).mockResolvedValue({
    pagination: { limit: 1, offset: 0, total: 1 },
    trials: [{ experimentId: EXPERIMENT_ID, id: TRIAL_ID } as TrialItem],
  });
  vi.mocked(getTrialDetails).mockResolvedValue(trialDetails);
  vi.mocked(getExperimentDetails).mockResolvedValue(experimentDetails);
};

const expectTrialLogsTab = async (memoryRouter: ReturnType<typeof createMemoryRouter>) => {
  const trialLogsPath = `/experiments/${EXPERIMENT_ID}/trials/${TRIAL_ID}/logs`;
  await waitFor(() => expect(pathname(memoryRouter)).toBe(trialLogsPath));
  expect(getExpTrials).toHaveBeenCalledWith({ id: EXPERIMENT_ID, limit: 1 });
  expect(await screen.findByText(`Logs of trial ${TRIAL_ID}`)).toBeInTheDocument();
  // The tab is named with the log retention left, e.g. "Logs Frvr".
  expect(screen.getByRole('tab', { selected: true })).toHaveAccessibleName(/^Logs\b/);
  // The trial page stays: nothing redirects it.
  expect(pathname(memoryRouter)).toBe(trialLogsPath);
};

beforeAll(async () => {
  // Loaded user settings, with no feature setting: flat runs is on, its default.
  await userSettings.clear();
});

beforeEach(() => {
  vi.mocked(getExperimentDetails).mockReset();
  vi.mocked(getExpTrials).mockReset();
  vi.mocked(getTrialDetails).mockReset();
});

describe('ExperimentActionDropdown View Logs through the router, flat runs on', () => {
  it('redirects the experiment page’s Logs tab path to the search page', async () => {
    // Why the menu cannot lead to /experiments/:id/logs with flat runs: the experiment page
    // redirects to the search page, which has no Logs tab.
    vi.mocked(getExperimentDetails).mockRejectedValue(new Error('not needed'));
    const memoryRouter = setup(`/experiments/${EXPERIMENT_ID}/logs`);
    await waitFor(() => expect(pathname(memoryRouter)).toBe(`/searches/${EXPERIMENT_ID}`));
  });

  it('opens the Logs tab of the trial of a single-trial search', async () => {
    mockTrialPage();
    const memoryRouter = setup();

    await user.click(screen.getByText(Action.ViewLogs));

    await expectTrialLogsTab(memoryRouter);
  });

  it('opens the search page when the trial cannot be fetched', async () => {
    vi.mocked(getExpTrials).mockRejectedValue(new Error('Fetch Error'));
    const memoryRouter = setup();

    await user.click(screen.getByText(Action.ViewLogs));

    await waitFor(() => expect(pathname(memoryRouter)).toBe(`/searches/${EXPERIMENT_ID}`));
    expect(getExpTrials).toHaveBeenCalledWith({ id: EXPERIMENT_ID, limit: 1 });
    expect(screen.getByText(`Search ${EXPERIMENT_ID} runs`)).toBeInTheDocument();
  });

  it('opens the search page when the search has no trial to fetch', async () => {
    vi.mocked(getExpTrials).mockResolvedValue({
      pagination: { limit: 1, offset: 0, total: 0 },
      trials: [],
    });
    const memoryRouter = setup();

    await user.click(screen.getByText(Action.ViewLogs));

    await waitFor(() => expect(pathname(memoryRouter)).toBe(`/searches/${EXPERIMENT_ID}`));
    expect(getExpTrials).toHaveBeenCalledWith({ id: EXPERIMENT_ID, limit: 1 });
    expect(screen.getByText(`Search ${EXPERIMENT_ID} runs`)).toBeInTheDocument();
  });
});

describe('ExperimentActionDropdown View Logs through the router, flat runs off', () => {
  beforeAll(() => {
    userSettings.set(FeatureSettingsConfig, FEATURE_SETTINGS_PATH, { flat_runs: false });
  });

  afterAll(async () => {
    await userSettings.clear();
  });

  it('opens the Logs tab of the trial of a single-trial experiment', async () => {
    mockTrialPage();
    const memoryRouter = setup();

    await user.click(screen.getByText(Action.ViewLogs));

    await expectTrialLogsTab(memoryRouter);
  });

  it('opens the Logs tab of the experiment page when the trial cannot be fetched', async () => {
    // As before the trial was fetched: the single-trial experiment page has a Logs tab.
    vi.mocked(getExpTrials).mockRejectedValue(new Error('Fetch Error'));
    vi.mocked(getExperimentDetails).mockResolvedValue(singleTrialExperimentDetails);
    const memoryRouter = setup();

    await user.click(screen.getByText(Action.ViewLogs));

    await waitFor(() => expect(pathname(memoryRouter)).toBe(`/experiments/${EXPERIMENT_ID}/logs`));
    expect(await screen.findByText(`Experiment ${EXPERIMENT_ID}`)).toBeInTheDocument();
    expect(screen.getByRole('tab', { selected: true })).toHaveAccessibleName(/^Logs\b/);
    expect(pathname(memoryRouter)).toBe(`/experiments/${EXPERIMENT_ID}/logs`);
  });
});
