// Package sut 把被测系统的四层组装起来：接入层 → 匹配器 → 分配器 → 房间服。
//
// 为什么单独一个包而不是全塞进 main：
//   - `cmd/sutd` 只剩参数解析，读起来五秒钟能看懂这个系统由哪几块组成；
//   - 测试可以进程内起一整套 SUT（端口取 0），做真正的端到端验证，
//     而不是用一堆 mock 证明「我的 mock 能通」。
package sut

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"aiplayer/swarm/internal/metrics"
	"aiplayer/swarm/internal/obs"
	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/srv/account"
	"aiplayer/swarm/srv/alloc"
	"aiplayer/swarm/srv/config"
	"aiplayer/swarm/srv/gateway"
	"aiplayer/swarm/srv/match"
	"aiplayer/swarm/srv/room"
	"aiplayer/swarm/srv/store"
)

// MetricsDisabled 是「关掉指标端点」的配置值。
//
// 为什么需要它：空字符串在 config.Defaults() 里会被当成「没填」而填上默认端口，
// 于是测试想关掉指标端点却关不掉，还会和本机跑着的 sutd 抢 9090。
const MetricsDisabled = "-"

// System 是一整套被测系统。
type System struct {
	Cfg     *config.Config
	Reg     *metrics.Registry
	O       *obs.Server
	Redis   *store.RedisLike
	Settle  *store.Settlement
	Rooms   *room.Manager
	Alloc   *alloc.Allocator
	Match   *match.Matchmaker
	Gateway *gateway.Server

	gatewayAddr string
	roomAddr    string
	metricsAddr string

	metricsSrv *http.Server
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	started    bool
}

// New 按配置组装系统（尚未监听端口）。
func New(cfg *config.Config) (*System, error) {
	if cfg == nil {
		return nil, fmt.Errorf("sut: 配置为空")
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	reg := metrics.New()
	reg.EnableRuntimeMetrics()
	o := obs.NewServer(reg)

	redis := store.NewRedisLike(cfg.Redis.Shards, durationMs(cfg.Redis.LatencyMs))
	settle := store.NewSettlement(durationMs(cfg.Settlement.FsyncMs))

	// 结算回调：房间把战绩交出来，这里回写 MMR。
	// 用函数变量而不是直接塞闭包，是因为 room.Hooks 在构造房间时按值拷贝。
	var onSettled func(room.Summary)
	hooks := room.Hooks{
		OnClosed: func(s room.Summary) {
			if onSettled != nil {
				onSettled(s)
			}
		},
	}

	mgr := room.NewManager(cfg.Room, o, settle, hooks)
	al := alloc.New(cfg.Alloc, o, mgr, redis)
	mm := match.New(cfg.Match, o, al, cfg.Room)
	gw := gateway.New(cfg.Gateway, o, reg, mm)
	acct := gw.AccountService()

	onSettled = func(s room.Summary) {
		if !s.Settled || len(s.Players) == 0 {
			return
		}
		var sum [2]int
		var cnt [2]int
		for _, p := range s.Players {
			t := int(p.Team) % 2
			sum[t] += p.MMR
			cnt[t]++
		}
		avg := func(t int) int {
			if cnt[t] == 0 {
				return 1500
			}
			return sum[t] / cnt[t]
		}
		outcomes := make([]account.MatchOutcome, 0, len(s.Players))
		for _, p := range s.Players {
			win := (s.Result == protocol.ResultTeam0 && p.Team == 0) ||
				(s.Result == protocol.ResultTeam1 && p.Team == 1)
			outcomes = append(outcomes, account.MatchOutcome{
				PlayerID: p.PlayerID,
				Win:      win,
				TeamMMR:  avg(1 - int(p.Team)%2),
			})
		}
		acct.ApplyMatch(outcomes)
	}

	return &System{
		Cfg:     cfg,
		Reg:     reg,
		O:       o,
		Redis:   redis,
		Settle:  settle,
		Rooms:   mgr,
		Alloc:   al,
		Match:   mm,
		Gateway: gw,
	}, nil
}

// Start 开始监听并启动各层循环。地址为空或端口为 0 时会自动取随机端口。
func (s *System) Start(ctx context.Context) error {
	if s.started {
		return fmt.Errorf("sut: 已启动")
	}
	s.ctx, s.cancel = context.WithCancel(ctx)

	roomAddr := s.Cfg.RoomAdvertiseAddr
	if roomAddr == "" {
		roomAddr = "127.0.0.1:0"
	}
	if err := s.Rooms.Listen(roomAddr); err != nil {
		return err
	}
	s.roomAddr = s.Rooms.AdvertiseAddr()

	gwAddr, err := s.Gateway.Start(s.Cfg.Gateway.Addr)
	if err != nil {
		return err
	}
	s.gatewayAddr = gwAddr

	if s.Cfg.MetricsAddr != "" && s.Cfg.MetricsAddr != MetricsDisabled {
		ln, err := net.Listen("tcp", s.Cfg.MetricsAddr)
		if err != nil {
			return fmt.Errorf("sut: 指标端点: %w", err)
		}
		s.metricsAddr = ln.Addr().String()
		mux := http.NewServeMux()
		mux.Handle("/metrics", s.Reg.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
		s.metricsSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			_ = s.metricsSrv.Serve(ln)
		}()
	}

	s.wg.Add(3)
	go func() { defer s.wg.Done(); _ = s.Rooms.Serve(s.ctx) }()
	go func() { defer s.wg.Done(); s.Alloc.Start(s.ctx) }()
	go func() { defer s.wg.Done(); s.Match.Start(s.ctx) }()

	s.started = true
	return nil
}

// Stop 优雅关闭。
func (s *System) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.Gateway.Shutdown(shutdownCtx)
	if s.metricsSrv != nil {
		_ = s.metricsSrv.Shutdown(shutdownCtx)
	}
	s.Rooms.Stop()
	s.wg.Wait()
}

// GatewayWSURL 返回 bot 应连接的接入层地址。
func (s *System) GatewayWSURL() string { return "ws://" + s.gatewayAddr + "/ws" }

// RoomAddr 返回房间服地址。
func (s *System) RoomAddr() string { return s.roomAddr }

// MetricsAddr 返回指标端点地址（未启用时为空）。
func (s *System) MetricsAddr() string { return s.metricsAddr }

// AccountService 暴露账号服务（测试里核对 MMR 用）。
func (s *System) AccountService() *account.Service { return s.Gateway.AccountService() }

func durationMs(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}
