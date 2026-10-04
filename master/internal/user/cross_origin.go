package user

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"
	log "github.com/sirupsen/logrus"

	"github.com/determined-ai/determined/master/internal/config"
)

// ErrCrossOriginRequest is returned for a request that changes state, relies on the browser's
// session cookie and was sent by a page on another origin.
var ErrCrossOriginRequest = echo.NewHTTPError(http.StatusForbidden,
	"cross-origin request refused: requests that rely on the session cookie must come from the "+
		"master's own pages; other clients must send an Authorization header")

// crossOriginExemptRoutes are routes that other sites legitimately send unsafe requests to, or
// that check credentials of their own:
//   - /proxy/:service/* authenticates the visitor itself; processProxyAuthentication applies the
//     check when it accepts the session cookie, and leaves services without authentication alone.
//   - /saml/sso receives the identity provider's cross-site POST.
//   - /oauth2/authorize and /oauth2/token are called by OAuth clients.
var crossOriginExemptRoutes = map[string]bool{
	"/proxy/:service/*": true,
	"/saml/sso":         true,
	"/oauth2/authorize": true,
	"/oauth2/token":     true,
}

// NeedsSameOriginCheck reports whether r must pass CheckSameOrigin: a browser may have attached
// the session cookie to it (or may store one from the reply, as a sign-in does) without the page
// asking for it, and it can change state. That is any method but GET, HEAD and OPTIONS, and a
// WebSocket handshake, which is a GET that opens a connection able to do anything the page could.
//
// Requests that carry a bearer token come from the CLI, the SDK, tasks or scripts and are exempt:
// browsers attach no such header by themselves, and only send one from another origin after a
// CORS preflight that the master refuses unless enable_cors is set. Other Authorization headers
// do not exempt a request, since browsers can attach Basic, Digest or Negotiate credentials to a
// cross-site form post.
func NeedsSameOriginCheck(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		if !IsWebSocketHandshake(r) {
			return false
		}
	}
	return !HasBearerToken(r)
}

// HasBearerToken reports whether r carries a non-empty "Authorization: Bearer" token.
func HasBearerToken(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get(echo.HeaderAuthorization), "Bearer ")
	return ok && strings.TrimSpace(token) != ""
}

// IsWebSocketHandshake reports whether r asks to open a WebSocket.
func IsWebSocketHandshake(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get(echo.HeaderUpgrade), "websocket")
}

// CheckSameOrigin returns ErrCrossOriginRequest when a browser sent r from a page on another
// origin than the master's own, unless that origin is in security.csrf.trusted_origins. It follows
// net/http.CrossOriginProtection from Go 1.25:
//   - Sec-Fetch-Site "same-origin" or "none" (typed into the address bar) passes; any other value
//     ("same-site", "cross-site") fails.
//   - Without Sec-Fetch-Site (older browsers), the Origin header's host must be the request's host.
//   - Without either header, the request did not come from a browser, or came from an old browser
//     on the same origin, and passes.
//
// The request's host is the Host header, or the X-Forwarded-Host header when a trusted proxy
// sent the request. Pages under /proxy/ share the master's origin, so this check cannot tell them
// apart from the web UI.
func CheckSameOrigin(r *http.Request) error {
	origin := r.Header.Get(echo.HeaderOrigin)
	switch r.Header.Get("Sec-Fetch-Site") {
	case "":
	case "same-origin", "none":
		return nil
	default:
		if isTrustedOrigin(origin) {
			return nil
		}
		logCrossOriginRefusal(r)
		return ErrCrossOriginRequest
	}

	if origin == "" {
		return nil
	}
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		for _, host := range requestHosts(r) {
			if sameHost(u.Host, host) {
				return nil
			}
		}
	}
	if isTrustedOrigin(origin) {
		return nil
	}
	logCrossOriginRefusal(r)
	return ErrCrossOriginRequest
}

// logCrossOriginRefusal records what CheckSameOrigin saw when it refused r. A reverse proxy that
// rewrites the Host header, or drops its port, makes the master refuse its own pages over plain
// HTTP, where browsers send no Sec-Fetch-Site; these fields show an operator why.
func logCrossOriginRefusal(r *http.Request) {
	log.WithFields(log.Fields{
		"method":             r.Method,
		"path":               r.URL.Path,
		"origin":             r.Header.Get(echo.HeaderOrigin),
		"sec_fetch_site":     r.Header.Get("Sec-Fetch-Site"),
		"host":               r.Host,
		"x_forwarded_host":   r.Header.Get("X-Forwarded-Host"),
		"remote_addr":        r.RemoteAddr,
		"from_trusted_proxy": FromTrustedProxy(r),
	}).Warn("refused a cross-origin request that relies on the session cookie; if it came from " +
		"the master's own pages, make the reverse proxy keep the Host header with its port, or " +
		"list the origin in security.csrf.trusted_origins")
}

// CrossOriginProtection is middleware that applies CheckSameOrigin to every request that
// NeedsSameOriginCheck, apart from crossOriginExemptRoutes and SCIM, which authenticates with an
// Authorization header of its own. It must run after routing, since it looks at c.Path().
func CrossOriginProtection(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		r := c.Request()
		if !NeedsSameOriginCheck(r) || crossOriginExemptRoutes[c.Path()] ||
			strings.HasPrefix(c.Path(), "/scim/") {
			return next(c)
		}
		if err := CheckSameOrigin(r); err != nil {
			return err
		}
		return next(c)
	}
}

func isTrustedOrigin(origin string) bool {
	return origin != "" && config.GetMasterConfig().Security.CSRF.IsTrustedOrigin(origin)
}

// requestHosts returns the hosts that the client may have addressed r to.
func requestHosts(r *http.Request) []string {
	hosts := []string{r.Host}
	if FromTrustedProxy(r) {
		if fwd := lastHeaderValue(r.Header, "X-Forwarded-Host"); fwd != "" {
			hosts = append(hosts, fwd)
		}
	}
	return hosts
}

// sameHost compares two host[:port] values, ignoring case and the default ports 80 and 443, which
// browsers leave out of the Origin header.
func sameHost(a, b string) bool {
	canonical := func(h string) string {
		h = strings.ToLower(h)
		for _, port := range []string{":80", ":443"} {
			h = strings.TrimSuffix(h, port)
		}
		return h
	}
	return a != "" && canonical(a) == canonical(b)
}
