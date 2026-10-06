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
	require.Equal(t, "readfrom tcp 10.0.0.1:8080->10.0.0.2:51432: websocket: close 1000 (normal): goodbye",
		normal.Error())
	brokenPipe := opError("writeto", opError("write", os.NewSyscallError("write", syscall.EPIPE)))
	require.Equal(t, "writeto tcp 10.0.0.1:8080->10.0.0.2:51432: write tcp 10.0.0.1:8080->10.0.0.2:51432: "+
		"write: broken pipe", brokenPipe.Error())

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

// requireOnlyDebugCopyError waits for the proxy to log the end of the connection to addr, and
// requires that it logged it at debug level, with a message that contains want.
func requireOnlyDebugCopyError(t *testing.T, hook *logrustest.Hook, addr, want string) {
	require.Eventually(t, func() bool {
		return len(copyErrorEntries(hook, addr)) > 0
	}, 5*time.Second, 10*time.Millisecond, "the proxy logged no copy error")
	// The proxy logs the request side first; give it a moment to log the response side too.
	time.Sleep(100 * time.Millisecond)
	entries := copyErrorEntries(hook, addr)
	found := false
	for _, e := range entries {
		require.Equal(t, logrus.DebugLevel, e.Level, e.Message)
		found = found || strings.Contains(e.Message, want)
	}
	require.True(t, found, "no entry contains %q: %v", want, entries)
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

	requireOnlyDebugCopyError(t, hook, addr, "websocket: close 1000 (normal): goodbye")
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

	requireOnlyDebugCopyError(t, hook, addr, "websocket: close sent")
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
	requireOnlyDebugCopyError(t, hook, addr, "connection reset by peer")
	require.NoError(t, upstream.Close())
}
