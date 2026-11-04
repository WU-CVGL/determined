import { getTaskServiceCredential, getTaskServices } from 'services/api';
import {
  TaskServiceProtocol,
  V1GetTaskServicesResponse,
  V1TaskService,
  V1TaskServiceState,
} from 'services/api-ts-sdk';
import { DetError } from 'utils/error';

import { errorText, loginCodeServer, taskServiceUrl } from './taskService';

const TASK_ID = 'task-1';
const JUPYTER_ID = `${TASK_ID}:2900`;
const VSCODE_ID = `${TASK_ID}:2901`;
const VSCODE_LOGIN = `https://det.example.org/proxy/${VSCODE_ID}/login`;

vi.mock('services/api', () => ({
  getTaskServiceCredential: vi.fn(),
  getTaskServices: vi.fn(),
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

describe('taskServiceUrl', () => {
  beforeEach(() => {
    vi.mocked(getTaskServices).mockReset();
    vi.mocked(getTaskServiceCredential).mockReset();
    vi.stubGlobal('fetch', vi.fn());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('gives the URL of the credential', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(services());
    vi.mocked(getTaskServiceCredential).mockResolvedValue({
      token: 'tok',
      url: `/proxy/${JUPYTER_ID}/?token=tok`,
    });
    await expect(taskServiceUrl(TASK_ID, 'jupyter')).resolves.toBe(
      `https://det.example.org/proxy/${JUPYTER_ID}/?token=tok`,
    );
    expect(getTaskServices).toHaveBeenCalledWith({ taskId: TASK_ID });
    expect(getTaskServiceCredential).toHaveBeenCalledWith({ service: 'jupyter', taskId: TASK_ID });
    expect(fetch).not.toHaveBeenCalled();
  });

  it('logs in to code-server with its login form before giving its URL', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(withVSCode());
    vi.mocked(getTaskServiceCredential).mockResolvedValue({
      password: 'pass&word=',
      url: `/proxy/${VSCODE_ID}/`,
    });
    vi.mocked(fetch).mockResolvedValue({ status: 0, type: 'opaqueredirect' } as Response);
    await expect(taskServiceUrl(TASK_ID, 'vscode')).resolves.toBe(
      `https://det.example.org/proxy/${VSCODE_ID}/`,
    );
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
  });

  it('fails a code-server login once, without a retry', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(withVSCode());
    vi.mocked(getTaskServiceCredential).mockResolvedValue({
      password: 'wrong',
      url: `/proxy/${VSCODE_ID}/`,
    });
    // code-server answers a failed login with its form.
    vi.mocked(fetch).mockResolvedValue({ status: 200, type: 'basic' } as Response);
    await expect(taskServiceUrl(TASK_ID, 'vscode')).rejects.toThrow('vscode: login failed');
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it('fails code-server without a password', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(withVSCode());
    vi.mocked(getTaskServiceCredential).mockResolvedValue({ url: `/proxy/${VSCODE_ID}/` });
    await expect(taskServiceUrl(TASK_ID, 'vscode')).rejects.toThrow('vscode: login failed');
    expect(fetch).not.toHaveBeenCalled();
  });

  it('gives the URL of the service without one in the credential', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(services({ name: 'web' }));
    vi.mocked(getTaskServiceCredential).mockResolvedValue({});
    await expect(taskServiceUrl(TASK_ID, 'web')).resolves.toBe(
      `https://det.example.org/proxy/${JUPYTER_ID}/`,
    );
  });

  it('passes on the error of the credential', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(services());
    vi.mocked(getTaskServiceCredential).mockRejectedValue(
      new DetError(undefined, {
        publicMessage: "only the task's owner or an admin may read its credentials",
        publicSubject: 'Request getTaskServiceCredential failed.',
      }),
    );
    await expect(taskServiceUrl(TASK_ID, 'jupyter').catch(errorText)).resolves.toBe(
      "only the task's owner or an admin may read its credentials",
    );
  });

  it('names the state of a service that is not ready, without reading its credential', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(services({ state: V1TaskServiceState.STARTING }));
    await expect(taskServiceUrl(TASK_ID, 'jupyter')).rejects.toThrow('jupyter: STARTING');
    expect(getTaskServiceCredential).not.toHaveBeenCalled();
  });

  it('refuses a TCP service or one the task does not have', async () => {
    vi.mocked(getTaskServices).mockResolvedValue(services());
    await expect(taskServiceUrl(TASK_ID, 'ssh')).rejects.toThrow('ssh: TCP');
    await expect(taskServiceUrl(TASK_ID, 'vscode')).rejects.toThrow('No service vscode');
    expect(getTaskServiceCredential).not.toHaveBeenCalled();
  });
});

describe('loginCodeServer', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn()));
  afterEach(() => vi.unstubAllGlobals());

  it.each([
    [{ status: 0, type: 'opaqueredirect' }, true],
    [{ status: 302, type: 'basic' }, true],
    [{ status: 200, type: 'basic' }, false],
    [{ status: 401, type: 'basic' }, false],
  ])('reads a redirect as a login (%o)', async (response, loggedIn) => {
    vi.mocked(fetch).mockResolvedValue(response as Response);
    await expect(loginCodeServer(`/proxy/${VSCODE_ID}/`, 'pw')).resolves.toBe(loggedIn);
  });
});

describe('errorText', () => {
  it('prefers the public message of a DetError', () => {
    expect(errorText(new DetError(new Error('raw'), { publicMessage: 'public' }))).toBe('public');
    expect(errorText(new Error('plain'))).toBe('plain');
    expect(errorText('text')).toBe('text');
  });
});
