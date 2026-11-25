// Package bot 实现协议级 bot 的会话状态机。
//
// 对应知识清单 E4（会话状态机：失败路径与主路径同等重要）、E8（非法输入注入）、
// D6（断线重连）与 C3（bot 侧 coordinated omission 修正）。
//
// 一条完整生命周期：
//
//	连接 → 握手 → 鉴权 → 排队（可取消）→ 匹配成功 → 进房 → 对局（可断线重连）
//	     → 结算（可提前退出）→ 再排 / 退出
//
// 所有耗时都以**应到时刻**为基准，而不是「实际开始时刻」：探针迟到是系统的锅，
// 不能让探针自己把这段延迟藏起来。
package bot

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"aiplayer/swarm/internal/behavior"
	"aiplayer/swarm/internal/obs"
	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/internal/report"
	"aiplayer/swarm/internal/runstats"
	"aiplayer/swarm/internal/sink"
)

// State 是会话状态机的显式状态。
type State string

// 状态取值。失败路径（cancelled / abandoned / reconnecting）与主路径同等重要。
const (
	StateIdle         State = "idle"
	StateConnecting   State = "connecting"
	StateHandshaking  State = "handshaking"
	StateAuth         State = "authenticating"
	StateQueuing      State = "queuing"
	StateMatched      State = "matched"
	StateJoining      State = "joining"
	StatePlaying      State = "playing"
	StateSettling     State = "settling"
	StateReconnecting State = "reconnecting"
	StateClosing      State = "closing"
)

// Config 是 bot 的运行参数。
type Config struct {
	GatewayURL        string
	Mode              string
	ClientSecret      []byte
	Platform          string
	MaxMessageBytes   int64
	DialTimeout       time.Duration
	MatchWaitTimeout  time.Duration
	HeartbeatInterval time.Duration
	RoomDrainTimeout  time.Duration
	InputTickInterval time.Duration
}

// Defaults 填充默认值。
func (c *Config) Defaults() {
	if c.Mode == "" {
		c.Mode = "ranked5v5"
	}
	if c.Platform == "" {
		c.Platform = "windows"
	}
	if len(c.ClientSecret) == 0 {
		c.ClientSecret = []byte("dsh-swarm-dev-secret")
	}
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = 64 << 10
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 5 * time.Second
	}
	if c.MatchWaitTimeout <= 0 {
		c.MatchWaitTimeout = 120 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 5 * time.Second
	}
	if c.RoomDrainTimeout <= 0 {
		c.RoomDrainTimeout = 2 * time.Minute
	}
	if c.InputTickInterval <= 0 {
		c.InputTickInterval = 20 * time.Millisecond
	}
}

// Deps 是 bot 的外部依赖。
type Deps struct {
	Runner     *obs.Runner
	Writer     *sink.Writer
	Reporter   *report.Reporter
	Stats      *runstats.Stats
	RunID      string
	Experiment string
	RunSeed    int64
}

// Bot 是一个 bot 实例：一个 persona 的一次会话。
type Bot struct {
	cfg     Config
	deps    Deps
	index   int
	persona *behavior.Persona

	mu    sync.Mutex
	state State
	// playerID 在登录后写入，进房/重连时读出（同一把锁保护）。
	playerID uint64
}

// New 创建 bot。
func New(cfg Config, deps Deps, index int, persona *behavior.Persona) *Bot {
	cfg.Defaults()
	return &Bot{cfg: cfg, deps: deps, index: index, persona: persona, state: StateIdle}
}

// State 返回当前状态（调试与测试用）。
func (b *Bot) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Bot) setState(s State) {
	b.mu.Lock()
	b.state = s
	b.mu.Unlock()
}

func (b *Bot) currentPlayerID() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.playerID
}

func (b *Bot) setPlayerID(id uint64) {
	b.mu.Lock()
	b.playerID = id
	b.mu.Unlock()
}

// matchOutcome 是一局的结果分类。
type matchOutcome int

const (
	outcomeAbort matchOutcome = iota
	outcomeCancelled
	outcomeWin
	outcomeLoss
	outcomeDraw
	outcomePreSettleQuit
)

func (o matchOutcome) String() string {
	switch o {
	case outcomeCancelled:
		return "queue_cancelled"
	case outcomeWin:
		return "win"
	case outcomeLoss:
		return "loss"
	case outcomeDraw:
		return "draw"
	case outcomePreSettleQuit:
		return "pre_settle_quit"
	default:
		return "abort"
	}
}

// Run 跑完一次完整会话。scheduledAt 是该 bot 的**应到时刻**（开环时间表）。
func (b *Bot) Run(ctx context.Context, scheduledAt time.Time) {
	start := time.Now()
	r := behavior.NewRand(b.deps.RunSeed, b.index)
	seed := behavior.Splitmix64(uint64(b.deps.RunSeed) ^ (uint64(b.index)+1)*0x9e3779b97f4a7c15)

	rec := &sink.SessionRecord{
		RunID:         b.deps.RunID,
		Experiment:    b.deps.Experiment,
		BotIndex:      b.index,
		Persona:       b.persona.Name,
		Seed:          seed,
		AccountID:     fmt.Sprintf("bot-%06d", b.index),
		ScheduledAtMs: scheduledAt.UnixMilli(),
		StartLagMs:    start.Sub(scheduledAt).Milliseconds(),
	}
	if rec.StartLagMs < 0 {
		rec.StartLagMs = 0
	}
	b.deps.Runner.ScheduleLag.With().Observe(start.Sub(scheduledAt).Seconds())

	defer func() {
		if rec.Outcome == "" {
			rec.Outcome = "incomplete"
		}
		rec.SessionMs = time.Since(start).Milliseconds()
		b.deps.Runner.Sessions.With(b.persona.Name, rec.Outcome).Inc()
		b.deps.Runner.SessionSeconds.With(b.persona.Name).Observe(time.Since(start).Seconds())
		if b.deps.Stats != nil {
			b.deps.Stats.Sessions.Add(1)
			switch rec.Outcome {
			case "queue_cancelled":
				b.deps.Stats.QueueCancelled.Add(1)
			case "incomplete", "match_aborted", "pre_settle_quit":
				b.deps.Stats.Abandoned.Add(1)
			}
		}
		if err := b.deps.Writer.Write(rec); err != nil {
			b.deps.Runner.BotErrors.With("sink_write").Inc()
		}
		b.setState(StateClosing)
	}()

	// persona 的 MMR 是一个分布（§2.9：人为设定，报告里要标注）。
	initMMR := int(b.persona.MMRMean + b.persona.MMRStd*r.NormFloat64())
	if initMMR < 100 {
		initMMR = 100
	}
	rec.MMRInitial = initMMR

	b.setState(StateConnecting)
	gwCfg := gwConfig{
		URL:             b.cfg.GatewayURL,
		DialTimeout:     b.cfg.DialTimeout,
		ClientID:        fmt.Sprintf("runner/%s/bot-%06d", b.deps.RunID, b.index),
		Platform:        b.cfg.Platform,
		Seed:            seed,
		ProtoVersion:    protocol.ProtoVersion,
		MaxMessageBytes: b.cfg.MaxMessageBytes,
	}

	tConnect := time.Now()
	gw, err := dialGateway(ctx, gwCfg)
	if err != nil {
		rec.Outcome = "connect_failed"
		rec.ErrorCode = "dial"
		b.deps.Runner.BotErrors.With("dial").Inc()
		return
	}
	defer gw.close()
	b.markPhase(rec, scheduledAt, tConnect, "connect")

	b.setState(StateHandshaking)
	tHandshake := time.Now()
	welcome, err := gw.handshake(ctx, gwCfg)
	if err != nil {
		rec.Outcome = "handshake_failed"
		rec.ErrorCode = "handshake"
		b.deps.Runner.BotErrors.With("handshake").Inc()
		return
	}
	b.markPhase(rec, scheduledAt, tHandshake, "handshake")

	b.setState(StateAuth)
	tLogin := time.Now()
	login, err := gw.login(ctx, rec.AccountID, initMMR, welcome.Nonce, b.cfg.ClientSecret)
	if err != nil {
		rec.Outcome = "login_failed"
		rec.ErrorCode = "login"
		b.deps.Runner.BotErrors.With("login").Inc()
		return
	}
	rec.PlayerID = login.PlayerID
	rec.MMRInitial = login.MMR
	rec.MMRFinal = login.MMR
	b.setPlayerID(login.PlayerID)
	b.markPhase(rec, scheduledAt, tLogin, "login")

	// 应用层心跳：独立 goroutine，按固定间隔 ping，顺带采集 RTT（A8）。
	hbCtx, hbCancel := context.WithCancel(ctx)
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		b.heartbeat(hbCtx, gw)
	}()

	sessionDur := time.Duration(b.persona.SessionDuration(r)) * time.Second
	deadline := time.Now().Add(sessionDur)
	planned := int(b.persona.MatchesPerSession.Sample(r))
	if planned < 1 {
		planned = 1
	}

	consecutiveLosses := 0
	for i := 0; i < planned; i++ {
		if ctx.Err() != nil || time.Now().After(deadline) {
			break
		}
		res := b.playMatch(ctx, gw, rec, r, scheduledAt)
		rec.Matches++
		if res == outcomeLoss {
			consecutiveLosses++
		} else if res == outcomeWin {
			consecutiveLosses = 0
		}
		if res == outcomeAbort {
			rec.ErrorCode = "match_aborted"
			break
		}
		// 退出危险率模型：连败越多越可能直接关游戏（§2.4.2）。
		if b.persona.ShouldQuitAfterLoss(consecutiveLosses, r) {
			if rec.Outcome == "" {
				rec.Outcome = "quit_after_losses"
			}
			break
		}
		gap := time.Duration(b.persona.RequeueGapS.Sample(r) * float64(time.Second))
		if !sleepCtx(ctx, gap) {
			break
		}
	}

	hbCancel()
	hbWG.Wait()

	if rec.Outcome == "" {
		rec.Outcome = "session_complete"
	}
}

// markPhase 记录一个阶段的耗时与偏移。
//
// 写进 Prometheus 与 HdrHistogram 的都是 (offset+dur)，即「从应到时刻到该里程碑」
// 的累计耗时——这才是开环压测下真正有意义的分位数（C3）。
func (b *Bot) markPhase(rec *sink.SessionRecord, base, start time.Time, name string) {
	dur := time.Since(start)
	off := start.Sub(base)
	rec.Phases = append(rec.Phases, sink.Phase{
		Name:     name,
		OffsetMs: off.Milliseconds(),
		DurMs:    dur.Milliseconds(),
	})
	b.deps.Runner.Phase.With(name).Observe((off + dur).Seconds())
	if b.deps.Reporter != nil {
		b.deps.Reporter.Phase(name, off+dur)
	}
}

// reportLatency 记录一段独立耗时（同时进 Prometheus 与 HdrHistogram）。
func (b *Bot) reportLatency(name string, d time.Duration) {
	if d < 0 {
		d = 0
	}
	if b.deps.Reporter != nil {
		b.deps.Reporter.Latency(name, d)
	}
}

// recordMatch 记录一局完成（业务口径的 goodput 来源）。
func (b *Bot) recordMatch(result string) {
	if b.deps.Stats == nil {
		return
	}
	b.deps.Stats.Matches.Add(1)
	switch result {
	case "win":
		b.deps.Stats.MatchesWin.Add(1)
	case "loss":
		b.deps.Stats.MatchesLoss.Add(1)
	default:
		b.deps.Stats.MatchesDraw.Add(1)
	}
}

func (b *Bot) heartbeat(ctx context.Context, gw *gwClient) {
	t := time.NewTicker(b.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-gw.closed:
			return
		case <-t.C:
			rtt, err := gw.ping(ctx)
			if err != nil {
				return
			}
			b.deps.Runner.RTT.With().Observe(rtt.Seconds())
		}
	}
}

// playMatch 跑完一局：排队 → 匹配 → 进房 → 对局 → 结算。
func (b *Bot) playMatch(ctx context.Context, gw *gwClient, rec *sink.SessionRecord, r *rand.Rand, base time.Time) matchOutcome {
	b.setState(StateQueuing)
	tQueue := time.Now()
	if _, err := gw.enterQueue(ctx, b.cfg.Mode); err != nil {
		b.deps.Runner.BotErrors.With("queue_join").Inc()
		return outcomeAbort
	}

	// 分支 A：排队中取消（§2.4.3）。这条路径专门压匹配队列的清理逻辑。
	if b.persona.QueueCancelP > 0 && r.Float64() < b.persona.QueueCancelP {
		after := time.Duration(b.persona.QueueCancelAfterS) * time.Second
		if after <= 0 {
			after = 10 * time.Second
		}
		if sleepCtx(ctx, after) {
			if resp, err := gw.cancelQueue(ctx); err == nil && resp.OK {
				if rec.Outcome == "" {
					rec.Outcome = "queue_cancelled"
				}
				return outcomeCancelled
			}
		}
	}

	b.setState(StateMatched)
	mf, err := gw.waitMatch(ctx, b.cfg.MatchWaitTimeout)
	if err != nil {
		b.deps.Runner.BotErrors.With("match_wait").Inc()
		return outcomeAbort
	}
	b.markPhase(rec, base, tQueue, "queue_wait")
	rec.MatchID = mf.MatchID
	rec.MatchWaitMs = mf.WaitMs
	rec.AllocMicros = mf.AllocMicros
	// 服务端上报的分层延迟同样进本机 HdrHistogram：这样报告里的 p99 与
	// 服务端 Prometheus 面板可以互相印证（两套埋点对不上就是埋点写错了）。
	b.reportLatency("match_wait", time.Duration(mf.WaitMs)*time.Millisecond)
	b.reportLatency("room_alloc", time.Duration(mf.AllocMicros)*time.Microsecond)

	myTeam := uint8(0)
	for _, p := range mf.Players {
		if p.PlayerID == rec.PlayerID {
			myTeam = p.Team
		}
	}

	b.setState(StateJoining)
	tJoin := time.Now()
	room, join, err := b.openRoom(ctx, mf)
	if err != nil {
		b.deps.Runner.BotErrors.With("room_join").Inc()
		return outcomeAbort
	}
	defer room.close()
	b.markPhase(rec, base, tJoin, "room_join")
	if join.Players < uint8(len(mf.Players)) {
		// 房间没坐满就开了（冷启动 / 不满开局），这是正常路径，不是错误。
		b.deps.Runner.BotErrors.With("room_underfilled").Inc()
	}

	b.setState(StatePlaying)
	return b.play(ctx, &room, mf, rec, r, myTeam, join.Slot, base)
}

// openRoom 建立房间连接并完成进房握手。
func (b *Bot) openRoom(ctx context.Context, mf protocol.MatchFound) (*roomClient, protocol.RoomJoinResp, error) {
	room, err := dialRoom(ctx, mf.RoomAddr, b.cfg.DialTimeout)
	if err != nil {
		return nil, protocol.RoomJoinResp{}, err
	}
	join, err := room.join(ctx, mf.RoomToken, b.currentPlayerID(), false)
	if err != nil {
		room.close()
		return nil, join, err
	}
	if !join.OK {
		room.close()
		return nil, join, fmt.Errorf("bot: 进房被拒 code=%d", join.ErrCode)
	}
	return room, join, nil
}

// play 是对局主循环。返回值决定这一局算赢、输、还是中途退出。
//
// mySlot / myTeam 用来做一件很基本的事：**看见敌人就上去打**。
// 纯随机游走的 bot 在短对局里永远碰不到人，胜负逻辑等于没被测到——
// 这不是「保真度」问题，是「压测覆盖度」问题。
func (b *Bot) play(ctx context.Context, roomPtr **roomClient, mf protocol.MatchFound, rec *sink.SessionRecord, r *rand.Rand, myTeam, mySlot uint8, base time.Time) matchOutcome {
	room := *roomPtr
	afk := b.persona.AfkP > 0 && r.Float64() < b.persona.AfkP
	movement := behavior.NewMovement(r, 0.35)

	var teamOfSlot [protocol.PlayersPerRoom]uint8
	for _, p := range mf.Players {
		if int(p.Slot) < protocol.PlayersPerRoom {
			teamOfSlot[p.Slot] = p.Team
		}
	}

	maxInputs := int(b.persona.ActionsPerMatch.Sample(r))
	if maxInputs < 1 {
		maxInputs = 1
	}

	// 断线注入：在某个 tick 主动断开，然后走重连流程（D6）。
	disconnectAtTick := -1
	if b.persona.DisconnectP > 0 && r.Float64() < b.persona.DisconnectP {
		disconnectAtTick = 10 + r.IntN(40)
	}
	reconnectsLeft := b.persona.MaxReconnects
	if reconnectsLeft <= 0 {
		reconnectsLeft = 1
	}

	preSettleQuit := b.persona.PreSettleQuitP > 0 && r.Float64() < b.persona.PreSettleQuitP

	// finish 处理「结算帧已到」这件事。
	//
	// 单独抽出来是因为它会被两处调用：正常收到结算帧，以及连接关闭后的补收。
	// 后者不是洁癖——服务端是「先发结算再关连接」，客户端读循环可能先把 EOF 报出来，
	// 于是 select 会随机选中「连接已关闭」，把一局已经打完的对局误记成中断。
	// 这类竞态在压测工具里会直接污染 goodput，必须显式处理。
	finish := func(st protocol.Settle) matchOutcome {
		b.setState(StateSettling)
		b.markPhase(rec, base, time.Now(), "match_end")
		rec.Result = outcomeFor(st.Result, myTeam)
		rec.MatchMs = int64(st.DurationTicks) * int64(mf.TickRateMs)
		b.recordMatch(rec.Result)
		switch rec.Result {
		case "win":
			return outcomeWin
		case "loss":
			return outcomeLoss
		default:
			return outcomeDraw
		}
	}

	ticker := time.NewTicker(b.cfg.InputTickInterval)
	defer ticker.Stop()

	var lastTick uint32
	inputsSent := 0
	nextInputAt := time.Now().Add(time.Duration(b.persona.ReactionMs(r)) * time.Millisecond)
	// 房间层心跳：即使挂机也必须发，否则会被服务端心跳超时踢掉（D7）。
	// 这不是压测工具的取巧——真实客户端同样有保活包，bot 不发明人不会发的行为。
	nextPing := time.Now().Add(2 * time.Second)
	started := false
	disconnected := false

	// 交战状态：从快照里算出来，供输入生成使用。
	// 视野比命中半径大得多：看见了才追，追上了才打（人不会隔半张地图挥刀）。
	const aggroRange2 = int32(1500) * int32(1500)
	const attackRange2 = int32(protocol.AttackRange) * int32(protocol.AttackRange)
	var (
		myX, myY       int16
		enemyX, enemyY int16
		chasing        bool
		inAttackRange  bool
	)

	for {
		select {
		case <-ctx.Done():
			return outcomeAbort

		case snap, ok := <-room.snapshots:
			if !ok {
				return outcomeAbort
			}
			lastTick = snap.Tick
			rec.Ticks = int(snap.Tick)
			b.deps.Runner.Snapshots.With().Inc()
			if !started {
				started = true
				b.markPhase(rec, base, time.Now(), "match_start")
			}
			// 算出自己位置与最近敌人：看不见人就随机游走，看见了就追。
			for _, p := range snap.Players {
				if p.Slot == mySlot {
					myX, myY = p.X, p.Y
				}
			}
			chasing = false
			inAttackRange = false
			best := int32(1 << 30)
			for _, p := range snap.Players {
				if p.Slot == mySlot || (int(p.Slot) < protocol.PlayersPerRoom && teamOfSlot[p.Slot] == myTeam) {
					continue
				}
				dx := int32(p.X) - int32(myX)
				dy := int32(p.Y) - int32(myY)
				if d2 := dx*dx + dy*dy; d2 < best {
					best = d2
					enemyX, enemyY = p.X, p.Y
				}
			}
			if best <= aggroRange2 {
				chasing = true
				inAttackRange = best <= attackRange2
			}
			if disconnectAtTick >= 0 && !disconnected && int(lastTick) >= disconnectAtTick && reconnectsLeft > 0 {
				disconnected = true
				rec.Disconnects++
				if b.deps.Stats != nil {
					b.deps.Stats.Disconnects.Add(1)
				}
				b.deps.Runner.Disconnects.With(b.persona.Name).Inc()
				room.close()
				b.setState(StateReconnecting)
				nr, join, err := b.reconnect(ctx, mf, r)
				rec.Reconnects++
				if b.deps.Stats != nil {
					b.deps.Stats.Reconnects.Add(1)
				}
				if err != nil || nr == nil || !join.OK {
					b.deps.Runner.Reconnects.With("fail").Inc()
					return outcomeAbort
				}
				room = nr
				*roomPtr = nr
				reconnectsLeft--
				b.deps.Runner.Reconnects.With("ok").Inc()
				b.setState(StatePlaying)
			}

		case ack := <-room.acks:
			if !ack.Accepted {
				rec.InputsRejected++
				if b.deps.Stats != nil {
					b.deps.Stats.InputsRejected.Add(1)
				}
				b.deps.Runner.InputsRejected.With(rejectLabel(ack.ErrCode)).Inc()
			}

		case st := <-room.settles:
			return finish(st)

		case <-room.closed:
			// 房间服把连接关了（结算后回收、心跳超时踢人、或房间服进程挂了）。
			// 先补收一次结算帧：结算与关连接几乎同时发生，顺序不保证。
			select {
			case st := <-room.settles:
				return finish(st)
			case <-time.After(200 * time.Millisecond):
			}
			if preSettleQuit {
				if rec.Outcome == "" {
					rec.Outcome = "pre_settle_quit"
				}
				return outcomePreSettleQuit
			}
			return outcomeAbort

		case <-ticker.C:
			// 心跳优先：挂机分支也不能漏掉保活包。
			if time.Now().After(nextPing) {
				_ = room.ping()
				nextPing = time.Now().Add(2 * time.Second)
			}
			if afk || inputsSent >= maxInputs || !started {
				continue
			}
			if time.Now().Before(nextInputAt) {
				continue
			}
			// 分支 C：结算前直接退出，制造脏房间与分配泄漏（§2.4.3）。
			if preSettleQuit && lastTick > 0 && int(lastTick) > mf.DurationTicks/2 {
				_ = room.leave()
				room.close()
				if rec.Outcome == "" {
					rec.Outcome = "pre_settle_quit"
				}
				b.deps.Runner.BotErrors.With("pre_settle_quit").Inc()
				return outcomePreSettleQuit
			}

			// 单轴位移限制在 0.7 倍上限，保证斜向合成位移也不会超过服务端允许的步长。
			step := int32(protocol.MaxMovePerTick * 7 / 10)
			dx, dy := movement.Next(int(step))
			action := protocol.ActionNone
			if r.Float64() < 0.35 {
				action = protocol.ActionAttack
			}
			if chasing {
				dx = int16(clampStep(int32(enemyX)-int32(myX), step))
				dy = int16(clampStep(int32(enemyY)-int32(myY), step))
				if inAttackRange {
					action = protocol.ActionAttack
				}
			}
			flags := uint8(0)
			// 分支 E：非法输入注入（§2.4.4）。这是 bug 密度最高的地方。
			if b.persona.InvalidInputP > 0 && r.Float64() < b.persona.InvalidInputP {
				flags |= protocol.FlagIllegal
				dx = protocol.MaxMovePerTick * 4
				rec.InvalidInputs++
				if b.deps.Stats != nil {
					b.deps.Stats.InvalidInputs.Add(1)
				}
				b.deps.Runner.InputsInvalid.With(b.persona.Name).Inc()
			}
			in := protocol.Input{
				Tick:   lastTick + 1,
				MoveX:  dx,
				MoveY:  dy,
				Action: action,
				Flags:  flags,
			}
			if _, err := room.sendInput(in); err != nil {
				return outcomeAbort
			}
			inputsSent++
			rec.InputsSent++
			if b.deps.Stats != nil {
				b.deps.Stats.InputsSent.Add(1)
			}
			b.deps.Runner.InputsSent.With(b.persona.Name).Inc()
			// 打架时的节奏远快于走路时（§2.4.2 的双峰分布）。
			// 追击状态直接用爆发段间隔：否则短对局里 bot 还在「走路看地图」，
			// 胜负逻辑一行都执行不到，压测就白跑了。
			var gapMs float64
			if chasing {
				gapMs = b.persona.BurstIntervalMs.Sample(r)
				if gapMs < 60 {
					gapMs = 60
				}
			} else {
				gapMs = b.persona.NextThink(r)
			}
			nextInputAt = time.Now().Add(time.Duration(gapMs) * time.Millisecond)
		}
	}
}

// reconnect 断线重连，带指数退避（D6）。重连风暴正是这样形成的：
// 一台房间服挂掉，几万个 bot 同时按同样的退避节奏回来。
func (b *Bot) reconnect(ctx context.Context, mf protocol.MatchFound, r *rand.Rand) (*roomClient, protocol.RoomJoinResp, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		backoff := time.Duration(150*(1<<attempt)) * time.Millisecond
		backoff += time.Duration(r.IntN(150)) * time.Millisecond
		if !sleepCtx(ctx, backoff) {
			return nil, protocol.RoomJoinResp{}, ctx.Err()
		}
		room, err := dialRoom(ctx, mf.RoomAddr, b.cfg.DialTimeout)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := room.join(ctx, mf.RoomToken, b.currentPlayerID(), true)
		if err != nil || !resp.OK {
			room.close()
			if err != nil {
				lastErr = err
			} else {
				lastErr = fmt.Errorf("bot: 重连被拒 code=%d", resp.ErrCode)
			}
			continue
		}
		return room, resp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("bot: 重连失败")
	}
	return nil, protocol.RoomJoinResp{}, lastErr
}

func outcomeFor(result uint8, myTeam uint8) string {
	switch result {
	case protocol.ResultDraw:
		return "draw"
	case protocol.ResultTeam0:
		if myTeam == 0 {
			return "win"
		}
		return "loss"
	case protocol.ResultTeam1:
		if myTeam == 1 {
			return "win"
		}
		return "loss"
	}
	return "unknown"
}

// clampStep 把位移限制在单 tick 允许的步长内（服务端会拒绝超限位移）。
func clampStep(v, step int32) int32 {
	if v > step {
		return step
	}
	if v < -step {
		return -step
	}
	return v
}

func rejectLabel(code uint8) string {
	switch code {
	case protocol.InputErrOutOfRange:
		return "out_of_range"
	case protocol.InputErrRateLimited:
		return "rate_limited"
	case protocol.InputErrRoomNotRunning:
		return "not_running"
	case protocol.InputErrBadTick:
		return "bad_tick"
	}
	return "other"
}

// sleepCtx 可被取消的 sleep。返回 false 表示 ctx 已取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
