//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apiPkg "github.com/determined-ai/determined/master/internal/api"
	authz2 "github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/config"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/job/jobservice"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/shellterm"
	"github.com/determined-ai/determined/master/internal/shellterm/shelltermtest"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

const shellTerminalAgentUser = "det-agent-user"

// shellTerminalEnv runs the terminal endpoint behind the master's authentication middleware,
// for a shell whose sshd is an in-process SSH server.
type shellTerminalEnv struct {
	api     *apiServer
	svc     *shellTerminalService
	srv     *httptest.Server
	sshd    *shelltermtest.SSHD
	shellID model.TaskID
	// notebookID is a task of another type.
	notebookID model.TaskID
	keyPEM     string

	owner, other, admin model.User
	tokens              map[model.UserID]string

	ready    atomic.Bool
	dials    atomic.Int32
	logHook  *logrustest.Hook
	mu       sync.Mutex
	taskLogs []string
}

func setupShellTerminalTest(
	t *testing.T, behavior shelltermtest.Behavior, cfgFn func(*config.ShellTerminalConfig),
) *shellTerminalEnv {
	api, admin, _ := setupAPITest(t, nil)
	user.InitService(api.m.db, &model.ExternalSessions{})
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	jobservice.SetDefaultService(api.m.rm)
	config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{Type: "basic"}

	signer, pub, keys := shelltermtest.Keys(t, config.KeyTypeED25519)
	env := &shellTerminalEnv{
		api:    api,
		admin:  admin,
		sshd:   shelltermtest.NewSSHD(t, shelltermtest.Config{HostKey: signer, AuthorizedKey: pub, Behavior: behavior}),
		keyPEM: string(keys.PrivateKey),
		tokens: map[model.UserID]string{},
	}
	env.ready.Store(true)

	req := mockGenericReq(t, api.m.db)
	req.Spec.Metadata.WorkspaceID = model.DefaultWorkspaceID
	req.Spec.Metadata.PrivateKey = ptrs.Ptr(string(keys.PrivateKey))
	req.Spec.Metadata.PublicKey = ptrs.Ptr(string(keys.PublicKey))
	req.Spec.Base.AgentUserGroup = &model.AgentUserGroup{User: shellTerminalAgentUser, Group: "det"}
	owner, err := user.ByID(context.TODO(), req.Spec.Base.Owner.ID)
	require.NoError(t, err)
	env.owner = owner.ToUser()
	env.other = db.RequireMockUser(t, api.m.db)

	shell, err := command.DefaultCmdService.LaunchGenericCommand(model.TaskTypeShell, model.JobTypeShell, req)
	require.NoError(t, err)
	env.shellID = model.TaskID(shell.ToV1Shell().Id)
	nbReq := mockGenericReq(t, api.m.db)
	nbReq.Spec.Base.Owner = &model.User{ID: env.owner.ID}
	nb, err := command.DefaultCmdService.LaunchGenericCommand(model.TaskTypeNotebook, model.JobTypeNotebook, nbReq)
	require.NoError(t, err)
	env.notebookID = model.TaskID(nb.ToV1Notebook().Id)

	cfg := config.DefaultShellTerminalConfig()
	if cfgFn != nil {
		cfgFn(&cfg)
	}
	svc, err := newShellTerminalService(context.Background(), cfg)
	require.NoError(t, err)
	// The mock resource manager never starts the shell's container; pretend its sshd runs at the
	// in-process server. Everything else comes from the real command registry.
	svc.lookup = func(id model.TaskID) (*command.ShellTerminalTarget, error) {
		target, err := command.DefaultCmdService.ShellTerminalTarget(id)
		if err != nil {
			return nil, err
		}
		target.State = model.AllocationStateRunning
		target.Ended = false
		target.Ready = env.ready.Load()
		target.SSHAddr = env.sshd.Addr()
		return target, nil
	}
	svc.bridge.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		env.dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	svc.taskLog = func(_ model.AllocationID, msg string) {
		env.mu.Lock()
		defer env.mu.Unlock()
		env.taskLogs = append(env.taskLogs, msg)
	}
	env.svc = svc

	// The master's middleware chain, as far as it concerns this route.
	e := echo.New()
	e.HTTPErrorHandler = apiPkg.JSONErrorHandler
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error { return next(&detContext.DetContext{Context: c}) }
	})
	e.Use(processAuthWithRedirect(nil))
	e.GET(shellTerminalRoute, svc.handle)
	env.srv = httptest.NewServer(e)
	t.Cleanup(env.srv.Close)

	env.logHook = logrustest.NewGlobal()
	t.Cleanup(env.logHook.Reset)
	return env
}

func (e *shellTerminalEnv) token(t *testing.T, u model.User) string {
	if tok, ok := e.tokens[u.ID]; ok {
		return tok
	}
	tok, err := user.StartSession(context.TODO(), &u)
	require.NoError(t, err)
	e.tokens[u.ID] = tok
	return tok
}

func (e *shellTerminalEnv) url(id model.TaskID, query string) string {
	u := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws/shells/" + id.String() + "/terminal"
	if query != "" {
		u += "?" + query
	}
	return u
}

func (e *shellTerminalEnv) origin() string { return e.srv.URL }

func (e *shellTerminalEnv) dial(
	t *testing.T, u *model.User, id model.TaskID, query string, header http.Header,
) (*websocket.Conn, *http.Response, error) {
	if header == nil {
		header = http.Header{}
	}
	if u != nil && header.Get("Authorization") == "" {
		header.Set("Cookie", "auth="+e.token(t, *u))
	}
	ws, resp, err := websocket.DefaultDialer.Dial(e.url(id, query), header)
	if ws != nil {
		t.Cleanup(func() { _ = ws.Close() })
	}
	return ws, resp, err
}

// open opens a terminal with the user's session cookie, as a browser would, and requires it to
// succeed.
func (e *shellTerminalEnv) open(
	t *testing.T, u *model.User, id model.TaskID, query string, header http.Header,
) *websocket.Conn {
	t.Helper()
	ws, resp, err := e.dial(t, u, id, query, header)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return ws
}

// refused requires opening a terminal to fail with an HTTP status, and returns the body.
func (e *shellTerminalEnv) refused(
	t *testing.T, u *model.User, id model.TaskID, header http.Header, status int,
) string {
	t.Helper()
	_, resp, err := e.dial(t, u, id, "", header)
	require.ErrorIs(t, err, websocket.ErrBadHandshake)
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, status, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

type termFrames struct {
	mu      sync.Mutex
	output  strings.Builder
	control []shellterm.ControlMessage
	all     []string
	err     error
	done    chan struct{}
}

func readTermFrames(ws *websocket.Conn) *termFrames {
	f := &termFrames{done: make(chan struct{})}
	go func() {
		defer close(f.done)
		for {
			kind, data, err := ws.ReadMessage()
			f.mu.Lock()
			if err != nil {
				f.err = err
				f.mu.Unlock()
				return
			}
			f.all = append(f.all, string(data))
			if kind == websocket.BinaryMessage {
				f.output.Write(data)
			} else {
				var m shellterm.ControlMessage
				if json.Unmarshal(data, &m) == nil {
					f.control = append(f.control, m)
				}
			}
			f.mu.Unlock()
		}
	}()
	return f
}

func (f *termFrames) waitControl(t *testing.T, typ string) shellterm.ControlMessage {
	t.Helper()
	var found shellterm.ControlMessage
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, m := range f.control {
			if m.Type == typ {
				found = m
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "no %q message", typ)
	return found
}

func (f *termFrames) waitOutput(t *testing.T, s string) {
	t.Helper()
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Contains(f.output.String(), s)
	}, 10*time.Second, 10*time.Millisecond, "no output %q", s)
}

func (f *termFrames) closeCode(t *testing.T) int {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(15 * time.Second):
		t.Fatal("the terminal did not close")
	}
	var ce *websocket.CloseError
	if errors.As(f.err, &ce) {
		return ce.Code
	}
	return 0
}

func (e *shellTerminalEnv) auditEntries(msg string) []*logrus.Entry {
	var out []*logrus.Entry
	for _, entry := range e.logHook.AllEntries() {
		if entry.Data["component"] == "shell-terminal" && entry.Message == msg {
			out = append(out, entry)
		}
	}
	return out
}

func (e *shellTerminalEnv) waitAudit(t *testing.T, msg string, n int) []*logrus.Entry {
	t.Helper()
	require.Eventually(t, func() bool { return len(e.auditEntries(msg)) >= n }, 10*time.Second,
		10*time.Millisecond, "no %q audit entry", msg)
	return e.auditEntries(msg)
}

func TestShellTerminalAuthentication(t *testing.T) {
	env := setupShellTerminalTest(t, shelltermtest.Echo, nil)

	// No session.
	env.refused(t, nil, env.shellID, nil, http.StatusUnauthorized)
	// A forged token.
	env.refused(t, nil, env.shellID, http.Header{"Cookie": {"auth=v2.public.forged"}}, http.StatusUnauthorized)
	// An inactive user.
	inactive := db.RequireMockUser(t, env.api.m.db)
	env.token(t, inactive)
	_, err := db.Bun().NewUpdate().Table("users").Set("active = false").Where("id = ?", inactive.ID).
		Exec(context.TODO())
	require.NoError(t, err)
	env.refused(t, &inactive, env.shellID, nil, http.StatusForbidden)

	// A bearer token works as well as the cookie.
	ws := env.open(t, &env.owner, env.shellID, "", http.Header{
		"Authorization": {"Bearer " + env.token(t, env.owner)},
	})
	readTermFrames(ws).waitControl(t, "ready")
	require.EqualValues(t, 1, env.dials.Load())
}

func TestShellTerminalOrigin(t *testing.T) {
	env := setupShellTerminalTest(t, shelltermtest.Echo, nil)
	for _, origin := range []string{
		"https://evil.example.org",
		"http://" + strings.TrimPrefix(env.srv.URL, "http://") + ".evil.org",
		"null",
	} {
		env.refused(t, &env.owner, env.shellID, http.Header{"Origin": {origin}}, http.StatusForbidden)
	}
	require.Zero(t, env.dials.Load(), "dialed sshd for a cross-origin request")

	ws := env.open(t, &env.owner, env.shellID, "", http.Header{"Origin": {env.origin()}})
	readTermFrames(ws).waitControl(t, "ready")
}

func TestShellTerminalAuthZ(t *testing.T) {
	env := setupShellTerminalTest(t, shelltermtest.Echo, nil)

	// Unknown IDs, other task types and other users' shells all get the same plain 404.
	unknown := env.refused(t, &env.owner, model.TaskID("no-such-shell"), nil, http.StatusNotFound)
	require.JSONEq(t, `{"message":"Not Found"}`, unknown)
	require.Equal(t, unknown, env.refused(t, &env.owner, env.notebookID, nil, http.StatusNotFound))
	require.Equal(t, unknown, env.refused(t, &env.other, env.shellID, nil, http.StatusNotFound))
	require.Zero(t, env.dials.Load(), "dialed sshd for a refused request")

	// The owner gets a terminal. The login user, the address and the key come from the master,
	// whatever the request says.
	ws := env.open(t, &env.owner, env.shellID,
		"cols=90&rows=20&lang=de_DE.UTF-8&user=root&login_user=root&addr=10.0.0.1:22", nil)
	frames := readTermFrames(ws)
	frames.waitControl(t, "ready")
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte("whoami\r")))
	frames.waitOutput(t, "whoami")
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte("exit 0\r")))
	require.Equal(t, shellterm.CloseNormal, frames.closeCode(t))

	rec := env.sshd.Record()
	require.Equal(t, []string{shellTerminalAgentUser}, rec.Users)
	require.Len(t, rec.PTYs, 1)
	require.Equal(t, uint32(90), rec.PTYs[0].Columns)
	require.Equal(t, uint32(20), rec.PTYs[0].Rows)
	require.Equal(t, map[string]string{"LANG": "de_DE.UTF-8"}, rec.Env)

	opened := env.waitAudit(t, "shell terminal opened", 1)
	require.Equal(t, false, opened[0].Data["admin_override"])
	require.Equal(t, env.owner.ID, opened[0].Data["user_id"])
	closed := env.waitAudit(t, "shell terminal closed", 1)
	require.Equal(t, 0, closed[0].Data["exit_code"])
	require.Equal(t, int64(len("whoami\rexit 0\r")), closed[0].Data["bytes_in"])
	require.NotEmpty(t, closed[0].Data["duration"])

	// An admin gets a terminal in another user's shell, and the owner can see that in the
	// shell's task log.
	ws = env.open(t, &env.admin, env.shellID, "", nil)
	frames = readTermFrames(ws)
	frames.waitControl(t, "ready")
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte("exit 3\r")))
	require.Equal(t, shellterm.CloseNormal, frames.closeCode(t))
	opened = env.waitAudit(t, "shell terminal opened", 2)
	require.Equal(t, true, opened[1].Data["admin_override"])
	require.Equal(t, env.admin.ID, opened[1].Data["user_id"])
	require.Equal(t, env.owner.ID, opened[1].Data["owner_id"])
	env.waitAudit(t, "shell terminal closed", 2)
	env.mu.Lock()
	require.Contains(t, strings.Join(env.taskLogs, "\n"), env.admin.Username+" (admin)")
	env.mu.Unlock()

	// Neither the logs nor the frames contain the key.
	for _, entry := range env.logHook.AllEntries() {
		line, err := entry.String()
		require.NoError(t, err)
		require.NotContains(t, line, "PRIVATE KEY")
	}
	frames.mu.Lock()
	for _, f := range frames.all {
		require.NotContains(t, f, "PRIVATE KEY")
		require.NotContains(t, f, strings.Split(env.keyPEM, "\n")[1])
	}
	frames.mu.Unlock()
}

func TestShellTerminalAdminSwitch(t *testing.T) {
	env := setupShellTerminalTest(t, shelltermtest.Echo, func(c *config.ShellTerminalConfig) {
		c.AllowAdmin = false
	})
	env.refused(t, &env.admin, env.shellID, nil, http.StatusNotFound)
	require.Zero(t, env.dials.Load())

	ws := env.open(t, &env.owner, env.shellID, "", nil)
	readTermFrames(ws).waitControl(t, "ready")
}

// Workspace permissions, such as RBAC's UPDATE_NSC behind CanControlGenericTask, never open
// another user's shell; only CanGetNSC is consulted, and it can only take access away.
func TestShellTerminalIgnoresWorkspacePermissions(t *testing.T) {
	env := setupShellTerminalTest(t, shelltermtest.Echo, nil)
	authz := &mocks.NSCAuthZ{}
	command.AuthZProvider.RegisterOverride("mock", authz)
	config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{Type: "mock"}
	t.Cleanup(func() { config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{Type: "basic"} })

	authz.On("CanControlGenericTask", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Maybe()
	authz.On("CanGetNSC", mock.Anything, mock.MatchedBy(func(u model.User) bool {
		return u.ID != env.owner.ID
	}), mock.Anything).Return(nil)
	authz.On("CanGetNSC", mock.Anything, mock.MatchedBy(func(u model.User) bool {
		return u.ID == env.owner.ID
	}), mock.Anything).Return(authz2.PermissionDeniedError{})

	other := env.refused(t, &env.other, env.shellID, nil, http.StatusNotFound)
	// The owner, who may no longer see the shell's workspace, is refused, too.
	require.Equal(t, other, env.refused(t, &env.owner, env.shellID, nil, http.StatusNotFound))
	require.JSONEq(t, `{"message":"Not Found"}`, other)
	require.Zero(t, env.dials.Load())
	authz.AssertNotCalled(t, "CanControlGenericTask", mock.Anything, mock.Anything, mock.Anything,
		mock.Anything)
}

func TestShellTerminalNotReady(t *testing.T) {
	env := setupShellTerminalTest(t, shelltermtest.Echo, nil)
	env.ready.Store(false)

	ws := env.open(t, &env.owner, env.shellID, "", nil)
	frames := readTermFrames(ws)
	msg := frames.waitControl(t, "error")
	require.Equal(t, shellterm.ErrCodeNotReady, msg.Code)
	require.Equal(t, shellterm.CloseNotReady, frames.closeCode(t))
	require.Zero(t, env.dials.Load())
}

func TestShellTerminalRecheck(t *testing.T) {
	interval := func(c *config.ShellTerminalConfig) {
		c.RecheckInterval = model.Duration(200 * time.Millisecond)
	}
	open := func(t *testing.T, env *shellTerminalEnv, u model.User) *termFrames {
		ws := env.open(t, &u, env.shellID, "", nil)
		frames := readTermFrames(ws)
		frames.waitControl(t, "ready")
		return frames
	}

	t.Run("logout", func(t *testing.T) {
		env := setupShellTerminalTest(t, shelltermtest.Silent, interval)
		frames := open(t, env, env.owner)
		require.NoError(t, user.DeleteSessionByToken(context.TODO(), env.token(t, env.owner)))
		require.Equal(t, shellterm.CloseUnauthenticated, frames.closeCode(t))
		require.Equal(t, shellterm.ErrCodeUnauthenticate, frames.waitControl(t, "error").Code)
	})

	t.Run("deactivated", func(t *testing.T) {
		env := setupShellTerminalTest(t, shelltermtest.Silent, interval)
		frames := open(t, env, env.owner)
		_, err := db.Bun().NewUpdate().Table("users").Set("active = false").
			Where("id = ?", env.owner.ID).Exec(context.TODO())
		require.NoError(t, err)
		require.Equal(t, shellterm.CloseUnauthenticated, frames.closeCode(t))
	})

	t.Run("admin demoted", func(t *testing.T) {
		env := setupShellTerminalTest(t, shelltermtest.Silent, interval)
		frames := open(t, env, env.admin)
		_, err := db.Bun().NewUpdate().Table("users").Set("admin = false").
			Where("id = ?", env.admin.ID).Exec(context.TODO())
		require.NoError(t, err)
		require.Equal(t, shellterm.CloseForbidden, frames.closeCode(t))
		require.Equal(t, shellterm.ErrCodeForbidden, frames.waitControl(t, "error").Code)
	})

	t.Run("still valid", func(t *testing.T) {
		env := setupShellTerminalTest(t, shelltermtest.Silent, interval)
		frames := open(t, env, env.owner)
		time.Sleep(time.Second)
		select {
		case <-frames.done:
			t.Fatalf("a valid terminal was closed: %v", frames.err)
		default:
		}
	})
}

func TestShellTerminalLimits(t *testing.T) {
	env := setupShellTerminalTest(t, shelltermtest.Silent, func(c *config.ShellTerminalConfig) {
		c.MaxSessionsPerUser = 1
		c.MaxSessions = 2
	})
	first := env.open(t, &env.owner, env.shellID, "", nil)
	readTermFrames(first).waitControl(t, "ready")

	env.refused(t, &env.owner, env.shellID, nil, http.StatusTooManyRequests)

	second := env.open(t, &env.admin, env.shellID, "", nil)
	readTermFrames(second).waitControl(t, "ready")
	third := db.RequireMockUser(t, env.api.m.db)
	_, err := db.Bun().NewUpdate().Table("users").Set("admin = true").Where("id = ?", third.ID).
		Exec(context.TODO())
	require.NoError(t, err)
	third.Admin = true
	env.refused(t, &third, env.shellID, nil, http.StatusTooManyRequests)

	// Closing a terminal frees its slot.
	require.NoError(t, first.Close())
	require.Eventually(t, func() bool {
		total, _ := env.svc.limiter.Active(int(env.owner.ID))
		return total == 1
	}, 10*time.Second, 10*time.Millisecond)
	again := env.open(t, &env.owner, env.shellID, "", nil)
	readTermFrames(again).waitControl(t, "ready")
}

func TestShellTerminalAuditClientIP(t *testing.T) {
	spoofed := http.Header{"X-Forwarded-For": {"203.0.113.66"}, "X-Real-IP": {"203.0.113.66"}}

	env := setupShellTerminalTest(t, shelltermtest.Silent, nil)
	ws := env.open(t, &env.owner, env.shellID, "", spoofed.Clone())
	readTermFrames(ws).waitControl(t, "ready")
	entry := env.waitAudit(t, "shell terminal opened", 1)[0]
	require.Equal(t, "127.0.0.1", entry.Data["client_ip"])
	require.Equal(t, "127.0.0.1", entry.Data["remote_addr"])

	// Behind a trusted proxy, its X-Real-IP is the client address.
	env = setupShellTerminalTest(t, shelltermtest.Silent, func(c *config.ShellTerminalConfig) {
		c.TrustedProxies = []string{"127.0.0.1"}
	})
	ws = env.open(t, &env.owner, env.shellID, "", spoofed.Clone())
	readTermFrames(ws).waitControl(t, "ready")
	entry = env.waitAudit(t, "shell terminal opened", 1)[0]
	require.Equal(t, "203.0.113.66", entry.Data["client_ip"])
	require.Equal(t, "127.0.0.1", entry.Data["remote_addr"])
}

func TestShellTerminalLangValidation(t *testing.T) {
	env := setupShellTerminalTest(t, shelltermtest.Silent, nil)
	ws := env.open(t, &env.owner, env.shellID, "lang=en_US.UTF-8%0ALD_PRELOAD%3D%2Ftmp%2Fx", nil)
	readTermFrames(ws).waitControl(t, "ready")
	require.Equal(t, map[string]string{"LANG": shellterm.DefaultLang}, env.sshd.Record().Env)
}

func TestShellTerminalTargetSnapshot(t *testing.T) {
	env := setupShellTerminalTest(t, shelltermtest.Silent, nil)
	target, err := command.DefaultCmdService.ShellTerminalTarget(env.shellID)
	require.NoError(t, err)
	require.Equal(t, env.owner.ID, target.OwnerID)
	require.Equal(t, model.AccessScopeID(model.DefaultWorkspaceID), target.WorkspaceID)
	require.Equal(t, shellTerminalAgentUser, target.LoginUser)
	require.Equal(t, env.keyPEM, string(target.PrivateKey))

	_, err = command.DefaultCmdService.ShellTerminalTarget(env.notebookID)
	require.ErrorIs(t, err, command.ErrShellTerminalNotFound)
	_, err = command.DefaultCmdService.ShellTerminalTarget("no-such-task")
	require.ErrorIs(t, err, command.ErrShellTerminalNotFound)

	// Without an agent user group, the terminal logs in as root, like `det shell open`.
	req := mockGenericReq(t, env.api.m.db)
	req.Spec.Metadata.PrivateKey = ptrs.Ptr(env.keyPEM)
	shell, err := command.DefaultCmdService.LaunchGenericCommand(model.TaskTypeShell, model.JobTypeShell, req)
	require.NoError(t, err)
	target, err = command.DefaultCmdService.ShellTerminalTarget(model.TaskID(shell.ToV1Shell().Id))
	require.NoError(t, err)
	require.Equal(t, "root", target.LoginUser)
}
