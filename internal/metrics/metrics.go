// Package metrics implements a small, dependency-free subset of a Prometheus
// client: counters, gauges, labeled vectors, histograms, a registry that
// renders the Prometheus text exposition format (version 0.0.4), and an
// optional runtime-metrics collector.
//
// All metric handles returned by a Registry are safe for concurrent use. The
// Registry itself is also safe for concurrent registration, vector lookups and
// scrapes.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	kindCounter   = "counter"
	kindGauge     = "gauge"
	kindHistogram = "histogram"
)

// ---------------------------------------------------------------------------
// Counters and gauges
// ---------------------------------------------------------------------------

// Counter is a monotonically increasing float64 counter. Its zero value is
// ready to use and it is safe for concurrent use.
type Counter struct {
	bits atomic.Uint64 // float64 stored as its IEEE-754 bits
}

// Add adds delta to the counter.
func (c *Counter) Add(delta float64) {
	for {
		old := c.bits.Load()
		next := math.Float64frombits(old) + delta
		if c.bits.CompareAndSwap(old, math.Float64bits(next)) {
			return
		}
	}
}

// Inc adds one to the counter.
func (c *Counter) Inc() { c.Add(1) }

// Value returns the current value.
func (c *Counter) Value() float64 {
	return math.Float64frombits(c.bits.Load())
}

// Gauge is a float64 value that can go up and down. Its zero value is ready to
// use and it is safe for concurrent use.
type Gauge struct {
	mu    sync.RWMutex
	value float64
	fn    func() float64
}

// Set sets the gauge to v.
func (g *Gauge) Set(v float64) {
	g.mu.Lock()
	g.value = v
	g.mu.Unlock()
}

// Add adds delta to the gauge.
func (g *Gauge) Add(delta float64) {
	g.mu.Lock()
	g.value += delta
	g.mu.Unlock()
}

// Inc adds one to the gauge.
func (g *Gauge) Inc() { g.Add(1) }

// Dec subtracts one from the gauge.
func (g *Gauge) Dec() { g.Add(-1) }

// SetFunc makes the gauge evaluate f on every read. This is useful for values
// that are cheap to obtain but expensive to keep updated, such as
// runtime.NumGoroutine(). Passing nil removes the function, after which Value
// returns the last stored value again. The function is called without any
// internal lock held, so it may itself read other metrics.
func (g *Gauge) SetFunc(f func() float64) {
	g.mu.Lock()
	g.fn = f
	g.mu.Unlock()
}

// Value returns the current value, evaluating the installed function if any.
func (g *Gauge) Value() float64 {
	g.mu.RLock()
	fn := g.fn
	v := g.value
	g.mu.RUnlock()
	if fn != nil {
		return fn()
	}
	return v
}

// ---------------------------------------------------------------------------
// Histogram observations
// ---------------------------------------------------------------------------

// Observer records observations into one series of a HistogramVec.
type Observer struct {
	bounds []float64 // sorted finite bucket upper bounds
	counts []atomic.Uint64
	sum    atomic.Uint64 // float64 bits
	count  atomic.Uint64
}

func newObserver(bounds []float64) *Observer {
	return &Observer{
		bounds: bounds,
		counts: make([]atomic.Uint64, len(bounds)),
	}
}

// Observe records one observation. Each observation increments exactly one
// finite bucket (the first bucket whose upper bound is >= v); observations
// above the largest finite bucket only appear in the implicit +Inf bucket.
// Observe is safe for concurrent use.
func (o *Observer) Observe(v float64) {
	idx := sort.SearchFloat64s(o.bounds, v)
	o.count.Add(1)
	atomicAddFloat(&o.sum, v)
	if idx < len(o.counts) {
		o.counts[idx].Add(1)
	}
}

func atomicAddFloat(addr *atomic.Uint64, delta float64) {
	for {
		old := addr.Load()
		next := math.Float64frombits(old) + delta
		if addr.CompareAndSwap(old, math.Float64bits(next)) {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Labeled vectors and series cache
// ---------------------------------------------------------------------------

type seriesCache[T any] struct {
	mu sync.RWMutex
	m  map[string]*T
}

func (c *seriesCache[T]) getOrCreate(key string, create func() *T) *T {
	c.mu.RLock()
	if c.m != nil {
		if s, ok := c.m[key]; ok {
			c.mu.RUnlock()
			return s
		}
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]*T)
	}
	if s, ok := c.m[key]; ok {
		return s
	}
	s := create()
	c.m[key] = s
	return s
}

func (c *seriesCache[T]) remove(key string) {
	c.mu.Lock()
	if c.m != nil {
		delete(c.m, key)
	}
	c.mu.Unlock()
}

func (c *seriesCache[T]) snapshot() map[string]*T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]*T, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}

func sortedKeys[T any](m map[string]*T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// labelSignature renders the label values as they appear inside {} in the
// text format, in registration order. It doubles as the series cache key: the
// escaping makes the representation unambiguous for fixed label names.
func labelSignature(metricName string, labelNames, labelValues []string) string {
	if len(labelValues) != len(labelNames) {
		panic(fmt.Sprintf("metrics: metric %q expects %d label values, got %d",
			metricName, len(labelNames), len(labelValues)))
	}
	if len(labelNames) == 0 {
		return ""
	}
	var b strings.Builder
	for i, name := range labelNames {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(labelValues[i]))
		b.WriteByte('"')
	}
	return b.String()
}

// normalizeSignature accepts either the bare label text (a="1",b="x") or the
// same text wrapped in braces, and returns the bare form used as a cache key.
func normalizeSignature(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}' {
		s = s[1 : len(s)-1]
	}
	return s
}

// CounterVec is a family of counters sharing a name, help text and labelNames.
type CounterVec struct {
	name       string
	help       string
	labelNames []string
	series     seriesCache[Counter]
}

// With returns the counter for the given label values, creating it on first
// use. The number of values must equal the number of registered label names;
// otherwise With panics (a programming error).
func (v *CounterVec) With(labelValues ...string) *Counter {
	key := labelSignature(v.name, v.labelNames, labelValues)
	return v.series.getOrCreate(key, func() *Counter { return &Counter{} })
}

func (v *CounterVec) metricName() string      { return v.name }
func (v *CounterVec) removeSeries(sig string) { v.series.remove(sig) }
func (v *CounterVec) render(b *strings.Builder) {
	writeFamilyHeader(b, v.name, v.help, kindCounter)
	m := v.series.snapshot()
	for _, key := range sortedKeys(m) {
		writeSampleName(b, v.name, key)
		b.WriteByte(' ')
		b.WriteString(formatFloat(m[key].Value()))
		b.WriteByte('\n')
	}
}

// GaugeVec is a family of gauges sharing a name, help text and labelNames.
type GaugeVec struct {
	name       string
	help       string
	labelNames []string
	series     seriesCache[Gauge]
}

// With returns the gauge for the given label values, creating it on first use.
// The number of values must equal the number of registered label names;
// otherwise With panics (a programming error).
func (v *GaugeVec) With(labelValues ...string) *Gauge {
	key := labelSignature(v.name, v.labelNames, labelValues)
	return v.series.getOrCreate(key, func() *Gauge { return &Gauge{} })
}

func (v *GaugeVec) metricName() string      { return v.name }
func (v *GaugeVec) removeSeries(sig string) { v.series.remove(sig) }
func (v *GaugeVec) render(b *strings.Builder) {
	writeFamilyHeader(b, v.name, v.help, kindGauge)
	m := v.series.snapshot()
	for _, key := range sortedKeys(m) {
		writeSampleName(b, v.name, key)
		b.WriteByte(' ')
		b.WriteString(formatFloat(m[key].Value()))
		b.WriteByte('\n')
	}
}

// HistogramVec is a family of histograms sharing a name, help text and bucket
// boundaries. Buckets are normalized at registration (sorted, deduplicated,
// +Inf and NaN removed).
type HistogramVec struct {
	name       string
	help       string
	labelNames []string
	buckets    []float64
	series     seriesCache[Observer]
}

// With returns the observer for the given label values, creating it on first
// use. The number of values must equal the number of registered label names;
// otherwise With panics (a programming error).
func (v *HistogramVec) With(labelValues ...string) *Observer {
	key := labelSignature(v.name, v.labelNames, labelValues)
	return v.series.getOrCreate(key, func() *Observer { return newObserver(v.buckets) })
}

func (v *HistogramVec) metricName() string      { return v.name }
func (v *HistogramVec) removeSeries(sig string) { v.series.remove(sig) }
func (v *HistogramVec) render(b *strings.Builder) {
	writeFamilyHeader(b, v.name, v.help, kindHistogram)
	m := v.series.snapshot()
	for _, key := range sortedKeys(m) {
		o := m[key]
		var cumulative uint64
		for i, bound := range v.buckets {
			cumulative += o.counts[i].Load()
			writeHistogramBucket(b, v.name, key, formatFloat(bound), cumulative)
		}
		count := o.count.Load()
		writeHistogramBucket(b, v.name, key, "+Inf", count)
		writeHistogramSum(b, v.name, key, math.Float64frombits(o.sum.Load()))
		writeHistogramCount(b, v.name, key, count)
	}
}

// normalizeBuckets sorts the boundaries, removes NaNs and +Inf (the +Inf
// bucket is always implied) and removes duplicates.
func normalizeBuckets(in []float64) []float64 {
	out := make([]float64, 0, len(in))
	for _, v := range in {
		if math.IsNaN(v) || math.IsInf(v, 1) {
			continue
		}
		out = append(out, v)
	}
	sort.Float64s(out)
	n := 0
	for _, v := range out {
		if n == 0 || v != out[n-1] {
			out[n] = v
			n++
		}
	}
	return out[:n]
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

type family interface {
	metricName() string
	removeSeries(sig string)
	render(b *strings.Builder)
}

// Registry owns a set of metric families and renders them in registration
// order. The zero value is an empty ready-to-use registry, but New is
// preferred.
//
// Registering an already registered name with the same kind and the same
// labels (and, for histograms, the same bucket boundaries) returns the
// existing family. Registering a name with a different kind, labels or buckets
// panics: that can only be a programming error. The help text of the first
// registration wins.
type Registry struct {
	mu       sync.RWMutex
	families []family
	byName   map[string]family
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{byName: make(map[string]family)}
}

func (r *Registry) addLocked(f family) {
	if r.byName == nil {
		r.byName = make(map[string]family)
	}
	r.byName[f.metricName()] = f
	r.families = append(r.families, f)
}

func equalLabels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalBuckets(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CounterVec registers (or returns) a counter family.
func (r *Registry) CounterVec(name, help string, labelNames ...string) *CounterVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byName[name]; ok {
		v, ok := existing.(*CounterVec)
		if !ok {
			panic(fmt.Sprintf("metrics: %q already registered with a different kind", name))
		}
		if !equalLabels(v.labelNames, labelNames) {
			panic(fmt.Sprintf("metrics: %q already registered with different label names", name))
		}
		return v
	}
	v := &CounterVec{
		name:       name,
		help:       help,
		labelNames: append([]string(nil), labelNames...),
	}
	r.addLocked(v)
	return v
}

// GaugeVec registers (or returns) a gauge family.
func (r *Registry) GaugeVec(name, help string, labelNames ...string) *GaugeVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byName[name]; ok {
		v, ok := existing.(*GaugeVec)
		if !ok {
			panic(fmt.Sprintf("metrics: %q already registered with a different kind", name))
		}
		if !equalLabels(v.labelNames, labelNames) {
			panic(fmt.Sprintf("metrics: %q already registered with different label names", name))
		}
		return v
	}
	v := &GaugeVec{
		name:       name,
		help:       help,
		labelNames: append([]string(nil), labelNames...),
	}
	r.addLocked(v)
	return v
}

// HistogramVec registers (or returns) a histogram family. Bucket boundaries
// are sorted and deduplicated; +Inf is always appended at render time.
func (r *Registry) HistogramVec(name, help string, buckets []float64, labelNames ...string) *HistogramVec {
	normalized := normalizeBuckets(buckets)
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byName[name]; ok {
		v, ok := existing.(*HistogramVec)
		if !ok {
			panic(fmt.Sprintf("metrics: %q already registered with a different kind", name))
		}
		if !equalLabels(v.labelNames, labelNames) || !equalBuckets(v.buckets, normalized) {
			panic(fmt.Sprintf("metrics: %q already registered with different labels or buckets", name))
		}
		return v
	}
	v := &HistogramVec{
		name:       name,
		help:       help,
		labelNames: append([]string(nil), labelNames...),
		buckets:    normalized,
	}
	r.addLocked(v)
	return v
}

// WriteText writes all registered families in the Prometheus text exposition
// format (version 0.0.4): families in registration order, series sorted by
// their label signature. It is safe to call concurrently with metric updates.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.RLock()
	families := make([]family, len(r.families))
	copy(families, r.families)
	r.mu.RUnlock()

	var b strings.Builder
	for _, f := range families {
		f.render(&b)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Handler returns an http.Handler that serves the registry in the Prometheus
// text format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = r.WriteText(w)
	})
}

// RemoveMetricSeries removes the series addressed by labelSignature from the
// named family, so that ended runs do not leak label cardinality. The
// signature is the label text exactly as it appears inside {} in the text
// format, in registration order (for example `run_id="abc",shard="0"`); the
// surrounding braces are optional. An empty signature addresses the single
// series of a family without labels. Removing an unknown family or series is a
// no-op.
func (r *Registry) RemoveMetricSeries(name, labelSignature string) {
	sig := normalizeSignature(labelSignature)
	r.mu.RLock()
	f := r.byName[name]
	r.mu.RUnlock()
	if f == nil {
		return
	}
	f.removeSeries(sig)
}

// EnableRuntimeMetrics registers process-level gauges: go_goroutines,
// go_threads, go_gc_count and a set of go_memstats_* gauges. They are
// evaluated lazily on every scrape. Calling it twice is harmless.
func (r *Registry) EnableRuntimeMetrics() {
	r.gaugeFunc("go_goroutines", "Number of goroutines that currently exist.", func() float64 {
		return float64(runtime.NumGoroutine())
	})
	r.gaugeFunc("go_threads", "Number of OS threads created.", func() float64 {
		n, _ := runtime.ThreadCreateProfile(nil)
		return float64(n)
	})
	r.gaugeFunc("go_gc_count", "Number of completed garbage collection cycles.", func() float64 {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return float64(ms.NumGC)
	})

	memstats := []struct {
		name  string
		help  string
		value func(*runtime.MemStats) float64
	}{
		{"go_memstats_alloc_bytes", "Number of bytes allocated and still in use.",
			func(ms *runtime.MemStats) float64 { return float64(ms.Alloc) }},
		{"go_memstats_alloc_bytes_total", "Total number of bytes allocated, even if freed.",
			func(ms *runtime.MemStats) float64 { return float64(ms.TotalAlloc) }},
		{"go_memstats_sys_bytes", "Number of bytes obtained from system.",
			func(ms *runtime.MemStats) float64 { return float64(ms.Sys) }},
		{"go_memstats_heap_alloc_bytes", "Number of heap bytes allocated and still in use.",
			func(ms *runtime.MemStats) float64 { return float64(ms.HeapAlloc) }},
		{"go_memstats_heap_sys_bytes", "Number of heap bytes obtained from system.",
			func(ms *runtime.MemStats) float64 { return float64(ms.HeapSys) }},
		{"go_memstats_heap_idle_bytes", "Number of heap bytes waiting to be used.",
			func(ms *runtime.MemStats) float64 { return float64(ms.HeapIdle) }},
		{"go_memstats_heap_inuse_bytes", "Number of heap bytes that are in use.",
			func(ms *runtime.MemStats) float64 { return float64(ms.HeapInuse) }},
		{"go_memstats_heap_released_bytes", "Number of heap bytes released to OS.",
			func(ms *runtime.MemStats) float64 { return float64(ms.HeapReleased) }},
		{"go_memstats_heap_objects", "Number of allocated objects.",
			func(ms *runtime.MemStats) float64 { return float64(ms.HeapObjects) }},
		{"go_memstats_stack_inuse_bytes", "Number of bytes in use by the stack allocator.",
			func(ms *runtime.MemStats) float64 { return float64(ms.StackInuse) }},
		{"go_memstats_stack_sys_bytes", "Number of bytes obtained from system for stack allocator.",
			func(ms *runtime.MemStats) float64 { return float64(ms.StackSys) }},
		{"go_memstats_mallocs_total", "Total number of mallocs.",
			func(ms *runtime.MemStats) float64 { return float64(ms.Mallocs) }},
		{"go_memstats_frees_total", "Total number of frees.",
			func(ms *runtime.MemStats) float64 { return float64(ms.Frees) }},
		{"go_memstats_gc_sys_bytes", "Number of bytes used for garbage collection system metadata.",
			func(ms *runtime.MemStats) float64 { return float64(ms.GCSys) }},
		{"go_memstats_other_sys_bytes", "Number of bytes used for other system allocations.",
			func(ms *runtime.MemStats) float64 { return float64(ms.OtherSys) }},
		{"go_memstats_next_gc_bytes", "Number of heap bytes when next garbage collection will take place.",
			func(ms *runtime.MemStats) float64 { return float64(ms.NextGC) }},
		{"go_memstats_last_gc_time_seconds", "Number of seconds since 1970 of last garbage collection.",
			func(ms *runtime.MemStats) float64 { return float64(ms.LastGC) / 1e9 }},
		{"go_memstats_pause_total_ns", "Cumulative nanoseconds in GC stop-the-world pauses since the program started.",
			func(ms *runtime.MemStats) float64 { return float64(ms.PauseTotalNs) }},
		{"go_memstats_num_gc", "Number of completed GC cycles.",
			func(ms *runtime.MemStats) float64 { return float64(ms.NumGC) }},
	}
	for _, s := range memstats {
		s := s
		r.gaugeFunc(s.name, s.help, func() float64 {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			return s.value(&ms)
		})
	}
}

func (r *Registry) gaugeFunc(name, help string, fn func() float64) {
	r.GaugeVec(name, help).With().SetFunc(fn)
}

// ---------------------------------------------------------------------------
// Text rendering helpers
// ---------------------------------------------------------------------------

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func writeFamilyHeader(b *strings.Builder, name, help, kind string) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(escapeHelp(help))
	b.WriteByte('\n')
	b.WriteString("# TYPE ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(kind)
	b.WriteByte('\n')
}

func writeSampleName(b *strings.Builder, name, labels string) {
	b.WriteString(name)
	if labels == "" {
		return
	}
	b.WriteByte('{')
	b.WriteString(labels)
	b.WriteByte('}')
}

func writeHistogramBucket(b *strings.Builder, name, labels, le string, count uint64) {
	b.WriteString(name)
	b.WriteString("_bucket{")
	if labels != "" {
		b.WriteString(labels)
		b.WriteByte(',')
	}
	b.WriteString(`le="`)
	b.WriteString(le)
	b.WriteString(`"}`)
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(count, 10))
	b.WriteByte('\n')
}

func writeHistogramSum(b *strings.Builder, name, labels string, sum float64) {
	b.WriteString(name)
	b.WriteString("_sum")
	if labels != "" {
		b.WriteByte('{')
		b.WriteString(labels)
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(formatFloat(sum))
	b.WriteByte('\n')
}

func writeHistogramCount(b *strings.Builder, name, labels string, count uint64) {
	b.WriteString(name)
	b.WriteString("_count")
	if labels != "" {
		b.WriteByte('{')
		b.WriteString(labels)
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(count, 10))
	b.WriteByte('\n')
}
