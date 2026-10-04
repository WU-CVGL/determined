package user

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
)

// withSecurityConfig changes the master's security configuration for the duration of a test.
func withSecurityConfig(t *testing.T, change func(*config.SecurityConfig)) {
	t.Helper()
	sec := &config.GetMasterConfig().Security
	saved := *sec
	change(sec)
	t.Cleanup(func() { *sec = saved })
}

func TestNewSessionCookie(t *testing.T) {
	for _, secure := range []bool{false, true} {
		c := NewSessionCookie("tok", secure)
		require.Equal(t, "auth", c.Name)
		require.Equal(t, "tok", c.Value)
		require.Equal(t, "/", c.Path)
		require.True(t, c.HttpOnly)
		require.Equal(t, http.SameSiteLaxMode, c.SameSite)
		require.Equal(t, secure, c.Secure)
		require.Equal(t, int(SessionDuration/time.Second), c.MaxAge)
		require.WithinDuration(t, time.Now().Add(SessionDuration), c.Expires, time.Minute)

		header := c.String()
		require.Contains(t, header, "; HttpOnly")
		require.Contains(t, header, "; SameSite=Lax")
		require.Contains(t, header, "; Path=/")
		require.Equal(t, secure, strings.Contains(header, "; Secure"))
	}
}

func TestExpiredSessionCookie(t *testing.T) {
	for _, secure := range []bool{false, true} {
		header := ExpiredSessionCookie(secure).String()
		// The browser only removes the cookie that login set if the name and path match.
		require.True(t, strings.HasPrefix(header, "auth=; Path=/;"), header)
		require.Contains(t, header, "; Max-Age=0")
		require.Contains(t, header, "; Expires=Thu, 01 Jan 1970 00:00:00 GMT")
		require.Contains(t, header, "; HttpOnly")
		require.Contains(t, header, "; SameSite=Lax")
		require.Equal(t, secure, strings.Contains(header, "; Secure"))
	}
}

func TestSessionCookieSecure(t *testing.T) {
	plain := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://gpu.example/api/v1/auth/login", nil)
		r.RemoteAddr = "10.0.1.66:40000"
		return r
	}
	forwarded := func(proto ...string) *http.Request {
		r := plain()
		for _, p := range proto {
			r.Header.Add("X-Forwarded-Proto", p)
		}
		return r
	}
	tls := httptest.NewRequest(http.MethodPost, "https://gpu.example/api/v1/auth/login", nil)

	cases := []struct {
		name    string
		mode    string
		proxies []string
		req     *http.Request
		secure  bool
	}{
		{"auto, plain HTTP", "auto", nil, plain(), false},
		{"empty means auto", "", nil, tls, true},
		{"auto, TLS", "auto", nil, tls, true},
		{"auto, header from untrusted peer", "auto", nil, forwarded("https"), false},
		{"auto, header from other peer", "auto", []string{"10.0.2.0/24"}, forwarded("https"), false},
		{"auto, header from trusted proxy", "auto", []string{"10.0.1.0/24"}, forwarded("https"), true},
		{"auto, trusted proxy by address", "auto", []string{"10.0.1.66"}, forwarded("HTTPS"), true},
		{"auto, trusted proxy says http", "auto", []string{"10.0.1.66"}, forwarded("http"), false},
		{"auto, last proxy wins", "auto", []string{"10.0.1.66"}, forwarded("https, http"), false},
		{"auto, last header wins", "auto", []string{"10.0.1.66"}, forwarded("http", "https"), true},
		{"always", "always", nil, plain(), true},
		{"never", "never", nil, tls, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withSecurityConfig(t, func(s *config.SecurityConfig) {
				s.SessionCookie.Secure = tc.mode
				s.TrustedProxies = tc.proxies
			})
			require.Equal(t, tc.secure, SessionCookieSecure(tc.req))
		})
	}
}

func TestPostSessionCookie(t *testing.T) {
	post := func(header http.Header) (*httptest.ResponseRecorder, error) {
		req := httptest.NewRequest(http.MethodPost, "http://gpu.example/auth/session-cookie", nil)
		for k, v := range header {
			req.Header[k] = v
		}
		rec := httptest.NewRecorder()
		_, err := (&Service{}).postSessionCookie(echo.New().NewContext(req, rec))
		return rec, err
	}

	// The token must arrive in an Authorization header, not a cookie.
	_, err := post(http.Header{"Cookie": {"auth=tok"}})
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusBadRequest, httpErr.Code)

	// Even with the header, the request must come from the master's own pages.
	rec, err := post(http.Header{
		"Authorization":  {"Bearer tok"},
		"Sec-Fetch-Site": {"cross-site"},
		"Origin":         {"https://evil.example"},
	})
	require.ErrorIs(t, err, ErrCrossOriginRequest)
	require.Empty(t, rec.Header().Values("Set-Cookie"))

	rec, err = post(http.Header{"Authorization": {"Bearer tok"}, "Sec-Fetch-Site": {"same-origin"}})
	require.NoError(t, err)
	cookies := recordedCookies(rec)
	require.Len(t, cookies, 1)
	require.Equal(t, "auth", cookies[0].Name)
	require.Equal(t, "tok", cookies[0].Value)
	require.True(t, cookies[0].HttpOnly)
	require.Equal(t, http.SameSiteLaxMode, cookies[0].SameSite)
	require.Equal(t, "/", cookies[0].Path)
}

func TestClearSessionCookieOnLogout(t *testing.T) {
	e := echo.New()
	e.Use(ClearSessionCookieOnLogout)
	// The handlers fail as they do when the session has already ended.
	unauthenticated := func(c echo.Context) error { return echo.NewHTTPError(http.StatusUnauthorized) }
	e.POST("/logout", unauthenticated)
	e.POST("/api/v1/*", unauthenticated)
	e.GET("/api/v1/*", unauthenticated)

	send := func(method, target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		r.Header.Set("Cookie", "auth=stale")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, r)
		return rec
	}
	for _, target := range []string{
		"http://gpu.example/logout", "http://gpu.example/api/v1/auth/logout",
		"http://gpu.example/api/v1/auth/logout/", "https://gpu.example/api/v1/auth/logout",
		// The gateway serves these as /api/v1/auth/logout too.
		"http://gpu.example/api/v1/auth/logout:", "http://gpu.example/api/v1/auth/logout:/",
	} {
		rec := send(http.MethodPost, target)
		require.Equal(t, http.StatusUnauthorized, rec.Code, target)
		cookies := recordedCookies(rec)
		require.Len(t, cookies, 1, target)
		require.Equal(t, "auth", cookies[0].Name)
		require.Empty(t, cookies[0].Value)
		require.Equal(t, "/", cookies[0].Path)
		require.Negative(t, cookies[0].MaxAge)
		require.True(t, cookies[0].HttpOnly)
		require.Equal(t, strings.HasPrefix(target, "https:"), cookies[0].Secure, target)
	}
	// Other requests leave the cookie alone.
	for _, req := range [][2]string{
		{http.MethodGet, "http://gpu.example/api/v1/auth/logout"},
		{http.MethodPost, "http://gpu.example/api/v1/auth/login"},
		{http.MethodPost, "http://gpu.example/api/v1/users/1/logout"},
		{http.MethodPost, "http://gpu.example/api/v1/auth/logout:x"},
	} {
		require.Empty(t, recordedCookies(send(req[0], req[1])), req)
	}
}

// recordedCookies returns the cookies that a recorded response sets.
func recordedCookies(rec *httptest.ResponseRecorder) []*http.Cookie {
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()
	return resp.Cookies()
}
