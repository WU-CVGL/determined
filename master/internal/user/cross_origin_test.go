package user

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
)

func TestNeedsSameOriginCheck(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		require.False(t, NeedsSameOriginCheck(httptest.NewRequest(method, "/api/v1/me", nil)), method)
	}
	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, "PROPFIND",
	} {
		r := httptest.NewRequest(method, "/api/v1/users", nil)
		require.True(t, NeedsSameOriginCheck(r), method)
		// The CLI, the SDK and tasks authenticate with a header that browsers do not add.
		r.Header.Set("Authorization", "Bearer tok")
		require.False(t, NeedsSameOriginCheck(r), method)
		// Browsers can add other credentials to a cross-site form post by themselves.
		for _, other := range []string{"Basic dXNlcjpwYXNz", "Negotiate abc", "Bearer ", "Bearer  ", "bearer tok"} {
			r.Header.Set("Authorization", other)
			require.True(t, NeedsSameOriginCheck(r), "%s %q", method, other)
		}
	}

	// A WebSocket handshake is a GET, but the connection can do anything the page could.
	ws := httptest.NewRequest(http.MethodGet, "/stream", nil)
	ws.Header.Set("Upgrade", "WebSocket")
	ws.Header.Set("Connection", "Upgrade")
	require.True(t, NeedsSameOriginCheck(ws))
	ws.Header.Set("Authorization", "Bearer tok")
	require.False(t, NeedsSameOriginCheck(ws))
}

func TestCheckSameOrigin(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		peer    string
		headers map[string]string
		ok      bool
	}{
		{"no browser headers", "gpu.example", "", nil, true},
		{"same-origin fetch", "gpu.example", "", map[string]string{
			"Sec-Fetch-Site": "same-origin", "Origin": "https://gpu.example",
		}, true},
		{
			"browser says same-origin through a proxy that rewrote Host", "localhost:8080", "",
			map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://localhost:3000"},
			true,
		},
		{"user-initiated", "gpu.example", "", map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"sibling subdomain", "gpu.example", "", map[string]string{
			"Sec-Fetch-Site": "same-site", "Origin": "https://wandb.example",
		}, false},
		{"other site", "gpu.example", "", map[string]string{
			"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.test",
		}, false},
		{"other site without Origin", "gpu.example", "", map[string]string{
			"Sec-Fetch-Site": "cross-site",
		}, false},
		{"other site, trusted origin", "gpu.example", "", map[string]string{
			"Sec-Fetch-Site": "same-site", "Origin": "http://localhost:3000",
		}, true},
		{
			"old browser, same host", "gpu.example", "",
			map[string]string{"Origin": "https://gpu.example"},
			true,
		},
		{
			"old browser, host in other case", "gpu.example", "",
			map[string]string{"Origin": "https://GPU.example"},
			true,
		},
		{
			"old browser, default port in Host", "gpu.example:443", "",
			map[string]string{"Origin": "https://gpu.example"},
			true,
		},
		{
			"old browser, same non-default port", "gpu.example:8080", "",
			map[string]string{"Origin": "http://gpu.example:8080"},
			true,
		},
		{
			"old browser, other port", "gpu.example", "",
			map[string]string{"Origin": "http://gpu.example:8080"},
			false,
		},
		{
			"old browser, other host", "gpu.example", "",
			map[string]string{"Origin": "https://evil.test"},
			false,
		},
		{"old browser, opaque origin", "gpu.example", "", map[string]string{"Origin": "null"}, false},
		{
			"old browser, trusted origin", "gpu.example", "",
			map[string]string{"Origin": "http://localhost:3000"},
			true,
		},
		{"forwarded host from untrusted peer", "10.0.1.6:8080", "192.0.2.1:5000", map[string]string{
			"Origin": "https://gpu.example", "X-Forwarded-Host": "gpu.example",
		}, false},
		{"forwarded host from trusted proxy", "10.0.1.6:8080", "10.0.1.66:5000", map[string]string{
			"Origin": "https://gpu.example", "X-Forwarded-Host": "gpu.example",
		}, true},
		{"trusted proxy, other origin", "10.0.1.6:8080", "10.0.1.66:5000", map[string]string{
			"Origin": "https://evil.test", "X-Forwarded-Host": "gpu.example",
		}, false},
	}
	withSecurityConfig(t, func(s *config.SecurityConfig) {
		s.CSRF.TrustedOrigins = []string{"http://localhost:3000"}
		s.TrustedProxies = []string{"10.0.1.66"}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/users", nil)
			r.Host = tc.host
			if tc.peer != "" {
				r.RemoteAddr = tc.peer
			}
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if tc.ok {
				require.NoError(t, CheckSameOrigin(r))
			} else {
				require.ErrorIs(t, CheckSameOrigin(r), ErrCrossOriginRequest)
			}
		})
	}
}

func TestCrossOriginProtection(t *testing.T) {
	e := echo.New()
	e.Use(CrossOriginProtection)
	ok := func(c echo.Context) error { return c.NoContent(http.StatusNoContent) }
	e.Any("/api/v1/*", ok)
	e.Any("/proxy/:service/*", ok)
	e.POST("/saml/sso", ok)
	e.Any("/oauth2/token", ok)
	e.POST("/oauth2/clients", ok)
	e.POST("/scim/v2/Users", ok)
	e.POST("/logout", ok)
	e.Any("/*", ok)

	send := func(method, path string, headers map[string]string) int {
		r := httptest.NewRequest(method, "http://gpu.example"+path, nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, r)
		return rec.Code
	}
	crossSite := map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.test"}

	for _, path := range []string{
		"/api/v1/users/1/password", "/api/v1/auth/login", "/oauth2/clients", "/logout", "/unknown",
	} {
		require.Equal(t, http.StatusForbidden, send(http.MethodPost, path, crossSite), path)
		require.Equal(t, http.StatusNoContent, send(http.MethodPost, path, map[string]string{
			"Sec-Fetch-Site": "same-origin", "Origin": "http://gpu.example",
		}), path)
		require.Equal(t, http.StatusNoContent, send(http.MethodPost, path, nil), path)
	}
	require.Equal(t, http.StatusForbidden, send(http.MethodDelete, "/api/v1/users/1", crossSite))
	require.Equal(t, http.StatusNoContent, send(http.MethodGet, "/api/v1/me", crossSite))

	// WebSocket handshakes are checked although they are GETs.
	wsHeaders := func(h map[string]string) map[string]string {
		out := map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}
		for k, v := range h {
			out[k] = v
		}
		return out
	}
	require.Equal(t, http.StatusForbidden, send(http.MethodGet, "/stream", wsHeaders(crossSite)))
	require.Equal(t, http.StatusForbidden, send(http.MethodGet, "/stream", wsHeaders(map[string]string{
		"Sec-Fetch-Site": "same-site", "Origin": "https://notebooks.gpu.example",
	})))
	require.Equal(t, http.StatusNoContent, send(http.MethodGet, "/stream", wsHeaders(map[string]string{
		"Sec-Fetch-Site": "same-origin", "Origin": "http://gpu.example",
	})))
	// Agents and other programs send no Origin.
	require.Equal(t, http.StatusNoContent, send(http.MethodGet, "/agents", wsHeaders(nil)))

	// A cross-site form post that the browser adds Basic credentials to is still refused.
	withBasic := map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}
	for k, v := range crossSite {
		withBasic[k] = v
	}
	require.Equal(t, http.StatusForbidden, send(http.MethodPost, "/api/v1/users", withBasic))

	withAuthorization := map[string]string{"Authorization": "Bearer tok"}
	for k, v := range crossSite {
		withAuthorization[k] = v
	}
	require.Equal(t, http.StatusNoContent, send(http.MethodPost, "/api/v1/users", withAuthorization))

	// Routes that other sites post to, or that check their own credentials.
	for _, path := range []string{"/proxy/abc/api/kernels", "/saml/sso", "/oauth2/token", "/scim/v2/Users"} {
		require.Equal(t, http.StatusNoContent, send(http.MethodPost, path, crossSite), path)
	}
}
