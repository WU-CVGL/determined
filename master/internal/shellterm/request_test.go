package shellterm

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckSameOrigin(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		origins []string
		ok      bool
	}{
		{"no origin (not a browser)", "gpu.example.org", nil, true},
		{"same origin", "gpu.example.org", []string{"https://gpu.example.org"}, true},
		{"same origin, case", "GPU.example.org", []string{"https://gpu.EXAMPLE.org"}, true},
		{"same origin with port", "localhost:3000", []string{"http://localhost:3000"}, true},
		{"other host", "gpu.example.org", []string{"https://evil.example.org"}, false},
		{"sibling subdomain", "gpu.example.org", []string{"https://frp.example.org"}, false},
		{"other port", "gpu.example.org", []string{"https://gpu.example.org:8443"}, false},
		{"suffix trick", "gpu.example.org", []string{"https://gpu.example.org.evil.com"}, false},
		{"null origin", "gpu.example.org", []string{"null"}, false},
		{"malformed", "gpu.example.org", []string{"://gpu.example.org"}, false},
		{"other scheme", "gpu.example.org", []string{"file://gpu.example.org"}, false},
		{"empty value", "gpu.example.org", []string{""}, false},
		{"two origins", "gpu.example.org", []string{"https://gpu.example.org", "https://evil.org"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/ws/shells/x/terminal", nil)
			r.Host = tc.host
			for _, o := range tc.origins {
				r.Header.Add("Origin", o)
			}
			require.Equal(t, tc.ok, CheckSameOrigin(r))
		})
	}
}

func TestValidLang(t *testing.T) {
	for in, want := range map[string]string{
		"en_US.UTF-8":                 "en_US.UTF-8",
		"de_DE.UTF-8":                 "de_DE.UTF-8",
		"zh_CN.UTF-8":                 "zh_CN.UTF-8",
		"fil_PH.UTF-8":                "fil_PH.UTF-8",
		"C.UTF-8":                     "C.UTF-8",
		"en.UTF-8":                    "en.UTF-8",
		"":                            DefaultLang,
		"en_US":                       DefaultLang,
		"en_US.ISO-8859-1":            DefaultLang,
		"en_US.utf8":                  DefaultLang,
		"en-US.UTF-8":                 DefaultLang,
		"EN_us.UTF-8":                 DefaultLang,
		"en_US.UTF-8\nLD_PRELOAD=/x":  DefaultLang,
		"en_US.UTF-8 ":                DefaultLang,
		"en_US.UTF-8;rm -rf /":        DefaultLang,
		"../../etc/passwd":            DefaultLang,
		"aaaaaaaaaaaaaaaaaaaa.UTF-8":  DefaultLang,
		"en_US.UTF-8@euro":            DefaultLang,
		"$(reboot).UTF-8":             DefaultLang,
		"en_US.UTF-8\x00LANGUAGE=foo": DefaultLang,
	} {
		require.Equal(t, want, ValidLang(in), "ValidLang(%q)", in)
	}
}

func TestClampSize(t *testing.T) {
	for _, tc := range []struct{ cols, rows, wantCols, wantRows int }{
		{80, 24, 80, 24},
		{0, 0, DefaultCols, DefaultRows},
		{-5, 10, DefaultCols, 10},
		{1, 1, 1, 1},
		{1000, 1000, 1000, 1000},
		{1001, 99999, 1000, 1000},
	} {
		c, r := ClampSize(tc.cols, tc.rows)
		require.Equal(t, tc.wantCols, c)
		require.Equal(t, tc.wantRows, r)
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(2, 3)
	r1, ok := l.Acquire(1)
	require.True(t, ok)
	r2, ok := l.Acquire(1)
	require.True(t, ok)
	_, ok = l.Acquire(1)
	require.False(t, ok, "per-user limit")

	r3, ok := l.Acquire(2)
	require.True(t, ok)
	_, ok = l.Acquire(3)
	require.False(t, ok, "global limit")

	r1()
	r1() // Releasing twice frees one slot only.
	total, user := l.Active(1)
	require.Equal(t, 2, total)
	require.Equal(t, 1, user)
	r4, ok := l.Acquire(3)
	require.True(t, ok)
	_, ok = l.Acquire(1)
	require.False(t, ok, "global limit")
	r2()
	r3()
	r4()
	total, _ = l.Active(1)
	require.Zero(t, total)

	unlimited := NewLimiter(0, 0)
	for i := 0; i < 100; i++ {
		_, ok := unlimited.Acquire(1)
		require.True(t, ok)
	}
}

func TestClientIP(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"10.0.0.5", "192.168.233.0/24"})
	require.NoError(t, err)
	_, err = ParseTrustedProxies([]string{"not-an-ip"})
	require.Error(t, err)
	_, err = ParseTrustedProxies([]string{"10.0.0.0/33"})
	require.Error(t, err)

	req := func(peer string, headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = peer
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}
	// A client that is not a trusted proxy cannot choose its logged address.
	require.Equal(t, "203.0.113.9", ClientIP(req("203.0.113.9:5000", map[string]string{
		"X-Real-IP": "10.0.0.5", "X-Forwarded-For": "1.2.3.4",
	}), trusted))
	// Without trusted proxies, the peer is always used.
	require.Equal(t, "10.0.0.5", ClientIP(req("10.0.0.5:5000", map[string]string{
		"X-Real-IP": "198.51.100.7",
	}), nil))
	// The trusted proxy's X-Real-IP is used; X-Forwarded-For never is.
	require.Equal(t, "198.51.100.7", ClientIP(req("192.168.233.6:5000", map[string]string{
		"X-Real-IP": "198.51.100.7", "X-Forwarded-For": "1.2.3.4, 198.51.100.7",
	}), trusted))
	require.Equal(t, "192.168.233.6", ClientIP(req("192.168.233.6:5000", map[string]string{
		"X-Forwarded-For": "1.2.3.4",
	}), trusted))
	require.Equal(t, "192.168.233.6", ClientIP(req("192.168.233.6:5000", map[string]string{
		"X-Real-IP": "garbage",
	}), trusted))
	require.Equal(t, "2001:db8::1", ClientIP(req("10.0.0.5:1", map[string]string{
		"X-Real-IP": "[2001:db8::1]",
	}), trusted))
}
