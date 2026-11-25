package ws

import (
	"bufio"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	closeNoStatusReceived = 1005
	closeProtocolError    = 1002
	closeTooLarge         = 1009
)

// Conn is a WebSocket connection.
//
// ReadMessage may be called from at most one goroutine at a time. WriteMessage,
// WritePing and WriteClose may be called concurrently from multiple
// goroutines; frames are written atomically with respect to one another.
type Conn struct {
	conn           net.Conn
	br             *bufio.Reader
	bw             *bufio.Writer
	isServer       bool
	subprotocol    string
	maxMessageSize int64

	writeMu sync.Mutex

	stateMu   sync.Mutex
	closed    bool
	sentClose bool
	recvClose bool

	closeOnce sync.Once
	closeErr  error
}

func newConn(conn net.Conn, br *bufio.Reader, bw *bufio.Writer, isServer bool, subprotocol string, maxMessageSize int64) *Conn {
	if maxMessageSize <= 0 {
		maxMessageSize = defaultMaxMessageSize
	}
	return &Conn{
		conn:           conn,
		br:             br,
		bw:             bw,
		isServer:       isServer,
		subprotocol:    subprotocol,
		maxMessageSize: maxMessageSize,
	}
}

// ReadMessage reads the next complete data message. Ping frames are answered
// with a Pong automatically and Pong frames are discarded, so they are never
// returned to the caller.
//
// When a Close frame is received, a Close frame is echoed (unless one was
// already sent) and ReadMessage returns either a *CloseError (when the peer
// initiated the close) or ErrClosed (when this endpoint initiated it).
func (c *Conn) ReadMessage() (OpCode, []byte, error) {
	if c.isClosed() {
		return 0, nil, ErrClosed
	}
	if c.wasCloseSent() && c.wasCloseRecv() {
		return 0, nil, ErrClosed
	}

	var msgOp OpCode
	var msg []byte
	inFragment := false

	for {
		hdr, err := readFrameHeader(c.br)
		if err != nil {
			if errors.Is(err, ErrProtocol) {
				_ = c.sendClose(closePayload(closeProtocolError, "invalid frame length"))
			}
			return 0, nil, c.translateReadError(err)
		}

		if hdr.rsv != 0 {
			return c.failProtocol(hdr, "reserved bits are not zero")
		}
		if !validOpcode(hdr.op) {
			return c.failProtocol(hdr, "invalid opcode")
		}

		if c.isServer {
			if !hdr.masked {
				return c.failProtocol(hdr, "client frame is not masked")
			}
		} else if hdr.masked {
			return c.failProtocol(hdr, "server frame is masked")
		}

		if isControlOpcode(hdr.op) {
			if !hdr.fin {
				return c.failProtocol(hdr, "control frame is fragmented")
			}
			if hdr.length > maxControlPayload {
				return c.failProtocol(hdr, "control frame payload exceeds 125 bytes")
			}
		} else {
			if hdr.op == ContinuationMessage {
				if !inFragment {
					return c.failProtocol(hdr, "unexpected continuation frame")
				}
				if int64(len(msg))+hdr.length > c.maxMessageSize {
					return c.failTooLarge(hdr)
				}
			} else {
				if inFragment {
					return c.failProtocol(hdr, "expected continuation frame")
				}
				if hdr.length > c.maxMessageSize {
					return c.failTooLarge(hdr)
				}
			}
		}

		payload, err := c.readPayload(hdr)
		if err != nil {
			return 0, nil, c.translateReadError(err)
		}

		if isControlOpcode(hdr.op) {
			switch hdr.op {
			case PingMessage:
				if !c.wasCloseSent() {
					if err := c.writeControl(PongMessage, payload); err != nil {
						return 0, nil, err
					}
				}
				continue
			case PongMessage:
				continue
			case CloseMessage:
				return c.handleClose(payload)
			}
		}

		if hdr.op == ContinuationMessage {
			msg = append(msg, payload...)
			if hdr.fin {
				return msgOp, msg, nil
			}
			continue
		}

		msgOp = hdr.op
		if hdr.fin {
			return msgOp, payload, nil
		}
		msg = append(msg, payload...)
		inFragment = true
	}
}

func (c *Conn) readPayload(hdr frameHeader) ([]byte, error) {
	var key [4]byte
	if hdr.masked {
		if _, err := io.ReadFull(c.br, key[:]); err != nil {
			return nil, err
		}
	}
	if hdr.length == 0 {
		return []byte{}, nil
	}
	payload := make([]byte, int(hdr.length))
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return nil, err
	}
	if hdr.masked {
		maskBytes(payload, key)
	}
	return payload, nil
}

// discardFrame consumes the remainder of a frame that is being rejected. This
// keeps unread bytes out of the receive buffer so closing the connection
// afterwards does not turn into a TCP reset that could hide the close frame.
func (c *Conn) discardFrame(hdr frameHeader) {
	if hdr.length > drainLimit {
		return
	}
	if c.conn != nil {
		_ = c.conn.SetReadDeadline(time.Now().Add(time.Second))
		defer c.conn.SetReadDeadline(time.Time{})
	}
	if hdr.masked {
		var key [4]byte
		if _, err := io.ReadFull(c.br, key[:]); err != nil {
			return
		}
	}
	if hdr.length <= 0 {
		return
	}
	_, _ = io.CopyN(io.Discard, c.br, hdr.length)
}

func (c *Conn) failProtocol(hdr frameHeader, reason string) (OpCode, []byte, error) {
	c.discardFrame(hdr)
	_ = c.sendClose(closePayload(closeProtocolError, reason))
	return 0, nil, fmt.Errorf("%w: %s", ErrProtocol, reason)
}

func (c *Conn) failTooLarge(hdr frameHeader) (OpCode, []byte, error) {
	c.discardFrame(hdr)
	_ = c.sendClose(closePayload(closeTooLarge, "message too big"))
	return 0, nil, ErrFrameTooLarge
}

func (c *Conn) handleClose(payload []byte) (OpCode, []byte, error) {
	if len(payload) == 1 {
		_ = c.sendClose(closePayload(closeProtocolError, "invalid close payload"))
		return 0, nil, fmt.Errorf("%w: invalid close payload", ErrProtocol)
	}

	code := closeNoStatusReceived
	reason := ""
	if len(payload) >= 2 {
		code = int(binary.BigEndian.Uint16(payload[:2]))
		reason = string(payload[2:])
	}

	initiated := c.wasCloseSent()
	if !initiated {
		// Echo the received status code and reason.
		_ = c.sendClose(payload)
	}
	c.markCloseRecv()

	if initiated {
		return 0, nil, ErrClosed
	}
	return 0, nil, &CloseError{Code: code, Reason: reason}
}

func (c *Conn) translateReadError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrProtocol) {
		return err
	}
	if errors.Is(err, net.ErrClosed) || err == io.EOF || err == io.ErrUnexpectedEOF || c.isClosed() || c.wasCloseRecv() {
		return ErrClosed
	}
	return err
}

// WriteMessage writes a single complete data message. Control frames must be
// sent with WritePing or WriteClose.
func (c *Conn) WriteMessage(op OpCode, payload []byte) error {
	switch op {
	case TextMessage, BinaryMessage, ContinuationMessage:
		return c.writeData(op, payload)
	case PingMessage:
		return c.WritePing(payload)
	case PongMessage:
		return c.writeControl(PongMessage, payload)
	case CloseMessage:
		return fmt.Errorf("%w: use WriteClose to send a close frame", ErrProtocol)
	default:
		return fmt.Errorf("%w: invalid opcode 0x%x", ErrProtocol, byte(op))
	}
}

// WritePing writes a Ping control frame with the given payload.
func (c *Conn) WritePing(payload []byte) error {
	if len(payload) > maxControlPayload {
		return fmt.Errorf("%w: control frame payload exceeds 125 bytes", ErrProtocol)
	}
	return c.writeControl(PingMessage, payload)
}

// WriteClose sends a Close control frame with the given status code and
// reason. A zero code sends an empty Close frame. Calling WriteClose more than
// once only sends one frame.
func (c *Conn) WriteClose(code uint16, reason string) error {
	var payload []byte
	if code != 0 {
		payload = closePayload(code, reason)
	}
	return c.sendClose(payload)
}

func (c *Conn) writeData(op OpCode, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.isClosed() {
		return ErrClosed
	}
	if c.wasCloseSent() {
		return ErrClosed
	}
	return c.mapWriteError(c.writeFrameLocked(op, payload, true))
}

func (c *Conn) writeControl(op OpCode, payload []byte) error {
	if len(payload) > maxControlPayload {
		return fmt.Errorf("%w: control frame payload exceeds 125 bytes", ErrProtocol)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.isClosed() {
		return ErrClosed
	}
	if c.wasCloseSent() {
		return ErrClosed
	}
	return c.mapWriteError(c.writeFrameLocked(op, payload, true))
}

func (c *Conn) sendClose(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.isClosed() {
		return ErrClosed
	}
	if c.wasCloseSent() {
		return nil
	}
	if len(payload) > maxControlPayload {
		payload = payload[:maxControlPayload]
	}
	if err := c.writeFrameLocked(CloseMessage, payload, true); err != nil {
		return c.mapWriteError(err)
	}
	c.markCloseSent()
	return nil
}

func (c *Conn) mapWriteError(err error) error {
	if err != nil && c.isClosed() {
		return ErrClosed
	}
	return err
}

// writeFrameLocked writes one complete frame and flushes the buffered writer.
// The caller must hold writeMu.
func (c *Conn) writeFrameLocked(op OpCode, payload []byte, fin bool) error {
	var header [14]byte
	n := 0

	first := byte(op)
	if fin {
		first |= 0x80
	}
	header[n] = first
	n++

	masked := !c.isServer
	var maskBit byte
	if masked {
		maskBit = 0x80
	}

	length := len(payload)
	switch {
	case length <= maxControlPayload:
		header[n] = maskBit | byte(length)
		n++
	case length <= 65535:
		header[n] = maskBit | 126
		n++
		binary.BigEndian.PutUint16(header[n:], uint16(length))
		n += 2
	default:
		header[n] = maskBit | 127
		n++
		binary.BigEndian.PutUint64(header[n:], uint64(length))
		n += 8
	}

	var maskKey [4]byte
	if masked {
		if _, err := crand.Read(maskKey[:]); err != nil {
			return fmt.Errorf("ws: cannot generate frame mask: %w", err)
		}
		copy(header[n:], maskKey[:])
		n += 4
	}

	if err := c.writeAll(header[:n]); err != nil {
		return err
	}
	if length > 0 {
		if masked {
			const chunkSize = 32 * 1024
			size := length
			if size > chunkSize {
				size = chunkSize
			}
			buf := make([]byte, size)
			for offset := 0; offset < length; {
				k := copy(buf, payload[offset:])
				for i := 0; i < k; i++ {
					buf[i] ^= maskKey[(offset+i)&3]
				}
				if err := c.writeAll(buf[:k]); err != nil {
					return err
				}
				offset += k
			}
		} else {
			if err := c.writeAll(payload); err != nil {
				return err
			}
		}
	}
	return c.bw.Flush()
}

func (c *Conn) writeAll(p []byte) error {
	for len(p) > 0 {
		n, err := c.bw.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// SetReadDeadline sets the deadline for future read operations.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the deadline for future write operations.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

// Close closes the underlying network connection. It is idempotent and may be
// called concurrently; blocked read and write operations are unblocked.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.stateMu.Lock()
		c.closed = true
		c.stateMu.Unlock()
		c.closeErr = c.conn.Close()
	})
	return c.closeErr
}

// LocalAddr returns the local network address.
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// Subprotocol returns the subprotocol negotiated during the handshake, or an
// empty string if none was selected.
func (c *Conn) Subprotocol() string {
	return c.subprotocol
}

func (c *Conn) isClosed() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.closed
}

func (c *Conn) wasCloseSent() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.sentClose
}

func (c *Conn) markCloseSent() {
	c.stateMu.Lock()
	c.sentClose = true
	c.stateMu.Unlock()
}

func (c *Conn) wasCloseRecv() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.recvClose
}

func (c *Conn) markCloseRecv() {
	c.stateMu.Lock()
	c.recvClose = true
	c.stateMu.Unlock()
}
