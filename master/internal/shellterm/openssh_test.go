package shellterm

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
)

// startOpenSSH runs the system's OpenSSH sshd as the current user, with the sshd_config that the
// master installs in shell containers. Only the paths, the port and StrictModes (the test files
// live under /tmp) are changed.
func startOpenSSH(t *testing.T, hostKeyPEM, authorizedKey []byte) (addr string) {
	t.Helper()
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Skip("OpenSSH sshd is not installed")
	}
	sshd, err = filepath.Abs(sshd)
	require.NoError(t, err)

	dir := t.TempDir()
	hostKey := filepath.Join(dir, "id_shell")
	require.NoError(t, os.WriteFile(hostKey, hostKeyPEM, 0o600))
	// Like shell-entrypoint.sh, inject environment variables through authorized_keys options.
	// HOME and ZDOTDIR keep the user's own shell startup files out of the test.
	opts := fmt.Sprintf(`environment="DET_MARKER=det-web-terminal",environment="HOME=%[1]s",`+
		`environment="ZDOTDIR=%[1]s"`, dir)
	for _, rc := range []string{".zshrc", ".bash_profile", ".profile"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, rc), []byte("PS1='det$ '\nPROMPT='det$ '\n"), 0o600))
	}
	authKeys := filepath.Join(dir, "authorized_keys")
	require.NoError(t, os.WriteFile(authKeys,
		[]byte(opts+" "+strings.TrimSpace(string(authorizedKey))+"\n"), 0o600))

	shipped, err := os.ReadFile("../../static/srv/sshd_config")
	require.NoError(t, err)
	require.Regexp(t, `(?m)^ClientAliveInterval 30$`, string(shipped))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	cfg := string(shipped)
	for pattern, repl := range map[string]string{
		`(?m)^Port .*$`:                fmt.Sprintf("Port %d\nListenAddress 127.0.0.1\nPidFile none", port),
		`(?m)^HostKey .*$`:             "HostKey " + hostKey,
		`(?m)^AuthorizedKeysFile.*$`:   "AuthorizedKeysFile " + authKeys,
		`(?m)^StrictModes .*$`:         "StrictModes no",
		`(?m)^SyslogFacility .*$`:      "",
		`(?m)^Subsystem .*$`:           "",
		`(?m)^PermitRootLogin .*$`:     "PermitRootLogin prohibit-password",
		`(?m)^#?LogLevel .*$`:          "LogLevel VERBOSE",
		`(?m)^PermitUserEnvironment.*`: "PermitUserEnvironment yes",
	} {
		re := regexp.MustCompile(pattern)
		require.Regexp(t, re, cfg)
		cfg = re.ReplaceAllString(cfg, repl)
	}
	cfgPath := filepath.Join(dir, "sshd_config")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o600))

	cmd := exec.Command(sshd, "-D", "-e", "-f", cfgPath) //nolint:gosec
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Wait for the line that a shell's readiness check waits for, too.
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stderr)
		signaled := false
		for scanner.Scan() {
			line := scanner.Text()
			t.Logf("sshd: %s", line)
			if !signaled && strings.Contains(line, "Server listening on") {
				close(ready)
				signaled = true
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("sshd did not start")
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

func TestBridgeOpenSSH(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a real sshd")
	}
	cur, err := user.Current()
	require.NoError(t, err)

	for _, keyType := range []string{config.KeyTypeED25519, config.KeyTypeRSA, config.KeyTypeECDSA} {
		t.Run(keyType, func(t *testing.T) {
			signer, pub, keys := shellKeys(t, keyType)
			// The shell's key pair is both sshd's host key and the authorized key.
			addr := startOpenSSH(t, keys.PrivateKey, keys.PublicKey)

			opts := fastOptions()
			opts.SetupTimeout = 20 * time.Second
			opts.PongWait = 30 * time.Second
			opts.KeepaliveInterval = 200 * time.Millisecond
			opts.KeepaliveTimeout = 5 * time.Second
			target := Target{Addr: addr, User: cur.Username, Signer: signer, HostKey: pub}
			srv := newBridgeServer(context.Background(), t, target, opts)

			ws := srv.dial(t)
			c := readTerm(ws)
			c.waitFor(t, "ready", func(c *termClient) bool { return c.hasControl(msgReady) })

			send := func(s string) {
				require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte(s)))
			}
			c.waitOutput(t, "det$ ")
			send(`printf '<%s:%s:%s>\n' "$((6*7))" "$LANG" "$DET_MARKER"; stty size` + "\r")
			c.waitOutput(t, "<42:en_US.UTF-8:det-web-terminal>")
			c.waitOutput(t, "30 100")

			sendResize(t, ws, 132, 50)
			time.Sleep(200 * time.Millisecond)
			send("stty size\r")
			c.waitOutput(t, "50 132")

			// Keepalives get answers from OpenSSH, so the session stays up.
			time.Sleep(time.Second)
			send("exit 7\r")
			require.Equal(t, CloseNormal, c.closeCode(t))
			stats := srv.waitStats(t, 10*time.Second)
			require.True(t, stats.Started)
			require.NotNil(t, stats.ExitCode)
			require.Equal(t, 7, *stats.ExitCode)
		})
	}

	t.Run("wrong host key", func(t *testing.T) {
		signer, pub, keys := shellKeys(t, config.KeyTypeED25519)
		_, otherPub, _ := shellKeys(t, config.KeyTypeED25519)
		addr := startOpenSSH(t, keys.PrivateKey, keys.PublicKey)

		target := Target{Addr: addr, User: cur.Username, Signer: signer, HostKey: otherPub}
		srv := newBridgeServer(context.Background(), t, target, fastOptions())
		c := readTerm(srv.dial(t))
		require.Equal(t, CloseUnavailable, c.closeCode(t))
		stats := srv.waitStats(t, 10*time.Second)
		require.False(t, stats.Started)
		require.Contains(t, stats.Reason, "host key mismatch")
		c.mu.Lock()
		defer c.mu.Unlock()
		require.Zero(t, c.output.Len())
		require.False(t, c.hasControl(msgReady))
		_ = pub
	})
}
