package user

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/determined-ai/determined/master/internal/config"
)

// SessionCookieName is the name of the cookie that holds a browser's session token. The master
// sets it when a user signs in; the web UI only looks for one that an earlier version left
// readable to scripts, to replace or expire it.
const SessionCookieName = "auth"

// NewSessionCookie returns the cookie that keeps token as a browser's session. It is HttpOnly, so
// that scripts cannot read the token, and SameSite=Lax, so that browsers leave it off requests that
// other sites start, apart from top-level navigations. It covers the whole master (Path=/),
// because the proxied services under /proxy/ and the downloads that the web UI opens as pages
// authenticate with it too. secure comes from SessionCookieSecure.
func NewSessionCookie(token string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  time.Now().Add(SessionDuration),
		MaxAge:   int(SessionDuration / time.Second),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// ExpiredSessionCookie returns a cookie that removes the session cookie from a browser. Browsers
// only replace a cookie with the same name, domain and path, so it must keep Path=/.
func ExpiredSessionCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// logoutPaths are the routes that end a browser's session: the gateway's and the legacy one.
var logoutPaths = map[string]bool{
	"/api/v1/auth/logout": true,
	"/logout":             true,
}

// signInPaths are the routes whose reply sets the session cookie: the gateway's sign-in, the legacy
// one (with ?cookie=true), and storing a token from the web UI's URL (postSessionCookie).
var signInPaths = map[string]bool{
	"/api/v1/auth/login":   true,
	"/login":               true,
	"/auth/session-cookie": true,
}

// sessionCookiePath returns the path of r the way signInPaths and logoutPaths list it. The gateway
// serves other paths as the same routes: its echo route removes a trailing slash, and grpc-gateway
// then splits a custom verb off the last segment at its last colon, so /api/v1/auth/login: and
// /api/v1/auth/login:/ sign in like /api/v1/auth/login, and must not escape the origin check.
// echo serves the legacy routes at their own paths only; matching them more widely only checks
// requests that echo does not serve.
func sessionCookiePath(r *http.Request) string {
	return strings.TrimSuffix(strings.TrimSuffix(r.URL.Path, "/"), ":")
}

// changesSessionCookie reports whether the reply to r sets or clears the session cookie.
func changesSessionCookie(r *http.Request) bool {
	path := sessionCookiePath(r)
	return signInPaths[path] || logoutPaths[path]
}

// ClearSessionCookieOnLogout is middleware that removes the session cookie on every sign-out
// request, before authentication runs. The cookie must go even when its session has already ended
// (expired, revoked by a password change, or deleted), when the request fails authentication and
// the logout handlers never run; the web UI cannot remove the HttpOnly cookie itself. It must run
// after CrossOriginProtection, so that other sites cannot sign a visitor out.
func ClearSessionCookieOnLogout(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		r := c.Request()
		if r.Method == http.MethodPost && logoutPaths[sessionCookiePath(r)] {
			c.SetCookie(ExpiredSessionCookie(SessionCookieSecure(r)))
		}
		return next(c)
	}
}

// SessionCookieSecure reports whether a session cookie set in response to r is marked Secure,
// following security.session_cookie.secure. With "auto", it is when r arrived over HTTPS.
func SessionCookieSecure(r *http.Request) bool {
	switch config.GetMasterConfig().Security.SessionCookie.Secure {
	case config.SessionCookieSecureAlways:
		return true
	case config.SessionCookieSecureNever:
		return false
	default:
		return RequestIsHTTPS(r)
	}
}

// RequestIsHTTPS reports whether the client sent r over HTTPS: either the master itself served
// TLS, or r came from a trusted proxy that sets X-Forwarded-Proto to https. The header is
// ignored from other peers, since any client can send it.
func RequestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return FromTrustedProxy(r) && strings.EqualFold(lastHeaderValue(r.Header, "X-Forwarded-Proto"), "https")
}

// FromTrustedProxy reports whether the peer that sent r is one of security.trusted_proxies.
func FromTrustedProxy(r *http.Request) bool {
	proxies := config.GetMasterConfig().Security.TrustedProxies
	if len(proxies) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return proxies.Contains(net.ParseIP(host))
}

// lastHeaderValue returns the last element of a header that proxies may append to, such as
// X-Forwarded-Proto: "https, http" from two proxies. The last one was set by the proxy closest to
// the master, which is the one the master trusts.
func lastHeaderValue(h http.Header, name string) string {
	values := h.Values(name)
	if len(values) == 0 {
		return ""
	}
	parts := strings.Split(values[len(values)-1], ",")
	return strings.TrimSpace(parts[len(parts)-1])
}
