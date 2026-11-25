package play

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"aiplayer/swarm/internal/protocol"
)

func sampleView() *View {
	v := &View{
		Account: "human-1", Server: "swarm-sut/test", PlayerID: 7, MMR: 1500,
		MatchID: "ranked5v5-000042", Slot: 0, Team: 0,
		Total:   600,
		RTT:     12 * time.Millisecond,
		Inputs:  42,
		Elapsed: 3 * time.Second,
		Snap: protocol.Snapshot{
			Tick:  120,
			State: protocol.RoomStateRunning,
			Score: 300,
			Players: []protocol.SnapshotPlayer{
				{Slot: 0, X: 4500, Y: 5000, HP: 80, Flags: 1, Score: 200},
				{Slot: 5, X: 5500, Y: 5000, HP: 25, Flags: 1, Score: 100},
			},
		},
	}
	v.Teams[0] = 0
	v.Teams[5] = 1
	v.Present[0] = true
	v.Present[5] = true
	return v
}

// TestPlainRender 逐行模式必须是可 grep 的 KV 形式：CI 里要能直接抽字段。
func TestPlainRender(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)
	r.Draw(sampleView())
	out := buf.String()
	for _, want := range []string{"status=", "tick=120/600", "hp=80", "score=200", "inputs=42"} {
		if !strings.Contains(out, want) {
			t.Errorf("逐行输出缺少 %q：%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("逐行模式不应输出 ANSI 转义（会污染日志/管道）")
	}
}

// TestPlainThrottle 限流：重定向到文件时不能一帧一行，否则日志会被写爆。
func TestPlainThrottle(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)
	v := sampleView()
	for i := 0; i < 50; i++ {
		v.Snap.Tick++
		r.Draw(v)
	}
	lines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1
	if lines > 2 {
		t.Fatalf("1 秒内应当只输出 1 行（最多 2 行含首帧），实际 %d 行", lines)
	}
}

// TestTUIRender 全屏模式：首帧必须清屏，之后靠光标归位重绘；战场必须有行。
func TestTUIRender(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)
	r.Draw(sampleView())
	first := buf.String()
	if !strings.HasPrefix(first, "\x1b[2J") {
		t.Error("首帧应当先清屏")
	}
	if strings.Count(first, "\x1b[H") != 1 {
		t.Error("每帧应当只有一次光标归位")
	}
	if !strings.Contains(first, "@") {
		t.Error("战场上没有画出自己（@）")
	}
	if !strings.Contains(first, "x") {
		t.Error("战场上没有画出敌人（x）")
	}
	if n := strings.Count(first, "\n"); n < arenaRows {
		t.Errorf("画面只有 %d 行，战场应有 %d 行", n, arenaRows)
	}

	// 第二帧不应再清屏（否则整屏闪烁）
	buf.Reset()
	r.Draw(sampleView())
	if strings.HasPrefix(buf.String(), "\x1b[2J") {
		t.Error("后续帧不应重复清屏")
	}
}

// TestVisLen 是两栏对齐的基础：ANSI 转义不能算进可见宽度。
func TestVisLen(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"", 0},
		{"\x1b[31m红色\x1b[0m", 2}, // 两个汉字 + 两段转义（转义不计宽）
		{"\x1b[1;32m@\x1b[0m", 1},
		{"普通文本", 4},
		{"\x1b[K", 0},
	}
	for _, c := range cases {
		if got := visLen(c.in); got != c.want {
			t.Errorf("visLen(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// TestHPBar 血条宽度必须恒定，否则数字变化时整行会抖动。
func TestHPBar(t *testing.T) {
	for _, hp := range []uint16{0, 1, 25, 50, 99, 100, 200} {
		bar := hpBar(hp, 10)
		if got := visLen(bar); got != 10 {
			t.Errorf("hp=%d 时血条可见宽度 %d，期望 10", hp, got)
		}
	}
	if visLen(hpBar(0, 10)) != 10 || !strings.Contains(hpBar(0, 10), "░") {
		t.Error("0 血应当全是空槽")
	}
}

func TestPadVisible(t *testing.T) {
	if got := visLen(padVisible("\x1b[31mab\x1b[0m", 6)); got != 6 {
		t.Fatalf("补齐后可见宽度应为 6，实际 %d", got)
	}
	long := strings.Repeat("x", 10)
	if got := padVisible(long, 4); got != long {
		t.Fatal("超宽字符串不应被截断（截断会丢信息，交给终端换行）")
	}
}

// TestShowStatusAndCleanup 覆盖 TUI 下「原地刷新」与退出复位这两处容易留下垃圾的路径。
func TestShowStatusAndCleanup(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)
	r.ShowStatus("排队中", true)
	out := buf.String()
	if !strings.Contains(out, "\r\x1b[K") {
		t.Error("TUI 状态行应当用 \\r + 擦除原地刷新")
	}
	buf.Reset()
	r.Cleanup(true)
	if !strings.Contains(buf.String(), "\x1b[2J") || !strings.Contains(buf.String(), "\x1b[0m") {
		t.Error("退出时应当清屏并复位颜色，否则终端会留在奇怪状态")
	}
}
