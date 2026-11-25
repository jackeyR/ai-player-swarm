package sut_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aiplayer/swarm/internal/behavior"
	"aiplayer/swarm/internal/bot"
	"aiplayer/swarm/internal/metrics"
	"aiplayer/swarm/internal/obs"
	"aiplayer/swarm/internal/runstats"
	"aiplayer/swarm/internal/sink"
	"aiplayer/swarm/srv/config"
	"aiplayer/swarm/srv/sut"
)

// testPersona 是确定性 persona：不注入任何失败路径，专测主路径能不能跑通。
func testPersona() *behavior.Persona {
	p := &behavior.Persona{
		Name:              "e2e",
		Weight:            1,
		SessionMinutes:    behavior.Const(1),
		MatchesPerSession: behavior.Const(1),
		RequeueGapS:       behavior.Const(0.1),
		ThinkTimeMs:       behavior.Const(40),
		BurstIntervalMs:   behavior.Const(40),
		IdleIntervalMs:    behavior.Const(40),
		BurstP:            1,
		ActionsPerMatch:   behavior.Const(40),
		MaxReconnects:     1,
		MMRMean:           1500,
		MMRStd:            0,
	}
	return p
}

func smokeConfig() *config.Config {
	cfg := config.Default()
	cfg.Gateway.Addr = "127.0.0.1:0"
	cfg.RoomAdvertiseAddr = "127.0.0.1:0"
	cfg.MetricsAddr = sut.MetricsDisabled
	// 极短的对局：20ms tick × 100 帧 = 2s（足够走完出生点的 1000 单位距离并打出击杀）。
	cfg.Room.TickRateMs = 20
	cfg.Room.DurationTicks = 100
	cfg.Room.FillTimeoutMs = 300
	cfg.Room.EmptyTTLMs = 400
	cfg.Room.HeartbeatTimeoutMs = 3000
	// 1v1 就足够验证「匹配 → 分配 → 进房 → 打 → 结算」整条链路。
	cfg.Match.TeamSize = 1
	cfg.Match.StarveAfterMs = 3000
	cfg.Match.LoopIntervalMs = 20
	// 关掉刻意瓶颈：这个测试验证的是正确性，不是性能。
	cfg.Alloc.GlobalLock = false
	cfg.Alloc.PoolHotKey = false
	cfg.Alloc.RedisLatencyMs = 0
	cfg.Alloc.RefillIntervalMs = 20
	cfg.Settlement.FsyncMs = 0
	cfg.Redis.LatencyMs = 0
	return cfg
}

// TestEndToEndMatch 是最重要的一条测试：进程内起整套 SUT，
// 用真的 WebSocket + TCP 客户端跑完一局，并检查房间被正确回收。
//
// 它覆盖的链路：Hello → Welcome → 登录签名 → 排队 → 匹配 → 分配 →
// 房间 TCP 进房 → 10Hz tick + 输入与快照 → 结算 → 房间回收。
func TestEndToEndMatch(t *testing.T) {
	cfg := smokeConfig()
	sys, err := sut.New(cfg)
	if err != nil {
		t.Fatalf("组装 SUT: %v", err)
	}
	if err := sys.Start(context.Background()); err != nil {
		t.Fatalf("启动 SUT: %v", err)
	}
	defer sys.Stop()

	dir := t.TempDir()
	w, err := sink.New(filepath.Join(dir, "bots.jsonl"), 64<<10)
	if err != nil {
		t.Fatalf("创建结果文件: %v", err)
	}
	defer func() { _ = w.Close() }()

	reg := metrics.New()
	o := obs.NewRunner(reg)
	stats := &runstats.Stats{}
	persona := testPersona()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	now := time.Now()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b := bot.New(bot.Config{
				GatewayURL:        sys.GatewayWSURL(),
				Mode:              "ranked5v5",
				ClientSecret:      []byte(cfg.Gateway.ClientSecret),
				DialTimeout:       5 * time.Second,
				MatchWaitTimeout:  10 * time.Second,
				HeartbeatInterval: 2 * time.Second,
				InputTickInterval: 5 * time.Millisecond,
			}, bot.Deps{
				Runner:  o,
				Writer:  w,
				Stats:   stats,
				RunID:   "e2e",
				RunSeed: 42,
			}, idx, persona)
			b.Run(ctx, now)
		}(i)
	}
	wg.Wait()
	_ = w.Flush()
	data, _ := os.ReadFile(w.Path())
	t.Logf("结果文件：\n%s", string(data))
	t.Logf("结算记录：%+v", sys.Settle.Tail(4))
	for _, rec := range sys.Settle.Tail(1) {
		for _, p := range rec.Players {
			t.Logf("玩家 %d（队 %d 槽 %d）击杀=%d 死亡=%d 得分=%d",
				p.PlayerID, p.Team, p.Slot, p.Kills, p.Deaths, p.Score)
		}
	}

	completed := stats.Matches.Load()
	if completed != 2 {
		t.Fatalf("2 个 bot 各应完成 1 局，实际 %d：%+v", completed, stats.Take())
	}
	// 胜负必须被真实判定出来：三类结果之和等于完成局数，不许有「算了但没归类」。
	if wins, losses, draws := stats.MatchesWin.Load(), stats.MatchesLoss.Load(), stats.MatchesDraw.Load(); wins+losses+draws != completed {
		t.Fatalf("结果分类与完成局数不一致：%+v", stats.Take())
	}

	// 最关键的断言：这一局里真的发生了击杀。
	//
	// 只断言「跑完了一局」是不够的——两队出生点离得太远时，链路全程绿灯、
	// 指标也有数，但对局是 0:0，胜负逻辑一行都没执行到。这种人眼看不出的
	// 假通过，只有对着服务端的击杀数做断言才拦得住。
	kills := 0
	for _, rec := range sys.Settle.Tail(4) {
		for _, p := range rec.Players {
			kills += int(p.Kills)
		}
	}
	if kills == 0 {
		t.Fatalf("整局没有任何击杀，胜负逻辑未被覆盖（对局成了 0:0 平局）")
	}

	// 房间必须被回收：这是 D4「空房泄漏」的守卫。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sys.Rooms.RoomCount() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := sys.Rooms.RoomCount(); n != 0 {
		t.Fatalf("房间未回收，剩余 %d 间（janitor 失效）", n)
	}

	// 结算记录必须写进库（goodput 的服务端口径）。
	if n := sys.Settle.Count(); n < 1 {
		t.Fatalf("结算库为空，期望至少 1 条")
	}

	// 结果文件：每个 bot 一行。
	if got := w.Lines(); got != 2 {
		t.Fatalf("期望 2 行结果，实际 %d 行：\n%s", got, string(data))
	}
	if !strings.Contains(string(data), `"outcome":"session_complete"`) {
		t.Errorf("结果文件里没有 session_complete：\n%s", string(data))
	}
}

// TestMetricsExposition 检查指标端点的文本格式真的符合 Prometheus 暴露格式，
// 并且 §2.8 的六个必埋指标名都在。
func TestMetricsExposition(t *testing.T) {
	cfg := smokeConfig()
	cfg.MetricsAddr = "127.0.0.1:0"
	sys, err := sut.New(cfg)
	if err != nil {
		t.Fatalf("组装 SUT: %v", err)
	}
	if err := sys.Start(context.Background()); err != nil {
		t.Fatalf("启动 SUT: %v", err)
	}
	defer sys.Stop()

	url := "http://" + sys.MetricsAddr() + "/metrics"
	client := &http.Client{Timeout: 5 * time.Second}
	var body string
	for i := 0; i < 20; i++ {
		resp, err := client.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if body == "" {
		t.Fatalf("指标端点在 %s 不可用", url)
	}

	for _, name := range []string{
		"match_wait_seconds", "room_alloc_seconds", "room_tick_total",
		"room_tick_overrun_total", "session_total", "session_errors_total",
		"reconnect_total", "reconnect_success_total", "room_leak_gauge",
	} {
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("缺少指标 %s 的 TYPE 声明", name)
		}
	}
}
