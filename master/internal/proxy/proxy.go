package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/go-cleanhttp"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
)

// Service represents a registered service. The LastRequested field is used by
// the Tensorboard manager to spin down idle instances of Tensorboard.
type Service struct {
	URL                  *url.URL
	LastRequested        time.Time
	ProxyTCP             bool
	AllowUnauthenticated bool
}

// Clone returns a deep copy of the Service.
func (s Service) Clone() Service {
	sURL := *s.URL
	return Service{
		URL:                  &sURL,
		LastRequested:        s.LastRequested,
		ProxyTCP:             s.ProxyTCP,
		AllowUnauthenticated: s.AllowUnauthenticated,
	}
}

// ProxyHTTPAuth processes a proxy request, returning true if the request should terminate
// immediately and an error if one was encountered during authentication.
type ProxyHTTPAuth func(echo.Context) (done bool, err error)

// IsMasterToken reports whether a token was issued by the master, whether or not its session is
// still valid. It only identifies the master's credentials to keep them from proxied services; it
// never authenticates anyone.
type IsMasterToken func(token string) bool

// Proxy is an actor that proxies requests to registered services.
type Proxy struct {
	lock          sync.RWMutex
	HTTPAuth      ProxyHTTPAuth
	IsMasterToken IsMasterToken
	services      map[string]*Service
	syslog        *logrus.Entry
}

// DefaultProxy is the global proxy singleton.
var DefaultProxy *Proxy

// InitProxy initializes the global proxy.
func InitProxy(httpAuth ProxyHTTPAuth, isMasterToken IsMasterToken) {
	if DefaultProxy != nil {
		logrus.Warn(
			"detected re-initialization of Proxy that should never occur outside of tests",
		)
	}
	DefaultProxy = &Proxy{
		HTTPAuth:      httpAuth,
		IsMasterToken: isMasterToken,
		services:      make(map[string]*Service),
		syslog:        logrus.WithField("component", "proxy"),
	}
	err := LoadOrGenCA()
	if err != nil {
		logrus.Errorf("error generating key and cert: %t", err)
	}
	err = LoadOrGenSignedMasterCert()
	if err != nil {
		logrus.Errorf("error generating key and cert: %t", err)
	}
}

// Register registers the service name with the associated target URL. All requests with the
// format ".../:service-name/*" are forwarded to the service via the target URL.
func (p *Proxy) Register(serviceID string, url *url.URL, proxyTCP bool, unauth bool) {
	if serviceID == "" {
		return
	}
	p.lock.Lock()
	defer p.lock.Unlock()

	p.syslog.Infof("registering service: %s (%v)", serviceID, url)
	p.services[serviceID] = &Service{
		URL:                  url,
		LastRequested:        time.Now(),
		ProxyTCP:             proxyTCP,
		AllowUnauthenticated: unauth,
	}
}

// Unregister removes the service from the proxy. All future requests until the service name is
// registered again will be responded with a 404 response. If the service is not registered with
// the proxy, the message is ignored.
func (p *Proxy) Unregister(serviceID string) {
	p.lock.Lock()
	defer p.lock.Unlock()
	delete(p.services, serviceID)
}

// ClearProxy erases all services from the proxy in case any handlers are still active.
func (p *Proxy) ClearProxy() {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.services = make(map[string]*Service)
}

// GetService returns the Service, if any, given the serviceID key.
func (p *Proxy) GetService(serviceID string) *Service {
	p.lock.Lock()
	defer p.lock.Unlock()
	service := p.services[serviceID]
	if service == nil {
		return nil
	}
	service.LastRequested = time.Now()

	// Make a copy to avoid callers mutating the object outside of this locked method.
	clone := service.Clone()
	return &clone
}

// NewProxyHandler returns a middleware function for proxying HTTP-like traffic to services
// running in the cluster. Services an HTTP request through the /proxy/:service/* route.
func (p *Proxy) NewProxyHandler(serviceID string) echo.HandlerFunc {
	return func(c echo.Context) error {
		// Look up the service name in the url path.
		serviceName := c.Param(serviceID)
		service := p.GetService(serviceName)

		if service == nil {
			return echo.NewHTTPError(http.StatusNotFound,
				fmt.Sprintf("service not found: %s", serviceName))
		}

		if !service.AllowUnauthenticated {
			switch done, err := p.HTTPAuth(c); {
			case err != nil:
				return err
			case done:
				return nil
			}
		}

		// The proxied service runs whatever its task's owner chose, so it must never see the
		// visitor's master credentials, whether or not the master authenticated the request.
		req := c.Request()
		stripMasterCredentials(req.Header, !service.AllowUnauthenticated, p.IsMasterToken)

		// Set proxy headers.
		if req.Header.Get(echo.HeaderXRealIP) == "" {
			req.Header.Set(echo.HeaderXRealIP, c.RealIP())
		}
		if req.Header.Get(echo.HeaderXForwardedProto) == "" {
			req.Header.Set(echo.HeaderXForwardedProto, c.Scheme())
		}
		if c.IsWebSocket() && req.Header.Get(echo.HeaderXForwardedFor) == "" {
			req.Header.Set(echo.HeaderXForwardedFor, c.RealIP())
		}

		// Proxy the request to the target host.
		var proxy http.Handler
		switch {
		case service.ProxyTCP:
			proxy = newSingleHostReverseTCPOverWebSocketProxy(c, service.URL)
		case c.IsWebSocket():
			proxy = newSingleHostReverseWebSocketProxy(c, service.URL)
		default:
			newProxy, err := setUpProxy(service.URL)
			if err != nil {
				return err
			}

			proxy = newProxy
		}
		proxy.ServeHTTP(c.Response(), req)

		return nil
	}
}

// masterAuthCookies are the cookies that carry a master session: "auth" is set at login and by
// the web UI, and "det_jwt" holds an external session token.
var masterAuthCookies = map[string]bool{"auth": true, "det_jwt": true}

// masterTokenHeaders are the headers that carry master tokens to the master's gRPC gateway as gRPC
// metadata. The Python SDK sends a task's session token in Grpc-Metadata-X-Allocation-Token.
var masterTokenHeaders = []string{
	"Grpc-Metadata-X-Allocation-Token",
	"Grpc-Metadata-X-User-Token",
	"Grpc-Metadata-Grpcgateway-Authorization",
}

// stripMasterCredentials removes the master's own credentials from a request that is about to be
// forwarded to a proxied service, whether or not the master authenticated it:
//   - the master's session cookies; other cookies, such as JupyterLab's, are kept as they are;
//   - the headers that only carry master tokens (masterTokenHeaders);
//   - Authorization headers with the Bearer scheme that hold a master token. When the master
//     authenticated the request (authenticated is true), every bearer token is one, since any
//     other fails master authentication. Otherwise, isMasterToken recognizes the master's tokens,
//     including ones whose session has ended, and the service keeps any other bearer token, which
//     may be its own credential.
//
// Other Authorization schemes are kept, such as the "token" scheme that JupyterLab uses for its
// notebook token.
func stripMasterCredentials(header http.Header, authenticated bool, isMasterToken IsMasterToken) {
	stripCookies(header, masterAuthCookies)
	for _, name := range masterTokenHeaders {
		header.Del(name)
	}

	values := header.Values(echo.HeaderAuthorization)
	header.Del(echo.HeaderAuthorization)
	for _, v := range values {
		fields := strings.Fields(v)
		if len(fields) > 0 && strings.EqualFold(fields[0], "Bearer") &&
			(authenticated || (len(fields) == 2 && isMasterToken != nil && isMasterToken(fields[1]))) {
			continue
		}
		header.Add(echo.HeaderAuthorization, v)
	}
}

// stripCookies removes the named cookies from the request's Cookie headers. Every other cookie is
// kept exactly as the client sent it; re-encoding would drop the quotes from values such as
// Tornado's signed cookies.
func stripCookies(header http.Header, names map[string]bool) {
	var kept []string
	removed := false
	for _, line := range header.Values(echo.HeaderCookie) {
		for _, pair := range strings.Split(line, ";") {
			pair = textproto.TrimString(pair)
			if pair == "" {
				continue
			}
			if name, _, _ := strings.Cut(pair, "="); names[textproto.TrimString(name)] {
				removed = true
				continue
			}
			kept = append(kept, pair)
		}
	}
	if !removed {
		return
	}

	header.Del(echo.HeaderCookie)
	if len(kept) > 0 {
		header.Set(echo.HeaderCookie, strings.Join(kept, "; "))
	}
}

func setUpProxy(serviceURL *url.URL) (*httputil.ReverseProxy, error) {
	proxy := httputil.NewSingleHostReverseProxy(serviceURL)
	if serviceURL.Scheme != https {
		return proxy, nil
	}
	keyBytes, certBytes, err := MasterKeyAndCert()
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certBytes, keyBytes)
	if err != nil {
		return nil, err
	}

	masterCaBytes, err := MasterCACert()
	if err != nil {
		return nil, err
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(masterCaBytes)

	transport := cleanhttp.DefaultTransport()
	transport.TLSClientConfig = &tls.Config{
		RootCAs:            caCertPool,
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, //nolint:gosec
		VerifyConnection:   VerifyMasterSigned,
	}
	proxy.Transport = transport

	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		req.Host = serviceURL.Host
	}

	return proxy, nil
}

// Summaries returns a snapshot of the registered services.
func (p *Proxy) Summaries() map[string]Service {
	p.lock.RLock()
	defer p.lock.RUnlock()

	snapshot := make(map[string]Service)
	for id, service := range p.services {
		snapshot[id] = service.Clone()
	}
	return snapshot
}

// Summary returns a snapshot of a specific registered service.
func (p *Proxy) Summary(id string) (Service, bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	service, ok := p.services[id]
	if !ok {
		return Service{}, false
	}
	return service.Clone(), true
}

func asyncCopy(dst io.Writer, src io.Reader) chan error {
	errs := make(chan error, 1)
	go func() {
		defer close(errs)
		_, err := io.Copy(dst, src)
		if err != io.EOF {
			errs <- err
		}
	}()
	return errs
}

// closedConnectionErrors are the errors with which a copy stops when a side ended the connection:
// the end of the stream, a connection closed here, and one that the other side closed (broken
// pipe) or reset.
var closedConnectionErrors = []error{
	io.EOF, io.ErrUnexpectedEOF, net.ErrClosed, syscall.EPIPE, syscall.ECONNRESET,
}

// isConnectionClosed reports whether err, an error from copying between the two sides of a
// proxied connection, only says that one side ended the connection, as SSH sessions through a
// shell and closed JupyterLab tabs do all the time: a WebSocket close with code 1000 (normal),
// 1001 (going away), 1005 (no status) or 1006 (closed without a close message), or one of
// closedConnectionErrors. Both may be wrapped in *net.OpError, as io.Copy returns them.
func isConnectionClosed(err error) bool {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return websocket.IsCloseError(closeErr, websocket.CloseNormalClosure,
			websocket.CloseGoingAway, websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure)
	}
	for _, closed := range closedConnectionErrors {
		if errors.Is(err, closed) {
			return true
		}
	}
	return false
}

// copyErrorLogf returns the method to log err, an error from copying between the two sides of a
// proxied connection, with: Debugf for an ordinary end of the connection, Errorf otherwise.
func copyErrorLogf(c echo.Context, err error) func(format string, args ...interface{}) {
	if isConnectionClosed(err) {
		return c.Logger().Debugf
	}
	return c.Logger().Errorf
}
