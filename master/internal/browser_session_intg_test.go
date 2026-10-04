//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/api"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/logger"
	"github.com/determined-ai/determined/master/pkg/model"
)

// browserSessionServer serves the master's HTTP API the way Master.Run does: the gRPC server
// behind the gateway, the legacy user routes, and the middleware from useAuthenticationMiddleware.
func browserSessionServer(t *testing.T, apiSrv *apiServer) *httptest.Server {
	user.InitService(apiSrv.m.db, &apiSrv.m.config.InternalConfig.ExternalSessions)

	grpcS := grpcutil.NewGRPCServer(apiSrv.m.db, apiSrv, false,
		&apiSrv.m.config.InternalConfig.ExternalSessions, logger.NewLogBuffer(100))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = grpcS.Serve(lis) }()
	t.Cleanup(grpcS.Stop)

	e := echo.New()
	e.HTTPErrorHandler = api.JSONErrorHandler
	e.Use(func(h echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error { return h(&detContext.DetContext{Context: c}) }
	})
	useAuthenticationMiddleware(e, nil)
	user.RegisterAPIHandler(e, user.GetService())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, grpcutil.RegisterHTTPProxy(ctx, e, lis.Addr().(*net.TCPAddr).Port, nil))

	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

// sameOrigin is the Sec-Fetch-Site header of the master's own pages.
const sameOrigin = "same-origin"

const jsonContentType = "application/json"

type browserRequest struct {
	method, path, contentType, body string
	cookie, authorization           string
	// headers that a browser adds: Origin and Sec-Fetch-Site.
	origin, fetchSite string
}

// browserResponse is what a test needs from a response; its body has been read and closed.
type browserResponse struct {
	StatusCode int
	cookies    []*http.Cookie
}

func (r browserResponse) Cookies() []*http.Cookie { return r.cookies }

func (r browserRequest) send(t *testing.T, srv *httptest.Server) (browserResponse, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(
		context.Background(), r.method, srv.URL+r.path, strings.NewReader(r.body))
	require.NoError(t, err)
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	if r.cookie != "" {
		req.AddCookie(&http.Cookie{Name: user.SessionCookieName, Value: r.cookie})
	}
	if r.authorization != "" {
		req.Header.Set("Authorization", r.authorization)
	}
	if r.origin != "" {
		req.Header.Set("Origin", r.origin)
	}
	if r.fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", r.fetchSite)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return browserResponse{StatusCode: resp.StatusCode, cookies: resp.Cookies()}, string(body)
}

func sessionCookieOf(t *testing.T, resp browserResponse) *http.Cookie {
	t.Helper()
	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == user.SessionCookieName {
			require.Nil(t, found, "the response sets the session cookie twice")
			found = c
		}
	}
	require.NotNil(t, found, "the response does not set the session cookie")
	return found
}

func TestBrowserSessionThroughGateway(t *testing.T) {
	apiSrv, _, _ := setupAPITest(t, nil)
	srv := browserSessionServer(t, apiSrv)
	const password = "Browser-password-1"
	u := addPasswordUser(t, password)
	self := srv.URL // The master's own origin.
	const evil = "https://evil.test"

	login := func(origin, fetchSite string) (browserResponse, string) {
		return browserRequest{
			method: http.MethodPost, path: "/api/v1/auth/login", contentType: jsonContentType,
			body:   fmt.Sprintf(`{"username": %q, "password": %q}`, u.Username, password),
			origin: origin, fetchSite: fetchSite,
		}.send(t, srv)
	}

	// Another site cannot sign a visitor in to an account of its choosing.
	resp, _ := login(evil, "cross-site")
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Empty(t, resp.Cookies())

	// The web UI signs in and receives an HttpOnly cookie, which authenticates its requests.
	resp, body := login(self, sameOrigin)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	c := sessionCookieOf(t, resp)
	require.NotEmpty(t, c.Value)
	require.True(t, c.HttpOnly)
	require.Equal(t, http.SameSiteLaxMode, c.SameSite)
	require.Equal(t, "/", c.Path)
	require.False(t, c.Secure, "plain HTTP without a trusted proxy")
	token := c.Value

	resp, body = browserRequest{method: http.MethodGet, path: "/api/v1/me", cookie: token}.send(t, srv)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, u.Username)

	// A cross-site form can post text/plain that the gateway parses as JSON. With the cookie it
	// would change the visitor's password; the master refuses it whether the browser sends
	// Sec-Fetch-Site or, like older Safari and Firefox over plain HTTP, only Origin.
	const newPassword = "Browser-password-2"
	changePassword := browserRequest{
		method: http.MethodPost, path: fmt.Sprintf("/api/v1/users/%d/password", u.ID),
		contentType: "text/plain", cookie: token,
		body: fmt.Sprintf(`{"password": %q, "old_password": %q}`, newPassword, password),
	}
	for _, headers := range [][2]string{{evil, "cross-site"}, {evil, ""}, {"null", ""}, {"", "same-site"}} {
		r := changePassword
		r.origin, r.fetchSite = headers[0], headers[1]
		resp, body = r.send(t, srv)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "%v: %s", headers, body)
		require.Contains(t, body, "cross-origin request refused")
	}
	requireLogin(t, apiSrv, u.Username, password, true)

	// A request with a bearer token is exempt; the browser cannot send one from another site
	// without a CORS preflight. A cookie does not exempt a request that also carries Basic
	// credentials, which browsers can add to a cross-site post themselves.
	r := changePassword
	r.origin, r.fetchSite, r.authorization = evil, "cross-site", "Basic dXNlcjpwYXNz"
	resp, _ = r.send(t, srv)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	other, err := user.StartSession(context.Background(), &u)
	require.NoError(t, err)
	resp, body = browserRequest{
		method: http.MethodPost, path: "/api/v1/auth/logout", authorization: "Bearer " + other,
		origin: evil, fetchSite: "cross-site",
	}.send(t, srv)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	// The body is the whole request, {"password", "old_password"}; a bare JSON string, the
	// format before the current password was required, is refused.
	r = changePassword
	r.origin, r.fetchSite, r.contentType, r.body = self, sameOrigin, jsonContentType, `"Bare-1234"`
	resp, _ = r.send(t, srv)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// The web UI's own request passes, with the cookie and the current password.
	r = changePassword
	r.origin, r.fetchSite, r.contentType = self, sameOrigin, jsonContentType
	resp, body = r.send(t, srv)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	requireLogin(t, apiSrv, u.Username, newPassword, true)

	// The password change ended the session. Signing out still removes the browser's cookie,
	// although the request no longer authenticates.
	resp, _ = browserRequest{method: http.MethodGet, path: "/api/v1/me", cookie: token}.send(t, srv)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	for _, path := range []string{"/api/v1/auth/logout", "/logout"} {
		resp, _ = browserRequest{
			method: http.MethodPost, path: path, cookie: token, origin: self, fetchSite: sameOrigin,
		}.send(t, srv)
		// The gateway answers 401; the legacy route fails while looking up the session.
		require.GreaterOrEqual(t, resp.StatusCode, http.StatusBadRequest, path)
		c := sessionCookieOf(t, resp)
		require.Empty(t, c.Value, path)
		require.Equal(t, "/", c.Path, path)
		require.Negative(t, c.MaxAge, path)
		require.True(t, c.HttpOnly, path)
	}
}

func TestBrowserSessionLogoutAndSessionCookie(t *testing.T) {
	apiSrv, _, _ := setupAPITest(t, nil)
	srv := browserSessionServer(t, apiSrv)
	const password = "Browser-password-1"
	u := addPasswordUser(t, password)
	self := srv.URL

	newToken := func() string {
		token, err := user.StartSession(context.Background(), &u)
		require.NoError(t, err)
		return token
	}

	// POST /auth/session-cookie stores a valid bearer token in the cookie, for ?jwt= sign-in,
	// but only for the master's own pages.
	token := newToken()
	resp, body := browserRequest{
		method: http.MethodPost, path: "/auth/session-cookie", authorization: "Bearer " + token,
		origin: "https://evil.test", fetchSite: "cross-site",
	}.send(t, srv)
	require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
	require.Empty(t, resp.Cookies())
	resp, _ = browserRequest{
		method: http.MethodPost, path: "/auth/session-cookie", authorization: "Bearer not-a-token",
		origin: self, fetchSite: sameOrigin,
	}.send(t, srv)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Empty(t, resp.Cookies())
	resp, body = browserRequest{
		method: http.MethodPost, path: "/auth/session-cookie", authorization: "Bearer " + token,
		origin: self, fetchSite: sameOrigin,
	}.send(t, srv)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	c := sessionCookieOf(t, resp)
	require.Equal(t, token, c.Value)
	require.True(t, c.HttpOnly)

	// Signing out through the gateway ends the session and removes the cookie, once.
	resp, body = browserRequest{
		method: http.MethodPost, path: "/api/v1/auth/logout", cookie: token,
		origin: self, fetchSite: sameOrigin,
	}.send(t, srv)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Empty(t, sessionCookieOf(t, resp).Value)
	resp, _ = browserRequest{method: http.MethodGet, path: "/api/v1/me", cookie: token}.send(t, srv)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Signing out with a session that has already ended fails, and still removes the cookie,
	// which the web UI cannot remove itself.
	for _, path := range []string{"/api/v1/auth/logout", "/logout"} {
		resp, _ = browserRequest{
			method: http.MethodPost, path: path, cookie: token, origin: self, fetchSite: sameOrigin,
		}.send(t, srv)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, path)
		c := sessionCookieOf(t, resp)
		require.Empty(t, c.Value, path)
		require.Equal(t, "/", c.Path, path)
		require.Negative(t, c.MaxAge, path)
	}

	// Another site cannot sign the visitor out.
	token = newToken()
	resp, _ = browserRequest{
		method: http.MethodPost, path: "/api/v1/auth/logout", cookie: token,
		origin: "https://evil.test", fetchSite: "cross-site",
	}.send(t, srv)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Empty(t, resp.Cookies())
	resp, _ = browserRequest{method: http.MethodGet, path: "/api/v1/me", cookie: token}.send(t, srv)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The legacy route signs out too.
	resp, body = browserRequest{
		method: http.MethodPost, path: "/logout", cookie: token, origin: self, fetchSite: sameOrigin,
	}.send(t, srv)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Empty(t, sessionCookieOf(t, resp).Value)
	resp, _ = browserRequest{method: http.MethodGet, path: "/api/v1/me", cookie: token}.send(t, srv)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestProxyAuthenticationChecksOrigin(t *testing.T) {
	apiSrv, _, _ := setupAPITest(t, nil)
	user.InitService(apiSrv.m.db, &model.ExternalSessions{})

	authenticate := func(method string, headers map[string]string) error {
		req := httptest.NewRequest(method, "http://gpu.example/proxy/abc/api/kernels", nil)
		req.AddCookie(&http.Cookie{Name: user.SessionCookieName, Value: "not-a-session"})
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		c := echo.New().NewContext(req, httptest.NewRecorder())
		c.SetParamNames("service")
		c.SetParamValues("abc")
		_, err := processProxyAuthentication(&detContext.DetContext{Context: c})
		return err
	}
	crossSite := map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.test"}
	webSocket := func(h map[string]string) map[string]string {
		out := map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}
		for k, v := range h {
			out[k] = v
		}
		return out
	}

	// Requests that the session cookie authenticates must come from the master's origin,
	// WebSocket handshakes included.
	require.ErrorIs(t, authenticate(http.MethodPost, crossSite), user.ErrCrossOriginRequest)
	require.ErrorIs(t, authenticate(http.MethodGet, webSocket(crossSite)), user.ErrCrossOriginRequest)
	require.ErrorIs(t, authenticate(http.MethodGet, webSocket(map[string]string{
		"Sec-Fetch-Site": "same-site", "Origin": "https://other.gpu.example",
	})), user.ErrCrossOriginRequest)

	// Other requests go on to authenticate the cookie, which names no session here.
	for _, req := range []struct {
		method  string
		headers map[string]string
	}{
		{http.MethodGet, crossSite},
		{http.MethodPost, map[string]string{"Sec-Fetch-Site": sameOrigin}},
		{http.MethodGet, webSocket(map[string]string{"Sec-Fetch-Site": sameOrigin})},
		{http.MethodGet, webSocket(nil)},
	} {
		err := authenticate(req.method, req.headers)
		require.NotErrorIs(t, err, user.ErrCrossOriginRequest, "%s %v", req.method, req.headers)
	}
}
