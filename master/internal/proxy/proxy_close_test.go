package proxy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
)

// opError wraps err as the net package does for an operation on a TCP connection.
func opError(op string, err error) *net.OpError {
	return &net.OpError{
		Op:     op,
		Net:    "tcp",
		Source: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 8080},
		Addr:   &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 51432},
		Err:    err,
	}
}

func TestIsConnectionClosed(t *testing.T) {
	// io.Copy wraps what the WebSocket returns in the *net.OpError of the TCP side: "readfrom"
	// when it copies into a TCP connection, "writeto" when it copies out of one.
	normal := opError("readfrom", &websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "goodbye"})
	brokenPipe := opError("writeto", opError("write", os.NewSyscallError("write", syscall.EPIPE)))

	cases := []struct {
		name   string
		err    error
		closed bool
	}{
		{"websocket close 1000", normal, true},
		{"websocket close 1001", opError("readfrom", &websocket.CloseError{Code: websocket.CloseGoingAway}), true},
		{"websocket close 1005", opError("readfrom", &websocket.CloseError{Code: websocket.CloseNoStatusReceived}), true},
		{
			"websocket close 1006",
			opError("readfrom", &websocket.CloseError{
				Code: websocket.CloseAbnormalClosure, Text: io.ErrUnexpectedEOF.Error(),
			}),
			true,
		},
		{"unwrapped websocket close 1000", &websocket.CloseError{Code: websocket.CloseNormalClosure}, true},
		{"broken pipe writing to the websocket", brokenPipe, true},
		{
			"broken pipe writing to the service",
			opError("readfrom", opError("write", os.NewSyscallError("write", syscall.EPIPE))),
			true,
		},
		{
			"connection reset",
			opError("writeto", opError("readfrom", os.NewSyscallError("splice", syscall.ECONNRESET))),
			true,
		},
		{"connection reset on read", opError("read", os.NewSyscallError("read", syscall.ECONNRESET)), true},
		{"closed connection", opError("read", net.ErrClosed), true},
		{"write after the client's close", opError("writeto", websocket.ErrCloseSent), true},
		{"unwrapped write after the client's close", websocket.ErrCloseSent, true},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"wrapped unexpected EOF", fmt.Errorf("reading: %w", io.ErrUnexpectedEOF), true},

		{"no error", nil, false},
		{
			"websocket close 1011",
			opError("readfrom", &websocket.CloseError{Code: websocket.CloseInternalServerErr}),
			false,
		},
		{"websocket close 1002", &websocket.CloseError{Code: websocket.CloseProtocolError}, false},
		// gorilla/websocket returns a protocol error that it found as a plain error.
		{"websocket protocol error", opError("readfrom", errors.New("websocket: bad MASK")), false},
		{"TLS error", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, false},
		{"timeout", opError("read", os.ErrDeadlineExceeded), false},
		{"connection refused", opError("dial", os.NewSyscallError("connect", syscall.ECONNREFUSED)), false},
		{"other error", errors.New("proxy failure"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.closed, isConnectionClosed(tc.err))
		})
	}
}

// captureLogs records what is logged through logrus, at every level, until the test ends.
func captureLogs(t *testing.T) *logrustest.Hook {
	std := logrus.StandardLogger()
	level := std.GetLevel()
	hooks := std.ReplaceHooks(make(logrus.LevelHooks))
	t.Cleanup(func() {
		std.ReplaceHooks(hooks)
		std.SetLevel(level)
	})
	std.SetLevel(logrus.DebugLevel)
	return logrustest.NewLocal(std)
}

// copyErrorEntries returns the entries that report an error from copying the connection to addr.
func copyErrorEntries(hook *logrustest.Hook, addr string) []logrus.Entry {
	var found []logrus.Entry
	for _, e := range hook.AllEntries() {
		if strings.HasPrefix(e.Message, "error copying") && strings.Contains(e.Message, addr) {
			found = append(found, *e)
		}
	}
	return found
}

// requireCopyErrors waits until the proxy has logged, for the connection to addr, a copy error
// that contains each key of want, then requires that each was logged at the level in want and that
// no other copy error was logged.
func requireCopyErrors(t *testing.T, hook *logrustest.Hook, addr string, want map[string]logrus.Level) {
	t.Helper()
	matches := func(e logrus.Entry) (string, bool) {
		for msg := range want {
			if strings.Contains(e.Message, msg) {
				return msg, true
			}
		}
		return "", false
	}
	require.Eventually(t, func() bool {
		seen := map[string]bool{}
		for _, e := range copyErrorEntries(hook, addr) {
			if msg, ok := matches(e); ok {
				seen[msg] = true
			}
		}
		return len(seen) == len(want)
	}, 5*time.Second, 10*time.Millisecond, "the proxy did not log each of %v", want)
	for _, e := range copyErrorEntries(hook, addr) {
		msg, ok := matches(e)
		require.True(t, ok, "unexpected copy error: %s", e.Message)
		require.Equal(t, want[msg].String(), e.Level.String(), e.Message)
	}
}

func listenUpstream(t *testing.T) (net.Listener, <-chan net.Conn) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conns <- conn
		}
	}()
	return ln, conns
}

func acceptUpstream(t *testing.T, conns <-chan net.Conn) net.Conn {
	select {
	case conn := <-conns:
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the proxy never connected to the service")
		return nil
	}
}

// An SSH client of `det shell open` closes its WebSocket with close code 1000 when it disconnects.
func TestTCPProxyLogsNormalCloseAtDebug(t *testing.T) {
	hook := captureLogs(t)
	var sawAuthCookie string
	p, base := newTestProxy(t, &sawAuthCookie)
	ln, conns := listenUpstream(t)
	addr := ln.Addr().String()
	p.Register("svc", &url.URL{Scheme: "http", Host: addr}, true, true)

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, resp, err := dialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/proxy/svc/", nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	defer func() { _ = ws.Close() }()
	upstream := acceptUpstream(t, conns)

	_, err = upstream.Write([]byte("hello"))
	require.NoError(t, err)
	typ, data, err := ws.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.BinaryMessage, typ)
	require.Equal(t, "hello", string(data))
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte("bye")))
	data = make([]byte, 3)
	_, err = io.ReadFull(upstream, data)
	require.NoError(t, err)
	require.Equal(t, "bye", string(data))

	// The client disconnects, then the service hangs up.
	require.NoError(t, ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "goodbye"),
		time.Now().Add(5*time.Second)))
	_, _, err = ws.ReadMessage()
	require.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure), err)
	require.NoError(t, upstream.Close())

	// The service's hang-up ends the other direction without an error.
	requireCopyErrors(t, hook, addr, map[string]logrus.Level{
		"websocket: close 1000 (normal): goodbye": logrus.DebugLevel,
	})
}

// The service can still write after the client closed its WebSocket and the proxy replied to the
// close, as sshd does with trailing output; the proxy then cannot send it.
func TestTCPProxyLogsWriteAfterCloseAtDebug(t *testing.T) {
	hook := captureLogs(t)
	var sawAuthCookie string
	p, base := newTestProxy(t, &sawAuthCookie)
	ln, conns := listenUpstream(t)
	addr := ln.Addr().String()
	p.Register("svc", &url.URL{Scheme: "http", Host: addr}, true, true)

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, resp, err := dialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/proxy/svc/", nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	defer func() { _ = ws.Close() }()
	upstream := acceptUpstream(t, conns)

	// The client disconnects and the proxy replies to its close, then the service writes and
	// hangs up.
	require.NoError(t, ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "goodbye"),
		time.Now().Add(5*time.Second)))
	_, _, err = ws.ReadMessage()
	require.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure), err)
	_, err = upstream.Write([]byte("late data"))
	require.NoError(t, err)
	require.NoError(t, upstream.Close())

	requireCopyErrors(t, hook, addr, map[string]logrus.Level{
		"websocket: close 1000 (normal): goodbye": logrus.DebugLevel,
		"websocket: close sent":                   logrus.DebugLevel,
	})
}

// A client that breaks the WebSocket protocol, here with an unmasked frame, makes gorilla/websocket
// send a close 1002 itself; the service's next write then fails with websocket.ErrCloseSent. The
// protocol error stays an error, the secondary ErrCloseSent is logged at debug level.
func TestTCPProxyLogsProtocolErrorAsError(t *testing.T) {
	hook := captureLogs(t)
	var sawAuthCookie string
	p, base := newTestProxy(t, &sawAuthCookie)
	ln, conns := listenUpstream(t)
	addr := ln.Addr().String()
	p.Register("svc", &url.URL{Scheme: "http", Host: addr}, true, true)

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, resp, err := dialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/proxy/svc/", nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	defer func() { _ = ws.Close() }()
	upstream := acceptUpstream(t, conns)

	// A final binary frame with one byte of payload and no mask, which a client must set.
	_, err = ws.UnderlyingConn().Write([]byte{0x82, 0x01, 'x'})
	require.NoError(t, err)
	_, _, err = ws.ReadMessage()
	require.True(t, websocket.IsCloseError(err, websocket.CloseProtocolError), err)
	_, err = upstream.Write([]byte("late data"))
	require.NoError(t, err)
	require.NoError(t, upstream.Close())

	requireCopyErrors(t, hook, addr, map[string]logrus.Level{
		"websocket: bad MASK":   logrus.ErrorLevel,
		"websocket: close sent": logrus.DebugLevel,
	})
}

// A browser that goes away can reset its connection to a proxied JupyterLab or TensorBoard.
func TestWebSocketProxyLogsResetAtDebug(t *testing.T) {
	hook := captureLogs(t)
	var sawAuthCookie string
	p, base := newTestProxy(t, &sawAuthCookie)
	ln, conns := listenUpstream(t)
	addr := ln.Addr().String()
	p.Register("svc", &url.URL{Scheme: "http", Host: addr}, false, true)

	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte("GET /proxy/svc/api/kernels/1/channels HTTP/1.1\r\n" +
		"Host: master\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"))
	require.NoError(t, err)

	upstream := acceptUpstream(t, conns)
	_, err = http.ReadRequest(bufio.NewReader(upstream))
	require.NoError(t, err)
	_, err = upstream.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	// The browser resets the connection, then the service hangs up.
	tcpConn, ok := conn.(*net.TCPConn)
	require.True(t, ok)
	require.NoError(t, tcpConn.SetLinger(0))
	require.NoError(t, conn.Close())
	requireCopyErrors(t, hook, addr, map[string]logrus.Level{
		"connection reset by peer": logrus.DebugLevel,
	})
	require.NoError(t, upstream.Close())
}
