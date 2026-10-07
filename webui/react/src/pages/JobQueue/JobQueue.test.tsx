import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { Loaded } from 'hew/utils/loadable';
import { Map } from 'immutable';
import { DndProvider } from 'react-dnd';
import { HTML5Backend } from 'react-dnd-html5-backend';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import {
  getJobQ,
  getJupyterLab,
  getShell,
  getShells,
  getTask,
  getUsers,
  updateUserSetting,
} from 'services/api';
import * as Api from 'services/api-ts-sdk';
import userStore from 'stores/users';
import userSettings from 'stores/userSettings';
import {
  CommandResponse,
  CommandState,
  CommandTask,
  CommandType,
  FullJob,
  Job,
  JobState,
  JobType,
  JsonObject,
  LimitedJob,
  ResourcePool,
  TaskItem,
} from 'types';
import { isDangerMenuItem, menuLabels } from 'utils/tests/menu';

import JobQueue from './JobQueue';

const OWNER_ID = 7;

const shellJob: FullJob = {
  allocatedSlots: 1,
  entityId: 'shell-1',
  isPreemptible: true,
  jobId: 'job-1',
  name: 'Shell (lively-calm-fox)',
  priority: 42,
  requestedSlots: 1,
  resourcePool: 'default',
  submissionTime: new Date('2026-01-01T00:00:00Z'),
  summary: { jobsAhead: 0, state: JobState.SCHEDULED },
  type: JobType.SHELL,
  userId: OWNER_ID,
  username: 'alice',
  workspaceId: 1,
};

const experimentJob: FullJob = {
  ...shellJob,
  entityId: '12',
  jobId: 'job-2',
  name: 'mnist',
  type: JobType.EXPERIMENT,
};

const runningShell: CommandTask = {
  id: 'shell-1',
  name: 'Shell (lively-calm-fox)',
  resourcePool: 'default',
  startTime: '2026-01-01T00:00:00Z',
  state: CommandState.Running,
  type: CommandType.Shell,
  userId: OWNER_ID,
  workspaceId: 1,
};

/** The task's record (GET /api/v1/tasks/{id}), with the state of its allocation. */
const taskRecord = (taskId: string, state: CommandState = CommandState.Running): TaskItem => ({
  allocations: [{ allocationId: `${taskId}.1`, isReady: true, state, taskId }],
  startTime: '2026-01-01T00:00:00Z',
  taskId,
  taskType: Api.V1TaskType.SHELL,
});

/** The list API's first page of shells: 1000 other shells, none of them on the page. */
const otherShells: CommandTask[] = Array.from({ length: 1000 }, (_, index) => ({
  ...runningShell,
  id: `other-shell-${index}`,
  name: `Shell ${index}`,
}));

/** A promise that the test resolves by hand. */
function deferred<T>() {
  let resolve: (value: T) => void = () => undefined;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

const mocks = vi.hoisted(() => ({ jobs: [] as unknown[] }));
const permissions = vi.hoisted(() => ({ canModify: true }));
const launched = vi.hoisted(() => ({ response: undefined as CommandResponse | undefined }));

vi.mock('services/api', () => ({
  cancelExperiment: vi.fn(),
  getCommands: vi.fn(() => Promise.resolve([])),
  // The jobs in queue order, or the reverse, paged as the master pages them.
  getJobQ: vi.fn(({ limit, offset, orderBy }) => {
    const jobs = orderBy === 'ORDER_BY_DESC' ? [...mocks.jobs].reverse() : mocks.jobs;
    const start = offset ?? 0;
    return Promise.resolve({
      jobs: jobs.slice(start, start + (limit || 100)),
      pagination: { total: jobs.length },
    });
  }),
  getJupyterLab: vi.fn(),
  getJupyterLabs: vi.fn(() => Promise.resolve([])),
  getShell: vi.fn(),
  getShells: vi.fn(() => Promise.resolve([])),
  getTask: vi.fn(),
  getTensorBoards: vi.fn(() => Promise.resolve([])),
  getUsers: vi.fn(() => Promise.resolve({ pagination: {}, users: [] })),
  killExperiment: vi.fn(),
  killGenericTask: vi.fn(),
  killTask: vi.fn(),
  updateUserSetting: vi.fn(() => Promise.resolve()),
}));
vi.mock('hooks/usePermissions', () => ({
  default: () => ({
    canCreateWorkspaceNSC: () => true,
    canModifyExperiment: () => permissions.canModify,
    canModifyWorkspaceNSC: () => permissions.canModify,
  }),
}));
vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => true }));
vi.mock('hooks/useFeature', () => ({ default: () => ({ isOn: () => true }) }));
// The launch form itself is tested in NtscLaunchModal.test.tsx and useLaunchForm.test.tsx. The
// stand-in is a modal, so it shows only once Launch Again opens it, and its title shows the task
// and the type the form opens with.
vi.mock('components/NtscLaunchModal', async () => {
  const { Modal } = await import('hew/Modal');
  const Button = (await import('hew/Button')).default;
  return {
    default: ({
      initialTask,
      initialType,
      onShellLaunched,
    }: {
      initialTask?: CommandTask;
      initialType: CommandType;
      onShellLaunched?: (response: CommandResponse) => void;
    }) => (
      <Modal title={`Launch form for ${initialTask?.id} (${initialType})`}>
        <Button onClick={() => launched.response && onShellLaunched?.(launched.response)}>
          Launch
        </Button>
      </Modal>
    ),
  };
});

const pool = (schedulerType: Api.V1SchedulerType) =>
  ({ name: 'default', schedulerType }) as unknown as ResourcePool;

const setup = (
  schedulerType: Api.V1SchedulerType = Api.V1SchedulerType.PRIORITY,
  jobState: JobState = JobState.SCHEDULED,
) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <DndProvider backend={HTML5Backend}>
          <SettingsProvider>
            <BrowserRouter>
              <ConfirmationProvider>
                <JobQueue jobState={jobState} rpStats={[]} selectedRp={pool(schedulerType)} />
              </ConfirmationProvider>
            </BrowserRouter>
          </SettingsProvider>
        </DndProvider>
      </ThemeProvider>
    </UIProvider>,
  );

const openRowMenu = async (rowText: string | RegExp) => {
  const row = (await screen.findByText(rowText)).closest('tr');
  if (!row) throw new Error('no row');
  await userEvent.click(within(row).getByRole('button'));
};

/** Waits until the page has looked up the tasks of its rows, so that they have the task menu. */
const waitForShells = async () => {
  await waitFor(() => expect(getTask).toHaveBeenCalled());
  await act(async () => {
    await Promise.allSettled(vi.mocked(getTask).mock.results.map((result) => result.value));
  });
};

const jan = (day: number) => new Date(`2026-01-0${day}T00:00:00Z`);

/** An experiment job, the given number of jobs into the queue. */
const queued = (index: number, fields: Partial<FullJob>): FullJob => ({
  ...experimentJob,
  entityId: `${100 + index}`,
  jobId: `q${index}`,
  summary: { jobsAhead: index, state: JobState.SCHEDULED },
  ...fields,
});

/** A job the user may not see: without its name, owner and submission time. */
const limitedJob = (index: number, slots: number): LimitedJob => ({
  allocatedSlots: slots,
  isPreemptible: true,
  jobId: `q${index}`,
  requestedSlots: slots,
  resourcePool: 'default',
  summary: { jobsAhead: index, state: JobState.SCHEDULED },
  type: JobType.EXPERIMENT,
  workspaceId: 1,
});

/** Active jobs in queue order, with equal values (q0 and q4) and a job without most values. */
const activeJobs: Job[] = [
  queued(0, {
    allocatedSlots: 2,
    name: 'beta',
    requestedSlots: 2,
    submissionTime: jan(3),
    username: 'bob',
  }),
  queued(1, {
    allocatedSlots: 8,
    name: 'Alpha',
    requestedSlots: 8,
    submissionTime: jan(1),
    username: 'carol',
  }),
  limitedJob(2, 4),
  queued(3, {
    allocatedSlots: 1,
    name: 'alpha_2',
    requestedSlots: 1,
    submissionTime: jan(2),
    username: 'Alice',
  }),
  queued(4, {
    allocatedSlots: 2,
    name: 'beta',
    requestedSlots: 2,
    submissionTime: jan(3),
    username: 'bob',
  }),
  queued(5, {
    allocatedSlots: 1,
    name: 'alpha2',
    requestedSlots: 1,
    submissionTime: jan(4),
    username: 'alice',
  }),
];

const QUEUE_ORDER = ['q0', 'q1', 'q2', 'q3', 'q4', 'q5'];

/** The job IDs of the rows, from the top. */
const rowOrder = () =>
  Array.from(document.querySelectorAll('tbody tr[data-row-key]')).map((row) =>
    row.getAttribute('data-row-key'),
  );

const settingsPath = (jobState: JobState) => `job-queue-${jobState}`;

/** Loaded user settings, with these settings stored for the tab. */
const storeSettings = (jobState: JobState, settings: JsonObject = {}) =>
  userSettings._forUseSettingsOnly().set(Loaded(Map({ [settingsPath(jobState)]: settings })));

/** The last value of this tab's setting that the page sent to the master. */
const saved = (jobState: JobState, key: string): unknown => {
  const value = vi
    .mocked(updateUserSetting)
    .mock.calls.flatMap(([params]) => params.settings ?? [])
    .filter((setting) => setting.storagePath === settingsPath(jobState) && setting.key === key)
    .at(-1)?.value;
  return value === undefined ? undefined : JSON.parse(value);
};

/** The last listing of the tab's jobs: not the lookup of the first job of the pool. */
const lastListing = () =>
  vi
    .mocked(getJobQ)
    .mock.calls.map(([params]) => params)
    .filter((params) => params.states !== undefined)
    .at(-1);

/** The largest limit the API takes, which the Active tab asks for to sort all its jobs. */
const ALL_JOBS = 2 ** 31 - 1;

const clickHeader = (title: string) => userEvent.click(screen.getByTestId(title));

describe('JobQueue', () => {
  beforeEach(() => {
    userStore.reset();
    userStore.updateCurrentUser({ id: OWNER_ID, isActive: true, isAdmin: false, username: 'a' });
    // No settings from an earlier test, in the store or in the URL.
    userSettings.reset();
    window.history.replaceState(null, '', '/');
    mocks.jobs = [shellJob];
    permissions.canModify = true;
    launched.response = {
      command: { ...runningShell, id: 'shell-2', state: CommandState.Queued },
      warnings: [],
    };
    vi.mocked(getShells).mockResolvedValue(otherShells);
    vi.mocked(getTask).mockImplementation(({ taskId }) => Promise.resolve(taskRecord(taskId)));
  });

  afterEach(() => {
    // Unmounted first: a page still mounted would call the cleared mocks with its next rows.
    cleanup();
    vi.clearAllMocks();
  });

  it('names a task job by its name and short task ID', async () => {
    setup();
    await waitForShells();
    expect(await screen.findByText('Shell (lively-calm-fox) (shell)')).toBeInTheDocument();
    expect(screen.queryByText('Shell shell')).not.toBeInTheDocument();
  });

  it('gives an active shell the full task menu, in the Tasks page order', async () => {
    setup();
    await waitForShells();
    await openRowMenu('Shell (lively-calm-fox) (shell)');
    await screen.findByText('Copy Task ID');
    expect(menuLabels()).toEqual([
      'View Logs',
      'View Resources',
      'Copy Task ID',
      'Connect via CLI',
      'Open Terminal',
      'Launch Again',
      'Manage Job',
      'Kill',
    ]);
    expect(isDangerMenuItem('Kill')).toBe(true);
    expect(isDangerMenuItem('Manage Job')).toBe(false);
  });

  it('leaves out Manage Job where the scheduler cannot manage the task', async () => {
    setup(Api.V1SchedulerType.KUBERNETES);
    await waitForShells();
    await openRowMenu('Shell (lively-calm-fox) (shell)');
    await screen.findByText('Copy Task ID');
    expect(menuLabels()).not.toContain('Manage Job');
    expect(menuLabels().at(-1)).toBe('Kill');
  });

  it('leaves out Connect via CLI, Manage Job and Kill on another user’s task', async () => {
    permissions.canModify = false;
    mocks.jobs = [{ ...shellJob, userId: OWNER_ID + 1 }];
    setup();
    await waitForShells();
    await openRowMenu('Shell (lively-calm-fox) (shell)');
    await screen.findByText('Copy Task ID');
    expect(menuLabels()).toEqual(['View Logs', 'View Resources', 'Copy Task ID']);
  });

  it('leaves out Manage Job and Kill in the job menu where the user cannot control the job', async () => {
    permissions.canModify = false;
    mocks.jobs = [experimentJob];
    setup();
    await openRowMenu('mnist');
    await screen.findByText('View Resources');
    expect(menuLabels()).toEqual(['View Resources']);
  });

  it('opens the launch form from Launch Again and shows the launch result', async () => {
    setup();
    await waitForShells();
    expect(screen.queryByText(/^Launch form for/)).not.toBeInTheDocument();
    await openRowMenu('Shell (lively-calm-fox) (shell)');
    await userEvent.click(await screen.findByText('Launch Again'));
    // The form opens with the task's type selected.
    expect(await screen.findByText('Launch form for shell-1 (shell)')).toBeInTheDocument();
    const fetches = vi.mocked(getJobQ).mock.calls.length;
    await userEvent.click(screen.getByRole('button', { name: 'Launch' }));
    expect(await screen.findByText('Shell Launched')).toBeInTheDocument();
    // The list is fetched again, so the new shell shows up.
    await waitFor(() => expect(vi.mocked(getJobQ).mock.calls.length).toBeGreaterThan(fetches));
  });

  it('keeps the job menu, in the same order, for an experiment', async () => {
    mocks.jobs = [experimentJob];
    setup();
    await openRowMenu('mnist');
    await screen.findByText('Manage Job');
    // Experiments have no View Logs in the job menu.
    expect(menuLabels()).toEqual(['View Resources', 'Manage Job', 'Cancel', 'Kill']);
    expect(getTask).not.toHaveBeenCalled();
  });

  it('shows Kill in red in an experiment’s job menu, and Cancel not', async () => {
    mocks.jobs = [experimentJob];
    setup();
    await openRowMenu('mnist');
    await screen.findByText('Manage Job');
    expect(isDangerMenuItem('Kill')).toBe(true);
    expect(isDangerMenuItem('Cancel')).toBe(false);
  });
  describe('the task menu of a task job', () => {
    it('looks the task up by its ID, also when the list API does not return it', async () => {
      setup();
      await waitForShells();
      await openRowMenu('Shell (lively-calm-fox) (shell)');
      await screen.findByText('Copy Task ID');
      expect(getTask).toHaveBeenCalledWith({ taskId: 'shell-1' });
      expect(menuLabels()).toEqual([
        'View Logs',
        'View Resources',
        'Copy Task ID',
        'Connect via CLI',
        'Open Terminal',
        'Launch Again',
        'Manage Job',
        'Kill',
      ]);
      // Neither the lists nor the shell and notebook APIs, which return the owner's key and token.
      expect(getShells).not.toHaveBeenCalled();
      expect(getShell).not.toHaveBeenCalled();
      expect(getJupyterLab).not.toHaveBeenCalled();
    });

    it('takes the task state from its record: no Connect via CLI before the shell runs', async () => {
      vi.mocked(getTask).mockResolvedValue(taskRecord('shell-1', CommandState.Pulling));
      setup();
      await waitForShells();
      await openRowMenu('Shell (lively-calm-fox) (shell)');
      await screen.findByText('Copy Task ID');
      expect(menuLabels()).toEqual([
        'View Logs',
        'View Resources',
        'Copy Task ID',
        'Launch Again',
        'Manage Job',
        'Kill',
      ]);
    });

    it('shows the jobs while the lookup runs, with the job menu until it is done', async () => {
      const lookup = deferred<TaskItem>();
      vi.mocked(getTask).mockReturnValue(lookup.promise);
      setup();
      await openRowMenu('Shell (lively-calm-fox) (shell)');
      await screen.findByText('Manage Job');
      expect(menuLabels()).toEqual(['View Logs', 'View Resources', 'Manage Job', 'Kill']);
      await act(async () => {
        lookup.resolve(taskRecord('shell-1'));
        await lookup.promise;
      });
      await openRowMenu('Shell (lively-calm-fox) (shell)');
      expect(await screen.findByText('Copy Task ID')).toBeInTheDocument();
    });

    it('keeps the task menu of a row when the lookup of another row fails', async () => {
      const failingJob: FullJob = {
        ...shellJob,
        entityId: 'shell-2',
        jobId: 'job-3',
        name: 'Shell (quiet-brave-owl)',
      };
      mocks.jobs = [shellJob, failingJob];
      vi.mocked(getTask).mockImplementation(({ taskId }) =>
        taskId === 'shell-2'
          ? Promise.reject(new Error('task lookup failed'))
          : Promise.resolve(taskRecord(taskId)),
      );
      setup();
      await waitForShells();
      expect(getTask).toHaveBeenCalledWith({ taskId: 'shell-2' });

      await openRowMenu('Shell (lively-calm-fox) (shell)');
      await screen.findByText('Copy Task ID');
      expect(menuLabels()).toContain('Connect via CLI');

      // The failed row keeps the job menu, which opens as a second menu.
      await openRowMenu('Shell (quiet-brave-owl) (shell)');
      await waitFor(() => expect(screen.getAllByRole('menu')).toHaveLength(2));
      const jobMenu = screen.getAllByRole('menu')[1];
      expect(
        within(jobMenu)
          .getAllByRole('menuitem')
          .map((item) => item.textContent),
      ).toEqual(['View Logs', 'View Resources', 'Manage Job', 'Kill']);
    });
  });

  describe('the column sorts of the Active tab', () => {
    beforeEach(() => {
      mocks.jobs = activeJobs;
      storeSettings(JobState.SCHEDULED);
    });

    it('opens in queue order, a page at a time', async () => {
      setup();
      await waitFor(() => expect(rowOrder()).toEqual(QUEUE_ORDER));
      expect(screen.getByTestId('#')).toHaveAttribute('aria-sort', 'ascending');
      expect(lastListing()).toMatchObject({ limit: 10, offset: 0, orderBy: 'ORDER_BY_ASC' });
    });

    it.each([
      {
        ascending: ['q1', 'q5', 'q3', 'q0', 'q4', 'q2'],
        descending: ['q0', 'q4', 'q3', 'q5', 'q1', 'q2'],
        firstClick: 'ascending',
        title: 'Job Name',
      },
      {
        ascending: ['q3', 'q5', 'q0', 'q4', 'q1', 'q2'],
        descending: ['q1', 'q0', 'q4', 'q5', 'q3', 'q2'],
        firstClick: 'ascending',
        title: 'User',
      },
      {
        ascending: ['q3', 'q5', 'q0', 'q4', 'q2', 'q1'],
        descending: ['q1', 'q2', 'q0', 'q4', 'q3', 'q5'],
        firstClick: 'descending',
        title: 'Slots',
      },
      {
        ascending: ['q1', 'q3', 'q0', 'q4', 'q5', 'q2'],
        descending: ['q5', 'q0', 'q4', 'q3', 'q1', 'q2'],
        firstClick: 'descending',
        title: 'Submitted',
      },
    ])(
      'sorts by $title both ways, with missing values last and ties in queue order, then goes back to the queue order',
      async ({ ascending, descending, firstClick, title }) => {
        const [first, second] =
          firstClick === 'ascending' ? [ascending, descending] : [descending, ascending];
        const secondClick = firstClick === 'ascending' ? 'descending' : 'ascending';
        setup();
        await waitFor(() => expect(rowOrder()).toEqual(QUEUE_ORDER));

        await clickHeader(title);
        await waitFor(() => expect(rowOrder()).toEqual(first));
        expect(screen.getByTestId(title)).toHaveAttribute('aria-sort', firstClick);
        expect(screen.getByTestId('#')).not.toHaveAttribute('aria-sort');
        // All jobs of the tab, in queue order, for the browser to sort.
        await waitFor(() =>
          expect(lastListing()).toMatchObject({
            limit: ALL_JOBS,
            offset: 0,
            orderBy: 'ORDER_BY_ASC',
          }),
        );

        await clickHeader(title);
        await waitFor(() => expect(rowOrder()).toEqual(second));
        expect(screen.getByTestId(title)).toHaveAttribute('aria-sort', secondClick);

        await clickHeader(title);
        await waitFor(() => expect(rowOrder()).toEqual(QUEUE_ORDER));
        expect(screen.getByTestId(title)).not.toHaveAttribute('aria-sort');
        expect(screen.getByTestId('#')).toHaveAttribute('aria-sort', 'ascending');
        await waitFor(() =>
          expect(lastListing()).toMatchObject({ limit: 10, offset: 0, orderBy: 'ORDER_BY_ASC' }),
        );
      },
    );

    it('sorts users by the name their avatar shows', async () => {
      vi.mocked(getUsers).mockResolvedValue({
        pagination: {},
        users: [
          {
            displayName: 'Aaron',
            id: OWNER_ID + 1,
            isActive: true,
            isAdmin: false,
            username: 'carol',
          },
        ],
      });
      userStore.fetchUsers();
      mocks.jobs = activeJobs.map((job) =>
        job.jobId === 'q1' ? { ...job, userId: OWNER_ID + 1 } : job,
      );
      setup();
      await waitFor(() => expect(rowOrder()).toEqual(QUEUE_ORDER));
      // carol, shown as Aaron, comes first.
      await clickHeader('User');
      await waitFor(() => expect(rowOrder()).toEqual(['q1', 'q3', 'q5', 'q0', 'q4', 'q2']));
      await clickHeader('User');
      await waitFor(() => expect(rowOrder()).toEqual(['q0', 'q4', 'q5', 'q3', 'q1', 'q2']));
    });

    it('goes back to the queue order without a queue position column', async () => {
      setup(Api.V1SchedulerType.FAIRSHARE);
      await waitFor(() => expect(rowOrder()).toEqual(QUEUE_ORDER));
      expect(screen.getByTestId('Preemptible')).not.toHaveAttribute('aria-sort');
      for (let click = 0; click < 3; click++) await clickHeader('Job Name');
      await waitFor(() => expect(saved(JobState.SCHEDULED, 'sortKey')).toBe('jobsAhead'));
      await waitFor(() => expect(rowOrder()).toEqual(QUEUE_ORDER));
      expect(screen.getByTestId('Job Name')).not.toHaveAttribute('aria-sort');
    });

    it('sorts all jobs of the tab, not the page, and starts a new sort on the first page', async () => {
      // Twelve jobs, named in the reverse of the queue order: q0 is "n11", q11 is "n00".
      mocks.jobs = Array.from({ length: 12 }, (_, index) =>
        queued(index, { name: `n${String(11 - index).padStart(2, '0')}` }),
      );
      storeSettings(JobState.SCHEDULED, { tableOffset: 10 });
      setup();
      await waitFor(() => expect(rowOrder()).toEqual(['q10', 'q11']));

      await clickHeader('Job Name');
      await waitFor(() =>
        expect(rowOrder()).toEqual(['q11', 'q10', 'q9', 'q8', 'q7', 'q6', 'q5', 'q4', 'q3', 'q2']),
      );
      await waitFor(() => expect(saved(JobState.SCHEDULED, 'tableOffset')).toBe(0));

      await userEvent.click(screen.getByTitle('2'));
      await waitFor(() => expect(rowOrder()).toEqual(['q1', 'q0']));
      expect(saved(JobState.SCHEDULED, 'sortKey')).toBe('name');
    });

    it('keeps the sort for the next visit', async () => {
      const { unmount } = setup();
      await waitFor(() => expect(rowOrder()).toEqual(QUEUE_ORDER));
      await clickHeader('Slots');
      await waitFor(() => expect(rowOrder()).toEqual(['q1', 'q2', 'q0', 'q4', 'q3', 'q5']));
      await waitFor(() => expect(saved(JobState.SCHEDULED, 'sortKey')).toBe('slots'));
      expect(saved(JobState.SCHEDULED, 'sortDesc')).toBe(true);
      unmount();

      // Not from the URL: from the stored settings.
      window.history.replaceState(null, '', '/');
      setup();
      await waitFor(() => expect(rowOrder()).toEqual(['q1', 'q2', 'q0', 'q4', 'q3', 'q5']));
      expect(screen.getByTestId('Slots')).toHaveAttribute('aria-sort', 'descending');
    });
  });

  describe('the Queued tab', () => {
    const queuedJobs = activeJobs.map((job) => ({
      ...job,
      summary: { ...job.summary, state: JobState.QUEUED },
    }));

    beforeEach(() => {
      mocks.jobs = queuedJobs;
    });

    it('has no column sorts', async () => {
      storeSettings(JobState.QUEUED);
      setup(Api.V1SchedulerType.PRIORITY, JobState.QUEUED);
      await waitFor(() => expect(rowOrder()).toEqual(QUEUE_ORDER));
      for (const title of ['Job Name', 'User', 'Slots', 'Submitted', 'State', 'Type', 'Priority']) {
        await clickHeader(title);
        expect(screen.getByTestId(title)).not.toHaveAttribute('aria-sort');
      }
      expect(rowOrder()).toEqual(QUEUE_ORDER);
      expect(saved(JobState.QUEUED, 'sortKey')).toBeUndefined();
      expect(vi.mocked(getJobQ).mock.calls.every(([params]) => params.limit !== ALL_JOBS)).toBe(
        true,
      );
    });

    it('shows the queue order for a sort key of the Active tab', async () => {
      storeSettings(JobState.QUEUED, { sortDesc: true, sortKey: 'name' });
      setup(Api.V1SchedulerType.PRIORITY, JobState.QUEUED);
      await waitFor(() => expect(rowOrder()).toEqual(QUEUE_ORDER));
      expect(lastListing()).toMatchObject({ limit: 10, offset: 0, orderBy: 'ORDER_BY_ASC' });
      expect(screen.getByTestId('#')).toHaveAttribute('aria-sort', 'ascending');
    });

    it('still reverses the queue from the queue position column', async () => {
      storeSettings(JobState.QUEUED);
      setup(Api.V1SchedulerType.PRIORITY, JobState.QUEUED);
      await waitFor(() => expect(rowOrder()).toEqual(QUEUE_ORDER));
      await clickHeader('#');
      await waitFor(() =>
        expect(lastListing()).toMatchObject({ limit: 10, offset: 0, orderBy: 'ORDER_BY_DESC' }),
      );
      await waitFor(() => expect(rowOrder()).toEqual([...QUEUE_ORDER].reverse()));
      expect(screen.getByTestId('#')).toHaveAttribute('aria-sort', 'descending');
    });
  });
});
