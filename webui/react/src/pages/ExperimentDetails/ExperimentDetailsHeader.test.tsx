import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { makeToast } from 'hew/Toast';
import { ConfirmationProvider } from 'hew/useConfirm';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { ExperimentBase, RunState } from 'types';
import { DetError } from 'utils/error';
import { openTensorBoardSession } from 'utils/tensorBoardSession';

import RESPONSES from './ExperimentDetails.test.mock';
import ExperimentDetailsHeader from './ExperimentDetailsHeader';

vi.mock('services/api', () => ({
  activateExperiment: vi.fn(),
  archiveExperiment: vi.fn(),
  continueExperiment: vi.fn(),
  getExpTrials: vi.fn(() => Promise.resolve({ pagination: { total: 0 }, trials: [] })),
  getWorkspaceProjects: vi.fn(() => Promise.resolve({ pagination: { total: 0 }, projects: [] })),
  getWorkspaces: vi.fn(() => Promise.resolve({ pagination: { total: 0 }, workspaces: [] })),
  pauseExperiment: vi.fn(),
  unarchiveExperiment: vi.fn(),
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

const QUEUED: ExperimentBase = {
  ...(RESPONSES.singleTrial.getExperimentsDetails as unknown as ExperimentBase),
  endTime: undefined,
  jobSummary: { jobsAhead: 2, state: 'STATE_QUEUED' },
  state: RunState.Queued,
};

const setup = () =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <ConfirmationProvider>
          <BrowserRouter>
            <ExperimentDetailsHeader experiment={QUEUED} fetchExperimentDetails={vi.fn()} />
          </BrowserRouter>
        </ConfirmationProvider>
      </ThemeProvider>
    </UIProvider>,
  );

const tensorBoardButton = () => screen.getByRole('button', { name: /TensorBoard/ });

describe('ExperimentDetailsHeader', () => {
  beforeEach(() => {
    vi.mocked(openTensorBoardSession).mockReset();
    vi.mocked(makeToast).mockReset();
  });

  it("links a queued experiment's job info to the cluster page", async () => {
    setup();

    await userEvent.click(screen.getByRole('button', { name: 'Toggle expansion' }));
    const jobInfo = await screen.findByText('2 jobs ahead of this one');
    // The Jobs page now lists runs; the job queue is on the cluster page.
    expect(jobInfo.closest('a')).toHaveAttribute('href', '/clusters');
  });

  it("opens the experiment's TensorBoard session", async () => {
    vi.mocked(openTensorBoardSession).mockResolvedValue({
      command: { id: 'shell-1' } as never,
      warnings: [],
    });
    setup();

    await userEvent.click(tensorBoardButton());
    expect(openTensorBoardSession).toHaveBeenCalledWith({
      experimentIds: [QUEUED.id],
      workspaceId: QUEUED.workspaceId,
    });
    expect(makeToast).not.toHaveBeenCalled();
  });

  it("shows the master's text when the session is refused and stops loading", async () => {
    vi.mocked(openTensorBoardSession).mockRejectedValue(
      new DetError(new Error('refused'), {
        publicMessage: 'permission denied',
        publicSubject: 'Request launchShell failed.',
      }),
    );
    setup();

    await userEvent.click(tensorBoardButton());
    await waitFor(() =>
      expect(makeToast).toHaveBeenCalledWith({
        description: 'permission denied',
        severity: 'Error',
        title: 'Unable to View TensorBoard',
      }),
    );
    await waitFor(() => expect(screen.queryByText('Loading')).not.toBeInTheDocument());
    expect(tensorBoardButton()).toBeEnabled();
  });
});
