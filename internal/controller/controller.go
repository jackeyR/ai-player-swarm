// Package controller 是控制面：接受负载计划、启动/停止 runner、汇总状态。
//
// 对应知识清单 §2.3（控制面 10 个端点足够）与 B12（优雅退出）。
// 刻意不引框架：标准库 net/http + 一个 map 就够了，引 Gin 只是给自己加依赖。
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"aiplayer/swarm/internal/behavior"
	"aiplayer/swarm/internal/metrics"
	"aiplayer/swarm/internal/plan"
	"aiplayer/swarm/internal/runner"
)

// Config 是控制面配置。
type Config struct {
	Addr string
	// SinkDir 是结果根目录（覆盖计划里的 sink.dir）。
	SinkDir string
	// MaxConcurrentRuns 限制同时运行的实验数。默认 1：
	// 两个实验同时跑，指标会互相污染，那样的数据没有意义。
	MaxConcurrentRuns int
	// Registry 是共享的指标 registry。
	Registry *metrics.Registry
	// DefaultCatalog 是未指定 persona 文件时使用的目录。
	DefaultCatalog *behavior.Catalog
}

// Status 是 run 的状态。
type Status string

// 状态取值。
const (
	StatusRunning   Status = "running"
	StatusFinished  Status = "finished"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Run 是一次实验的状态。
type Run struct {
	ID          string         `json:"id"`
	PlanName    string         `json:"plan_name"`
	Fingerprint string         `json:"plan_fingerprint,omitempty"`
	Status      Status         `json:"status"`
	StartedAt   time.Time      `json:"started_at"`
	FinishedAt  *time.Time     `json:"finished_at,omitempty"`
	Error       string         `json:"error,omitempty"`
	OutputDir   string         `json:"output_dir,omitempty"`
	Result      *runner.Result `json:"result,omitempty"`

	cancel context.CancelFunc
}

// Controller 是控制面实现。
type Controller struct {
	cfg Config
	reg *metrics.Registry

	mu   sync.Mutex
	runs map[string]*Run
	seq  []string

	httpSrv *http.Server
}

// New 创建控制面。
func New(cfg Config) *Controller {
	if cfg.Addr == "" {
		cfg.Addr = ":7070"
	}
	if cfg.MaxConcurrentRuns <= 0 {
		cfg.MaxConcurrentRuns = 1
	}
	if cfg.Registry == nil {
		cfg.Registry = metrics.New()
		cfg.Registry.EnableRuntimeMetrics()
	}
	if cfg.DefaultCatalog == nil {
		cfg.DefaultCatalog = behavior.Default()
	}
	return &Controller{
		cfg:  cfg,
		reg:  cfg.Registry,
		runs: make(map[string]*Run, 8),
	}
}

// Handler 返回 HTTP 路由（便于测试挂 httptest）。
func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.Handle("/metrics", c.reg.Handler())
	mux.HandleFunc("/v1/runs", c.handleRuns)
	mux.HandleFunc("/v1/runs/", c.handleRunByID)
	return mux
}

// ListenAndServe 启动控制面。
func (c *Controller) ListenAndServe() error {
	c.httpSrv = &http.Server{Addr: c.cfg.Addr, Handler: c.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return c.httpSrv.ListenAndServe()
}

// Shutdown 优雅退出：先取消所有在跑的 run，再关 HTTP。
func (c *Controller) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	for _, r := range c.runs {
		if r.Status == StatusRunning && r.cancel != nil {
			r.cancel()
		}
	}
	c.mu.Unlock()
	if c.httpSrv != nil {
		return c.httpSrv.Shutdown(ctx)
	}
	return nil
}

// RunRequest 是 POST /v1/runs 的请求体。
type RunRequest struct {
	// PlanPath 是服务器本地路径（控制面与计划文件在同一台机器时最方便）。
	PlanPath string `json:"plan_path" yaml:"plan_path"`
	// Plan 是内联计划（远程提交时用）。
	Plan *plan.Plan `json:"plan" yaml:"plan"`
}

func (c *Controller) handleRuns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		c.mu.Lock()
		out := make([]*Run, 0, len(c.seq))
		for i := len(c.seq) - 1; i >= 0; i-- {
			if run, ok := c.runs[c.seq[i]]; ok {
				cp := *run
				cp.cancel = nil
				out = append(out, &cp)
			}
		}
		c.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"runs": out})
	case http.MethodPost:
		c.startRun(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (c *Controller) startRun(w http.ResponseWriter, r *http.Request) {
	limited := 200 << 10
	body, err := readAll(r, limited)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	p, err := decodePlan(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if c.cfg.SinkDir != "" {
		p.Sink.Dir = c.cfg.SinkDir
	}
	p.Defaults()
	if err := p.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	c.mu.Lock()
	running := 0
	for _, run := range c.runs {
		if run.Status == StatusRunning {
			running++
		}
	}
	if running >= c.cfg.MaxConcurrentRuns {
		c.mu.Unlock()
		writeJSON(w, http.StatusTooManyRequests,
			map[string]any{"error": fmt.Sprintf("已有 %d 个实验在跑（上限 %d）；并发跑实验会互相污染指标", running, c.cfg.MaxConcurrentRuns)})
		return
	}

	catalog := c.cfg.DefaultCatalog
	if p.Behaviors.CatalogFile != "" {
		loaded, err := behavior.LoadFile(p.Behaviors.CatalogFile)
		if err != nil {
			c.mu.Unlock()
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		catalog = loaded
	}

	runID := fmt.Sprintf("%s-%s-%s", sanitize(p.Name), time.Now().Format("20060102-150405"), randHex(3))
	rl, err := runner.New(runner.Config{
		ID:          runID,
		Plan:        p,
		Catalog:     catalog,
		Registry:    c.reg,
		MetricsAddr: "", // 控制面统一暴露 /metrics
	})
	if err != nil {
		c.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	run := &Run{
		ID:        runID,
		PlanName:  p.Name,
		Status:    StatusRunning,
		StartedAt: time.Now(),
		OutputDir: rl.Dir(),
		cancel:    cancel,
	}
	if fp, err := p.Fingerprint(); err == nil {
		run.Fingerprint = fp
	}
	c.runs[runID] = run
	c.seq = append(c.seq, runID)
	c.mu.Unlock()

	go func() {
		res, err := rl.Run(ctx)
		now := time.Now()
		c.mu.Lock()
		defer c.mu.Unlock()
		run.FinishedAt = &now
		run.cancel = nil
		if err != nil {
			run.Status = StatusFailed
			run.Error = err.Error()
			return
		}
		if ctx.Err() != nil {
			run.Status = StatusCancelled
		} else {
			run.Status = StatusFinished
		}
		run.Result = res
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":         run.ID,
		"status":     run.Status,
		"output_dir": run.OutputDir,
	})
}

func (c *Controller) handleRunByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "missing run id"})
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	c.mu.Lock()
	run, ok := c.runs[id]
	c.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found: " + id})
		return
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		c.mu.Lock()
		cp := *run
		cp.cancel = nil
		c.mu.Unlock()
		writeJSON(w, http.StatusOK, cp)
	case action == "stop" && r.Method == http.MethodPost:
		c.mu.Lock()
		if run.Status != StatusRunning {
			status := run.Status
			c.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "run is not running", "status": status})
			return
		}
		if run.cancel != nil {
			run.cancel()
		}
		run.Status = StatusCancelled
		c.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": StatusCancelled})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "unsupported action"})
	}
}

func decodePlan(body []byte) (*plan.Plan, error) {
	var req RunRequest
	if err := yaml.Unmarshal(body, &req); err == nil {
		if req.Plan != nil {
			return req.Plan, nil
		}
		if req.PlanPath != "" {
			return plan.Load(req.PlanPath)
		}
	}
	var p plan.Plan
	if err := yaml.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("controller: 解析计划: %w", err)
	}
	if p.Target.GatewayURL == "" {
		return nil, fmt.Errorf("controller: 请求体既不是 {plan_path} 也不是 {plan}，也不是完整的负载计划")
	}
	return &p, nil
}

func readAll(r *http.Request, limit int) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if len(buf) > limit {
			return nil, fmt.Errorf("请求体超过 %d 字节", limit)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func sanitize(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "run"
	}
	if len(out) > 24 {
		out = out[:24]
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(b)
}
