import { act, render, screen, waitFor } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import { ThemeProvider } from 'components/ThemeProvider';
import usePageVisibility from 'hooks/usePageVisibility';
import { detApi } from 'services/apiConfig';
import handleError from 'utils/error';

import { TaskLogsViewer } from './TaskLogs';

vi.mock('services/api', () => ({ getTask: vi.fn() }));
vi.mock('services/apiConfig', () => ({
  detApi: {
    StreamingJobs: {
      taskLogs: vi.fn(() => ({ options: {}, url: '/api/v1/tasks/task-1/logs' })),
      taskLogsFields: vi.fn(() => ({ options: {}, url: '/api/v1/tasks/task-1/logs/fields' })),
    },
  },
}));
vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: vi.fn(),
}));

const TASK_ID = 'task-1';

const PageVisibility = () => {
  usePageVisibility();
  return null;
};

const viewer = (logsHidden?: boolean) => (
  <UIProvider theme={DefaultTheme.Light}>
    <ThemeProvider>
      <PageVisibility />
      <TaskLogsViewer
        logsHidden={logsHidden}
        resetSettings={vi.fn()}
        settings={{}}
        taskId={TASK_ID}
        updateSettings={vi.fn()}
      />
    </ThemeProvider>
  </UIProvider>
);

const setup = (logsHidden?: boolean) => render(viewer(logsHidden));

// The master refuses the logs of a job whose logs are not shared before it sends a line.
const refusal = () =>
  new Response(JSON.stringify({ error: { httpCode: 403, message: 'logs not shared' } }), {
    status: 403,
  });

const readable = () => new Response('', { status: 200 });

const setPageHidden = (hidden: boolean) =>
  act(() => {
    Object.defineProperty(document, 'hidden', { configurable: true, value: hidden });
    document.dispatchEvent(new Event('visibilitychange'));
  });

// Gives a request the time to start.
const settle = () => act(() => new Promise((resolve) => setTimeout(resolve, 300)));

describe('TaskLogsViewer', () => {
  const fetchMock = vi.fn();

  beforeEach(() => {
    vi.mocked(detApi.StreamingJobs.taskLogs).mockClear();
    vi.mocked(detApi.StreamingJobs.taskLogsFields).mockClear();
    vi.mocked(handleError).mockClear();
    fetchMock.mockReset().mockImplementation(() => Promise.resolve(refusal()));
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    Object.defineProperty(document, 'hidden', { configurable: true, value: false });
  });

  it('opens no stream for logs that are not shared', () => {
    setup(true);
    expect(screen.getByText('Logs not shared')).toBeInTheDocument();
    expect(detApi.StreamingJobs.taskLogs).not.toHaveBeenCalled();
    expect(detApi.StreamingJobs.taskLogsFields).not.toHaveBeenCalled();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('shows the refusal of the master once, without retrying or an error toast', async () => {
    setup();
    expect(await screen.findByText('Logs not shared')).toBeInTheDocument();
    const calls = fetchMock.mock.calls.length;
    // Give a retry the time to start: none does.
    await new Promise((resolve) => setTimeout(resolve, 300));
    expect(fetchMock.mock.calls.length).toBe(calls);
    await waitFor(() => expect(handleError).not.toHaveBeenCalled());
  });

  it('opens the logs again when the capabilities show them again after a refusal', async () => {
    const view = setup(false);
    expect(await screen.findByText('Logs not shared')).toBeInTheDocument();
    view.rerender(viewer(true));
    expect(screen.getByText('Logs not shared')).toBeInTheDocument();
    const opened = vi.mocked(detApi.StreamingJobs.taskLogsFields).mock.calls.length;

    fetchMock.mockImplementation(() => Promise.resolve(readable()));
    view.rerender(viewer(false));
    await waitFor(() => expect(screen.queryByText('Logs not shared')).not.toBeInTheDocument());
    expect(detApi.StreamingJobs.taskLogsFields).toHaveBeenCalledTimes(opened + 1);
  });

  it('keeps a refusal while stale capabilities still show the logs', async () => {
    const view = setup(false);
    expect(await screen.findByText('Logs not shared')).toBeInTheDocument();
    await settle();
    const calls = fetchMock.mock.calls.length;
    const opened = vi.mocked(detApi.StreamingJobs.taskLogsFields).mock.calls.length;

    view.rerender(viewer(false));
    view.rerender(viewer(undefined));
    view.rerender(viewer(false));
    await settle();
    expect(screen.getByText('Logs not shared')).toBeInTheDocument();
    expect(fetchMock.mock.calls.length).toBe(calls);
    expect(detApi.StreamingJobs.taskLogsFields).toHaveBeenCalledTimes(opened);
  });

  it('opens the logs again once when the user comes back to the page', async () => {
    setup(false);
    expect(await screen.findByText('Logs not shared')).toBeInTheDocument();
    await settle();
    const opened = vi.mocked(detApi.StreamingJobs.taskLogsFields).mock.calls.length;

    setPageHidden(true);
    setPageHidden(false);
    // Still refused: one attempt per return, then the refusal again.
    expect(await screen.findByText('Logs not shared')).toBeInTheDocument();
    await settle();
    expect(detApi.StreamingJobs.taskLogsFields).toHaveBeenCalledTimes(opened + 1);
    expect(screen.getByText('Logs not shared')).toBeInTheDocument();

    fetchMock.mockImplementation(() => Promise.resolve(readable()));
    setPageHidden(true);
    setPageHidden(false);
    await waitFor(() => expect(screen.queryByText('Logs not shared')).not.toBeInTheDocument());
  });
});
