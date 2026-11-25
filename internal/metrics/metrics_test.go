package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func testRenderText(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return b.String()
}

func TestCounterGaugeTextSnapshot(t *testing.T) {
	r := New()
	c := r.CounterVec("http_requests_total", "Total HTTP requests.", "method", "code")
	c.With("GET", "200").Add(3)
	c.With("POST", "500").Inc()

	g := r.GaugeVec("http_in_flight", "In-flight HTTP requests.")
	g.With().Set(7)

	want := `# HELP http_requests_total Total HTTP requests.
# TYPE http_requests_total counter
http_requests_total{method="GET",code="200"} 3
http_requests_total{method="POST",code="500"} 1
# HELP http_in_flight In-flight HTTP requests.
# TYPE http_in_flight gauge
http_in_flight 7
`
	if got := testRenderText(t, r); got != want {
		t.Fatalf("unexpected text output\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestHistogramTextSnapshot(t *testing.T) {
	r := New()
	h := r.HistogramVec("rpc_duration_seconds", "RPC duration.", []float64{5, 1, 2}, "method")
	o := h.With("GET")
	o.Observe(1)
	o.Observe(3)
	o.Observe(7)

	want := `# HELP rpc_duration_seconds RPC duration.
# TYPE rpc_duration_seconds histogram
rpc_duration_seconds_bucket{method="GET",le="1"} 1
rpc_duration_seconds_bucket{method="GET",le="2"} 1
rpc_duration_seconds_bucket{method="GET",le="5"} 2
rpc_duration_seconds_bucket{method="GET",le="+Inf"} 3
rpc_duration_seconds_sum{method="GET"} 11
rpc_duration_seconds_count{method="GET"} 3
`
	if got := testRenderText(t, r); got != want {
		t.Fatalf("unexpected text output\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestHistogramNoLabels(t *testing.T) {
	r := New()
	h := r.HistogramVec("latency_seconds", "Latency.", []float64{0.5})
	h.With().Observe(0.25)

	want := `# HELP latency_seconds Latency.
# TYPE latency_seconds histogram
latency_seconds_bucket{le="0.5"} 1
latency_seconds_bucket{le="+Inf"} 1
latency_seconds_sum 0.25
latency_seconds_count 1
`
	if got := testRenderText(t, r); got != want {
		t.Fatalf("unexpected text output\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestCounterNoLabelsNoBraces(t *testing.T) {
	r := New()
	r.CounterVec("build_info", "Build information.").With().Inc()

	text := testRenderText(t, r)
	want := `# HELP build_info Build information.
# TYPE build_info counter
build_info 1
`
	if text != want {
		t.Fatalf("unexpected text output\n--- got ---\n%s\n--- want ---\n%s", text, want)
	}
	if strings.Contains(text, "build_info{") {
		t.Fatalf("unlabeled metric must not have braces: %s", text)
	}
}

func TestLabelAndHelpEscaping(t *testing.T) {
	r := New()
	c := r.CounterVec("escaped_total", "help with \\ and\nnewline", "path")
	c.With(`a"b\c` + "\n" + "d").Inc()

	want := `# HELP escaped_total help with \\ and\nnewline
# TYPE escaped_total counter
escaped_total{path="a\"b\\c\nd"} 1
`
	if got := testRenderText(t, r); got != want {
		t.Fatalf("unexpected escaped output\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestSetFunc(t *testing.T) {
	r := New()
	var calls atomic.Int32
	g := r.GaugeVec("estimated_leaks", "Estimated leaked rooms.")
	g.With().SetFunc(func() float64 {
		return float64(calls.Add(1)) * 10
	})

	if got := g.With().Value(); got != 10 {
		t.Fatalf("first Value = %v, want 10", got)
	}
	if got := g.With().Value(); got != 20 {
		t.Fatalf("second Value = %v, want 20", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("SetFunc called %d times, want 2", calls.Load())
	}

	// A scrape evaluates the function again.
	if text := testRenderText(t, r); !strings.Contains(text, "\nestimated_leaks 30\n") {
		t.Fatalf("scrape did not re-evaluate SetFunc:\n%s", text)
	}

	g.With().SetFunc(nil)
	g.With().Set(5)
	if got := g.With().Value(); got != 5 {
		t.Fatalf("Value after clearing func = %v, want 5", got)
	}
}

func TestHistogramBucketSemantics(t *testing.T) {
	r := New()
	o := r.HistogramVec("obs", "Observations.", []float64{1, 2, 3}).With()
	for _, v := range []float64{1, 2, 3, 4} {
		o.Observe(v)
	}

	text := testRenderText(t, r)
	buckets := testParseBuckets(t, text, "obs")
	want := map[string]uint64{"1": 1, "2": 2, "3": 3, "+Inf": 4}
	if len(buckets) != len(want) {
		t.Fatalf("got buckets %v, want %v", buckets, want)
	}
	for le, wantCount := range want {
		if got := buckets[le]; got != wantCount {
			t.Errorf("bucket le=%q = %d, want %d", le, got, wantCount)
		}
	}
	// +Inf must equal _count.
	if got := buckets["+Inf"]; got != testParseUintSample(t, text, "obs_count") {
		t.Errorf("+Inf bucket %d != _count %d", got, testParseUintSample(t, text, "obs_count"))
	}
	// Each observation increments exactly one finite bucket, so the
	// differences between successive cumulative buckets are all 1.
	var prev uint64
	for _, le := range []string{"1", "2", "3", "+Inf"} {
		if delta := buckets[le] - prev; delta != 1 {
			t.Errorf("bucket le=%q added %d observations, want 1", le, delta)
		}
		prev = buckets[le]
	}
	if got := testParseFloatSample(t, text, "obs_sum"); got != 10 {
		t.Errorf("_sum = %v, want 10", got)
	}
}

func testParseBuckets(t *testing.T, text, name string) map[string]uint64 {
	t.Helper()
	out := make(map[string]uint64)
	prefix := name + "_bucket{"
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		leIdx := strings.Index(line, `le="`)
		if leIdx < 0 {
			t.Fatalf("bucket line without le label: %s", line)
		}
		rest := line[leIdx+len(`le="`):]
		end := strings.Index(rest, `"`)
		if end < 0 {
			t.Fatalf("bucket line with unterminated le label: %s", line)
		}
		le := rest[:end]
		fields := strings.Fields(line)
		v, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
		if err != nil {
			t.Fatalf("bucket line %q: %v", line, err)
		}
		out[le] = v
	}
	return out
}

func testParseUintSample(t *testing.T, text, name string) uint64 {
	t.Helper()
	value, ok := testFindSample(text, name)
	if !ok {
		t.Fatalf("sample %q not found", name)
	}
	v, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		t.Fatalf("sample %q value %q: %v", name, value, err)
	}
	return v
}

func testParseFloatSample(t *testing.T, text, name string) float64 {
	t.Helper()
	value, ok := testFindSample(text, name)
	if !ok {
		t.Fatalf("sample %q not found", name)
	}
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		t.Fatalf("sample %q value %q: %v", name, value, err)
	}
	return v
}

func testFindSample(text, name string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(line, name+" ")
		if !ok {
			continue
		}
		return rest, true
	}
	return "", false
}

func TestRemoveMetricSeries(t *testing.T) {
	r := New()
	c := r.CounterVec("runs_total", "Runs.", "run_id")
	c.With("a").Inc()
	c.With("b").Add(2)

	r.RemoveMetricSeries("runs_total", `run_id="a"`)
	text := testRenderText(t, r)
	if strings.Contains(text, `run_id="a"`) {
		t.Fatalf("series a still present after removal:\n%s", text)
	}
	if !strings.Contains(text, `runs_total{run_id="b"} 2`) {
		t.Fatalf("series b missing after removing a:\n%s", text)
	}

	// Braces around the signature are accepted too.
	r.RemoveMetricSeries("runs_total", `{run_id="b"}`)
	text = testRenderText(t, r)
	if strings.Contains(text, "run_id=") {
		t.Fatalf("series b still present after removal:\n%s", text)
	}
	// The family header stays even when no series remain.
	if !strings.Contains(text, "# TYPE runs_total counter") {
		t.Fatalf("family header missing after removing all series:\n%s", text)
	}

	// Unknown families and signatures are ignored.
	r.RemoveMetricSeries("does_not_exist", `x="1"`)
	r.RemoveMetricSeries("runs_total", `run_id="zzz"`)

	// An empty signature addresses the only series of an unlabeled family.
	g := r.GaugeVec("singleton", "One value.")
	g.With().Set(4)
	r.RemoveMetricSeries("singleton", "")
	if text := testRenderText(t, r); strings.Contains(text, "\nsingleton 4\n") {
		t.Fatalf("unlabeled series still present after removal:\n%s", text)
	}
}

func TestConcurrentUse(t *testing.T) {
	r := New()
	c := r.CounterVec("ops_total", "Operations.", "worker")
	g := r.GaugeVec("active_workers", "Active workers.")
	h := r.HistogramVec("op_duration_seconds", "Operation duration.", []float64{0.001, 0.01, 0.1, 1}, "worker")
	tmp := r.CounterVec("temp_total", "Temporary series.", "id")

	const workers = 8
	const iterations = 500
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			label := strconv.Itoa(w % 4)
			for i := 0; i < iterations; i++ {
				c.With(label).Inc()
				h.With(label).Observe(float64(i%100) / 1000)
				g.With().Set(float64(w))
				if i%100 == 0 {
					var b strings.Builder
					if err := r.WriteText(&b); err != nil {
						t.Errorf("WriteText: %v", err)
						return
					}
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			tmp.With("x").Inc()
			r.RemoveMetricSeries("temp_total", `id="x"`)
		}
	}()
	wg.Wait()

	var total float64
	for i := 0; i < 4; i++ {
		total += c.With(strconv.Itoa(i)).Value()
	}
	if want := float64(workers * iterations); total != want {
		t.Fatalf("counter total = %v, want %v", total, want)
	}

	text := testRenderText(t, r)
	if !strings.Contains(text, `ops_total{worker="0"} 1000`) {
		t.Fatalf("expected 1000 operations for worker 0:\n%s", text)
	}
	active, ok := testFindSample(text, "active_workers")
	if !ok {
		t.Fatalf("expected active_workers gauge in output:\n%s", text)
	}
	activeValue, err := strconv.ParseFloat(active, 64)
	if err != nil {
		t.Fatalf("active_workers value %q: %v", active, err)
	}
	if activeValue < 0 || activeValue >= workers {
		t.Fatalf("active_workers = %v, want one of the worker indexes", activeValue)
	}

	// Histogram series must remain internally consistent: two workers per
	// label, 500 observations each, one _count per series.
	for w := 0; w < 4; w++ {
		label := strconv.Itoa(w)
		if !strings.Contains(text, `op_duration_seconds_bucket{worker="`+label+`",le="+Inf"} 1000`) {
			t.Errorf("histogram +Inf bucket for worker %s is not 1000", label)
		}
		if !strings.Contains(text, `op_duration_seconds_count{worker="`+label+`"} 1000`) {
			t.Errorf("histogram _count for worker %s is not 1000", label)
		}
	}
}

func TestRuntimeMetrics(t *testing.T) {
	r := New()
	r.EnableRuntimeMetrics()
	// Calling it again must not duplicate families or panic.
	r.EnableRuntimeMetrics()

	text := testRenderText(t, r)
	for _, name := range []string{
		"go_goroutines", "go_threads", "go_gc_count",
		"go_memstats_alloc_bytes", "go_memstats_heap_objects",
	} {
		if !strings.Contains(text, "# TYPE "+name+" gauge") {
			t.Errorf("runtime metric %q is missing its TYPE line", name)
		}
	}
	if strings.Count(text, "# TYPE go_goroutines gauge") != 1 {
		t.Fatalf("go_goroutines family registered more than once")
	}
	if strings.Contains(text, "go_goroutines{") {
		t.Fatalf("go_goroutines should not have labels:\n%s", text)
	}

	v, ok := testFindSample(text, "go_goroutines")
	if !ok {
		t.Fatalf("go_goroutines sample not found")
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		t.Fatalf("go_goroutines value %q: %v", v, err)
	}
	if f <= 0 {
		t.Fatalf("go_goroutines = %v, want > 0", f)
	}

	if _, ok := testFindSample(text, "go_memstats_alloc_bytes"); !ok {
		t.Fatalf("go_memstats_alloc_bytes sample not found")
	}
}

func TestHandler(t *testing.T) {
	r := New()
	r.CounterVec("requests_total", "Requests.").With().Inc()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "version=0.0.4") {
		t.Fatalf("Content-Type = %q, want version 0.0.4", ct)
	}
	if body, want := rec.Body.String(), testRenderText(t, r); body != want {
		t.Fatalf("handler body mismatch\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
}

type testErrWriter struct{}

func (testErrWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestWriteTextPropagatesError(t *testing.T) {
	r := New()
	r.CounterVec("x_total", "X.").With().Inc()
	if err := r.WriteText(testErrWriter{}); err == nil {
		t.Fatalf("WriteText with failing writer returned nil error")
	}
}

func TestReRegistration(t *testing.T) {
	r := New()
	c1 := r.CounterVec("dup_total", "First help.")
	c2 := r.CounterVec("dup_total", "Second help is ignored.")
	if c1 != c2 {
		t.Fatalf("re-registering the same counter returned a different family")
	}
	c1.With().Inc()
	if got := c2.With().Value(); got != 1 {
		t.Fatalf("re-registered family does not share series, value = %v", got)
	}

	if got := r.CounterVec("dup_total", "First help."); got != c1 {
		t.Fatalf("re-registration with identical parameters returned a different family")
	}

	testAssertPanics(t, func() { r.CounterVec("dup_total", "First help.", "extra") })
	testAssertPanics(t, func() { r.GaugeVec("dup_total", "First help.") })

	h1 := r.HistogramVec("dup_hist", "Hist.", []float64{1, 2})
	h2 := r.HistogramVec("dup_hist", "Hist.", []float64{2, 1, 2, 1})
	if h1 != h2 {
		t.Fatalf("histogram re-registration with equivalent buckets returned a different family")
	}
	testAssertPanics(t, func() { r.HistogramVec("dup_hist", "Hist.", []float64{1, 3}) })
}

func testAssertPanics(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("expected panic, got none")
		}
	}()
	f()
}
