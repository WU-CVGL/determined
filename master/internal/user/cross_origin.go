package user

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"

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

// NeedsSameOriginCheck reports whether r must pass CheckSameOrigin: it uses a method that can
// change state and carries no Authorization header, so a browser may have attached the session
// cookie (or may set one in reply, as a sign-in does) without the page asking for it. Requests
// with an Authorization header come from the CLI, the SDK, tasks or scripts; browsers only send
// one from another origin after a CORS preflight that the master refuses unless enable_cors is set.
func NeedsSameOriginCheck(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return r.Header.Get(echo.HeaderAuthorization) == ""
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
	return ErrCrossOriginRequest
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
