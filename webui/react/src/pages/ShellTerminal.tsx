import Button from 'hew/Button';
import Spinner from 'hew/Spinner';
import { useToast } from 'hew/Toast';
import useConfirm from 'hew/useConfirm';
import { Loadable } from 'hew/utils/loadable';
import { useObservable } from 'micro-observables';
import React, { useCallback, useEffect, useRef, useState } from 'react';
import { Helmet } from 'react-helmet-async';
import { useParams } from 'react-router-dom';

import ShellTerminalView, { ShellTerminalViewHandle } from 'components/ShellTerminalView';
import useUI from 'components/ThemeProvider';
import usePermissions from 'hooks/usePermissions';
import { paths } from 'routes/utils';
import { getShells, getTask, killTask } from 'services/api';
import {
  browserLang,
  describeClose,
  ShellTerminalClose,
  ShellTerminalSize,
  ShellTerminalSocket,
  shellTerminalUrl,
} from 'services/shellTerminal';
import userStore from 'stores/users';
import { CommandState, CommandTask, CommandType } from 'types';
import { copyToClipboard } from 'utils/dom';
import handleError from 'utils/error';

import css from './ShellTerminal.module.scss';

type Params = {
  taskId: string;
};

/** Where the page is: waiting for the shell, showing the terminal, or done. */
type Phase = 'waiting' | 'terminal' | 'ended' | 'missing';
type Connection = 'connecting' | 'connected' | 'closed';

const POLL_MS = 1000;
const MAX_POLL_FAILURES = 5;
const MAX_TITLE = 200;

const retryDelay = (attempt: number): number => Math.min(1000 * 2 ** attempt, 10000);

/**
 * A full-page terminal in a shell. The page never receives the shell's private key: it reads the
 * shell's details from the shell list, which has no keys, and the master opens the SSH session.
 */
const ShellTerminal: React.FC = () => {
  const { taskId } = useParams<Params>();
  const shellId = taskId ?? '';
  const { actions: uiActions, ui } = useUI();
  const confirm = useConfirm();
  const { openToast } = useToast();
  const { canModifyWorkspaceNSC } = usePermissions();
  const currentUser = Loadable.getOrElse(undefined, useObservable(userStore.currentUser));

  const [shell, setShell] = useState<CommandTask>();
  const [phase, setPhase] = useState<Phase>('waiting');
  const [connection, setConnection] = useState<Connection>('connecting');
  const [session, setSession] = useState(0);
  const [notice, setNotice] = useState<string>();
  const [canReconnect, setCanReconnect] = useState(false);
  const [title, setTitle] = useState('');
  const terminalRef = useRef<ShellTerminalViewHandle>(null);
  const socketRef = useRef<ShellTerminalSocket>();
  const retries = useRef(0);

  useEffect(() => {
    uiActions.hideChrome();
    return uiActions.showChrome;
  }, [uiActions]);

  // The shell's name and owner. The shell list never includes private keys.
  useEffect(() => {
    let cancelled = false;
    getShells({})
      .then((shells) => {
        if (!cancelled) setShell(shells.find((s) => s.id === shellId));
      })
      .catch((e) => handleError(e, { publicMessage: 'Unable to load the shell.', silent: true }));
    return () => {
      cancelled = true;
    };
  }, [shellId, phase]);

  // Wait until the shell is ready, then start a terminal session.
  useEffect(() => {
    if (phase !== 'waiting') return;
    let cancelled = false;
    let failures = 0;
    const poll = async () => {
      try {
        const task = await getTask({ taskId: shellId });
        if (cancelled) return;
        failures = 0;
        if (!task) {
          setPhase('missing');
          return;
        }
        const allocation = task.allocations[0];
        if (task.endTime || allocation?.state === CommandState.Terminated) {
          setPhase('ended');
        } else if (allocation?.isReady) {
          setPhase('terminal');
          setConnection('connecting');
          setSession((s) => s + 1);
        }
      } catch (e) {
        if (!cancelled && ++failures >= MAX_POLL_FAILURES) setPhase('missing');
        handleError(e, { publicMessage: 'Unable to find the shell.', silent: true });
      }
    };
    void poll();
    const timer = setInterval(() => void poll(), POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [phase, shellId]);

  const onClose = useCallback((close: ShellTerminalClose) => {
    const desc = describeClose(close);
    socketRef.current = undefined;
    if (desc.retry) {
      // The shell is not ready after all; wait for it again.
      const delay = retryDelay(retries.current++);
      setTimeout(() => setPhase('waiting'), delay);
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

  // Closing the tab hangs up the login shell, so ask first.
  useEffect(() => {
    if (connection !== 'connected') return;
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
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
    setPhase('waiting');
  }, []);

  const copyCommand = useCallback(async () => {
    try {
      await copyToClipboard(`det shell open ${shellId}`);
      openToast({ severity: 'Confirm', title: 'Command copied to the clipboard.' });
    } catch (e) {
      handleError(e, { publicMessage: 'Unable to copy to the clipboard.', silent: false });
    }
  }, [openToast, shellId]);

  const kill = useCallback(() => {
    confirm({
      content: 'Are you sure you want to kill this shell? Programs running in it will stop.',
      danger: true,
      okText: 'Kill',
      onConfirm: async () => {
        await killTask({ id: shellId, type: CommandType.Shell });
        socketRef.current?.close();
        setPhase('ended');
      },
      onError: handleError,
      title: 'Confirm Shell Kill',
    });
  }, [confirm, shellId]);

  const name = shell?.name || shellId;
  const asAdmin = !!shell && !!currentUser && currentUser.id !== shell.userId;
  const canKill =
    !!shell &&
    canModifyWorkspaceNSC({ userId: shell.userId, workspace: { id: shell.workspaceId } });
  const logsUrl = `${process.env.PUBLIC_URL}${paths.taskLogs({
    id: shellId,
    name,
    type: CommandType.Shell,
  })}`;

  let status: string;
  if (phase === 'waiting') status = 'Waiting for the shell…';
  else if (phase === 'ended') status = 'Ended';
  else if (phase === 'missing') status = 'Unavailable';
  else if (connection === 'connecting') status = 'Connecting…';
  else if (connection === 'connected') status = 'Connected';
  else status = 'Disconnected';

  return (
    <>
      <Helmet defer={false}>
        <title>{`${title || name} - Terminal`}</title>
      </Helmet>
      <div className={css.base}>
        <div className={css.bar}>
          <div className={css.info}>
            <span className={css.name}>{name}</span>
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
            {phase === 'terminal' && connection === 'closed' && canReconnect && (
              <Button size="small" type="primary" onClick={reconnect}>
                Reconnect
              </Button>
            )}
            <Button size="small" onClick={copyCommand}>
              Copy CLI Command
            </Button>
            <Button
              size="small"
              onClick={() => window.open(logsUrl, '_blank', 'noopener,noreferrer')}>
              View Logs
            </Button>
            {canKill && phase !== 'ended' && (
              <Button danger size="small" onClick={kill}>
                Kill
              </Button>
            )}
          </div>
        </div>
        {notice && (
          <div className={css.notice} role="status">
            {notice}
          </div>
        )}
        <div className={css.content}>
          {phase === 'waiting' && !notice && (
            <div className={css.message}>
              <Spinner spinning tip="Waiting for the shell to be ready…" />
            </div>
          )}
          {phase === 'ended' && <div className={css.message}>The shell has ended.</div>}
          {phase === 'missing' && (
            <div className={css.message}>This shell does not exist, or you cannot see it.</div>
          )}
          {(phase === 'terminal' || (phase === 'waiting' && session > 0)) && (
            <ShellTerminalView
              darkMode={ui.darkLight === 'dark'}
              ref={terminalRef}
              onInput={onInput}
              onResize={onResize}
              onTitleChange={onTitleChange}
            />
          )}
        </div>
        <div className={css.hint}>
          {asAdmin && (
            <span className={css.admin}>
              You are using another user&apos;s shell as an administrator; its log records this.{' '}
            </span>
          )}
          Closing this tab ends the terminal session and stops the programs running in it. Use tmux
          or nohup for long jobs.
        </div>
      </div>
    </>
  );
};

export default ShellTerminal;
