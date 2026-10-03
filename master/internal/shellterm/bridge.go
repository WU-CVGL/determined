package shellterm

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// Default timeouts of a terminal session.
const (
	// DefaultSetupTimeout bounds the TCP dial, the SSH handshake, and the session, PTY and shell
	// requests together.
	DefaultSetupTimeout = 15 * time.Second
	// DefaultPingInterval is how often the master pings the browser.
	DefaultPingInterval = 30 * time.Second
	// DefaultPongWait is how long the master waits for the browser to answer a ping.
	DefaultPongWait = 75 * time.Second
	// DefaultWriteWait bounds every write to the browser.
	DefaultWriteWait = 15 * time.Second
	// DefaultKeepaliveInterval is how often the master checks that the shell's sshd still answers.
	DefaultKeepaliveInterval = 30 * time.Second
	// DefaultKeepaliveTimeout is how long the master waits for sshd to answer a keepalive.
	DefaultKeepaliveTimeout = 15 * time.Second

	// ReadLimit is the largest WebSocket message the master accepts. Clients split input into
	// messages of at most 32 KiB.
	ReadLimit = 64 << 10

	outputChunk = 32 << 10
	termType    = "xterm-256color"
	closeGrace  = 2 * time.Second
	exitWait    = 5 * time.Second
	keepaliveRq = "keepalive@openssh.com"
)

// Target is a shell's SSH server and the credentials to log in to it. Every field comes from the
// master's own records, never from the request.
type Target struct {
	// Addr is the host:port of the shell's sshd.
	Addr string
	// User is the login user.
	User string
	// Signer holds the shell's private key.
	Signer ssh.Signer
	// HostKey is the shell's public key, which its sshd also uses as its host key.
	HostKey ssh.PublicKey
}

// Options configure a terminal session. Zero durations take the defaults above.
type Options struct {
	// Cols and Rows are the initial terminal size; see ClampSize.
	Cols, Rows int
	// Lang is sent as LANG when not empty. Callers pass it through ValidLang.
	Lang string

	SetupTimeout      time.Duration
	PingInterval      time.Duration
	PongWait          time.Duration
	WriteWait         time.Duration
	KeepaliveInterval time.Duration
	KeepaliveTimeout  time.Duration
	// IdleTimeout ends a session without input or output for this long. Zero disables it.
	IdleTimeout time.Duration
	// Deadline, when set, ends the session at that time.
	Deadline time.Time

	// Dial opens the TCP connection to the shell's sshd. It defaults to a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// OnStarted, when set, is called once the login shell has started.
	OnStarted func()
}

func (o Options) withDefaults() Options {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.SetupTimeout, DefaultSetupTimeout)
	def(&o.PingInterval, DefaultPingInterval)
	def(&o.PongWait, DefaultPongWait)
	def(&o.WriteWait, DefaultWriteWait)
	def(&o.KeepaliveInterval, DefaultKeepaliveInterval)
	def(&o.KeepaliveTimeout, DefaultKeepaliveTimeout)
	o.Cols, o.Rows = ClampSize(o.Cols, o.Rows)
	if o.Dial == nil {
		d := &net.Dialer{KeepAlive: 30 * time.Second}
		o.Dial = d.DialContext
	}
	return o
}

// Stats summarizes a finished session for the audit log.
type Stats struct {
	// Started is true when the login shell started.
	Started bool
	// BytesIn counts terminal input from the browser; BytesOut counts output sent to it.
	BytesIn, BytesOut int64
	// ExitCode is the login shell's exit status, when it reported one.
	ExitCode *int
	// CloseCode is the WebSocket close code sent to the browser, or zero when the browser went
	// away first.
	CloseCode int
	// Reason describes why the session ended. It may contain internal details: log it, never send
	// it to the browser.
	Reason string
}

// clientGoneError ends a session whose browser closed the connection, stopped reading, or stopped
// answering pings. No close frame is sent.
type clientGoneError struct{ err error }

func (e clientGoneError) Error() string { return "browser connection ended: " + e.err.Error() }
func (e clientGoneError) Unwrap() error { return e.err }

type frame struct {
	kind int
	data []byte
}

type session struct {
	ws     *websocket.Conn
	opts   Options
	log    *logrus.Entry
	cancel context.CancelCauseFunc

	out        chan frame
	stdin      chan []byte
	resizeSig  chan struct{}
	sizeMu     sync.Mutex
	size       [2]int // cols and rows of the latest resize request
	readerDone chan struct{}

	pingNonce    atomic.Uint64 // nonce of the outstanding ping, zero when none
	lastPong     atomic.Int64
	lastActivity atomic.Int64
	bytesIn      atomic.Int64
	bytesOut     atomic.Int64
	exitCode     atomic.Pointer[int]
	closeCode    atomic.Int64
}

// Serve runs a terminal session over an upgraded WebSocket until the login shell exits, the
// browser goes away, a timeout fires, or ctx is canceled. Cancel ctx with a CloseError cause (see
// context.WithCancelCause) to end the session with that close code; any other cancellation ends
// it with CloseGoingAway. Serve closes ws before it returns.
func Serve(ctx context.Context, ws *websocket.Conn, t Target, o Options, log *logrus.Entry) Stats {
	o = o.withDefaults()
	if log == nil {
		log = logrus.WithField("component", "shell-terminal")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	s := &session{
		ws:         ws,
		opts:       o,
		log:        log,
		cancel:     cancel,
		out:        make(chan frame, 16),
		stdin:      make(chan []byte, 16),
		resizeSig:  make(chan struct{}, 1),
		readerDone: make(chan struct{}),
	}
	now := time.Now().UnixNano()
	s.lastPong.Store(now)
	s.lastActivity.Store(now)

	ws.SetReadLimit(ReadLimit)
	ws.SetPongHandler(s.handlePong)
	if err := ws.SetReadDeadline(time.Now().Add(o.PongWait)); err != nil {
		cancel(clientGoneError{err})
	}

	var wg sync.WaitGroup
	goFn := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f()
		}()
	}
	// The browser side runs from the start, so a closed tab or a stalled browser also ends a
	// session that is still connecting.
	goFn(func() { s.readLoop(ctx) })
	goFn(func() { s.writeLoop(ctx) })
	goFn(func() { s.watchIdle(ctx) })
	if !o.Deadline.IsZero() {
		timer := time.AfterFunc(time.Until(o.Deadline), func() {
			cancel(NewCloseError(CloseTimeout, ErrCodeTimeLimit, "maximum session length reached"))
		})
		defer timer.Stop()
	}

	client, sess, stdin, stdout, err := s.connect(ctx, t)
	started := err == nil
	if err != nil {
		cancel(NewCloseError(CloseUnavailable, ErrCodeUnavailable, err.Error()))
	} else {
		if o.OnStarted != nil {
			o.OnStarted()
		}
		s.send(ctx, frame{websocket.TextMessage, encodeControl(ControlMessage{Type: msgReady})})
		waitErr := make(chan error, 1)
		goFn(func() { waitErr <- sess.Wait() })
		goFn(func() { s.pumpOutput(ctx, stdout, waitErr) })
		goFn(func() { s.pumpInput(ctx, sess, stdin) })
		goFn(func() { s.keepalive(ctx, client) })
	}

	<-ctx.Done()
	if client != nil {
		// Closing the client hangs up the login shell and unblocks the SSH goroutines.
		_ = client.Close()
	}
	wg.Wait()

	return Stats{
		Started:   started,
		BytesIn:   s.bytesIn.Load(),
		BytesOut:  s.bytesOut.Load(),
		ExitCode:  s.exitCode.Load(),
		CloseCode: int(s.closeCode.Load()),
		Reason:    context.Cause(ctx).Error(),
	}
}

// HostKeyAlgorithms returns the host key algorithms to offer for a pinned host key. RSA keys use
// SHA-2 signatures; OpenSSH 8.8 and later refuse ssh-rsa (SHA-1).
func HostKeyAlgorithms(key ssh.PublicKey) []string {
	if key.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{key.Type()}
}

// ClientConfig returns the SSH client configuration for a target: public key authentication with
// the shell's key as the target's user, and the shell's own public key as the only accepted host
// key.
func ClientConfig(t Target, timeout time.Duration) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:              t.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(t.Signer)},
		HostKeyCallback:   ssh.FixedHostKey(t.HostKey),
		HostKeyAlgorithms: HostKeyAlgorithms(t.HostKey),
		Timeout:           timeout,
	}
}

// connect opens the SSH session and starts the login shell. A single deadline covers every step,
// because x/crypto's ClientConfig.Timeout covers only ssh.Dial's TCP dial.
func (s *session) connect(
	ctx context.Context, t Target,
) (*ssh.Client, *ssh.Session, io.WriteCloser, io.Reader, error) {
	setupCtx, cancelSetup := context.WithTimeout(ctx, s.opts.SetupTimeout)
	defer cancelSetup()

	conn, err := s.opts.Dial(setupCtx, "tcp", t.Addr)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("dialing %s: %w", t.Addr, err)
	}
	if err := conn.SetDeadline(time.Now().Add(s.opts.SetupTimeout)); err != nil {
		_ = conn.Close()
		return nil, nil, nil, nil, fmt.Errorf("setting the setup deadline: %w", err)
	}
	// Until the shell starts, closing the session or the setup timeout closes the connection.
	stopWatch := context.AfterFunc(setupCtx, func() { _ = conn.Close() })

	fail := func(closer io.Closer, step string, err error) (
		*ssh.Client, *ssh.Session, io.WriteCloser, io.Reader, error,
	) {
		_ = closer.Close()
		if ctxErr := context.Cause(setupCtx); ctxErr != nil {
			return nil, nil, nil, nil, fmt.Errorf("%s with %s: %w (%w)", step, t.Addr, err, ctxErr)
		}
		return nil, nil, nil, nil, fmt.Errorf("%s with %s: %w", step, t.Addr, err)
	}

	c, chans, reqs, err := ssh.NewClientConn(conn, t.Addr, ClientConfig(t, s.opts.SetupTimeout))
	if err != nil {
		return fail(conn, "SSH handshake", err)
	}
	client := ssh.NewClient(c, chans, reqs)

	sess, err := client.NewSession()
	if err != nil {
		return fail(client, "opening an SSH session", err)
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.IUTF8:         1,
		ssh.TTY_OP_ISPEED: 38400,
		ssh.TTY_OP_OSPEED: 38400,
	}
	if err := sess.RequestPty(termType, s.opts.Rows, s.opts.Cols, modes); err != nil {
		return fail(client, "requesting a PTY", err)
	}
	if s.opts.Lang != "" {
		// sshd may refuse the variable; the shell then starts with the container's default.
		if err := sess.Setenv("LANG", s.opts.Lang); err != nil {
			s.log.WithError(err).Debug("sshd did not accept LANG")
		}
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return fail(client, "opening stdin", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return fail(client, "opening stdout", err)
	}
	if err := sess.Shell(); err != nil {
		return fail(client, "starting the login shell", err)
	}
	if !stopWatch() {
		// The setup timed out or the session ended just as the shell started.
		return fail(client, "starting the login shell", context.Cause(setupCtx))
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fail(client, "clearing the setup deadline", err)
	}
	return client, sess, stdin, stdout, nil
}

// send queues a frame for the writer. It returns false once the session is ending.
func (s *session) send(ctx context.Context, f frame) bool {
	select {
	case s.out <- f:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *session) touch() {
	s.lastActivity.Store(time.Now().UnixNano())
}

// handlePong extends the read deadline only for a pong that answers the outstanding ping, so
// unsolicited pongs cannot keep a session alive.
func (s *session) handlePong(data string) error {
	if len(data) != 8 {
		return nil
	}
	nonce := binary.BigEndian.Uint64([]byte(data))
	if nonce == 0 || !s.pingNonce.CompareAndSwap(nonce, 0) {
		return nil
	}
	now := time.Now()
	s.lastPong.Store(now.UnixNano())
	return s.ws.SetReadDeadline(now.Add(s.opts.PongWait))
}

func (s *session) readLoop(ctx context.Context) {
	defer close(s.readerDone)
	for {
		kind, data, err := s.ws.ReadMessage()
		if err != nil {
			s.cancel(clientGoneError{err})
			return
		}
		if ctx.Err() != nil {
			// Keep reading until the browser answers the close frame.
			continue
		}
		switch kind {
		case websocket.BinaryMessage:
			if len(data) == 0 {
				continue
			}
			s.bytesIn.Add(int64(len(data)))
			s.touch()
			select {
			case s.stdin <- data:
			case <-ctx.Done():
			}
		case websocket.TextMessage:
			var msg ControlMessage
			if json.Unmarshal(data, &msg) != nil || msg.Type != msgResize {
				continue
			}
			cols, rows := ClampSize(msg.Cols, msg.Rows)
			s.sizeMu.Lock()
			s.size = [2]int{cols, rows}
			s.sizeMu.Unlock()
			s.touch()
			select {
			case s.resizeSig <- struct{}{}:
			default:
			}
		}
	}
}

func (s *session) write(f frame) error {
	if err := s.ws.SetWriteDeadline(time.Now().Add(s.opts.WriteWait)); err != nil {
		return err
	}
	return s.ws.WriteMessage(f.kind, f.data)
}

func (s *session) writeLoop(ctx context.Context) {
	ping := time.NewTicker(s.opts.PingInterval)
	defer ping.Stop()
	for {
		select {
		case f := <-s.out:
			if err := s.write(f); err != nil {
				s.cancel(clientGoneError{fmt.Errorf("writing to the browser: %w", err)})
			}
		case <-ping.C:
			if time.Since(time.Unix(0, s.lastPong.Load())) > s.opts.PongWait {
				s.cancel(clientGoneError{errors.New("no pong from the browser")})
				continue
			}
			// A random nonce, so that a client cannot answer pings it never read.
			payload := make([]byte, 8)
			if _, err := rand.Read(payload); err != nil {
				s.cancel(fmt.Errorf("generating a ping nonce: %w", err))
				continue
			}
			payload[0] |= 1 // Never zero, which means "no ping outstanding".
			s.pingNonce.Store(binary.BigEndian.Uint64(payload))
			err := s.ws.WriteControl(websocket.PingMessage, payload, time.Now().Add(s.opts.WriteWait))
			if err != nil {
				s.cancel(clientGoneError{fmt.Errorf("pinging the browser: %w", err)})
			}
		case <-ctx.Done():
			s.finish(ctx)
			return
		}
	}
}

// finish sends what is left to send and closes the WebSocket.
func (s *session) finish(ctx context.Context) {
	defer func() { _ = s.ws.Close() }()

	cause := context.Cause(ctx)
	var ce CloseError
	var gone clientGoneError
	switch {
	case errors.As(cause, &gone):
		return
	case errors.As(cause, &ce):
	case errors.Is(cause, context.Canceled):
		ce = NewCloseError(CloseGoingAway, ErrCodeShutdown, "")
	default:
		ce = NewCloseError(CloseInternal, ErrCodeInternal, "")
	}

	// Flush queued output, such as the end of the shell's output and its exit status.
	for flushing := true; flushing; {
		select {
		case f := <-s.out:
			if s.write(f) != nil {
				return
			}
		default:
			flushing = false
		}
	}
	if ce.ErrCode != "" {
		msg := ControlMessage{Type: msgError, Code: ce.ErrCode, Message: ce.Message}
		if s.write(frame{websocket.TextMessage, encodeControl(msg)}) != nil {
			return
		}
	}
	msg := websocket.FormatCloseMessage(ce.Code, ce.ErrCode)
	if s.ws.WriteControl(websocket.CloseMessage, msg, time.Now().Add(s.opts.WriteWait)) != nil {
		return
	}
	s.closeCode.Store(int64(ce.Code))
	// Give the browser a moment to answer, so that it sees the close code before the TCP
	// connection closes.
	timer := time.NewTimer(closeGrace)
	defer timer.Stop()
	select {
	case <-s.readerDone:
	case <-timer.C:
	}
}

func (s *session) watchIdle(ctx context.Context) {
	if s.opts.IdleTimeout <= 0 {
		return
	}
	timer := time.NewTimer(s.opts.IdleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			idle := time.Since(time.Unix(0, s.lastActivity.Load()))
			if idle >= s.opts.IdleTimeout {
				s.cancel(NewCloseError(CloseTimeout, ErrCodeIdle, "idle timeout"))
				return
			}
			timer.Reset(s.opts.IdleTimeout - idle)
		}
	}
}

func (s *session) pumpOutput(ctx context.Context, stdout io.Reader, waitErr <-chan error) {
	buf := make([]byte, outputChunk)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			s.bytesOut.Add(int64(n))
			s.touch()
			data := make([]byte, n)
			copy(data, buf[:n])
			if !s.send(ctx, frame{websocket.BinaryMessage, data}) {
				return
			}
		}
		if err != nil {
			break
		}
	}

	timer := time.NewTimer(exitWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		s.cancel(NewCloseError(CloseUnavailable, ErrCodeUnavailable, "no exit status from the shell"))
	case err := <-waitErr:
		msg := ControlMessage{Type: msgExit}
		var exitErr *ssh.ExitError
		var missing *ssh.ExitMissingError
		switch {
		case err == nil:
			code := 0
			s.exitCode.Store(&code)
			msg.Code = code
		case errors.As(err, &exitErr):
			code := exitErr.ExitStatus()
			s.exitCode.Store(&code)
			msg.Code = code
		case errors.As(err, &missing):
			// The connection or channel closed without an exit status.
		default:
			s.cancel(NewCloseError(CloseUnavailable, ErrCodeUnavailable, "waiting for the shell: "+err.Error()))
			return
		}
		s.send(ctx, frame{websocket.TextMessage, encodeControl(msg)})
		s.cancel(CloseError{Code: CloseNormal, Detail: fmt.Sprintf("shell exited (%v)", err)})
	}
}

func (s *session) pumpInput(ctx context.Context, sess *ssh.Session, stdin io.Writer) {
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-s.stdin:
			if _, err := stdin.Write(data); err != nil {
				return
			}
		case <-s.resizeSig:
			s.sizeMu.Lock()
			cols, rows := s.size[0], s.size[1]
			s.sizeMu.Unlock()
			if err := sess.WindowChange(rows, cols); err != nil {
				return
			}
		}
	}
}

// keepalive ends the session when the shell's sshd stops answering, for example after its node
// fails without closing the TCP connection.
func (s *session) keepalive(ctx context.Context, client *ssh.Client) {
	ticker := time.NewTicker(s.opts.KeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		res := make(chan error, 1)
		go func() {
			// A reply of either kind proves that sshd is alive. The call returns when the
			// client is closed.
			_, _, err := client.SendRequest(keepaliveRq, true, nil)
			res <- err
		}()
		timer := time.NewTimer(s.opts.KeepaliveTimeout)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case err := <-res:
			timer.Stop()
			if err != nil {
				// The connection is closed; the output pump reports how the session ended.
				return
			}
		case <-timer.C:
			s.cancel(NewCloseError(CloseUnavailable, ErrCodeUnavailable, "sshd did not answer a keepalive"))
			return
		}
	}
}
