package shellterm

import (
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// CheckSameOrigin reports whether a WebSocket upgrade request may proceed. Browsers always send an
// Origin header on WebSocket handshakes, and the master's session cookie is sent with cross-site
// handshakes too, so a browser request must come from a page on the master's own origin. Requests
// without an Origin header come from programs that hold their own token and are allowed.
//
// This does not protect against pages served from the master's own origin, such as task services
// under /proxy/; see the release notes.
func CheckSameOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	switch len(origins) {
	case 0:
		return true
	case 1:
	default:
		return false
	}
	u, err := url.Parse(origins[0])
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// DefaultLang is the locale used when the browser sends none or an invalid one.
const DefaultLang = "C.UTF-8"

var langPattern = regexp.MustCompile(`^(C|[a-z]{2,3}(_[A-Z]{2})?)\.UTF-8$`)

// ValidLang returns lang if it is a plain UTF-8 locale name such as "en_US.UTF-8", and
// DefaultLang otherwise. LANG is the only variable the master forwards from the browser.
func ValidLang(lang string) string {
	if len(lang) <= 16 && langPattern.MatchString(lang) {
		return lang
	}
	return DefaultLang
}

// ParseTrustedProxies parses a list of IP addresses and CIDR ranges.
func ParseTrustedProxies(entries []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, e := range entries {
		if !strings.Contains(e, "/") {
			ip := net.ParseIP(e)
			if ip == nil {
				return nil, &net.ParseError{Type: "IP address", Text: e}
			}
			bits := 8 * net.IPv4len
			if ip.To4() == nil {
				bits = 8 * net.IPv6len
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			return nil, err
		}
		nets = append(nets, n)
	}
	return nets, nil
}

// PeerIP returns the IP address of the TCP peer of a request.
func PeerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ClientIP returns the client address to record for a request: the X-Real-IP header when the TCP
// peer is one of the trusted proxies, and the peer address otherwise. X-Forwarded-For is never
// used, because proxies append to whatever the client sent.
//
// echo's ExtractIPFromRealIPHeader (v4.11) is not used: it checks the header's value, not the
// peer, against the trusted ranges, so any client could claim a trusted address.
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	peer := PeerIP(r)
	peerIP := net.ParseIP(peer)
	if peerIP == nil {
		return peer
	}
	for _, n := range trusted {
		if !n.Contains(peerIP) {
			continue
		}
		realIP := strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("X-Real-IP"), "["), "]")
		if ip := net.ParseIP(realIP); ip != nil {
			return ip.String()
		}
		break
	}
	return peer
}
