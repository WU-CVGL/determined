import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { DndProvider } from 'react-dnd';
import { HTML5Backend } from 'react-dnd-html5-backend';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { SettingsProvider } from 'hooks/useSettingsProvider';
import { getJobQ, getShells } from 'services/api';
import * as Api from 'services/api-ts-sdk';
import userStore from 'stores/users';
import {
  CommandResponse,
  CommandState,
  CommandTask,
  CommandType,
  FullJob,
  JobState,
  JobType,
  ResourcePool,
} from 'types';
import { isDangerMenuItem } from 'utils/tests/menu';

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

const mocks = vi.hoisted(() => ({ jobs: [] as unknown[] }));
const permissions = vi.hoisted(() => ({ canModify: true }));
const launched = vi.hoisted(() => ({ response: undefined as CommandResponse | undefined }));

vi.mock('services/api', () => ({
  cancelExperiment: vi.fn(),
  getCommands: vi.fn(() => Promise.resolve([])),
  getJobQ: vi.fn(() =>
    Promise.resolve({ jobs: mocks.jobs, pagination: { total: mocks.jobs.length } }),
  ),
  getJupyterLabs: vi.fn(() => Promise.resolve([])),
  getShells: vi.fn(() => Promise.resolve([])),
  getTensorBoards: vi.fn(() => Promise.resolve([])),
  killExperiment: vi.fn(),
  killGenericTask: vi.fn(),
  killTask: vi.fn(),
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

const setup = (schedulerType: Api.V1SchedulerType = Api.V1SchedulerType.PRIORITY) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <DndProvider backend={HTML5Backend}>
          <SettingsProvider>
            <BrowserRouter>
              <ConfirmationProvider>
                <JobQueue
                  jobState={JobState.SCHEDULED}
                  rpStats={[]}
                  selectedRp={pool(schedulerType)}
                />
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

/** Waits until the page has the shells, so that their rows have the task menu. */
const waitForShells = async () => {
  await waitFor(() => expect(getShells).toHaveBeenCalled());
  await act(async () => {
    await vi.mocked(getShells).mock.results[0].value;
  });
};

const menuLabels = () => screen.getAllByRole('menuitem').map((item) => item.textContent);

describe('JobQueue', () => {
  beforeAll(() => {
    userStore.updateCurrentUser({ id: OWNER_ID, isActive: true, isAdmin: false, username: 'a' });
  });

  beforeEach(() => {
    mocks.jobs = [shellJob];
    permissions.canModify = true;
    launched.response = {
      command: { ...runningShell, id: 'shell-2', state: CommandState.Queued },
      warnings: [],
    };
    vi.mocked(getShells).mockResolvedValue([runningShell]);
  });

  afterEach(() => vi.clearAllMocks());

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
    vi.mocked(getShells).mockResolvedValue([{ ...runningShell, userId: OWNER_ID + 1 }]);
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
    expect(getShells).not.toHaveBeenCalled();
  });

  it('shows Kill in red in an experiment’s job menu, and Cancel not', async () => {
    mocks.jobs = [experimentJob];
    setup();
    await openRowMenu('mnist');
    await screen.findByText('Manage Job');
    expect(isDangerMenuItem('Kill')).toBe(true);
    expect(isDangerMenuItem('Cancel')).toBe(false);
  });
});
