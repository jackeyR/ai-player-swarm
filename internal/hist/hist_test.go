package hist

import (
	"encoding/json"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestQuantiles(t *testing.T) {
	h := New(3, 1_000_000)
	for i := int64(1); i <= 10000; i++ {
		h.Record(i)
	}

	if got := h.TotalCount(); got != 10000 {
		t.Fatalf("TotalCount = %d, want 10000", got)
	}
	if got := h.Min(); got != 1 {
		t.Fatalf("Min = %d, want 1", got)
	}
	if got := h.Max(); got != 10000 {
		t.Fatalf("Max = %d, want 10000", got)
	}
	if got := h.Mean(); got != 5000.5 {
		t.Fatalf("Mean = %v, want 5000.5", got)
	}

	for _, tc := range []struct {
		q    float64
		want float64
	}{
		{0.50, 5000},
		{0.90, 9000},
		{0.99, 9900},
	} {
		got := float64(h.ValueAtQuantile(tc.q))
		if rel := math.Abs(got-tc.want) / tc.want; rel >= 0.02 {
			t.Errorf("ValueAtQuantile(%v) = %v, relative error %.4f >= 2%% (want ~%v)",
				tc.q, got, rel, tc.want)
		}
	}
}

func TestConstantValues(t *testing.T) {
	h := New(3, 1000)
	for i := 0; i < 1000; i++ {
		h.Record(42)
	}

	if got := h.Min(); got != 42 {
		t.Fatalf("Min = %d, want 42", got)
	}
	if got := h.Max(); got != 42 {
		t.Fatalf("Max = %d, want 42", got)
	}
	if got := h.Mean(); got != 42.0 {
		t.Fatalf("Mean = %v, want 42", got)
	}
	for _, q := range []float64{0.5, 0.9, 0.95, 0.99, 0.999, 1.0} {
		if got := h.ValueAtQuantile(q); got != 42 {
			t.Fatalf("ValueAtQuantile(%v) = %d, want 42", q, got)
		}
	}

	snap := h.Snapshot()
	want := Snapshot{Count: 1000, Min: 42, Max: 42, Mean: 42, P50: 42, P90: 42, P95: 42, P99: 42, P999: 42}
	if snap != want {
		t.Fatalf("Snapshot = %+v, want %+v", snap, want)
	}
}

func TestEmptyHistogram(t *testing.T) {
	h := New(3, 1000)
	if got := h.TotalCount(); got != 0 {
		t.Fatalf("TotalCount = %d, want 0", got)
	}
	if got := h.Min(); got != 0 {
		t.Fatalf("Min = %d, want 0", got)
	}
	if got := h.Max(); got != 0 {
		t.Fatalf("Max = %d, want 0", got)
	}
	if got := h.Mean(); got != 0 {
		t.Fatalf("Mean = %v, want 0", got)
	}
	if got := h.ValueAtQuantile(0.5); got != 0 {
		t.Fatalf("ValueAtQuantile = %d, want 0", got)
	}
	if snap := h.Snapshot(); snap != (Snapshot{}) {
		t.Fatalf("Snapshot = %+v, want zero value", snap)
	}
	called := false
	h.Export(func(int64, int64) bool {
		called = true
		return true
	})
	if called {
		t.Fatalf("Export invoked callback on an empty histogram")
	}
	if b := h.Buckets(10); b != nil {
		t.Fatalf("Buckets = %v, want nil", b)
	}
}

func TestClamping(t *testing.T) {
	h := New(3, 100)
	h.Record(1000)
	h.Record(0)
	h.Record(-123)
	h.Record(100)
	h.Record(101)

	if got := h.TotalCount(); got != 5 {
		t.Fatalf("TotalCount = %d, want 5", got)
	}
	if got := h.Min(); got != 1 {
		t.Fatalf("Min = %d, want 1 (0 and negative clamp to 1)", got)
	}
	if got := h.Max(); got != 100 {
		t.Fatalf("Max = %d, want 100", got)
	}

	perBucket := map[int64]int64{}
	h.Export(func(value, count int64) bool {
		perBucket[value] = count
		return true
	})
	if got := perBucket[1]; got != 2 {
		t.Errorf("count at 1 = %d, want 2", got)
	}
	if got := perBucket[100]; got != 3 {
		t.Errorf("count at 100 = %d, want 3", got)
	}
	var total int64
	for _, c := range perBucket {
		total += c
	}
	if total != 5 {
		t.Fatalf("sum of exported counts = %d, want 5", total)
	}
}

func TestRecordWithExpectedIntervalCount(t *testing.T) {
	h := New(3, 1000)
	h.RecordWithExpectedInterval(1000, 100)

	// 900, 800, ..., 100 (9 synthesized) plus the original 1000 => 10.
	if got := h.TotalCount(); got != 10 {
		t.Fatalf("TotalCount = %d, want 10", got)
	}
	seen := make(map[int64]int64)
	h.Export(func(value, count int64) bool {
		seen[value] = count
		return true
	})
	if len(seen) != 10 {
		t.Fatalf("distinct buckets = %d, want 10 (%v)", len(seen), seen)
	}
	for v, c := range seen {
		if c != 1 {
			t.Errorf("count at %d = %d, want 1", v, c)
		}
	}
	for _, v := range []int64{100, 200, 300, 400, 500, 600, 700, 800, 900, 1000} {
		if seen[v] != 1 {
			t.Errorf("missing synthesized value %d", v)
		}
	}

	// Non-positive expected interval behaves like Record.
	h.RecordWithExpectedInterval(1000, 0)
	if got := h.TotalCount(); got != 11 {
		t.Fatalf("after expected=0: TotalCount = %d, want 11", got)
	}
	h.RecordWithExpectedInterval(1000, -5)
	if got := h.TotalCount(); got != 12 {
		t.Fatalf("after expected=-5: TotalCount = %d, want 12", got)
	}
	// v <= expected interval: only the original value is recorded.
	h.RecordWithExpectedInterval(50, 100)
	if got := h.TotalCount(); got != 13 {
		t.Fatalf("after v<expected: TotalCount = %d, want 13", got)
	}
}

func TestCoordinatedOmissionCorrection(t *testing.T) {
	uncorrected := New(3, 2_000_000)
	corrected := New(3, 2_000_000)

	for i := 0; i < 1000; i++ {
		uncorrected.RecordDuration(10 * time.Millisecond)
		corrected.RecordDurationWithExpected(10*time.Millisecond, 10*time.Millisecond)
	}
	uncorrected.RecordDuration(time.Second)
	corrected.RecordDurationWithExpected(time.Second, 10*time.Millisecond)

	// 1000 fast samples + 99 synthesized values (10000us..990000us) + the
	// original 1s sample.
	if got := corrected.TotalCount(); got != 1100 {
		t.Fatalf("corrected TotalCount = %d, want 1100", got)
	}

	// Without correction the single 1s outlier is only the 1001st smallest
	// sample, so p99 (the 991st) is still the normal 10ms latency.
	up99 := uncorrected.ValueAtQuantile(0.99)
	if up99 != 10_000 {
		t.Fatalf("uncorrected p99 = %dus, want 10000us", up99)
	}

	// With correction 99 extra slow observations are inserted. p99 is the
	// 1089th of 1100 samples, which lands on the synthesized 890ms value.
	cp99 := corrected.ValueAtQuantile(0.99)
	if cp99 != 890_000 {
		t.Fatalf("corrected p99 = %dus, want 890000us", cp99)
	}
	if cp99 < 100_000 {
		t.Fatalf("corrected p99 = %dus, want >= 100000us", cp99)
	}
	if cp99 <= up99 {
		t.Fatalf("corrected p99 (%d) should be much larger than uncorrected p99 (%d)", cp99, up99)
	}
	t.Logf("p99: uncorrected=%dus corrected=%dus", up99, cp99)
}

func TestMerge(t *testing.T) {
	a := New(3, 1_000_000)
	b := New(3, 1_000_000)
	for i := int64(1); i <= 3; i++ {
		a.Record(i)
	}
	for i := int64(4); i <= 5; i++ {
		b.Record(i)
	}

	if err := a.Merge(b); err != nil {
		t.Fatalf("Merge compatible histograms: %v", err)
	}
	if got := a.TotalCount(); got != 5 {
		t.Fatalf("merged TotalCount = %d, want 5", got)
	}
	if got := a.Min(); got != 1 {
		t.Fatalf("merged Min = %d, want 1", got)
	}
	if got := a.Max(); got != 5 {
		t.Fatalf("merged Max = %d, want 5", got)
	}
	if got := a.Mean(); got != 3 {
		t.Fatalf("merged Mean = %v, want 3", got)
	}
	if got := a.ValueAtQuantile(0.5); got != 3 {
		t.Fatalf("merged p50 = %d, want 3", got)
	}

	differentMax := New(3, 2_000_000)
	differentSigFigs := New(4, 1_000_000)
	if err := a.Merge(differentMax); err == nil {
		t.Fatalf("Merge with different maxValue returned nil error")
	}
	if err := a.Merge(differentSigFigs); err == nil {
		t.Fatalf("Merge with different sigfigs returned nil error")
	}
	if err := a.Merge(nil); err == nil {
		t.Fatalf("Merge(nil) returned nil error")
	}
	if got := a.TotalCount(); got != 5 {
		t.Fatalf("failed merges changed TotalCount to %d, want 5", got)
	}
}

func TestBucketsCumulative(t *testing.T) {
	h := New(3, 10000)
	for i := int64(1); i <= 10000; i++ {
		h.Record(i)
	}

	bs := h.Buckets(10)
	if len(bs) == 0 || len(bs) > 10 {
		t.Fatalf("Buckets(10) returned %d buckets, want 1..10", len(bs))
	}
	var prevValue, prevCount int64
	for i, b := range bs {
		if i > 0 && b.Value <= prevValue {
			t.Fatalf("bucket values not strictly increasing: %v then %v", prevValue, b.Value)
		}
		if b.Count < prevCount {
			t.Fatalf("bucket counts not cumulative: %d then %d", prevCount, b.Count)
		}
		prevValue, prevCount = b.Value, b.Count
	}
	if got := bs[len(bs)-1].Count; got != h.TotalCount() {
		t.Fatalf("last bucket count = %d, want TotalCount %d", got, h.TotalCount())
	}
	if got := bs[len(bs)-1].Value; got != h.Max() {
		t.Fatalf("last bucket value = %d, want Max %d", got, h.Max())
	}

	one := h.Buckets(1)
	if len(one) != 1 {
		t.Fatalf("Buckets(1) returned %d buckets, want 1", len(one))
	}
	if one[0].Count != h.TotalCount() {
		t.Fatalf("Buckets(1) count = %d, want %d", one[0].Count, h.TotalCount())
	}
	if all := h.Buckets(100000); len(all) < len(bs) {
		t.Fatalf("Buckets(large) returned %d buckets, want >= %d", len(all), len(bs))
	}
}

func TestRecorderConcurrent(t *testing.T) {
	r := NewRecorder(3, 1_000_000)
	const goroutines = 8
	const perGoroutine = 500

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				r.Record(int64(i + 1))
				r.RecordDuration(time.Duration(i+1) * time.Microsecond)
				_ = r.Snapshot()
				if i%50 == 0 {
					r.Lock()
					_ = r.Histogram().Max()
					r.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * perGoroutine * 2)
	if got := r.Histogram().TotalCount(); got != want {
		t.Fatalf("TotalCount = %d, want %d", got, want)
	}
	if snap := r.Snapshot(); snap.Count != want {
		t.Fatalf("Snapshot.Count = %d, want %d", snap.Count, want)
	}

	r.Reset()
	if got := r.Snapshot().Count; got != 0 {
		t.Fatalf("after Reset Snapshot.Count = %d, want 0", got)
	}
}

func TestRecorderZeroValue(t *testing.T) {
	var r Recorder
	r.Record(7)
	r.RecordDuration(3 * time.Microsecond)
	if got := r.Snapshot().Count; got != 2 {
		t.Fatalf("zero-value Recorder count = %d, want 2", got)
	}
	if got := r.Histogram(); got == nil {
		t.Fatalf("Histogram() = nil after recording")
	}
}

func TestZeroValueHistogram(t *testing.T) {
	var h Histogram
	h.Record(5)
	if got := h.TotalCount(); got != 1 {
		t.Fatalf("TotalCount = %d, want 1", got)
	}
	if got := h.Max(); got != 5 {
		t.Fatalf("Max = %d, want 5", got)
	}
	h.Reset()
	if got := h.TotalCount(); got != 0 {
		t.Fatalf("TotalCount after Reset = %d, want 0", got)
	}
}

func TestSnapshotJSON(t *testing.T) {
	h := New(3, 1_000_000)
	for i := int64(1); i <= 10000; i++ {
		h.Record(i)
	}
	snap := h.Snapshot()

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var got Snapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(snap, got) {
		t.Fatalf("round trip mismatch: %+v != %+v", snap, got)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal into map: %v", err)
	}
	for _, key := range []string{"count", "min", "max", "mean", "p50", "p90", "p95", "p99", "p999"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("JSON snapshot is missing key %q", key)
		}
	}
}
