import { render, screen, waitFor } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';

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

const setup = (logsHidden?: boolean) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <TaskLogsViewer
        logsHidden={logsHidden}
        resetSettings={vi.fn()}
        settings={{}}
        taskId={TASK_ID}
        updateSettings={vi.fn()}
      />
    </UIProvider>,
  );

// The master refuses the logs of a job whose logs are not shared before it sends a line.
const refusal = () =>
  new Response(JSON.stringify({ error: { httpCode: 403, message: 'logs not shared' } }), {
    status: 403,
  });

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
});
