package room

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"aiplayer/swarm/internal/wire"
)

// peerOutQueue 是每个连接的发送队列长度。
//
// 有界队列就是背压（A14）：房间产出的快照比 socket 写得快时，宁可丢快照也不让
// tick goroutine 阻塞——一个慢客户端绝不能拖慢整间房的 10Hz 循环。
const peerOutQueue = 16

const peerWriteTimeout = 3 * time.Second

// peer 是对一个玩家连接的写侧封装。读侧由 manager 的连接 goroutine 负责。
type peer struct {
	slot uint8
	conn net.Conn
	w    *wire.Writer
	out  chan wire.Frame
	done chan struct{}

	// wmu 保护底层 socket 的写入。异步写（writeLoop）与同步写（sendSync）共用它，
	// 保证帧在连接上的顺序与调用顺序一致。
	wmu sync.Mutex

	once    sync.Once
	dropped atomic.Int64
	writes  atomic.Int64
}

func newPeer(slot uint8, c net.Conn) *peer {
	p := &peer{
		slot: slot,
		conn: c,
		w:    wire.NewWriter(c, wire.DefaultMaxBody),
		out:  make(chan wire.Frame, peerOutQueue),
		done: make(chan struct{}),
	}
	go p.writeLoop()
	return p
}

// send 非阻塞投递一帧。队列满时丢弃并返回 false（调用方计入 snapshot_dropped）。
//
// 只用于**可丢**的帧（快照、输入 ack）。结算这类决定成败的帧必须用 sendSync。
func (p *peer) send(f wire.Frame) bool {
	select {
	case <-p.done:
		return false
	default:
	}
	select {
	case p.out <- f:
		return true
	default:
		p.dropped.Add(1)
		return false
	}
}

// sendImportant 尝试把一帧塞进队列，队列满时最多等 200ms。
func (p *peer) sendImportant(f wire.Frame) bool {
	select {
	case <-p.done:
		return false
	default:
	}
	select {
	case p.out <- f:
		return true
	case <-time.After(200 * time.Millisecond):
		return false
	}
}

// sendSync 同步写一帧，返回是否成功。
//
// 为什么必须存在：结算帧发出去之后房间立刻就 Close 并关闭连接，
// 如果结算走异步队列，writeLoop 很可能还没轮到它就把连接关了——
// 结果就是「一局打完了，但 bot 侧记成中断」，goodput 直接被污染。
// 这类竞态在压测工具里是头号隐性 bug，因为它只在负载高的时候偶尔出现。
func (p *peer) sendSync(f wire.Frame, timeout time.Duration) bool {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(timeout))
	if err := p.w.WriteFrame(f); err != nil {
		return false
	}
	p.writes.Add(1)
	return true
}

func (p *peer) writeLoop() {
	defer p.conn.Close()
	for {
		select {
		case <-p.done:
			return
		case f := <-p.out:
			p.wmu.Lock()
			_ = p.conn.SetWriteDeadline(time.Now().Add(peerWriteTimeout))
			err := p.w.WriteFrame(f)
			p.wmu.Unlock()
			if err != nil {
				p.close()
				return
			}
			p.writes.Add(1)
		}
	}
}

// close 幂等关闭。
func (p *peer) close() {
	p.once.Do(func() {
		close(p.done)
		_ = p.conn.Close()
	})
}
