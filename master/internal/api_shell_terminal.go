package internal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/rbac/audit"
	"github.com/determined-ai/determined/master/internal/shellterm"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

const (
	// shellTerminalRoute serves the browser terminal of a shell.
	shellTerminalRoute = "/ws/shells/:shell_id/terminal"
	// shellTerminalOffSwitch in the master's feature_switches turns the browser terminal off.
	shellTerminalOffSwitch = "-shell_terminal"
)

var errShellTerminalDenied = errors.New("only the shell's owner or an admin may open its terminal")

// shellTerminalService serves the browser terminal of shells: a WebSocket on the master that it
// bridges to an SSH session in the shell's container, logged in with the shell's own key. The
// key stays in the master.
type shellTerminalService struct {
	cfg      config.ShellTerminalConfig
	trusted  []*net.IPNet
	limiter  *shellterm.Limiter
	baseCtx  context.Context //nolint:containedctx // Canceled when the master shuts down.
	upgrader websocket.Upgrader
	log      *logrus.Entry

	// bridge holds the transport options; tests shorten its timeouts.
	bridge shellterm.Options
	// lookup returns a snapshot of a shell.
	lookup func(model.TaskID) (*command.ShellTerminalTarget, error)
	// reauthenticate reads the user and login session behind a request again.
	reauthenticate func(*http.Request) (*model.User, *model.UserSession, error)
	// taskLog writes a line to a shell's task log.
	taskLog func(model.AllocationID, string)
}

func newShellTerminalService(
	ctx context.Context, cfg config.ShellTerminalConfig,
) (*shellTerminalService, error) {
	trusted, err := shellterm.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("parsing shell_terminal.trusted_proxies: %w", err)
	}
	return &shellTerminalService{
		cfg:     cfg,
		trusted: trusted,
		limiter: shellterm.NewLimiter(cfg.MaxSessionsPerUser, cfg.MaxSessions),
		baseCtx: ctx,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			ReadBufferSize:   32 << 10,
			WriteBufferSize:  32 << 10,
			// Checked before the upgrade as well. Never allow all origins, whatever enable_cors
			// says: the session cookie is sent with cross-site WebSocket handshakes.
			CheckOrigin: shellterm.CheckSameOrigin,
		},
		log:    logrus.WithField("component", "shell-terminal"),
		bridge: shellterm.Options{Dial: dialShellSSHD},
		lookup: func(id model.TaskID) (*command.ShellTerminalTarget, error) {
			return command.DefaultCmdService.ShellTerminalTarget(id)
		},
		reauthenticate: func(r *http.Request) (*model.User, *model.UserSession, error) {
			return user.GetService().UserAndSessionFromRequest(r)
		},
		taskLog: func(id model.AllocationID, msg string) {
			task.DefaultService.SendLog(context.Background(), id, &sproto.ContainerLog{
				Timestamp:  time.Now().UTC(),
				Level:      ptrs.Ptr("INFO"),
				AuxMessage: &msg,
			})
		},
	}, nil
}

// dialShellSSHD dials a shell's sshd the way the master's TCP proxy does, honoring the proxy
// environment variables.
func dialShellSSHD(ctx context.Context, network, addr string) (net.Conn, error) {
	d := proxy.FromEnvironmentUsing(&net.Dialer{KeepAlive: 30 * time.Second})
	if cd, ok := d.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	return d.Dial(network, addr)
}

// authorize returns the shell if the user may open its terminal: the shell's owner, or an admin
// when shell_terminal.allow_admin is set, in every authorization mode. Workspace permissions such
// as RBAC's UPDATE_NSC are not enough, because the terminal runs as the shell's user. The user
// must also be allowed to see the shell.
func (s *shellTerminalService) authorize(
	ctx context.Context, curUser model.User, id model.TaskID,
) (target *command.ShellTerminalTarget, adminOverride bool, err error) {
	target, err = s.lookup(id)
	if err != nil {
		return nil, false, err
	}
	if err := command.AuthZProvider.Get().CanGetNSC(ctx, curUser, target.WorkspaceID); err != nil {
		return nil, false, err
	}
	switch {
	case target.OwnerID > 0 && curUser.ID == target.OwnerID:
		return target, false, nil
	case curUser.Admin && s.cfg.AllowAdmin:
		return target, true, nil
	default:
		return nil, false, errShellTerminalDenied
	}
}

// handle serves GET /ws/shells/:shell_id/terminal. The master's authentication middleware has
// already authenticated the request with the session cookie or a bearer token.
func (s *shellTerminalService) handle(c echo.Context) error {
	req := c.Request()
	curUser, ok := c.Get("user").(model.User)
	if !ok {
		return echo.NewHTTPError(http.StatusUnauthorized)
	}
	loginSession, _ := c.Get("user-session").(model.UserSession)
	shellID := model.TaskID(c.Param("shell_id"))
	log := s.log.WithFields(logrus.Fields{
		"shell_id":    shellID,
		"user":        curUser.Username,
		"user_id":     curUser.ID,
		"remote_addr": shellterm.PeerIP(req),
		"client_ip":   shellterm.ClientIP(req, s.trusted),
	})

	if !shellterm.CheckSameOrigin(req) {
		log.WithField("origin", req.Header.Get("Origin")).Warn("refused a shell terminal: wrong origin")
		return echo.NewHTTPError(http.StatusForbidden)
	}

	ctx := audit.SupplyEntityID(req.Context(), shellID.String())
	target, adminOverride, err := s.authorize(ctx, curUser, shellID)
	if err != nil {
		// Unknown shells, other tasks and shells the user may not open look the same.
		log.WithError(err).Info("refused a shell terminal")
		return echo.NewHTTPError(http.StatusNotFound)
	}
	defer clear(target.PrivateKey)
	log = log.WithFields(logrus.Fields{
		"owner_id":       target.OwnerID,
		"workspace_id":   target.WorkspaceID,
		"admin_override": adminOverride,
		"login_user":     target.LoginUser,
	})

	release, ok := s.limiter.Acquire(int(curUser.ID))
	if !ok {
		log.Info("refused a shell terminal: too many open terminals")
		return echo.NewHTTPError(http.StatusTooManyRequests, "too many open terminals")
	}
	defer release()

	ws, err := s.upgrader.Upgrade(c.Response(), req, nil)
	if err != nil {
		// The upgrader has written the HTTP error.
		log.WithError(err).Info("shell terminal WebSocket upgrade failed")
		return nil
	}
	s.serve(ws, req, curUser, loginSession, target, adminOverride, log)
	return nil
}

func (s *shellTerminalService) serve(
	ws *websocket.Conn,
	req *http.Request,
	curUser model.User,
	loginSession model.UserSession,
	target *command.ShellTerminalTarget,
	adminOverride bool,
	log *logrus.Entry,
) {
	switch {
	case target.Ended:
		shellterm.Reject(ws, shellterm.NewCloseError(shellterm.CloseEnded, shellterm.ErrCodeEnded, ""),
			s.bridge.WriteWait)
		return
	case !target.Ready:
		shellterm.Reject(ws, shellterm.NewCloseError(shellterm.CloseNotReady, shellterm.ErrCodeNotReady, ""),
			s.bridge.WriteWait)
		return
	}

	sshTarget, err := shellSSHTarget(target)
	if err != nil {
		log.WithError(err).Error("cannot open a shell terminal")
		shellterm.Reject(ws, shellterm.NewCloseError(shellterm.CloseUnavailable,
			shellterm.ErrCodeUnavailable, ""), s.bridge.WriteWait)
		return
	}

	query := req.URL.Query()
	cols, _ := strconv.Atoi(query.Get("cols"))
	rows, _ := strconv.Atoi(query.Get("rows"))
	opts := s.bridge
	opts.Cols, opts.Rows = shellterm.ClampSize(cols, rows)
	opts.Lang = shellterm.ValidLang(query.Get("lang"))
	opts.IdleTimeout = time.Duration(s.cfg.IdleTimeout)
	opts.Deadline = time.Now().Add(time.Duration(s.cfg.MaxSessionDuration))
	// The terminal also ends when its login session expires, which asks the user to sign in again.
	opts.LoginExpiry = loginSession.Expiry

	start := time.Now()
	who := curUser.Username
	if adminOverride {
		who += " (admin)"
	}
	opts.OnStarted = func() {
		log.Info("shell terminal opened")
		s.taskLog(target.AllocationID, fmt.Sprintf("Browser terminal opened by %s.", who))
	}

	ctx, cancel := context.WithCancelCause(s.baseCtx)
	// The terminal's slot is released only after its recheck has stopped, so that nothing the
	// terminal started outlives it.
	var recheckDone sync.WaitGroup
	defer recheckDone.Wait()
	defer cancel(nil)
	recheckDone.Add(1)
	go func() {
		defer recheckDone.Done()
		s.recheck(ctx, cancel, req, curUser, target.TaskID)
	}()

	stats := shellterm.Serve(ctx, ws, sshTarget, opts, log)

	fields := logrus.Fields{
		"started":    stats.Started,
		"duration":   time.Since(start).Round(time.Millisecond).String(),
		"bytes_in":   stats.BytesIn,
		"bytes_out":  stats.BytesOut,
		"close_code": stats.CloseCode,
		"reason":     stats.Reason,
	}
	if stats.ExitCode != nil {
		fields["exit_code"] = *stats.ExitCode
	}
	log.WithFields(fields).Info("shell terminal closed")
	if stats.Started {
		s.taskLog(target.AllocationID, fmt.Sprintf("Browser terminal of %s closed after %s.",
			who, time.Since(start).Round(time.Second)))
	}
}

// shellSSHTarget parses the shell's keys. The login user and the address come from the master's
// records only, never from the request.
func shellSSHTarget(t *command.ShellTerminalTarget) (shellterm.Target, error) {
	if t.SSHAddr == "" {
		return shellterm.Target{}, fmt.Errorf("shell %s has no SSH address", t.TaskID)
	}
	signer, err := ssh.ParsePrivateKey(t.PrivateKey)
	if err != nil {
		return shellterm.Target{}, fmt.Errorf("parsing the key of shell %s: %w", t.TaskID, err)
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey(t.PublicKey) //nolint:dogsled
	if err != nil {
		return shellterm.Target{}, fmt.Errorf("parsing the public key of shell %s: %w", t.TaskID, err)
	}
	return shellterm.Target{Addr: t.SSHAddr, User: t.LoginUser, Signer: signer, HostKey: hostKey}, nil
}

// recheck periodically checks that the login session behind an open terminal is still valid and
// that its user may still open the shell, and ends the terminal otherwise. Logging out, an
// expired or revoked token, deactivation and the loss of admin rights all end open terminals.
func (s *shellTerminalService) recheck(
	ctx context.Context, cancel context.CancelCauseFunc, req *http.Request,
	curUser model.User, shellID model.TaskID,
) {
	ticker := time.NewTicker(time.Duration(s.cfg.RecheckInterval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		u, _, err := s.reauthenticate(req)
		switch {
		case err != nil:
			cancel(shellterm.NewCloseError(shellterm.CloseUnauthenticated, shellterm.ErrCodeUnauthenticate,
				"login session no longer valid: "+err.Error()))
			return
		case u == nil || u.ID != curUser.ID || !u.Active:
			cancel(shellterm.NewCloseError(shellterm.CloseUnauthenticated, shellterm.ErrCodeUnauthenticate,
				"user no longer active"))
			return
		}
		target, _, err := s.authorize(ctx, *u, shellID)
		if err != nil {
			cancel(shellterm.NewCloseError(shellterm.CloseForbidden, shellterm.ErrCodeForbidden,
				"no longer allowed: "+err.Error()))
			return
		}
		clear(target.PrivateKey)
	}
}
