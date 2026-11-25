// Command sutd 启动「被测系统」：接入层 + 匹配器 + 分配器 + 房间服。
//
// 一个进程托管整条链路，是为了让一个人能在笔记本上跑通
// 「发现—定位—修复—验证」的闭环。真实集群里这四层会分开放，
// 但**分层复用的边界是一样的**（方案 §1.2）。
//
// 用法：
//
//	sutd -config deploy/sut.yaml
//	sutd -gateway :8080 -room 127.0.0.1:8090 -leak=false
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aiplayer/swarm/srv/config"
	"aiplayer/swarm/srv/sut"
)

func main() {
	cfgPath := flag.String("config", "deploy/sut.yaml", "SUT 配置文件路径（不存在时用默认值）")
	gatewayAddr := flag.String("gateway", "", "覆盖接入层监听地址，例如 :8080")
	roomAddr := flag.String("room", "", "覆盖房间服监听/通告地址，例如 127.0.0.1:8090")
	metricsAddr := flag.String("metrics", "", "覆盖指标端点地址，例如 :9090")
	leak := flag.Bool("leak", false, "打开房间泄漏注入（复现 room_leak_gauge > 0）")
	noLock := flag.Bool("no-global-lock", false, "关闭分配器全局锁（修复对照实验）")
	noHotKey := flag.Bool("no-hot-key", false, "关闭空闲池单分片热点（修复对照实验）")
	fastSettle := flag.Bool("fast-settle", false, "关闭结算模拟 fsync（修复对照实验）")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("sutd: %v", err)
	}
	applyOverrides(cfg, overrides{
		gatewayAddr: *gatewayAddr,
		roomAddr:    *roomAddr,
		metricsAddr: *metricsAddr,
		leak:        *leak,
		noLock:      *noLock,
		noHotKey:    *noHotKey,
		fastSettle:  *fastSettle,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sys, err := sut.New(cfg)
	if err != nil {
		log.Fatalf("sutd: %v", err)
	}
	if err := sys.Start(ctx); err != nil {
		log.Fatalf("sutd: %v", err)
	}

	fmt.Printf("sutd: 接入层 %s\n", sys.GatewayWSURL())
	fmt.Printf("sutd: 房间服 %s\n", sys.RoomAddr())
	fmt.Printf("sutd: 指标   http://%s/metrics\n", sys.MetricsAddr())
	fmt.Printf("sutd: 房间 tick=%dms 一局=%d ticks  分配器全局锁=%v 空闲池热键=%v 结算fsync=%.1fms 房间泄漏注入=%v\n",
		cfg.Room.TickRateMs, cfg.Room.DurationTicks,
		cfg.Alloc.GlobalLock, cfg.Alloc.PoolHotKey, cfg.Settlement.FsyncMs, cfg.Room.LeakInjection)

	<-ctx.Done()
	fmt.Println("sutd: 收到退出信号，开始优雅关闭")
	start := time.Now()
	sys.Stop()
	rooms := sys.Rooms.RoomCount()
	fmt.Printf("sutd: 已退出（%.0fms）。剩余房间=%d 结算写入=%d 登录=%d\n",
		float64(time.Since(start).Milliseconds()), rooms, sys.Settle.Count(), sys.AccountService().LoginCount())
}

type overrides struct {
	gatewayAddr string
	roomAddr    string
	metricsAddr string
	leak        bool
	noLock      bool
	noHotKey    bool
	fastSettle  bool
}

// applyOverrides 让命令行开关覆盖配置文件，方便做同名实验的对照跑法。
func applyOverrides(cfg *config.Config, ov overrides) {
	if ov.gatewayAddr != "" {
		cfg.Gateway.Addr = ov.gatewayAddr
	}
	if ov.roomAddr != "" {
		cfg.RoomAdvertiseAddr = ov.roomAddr
	}
	if ov.metricsAddr != "" {
		cfg.MetricsAddr = ov.metricsAddr
	}
	if ov.leak {
		cfg.Room.LeakInjection = true
	}
	if ov.noLock {
		cfg.Alloc.GlobalLock = false
	}
	if ov.noHotKey {
		cfg.Alloc.PoolHotKey = false
	}
	if ov.fastSettle {
		cfg.Settlement.FsyncMs = 0
	}
}

func loadConfig(path string) (*config.Config, error) {
	if _, err := os.Stat(path); err != nil {
		fmt.Printf("sutd: 未找到 %s，使用默认配置\n", path)
		return config.Default(), nil
	}
	return config.Load(path)
}
