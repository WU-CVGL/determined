package shellterm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/determined-ai/determined/master/internal/shellterm/shelltermtest"
	detssh "github.com/determined-ai/determined/master/pkg/ssh"
)

// shellKeys generates a shell key pair the way LaunchShell does.
func shellKeys(t *testing.T, keyType string) (ssh.Signer, ssh.PublicKey, detssh.PrivateAndPublicKeys) {
	t.Helper()
	return shelltermtest.Keys(t, keyType)
}

const (
	behaviorEcho   = shelltermtest.Echo
	behaviorFlood  = shelltermtest.Flood
	behaviorSilent = shelltermtest.Silent
)

func newFakeSSHD(
	t *testing.T, hostKey ssh.Signer, authKey ssh.PublicKey, b shelltermtest.Behavior,
) *shelltermtest.SSHD {
	t.Helper()
	return shelltermtest.NewSSHD(t, shelltermtest.Config{HostKey: hostKey, AuthorizedKey: authKey, Behavior: b})
}

// bridgeServer serves terminal sessions over WebSocket for tests.
type bridgeServer struct {
	url   string
	stats chan Stats
	// cancel ends the session in progress with a cause.
	cancel chan context.CancelCauseFunc
}

func newBridgeServer(
	parent context.Context, t *testing.T, target Target, opts Options,
) *bridgeServer {
	t.Helper()
	b := &bridgeServer{stats: make(chan Stats, 4), cancel: make(chan context.CancelCauseFunc, 4)}
	upgrader := websocket.Upgrader{CheckOrigin: CheckSameOrigin}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancelCause(parent)
		defer cancel(nil)
		b.cancel <- cancel
		b.stats <- Serve(ctx, ws, target, opts, nil)
	}))
	t.Cleanup(srv.Close)
	b.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return b
}

func (b *bridgeServer) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	ws, resp, err := websocket.DefaultDialer.Dial(b.url, nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

func (b *bridgeServer) waitStats(t *testing.T, within time.Duration) Stats {
	t.Helper()
	select {
	case s := <-b.stats:
		return s
	case <-time.After(within):
		t.Fatalf("the session did not end within %s", within)
		return Stats{}
	}
}

// termClient reads frames from a terminal WebSocket in the background.
type termClient struct {
	ws      *websocket.Conn
	mu      sync.Mutex
	output  bytes.Buffer
	control []ControlMessage
	frames  [][]byte
	done    chan struct{}
	err     error
}

func readTerm(ws *websocket.Conn) *termClient {
	c := &termClient{ws: ws, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				c.mu.Lock()
				c.err = err
				c.mu.Unlock()
				return
			}
			c.mu.Lock()
			c.frames = append(c.frames, data)
			switch kind {
			case websocket.BinaryMessage:
				c.output.Write(data)
			case websocket.TextMessage:
				var m ControlMessage
				if json.Unmarshal(data, &m) == nil {
					c.control = append(c.control, m)
				}
			}
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *termClient) waitFor(t *testing.T, what string, cond func(c *termClient) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		ok := cond(c)
		c.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("timed out waiting for %s; output %q, control %+v, err %v",
		what, c.output.String(), c.control, c.err)
}

func (c *termClient) waitOutput(t *testing.T, substr string) {
	t.Helper()
	c.waitFor(t, fmt.Sprintf("output %q", substr), func(c *termClient) bool {
		return strings.Contains(c.output.String(), substr)
	})
}

func (c *termClient) hasControl(typ string) bool {
	for _, m := range c.control {
		if m.Type == typ {
			return true
		}
	}
	return false
}

// closeCode waits for the connection to end and returns its close code, which is 1006
// (websocket.CloseAbnormalClosure) when the connection ended without a close frame.
func (c *termClient) closeCode(t *testing.T) int {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(15 * time.Second):
		t.Fatal("the WebSocket did not close")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var ce *websocket.CloseError
	if errors.As(c.err, &ce) {
		return ce.Code
	}
	return -1
}

func (c *termClient) errorCode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.control {
		if m.Type == msgError {
			code, _ := m.Code.(string)
			return code
		}
	}
	return ""
}

func sendResize(t *testing.T, ws *websocket.Conn, cols, rows int) {
	t.Helper()
	msg := encodeControl(ControlMessage{Type: msgResize, Cols: cols, Rows: rows})
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, msg))
}
