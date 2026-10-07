package proxy

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/o1egl/paseto"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/logger"
)

// A JupyterLab session: Tornado quotes its signed cookie values.
const serviceCookies = `_xsrf=2|1a2b|3c4d|1700000000; ` +
	`username-host-8888="2|1:0|10:1700000000|19:username-host-8888|4:e30=|abcdef"`

func mustGenerateKey() (ed25519.PublicKey, ed25519.PrivateKey) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		panic(err)
	}
	return public, private
}

func mustSign(key ed25519.PrivateKey, expiration time.Time) string {
	token, err := paseto.NewV2().Sign(key, paseto.JSONToken{Expiration: expiration}, nil)
	if err != nil {
		panic(err)
	}
	return token
}

var (
	masterPublicKey, masterPrivateKey = mustGenerateKey()
	_, foreignPrivateKey              = mustGenerateKey()

	masterToken        = mustSign(masterPrivateKey, time.Now().Add(time.Hour))
	expiredMasterToken = mustSign(masterPrivateKey, time.Now().Add(-time.Hour))
	// A PASETO token that the master did not sign, such as one the service issues itself.
	foreignToken = mustSign(foreignPrivateKey, time.Now().Add(time.Hour))
)

// isTestMasterToken checks the signature only, like user.IsMasterSignedToken.
func isTestMasterToken(token string) bool {
	return paseto.NewV2().Verify(token, masterPublicKey, nil, nil) == nil
}

// masterTokenHeaderLines are the gRPC metadata headers in which clients send master tokens, as a
// client may spell them: the Python SDK sends a task's session token in the first.
var masterTokenHeaderLines = []string{
	"Grpc-Metadata-x-allocation-token: Bearer " + masterToken,
	"Grpc-Metadata-X-User-Token: Bearer " + masterToken,
	"grpc-metadata-grpcgateway-authorization: Bearer " + masterToken,
}

func requireNoMasterTokenHeaders(t *testing.T, got http.Header) {
	for _, name := range masterTokenHeaders {
		require.Empty(t, got.Values(name), name)
	}
}

// visitorCookies are what a signed-in browser sends to /proxy/: the master's session cookies are
// mixed in with the service's own cookies.
var visitorCookies = []string{
	"auth=" + masterToken + "; " + serviceCookies,
	"det_jwt=external-session",
}

func newTestProxy(t *testing.T, sawAuthCookie *string) (*Proxy, string) {
	p := &Proxy{
		HTTPAuth: func(c echo.Context) (bool, error) {
			// Authentication runs on the request as the visitor sent it.
			if cookie, err := c.Request().Cookie("auth"); err == nil {
				*sawAuthCookie = cookie.Value
			}
			return false, nil
		},
		IsMasterToken: isTestMasterToken,
		services:      map[string]*Service{},
		syslog:        logrus.WithField("component", "proxy"),
	}
	e := echo.New()
	// As in the master, the proxies log through logrus.
	e.Logger = logger.New()
	e.Any("/proxy/:service/*", p.NewProxyHandler("service"))
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return p, srv.URL
}

var credentialCases = []struct {
	name              string
	unauthenticated   bool
	authorization     string
	wantAuthorization []string
}{
	{"authenticated, master bearer token", false, "Bearer " + masterToken, nil},
	{"authenticated, lowercase bearer", false, "bearer " + masterToken, nil},
	{"authenticated, notebook token", false, "token nb", []string{"token nb"}},
	{"unauthenticated, master bearer token", true, "Bearer " + masterToken, nil},
	{"unauthenticated, lowercase master bearer", true, "bearer " + masterToken, nil},
	{"unauthenticated, expired master bearer token", true, "Bearer " + expiredMasterToken, nil},
	{"unauthenticated, service bearer token", true, "Bearer svc-key", []string{"Bearer svc-key"}},
	{
		"unauthenticated, bearer token signed by another key", true, "Bearer " + foreignToken,
		[]string{"Bearer " + foreignToken},
	},
	{"unauthenticated, basic", true, "Basic dXNlcjpwYXNz", []string{"Basic dXNlcjpwYXNz"}},
	{"unauthenticated, notebook token", true, "token nb", []string{"token nb"}},
	{"unauthenticated, no authorization", true, "", nil},
}

func TestProxyStripsMasterCredentialsHTTP(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			var sawAuthCookie string
			p, base := newTestProxy(t, &sawAuthCookie)

			upstreamHeaders := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHeaders <- r.Header.Clone()
			}))
			defer upstream.Close()
			upstreamURL, err := url.Parse(upstream.URL)
			require.NoError(t, err)
			p.Register("svc", upstreamURL, false, tc.unauthenticated)

			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				base+"/proxy/svc/lab?token=nb", nil)
			require.NoError(t, err)
			for _, line := range visitorCookies {
				req.Header.Add("Cookie", line)
			}
			for _, line := range masterTokenHeaderLines {
				name, value, _ := strings.Cut(line, ": ")
				req.Header.Add(name, value)
			}
			if tc.authorization != "" {
				req.Header.Set("Authorization", tc.authorization)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusOK, resp.StatusCode)

			got := <-upstreamHeaders
			require.Equal(t, []string{serviceCookies}, got.Values("Cookie"))
			require.Equal(t, tc.wantAuthorization, got.Values("Authorization"))
			requireNoMasterTokenHeaders(t, got)
			if !tc.unauthenticated {
				require.Equal(t, masterToken, sawAuthCookie)
			}
		})
	}
}

func TestProxyStripsMasterCredentialsWebSocket(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			var sawAuthCookie string
			p, base := newTestProxy(t, &sawAuthCookie)

			// The WebSocket proxy writes the visitor's upgrade request to the service as is.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer func() { _ = ln.Close() }()
			upstreamHeaders := make(chan http.Header, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				r, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				upstreamHeaders <- r.Header
				_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
					"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
			}()
			p.Register("svc", &url.URL{Scheme: "http", Host: ln.Addr().String()},
				false, tc.unauthenticated)

			conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			var upgrade strings.Builder
			upgrade.WriteString("GET /proxy/svc/api/kernels/1/channels HTTP/1.1\r\n" +
				"Host: master\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
				"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n")
			for _, line := range visitorCookies {
				fmt.Fprintf(&upgrade, "Cookie: %s\r\n", line)
			}
			for _, line := range masterTokenHeaderLines {
				fmt.Fprintf(&upgrade, "%s\r\n", line)
			}
			if tc.authorization != "" {
				fmt.Fprintf(&upgrade, "Authorization: %s\r\n", tc.authorization)
			}
			upgrade.WriteString("\r\n")
			_, err = conn.Write([]byte(upgrade.String()))
			require.NoError(t, err)

			select {
			case got := <-upstreamHeaders:
				require.Equal(t, []string{serviceCookies}, got.Values("Cookie"))
				require.Equal(t, tc.wantAuthorization, got.Values("Authorization"))
				requireNoMasterTokenHeaders(t, got)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "the upgrade request never reached the service")
			}
			if !tc.unauthenticated {
				require.Equal(t, masterToken, sawAuthCookie)
			}
		})
	}
}

func TestStripCookies(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"no cookies", nil, nil},
		{"nothing to strip is left untouched", []string{"a=1;b=2", "c=3"}, []string{"a=1;b=2", "c=3"}},
		{"only master cookies", []string{"auth=x; det_jwt=y"}, nil},
		{"spaces around names", []string{" auth =x;a=1 ;\tb=\"q\" "}, []string{`a=1; b="q"`}},
		{"repeated and empty pairs", []string{"auth=x;; auth=y; a=1", "auth=z"}, []string{"a=1"}},
		{"names are case sensitive", []string{"Auth=x; authx=y"}, []string{"Auth=x; authx=y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for _, line := range tc.in {
				h.Add("Cookie", line)
			}
			stripCookies(h, masterAuthCookies)
			require.Equal(t, tc.want, h.Values("Cookie"))
		})
	}
}
