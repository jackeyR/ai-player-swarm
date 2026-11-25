// Package behavior 负责「像不像人」：persona 定义、参数分布采样与可复现随机。
//
// 对应知识清单 E6 / E7 / J2 与方案 §2.4：
//   - 每个 persona 是一组**分布**，不是一个固定脚本；
//   - 思考时间是右偏的对数正态、操作间隔是双峰、退出是危险率模型；
//   - 每 bot 独立 seed（PCG），同一个 seed 能回放出同一局，否则发现 bug 也复现不了。
package behavior

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// DistKind 是分布类型。
type DistKind string

// 支持的分布。
const (
	DistConst   DistKind = "const"
	DistUniform DistKind = "uniform"
	DistExp     DistKind = "exp"
	DistLognorm DistKind = "lognorm"
	DistPoisson DistKind = "poisson"
	DistWeibull DistKind = "weibull"
	DistBimodal DistKind = "bimodal"
)

// Dist 是一条参数分布。单位由使用处决定（秒 / 毫秒 / 局数）。
type Dist struct {
	Kind DistKind `yaml:"dist" json:"dist"`

	Value float64 `yaml:"value,omitempty" json:"value,omitempty"` // const

	Mean float64 `yaml:"mean,omitempty" json:"mean,omitempty"` // exp / uniform 的均值
	Low  float64 `yaml:"low,omitempty" json:"low,omitempty"`
	High float64 `yaml:"high,omitempty" json:"high,omitempty"`

	Mu    float64 `yaml:"mu,omitempty" json:"mu,omitempty"`       // lognorm 的对数均值
	Sigma float64 `yaml:"sigma,omitempty" json:"sigma,omitempty"` // lognorm 的对数标准差

	Lambda float64 `yaml:"lambda,omitempty" json:"lambda,omitempty"` // poisson

	Shape float64 `yaml:"shape,omitempty" json:"shape,omitempty"` // weibull
	Scale float64 `yaml:"scale,omitempty" json:"scale,omitempty"`

	// bimodal：以 BurstP 的概率取 Burst，否则取 Idle。
	BurstP float64 `yaml:"burst_p,omitempty" json:"burst_p,omitempty"`
	Burst  *Dist   `yaml:"burst,omitempty" json:"burst,omitempty"`
	Idle   *Dist   `yaml:"idle,omitempty" json:"idle,omitempty"`

	// Min / Max 是截断区间（0 表示不限）。
	Min float64 `yaml:"min,omitempty" json:"min,omitempty"`
	Max float64 `yaml:"max,omitempty" json:"max,omitempty"`
}

// Const 构造一个常量分布。
func Const(v float64) Dist { return Dist{Kind: DistConst, Value: v} }

// Lognorm 构造一个对数正态分布（参数为自然对数空间的均值与标准差）。
func Lognorm(mu, sigma float64) Dist { return Dist{Kind: DistLognorm, Mu: mu, Sigma: sigma} }

// Median 返回对数正态分布的中位数，便于把 mu 换算成人能理解的值。
func (d Dist) Median() float64 {
	if d.Kind == DistLognorm {
		return math.Exp(d.Mu)
	}
	return d.Mean
}

// Validate 检查分布参数是否可用。
func (d Dist) Validate() error {
	switch d.Kind {
	case DistConst, DistUniform, DistExp, DistLognorm, DistPoisson, DistWeibull, DistBimodal:
	case "":
		return fmt.Errorf("behavior: dist 未指定")
	default:
		return fmt.Errorf("behavior: 未知分布 %q", d.Kind)
	}
	if d.Kind == DistBimodal {
		if d.Burst == nil || d.Idle == nil {
			return fmt.Errorf("behavior: bimodal 需要 burst 与 idle")
		}
		if err := d.Burst.Validate(); err != nil {
			return err
		}
		if err := d.Idle.Validate(); err != nil {
			return err
		}
	}
	if d.Kind == DistLognorm && d.Sigma < 0 {
		return fmt.Errorf("behavior: lognorm sigma 不能为负")
	}
	return nil
}

// Sample 采样一次。返回值已按 Min/Max 截断。
//
// 截断策略：最多重采样 16 次，仍落在区间外就钳位——保证调用方永远拿到合法值，
// 也不会因为参数写错而在 bot 里死循环。
func (d Dist) Sample(r *rand.Rand) float64 {
	if d.Kind == "" {
		return d.Value
	}
	var v float64
	switch d.Kind {
	case DistConst:
		v = d.Value
	case DistUniform:
		lo, hi := d.Low, d.High
		if hi <= lo {
			lo, hi = d.Mean*0.5, d.Mean*1.5
		}
		v = lo + r.Float64()*(hi-lo)
	case DistExp:
		mean := d.Mean
		if mean <= 0 {
			mean = 1
		}
		v = r.ExpFloat64() * mean
	case DistLognorm:
		v = math.Exp(d.Mu + d.Sigma*r.NormFloat64())
	case DistPoisson:
		v = samplePoisson(d.Lambda, r)
	case DistWeibull:
		shape, scale := d.Shape, d.Scale
		if shape <= 0 {
			shape = 1
		}
		if scale <= 0 {
			scale = d.Mean
		}
		if scale <= 0 {
			scale = 1
		}
		u := r.Float64()
		if u <= 0 {
			u = math.SmallestNonzeroFloat64
		}
		v = scale * math.Pow(-math.Log(u), 1/shape)
	case DistBimodal:
		p := d.BurstP
		if p > 0 && r.Float64() < p {
			v = d.Burst.Sample(r)
		} else {
			v = d.Idle.Sample(r)
		}
	default:
		v = d.Value
	}

	// 截断
	if d.Min > 0 || d.Max > 0 {
		for i := 0; i < 16; i++ {
			okLow := d.Min <= 0 || v >= d.Min
			okHigh := d.Max <= 0 || v <= d.Max
			if okLow && okHigh {
				return v
			}
			v = d.resampleOnce(r)
		}
		if d.Min > 0 && v < d.Min {
			v = d.Min
		}
		if d.Max > 0 && v > d.Max {
			v = d.Max
		}
	}
	return v
}

func (d Dist) resampleOnce(r *rand.Rand) float64 {
	d2 := d
	d2.Min, d2.Max = 0, 0
	return d2.Sample(r)
}

// samplePoisson 采样泊松分布：lambda 小用 Knuth，大用正态近似（误差可忽略且更快）。
func samplePoisson(lambda float64, r *rand.Rand) float64 {
	if lambda <= 0 {
		return 0
	}
	if lambda > 30 {
		v := r.NormFloat64()*math.Sqrt(lambda) + lambda
		if v < 0 {
			return 0
		}
		return math.Round(v)
	}
	l := math.Exp(-lambda)
	k := 0.0
	p := 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
		if k > 1000 {
			return k
		}
	}
}

// Persona 是一种玩家原型（§2.4.1）。字段名与 plans/personas/default.yaml 一一对应。
type Persona struct {
	Name        string  `yaml:"name" json:"name"`
	Weight      float64 `yaml:"weight" json:"weight"`
	Description string  `yaml:"description,omitempty" json:"description,omitempty"`
	Targets     string  `yaml:"targets,omitempty" json:"targets,omitempty"` // 专门压什么

	// 会话层
	SessionMinutes    Dist `yaml:"session_minutes" json:"session_minutes"`         // 单次会话时长（分钟，对数正态）
	MatchesPerSession Dist `yaml:"matches_per_session" json:"matches_per_session"` // 一次会话打几局
	RequeueGapS       Dist `yaml:"requeue_gap_s" json:"requeue_gap_s"`             // 局间再排间隔（秒）

	// 操作层
	ThinkTimeMs     Dist    `yaml:"think_time_ms" json:"think_time_ms"`         // 单次思考时间（毫秒）
	BurstIntervalMs Dist    `yaml:"burst_interval_ms" json:"burst_interval_ms"` // 爆发段操作间隔
	IdleIntervalMs  Dist    `yaml:"idle_interval_ms" json:"idle_interval_ms"`   // 间歇段操作间隔
	BurstP          float64 `yaml:"burst_p" json:"burst_p"`                     // 处于爆发段的概率
	ActionsPerMatch Dist    `yaml:"actions_per_match" json:"actions_per_match"` // 每局输入条数

	// 异常与失败路径（§2.4.3 / §2.4.4：这些才是 bug 密度最高的地方）
	InvalidInputP     float64 `yaml:"invalid_input_p" json:"invalid_input_p"`           // 非法输入概率
	QueueCancelP      float64 `yaml:"queue_cancel_p" json:"queue_cancel_p"`             // 排队中取消概率
	QueueCancelAfterS float64 `yaml:"queue_cancel_after_s" json:"queue_cancel_after_s"` // 入队多久后取消
	PreSettleQuitP    float64 `yaml:"pre_settle_quit_p" json:"pre_settle_quit_p"`       // 结算前直接退出（制造脏房间）
	DisconnectP       float64 `yaml:"disconnect_p" json:"disconnect_p"`                 // 对局中断线概率
	MaxReconnects     int     `yaml:"max_reconnects" json:"max_reconnects"`             // 最多重连次数
	AfkP              float64 `yaml:"afk_p" json:"afk_p"`                               // 整局不动（挂机）
	QuitAfterLosses   int     `yaml:"quit_after_losses" json:"quit_after_losses"`       // 连败 N 局后开始考虑退出
	LossHazard        float64 `yaml:"loss_hazard" json:"loss_hazard"`                   // 危险率系数

	// 匹配画像（bot 的隐藏真实水平，§2.9 明确是人为分布）
	MMRMean float64 `yaml:"mmr_mean" json:"mmr_mean"`
	MMRStd  float64 `yaml:"mmr_std" json:"mmr_std"`
}

// Validate 校验 persona 完整性。
func (p *Persona) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("behavior: persona 缺少 name")
	}
	for name, d := range map[string]Dist{
		"session_minutes":     p.SessionMinutes,
		"matches_per_session": p.MatchesPerSession,
		"requeue_gap_s":       p.RequeueGapS,
		"think_time_ms":       p.ThinkTimeMs,
		"burst_interval_ms":   p.BurstIntervalMs,
		"idle_interval_ms":    p.IdleIntervalMs,
		"actions_per_match":   p.ActionsPerMatch,
	} {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("%s.%s: %w", p.Name, name, err)
		}
	}
	if p.Weight <= 0 {
		return fmt.Errorf("behavior: persona %s 的 weight 必须为正", p.Name)
	}
	for _, f := range []struct {
		name string
		v    float64
	}{
		{"invalid_input_p", p.InvalidInputP},
		{"queue_cancel_p", p.QueueCancelP},
		{"pre_settle_quit_p", p.PreSettleQuitP},
		{"disconnect_p", p.DisconnectP},
		{"afk_p", p.AfkP},
		{"burst_p", p.BurstP},
	} {
		if f.v < 0 || f.v > 1 {
			return fmt.Errorf("behavior: %s.%s 必须落在 [0,1]，当前 %v", p.Name, f.name, f.v)
		}
	}
	return nil
}

// SessionDuration 返回一次会话的时长（秒）。
func (p *Persona) SessionDuration(r *rand.Rand) float64 {
	return p.SessionMinutes.Sample(r) * 60
}

// NextThink 返回一次「思考时间」（毫秒）。双峰：人在打架和走路时节奏完全不同。
func (p *Persona) NextThink(r *rand.Rand) float64 {
	if p.BurstP > 0 && r.Float64() < p.BurstP {
		return math.Max(0.1, p.BurstIntervalMs.Sample(r))
	}
	return math.Max(0.1, p.IdleIntervalMs.Sample(r))
}

// ReactionMs 返回一次「注意到变化后的反应时间」（毫秒）。
// 与 NextThink 的区别：反应时间用于对局开始、复活、被击中之后的第一反应，
// 服从思考时间分布；持续操作之间的间隔用 NextThink 的双峰分布。
func (p *Persona) ReactionMs(r *rand.Rand) float64 {
	return math.Max(50, p.ThinkTimeMs.Sample(r))
}

// ShouldQuitAfterLoss 是退出危险率模型：连败越多，退出概率越陡（F2P 留存曲线的形状）。
func (p *Persona) ShouldQuitAfterLoss(consecutiveLosses int, r *rand.Rand) bool {
	if p.QuitAfterLosses <= 0 || consecutiveLosses < p.QuitAfterLosses {
		return false
	}
	h := p.LossHazard
	if h <= 0 {
		h = 0.25
	}
	k := float64(consecutiveLosses - p.QuitAfterLosses + 1)
	prob := 1 - math.Exp(-h*k)
	return r.Float64() < prob
}

// Catalog 是一组 persona 及其权重。
type Catalog struct {
	Personas []Persona `yaml:"personas" json:"personas"`
}

// LoadFile 从 YAML 读取 persona 目录。
func LoadFile(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("behavior: 读取 %s: %w", path, err)
	}
	var c Catalog
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("behavior: 解析 %s: %w", path, err)
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Normalize 校验并归一化权重，返回错误时目录不可用。
func (c *Catalog) Normalize() error {
	if len(c.Personas) == 0 {
		return fmt.Errorf("behavior: persona 目录为空")
	}
	var sum float64
	for i := range c.Personas {
		if err := c.Personas[i].Validate(); err != nil {
			return err
		}
		sum += c.Personas[i].Weight
	}
	if sum <= 0 {
		return fmt.Errorf("behavior: persona 权重之和必须为正")
	}
	for i := range c.Personas {
		c.Personas[i].Weight /= sum
	}
	return nil
}

// Pick 按权重随机挑一个 persona。
func (c *Catalog) Pick(r *rand.Rand) *Persona {
	x := r.Float64()
	var acc float64
	for i := range c.Personas {
		acc += c.Personas[i].Weight
		if x < acc {
			return &c.Personas[i]
		}
	}
	return &c.Personas[len(c.Personas)-1]
}

// ForIndex 按 bot 序号确定性地挑 persona。
//
// 用 (seed, index) 的哈希分位而不是「边跑边抽」：同一个负载计划重复跑，
// 第 42 号 bot 永远是同一个 persona，实验才可复现（K1 / E7）。
func (c *Catalog) ForIndex(index int, experimentSeed int64) *Persona {
	x := unitFloat(Splitmix64(uint64(experimentSeed) ^ (uint64(index)+1)*0x9e3779b97f4a7c15))
	var acc float64
	for i := range c.Personas {
		acc += c.Personas[i].Weight
		if x < acc {
			return &c.Personas[i]
		}
	}
	return &c.Personas[len(c.Personas)-1]
}

// Names 返回所有 persona 名字（按权重降序），用于日志与报告。
func (c *Catalog) Names() []string {
	idx := make([]int, len(c.Personas))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return c.Personas[idx[a]].Weight > c.Personas[idx[b]].Weight
	})
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		out = append(out, c.Personas[i].Name)
	}
	return out
}

// Splitmix64 是拆分种子的标准做法：把 (实验 seed, bot 序号) 变成两个独立的 PCG 种子。
func Splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// NewRand 为第 index 个 bot 创建独立、可复现的随机源。
func NewRand(experimentSeed int64, index int) *rand.Rand {
	s1 := Splitmix64(uint64(experimentSeed)*0x2545f4914f6cdd1d + uint64(index))
	s2 := Splitmix64(s1 ^ 0xd1b54a32d192ed03)
	return rand.New(rand.NewPCG(s1, s2))
}

func unitFloat(u uint64) float64 {
	// 取高 53 位映射到 [0,1)
	return float64(u>>11) / float64(1<<53)
}

// Movement 生成「不完美」的移动：朝向随机游走 + 偶发转向 + 停顿时有发生。
// 纯均匀随机的位移在服务端看是白噪声，一眼就是 bot；这里保留一点惯性。
type Movement struct {
	heading float64
	r       *rand.Rand
	jitter  float64
}

// NewMovement 创建移动生成器。jitter 是朝向抖动幅度（弧度），默认 0.35。
func NewMovement(r *rand.Rand, jitter float64) *Movement {
	if jitter <= 0 {
		jitter = 0.35
	}
	return &Movement{heading: r.Float64() * 2 * math.Pi, r: r, jitter: jitter}
}

// Next 返回本 tick 的位移向量，长度不超过 maxStep。
func (m *Movement) Next(maxStep int) (int16, int16) {
	// 6% 概率大幅转向（捡东西、换目标），其余为小幅抖动。
	if m.r.Float64() < 0.06 {
		m.heading += (m.r.Float64()*2 - 1) * math.Pi
	} else {
		m.heading += (m.r.Float64()*2 - 1) * m.jitter
	}
	// 85% 的时间在移动，15% 原地（看地图、聊天、发呆）。
	if m.r.Float64() < 0.15 {
		return 0, 0
	}
	speed := 0.4 + 0.6*m.r.Float64()
	step := float64(maxStep) * speed
	dx := int16(step * math.Cos(m.heading))
	dy := int16(step * math.Sin(m.heading))
	return dx, dy
}
