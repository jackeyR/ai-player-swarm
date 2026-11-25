// Package play 提供「真人客户端」的终端界面与输入。
//
// 分成 TUI / plain 两套渲染和一套可插拔的按键源，是因为这个客户端要在三种环境下跑：
//
//	TUI     —— 本地终端，人真的用 WASD 玩
//	plain   —— 输出被重定向到文件或管道（CI、录日志）时绝不能用 ANSI 转义
//	auto    —— 无人值守跑冒烟（不渲染，但走完全相同的协议路径）
//
// 判定依据不是「有没有传 -plain」，而是**stdout 是不是终端**：脚本里忘记加参数时，
// 宁可退化成一堆可读的日志行，也不要往日志里灌一屏乱码。
package play

import (
	"fmt"
	"io"
	"strings"
	"time"

	"aiplayer/swarm/internal/protocol"
)

// ANSI 颜色。仅在 TUI 模式使用。
const (
	escReset   = "\x1b[0m"
	escDim     = "\x1b[2m"
	escBold    = "\x1b[1m"
	escRed     = "\x1b[31m"
	escGreen   = "\x1b[32m"
	escYellow  = "\x1b[33m"
	escBlue    = "\x1b[34m"
	escCyan    = "\x1b[36m"
	escMagenta = "\x1b[35m"
)

const (
	// arenaCols / arenaRows 是战场网格尺寸。10 行是为了整屏塞进 24 行终端。
	arenaCols = 60
	arenaRows = 10
	// frameWidth 是整帧宽度。不读终端宽度是有意的：读宽度要处理 SIGWINCH/控制台事件，
	// 收益很小；固定 78 列在绝大多数终端里都不会换行。
	frameWidth = 78
)

// View 是一帧画面所需的全部状态。
type View struct {
	Account  string
	Server   string
	PlayerID uint64
	MMR      int
	MatchID  string
	Slot     uint8
	Team     uint8
	// Teams / Present 描述房间里每个槽位的归属，来自 MatchFound（快照里没有队伍字段）。
	Teams   [protocol.PlayersPerRoom]uint8
	Present [protocol.PlayersPerRoom]bool

	Snap    protocol.Snapshot
	Total   uint32
	RTT     time.Duration
	Inputs  int
	Status  string
	Elapsed time.Duration
}

// Renderer 按模式渲染画面。
type Renderer struct {
	out       io.Writer
	plain     bool
	started   bool
	lastPlain time.Time
}

// NewRenderer 创建渲染器。plain=true 时退化为逐行输出。
func NewRenderer(out io.Writer, plain bool) *Renderer {
	return &Renderer{out: out, plain: plain}
}

// Draw 渲染一帧。TUI 模式用「光标归位 + 逐行擦除」而不是清屏，
// 这样画面不会闪。
func (r *Renderer) Draw(v *View) {
	if r.plain {
		// 逐行模式限流到 1 秒一条，否则重定向到文件时会写爆磁盘。
		now := time.Now()
		if now.Sub(r.lastPlain) < time.Second && v.Snap.Tick != 0 {
			return
		}
		r.lastPlain = now
		fmt.Fprintln(r.out, plainLine(v))
		return
	}

	var b strings.Builder
	if !r.started {
		b.WriteString("\x1b[2J") // 首帧清屏
		r.started = true
	}
	b.WriteString("\x1b[H") // 之后每帧光标归位

	// 第 1~3 行：身份与状态
	b.WriteString(r.line(topBar(v), frameWidth))
	b.WriteString(r.line(identityLine(v), frameWidth))
	b.WriteString(r.line(statusLine(v), frameWidth))
	b.WriteString(r.line(escDim+strings.Repeat("─", frameWidth)+escReset, frameWidth))

	// 战场
	for _, row := range r.arena(v) {
		b.WriteString(row + "\x1b[K\n")
	}
	b.WriteString(r.line(escDim+strings.Repeat("─", frameWidth)+escReset, frameWidth))

	// 记分板：两队各 5 行并排
	left, right := r.scoreboard(v)
	for i := 0; i < protocol.TeamSize; i++ {
		l, rr := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			rr = right[i]
		}
		b.WriteString(fmt.Sprintf("%s%s%s\x1b[K\n", padVisible(l, 38), escDim+"│"+escReset, rr))
	}
	b.WriteString(r.line(escDim+strings.Repeat("─", frameWidth)+escReset, frameWidth))
	b.WriteString(r.line(escDim+"WASD 移动   空格 攻击   Q 退出   （挂机也会自动发心跳）"+escReset, frameWidth))

	fmt.Fprint(r.out, b.String())
}

// line 输出一行并擦掉行尾残留。
func (r *Renderer) line(s string, width int) string {
	return s + "\x1b[K\n"
}

func topBar(v *View) string {
	left := escBold + escCyan + " AI 玩家集群 · 真人客户端 " + escReset
	right := escDim + "server=" + v.Server + "  proto=v1" + escReset
	gap := frameWidth - visLen(left) - visLen(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func identityLine(v *View) string {
	team := "观战"
	if v.Team == 0 {
		team = "队伍 0"
	} else if v.Team == 1 {
		team = "队伍 1"
	}
	return fmt.Sprintf(" 账号 %s   玩家 #%d   MMR %d   %s   槽位 %d   %s",
		v.Account, v.PlayerID, v.MMR, v.MatchID, v.Slot, team)
}

func statusLine(v *View) string {
	tick := v.Snap.Tick
	status := v.Status
	if status == "" {
		status = "对局中"
	}
	rtt := "—"
	if v.RTT > 0 {
		rtt = fmt.Sprintf("%dms", v.RTT.Milliseconds())
	}
	return fmt.Sprintf(" 状态 %s   tick %d/%d   延迟 %s   输入 %d 条   用时 %.0fs",
		status, tick, v.Total, rtt, v.Inputs, v.Elapsed.Seconds())
}

// arena 把世界坐标映射成字符网格。
func (r *Renderer) arena(v *View) []string {
	// 网格：世界 10000×10000 → 60×10 字符，每格约 167×1000 单位
	grid := make([][]rune, arenaRows)
	colors := make([][]string, arenaRows)
	for i := range grid {
		grid[i] = make([]rune, arenaCols)
		colors[i] = make([]string, arenaCols)
		for j := range grid[i] {
			grid[i][j] = '·'
			colors[i][j] = escDim
		}
	}

	place := func(p *protocol.SnapshotPlayer) {
		col := int(p.X) * arenaCols / protocol.WorldSize
		row := int(p.Y) * arenaRows / protocol.WorldSize
		if col < 0 {
			col = 0
		}
		if col >= arenaCols {
			col = arenaCols - 1
		}
		if row < 0 {
			row = 0
		}
		if row >= arenaRows {
			row = arenaRows - 1
		}
		switch {
		case p.Slot == v.Slot:
			grid[row][col] = '@'
			colors[row][col] = escBold + escGreen
		case v.Teams[p.Slot] == v.Team:
			grid[row][col] = 'o'
			colors[row][col] = escBlue
		default:
			grid[row][col] = 'x'
			colors[row][col] = escBold + escRed
		}
	}
	for i := range v.Snap.Players {
		p := &v.Snap.Players[i]
		if int(p.Slot) >= protocol.PlayersPerRoom {
			continue
		}
		if p.HP == 0 {
			continue
		}
		place(p)
	}

	out := make([]string, arenaRows)
	for i := 0; i < arenaRows; i++ {
		var b strings.Builder
		b.WriteString(" ")
		// 逐个字符着色（每格颜色可能不同），因此不能用整行一个色号
		for j := 0; j < arenaCols; j++ {
			b.WriteString(colors[i][j])
			b.WriteRune(grid[i][j])
			b.WriteString(escReset)
		}
		out[i] = b.String()
	}
	return out
}

// scoreboard 生成两队各自的战绩行。
func (r *Renderer) scoreboard(v *View) (left, right []string) {
	for slot := 0; slot < protocol.PlayersPerRoom; slot++ {
		line := scoreLine(v, uint8(slot))
		if v.Teams[slot] == 0 {
			left = append(left, line)
		} else {
			right = append(right, line)
		}
	}
	return left, right
}

func scoreLine(v *View, slot uint8) string {
	var p *protocol.SnapshotPlayer
	for i := range v.Snap.Players {
		if v.Snap.Players[i].Slot == slot {
			p = &v.Snap.Players[i]
			break
		}
	}
	hp := uint16(0)
	score := uint16(0)
	online := false
	if p != nil {
		hp, score, online = p.HP, p.Score, p.Flags&1 == 1
	}
	name := fmt.Sprintf("玩家%02d", slot)
	if slot == v.Slot {
		name = "你    "
	}
	mark := " "
	if !online {
		mark = "×"
	}
	bar := hpBar(hp, 10)
	return fmt.Sprintf("%s %2d %s %s %3d %4d分", mark, slot, name, bar, hp, score)
}

// hpBar 生成血条。宽度固定，避免数字变化时整行抖动。
func hpBar(hp uint16, width int) string {
	filled := int(hp) * width / int(protocol.MaxHp)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	color := escGreen
	switch {
	case filled <= width/4:
		color = escRed
	case filled <= width/2:
		color = escYellow
	}
	return color + strings.Repeat("█", filled) + escDim + strings.Repeat("░", width-filled) + escReset
}

// plainLine 是逐行模式的一行输出。刻意做成「可 grep」的单行 KV 形式。
func plainLine(v *View) string {
	hp := uint16(0)
	score := uint16(0)
	for i := range v.Snap.Players {
		if v.Snap.Players[i].Slot == v.Slot {
			hp = v.Snap.Players[i].HP
			score = v.Snap.Players[i].Score
		}
	}
	status := v.Status
	if status == "" {
		status = "playing"
	}
	return fmt.Sprintf("status=%s tick=%d/%d hp=%d score=%d inputs=%d rtt=%s",
		status, v.Snap.Tick, v.Total, hp, score, v.Inputs, v.RTT.Round(time.Millisecond))
}

// padVisible 按可见宽度补齐（忽略 ANSI 转义），让两栏对齐。
func padVisible(s string, width int) string {
	n := visLen(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// ShowStatus 输出一行状态。
//
// TUI 模式下用 \r + 擦除原地刷新，不滚屏——排队阶段每 120ms 调一次，
// 直接换行会在一分钟内刷出 500 行垃圾。
func (r *Renderer) ShowStatus(msg string, tui bool) {
	if !tui {
		fmt.Fprintln(r.out, msg)
		return
	}
	fmt.Fprintf(r.out, "\r\x1b[K%s%s%s", escCyan, msg, escReset)
}

// ShowResult 输出一局的结果。TUI 下独占几行，并让下一帧重新清屏。
func (r *Renderer) ShowResult(msg string, tui bool) {
	if !tui {
		fmt.Fprintln(r.out, msg)
		return
	}
	fmt.Fprintf(r.out, "\r\x1b[K\n\n  %s%s%s\n\n", escBold+escYellow, msg, escReset)
	r.started = false
}

// Cleanup 收尾：TUI 下清屏并复位颜色，避免退出后把终端留在奇怪的状态。
func (r *Renderer) Cleanup(tui bool) {
	if !tui {
		return
	}
	fmt.Fprint(r.out, escReset+"\x1b[2J\x1b[H")
}

// visLen 返回去掉 ANSI 转义后的可见长度。
func visLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			// CSI 序列以字母结尾
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			n++
		}
	}
	return n
}
