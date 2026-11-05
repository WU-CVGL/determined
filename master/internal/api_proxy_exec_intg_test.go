//go:build integration
// +build integration

package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apiPkg "github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/proxy"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
)

// TestProxyExecPortOwnerOrAdmin checks that a port whose service runs code as the task's owner is
// reached only by the owner, with a session or the task's notebook token, and by admins, whose
// first request in each session is logged, while every other port keeps the visibility rule.
func TestProxyExecPortOwnerOrAdmin(t *testing.T) {
	api, authz, admin, _ := setupNTSCAuthzTest(t)
	// Everyone can see every task, as under basic authz, so only the exec rule refuses anyone.
	authz.On("CanGetNSC", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	user.InitService(api.m.db, &model.ExternalSessions{})
	if proxy.DefaultProxy == nil {
		proxy.InitProxy(processProxyAuthentication, user.IsMasterSignedToken)
	}
	audits := credentialReadLogs(t)

	ctx := context.TODO()
	owner, _, taskID := launchOwnedCommand(t, api)
	other := db.RequireMockUser(t, api.m.db)
	_, _, otherTaskID := launchOwnedCommand(t, api)
	sessionOf := func(u model.User) string {
		token, err := user.StartSession(ctx, &u)
		require.NoError(t, err)
		return token
	}
	ownerToken, otherToken := sessionOf(owner), sessionOf(other)
	adminToken, secondAdminToken := sessionOf(admin), sessionOf(admin)
	notebookToken, err := db.GenerateNotebookSessionToken(owner.ID, model.TaskID(taskID))
	require.NoError(t, err)
	otherTaskNotebookToken, err := db.GenerateNotebookSessionToken(
		owner.ID, model.TaskID(otherTaskID))
	require.NoError(t, err)

	reached := make(chan string, 1)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.URL.Path
	}))
	defer service.Close()
	serviceURL, err := url.Parse(service.URL)
	require.NoError(t, err)
	exec, plain := taskID+":exec", taskID+":plain"
	proxy.DefaultProxy.RegisterService(exec, proxy.Service{URL: serviceURL, Exec: true})
	proxy.DefaultProxy.RegisterService(plain, proxy.Service{URL: serviceURL})
	defer proxy.DefaultProxy.Unregister(exec)
	defer proxy.DefaultProxy.Unregister(plain)

	e := echo.New()
	e.HTTPErrorHandler = apiPkg.JSONErrorHandler
	e.Any("/proxy/:service/*", proxy.DefaultProxy.NewProxyHandler("service"))
	master := httptest.NewServer(e)
	defer master.Close()
	client := cleanhttp.DefaultClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	send := func(serviceID, query string, header http.Header) (int, bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			master.URL+"/proxy/"+serviceID+"/lab"+query, nil)
		require.NoError(t, err)
		req.Header = header
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		select {
		case <-reached:
			return resp.StatusCode, true
		default:
			return resp.StatusCode, false
		}
	}
	bearer := func(token string) http.Header {
		return http.Header{"Authorization": {"Bearer " + token}}
	}
	requireReached := func(name, serviceID, query string, header http.Header) {
		t.Helper()
		code, ok := send(serviceID, query, header)
		require.Equal(t, http.StatusOK, code, name)
		require.True(t, ok, name)
	}
	requireNotFound := func(name, serviceID, query string, header http.Header) {
		t.Helper()
		code, ok := send(serviceID, query, header)
		require.Equal(t, http.StatusNotFound, code, name)
		require.False(t, ok, name)
	}

	// The owner reaches both ports, with a session or the task's notebook token.
	requireReached("owner", exec, "", bearer(ownerToken))
	requireReached("owner, cookie", exec, "", http.Header{
		"Cookie": {user.SessionCookieName + "=" + ownerToken}, "Sec-Fetch-Site": {"same-origin"},
	})
	requireReached("notebook token", exec, "?token="+notebookToken, nil)
	requireReached("notebook token header", exec, "", http.Header{
		"Authorization": {"token " + notebookToken},
	})
	requireReached("owner, plain port", plain, "", bearer(ownerToken))
	code, ok := send(exec, "?token="+otherTaskNotebookToken, nil)
	require.NotEqual(t, http.StatusOK, code)
	require.False(t, ok)

	// Another user who can see the task reaches its other ports only; the exec port looks
	// missing to them, WebSocket handshakes included, as it does for a task they cannot see.
	requireReached("other user, plain port", plain, "", bearer(otherToken))
	requireNotFound("other user", exec, "", bearer(otherToken))
	upgrade := bearer(otherToken)
	upgrade.Set("Connection", "Upgrade")
	upgrade.Set("Upgrade", "websocket")
	upgrade.Set("Sec-WebSocket-Version", "13")
	upgrade.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	requireNotFound("other user, WebSocket", exec, "", upgrade)
	require.Empty(t, audits())

	// Admins reach it; the first request of each admin session to the task is logged.
	requireReached("admin", exec, "", bearer(adminToken))
	requireReached("admin, again", exec, "", bearer(adminToken))
	requireReached("admin, plain port", plain, "", bearer(adminToken))
	requireCredentialReadLog(t, audits(), admin, "exec port", taskID, int32(owner.ID))
	requireReached("admin, second session", exec, "", bearer(secondAdminToken))
	require.Len(t, audits(), 2)

	// When the command service does not run the task, the owner comes from its persisted state.
	runningService := command.DefaultCmdService
	command.SetDefaultService(&command.CommandService{})
	t.Cleanup(func() { command.SetDefaultService(runningService) })
	requireReached("owner, persisted", exec, "", bearer(ownerToken))
	requireNotFound("other user, persisted", exec, "", bearer(otherToken))
	requireReached("admin, persisted", exec, "", bearer(adminToken))
	require.Len(t, audits(), 2)

	// An exec port of a task the master does not know is refused.
	unknown := uuid.NewString() + ":exec"
	proxy.DefaultProxy.RegisterService(unknown, proxy.Service{URL: serviceURL, Exec: true})
	defer proxy.DefaultProxy.Unregister(unknown)
	code, ok = send(unknown, "", bearer(adminToken))
	require.NotEqual(t, http.StatusOK, code)
	require.False(t, ok)
}

// A service with a token, as the tensorboard service, gets it from the master's proxy on the
// owner's request, in place of one that the visitor sent; another user's request never reaches
// the service.
func TestProxyServiceTokenOnlyAfterAccessCheck(t *testing.T) {
	api, authz, _, _ := setupNTSCAuthzTest(t)
	authz.On("CanGetNSC", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	user.InitService(api.m.db, &model.ExternalSessions{})
	if proxy.DefaultProxy == nil {
		proxy.InitProxy(processProxyAuthentication, user.IsMasterSignedToken)
	}

	ctx := context.TODO()
	owner, _, taskID := launchOwnedCommand(t, api)
	other := db.RequireMockUser(t, api.m.db)
	sessionOf := func(u model.User) string {
		token, err := user.StartSession(ctx, &u)
		require.NoError(t, err)
		return token
	}
	ownerToken, otherToken := sessionOf(owner), sessionOf(other)

	reached := make(chan http.Header, 1)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.Header.Clone()
	}))
	defer service.Close()
	serviceURL, err := url.Parse(service.URL)
	require.NoError(t, err)
	tensorBoard := taskID + ":tensorboard"
	proxy.DefaultProxy.RegisterService(tensorBoard, proxy.Service{
		URL: serviceURL, Exec: true, ServiceToken: "tb-token",
	})
	defer proxy.DefaultProxy.Unregister(tensorBoard)

	e := echo.New()
	e.HTTPErrorHandler = apiPkg.JSONErrorHandler
	e.Any("/proxy/:service/*", proxy.DefaultProxy.NewProxyHandler("service"))
	master := httptest.NewServer(e)
	defer master.Close()
	send := func(session string) (int, http.Header) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			master.URL+"/proxy/"+tensorBoard+"/data", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+session)
		req.Header.Set(proxy.ServiceTokenHeader, "forged")
		resp, err := cleanhttp.DefaultClient().Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		select {
		case header := <-reached:
			return resp.StatusCode, header
		default:
			return resp.StatusCode, nil
		}
	}

	code, header := send(ownerToken)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{"tb-token"}, header.Values(proxy.ServiceTokenHeader))
	require.Empty(t, header.Values("Authorization"))

	code, header = send(otherToken)
	require.Equal(t, http.StatusNotFound, code)
	require.Nil(t, header)
}
