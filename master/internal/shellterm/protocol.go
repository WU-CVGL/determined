// Package shellterm bridges a browser terminal (a WebSocket) to an interactive SSH session in a
// shell task's container. The master authenticates to the shell's sshd with the shell's own key,
// so the key never leaves the master.
//
// The package holds only the transport. Authentication, authorization and the lookup of the shell
// happen in the caller before a session starts.
//
// Wire protocol, one WebSocket per terminal session:
//   - Browser to master, binary frame: raw terminal input.
//   - Browser to master, text frame: a JSON control message. Only {"type":"resize","cols":C,"rows":R}
//     is understood; anything else is ignored.
//   - Master to browser, binary frame: raw terminal output.
//   - Master to browser, text frame: a JSON control message, {"type":"ready"} once the terminal is
//     open, {"type":"exit","code":N} when the login shell exits, or {"type":"error","code":...}.
//   - The master ends the session with one of the close codes below.
package shellterm

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocket close codes the master uses for terminal sessions.
const (
	// CloseNormal ends a session whose login shell exited.
	CloseNormal = websocket.CloseNormalClosure
	// CloseGoingAway ends sessions when the master shuts down.
	CloseGoingAway = websocket.CloseGoingAway
	// CloseInternal ends a session after an unexpected error in the master.
	CloseInternal = websocket.CloseInternalServerErr
	// CloseUnauthenticated ends a session whose login session ended, expired or was revoked, or
	// whose user was deactivated.
	CloseUnauthenticated = 4401
	// CloseForbidden ends a session that the user may no longer open, for example after losing
	// admin rights, or whose shell is gone.
	CloseForbidden = 4403
	// CloseTimeout ends a session that was idle too long or reached the maximum session length.
	CloseTimeout = 4408
	// CloseNotReady refuses a session because the shell is not ready yet. Clients may retry.
	CloseNotReady = 4409
	// CloseEnded refuses a session because the shell has ended.
	CloseEnded = 4410
	// CloseUnavailable ends a session because the master could not reach the shell's SSH server,
	// or lost the connection to it. Dial and handshake failures share this code.
	CloseUnavailable = 4502
)

// Error codes sent in {"type":"error"} control messages. Messages are fixed strings; details stay
// in the master's log.
const (
	ErrCodeNotReady       = "not_ready"
	ErrCodeEnded          = "ended"
	ErrCodeUnavailable    = "unavailable"
	ErrCodeUnauthenticate = "session_expired"
	ErrCodeForbidden      = "forbidden"
	ErrCodeIdle           = "idle_timeout"
	ErrCodeTimeLimit      = "time_limit"
	ErrCodeShutdown       = "shutdown"
	ErrCodeInternal       = "internal"
)

// Control message types.
const (
	msgResize = "resize"
	msgReady  = "ready"
	msgExit   = "exit"
	msgError  = "error"
)

// Terminal size limits applied to the initial size and to every resize.
const (
	DefaultCols = 80
	DefaultRows = 24
	maxDim      = 1000
)

// ControlMessage is a JSON control message sent in a text frame.
type ControlMessage struct {
	Type    string `json:"type"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Code    any    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// CloseError asks a session to end with a WebSocket close code. Cancel a session's context with
// context.WithCancelCause and a CloseError to end it with that code. ErrCode and Message, when
// set, are sent in an error control message before the close frame.
type CloseError struct {
	Code    int
	ErrCode string
	Message string
	// Detail is logged on the master and never sent to the browser.
	Detail string
}

func (e CloseError) Error() string {
	msg := fmt.Sprintf("terminal closed (%d)", e.Code)
	if e.ErrCode != "" {
		msg = fmt.Sprintf("terminal closed (%d %s)", e.Code, e.ErrCode)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// NewCloseError returns a CloseError with the standard message for errCode.
func NewCloseError(code int, errCode, detail string) CloseError {
	return CloseError{Code: code, ErrCode: errCode, Message: errorMessages[errCode], Detail: detail}
}

var errorMessages = map[string]string{
	ErrCodeNotReady:       "The shell is not ready yet.",
	ErrCodeEnded:          "The shell has ended.",
	ErrCodeUnavailable:    "Could not connect to the shell.",
	ErrCodeUnauthenticate: "Your login session has ended.",
	ErrCodeForbidden:      "You can no longer use this shell.",
	ErrCodeIdle:           "The terminal was idle for too long.",
	ErrCodeTimeLimit:      "The terminal reached its maximum session length.",
	ErrCodeShutdown:       "The master is shutting down.",
	ErrCodeInternal:       "Internal error.",
}

// ClampSize returns a terminal size within 1..1000 columns and rows. Zero or negative values fall
// back to the defaults.
func ClampSize(cols, rows int) (clampedCols, clampedRows int) {
	clamp := func(v, def int) int {
		switch {
		case v <= 0:
			return def
		case v > maxDim:
			return maxDim
		default:
			return v
		}
	}
	return clamp(cols, DefaultCols), clamp(rows, DefaultRows)
}

func encodeControl(m ControlMessage) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		// ControlMessage holds only strings and numbers.
		panic(err)
	}
	return b
}

// Reject ends a WebSocket before a session starts: it sends the error control message and the
// close frame of ce, then closes the connection.
func Reject(ws *websocket.Conn, ce CloseError, writeWait time.Duration) {
	defer func() { _ = ws.Close() }()
	if writeWait <= 0 {
		writeWait = DefaultWriteWait
	}
	deadline := time.Now().Add(writeWait)
	if ce.ErrCode != "" {
		msg := ControlMessage{Type: msgError, Code: ce.ErrCode, Message: ce.Message}
		if ws.SetWriteDeadline(deadline) != nil ||
			ws.WriteMessage(websocket.TextMessage, encodeControl(msg)) != nil {
			return
		}
	}
	if ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(ce.Code, ce.ErrCode),
		deadline) != nil {
		return
	}
	// Wait briefly for the browser's close frame, so that it sees the code.
	_ = ws.SetReadDeadline(time.Now().Add(closeGrace))
	for {
		if _, _, err := ws.NextReader(); err != nil {
			return
		}
	}
}
