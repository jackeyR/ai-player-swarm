package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Upgrader upgrades an HTTP request to a WebSocket connection.
type Upgrader struct {
	// HandshakeTimeout bounds the time spent writing the handshake response.
	HandshakeTimeout time.Duration
	// ReadBufferSize is the size of the read buffer. Zero means 4096.
	ReadBufferSize int
	// WriteBufferSize is the size of the write buffer. Zero means 4096.
	WriteBufferSize int
	// CheckOrigin may reject a request during the handshake. A nil function
	// allows every origin.
	CheckOrigin func(r *http.Request) bool
	// Subprotocols lists the subprotocols the server supports, in order of
	// preference.
	Subprotocols []string
	// MaxMessageSize is the largest message ReadMessage accepts. Zero means
	// 1 MiB.
	MaxMessageSize int64
}

// Upgrade completes the WebSocket handshake and returns the established
// connection. On failure it writes an HTTP error response itself and returns
// the error.
func (u *Upgrader) Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		return nil, upgradeError(w, http.StatusMethodNotAllowed, "request method is not GET")
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") {
		return nil, upgradeError(w, http.StatusBadRequest, "missing or invalid Connection header")
	}
	if !headerHasToken(r.Header, "Upgrade", "websocket") {
		return nil, upgradeError(w, http.StatusBadRequest, "missing or invalid Upgrade header")
	}
	if strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")) != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		return nil, upgradeError(w, http.StatusUpgradeRequired, "unsupported WebSocket version")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, upgradeError(w, http.StatusBadRequest, "missing Sec-WebSocket-Key header")
	}
	if u.CheckOrigin != nil && !u.CheckOrigin(r) {
		return nil, upgradeError(w, http.StatusForbidden, "origin not allowed")
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, upgradeError(w, http.StatusInternalServerError, "response writer does not support hijacking")
	}
	netConn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, fmt.Errorf("ws: hijack: %w", err)
	}

	if u.HandshakeTimeout > 0 {
		_ = netConn.SetWriteDeadline(time.Now().Add(u.HandshakeTimeout))
	}

	subprotocol := selectSubprotocol(u.Subprotocols, r)
	if _, err := fmt.Fprintf(rw,
		"HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\n",
		acceptKey(key)); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("ws: write handshake response: %w", err)
	}
	if subprotocol != "" {
		if _, err := fmt.Fprintf(rw, "Sec-WebSocket-Protocol: %s\r\n", subprotocol); err != nil {
			_ = netConn.Close()
			return nil, fmt.Errorf("ws: write handshake response: %w", err)
		}
	}
	if _, err := rw.WriteString("\r\n"); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("ws: write handshake response: %w", err)
	}
	if err := rw.Flush(); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("ws: write handshake response: %w", err)
	}
	// Clear any deadlines inherited from the HTTP server (and the handshake
	// timeout) so a long-lived WebSocket is not cut off.
	_ = netConn.SetDeadline(time.Time{})

	readSize := u.ReadBufferSize
	if readSize <= 0 {
		readSize = defaultBufferSize
	}
	writeSize := u.WriteBufferSize
	if writeSize <= 0 {
		writeSize = defaultBufferSize
	}
	maxSize := u.MaxMessageSize
	if maxSize <= 0 {
		maxSize = defaultMaxMessageSize
	}

	// Wrap the hijacked reader instead of replacing it: any frame bytes that
	// the HTTP server already buffered must remain readable.
	br := bufio.NewReaderSize(rw.Reader, readSize)
	bw := bufio.NewWriterSize(netConn, writeSize)
	return newConn(netConn, br, bw, true, subprotocol, maxSize), nil
}

func upgradeError(w http.ResponseWriter, status int, message string) error {
	http.Error(w, "websocket: "+message, status)
	return fmt.Errorf("ws: upgrade failed: %s", message)
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func selectSubprotocol(serverProtocols []string, r *http.Request) string {
	if len(serverProtocols) == 0 {
		return ""
	}
	offeredHeader := r.Header.Get("Sec-WebSocket-Protocol")
	if offeredHeader == "" {
		return ""
	}
	offered := strings.Split(offeredHeader, ",")
	for _, serverProto := range serverProtocols {
		if serverProto == "" {
			continue
		}
		for _, clientProto := range offered {
			if strings.TrimSpace(clientProto) == serverProto {
				return serverProto
			}
		}
	}
	return ""
}

func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}
