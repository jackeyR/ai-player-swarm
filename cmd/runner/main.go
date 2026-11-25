// Command runner 是 bot 集群的执行单元（单机）。
//
// 用法：
//
//	runner -plan plans/smoke.yaml -metrics :9091
//
// 它会：
//  1. 读负载计划与 persona 目录；
//  2. 按开环时间表起 bot；
//  3. 把每个 bot 的会话结果写进 results/<run-id>/bots.jsonl；
//  4. 结束时打印分位数汇总（HdrHistogram，已做 coordinated-omission 修正）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"aiplayer/swarm/internal/plan"
	"aiplayer/swarm/internal/runner"
)

func main() {
	planPath := flag.String("plan", "plans/smoke.yaml", "负载计划 YAML 路径")
	id := flag.String("id", "", "run id（默认按时间生成）")
	metricsAddr := flag.String("metrics", ":9091", "本 runner 的指标端点；留空关闭")
	sinkDir := flag.String("out", "", "覆盖结果目录")
	timeout := flag.Duration("timeout", 0, "整体超时（0 表示不限，直到计划跑完）")
	flag.Parse()

	p, err := plan.Load(*planPath)
	if err != nil {
		fatalf("%v", err)
	}
	if *sinkDir != "" {
		p.Sink.Dir = *sinkDir
	}
	// 计划里的相对路径按计划文件所在目录解析，保证「在哪跑都一样」。
	base := filepath.Dir(*planPath)
	if p.Behaviors.CatalogFile != "" {
		p.Behaviors.CatalogFile = plan.ResolvePath(base, p.Behaviors.CatalogFile)
	}
	if p.SUTConfig != "" {
		p.SUTConfig = plan.ResolvePath(base, p.SUTConfig)
	}
	p.Sink.Dir = plan.ResolvePath(base, p.Sink.Dir)

	catalog, err := p.LoadCatalog()
	if err != nil {
		fatalf("%v", err)
	}

	rl, err := runner.New(runner.Config{
		ID:          *id,
		Plan:        p,
		Catalog:     catalog,
		MetricsAddr: *metricsAddr,
	})
	if err != nil {
		fatalf("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	fmt.Printf("runner: 计划=%s bot=%d 到达率=%.2f/s 并发上限=%d\n",
		p.Name, p.Traffic.TotalBots, p.Traffic.Rate, p.Limits.MaxConcurrentBots)
	fmt.Printf("runner: persona=%v\n", catalog.Names())
	fmt.Printf("runner: 结果目录=%s 指标=http://%s/metrics\n", rl.Dir(), *metricsAddr)

	start := time.Now()
	res, err := rl.Run(ctx)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("runner: 用时 %s，到达 %d 个 bot\n", time.Since(start).Round(time.Millisecond), res.Arrivals)

	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "runner: "+format+"\n", args...)
	os.Exit(1)
}
