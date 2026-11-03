package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"math"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
)

func connectWithbackoff(tlsConn *tls.Conn) error {
	var err error
	for x := 0; x < 3; x++ {
		err = tlsConn.Handshake()
		if err == nil {
			return nil
		}
		time.Sleep(time.Duration(math.Pow(2, float64(x))) * time.Second)
	}
	return err
}

// newSingleHostReverseWebSocketProxy proxies a WebSocket connection to t. The connection holds
// its task active from the forwarded upgrade request until it closes. It closes when either side
// ends it, also without a close handshake.
func newSingleHostReverseWebSocketProxy(c echo.Context, t *url.URL, a activity) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in, _, err := c.Response().Hijack()
		if err != nil {
			c.Error(errors.Errorf("error hijacking connection to %v: %v", t, err))
			return
		}
		var out net.Conn
		var closeOnce sync.Once
		closeConns := func() {
			closeOnce.Do(func() {
				if cerr := in.Close(); cerr != nil {
					c.Logger().Error(cerr)
				}
				if out == nil {
					return
				}
				if cerr := out.Close(); cerr != nil {
					c.Logger().Error(cerr)
				}
			})
		}
		defer closeConns()

		out, err = net.Dial("tcp", t.Host)
		if err != nil {
			c.Error(echo.NewHTTPError(http.StatusBadGateway,
				errors.Errorf("error dialing to %v: %v", t, err)))
			return
		}

		if t.Scheme == https {
			keyBytes, certBytes, err := MasterKeyAndCert()
			if err != nil {
				c.Error(echo.NewHTTPError(http.StatusBadGateway,
					errors.Errorf("error getting tls key or cert: %v", err)))
				return
			}
			cert, err := tls.X509KeyPair(certBytes, keyBytes)
			if err != nil {
				return
			}

			masterCaBytes, err := MasterCACert()
			if err != nil {
				return
			}
			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(masterCaBytes)

			//nolint:gosec
			tlsConfig := &tls.Config{
				ServerName:         "127.0.0.1",
				Certificates:       []tls.Certificate{cert},
				RootCAs:            caCertPool,
				InsecureSkipVerify: true,
				VerifyConnection:   VerifyMasterSigned,
			}
			tlsConn := tls.Client(out, tlsConfig)
			out = tlsConn

			err = connectWithbackoff(tlsConn)
			if err != nil {
				c.Error(echo.NewHTTPError(http.StatusBadGateway,
					errors.Errorf("tls handshake error %v", err)))
				return
			}
		}

		err = r.Write(out)
		if err != nil {
			c.Error(echo.NewHTTPError(http.StatusBadGateway,
				errors.Errorf("error copying headers for %v: %v", t, err)))
			return
		}

		release := a.hold()
		defer release()
		copyReqErr := asyncCopy(out, in)
		copyResErr := asyncCopy(in, out)
		// The first copy to end, on a close, an end of stream or an error of either side, closes
		// both connections, which ends the other copy: a client that goes away without a close
		// handshake does not leave the service's connection open, nor the task held. The other
		// copy's error from that close is not logged.
		logOther := func(direction string, err error) {
			if !errors.Is(err, net.ErrClosed) {
				logCopyError(c, direction, t, err)
			}
		}
		select {
		case err := <-copyReqErr:
			logCopyError(c, "request", t, err)
			closeConns()
			logOther("response", <-copyResErr)
		case err := <-copyResErr:
			logCopyError(c, "response", t, err)
			closeConns()
			logOther("request", <-copyReqErr)
		}
	})
}
