import { act, render, screen, waitFor } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { getTaskServiceCredential, getTaskServices } from 'services/api';
import {
  TaskServiceProtocol,
  V1GetTaskServicesResponse,
  V1TaskServiceState,
} from 'services/api-ts-sdk';
import { DetError } from 'utils/error';
import { replaceWithExternalUrl } from 'utils/routes';

import TaskServiceOpen from './TaskServiceOpen';

const TASK_ID = 'task-1';
const JUPYTER_ID = `${TASK_ID}:2900`;

vi.mock('services/api', () => ({
  getTaskServiceCredential: vi.fn(),
  getTaskServices: vi.fn(),
}));

vi.mock('utils/routes', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/routes')>()),
  replaceWithExternalUrl: vi.fn(),
}));

vi.mock('routes/utils', async (importOriginal) => ({
  ...(await importOriginal<typeof import('routes/utils')>()),
  serverAddress: (path = '') => `https://det.example.org${path}`,
}));

const services = (): V1GetTaskServicesResponse => ({
  primary: 'ssh',
  services: [
    {
      mayConnect: true,
      name: 'ssh',
      protocol: TaskServiceProtocol.TCP,
      serviceId: TASK_ID,
      state: V1TaskServiceState.READY,
      url: '',
    },
    {
      mayConnect: true,
      name: 'jupyter',
      protocol: TaskServiceProtocol.HTTP,
      serviceId: JUPYTER_ID,
      state: V1TaskServiceState.READY,
      url: `/proxy/${JUPYTER_ID}/`,
    },
  ],
  taskId: TASK_ID,
  userId: 1,
  workspaceId: 1,
});

const setup = (service: string) =>
  render(
    <UIProvider theme={DefaultTheme.Light}>
      <ThemeProvider>
        <MemoryRouter initialEntries={[`/tasks/${TASK_ID}/services/${service}/open`]}>
          <Routes>
            <Route element={<TaskServiceOpen />} path="/tasks/:taskId/services/:service/open" />
          </Routes>
        </MemoryRouter>
      </ThemeProvider>
    </UIProvider>,
  );

describe('TaskServiceOpen', () => {
  beforeEach(() => {
    vi.mocked(getTaskServices).mockReset();
    vi.mocked(getTaskServiceCredential).mockReset();
    vi.mocked(replaceWithExternalUrl).mockReset();
    vi.stubGlobal('fetch', vi.fn());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('opens the URL of the credential', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(services());
    vi.mocked(getTaskServiceCredential).mockResolvedValue({
      token: 'tok',
      url: `/proxy/${JUPYTER_ID}/?token=tok`,
    });
    setup('jupyter');
    await waitFor(() =>
      expect(replaceWithExternalUrl).toHaveBeenCalledWith(
        `https://det.example.org/proxy/${JUPYTER_ID}/?token=tok`,
      ),
    );
    expect(getTaskServices).toHaveBeenCalledWith({ taskId: TASK_ID });
    expect(getTaskServiceCredential).toHaveBeenCalledWith({ service: 'jupyter', taskId: TASK_ID });
    expect(fetch).not.toHaveBeenCalled();
  });

  it('shows the error of the credential', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(services());
    vi.mocked(getTaskServiceCredential).mockRejectedValue(
      new DetError(undefined, {
        publicMessage: "only the task's owner or an admin may read its credentials",
        publicSubject: 'Request getTaskServiceCredential failed.',
      }),
    );
    setup('jupyter');
    expect(
      await screen.findByText("only the task's owner or an admin may read its credentials"),
    ).toBeInTheDocument();
    expect(replaceWithExternalUrl).not.toHaveBeenCalled();
  });

  it('does not open the service once the page is gone', async () => {
    let answer: (response: V1GetTaskServicesResponse) => void = () => undefined;
    vi.mocked(getTaskServices).mockImplementation(
      () => new Promise<V1GetTaskServicesResponse>((resolve) => (answer = resolve)),
    );
    vi.mocked(getTaskServiceCredential).mockResolvedValue({
      token: 'tok',
      url: `/proxy/${JUPYTER_ID}/?token=tok`,
    });
    const { unmount } = setup('jupyter');
    await waitFor(() => expect(getTaskServices).toHaveBeenCalledTimes(1));
    unmount();
    answer(services());
    await act(() => new Promise((resolve) => setTimeout(resolve)));
    expect(replaceWithExternalUrl).not.toHaveBeenCalled();
  });
});
