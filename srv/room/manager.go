package room

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"aiplayer/swarm/internal/obs"
	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/internal/wire"
	"aiplayer/swarm/srv/store"
)

// tokenTTL 是进房 token 的有效期。过期不候（D6：重连有窗口，不能无限期保留）。
const tokenTTL = 2 * time.Minute

type tokenEntry struct {
	roomID    string
	players   map[uint64]uint8 // playerID -> slot
	expiresAt time.Time
}

// Manager 管理一台房间服节点上的全部房间、进房 token 与 TCP 监听。
//
// 单进程托管 N 个房间是常见的「房间服」形态：一个节点跑几十到几百间房，
// 进程级故障会一次带走整批房间——这正是「房间服挂掉后的重连风暴」的物理来源（§2.7）。
type Manager struct {
	cfg    Config
	o      *obs.Server
	settle *store.Settlement
	hooks  Hooks

	mu     sync.Mutex
	rooms  map[string]*Room
	tokens map[string]*tokenEntry
	nextID atomic.Uint64
	leaked int

	advertise string

	ln   net.Listener
	wg   sync.WaitGroup
	stop chan struct{}
	once sync.Once
}

// NewManager 创建房间服管理器。
func NewManager(cfg Config, o *obs.Server, settle *store.Settlement, hooks Hooks) *Manager {
	cfg.Defaults()
	return &Manager{
		cfg:    cfg,
		o:      o,
		settle: settle,
		hooks:  hooks,
		rooms:  make(map[string]*Room, 256),
		tokens: make(map[string]*tokenEntry, 256),
		stop:   make(chan struct{}),
	}
}

// SetAdvertiseAddr 设置对外通告的地址（容器里监听 0.0.0.0，通告要给客户端连得上的 IP）。
func (m *Manager) SetAdvertiseAddr(addr string) { m.advertise = addr }

// AdvertiseAddr 返回客户端应连接的地址。
func (m *Manager) AdvertiseAddr() string {
	if m.advertise != "" {
		return m.advertise
	}
	if m.ln != nil {
		return m.ln.Addr().String()
	}
	return ""
}

// Listen 开始监听房间服的 TCP 端口。
func (m *Manager) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("room: 监听 %s: %w", addr, err)
	}
	m.ln = ln
	if m.advertise == "" {
		m.advertise = ln.Addr().String()
	}
	return nil
}

// Serve 接受连接并运行 janitor，阻塞到 ctx 取消或 Stop。
func (m *Manager) Serve(ctx context.Context) error {
	if m.ln == nil {
		return fmt.Errorf("room: 未调用 Listen")
	}
	go m.janitor(ctx)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			c, err := m.ln.Accept()
			if err != nil {
				select {
				case <-m.stop:
					return
				case <-ctx.Done():
					return
				default:
				}
				// 临时错误（EMFILE 等）不应该让房间服停摆：退避后继续 accept。
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if tc, ok := c.(*net.TCPConn); ok {
				// A11：延迟敏感的长连接必须关 Nagle，否则小包会被攒到约 40ms 才发出去，
				// 10Hz 的状态同步会被硬生生拖成 20Hz 的抖动。
				_ = tc.SetNoDelay(true)
			}
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				m.handleConn(ctx, c)
			}()
		}
	}()
	select {
	case <-m.stop:
	case <-ctx.Done():
		m.Stop()
	}
	return nil
}

// Stop 关闭监听与全部房间。
func (m *Manager) Stop() {
	m.once.Do(func() {
		close(m.stop)
		if m.ln != nil {
			_ = m.ln.Close()
		}
		m.mu.Lock()
		rooms := make([]*Room, 0, len(m.rooms))
		for _, r := range m.rooms {
			rooms = append(rooms, r)
		}
		m.rooms = make(map[string]*Room)
		m.mu.Unlock()
		for _, r := range rooms {
			r.Stop()
		}
	})
}

// Create 创建一间房并把进房 token 交出去。返回 token 与错误。
func (m *Manager) Create(mode, matchID string, seed uint64, specs []PlayerSpec) (string, error) {
	if len(specs) == 0 {
		return "", fmt.Errorf("room: 空对局")
	}
	id := fmt.Sprintf("room-%06d", m.nextID.Add(1))
	r := newRoom(id, mode, matchID, seed, m.cfg, m.o, m.settle, m.hooks)
	r.assign(specs)

	tokBytes := make([]byte, 12)
	if _, err := rand.Read(tokBytes); err != nil {
		return "", fmt.Errorf("room: 生成 token: %w", err)
	}
	tok := hex.EncodeToString(tokBytes)

	players := make(map[uint64]uint8, len(specs))
	for _, s := range specs {
		players[s.PlayerID] = s.Slot
	}

	m.mu.Lock()
	m.rooms[id] = r
	m.tokens[tok] = &tokenEntry{roomID: id, players: players, expiresAt: time.Now().Add(tokenTTL)}
	m.mu.Unlock()

	m.o.RoomsCreated.With(mode).Inc()

	// 房间主循环独立 goroutine。单个房间 panic 不应该带走整个进程。
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				r.Close(fmt.Sprintf("panic:%v", rec))
			}
		}()
		r.Run(context.Background())
	}()
	return tok, nil
}

func (m *Manager) lookupToken(token string) (*Room, map[uint64]uint8, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[token]
	if !ok || time.Now().After(t.expiresAt) {
		return nil, nil, false
	}
	r, ok := m.rooms[t.roomID]
	if !ok {
		return nil, nil, false
	}
	return r, t.players, true
}

func (m *Manager) handleConn(ctx context.Context, c net.Conn) {
	defer c.Close()

	rd := wire.NewReader(c, wire.DefaultMaxBody)
	// 首帧限时：连上不发进房请求的空连接不能长期占着 fd（A14）。
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := rd.ReadFrame()
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})

	switch f.MsgID {
	case protocol.MsgRoomJoinReq:
		req, err := protocol.DecodeRoomJoinReq(f.Body)
		if err != nil {
			return
		}
		m.servePlayer(ctx, c, rd, req.Token, req.PlayerID, false)
	case protocol.MsgReconnectReq:
		req, err := protocol.DecodeReconnectReq(f.Body)
		if err != nil {
			return
		}
		m.o.ReconnectTotal.With().Inc()
		if m.servePlayer(ctx, c, rd, req.Token, req.PlayerID, true) {
			m.o.ReconnectSuccess.With().Inc()
		}
	default:
		// 未知消息：静默断开，避免对端拿错误用法刷日志。
	}
}

// writeSync 同步写一帧，用于握手失败这类「必须送达再断开」的控制帧。
func writeSync(c net.Conn, f wire.Frame) {
	_ = c.SetWriteDeadline(time.Now().Add(time.Second))
	w := wire.NewWriter(c, wire.DefaultMaxBody)
	_ = w.WriteFrame(f)
}

// servePlayer 完成进房握手后进入读循环。返回是否算作一次成功重连。
func (m *Manager) servePlayer(ctx context.Context, c net.Conn, rd *wire.Reader, token string, playerID uint64, reconnect bool) bool {
	reject := func(code uint8) bool {
		writeSync(c, wire.Frame{MsgID: protocol.MsgRoomJoinResp,
			Body: (&protocol.RoomJoinResp{OK: false, ErrCode: code}).Encode()})
		return false
	}
	r, players, ok := m.lookupToken(token)
	if !ok {
		return reject(protocol.JoinErrBadToken)
	}
	if _, exists := players[playerID]; !exists {
		return reject(protocol.JoinErrBadToken)
	}

	resp, p := r.attach(playerID, c)
	if p == nil {
		return reject(resp.ErrCode)
	}
	if !p.sendImportant(wire.Frame{MsgID: protocol.MsgRoomJoinResp, Seq: 0, Body: resp.Encode()}) {
		r.detach(resp.Slot, p)
		return false
	}

	slot := resp.Slot
	defer r.detach(slot, p)

	for {
		f, err := rd.ReadFrame()
		if err != nil {
			return reconnect
		}
		switch f.MsgID {
		case protocol.MsgInput:
			in, derr := protocol.DecodeInput(f.Body)
			if derr != nil {
				continue
			}
			if !r.submitInput(slot, f.Seq, in) {
				// 房间输入队列满：直接丢弃并回一个 rate_limited 的 ack，
				// 让 bot 侧能看见「服务端正在背压」，而不是无声黑洞。
				p.send(wire.Frame{MsgID: protocol.MsgInputAck, Seq: f.Seq,
					Body: (&protocol.InputAck{Seq: f.Seq, Accepted: false, ErrCode: protocol.InputErrRateLimited}).Encode()})
			}
		case protocol.MsgRoomPing:
			r.touch(slot)
			p.send(wire.Frame{MsgID: protocol.MsgRoomPong, Seq: f.Seq, Body: f.Body})
		case protocol.MsgRoomLeave:
			return reconnect
		default:
			r.touch(slot)
		}
	}
}

// attach 把一个新连接绑定到房间内的玩家槽位。
func (r *Room) attach(playerID uint64, c net.Conn) (protocol.RoomJoinResp, *peer) {
	r.mu.Lock()

	var pl *roomPlayer
	for _, q := range r.players {
		if q != nil && q.spec.PlayerID == playerID {
			pl = q
			break
		}
	}
	if pl == nil {
		r.mu.Unlock()
		return protocol.RoomJoinResp{OK: false, ErrCode: protocol.JoinErrBadToken}, nil
	}
	if r.state == protocol.RoomStateClosed || r.state == protocol.RoomStateSettling {
		r.mu.Unlock()
		return protocol.RoomJoinResp{OK: false, ErrCode: protocol.JoinErrRoomClosed}, nil
	}

	old := pl.peer
	p := newPeer(pl.spec.Slot, c)
	pl.peer = p
	pl.connected = true
	pl.lastSeen = time.Now()
	if !pl.everConnected {
		pl.everConnected = true
		r.joined++
	}
	r.lastActivity = time.Now()

	resp := protocol.RoomJoinResp{
		OK:            true,
		Slot:          pl.spec.Slot,
		Players:       uint8(r.joined),
		Seed:          r.Seed,
		TickRateMs:    uint16(r.cfg.TickRateMs),
		DurationTicks: uint32(r.cfg.DurationTicks),
		StartAtMs:     uint64(r.startedAt.UnixMilli()),
		ServerNowMs:   uint64(time.Now().UnixMilli()),
	}
	if r.joined >= protocol.PlayersPerRoom && r.state == protocol.RoomStateWaiting {
		select {
		case r.fullCh <- struct{}{}:
		default:
		}
	}
	r.mu.Unlock()

	// 顶掉旧连接：断线重连后老连接可能还挂着（半开连接），必须断开，
	// 否则两个连接会同时往同一个槽位送输入（D6 里最常见的实现错误）。
	if old != nil {
		old.close()
	}
	return resp, p
}

// detach 解除连接绑定。
func (r *Room) detach(slot uint8, p *peer) {
	r.mu.Lock()
	if int(slot) < protocol.PlayersPerRoom {
		if pl := r.players[slot]; pl != nil && pl.peer == p {
			pl.connected = false
			pl.peer = nil
			pl.disconnectedAt = time.Now()
		}
	}
	r.lastActivity = time.Now()
	r.mu.Unlock()
	p.close()
}

// touch 刷新心跳时间戳。
func (r *Room) touch(slot uint8) {
	r.mu.Lock()
	if int(slot) < protocol.PlayersPerRoom {
		if pl := r.players[slot]; pl != nil {
			pl.lastSeen = time.Now()
		}
	}
	r.lastActivity = time.Now()
	r.mu.Unlock()
}

// submitInput 把输入投递给 tick goroutine。队列满返回 false（背压）。
func (r *Room) submitInput(slot uint8, seq uint32, in protocol.Input) bool {
	select {
	case <-r.stopCh:
		return false
	default:
	}
	select {
	case r.inputCh <- inputMsg{slot: slot, seq: seq, in: in, at: time.Now()}:
		return true
	default:
		return false
	}
}

// janitor 周期性回收房间（D4）。LeakInjection 打开时它「忘了」回收——
// 这就是我们要 bot 压出来的那个 bug。
func (m *Manager) janitor(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-t.C:
			m.sweep()
		}
	}
}

func (m *Manager) sweep() {
	now := time.Now()
	var toStop []*Room
	leaked := 0
	running := map[string]int{}
	players := 0

	m.mu.Lock()
	for id, r := range m.rooms {
		st := r.State()
		if st == protocol.RoomStateRunning {
			running[r.Mode]++
		}
		players += r.connectedPlayers()
		if r.ShouldReclaim(now) {
			if m.cfg.LeakInjection {
				r.markLeaked()
				leaked++
				continue
			}
			delete(m.rooms, id)
			toStop = append(toStop, r)
			m.o.RoomsClosed.With("janitor").Inc()
			continue
		}
		if r.Leaked() {
			leaked++
		}
	}
	for k, tok := range m.tokens {
		if now.After(tok.expiresAt) {
			delete(m.tokens, k)
		}
	}
	m.leaked = leaked
	m.mu.Unlock()

	for _, r := range toStop {
		r.Stop()
	}
	m.o.RoomLeak.With().Set(float64(leaked))
	m.o.ActivePlayers.With().Set(float64(players))
	m.o.ActiveMatches.With().Set(float64(running["ranked5v5"]))
	m.o.RoomsRunning.With("ranked5v5").Set(float64(running["ranked5v5"]))
}

// connectedPlayers 返回当前已连接的玩家数。
func (r *Room) connectedPlayers() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.players {
		if p != nil && p.connected {
			n++
		}
	}
	return n
}

// Stats 返回房间数与泄漏数。
func (m *Manager) Stats() (rooms int, leaked int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rooms), m.leaked
}

// RoomCount 返回房间总数（含已关闭但未回收的）。
func (m *Manager) RoomCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rooms)
}
