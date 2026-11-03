package shellterm

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
)

// countingActivity counts the calls of a session's Activity.
type countingActivity struct {
	holds, releases, touches atomic.Int64
}

func (a *countingActivity) Hold() func() {
	a.holds.Add(1)
	return func() { a.releases.Add(1) }
}

func (a *countingActivity) Touch() { a.touches.Add(1) }

// A session holds its task active while it is open, its input and output touch the record, and it
// releases the hold once when it ends.
func TestBridgeActivity(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	sshd := newFakeSSHD(t, signer, pub, behaviorEcho)
	target := Target{Addr: sshd.Addr(), User: "root", Signer: signer, HostKey: pub}
	activity := &countingActivity{}
	opts := fastOptions()
	opts.Activity = activity
	srv := newBridgeServer(context.Background(), t, target, opts)

	ws := srv.dial(t)
	c := readTerm(ws)
	c.waitFor(t, "ready", func(c *termClient) bool { return c.hasControl(msgReady) })
	require.Equal(t, int64(1), activity.holds.Load())
	require.Zero(t, activity.releases.Load())

	before := activity.touches.Load()
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte("hello activity\r")))
	c.waitOutput(t, "hello activity")
	// The input and at least one chunk of its echo.
	require.GreaterOrEqual(t, activity.touches.Load()-before, int64(2))
	require.Zero(t, activity.releases.Load())

	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte("exit 0\r")))
	require.Equal(t, CloseNormal, c.closeCode(t))
	srv.waitStats(t, 5*time.Second)
	require.Equal(t, int64(1), activity.holds.Load())
	require.Equal(t, int64(1), activity.releases.Load())
}

// A session that cannot reach sshd also releases its hold.
func TestBridgeActivityReleasedOnFailure(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	activity := &countingActivity{}
	opts := fastOptions()
	opts.Activity = activity
	target := Target{Addr: "127.0.0.1:1", User: "root", Signer: signer, HostKey: pub}
	srv := newBridgeServer(context.Background(), t, target, opts)

	c := readTerm(srv.dial(t))
	require.Equal(t, CloseUnavailable, c.closeCode(t))
	srv.waitStats(t, 5*time.Second)
	require.Equal(t, int64(1), activity.holds.Load())
	require.Equal(t, int64(1), activity.releases.Load())
}
