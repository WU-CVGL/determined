import Button from 'hew/Button';
import Spinner from 'hew/Spinner';
import { useToast } from 'hew/Toast';
import React, { useCallback, useEffect, useRef, useState } from 'react';

import ShellTerminalView, { ShellTerminalViewHandle } from 'components/ShellTerminalView';
import useUI from 'components/ThemeProvider';
import {
  browserLang,
  describeClose,
  ShellTerminalClose,
  ShellTerminalSize,
  ShellTerminalSocket,
  shellTerminalUrl,
} from 'services/shellTerminal';
import authStore from 'stores/auth';
import { copyToClipboard } from 'utils/dom';
import handleError from 'utils/error';

import css from './ShellTerminalPane.module.scss';

/** Where the shell is, as the page that polls it says. */
export type ShellTerminalPhase = 'waiting' | 'ready' | 'ended';
type Connection = 'connecting' | 'connected' | 'closed';

interface Props {
  /** Whether the pane is shown; a hidden terminal measures nothing, so it is fitted when shown. */
  active: boolean;
  phase: ShellTerminalPhase;
  shellId: string;
}

const MAX_TITLE = 200;

const retryDelay = (attempt: number): number => Math.min(1000 * 2 ** attempt, 10000);

/**
 * A terminal in a shell, as a pane of the task page, which polls the shell and says whether it is
 * ready. The pane never receives the shell's private key: the master opens the SSH session.
 */
const ShellTerminalPane: React.FC<Props> = ({ active, phase, shellId }: Props) => {
  const { ui } = useUI();
  const { openToast } = useToast();

  const [connection, setConnection] = useState<Connection>('connecting');
  const [session, setSession] = useState(0);
  const [notice, setNotice] = useState<string>();
  const [canReconnect, setCanReconnect] = useState(false);
  const [title, setTitle] = useState('');
  const terminalRef = useRef<ShellTerminalViewHandle>(null);
  const socketRef = useRef<ShellTerminalSocket>();
  const retries = useRef(0);
  const retryTimer = useRef<ReturnType<typeof setTimeout>>();

  // The first session starts once the shell is ready.
  useEffect(() => {
    if (phase === 'ready' && session === 0) {
      setConnection('connecting');
      setSession(1);
    }
  }, [phase, session]);

  useEffect(() => () => clearTimeout(retryTimer.current), []);

  useEffect(() => {
    if (active) terminalRef.current?.fit();
  }, [active]);

  const onClose = useCallback((close: ShellTerminalClose) => {
    const desc = describeClose(close);
    socketRef.current = undefined;
    if (desc.retry) {
      // The shell is not ready after all: try again in a while.
      retryTimer.current = setTimeout(
        () => setSession((s) => s + 1),
        retryDelay(retries.current++),
      );
      return;
    }
    setConnection('closed');
    setNotice(desc.message);
    setCanReconnect(desc.canReconnect);
    terminalRef.current?.write(`\r\n\x1b[2m[${desc.message}]\x1b[0m\r\n`);
  }, []);

  // One WebSocket per session; reconnecting starts a new session in the shell.
  useEffect(() => {
    if (session === 0) return;
    terminalRef.current?.fit();
    const size = terminalRef.current?.size() ?? { cols: 80, rows: 24 };
    const socket = new ShellTerminalSocket(shellTerminalUrl(shellId, size, browserLang()), {
      onClose,
      onData: (data) => terminalRef.current?.write(data),
      onReady: () => {
        retries.current = 0;
        setConnection('connected');
        setNotice(undefined);
        terminalRef.current?.focus();
      },
    });
    socketRef.current = socket;
    return () => {
      socketRef.current = undefined;
      socket.close();
    };
  }, [session, shellId, onClose]);

  // Closing the tab hangs up the login shell, so ask first. Not when the page is no longer signed
  // in: it reloads because another tab signed in as someone else (useAuthCheck), and must not stay
  // with this user's state while its requests run as the other user.
  useEffect(() => {
    if (connection !== 'connected') return;
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (!authStore.isAuthenticated.get()) return;
      e.preventDefault();
      e.returnValue = '';
      return '';
    };
    window.addEventListener('beforeunload', onBeforeUnload);
    return () => window.removeEventListener('beforeunload', onBeforeUnload);
  }, [connection]);

  const onInput = useCallback((data: Uint8Array) => socketRef.current?.send(data), []);
  const onResize = useCallback((size: ShellTerminalSize) => socketRef.current?.resize(size), []);
  // Terminal titles come from the container: keep them as plain text.
  const onTitleChange = useCallback((t: string) => setTitle(t.slice(0, MAX_TITLE)), []);

  const reconnect = useCallback(() => {
    terminalRef.current?.reset();
    setNotice(undefined);
    setCanReconnect(false);
    setConnection('connecting');
    setSession((s) => s + 1);
  }, []);

  const copyCommand = useCallback(async () => {
    try {
      await copyToClipboard(`det shell open ${shellId}`);
      openToast({ severity: 'Confirm', title: 'Command copied to the clipboard.' });
    } catch (e) {
      handleError(e, { publicMessage: 'Unable to copy to the clipboard.', silent: false });
    }
  }, [openToast, shellId]);

  let status: string;
  if (phase === 'ended') status = 'Ended';
  else if (phase === 'waiting' || session === 0) status = 'Waiting…';
  else if (connection === 'connecting') status = 'Connecting…';
  else if (connection === 'connected') status = 'Connected';
  else status = 'Disconnected';

  return (
    <div className={css.base}>
      <div className={css.bar}>
        <div className={css.info}>
          <span className={css.status} data-testid="shell-terminal-status">
            {status}
          </span>
          {title && (
            <span className={css.title} data-testid="shell-terminal-title" title={title}>
              {title}
            </span>
          )}
        </div>
        <div className={css.actions}>
          {phase === 'ready' && connection === 'closed' && canReconnect && (
            <Button size="small" type="primary" onClick={reconnect}>
              Reconnect
            </Button>
          )}
          <Button size="small" onClick={copyCommand}>
            Copy CLI Command
          </Button>
        </div>
      </div>
      {notice && (
        <div className={css.notice} role="status">
          {notice}
        </div>
      )}
      <div className={css.content}>
        {session === 0 && phase !== 'ended' && (
          <div className={css.message}>
            <Spinner spinning />
          </div>
        )}
        {session === 0 && phase === 'ended' && (
          <div className={css.message}>The shell has ended.</div>
        )}
        {session > 0 && (
          <ShellTerminalView
            darkMode={ui.darkLight === 'dark'}
            ref={terminalRef}
            onInput={onInput}
            onResize={onResize}
            onTitleChange={onTitleChange}
          />
        )}
      </div>
    </div>
  );
};

export default ShellTerminalPane;
