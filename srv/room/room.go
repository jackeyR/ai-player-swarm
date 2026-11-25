// Package room 实现被测系统的房间服：10Hz 固定步长 tick、权威服务器裁决、
// 状态同步、结算与回收。
//
// 对应知识清单 D1（权威服务器）、D2（tick 与 budget）、D3（状态同步）、
// D4（房间生命周期）、D6（重连）、D7（心跳超时与踢人）。
//
// 这一层是整个项目的靶子：bot 压的就是它的 tick 预算、结算写库与回收逻辑。
package room

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"aiplayer/swarm/internal/clock"
	"aiplayer/swarm/internal/obs"
	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/internal/wire"
	"aiplayer/swarm/srv/store"
)

// ErrRoomClosed 表示房间已关闭。
var ErrRoomClosed = errors.New("room: closed")

// Config 是房间服配置。刻意做成可调的：压测的价值一半来自于「改一个参数，看指标怎么动」。
type Config struct {
	// TickRateMs 是 tick 步长（毫秒），10Hz => 100。
	TickRateMs int `yaml:"tick_rate_ms" json:"tick_rate_ms"`
	// DurationTicks 是一局的总 tick 数，600 => 60s。
	DurationTicks int `yaml:"duration_ticks" json:"duration_ticks"`
	// FillTimeoutMs 是等人进房的最长等待时间。
	FillTimeoutMs int `yaml:"fill_timeout_ms" json:"fill_timeout_ms"`
	// EmptyTTLMs 是房间全空后多久回收。
	EmptyTTLMs int `yaml:"empty_ttl_ms" json:"empty_ttl_ms"`
	// HeartbeatTimeoutMs 是心跳超时判定（D7）。
	HeartbeatTimeoutMs int `yaml:"heartbeat_timeout_ms" json:"heartbeat_timeout_ms"`
	// BudgetMs 是单帧预算（D2），默认 16。
	BudgetMs int `yaml:"budget_ms" json:"budget_ms"`
	// StartDelayMs 是进房后到开局的延迟，给客户端加载时间。
	StartDelayMs int `yaml:"start_delay_ms" json:"start_delay_ms"`
	// LeakInjection 打开后，janitor 不会真正回收房间，用来复现「空房泄漏」这个
	// 最隐蔽的故障（D4）。默认关闭，findings 里用它做对照实验。
	LeakInjection bool `yaml:"leak_injection" json:"leak_injection"`
}

// Defaults 填充默认值。
func (c *Config) Defaults() {
	if c.TickRateMs <= 0 {
		c.TickRateMs = 100
	}
	if c.DurationTicks <= 0 {
		c.DurationTicks = 600
	}
	if c.FillTimeoutMs <= 0 {
		c.FillTimeoutMs = 5000
	}
	if c.EmptyTTLMs <= 0 {
		c.EmptyTTLMs = 10000
	}
	if c.HeartbeatTimeoutMs <= 0 {
		c.HeartbeatTimeoutMs = 5000
	}
	if c.BudgetMs <= 0 {
		c.BudgetMs = 16
	}
	if c.StartDelayMs < 0 {
		c.StartDelayMs = 0
	}
}

func (c Config) tickStep() time.Duration { return time.Duration(c.TickRateMs) * time.Millisecond }

// PlayerSpec 是匹配器交给房间的一席。
type PlayerSpec struct {
	PlayerID uint64
	Team     uint8
	Slot     uint8
	MMR      int
}

// PlayerOutcome 是结算时单个玩家的战绩。
type PlayerOutcome struct {
	PlayerID  uint64
	Slot      uint8
	Team      uint8
	Kills     uint8
	Deaths    uint8
	Score     uint16
	MMR       int
	Connected bool
}

// Summary 是房间关闭后交给外部的汇总（用于更新 MMR、写结果、统计泄漏）。
type Summary struct {
	RoomID         string
	MatchID        string
	Mode           string
	Result         uint8
	Ticks          uint32
	CreatedAt      time.Time
	StartedAt      time.Time
	ClosedAt       time.Time
	Reason         string
	Filled         bool
	Settled        bool
	Players        []PlayerOutcome
	InputsAccepted int
	InputsRejected int
}

// roomPlayer 是房间内的玩家状态。所有字段只在持有 Room.mu 时读写。
type roomPlayer struct {
	spec           PlayerSpec
	x, y           int16
	hp             uint16
	kills          uint8
	deaths         uint8
	score          uint16
	peer           *peer
	connected      bool
	everConnected  bool
	lastSeen       time.Time
	inputsThisTick int
	accepted       int
	rejected       int
	disconnectedAt time.Time
}

// inputMsg 是从连接 goroutine 投递到 tick goroutine 的输入。
type inputMsg struct {
	slot uint8
	seq  uint32
	in   protocol.Input
	at   time.Time
}

// Room 是一个房间实例。
type Room struct {
	ID      string
	Mode    string
	MatchID string
	Seed    uint64

	cfg    Config
	o      *obs.Server
	settle *store.Settlement
	hooks  Hooks

	mu             sync.Mutex
	players        [protocol.PlayersPerRoom]*roomPlayer
	joined         int
	state          uint8
	tick           uint32
	result         uint8
	createdAt      time.Time
	startedAt      time.Time
	closedAt       time.Time
	reported       bool
	reason         string
	filled         bool
	settled        bool
	leaked         bool
	lastActivity   time.Time
	inputsAccepted int
	inputsRejected int

	inputCh   chan inputMsg
	fullCh    chan struct{}
	stopCh    chan struct{}
	doneCh    chan struct{}
	stopOnce  sync.Once
	closeOnce sync.Once
}

// Hooks 是房间生命周期回调，由 manager 注入。
type Hooks struct {
	OnClosed func(Summary)
}

// newRoom 由 manager 调用。
func newRoom(id, mode, matchID string, seed uint64, cfg Config, o *obs.Server, settle *store.Settlement, hooks Hooks) *Room {
	now := time.Now()
	return &Room{
		ID:           id,
		Mode:         mode,
		MatchID:      matchID,
		Seed:         seed,
		cfg:          cfg,
		o:            o,
		settle:       settle,
		hooks:        hooks,
		state:        protocol.RoomStateWaiting,
		createdAt:    now,
		lastActivity: now,
		inputCh:      make(chan inputMsg, 256),
		fullCh:       make(chan struct{}, 1),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}
}

// assign 把匹配结果写进房间（创建后、开局前调用一次）。
func (r *Room) assign(specs []PlayerSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range specs {
		s := specs[i]
		slot := int(s.Slot)
		if slot < 0 || slot >= protocol.PlayersPerRoom {
			continue
		}
		spawnX, spawnY := spawnPoint(s.Team, slot)
		r.players[slot] = &roomPlayer{
			spec: s,
			x:    spawnX,
			y:    spawnY,
			hp:   protocol.MaxHp,
		}
	}
}

// spawnPoint 把两队分散在地图两端，但**不能太远**：
// 如果两队出生点间隔等于整张地图，10Hz、1.5m/s 的移动在短对局里根本走不到一起，
// 结果就是「所有对局都是 0:0 平局」——压测能跑通，但胜负逻辑一行代码都没被执行到。
// 这里把两队放在 45% / 55% 位置，纵向间距控制在 ±1/5 地图内，保证短对局也能真实交火。
func spawnPoint(team uint8, slot int) (int16, int16) {
	spread := int16(protocol.WorldSize/20) * int16(slot%protocol.TeamSize-(protocol.TeamSize/2))
	x := int16(protocol.WorldSize * 45 / 100)
	if team == 1 {
		x = int16(protocol.WorldSize * 55 / 100)
	}
	y := int16(protocol.WorldSize/2) + spread
	return x, y
}

// Run 是房间主循环。由 manager 在创建后以 goroutine 方式启动。
func (r *Room) Run(ctx context.Context) {
	defer close(r.doneCh)

	// 阶段一：等人进房（D4 的 waiting 段）。
	fillTimer := time.NewTimer(time.Duration(r.cfg.FillTimeoutMs) * time.Millisecond)
	defer fillTimer.Stop()
	select {
	case <-ctx.Done():
		r.Close("shutdown")
		return
	case <-r.stopCh:
		r.Close("stopped")
		return
	case <-r.fullCh:
	case <-fillTimer.C:
		r.mu.Lock()
		joined := r.joined
		r.mu.Unlock()
		if joined < protocol.PlayersPerRoom {
			r.o.RoomFillTimeout.With().Inc()
		}
		if joined == 0 {
			// 一个人都没来：房间直接空转结束，交给 janitor 回收。
			// LeakInjection 打开时它不会被回收，room_leak_gauge 会把它数出来。
			r.Close("empty")
			return
		}
	}

	// 阶段二：开局，按绝对时间表 10Hz tick。
	r.mu.Lock()
	r.state = protocol.RoomStateRunning
	r.startedAt = time.Now().Add(time.Duration(r.cfg.StartDelayMs) * time.Millisecond)
	r.filled = r.joined >= protocol.PlayersPerRoom
	r.mu.Unlock()

	stepper := clock.NewStepper(r.cfg.tickStep(), 1)
	defer stepper.Stop()

	for {
		select {
		case <-ctx.Done():
			r.Close("shutdown")
			return
		case <-r.stopCh:
			r.Close("stopped")
			return
		case t := <-stepper.C():
			if r.tickOnce(ctx, t) {
				return
			}
		}
	}
}

// tickOnce 执行一拍。返回 true 表示房间已结算结束。
func (r *Room) tickOnce(ctx context.Context, t clock.Tick) bool {
	start := time.Now()
	r.o.TickLag.With().Observe(t.Lag.Seconds())

	r.mu.Lock()
	r.tick++
	tickNo := r.tick
	r.drainInputsLocked()
	var dead []*peer
	if tickNo%10 == 0 {
		dead = r.checkHeartbeatsLocked(time.Now())
	}
	payload := r.encodeSnapshotLocked(tickNo)
	peers := r.activePeersLocked()
	r.mu.Unlock()

	for _, p := range dead {
		p.close()
	}
	frame := wire.Frame{MsgID: protocol.MsgSnapshot, Seq: tickNo, Body: payload}
	for _, p := range peers {
		if !p.send(frame) {
			r.o.SnapshotDropped.With().Inc()
		}
	}
	if len(peers) > 0 {
		r.o.SnapshotSent.With().Add(float64(len(peers)))
	}

	if tickNo%10 == 0 && len(peers) > 0 {
		prog := protocol.SessionProgress{
			Tick:       tickNo,
			TotalTicks: uint32(r.cfg.DurationTicks),
			ElapsedMs:  uint32(int(tickNo) * r.cfg.TickRateMs),
		}
		pf := wire.Frame{MsgID: protocol.MsgSessionProgress, Seq: tickNo, Body: prog.Encode()}
		for _, p := range peers {
			p.send(pf)
		}
	}

	settled := false
	if int(tickNo) >= r.cfg.DurationTicks {
		r.finalize(ctx, tickNo)
		settled = true
	}

	dur := time.Since(start)
	r.o.TickDuration.With().Observe(dur.Seconds())
	r.o.TickTotal.With().Inc()
	if dur > time.Duration(r.cfg.BudgetMs)*time.Millisecond {
		r.o.TickOverrun.With().Inc()
	}
	return settled
}

// drainInputsLocked 把队列里的输入搬进房间状态。
//
// 每拍最多处理 3 倍人数的输入：给突发留余量，但不允许无限积压——
// 积压的输入会变成「延迟补偿」，让 tick 耗时不可预测（这是真实项目里常见的坑）。
func (r *Room) drainInputsLocked() {
	// 每拍必须先清零「本 tick 已用输入配额」，否则限流会把玩家永久锁死。
	for _, p := range r.players {
		if p != nil {
			p.inputsThisTick = 0
		}
	}
	maxInputs := protocol.PlayersPerRoom * 3
	for i := 0; i < maxInputs; i++ {
		select {
		case in := <-r.inputCh:
			r.applyInputLocked(in)
		default:
			return
		}
	}
}

var inputRejectLabels = map[uint8]string{
	protocol.InputErrOutOfRange:     "out_of_range",
	protocol.InputErrRateLimited:    "rate_limited",
	protocol.InputErrRoomNotRunning: "not_running",
	protocol.InputErrBadTick:        "bad_tick",
}

func abs16(v int16) int16 {
	if v < 0 {
		return -v
	}
	return v
}

func (r *Room) applyInputLocked(in inputMsg) {
	if int(in.slot) >= protocol.PlayersPerRoom {
		return
	}
	p := r.players[in.slot]
	if p == nil {
		return
	}
	p.lastSeen = in.at
	r.lastActivity = in.at

	code := protocol.InputOK
	switch {
	case r.state != protocol.RoomStateRunning:
		code = protocol.InputErrRoomNotRunning
	case p.inputsThisTick >= 2:
		code = protocol.InputErrRateLimited
	case in.in.Tick+3 < r.tick || in.in.Tick > r.tick+3:
		code = protocol.InputErrBadTick
	case abs16(in.in.MoveX) > protocol.MaxMovePerTick || abs16(in.in.MoveY) > protocol.MaxMovePerTick:
		code = protocol.InputErrOutOfRange
	case in.in.Flags&protocol.FlagIllegal != 0:
		code = protocol.InputErrOutOfRange
	}

	if code != protocol.InputOK {
		p.rejected++
		r.inputsRejected++
		r.o.InputRejected.With(inputRejectLabels[code]).Inc()
		r.ackInputLocked(p, in.seq, false, code)
		return
	}

	p.inputsThisTick++
	p.accepted++
	r.inputsAccepted++
	r.o.InputTotal.With().Inc()

	p.x = clampWorld(int(p.x) + int(in.in.MoveX))
	p.y = clampWorld(int(p.y) + int(in.in.MoveY))

	if in.in.Action == protocol.ActionAttack {
		r.resolveAttackLocked(p)
	}
	r.ackInputLocked(p, in.seq, true, protocol.InputOK)
}

func clampWorld(v int) int16 {
	if v < 0 {
		return 0
	}
	if v > protocol.WorldSize {
		return int16(protocol.WorldSize)
	}
	return int16(v)
}

// resolveAttackLocked 是权威裁决：命中判定只在服务端做，bot 发来的只是「按下攻击键」。
func (r *Room) resolveAttackLocked(attacker *roomPlayer) {
	var target *roomPlayer
	best := math.MaxInt32
	for _, q := range r.players {
		if q == nil || q == attacker || q.spec.Team == attacker.spec.Team || q.hp == 0 {
			continue
		}
		dx := int(q.x) - int(attacker.x)
		dy := int(q.y) - int(attacker.y)
		d2 := dx*dx + dy*dy
		if d2 < best && d2 <= protocol.AttackRange*protocol.AttackRange {
			best = d2
			target = q
		}
	}
	if target == nil {
		return
	}
	if target.hp > protocol.AttackDamage {
		target.hp -= protocol.AttackDamage
		return
	}
	target.hp = 0
	target.deaths++
	attacker.kills++
	attacker.score += 100
	// 复活：满血回到出生点，避免死亡后整局空转（否则房间会「安静得不像话」）。
	target.hp = protocol.MaxHp
	target.x, target.y = spawnPoint(target.spec.Team, int(target.spec.Slot))
}

func (r *Room) ackInputLocked(p *roomPlayer, seq uint32, ok bool, code uint8) {
	if p.peer == nil {
		return
	}
	ack := protocol.InputAck{Seq: seq, Accepted: ok, ErrCode: code}
	p.peer.send(wire.Frame{MsgID: protocol.MsgInputAck, Seq: seq, Body: ack.Encode()})
}

func (r *Room) encodeSnapshotLocked(tickNo uint32) []byte {
	players := make([]protocol.SnapshotPlayer, 0, protocol.PlayersPerRoom)
	var score uint16
	for _, p := range r.players {
		if p == nil {
			continue
		}
		players = append(players, protocol.SnapshotPlayer{
			Slot:  p.spec.Slot,
			X:     p.x,
			Y:     p.y,
			HP:    p.hp,
			Flags: boolFlag(p.connected),
			Score: p.score,
		})
		score += p.score
	}
	snap := protocol.Snapshot{Tick: tickNo, State: r.state, Score: score, Players: players}
	return snap.Encode()
}

func boolFlag(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// checkHeartbeatsLocked 处理心跳超时（D7）。
//
// 判定标准是「应用层多久没收到任何帧」，不是 TCP keepalive——后者在
// 客户端进程被 kill 时可能要几分钟才发现，那几分钟里玩家占着房间不出结果。
func (r *Room) checkHeartbeatsLocked(now time.Time) []*peer {
	timeout := time.Duration(r.cfg.HeartbeatTimeoutMs) * time.Millisecond
	var dead []*peer
	for _, p := range r.players {
		if p == nil || !p.connected {
			continue
		}
		if now.Sub(p.lastSeen) > timeout {
			p.connected = false
			p.disconnectedAt = now
			if p.peer != nil {
				dead = append(dead, p.peer)
				p.peer = nil
			}
			r.o.SessionErrors.With("heartbeat_timeout").Inc()
		}
	}
	return dead
}

func (r *Room) activePeersLocked() []*peer {
	out := make([]*peer, 0, protocol.PlayersPerRoom)
	for _, p := range r.players {
		if p != nil && p.connected && p.peer != nil {
			out = append(out, p.peer)
		}
	}
	return out
}

func (r *Room) collectPeersLocked() []*peer {
	out := make([]*peer, 0, protocol.PlayersPerRoom)
	for _, p := range r.players {
		if p != nil && p.peer != nil {
			out = append(out, p.peer)
		}
	}
	return out
}

// finalize 结算并写库。
//
// 注意 Write 是**同步**的、且在 tick goroutine 里——这正是方案里故意留下的
// 第三个可被压垮的点：结算写库一慢，tick 超预算率立刻起飞。
func (r *Room) finalize(ctx context.Context, tickNo uint32) {
	r.mu.Lock()
	r.state = protocol.RoomStateSettling
	rec, settleMsg, peers, outcomes := r.buildSettlementLocked(tickNo)
	r.mu.Unlock()

	writeStart := time.Now()
	err := r.settle.Write(ctx, rec)
	writeDur := time.Since(writeStart)
	if err != nil {
		r.o.SettleWrite.With("error").Observe(writeDur.Seconds())
		r.o.SessionErrors.With("settle_write").Inc()
	} else {
		r.o.SettleWrite.With("ok").Observe(writeDur.Seconds())
	}

	frame := wire.Frame{MsgID: protocol.MsgSettle, Seq: tickNo, Body: settleMsg.Encode()}
	for _, p := range peers {
		// 结算帧必须送到，而且是同步写：它决定 bot 能否算出这一局的成败。
		if !p.sendSync(frame, time.Second) {
			r.o.SnapshotDropped.With().Inc()
		}
	}

	for _, oc := range outcomes {
		switch {
		case !oc.Connected:
			r.o.SessionTotal.With("abandoned").Inc()
			r.o.SessionErrors.With("disconnected").Inc()
		case r.result == protocol.ResultDraw:
			r.o.SessionTotal.With("draw").Inc()
		case (r.result == protocol.ResultTeam0) == (oc.Team == 0):
			r.o.SessionTotal.With("win").Inc()
		default:
			r.o.SessionTotal.With("loss").Inc()
		}
	}

	r.mu.Lock()
	r.settled = true
	r.mu.Unlock()

	r.Close("settled")
}

func (r *Room) buildSettlementLocked(tickNo uint32) (store.SettlementRecord, protocol.Settle, []*peer, []PlayerOutcome) {
	var kills [2]uint8
	for _, p := range r.players {
		if p == nil {
			continue
		}
		if p.spec.Team == 0 {
			kills[0] += p.kills
		} else {
			kills[1] += p.kills
		}
	}
	switch {
	case kills[0] > kills[1]:
		r.result = protocol.ResultTeam0
	case kills[1] > kills[0]:
		r.result = protocol.ResultTeam1
	default:
		r.result = protocol.ResultDraw
	}

	outcomes := make([]PlayerOutcome, 0, protocol.PlayersPerRoom)
	recPlayers := make([]store.SettlementPlayer, 0, protocol.PlayersPerRoom)
	var totalScore uint16
	for _, p := range r.players {
		if p == nil {
			continue
		}
		totalScore += p.score
		oc := PlayerOutcome{
			PlayerID:  p.spec.PlayerID,
			Slot:      p.spec.Slot,
			Team:      p.spec.Team,
			Kills:     p.kills,
			Deaths:    p.deaths,
			Score:     p.score,
			MMR:       p.spec.MMR,
			Connected: p.connected,
		}
		outcomes = append(outcomes, oc)
		recPlayers = append(recPlayers, store.SettlementPlayer{
			PlayerID: p.spec.PlayerID,
			Slot:     p.spec.Slot,
			Team:     p.spec.Team,
			Kills:    p.kills,
			Deaths:   p.deaths,
			Score:    p.score,
		})
	}

	rec := store.SettlementRecord{
		MatchID:       r.MatchID,
		Mode:          r.Mode,
		Result:        r.result,
		DurationTicks: int(tickNo),
		Players:       recPlayers,
		FinishedAt:    time.Now(),
	}
	settleMsg := protocol.Settle{
		Tick:          tickNo,
		Result:        r.result,
		Kills:         kills[0] + kills[1],
		Deaths:        kills[1] + kills[0],
		Score:         totalScore,
		DurationTicks: tickNo,
	}
	return rec, settleMsg, r.collectPeersLocked(), outcomes
}

// Close 关闭房间，幂等。
func (r *Room) Close(reason string) {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.state = protocol.RoomStateClosed
		r.closedAt = time.Now()
		r.reason = reason
		peers := r.collectPeersLocked()
		r.mu.Unlock()

		r.stopOnce.Do(func() { close(r.stopCh) })

		frame := wire.Frame{MsgID: protocol.MsgRoomClose, Seq: 0, Body: []byte(reason)}
		for _, p := range peers {
			// 同步写 RoomClose 再关连接：异步队列里的帧会随连接一起丢掉。
			p.sendSync(frame, 500*time.Millisecond)
			p.close()
		}
		if r.hooks.OnClosed != nil {
			r.reported = true
			r.hooks.OnClosed(r.Summary())
		}
	})
}

// Stop 只请求停止（不产出 Summary 之外的东西），供 shutdown 用。
func (r *Room) Stop() { r.stopOnce.Do(func() { close(r.stopCh) }) }

// Done 在 Run 退出后关闭。
func (r *Room) Done() <-chan struct{} { return r.doneCh }

// Summary 返回房间汇总快照。
func (r *Room) Summary() Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Summary{
		RoomID:         r.ID,
		MatchID:        r.MatchID,
		Mode:           r.Mode,
		Result:         r.result,
		Ticks:          r.tick,
		CreatedAt:      r.createdAt,
		StartedAt:      r.startedAt,
		ClosedAt:       r.closedAt,
		Reason:         r.reason,
		Filled:         r.filled,
		Settled:        r.settled,
		InputsAccepted: r.inputsAccepted,
		InputsRejected: r.inputsRejected,
	}
	for _, p := range r.players {
		if p == nil {
			continue
		}
		out.Players = append(out.Players, PlayerOutcome{
			PlayerID:  p.spec.PlayerID,
			Slot:      p.spec.Slot,
			Team:      p.spec.Team,
			Kills:     p.kills,
			Deaths:    p.deaths,
			Score:     p.score,
			MMR:       p.spec.MMR,
			Connected: p.connected,
		})
	}
	return out
}

// State 返回房间状态。
func (r *Room) State() uint8 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// ShouldReclaim 判断房间是否该被 janitor 回收。
func (r *Room) ShouldReclaim(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case protocol.RoomStateClosed:
		return true
	case protocol.RoomStateWaiting:
		ttl := time.Duration(r.cfg.FillTimeoutMs+r.cfg.EmptyTTLMs) * time.Millisecond
		return now.Sub(r.createdAt) > ttl && r.joined == 0
	case protocol.RoomStateRunning, protocol.RoomStateSettling:
		for _, p := range r.players {
			if p != nil && p.connected {
				return false
			}
		}
		return now.Sub(r.lastActivity) > time.Duration(r.cfg.EmptyTTLMs)*time.Millisecond
	}
	return false
}

// markLeaked 标记房间为泄漏（janitor 在 LeakInjection 下调用）。
func (r *Room) markLeaked() {
	r.mu.Lock()
	r.leaked = true
	r.mu.Unlock()
}

// Leaked 返回是否已被标记为泄漏。
func (r *Room) Leaked() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leaked
}
