package ws

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	// wsGUID is the fixed UUID from RFC 6455 section 4.2.2.
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	defaultBufferSize     = 4096
	defaultMaxMessageSize = 1 << 20
	maxControlPayload     = 125

	// drainLimit bounds how much of a rejected frame is discarded before the
	// close frame is sent. Draining avoids a TCP reset that could hide the
	// close frame from the peer.
	drainLimit = 8 << 20
)

// frameHeader is the fixed part of a WebSocket frame.
type frameHeader struct {
	fin    bool
	rsv    byte
	op     OpCode
	masked bool
	length int64
}

// readFrameHeader reads and decodes a frame header from br. The payload (and
// masking key, if present) is left untouched. A frame length with the most
// significant bit set is rejected with ErrProtocol because RFC 6455 limits
// payload lengths to 63 bits.
func readFrameHeader(br *bufio.Reader) (frameHeader, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return frameHeader{}, err
	}

	h := frameHeader{
		fin:    hdr[0]&0x80 != 0,
		rsv:    (hdr[0] >> 4) & 0x07,
		op:     OpCode(hdr[0] & 0x0f),
		masked: hdr[1]&0x80 != 0,
	}
	length := int64(hdr[1] & 0x7f)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return frameHeader{}, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return frameHeader{}, err
		}
		u := binary.BigEndian.Uint64(ext[:])
		if u&(1<<63) != 0 {
			return frameHeader{}, fmt.Errorf("%w: invalid 64-bit payload length", ErrProtocol)
		}
		length = int64(u)
	}

	h.length = length
	return h, nil
}

func isControlOpcode(op OpCode) bool {
	return op >= 0x8
}

func validOpcode(op OpCode) bool {
	switch op {
	case ContinuationMessage, TextMessage, BinaryMessage,
		CloseMessage, PingMessage, PongMessage:
		return true
	}
	return false
}

// maskBytes applies the 4-byte WebSocket masking key to p.
func maskBytes(p []byte, key [4]byte) {
	for i := range p {
		p[i] ^= key[i&3]
	}
}

// closePayload builds a control frame payload containing a close code and an
// optional reason. The reason is truncated so the payload never exceeds the
// 125 byte limit for control frames.
func closePayload(code uint16, reason string) []byte {
	if len(reason) > maxControlPayload-2 {
		reason = reason[:maxControlPayload-2]
	}
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, code)
	copy(payload[2:], reason)
	return payload
}
