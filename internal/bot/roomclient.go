package bot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/internal/wire"
)

// 房间连接相关的错误。
var (
	// ErrRoomClosed 表示房间连接已关闭。
	ErrRoomClosed = errors.New("bot: room connection closed")
)

// roomClient 是 bot 侧的房间服客户端：裸 TCP + 长度前缀二进制帧。
//
// 为什么对局不走 WebSocket：数据面每 tick 都要收发定长小包，
// 也不需要 HTTP 语义。裸 TCP + 定长头部把每包开销压到 10 字节，
// 未来换成 KCP/UDP 时上层协议不用动（A1 / A4）。
type roomClient struct {
	conn net.Conn
	rd   *wire.Reader
	w    *wire.Writer
	seq  atomic.Uint32

	snapshots chan protocol.Snapshot
	acks      chan protocol.InputAck
	settles   chan protocol.Settle
	joinResp  chan protocol.RoomJoinResp
	progress  chan protocol.SessionProgress

	closed  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	readErr error
}

func dialRoom(ctx context.Context, addr string, timeout time.Duration) (*roomClient, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bot: 连接房间服 %s: %w", addr, err)
	}
	// A11：延迟敏感，必须关 Nagle。
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	c := &roomClient{
		conn:      conn,
		rd:        wire.NewReader(conn, wire.DefaultMaxBody),
		w:         wire.NewWriter(conn, wire.DefaultMaxBody),
		snapshots: make(chan protocol.Snapshot, 8),
		acks:      make(chan protocol.InputAck, 64),
		settles:   make(chan protocol.Settle, 1),
		joinResp:  make(chan protocol.RoomJoinResp, 1),
		progress:  make(chan protocol.SessionProgress, 8),
		closed:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *roomClient) close() {
	c.once.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

func (c *roomClient) readLoop() {
	defer c.close()
	for {
		f, err := c.rd.ReadFrame()
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			return
		}
		switch f.MsgID {
		case protocol.MsgSnapshot:
			snap, err := protocol.DecodeSnapshot(f.Body)
			if err != nil {
				continue
			}
			select {
			case c.snapshots <- snap:
			default:
				// bot 侧跟不上服务端的 10Hz：丢快照而不是阻塞读循环。
				// 这条丢弃本身就是「bot 先成为瓶颈」的证据，由 runner 侧统计。
			}
		case protocol.MsgInputAck:
			ack, err := protocol.DecodeInputAck(f.Body)
			if err != nil {
				continue
			}
			select {
			case c.acks <- ack:
			default:
			}
		case protocol.MsgSettle:
			st, err := protocol.DecodeSettle(f.Body)
			if err != nil {
				continue
			}
			select {
			case c.settles <- st:
			default:
			}
		case protocol.MsgRoomJoinResp:
			resp, err := protocol.DecodeRoomJoinResp(f.Body)
			if err != nil {
				continue
			}
			select {
			case c.joinResp <- resp:
			default:
			}
		case protocol.MsgReconnectResp:
			resp, err := protocol.DecodeRoomJoinResp(f.Body)
			if err != nil {
				continue
			}
			select {
			case c.joinResp <- resp:
			default:
			}
		case protocol.MsgSessionProgress:
			p, err := protocol.DecodeSessionProgress(f.Body)
			if err != nil {
				continue
			}
			select {
			case c.progress <- p:
			default:
			}
		case protocol.MsgRoomClose:
			return
		}
	}
}

// join 进房（首次或重连）。
func (c *roomClient) join(ctx context.Context, token string, playerID uint64, reconnect bool) (protocol.RoomJoinResp, error) {
	seq := c.seq.Add(1)
	var body []byte
	msgID := protocol.MsgRoomJoinReq
	if reconnect {
		msgID = protocol.MsgReconnectReq
		body = (&protocol.ReconnectReq{Token: token, PlayerID: playerID}).Encode()
	} else {
		body = (&protocol.RoomJoinReq{Token: token, PlayerID: playerID}).Encode()
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.w.WriteFrame(wire.Frame{MsgID: msgID, Seq: seq, Body: body}); err != nil {
		return protocol.RoomJoinResp{}, fmt.Errorf("bot: 发送进房请求: %w", err)
	}

	ch := c.joinResp
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return protocol.RoomJoinResp{}, ctx.Err()
	case <-c.closed:
		return protocol.RoomJoinResp{}, ErrRoomClosed
	case <-timer.C:
		return protocol.RoomJoinResp{}, fmt.Errorf("bot: 等进房应答: %w", ErrTimeout)
	}
}

// sendInput 发送一条输入。seq 用于和服务端 ack 配对。
func (c *roomClient) sendInput(in protocol.Input) (uint32, error) {
	seq := c.seq.Add(1)
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := c.w.WriteFrame(wire.Frame{MsgID: protocol.MsgInput, Seq: seq, Body: in.Encode()}); err != nil {
		return seq, err
	}
	return seq, nil
}

// ping 房间层心跳。
func (c *roomClient) ping() error {
	seq := c.seq.Add(1)
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return c.w.WriteFrame(wire.Frame{MsgID: protocol.MsgRoomPing, Seq: seq, Body: nil})
}

// leave 主动离房。
func (c *roomClient) leave() error {
	seq := c.seq.Add(1)
	_ = c.conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
	return c.w.WriteFrame(wire.Frame{MsgID: protocol.MsgRoomLeave, Seq: seq, Body: nil})
}

// waitSettle 等待结算。timeout <= 0 表示一直等。
func (c *roomClient) waitSettle(ctx context.Context, timeout time.Duration) (protocol.Settle, error) {
	var timerC <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timerC = timer.C
	}
	select {
	case st := <-c.settles:
		return st, nil
	case <-ctx.Done():
		return protocol.Settle{}, ctx.Err()
	case <-c.closed:
		return protocol.Settle{}, ErrRoomClosed
	case <-timerC:
		return protocol.Settle{}, fmt.Errorf("bot: 等结算: %w", ErrTimeout)
	}
}
