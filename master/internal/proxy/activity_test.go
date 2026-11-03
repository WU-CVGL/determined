package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/model"
)

// recordedActivity is what a proxy's TaskActivity heard of one task.
type recordedActivity struct {
	touches, holds, releases int
}

// recordingActivity records the calls of a proxy's TaskActivity by task.
type recordingActivity struct {
	mu    sync.Mutex
	tasks map[model.TaskID]recordedActivity
}

func (a *recordingActivity) Touch(taskID model.TaskID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.tasks[taskID]
	r.touches++
	a.tasks[taskID] = r
}

func (a *recordingActivity) Hold(taskID model.TaskID) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.tasks[taskID]
	r.holds++
	a.tasks[taskID] = r
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		r := a.tasks[taskID]
		r.releases++
		a.tasks[taskID] = r
	}
}

func (a *recordingActivity) of(taskID model.TaskID) recordedActivity {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tasks[taskID]
}

// newActivityProxy returns a test proxy that records task activity and whose HTTPAuth refuses
// requests with the header "Refuse", as the master refuses requests without a valid login.
func newActivityProxy(t *testing.T) (*Proxy, string, *recordingActivity) {
	var sawAuthCookie string
	p, base := newTestProxy(t, &sawAuthCookie)
	activity := &recordingActivity{tasks: map[model.TaskID]recordedActivity{}}
	p.TaskActivity = activity
	p.HTTPAuth = func(c echo.Context, _ *Service) (bool, error) {
		if c.Request().Header.Get("Refuse") != "" {
			return true, c.NoContent(http.StatusUnauthorized)
		}
		return false, nil
	}
	return p, base, activity
}

// Requests that pass the access check touch the task's record, whichever of the task's services
// they reach; refused ones do not, and services without a task record nothing.
func TestProxyHTTPRequestTouchesTask(t *testing.T) {
	p, base, activity := newActivityProxy(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	p.RegisterService("task", Service{URL: upstreamURL, TaskID: "task"})
	p.RegisterService("task:2900", Service{URL: upstreamURL, TaskID: "task", Exec: true})
	p.RegisterService("task:8080", Service{URL: upstreamURL, TaskID: "task", AllowUnauthenticated: true})
	p.RegisterService("other", Service{URL: upstreamURL})

	get := func(serviceID string, refuse bool) int {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			base+"/proxy/"+serviceID+"/lab", nil)
		require.NoError(t, err)
		if refuse {
			req.Header.Set("Refuse", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp.StatusCode
	}

	for _, id := range []string{"task", "task:2900"} {
		require.Equal(t, http.StatusUnauthorized, get(id, true), id)
	}
	require.Equal(t, recordedActivity{}, activity.of("task"), "refused requests are not activity")

	require.Equal(t, http.StatusOK, get("task", false))
	require.Equal(t, http.StatusOK, get("task:2900", false))
	// A port that needs no master login reaches its service without an access check.
	require.Equal(t, http.StatusOK, get("task:8080", true))
	require.Equal(t, recordedActivity{touches: 3}, activity.of("task"))

	require.Equal(t, http.StatusOK, get("other", false))
	require.Equal(t, map[model.TaskID]recordedActivity{"task": {touches: 3}}, activity.tasks)
}

// A WebSocket connection holds its task active while it is open, without touching the record per
// frame, and releases it once when it closes. A refused upgrade holds nothing.
func TestProxyWebSocketHoldsTask(t *testing.T) {
	p, base, activity := newActivityProxy(t)
	ln, conns := listenUpstream(t)
	p.RegisterService("task:2900", Service{
		URL: &url.URL{Scheme: "http", Host: ln.Addr().String()}, TaskID: "task",
	})
	target := "ws" + strings.TrimPrefix(base, "http") + "/proxy/task:2900/"
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}

	_, resp, err := dialer.Dial(target, http.Header{"Refuse": {"1"}})
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, recordedActivity{}, activity.of("task"))

	// The service answers the upgrade, then echoes what it reads until it hangs up.
	hangUp := make(chan struct{})
	served := make(chan struct{})
	go func() {
		defer close(served)
		var conn net.Conn
		select {
		case conn = <-conns:
		case <-time.After(5 * time.Second):
			return
		}
		defer func() { _ = conn.Close() }()
		go func() {
			<-hangUp
			_ = conn.Close()
		}()
		r := bufio.NewReader(conn)
		if _, err := http.ReadRequest(r); err != nil {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n"))
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if err != nil {
				return
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return
			}
		}
	}()

	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	require.NoError(t, err)
	_, err = conn.Write([]byte("GET /proxy/task:2900/ HTTP/1.1\r\n" +
		"Host: master\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"))
	require.NoError(t, err)
	r := bufio.NewReader(conn)
	resp, err = http.ReadResponse(r, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	for i := 0; i < 3; i++ {
		_, err = conn.Write([]byte("frame"))
		require.NoError(t, err)
		got := make([]byte, 5)
		_, err = r.Read(got)
		require.NoError(t, err)
		require.Equal(t, "frame", string(got))
	}
	require.Equal(t, recordedActivity{holds: 1}, activity.of("task"))

	// The client goes away and the service hangs up.
	require.NoError(t, conn.Close())
	close(hangUp)
	require.Eventually(t, func() bool {
		return activity.of("task") == recordedActivity{holds: 1, releases: 1}
	}, 5*time.Second, 10*time.Millisecond)
	<-served
}

// A client that goes away without a close handshake ends the connection to a service that stays
// silent, and releases the task.
func TestProxyWebSocketClientGoneReleasesTask(t *testing.T) {
	p, base, activity := newActivityProxy(t)
	ln, conns := listenUpstream(t)
	p.RegisterService("task:2900", Service{
		URL: &url.URL{Scheme: "http", Host: ln.Addr().String()}, TaskID: "task",
	})

	// The service answers the upgrade, then only reads.
	upstreamEnd := make(chan error, 1)
	go func() {
		var conn net.Conn
		select {
		case conn = <-conns:
		case <-time.After(5 * time.Second):
			upstreamEnd <- errors.New("the proxy never connected to the service")
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		if _, err := http.ReadRequest(r); err != nil {
			upstreamEnd <- err
			return
		}
		if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")); err != nil {
			upstreamEnd <- err
			return
		}
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			upstreamEnd <- err
			return
		}
		_, err := r.Read(make([]byte, 1))
		upstreamEnd <- err
	}()

	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	require.NoError(t, err)
	_, err = conn.Write([]byte("GET /proxy/task:2900/ HTTP/1.1\r\n" +
		"Host: master\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"))
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, recordedActivity{holds: 1}, activity.of("task"))

	require.NoError(t, conn.Close())
	require.Equal(t, io.EOF, <-upstreamEnd, "the service's connection ends")
	require.Eventually(t, func() bool {
		return activity.of("task") == recordedActivity{holds: 1, releases: 1}
	}, 5*time.Second, 10*time.Millisecond)
}

// tcpProxyClient opens a proxied TCP connection, as an SSH client of `det shell open` does, to a
// service of the task "task", and checks that the service's first bytes reach it.
func tcpProxyClient(t *testing.T) (*websocket.Conn, net.Conn, *recordingActivity) {
	p, base, activity := newActivityProxy(t)
	ln, conns := listenUpstream(t)
	p.RegisterService("task", Service{
		URL: &url.URL{Scheme: "http", Host: ln.Addr().String()}, ProxyTCP: true,
		AllowUnauthenticated: true, TaskID: "task",
	})

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, resp, err := dialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/proxy/task/", nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	t.Cleanup(func() { _ = ws.Close() })
	upstream := acceptUpstream(t, conns)
	_, err = upstream.Write([]byte("SSH-2.0-test\r\n"))
	require.NoError(t, err)
	_, data, err := ws.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, "SSH-2.0-test\r\n", string(data))
	return ws, upstream, activity
}

// An accepted TCP connection, such as an SSH client's, holds its task's record while it is open.
// A client that goes away without closing its WebSocket ends the connection to the service and
// releases the task.
func TestProxyTCPConnectionHoldsTask(t *testing.T) {
	ws, upstream, activity := tcpProxyClient(t)
	require.Equal(t, recordedActivity{holds: 1}, activity.of("task"))

	require.NoError(t, ws.Close())
	require.NoError(t, upstream.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err := upstream.Read(make([]byte, 1))
	require.Equal(t, io.EOF, err, "the service's connection ends")
	require.Eventually(t, func() bool {
		return activity.of("task") == recordedActivity{holds: 1, releases: 1}
	}, 5*time.Second, 10*time.Millisecond)
}

// A service that hangs up, as an SSH server does at the end of a session, ends the client's
// connection and releases the task.
func TestProxyTCPServiceHangUpReleasesTask(t *testing.T) {
	ws, upstream, activity := tcpProxyClient(t)
	require.Equal(t, recordedActivity{holds: 1}, activity.of("task"))

	require.NoError(t, upstream.Close())
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, _, err := ws.ReadMessage()
	require.Error(t, err, "the client's connection ends")
	require.Eventually(t, func() bool {
		return activity.of("task") == recordedActivity{holds: 1, releases: 1}
	}, 5*time.Second, 10*time.Millisecond)
}
