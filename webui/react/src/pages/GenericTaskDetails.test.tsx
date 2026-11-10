import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { HelmetProvider } from 'react-helmet-async';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import usePageVisibility from 'hooks/usePageVisibility';
import {
  getGenericTask,
  getGenericTaskConfig,
  getGenericTasks,
  getJobSharing,
  getTask,
  patchJobSharing,
} from 'services/api';
import { V1JobContentKind, V1TaskType } from 'services/api-ts-sdk';
import { detApi } from 'services/apiConfig';
import { CommandState, GenericTask, GenericTaskState, JobCapabilities, TaskItem } from 'types';
import { DetError } from 'utils/error';

import GenericTaskDetails from './GenericTaskDetails';

const TASK_ID = 'task-1';

const params = vi.hoisted(() => ({ tab: undefined as string | undefined, taskId: 'task-1' }));
vi.mock('react-router-dom', async (importOriginal) => ({
  ...(await importOriginal<typeof import('react-router-dom')>()),
  useParams: () => ({ tab: params.tab, taskId: params.taskId }),
}));

vi.mock('hooks/useTaskResourcesEnabled', () => ({ default: () => false }));

vi.mock('hew/CodeEditor', () => ({
  __esModule: true,
  default: ({ file }: { file: string }) => <pre data-testid="code-editor">{file}</pre>,
}));

vi.mock('services/api', () => ({
  getGenericTask: vi.fn(),
  getGenericTaskConfig: vi.fn(() => Promise.resolve({ name: 'eval-sweep' })),
  getGenericTasks: vi.fn(() =>
    Promise.resolve({ pagination: { limit: 0, offset: 0, total: 0 }, tasks: [] }),
  ),
  getJobSharing: vi.fn(),
  getTask: vi.fn(),
  patchJobSharing: vi.fn(),
}));

const taskItem = (taskState: GenericTaskState): TaskItem => ({
  allocations: [{ isReady: false, state: CommandState.Terminated }],
  noPause: false,
  startTime: '2026-01-01T00:00:00Z',
  taskId: TASK_ID,
  taskState,
  taskType: V1TaskType.GENERIC,
});

const summary: GenericTask = {
  description: '',
  jobId: 'job-1',
  name: 'eval-sweep',
  noPause: false,
  projectId: 1,
  resourcePool: 'default',
  slots: 1,
  startTime: '2026-01-01T00:00:00Z',
  state: GenericTaskState.Active,
  taskId: TASK_ID,
  userId: 1,
  username: 'alice',
  workspaceId: 1,
};

const PageVisibility = () => {
  usePageVisibility();
  return null;
};

const tree = () => (
  <BrowserRouter>
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <PageVisibility />
        <HelmetProvider>
          <ConfirmationProvider>
            <GenericTaskDetails />
          </ConfirmationProvider>
        </HelmetProvider>
      </ThemeProvider>
    </UIProvider>
  </BrowserRouter>
);

const setup = () => render(tree());

// The user leaves the page, e.g. for another browser tab, and comes back.
const returnToPage = async () => {
  for (const hidden of [true, false]) {
    act(() => {
      Object.defineProperty(document, 'hidden', { configurable: true, value: hidden });
      document.dispatchEvent(new Event('visibilitychange'));
    });
  }
  await act(async () => {
    await vi.advanceTimersByTimeAsync(100);
  });
};

/* Runs polls, in steps so that React renders the responses in between as in a browser. */
const poll = async (times: number) => {
  for (let i = 0; i < times * 10; i++) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(500);
    });
  }
};

describe('GenericTaskDetails', () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.mocked(getGenericTask).mockResolvedValue(summary);
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.clearAllMocks();
    params.tab = undefined;
    params.taskId = TASK_ID;
    Object.defineProperty(document, 'hidden', { configurable: true, value: false });
  });

  describe('job privacy', () => {
    const hidden: JobCapabilities = {
      manageSharing: false,
      viewConfig: false,
      viewLogs: false,
      viewTitle: false,
    };

    it('names a task whose title is not shared by its kind and short ID', async () => {
      vi.mocked(getTask).mockResolvedValue(taskItem(GenericTaskState.Active));
      vi.mocked(getGenericTask).mockResolvedValue({
        ...summary,
        capabilities: hidden,
        description: '',
        name: '',
      });
      setup();
      const name = await screen.findByTestId('generic-task-name');
      await waitFor(() => expect(name).toHaveTextContent('Generic Task task'));
      expect(name).toHaveTextContent(/^Generic Task task$/);
    });

    it('opens no log stream for logs that are not shared', async () => {
      params.tab = 'logs';
      const taskLogs = vi.spyOn(detApi.StreamingJobs, 'taskLogs');
      const taskLogsFields = vi.spyOn(detApi.StreamingJobs, 'taskLogsFields');
      const fetchMock = vi.fn(() => new Promise<Response>(() => {}));
      vi.stubGlobal('fetch', fetchMock);
      let resolveSummary: (value: GenericTask) => void = () => {};
      vi.mocked(getTask).mockResolvedValue(taskItem(GenericTaskState.Active));
      vi.mocked(getGenericTask).mockReturnValue(
        new Promise((resolve) => {
          resolveSummary = resolve;
        }),
      );
      try {
        setup();
        await screen.findByTestId('generic-task-name');
        await poll(1);
        resolveSummary({ ...summary, capabilities: hidden });
        expect(await screen.findByText('Logs not shared')).toBeInTheDocument();
        expect(taskLogs).not.toHaveBeenCalled();
        expect(taskLogsFields).not.toHaveBeenCalled();
        expect(fetchMock).not.toHaveBeenCalled();
      } finally {
        vi.unstubAllGlobals();
        taskLogs.mockRestore();
        taskLogsFields.mockRestore();
      }
    });

    it('offers Sharing only when the master lets the user manage it', async () => {
      vi.mocked(getTask).mockResolvedValue(taskItem(GenericTaskState.Active));
      setup();
      await screen.findByTestId('generic-task-pause');
      expect(screen.queryByTestId('generic-task-sharing')).not.toBeInTheDocument();
    });

    it('looks the task up again after its sharing was saved', async () => {
      const rule = { newJobAudience: { allUsers: true }, sharingAllowed: true };
      vi.mocked(getTask).mockResolvedValue(taskItem(GenericTaskState.Active));
      vi.mocked(getGenericTask).mockResolvedValue({
        ...summary,
        capabilities: { manageSharing: true, viewConfig: true, viewLogs: true, viewTitle: true },
      });
      vi.mocked(getJobSharing).mockResolvedValue({
        rules: [
          { ...rule, kind: V1JobContentKind.TITLE },
          { ...rule, kind: V1JobContentKind.LOGS },
          { ...rule, kind: V1JobContentKind.CONFIG },
        ],
        sharing: {},
      });
      vi.mocked(patchJobSharing).mockResolvedValue({});
      setup();
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      await user.click(await screen.findByTestId('generic-task-sharing'));
      await screen.findByTestId('job-sharing-title');
      expect(vi.mocked(getJobSharing).mock.calls[0][0]).toEqual({ jobId: 'job-1' });
      expect(getGenericTask).toHaveBeenCalledTimes(1);
      await user.click(screen.getByRole('button', { name: 'Save' }));
      await waitFor(() => expect(getGenericTask).toHaveBeenCalledTimes(2));
    });

    it('shows a config that the master refuses as not shared', async () => {
      params.tab = 'config';
      vi.mocked(getTask).mockResolvedValue(taskItem(GenericTaskState.Active));
      vi.mocked(getGenericTaskConfig).mockRejectedValueOnce(
        // As processApiError wraps the master's refusal.
        new DetError(new Response(null, { status: 403 }), {
          publicMessage: 'config not shared',
          publicSubject: 'Request getGenericTaskConfig failed.',
        }),
      );
      setup();
      expect(await screen.findByText('Config not shared')).toBeInTheDocument();
      expect(screen.queryByText('Unable to load config')).not.toBeInTheDocument();
    });

    describe('config sharing restored', () => {
      const SENTINEL = 'sentinel-config-value';
      const shared: JobCapabilities = {
        manageSharing: false,
        viewConfig: true,
        viewLogs: true,
        viewTitle: true,
      };
      // Whether each task's config is shared, as the master decides it now.
      let configShared: Record<string, boolean>;
      // Whether the lookup of each task says the config is shared; it can lag behind the master.
      let lookupShared: Record<string, boolean>;

      // As processApiError wraps the master's refusal.
      const refusal = () =>
        new DetError(new Response(null, { status: 403 }), {
          publicMessage: 'config not shared',
          publicSubject: 'Request getGenericTaskConfig failed.',
        });

      const configRequests = (taskId = TASK_ID) =>
        vi.mocked(getGenericTaskConfig).mock.calls.filter(([args]) => args.taskId === taskId)
          .length;

      beforeEach(() => {
        params.tab = 'config';
        configShared = {};
        lookupShared = {};
        vi.mocked(getTask).mockImplementation(({ taskId }) =>
          Promise.resolve({ ...taskItem(GenericTaskState.Active), taskId }),
        );
        vi.mocked(getGenericTask).mockImplementation((taskId) =>
          Promise.resolve({
            ...summary,
            capabilities: { ...shared, viewConfig: !!lookupShared[taskId] },
            taskId,
          }),
        );
        vi.mocked(getGenericTaskConfig).mockImplementation(({ taskId }) =>
          configShared[taskId]
            ? Promise.resolve({ entrypoint: `${SENTINEL}-${taskId}` })
            : Promise.reject(refusal()),
        );
        // A new answer at each poll, so that the page renders again in between.
        let polls = 0;
        vi.mocked(getGenericTasks).mockImplementation(() => {
          polls++;
          return Promise.resolve({
            pagination: { limit: 0, offset: 0, total: polls },
            tasks: Array.from({ length: polls }, (_, i) => ({
              ...summary,
              name: `child-${i}`,
              parentId: TASK_ID,
              taskId: `c${i}`,
            })),
          });
        });
      });

      it('reads the config again when it is shared again and the user comes back', async () => {
        setup();
        expect(await screen.findByText('Config not shared')).toBeInTheDocument();
        await poll(1);
        const before = configRequests();
        expect(before).toBe(1);

        configShared[TASK_ID] = true;
        lookupShared[TASK_ID] = true;
        await returnToPage();
        expect(await screen.findByText(new RegExp(SENTINEL))).toBeInTheDocument();
        expect(screen.queryByText('Config not shared')).not.toBeInTheDocument();
        expect(screen.queryByText('Unable to load config')).not.toBeInTheDocument();
        await poll(2);
        expect(configRequests()).toBe(before + 1);
      });

      it('reads a refused config once per return while the lookup still shows it', async () => {
        lookupShared[TASK_ID] = true;
        setup();
        expect(await screen.findByText('Config not shared')).toBeInTheDocument();
        await poll(1);
        expect(configRequests()).toBe(1);

        // Renders for new answers do not read the config again.
        const childPolls = vi.mocked(getGenericTasks).mock.calls.length;
        await poll(4);
        expect(vi.mocked(getGenericTasks).mock.calls.length).toBeGreaterThan(childPolls + 2);
        expect(configRequests()).toBe(1);

        for (const expected of [2, 3, 4]) {
          await returnToPage();
          await poll(2);
          expect(configRequests()).toBe(expected);
          expect(screen.getByText('Config not shared')).toBeInTheDocument();
        }
      });

      it('sends no config request at a return while the lookup hides the config', async () => {
        setup();
        expect(await screen.findByText('Config not shared')).toBeInTheDocument();
        await poll(1);
        const before = configRequests();
        await returnToPage();
        await poll(2);
        expect(getGenericTask).toHaveBeenCalledTimes(2);
        expect(configRequests()).toBe(before);
        expect(screen.getByText('Config not shared')).toBeInTheDocument();
      });

      it('starts over for another task and drops the answer for the last one', async () => {
        lookupShared[TASK_ID] = true;
        const view = setup();
        expect(await screen.findByText('Config not shared')).toBeInTheDocument();

        // The config of the first task is shared while the second task is opened.
        let answerFirst: (value: { entrypoint: string }) => void = () => {};
        vi.mocked(getGenericTaskConfig).mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              answerFirst = resolve;
            }),
        );
        await returnToPage();
        expect(configRequests()).toBe(2);

        configShared['task-2'] = true;
        lookupShared['task-2'] = true;
        params.taskId = 'task-2';
        view.rerender(tree());
        expect(await screen.findByText(new RegExp(`${SENTINEL}-task-2`))).toBeInTheDocument();
        const second = configRequests('task-2');

        await act(async () => {
          answerFirst({ entrypoint: `${SENTINEL}-${TASK_ID}` });
          await vi.advanceTimersByTimeAsync(100);
        });
        expect(screen.getByText(new RegExp(`${SENTINEL}-task-2`))).toBeInTheDocument();
        expect(screen.queryByText(new RegExp(`${SENTINEL}-${TASK_ID}`))).not.toBeInTheDocument();
        await poll(2);
        expect(configRequests('task-2')).toBe(second);
        expect(configRequests()).toBe(2);
      });
    });
  });

  it('looks the task up by its ID once', async () => {
    vi.mocked(getTask).mockResolvedValue(taskItem(GenericTaskState.Active));
    setup();
    const name = await screen.findByTestId('generic-task-name');
    await waitFor(() => expect(name).toHaveTextContent('eval-sweep'));
    expect(getGenericTask).toHaveBeenCalledTimes(1);
    expect(vi.mocked(getGenericTask).mock.calls[0][0]).toBe(TASK_ID);
    expect(getGenericTasks).toHaveBeenCalledWith(
      { limit: 0, parentId: TASK_ID },
      expect.anything(),
    );
  });

  it('is under Jobs in the breadcrumb', async () => {
    vi.mocked(getTask).mockResolvedValue(taskItem(GenericTaskState.Active));
    setup();
    await screen.findByTestId('generic-task-name');
    expect(screen.getByRole('link', { name: 'Jobs' })).toHaveAttribute('href', '/jobs');
    expect(screen.queryByRole('link', { name: 'Tasks' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Generic Tasks' })).not.toBeInTheDocument();
  });

  it('keeps polling an active task', async () => {
    vi.mocked(getTask).mockResolvedValue(taskItem(GenericTaskState.Active));
    setup();
    await screen.findByTestId('generic-task-name');
    await poll(2);
    expect(vi.mocked(getTask).mock.calls.length).toBeGreaterThanOrEqual(3);
  });

  it('keeps refreshing a child that outlives its completed parent', async () => {
    const child: GenericTask = { ...summary, name: 'child-task', parentId: TASK_ID, taskId: 'c1' };
    const children = (state: GenericTaskState) => ({
      pagination: { limit: 0, offset: 0, total: 1 },
      tasks: [{ ...child, state }],
    });
    vi.mocked(getTask).mockResolvedValue(taskItem(GenericTaskState.Completed));
    // The child ends only after several refreshes, so they must go on after the parent's stop.
    vi.mocked(getGenericTasks)
      .mockResolvedValueOnce(children(GenericTaskState.Active))
      .mockResolvedValueOnce(children(GenericTaskState.Active))
      .mockResolvedValueOnce(children(GenericTaskState.Active))
      .mockResolvedValue(children(GenericTaskState.Error));
    setup();
    const childRow = (await screen.findByText('child-task')).closest('tr') as HTMLElement;
    expect(within(childRow).getByText('Active')).toBeInTheDocument();

    await poll(4);
    expect(getTask).toHaveBeenCalledTimes(1);
    await waitFor(() =>
      expect(
        within(screen.getByText('child-task').closest('tr') as HTMLElement).getByText('Errored'),
      ).toBeInTheDocument(),
    );

    // Once the child has ended too, the children are not fetched any more.
    const calls = vi.mocked(getGenericTasks).mock.calls.length;
    await poll(2);
    expect(getGenericTasks).toHaveBeenCalledTimes(calls);
  });

  it('stops polling once the task is completed', async () => {
    vi.mocked(getTask).mockResolvedValue(taskItem(GenericTaskState.Completed));
    setup();
    await screen.findByTestId('generic-task-name');
    await poll(2);
    expect(getTask).toHaveBeenCalledTimes(1);
    expect(getGenericTasks).toHaveBeenCalledTimes(1);
  });
});
