import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';

import type { ValidFeature } from 'hooks/useFeature';
import { handlePath } from 'routes/utils';
import {
  archiveExperiment,
  cancelExperiment,
  deleteExperiment,
  getExpTrials,
  killExperiment,
  patchExperiment,
  pauseExperiment,
  unarchiveExperiment,
} from 'services/api';
import { ProjectExperiment, RunState } from 'types';
import { isDangerMenuItem, menuLabels } from 'utils/tests/menu';

import ExperimentActionDropdown, { Action } from './ExperimentActionDropdown';
import { cell, experiment } from './ExperimentActionDropdown.test.mock';

const user = userEvent.setup();

const mockNavigatorClipboard = () => {
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: {
      readText: vi.fn(),
      writeText: vi.fn(),
    },
    writable: true,
  });
};

vi.mock('routes/utils', () => ({
  handlePath: vi.fn(),
  paths: {
    experimentDetails: (id: number) => `/experiments/${id}`,
    experimentResources: (id: number) => `/experiments/${id}/resources`,
    searchDetails: (id: number) => `/searches/${id}`,
    trialLogs: (trialId: number, experimentId: number) =>
      `/experiments/${experimentId}/trials/${trialId}/logs`,
  },
  serverAddress: () => 'http://localhost',
}));

const resources = vi.hoisted(() => ({ enabled: true }));
vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => resources.enabled }));

const flags = vi.hoisted(() => ({ flatRuns: true }));
vi.mock('hooks/useFeature', async (importOriginal) => {
  const actual = await importOriginal<typeof import('hooks/useFeature')>();
  return {
    ...actual,
    default: () => ({
      isOn: (feature: ValidFeature) =>
        feature === 'flat_runs' ? flags.flatRuns : actual.FEATURES[feature].defaultValue,
    }),
  };
});

vi.mock('services/api', () => ({
  archiveExperiment: vi.fn(),
  cancelExperiment: vi.fn(),
  deleteExperiment: vi.fn(),
  getExpTrials: vi.fn(),
  getWorkspaces: vi.fn(() => Promise.resolve({ workspaces: [] })),
  killExperiment: vi.fn(),
  patchExperiment: vi.fn(),
  pauseExperiment: vi.fn(),
  unarchiveExperiment: vi.fn(),
}));

const mocks = vi.hoisted(() => {
  return {
    canCreateExperiment: vi.fn(),
    canDeleteExperiment: vi.fn(),
    canModifyExperiment: vi.fn(),
    canModifyExperimentMetadata: vi.fn(),
    canMoveExperiment: vi.fn(),
    canViewExperimentArtifacts: vi.fn(),
  };
});

vi.mock('hooks/usePermissions', () => {
  const usePermissions = vi.fn(() => {
    return {
      canCreateExperiment: mocks.canCreateExperiment,
      canDeleteExperiment: mocks.canDeleteExperiment,
      canModifyExperiment: mocks.canModifyExperiment,
      canModifyExperimentMetadata: mocks.canModifyExperimentMetadata,
      canMoveExperiment: mocks.canMoveExperiment,
      canViewExperimentArtifacts: mocks.canViewExperimentArtifacts,
    };
  });
  return {
    default: usePermissions,
  };
});

const setup = (
  link?: string,
  state?: RunState,
  archived?: boolean,
  overrides: Partial<ProjectExperiment> = {},
) => {
  const onComplete = vi.fn();
  const onVisibleChange = vi.fn();
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ConfirmationProvider>
        <ExperimentActionDropdown
          cell={cell}
          experiment={{
            ...experiment,
            archived: archived === undefined ? experiment.archived : archived,
            state: state === undefined ? experiment.state : state,
            ...overrides,
          }}
          isContextMenu
          link={link}
          makeOpen
          onComplete={onComplete}
          onVisibleChange={onVisibleChange}>
          <div />
        </ExperimentActionDropdown>
      </ConfirmationProvider>
    </UIProvider>,
  );
  return {
    onComplete,
    onVisibleChange,
  };
};

const allowEverything = () => {
  Object.values(mocks).forEach((mock) => mock.mockImplementation(() => true));
};

beforeEach(() => {
  resources.enabled = true;
  flags.flatRuns = true;
});

describe('ExperimentActionDropdown', () => {
  beforeEach(() => {
    vi.mocked(handlePath).mockClear();
    vi.mocked(cancelExperiment).mockClear();
  });

  it('opens the experiment resource selector when artifacts are visible', async () => {
    mocks.canViewExperimentArtifacts.mockImplementation(() => true);
    setup();
    await user.click(screen.getByText(Action.ViewResources));
    expect(handlePath).toHaveBeenCalledWith(expect.anything(), {
      path: `/experiments/${experiment.id}/resources`,
    });
  });

  it('should provide Copy Data option', async () => {
    setup();
    mockNavigatorClipboard();
    await user.click(screen.getByText(Action.Copy));
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(cell.copyData);
  });

  it('should provide Link option', async () => {
    const link = 'https://www.google.com/';
    setup(link);
    await user.click(screen.getByText(Action.NewTab));
    const tabClick = vi.mocked(handlePath).mock.calls[0];
    expect(tabClick[0]).toMatchObject({ type: 'click' });
    expect(tabClick[1]).toMatchObject({
      path: link,
      popout: 'tab',
    });
    await user.click(screen.getByText(Action.NewWindow));
    const windowClick = vi.mocked(handlePath).mock.calls[1];
    expect(windowClick[0]).toMatchObject({ type: 'click' });
    expect(windowClick[1]).toMatchObject({
      path: link,
      popout: 'window',
    });
  });

  it('should provide Delete option', async () => {
    mocks.canDeleteExperiment.mockImplementation(() => true);
    setup();
    await user.click(screen.getByText(Action.Delete));
    await user.click(screen.getByRole('button', { name: Action.Delete }));
    expect(vi.mocked(deleteExperiment)).toBeCalled();
  });

  it('should hide Delete option without permissions', () => {
    mocks.canDeleteExperiment.mockImplementation(() => false);
    setup();
    expect(screen.queryByText(Action.Delete)).not.toBeInTheDocument();
  });

  it('should provide Kill option', async () => {
    mocks.canModifyExperiment.mockImplementation(() => true);
    setup(undefined, RunState.Paused, undefined);
    await user.click(screen.getByText(Action.Kill));
    await user.click(screen.getByRole('button', { name: Action.Kill }));
    expect(vi.mocked(killExperiment)).toBeCalled();
  });

  it('should hide Kill option without permissions', () => {
    mocks.canModifyExperiment.mockImplementation(() => false);
    setup(undefined, RunState.Paused, undefined);
    expect(screen.queryByText(Action.Kill)).not.toBeInTheDocument();
  });

  it('should provide Archive option', async () => {
    mocks.canModifyExperiment.mockImplementation(() => true);
    setup();
    await user.click(screen.getByText(Action.Archive));
    expect(vi.mocked(archiveExperiment)).toBeCalled();
  });

  it('should hide Archive option without permissions', () => {
    mocks.canModifyExperiment.mockImplementation(() => false);
    setup();
    expect(screen.queryByText(Action.Archive)).not.toBeInTheDocument();
  });

  it('should provide Unarchive option', async () => {
    mocks.canModifyExperiment.mockImplementation(() => true);
    setup(undefined, undefined, true);
    await user.click(screen.getByText(Action.Unarchive));
    expect(vi.mocked(unarchiveExperiment)).toBeCalled();
  });

  it('should hide Unarchive option without permissions', () => {
    mocks.canModifyExperiment.mockImplementation(() => false);
    setup(undefined, undefined, true);
    expect(screen.queryByText(Action.Unarchive)).not.toBeInTheDocument();
  });

  it('should provide Move option', () => {
    mocks.canMoveExperiment.mockImplementation(() => true);
    setup();
    expect(screen.getByText(Action.Move)).toBeInTheDocument();
  });

  it('should hide Move option without permissions', () => {
    mocks.canMoveExperiment.mockImplementation(() => false);
    setup();
    expect(screen.queryByText(Action.Move)).not.toBeInTheDocument();
  });

  it('should provide Edit option', async () => {
    mocks.canModifyExperimentMetadata.mockImplementation(() => true);
    setup();
    await user.click(screen.getByText(Action.Edit));
    await user.type(screen.getByRole('textbox', { name: 'Name' }), 'edit');
    await user.click(screen.getByText('Save'));
    expect(vi.mocked(patchExperiment)).toBeCalled();
  });

  it('should hide Edit option without permissions', () => {
    mocks.canModifyExperimentMetadata.mockImplementation(() => false);
    setup();
    expect(screen.queryByText(Action.Edit)).not.toBeInTheDocument();
  });

  it('should provide Pause option', async () => {
    mocks.canModifyExperiment.mockImplementation(() => true);
    setup(undefined, RunState.Running);
    await user.click(screen.getByText(Action.Pause));
    expect(vi.mocked(pauseExperiment)).toBeCalled();
  });

  it('should hide Pause option without permissions', () => {
    mocks.canModifyExperiment.mockImplementation(() => false);
    setup(undefined, RunState.Running);
    expect(screen.queryByText(Action.Pause)).not.toBeInTheDocument();
  });

  it('should provide Cancel option, labelled Stop, after a confirmation that is not red', async () => {
    mocks.canModifyExperiment.mockImplementation(() => true);
    const { onComplete } = setup(undefined, RunState.Running);
    expect(Action.Cancel).toBe('Stop');
    await user.click(screen.getByText(Action.Cancel));
    const stop = await screen.findByRole('button', { name: 'Stop' });
    expect(screen.getByText('Confirm Search Stop')).toBeInTheDocument();
    expect(
      screen.getByText(
        `Stop search ${experiment.id}? Its runs are asked to save a checkpoint and exit.`,
      ),
    ).toBeInTheDocument();
    expect(stop).not.toHaveClass('ant-btn-dangerous');
    expect(vi.mocked(cancelExperiment)).not.toBeCalled();
    await user.click(stop);
    expect(vi.mocked(cancelExperiment)).toBeCalledWith({ experimentId: experiment.id });
    expect(onComplete).toBeCalledWith(Action.Cancel, experiment.id);
  });

  it('names experiments and trials in the Stop confirmation without flat runs', async () => {
    flags.flatRuns = false;
    mocks.canModifyExperiment.mockImplementation(() => true);
    setup(undefined, RunState.Running);
    await user.click(screen.getByText(Action.Cancel));
    expect(await screen.findByText('Confirm Experiment Stop')).toBeInTheDocument();
    expect(
      screen.getByText(
        `Stop experiment ${experiment.id}? Its trials are asked to save a checkpoint and exit.`,
      ),
    ).toBeInTheDocument();
  });

  it('does not stop the experiment when the Stop confirmation is cancelled', async () => {
    mocks.canModifyExperiment.mockImplementation(() => true);
    setup(undefined, RunState.Running);
    await user.click(screen.getByText(Action.Cancel));
    await user.click(await screen.findByRole('button', { name: 'Cancel' }));
    expect(vi.mocked(cancelExperiment)).not.toBeCalled();
  });

  it('confirms Kill with a red button', async () => {
    mocks.canModifyExperiment.mockImplementation(() => true);
    setup(undefined, RunState.Running);
    await user.click(screen.getByText(Action.Kill));
    expect(await screen.findByRole('button', { name: Action.Kill })).toHaveClass(
      'ant-btn-dangerous',
    );
  });

  it('should hide Cancel option without permissions', () => {
    mocks.canModifyExperiment.mockImplementation(() => false);
    setup(undefined, RunState.Running);
    expect(screen.queryByText(Action.Cancel)).not.toBeInTheDocument();
  });

  it('should provide Retain Logs option', () => {
    mocks.canModifyExperiment.mockImplementation(() => true);
    setup();
    expect(screen.queryByText(Action.RetainLogs)).toBeInTheDocument();
  });

  it('should hide Retain Logs option without permissions', () => {
    mocks.canModifyExperiment.mockImplementation(() => false);
    setup();
    expect(screen.queryByText(Action.RetainLogs)).not.toBeInTheDocument();
  });

  it('should provide Tensor Board option', () => {
    mocks.canViewExperimentArtifacts.mockImplementation(() => true);
    setup();
    expect(screen.getByText(Action.OpenTensorBoard)).toBeInTheDocument();
  });

  it('should hide Tensor Board option without permissions', () => {
    mocks.canViewExperimentArtifacts.mockImplementation(() => false);
    setup();
    expect(screen.queryByText(Action.OpenTensorBoard)).not.toBeInTheDocument();
  });
});

describe('ExperimentActionDropdown order', () => {
  beforeEach(allowEverything);

  it('lists an active experiment’s actions in the fixed order', () => {
    setup('/experiments/7261', RunState.Running, false);
    expect(menuLabels()).toEqual([
      Action.NewTab,
      Action.NewWindow,
      Action.Copy,
      'View Logs',
      'View Resources',
      'View in TensorBoard',
      'Copy Experiment ID',
      'Pause',
      'Edit',
      'Move',
      'Hyperparameter Search',
      'Retain Logs',
      'Stop',
      'Kill',
    ]);
  });

  it('puts Resume where Pause was for a paused experiment', () => {
    setup(undefined, RunState.Paused, false);
    expect(menuLabels()).toEqual([
      Action.Copy,
      'View Logs',
      'View Resources',
      'View in TensorBoard',
      'Copy Experiment ID',
      'Resume',
      'Edit',
      'Move',
      'Hyperparameter Search',
      'Retain Logs',
      'Stop',
      'Kill',
    ]);
  });

  it('ends an archived experiment’s menu with Unarchive and Delete', () => {
    setup(undefined, RunState.Canceled, true);
    expect(menuLabels()).toEqual([
      Action.Copy,
      'View Logs',
      'View Resources',
      'View in TensorBoard',
      'Copy Experiment ID',
      'Hyperparameter Search',
      'Retain Logs',
      'Unarchive',
      'Delete',
    ]);
  });

  it('ends a finished experiment’s menu with Archive and Delete', () => {
    setup(undefined, RunState.Completed, false);
    expect(menuLabels().slice(-6)).toEqual([
      'Edit',
      'Move',
      'Hyperparameter Search',
      'Retain Logs',
      'Archive',
      'Delete',
    ]);
  });

  it('offers View Logs and Copy Experiment ID without any permission', () => {
    Object.values(mocks).forEach((mock) => mock.mockImplementation(() => false));
    setup(undefined, RunState.Running, false);
    expect(menuLabels()).toEqual([Action.Copy, 'View Logs', 'Copy Experiment ID']);
  });
});

describe('ExperimentActionDropdown View Logs', () => {
  beforeEach(() => {
    allowEverything();
    vi.mocked(handlePath).mockClear();
    vi.mocked(getExpTrials).mockReset();
  });

  const viewLogsPath = async (overrides: Partial<ProjectExperiment>) => {
    setup(undefined, RunState.Running, false, overrides);
    await user.click(screen.getByText(Action.ViewLogs));
    // View Logs may first fetch the trial, so it navigates once that is done.
    await waitFor(() => expect(handlePath).toHaveBeenCalled());
    expect(handlePath).toHaveBeenCalledTimes(1);
    return vi.mocked(handlePath).mock.calls[0][1]?.path;
  };

  const mockTrial = (id: number) =>
    vi.mocked(getExpTrials).mockResolvedValue({
      pagination: { limit: 1, offset: 0, total: 1 },
      trials: [{ id } as Awaited<ReturnType<typeof getExpTrials>>['trials'][number]],
    });

  // List rows carry the raw experiment config, as the API sends it.
  const randomOneTrialConfig = {
    searcher: { max_trials: 1, metric: 'loss', name: 'random', smaller_is_better: true },
  } as unknown as ProjectExperiment['config'];

  it('fetches the trial of a single-trial experiment and opens its logs page', async () => {
    mockTrial(7);
    expect(await viewLogsPath({ numTrials: 1, searcherType: 'single', trialIds: [] })).toBe(
      `/experiments/${experiment.id}/trials/7/logs`,
    );
    expect(getExpTrials).toHaveBeenCalledWith({ id: experiment.id, limit: 1 });
  });

  it('fetches the trial when the config allows a single trial', async () => {
    mockTrial(7);
    expect(
      await viewLogsPath({
        config: randomOneTrialConfig,
        numTrials: 1,
        searcherType: 'random',
        trialIds: [],
      }),
    ).toBe(`/experiments/${experiment.id}/trials/7/logs`);
  });

  it.each([
    [true, `/searches/${experiment.id}`],
    [false, `/experiments/${experiment.id}/logs`],
  ])(
    'opens the search page or, without flat runs, the experiment’s Logs tab when the trial cannot be fetched (flat runs %s)',
    async (flatRuns, path) => {
      flags.flatRuns = flatRuns;
      vi.mocked(getExpTrials).mockRejectedValue(new Error('no trials'));
      expect(
        await viewLogsPath({
          config: randomOneTrialConfig,
          numTrials: 1,
          searcherType: 'random',
          trialIds: [],
        }),
      ).toBe(path);
    },
  );

  it.each([
    [true, `/searches/${experiment.id}`],
    [false, `/experiments/${experiment.id}/logs`],
  ])(
    'opens the search page or, without flat runs, the experiment’s Logs tab when no trial comes back (flat runs %s)',
    async (flatRuns, path) => {
      flags.flatRuns = flatRuns;
      vi.mocked(getExpTrials).mockResolvedValue({
        pagination: { limit: 1, offset: 0, total: 0 },
        trials: [],
      });
      expect(await viewLogsPath({ numTrials: 1, searcherType: 'single', trialIds: [] })).toBe(path);
    },
  );

  it('opens the trial’s logs page when the trial ID is known', async () => {
    expect(await viewLogsPath({ numTrials: 1, searcherType: 'random', trialIds: [42] })).toBe(
      `/experiments/${experiment.id}/trials/42/logs`,
    );
    expect(getExpTrials).not.toHaveBeenCalled();
  });

  it('opens the trials tab of an experiment with several trials', async () => {
    expect(await viewLogsPath({ numTrials: 5, searcherType: 'random', trialIds: [] })).toBe(
      `/experiments/${experiment.id}/trials`,
    );
    expect(getExpTrials).not.toHaveBeenCalled();
  });

  it('opens the trials tab of a search that has started only one trial so far', async () => {
    expect(await viewLogsPath({ numTrials: 1, searcherType: 'adaptive_asha', trialIds: [] })).toBe(
      `/experiments/${experiment.id}/trials`,
    );
    expect(getExpTrials).not.toHaveBeenCalled();
  });

  it('is left out while the experiment has no trial', () => {
    setup(undefined, RunState.Running, false, { numTrials: 0 });
    expect(screen.queryByText(Action.ViewLogs)).not.toBeInTheDocument();
    expect(screen.getByText(Action.CopyExperimentID)).toBeInTheDocument();
  });
});

describe('ExperimentActionDropdown View Resources', () => {
  beforeEach(allowEverything);

  it('is offered when task resources are enabled', () => {
    resources.enabled = true;
    setup();
    expect(screen.getByText(Action.ViewResources)).toBeInTheDocument();
  });

  it('is left out when task resources are not enabled', () => {
    resources.enabled = false;
    setup();
    expect(screen.queryByText(Action.ViewResources)).not.toBeInTheDocument();
    expect(screen.getByText(Action.OpenTensorBoard)).toBeInTheDocument();
  });
});

describe('ExperimentActionDropdown Copy Experiment ID', () => {
  it('copies the experiment ID', async () => {
    setup();
    mockNavigatorClipboard();
    await user.click(screen.getByText(Action.CopyExperimentID));
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(String(experiment.id));
    expect(await screen.findByText('Experiment ID has been copied to clipboard.')).toBeVisible();
  });
});

describe('ExperimentActionDropdown red items', () => {
  beforeEach(allowEverything);

  it('shows Kill in red and the other actions of an active experiment not', () => {
    setup(undefined, RunState.Running, false);
    expect(isDangerMenuItem(Action.Kill)).toBe(true);
    [Action.ViewLogs, Action.Pause, Action.Cancel, Action.Move].forEach((label) =>
      expect(isDangerMenuItem(label)).toBe(false),
    );
  });

  it('shows Delete in red and Archive not', () => {
    setup(undefined, RunState.Completed, false);
    expect(isDangerMenuItem(Action.Delete)).toBe(true);
    expect(isDangerMenuItem(Action.Archive)).toBe(false);
  });
});
