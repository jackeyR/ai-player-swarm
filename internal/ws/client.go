package ws

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DialOptions configures Dial.
type DialOptions struct {
	Header           http.Header
	HandshakeTimeout time.Duration
	Subprotocols     []string
	TLSConfig        *tls.Config
	ReadBufferSize   int
	WriteBufferSize  int
	MaxMessageSize   int64
}

// Dial connects to a ws:// or wss:// URL and performs the WebSocket
// handshake. opts may be nil.
func Dial(ctx context.Context, rawURL string, opts *DialOptions) (*Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("ws: parse URL %q: %w", rawURL, err)
	}
	var secure bool
	switch strings.ToLower(u.Scheme) {
	case "ws":
	case "wss":
		secure = true
	default:
		return nil, fmt.Errorf("ws: unsupported URL scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("ws: URL %q has no host", rawURL)
	}

	hostName := u.Hostname()
	if hostName == "" {
		return nil, fmt.Errorf("ws: URL %q has no host", rawURL)
	}
	port := u.Port()
	if port == "" {
		if secure {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(hostName, port)

	var o DialOptions
	if opts != nil {
		o = *opts
	}

	readSize := o.ReadBufferSize
	if readSize <= 0 {
		readSize = defaultBufferSize
	}
	writeSize := o.WriteBufferSize
	if writeSize <= 0 {
		writeSize = defaultBufferSize
	}
	maxSize := o.MaxMessageSize
	if maxSize <= 0 {
		maxSize = defaultMaxMessageSize
	}

	var deadline time.Time
	if o.HandshakeTimeout > 0 {
		deadline = time.Now().Add(o.HandshakeTimeout)
	}
	if d, ok := ctx.Deadline(); ok && (deadline.IsZero() || d.Before(deadline)) {
		deadline = d
	}

	dialer := &net.Dialer{}
	netConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ws: dial %s: %w", addr, err)
	}
	if !deadline.IsZero() {
		_ = netConn.SetDeadline(deadline)
	}

	if secure {
		cfg := o.TLSConfig
		if cfg == nil {
			cfg = &tls.Config{ServerName: hostName}
		} else if cfg.ServerName == "" {
			cfg = cfg.Clone()
			cfg.ServerName = hostName
		}
		tlsConn := tls.Client(netConn, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = netConn.Close()
			return nil, fmt.Errorf("ws: TLS handshake: %w", err)
		}
		netConn = tlsConn
	}

	var nonce [16]byte
	if _, err := crand.Read(nonce[:]); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("ws: cannot generate handshake key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	bw := bufio.NewWriterSize(netConn, writeSize)
	br := bufio.NewReaderSize(netConn, readSize)

	requestURI := u.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}
	fmt.Fprintf(bw, "GET %s HTTP/1.1\r\n", requestURI)
	fmt.Fprintf(bw, "Host: %s\r\n", u.Host)
	fmt.Fprint(bw, "Upgrade: websocket\r\n")
	fmt.Fprint(bw, "Connection: Upgrade\r\n")
	fmt.Fprintf(bw, "Sec-WebSocket-Key: %s\r\n", key)
	fmt.Fprint(bw, "Sec-WebSocket-Version: 13\r\n")
	if len(o.Subprotocols) > 0 {
		fmt.Fprintf(bw, "Sec-WebSocket-Protocol: %s\r\n", strings.Join(o.Subprotocols, ", "))
	}
	for name, values := range o.Header {
		if isReservedHeader(name) {
			continue
		}
		for _, value := range values {
			fmt.Fprintf(bw, "%s: %s\r\n", name, value)
		}
	}
	fmt.Fprint(bw, "\r\n")
	if err := bw.Flush(); err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("ws: write handshake request: %w", err)
	}

	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("ws: read handshake response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: unexpected status %s", ErrBadHandshake, resp.Status)
	}
	if !headerHasToken(resp.Header, "Upgrade", "websocket") {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: response is missing Upgrade: websocket", ErrBadHandshake)
	}
	if !headerHasToken(resp.Header, "Connection", "upgrade") {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: response is missing Connection: Upgrade", ErrBadHandshake)
	}
	if got := strings.TrimSpace(resp.Header.Get("Sec-WebSocket-Accept")); got != acceptKey(key) {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: invalid Sec-WebSocket-Accept value", ErrBadHandshake)
	}
	subprotocol := strings.TrimSpace(resp.Header.Get("Sec-WebSocket-Protocol"))
	if subprotocol != "" && !containsString(o.Subprotocols, subprotocol) {
		_ = netConn.Close()
		return nil, fmt.Errorf("%w: server selected unrequested subprotocol %q", ErrBadHandshake, subprotocol)
	}

	_ = netConn.SetDeadline(time.Time{})
	return newConn(netConn, br, bw, false, subprotocol, maxSize), nil
}

func isReservedHeader(name string) bool {
	switch strings.ToLower(name) {
	case "host", "upgrade", "connection", "sec-websocket-key",
		"sec-websocket-version", "sec-websocket-protocol":
		return true
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
