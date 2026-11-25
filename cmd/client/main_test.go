package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/internal/sink"
)

// TestResultCodeMapping 结果码必须机器可读（英文），中文只出现在界面。
func TestResultCodeMapping(t *testing.T) {
	cases := []struct {
		code   uint8
		myTeam uint8
		want   string
	}{
		{protocol.ResultDraw, 0, "draw"},
		{protocol.ResultTeam0, 0, "win"},
		{protocol.ResultTeam0, 1, "loss"},
		{protocol.ResultTeam1, 1, "win"},
		{protocol.ResultTeam1, 0, "loss"},
		{99, 0, "unknown"},
	}
	for _, c := range cases {
		if got := resultCode(c.code, c.myTeam); got != c.want {
			t.Errorf("resultCode(%d,%d) = %s，期望 %s", c.code, c.myTeam, got, c.want)
		}
	}
	if resultChinese("win") != "胜利" || resultChinese("draw") != "平局" {
		t.Error("中文映射错误")
	}
}

// TestInputStateTUI 全屏模式是「按住移动」：一次按键之后每个 tick 都该继续移动。
func TestInputStateTUI(t *testing.T) {
	in := newInputState(true)
	in.onKey('d')
	for i := 0; i < 3; i++ {
		dx, dy, action := in.vector()
		if dx <= 0 || dx > protocol.MaxMovePerTick {
			t.Fatalf("第 %d 次位移 dx=%d 不在 (0, %d] 区间", i, dx, protocol.MaxMovePerTick)
		}
		if dy != 0 {
			t.Fatalf("只按了 d，dy 应为 0，实际 %d", dy)
		}
		if action != protocol.ActionNone {
			t.Fatalf("没按攻击键却产生了动作 %d", action)
		}
	}
}

// TestInputStateOneShot 逐行模式是「执行一步」：按键只能生效一次，
// 否则按一下会走一整局（这是把两种输入语义混在一起时最容易出的 bug）。
func TestInputStateOneShot(t *testing.T) {
	in := newInputState(false)
	in.onKey('w')
	_, dy, action := in.vector()
	if dy >= 0 {
		t.Fatalf("按 w 应当向负方向走，实际 dy=%d", dy)
	}
	if action != protocol.ActionAttack {
		t.Error("逐行模式下按方向键应同时触发攻击（省一次回车）")
	}
	dx2, dy2, action2 := in.vector()
	if dx2 != 0 || dy2 != 0 || action2 != protocol.ActionNone {
		t.Fatalf("逐行模式的方向只能生效一次，第二次却给出 (%d,%d,%d)", dx2, dy2, action2)
	}
}

func TestInputStateQuit(t *testing.T) {
	in := newInputState(true)
	if in.takeQuit() {
		t.Fatal("没按 q 不应退出")
	}
	in.onKey('q')
	if !in.takeQuit() {
		t.Fatal("按了 q 应当退出")
	}
	if in.takeQuit() {
		t.Fatal("退出标记应当只生效一次")
	}
}

// TestAutoDecide 无人值守策略：朝敌人走，进范围就打，没敌人就原地。
func TestAutoDecide(t *testing.T) {
	var teams [protocol.PlayersPerRoom]uint8
	snap := &protocol.Snapshot{Players: []protocol.SnapshotPlayer{
		{Slot: 0, X: 5000, Y: 5000, HP: 100},
		{Slot: 5, X: 5300, Y: 5000, HP: 100},
	}}
	teams[5] = 1
	dx, _, action := autoDecide(snap, 0, 0, teams)
	if dx <= 0 {
		t.Fatalf("敌人右方，dx 应为正，实际 %d", dx)
	}
	if action != protocol.ActionNone {
		t.Fatalf("距离 300 > 命中半径 %d，不该攻击，实际 action=%d", protocol.AttackRange, action)
	}

	// 贴脸时必须攻击
	snap.Players[1].X = 5000 + protocol.AttackRange/2
	_, _, action = autoDecide(snap, 0, 0, teams)
	if action != protocol.ActionAttack {
		t.Fatalf("进入命中半径后应当攻击，实际 action=%d", action)
	}

	// 没有敌人：不动
	empty := &protocol.Snapshot{Players: snap.Players[:1]}
	if dx, dy, a := autoDecide(empty, 0, 0, teams); dx != 0 || dy != 0 || a != protocol.ActionNone {
		t.Fatalf("没有敌人时不该有动作，实际 (%d,%d,%d)", dx, dy, a)
	}

	// 队友不追
	snap.Players[1].X = 5100
	teams[5] = 0
	if dx, _, _ := autoDecide(snap, 0, 0, teams); dx != 0 {
		t.Fatalf("不该追队友，实际 dx=%d", dx)
	}
}

// TestRecorderWritesValidJSONL 记录器产物必须是「能被 analysis 直接读」的格式：
// bots.jsonl 逐行 JSON，sessions.json 是 analyze.py --reference 的输入格式。
func TestRecorderWritesValidJSONL(t *testing.T) {
	dir := t.TempDir()
	rec := newRecorder(dir, "human-test", 42)
	if rec.Count() != 0 {
		t.Fatal("新记录器应当是 0 局")
	}
	rec.Trace(10, 28, 0, protocol.ActionAttack)
	rec.Match(sink.SessionRecord{
		RunID: rec.runID, Persona: "human", AccountID: "human-test", PlayerID: 42,
		ScheduledAtMs: time.Now().UnixMilli(), SessionMs: 8000,
		Outcome: "match_complete", Matches: 1, Result: "draw", InputsSent: 33, Ticks: 80,
	}, 8*time.Second)
	rec.Close()

	raw, err := os.ReadFile(filepath.Join(dir, "bots.jsonl"))
	if err != nil {
		t.Fatalf("bots.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("应当只有 1 行记录，实际 %d 行", len(lines))
	}
	var decoded sink.SessionRecord
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("bots.jsonl 不是合法 JSON: %v", err)
	}
	if decoded.Persona != "human" || decoded.SessionMs != 8000 || decoded.Result != "draw" {
		t.Fatalf("记录字段不对：%+v", decoded)
	}

	// trace.jsonl 也必须是合法 JSON
	traw, err := os.ReadFile(filepath.Join(dir, "trace.jsonl"))
	if err != nil {
		t.Fatalf("trace.jsonl: %v", err)
	}
	var tl traceLine
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(traw))), &tl); err != nil {
		t.Fatalf("trace.jsonl 不是合法 JSON: %v", err)
	}
	if tl.Tick != 10 || tl.Action != protocol.ActionAttack {
		t.Fatalf("trace 内容不对：%+v", tl)
	}

	// sessions.json 必须能被 analyze.py 的 load_reference 解析（对象 + session_seconds 数组）
	sraw, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("sessions.json: %v", err)
	}
	var ref struct {
		SessionSeconds []float64 `json:"session_seconds"`
	}
	if err := json.Unmarshal(sraw, &ref); err != nil {
		t.Fatalf("sessions.json 不是合法 JSON: %v", err)
	}
	if len(ref.SessionSeconds) != 1 || ref.SessionSeconds[0] != 8 {
		t.Fatalf("sessions.json 内容不对：%+v", ref)
	}
}

// TestRecorderDisabled 不给 -out 时必须是彻底的空操作，不能 panic。
func TestRecorderDisabled(t *testing.T) {
	rec := newRecorder("", "x", 1)
	rec.Trace(1, 0, 0, 0)
	rec.Match(sink.SessionRecord{}, time.Second)
	rec.Close()
	if rec.Count() != 1 {
		t.Fatalf("内存内计数应当仍然累加，实际 %d", rec.Count())
	}
}
