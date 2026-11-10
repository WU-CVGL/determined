import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { DndProvider } from 'react-dnd';
import { HTML5Backend } from 'react-dnd-html5-backend';
import { HelmetProvider } from 'react-helmet-async';

import { ThemeProvider } from 'components/ThemeProvider';
import usePageVisibility from 'hooks/usePageVisibility';
import {
  getExperimentDetails,
  getExpTrials,
  getExpValidationHistory,
  getProject,
  getTrialDetails,
  getWorkspace,
} from 'services/api';
import {} from 'stores/cluster';
import { RunState } from 'types';

import ExperimentDetails, { ERROR_MESSAGE, INVALID_ID_MESSAGE } from './ExperimentDetails';
import RESPONSES from './ExperimentDetails.test.mock';

vi.useFakeTimers();
/**
 * Setup mock functions in a way that the responses can
 * be overridden dynamically between test sections.
 * The idea is to import the function from the module,
 * mock the module and replace the function(s) with vi.fn(),
 * then override the implementation or return value
 */
const { BrowserRouter, useParams } = await import('react-router-dom');
vi.mock('react-router-dom', async (importOriginal) => ({
  ...(await importOriginal<typeof import('react-router-dom')>()),
  unstable_useBlocker: () => ({ state: 'unblocked' }),
  useParams: vi.fn(),
}));

vi.mock('hooks/useFeature', () => {
  return {
    default: () => ({
      isOn() {
        return false;
      },
    }),
  };
});

// Lets a test grant the experiment permissions that the default user has not got.
const grants = vi.hoisted(() => ({ experiment: false }));
vi.mock('hooks/usePermissions', async (importOriginal) => {
  const actual = await importOriginal<typeof import('hooks/usePermissions')>();
  return {
    ...actual,
    default: () => {
      const permissions = actual.default();
      return grants.experiment
        ? {
            ...permissions,
            canCreateExperiment: () => true,
            canModifyExperimentMetadata: () => true,
            canViewExperimentArtifacts: () => true,
          }
        : permissions;
    },
  };
});

vi.mock('services/api', () => ({
  getExperimentDetails: vi.fn(),
  getExpTrials: vi.fn(),
  getExpValidationHistory: vi.fn(),
  getProject: vi.fn(),
  getResourcePools: vi.fn().mockReturnValue(Promise.resolve([])),
  getTrialDetails: vi.fn(),
  getWorkspace: vi.fn(),
  getWorkspaceProjects: vi.fn().mockReturnValue({ projects: [] }),
  getWorkspaces: vi.fn().mockResolvedValue({ workspaces: [] }),
}));

vi.mock('routes/utils', async (importOriginal) => ({
  ...(await importOriginal<typeof import('routes/utils')>()),
  serverAddress: () => 'http://localhost',
}));

/**
 * TODO: Temporarily mock ExperimentVisualization module.
 * This is a challenging module to test as it has `readStream` calls.
 * For now, simply return a simple placeholder.
 */
vi.mock('./ExperimentVisualization', () => ({
  __esModule: true,
  default: () => <div>Experiment Visualization</div>,
}));

const PageVisibility = () => {
  usePageVisibility();
  return null;
};

const setPageHidden = (hidden: boolean) =>
  act(() => {
    Object.defineProperty(document, 'hidden', { configurable: true, value: hidden });
    document.dispatchEvent(new Event('visibilitychange'));
  });

const setup = () => {
  const view = render(
    <UIProvider theme={DefaultTheme.Light}>
      <ConfirmationProvider>
        <ThemeProvider>
          <PageVisibility />
          <HelmetProvider>
            <BrowserRouter>
              <DndProvider backend={HTML5Backend}>
                <ExperimentDetails />
              </DndProvider>
            </BrowserRouter>
          </HelmetProvider>
        </ThemeProvider>
      </ConfirmationProvider>
    </UIProvider>,
  );
  return { view };
};

describe('Experiment Details Page', () => {
  describe('Invalid Experiment ID', () => {
    const INVALID_ID = 'beadbead';

    beforeAll(() => {
      vi.mocked(useParams).mockReturnValue({ experimentId: INVALID_ID });
    });

    it('should show invalid experiment page without id', async () => {
      setup();
      const invalidMessage = await screen.findByText(`${INVALID_ID_MESSAGE} ${INVALID_ID}`);
      expect(invalidMessage).toBeInTheDocument();
    });
  });

  describe('Missing Experiment', () => {
    const NON_EXISTING_ID = 9999;

    beforeAll(() => {
      vi.mocked(useParams).mockReturnValue({ experimentId: `${NON_EXISTING_ID}` });
      vi.mocked(getExperimentDetails).mockRejectedValue(new Error('Fetch Error'));
    });

    it('should show experiment is unfetchable', async () => {
      setup();
      const errorMessage = await screen.findByText(`${ERROR_MESSAGE} ${NON_EXISTING_ID}`);
      expect(errorMessage).toBeInTheDocument();
    });
  });

  describe('Single Trial Experiment', () => {
    beforeAll(() => {
      vi.mocked(useParams).mockReturnValue({ experimentId: '1241' });
      vi.mocked(getExperimentDetails).mockResolvedValue(
        RESPONSES.singleTrial.getExperimentsDetails,
      );
      vi.mocked(getExpValidationHistory).mockResolvedValue(
        RESPONSES.singleTrial.getExpValidationHistory,
      );
      vi.mocked(getExpTrials).mockResolvedValue(RESPONSES.singleTrial.getExpTrials);
      vi.mocked(getProject).mockResolvedValue(RESPONSES.singleTrial.getProject);
      vi.mocked(getTrialDetails).mockResolvedValue(RESPONSES.singleTrial.getTrialDetails);
      vi.mocked(getWorkspace).mockResolvedValue(RESPONSES.multiTrial.getWorkspace);
    });

    it('should show single trial experiment page with id', async () => {
      setup();

      const experimentId = RESPONSES.singleTrial.getExperimentsDetails.id;
      const experimentName = RESPONSES.singleTrial.getExperimentsDetails.name;
      await waitFor(() => {
        expect(screen.getByText(`Experiment ${experimentId}`)).toBeInTheDocument();
        expect(screen.getByRole('experimentName')).toHaveTextContent(experimentName);
      });

      expect(screen.getByText('Overview')).toBeInTheDocument();
      expect(screen.getByText('Hyperparameters')).toBeInTheDocument();
      expect(screen.getByText('Logs')).toBeInTheDocument();
    });
  });

  describe('Multi-Trial Experiment', () => {
    beforeAll(() => {
      vi.mocked(useParams).mockReturnValue({ experimentId: '1249' });
      vi.mocked(getExperimentDetails).mockResolvedValue(RESPONSES.multiTrial.getExperimentsDetails);
      vi.mocked(getExpValidationHistory).mockResolvedValue(
        RESPONSES.multiTrial.getExpValidationHistory,
      );
      vi.mocked(getExpTrials).mockResolvedValue(RESPONSES.multiTrial.getExpTrials);
      vi.mocked(getProject).mockResolvedValue(RESPONSES.multiTrial.getProject);
      vi.mocked(getWorkspace).mockResolvedValue(RESPONSES.multiTrial.getWorkspace);
    });

    it('should show multi-trial experiment page with id', async () => {
      setup();

      const experimentId = RESPONSES.multiTrial.getExperimentsDetails.id;
      const experimentName = RESPONSES.multiTrial.getExperimentsDetails.name;
      await waitFor(() => {
        expect(screen.getByText(`Experiment ${experimentId}`)).toBeInTheDocument();
        expect(screen.getByRole('experimentName')).toHaveTextContent(experimentName);
      });

      expect(screen.getByText('Visualization')).toBeInTheDocument();
      expect(screen.getAllByText('Trials').length).toBeGreaterThan(0);
    });

    it('reads an ended experiment again on a return to the page, and keeps it on a failure', async () => {
      const ended = { ...RESPONSES.multiTrial.getExperimentsDetails, state: RunState.Completed };
      vi.mocked(getExperimentDetails).mockResolvedValue(ended);
      setup();
      await waitFor(() =>
        expect(screen.getByRole('experimentName')).toHaveTextContent(ended.name ?? ''),
      );
      const reads = vi.mocked(getExperimentDetails).mock.calls.length;

      vi.mocked(getExperimentDetails).mockRejectedValueOnce(new Error('Fetch Error'));
      setPageHidden(true);
      setPageHidden(false);
      await waitFor(() => expect(getExperimentDetails).toHaveBeenCalledTimes(reads + 1));
      await act(() => Promise.resolve());
      expect(screen.queryByText(`${ERROR_MESSAGE} ${ended.id}`)).not.toBeInTheDocument();
      expect(screen.getByRole('experimentName')).toHaveTextContent(ended.name ?? '');
      Object.defineProperty(document, 'hidden', { configurable: true, value: false });
    });
  });

  describe('Single Trial Experiment whose title and config are not shared', () => {
    const shown = RESPONSES.singleTrial.getExperimentsDetails;
    const hidden = {
      ...shown,
      capabilities: { manageSharing: false, viewConfig: false, viewLogs: true, viewTitle: false },
      config: undefined,
      configRaw: undefined,
      description: '',
      labels: [],
      name: '',
      notes: '',
    } as unknown as typeof shown;

    beforeAll(() => {
      grants.experiment = true;
      vi.mocked(getExpValidationHistory).mockResolvedValue(
        RESPONSES.singleTrial.getExpValidationHistory,
      );
      vi.mocked(getExpTrials).mockResolvedValue(RESPONSES.singleTrial.getExpTrials);
      vi.mocked(getProject).mockResolvedValue(RESPONSES.singleTrial.getProject);
      vi.mocked(getTrialDetails).mockResolvedValue(RESPONSES.singleTrial.getTrialDetails);
      vi.mocked(getWorkspace).mockResolvedValue(RESPONSES.multiTrial.getWorkspace);
    });

    afterAll(() => {
      grants.experiment = false;
    });

    const open = async (experiment: typeof shown, tab: string) => {
      vi.mocked(useParams).mockReturnValue({ experimentId: `${shown.id}`, tab });
      vi.mocked(getExperimentDetails).mockResolvedValue(experiment);
      const { view } = setup();
      // The breadcrumb names an experiment whose title is not shared by its ID too.
      await waitFor(() =>
        expect(screen.getAllByText(`Experiment ${shown.id}`).length).toBeGreaterThan(0),
      );
      return view;
    };

    it('shows the single trial tabs from the searcher type', async () => {
      await open(hidden, 'hyperparameters');
      expect(screen.getByText('Hyperparameters')).toBeInTheDocument();
      expect(screen.queryByText('Visualization')).not.toBeInTheDocument();
    });

    it('offers no hyperparameter search', async () => {
      const view = await open(shown, 'hyperparameters');
      await waitFor(() =>
        expect(screen.getByRole('button', { name: 'Hyperparameter Search' })).toBeInTheDocument(),
      );
      view.unmount();

      await open(hidden, 'hyperparameters');
      expect(
        screen.queryByRole('button', { name: 'Hyperparameter Search' }),
      ).not.toBeInTheDocument();
    });

    it('keeps the notes read-only', async () => {
      const view = await open(shown, 'notes');
      await waitFor(() => expect(screen.getByLabelText('Edit')).toBeInTheDocument());
      view.unmount();

      await open(hidden, 'notes');
      expect(screen.getByText('Notes')).toBeInTheDocument();
      expect(screen.queryByLabelText('Edit')).not.toBeInTheDocument();
    });
  });

  describe('Multi-Trial Experiment whose title is not shared', () => {
    const shown = RESPONSES.multiTrial.getExperimentsDetails;
    const hidden = {
      ...shown,
      capabilities: { manageSharing: false, viewConfig: true, viewLogs: true, viewTitle: false },
      description: '',
      labels: [],
      name: '',
      notes: '',
    } as unknown as typeof shown;

    beforeAll(() => {
      grants.experiment = true;
      vi.mocked(getExpValidationHistory).mockResolvedValue(
        RESPONSES.multiTrial.getExpValidationHistory,
      );
      vi.mocked(getExpTrials).mockResolvedValue(RESPONSES.multiTrial.getExpTrials);
      vi.mocked(getProject).mockResolvedValue(RESPONSES.multiTrial.getProject);
      vi.mocked(getWorkspace).mockResolvedValue(RESPONSES.multiTrial.getWorkspace);
    });

    afterAll(() => {
      grants.experiment = false;
    });

    it('offers no hyperparameter search from the trials when the config is not shared', async () => {
      const openTrials = async (experiment: typeof shown) => {
        vi.mocked(useParams).mockReturnValue({ experimentId: `${shown.id}`, tab: 'trials' });
        vi.mocked(getExperimentDetails).mockResolvedValue(experiment);
        const { view } = setup();
        await waitFor(() => expect(screen.getByText('Trial 3581')).toBeInTheDocument());
        return view;
      };
      const menuItems = async (open: () => void) => {
        open();
        await waitFor(() => expect(screen.getAllByRole('menuitem').length).toBeGreaterThan(0));
        return screen.getAllByRole('menuitem').map((item) => item.textContent);
      };
      const row = () => screen.getByText('Trial 3581').closest('tr') as HTMLElement;
      const rowMenu = () =>
        fireEvent.click(within(row()).getByRole('button', { name: 'Action menu' }));
      const contextMenu = () => fireEvent.contextMenu(screen.getByText('Trial 3581'));
      const configHidden = {
        ...shown,
        capabilities: { manageSharing: false, viewConfig: false, viewLogs: true, viewTitle: true },
        config: undefined,
        configRaw: undefined,
      } as unknown as typeof shown;

      for (const open of [rowMenu, contextMenu]) {
        const view = await openTrials(shown);
        expect(await menuItems(open)).toContain('Hyperparameter Search');
        view.unmount();

        const hiddenView = await openTrials(configHidden);
        expect(await menuItems(open)).not.toContain('Hyperparameter Search');
        hiddenView.unmount();
      }
    });

    it('keeps the notes read-only', async () => {
      const open = async (experiment: typeof shown) => {
        vi.mocked(useParams).mockReturnValue({ experimentId: `${shown.id}`, tab: 'notes' });
        vi.mocked(getExperimentDetails).mockResolvedValue(experiment);
        const { view } = setup();
        await waitFor(() =>
          expect(screen.getAllByText(`Experiment ${shown.id}`).length).toBeGreaterThan(0),
        );
        return view;
      };
      const view = await open(shown);
      await waitFor(() => expect(screen.getByLabelText('Edit')).toBeInTheDocument());
      view.unmount();

      await open(hidden);
      expect(screen.getByText('Notes')).toBeInTheDocument();
      expect(screen.queryByLabelText('Edit')).not.toBeInTheDocument();
    });
  });
});
