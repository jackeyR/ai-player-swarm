package sched

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConstantArrivals(t *testing.T) {
	s := Spec{Shape: ShapeConstant, TotalBots: 100, Rate: 10, Seed: 1}
	s.Defaults()
	arr, err := s.Arrivals()
	if err != nil {
		t.Fatalf("生成到达表: %v", err)
	}
	if len(arr) != 100 {
		t.Fatalf("到达数应为 100，实际 %d", len(arr))
	}
	// 10/s 意味着第 i 个在 i/10 秒。
	for i, a := range arr {
		want := time.Duration(float64(i) / 10 * float64(time.Second))
		if diff := a.At - want; diff > 30*time.Millisecond || diff < -30*time.Millisecond {
			t.Fatalf("第 %d 个到达时刻 %v，期望 %v（偏差 %v）", i, a.At, want, diff)
		}
		if a.Index != i {
			t.Fatalf("Index 应为 %d，实际 %d", i, a.Index)
		}
	}
}

// TestArrivalsMonotonic 是所有形状的共同约束：时间表必须单调不减，否则调度器会乱序。
func TestArrivalsMonotonic(t *testing.T) {
	shapes := []Spec{
		{Shape: ShapeConstant, TotalBots: 60, Rate: 6, Seed: 1},
		{Shape: ShapeRamp, TotalBots: 60, Rate: 10, RampSec: 5, Seed: 2},
		{Shape: ShapeStep, TotalBots: 80, Rate: 4, DurationSec: 30, Steps: []Step{{AtSec: 0, Rate: 4}, {AtSec: 3, Rate: 20}}, Seed: 3},
		{Shape: ShapeSpike, TotalBots: 80, Rate: 4, Spikes: []Spike{{AtSec: 2, DurationSec: 1, ExtraRate: 30}}, Seed: 4},
		{Shape: ShapeDiurnal, TotalBots: 60, Rate: 6, DiurnalAmp: 0.5, DiurnalPeriodSec: 60, Seed: 5},
	}
	for _, s := range shapes {
		s.Defaults()
		arr, err := s.Arrivals()
		if err != nil {
			t.Fatalf("%s: %v", s.Shape, err)
		}
		if len(arr) != s.TotalBots {
			t.Fatalf("%s: 到达数 %d != %d", s.Shape, len(arr), s.TotalBots)
		}
		for i := 1; i < len(arr); i++ {
			if arr[i].At < arr[i-1].At {
				t.Fatalf("%s: 第 %d 个到达时刻早于前一个", s.Shape, i)
			}
		}
	}
}

// TestRampShape 验证爬坡：前 RampSec 秒内的到达率应明显低于稳态。
func TestRampShape(t *testing.T) {
	s := Spec{Shape: ShapeRamp, TotalBots: 200, Rate: 20, RampSec: 5, Seed: 7}
	s.Defaults()
	arr, err := s.Arrivals()
	if err != nil {
		t.Fatalf(": %v", err)
	}
	early, late := 0, 0
	for _, a := range arr {
		if a.At < 2*time.Second {
			early++
		} else if a.At > 6*time.Second && a.At < 8*time.Second {
			late++
		}
	}
	// 2s 处瞬时速率约为稳态的 2/5；两秒窗口内早起到达应少于后段两秒窗口。
	if early == 0 {
		t.Fatal("爬坡前期没有任何到达")
	}
	if late <= early {
		t.Fatalf("爬坡形状不符合预期：前 2s %d 个，稳态后 2s %d 个", early, late)
	}
}

func TestSpikeShape(t *testing.T) {
	s := Spec{Shape: ShapeSpike, TotalBots: 300, Rate: 2, Spikes: []Spike{{AtSec: 5, DurationSec: 2, ExtraRate: 60}}, Seed: 8}
	s.Defaults()
	if got := s.RateAt(5.5); math.Abs(got-62) > 0.01 {
		t.Fatalf("尖峰期间速率应为 62，实际 %v", got)
	}
	if got := s.RateAt(10); math.Abs(got-2) > 0.01 {
		t.Fatalf("尖峰后速率应回到 2，实际 %v", got)
	}
	arr, err := s.Arrivals()
	if err != nil {
		t.Fatalf(": %v", err)
	}
	inSpike := 0
	for _, a := range arr {
		if a.At >= 5*time.Second && a.At < 7*time.Second {
			inSpike++
		}
	}
	if inSpike < 50 {
		t.Fatalf("尖峰窗口内应密集到达，实际只有 %d 个", inSpike)
	}
}

func TestArrivalsUnreachable(t *testing.T) {
	s := Spec{Shape: ShapeConstant, TotalBots: 1000, Rate: 1, DurationSec: 5, Seed: 1}
	s.Defaults()
	if _, err := s.Arrivals(); err == nil {
		t.Fatal("5 秒内排不下 1000 个 bot，应当报错而不是静默少排")
	}
}

func TestValidate(t *testing.T) {
	for _, s := range []Spec{
		{Shape: ShapeConstant, TotalBots: 0, Rate: 1},
		{Shape: ShapeStep, TotalBots: 10, Rate: 1},
		{Shape: ShapeSpike, TotalBots: 10, Rate: 1},
		{Shape: Shape("nonsense"), TotalBots: 10, Rate: 1},
	} {
		s.Defaults()
		if err := s.Validate(); err == nil {
			t.Fatalf("非法 spec 应当报错: %+v", s)
		}
	}
}

// TestDispatcherOpenLoop 是整块代码最重要的性质：
// spawn 里起的任务执行慢**不能**让后面的到达时间跟着推迟（否则就退化成闭环压测）。
func TestDispatcherOpenLoop(t *testing.T) {
	arr := []Arrival{{Index: 0, At: 0}, {Index: 1, At: 20 * time.Millisecond}, {Index: 2, At: 40 * time.Millisecond}}
	var fired atomic.Int64
	var wg sync.WaitGroup
	slowOnce := sync.Once{}

	d := &Dispatcher{}
	start := time.Now()
	d.Run(context.Background(), start, arr, func(ctx context.Context, a Arrival, due time.Time) {
		// 关键：spawn 本身必须立刻返回，慢活放到 goroutine 里。
		wg.Add(1)
		go func() {
			defer wg.Done()
			slowOnce.Do(func() {
				// 第一个被唤起的 bot 卡住 200ms：闭环实现会拖慢后续到达，开环不会。
				time.Sleep(200 * time.Millisecond)
			})
			fired.Add(1)
		}()
	})
	dispatchDone := time.Since(start)

	// 调度本身不能被第一个 bot 阻塞：3 个到达都在 40ms 内，spawn 应当立即跑完。
	if dispatchDone > 150*time.Millisecond {
		t.Fatalf("调度器被 spawn 内的任务拖慢了：%v", dispatchDone)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("任务超时未完成")
	}
	if fired.Load() != int64(len(arr)) {
		t.Fatalf("应触发 %d 次，实际 %d", len(arr), fired.Load())
	}
}

func TestDispatcherRespectsContext(t *testing.T) {
	arr := make([]Arrival, 100)
	for i := range arr {
		arr[i] = Arrival{Index: i, At: time.Duration(i) * time.Second}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	d := &Dispatcher{}
	d.Run(ctx, time.Now(), arr, func(context.Context, Arrival, time.Time) {})
	if time.Since(start) > 2*time.Second {
		t.Fatal("ctx 取消后调度器没有及时退出")
	}
}

func TestExpectedInterval(t *testing.T) {
	s := Spec{Shape: ShapeConstant, TotalBots: 10, Rate: 4}
	s.Defaults()
	if got := s.ExpectedInterval(); got != 250*time.Millisecond {
		t.Fatalf("4/s 的预期间隔应为 250ms，实际 %v", got)
	}
}

func TestJitterKeepsCount(t *testing.T) {
	s := Spec{Shape: ShapeConstant, TotalBots: 200, Rate: 50, Jitter: 0.3, Seed: 11}
	s.Defaults()
	arr, err := s.Arrivals()
	if err != nil {
		t.Fatalf(": %v", err)
	}
	if len(arr) != s.TotalBots {
		t.Fatalf("抖动不应改变到达总数：%d != %d", len(arr), s.TotalBots)
	}
	for i := 1; i < len(arr); i++ {
		if arr[i].At < arr[i-1].At {
			t.Fatal("抖动后必须重新排序")
		}
	}
}
