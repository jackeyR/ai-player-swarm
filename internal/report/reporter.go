// Package report 把 bot 侧的测量聚合成分位数快照。
//
// 这层存在的理由（C4）：Prometheus 的 histogram 是固定桶，桶边界一旦定死就没法
// 事后改；而压测报告的 p99 需要高精度。所以同一次测量走两条路：
//
//	Prometheus histogram → 看板与告警（粗粒度、可聚合）
//	HdrHistogram         → 报告里的分位数（高精度、可跨进程合并）
//
// 两边都做 coordinated-omission 修正：以「平均到达间隔」作为 expected interval
// 传给 RecordDurationWithExpected。
//
// 一个必须说清的细节：**修正后直方图的 count 不是观测次数**。
// 一个 5s 的响应在 200ms 的预期间隔下会被补记 25 个样本，这是 on purpose。
// 所以这里另外维护一套真实观测计数（Samples），报告里两个数都要写：
// 「178 次观测，修正后样本数 521」——只写一个都是误导。
package report

import (
	"sync"
	"time"

	"aiplayer/swarm/internal/hist"
)

// Reporter 是并发安全的分位数采集器。
type Reporter struct {
	mu       sync.Mutex
	expected time.Duration
	phases   map[string]*hist.Recorder
	latency  map[string]*hist.Recorder
	samples  map[string]int64
}

const (
	maxValueMicros = int64(600_000_000) // 10 分钟，够覆盖任何一次会话
	sigfigs        = 3
)

// New 创建采集器。expected 是平均到达间隔，用于 coordinated omission 修正。
func New(expected time.Duration) *Reporter {
	if expected <= 0 {
		expected = time.Second
	}
	return &Reporter{
		expected: expected,
		phases:   make(map[string]*hist.Recorder, 8),
		latency:  make(map[string]*hist.Recorder, 8),
		samples:  make(map[string]int64, 16),
	}
}

// Phase 记录「从应到时刻到某个里程碑」的累计耗时。
func (r *Reporter) Phase(name string, offsetPlusDur time.Duration) {
	if offsetPlusDur < 0 {
		return
	}
	r.record(r.phases, "phase."+name, offsetPlusDur)
}

// Latency 记录一段独立的耗时（例如匹配等待）。
func (r *Reporter) Latency(name string, d time.Duration) {
	if d < 0 {
		return
	}
	r.record(r.latency, "latency."+name, d)
}

func (r *Reporter) record(m map[string]*hist.Recorder, key string, d time.Duration) {
	r.mu.Lock()
	rec, ok := m[key]
	if !ok {
		rec = hist.NewRecorder(sigfigs, maxValueMicros)
		m[key] = rec
	}
	r.samples[key]++
	exp := r.expected
	r.mu.Unlock()
	rec.RecordDurationWithExpected(d, exp)
}

// Snapshot 返回全部分位数快照（已做 CO 修正，count 是修正后的样本数）。
func (r *Reporter) Snapshot() map[string]hist.Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]hist.Snapshot, len(r.phases)+len(r.latency))
	for k, v := range r.phases {
		out[k] = v.Snapshot()
	}
	for k, v := range r.latency {
		out[k] = v.Snapshot()
	}
	return out
}

// Samples 返回每个指标的**真实观测次数**（CO 修正前的分母）。
func (r *Reporter) Samples() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int64, len(r.samples))
	for k, v := range r.samples {
		out[k] = v
	}
	return out
}

// ExpectedInterval 返回用于修正的预期间隔，写进报告便于复核。
func (r *Reporter) ExpectedInterval() time.Duration { return r.expected }
