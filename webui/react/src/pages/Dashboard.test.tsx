import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import UIProvider, { DefaultTheme } from 'hew/Theme';
import { useEffect } from 'react';
import { HelmetProvider } from 'react-helmet-async';

import { ThemeProvider } from 'components/ThemeProvider';
import usePermissions from 'hooks/usePermissions';
import authStore from 'stores/auth';
import userStore from 'stores/users';
import { DetailedUser } from 'types';
import handleError from 'utils/error';

import Dashboard from './Dashboard';

const mocks = vi.hoisted(() => {
  return {
    getCommands: vi.fn().mockImplementation(() => Promise.resolve([])),
    getExperiments: vi.fn().mockImplementation(() =>
      Promise.resolve({
        experiments: [],
      }),
    ),
    getProjectsByUserActivity: vi.fn().mockImplementation(() => Promise.resolve([])),
    getShells: vi.fn().mockImplementation(() => Promise.resolve([])),
  };
});

vi.mock('services/api', () => ({
  // for the launch forms:
  getAvailableResourcePools: () => Promise.resolve([]),
  getCommands: mocks.getCommands,
  getExperiments: mocks.getExperiments,
  getProjectsByUserActivity: mocks.getProjectsByUserActivity,
  getShells: mocks.getShells,
  getTaskTemplates: () => Promise.resolve([]),
  getWorkspaces: () => Promise.resolve({ workspaces: [] }),
  updateUserSetting: () => Promise.resolve(),
}));

vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: vi.fn(),
}));

vi.mock('pages/UserProfile/UserOverview', () => ({
  default: ({ userId }: { userId: number }) => <div data-testid="user-overview">{userId}</div>,
}));

const CURRENT_USER: DetailedUser = { id: 1, isActive: true, isAdmin: false, username: 'bunny' };
const RENAMED_USER: DetailedUser = { ...CURRENT_USER, displayName: 'Bunny' };

vi.mock('hooks/usePermissions', () => {
  const usePermissions = vi.fn(() => {
    return {
      canCreateNSC: false,
    };
  });
  return {
    default: usePermissions,
  };
});

// for the launch form:
vi.mock('hooks/useSettings', async (importOriginal) => {
  const useSettings = vi.fn(() => ({ isLoading: false, settings: {}, updateSettings: vi.fn() }));

  return {
    __esModule: true,
    ...(await importOriginal<typeof import('hooks/useSettings')>()),
    useSettings,
  };
});

const Container: React.FC = () => {
  useEffect(() => {
    authStore.setAuth({ isAuthenticated: true });
    authStore.setAuthChecked();
    userStore.updateCurrentUser(CURRENT_USER);
  }, []);

  return <Dashboard />;
};

const setup = () => {
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <HelmetProvider>
          <Container />
        </HelmetProvider>
      </ThemeProvider>
    </UIProvider>,
  );
};

describe('Dashboard', () => {
  it("shows the signed-in user's overview between the header and the sections", async () => {
    setup();

    const overview = await screen.findByTestId('user-overview');
    expect(overview).toHaveTextContent(/^1$/);
    const submissions = await screen.findByText('Your Recent Submissions');
    const launch = screen.getByRole('button', { name: 'Launch Interactive Task' });
    const follows = (a: Node, b: Node) =>
      !!(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
    expect(follows(launch, overview)).toBe(true);
    expect(follows(overview, submissions)).toBe(true);
  });

  it('renders empty state', async () => {
    setup();

    await waitFor(() => {
      expect(screen.getByText('Your Recent Submissions')).toBeInTheDocument();
      expect(screen.getByText('No submissions')).toBeInTheDocument();
      expect(
        screen.getByText('Your recent searches and tasks will show up here.'),
      ).toBeInTheDocument();
      expect(screen.getByText('Get started')).toBeInTheDocument();
      expect(screen.queryByText('Recently Viewed Projects')).not.toBeInTheDocument();
    });
  });

  it('renders ProjectCards with project data', async () => {
    mocks.getProjectsByUserActivity.mockImplementation(() =>
      Promise.resolve([
        {
          archived: false,
          description: '',
          errorMessage: '',
          id: 1,
          immutable: true,
          key: '',
          lastExperimentStartedAt: '2024-07-17T16:18:56.813686Z',
          name: 'Uncategorized',
          notes: [
            {
              contents: '',
              name: 'Untitled',
            },
          ],
          numActiveExperiments: 0,
          numExperiments: 1297,
          numRuns: 41995,
          state: 'UNSPECIFIED',
          userId: 1,
          username: 'admin',
          workspaceId: 1,
          workspaceName: 'Uncategorized',
        },
      ]),
    );

    setup();

    await waitFor(() => {
      expect(screen.queryByText('Recently Viewed Projects')).toBeInTheDocument();
      expect(screen.getByText('Uncategorized')).toBeInTheDocument();
    });
  });
  it('renders submissions', async () => {
    mocks.getShells.mockImplementation(() =>
      Promise.resolve([
        {
          displayName: 'Test',
          id: 'dbf259fa-611a-4940-85c5-92463b289835',
          name: 'Shell (eminently-moving-oryx)',
          resourcePool: 'aux-pool',
          startTime: '2024-07-18T15:13:07.401Z',
          state: 'RUNNING',
          type: 'shell',
          userId: 1354,
          workspaceId: 1,
        },
      ]),
    );
    mocks.getExperiments.mockImplementation(() =>
      Promise.resolve({
        experiments: [
          {
            archived: false,
            checkpoints: 0,
            checkpointSize: 0,
            config: {
              bind_mounts: [],
              checkpoint_policy: 'best',
              checkpoint_storage: {
                access_key: null,
                bucket: 'det-determined-main-us-west-2-573932760021',
                endpoint_url: null,
                prefix: null,
                save_experiment_best: 0,
                save_trial_best: 1,
                save_trial_latest: 1,
                secret_key: null,
                type: 's3',
              },
              data: {},
              debug: false,
              description: '',
              entrypoint: 'python3 -m determined.launch.torch_distributed python3 train.py',
              environment: {
                add_capabilities: [],
                drop_capabilities: [],
                environment_variables: {
                  cpu: [],
                  cuda: [],
                  rocm: [],
                },
                force_pull_image: false,
                image: {
                  cpu: 'determinedai/pytorch-tensorflow-cpu-dev:8b3bea3',
                  cuda: 'determinedai/pytorch-tensorflow-cuda-dev:8b3bea3',
                  rocm: 'determinedai/environments:rocm-5.0-pytorch-1.10-tf-2.7-rocm-622d512',
                },
                pod_spec: null,
                ports: {},
                proxy_ports: [],
                registry_auth: null,
              },
              hyperparameters: {
                dropout1: {
                  maxval: 0.8,
                  minval: 0.2,
                  type: 'double',
                },
                dropout2: {
                  maxval: 0.8,
                  minval: 0.2,
                  type: 'double',
                },
                learning_rate: {
                  maxval: 2,
                  minval: 0.0002,
                  type: 'double',
                },
                n_filters1: {
                  maxval: 64,
                  minval: 8,
                  type: 'int',
                },
                n_filters2: {
                  maxval: 72,
                  minval: 8,
                  type: 'int',
                },
              },
              integrations: null,
              labels: [],
              log_policies: [],
              max_restarts: 5,
              min_checkpoint_period: {
                batches: 0,
              },
              min_validation_period: {
                batches: 0,
              },
              name: 'mnist_pytorch_dist_random_search',
              optimizations: {
                aggregation_frequency: 1,
                auto_tune_tensor_fusion: false,
                average_aggregated_gradients: true,
                average_training_metrics: true,
                grad_updates_size_file: null,
                gradient_compression: false,
                mixed_precision: 'O0',
                tensor_fusion_cycle_time: 1,
                tensor_fusion_threshold: 64,
              },
              pbs: {},
              perform_initial_validation: false,
              profiling: {
                begin_on_batch: 0,
                enabled: false,
                end_after_batch: null,
                sync_timings: true,
              },
              project: 'Uncategorized',
              records_per_epoch: 0,
              reproducibility: {
                experiment_seed: 1717172336,
              },
              resources: {
                devices: [],
                is_single_node: null,
                max_slots: null,
                native_parallel: false,
                priority: null,
                resource_pool: 'compute-pool',
                shm_size: null,
                slots_per_trial: 1,
                weight: 1,
              },
              scheduling_unit: 100,
              searcher: {
                max_concurrent_trials: 16,
                max_trials: 2,
                metric: 'accuracy',
                name: 'random',
                smaller_is_better: true,
                source_checkpoint_uuid: null,
                source_trial_id: null,
              },
              slurm: {},
              workspace: 'Uncategorized',
            },
            description: '',
            duration: 35,
            endTime: '2024-06-14T15:51:34.475103Z',
            forkedFrom: 6832,
            hyperparameters: {
              dropout1: {
                maxval: 0.8,
                minval: 0.2,
                type: 'double',
              },
              dropout2: {
                maxval: 0.8,
                minval: 0.2,
                type: 'double',
              },
              learning_rate: {
                maxval: 2,
                minval: 0.0002,
                type: 'double',
              },
              n_filters1: {
                maxval: 64,
                minval: 8,
                type: 'int',
              },
              n_filters2: {
                maxval: 72,
                minval: 8,
                type: 'int',
              },
            },
            id: 6936,
            jobId: '7a1f1e82-04ca-4e99-88ca-2a62230ca064',
            labels: [],
            name: 'mnist_pytorch_dist_random_search',
            notes: '',
            numTrials: 2,
            progress: 0,
            projectId: 1,
            projectName: 'Uncategorized',
            resourcePool: 'compute-pool',
            searcherType: 'random',
            startTime: '2024-06-14T15:50:59.059559Z',
            state: 'CANCELED',
            trialIds: [],
            unmanaged: false,
            userId: 1354,
            workspaceId: 1,
            workspaceName: 'Uncategorized',
          },
        ],
      }),
    );

    setup();

    await waitFor(() => {
      expect(screen.getByText('Shell (eminently-moving-oryx)')).toBeInTheDocument();
      expect(screen.getByText('mnist_pytorch_dist_random_search')).toBeInTheDocument();
    });
  });
  describe('launch buttons', () => {
    afterEach(() => {
      vi.mocked(usePermissions).mockImplementation(
        () => ({ canCreateNSC: false }) as ReturnType<typeof usePermissions>,
      );
    });

    it('offers Launch Interactive Task only, disabled without permission to launch', async () => {
      setup();

      const shell = await screen.findByRole('button', { name: 'Launch Interactive Task' });
      expect(shell).toBeDisabled();
    });

    it('"Launch Interactive Task" opens the shell launch form, with permission to launch', async () => {
      vi.mocked(usePermissions).mockImplementation(
        () =>
          ({
            canCreateNSC: true,
            canCreateTemplateWorkspace: () => false,
            canCreateWorkspaceNSC: () => true,
          }) as unknown as ReturnType<typeof usePermissions>,
      );
      setup();

      const entry = await screen.findByRole('button', { name: 'Launch Interactive Task' });
      expect(entry).toBeEnabled();
      await userEvent.click(entry);
      expect(await screen.findByText('Start from')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Launch' })).toBeInTheDocument();
      expect(screen.queryByRole('radio')).not.toBeInTheDocument();
    });
  });
});

const POLL_INTERVAL = 5000;

const project = (id: number, name: string) => ({
  archived: false,
  description: '',
  errorMessage: '',
  id,
  immutable: true,
  key: '',
  lastExperimentStartedAt: '2024-07-17T16:18:56.813686Z',
  name,
  notes: [],
  numActiveExperiments: 0,
  numExperiments: 1,
  numRuns: 1,
  state: 'UNSPECIFIED',
  userId: 1,
  username: 'admin',
  workspaceId: 2,
  workspaceName: 'Workspace',
});

const experiment = (id: number, name: string) => ({
  archived: false,
  id,
  name,
  projectId: 1,
  projectName: 'Uncategorized',
  startTime: `2024-06-14T15:50:${id}Z`,
  state: 'RUNNING',
  workspaceId: 1,
  workspaceName: 'Uncategorized',
});

const shell = (id: string, displayName: string) => ({
  displayName,
  id,
  name: displayName,
  startTime: '2024-07-18T15:13:07.401Z',
  state: 'RUNNING',
  type: 'shell',
  userId: 1,
  workspaceId: 1,
});

interface Deferred<T> {
  promise: Promise<T>;
  reject: (reason: unknown) => void;
  resolve: (value: T) => void;
}

function deferred<T>(): Deferred<T> {
  const d = {} as Deferred<T>;
  d.promise = new Promise<T>((resolve, reject) => Object.assign(d, { reject, resolve }));
  return d;
}

/** Each call of the mock returns the next deferred answer. */
function queue<T>(mock: ReturnType<typeof vi.fn>): Deferred<T>[] {
  const answers: Deferred<T>[] = [];
  mock.mockImplementation(() => {
    const d = deferred<T>();
    answers.push(d);
    return d.promise;
  });
  return answers;
}

/** Resolves a deferred answer and lets the page take it. */
async function answer<T>(d: Deferred<T>, value: T): Promise<void> {
  await act(async () => {
    d.resolve(value);
    await d.promise;
  });
}

const submissionsSection = () =>
  screen.getByText('Your Recent Submissions').closest('section') as HTMLElement;
const spinners = (root: ParentNode) => root.querySelectorAll('.ant-spin.ant-spin-spinning').length;
const submissionsSpinning = () => spinners(submissionsSection()) > 0;
const projectsSpinning = () => spinners(document) - spinners(submissionsSection()) > 0;

describe('Dashboard data loading', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    userStore.updateCurrentUser(CURRENT_USER);
    mocks.getCommands.mockImplementation(() => Promise.resolve([]));
    mocks.getExperiments.mockImplementation(() => Promise.resolve({ experiments: [] }));
    mocks.getProjectsByUserActivity.mockImplementation(() => Promise.resolve([]));
    mocks.getShells.mockImplementation(() => Promise.resolve([]));
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.clearAllMocks();
  });

  it('starts no batch while the last one is pending', async () => {
    const projects = queue<unknown[]>(mocks.getProjectsByUserActivity);
    setup();
    await waitFor(() => expect(mocks.getExperiments).toHaveBeenCalledTimes(1));
    expect(projects).toHaveLength(1);

    await act(() => vi.advanceTimersByTimeAsync(4 * POLL_INTERVAL));
    expect(projects).toHaveLength(1);
    expect(mocks.getExperiments).toHaveBeenCalledTimes(1);

    await answer(projects[0], []);
    expect(projects).toHaveLength(1);
    await act(() => vi.advanceTimersByTimeAsync(POLL_INTERVAL));
    expect(projects).toHaveLength(2);
    expect(mocks.getExperiments).toHaveBeenCalledTimes(2);
  });

  it('keeps the newer answer when an older batch answers last', async () => {
    // The user arrives after the first batch has started, which restarts the polling.
    userStore.reset();
    const projects = queue<unknown[]>(mocks.getProjectsByUserActivity);
    setup();
    await waitFor(() => expect(projects).toHaveLength(2));

    await answer(projects[1], [project(2, 'Newer')]);
    expect(await screen.findByText('Newer')).toBeInTheDocument();
    await answer(projects[0], [project(3, 'Older')]);
    expect(screen.getByText('Newer')).toBeInTheDocument();
    expect(screen.queryByText('Older')).not.toBeInTheDocument();
  });

  it('keeps the newer submissions when an older batch answers last', async () => {
    const experiments = queue<{ experiments: unknown[] }>(mocks.getExperiments);
    const shells = queue<unknown[]>(mocks.getShells);
    setup();
    await waitFor(() => expect(experiments).toHaveLength(1));

    // An update of the signed-in user restarts the polling while the first batch is pending.
    act(() => userStore.updateCurrentUser(RENAMED_USER));
    await waitFor(() => expect(experiments).toHaveLength(2));
    expect(shells).toHaveLength(2);

    await answer(experiments[1], { experiments: [experiment(11, 'Newer search')] });
    await answer(shells[1], [shell('newer', 'Newer shell')]);
    expect(await screen.findByText('Newer search')).toBeInTheDocument();
    expect(screen.getByText('Newer shell')).toBeInTheDocument();

    await answer(experiments[0], { experiments: [experiment(10, 'Older search')] });
    await answer(shells[0], [shell('older', 'Older shell')]);
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByText('Newer search')).toBeInTheDocument();
    expect(screen.getByText('Newer shell')).toBeInTheDocument();
    expect(screen.queryByText('Older search')).not.toBeInTheDocument();
    expect(screen.queryByText('Older shell')).not.toBeInTheDocument();
  });

  it('an older batch does not end the loading of the submissions', async () => {
    const experiments = queue<{ experiments: unknown[] }>(mocks.getExperiments);
    setup();
    await waitFor(() => expect(experiments).toHaveLength(1));

    act(() => userStore.updateCurrentUser(RENAMED_USER));
    await waitFor(() => expect(experiments).toHaveLength(2));

    await answer(experiments[0], { experiments: [] });
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(submissionsSpinning()).toBe(true);
    expect(screen.queryByText('No submissions')).not.toBeInTheDocument();

    await answer(experiments[1], { experiments: [] });
    expect(await screen.findByText('No submissions')).toBeInTheDocument();
  });

  it('shows the page and the overview while both sections load', async () => {
    const projects = queue<unknown[]>(mocks.getProjectsByUserActivity);
    const experiments = queue<{ experiments: unknown[] }>(mocks.getExperiments);
    setup();

    expect(await screen.findByTestId('user-overview')).toHaveTextContent(/^1$/);
    expect(screen.getByRole('button', { name: 'Launch Interactive Task' })).toBeInTheDocument();
    expect(submissionsSpinning()).toBe(true);
    expect(projectsSpinning()).toBe(true);
    expect(projects).toHaveLength(1);
    expect(experiments).toHaveLength(1);
    expect(screen.queryByText('No submissions')).not.toBeInTheDocument();
  });

  it('shows each section once its own first answer arrives', async () => {
    const projects = queue<unknown[]>(mocks.getProjectsByUserActivity);
    const experiments = queue<{ experiments: unknown[] }>(mocks.getExperiments);
    setup();
    await waitFor(() => expect(experiments).toHaveLength(1));

    await answer(projects[0], [project(2, 'Answered')]);
    expect(await screen.findByText('Recently Viewed Projects')).toBeInTheDocument();
    expect(projectsSpinning()).toBe(false);
    expect(submissionsSpinning()).toBe(true);
    expect(screen.queryByText('No submissions')).not.toBeInTheDocument();

    await answer(experiments[0], { experiments: [] });
    expect(await screen.findByText('No submissions')).toBeInTheDocument();
    expect(submissionsSpinning()).toBe(false);
  });

  it('shows the submissions while the projects have not answered', async () => {
    const projects = queue<unknown[]>(mocks.getProjectsByUserActivity);
    setup();

    expect(await screen.findByText('No submissions')).toBeInTheDocument();
    expect(projectsSpinning()).toBe(true);
    await answer(projects[0], []);
    await waitFor(() => expect(projectsSpinning()).toBe(false));
    expect(screen.queryByText('Recently Viewed Projects')).not.toBeInTheDocument();
  });

  it('reports no error of a batch that a newer one replaced', async () => {
    const projects = queue<unknown[]>(mocks.getProjectsByUserActivity);
    const experiments = queue<{ experiments: unknown[] }>(mocks.getExperiments);
    setup();
    await waitFor(() => expect(experiments).toHaveLength(1));

    // An update of the signed-in user restarts the polling while the first batch is pending.
    act(() => userStore.updateCurrentUser(RENAMED_USER));
    await waitFor(() => expect(experiments).toHaveLength(2));
    expect(projects).toHaveLength(2);

    await answer(projects[1], [project(2, 'Newer')]);
    await answer(experiments[1], { experiments: [experiment(11, 'Newer search')] });
    expect(await screen.findByText('Newer')).toBeInTheDocument();
    expect(screen.getByText('Newer search')).toBeInTheDocument();

    // The older batch fails after the newer one has answered.
    await act(async () => {
      projects[0].reject(new Error('unavailable'));
      experiments[0].reject(new Error('unavailable'));
      await Promise.allSettled([projects[0].promise, experiments[0].promise]);
    });
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(vi.mocked(handleError)).not.toHaveBeenCalled();
    expect(screen.getByText('Newer')).toBeInTheDocument();
    expect(screen.getByText('Newer search')).toBeInTheDocument();
  });

  it('reports the error of the latest batch', async () => {
    const projects = queue<unknown[]>(mocks.getProjectsByUserActivity);
    const experiments = queue<{ experiments: unknown[] }>(mocks.getExperiments);
    setup();
    await waitFor(() => expect(experiments).toHaveLength(1));

    await act(async () => {
      projects[0].reject(new Error('unavailable'));
      experiments[0].reject(new Error('unavailable'));
      await Promise.allSettled([projects[0].promise, experiments[0].promise]);
    });
    await waitFor(() => expect(vi.mocked(handleError)).toHaveBeenCalledTimes(2));
  });

  it('a failed projects request blocks neither the submissions nor the next poll', async () => {
    mocks.getProjectsByUserActivity
      .mockImplementationOnce(() => Promise.reject(new Error('unavailable')))
      .mockImplementation(() => Promise.resolve([project(2, 'Recovered')]));
    setup();

    expect(await screen.findByText('No submissions')).toBeInTheDocument();
    expect(projectsSpinning()).toBe(true);
    expect(mocks.getProjectsByUserActivity).toHaveBeenCalledTimes(1);

    await act(() => vi.advanceTimersByTimeAsync(POLL_INTERVAL));
    expect(mocks.getProjectsByUserActivity).toHaveBeenCalledTimes(2);
    expect(await screen.findByText('Recovered')).toBeInTheDocument();
    expect(projectsSpinning()).toBe(false);
  });
});
