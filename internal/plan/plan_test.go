package plan

import (
	"path/filepath"
	"testing"

	"aiplayer/swarm/internal/sched"
)

func basePlan() *Plan {
	p := &Plan{
		Name:      "fp-test",
		Seed:      42,
		Traffic:   sched.Spec{Shape: sched.ShapeConstant, TotalBots: 10, Rate: 2, DurationSec: 30},
		Target:    Target{GatewayURL: "ws://127.0.0.1:8080/ws", Mode: "ranked5v5"},
		Behaviors: Behaviors{CatalogFile: "personas/default.yaml"},
		Limits:    Limits{MaxConcurrentBots: 100},
		Sink:      SinkCfg{Dir: "results"},
	}
	p.Defaults()
	return p
}

// TestFingerprintIgnoresMachinePaths 是这条规则的回归守卫：
// **同一份计划在不同机器/不同目录下必须得到同一个指纹。**
//
// 背景：cmd/runner 在读计划时会把三个路径字段解析成绝对路径。如果指纹直接
// 哈希整份计划，`D:\work\results` 和 `/home/me/results` 会算出不同指纹，
// 于是「指纹相同 ⇒ 是同一次实验」这个唯一用途就失效了（H8）。
func TestFingerprintIgnoresMachinePaths(t *testing.T) {
	p1 := basePlan()
	p1.Sink.Dir = "results"
	p1.Behaviors.CatalogFile = "personas/default.yaml"
	p1.SUTConfig = "deploy/sut.smoke.yaml"

	f1, err := p1.Fingerprint()
	if err != nil {
		t.Fatalf("指纹: %v", err)
	}

	// 同一个计划，被 runner 解析成了不同平台上的绝对路径。
	for _, variant := range []struct {
		dir, catalog, sut string
	}{
		{"D:\\work\\临时\\results", "D:\\work\\临时\\plans\\personas\\default.yaml", "D:\\work\\临时\\deploy\\sut.smoke.yaml"},
		{"/home/me/results", "/home/me/plans/personas/default.yaml", "/home/me/deploy/sut.smoke.yaml"},
		{"../results", filepath.Join("..", "plans", "personas", "default.yaml"), filepath.Join("..", "deploy", "sut.smoke.yaml")},
	} {
		p2 := basePlan()
		p2.Sink.Dir = variant.dir
		p2.Behaviors.CatalogFile = variant.catalog
		p2.SUTConfig = variant.sut
		f2, err := p2.Fingerprint()
		if err != nil {
			t.Fatalf("指纹: %v", err)
		}
		if f1 != f2 {
			t.Fatalf("同一份计划的指纹随目录变化：\n  %s (results)\n  %s (%s)", f1, f2, variant.dir)
		}
	}
}

// TestFingerprintSensitiveToSemantics 反过来也要守住：
// 真正影响实验结果的字段变了，指纹必须变，否则「指纹相同」就不可信了。
func TestFingerprintSensitiveToSemantics(t *testing.T) {
	base := basePlan()
	f0, err := base.Fingerprint()
	if err != nil {
		t.Fatalf("指纹: %v", err)
	}

	cases := map[string]func(p *Plan){
		"seed":       func(p *Plan) { p.Seed = 43 },
		"bot 数":      func(p *Plan) { p.Traffic.TotalBots++ },
		"到达率":        func(p *Plan) { p.Traffic.Rate *= 2 },
		"persona 文件": func(p *Plan) { p.Behaviors.CatalogFile = "personas/smoke.yaml" },
		"SUT 配置":     func(p *Plan) { p.SUTConfig = "deploy/sut.fixed.yaml" },
		"并发上限":       func(p *Plan) { p.Limits.MaxConcurrentBots = 200 },
		"网关地址":       func(p *Plan) { p.Target.GatewayURL = "ws://10.0.0.2:8080/ws" },
	}
	for name, mutate := range cases {
		p := basePlan()
		mutate(p)
		f, err := p.Fingerprint()
		if err != nil {
			t.Fatalf("%s: 指纹: %v", name, err)
		}
		if f == f0 {
			t.Errorf("%s 变化后指纹没变（%s），「指纹相同=同一次实验」不再成立", name, f)
		}
	}
}

// TestFingerprintDeterministic 同一对象重复计算必须稳定。
func TestFingerprintDeterministic(t *testing.T) {
	p := basePlan()
	a, err := p.Fingerprint()
	if err != nil {
		t.Fatalf("指纹: %v", err)
	}
	for i := 0; i < 5; i++ {
		b, err := p.Fingerprint()
		if err != nil {
			t.Fatalf("指纹: %v", err)
		}
		if a != b {
			t.Fatalf("同一计划的指纹不稳定：%s != %s", a, b)
		}
	}
	if len(a) != 16 {
		t.Fatalf("指纹长度应为 16，实际 %d（%s）", len(a), a)
	}
}

// TestLoadSmokePlan 确认仓库里的示例计划都能被解析并通过校验。
func TestLoadSmokePlan(t *testing.T) {
	for _, path := range []string{"../../plans/smoke.yaml", "../../plans/baseline.yaml", "../../plans/peak.yaml"} {
		p, err := Load(path)
		if err != nil {
			t.Errorf("加载 %s: %v", path, err)
			continue
		}
		if _, err := p.Fingerprint(); err != nil {
			t.Errorf("%s 指纹: %v", path, err)
		}
	}
}
