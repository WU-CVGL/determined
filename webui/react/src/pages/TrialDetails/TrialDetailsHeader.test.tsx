import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { makeToast } from 'hew/Toast';
import { ConfirmationProvider } from 'hew/useConfirm';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import RESPONSES from 'pages/ExperimentDetails/ExperimentDetails.test.mock';
import { ExperimentBase, RunState, TrialDetails } from 'types';
import { DetError } from 'utils/error';
import { openTensorBoardSession } from 'utils/tensorBoardSession';

import TrialDetailsHeader from './TrialDetailsHeader';

vi.mock('services/api', () => ({
  getTrialWorkloads: vi.fn(() => Promise.resolve({ pagination: { total: 0 }, workloads: [] })),
  getWorkspaceProjects: vi.fn(() => Promise.resolve({ pagination: { total: 0 }, projects: [] })),
  getWorkspaces: vi.fn(() => Promise.resolve({ pagination: { total: 0 }, workspaces: [] })),
}));
vi.mock('utils/tensorBoardSession', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/tensorBoardSession')>()),
  openTensorBoardSession: vi.fn(),
}));
vi.mock('hew/Toast', async (importOriginal) => ({
  ...(await importOriginal<typeof import('hew/Toast')>()),
  makeToast: vi.fn(),
}));
vi.mock('hooks/useFeature', () => ({ default: () => ({ isOn: () => false }) }));

const EXPERIMENT = RESPONSES.singleTrial.getExperimentsDetails as unknown as ExperimentBase;

const TRIAL: TrialDetails = {
  autoRestarts: 0,
  experimentId: EXPERIMENT.id,
  hyperparameters: {},
  id: 42,
  startTime: '2026-01-01T00:00:00Z',
  state: RunState.Running,
  totalBatchesProcessed: 0,
  totalCheckpointSize: 0,
};

const setup = (fetchTrialDetails = vi.fn()) => {
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <ConfirmationProvider>
          <BrowserRouter>
            <TrialDetailsHeader
              experiment={EXPERIMENT}
              fetchTrialDetails={fetchTrialDetails}
              trial={TRIAL}
            />
          </BrowserRouter>
        </ConfirmationProvider>
      </ThemeProvider>
    </UIProvider>,
  );
  return { fetchTrialDetails };
};

const tensorBoardButton = () => screen.getByRole('button', { name: /TensorBoard/ });

describe('TrialDetailsHeader', () => {
  beforeEach(() => {
    vi.mocked(openTensorBoardSession).mockReset();
    vi.mocked(makeToast).mockReset();
  });

  it("opens the trial's TensorBoard session", async () => {
    vi.mocked(openTensorBoardSession).mockResolvedValue({
      command: { id: 'shell-1' } as never,
      warnings: [],
    });
    const { fetchTrialDetails } = setup();

    await userEvent.click(tensorBoardButton());
    expect(openTensorBoardSession).toHaveBeenCalledWith({
      trialIds: [TRIAL.id],
      workspaceId: EXPERIMENT.workspaceId,
    });
    await waitFor(() => expect(fetchTrialDetails).toHaveBeenCalled());
  });

  it("shows the master's text when the session is refused and stops loading", async () => {
    vi.mocked(openTensorBoardSession).mockRejectedValue(
      new DetError(new Error('refused'), {
        publicMessage: "trial '42' not found",
        publicSubject: 'Request launchShell failed.',
      }),
    );
    const { fetchTrialDetails } = setup();

    await userEvent.click(tensorBoardButton());
    await waitFor(() =>
      expect(makeToast).toHaveBeenCalledWith({
        description: "trial '42' not found",
        severity: 'Error',
        title: 'Unable to View TensorBoard',
      }),
    );
    await waitFor(() => expect(screen.queryByText('Loading')).not.toBeInTheDocument());
    expect(tensorBoardButton()).toBeEnabled();
    expect(fetchTrialDetails).not.toHaveBeenCalled();
  });
});
