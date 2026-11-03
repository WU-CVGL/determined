import { render, screen, waitFor } from '@testing-library/react';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { getTaskServiceCredential, getTaskServices } from 'services/api';
import {
  TaskServiceProtocol,
  V1GetTaskServicesResponse,
  V1TaskService,
  V1TaskServiceState,
} from 'services/api-ts-sdk';
import { DetError } from 'utils/error';
import { replaceWithExternalUrl } from 'utils/routes';

import TaskServiceOpen from './TaskServiceOpen';

const TASK_ID = 'task-1';
const JUPYTER_ID = `${TASK_ID}:2900`;
const VSCODE_ID = `${TASK_ID}:2901`;
const VSCODE_LOGIN = `https://det.example.org/proxy/${VSCODE_ID}/login`;

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

const services = (jupyter: Partial<V1TaskService> = {}): V1GetTaskServicesResponse => ({
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
      ...jupyter,
    },
  ],
  taskId: TASK_ID,
  userId: 1,
  workspaceId: 1,
});

const withVSCode = (): V1GetTaskServicesResponse => {
  const response = services();
  response.services.push({
    mayConnect: true,
    name: 'vscode',
    protocol: TaskServiceProtocol.HTTP,
    serviceId: VSCODE_ID,
    state: V1TaskServiceState.READY,
    url: `/proxy/${VSCODE_ID}/`,
  });
  return response;
};

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

  it('logs in to code-server with its login form, then opens it', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(withVSCode());
    vi.mocked(getTaskServiceCredential).mockResolvedValue({
      password: 'pass&word=',
      url: `/proxy/${VSCODE_ID}/`,
    });
    vi.mocked(fetch).mockResolvedValue({ status: 0, type: 'opaqueredirect' } as Response);
    setup('vscode');
    await waitFor(() =>
      expect(replaceWithExternalUrl).toHaveBeenCalledWith(
        `https://det.example.org/proxy/${VSCODE_ID}/`,
      ),
    );
    expect(getTaskServiceCredential).toHaveBeenCalledWith({ service: 'vscode', taskId: TASK_ID });
    expect(fetch).toHaveBeenCalledTimes(1);
    const [address, init] = vi.mocked(fetch).mock.calls[0];
    expect(address).toBe(VSCODE_LOGIN);
    expect(init).toMatchObject({
      credentials: 'include',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      method: 'POST',
      redirect: 'manual',
    });
    expect(Object.fromEntries(new URLSearchParams(init?.body as string))).toEqual({
      base: `/proxy/${VSCODE_ID}/`,
      href: VSCODE_LOGIN,
      password: 'pass&word=',
    });
    expect(vi.mocked(fetch).mock.invocationCallOrder[0]).toBeLessThan(
      vi.mocked(replaceWithExternalUrl).mock.invocationCallOrder[0],
    );
  });

  it('shows a failed code-server login once and does not open it', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(withVSCode());
    vi.mocked(getTaskServiceCredential).mockResolvedValue({
      password: 'wrong',
      url: `/proxy/${VSCODE_ID}/`,
    });
    // code-server answers a failed login with its form.
    vi.mocked(fetch).mockResolvedValue({ status: 200, type: 'basic' } as Response);
    setup('vscode');
    expect(await screen.findByText('vscode: login failed')).toBeInTheDocument();
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(replaceWithExternalUrl).not.toHaveBeenCalled();
  });

  it('opens the URL of the service without one in the credential', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(services({ name: 'web' }));
    vi.mocked(getTaskServiceCredential).mockResolvedValue({});
    setup('web');
    await waitFor(() =>
      expect(replaceWithExternalUrl).toHaveBeenCalledWith(
        `https://det.example.org/proxy/${JUPYTER_ID}/`,
      ),
    );
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

  it('shows the state of a service that is not ready', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(services({ state: V1TaskServiceState.STARTING }));
    setup('jupyter');
    expect(await screen.findByText('jupyter: STARTING')).toBeInTheDocument();
    expect(getTaskServiceCredential).not.toHaveBeenCalled();
    expect(replaceWithExternalUrl).not.toHaveBeenCalled();
  });

  it('does not open a TCP service or one the task does not have', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(services());
    setup('ssh');
    expect(await screen.findByText('ssh: TCP')).toBeInTheDocument();
    setup('vscode');
    expect(await screen.findByText('No service vscode')).toBeInTheDocument();
    expect(getTaskServiceCredential).not.toHaveBeenCalled();
    expect(replaceWithExternalUrl).not.toHaveBeenCalled();
  });
});
