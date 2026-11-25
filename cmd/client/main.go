// Command client 是「真人客户端」：一个人能真的坐在这儿打一局。
//
// 它的意义不只是好玩。项目里其它所有流量都是 bot 造的，而 bot 与服务器由同一套
// 假设写出来——**两边一起错**是最难发现的失败模式。真人客户端复用同一套
// wire/protocol 实现，却由完全不同的代码路径驱动（人按键 → 输入 → 服务端裁决），
// 因此它能连上、能打完一局，等于给协议层做了一次独立的符合性验证。
//
// 它同时是「保真度」闭环里缺失的那一半：把真人玩的对局时长与输入节奏记录下来，
// 就能反过来校准 bot 的参数：
//
//	.\bin\client.exe -play -out results/human-1        # 人玩几局，落盘
//	python analysis/analyze.py results/bot-run --reference results/human-1/sessions.json
//
// 用法：
//
//	client                                   # 交互模式（默认）
//	client -plain                            # 逐行输入，日志友好
//	client -auto -matches 3                  # 无人值守，CI 用
//	client -play -out results/human-1        # 记录真人数据
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"aiplayer/swarm/internal/bot"
	"aiplayer/swarm/internal/play"
	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/internal/sink"
)

func main() {
	gateway := flag.String("gateway", "ws://127.0.0.1:8080/ws", "接入层地址")
	account := flag.String("account", "human-1", "账号名（同名重复登录会让旧 token 失效）")
	mode := flag.String("mode", "ranked5v5", "匹配模式")
	mmr := flag.Int("mmr", 1500, "初始 MMR（只对首次注册的账号生效）")
	maxMatches := flag.Int("matches", 0, "最多打几局，0 表示一直玩到退出")
	auto := flag.Bool("auto", false, "无人值守：自动打，不渲染界面（CI/冒烟用）")
	plain := flag.Bool("plain", false, "逐行输出，不用全屏界面")
	forceTUI := flag.Bool("tui", false, "强制全屏界面（即使 stdout 不是终端）。用于录制/截图，不适合管道场景")
	outDir := flag.String("out", "", "记录目录（给了才写 bots.jsonl / sessions.json / trace.jsonl）")
	secret := flag.String("secret", "dsh-swarm-dev-secret", "登录签名密钥，必须与被测系统一致")
	platform := flag.String("platform", "windows", "上报给服务端的平台标识")
	matchTimeout := flag.Duration("match-timeout", 3*time.Minute, "单次排队最长等待")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 是否用全屏界面：看 stdout 是不是终端，而不是看有没有传参数。
	// 脚本里忘记加 -plain 时，宁可退化成日志行，也不要往日志里灌 ANSI 乱码。
	tty := isTerminal(os.Stdout)
	useTUI := *forceTUI || (tty && !*auto && !*plain)
	if useTUI && tty && !play.EnableVT() {
		useTUI = false
		fmt.Fprintln(os.Stderr, "提示：当前控制台不支持 ANSI 转义，已切换为逐行模式")
	}

	cfg := bot.Config{
		GatewayURL:       *gateway,
		Mode:             *mode,
		ClientSecret:     []byte(*secret),
		Platform:         *platform,
		MatchWaitTimeout: *matchTimeout,
	}
	p, err := bot.ConnectPlayer(ctx, cfg, *account, *mmr)
	if err != nil {
		fail("连接失败：%v\n提示：先起被测系统 —— .\\bin\\sutd.exe -config deploy\\sut.smoke.yaml", err)
	}
	defer p.Close()

	fmt.Printf("已连接 %s（%s · 协议 v%d）\n", *gateway, p.ServerName(), p.ProtoVersion())
	fmt.Printf("账号 %s · 玩家 #%d · MMR %d\n", p.Account(), p.PlayerID(), p.MMR())

	// 按键源：TUI 用原始模式；逐行模式下把每行首字符当成一次按键。
	var keys play.KeySource
	var keyCh <-chan play.KeyEvent
	if useTUI {
		if ks, err := play.NewKeys(); err == nil {
			keys, keyCh = ks, ks.Events()
			defer keys.Close()
		} else {
			useTUI = false
			fmt.Fprintf(os.Stderr, "提示：%v\n", err)
		}
	}
	if !useTUI && keyCh == nil {
		lk := newLineKeys(os.Stdin)
		go lk.run(ctx)
		keyCh = lk.Events()
	}
	if !useTUI && !*auto {
		fmt.Println("逐行模式：输入 w/a/s/d 移动、f 攻击、q 退出，每次回车执行一步。")
	}

	ui := play.NewRenderer(os.Stdout, !useTUI)
	rec := newRecorder(*outDir, p.Account(), p.PlayerID())
	defer rec.Close()

	round := 0
	for *maxMatches == 0 || round < *maxMatches {
		if ctx.Err() != nil {
			break
		}
		round++
		cont := playOne(ctx, ui, p, rec, keyCh, useTUI, *auto, round, *matchTimeout)
		if !cont || ctx.Err() != nil {
			break
		}
		if *auto {
			continue
		}
		if !askContinue(ctx, ui, keyCh, useTUI) {
			break
		}
	}

	ui.Cleanup(useTUI)
	if n := rec.Count(); n > 0 {
		fmt.Printf("\n本次记录 %d 局。", n)
		if *outDir != "" {
			fmt.Printf("已写入 %s", *outDir)
		}
		fmt.Println()
	}
}

// playOne 打完一局：排队 → 匹配 → 进房 → 对局 → 结算。返回是否继续。
func playOne(ctx context.Context, ui *play.Renderer, p *bot.Player, rec *recorder, keyCh <-chan play.KeyEvent, useTUI, auto bool, round int, matchTimeout time.Duration) bool {
	// ---- 1. 排队 ----
	queueStart := time.Now()
	if err := p.EnterQueue(ctx, ""); err != nil {
		fmt.Fprintf(os.Stderr, "\n入队失败：%v\n", err)
		return false
	}

	matchCh := make(chan protocol.MatchFound, 1)
	errCh := make(chan error, 1)
	go func() {
		mf, err := p.WaitMatch(ctx, matchTimeout)
		if err != nil {
			errCh <- err
			return
		}
		matchCh <- mf
	}()

	drawTicker := time.NewTicker(120 * time.Millisecond)
	defer drawTicker.Stop()

	var mf protocol.MatchFound
	queued := true
waitLoop:
	for {
		select {
		case <-ctx.Done():
			return false
		case m := <-matchCh:
			mf = m
			queued = false
			break waitLoop
		case err := <-errCh:
			fmt.Fprintf(os.Stderr, "\n等待匹配失败：%v\n", err)
			return false
		case ev, ok := <-keyCh:
			// 逐行模式（stdin 是管道/文件）读到 EOF 后通道会关闭。
			// 不处理就会在已关闭的通道上拿到无穷多个零值事件，把 select 打成空转。
			if !ok {
				keyCh = nil
				continue
			}
			if ev.Key == 'q' {
				if ok, _ := p.CancelQueue(ctx); ok {
					ui.ShowStatus("已取消排队", useTUI)
					return false
				}
			}
		case <-drawTicker.C:
			if queued {
				ui.ShowStatus(fmt.Sprintf("排队中… 第 %d 局，已等 %s（按 Q 取消）", round, time.Since(queueStart).Round(time.Second)), useTUI)
			}
		}
	}

	queueWait := time.Since(queueStart)
	// ---- 2. 进房 ----
	joinStart := time.Now()
	room, err := p.JoinMatch(ctx, mf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n进房失败：%v\n", err)
		return false
	}
	defer room.Close()
	joinDur := time.Since(joinStart)

	// ---- 3. 对局 ----
	var teams [protocol.PlayersPerRoom]uint8
	var present [protocol.PlayersPerRoom]bool
	myTeam := uint8(0)
	for _, mp := range mf.Players {
		if int(mp.Slot) < protocol.PlayersPerRoom {
			teams[mp.Slot] = mp.Team
			present[mp.Slot] = true
		}
		if mp.PlayerID == p.PlayerID() {
			myTeam = mp.Team
		}
	}
	mySlot := room.Slot()

	var (
		snap     protocol.Snapshot
		inputs   int
		lastTick uint32
		started  bool
		settle   protocol.Settle
		settled  bool
		rtt      time.Duration
	)
	matchStart := time.Now()
	if !useTUI && !auto {
		// 排队期间按下的键会被丢弃（那是上一局残留的状态）。
		// 逐行模式下用户看不到画面变化，必须显式告诉他「现在可以操作了」。
		fmt.Printf("对局开始：%d 个 tick，输入 w/a/s/d 移动、f 攻击（排队时按的键会被忽略）\n", room.DurationTicks())
	}

	in := newInputState(useTUI)
	autoRand := rand.New(rand.NewSource(time.Now().UnixNano()))
	var autoNext time.Time

	renderTick := time.NewTicker(80 * time.Millisecond)
	defer renderTick.Stop()
	heartbeat := time.NewTicker(2 * time.Second)
	defer heartbeat.Stop()

	closedByServer := false
	for !settled && !closedByServer {
		select {
		case <-ctx.Done():
			return false

		case <-room.Closed():
			// 服务端「先发结算帧再关连接」，两条几乎同时到达，select 会随机选一个。
			// 直接判掉线会把一局已经打完的对局记成中断（bot 侧踩过同一个坑），
			// 所以这里先补收一次结算，等不到才认掉线。
			if st, ok := room.TrySettle(); ok {
				settle = st
				settled = true
				break
			}
			select {
			case st := <-room.Settles():
				settle = st
				settled = true
			case <-time.After(200 * time.Millisecond):
				closedByServer = true
			}

		case s, ok := <-room.Snapshots():
			if !ok {
				return false
			}
			snap = s
			lastTick = s.Tick
			if !started {
				started = true
			}

			dx, dy, action := 0, 0, uint8(protocol.ActionNone)
			if auto {
				// 无人值守：朝最近敌人走、进范围就打，节奏略慢于 tick 以接近真人。
				if time.Now().After(autoNext) {
					dx, dy, action = autoDecide(&snap, mySlot, myTeam, teams)
					autoNext = time.Now().Add(time.Duration(60+autoRand.Intn(120)) * time.Millisecond)
				}
			} else {
				dx, dy, action = in.vector()
				if in.takeQuit() {
					// 主动离房：制造一条「玩家中途退出」的真实路径，
					// 服务端会看到 9/10 的房间——这正是要压的那类脏房间。
					_ = room.Leave()
					return false
				}
			}
			if dx == 0 && dy == 0 && action == protocol.ActionNone {
				continue
			}
			input := protocol.Input{Tick: lastTick + 1, MoveX: int16(dx), MoveY: int16(dy), Action: action}
			if _, err := room.SendInput(input); err != nil {
				return false
			}
			inputs++
			rec.Trace(lastTick+1, dx, dy, action)

		case ack := <-room.Acks():
			_ = ack // 真人客户端不需要逐条处理 ack，拒绝率在结算时看

		case st := <-room.Settles():
			settle = st
			settled = true

		case pr := <-room.Progress():
			_ = pr

		case ev, ok := <-keyCh:
			if !ok {
				keyCh = nil // stdin 结束：之后只靠心跳与渲染驱动
				continue
			}
			switch ev.Key {
			case 'p':
				if d, err := p.Ping(ctx); err == nil {
					rtt = d
				}
			default:
				in.onKey(ev.Key)
			}

		case <-heartbeat.C:
			// 房间层心跳：不发会被服务端 5 秒判死连接踢掉。
			_ = room.Ping()
			if d, err := p.Ping(ctx); err == nil {
				rtt = d
			}

		case <-renderTick.C:
			v := &play.View{
				Account: p.Account(), Server: p.ServerName(), PlayerID: p.PlayerID(),
				MMR: p.MMR(), MatchID: mf.MatchID, Slot: mySlot, Team: myTeam,
				Teams: teams, Present: present,
				Snap: snap, Total: room.DurationTicks(), RTT: rtt, Inputs: inputs,
				Elapsed: time.Since(matchStart),
				Status:  "对局中",
			}
			if !started {
				v.Status = "等待开局"
			}
			ui.Draw(v)
		}
	}

	if !settled {
		ui.ShowStatus("房间连接已断开（未收到结算）", useTUI)
		return false
	}

	matchDur := time.Since(matchStart)
	result := resultCode(settle.Result, myTeam)
	ui.ShowResult(fmt.Sprintf("第 %d 局结束：%s（tick %d，我方得分 %d）", round, resultChinese(result), settle.Tick, settle.Score), useTUI)

	// ---- 4. 落盘 ----
	rec.Match(sink.SessionRecord{
		RunID:         rec.runID,
		Experiment:    "human",
		Persona:       "human",
		AccountID:     p.Account(),
		PlayerID:      p.PlayerID(),
		ScheduledAtMs: queueStart.UnixMilli(),
		SessionMs:     matchDur.Milliseconds(),
		Outcome:       "match_complete",
		Matches:       1,
		Phases: []sink.Phase{
			{Name: "queue_wait", OffsetMs: 0, DurMs: queueWait.Milliseconds()},
			{Name: "room_join", OffsetMs: queueWait.Milliseconds(), DurMs: joinDur.Milliseconds()},
			{Name: "match_start", OffsetMs: (queueWait + joinDur).Milliseconds(), DurMs: 0},
			{Name: "match_end", OffsetMs: (queueWait + joinDur + matchDur).Milliseconds(), DurMs: 0},
		},
		MatchID:    mf.MatchID,
		MatchMs:    matchDur.Milliseconds(),
		Result:     result,
		InputsSent: inputs,
		Ticks:      int(settle.Tick),
	}, matchDur)
	return true
}

// resultCode 返回**机器可读**的结果码。
//
// 记录文件里一律用英文（win/loss/draw）：analysis/analyze.py 按值聚合，
// 中文会让脚本和 PromQL 都得跟着改；中文只出现在给人看的界面上。
func resultCode(code uint8, myTeam uint8) string {
	switch code {
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

// resultChinese 把结果码翻成界面上显示的中文。
func resultChinese(code string) string {
	switch code {
	case "win":
		return "胜利"
	case "loss":
		return "失败"
	case "draw":
		return "平局"
	}
	return "未知"
}

// autoDecide 是无人值守模式的策略：朝最近的敌人走，进攻击范围就打。
//
// 它**不是**保真度数据来源——真数据只能由人打出来。这里只是让 CI 能在没有键盘的
// 环境下把整条协议路径跑一遍。
func autoDecide(snap *protocol.Snapshot, mySlot, myTeam uint8, teams [protocol.PlayersPerRoom]uint8) (int, int, uint8) {
	var myX, myY int16
	for i := range snap.Players {
		if snap.Players[i].Slot == mySlot {
			myX, myY = snap.Players[i].X, snap.Players[i].Y
		}
	}
	best := int32(1 << 30)
	var ex, ey int16
	for i := range snap.Players {
		p := &snap.Players[i]
		if p.Slot == mySlot || p.HP == 0 {
			continue
		}
		if int(p.Slot) < protocol.PlayersPerRoom && teams[p.Slot] == myTeam {
			continue
		}
		dx := int32(p.X) - int32(myX)
		dy := int32(p.Y) - int32(myY)
		if d := dx*dx + dy*dy; d < best {
			best, ex, ey = d, p.X, p.Y
		}
	}
	if best == 1<<30 {
		return 0, 0, protocol.ActionNone
	}
	step := int32(protocol.MaxMovePerTick * 7 / 10)
	dx := clampStep(int32(ex)-int32(myX), step)
	dy := clampStep(int32(ey)-int32(myY), step)
	action := uint8(protocol.ActionNone)
	if best <= int32(protocol.AttackRange)*int32(protocol.AttackRange) {
		action = protocol.ActionAttack
	}
	return int(dx), int(dy), action
}

func clampStep(v, step int32) int32 {
	if v > step {
		return step
	}
	if v < -step {
		return -step
	}
	return v
}

// ---------------------------------------------------------------------------
// 输入状态
// ---------------------------------------------------------------------------

// inputState 把两种输入方式统一成「位移 + 动作」。
//
// TUI 是**按住**移动（连续），逐行模式是**执行一步**移动（一次性）。
// 两种语义混在一起最容易出的 bug 是按一次键走一整局，所以用 tui 标志显式区分。
type inputState struct {
	tui    bool
	held   map[rune]bool
	oneDir rune
	attack bool
	quit   bool
}

func newInputState(tui bool) *inputState {
	return &inputState{tui: tui, held: map[rune]bool{}}
}

func (s *inputState) onKey(k rune) {
	switch k {
	case 'q':
		s.quit = true
	case ' ':
		s.attack = true
	case 'w', 'a', 's', 'd':
		if s.tui {
			s.held[k] = true
		} else {
			s.oneDir = k
			s.attack = true // 逐行模式下按一下同时攻击，省一次回车
		}
	}
}

// vector 返回本 tick 的位移与动作。
func (s *inputState) vector() (int, int, uint8) {
	const step = protocol.MaxMovePerTick * 7 / 10
	dx, dy := 0, 0
	if s.tui {
		if s.held['w'] {
			dy -= step
		}
		if s.held['s'] {
			dy += step
		}
		if s.held['a'] {
			dx -= step
		}
		if s.held['d'] {
			dx += step
		}
	} else if s.oneDir != 0 {
		switch s.oneDir {
		case 'w':
			dy = -step
		case 's':
			dy = step
		case 'a':
			dx = -step
		case 'd':
			dx = step
		}
		s.oneDir = 0
	}
	action := uint8(protocol.ActionNone)
	if s.attack {
		action = protocol.ActionAttack
		s.attack = false
	}
	return dx, dy, action
}

func (s *inputState) takeQuit() bool {
	if s.quit {
		s.quit = false
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 逐行按键源（无原始键盘时的降级）
// ---------------------------------------------------------------------------

type lineKeys struct {
	ch   chan play.KeyEvent
	in   *bufio.Scanner
	once sync.Once
}

func newLineKeys(f *os.File) *lineKeys {
	return &lineKeys{ch: make(chan play.KeyEvent, 8), in: bufio.NewScanner(f)}
}

func (l *lineKeys) Events() <-chan play.KeyEvent { return l.ch }

func (l *lineKeys) run(ctx context.Context) {
	defer close(l.ch)
	for l.in.Scan() {
		line := strings.TrimSpace(l.in.Text())
		if line == "" {
			continue
		}
		// 一行可以写多个键：`ww` = 连走两步
		for _, r := range line {
			if r >= 'A' && r <= 'Z' {
				r = r - 'A' + 'a'
			}
			switch r {
			case 'w', 'a', 's', 'd', 'f', ' ', 'q', 'y', 'n', 'p':
				if r == 'f' {
					r = ' '
				}
				select {
				case l.ch <- play.KeyEvent{Key: r}:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// askContinue 问「再来一局吗」。
func askContinue(ctx context.Context, ui *play.Renderer, keyCh <-chan play.KeyEvent, useTUI bool) bool {
	ui.ShowStatus("再来一局？按 Y 继续，按 N 或 Q 退出", useTUI)
	timeout := time.After(60 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timeout:
			return false
		case ev, ok := <-keyCh:
			if !ok {
				return false
			}
			switch ev.Key {
			case 'y':
				return true
			case 'n', 'q':
				return false
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 记录器
// ---------------------------------------------------------------------------

// recorder 把真人会话写成与 bot 完全相同的 JSONL 格式。
//
// 这一点是刻意的：analysis/analyze.py 不需要为「真人数据」写第二套解析，
// 直接把 results/human-x 当成一个 run 就能出报告，也能拿它的 sessions.json
// 作为 --reference 去和 bot 的分布做 KS 检验。
type recorder struct {
	dir      string
	runID    string
	account  string
	playerID uint64
	bots     *sink.Writer
	traceLog *sink.Writer
	sessions []float64
	matches  int
}

type traceLine struct {
	TMs    int64  `json:"t_ms"`
	Tick   uint32 `json:"tick"`
	DX     int    `json:"dx"`
	DY     int    `json:"dy"`
	Action uint8  `json:"action"`
}

func newRecorder(dir, account string, playerID uint64) *recorder {
	r := &recorder{account: account, playerID: playerID, runID: "human-" + account}
	if dir == "" {
		return r
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "警告：无法创建记录目录 %s：%v\n", dir, err)
		return r
	}
	r.dir = dir
	w, err := sink.New(filepath.Join(dir, "bots.jsonl"), 64<<10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告：无法写记录文件：%v\n", err)
		return r
	}
	r.bots = w
	if t, err := sink.New(filepath.Join(dir, "trace.jsonl"), 64<<10); err == nil {
		r.traceLog = t
	}
	return r
}

// Trace 记录一条输入，用于事后分析真人的操作节奏。
func (r *recorder) Trace(tick uint32, dx, dy int, action uint8) {
	if r.traceLog == nil {
		return
	}
	_ = r.traceLog.Write(traceLine{
		TMs: time.Now().UnixMilli(), Tick: tick, DX: dx, DY: dy, Action: action,
	})
}

// Match 记录一局的汇总。
func (r *recorder) Match(rec sink.SessionRecord, dur time.Duration) {
	r.matches++
	if r.bots != nil {
		_ = r.bots.Write(rec)
	}
	r.sessions = append(r.sessions, dur.Seconds())
}

// Count 返回已记录的局数。
func (r *recorder) Count() int { return r.matches }

// Close 落盘：bots.jsonl / trace.jsonl 关闭，另写一个 sessions.json
// 供 analysis/analyze.py --reference 直接使用。
func (r *recorder) Close() {
	if r.dir == "" {
		return
	}
	if r.bots != nil {
		_ = r.bots.Flush()
		_ = r.bots.Close()
	}
	if r.traceLog != nil {
		_ = r.traceLog.Close()
	}
	if len(r.sessions) > 0 {
		var b strings.Builder
		b.WriteString("{\n  \"_comment\": \"真人客户端记录的对局时长（秒），可直接作为 analyze.py --reference\",\n  \"session_seconds\": [")
		for i, s := range r.sessions {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%.3f", s)
		}
		b.WriteString("]\n}\n")
		_ = os.WriteFile(filepath.Join(r.dir, "sessions.json"), []byte(b.String()), 0o644)
	}
}

// ---------------------------------------------------------------------------
// 杂项
// ---------------------------------------------------------------------------

// isTerminal 判断文件是否是终端（字符设备）。
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
