// Package obs 集中定义全平台的指标名、标签与分桶。
//
// 为什么要单独一个包：指标名一旦散落在各处，PromQL 与 Grafana 面板就会漂。
// 这里是单一事实来源，docs/METRICS.md 与本文件一一对应（对应知识清单 G2 / G3 / G5）。
//
// 纪律（G5）：**bot_id / player_id / room_id 绝不进标签**。它们是无界基数，
// 一旦进标签，Prometheus 的倒排索引会先把你压垮。个体信息只进日志与结果文件。
package obs

import (
	"time"

	"aiplayer/swarm/internal/metrics"
)

// 分桶定义。直方图一律用固定桶（G3：summary 的分位数不可聚合，所以统一用 histogram）。
var (
	// LatencyBuckets 用于各类「秒级」耗时，覆盖 1ms 到 30s。
	LatencyBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

	// MatchWaitBuckets 用于排队等待，从 100ms 到 2min（§2.8 目标 p99 < 8s）。
	MatchWaitBuckets = []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 60, 120}

	// TickBuckets 用于房间单帧耗时，围绕 16ms 预算布桶（D2）。
	TickBuckets = []float64{0.001, 0.002, 0.004, 0.008, 0.012, 0.016, 0.024, 0.033, 0.05, 0.1, 0.25}

	// AllocBuckets 用于分配器，§2.8 目标 p99 < 300ms。
	AllocBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.3, 1, 3}
)

// MMRBucketLabel 把 MMR 映射成低基数标签。
// 匹配等待必须按 MMR 分桶看，否则冷门分段的饥饿会被整体 p99 掩盖（D13）。
func MMRBucketLabel(mmr int) string {
	switch {
	case mmr < 900:
		return "lt900"
	case mmr < 1200:
		return "900_1199"
	case mmr < 1500:
		return "1200_1499"
	case mmr < 1800:
		return "1500_1799"
	case mmr < 2100:
		return "1800_2099"
	default:
		return "ge2100"
	}
}

// Server 是被测系统（SUT）侧的指标集合。
//
// 方案 §2.8 规定的 6 个必埋指标全部在此：match_wait_seconds / room_alloc_seconds /
// room_tick_budget_overrun_ratio（由 tick_overrun_total 与 tick_total 相除得到）/
// session_error_ratio（session_errors_total / session_total）/ reconnect_success_ratio /
// room_leak_gauge。
type Server struct {
	Reg *metrics.Registry

	// 登录与鉴权
	LoginTotal  *metrics.CounterVec // labels: result
	LoginErrors *metrics.CounterVec // labels: code

	// 匹配（match_wait_seconds 按 MMR 桶，D10）
	QueueJoin        *metrics.CounterVec   // labels: mode
	QueueCancel      *metrics.CounterVec   // labels: mode
	MatchWait        *metrics.HistogramVec // labels: mmr_bucket
	MatchTotal       *metrics.CounterVec   // labels: mode
	MatchMMRSpread   *metrics.HistogramVec // labels: mode，匹配质量代理指标（D11）
	MatchesImmediate *metrics.CounterVec   // labels: mode，不等凑齐就开局

	// 分配（room_alloc_seconds）
	RoomAlloc     *metrics.HistogramVec // labels: result
	RoomAllocMiss *metrics.CounterVec   // labels: reason
	RoomsCreated  *metrics.CounterVec   // labels: mode
	RoomsClosed   *metrics.CounterVec   // labels: why
	RoomsRunning  *metrics.GaugeVec     // labels: mode

	// 房间 tick（room_tick_budget_overrun_ratio 的分子分母）
	TickTotal    *metrics.CounterVec   // 无标签
	TickOverrun  *metrics.CounterVec   // 无标签
	TickDuration *metrics.HistogramVec // 无标签
	TickLag      *metrics.HistogramVec // 无标签，调度滞后

	// 会话
	SessionTotal  *metrics.CounterVec // labels: result
	SessionErrors *metrics.CounterVec // labels: code

	// 输入与状态同步（房间内热路径）
	InputTotal      *metrics.CounterVec // 无标签
	InputRejected   *metrics.CounterVec // labels: code
	SnapshotSent    *metrics.CounterVec // 无标签
	SnapshotDropped *metrics.CounterVec // 无标签，写队列满被丢（A14 背压）
	RoomFillTimeout *metrics.CounterVec // 无标签，等人进房超时

	// 重连（reconnect_success_ratio 的分子分母）
	ReconnectTotal   *metrics.CounterVec // 无标签
	ReconnectSuccess *metrics.CounterVec // 无标签

	// 房间泄漏（恒为 0 是目标）
	RoomLeak *metrics.GaugeVec // 无标签

	// 结算写库
	SettleWrite *metrics.HistogramVec // labels: result

	// 在线量
	ActivePlayers *metrics.GaugeVec // 无标签
	ActiveMatches *metrics.GaugeVec // 无标签
}

// NewServer 在给定 registry 上注册全部服务端指标。
func NewServer(reg *metrics.Registry) *Server {
	s := &Server{Reg: reg}

	s.LoginTotal = reg.CounterVec("login_total", "login requests handled by the gateway", "result")
	s.LoginErrors = reg.CounterVec("login_errors_total", "login failures by error code", "code")

	s.QueueJoin = reg.CounterVec("queue_join_total", "players entering the match queue", "mode")
	s.QueueCancel = reg.CounterVec("queue_cancel_total", "players cancelling while queued", "mode")
	s.MatchWait = reg.HistogramVec("match_wait_seconds",
		"seconds from queue join to match formed, bucketed by MMR band", MatchWaitBuckets, "mmr_bucket")
	s.MatchTotal = reg.CounterVec("matches_created_total", "matches formed by the matchmaker", "mode")
	s.MatchMMRSpread = reg.HistogramVec("match_mmr_spread",
		"max-min MMR inside a formed match (matchmaking quality proxy)",
		[]float64{50, 100, 200, 400, 800, 1600}, "mode")
	s.MatchesImmediate = reg.CounterVec("match_immediate_total", "matches formed before the roster was full", "mode")

	s.RoomAlloc = reg.HistogramVec("room_alloc_seconds",
		"seconds for the allocator to hand out a room", AllocBuckets, "result")
	s.RoomAllocMiss = reg.CounterVec("room_alloc_miss_total", "allocation attempts that found an empty warm pool", "reason")
	s.RoomsCreated = reg.CounterVec("rooms_created_total", "rooms created", "mode")
	s.RoomsClosed = reg.CounterVec("rooms_closed_total", "rooms reclaimed", "why")
	s.RoomsRunning = reg.GaugeVec("rooms_running", "rooms currently in the running state", "mode")

	s.TickTotal = reg.CounterVec("room_tick_total", "room ticks executed")
	s.TickOverrun = reg.CounterVec("room_tick_overrun_total", "room ticks that exceeded the frame budget")
	s.TickDuration = reg.HistogramVec("room_tick_duration_seconds", "time spent inside one room tick", TickBuckets)
	s.TickLag = reg.HistogramVec("room_tick_lag_seconds", "delay between scheduled and actual tick start", TickBuckets)

	s.SessionTotal = reg.CounterVec("session_total", "player sessions by outcome", "result")
	s.SessionErrors = reg.CounterVec("session_errors_total", "player sessions ending in an error", "code")

	s.InputTotal = reg.CounterVec("room_input_total", "inputs accepted by room servers")
	s.InputRejected = reg.CounterVec("room_input_rejected_total", "inputs rejected by the authoritative server", "code")
	s.SnapshotSent = reg.CounterVec("room_snapshot_sent_total", "snapshots broadcast to players")
	s.SnapshotDropped = reg.CounterVec("room_snapshot_dropped_total", "snapshots dropped because a send queue was full")
	s.RoomFillTimeout = reg.CounterVec("room_fill_timeout_total", "rooms where the fill timeout expired before all players joined")

	s.ReconnectTotal = reg.CounterVec("reconnect_total", "reconnect attempts")
	s.ReconnectSuccess = reg.CounterVec("reconnect_success_total", "successful reconnects")

	s.RoomLeak = reg.GaugeVec("room_leak_gauge", "rooms that outlived their lifecycle and were not reclaimed")

	s.SettleWrite = reg.HistogramVec("settle_write_seconds", "time to persist a settlement record", AllocBuckets, "result")

	s.ActivePlayers = reg.GaugeVec("players_online", "connected players")
	s.ActiveMatches = reg.GaugeVec("matches_active", "matches in progress")
	return s
}

// Runner 是 bot 集群侧的指标集合。
//
// 「控制器开销占比」（§5.1）靠 bot 端进程 CPU 与 SUT 侧 CPU 对比得出。
// 这里把 bot 端自己的调度滞后、goroutine 数一并暴露，避免「测的是 bot 的性能」。
type Runner struct {
	Reg *metrics.Registry

	Arrivals       *metrics.CounterVec   // labels: persona
	ScheduleLag    *metrics.HistogramVec // 无标签，实际唤起相对应到时刻的滞后
	Active         *metrics.GaugeVec     // labels: run
	Phase          *metrics.HistogramVec // labels: phase，从应到时刻到该里程碑的累计耗时
	Sessions       *metrics.CounterVec   // labels: persona, outcome
	BotErrors      *metrics.CounterVec   // labels: code
	InputsSent     *metrics.CounterVec   // labels: persona
	InputsInvalid  *metrics.CounterVec   // labels: persona
	InputsRejected *metrics.CounterVec   // labels: code
	Disconnects    *metrics.CounterVec   // labels: persona
	Reconnects     *metrics.CounterVec   // labels: outcome
	Snapshots      *metrics.CounterVec   // 无标签
	SessionSeconds *metrics.HistogramVec // labels: persona
	RTT            *metrics.HistogramVec // 无标签，应用层心跳 RTT
	Goroutines     *metrics.GaugeVec     // 无标签
}

// NewRunner 在给定 registry 上注册全部 bot 侧指标。
func NewRunner(reg *metrics.Registry) *Runner {
	r := &Runner{Reg: reg}

	r.Arrivals = reg.CounterVec("bot_arrivals_total", "bots dispatched by the open-loop scheduler", "persona")
	r.ScheduleLag = reg.HistogramVec("bot_schedule_lag_seconds",
		"delay between the scheduled arrival time and the bot actually starting", LatencyBuckets)
	r.Active = reg.GaugeVec("bot_active", "bots currently running", "run")
	r.Phase = reg.HistogramVec("bot_phase_seconds",
		"latency from scheduled arrival to each milestone (coordinated-omission corrected)",
		LatencyBuckets, "phase")
	r.Sessions = reg.CounterVec("bot_sessions_total", "bot sessions by outcome", "persona", "outcome")
	r.BotErrors = reg.CounterVec("bot_errors_total", "bot errors by code", "code")
	r.InputsSent = reg.CounterVec("bot_inputs_sent_total", "inputs sent to room servers", "persona")
	r.InputsInvalid = reg.CounterVec("bot_invalid_inputs_total", "deliberately invalid inputs injected", "persona")
	r.InputsRejected = reg.CounterVec("bot_inputs_rejected_total", "inputs rejected by the authoritative server", "code")
	r.Disconnects = reg.CounterVec("bot_disconnects_total", "injected or observed disconnects", "persona")
	r.Reconnects = reg.CounterVec("bot_reconnect_total", "reconnect attempts by outcome", "outcome")
	r.Snapshots = reg.CounterVec("bot_snapshots_total", "room snapshots received")
	r.SessionSeconds = reg.HistogramVec("bot_session_seconds",
		"bot session duration", []float64{5, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200}, "persona")
	r.RTT = reg.HistogramVec("bot_heartbeat_rtt_seconds", "application-level heartbeat RTT", LatencyBuckets)
	r.Goroutines = reg.GaugeVec("bot_goroutines", "goroutines in the runner process")
	return r
}

// Seconds 把时长转成秒，统一指标单位。
func Seconds(d time.Duration) float64 { return d.Seconds() }
