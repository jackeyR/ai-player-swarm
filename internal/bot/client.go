package bot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/internal/ws"
	"aiplayer/swarm/srv/account"
)

// 客户端错误。
var (
	// ErrClientClosed 表示连接已被关闭。
	ErrClientClosed = errors.New("bot: gateway connection closed")
	// ErrTimeout 表示等待响应超时。
	ErrTimeout = errors.New("bot: timeout waiting for response")
)

// signLogin 复用服务端同一个签名函数（K2：协议兼容靠「同一份实现」守住）。
func signLogin(secret []byte, accountID string, ts int64, nonce string) string {
	return account.Sign(secret, accountID, ts, nonce)
}

// gwClient 是 bot 侧的接入层客户端。
//
// 设计要点：**同一个连接上并发多个请求**，靠信封里的 seq 做请求响应配对。
// 没有这个配对能力，就只能「发一个等一个」，bot 的行为立刻退化成不同步的脚本。
type gwClient struct {
	conn *ws.Conn
	seq  atomic.Uint32

	mu      sync.Mutex
	pending map[uint32]chan protocol.Envelope

	matches chan protocol.MatchFound
	appErrs chan protocol.Error
	closed  chan struct{}
	once    sync.Once
	readErr error
}

// gwConfig 是连接接入层的参数。
type gwConfig struct {
	URL             string
	DialTimeout     time.Duration
	ClientID        string
	Platform        string
	Seed            uint64
	ProtoVersion    int
	MaxMessageBytes int64
}

func dialGateway(ctx context.Context, cfg gwConfig) (*gwClient, error) {
	dctx := ctx
	if cfg.DialTimeout > 0 {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(ctx, cfg.DialTimeout)
		defer cancel()
	}
	conn, err := ws.Dial(dctx, cfg.URL, &ws.DialOptions{
		HandshakeTimeout: cfg.DialTimeout,
		MaxMessageSize:   cfg.MaxMessageBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("bot: 连接 gateway: %w", err)
	}
	c := &gwClient{
		conn:    conn,
		pending: make(map[uint32]chan protocol.Envelope, 8),
		matches: make(chan protocol.MatchFound, 4),
		appErrs: make(chan protocol.Error, 8),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// close 幂等关闭。
func (c *gwClient) close() {
	c.once.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

func (c *gwClient) readLoop() {
	defer c.close()
	for {
		op, payload, err := c.conn.ReadMessage()
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			return
		}
		if op != ws.BinaryMessage {
			continue
		}
		env, uerr := protocol.Unmarshal(payload, nil)
		if uerr != nil {
			continue
		}
		switch env.MsgID {
		case protocol.MsgMatchFound:
			var mf protocol.MatchFound
			if _, err := protocol.Unmarshal(payload, &mf); err == nil {
				select {
				case c.matches <- mf:
				default:
					// 匹配结果无处可放：说明 bot 侧比服务端还慢，直接丢。
				}
			}
			continue
		case protocol.MsgError:
			var perr protocol.Error
			_, _ = protocol.Unmarshal(payload, &perr)
			if env.Seq != 0 {
				c.deliver(env.Seq, env)
			} else {
				select {
				case c.appErrs <- perr:
				default:
				}
			}
			continue
		}
		c.deliver(env.Seq, env)
	}
}

// deliver 把响应交给等待者。
//
// 注意这里拷贝了消息体：ws 的接收缓冲可能被下一条消息复用，
// 跨 goroutine 传递前必须自己持有数据（这是自己写协议层最容易踩的坑）。
func (c *gwClient) deliver(seq uint32, env protocol.Envelope) {
	body := append([]byte(nil), env.Body...)
	c.mu.Lock()
	ch, ok := c.pending[seq]
	if ok {
		delete(c.pending, seq)
	}
	c.mu.Unlock()
	if ok {
		select {
		case ch <- protocol.Envelope{MsgID: env.MsgID, Seq: seq, Body: body}:
		default:
		}
	}
}

// call 发送请求并等待同 seq 的响应。
func (c *gwClient) call(ctx context.Context, msgID uint16, req any, resp any, timeout time.Duration) error {
	seq := c.seq.Add(1)
	body, err := protocol.Marshal(msgID, seq, req)
	if err != nil {
		return err
	}
	ch := make(chan protocol.Envelope, 1)
	c.mu.Lock()
	c.pending[seq] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
	}()

	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.conn.WriteMessage(ws.BinaryMessage, body); err != nil {
		return fmt.Errorf("bot: 写 %s: %w", protocol.MsgName(msgID), err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case env := <-ch:
		if env.MsgID == protocol.MsgError {
			var pe protocol.Error
			if _, err := protocol.Unmarshal(appendEnvelope(env), &pe); err != nil {
				return err
			}
			return &pe
		}
		if resp != nil {
			_, err := protocol.Unmarshal(appendEnvelope(env), resp)
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrClientClosed
	case <-timer.C:
		return fmt.Errorf("bot: 等 %s 响应: %w", protocol.MsgName(msgID), ErrTimeout)
	}
}

// appendEnvelope 把拆开的信封拼回完整字节，供二次 Unmarshal 使用。
func appendEnvelope(env protocol.Envelope) []byte {
	out := make([]byte, 6+len(env.Body))
	out[0] = byte(env.MsgID >> 8)
	out[1] = byte(env.MsgID)
	out[2] = byte(env.Seq >> 24)
	out[3] = byte(env.Seq >> 16)
	out[4] = byte(env.Seq >> 8)
	out[5] = byte(env.Seq)
	copy(out[6:], env.Body)
	return out
}

// handshake 完成 Hello → Welcome。
func (c *gwClient) handshake(ctx context.Context, cfg gwConfig) (protocol.Welcome, error) {
	var welcome protocol.Welcome
	hello := protocol.Hello{
		ProtoVersion: cfg.ProtoVersion,
		ClientID:     cfg.ClientID,
		Platform:     cfg.Platform,
		Seed:         cfg.Seed,
	}
	if err := c.call(ctx, protocol.MsgHello, hello, &welcome, 5*time.Second); err != nil {
		return welcome, err
	}
	if !welcome.OK {
		return welcome, fmt.Errorf("bot: 握手被拒: %v", welcome.Err)
	}
	return welcome, nil
}

// login 用账号签名登录。
func (c *gwClient) login(ctx context.Context, accountID string, initMMR int, nonce string, secret []byte) (protocol.LoginResp, error) {
	ts := time.Now().UnixMilli()
	req := protocol.LoginReq{
		AccountID:  accountID,
		ClientTsMs: ts,
		Nonce:      nonce,
		Sign:       signLogin(secret, accountID, ts, nonce),
		InitMMR:    initMMR,
	}
	var resp protocol.LoginResp
	if err := c.call(ctx, protocol.MsgLoginReq, req, &resp, 5*time.Second); err != nil {
		return resp, err
	}
	if !resp.OK {
		return resp, fmt.Errorf("bot: 登录失败: %v", resp.Err)
	}
	return resp, nil
}

// enterQueue 进队列。
func (c *gwClient) enterQueue(ctx context.Context, mode string) (protocol.EnterQueueResp, error) {
	var resp protocol.EnterQueueResp
	err := c.call(ctx, protocol.MsgEnterQueue, protocol.EnterQueueReq{Mode: mode}, &resp, 5*time.Second)
	return resp, err
}

// cancelQueue 取消排队。
func (c *gwClient) cancelQueue(ctx context.Context) (protocol.CancelQueueResp, error) {
	var resp protocol.CancelQueueResp
	err := c.call(ctx, protocol.MsgCancelQueue, nil, &resp, 5*time.Second)
	return resp, err
}

// waitMatch 等待匹配结果推送。
func (c *gwClient) waitMatch(ctx context.Context, timeout time.Duration) (protocol.MatchFound, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case mf := <-c.matches:
		return mf, nil
	case <-ctx.Done():
		return protocol.MatchFound{}, ctx.Err()
	case <-c.closed:
		return protocol.MatchFound{}, ErrClientClosed
	case <-timer.C:
		return protocol.MatchFound{}, fmt.Errorf("bot: 等匹配结果: %w", ErrTimeout)
	}
}

// ping 发应用层心跳，返回 RTT。
func (c *gwClient) ping(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	var pong protocol.Pong
	err := c.call(ctx, protocol.MsgPing, protocol.Ping{ClientNowMs: start.UnixMilli()}, &pong, 5*time.Second)
	if err != nil {
		return 0, err
	}
	return time.Since(start), nil
}
