// Package sched 实现开环（open-loop）到达调度。
//
// 这是整个压测平台的地基，对应知识清单 C2 / C3 / E5：
//
//	闭环：做完一个再做下一个 —— 服务端慢时 bot 自动减速，p99 被系统性抹平（coordinated omission）
//	开环：按**到达时间表**发起，绝不等待上一次完成 —— 迟到就是迟到，会被如实记录
//
// 具体做法是先生成一张绝对时间偏移表（Arrival.At），再由 Dispatcher 按表触发；
// 每个 bot 携带自己的「应到时刻」，所有延迟都以应到时刻为起点计算（见 internal/bot）。
package sched

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"time"
)

// Shape 是流量形状（§2.7：真实事故大多由瞬时形状触发，而非平均负载高）。
type Shape string

// 支持的形状。
const (
	// ShapeConstant 恒定到达率。
	ShapeConstant Shape = "constant"
	// ShapeRamp 线性爬坡到稳态速率（验证扩容 / 慢启动，A3）。
	ShapeRamp Shape = "ramp"
	// ShapeStep 分段阶跃（阶梯 → 平台 → 断崖）。
	ShapeStep Shape = "step"
	// ShapeSpike 基线 + 瞬时尖峰（开服、活动开启、整点惊群）。
	ShapeSpike Shape = "spike"
	// ShapeDiurnal 正弦日活曲线（ramp up / down 的平滑版）。
	ShapeDiurnal Shape = "diurnal"
)

// Step 是 ShapeStep 的一段：从 AtSec 起到达率变为 Rate（bot/s）。
type Step struct {
	AtSec float64 `yaml:"at_s" json:"at_s"`
	Rate  float64 `yaml:"rate" json:"rate"`
}

// Spike 是一次瞬时尖峰：在 [AtSec, AtSec+DurationSec) 内额外叠加 ExtraRate。
type Spike struct {
	AtSec       float64 `yaml:"at_s" json:"at_s"`
	DurationSec float64 `yaml:"duration_s" json:"duration_s"`
	ExtraRate   float64 `yaml:"extra_rate" json:"extra_rate"`
}

// Spec 是一份流量模型（来自负载计划 YAML 的 traffic 段）。
type Spec struct {
	Shape            Shape   `yaml:"shape" json:"shape"`
	TotalBots        int     `yaml:"total_bots" json:"total_bots"`
	Rate             float64 `yaml:"rate" json:"rate"`                         // 稳态到达率 bot/s
	DurationSec      float64 `yaml:"duration_s" json:"duration_s"`             // 实验时长；0 表示由 TotalBots 决定
	RampSec          float64 `yaml:"ramp_s" json:"ramp_s"`                     // ShapeRamp 的爬坡时长
	Steps            []Step  `yaml:"steps" json:"steps"`                       // ShapeStep
	Spikes           []Spike `yaml:"spikes" json:"spikes"`                     // ShapeSpike
	DiurnalAmp       float64 `yaml:"diurnal_amp" json:"diurnal_amp"`           // ShapeDiurnal 的振幅，0..1
	DiurnalPeriodSec float64 `yaml:"diurnal_period_s" json:"diurnal_period_s"` // 默认 86400
	Jitter           float64 `yaml:"jitter" json:"jitter"`                     // 到达时刻抖动比例 0..0.5
	Seed             int64   `yaml:"seed" json:"seed"`
}

// Defaults 填充默认值并做基本校验。
func (s *Spec) Defaults() {
	if s.Shape == "" {
		s.Shape = ShapeConstant
	}
	if s.Rate <= 0 {
		s.Rate = 2
	}
	if s.Jitter < 0 {
		s.Jitter = 0
	}
	if s.Jitter > 0.5 {
		s.Jitter = 0.5
	}
	if s.DiurnalPeriodSec <= 0 {
		s.DiurnalPeriodSec = 86400
	}
	if s.DiurnalAmp < 0 {
		s.DiurnalAmp = 0
	}
	if s.DiurnalAmp > 1 {
		s.DiurnalAmp = 1
	}
	if s.RampSec <= 0 {
		s.RampSec = 10
	}
}

// Validate 检查配置是否自洽。
func (s *Spec) Validate() error {
	if s.TotalBots <= 0 {
		return fmt.Errorf("sched: total_bots 必须为正数，当前 %d", s.TotalBots)
	}
	switch s.Shape {
	case ShapeConstant, ShapeRamp, ShapeSpike, ShapeDiurnal:
	case ShapeStep:
		if len(s.Steps) == 0 {
			return fmt.Errorf("sched: shape=step 需要 steps")
		}
	default:
		return fmt.Errorf("sched: 未知 shape %q", s.Shape)
	}
	if s.Shape == ShapeSpike && len(s.Spikes) == 0 {
		return fmt.Errorf("sched: shape=spike 需要 spikes")
	}
	return nil
}

// RateAt 返回 t 秒时刻的瞬时到达率（bot/s）。
func (s *Spec) RateAt(tSec float64) float64 {
	if tSec < 0 {
		return 0
	}
	base := s.Rate
	switch s.Shape {
	case ShapeRamp:
		if s.RampSec > 0 && tSec < s.RampSec {
			return base * tSec / s.RampSec
		}
		return base
	case ShapeStep:
		rate := s.Steps[0].Rate
		for _, st := range s.Steps {
			if tSec >= st.AtSec {
				rate = st.Rate
			} else {
				break
			}
		}
		return rate
	case ShapeSpike:
		r := base
		for _, sp := range s.Spikes {
			if tSec >= sp.AtSec && tSec < sp.AtSec+sp.DurationSec {
				r += sp.ExtraRate
			}
		}
		return r
	case ShapeDiurnal:
		k := 2 * math.Pi / s.DiurnalPeriodSec
		v := base * (1 + s.DiurnalAmp*math.Sin(k*tSec))
		if v < 0 {
			return 0
		}
		return v
	default:
		return base
	}
}

// Arrival 是一次到达：第 Index 个 bot 在 At（相对实验起点的偏移）被调度。
type Arrival struct {
	Index int
	At    time.Duration
}

// 采样粒度与上限：累计表最多 200k 格，够精确又不会吃掉内存。
const (
	maxTableCells = 200_000
	minDT         = 0.001
	maxHorizonSec = 24 * 3600
)

// Arrivals 生成完整的到达时间表。
//
// 算法：对累计到达数 C(t)=∫rate 采样建表，再对每个 i 反解 C(t)=i。
// 这样任何 rate(t) 形状（阶跃、尖峰、正弦）都能统一处理，不用为每种形状写一遍反函数。
func (s *Spec) Arrivals() ([]Arrival, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	horizon := s.DurationSec
	if horizon <= 0 {
		horizon = s.estimateHorizon()
	}
	if horizon > maxHorizonSec {
		horizon = maxHorizonSec
	}
	if horizon <= 0 {
		horizon = 1
	}

	dt := horizon / maxTableCells
	if dt < minDT {
		dt = minDT
	}
	n := int(horizon/dt) + 2

	// cum[k] = 从 0 到 k*dt 的累计到达数。
	cum := make([]float64, n)
	var acc float64
	prev := s.RateAt(0)
	for k := 1; k < n; k++ {
		t := float64(k) * dt
		cur := s.RateAt(t)
		acc += (prev + cur) * 0.5 * dt // 梯形积分
		cum[k] = acc
		prev = cur
	}

	if cum[n-1] < float64(s.TotalBots) {
		return nil, fmt.Errorf("sched: 在 %.1fs 内只能排下 %.0f 个 bot，少于 total_bots=%d（提高 rate 或 duration_s）",
			horizon, cum[n-1], s.TotalBots)
	}

	out := make([]Arrival, s.TotalBots)
	k := 1
	for i := 0; i < s.TotalBots; i++ {
		target := float64(i) // 第 i 个 bot 在累计数达到 i 时到达
		for k < n-1 && cum[k] < target {
			k++
		}
		// 在 [k-1, k] 区间内线性插值。
		lo, hi := cum[k-1], cum[k]
		frac := 0.0
		if hi > lo {
			frac = (target - lo) / (hi - lo)
		}
		t := (float64(k-1) + frac) * dt
		out[i] = Arrival{Index: i, At: time.Duration(t * float64(time.Second))}
	}

	if s.Jitter > 0 {
		s.applyJitter(out, dt)
	}
	return out, nil
}

// applyJitter 给每个到达时刻加有界随机偏移，并把顺序重新排好。
// 目的不是改变速率，而是打掉「所有 bot 在同一毫秒发请求」这类非真实同步
// （整点惊群要专门构造，不能靠 jitter 无意造出来）。
func (s *Spec) applyJitter(a []Arrival, dt float64) {
	r := rand.New(rand.NewPCG(uint64(s.Seed), 0x9e3779b97f4a7c15))
	window := s.Jitter / math.Max(s.Rate, 1e-9) // 平均间隔 × jitter 比例
	if window <= 0 || window > dt*float64(maxTableCells) {
		window = dt
	}
	maxOff := time.Duration(window * float64(time.Second))
	for i := range a {
		d := time.Duration((r.Float64()*2 - 1) * float64(maxOff))
		a[i].At += d
		if a[i].At < 0 {
			a[i].At = 0
		}
	}
	sort.Slice(a, func(i, j int) bool {
		if a[i].At == a[j].At {
			return a[i].Index < a[j].Index
		}
		return a[i].At < a[j].At
	})
}

// estimateHorizon 在没有指定 duration 时，估算跑完 TotalBots 需要多长时间。
func (s *Spec) estimateHorizon() float64 {
	// 平均速率用 1s 采样估，避免阶梯形状里某段的瞬时值误导。
	probe := math.Min(60, maxHorizonSec)
	n := 60
	var sum float64
	for i := 1; i <= n; i++ {
		sum += s.RateAt(probe * float64(i) / float64(n))
	}
	avg := sum / float64(n)
	if avg <= 1e-9 {
		avg = s.Rate
	}
	if avg <= 0 {
		avg = 1
	}
	est := float64(s.TotalBots) / avg
	return est * 1.5
}

// LagStats 汇总调度滞后，用于判断 bot 端是否已经过载。
type LagStats struct {
	Count   int64
	MaxLag  time.Duration
	SumLag  time.Duration
	Skipped int64
}

// Dispatcher 按绝对时间表触发回调。
//
// 关键点：调度器绝不等待 bot 完成。迟到（lag > 0）只会被记录，
// 不会让后续 bot 的到达时间跟着推迟。
type Dispatcher struct {
	// LagThreshold 是判定「迟到」的阈值，供调用方做滞后统计时参考，默认 50ms。
	LagThreshold time.Duration
}

// Run 遍历到达表，在应到时刻**同步**调用 spawn。
//
// spawn 必须立刻返回——通常它里面只有 `wg.Add(1)` 和 `go ...`。
// 这一点很关键，踩过坑：
//
//   - 若调度器自己 `go spawn(...)`，那么 spawn 里的 `wg.Add(1)` 与调用方的
//     `wg.Wait()` 就存在竞态：Wait 可能在 Add 之前看到计数为 0 而直接返回，
//     于是最后几个 bot 还没起跑就被收尾流程掐掉，结果文件里静默少几行。
//   - 若 spawn 自己阻塞（比如在信号量上排队），调度器就被拖慢，退化成闭环压测。
//
// 所以契约是：**调度器负责准时喊人，喊到人不等待；是否并发由 spawn 自己决定。**
func (d *Dispatcher) Run(ctx context.Context, start time.Time, arrivals []Arrival, spawn func(ctx context.Context, a Arrival, due time.Time)) {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for _, a := range arrivals {
		select {
		case <-ctx.Done():
			return
		default:
		}
		due := start.Add(a.At)
		if wait := time.Until(due); wait > 0 {
			timer.Reset(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				return
			}
		}
		spawn(ctx, a, due)
	}
}

// ExpectedInterval 返回平均到达间隔，可直接用作 HdrHistogram 的
// coordinated-omission 修正间隔（C3 / C4）。
func (s *Spec) ExpectedInterval() time.Duration {
	iv := s.RateAt(0)
	if iv <= 1e-9 {
		iv = s.Rate
	}
	if iv <= 1e-9 {
		return time.Second
	}
	return time.Duration(float64(time.Second) / iv)
}
