// Package ws implements the server and client sides of the WebSocket
// protocol defined by RFC 6455 using only the Go standard library.
//
// A Conn supports a single reader goroutine and any number of concurrent
// writers. Control frames (ping, pong and close) are handled internally:
// Ping frames are answered with a Pong and Pong frames are discarded by
// ReadMessage.
package ws

import (
	"errors"
	"fmt"
)

// OpCode is a WebSocket frame opcode.
type OpCode byte

const (
	ContinuationMessage OpCode = 0x0
	TextMessage         OpCode = 0x1
	BinaryMessage       OpCode = 0x2
	CloseMessage        OpCode = 0x8
	PingMessage         OpCode = 0x9
	PongMessage         OpCode = 0xA
)

var (
	// ErrClosed is returned after the peer or this endpoint has closed the
	// connection.
	ErrClosed = errors.New("ws: connection closed")
	// ErrBadHandshake is returned by Dial when the server rejects the
	// WebSocket handshake.
	ErrBadHandshake = errors.New("ws: bad handshake")
	// ErrFrameTooLarge is returned by ReadMessage when a message exceeds the
	// configured maximum message size.
	ErrFrameTooLarge = errors.New("ws: frame too large")
	// ErrProtocol is returned by ReadMessage when the peer violates the
	// WebSocket framing protocol. A close frame with code 1002 is sent before
	// the error is returned.
	ErrProtocol = errors.New("ws: protocol error")
)

// CloseError is returned by ReadMessage when a Close frame is received. Code
// is the close status code (1000, 1001, 1002, 1009, ...). When the peer sent
// an empty Close frame, Code is 1005 ("no status received") and Reason is
// empty.
type CloseError struct {
	Code   int
	Reason string
}

// Error implements the error interface.
func (e *CloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("ws: close error %d", e.Code)
	}
	return fmt.Sprintf("ws: close error %d: %s", e.Code, e.Reason)
}
