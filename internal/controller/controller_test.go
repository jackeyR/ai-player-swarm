package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aiplayer/swarm/internal/plan"
	"aiplayer/swarm/internal/sched"
)

// testPlanJSON 构造一份最小的负载计划：bot 连不上任何服务端，
// 于是整轮实验会在几毫秒内以「连接失败」结束——
// 这正是我们要的：**在不依赖 SUT 的前提下验证控制面契约**。
func testPlanJSON(t *testing.T) []byte {
	t.Helper()
	p := plan.Plan{
		Name: "controller-test",
		Seed: 1,
		Traffic: sched.Spec{
			Shape:       sched.ShapeConstant,
			TotalBots:   2,
			Rate:        50,
			DurationSec: 5,
		},
		Target: plan.Target{GatewayURL: "ws://127.0.0.1:1/ws", Mode: "ranked5v5"},
		Limits: plan.Limits{MaxConcurrentBots: 10, MatchWaitTimeoutS: 2},
		Sink:   plan.SinkCfg{Dir: t.TempDir()},
	}
	b, err := json.Marshal(&p)
	if err != nil {
		t.Fatalf("序列化计划: %v", err)
	}
	return b
}

func newTestController(t *testing.T) *Controller {
	t.Helper()
	return New(Config{
		Addr:              "",
		SinkDir:           t.TempDir(),
		MaxConcurrentRuns: 1,
	})
}

// TestRunLifecycle 覆盖控制面的完整契约：
// 提交 → 受理 → 轮询 → 完成 → 列表 → 停止已结束的实验返回 409。
func TestRunLifecycle(t *testing.T) {
	c := newTestController(t)
	srv := httptest.NewServer(c.Handler())
	defer srv.Close()

	// 1) 提交
	resp, err := http.Post(srv.URL+"/v1/runs", "application/json", bytes.NewReader(testPlanJSON(t)))
	if err != nil {
		t.Fatalf("提交计划: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("提交应返回 202，实际 %d：%s", resp.StatusCode, body.String())
	}
	var created struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		OutputDir string `json:"output_dir"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("解析提交响应: %v", err)
	}
	_ = resp.Body.Close()
	if created.ID == "" {
		t.Fatal("提交响应缺少 id")
	}
	if created.Status != string(StatusRunning) {
		t.Fatalf("新提交的实验状态应为 running，实际 %s", created.Status)
	}

	// 2) 轮询到结束
	var final map[string]any
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r2, err := http.Get(srv.URL + "/v1/runs/" + created.ID)
		if err != nil {
			t.Fatalf("查询实验: %v", err)
		}
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r2.Body)
		_ = r2.Body.Close()
		if err := json.Unmarshal(body.Bytes(), &final); err != nil {
			t.Fatalf("解析实验状态: %v", err)
		}
		if final["status"] != string(StatusRunning) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final["status"] == string(StatusRunning) {
		t.Fatal("实验 20 秒内没有结束（连接 127.0.0.1:1 应当立刻失败）")
	}
	if final["status"] != string(StatusFinished) {
		t.Fatalf("期望 finished，实际 %v（error=%v）", final["status"], final["error"])
	}

	// 3) 结果里必须带上 run_id、计数与分位数（本场景连不上服务端，
	//    所以 histograms 会是空的——这本身就是「连接失败」的正确表现）。
	res, ok := final["result"].(map[string]any)
	if !ok {
		t.Fatalf("完成的实验必须带 result，实际 %v", final["result"])
	}
	for _, key := range []string{"run_id", "arrivals", "sessions", "stats", "duration_ms"} {
		if _, ok := res[key]; !ok {
			t.Fatalf("result 里缺少 %s：%v", key, res)
		}
	}
	// 连不上服务端时：会话可以开始（会话计数是「尝试数」），但绝不可能完成对局。
	st, ok := res["stats"].(map[string]any)
	if !ok {
		t.Fatalf("result.stats 缺失或类型错误：%v", res["stats"])
	}
	if n, _ := st["matches_completed"].(float64); n != 0 {
		t.Fatalf("连不上服务端不可能完成对局，实际 matches_completed=%v", st["matches_completed"])
	}

	// 4) 列表
	r3, err := http.Get(srv.URL + "/v1/runs")
	if err != nil {
		t.Fatalf("列出实验: %v", err)
	}
	var list struct {
		Runs []map[string]any `json:"runs"`
	}
	_ = json.NewDecoder(r3.Body).Decode(&list)
	_ = r3.Body.Close()
	if len(list.Runs) != 1 || list.Runs[0]["id"] != created.ID {
		t.Fatalf("实验列表不符：%+v", list.Runs)
	}

	// 5) 停止一个已结束的实验：409 而不是静默成功
	r4, err := http.Post(srv.URL+"/v1/runs/"+created.ID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("停止实验: %v", err)
	}
	_ = r4.Body.Close()
	if r4.StatusCode != http.StatusConflict {
		t.Fatalf("停止已结束的实验应返回 409，实际 %d", r4.StatusCode)
	}
}

func TestRejectsBadRequests(t *testing.T) {
	c := newTestController(t)
	srv := httptest.NewServer(c.Handler())
	defer srv.Close()

	t.Run("空请求体", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/v1/runs", "application/json", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("空计划应返回 400，实际 %d", resp.StatusCode)
		}
	})

	t.Run("缺少 gateway_url", func(t *testing.T) {
		body := `{"name":"x","traffic":{"shape":"constant","total_bots":1,"rate":1}}`
		resp, err := http.Post(srv.URL+"/v1/runs", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("缺少 gateway_url 应返回 400，实际 %d", resp.StatusCode)
		}
	})

	t.Run("未知实验 id", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/v1/runs/nope")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("未知 id 应返回 404，实际 %d", resp.StatusCode)
		}
	})

	t.Run("方法不允许", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/runs", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("DELETE 应返回 405，实际 %d", resp.StatusCode)
		}
	})
}
