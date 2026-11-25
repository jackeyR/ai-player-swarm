package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"aiplayer/swarm/internal/behavior"
	"aiplayer/swarm/internal/plan"
	"aiplayer/swarm/internal/sched"
)

// TestRunProducesValidArtifacts 验证一次 run 产出的每一个文件都能被
// 对应的解析器真的读进去。
//
// 这条测试来自一个真实的坑：`result.json` 曾经是拿 yaml.Marshal 写出来的——
// 文件名叫 .json，内容是 YAML。人眼看不出区别，Go 里读回来也不报错，
// 直到用 python 的 json.load 去读才炸。**格式类 bug 只能靠「让消费者真的解析一次」发现。**
func TestRunProducesValidArtifacts(t *testing.T) {
	dir := t.TempDir()
	p := &plan.Plan{
		Name: "runner-artifacts",
		Seed: 7,
		Traffic: sched.Spec{
			Shape:       sched.ShapeConstant,
			TotalBots:   3,
			Rate:        50,
			DurationSec: 5,
			Seed:        7,
		},
		// 故意连一个关闭的端口：bot 会快速失败，测试不需要 SUT。
		Target: plan.Target{GatewayURL: "ws://127.0.0.1:1/ws", Mode: "ranked5v5"},
		Limits: plan.Limits{MaxConcurrentBots: 10, MatchWaitTimeoutS: 1, HeartbeatIntervalS: 1},
		Sink:   plan.SinkCfg{Dir: dir},
	}
	p.Defaults()
	if err := p.Validate(); err != nil {
		t.Fatalf("计划不合法: %v", err)
	}

	rl, err := New(Config{
		ID:          "test-run",
		Plan:        p,
		Catalog:     behavior.Default(),
		MetricsAddr: "", // 不监听端口
	})
	if err != nil {
		t.Fatalf("创建 runner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := rl.Run(ctx)
	if err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	if res.Arrivals != 3 {
		t.Fatalf("到达数应为 3，实际 %d", res.Arrivals)
	}
	if res.Fingerprint == "" {
		t.Fatal("计划指纹不应为空")
	}

	runDir := filepath.Join(dir, "test-run")

	// 1) result.json 必须是合法 JSON
	raw, err := os.ReadFile(filepath.Join(runDir, "result.json"))
	if err != nil {
		t.Fatalf("读 result.json: %v", err)
	}
	var decoded Result
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("result.json 不是合法 JSON: %v\n内容前 200 字节：\n%s", err, head(raw, 200))
	}
	if decoded.RunID != "test-run" {
		t.Fatalf("result.json 的 run_id 错误: %q", decoded.RunID)
	}

	// 2) meta.json 必须是合法 JSON
	raw, err = os.ReadFile(filepath.Join(runDir, "meta.json"))
	if err != nil {
		t.Fatalf("读 meta.json: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("meta.json 不是合法 JSON: %v\n%s", err, head(raw, 200))
	}
	if meta["plan_fingerprint"] == nil {
		t.Fatal("meta.json 缺少计划指纹（H8：没有指纹就没法确认是不是同一次实验）")
	}

	// 3) plan.yaml 必须是合法 YAML，且能反解回一份等价计划
	raw, err = os.ReadFile(filepath.Join(runDir, "plan.yaml"))
	if err != nil {
		t.Fatalf("读 plan.yaml: %v", err)
	}
	var roundTrip plan.Plan
	if err := yaml.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatalf("plan.yaml 不是合法 YAML: %v", err)
	}
	if roundTrip.Name != p.Name || roundTrip.Traffic.TotalBots != p.Traffic.TotalBots {
		t.Fatalf("plan.yaml 反解后与原始计划不一致: %+v", roundTrip)
	}

	// 4) bots.jsonl 每行都必须是合法 JSON，且最后一行是 run_summary
	raw, err = os.ReadFile(filepath.Join(runDir, "bots.jsonl"))
	if err != nil {
		t.Fatalf("读 bots.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 { // 3 个 bot + 1 行汇总
		t.Fatalf("期望 4 行（3 条会话 + 1 条汇总），实际 %d 行\n"+
			"res.Lines=%d res.Sessions=%d stats=%+v\n文件内容：\n%s",
			len(lines), res.Lines, res.Sessions, res.Stats, string(raw))
	}
	sawSummary := false
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("bots.jsonl 第 %d 行不是合法 JSON: %v", i+1, err)
		}
		if obj["type"] == "run_summary" {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatal("bots.jsonl 缺少 run_summary 行（analysis 依赖它读概况）")
	}
}

func head(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
