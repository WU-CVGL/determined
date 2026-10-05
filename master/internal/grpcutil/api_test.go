package grpcutil

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	// TODO switch to google.golang.org/protobuf/proto/.
	"github.com/golang/protobuf/proto" //nolint:staticcheck
	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// testGateway serves fake login and logout calls through the real gateway mux and its response
// hooks, and a call that echoes the Authorization header that the gRPC server would see.
func testGateway(t *testing.T, externalSessions bool) *echo.Echo {
	mux := newGRPCGatewayMux()
	respond := func(resp proto.Message) runtime.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request, _ map[string]string) {
			_, outbound := runtime.MarshalerForRequest(mux, req)
			runtime.ForwardResponseMessage(
				req.Context(), mux, outbound, w, req, resp, mux.GetForwardResponseOptions()...)
		}
	}
	pattern := func(names ...string) runtime.Pattern {
		var ops []int
		for i := range append([]string{"api", "v1"}, names...) {
			ops = append(ops, 2, i) // OpLitPush
		}
		return runtime.MustPattern(runtime.NewPattern(1, ops, append([]string{"api", "v1"}, names...), ""))
	}
	mux.Handle(http.MethodPost, pattern("auth", "login"),
		respond(&apiv1.LoginResponse{Token: "new-token"}))
	mux.Handle(http.MethodPost, pattern("auth", "logout"), respond(&apiv1.LogoutResponse{}))
	mux.Handle(http.MethodGet, pattern("whoami"),
		func(w http.ResponseWriter, req *http.Request, _ map[string]string) {
			w.Header().Set("X-Seen-Authorization", req.Header.Get("Authorization"))
		})

	e := echo.New()
	e.Use(user.ClearSessionCookieOnLogout)
	e.Any("/api/v1/*", gatewayHandler(mux, externalSessions))
	return e
}

// gatewayResponse is what the tests need from a recorded response.
type gatewayResponse struct {
	StatusCode int
	Header     http.Header
	cookies    []*http.Cookie
}

func (r gatewayResponse) Cookies() []*http.Cookie { return r.cookies }

func sendToGateway(e *echo.Echo, method, url string, header http.Header) gatewayResponse {
	req := httptest.NewRequest(method, url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()
	return gatewayResponse{StatusCode: resp.StatusCode, Header: resp.Header, cookies: resp.Cookies()}
}

func TestGatewaySessionCookie(t *testing.T) {
	e := testGateway(t, false)

	for _, base := range []string{"http://gpu.example", "https://gpu.example"} {
		secure := base == "https://gpu.example"

		resp := sendToGateway(e, http.MethodPost, base+"/api/v1/auth/login", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		cookies := resp.Cookies()
		require.Len(t, cookies, 1)
		c := cookies[0]
		require.Equal(t, "auth", c.Name)
		require.Equal(t, "new-token", c.Value)
		require.Equal(t, "/", c.Path)
		require.True(t, c.HttpOnly)
		require.Equal(t, http.SameSiteLaxMode, c.SameSite)
		require.Equal(t, secure, c.Secure, base)
		require.Positive(t, c.MaxAge)

		// Logout removes the same cookie: same name and path, so the browser replaces it.
		// user.ClearSessionCookieOnLogout does this; the login hook must not add a second one.
		resp = sendToGateway(e, http.MethodPost, base+"/api/v1/auth/logout", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		cookies = resp.Cookies()
		require.Len(t, cookies, 1)
		c = cookies[0]
		require.Equal(t, "auth", c.Name)
		require.Empty(t, c.Value)
		require.Equal(t, "/", c.Path)
		require.Negative(t, c.MaxAge)
		require.True(t, c.HttpOnly)
		require.Equal(t, secure, c.Secure, base)
	}
}

func TestGatewayCookieAuthentication(t *testing.T) {
	e := testGateway(t, false)

	resp := sendToGateway(e, http.MethodGet, "http://gpu.example/api/v1/whoami",
		http.Header{"Cookie": {"auth=cookie-token"}})
	require.Equal(t, "Bearer cookie-token", resp.Header.Get("X-Seen-Authorization"))

	// An Authorization header wins over the cookie.
	resp = sendToGateway(e, http.MethodGet, "http://gpu.example/api/v1/whoami", http.Header{
		"Cookie": {"auth=cookie-token"}, "Authorization": {"Bearer header-token"},
	})
	require.Equal(t, "Bearer header-token", resp.Header.Get("X-Seen-Authorization"))

	// Other responses do not touch the cookie.
	require.Empty(t, resp.Cookies())
}

func TestGatewayExternalSessionCookie(t *testing.T) {
	e := testGateway(t, true)

	resp := sendToGateway(e, http.MethodGet, "http://gpu.example/api/v1/whoami",
		http.Header{"Cookie": {"det_jwt=jwt-token; auth=cookie-token"}})
	require.Equal(t, "Bearer jwt-token", resp.Header.Get("X-Seen-Authorization"))

	// Neither cookie replaces a header that the client sent. The cross-origin check exempts
	// requests with a bearer token, and lets through any other Authorization header only after
	// checking the origin, so a cookie must not authenticate them.
	for _, header := range []string{"Bearer header-token", "Basic dXNlcjpwYXNz"} {
		resp = sendToGateway(e, http.MethodGet, "http://gpu.example/api/v1/whoami", http.Header{
			"Cookie": {"det_jwt=jwt-token; auth=cookie-token"}, "Authorization": {header},
		})
		require.Equal(t, header, resp.Header.Get("X-Seen-Authorization"))
	}
}

// TestGatewayLegacySetUserPasswordBody checks the bodies that the gateway decodes for
// SetUserPassword: the new password alone as a JSON string, which clients built before 0.41.0
// send, becomes the request message, and every other body passes byte for byte.
func TestGatewayLegacySetUserPasswordBody(t *testing.T) {
	mux := newGRPCGatewayMux()
	seenBody := func(w http.ResponseWriter, req *http.Request, _ map[string]string) {
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		w.Header().Set("X-Seen-Content-Length", strconv.FormatInt(req.ContentLength, 10))
		_, err = w.Write(body)
		require.NoError(t, err)
	}
	// The pattern of the generated route POST /api/v1/users/{user_id}/<last>.
	userPattern := func(last string) runtime.Pattern {
		return runtime.MustPattern(runtime.NewPattern(1,
			[]int{2, 0, 2, 1, 2, 2, 1, 0, 4, 1, 5, 3, 2, 4},
			[]string{"api", "v1", "users", "user_id", last}, ""))
	}
	mux.Handle(http.MethodPost, userPattern("password"), seenBody)
	mux.Handle(http.MethodPut, userPattern("password"), seenBody)
	mux.Handle(http.MethodPost, userPattern("passwords"), seenBody)
	e := echo.New()
	e.Any("/api/v1/*", gatewayHandler(mux, false))

	send := func(method, path, body string, contentLength int64) (string, int64) {
		req := httptest.NewRequest(method, "http://gpu.example"+path, strings.NewReader(body))
		req.ContentLength = contentLength
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		seen, err := strconv.ParseInt(rec.Header().Get("X-Seen-Content-Length"), 10, 64)
		require.NoError(t, err)
		return rec.Body.String(), seen
	}
	const path = "/api/v1/users/7/password"

	for _, legacy := range []struct{ body, want string }{
		{`"New-password-1"`, `{"password":"New-password-1"}`},
		{" \"a\\\"b\\u00e9 c\"\n", `{"password":"a\"bé c"}`},
		{`""`, `{"password":""}`},
	} {
		// A chunked body has no length; the rewritten one has.
		for _, length := range []int64{int64(len(legacy.body)), -1} {
			seen, seenLength := send(http.MethodPost, path, legacy.body, length)
			require.Equal(t, legacy.want, seen, legacy.body)
			require.Equal(t, int64(len(legacy.want)), seenLength, legacy.body)
		}
	}

	unchanged := []string{
		`{"password":"New-password-1","old_password":"Old-password-1"}`,
		` {"password": "New-password-1"} `,
		``,
		`null`,
		`12`,
		`"unterminated`,
		`"two" "strings"`,
		// A JSON string longer than the shim reads is not recognized.
		`"` + strings.Repeat("x", maxLegacySetUserPasswordBody) + `"`,
	}
	for _, body := range unchanged {
		for _, length := range []int64{int64(len(body)), -1} {
			seen, seenLength := send(http.MethodPost, path, body, length)
			require.Equal(t, body, seen)
			require.Equal(t, length, seenLength)
		}
	}

	// Other routes and methods are not touched.
	for _, r := range []struct{ method, path string }{
		{http.MethodPut, path},
		{http.MethodPost, "/api/v1/users/7/passwords"},
	} {
		seen, _ := send(r.method, r.path, `"New-password-1"`, int64(len(`"New-password-1"`)))
		require.Equal(t, `"New-password-1"`, seen, r)
	}
}
