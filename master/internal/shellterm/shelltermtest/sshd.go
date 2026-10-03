// Package shelltermtest provides an in-process SSH server that behaves like the sshd in a
// Determined shell container, for tests of the browser terminal.
package shelltermtest

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/determined-ai/determined/master/internal/config"
	detssh "github.com/determined-ai/determined/master/pkg/ssh"
)

// Keys generates a shell key pair the way LaunchShell does.
func Keys(t *testing.T, keyType string) (ssh.Signer, ssh.PublicKey, detssh.PrivateAndPublicKeys) {
	t.Helper()
	keys, err := detssh.GenerateKey(config.SSHConfig{KeyType: keyType, RsaKeySize: 2048})
	require.NoError(t, err)
	signer, err := ssh.ParsePrivateKey(keys.PrivateKey)
	require.NoError(t, err)
	pub, _, _, _, err := ssh.ParseAuthorizedKey(keys.PublicKey) //nolint:dogsled
	require.NoError(t, err)
	return signer, pub, keys
}

// Behavior is what the fake login shell does.
type Behavior int

const (
	// Echo echoes input back. A line "exit N" makes the shell exit with status N.
	Echo Behavior = iota
	// Flood writes output as fast as possible.
	Flood
	// Silent never writes.
	Silent
)

// PTYRequest is a recorded pty-req.
type PTYRequest struct {
	Term          string
	Columns, Rows uint32
	Width, Height uint32
	Modes         string
}

// WindowChange is a recorded window-change request.
type WindowChange struct {
	Columns, Rows uint32
	Width, Height uint32
}

type envRequest struct {
	Name, Value string
}

// Record is what the server has seen so far.
type Record struct {
	Users   []string
	PTYs    []PTYRequest
	Resizes []WindowChange
	Env     map[string]string
	Shells  int
}

// Config configures an SSHD.
type Config struct {
	// HostKey is the server's only host key; Determined uses the shell's own key.
	HostKey ssh.Signer
	// AuthorizedKey is the only key the server accepts, for any user.
	AuthorizedKey ssh.PublicKey
	Behavior      Behavior
	// IgnoreGlobalRequests makes the server never answer keepalives.
	IgnoreGlobalRequests bool
}

// SSHD is an in-process SSH server.
type SSHD struct {
	cfg Config
	ln  net.Listener

	mu       sync.Mutex
	rec      Record
	closed   chan struct{}
	closeOne sync.Once
}

// NewSSHD starts an SSH server on a loopback port. It stops when the test ends.
func NewSSHD(t *testing.T, cfg Config) *SSHD {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &SSHD{cfg: cfg, ln: ln, rec: Record{Env: map[string]string{}}, closed: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

// Addr returns the server's host:port.
func (s *SSHD) Addr() string { return s.ln.Addr().String() }

// Closed is closed when the first session channel ends.
func (s *SSHD) Closed() <-chan struct{} { return s.closed }

// Record returns a copy of what the server has seen.
func (s *SSHD) Record() Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	env := map[string]string{}
	for k, v := range s.rec.Env {
		env[k] = v
	}
	return Record{
		Users:   append([]string(nil), s.rec.Users...),
		PTYs:    append([]PTYRequest(nil), s.rec.PTYs...),
		Resizes: append([]WindowChange(nil), s.rec.Resizes...),
		Env:     env,
		Shells:  s.rec.Shells,
	}
}

func (s *SSHD) serve() {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			s.mu.Lock()
			s.rec.Users = append(s.rec.Users, meta.User())
			s.mu.Unlock()
			if bytes.Equal(key.Marshal(), s.cfg.AuthorizedKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(s.cfg.HostKey)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn, cfg)
	}
}

func (s *SSHD) handleConn(conn net.Conn, cfg *ssh.ServerConfig) {
	defer func() { _ = conn.Close() }()
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go func() {
		for req := range reqs {
			if s.cfg.IgnoreGlobalRequests {
				continue
			}
			if req.WantReply {
				// OpenSSH answers keepalive@openssh.com with a failure, too.
				_ = req.Reply(false, nil)
			}
		}
	}()
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.handleSession(ch, chReqs)
	}
}

func (s *SSHD) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer s.closeOne.Do(func() { close(s.closed) })
	defer func() { _ = ch.Close() }()
	done := make(chan struct{})
	started := false
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var p PTYRequest
			if err := ssh.Unmarshal(req.Payload, &p); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			s.mu.Lock()
			s.rec.PTYs = append(s.rec.PTYs, p)
			s.mu.Unlock()
			_ = req.Reply(true, nil)
		case "env":
			var e envRequest
			if err := ssh.Unmarshal(req.Payload, &e); err == nil {
				s.mu.Lock()
				s.rec.Env[e.Name] = e.Value
				s.mu.Unlock()
			}
			_ = req.Reply(true, nil)
		case "window-change":
			var w WindowChange
			if err := ssh.Unmarshal(req.Payload, &w); err == nil {
				s.mu.Lock()
				s.rec.Resizes = append(s.rec.Resizes, w)
				s.mu.Unlock()
			}
		case "shell":
			s.mu.Lock()
			s.rec.Shells++
			s.mu.Unlock()
			started = true
			_ = req.Reply(true, nil)
			go func() {
				defer close(done)
				s.runShell(ch)
				// Like sshd, close the channel once the login shell exits.
				_ = ch.Close()
			}()
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
	if started {
		<-done
	}
}

func (s *SSHD) runShell(ch ssh.Channel) {
	switch s.cfg.Behavior {
	case Flood:
		block := bytes.Repeat([]byte("x"), 1<<20)
		for {
			if _, err := ch.Write(block); err != nil {
				return
			}
		}
	case Silent:
		_, _ = io.Copy(io.Discard, ch)
	case Echo:
		var line []byte
		buf := make([]byte, 4096)
		for {
			n, err := ch.Read(buf)
			if n > 0 {
				if _, werr := ch.Write(buf[:n]); werr != nil {
					return
				}
				line = append(line, buf[:n]...)
				for {
					i := bytes.IndexAny(line, "\r\n")
					if i < 0 {
						break
					}
					cmd := strings.TrimSpace(string(line[:i]))
					line = line[i+1:]
					if code, ok := strings.CutPrefix(cmd, "exit "); ok {
						status, _ := strconv.ParseUint(code, 10, 8)
						_, _ = ch.Write([]byte("logout\r\n"))
						_, _ = ch.SendRequest("exit-status", false,
							ssh.Marshal(struct{ Status uint32 }{uint32(status)})) //nolint:gosec // 8 bits.
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}
}
