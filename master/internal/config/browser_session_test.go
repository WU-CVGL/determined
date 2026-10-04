package config

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionCookieConfig(t *testing.T) {
	require.Equal(t, SessionCookieSecureAuto, DefaultConfig().Security.SessionCookie.Secure)
	for _, v := range []string{"", "auto", "always", "never"} {
		require.Empty(t, SessionCookieConfig{Secure: v}.Validate(), v)
	}
	for _, v := range []string{"true", "Always", "yes"} {
		require.Len(t, SessionCookieConfig{Secure: v}.Validate(), 1, v)
	}
}

func TestCSRFConfig(t *testing.T) {
	c := CSRFConfig{TrustedOrigins: []string{"http://localhost:3000", "HTTPS://Dev.Example/"}}
	require.Empty(t, c.Validate())
	require.True(t, c.IsTrustedOrigin("http://localhost:3000"))
	require.True(t, c.IsTrustedOrigin("https://dev.example"))
	require.False(t, c.IsTrustedOrigin("http://localhost:3001"))
	require.False(t, c.IsTrustedOrigin("https://localhost:3000"))
	require.False(t, c.IsTrustedOrigin("null"))
	require.False(t, c.IsTrustedOrigin(""))

	for _, bad := range []string{
		"localhost:3000", "ftp://example", "https://example/path", "https://example?q=1",
		"https://user@example", "https://", "*",
	} {
		require.Len(t, CSRFConfig{TrustedOrigins: []string{bad}}.Validate(), 1, bad)
	}
}

func TestTrustedProxies(t *testing.T) {
	p := TrustedProxies{"10.0.1.66", "192.168.233.0/24", "fd00::/8"}
	require.Empty(t, p.Validate())
	for ip, trusted := range map[string]bool{
		"10.0.1.66":        true,
		"10.0.1.67":        false,
		"192.168.233.8":    true,
		"192.168.234.8":    false,
		"::ffff:10.0.1.66": true,
		"fd00::1":          true,
		"fe80::1":          false,
	} {
		require.Equal(t, trusted, p.Contains(net.ParseIP(ip)), ip)
	}
	require.False(t, p.Contains(nil))
	require.Len(t, TrustedProxies{"10.0.1.66", "gpu.example", "10.0.0.0/33"}.Validate(), 2)
}

func TestUnmarshalBrowserSessionConfig(t *testing.T) {
	c := DefaultConfig()
	require.NoError(t, json.Unmarshal([]byte(`{"security": {
		"session_cookie": {"secure": "always"},
		"csrf": {"trusted_origins": ["http://localhost:3000"]},
		"trusted_proxies": ["10.0.1.66"]
	}}`), c))
	require.Equal(t, SessionCookieSecureAlways, c.Security.SessionCookie.Secure)
	require.Equal(t, []string{"http://localhost:3000"}, c.Security.CSRF.TrustedOrigins)
	require.Equal(t, TrustedProxies{"10.0.1.66"}, c.Security.TrustedProxies)
	// Fields that are left out keep their defaults.
	require.Equal(t, KeyTypeED25519, c.Security.SSH.KeyType)
}
