package user

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/determined-ai/determined/master/internal/config"
)

// SessionCookieName is the name of the cookie that holds a browser's session token. The master
// sets it when a user signs in; the web UI never reads or writes it.
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
