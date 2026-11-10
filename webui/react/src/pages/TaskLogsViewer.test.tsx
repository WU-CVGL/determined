import { act, render, screen, waitFor } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';

import { ThemeProvider } from 'components/ThemeProvider';
import usePageVisibility from 'hooks/usePageVisibility';
import { detApi } from 'services/apiConfig';
import handleError from 'utils/error';
import { logRequests, logsFetch } from 'utils/tests/logsFetch';

import { TaskLogsViewer } from './TaskLogs';

vi.mock('services/api', () => ({ getTask: vi.fn() }));
vi.mock('services/apiConfig', () => ({
  detApi: {
    StreamingJobs: {
      // The request options are the last argument, with the signal of the request.
      taskLogs: vi.fn((id: string, limit: number, follow: boolean, ...rest: unknown[]) => ({
        options: rest.at(-1),
        url: `/api/v1/tasks/${id}/logs?limit=${limit}&follow=${follow}`,
      })),
      taskLogsFields: vi.fn((id: string, follow: boolean, options: unknown) => ({
        options,
        url: `/api/v1/tasks/${id}/logs/fields?follow=${follow}`,
      })),
    },
  },
}));
// Renders every log line: jsdom gives the viewer's scroll parent no height, so Virtuoso shows none.
vi.mock('react-virtuoso', async (importOriginal) => {
  const { forwardRef, Fragment, useImperativeHandle } =
    await vi.importActual<typeof import('react')>('react');
  const List = forwardRef<
    { scrollToIndex: () => void },
    { data?: unknown[]; itemContent: (index: number, item: unknown) => React.ReactNode }
  >(({ data, itemContent }, ref) => {
    useImperativeHandle(ref, () => ({ scrollToIndex: () => undefined }));
    return (
      <div>
        {data?.map((item, index) => <Fragment key={index}>{itemContent(index, item)}</Fragment>)}
      </div>
    );
  });
  return { ...(await importOriginal<typeof import('react-virtuoso')>()), Virtuoso: List };
});
vi.mock('utils/error', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/error')>()),
  default: vi.fn(),
}));

const TASK_ID = 'task-1';

const PageVisibility = () => {
  usePageVisibility();
  return null;
};

// The same settings on every render, as the settings store gives them.
const settings = {};
const resetSettings = vi.fn();
const updateSettings = vi.fn();

const viewer = (logsHidden?: boolean) => (
  <UIProvider theme={DefaultTheme.Light}>
    <ThemeProvider>
      <PageVisibility />
      <TaskLogsViewer
        logsHidden={logsHidden}
        resetSettings={resetSettings}
        settings={settings}
        taskId={TASK_ID}
        updateSettings={updateSettings}
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

  it('reads the logs again and follows them when they are shared again without a refusal', async () => {
    const { fetch, requests } = logsFetch(['first line', 'second line']);
    vi.stubGlobal('fetch', fetch);
    const view = render(viewer(false));
    expect(await screen.findByText('second line')).toBeInTheDocument();
    await waitFor(() => expect(logRequests(requests, true)).toHaveLength(1));

    view.rerender(viewer(true));
    expect(screen.getByText('Logs not shared')).toBeInTheDocument();
    expect(requests.every((r) => r.signal?.aborted)).toBe(true);
    const sent = requests.length;

    view.rerender(viewer(false));
    expect(await screen.findByText('second line')).toBeInTheDocument();
    expect(screen.getByText('first line')).toBeInTheDocument();
    expect(screen.queryByText('No logs to show.')).not.toBeInTheDocument();
    const after = requests.slice(sent);
    expect(logRequests(after, false)).toHaveLength(1);
    expect(logRequests(after, true)).toHaveLength(1);
    expect(logRequests(after, true)[0].signal?.aborted).toBe(false);
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
