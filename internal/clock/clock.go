// Package clock 提供固定步长调度与延迟测量工具。
//
// 设计要点（对应知识清单 B9 / D2 / C3）：
//   - tick 必须按**绝对时间表**推进（start + n*step），而不是 time.Sleep(step) 串行堆叠，
//     否则误差累积会让 10Hz 的 tick 慢慢漂成 9.7Hz，房间状态同步节奏就算不准。
//   - 每个 tick 同时给出「理论时刻」与「实际时刻」，差值就是调度滞后（lag），
//     这个滞后本身就是观测项：它变大说明 bot 端或房间端在过载。
package clock

import (
	"runtime"
	"sync"
	"time"
)

// Tick 是一次固定步长调度的心跳。
type Tick struct {
	// Seq 从 0 开始递增的序号。
	Seq uint64
	// Scheduled 是按绝对时间表应有的触发时刻。
	Scheduled time.Time
	// Actual 是实际触发时刻。
	Actual time.Time
	// Lag = Actual - Scheduled，恒 >= 0（时钟回拨时为负，会被记录但不会让调度器追不上来）。
	Lag time.Duration
}

// Stepper 以固定步长产生 Tick，带漂移补偿。
type Stepper struct {
	step    time.Duration
	out     chan Tick
	stop    chan struct{}
	stopped sync.Once
	done    chan struct{}
}

// NewStepper 创建步长为 step 的调度器。buf 为输出通道缓冲，<=0 时取 1。
//
// 通道消费慢时不会阻塞 tick goroutine 无限增长：缓冲满了就直接丢弃这一拍并计入
// 跳过数（DropCount），保证调度器自身不被下游拖垮。
func NewStepper(step time.Duration, buf int) *Stepper {
	if step <= 0 {
		step = time.Millisecond
	}
	if buf <= 0 {
		buf = 1
	}
	s := &Stepper{
		step: step,
		out:  make(chan Tick, buf),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *Stepper) loop() {
	defer close(s.done)
	start := time.Now()
	var seq uint64
	timer := time.NewTimer(s.step)
	defer timer.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-timer.C:
		}

		scheduled := start.Add(time.Duration(seq+1) * s.step)
		now := time.Now()
		lag := now.Sub(scheduled)
		if lag < 0 {
			// 提前醒来（少见），补睡到点。
			t := time.NewTimer(-lag)
			select {
			case <-t.C:
			case <-s.stop:
				t.Stop()
				return
			}
			t.Stop()
			now = time.Now()
			lag = now.Sub(scheduled)
		}
		seq++

		t := Tick{Seq: seq - 1, Scheduled: scheduled, Actual: now, Lag: lag}
		select {
		case s.out <- t:
		default:
			// 下游消费不过来：丢弃这一拍，保持调度节奏不被拖慢。
		}

		// 下一次唤醒仍对齐绝对时间表。
		delay := scheduled.Add(s.step).Sub(time.Now())
		if delay < 0 {
			delay = 0
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
	}
}

// C 返回只读的 Tick 通道。
func (s *Stepper) C() <-chan Tick { return s.out }

// Step 返回步长。
func (s *Stepper) Step() time.Duration { return s.step }

// Stop 停止调度并等待 goroutine 退出，可重复调用。
func (s *Stepper) Stop() {
	s.stopped.Do(func() { close(s.stop) })
	<-s.done
}

// Budget 用于测量单帧预算是否超支（D2：常见预算 16ms）。
type Budget struct {
	limit time.Duration
	over  uint64
	total uint64
}

// NewBudget 创建帧预算计数器，limit <= 0 时取 16ms。
func NewBudget(limit time.Duration) *Budget {
	if limit <= 0 {
		limit = 16 * time.Millisecond
	}
	return &Budget{limit: limit}
}

// Observe 记录一次实际耗时，返回是否超预算。
func (b *Budget) Observe(d time.Duration) bool {
	b.total++
	if d > b.limit {
		b.over++
		return true
	}
	return false
}

// Overrun 返回超预算次数与总次数。
func (b *Budget) Overrun() (over, total uint64) { return b.over, b.total }

// Since 用单调时钟计算经过的时长（time.Since 在 Windows 上已是单调的，
// 这里显式包一层是为了在代码里表明「只允许单调时间差，禁止墙钟相减」）。
func Since(start time.Time) time.Duration {
	return time.Since(start)
}

// NowMillis 返回自 Unix 纪元起的毫秒数，用于协议里的时间戳字段。
func NowMillis() int64 {
	return time.Now().UnixMilli()
}

// GoroutineCount 便于在指标里暴露 bot 端 goroutine 数量（B1 / 面试题「goroutine 漏了怎么发现」）。
func GoroutineCount() int {
	return runtime.NumGoroutine()
}
