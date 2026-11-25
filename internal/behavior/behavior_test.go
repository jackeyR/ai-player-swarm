package behavior

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"
)

// TestDefaultCatalogValid 验证内置 persona 目录自身是合法的。
func TestDefaultCatalogValid(t *testing.T) {
	c := Default()
	if len(c.Personas) != 6 {
		t.Fatalf("内置目录应有 6 种原型，实际 %d", len(c.Personas))
	}
	var sum float64
	for _, p := range c.Personas {
		if err := p.Validate(); err != nil {
			t.Fatalf("%s: %v", p.Name, err)
		}
		sum += p.Weight
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("权重之和应为 1，实际 %v", sum)
	}
}

// TestForIndexDeterministic 是可复现性的地基：
// 同一个 (seed, index) 必须永远挑到同一个 persona，否则「同一份计划跑两次」没法对比。
func TestForIndexDeterministic(t *testing.T) {
	c := Default()
	for i := 0; i < 200; i++ {
		a := c.ForIndex(i, 12345)
		b := c.ForIndex(i, 12345)
		if a.Name != b.Name {
			t.Fatalf("第 %d 个 bot 的 persona 不确定: %s vs %s", i, a.Name, b.Name)
		}
	}
	// 换 seed 应该换分布，否则 seed 就白设了。
	diff := 0
	for i := 0; i < 500; i++ {
		if c.ForIndex(i, 1).Name != c.ForIndex(i, 2).Name {
			diff++
		}
	}
	if diff < 10 {
		t.Fatalf("不同 seed 下 persona 分布几乎相同（仅 %d/500 不同），种子没起作用", diff)
	}
}

// TestForIndexRespectsWeights 用大样本粗略校验权重真的生效。
func TestForIndexRespectsWeights(t *testing.T) {
	c := Default()
	counts := map[string]int{}
	const n = 20000
	for i := 0; i < n; i++ {
		counts[c.ForIndex(i, 99).Name]++
	}
	for _, p := range c.Personas {
		got := float64(counts[p.Name]) / n
		if math.Abs(got-p.Weight) > 0.02 {
			t.Errorf("%s 实际占比 %.3f，期望 %.3f", p.Name, got, p.Weight)
		}
	}
}

// TestNewRandDeterministic 验证随机源可复现（E7）。
func TestNewRandDeterministic(t *testing.T) {
	a := NewRand(7, 42)
	b := NewRand(7, 42)
	for i := 0; i < 100; i++ {
		if a.Float64() != b.Float64() {
			t.Fatalf("同种子第 %d 次抽样不一致", i)
		}
	}
	c := NewRand(7, 43)
	if c.Float64() == NewRand(7, 42).Float64() {
		t.Fatal("不同 bot 序号的随机序列不应相同")
	}
}

// TestLognormShape 验证对数正态的右偏：中位数应接近 exp(mu)，长尾明显。
func TestLognormShape(t *testing.T) {
	d := Lognorm(math.Log(30), 0.5) // 中位 30
	r := rand.New(rand.NewPCG(1, 2))
	samples := make([]float64, 5000)
	var sum float64
	for i := range samples {
		samples[i] = d.Sample(r)
		sum += samples[i]
	}
	sort.Float64s(samples)
	median := samples[len(samples)/2]
	if math.Abs(median-30)/30 > 0.15 {
		t.Fatalf("对数正态中位数应为 30 左右，实际 %.2f", median)
	}
	mean := sum / float64(len(samples))
	// 右偏：均值 > 中位数。
	if mean <= median {
		t.Fatalf("对数正态应右偏（均值 %.2f > 中位 %.2f），实际相反", mean, median)
	}
	p99 := samples[int(float64(len(samples))*0.99)]
	if p99/median < 2 {
		t.Fatalf("长尾不足：p99/中位 = %.2f", p99/median)
	}
}

func TestExpMean(t *testing.T) {
	d := Dist{Kind: DistExp, Mean: 45}
	r := rand.New(rand.NewPCG(3, 4))
	var sum float64
	const n = 20000
	for i := 0; i < n; i++ {
		sum += d.Sample(r)
	}
	if got := sum / n; math.Abs(got-45)/45 > 0.05 {
		t.Fatalf("指数分布均值应约 45，实际 %.2f", got)
	}
}

func TestTruncation(t *testing.T) {
	d := Dist{Kind: DistLognorm, Mu: math.Log(100), Sigma: 1.5, Min: 50, Max: 150}
	r := rand.New(rand.NewPCG(5, 6))
	for i := 0; i < 5000; i++ {
		v := d.Sample(r)
		if v < 50 || v > 150 {
			t.Fatalf("截断失效：采样到 %.2f（区间 [50,150]）", v)
		}
	}
}

func TestBimodalStaysInRange(t *testing.T) {
	d := Dist{
		Kind:   DistBimodal,
		BurstP: 0.6,
		Burst:  &Dist{Kind: DistUniform, Low: 100, High: 300},
		Idle:   &Dist{Kind: DistUniform, Low: 3000, High: 10000},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("双峰分布校验失败: %v", err)
	}
	r := rand.New(rand.NewPCG(7, 8))
	burst, idle := 0, 0
	for i := 0; i < 5000; i++ {
		v := d.Sample(r)
		switch {
		case v >= 100 && v <= 300:
			burst++
		case v >= 3000 && v <= 10000:
			idle++
		default:
			t.Fatalf("双峰分布采到区间外的值 %.2f", v)
		}
	}
	if frac := float64(burst) / 5000; math.Abs(frac-0.6) > 0.03 {
		t.Fatalf("爆发段占比应约 0.6，实际 %.3f", frac)
	}
}

// TestQuitHazard 验证退出危险率：连败越多，退出概率越陡。
func TestQuitHazard(t *testing.T) {
	p := Persona{Name: "x", Weight: 1, QuitAfterLosses: 3, LossHazard: 0.4}
	r := rand.New(rand.NewPCG(9, 10))
	rate := func(losses int) float64 {
		quits := 0
		const n = 20000
		for i := 0; i < n; i++ {
			if p.ShouldQuitAfterLoss(losses, r) {
				quits++
			}
		}
		return float64(quits) / n
	}
	if got := rate(2); got != 0 {
		t.Fatalf("连败未达阈值不应退出，实际概率 %.3f", got)
	}
	r3, r5 := rate(3), rate(5)
	if r3 <= 0 || r5 <= r3 {
		t.Fatalf("危险率应随连败单调上升：3 连败 %.3f，5 连败 %.3f", r3, r5)
	}
}

// TestNextThinkBimodal 验证「打架 vs 走路」两种节奏的切换是真实的。
func TestNextThinkBimodal(t *testing.T) {
	p := Default().Personas[0]
	r := rand.New(rand.NewPCG(11, 12))
	fast, slow := 0, 0
	for i := 0; i < 2000; i++ {
		v := p.NextThink(r)
		if v < 1000 {
			fast++
		} else {
			slow++
		}
	}
	if fast == 0 || slow == 0 {
		t.Fatalf("双峰节奏没有体现：fast=%d slow=%d", fast, slow)
	}
}

// TestMovementBounded 验证移动生成器不会超过服务端允许的步长。
func TestMovementBounded(t *testing.T) {
	r := rand.New(rand.NewPCG(13, 14))
	m := NewMovement(r, 0.35)
	const maxStep = 40
	for i := 0; i < 10000; i++ {
		dx, dy := m.Next(maxStep)
		if dx < -maxStep || dx > maxStep || dy < -maxStep || dy > maxStep {
			t.Fatalf("位移越界: dx=%d dy=%d（上限 %d）", dx, dy, maxStep)
		}
	}
}

// TestLoadFileMatchesDefaults 守住「YAML 与内置默认值一致」这条线，
// 避免代码改了、例子配置没改，大家拿着过期配置做实验。
func TestLoadFileMatchesDefaults(t *testing.T) {
	c, err := LoadFile("../../plans/personas/default.yaml")
	if err != nil {
		t.Fatalf("加载默认 persona 目录: %v", err)
	}
	def := Default()
	if len(c.Personas) != len(def.Personas) {
		t.Fatalf("persona 数量不一致: yaml=%d go=%d", len(c.Personas), len(def.Personas))
	}
	byName := map[string]Persona{}
	for _, p := range def.Personas {
		byName[p.Name] = p
	}
	for _, p := range c.Personas {
		gp, ok := byName[p.Name]
		if !ok {
			t.Fatalf("yaml 里有 Go 默认值没有的 persona: %s", p.Name)
		}
		if math.Abs(p.Weight-gp.Weight) > 1e-9 {
			t.Errorf("%s 权重不一致: yaml=%v go=%v", p.Name, p.Weight, gp.Weight)
		}
		if math.Abs(p.MMRMean-gp.MMRMean) > 1e-9 || math.Abs(p.MMRStd-gp.MMRStd) > 1e-9 {
			t.Errorf("%s 的 MMR 分布不一致", p.Name)
		}
		if p.QuitAfterLosses != gp.QuitAfterLosses {
			t.Errorf("%s 的 quit_after_losses 不一致: yaml=%d go=%d",
				p.Name, p.QuitAfterLosses, gp.QuitAfterLosses)
		}
		if math.Abs(p.SessionMinutes.Mu-gp.SessionMinutes.Mu) > 1e-9 ||
			math.Abs(p.SessionMinutes.Sigma-gp.SessionMinutes.Sigma) > 1e-9 {
			t.Errorf("%s 的会话时长分布不一致", p.Name)
		}
	}
}
