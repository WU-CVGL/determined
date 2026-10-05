import { paths, serverAddress } from 'routes/utils';

/**
 * Client for the master's shell terminal WebSocket (GET /ws/shells/:id/terminal).
 *
 * The master opens the SSH session to the shell itself, with the shell's key; this client only
 * carries terminal input and output. Binary frames carry raw terminal bytes in both directions;
 * text frames carry JSON control messages.
 */

/** The master accepts WebSocket messages of up to 64 KiB; input is sent in 32 KiB chunks. */
export const MAX_INPUT_CHUNK = 32 * 1024;
export const RESIZE_DEBOUNCE_MS = 100;

export const CloseCode = {
  Abnormal: 1006,
  Ended: 4410,
  Forbidden: 4403,
  GoingAway: 1001,
  Internal: 1011,
  Normal: 1000,
  NotReady: 4409,
  Timeout: 4408,
  TooBig: 1009,
  Unauthenticated: 4401,
  Unavailable: 4502,
} as const;

export interface ShellTerminalSize {
  cols: number;
  rows: number;
}

/** Maps the browser's language, such as "en-US", to a UTF-8 locale name like "en_US.UTF-8". */
export const browserLang = (
  language: string | undefined = navigator.language,
): string | undefined => {
  if (!language) return undefined;
  const match = /^([a-zA-Z]{2,3})(?:[-_]([a-zA-Z]{2}))?(?:[-_].*)?$/.exec(language);
  if (!match) return undefined;
  const [, lang, region] = match;
  return `${lang.toLowerCase()}${region ? `_${region.toUpperCase()}` : ''}.UTF-8`;
};

/**
 * Builds the terminal URL from the master's address, keeping any path prefix of a reverse proxy.
 */
export const shellTerminalUrl = (
  shellId: string,
  size: ShellTerminalSize,
  lang?: string,
  base: string = serverAddress(),
): string => {
  const url = new URL(base, window.location.href);
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  const prefix = url.pathname.endsWith('/') ? url.pathname : `${url.pathname}/`;
  url.pathname = `${prefix}ws/shells/${encodeURIComponent(shellId)}/terminal`;
  const params = new URLSearchParams({ cols: String(size.cols), rows: String(size.rows) });
  if (lang) params.set('lang', lang);
  url.search = params.toString();
  url.hash = '';
  return url.toString();
};

export interface ShellTerminalClose {
  code: number;
  /** True when the WebSocket was open before it closed. */
  wasOpen: boolean;
  /** True when the login shell had started. */
  wasReady: boolean;
  /** The login shell's exit status, when it exited. */
  exitCode?: number;
  /** The error code of the master's last error message, if any. */
  errorCode?: string;
}

export interface ShellTerminalHandlers {
  onClose: (close: ShellTerminalClose) => void;
  onData: (data: Uint8Array) => void;
  onReady?: () => void;
}

type WebSocketLike = Pick<
  WebSocket,
  'binaryType' | 'close' | 'onclose' | 'onerror' | 'onmessage' | 'onopen' | 'readyState' | 'send'
>;

export type WebSocketFactory = (url: string) => WebSocketLike;

const defaultFactory: WebSocketFactory = (url) => new WebSocket(url);

/** One terminal session. Create a new instance to reconnect; the master starts a new session. */
export class ShellTerminalSocket {
  readonly #ws: WebSocketLike;
  readonly #handlers: ShellTerminalHandlers;
  readonly #encoder = new TextEncoder();
  #resizeTimer?: ReturnType<typeof setTimeout>;
  #pendingSize?: ShellTerminalSize;
  #wasOpen = false;
  #wasReady = false;
  #exitCode?: number;
  #errorCode?: string;
  #closed = false;

  constructor(url: string, handlers: ShellTerminalHandlers, factory = defaultFactory) {
    this.#handlers = handlers;
    this.#ws = factory(url);
    this.#ws.binaryType = 'arraybuffer';
    this.#ws.onopen = () => {
      this.#wasOpen = true;
      // A resize requested while connecting is sent now, unless its timer is still running.
      if (!this.#resizeTimer) this.#sendPendingSize();
    };
    this.#ws.onmessage = (event: MessageEvent) => this.#onMessage(event);
    this.#ws.onclose = (event: CloseEvent) => this.#onClose(event.code);
    // An error is always followed by a close event.
    this.#ws.onerror = () => undefined;
  }

  get isReady(): boolean {
    return this.#wasReady && !this.#closed;
  }

  /** Sends terminal input, split into messages the master accepts. */
  send(data: string | Uint8Array): void {
    if (this.#closed || this.#ws.readyState !== WebSocket.OPEN) return;
    const bytes = typeof data === 'string' ? this.#encoder.encode(data) : data;
    for (let i = 0; i < bytes.length; i += MAX_INPUT_CHUNK) {
      this.#ws.send(bytes.slice(i, i + MAX_INPUT_CHUNK));
    }
  }

  /**
   * Requests a new terminal size; quick successive calls send only the last size. A size requested
   * before the WebSocket opens is sent once it opens.
   */
  resize(size: ShellTerminalSize): void {
    this.#pendingSize = size;
    if (this.#resizeTimer) return;
    this.#resizeTimer = setTimeout(() => {
      this.#resizeTimer = undefined;
      this.#sendPendingSize();
    }, RESIZE_DEBOUNCE_MS);
  }

  close(): void {
    if (this.#resizeTimer) clearTimeout(this.#resizeTimer);
    this.#resizeTimer = undefined;
    this.#ws.close(CloseCode.Normal);
  }

  /** Sends the pending size, and keeps it until the WebSocket is open. */
  #sendPendingSize(): void {
    const pending = this.#pendingSize;
    if (!pending || this.#closed || this.#ws.readyState !== WebSocket.OPEN) return;
    this.#pendingSize = undefined;
    this.#ws.send(JSON.stringify({ cols: pending.cols, rows: pending.rows, type: 'resize' }));
  }

  #onMessage(event: MessageEvent): void {
    if (event.data instanceof ArrayBuffer) {
      this.#handlers.onData(new Uint8Array(event.data));
      return;
    }
    if (typeof event.data !== 'string') return;
    let msg: { type?: unknown; code?: unknown };
    try {
      msg = JSON.parse(event.data);
    } catch {
      return;
    }
    switch (msg.type) {
      case 'ready':
        this.#wasReady = true;
        this.#handlers.onReady?.();
        break;
      case 'exit':
        if (typeof msg.code === 'number') this.#exitCode = msg.code;
        break;
      case 'error':
        if (typeof msg.code === 'string') this.#errorCode = msg.code;
        break;
    }
  }

  #onClose(code: number): void {
    if (this.#closed) return;
    this.#closed = true;
    if (this.#resizeTimer) clearTimeout(this.#resizeTimer);
    this.#handlers.onClose({
      code,
      errorCode: this.#errorCode,
      exitCode: this.#exitCode,
      wasOpen: this.#wasOpen,
      wasReady: this.#wasReady,
    });
  }
}

export interface CloseDescription {
  message: string;
  /** Whether the page should retry by itself, after a delay. */
  retry: boolean;
  /** Whether the user may reconnect, which starts a new session. */
  canReconnect: boolean;
}

/** Describes how a session ended, for the terminal page. */
export const describeClose = (close: ShellTerminalClose): CloseDescription => {
  switch (close.code) {
    case CloseCode.Normal:
      return {
        canReconnect: true,
        message:
          close.exitCode === undefined
            ? 'Session ended.'
            : `Session ended (exit status ${close.exitCode}).`,
        retry: false,
      };
    case CloseCode.NotReady:
      return { canReconnect: true, message: 'Waiting for the shell to be ready…', retry: true };
    case CloseCode.Ended:
      return { canReconnect: false, message: 'The shell has ended.', retry: false };
    case CloseCode.Unauthenticated:
      return {
        canReconnect: false,
        message: 'Your login session has ended. Sign in again to open a terminal.',
        retry: false,
      };
    case CloseCode.Forbidden:
      return { canReconnect: false, message: 'You can no longer use this shell.', retry: false };
    case CloseCode.Timeout:
      return {
        canReconnect: true,
        message:
          close.errorCode === 'idle_timeout'
            ? 'The terminal was closed after a period of inactivity.'
            : 'The terminal reached its maximum session length.',
        retry: false,
      };
    case CloseCode.Unavailable:
      return {
        canReconnect: true,
        message: close.wasReady
          ? 'The connection to the shell was lost.'
          : 'Could not connect to the shell.',
        retry: false,
      };
    case CloseCode.GoingAway:
      return { canReconnect: true, message: 'The master is restarting.', retry: false };
    default:
      if (!close.wasOpen) {
        return {
          canReconnect: true,
          message:
            'Cannot open a terminal. The shell may not exist or may have ended, only its owner ' +
            'and administrators can open it, and each user can have only a few terminals open.',
          retry: false,
        };
      }
      return { canReconnect: true, message: 'The connection was lost.', retry: false };
  }
};

/**
 * Returns the URL to open for a link in terminal output, or undefined when it must not be opened.
 * Terminal output is untrusted: only http and https links are allowed.
 */
export const safeTerminalLink = (uri: string): string | undefined => {
  let url: URL;
  try {
    url = new URL(uri);
  } catch {
    return undefined;
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return undefined;
  return url.href;
};

/** Opens a link from terminal output after the user confirms the exact URL. */
export const openTerminalLink = (
  uri: string,
  confirmFn: (message: string) => boolean = (m) => window.confirm(m),
  openFn: typeof window.open = (...args) => window.open(...args),
): boolean => {
  const href = safeTerminalLink(uri);
  if (!href) return false;
  if (!confirmFn(`Open this link from the terminal in a new tab?\n\n${href}`)) return false;
  openFn(href, '_blank', 'noopener,noreferrer');
  return true;
};

/** Opens the terminal page of a shell in its own browser tab. */
export const openShellTerminalTab = (shellId: string): void => {
  window.open(
    `${process.env.PUBLIC_URL}${paths.shellTerminal(shellId)}`,
    `shell-terminal-${shellId}`,
  );
};
