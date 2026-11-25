package clock

import (
	"testing"
	"time"
)

// TestStepperNoDrift 验证固定步长调度不会累积漂移（B9 / D2）。
// 如果实现是 time.Sleep(step) 串行堆叠，10Hz 的 tick 会慢慢漂成 9.xHz。
func TestStepperNoDrift(t *testing.T) {
	const step = 10 * time.Millisecond
	const ticks = 30
	s := NewStepper(step, 4)
	defer s.Stop()

	start := time.Now()
	n := 0
	var last Tick
	for n < ticks {
		select {
		case tk := <-s.C():
			last = tk
			n++
		case <-time.After(3 * time.Second):
			t.Fatalf("只收到 %d/%d 个 tick", n, ticks)
		}
	}
	elapsed := time.Since(start)

	// 30 个 tick 理论耗时 300ms。允许调度抖动，但不允许「越跑越慢」。
	if elapsed > ticks*step+400*time.Millisecond {
		t.Fatalf("调度严重滞后：%d 个 tick 用了 %v（理论 %v）", ticks, elapsed, ticks*step)
	}
	if last.Seq < uint64(ticks-1) {
		t.Fatalf("序号不连续：最后一个 Seq=%d，期望 >= %d", last.Seq, ticks-1)
	}
	// 绝对时间表：最后一个 tick 的理论时刻应接近 start + ticks*step。
	drift := last.Scheduled.Sub(start.Add(ticks * step))
	if drift > 60*time.Millisecond || drift < -60*time.Millisecond {
		t.Fatalf("累计漂移 %v 过大（说明按相对时间堆叠了）", drift)
	}
}

func TestStepperLagNonNegative(t *testing.T) {
	s := NewStepper(5*time.Millisecond, 8)
	defer s.Stop()
	for i := 0; i < 5; i++ {
		select {
		case tk := <-s.C():
			if tk.Lag < 0 {
				t.Fatalf("lag 不应为负: %v", tk.Lag)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("等 tick 超时")
		}
	}
}

// TestStepperSlowConsumerDoesNotBlock 验证下游消费慢时调度器不被拖垮。
func TestStepperSlowConsumerDoesNotBlock(t *testing.T) {
	s := NewStepper(time.Millisecond, 1)
	// 故意不读，睡一段时间后确认 Stop 仍然能及时返回。
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 被缓冲满的消费者卡住")
	}
}

func TestStepperStopIdempotent(t *testing.T) {
	s := NewStepper(time.Millisecond, 1)
	s.Stop()
	s.Stop() // 不应 panic
}

func TestBudget(t *testing.T) {
	b := NewBudget(16 * time.Millisecond)
	if b.Observe(5 * time.Millisecond) {
		t.Fatal("5ms 不应超预算")
	}
	if !b.Observe(20 * time.Millisecond) {
		t.Fatal("20ms 应超预算")
	}
	over, total := b.Overrun()
	if over != 1 || total != 2 {
		t.Fatalf("超预算统计错误: over=%d total=%d", over, total)
	}
}

func TestBudgetDefault(t *testing.T) {
	b := NewBudget(0) // 应回落到 16ms
	over, total := b.Overrun()
	if over != 0 || total != 0 {
		t.Fatal("新建 budget 计数应为 0")
	}
	if b.Observe(15 * time.Millisecond) {
		t.Fatal("默认预算应为 16ms，15ms 不应超")
	}
}

func TestSince(t *testing.T) {
	start := time.Now()
	time.Sleep(10 * time.Millisecond)
	if d := Since(start); d < 10*time.Millisecond {
		t.Fatalf("Since 结果偏小: %v", d)
	}
}
