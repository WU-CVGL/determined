import { act, render, screen, waitFor } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { HelmetProvider } from 'react-helmet-async';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import usePageVisibility from 'hooks/usePageVisibility';
import { getShells, getTask } from 'services/api';
import { V1TaskType } from 'services/api-ts-sdk';
import { detApi } from 'services/apiConfig';
import { readStream } from 'services/utils';
import { CommandState, CommandTask, CommandType, JobCapabilities, TaskItem } from 'types';

import { TaskLogsWrapper } from './TaskLogs';

const TASK_ID = '0123abcd-4567-89ef';

vi.mock('services/api', () => ({
  getCommand: vi.fn(() => Promise.reject(new Error('no command record'))),
  getGenericTask: vi.fn(() => Promise.resolve(undefined)),
  getShells: vi.fn(() => Promise.resolve([])),
  getTask: vi.fn(() => Promise.reject(new Error('no task record'))),
}));
vi.mock('services/apiConfig', () => ({
  detApi: { StreamingJobs: { taskLogs: vi.fn(), taskLogsFields: vi.fn() } },
}));
vi.mock('services/utils', () => ({ readStream: vi.fn() }));
vi.mock('hew/LogViewer/LogViewer', async (importOriginal) => ({
  ...(await importOriginal<typeof import('hew/LogViewer/LogViewer')>()),
  default: () => <div data-testid="log-viewer" />,
}));
vi.mock('hooks/useSettings', () => ({
  useSettings: () => ({ resetSettings: vi.fn(), settings: {}, updateSettings: vi.fn() }),
}));
vi.mock('hooks/usePermissions', () => ({ default: () => ({ loading: false }) }));

const PageVisibility = () => {
  usePageVisibility();
  return null;
};

const setPageHidden = (hidden: boolean) =>
  act(() => {
    Object.defineProperty(document, 'hidden', { configurable: true, value: hidden });
    document.dispatchEvent(new Event('visibilitychange'));
  });

const withLogs = (viewLogs: boolean): JobCapabilities => ({
  manageSharing: false,
  viewConfig: false,
  viewLogs,
  viewTitle: true,
});

const shellWithLogs = (viewLogs: boolean): CommandTask => ({
  capabilities: withLogs(viewLogs),
  id: TASK_ID,
  resourcePool: 'default',
  startTime: '2026-01-01T00:00:00Z',
  state: CommandState.Running,
  type: CommandType.Shell,
  userId: 2,
  workspaceId: 1,
});

/*
 * The master's answers: the task with its capabilities, or without them as an older master
 * answers, or a failed read; and the shell list, which has no record of a shell a day after its
 * end.
 */
const master: { failTask: boolean; shells: CommandTask[]; taskCapabilities?: JobCapabilities } = {
  failTask: false,
  shells: [],
};

const ended = (taskId: string, capabilities?: JobCapabilities): TaskItem => ({
  allocations: [{ isReady: false, state: CommandState.Terminated, taskId }],
  capabilities,
  endTime: '2026-01-01T01:00:00Z',
  startTime: '2026-01-01T00:00:00Z',
  taskId,
  taskType: V1TaskType.SHELL,
});

const comeBack = async (reads: number) => {
  setPageHidden(true);
  setPageHidden(false);
  await waitFor(() => expect(getTask).toHaveBeenCalledTimes(reads));
};

const setup = (url: string) =>
  render(
    <MemoryRouter initialEntries={[url]}>
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <PageVisibility />
          <HelmetProvider>
            <Routes>
              <Route element={<TaskLogsWrapper />} path="/:taskType/:taskId/logs" />
            </Routes>
          </HelmetProvider>
        </ThemeProvider>
      </UIProvider>
    </MemoryRouter>,
  );

describe('TaskLogs', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    master.failTask = false;
    master.shells = [];
    master.taskCapabilities = withLogs(true);
    vi.mocked(getTask).mockImplementation(({ taskId }) =>
      master.failTask
        ? Promise.reject(new Error('unavailable'))
        : Promise.resolve(ended(taskId, master.taskCapabilities)),
    );
    vi.mocked(getShells).mockImplementation(() => Promise.resolve(master.shells));
  });

  afterEach(() => {
    Object.defineProperty(document, 'hidden', { configurable: true, value: false });
  });

  it('titles the page from the task type and ID, not from the address', async () => {
    setup(`/shell/${TASK_ID}/logs?id=<script>secret`);
    await screen.findByTestId('log-viewer');
    await waitFor(() => expect(document.title).toMatch(/^Shell 0123abcd Logs - /));
    expect(document.title).not.toContain('secret');
    expect(document.title).not.toContain('<script>');
    expect(document.body.innerHTML).not.toContain('secret');
    expect(screen.getByRole('link', { name: 'Shell 0123abcd' })).toHaveAttribute(
      'href',
      `/shell/${TASK_ID}/logs`,
    );
  });

  it('opens no stream for logs that the task does not share, with no lookup of its type', async () => {
    master.taskCapabilities = withLogs(false);
    setup(`/shell/${TASK_ID}/logs`);
    expect(await screen.findByText('Logs not shared')).toBeInTheDocument();
    expect(screen.queryByTestId('log-viewer')).not.toBeInTheDocument();
    expect(detApi.StreamingJobs.taskLogsFields).not.toHaveBeenCalled();
    expect(readStream).not.toHaveBeenCalled();
    expect(getTask).toHaveBeenCalledWith({ taskId: TASK_ID });
    expect(getShells).not.toHaveBeenCalled();
  });

  it('opens the logs of a shell no longer in the shell list once the task shares them', async () => {
    // A shell that ended a day ago: the list has no record of it, the task still decides.
    master.taskCapabilities = withLogs(false);
    setup(`/shell/${TASK_ID}/logs`);
    expect(await screen.findByText('Logs not shared')).toBeInTheDocument();

    master.taskCapabilities = withLogs(true);
    await comeBack(2);
    expect(await screen.findByTestId('log-viewer')).toBeInTheDocument();
    expect(detApi.StreamingJobs.taskLogsFields).toHaveBeenCalledTimes(1);
    expect(getShells).not.toHaveBeenCalled();
  });

  it('keeps the last capabilities when a read of the task fails', async () => {
    master.taskCapabilities = withLogs(false);
    setup(`/shell/${TASK_ID}/logs`);
    expect(await screen.findByText('Logs not shared')).toBeInTheDocument();

    master.failTask = true;
    master.taskCapabilities = withLogs(true);
    await comeBack(2);
    expect(screen.getByText('Logs not shared')).toBeInTheDocument();

    master.failTask = false;
    await comeBack(3);
    expect(await screen.findByTestId('log-viewer')).toBeInTheDocument();
    expect(getShells).not.toHaveBeenCalled();
  });

  it('reads the logs when the first read of the task fails, for the master to refuse', async () => {
    master.failTask = true;
    setup(`/shell/${TASK_ID}/logs`);
    expect(await screen.findByTestId('log-viewer')).toBeInTheDocument();
    expect(getShells).not.toHaveBeenCalled();
  });

  it('looks the shell up for an older master, whose task has no capabilities', async () => {
    master.taskCapabilities = undefined;
    master.shells = [shellWithLogs(false)];
    setup(`/shell/${TASK_ID}/logs`);
    expect(await screen.findByText('Logs not shared')).toBeInTheDocument();
    expect(getShells).toHaveBeenCalledTimes(1);

    // A repeated lookup that fails keeps the capabilities of the last one.
    vi.mocked(getShells).mockRejectedValueOnce(new Error('unavailable'));
    await comeBack(2);
    await waitFor(() => expect(getShells).toHaveBeenCalledTimes(2));
    expect(screen.getByText('Logs not shared')).toBeInTheDocument();

    master.shells = [shellWithLogs(true)];
    await comeBack(3);
    expect(await screen.findByTestId('log-viewer')).toBeInTheDocument();
    expect(getShells).toHaveBeenCalledTimes(3);
  });
});
