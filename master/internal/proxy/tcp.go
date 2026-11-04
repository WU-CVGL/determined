package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
	"golang.org/x/net/proxy"
)

// websocketReadWriter exposes an io.ReadWriter interface to a WebSocket connection that is only
// being used for binary communication.
type websocketReadWriter struct {
	ws  *websocket.Conn
	buf *bytes.Buffer
}

func (w *websocketReadWriter) Read(buf []byte) (int, error) {
	if w.buf.Len() > 0 {
		return w.buf.Read(buf)
	}
	for {
		switch msg, data, err := w.ws.ReadMessage(); {
		case err != nil:
			return 0, err
		case msg == websocket.CloseMessage:
			return 0, io.EOF
		case msg == websocket.BinaryMessage:
			if len(data) > 0 {
				w.buf.Write(data)
				return w.buf.Read(buf)
			}
		}
	}
}

func (w *websocketReadWriter) Write(buf []byte) (int, error) {
	if err := w.ws.WriteMessage(websocket.BinaryMessage, buf); err != nil {
		return 0, err
	}
	return len(buf), nil
}

// newSingleHostReverseTCPOverWebSocketProxy proxies a TCP connection to t over a WebSocket. The
// connection, such as an SSH client's, holds its task active from when it is accepted until it
// closes. It closes when either side ends it, also without a close handshake.
func newSingleHostReverseTCPOverWebSocketProxy(c echo.Context, t *url.URL, a activity) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialer := proxy.FromEnvironment()

		// Make sure we can open the connection to the remote host.
		out, err := dialer.Dial("tcp", t.Host)
		if err != nil {
			c.Error(echo.NewHTTPError(http.StatusBadGateway,
				errors.Errorf("error dialing to %v: %v", t, err)))
			return
		}
		var ws *websocket.Conn
		var closeOnce sync.Once
		closeConns := func() {
			closeOnce.Do(func() {
				if cerr := out.Close(); cerr != nil {
					c.Logger().Error(cerr)
				}
				if ws == nil {
					return
				}
				if cerr := ws.Close(); cerr != nil {
					c.Logger().Error(cerr)
				}
			})
		}
		defer closeConns()

		ws, err = (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			c.Error(echo.NewHTTPError(http.StatusBadGateway, errors.Wrap(err, "error upgrading")))
			return
		}

		release := a.hold()
		defer release()
		rw := &websocketReadWriter{ws: ws, buf: new(bytes.Buffer)}
		copyReqErr := asyncCopy(rw, out)
		copyResErr := asyncCopy(out, rw)

		// The first copy to end, on a close, an end of stream or an error of either side, closes
		// both connections, which ends the other copy: a client that goes away without closing
		// its WebSocket does not leave the service's connection open, nor the task held. The
		// other copy's error from that close is not logged.
		select {
		case err := <-copyReqErr:
			logCopyError(c, "request", t, err)
			closeConns()
			logCopyErrorAfterClose(c, "response", t, <-copyResErr)
		case err := <-copyResErr:
			logCopyError(c, "response", t, err)
			closeConns()
			logCopyErrorAfterClose(c, "request", t, <-copyReqErr)
		}
	})
}
