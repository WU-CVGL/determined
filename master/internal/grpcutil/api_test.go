package grpcutil

import (
	"net/http"
	"net/http/httptest"
	"testing"

	// TODO switch to google.golang.org/protobuf/proto/.
	"github.com/golang/protobuf/proto" //nolint: staticcheck
	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// testGateway serves fake login and logout calls through the real gateway mux and its response
// hooks, and a call that echoes the Authorization header that the gRPC server would see.
func testGateway(t *testing.T) *echo.Echo {
	mux := newGRPCGatewayMux()
	respond := func(resp proto.Message) runtime.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request, _ map[string]string) {
			_, outbound := runtime.MarshalerForRequest(mux, req)
			runtime.ForwardResponseMessage(
				req.Context(), mux, outbound, w, req, resp, mux.GetForwardResponseOptions()...)
		}
	}
	pattern := func(name string) runtime.Pattern {
		return runtime.MustPattern(runtime.NewPattern(1, []int{2, 0, 2, 1, 2, 2},
			[]string{"api", "v1", name}, ""))
	}
	mux.Handle(http.MethodPost, pattern("login"), respond(&apiv1.LoginResponse{Token: "new-token"}))
	mux.Handle(http.MethodPost, pattern("logout"), respond(&apiv1.LogoutResponse{}))
	mux.Handle(http.MethodGet, pattern("whoami"),
		func(w http.ResponseWriter, req *http.Request, _ map[string]string) {
			w.Header().Set("X-Seen-Authorization", req.Header.Get("Authorization"))
		})

	e := echo.New()
	e.Any("/api/v1/*", gatewayHandler(mux, false))
	return e
}

func sendToGateway(e *echo.Echo, method, url string, header http.Header) *http.Response {
	req := httptest.NewRequest(method, url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Result()
}

func TestGatewaySessionCookie(t *testing.T) {
	e := testGateway(t)

	for _, base := range []string{"http://gpu.example", "https://gpu.example"} {
		secure := base == "https://gpu.example"

		resp := sendToGateway(e, http.MethodPost, base+"/api/v1/login", nil)
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
		resp = sendToGateway(e, http.MethodPost, base+"/api/v1/logout", nil)
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
	e := testGateway(t)

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
