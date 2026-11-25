package ws_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"aiplayer/swarm/internal/ws"
)

const (
	// RFC 6455 section 1.3 sample handshake key and its accept value.
	testKey    = "dGhlIHNhbXBsZSBub25jZQ=="
	testAccept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
)

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func newWSServer(t *testing.T, upgrader *ws.Upgrader, handler func(*ws.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		handler(c)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func echoHandler(c *ws.Conn) {
	for {
		op, payload, err := c.ReadMessage()
		if err != nil {
			return
		}
		if err := c.WriteMessage(op, payload); err != nil {
			return
		}
	}
}

func dialWS(t *testing.T, srv *httptest.Server, opts *ws.DialOptions) *ws.Conn {
	t.Helper()
	c, err := ws.Dial(context.Background(), wsURL(srv), opts)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// rawHandshake performs the opening handshake over a raw TCP connection and
// checks the mandatory response headers.
func rawHandshake(t *testing.T, srv *httptest.Server, extraHeaders string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	request := fmt.Sprintf(
		"GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n%s\r\n",
		u.Host, testKey, extraHeaders)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write handshake request: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Upgrade"); !strings.EqualFold(got, "websocket") {
		t.Errorf("Upgrade header = %q, want websocket", got)
	}
	if got := resp.Header.Get("Connection"); !strings.EqualFold(got, "Upgrade") {
		t.Errorf("Connection header = %q, want Upgrade", got)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != testAccept {
		t.Errorf("Sec-WebSocket-Accept = %q, want %q", got, testAccept)
	}
	return conn, br, resp
}

// rawHTTP sends a handshake-shaped HTTP request over a raw TCP connection and
// returns the parsed response and its body.
func rawHTTP(t *testing.T, srv *httptest.Server, method, headers string) (*http.Response, string) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	request := fmt.Sprintf("%s / HTTP/1.1\r\nHost: %s\r\n%s\r\n", method, u.Host, headers)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	_ = resp.Body.Close()
	return resp, string(body)
}

type rawFrame struct {
	fin        bool
	op         ws.OpCode
	masked     bool
	lengthKind int // 7, 16 or 64; the wire encoding used for the length
	payload    []byte
}

func writeRawFrame(t *testing.T, w io.Writer, fin bool, op ws.OpCode, masked bool, payload []byte) {
	t.Helper()
	frame := make([]byte, 0, len(payload)+14)
	first := byte(op)
	if fin {
		first |= 0x80
	}
	frame = append(frame, first)

	var maskBit byte
	if masked {
		maskBit = 0x80
	}
	length := len(payload)
	switch {
	case length <= 125:
		frame = append(frame, maskBit|byte(length))
	case length <= 65535:
		frame = append(frame, maskBit|126, byte(length>>8), byte(length))
	default:
		frame = append(frame, maskBit|127)
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(length))
		frame = append(frame, size[:]...)
	}
	if masked {
		key := [4]byte{0xa1, 0xb2, 0xc3, 0xd4}
		frame = append(frame, key[:]...)
		for i, b := range payload {
			frame = append(frame, b^key[i&3])
		}
	} else {
		frame = append(frame, payload...)
	}
	if err := writeAll(w, frame); err != nil {
		t.Fatalf("write raw frame: %v", err)
	}
}

func readRawFrame(t *testing.T, br *bufio.Reader) rawFrame {
	t.Helper()
	var first [2]byte
	if _, err := io.ReadFull(br, first[:]); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	f := rawFrame{
		fin:        first[0]&0x80 != 0,
		op:         ws.OpCode(first[0] & 0x0f),
		masked:     first[1]&0x80 != 0,
		lengthKind: 7,
	}
	length := int64(first[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			t.Fatalf("read 16-bit length: %v", err)
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
		f.lengthKind = 16
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			t.Fatalf("read 64-bit length: %v", err)
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
		f.lengthKind = 64
	}
	if length < 0 || length > 1<<20 {
		t.Fatalf("unexpected frame length %d", length)
	}
	var key [4]byte
	if f.masked {
		if _, err := io.ReadFull(br, key[:]); err != nil {
			t.Fatalf("read mask key: %v", err)
		}
	}
	f.payload = make([]byte, int(length))
	if _, err := io.ReadFull(br, f.payload); err != nil {
		t.Fatalf("read frame payload: %v", err)
	}
	if f.masked {
		for i := range f.payload {
			f.payload[i] ^= key[i&3]
		}
	}
	return f
}

func readRawClose(t *testing.T, br *bufio.Reader) (int, string) {
	t.Helper()
	f := readRawFrame(t, br)
	if f.op != ws.CloseMessage {
		t.Fatalf("opcode = %#x, want close frame", f.op)
	}
	if len(f.payload) == 0 {
		return 1005, ""
	}
	if len(f.payload) < 2 {
		t.Fatalf("invalid close payload of %d byte(s)", len(f.payload))
	}
	return int(binary.BigEndian.Uint16(f.payload[:2])), string(f.payload[2:])
}

func testPayload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i % 251)
	}
	return p
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
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

func TestHandshakeSuccessAndSubprotocol(t *testing.T) {
	upgrader := &ws.Upgrader{Subprotocols: []string{"chat"}}
	srv := newWSServer(t, upgrader, echoHandler)

	conn, _, resp := rawHandshake(t, srv, "Sec-WebSocket-Protocol: chat\r\n")
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "chat" {
		t.Fatalf("selected subprotocol = %q, want chat", got)
	}
	_ = conn.Close()

	c := dialWS(t, srv, &ws.DialOptions{Subprotocols: []string{"chat"}})
	if got := c.Subprotocol(); got != "chat" {
		t.Fatalf("Subprotocol() = %q, want chat", got)
	}
	if c.LocalAddr() == nil || c.RemoteAddr() == nil {
		t.Fatal("LocalAddr/RemoteAddr returned nil")
	}
	if err := c.WriteMessage(ws.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	op, payload, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != ws.TextMessage || string(payload) != "hello" {
		t.Fatalf("echo = (%#x, %q), want (text, hello)", op, payload)
	}
}

func TestHandshakeErrors(t *testing.T) {
	srv := newWSServer(t, &ws.Upgrader{}, echoHandler)

	t.Run("unsupported version", func(t *testing.T) {
		resp, body := rawHTTP(t, srv, http.MethodGet,
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
				"Sec-WebSocket-Key: "+testKey+"\r\nSec-WebSocket-Version: 12\r\n")
		if resp.StatusCode != http.StatusUpgradeRequired {
			t.Errorf("status = %d, want 426", resp.StatusCode)
		}
		if got := resp.Header.Get("Sec-WebSocket-Version"); got != "13" {
			t.Errorf("Sec-WebSocket-Version response header = %q, want 13", got)
		}
		if body == "" {
			t.Error("426 response has an empty body")
		}
	})

	t.Run("missing key", func(t *testing.T) {
		resp, body := rawHTTP(t, srv, http.MethodGet,
			"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		if body == "" {
			t.Error("400 response has an empty body")
		}
	})

	t.Run("non GET", func(t *testing.T) {
		resp, body := rawHTTP(t, srv, http.MethodPost,
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
				"Sec-WebSocket-Key: "+testKey+"\r\nSec-WebSocket-Version: 13\r\n")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != http.MethodGet {
			t.Errorf("Allow header = %q, want GET", got)
		}
		if body == "" {
			t.Error("405 response has an empty body")
		}
	})
}

func TestUpgradeWithoutHijacker(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", testKey)
	rec := httptest.NewRecorder()

	if _, err := (&ws.Upgrader{}).Upgrade(rec, req); err == nil {
		t.Fatal("Upgrade without http.Hijacker support returned nil error")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestEchoTextAndBinary(t *testing.T) {
	srv := newWSServer(t, &ws.Upgrader{}, echoHandler)
	c := dialWS(t, srv, nil)

	cases := []struct {
		name    string
		op      ws.OpCode
		payload []byte
	}{
		{"text", ws.TextMessage, []byte("hello, 涓栫晫")},
		{"binary", ws.BinaryMessage, []byte{0x00, 0x01, 0xfe, 0xff}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.WriteMessage(tc.op, tc.payload); err != nil {
				t.Fatalf("WriteMessage: %v", err)
			}
			op, payload, err := c.ReadMessage()
			if err != nil {
				t.Fatalf("ReadMessage: %v", err)
			}
			if op != tc.op {
				t.Errorf("opcode = %#x, want %#x", op, tc.op)
			}
			if !bytes.Equal(payload, tc.payload) {
				t.Errorf("payload mismatch: got %d bytes, want %d", len(payload), len(tc.payload))
			}
		})
	}
}

func TestPayloadBoundaryLengths(t *testing.T) {
	srv := newWSServer(t, &ws.Upgrader{}, echoHandler)
	c := dialWS(t, srv, nil)

	for _, n := range []int{0, 125, 126, 65535, 65536} {
		t.Run(fmt.Sprintf("%d", n), func(t *testing.T) {
			payload := testPayload(n)
			if err := c.WriteMessage(ws.BinaryMessage, payload); err != nil {
				t.Fatalf("WriteMessage(%d bytes): %v", n, err)
			}
			op, got, err := c.ReadMessage()
			if err != nil {
				t.Fatalf("ReadMessage(%d bytes): %v", n, err)
			}
			if op != ws.BinaryMessage {
				t.Errorf("opcode = %#x, want binary", op)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("payload mismatch for %d bytes", n)
			}
		})
	}
}

func TestExtendedLengthEncoding(t *testing.T) {
	srv := newWSServer(t, &ws.Upgrader{}, echoHandler)
	conn, br, _ := rawHandshake(t, srv, "")

	cases := []struct {
		size int
		kind int
	}{
		{125, 7},
		{126, 16},
		{65535, 16},
		{65536, 64},
	}
	for _, tc := range cases {
		payload := testPayload(tc.size)
		writeRawFrame(t, conn, true, ws.BinaryMessage, true, payload)

		f := readRawFrame(t, br)
		if f.op != ws.BinaryMessage || !f.fin {
			t.Fatalf("size %d: got op=%#x fin=%v, want binary fin", tc.size, f.op, f.fin)
		}
		if f.masked {
			t.Errorf("size %d: server frame is masked", tc.size)
		}
		if f.lengthKind != tc.kind {
			t.Errorf("size %d: server used %d-bit length encoding, want %d-bit", tc.size, f.lengthKind, tc.kind)
		}
		if !bytes.Equal(f.payload, payload) {
			t.Errorf("size %d: echoed payload mismatch", tc.size)
		}
	}
}

func TestUnmaskedClientFrameRejected(t *testing.T) {
	serverErr := make(chan error, 1)
	srv := newWSServer(t, &ws.Upgrader{}, func(c *ws.Conn) {
		_, _, err := c.ReadMessage()
		serverErr <- err
	})
	conn, br, _ := rawHandshake(t, srv, "")

	writeRawFrame(t, conn, true, ws.TextMessage, false, []byte("this frame is not masked"))
	code, _ := readRawClose(t, br)
	if code != 1002 {
		t.Fatalf("close code = %d, want 1002", code)
	}

	select {
	case err := <-serverErr:
		if !errors.Is(err, ws.ErrProtocol) {
			t.Fatalf("server ReadMessage error = %v, want ErrProtocol", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server read error")
	}
}

func TestFragmentedMessageWithInterleavedPing(t *testing.T) {
	srv := newWSServer(t, &ws.Upgrader{}, echoHandler)
	conn, br, _ := rawHandshake(t, srv, "")

	writeRawFrame(t, conn, false, ws.TextMessage, true, []byte("Hello, "))
	writeRawFrame(t, conn, true, ws.PingMessage, true, []byte("ping-1"))

	f := readRawFrame(t, br)
	if f.op != ws.PongMessage || !bytes.Equal(f.payload, []byte("ping-1")) {
		t.Fatalf("got op=%#x payload=%q, want pong ping-1", f.op, f.payload)
	}

	writeRawFrame(t, conn, false, ws.ContinuationMessage, true, []byte("frag"))
	writeRawFrame(t, conn, true, ws.PingMessage, true, []byte("ping-2"))
	f = readRawFrame(t, br)
	if f.op != ws.PongMessage || !bytes.Equal(f.payload, []byte("ping-2")) {
		t.Fatalf("got op=%#x payload=%q, want pong ping-2", f.op, f.payload)
	}

	writeRawFrame(t, conn, true, ws.ContinuationMessage, true, []byte("mented"))
	f = readRawFrame(t, br)
	if f.op != ws.TextMessage {
		t.Fatalf("opcode = %#x, want text", f.op)
	}
	if got, want := string(f.payload), "Hello, fragmented"; got != want {
		t.Fatalf("reassembled message = %q, want %q", got, want)
	}
}

func TestFragmentProtocolViolation(t *testing.T) {
	srv := newWSServer(t, &ws.Upgrader{}, echoHandler)
	conn, br, _ := rawHandshake(t, srv, "")

	// A new data frame while a fragmented message is open is a protocol error.
	writeRawFrame(t, conn, false, ws.TextMessage, true, []byte("first"))
	writeRawFrame(t, conn, true, ws.TextMessage, true, []byte("second"))
	code, _ := readRawClose(t, br)
	if code != 1002 {
		t.Fatalf("close code = %d, want 1002", code)
	}
}

func TestPingPong(t *testing.T) {
	srv := newWSServer(t, &ws.Upgrader{}, echoHandler)
	conn, br, _ := rawHandshake(t, srv, "")

	for _, payload := range [][]byte{nil, []byte("hello"), testPayload(125)} {
		writeRawFrame(t, conn, true, ws.PingMessage, true, payload)
		f := readRawFrame(t, br)
		if f.op != ws.PongMessage {
			t.Fatalf("opcode = %#x, want pong", f.op)
		}
		if !bytes.Equal(f.payload, payload) {
			t.Fatalf("pong payload mismatch: got %d bytes, want %d", len(f.payload), len(payload))
		}
	}
}

func TestCloseHandshake(t *testing.T) {
	t.Run("client initiated", func(t *testing.T) {
		serverErr := make(chan error, 1)
		srv := newWSServer(t, &ws.Upgrader{}, func(c *ws.Conn) {
			_, _, err := c.ReadMessage()
			serverErr <- err
		})
		c := dialWS(t, srv, nil)

		if err := c.WriteClose(1001, "going away"); err != nil {
			t.Fatalf("WriteClose: %v", err)
		}
		_, _, err := c.ReadMessage()
		var closeErr *ws.CloseError
		if !errors.Is(err, ws.ErrClosed) && !errors.As(err, &closeErr) {
			t.Fatalf("client ReadMessage after close = %v, want ErrClosed or CloseError", err)
		}

		select {
		case err := <-serverErr:
			if !errors.As(err, &closeErr) {
				t.Fatalf("server ReadMessage error = %v, want *CloseError", err)
			}
			if closeErr.Code != 1001 || closeErr.Reason != "going away" {
				t.Fatalf("CloseError = {%d, %q}, want {1001, going away}", closeErr.Code, closeErr.Reason)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for server close error")
		}
	})

	t.Run("server initiated", func(t *testing.T) {
		done := make(chan struct{})
		srv := newWSServer(t, &ws.Upgrader{}, func(c *ws.Conn) {
			_ = c.WriteClose(1000, "server bye")
			_, _, _ = c.ReadMessage()
			close(done)
		})
		c := dialWS(t, srv, nil)

		_, _, err := c.ReadMessage()
		var closeErr *ws.CloseError
		if !errors.As(err, &closeErr) {
			t.Fatalf("ReadMessage = %v, want *CloseError", err)
		}
		if closeErr.Code != 1000 || closeErr.Reason != "server bye" {
			t.Fatalf("CloseError = {%d, %q}, want {1000, server bye}", closeErr.Code, closeErr.Reason)
		}
		if closeErr.Error() == "" {
			t.Error("CloseError.Error() is empty")
		}

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for server close handshake")
		}
	})
}

func TestCloseIdempotent(t *testing.T) {
	srv := newWSServer(t, &ws.Upgrader{}, echoHandler)
	c := dialWS(t, srv, nil)

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := c.ReadMessage(); !errors.Is(err, ws.ErrClosed) {
		t.Fatalf("ReadMessage after Close = %v, want ErrClosed", err)
	}
}

func TestCloseConcurrent(t *testing.T) {
	srv := newWSServer(t, &ws.Upgrader{}, echoHandler)
	c := dialWS(t, srv, nil)

	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	}
	if _, _, err := c.ReadMessage(); !errors.Is(err, ws.ErrClosed) {
		t.Fatalf("ReadMessage after Close = %v, want ErrClosed", err)
	}
}

func TestCloseUnblocksRead(t *testing.T) {
	srv := newWSServer(t, &ws.Upgrader{}, echoHandler)
	c := dialWS(t, srv, nil)

	readErr := make(chan error, 1)
	go func() {
		_, _, err := c.ReadMessage()
		readErr <- err
	}()
	time.Sleep(20 * time.Millisecond) // let the goroutine block in ReadMessage
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-readErr:
		if !errors.Is(err, ws.ErrClosed) {
			t.Fatalf("blocked ReadMessage returned %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock ReadMessage")
	}
}

func TestDialTLS(t *testing.T) {
	upgrader := &ws.Upgrader{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		echoHandler(c)
	}))
	t.Cleanup(srv.Close)

	c, err := ws.Dial(context.Background(), wsURL(srv), &ws.DialOptions{
		TLSConfig:        &tls.Config{InsecureSkipVerify: true}, // test server uses a self-signed certificate
		HandshakeTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial wss: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if err := c.WriteMessage(ws.TextMessage, []byte("over tls")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	op, payload, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if op != ws.TextMessage || string(payload) != "over tls" {
		t.Fatalf("echo = (%#x, %q), want (text, over tls)", op, payload)
	}
}

func TestMaxMessageSize(t *testing.T) {
	serverErr := make(chan error, 1)
	upgrader := &ws.Upgrader{MaxMessageSize: 32}
	srv := newWSServer(t, upgrader, func(c *ws.Conn) {
		_, _, err := c.ReadMessage()
		serverErr <- err
	})
	c := dialWS(t, srv, nil)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))

	if err := c.WriteMessage(ws.BinaryMessage, testPayload(100)); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	select {
	case err := <-serverErr:
		if !errors.Is(err, ws.ErrFrameTooLarge) {
			t.Fatalf("server ReadMessage error = %v, want ErrFrameTooLarge", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server size error")
	}

	_, _, err := c.ReadMessage()
	var closeErr *ws.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("client ReadMessage = %v, want *CloseError with code 1009", err)
	}
	if closeErr.Code != 1009 {
		t.Fatalf("close code = %d, want 1009", closeErr.Code)
	}
}

func TestConcurrentWrites(t *testing.T) {
	const (
		writers     = 8
		perWriter   = 100
		totalMsgs   = writers * perWriter
		testTimeout = 30 * time.Second
	)

	srv := newWSServer(t, &ws.Upgrader{}, echoHandler)
	c := dialWS(t, srv, nil)
	_ = c.SetReadDeadline(time.Now().Add(testTimeout))
	_ = c.SetWriteDeadline(time.Now().Add(testTimeout))

	seen := make(map[string]int, totalMsgs)
	readDone := make(chan error, 1)
	go func() {
		for i := 0; i < totalMsgs; i++ {
			_, payload, err := c.ReadMessage()
			if err != nil {
				readDone <- fmt.Errorf("read %d/%d: %w", i, totalMsgs, err)
				return
			}
			seen[string(payload)]++
		}
		readDone <- nil
	}()

	var wg sync.WaitGroup
	writeErrs := make(chan error, totalMsgs)
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				msg := fmt.Sprintf("writer-%02d-message-%03d-%s", g, i, strings.Repeat("x", 24))
				if err := c.WriteMessage(ws.TextMessage, []byte(msg)); err != nil {
					writeErrs <- err
					return
				}
				if i%20 == 0 {
					if err := c.WritePing([]byte("ping")); err != nil {
						writeErrs <- err
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(writeErrs)
	for err := range writeErrs {
		t.Errorf("concurrent write: %v", err)
	}

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for concurrent echo messages")
	}

	if len(seen) != totalMsgs {
		t.Errorf("received %d distinct messages, want %d", len(seen), totalMsgs)
	}
	for g := 0; g < writers; g++ {
		for i := 0; i < perWriter; i++ {
			msg := fmt.Sprintf("writer-%02d-message-%03d-%s", g, i, strings.Repeat("x", 24))
			if seen[msg] != 1 {
				t.Errorf("message %q was received %d times, want exactly once", msg, seen[msg])
			}
		}
	}
}

func TestDialErrors(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := ws.Dial(ctx, "ws://"+addr, nil); err == nil {
			t.Fatal("Dial to a closed port succeeded, want error")
		}
	})

	t.Run("non 101 response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "not a websocket server")
		}))
		t.Cleanup(srv.Close)

		_, err := ws.Dial(context.Background(), wsURL(srv), &ws.DialOptions{HandshakeTimeout: 3 * time.Second})
		if err == nil {
			t.Fatal("Dial to a non-101 response succeeded, want error")
		}
		if !errors.Is(err, ws.ErrBadHandshake) {
			t.Fatalf("Dial error = %v, want ErrBadHandshake", err)
		}
	})

	t.Run("bad scheme", func(t *testing.T) {
		if _, err := ws.Dial(context.Background(), "http://127.0.0.1:1/", nil); err == nil {
			t.Fatal("Dial with http scheme succeeded, want error")
		}
	})
}
