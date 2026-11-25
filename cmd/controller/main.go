// Command controller 是控制面：一个 HTTP 服务，接受负载计划并启动 runner。
//
// 用法：
//
//	controller -addr :7070 -sink results
//
// 端点（10 个以内就够，别引框架）：
//
//	GET  /healthz
//	GET  /metrics
//	GET  /v1/runs                列出全部实验
//	POST /v1/runs                启动实验（body: {plan_path} 或 {plan} 或直接是计划 YAML）
//	GET  /v1/runs/{id}           查询单个实验
//	POST /v1/runs/{id}/stop      停止实验
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aiplayer/swarm/internal/controller"
)

func main() {
	addr := flag.String("addr", ":7070", "控制面监听地址")
	sinkDir := flag.String("sink", "", "结果根目录（覆盖计划里的 sink.dir）")
	maxRuns := flag.Int("max-runs", 1, "同时运行的实验数上限")
	flag.Parse()

	c := controller.New(controller.Config{
		Addr:              *addr,
		SinkDir:           *sinkDir,
		MaxConcurrentRuns: *maxRuns,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		fmt.Printf("controller: 控制面 http://%s\n", *addr)
		if err := c.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "controller: %v\n", err)
			stop()
		}
	}()

	<-ctx.Done()
	fmt.Println("controller: 收到退出信号，取消在跑的实验")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Shutdown(shutdownCtx)
}
