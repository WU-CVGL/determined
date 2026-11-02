package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// upstreamRequest is what reached a service: the request target as sent and the Host header.
type upstreamRequest struct {
	requestURI string
	host       string
}

var stripPrefixCases = []struct {
	name   string
	strip  bool
	target string
	want   string
}{
	{"path and query", true, "/proxy/svc:1/lab/tree?folder=%2Fwork&x=1", "/lab/tree?folder=%2Fwork&x=1"},
	{"service root", true, "/proxy/svc:1/", "/"},
	{"escaped path", true, "/proxy/svc:1/a%20b/c%2Fd?q", "/a%20b/c%2Fd?q"},
	{"off", false, "/proxy/svc:1/lab/tree?x=1", "/proxy/svc:1/lab/tree?x=1"},
	{"off, service root", false, "/proxy/svc:1/", "/proxy/svc:1/"},
}

func TestProxyStripsServicePrefixHTTP(t *testing.T) {
	for _, tc := range stripPrefixCases {
		t.Run(tc.name, func(t *testing.T) {
			var sawAuthCookie string
			p, base := newTestProxy(t, &sawAuthCookie)
			reached := make(chan upstreamRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached <- upstreamRequest{r.RequestURI, r.Host}
			}))
			defer upstream.Close()
			upstreamURL, err := url.Parse(upstream.URL)
			require.NoError(t, err)
			p.RegisterService("svc:1", Service{URL: upstreamURL, StripPrefix: tc.strip})

			resp, err := http.Get(base + tc.target) //nolint:noctx
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusOK, resp.StatusCode)
			got := <-reached
			require.Equal(t, tc.want, got.requestURI)
			// Plain HTTP forwards the master's own Host.
			require.Equal(t, strings.TrimPrefix(base, "http://"), got.host)
		})
	}
}

// TestProxyServiceIDPath checks how the router reads service IDs with a colon: the colon as is
// names the service; percent-encoded, it names no service, so the request never reaches one.
// Without a slash after the service ID, the request matches no route.
func TestProxyServiceIDPath(t *testing.T) {
	var sawAuthCookie string
	p, base := newTestProxy(t, &sawAuthCookie)
	reached := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.RequestURI
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	p.RegisterService("svc:1", Service{URL: upstreamURL, StripPrefix: true})

	get := func(target string) (int, string) {
		resp, err := http.Get(base + target) //nolint:noctx
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		select {
		case uri := <-reached:
			return resp.StatusCode, uri
		default:
			return resp.StatusCode, "body: " + string(body)
		}
	}

	code, uri := get("/proxy/svc:1/lab")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "/lab", uri)
	for _, target := range []string{"/proxy/svc%3A1/lab", "/proxy/svc%3a1/", "/proxy/svc:1"} {
		code, uri = get(target)
		require.Equal(t, http.StatusNotFound, code, target)
		require.True(t, strings.HasPrefix(uri, "body: "), "%s reached the service as %s", target, uri)
	}
}

func TestProxyStripsServicePrefixWebSocket(t *testing.T) {
	for _, tc := range stripPrefixCases {
		t.Run(tc.name, func(t *testing.T) {
			var sawAuthCookie string
			p, base := newTestProxy(t, &sawAuthCookie)

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer func() { _ = ln.Close() }()
			reached := make(chan upstreamRequest, 1)
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
				reached <- upstreamRequest{r.RequestURI, r.Host}
				_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
					"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
			}()
			p.RegisterService("svc:1", Service{
				URL:         &url.URL{Scheme: "http", Host: ln.Addr().String()},
				StripPrefix: tc.strip,
			})

			conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			_, err = conn.Write([]byte("GET " + tc.target + " HTTP/1.1\r\n" +
				"Host: master\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
				"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"))
			require.NoError(t, err)

			select {
			case got := <-reached:
				require.Equal(t, tc.want, got.requestURI)
				require.Equal(t, "master", got.host)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "the upgrade request never reached the service")
			}
		})
	}
}

func TestWithoutServicePrefixKeepsRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/proxy/svc:1/a%2Fb?x=1", nil)
	req.Host = "master"
	out := withoutServicePrefix(req, "a%2Fb")
	require.Equal(t, "/a%2Fb?x=1", out.URL.RequestURI())
	require.Equal(t, "/a/b", out.URL.Path)
	require.Equal(t, "/a%2Fb?x=1", out.RequestURI)
	require.Equal(t, "master", out.Host)
	require.Equal(t, "/proxy/svc:1/a%2Fb?x=1", req.URL.RequestURI())
	require.Equal(t, "/proxy/svc:1/a%2Fb?x=1", req.RequestURI)
}
