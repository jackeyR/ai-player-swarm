// Package hist implements a zero-dependency, HdrHistogram-style histogram.
//
// Values are kept in a logarithmic-linear bucket structure: values up to
// 2^subBucketCountMagnitude share one-unit-wide buckets, and every octave
// above that is split into the same number of sub-buckets, each twice as wide
// as the corresponding sub-bucket of the octave below. With the usual three
// significant figures that gives 2048 sub-buckets per octave (a worst-case
// relative quantization error of about 1/2048, i.e. 0.05%).
//
// A Histogram is NOT safe for concurrent use. Use Recorder when several
// goroutines record into the same histogram; Recorder serializes access with a
// mutex. (Or protect the Histogram with your own lock.)
package hist

import (
	"fmt"
	"math"
	"math/bits"
	"sync"
	"time"
)

const (
	defaultSigFigs = 3
	minSigFigs     = 1
	maxSigFigs     = 5

	// maxInt64 is used as the default upper bound for a lazily initialized
	// zero-value Histogram.
	maxInt64 = int64(1<<63 - 1)
)

// Histogram is a logarithmic-linear value histogram.
//
// The zero value is safe to use: it lazily initializes itself with three
// significant figures and the full int64 value range. For anything else use
// New. A Histogram must not be copied after first use, and it is not safe for
// concurrent use.
type Histogram struct {
	sigfigs   int
	maxValue  int64 // largest value that may be recorded (inclusive)
	subCount  int64 // sub-bucket count per octave, a power of two
	subCountM int   // log2(subCount)

	counts    []int64 // per-bucket observation counts
	bucketMax []int64 // largest value actually recorded in each bucket

	count    int64   // total observations
	sum      float64 // sum of recorded values, for Mean
	minValue int64   // smallest recorded value; only valid when count > 0
	maxSeen  int64   // largest recorded value; only valid when count > 0
}

// New creates a histogram with the given number of significant figures
// (normally 3) and the given inclusive maximum recordable value. Values are
// clamped into [1, maxValue]; maxValue is raised to 1 if smaller. sigfigs is
// clamped into [1, 5].
func New(sigfigs int, maxValue int64) *Histogram {
	h := &Histogram{}
	h.init(sigfigs, maxValue)
	return h
}

func (h *Histogram) init(sigfigs int, maxValue int64) {
	if sigfigs < minSigFigs {
		sigfigs = minSigFigs
	}
	if sigfigs > maxSigFigs {
		sigfigs = maxSigFigs
	}
	if maxValue < 1 {
		maxValue = 1
	}

	// The classic HdrHistogram rule: keep single-unit resolution up to
	// 2*10^sigfigs, then use that many sub-buckets per octave.
	pow10 := int64(1)
	for i := 0; i < sigfigs; i++ {
		pow10 *= 10
	}
	subCountM := bits.Len64(uint64(2*pow10) - 1) // ceil(log2(2*10^sigfigs))
	subCount := int64(1) << uint(subCountM)

	// Highest octave bucket that can hold maxValue. Bucket 0 covers
	// [0, 2^subCountM); bucket b >= 1 covers
	// [2^(subCountM+b-1), 2^(subCountM+b)) with sub-buckets of width 2^(b-1).
	maxBucket := bits.Len64(uint64(maxValue)) - subCountM
	if maxBucket < 1 {
		maxBucket = 1
	}

	h.sigfigs = sigfigs
	h.maxValue = maxValue
	h.subCount = subCount
	h.subCountM = subCountM
	n := (maxBucket + 1) * int(subCount)
	h.counts = make([]int64, n)
	h.bucketMax = make([]int64, n)
	h.minValue = 0
	h.maxSeen = 0
}

// ensure lazily initializes a zero-value Histogram.
func (h *Histogram) ensure() {
	if h.counts == nil {
		h.init(defaultSigFigs, maxInt64)
	}
}

// Record records v. Values <= 0 are recorded as 1 and values > maxValue are
// clamped to maxValue. Record does not lock; it is not safe to call
// concurrently on the same Histogram (use Recorder).
func (h *Histogram) Record(v int64) {
	h.ensure()
	if v < 1 {
		v = 1
	}
	if v > h.maxValue {
		v = h.maxValue
	}
	h.record(v)
}

// record records an already clamped value (1 <= v <= maxValue).
func (h *Histogram) record(v int64) {
	idx := h.bucketIndex(v)
	h.counts[idx]++
	if v > h.bucketMax[idx] {
		h.bucketMax[idx] = v
	}
	h.count++
	h.sum += float64(v)
	if h.count == 1 {
		h.minValue = v
		h.maxSeen = v
		return
	}
	if v < h.minValue {
		h.minValue = v
	}
	if v > h.maxSeen {
		h.maxSeen = v
	}
}

// bucketIndex maps a value in [1, maxValue] to its flat bucket index.
func (h *Histogram) bucketIndex(v int64) int {
	n := bits.Len64(uint64(v))
	if n <= h.subCountM {
		// Linear region: one bucket per value.
		return int(v)
	}
	b := n - h.subCountM
	// Sub-bucket width is 2^(b-1); subIndex selects a value range inside the
	// octave. The first octave (b == 1) still has unit width.
	sub := int64((v >> uint(b-1)) - h.subCount)
	return b*int(h.subCount) + int(sub)
}

// bucketUpper returns the inclusive upper bound of the flat bucket at idx.
// For the top bucket the bound is capped at maxValue, since larger values are
// clamped into it and can never actually be present.
func (h *Histogram) bucketUpper(idx int) int64 {
	b := idx / int(h.subCount)
	sub := int64(idx % int(h.subCount))
	if b == 0 {
		return sub
	}
	shift := uint(b - 1)
	upper := ((h.subCount + sub + 1) << shift) - 1
	if upper > h.maxValue {
		upper = h.maxValue
	}
	return upper
}

// RecordWithExpectedInterval records v with coordinated-omission correction.
//
// If v > expectedInterval it first records the synthesized values
// v-expectedInterval, v-2*expectedInterval, ... down to (and including) the
// first value that is still >= expectedInterval, then records v itself. This
// matches HdrHistogram's recordValueWithExpectedInterval: recording 1000 with
// an expected interval of 100 records 900, 800, ..., 100 and finally 1000,
// i.e. 10 observations in total. expectedInterval <= 0 is equivalent to
// Record(v).
func (h *Histogram) RecordWithExpectedInterval(v int64, expectedInterval int64) {
	h.ensure()
	if expectedInterval <= 0 || v <= 0 {
		h.Record(v)
		return
	}
	for missing := v - expectedInterval; missing >= expectedInterval; missing -= expectedInterval {
		h.Record(missing)
	}
	h.Record(v)
}

// RecordDuration records d in microseconds.
func (h *Histogram) RecordDuration(d time.Duration) {
	h.Record(d.Microseconds())
}

// RecordDurationWithExpected records d in microseconds using
// RecordWithExpectedInterval with the expected interval in microseconds.
func (h *Histogram) RecordDurationWithExpected(d, expected time.Duration) {
	h.RecordWithExpectedInterval(d.Microseconds(), expected.Microseconds())
}

// TotalCount returns the number of recorded observations.
func (h *Histogram) TotalCount() int64 { return h.count }

// Min returns the smallest recorded value, or 0 when the histogram is empty.
func (h *Histogram) Min() int64 {
	if h.count == 0 {
		return 0
	}
	return h.minValue
}

// Max returns the largest recorded value, or 0 when the histogram is empty.
func (h *Histogram) Max() int64 {
	if h.count == 0 {
		return 0
	}
	return h.maxSeen
}

// Mean returns the arithmetic mean of the recorded values, or 0 when empty.
func (h *Histogram) Mean() float64 {
	if h.count == 0 {
		return 0
	}
	return h.sum / float64(h.count)
}

// ValueAtQuantile returns the highest recorded value in the bucket containing
// the q-quantile (0 < q <= 1). The returned value was actually recorded (it is
// the largest observation in that bucket), not the bucket's lower bound. It
// returns 0 for an empty histogram.
func (h *Histogram) ValueAtQuantile(q float64) int64 {
	if h.count == 0 {
		return 0
	}
	if math.IsNaN(q) {
		return 0
	}
	if q > 1 {
		q = 1
	}
	var target int64
	if q >= 1 {
		target = h.count
	} else {
		target = int64(math.Ceil(q * float64(h.count)))
		if target < 1 {
			target = 1
		}
	}
	var cumulative int64
	for i, c := range h.counts {
		cumulative += c
		if cumulative >= target {
			return h.bucketMax[i]
		}
	}
	// Unreachable for a consistent histogram.
	return h.maxSeen
}

// Reset clears all observations while keeping the configured parameters.
func (h *Histogram) Reset() {
	h.count = 0
	h.sum = 0
	h.minValue = 0
	h.maxSeen = 0
	for i := range h.counts {
		h.counts[i] = 0
		h.bucketMax[i] = 0
	}
}

// Merge adds all observations of o into h. The two histograms must have been
// created with the same parameters (sigfigs and maxValue); otherwise Merge
// returns an error and h is unchanged.
//
// Histogram.Merge is not safe for concurrent use.
func (h *Histogram) Merge(o *Histogram) error {
	if o == nil {
		return fmt.Errorf("hist: cannot merge nil histogram")
	}
	if h.counts == nil && o.counts == nil {
		return nil
	}
	if h.counts == nil || o.counts == nil {
		return fmt.Errorf("hist: cannot merge an uninitialized histogram (use New)")
	}
	if h.sigfigs != o.sigfigs || h.maxValue != o.maxValue ||
		h.subCount != o.subCount || len(h.counts) != len(o.counts) {
		return fmt.Errorf("hist: incompatible histograms (sigfigs %d/%d, maxValue %d/%d)",
			h.sigfigs, o.sigfigs, h.maxValue, o.maxValue)
	}
	if o.count == 0 {
		return nil
	}
	hHadValues := h.count > 0
	for i, c := range o.counts {
		if c == 0 {
			continue
		}
		h.counts[i] += c
		if o.bucketMax[i] > h.bucketMax[i] {
			h.bucketMax[i] = o.bucketMax[i]
		}
	}
	if !hHadValues || o.minValue < h.minValue {
		h.minValue = o.minValue
	}
	if !hHadValues || o.maxSeen > h.maxSeen {
		h.maxSeen = o.maxSeen
	}
	h.count += o.count
	h.sum += o.sum
	return nil
}

// Export visits every non-empty bucket in ascending value order. value is the
// bucket's inclusive upper bound; count is the number of observations in that
// bucket. Iteration stops when iter returns false.
func (h *Histogram) Export(iter func(value int64, count int64) bool) {
	if iter == nil {
		return
	}
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		if !iter(h.bucketUpper(i), c) {
			return
		}
	}
}

// Bucket is one cumulative histogram bucket.
type Bucket struct {
	Value int64 // inclusive upper bound
	Count int64 // cumulative count of values <= Value
}

// Buckets returns cumulative counts in ascending value order, using at most n
// buckets. When the histogram has more non-empty buckets than n, a uniform
// subset is chosen that always includes the last non-empty bucket, so that the
// final Count equals TotalCount. It returns nil for an empty histogram or when
// n <= 0.
func (h *Histogram) Buckets(n int) []Bucket {
	if n <= 0 || len(h.counts) == 0 {
		return nil
	}
	idx := make([]int, 0, 64)
	for i, c := range h.counts {
		if c > 0 {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return nil
	}

	cum := make([]int64, len(idx))
	var running int64
	for k, i := range idx {
		running += h.counts[i]
		cum[k] = running
	}

	project := func(k int) Bucket {
		return Bucket{Value: h.bucketUpper(idx[k]), Count: cum[k]}
	}

	if len(idx) <= n {
		out := make([]Bucket, len(idx))
		for k := range idx {
			out[k] = project(k)
		}
		return out
	}

	out := make([]Bucket, 0, n)
	last := len(idx) - 1
	for k := 0; k < n; k++ {
		j := last
		if n > 1 {
			// Round(k * last / (n-1)); includes both endpoints and is
			// strictly increasing because last >= n here.
			j = (k*last + (n-1)/2) / (n - 1)
		}
		out = append(out, project(j))
	}
	return out
}

// Snapshot is the JSON-serializable quantile summary of a Histogram.
type Snapshot struct {
	Count int64   `json:"count"`
	Min   int64   `json:"min"`
	Max   int64   `json:"max"`
	Mean  float64 `json:"mean"`
	P50   int64   `json:"p50"`
	P90   int64   `json:"p90"`
	P95   int64   `json:"p95"`
	P99   int64   `json:"p99"`
	P999  int64   `json:"p999"`
}

// Snapshot returns a consistent quantile summary. The caller must ensure the
// histogram is not being modified concurrently.
func (h *Histogram) Snapshot() Snapshot {
	return Snapshot{
		Count: h.TotalCount(),
		Min:   h.Min(),
		Max:   h.Max(),
		Mean:  h.Mean(),
		P50:   h.ValueAtQuantile(0.50),
		P90:   h.ValueAtQuantile(0.90),
		P95:   h.ValueAtQuantile(0.95),
		P99:   h.ValueAtQuantile(0.99),
		P999:  h.ValueAtQuantile(0.999),
	}
}

// Recorder is a concurrency-safe recording wrapper around a Histogram.
//
// All Recorder methods except Histogram take the internal mutex. Histogram
// returns the internal pointer without locking; see its documentation. The
// zero value of Recorder lazily initializes a histogram on first Record,
// RecordDuration, Snapshot or Reset (three significant figures, full int64
// range). Prefer NewRecorder so the bounds are explicit.
type Recorder struct {
	mu sync.Mutex
	h  *Histogram
}

// NewRecorder creates a concurrency-safe recorder backed by a histogram with
// the given parameters.
func NewRecorder(sigfigs int, maxValue int64) *Recorder {
	return &Recorder{h: New(sigfigs, maxValue)}
}

func (r *Recorder) ensureLocked() {
	if r.h == nil {
		r.h = New(defaultSigFigs, maxInt64)
	}
}

// Record records v. It is safe to call from multiple goroutines.
func (r *Recorder) Record(v int64) {
	r.mu.Lock()
	r.ensureLocked()
	r.h.Record(v)
	r.mu.Unlock()
}

// RecordDuration records d in microseconds.
func (r *Recorder) RecordDuration(d time.Duration) {
	r.mu.Lock()
	r.ensureLocked()
	r.h.RecordDuration(d)
	r.mu.Unlock()
}

// RecordDurationWithExpected records d with coordinated-omission correction
// based on the expected interval, both in microseconds.
func (r *Recorder) RecordDurationWithExpected(d, expected time.Duration) {
	r.mu.Lock()
	r.ensureLocked()
	r.h.RecordDurationWithExpected(d, expected)
	r.mu.Unlock()
}

// Histogram returns a pointer to the internal Histogram. It does NOT take the
// Recorder lock, so concurrent Record calls may race with the caller's use of
// the returned pointer. Callers must synchronize themselves: either call this
// while all recording has stopped, or hold Lock/Unlock around every use of the
// returned histogram. The returned pointer is nil for a zero-value Recorder
// that has never recorded anything.
func (r *Recorder) Histogram() *Histogram {
	return r.h
}

// Lock acquires the Recorder's internal mutex. It is only needed when using
// the pointer returned by Histogram while other goroutines keep recording.
func (r *Recorder) Lock() { r.mu.Lock() }

// Unlock releases the Recorder's internal mutex.
func (r *Recorder) Unlock() { r.mu.Unlock() }

// Snapshot returns a consistent quantile snapshot of the recorded values.
func (r *Recorder) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLocked()
	return r.h.Snapshot()
}

// Reset clears all recorded observations.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.h != nil {
		r.h.Reset()
	}
}
