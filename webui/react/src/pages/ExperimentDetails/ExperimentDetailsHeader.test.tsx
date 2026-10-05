import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { ExperimentBase, RunState } from 'types';

import RESPONSES from './ExperimentDetails.test.mock';
import ExperimentDetailsHeader from './ExperimentDetailsHeader';

vi.mock('services/api', () => ({
  activateExperiment: vi.fn(),
  archiveExperiment: vi.fn(),
  continueExperiment: vi.fn(),
  getExpTrials: vi.fn(() => Promise.resolve({ pagination: { total: 0 }, trials: [] })),
  getWorkspaceProjects: vi.fn(() => Promise.resolve({ pagination: { total: 0 }, projects: [] })),
  getWorkspaces: vi.fn(() => Promise.resolve({ pagination: { total: 0 }, workspaces: [] })),
  openOrCreateTensorBoard: vi.fn(),
  pauseExperiment: vi.fn(),
  unarchiveExperiment: vi.fn(),
}));
vi.mock('hooks/useFeature', () => ({ default: () => ({ isOn: () => false }) }));

const QUEUED: ExperimentBase = {
  ...(RESPONSES.singleTrial.getExperimentsDetails as unknown as ExperimentBase),
  endTime: undefined,
  jobSummary: { jobsAhead: 2, state: 'STATE_QUEUED' },
  state: RunState.Queued,
};

describe('ExperimentDetailsHeader', () => {
  it("links a queued experiment's job info to the cluster page", async () => {
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

    await userEvent.click(screen.getByRole('button', { name: 'Toggle expansion' }));
    const jobInfo = await screen.findByText('2 jobs ahead of this one');
    // The Jobs page now lists runs; the job queue is on the cluster page.
    expect(jobInfo.closest('a')).toHaveAttribute('href', '/clusters');
  });
});
