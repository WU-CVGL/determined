import { act, render, screen, waitFor, within } from '@testing-library/react';
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
import { updateUserSetting } from 'services/api';
import * as Api from 'services/api-ts-sdk';
import userStore from 'stores/users';
import userSettings from 'stores/userSettings';
import {
  FullJob,
  JobState,
  JobType,
  JsonObject,
  LimitedJob,
  ResourcePool,
  ResourceType,
} from 'types';

import JobQueue from './JobQueue';

const mocks = vi.hoisted(() => ({ jobs: [] as unknown[] }));

vi.mock('services/api', () => ({
  cancelExperiment: vi.fn(),
  getJobQ: vi.fn(() =>
    Promise.resolve({ jobs: mocks.jobs, pagination: { total: mocks.jobs.length } }),
  ),
  getTask: vi.fn(),
  getUsers: vi.fn(() => Promise.resolve({ pagination: {}, users: [] })),
  killExperiment: vi.fn(),
  killGenericTask: vi.fn(),
  killTask: vi.fn(),
  updateUserSetting: vi.fn(() => Promise.resolve()),
}));
vi.mock('hooks/usePermissions', () => {
  const checks = {
    canCreateWorkspaceNSC: () => true,
    canModifyExperiment: () => true,
    canModifyWorkspaceNSC: () => true,
  };
  return { default: () => checks };
});
vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => true }));
vi.mock('hooks/useFeature', () => ({ default: () => ({ isOn: () => true }) }));
vi.mock('components/NtscLaunchModal', () => ({ default: () => null }));

const job = (jobId: string, entityId: string, name: string, extra: Partial<FullJob> = {}) =>
  ({
    allocatedSlots: 4,
    entityId,
    isPreemptible: true,
    jobId,
    name,
    priority: 42,
    requestedSlots: 6,
    resourcePool: 'gpus',
    submissionTime: new Date('2026-01-01T00:00:00Z'),
    summary: { jobsAhead: 0, state: JobState.SCHEDULED },
    type: JobType.EXPERIMENT,
    userId: 7,
    username: 'alice',
    workspaceId: 1,
    ...extra,
  }) as FullJob;

const SWEEP = [{ agentId: 'node01', deviceIds: [6, 0, 5, 1] }];
const sweep = job('job-1', '812', 'sweep', { placement: SWEEP });
const dist = job('job-2', '813', 'dist', {
  placement: [
    { agentId: 'node04', deviceIds: [0, 1, 2, 3, 4, 5, 6, 7] },
    { agentId: 'node03', deviceIds: [0, 1, 2, 3, 4, 5, 6, 7] },
  ],
});
const cpuOnly = job('job-3', '814', 'cpu only');
const hidden: LimitedJob = {
  allocatedSlots: 1,
  isPreemptible: true,
  jobId: 'job-4',
  priority: 42,
  requestedSlots: 1,
  resourcePool: 'gpus',
  summary: { jobsAhead: 0, state: JobState.SCHEDULED },
  type: JobType.EXPERIMENT,
  workspaceId: 2,
};

const pool = (
  schedulerType: Api.V1SchedulerType = Api.V1SchedulerType.PRIORITY,
  slotType: ResourceType = ResourceType.CUDA,
) => ({ name: 'gpus', schedulerType, slotType }) as unknown as ResourcePool;

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

const page = (
  selectedRp: ResourcePool = pool(),
  jobState: JobState = JobState.SCHEDULED,
  onHighlight?: (placement?: Api.V1JobPlacement[]) => void,
) => (
  <UIProvider theme={DefaultTheme.Light}>
    <ThemeProvider>
      <DndProvider backend={HTML5Backend}>
        <SettingsProvider>
          <BrowserRouter>
            <ConfirmationProvider>
              <JobQueue
                jobState={jobState}
                rpStats={[]}
                selectedRp={selectedRp}
                onHighlight={onHighlight}
              />
            </ConfirmationProvider>
          </BrowserRouter>
        </SettingsProvider>
      </DndProvider>
    </ThemeProvider>
  </UIProvider>
);

const setup = (...args: Parameters<typeof page>) => render(page(...args));

/** The titles of the table's columns, from the left. */
const headers = () =>
  Array.from(document.querySelectorAll<HTMLElement>('thead th')).flatMap((th) => {
    const title =
      th.dataset.testid ?? th.querySelector<HTMLElement>('[data-testid]')?.dataset.testid;
    return title ? [title] : [];
  });

/** The job's cell in the GPUs column. */
const gpusCell = (jobId: string): HTMLElement => {
  const ths = Array.from(document.querySelectorAll('thead th'));
  const index = ths.findIndex((th) => th.querySelector('[data-testid="GPUs"]'));
  const row = document.querySelector(`tbody tr[data-row-key="${jobId}"]`);
  const cell = row?.querySelectorAll<HTMLElement>('td')[index];
  if (index < 0 || !cell) throw new Error(`no GPUs cell for ${jobId}`);
  return cell;
};

const gpusButton = (jobId: string) => within(gpusCell(jobId)).getByRole('button');

/** The job IDs of the table's rows, from the top. */
const rowKeys = () =>
  Array.from(document.querySelectorAll<HTMLElement>('tbody tr[data-row-key]')).map(
    (tr) => tr.dataset.rowKey,
  );

/** Waits for the job's row. */
const waitForRow = (jobId: string) =>
  waitFor(() => expect(document.querySelector(`tbody tr[data-row-key="${jobId}"]`)).not.toBeNull());

/** Runs the next poll of the jobs. */
const poll = async () => {
  await act(() => vi.advanceTimersByTimeAsync(5000));
  await act(() => vi.advanceTimersByTimeAsync(100));
};

describe('JobQueue GPUs', () => {
  beforeEach(() => {
    userStore.reset();
    userStore.updateCurrentUser({ id: 7, isActive: true, isAdmin: false, username: 'a' });
    userSettings.reset();
    window.history.replaceState(null, '', '/');
    mocks.jobs = [sweep, dist, cpuOnly, hidden];
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.clearAllMocks();
  });

  it('lists the GPUs of each job after Slots, one line per agent', async () => {
    storeSettings(JobState.SCHEDULED);
    setup();
    await screen.findByText('node01: 0, 1, 5, 6');
    expect(headers()).toEqual([
      '#',
      'Type',
      'Job Name',
      'Priority',
      'Submitted',
      'Slots',
      'GPUs',
      'State',
      'User',
    ]);
    expect(
      Array.from(gpusCell('job-2').querySelectorAll('.gpuLine')).map((l) => l.textContent),
    ).toEqual(['node03: 0-7', 'node04: 0-7']);
    // A line cut off by the column's width shows in full on hover.
    expect(
      Array.from(gpusCell('job-2').querySelectorAll('.gpuLine')).map((l) =>
        l.getAttribute('title'),
      ),
    ).toEqual(['node03: 0-7', 'node04: 0-7']);
    // A job without a GPU has an empty cell; a job the user cannot view has the omitted mark.
    expect(gpusCell('job-3')).toBeEmptyDOMElement();
    expect(gpusCell('job-4')).toHaveTextContent(/^\*\*\*$/);
    // Without a topology panel to highlight in, the GPUs are text.
    expect(within(gpusCell('job-1')).queryByRole('button')).toBeNull();
  });

  it("adds the column to the tab's stored layout with its width after Slots", async () => {
    storeSettings(JobState.SCHEDULED, {
      columns: ['preemptible', 'type', 'name', 'priority', 'submissionTime', 'slots', 'status'],
      columnWidths: [106, 75, 300, 107, 117, 90, 160],
    });
    setup();
    await waitFor(() =>
      expect(saved(JobState.SCHEDULED, 'columns')).toEqual([
        'preemptible',
        'type',
        'name',
        'priority',
        'submissionTime',
        'slots',
        'gpus',
        'status',
      ]),
    );
    expect(saved(JobState.SCHEDULED, 'columnWidths')).toEqual([
      106, 75, 300, 107, 117, 90, 170, 160,
    ]);
  });

  it('has no column on the Queued tab and leaves its stored layout alone', async () => {
    storeSettings(JobState.QUEUED);
    mocks.jobs = [{ ...sweep, summary: { jobsAhead: 0, state: JobState.QUEUED } }];
    setup(pool(), JobState.QUEUED);
    await waitForRow('job-1');
    expect(headers()).not.toContain('GPUs');
    expect(saved(JobState.QUEUED, 'columns')).toBeUndefined();
    expect(saved(JobState.QUEUED, 'columnWidths')).toBeUndefined();
  });

  it('has no column for a Kubernetes pool, a CPU pool, or a pool without an agent', async () => {
    for (const rp of [
      pool(Api.V1SchedulerType.KUBERNETES),
      pool(Api.V1SchedulerType.PRIORITY, ResourceType.CPU),
      pool(Api.V1SchedulerType.PRIORITY, ResourceType.UNSPECIFIED),
    ]) {
      // A layout that has the column, as the tab of a GPU pool stores it.
      storeSettings(JobState.SCHEDULED, {
        columns: ['name', 'slots', 'gpus', 'status'],
        columnWidths: [150, 74, 170, 160],
      });
      const { unmount } = setup(rp);
      await waitForRow('job-1');
      expect(headers()).toEqual(['Job Name', 'Slots', 'State']);
      expect(screen.queryByText('node01: 0, 1, 5, 6')).toBeNull();
      unmount();
    }
  });

  it("highlights a job's GPUs on a click or Enter on its cell, and clears on a second", async () => {
    storeSettings(JobState.SCHEDULED);
    const onHighlight = vi.fn();
    setup(pool(), JobState.SCHEDULED, onHighlight);
    await screen.findByText('node01: 0, 1, 5, 6');

    // Jobs without GPUs and jobs the user cannot view have no toggle.
    expect(within(gpusCell('job-3')).queryByRole('button')).toBeNull();
    expect(within(gpusCell('job-4')).queryByRole('button')).toBeNull();

    await userEvent.click(gpusButton('job-1'));
    expect(gpusButton('job-1')).toHaveAttribute('aria-pressed', 'true');
    expect(gpusButton('job-2')).toHaveAttribute('aria-pressed', 'false');
    expect(onHighlight).toHaveBeenLastCalledWith(SWEEP);

    // Another job's cell moves the highlight.
    gpusButton('job-2').focus();
    await userEvent.keyboard('{Enter}');
    expect(gpusButton('job-1')).toHaveAttribute('aria-pressed', 'false');
    expect(gpusButton('job-2')).toHaveAttribute('aria-pressed', 'true');
    expect(onHighlight).toHaveBeenLastCalledWith(dist.placement);

    await userEvent.click(gpusButton('job-2'));
    expect(gpusButton('job-2')).toHaveAttribute('aria-pressed', 'false');
    expect(onHighlight).toHaveBeenLastCalledWith(undefined);
  });

  it('follows the job across polls, and clears once it holds no GPU or leaves', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    storeSettings(JobState.SCHEDULED);
    const onHighlight = vi.fn();
    setup(pool(), JobState.SCHEDULED, onHighlight);
    await screen.findByText('node01: 0, 1, 5, 6');
    await userEvent.click(gpusButton('job-1'));
    expect(onHighlight).toHaveBeenLastCalledWith(SWEEP);

    // A new trial holds one more GPU.
    const grown = [{ agentId: 'node01', deviceIds: [0, 1, 5, 6, 7] }];
    mocks.jobs = [{ ...sweep, placement: grown }, dist];
    await poll();
    await screen.findByText('node01: 0, 1, 5-7');
    expect(gpusButton('job-1')).toHaveAttribute('aria-pressed', 'true');
    expect(onHighlight).toHaveBeenLastCalledWith(grown);

    // The queue order changes.
    mocks.jobs = [dist, { ...sweep, placement: grown }];
    await poll();
    await waitFor(() => expect(rowKeys()).toEqual(['job-2', 'job-1']));
    expect(gpusButton('job-1')).toHaveAttribute('aria-pressed', 'true');
    expect(gpusButton('job-2')).toHaveAttribute('aria-pressed', 'false');
    expect(onHighlight).toHaveBeenLastCalledWith(grown);

    // The job holds no GPU.
    mocks.jobs = [{ ...sweep, placement: undefined }, dist];
    await poll();
    await waitFor(() => expect(onHighlight).toHaveBeenLastCalledWith(undefined));
    expect(gpusCell('job-1')).toBeEmptyDOMElement();

    // It holds GPUs again: the highlight stays cleared.
    mocks.jobs = [sweep, dist];
    await poll();
    await screen.findByText('node01: 0, 1, 5, 6');
    expect(gpusButton('job-1')).toHaveAttribute('aria-pressed', 'false');

    // The job leaves the list.
    await userEvent.click(gpusButton('job-1'));
    expect(onHighlight).toHaveBeenLastCalledWith(SWEEP);
    mocks.jobs = [dist];
    await poll();
    await waitFor(() => expect(onHighlight).toHaveBeenLastCalledWith(undefined));
  });

  it('clears the highlight when the topology panel has no GPU tiles', async () => {
    storeSettings(JobState.SCHEDULED);
    const onHighlight = vi.fn();
    const { rerender } = setup(pool(), JobState.SCHEDULED, onHighlight);
    await screen.findByText('node01: 0, 1, 5, 6');
    await userEvent.click(gpusButton('job-1'));
    expect(onHighlight).toHaveBeenLastCalledWith(SWEEP);

    rerender(page(pool(), JobState.SCHEDULED, undefined));
    expect(onHighlight).toHaveBeenLastCalledWith(undefined);
    expect(within(gpusCell('job-1')).queryByRole('button')).toBeNull();

    // The tiles return: the highlight stays cleared.
    rerender(page(pool(), JobState.SCHEDULED, onHighlight));
    expect(gpusButton('job-1')).toHaveAttribute('aria-pressed', 'false');
    expect(onHighlight).toHaveBeenLastCalledWith(undefined);
  });

  it('clears the highlight when the tab closes', async () => {
    storeSettings(JobState.SCHEDULED);
    const onHighlight = vi.fn();
    const { unmount } = setup(pool(), JobState.SCHEDULED, onHighlight);
    await screen.findByText('node01: 0, 1, 5, 6');
    await userEvent.click(gpusButton('job-1'));
    expect(onHighlight).toHaveBeenLastCalledWith(SWEEP);
    unmount();
    expect(onHighlight).toHaveBeenLastCalledWith(undefined);
  });
});
