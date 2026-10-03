import {
  browserLang,
  CloseCode,
  describeClose,
  MAX_INPUT_CHUNK,
  openTerminalLink,
  RESIZE_DEBOUNCE_MS,
  safeTerminalLink,
  ShellTerminalClose,
  ShellTerminalSocket,
  shellTerminalUrl,
} from './shellTerminal';

vi.mock('routes/utils', () => ({
  paths: { shellTerminal: (id: string) => `/shells/${id}/terminal` },
  serverAddress: () => 'http://localhost:3000',
}));

class FakeWebSocket {
  binaryType = 'blob';
  readyState: number = WebSocket.CONNECTING;
  sent: (string | Uint8Array)[] = [];
  closedWith?: number;
  onclose: ((e: CloseEvent) => void) | null = null;
  onerror: ((e: Event) => void) | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  onopen: ((e: Event) => void) | null = null;

  constructor(readonly url: string) {}

  send(data: string | Uint8Array) {
    this.sent.push(data);
  }

  close(code?: number) {
    this.closedWith = code;
  }

  open() {
    this.readyState = WebSocket.OPEN;
    this.onopen?.(new Event('open'));
  }

  receive(data: string | ArrayBuffer) {
    this.onmessage?.(new MessageEvent('message', { data }));
  }

  serverClose(code: number) {
    this.readyState = WebSocket.CLOSED;
    this.onclose?.(new CloseEvent('close', { code }));
  }
}

const connect = () => {
  let ws: FakeWebSocket | undefined;
  const handlers = { onClose: vi.fn(), onData: vi.fn(), onReady: vi.fn() };
  const socket = new ShellTerminalSocket('ws://x/ws/shells/s/terminal', handlers, (url) => {
    ws = new FakeWebSocket(url);
    return ws as unknown as WebSocket;
  });
  if (!ws) throw new Error('no socket');
  return { handlers, socket, ws };
};

describe('shellTerminalUrl', () => {
  it('derives the WebSocket URL from the master address', () => {
    expect(shellTerminalUrl('abc', { cols: 120, rows: 40 }, 'en_US.UTF-8')).toBe(
      'ws://localhost:3000/ws/shells/abc/terminal?cols=120&rows=40&lang=en_US.UTF-8',
    );
  });

  it('uses wss for https and keeps a path prefix', () => {
    expect(
      shellTerminalUrl('a/b', { cols: 80, rows: 24 }, undefined, 'https://gpu.lab/det-master'),
    ).toBe('wss://gpu.lab/det-master/ws/shells/a%2Fb/terminal?cols=80&rows=24');
    expect(shellTerminalUrl('id', { cols: 80, rows: 24 }, undefined, 'https://gpu.lab/')).toBe(
      'wss://gpu.lab/ws/shells/id/terminal?cols=80&rows=24',
    );
  });
});

describe('browserLang', () => {
  it('maps browser languages to UTF-8 locales', () => {
    expect(browserLang('en-US')).toBe('en_US.UTF-8');
    expect(browserLang('de')).toBe('de.UTF-8');
    expect(browserLang('zh-CN')).toBe('zh_CN.UTF-8');
    expect(browserLang('zh-Hans-CN')).toBe('zh.UTF-8');
    expect(browserLang('')).toBeUndefined();
    expect(browserLang('x; rm -rf /')).toBeUndefined();
  });
});

describe('ShellTerminalSocket', () => {
  it('passes output and readiness to its handlers', () => {
    const { handlers, ws } = connect();
    expect(ws.binaryType).toBe('arraybuffer');
    ws.open();
    ws.receive(JSON.stringify({ type: 'ready' }));
    expect(handlers.onReady).toHaveBeenCalled();
    ws.receive(new Uint8Array([104, 105]).buffer);
    expect(handlers.onData).toHaveBeenCalledWith(new Uint8Array([104, 105]));
  });

  it('sends input as binary messages of at most 32 KiB', () => {
    const { socket, ws } = connect();
    socket.send('ignored before the socket opens');
    expect(ws.sent).toHaveLength(0);
    ws.open();
    socket.send('ls\r');
    expect(ws.sent[0]).toEqual(new TextEncoder().encode('ls\r'));
    ws.sent = [];
    const paste = new Uint8Array(100 * 1024).fill(97);
    socket.send(paste);
    expect(ws.sent).toHaveLength(4);
    ws.sent.forEach((chunk) => {
      expect(chunk).toBeInstanceOf(Uint8Array);
      expect((chunk as Uint8Array).length).toBeLessThanOrEqual(MAX_INPUT_CHUNK);
    });
    expect(ws.sent.reduce((n, c) => n + (c as Uint8Array).length, 0)).toBe(paste.length);
  });

  it('debounces resizes and sends only the last size', () => {
    vi.useFakeTimers();
    try {
      const { socket, ws } = connect();
      ws.open();
      socket.resize({ cols: 100, rows: 30 });
      socket.resize({ cols: 120, rows: 40 });
      expect(ws.sent).toHaveLength(0);
      vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS);
      expect(ws.sent).toEqual([JSON.stringify({ cols: 120, rows: 40, type: 'resize' })]);
    } finally {
      vi.useRealTimers();
    }
  });

  it('reports how the session ended', () => {
    const { handlers, ws } = connect();
    ws.open();
    ws.receive(JSON.stringify({ type: 'ready' }));
    ws.receive(JSON.stringify({ code: 3, type: 'exit' }));
    ws.serverClose(CloseCode.Normal);
    ws.serverClose(CloseCode.Normal);
    expect(handlers.onClose).toHaveBeenCalledTimes(1);
    expect(handlers.onClose).toHaveBeenCalledWith({
      code: CloseCode.Normal,
      errorCode: undefined,
      exitCode: 3,
      wasOpen: true,
      wasReady: true,
    });
  });

  it('reports refused connections and server errors', () => {
    const refused = connect();
    refused.ws.serverClose(CloseCode.Abnormal);
    expect(refused.handlers.onClose).toHaveBeenCalledWith(
      expect.objectContaining({ code: CloseCode.Abnormal, wasOpen: false, wasReady: false }),
    );

    const expired = connect();
    expired.ws.open();
    expired.ws.receive(JSON.stringify({ code: 'session_expired', type: 'error' }));
    expired.ws.receive('not json');
    expired.ws.serverClose(CloseCode.Unauthenticated);
    expect(expired.handlers.onClose).toHaveBeenCalledWith(
      expect.objectContaining({ code: CloseCode.Unauthenticated, errorCode: 'session_expired' }),
    );
  });
});

describe('describeClose', () => {
  const close = (c: Partial<ShellTerminalClose>): ShellTerminalClose => ({
    code: CloseCode.Normal,
    wasOpen: true,
    wasReady: true,
    ...c,
  });

  it('retries only while the shell is not ready', () => {
    expect(describeClose(close({ code: CloseCode.NotReady })).retry).toBe(true);
    for (const code of Object.values(CloseCode).filter((c) => c !== CloseCode.NotReady)) {
      expect(describeClose(close({ code })).retry).toBe(false);
    }
  });

  it('describes exits, refusals and lost sessions', () => {
    expect(describeClose(close({ exitCode: 0 })).message).toBe('Session ended (exit status 0).');
    expect(describeClose(close({ code: CloseCode.Abnormal, wasOpen: false })).message).toMatch(
      /Cannot open a terminal/,
    );
    expect(describeClose(close({ code: CloseCode.Abnormal })).message).toBe(
      'The connection was lost.',
    );
    expect(describeClose(close({ code: CloseCode.Unauthenticated })).canReconnect).toBe(false);
    expect(describeClose(close({ code: CloseCode.Forbidden })).canReconnect).toBe(false);
    expect(
      describeClose(close({ code: CloseCode.Timeout, errorCode: 'idle_timeout' })).message,
    ).toMatch(/inactivity/);
  });
});

describe('terminal links', () => {
  it('allows only http and https links', () => {
    expect(safeTerminalLink('https://example.org/a?b=c')).toBe('https://example.org/a?b=c');
    expect(safeTerminalLink('http://example.org')).toBe('http://example.org/');
    expect(safeTerminalLink('javascript:alert(1)')).toBeUndefined();
    expect(safeTerminalLink('data:text/html,<script>alert(1)</script>')).toBeUndefined();
    expect(safeTerminalLink('file:///etc/passwd')).toBeUndefined();
    expect(safeTerminalLink('vbscript:x')).toBeUndefined();
    expect(safeTerminalLink('not a url')).toBeUndefined();
  });

  it('opens a link without an opener only after the user confirms the URL', () => {
    const open = vi.fn();
    const decline = vi.fn((message: string) => message.length < 0);
    expect(openTerminalLink('https://example.org/x', decline, open)).toBe(false);
    expect(decline.mock.calls[0][0]).toContain('https://example.org/x');
    expect(open).not.toHaveBeenCalled();

    const accept = vi.fn((message: string) => message.length > 0);
    expect(openTerminalLink('https://example.org/x', accept, open)).toBe(true);
    expect(open).toHaveBeenCalledWith('https://example.org/x', '_blank', 'noopener,noreferrer');

    open.mockClear();
    expect(openTerminalLink('javascript:alert(1)', accept, open)).toBe(false);
    expect(open).not.toHaveBeenCalled();
  });
});
