import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import React, { forwardRef, useImperativeHandle } from 'react';
import { HelmetProvider } from 'react-helmet-async';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { useSessionCheck } from 'hooks/useAuthCheck';
import { getCurrentUser, getShells, getTask } from 'services/api';
import { ShellTerminalClose, ShellTerminalHandlers } from 'services/shellTerminal';
import authStore from 'stores/auth';
import userStore from 'stores/users';
import { CommandState, CommandType } from 'types';
import { reloadPage } from 'utils/browser';

import ShellTerminal from './ShellTerminal';

const SHELL_ID = 'shell-1';

vi.mock('react-router-dom', async (importOriginal) => ({
  ...(await importOriginal<typeof import('react-router-dom')>()),
  useParams: () => ({ taskId: SHELL_ID }),
}));

// No getShell: the page must not fetch the endpoint that returns the shell's private key. Vitest
// throws if a module uses an export that the mock does not define.
vi.mock('services/api', () => ({
  getCurrentUser: vi.fn(),
  getShells: vi.fn(),
  getTask: vi.fn(),
  killTask: vi.fn(),
  storeSessionToken: vi.fn(),
}));

vi.mock('utils/browser', async (importOriginal) => ({
  ...(await importOriginal<typeof import('utils/browser')>()),
  reloadPage: vi.fn(),
}));

vi.mock('routes/utils', async (importOriginal) => ({
  ...(await importOriginal<typeof import('routes/utils')>()),
  serverAddress: () => 'https://det.example.org',
}));

vi.mock('hooks/usePermissions', () => ({
  default: () => ({ canModifyWorkspaceNSC: () => true }),
}));

const view = vi.hoisted(() => ({
  props: undefined as
    | undefined
    | {
        onInput: (d: Uint8Array) => void;
        onTitleChange: (t: string) => void;
      },
  reset: vi.fn(),
  write: vi.fn(),
}));

vi.mock('components/ShellTerminalView', () => ({
  default: forwardRef((props: NonNullable<typeof view.props>, ref) => {
    view.props = props;
    useImperativeHandle(ref, () => ({
      focus: vi.fn(),
      reset: view.reset,
      size: () => ({ cols: 132, rows: 43 }),
      write: view.write,
    }));
    return <div data-testid="terminal-view" />;
  }),
}));

const sockets = vi.hoisted(
  () =>
    [] as { url: string; handlers: ShellTerminalHandlers; sent: Uint8Array[]; closed: boolean }[],
);

vi.mock('services/shellTerminal', async (importOriginal) => ({
  ...(await importOriginal<typeof import('services/shellTerminal')>()),
  ShellTerminalSocket: class {
    #s: (typeof sockets)[number];
    constructor(url: string, handlers: ShellTerminalHandlers) {
      this.#s = { closed: false, handlers, sent: [], url };
      sockets.push(this.#s);
    }
    send(data: Uint8Array) {
      this.#s.sent.push(data);
    }
    resize() {}
    close() {
      this.#s.closed = true;
    }
  },
}));

const shell = {
  id: SHELL_ID,
  name: 'Shell (brave-otter)',
  resourcePool: 'default',
  startTime: '2026-01-01T00:00:00Z',
  state: CommandState.Running,
  type: CommandType.Shell,
  userId: 7,
  workspaceId: 1,
};

const taskWith = (
  isReady: boolean,
  state: CommandState = CommandState.Running,
  endTime?: string,
) => ({
  allocations: [{ isReady, state, taskId: SHELL_ID }],
  endTime,
  startTime: '2026-01-01T00:00:00Z',
  taskId: SHELL_ID,
});

const OWNER = { id: 7, isActive: true, isAdmin: false, username: 'owner' };

/* App's session check, which runs next to every page. */
const SessionCheck = () => {
  useSessionCheck(true);
  return null;
};

const setup = (beside?: React.ReactNode) =>
  render(
    <BrowserRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <HelmetProvider>
            <ConfirmationProvider>
              {beside}
              <ShellTerminal />
            </ConfirmationProvider>
          </HelmetProvider>
        </ThemeProvider>
      </UIProvider>
    </BrowserRouter>,
  );

const closeSession = (close: Partial<ShellTerminalClose>) =>
  act(() =>
    sockets[sockets.length - 1].handlers.onClose({
      code: 1000,
      wasOpen: true,
      wasReady: true,
      ...close,
    }),
  );

/* Shows the tab again, which checks the session, and waits for the check to end. */
const showTab = async () => {
  const checks = vi.mocked(getCurrentUser).mock.calls.length;
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
  document.dispatchEvent(new Event('visibilitychange'));
  await waitFor(() => expect(getCurrentUser).toHaveBeenCalledTimes(checks + 1));
  await act(() => new Promise((resolve) => setTimeout(resolve)));
};

/* Opens a connected terminal next to App's session check, signed in as the shell's owner. */
const connectWithSessionCheck = async () => {
  userStore.updateCurrentUser(OWNER);
  vi.mocked(getCurrentUser).mockResolvedValue(OWNER);
  vi.mocked(getTask).mockResolvedValue(taskWith(true));
  setup(<SessionCheck />);
  await waitFor(() => expect(sockets).toHaveLength(1));
  act(() => sockets[0].handlers.onReady?.());
  expect(screen.getByTestId('shell-terminal-status')).toHaveTextContent('Connected');
  await waitFor(() => expect(getCurrentUser).toHaveBeenCalledTimes(1));
};

describe('ShellTerminal', () => {
  beforeEach(() => {
    sockets.length = 0;
    view.props = undefined;
    vi.mocked(getShells).mockResolvedValue([shell]);
    userStore.reset();
    // The page's route needs a signed-in user.
    authStore.reset();
    authStore.setAuth({ isAuthenticated: true });
    authStore.setAuthChecked();
  });

  afterEach(() => vi.restoreAllMocks());

  it('connects only once the shell is ready', async () => {
    vi.mocked(getTask).mockResolvedValue(taskWith(false));
    setup();
    expect(await screen.findByText('Shell (brave-otter)')).toBeInTheDocument();
    expect(screen.getByTestId('shell-terminal-status')).toHaveTextContent('Waiting');
    await waitFor(() => expect(getTask).toHaveBeenCalledTimes(2), { timeout: 3000 });
    expect(sockets).toHaveLength(0);

    vi.mocked(getTask).mockResolvedValue(taskWith(true));
    await waitFor(() => expect(sockets).toHaveLength(1), { timeout: 3000 });
    const url = new URL(sockets[0].url);
    expect(url.origin).toBe('wss://det.example.org');
    expect(url.pathname).toBe(`/ws/shells/${SHELL_ID}/terminal`);
    expect(url.searchParams.get('cols')).toBe('132');
    expect(url.searchParams.get('rows')).toBe('43');
    expect(url.search).not.toMatch(/key|token/i);
  });

  it('forwards input, asks before leaving, and offers to reconnect', async () => {
    vi.mocked(getTask).mockResolvedValue(taskWith(true));
    setup();
    await waitFor(() => expect(sockets).toHaveLength(1));
    act(() => sockets[0].handlers.onReady?.());
    expect(screen.getByTestId('shell-terminal-status')).toHaveTextContent('Connected');

    act(() => view.props?.onInput(new Uint8Array([108, 115])));
    expect(sockets[0].sent).toEqual([new Uint8Array([108, 115])]);

    const leave = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(leave);
    expect(leave.defaultPrevented).toBe(true);

    act(() => sockets[0].handlers.onData(new Uint8Array([104, 105])));
    expect(view.write).toHaveBeenCalledWith(new Uint8Array([104, 105]));

    closeSession({ exitCode: 0 });
    expect(await screen.findByText('Session ended (exit status 0).')).toBeInTheDocument();
    expect(screen.getByTestId('shell-terminal-status')).toHaveTextContent('Disconnected');
    // Leaving no longer asks once the session has ended.
    const leaveAfter = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(leaveAfter);
    expect(leaveAfter.defaultPrevented).toBe(false);

    await userEvent.click(screen.getByRole('button', { name: 'Reconnect' }));
    await waitFor(() => expect(sockets).toHaveLength(2));
    expect(view.reset).toHaveBeenCalled();
  });

  it('does not offer to reconnect after the login session ended', async () => {
    vi.mocked(getTask).mockResolvedValue(taskWith(true));
    setup();
    await waitFor(() => expect(sockets).toHaveLength(1));
    // How the master closes a terminal when its login session expires or is revoked.
    closeSession({ code: 4401, errorCode: 'session_expired' });
    expect(
      await screen.findByText('Your login session has ended. Sign in again to open a terminal.'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Reconnect' })).not.toBeInTheDocument();
  });

  it('offers to reconnect after the maximum session length', async () => {
    vi.mocked(getTask).mockResolvedValue(taskWith(true));
    setup();
    await waitFor(() => expect(sockets).toHaveLength(1));
    closeSession({ code: 4408, errorCode: 'time_limit' });
    expect(
      await screen.findByText('The terminal reached its maximum session length.'),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Reconnect' })).toBeInTheDocument();
  });

  it('shows an ended shell without connecting', async () => {
    vi.mocked(getTask).mockResolvedValue(
      taskWith(false, CommandState.Terminated, '2026-01-01T01:00:00Z'),
    );
    setup();
    expect(await screen.findByText('The shell has ended.')).toBeInTheDocument();
    expect(sockets).toHaveLength(0);
  });

  it('shows terminal titles as plain text', async () => {
    vi.mocked(getTask).mockResolvedValue(taskWith(true));
    setup();
    await waitFor(() => expect(view.props).toBeDefined());
    act(() => view.props?.onTitleChange('<img src=x onerror="alert(1)">user@box: ~'));
    expect(screen.getByTestId('shell-terminal-title')).toHaveTextContent(
      '<img src=x onerror="alert(1)">user@box: ~',
    );
    expect(document.querySelector('img')).toBeNull();
  });

  it('still asks before leaving while the same user is signed in', async () => {
    await connectWithSessionCheck();

    await showTab();
    expect(reloadPage).not.toHaveBeenCalled();
    const leave = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(leave);
    expect(leave.defaultPrevented).toBe(true);
  });

  it('reloads without asking when another tab signed in as someone else', async () => {
    await connectWithSessionCheck();
    // The browser asks the page before it reloads.
    const leaving: Event[] = [];
    vi.mocked(reloadPage).mockImplementation(() => {
      const leave = new Event('beforeunload', { cancelable: true });
      window.dispatchEvent(leave);
      leaving.push(leave);
    });

    // Another tab signed out and signed in as someone else: the shared session cookie is theirs.
    vi.mocked(getCurrentUser).mockResolvedValue({ ...OWNER, id: 8, username: 'other' });
    await showTab();
    // Staying would keep this user's page while its requests run as the other user.
    expect(reloadPage).toHaveBeenCalledTimes(1);
    expect(leaving).toHaveLength(1);
    expect(leaving[0].defaultPrevented).toBe(false);
    expect(authStore.isAuthenticated.get()).toBe(false);
  });
});
