// Package runner 是 bot 集群的执行单元：一台机器上一个进程，跑 N 个 bot。
//
// 对应知识清单 B10（并发聚合）、E5（开环调度器）、E9（结果落盘）、C2/C3（开环与修正）。
//
// 关键纪律：
//   - 调度器只负责「到点就起一个 bot」，绝不等待 bot 完成（闭环会美化 p99）；
//   - 并发上限用信号量控制，超限表现为**调度滞后**而不是静默降速——
//     滞后会被如实记进 bot_schedule_lag_seconds，这样你才看得出「是 bot 端先崩了」。
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"aiplayer/swarm/internal/behavior"
	"aiplayer/swarm/internal/bot"
	"aiplayer/swarm/internal/hist"
	"aiplayer/swarm/internal/metrics"
	"aiplayer/swarm/internal/obs"
	"aiplayer/swarm/internal/plan"
	"aiplayer/swarm/internal/report"
	"aiplayer/swarm/internal/runstats"
	"aiplayer/swarm/internal/sched"
	"aiplayer/swarm/internal/sink"
)

// Config 是 runner 的运行参数。
type Config struct {
	// ID 是本次 run 的标识（也用作结果子目录名）。
	ID string
	// Plan 是负载计划（必填）。
	Plan *plan.Plan
	// Catalog 是 persona 目录（必填）。
	Catalog *behavior.Catalog
	// MetricsAddr 非空时 runner 自己起一个 /metrics 端点。
	MetricsAddr string
	// Registry 允许复用外部 registry（controller 模式下共享一个）。
	Registry *metrics.Registry
}

// Result 是一次 run 的汇总。
type Result struct {
	RunID       string            `json:"run_id"`
	PlanName    string            `json:"plan_name"`
	Fingerprint string            `json:"plan_fingerprint"`
	StartedAt   time.Time         `json:"started_at"`
	FinishedAt  time.Time         `json:"finished_at"`
	DurationMs  int64             `json:"duration_ms"`
	Arrivals    int               `json:"arrivals"`
	Sessions    int64             `json:"sessions"`
	Matches     int64             `json:"matches_completed"`
	Goodput     float64           `json:"goodput_matches_per_min"`
	OutputPath  string            `json:"output_path"`
	Lines       int64             `json:"lines"`
	Stats       runstats.Snapshot `json:"stats"`
	// Histograms 的分位数已做 coordinated-omission 修正；其中 count 是**修正后**的
	// 样本数，真实观测次数见 Samples（两个数都要看，否则会被自己的修正骗到）。
	Histograms   map[string]hist.Snapshot `json:"histograms,omitempty"`
	Samples      map[string]int64         `json:"samples,omitempty"`
	COIntervalMs int64                    `json:"co_expected_interval_ms"`
}

// Runner 是一次压测执行的载体。
type Runner struct {
	cfg    Config
	reg    *metrics.Registry
	o      *obs.Runner
	writer *sink.Writer
	rep    *report.Reporter
	stats  *runstats.Stats
	dir    string
	ownReg bool

	active atomic.Int64

	httpSrv  *http.Server
	stopCh   chan struct{}
	stopOnce sync.Once
}

// New 创建 runner。
func New(cfg Config) (*Runner, error) {
	if cfg.Plan == nil {
		return nil, fmt.Errorf("runner: 缺少 plan")
	}
	if cfg.Catalog == nil {
		return nil, fmt.Errorf("runner: 缺少 persona 目录")
	}
	if cfg.ID == "" {
		cfg.ID = fmt.Sprintf("run-%s", time.Now().Format("20060102-150405"))
	}
	ownReg := false
	if cfg.Registry == nil {
		cfg.Registry = metrics.New()
		cfg.Registry.EnableRuntimeMetrics()
		ownReg = true
	}

	dir := filepath.Join(cfg.Plan.Sink.Dir, cfg.ID)
	writer, err := sink.New(filepath.Join(dir, "bots.jsonl"), cfg.Plan.Sink.BufferBytes)
	if err != nil {
		return nil, err
	}

	r := &Runner{
		cfg:    cfg,
		reg:    cfg.Registry,
		o:      obs.NewRunner(cfg.Registry),
		writer: writer,
		rep:    report.New(cfg.Plan.ExpectedInterval()),
		stats:  &runstats.Stats{},
		dir:    dir,
		ownReg: ownReg,
		stopCh: make(chan struct{}),
	}
	r.o.Goroutines.With().SetFunc(func() float64 { return float64(runtime.NumGoroutine()) })
	r.o.Active.With(cfg.ID).Set(0)
	return r, nil
}

// ID 返回 run 标识。
func (r *Runner) ID() string { return r.cfg.ID }

// Dir 返回结果目录。
func (r *Runner) Dir() string { return r.dir }

// Stop 请求停止（幂等）。
func (r *Runner) Stop() {
	r.stopOnce.Do(func() { close(r.stopCh) })
}

// Run 执行一次完整的压测，阻塞到结束。
func (r *Runner) Run(ctx context.Context) (*Result, error) {
	started := time.Now()

	// 1) 实验快照（H8）：计划原文 + 元信息，先落盘再干活。
	//    这样即使是压到一半进程崩了，也还知道当时跑的是什么。
	if err := r.snapshotPlan(); err != nil {
		return nil, err
	}

	// 2) 自带 /metrics 端点（单进程模式）。
	if r.ownReg && r.cfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", r.reg.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
		r.httpSrv = &http.Server{Addr: r.cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := r.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "runner: metrics 端点: %v\n", err)
			}
		}()
	}

	// 3) 周期刷盘 + 周期性指标更新。
	var wgFlush sync.WaitGroup
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wgFlush.Add(1)
	go func() {
		defer wgFlush.Done()
		r.backgroundLoop(runCtx)
	}()

	// 4) 生成到达时间表并开跑。
	arrivals, err := r.cfg.Plan.Traffic.Arrivals()
	if err != nil {
		cancel()
		wgFlush.Wait()
		return nil, err
	}

	botCfg := r.botConfig()
	deps := bot.Deps{
		Runner:     r.o,
		Writer:     r.writer,
		Reporter:   r.rep,
		Stats:      r.stats,
		RunID:      r.cfg.ID,
		Experiment: r.cfg.Plan.Name,
		RunSeed:    r.cfg.Plan.Seed,
	}
	maxConcurrent := r.cfg.Plan.Limits.MaxConcurrentBots
	if maxConcurrent <= 0 {
		maxConcurrent = 2000
	}
	sem := make(chan struct{}, maxConcurrent)
	var wgBots sync.WaitGroup

	dispatcher := &sched.Dispatcher{LagThreshold: 50 * time.Millisecond}
	dispatcher.Run(runCtx, started, arrivals, func(bctx context.Context, a sched.Arrival, due time.Time) {
		// wgBots.Add 必须在**调度器 goroutine 里同步**执行：
		// 若放进下面那个 goroutine 内部，就会与 wgBots.Wait() 竞态——
		// Wait 可能在 Add 之前看到计数归零而提前返回，最后几个 bot 还没起跑
		// 就被收尾流程掐掉，结果文件里静默少几行（这个 bug 真的出现过）。
		wgBots.Add(1)
		r.active.Add(1)
		go func() {
			defer wgBots.Done()
			// 并发上限用信号量而不是丢请求：超限表现为「调度滞后」，
			// 滞后会被如实记进 bot_schedule_lag_seconds，这样你才看得出是 bot 端先崩了。
			select {
			case sem <- struct{}{}:
			case <-bctx.Done():
				r.active.Add(-1)
				return
			}
			defer func() {
				<-sem
				r.active.Add(-1)
			}()
			defer func() {
				// 单个 bot panic 不应该带走整个 runner（E10：崩溃隔离的第一层）。
				if rec := recover(); rec != nil {
					r.o.BotErrors.With("panic").Inc()
					fmt.Fprintf(os.Stderr, "runner: bot %d panic: %v\n", a.Index, rec)
				}
			}()
			persona := r.cfg.Catalog.ForIndex(a.Index, r.cfg.Plan.Seed)
			r.o.Arrivals.With(persona.Name).Inc()
			b := bot.New(botCfg, deps, a.Index, persona)
			b.Run(bctx, due)
		}()
	})

	// 5) 等全部 bot 收尾。ctx 取消时 bot 会快速返回，所以这里不会挂太久。
	wgBots.Wait()
	cancel()
	wgFlush.Wait()
	if r.httpSrv != nil {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 3*time.Second)
		_ = r.httpSrv.Shutdown(shutdownCtx)
		cancelShutdown()
	}

	finished := time.Now()
	st := r.stats.Take()
	res := &Result{
		RunID:        r.cfg.ID,
		PlanName:     r.cfg.Plan.Name,
		StartedAt:    started,
		FinishedAt:   finished,
		DurationMs:   finished.Sub(started).Milliseconds(),
		Arrivals:     len(arrivals),
		Sessions:     st.Sessions,
		Matches:      st.Matches,
		OutputPath:   r.writer.Path(),
		Lines:        r.writer.Lines(),
		Stats:        st,
		Histograms:   r.rep.Snapshot(),
		Samples:      r.rep.Samples(),
		COIntervalMs: r.rep.ExpectedInterval().Milliseconds(),
	}
	if fp, err := r.cfg.Plan.Fingerprint(); err == nil {
		res.Fingerprint = fp
	}
	// goodput 用「真正打完的局数」而不是会话数（§5.1：有效对局吞吐，不是连接数）。
	if d := finished.Sub(started).Seconds(); d > 0 {
		res.Goodput = float64(st.Matches) / d * 60
	}

	// 6) 汇总行写进同一个文件，方便 analysis 只读一个文件。
	summary := sink.RunRecord{
		Type:            "run_summary",
		RunID:           r.cfg.ID,
		Experiment:      r.cfg.Plan.Name,
		PlanFingerprint: res.Fingerprint,
		StartedAt:       started.UnixMilli(),
		FinishedAt:      finished.UnixMilli(),
		DurationMs:      res.DurationMs,
		Arrivals:        res.Arrivals,
		Sessions:        int(res.Sessions),
		Matches:         int(res.Matches),
		Goodput:         res.Goodput,
		Stats: map[string]any{
			"counters":         res.Stats,
			"histograms":       res.Histograms,
			"samples":          res.Samples,
			"co_interval_ms":   res.COIntervalMs,
			"plan_fingerprint": res.Fingerprint,
		},
	}
	// 汇总行必须写成功：analysis 靠它读概况，写丢了等于这轮实验没有结论。
	if err := r.writer.Write(summary); err != nil {
		return res, fmt.Errorf("runner: 写 run_summary: %w", err)
	}
	if err := r.writer.Close(); err != nil {
		return res, fmt.Errorf("runner: 关闭结果文件: %w", err)
	}
	if err := writeJSON(filepath.Join(r.dir, "result.json"), res); err != nil {
		return res, err
	}
	return res, nil
}

func (r *Runner) botConfig() bot.Config {
	p := r.cfg.Plan
	return bot.Config{
		GatewayURL:        p.Target.GatewayURL,
		Mode:              p.Target.Mode,
		ClientSecret:      []byte(p.Target.ClientSecret),
		Platform:          p.Target.Platform,
		DialTimeout:       5 * time.Second,
		MatchWaitTimeout:  time.Duration(p.Limits.MatchWaitTimeoutS) * time.Second,
		HeartbeatInterval: time.Duration(p.Limits.HeartbeatIntervalS) * time.Second,
		RoomDrainTimeout:  time.Duration(p.Limits.RoomDrainTimeoutS) * time.Second,
	}
}

func (r *Runner) snapshotPlan() error {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return fmt.Errorf("runner: 创建结果目录: %w", err)
	}
	b, err := yaml.Marshal(r.cfg.Plan)
	if err != nil {
		return fmt.Errorf("runner: 序列化计划: %w", err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "plan.yaml"), b, 0o644); err != nil {
		return fmt.Errorf("runner: 写计划快照: %w", err)
	}
	meta := map[string]any{
		"run_id":      r.cfg.ID,
		"plan_name":   r.cfg.Plan.Name,
		"seed":        r.cfg.Plan.Seed,
		"personas":    r.cfg.Catalog.Names(),
		"gateway_url": r.cfg.Plan.Target.GatewayURL,
		"started_at":  time.Now().UTC().Format(time.RFC3339),
		"go_version":  runtime.Version(),
		"gomaxprocs":  runtime.GOMAXPROCS(0),
	}
	if fp, err := r.cfg.Plan.Fingerprint(); err == nil {
		meta["plan_fingerprint"] = fp
	}
	return writeJSON(filepath.Join(r.dir, "meta.json"), meta)
}

func (r *Runner) backgroundLoop(ctx context.Context) {
	flush := time.NewTicker(time.Second)
	defer flush.Stop()
	gauges := time.NewTicker(2 * time.Second)
	defer gauges.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = r.writer.Flush()
			return
		case <-r.stopCh:
			_ = r.writer.Flush()
			return
		case <-flush.C:
			_ = r.writer.Flush()
		case <-gauges.C:
			r.o.Active.With(r.cfg.ID).Set(float64(r.active.Load()))
		}
	}
}

// writeJSON 写一个 JSON 文件。
//
// 注意这里**必须**用 encoding/json：之前偷懒用了 yaml.Marshal，结果文件名叫
// result.json、内容是 YAML，python 的 json.load 直接炸，而人眼完全看不出来。
// 这种「文件名与内容格式不一致」的 bug，只有让消费者真的去解析一次才会暴露。
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("runner: 编码 %s: %w", path, err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("runner: 写 %s: %w", path, err)
	}
	return nil
}
