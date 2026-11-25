// Package sink 负责把 bot 侧的结果落盘（E9 / H1 / H8）。
//
// 选 JSONL 而不是 Parquet 直接写：Go 侧 JSONL 零依赖、可按行 tail、崩溃也不丢已写行；
// Parquet 由 analysis/ 里的 Python 侧（DuckDB）从 JSONL 转换，一次转换换回列存查询性能。
// 这是「写的时候省事、读的时候用对的工具」的折中。
package sink

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Phase 是一个阶段耗时。
//
// OffsetMs 是相对**应到时刻**（scheduled arrival）的偏移，DurMs 是阶段自身耗时。
// 用应到时刻做基准而不是「实际开始时刻」，就是 bot 侧的 coordinated omission 修正：
// 探针迟到了，是系统的锅，不能让探针自己把这段延迟藏起来（C3）。
type Phase struct {
	Name     string `json:"name"`
	OffsetMs int64  `json:"offset_ms"`
	DurMs    int64  `json:"dur_ms"`
}

// SessionRecord 是一个 bot 一次会话的完整结果，一行一条。
type SessionRecord struct {
	RunID      string `json:"run_id"`
	Experiment string `json:"experiment,omitempty"`
	BotIndex   int    `json:"bot_index"`
	Persona    string `json:"persona"`
	Seed       uint64 `json:"seed"`
	AccountID  string `json:"account_id"`
	PlayerID   uint64 `json:"player_id,omitempty"`
	MMRInitial int    `json:"mmr_initial,omitempty"`
	MMRFinal   int    `json:"mmr_final,omitempty"`

	ScheduledAtMs int64 `json:"scheduled_at_unix_ms"`
	// StartLagMs 是 bot 真正被唤起相对应到时刻的滞后（bot 端过载的早期信号）。
	StartLagMs int64 `json:"start_lag_ms"`
	SessionMs  int64 `json:"session_ms"`

	Outcome   string `json:"outcome"`
	ErrorCode string `json:"error_code,omitempty"`

	Matches int     `json:"matches"`
	Phases  []Phase `json:"phases,omitempty"`

	MatchID     string `json:"match_id,omitempty"`
	MatchWaitMs int64  `json:"match_wait_ms,omitempty"`
	AllocMicros int64  `json:"room_alloc_micros,omitempty"`
	JoinMs      int64  `json:"room_join_ms,omitempty"`
	MatchMs     int64  `json:"match_duration_ms,omitempty"`
	Result      string `json:"result,omitempty"`

	InputsSent     int `json:"inputs_sent"`
	InputsRejected int `json:"inputs_rejected"`
	InvalidInputs  int `json:"invalid_inputs"`
	Disconnects    int `json:"disconnects"`
	Reconnects     int `json:"reconnects"`
	Ticks          int `json:"ticks"`
}

// RunRecord 是一次实验的汇总，写在同一个文件的最后一行。
type RunRecord struct {
	Type            string         `json:"type"` // "run_summary"
	RunID           string         `json:"run_id"`
	Experiment      string         `json:"experiment,omitempty"`
	PlanFingerprint string         `json:"plan_fingerprint,omitempty"`
	StartedAt       int64          `json:"started_at_unix_ms"`
	FinishedAt      int64          `json:"finished_at_unix_ms"`
	DurationMs      int64          `json:"duration_ms"`
	Arrivals        int            `json:"arrivals"`
	Sessions        int            `json:"sessions"`
	Matches         int            `json:"matches"`
	Goodput         float64        `json:"goodput_matches_per_min"`
	Stats           map[string]any `json:"stats,omitempty"`
}

// Writer 是并发安全的 JSONL 写入器。
//
// 关键点（E9）：**每 10 秒聚合写一次、行缓冲、崩溃最多丢 10 秒**。
// 3000 个 bot 逐个 flush 会把磁盘打成瓶颈，那样测出来的是自己的 IO 性能。
type Writer struct {
	mu      sync.Mutex
	f       *os.File
	bw      *bufio.Writer
	enc     *json.Encoder
	lines   atomic.Int64
	bytes   atomic.Int64
	errs    atomic.Int64
	closed  atomic.Bool
	path    string
	created time.Time
}

// New 创建写入器，必要时创建父目录。
func New(path string, bufBytes int) (*Writer, error) {
	if bufBytes <= 0 {
		bufBytes = 256 << 10
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("sink: 创建目录: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("sink: 打开 %s: %w", path, err)
	}
	bw := bufio.NewWriterSize(f, bufBytes)
	return &Writer{
		f:       f,
		bw:      bw,
		enc:     json.NewEncoder(bw),
		path:    path,
		created: time.Now(),
	}, nil
}

// Path 返回文件路径。
func (w *Writer) Path() string { return w.path }

// Write 写一条记录（自动补换行，JSONL）。
func (w *Writer) Write(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed.Load() {
		return fmt.Errorf("sink: writer closed")
	}
	n := w.bw.Buffered()
	if err := w.enc.Encode(v); err != nil {
		w.errs.Add(1)
		return fmt.Errorf("sink: 编码记录: %w", err)
	}
	w.lines.Add(1)
	w.bytes.Add(int64(w.bw.Buffered() - n))
	return nil
}

// Flush 把缓冲刷到磁盘。
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed.Load() {
		return nil
	}
	if err := w.bw.Flush(); err != nil {
		w.errs.Add(1)
		return err
	}
	// 落盘但不 fsync 每次：压测结束时才需要严格持久化，中途丢最后几行无所谓。
	return nil
}

// Close 刷盘并关闭文件。
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed.Swap(true) {
		return nil
	}
	flushErr := w.bw.Flush()
	closeErr := w.f.Close()
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

// Lines 返回已写行数。
func (w *Writer) Lines() int64 { return w.lines.Load() }

// Bytes 返回已写字节数。
func (w *Writer) Bytes() int64 { return w.bytes.Load() }

// Errors 返回编码错误次数。
func (w *Writer) Errors() int64 { return w.errs.Load() }
