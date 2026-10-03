package shellterm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/shellterm/shelltermtest"
)

// fastOptions shortens every timer so that timeout tests run quickly.
func fastOptions() Options {
	return Options{
		Cols:              100,
		Rows:              30,
		Lang:              "en_US.UTF-8",
		SetupTimeout:      2 * time.Second,
		PingInterval:      100 * time.Millisecond,
		PongWait:          500 * time.Millisecond,
		WriteWait:         500 * time.Millisecond,
		KeepaliveInterval: 100 * time.Millisecond,
		KeepaliveTimeout:  300 * time.Millisecond,
	}
}

func TestBridgeSession(t *testing.T) {
	for _, keyType := range []string{config.KeyTypeED25519, config.KeyTypeRSA, config.KeyTypeECDSA} {
		t.Run(keyType, func(t *testing.T) {
			signer, pub, keys := shellKeys(t, keyType)
			sshd := newFakeSSHD(t, signer, pub, behaviorEcho)
			target := Target{Addr: sshd.Addr(), User: "det-user", Signer: signer, HostKey: pub}
			srv := newBridgeServer(context.Background(), t, target, fastOptions())

			ws := srv.dial(t)
			c := readTerm(ws)
			c.waitFor(t, "ready", func(c *termClient) bool { return c.hasControl(msgReady) })

			require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte("hello terminal\r")))
			c.waitOutput(t, "hello terminal")

			waitResizes := func(n int) {
				require.Eventually(t, func() bool {
					return len(sshd.Record().Resizes) == n
				}, 5*time.Second, 10*time.Millisecond)
			}
			sendResize(t, ws, 120, 40)
			waitResizes(1)
			sendResize(t, ws, 5000, -1) // Clamped to 1000 columns and the default rows.
			waitResizes(2)

			require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte("exit 3\r")))
			require.Equal(t, CloseNormal, c.closeCode(t))
			stats := srv.waitStats(t, 5*time.Second)

			c.mu.Lock()
			var exit *ControlMessage
			for i := range c.control {
				if c.control[i].Type == msgExit {
					exit = &c.control[i]
				}
			}
			require.NotNil(t, exit, "no exit message")
			require.InDelta(t, 3, exit.Code, 0)
			require.Contains(t, c.output.String(), "logout")
			// Neither the key nor any part of it ever reaches the browser.
			pemBody := strings.Join(strings.Split(string(keys.PrivateKey), "\n")[1:3], "")
			raw, _ := base64.StdEncoding.DecodeString(pemBody)
			for _, f := range c.frames {
				require.NotContains(t, string(f), "PRIVATE KEY")
				require.NotContains(t, string(f), pemBody)
				if len(raw) > 16 {
					require.False(t, bytes.Contains(f, raw[:16]))
				}
			}
			c.mu.Unlock()

			require.True(t, stats.Started)
			require.NotNil(t, stats.ExitCode)
			require.Equal(t, 3, *stats.ExitCode)
			require.Equal(t, CloseNormal, stats.CloseCode)
			require.Equal(t, int64(len("hello terminal\rexit 3\r")), stats.BytesIn)
			require.Positive(t, stats.BytesOut)

			rec := sshd.Record()
			users, ptys, resizes, envs, shells := rec.Users, rec.PTYs, rec.Resizes, rec.Env, rec.Shells
			require.Equal(t, []string{"det-user"}, users)
			require.Len(t, ptys, 1)
			require.Equal(t, "xterm-256color", ptys[0].Term)
			require.Equal(t, uint32(100), ptys[0].Columns)
			require.Equal(t, uint32(30), ptys[0].Rows)
			require.Len(t, resizes, 2)
			require.Equal(t, [2]uint32{120, 40}, [2]uint32{resizes[0].Columns, resizes[0].Rows})
			require.Equal(t, [2]uint32{1000, 24}, [2]uint32{resizes[1].Columns, resizes[1].Rows})
			require.Equal(t, map[string]string{"LANG": "en_US.UTF-8"}, envs)
			require.Equal(t, 1, shells)
		})
	}
}

func TestBridgeWrongHostKey(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	otherHost, _, _ := shellKeys(t, config.KeyTypeED25519)
	// The server at the shell's address presents a host key other than the shell's.
	sshd := newFakeSSHD(t, otherHost, pub, behaviorEcho)
	target := Target{Addr: sshd.Addr(), User: "root", Signer: signer, HostKey: pub}
	srv := newBridgeServer(context.Background(), t, target, fastOptions())

	c := readTerm(srv.dial(t))
	require.Equal(t, CloseUnavailable, c.closeCode(t))
	require.Equal(t, ErrCodeUnavailable, c.errorCode())
	stats := srv.waitStats(t, 5*time.Second)
	require.False(t, stats.Started)
	require.Contains(t, stats.Reason, "host key mismatch")

	c.mu.Lock()
	defer c.mu.Unlock()
	require.Zero(t, c.output.Len(), "terminal output after a failed handshake")
	require.False(t, c.hasControl(msgReady))
	for _, m := range c.control {
		// The browser learns nothing about the address or the failure.
		require.NotContains(t, m.Message, sshd.Addr())
		require.NotContains(t, m.Message, "host key")
	}
	require.Empty(t, sshd.Record().PTYs)
	require.Zero(t, sshd.Record().Shells)
}

func TestBridgeRSAHostKeyNeedsSHA2(t *testing.T) {
	_, rsaPub, _ := shellKeys(t, config.KeyTypeRSA)
	require.Equal(t, []string{"rsa-sha2-512", "rsa-sha2-256"}, HostKeyAlgorithms(rsaPub))
	_, edPub, _ := shellKeys(t, config.KeyTypeED25519)
	require.Equal(t, []string{"ssh-ed25519"}, HostKeyAlgorithms(edPub))
	_, ecPub, _ := shellKeys(t, config.KeyTypeECDSA)
	require.Equal(t, []string{ecPub.Type()}, HostKeyAlgorithms(ecPub))
}

func TestBridgeHandshakeTimeout(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	// A server that accepts TCP connections and never sends an SSH banner.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	peerClosed := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Read(make([]byte, 1<<16)) // The client's banner.
		buf := make([]byte, 1024)
		for {
			if _, err := conn.Read(buf); err != nil {
				close(peerClosed)
				return
			}
		}
	}()

	opts := fastOptions()
	opts.SetupTimeout = 300 * time.Millisecond
	target := Target{Addr: ln.Addr().String(), User: "root", Signer: signer, HostKey: pub}
	srv := newBridgeServer(context.Background(), t, target, opts)

	start := time.Now()
	c := readTerm(srv.dial(t))
	require.Equal(t, CloseUnavailable, c.closeCode(t))
	require.Less(t, time.Since(start), 3*time.Second)
	stats := srv.waitStats(t, 5*time.Second)
	require.False(t, stats.Started)
	select {
	case <-peerClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("the master did not close the stalled SSH connection")
	}
}

func TestBridgeClosedTabDuringSetup(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			defer func() { _ = conn.Close() }()
			_, _ = conn.Read(make([]byte, 1<<16))
			time.Sleep(time.Minute)
		}
	}()

	opts := fastOptions()
	opts.SetupTimeout = time.Minute
	target := Target{Addr: ln.Addr().String(), User: "root", Signer: signer, HostKey: pub}
	srv := newBridgeServer(context.Background(), t, target, opts)
	ws := srv.dial(t)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, ws.Close())
	// The session ends although the SSH setup alone would wait a minute.
	stats := srv.waitStats(t, 5*time.Second)
	require.False(t, stats.Started)
}

func TestBridgeClientNeverReads(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	sshd := newFakeSSHD(t, signer, pub, behaviorFlood)
	opts := fastOptions()
	opts.PongWait = time.Minute // Only the write deadline can end this session.
	opts.PingInterval = time.Minute
	target := Target{Addr: sshd.Addr(), User: "root", Signer: signer, HostKey: pub}
	srv := newBridgeServer(context.Background(), t, target, opts)

	ws := srv.dial(t)
	_ = ws // Never read.
	start := time.Now()
	stats := srv.waitStats(t, 20*time.Second)
	require.True(t, stats.Started)
	require.Zero(t, stats.CloseCode, "a close frame cannot reach a client that does not read")
	require.Contains(t, stats.Reason, "writing to the browser")
	require.Less(t, time.Since(start), 15*time.Second)
	select {
	case <-sshd.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the SSH session stayed open")
	}
}

func TestBridgeUnsolicitedPongs(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	sshd := newFakeSSHD(t, signer, pub, behaviorSilent)
	target := Target{Addr: sshd.Addr(), User: "root", Signer: signer, HostKey: pub}
	srv := newBridgeServer(context.Background(), t, target, fastOptions())

	ws := srv.dial(t)
	// The client never answers pings, but sends a stream of pongs of its own.
	ws.SetPingHandler(func(string) error { return nil })
	c := readTerm(ws)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		var seq uint64
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			payload := make([]byte, 8)
			if seq++; seq%2 == 0 {
				_, _ = rand.Read(payload)
			} else {
				payload[7] = byte(seq) // Small counters, as if guessing sequential nonces.
			}
			if ws.WriteControl(websocket.PongMessage, payload, time.Now().Add(time.Second)) != nil {
				return
			}
		}
	}()

	start := time.Now()
	stats := srv.waitStats(t, 10*time.Second)
	// PongWait is 500ms; the session must not outlive it by much.
	require.Less(t, time.Since(start), 3*time.Second)
	require.True(t, stats.Started)
	require.Contains(t, stats.Reason, "browser connection ended")
	require.Equal(t, websocket.CloseAbnormalClosure, c.closeCode(t))
	select {
	case <-sshd.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the SSH session stayed open")
	}
}

func TestBridgePongsKeepSessionAlive(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	sshd := newFakeSSHD(t, signer, pub, behaviorSilent)
	target := Target{Addr: sshd.Addr(), User: "root", Signer: signer, HostKey: pub}
	srv := newBridgeServer(context.Background(), t, target, fastOptions())

	// gorilla clients answer pings by default, as browsers do.
	c := readTerm(srv.dial(t))
	c.waitFor(t, "ready", func(c *termClient) bool { return c.hasControl(msgReady) })
	select {
	case s := <-srv.stats:
		t.Fatalf("an answering client was disconnected: %+v", s)
	case <-time.After(2 * time.Second): // Four times PongWait.
	}
}

func TestBridgeIdleTimeout(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	sshd := newFakeSSHD(t, signer, pub, behaviorSilent)
	opts := fastOptions()
	opts.IdleTimeout = 400 * time.Millisecond
	target := Target{Addr: sshd.Addr(), User: "root", Signer: signer, HostKey: pub}
	srv := newBridgeServer(context.Background(), t, target, opts)

	c := readTerm(srv.dial(t))
	require.Equal(t, CloseTimeout, c.closeCode(t))
	require.Equal(t, ErrCodeIdle, c.errorCode())
	stats := srv.waitStats(t, 5*time.Second)
	require.Equal(t, CloseTimeout, stats.CloseCode)
}

func TestBridgeMaxSessionLength(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	sshd := newFakeSSHD(t, signer, pub, behaviorEcho)
	opts := fastOptions()
	opts.IdleTimeout = time.Hour
	opts.Deadline = time.Now().Add(time.Second)
	target := Target{Addr: sshd.Addr(), User: "root", Signer: signer, HostKey: pub}
	srv := newBridgeServer(context.Background(), t, target, opts)

	ws := srv.dial(t)
	c := readTerm(ws)
	go func() {
		for i := 0; i < 100; i++ {
			if ws.WriteMessage(websocket.BinaryMessage, []byte("busy\r")) != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	require.Equal(t, CloseTimeout, c.closeCode(t))
	require.Equal(t, ErrCodeTimeLimit, c.errorCode())
}

func TestBridgeKeepaliveTimeout(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	// sshd stops answering, as if its node had died.
	sshd := shelltermtest.NewSSHD(t, shelltermtest.Config{
		HostKey: signer, AuthorizedKey: pub, Behavior: behaviorSilent, IgnoreGlobalRequests: true,
	})
	target := Target{Addr: sshd.Addr(), User: "root", Signer: signer, HostKey: pub}
	srv := newBridgeServer(context.Background(), t, target, fastOptions())

	c := readTerm(srv.dial(t))
	require.Equal(t, CloseUnavailable, c.closeCode(t))
	stats := srv.waitStats(t, 5*time.Second)
	require.True(t, stats.Started)
	require.Contains(t, stats.Reason, "keepalive")
}

func TestBridgeCloseCause(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	sshd := newFakeSSHD(t, signer, pub, behaviorSilent)
	target := Target{Addr: sshd.Addr(), User: "root", Signer: signer, HostKey: pub}

	t.Run("cause", func(t *testing.T) {
		srv := newBridgeServer(context.Background(), t, target, fastOptions())
		c := readTerm(srv.dial(t))
		c.waitFor(t, "ready", func(c *termClient) bool { return c.hasControl(msgReady) })
		cancel := <-srv.cancel
		cancel(NewCloseError(CloseUnauthenticated, ErrCodeUnauthenticate, "logged out"))
		require.Equal(t, CloseUnauthenticated, c.closeCode(t))
		require.Equal(t, ErrCodeUnauthenticate, c.errorCode())
		stats := srv.waitStats(t, 5*time.Second)
		require.Equal(t, CloseUnauthenticated, stats.CloseCode)
		require.Contains(t, stats.Reason, "logged out")
	})

	t.Run("shutdown", func(t *testing.T) {
		parent, shutdown := context.WithCancel(context.Background())
		srv := newBridgeServer(parent, t, target, fastOptions())
		c := readTerm(srv.dial(t))
		c.waitFor(t, "ready", func(c *termClient) bool { return c.hasControl(msgReady) })
		shutdown()
		require.Equal(t, CloseGoingAway, c.closeCode(t))
		require.Equal(t, ErrCodeShutdown, c.errorCode())
	})
}

func TestBridgeOversizedFrame(t *testing.T) {
	signer, pub, _ := shellKeys(t, config.KeyTypeED25519)
	sshd := newFakeSSHD(t, signer, pub, behaviorEcho)
	target := Target{Addr: sshd.Addr(), User: "root", Signer: signer, HostKey: pub}
	srv := newBridgeServer(context.Background(), t, target, fastOptions())

	ws := srv.dial(t)
	c := readTerm(ws)
	c.waitFor(t, "ready", func(c *termClient) bool { return c.hasControl(msgReady) })
	// The master refuses the frame from its header, so the write itself may fail.
	_ = ws.WriteMessage(websocket.BinaryMessage, make([]byte, 2*ReadLimit))
	code := c.closeCode(t)
	require.Contains(t, []int{websocket.CloseMessageTooBig, websocket.CloseAbnormalClosure}, code)
	stats := srv.waitStats(t, 5*time.Second)
	require.Zero(t, stats.BytesIn)
	require.Contains(t, stats.Reason, "read limit")
}
