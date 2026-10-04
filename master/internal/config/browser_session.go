package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Values of SessionCookieConfig.Secure.
const (
	// SessionCookieSecureAuto marks the session cookie Secure when the request that sets it
	// arrived over HTTPS: directly, when the master serves TLS, or through a trusted proxy that
	// says so in X-Forwarded-Proto.
	SessionCookieSecureAuto = "auto"
	// SessionCookieSecureAlways always marks the session cookie Secure.
	SessionCookieSecureAlways = "always"
	// SessionCookieSecureNever never marks the session cookie Secure.
	SessionCookieSecureNever = "never"
)

// SessionCookieConfig configures the cookie that holds a browser's session token.
type SessionCookieConfig struct {
	// Secure is one of "auto" (the default), "always" or "never".
	Secure string `json:"secure"`
}

// Validate implements the check.Validatable interface.
func (c SessionCookieConfig) Validate() []error {
	switch c.Secure {
	case "", SessionCookieSecureAuto, SessionCookieSecureAlways, SessionCookieSecureNever:
		return nil
	default:
		return []error{fmt.Errorf(
			"security.session_cookie.secure must be %q, %q or %q, not %q",
			SessionCookieSecureAuto, SessionCookieSecureAlways, SessionCookieSecureNever, c.Secure)}
	}
}

// CSRFConfig configures the master's check against cross-site requests that rely on the browser's
// session cookie.
type CSRFConfig struct {
	// TrustedOrigins lists other origins (scheme://host[:port]) whose pages may send such requests,
	// for example a web UI development server on another port.
	TrustedOrigins []string `json:"trusted_origins"`
}

// Validate implements the check.Validatable interface.
func (c CSRFConfig) Validate() []error {
	var errs []error
	for _, o := range c.TrustedOrigins {
		if _, err := NormalizeOrigin(o); err != nil {
			errs = append(errs, fmt.Errorf("security.csrf.trusted_origins: %w", err))
		}
	}
	return errs
}

// IsTrustedOrigin reports whether origin, the value of a request's Origin header, is one of the
// trusted origins.
func (c CSRFConfig) IsTrustedOrigin(origin string) bool {
	o, err := NormalizeOrigin(origin)
	if err != nil {
		return false
	}
	for _, t := range c.TrustedOrigins {
		if n, err := NormalizeOrigin(t); err == nil && n == o {
			return true
		}
	}
	return false
}

// NormalizeOrigin checks that s is an origin as browsers send it in the Origin header (scheme,
// host and optional port, nothing else) and returns it in lower case without a trailing slash.
func NormalizeOrigin(s string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(s), "/"))
	switch {
	case err != nil:
		return "", fmt.Errorf("%q is not an origin: %w", s, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("%q is not an origin: the scheme must be http or https", s)
	case u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "":
		return "", fmt.Errorf("%q is not an origin: use scheme://host[:port] only", s)
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}

// TrustedProxies lists the reverse proxies, as IP addresses or CIDR ranges, whose
// X-Forwarded-Proto and X-Forwarded-Host headers the master believes.
type TrustedProxies []string

// Validate implements the check.Validatable interface.
func (p TrustedProxies) Validate() []error {
	var errs []error
	for _, s := range p {
		if parseIPNet(s) == nil {
			errs = append(errs, fmt.Errorf(
				"security.trusted_proxies: %q is not an IP address or CIDR range", s))
		}
	}
	return errs
}

// Contains reports whether ip is one of the trusted proxies.
func (p TrustedProxies) Contains(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, s := range p {
		if n := parseIPNet(s); n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

func parseIPNet(s string) *net.IPNet {
	s = strings.TrimSpace(s)
	if _, n, err := net.ParseCIDR(s); err == nil {
		return n
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil
	}
	bits := 8 * net.IPv6len
	if ip4 := ip.To4(); ip4 != nil {
		ip, bits = ip4, 8*net.IPv4len
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
}
