import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DefaultTheme, UIProvider } from 'hew/Theme';
import { ConfirmationProvider } from 'hew/useConfirm';
import { forwardRef, useImperativeHandle } from 'react';
import { HelmetProvider } from 'react-helmet-async';
import { BrowserRouter } from 'react-router-dom';

import { ThemeProvider } from 'components/ThemeProvider';
import { getShells, getTask } from 'services/api';
import { ShellTerminalClose, ShellTerminalHandlers } from 'services/shellTerminal';
import { CommandState, CommandType } from 'types';

import ShellTerminal from './ShellTerminal';

const SHELL_ID = 'shell-1';

vi.mock('react-router-dom', async (importOriginal) => ({
  ...(await importOriginal<typeof import('react-router-dom')>()),
  useParams: () => ({ taskId: SHELL_ID }),
}));

// No getShell: the page must not fetch the endpoint that returns the shell's private key. Vitest
// throws if a module uses an export that the mock does not define.
vi.mock('services/api', () => ({
  getShells: vi.fn(),
  getTask: vi.fn(),
  killTask: vi.fn(),
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

const setup = () =>
  render(
    <BrowserRouter>
      <UIProvider theme={DefaultTheme.Light}>
        <ThemeProvider>
          <HelmetProvider>
            <ConfirmationProvider>
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

describe('ShellTerminal', () => {
  beforeEach(() => {
    sockets.length = 0;
    view.props = undefined;
    vi.mocked(getShells).mockResolvedValue([shell]);
  });

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
});
