// Package alloc 实现房间分配器。
//
// 匹配与分配必须解耦并分开度量（D5）：匹配器负责「把谁和谁放一起」，
// 分配器负责「给他们找一台有位置的房间服」。两者的 p99 完全可能差一个数量级，
// 混在一起看就永远定位不到根因（§5.5 面试第 5 题）。
//
// 这里刻意保留两个可被压垮的点（方案 §2.1）：
//
//  1. 空闲房间池存在单个 Redis 热键上 —— 所有人抢同一把锁、同一个分片；
//  2. 分配路径上有一把全局互斥锁 —— 分配吞吐被硬限制在 1/lock_hold_time。
//
// 两者都可以用配置关掉，用来做「修复前后」对照实验。
package alloc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"aiplayer/swarm/internal/obs"
	"aiplayer/swarm/srv/room"
	"aiplayer/swarm/srv/store"
)

// Config 是分配器配置。
type Config struct {
	Mode string `yaml:"mode" json:"mode"`
	// PoolSize 是预热空闲房间数。给了池子，正常分配只花一次 Redis 往返。
	PoolSize int `yaml:"pool_size" json:"pool_size"`
	// RefillIntervalMs 是补池周期。
	RefillIntervalMs int `yaml:"refill_interval_ms" json:"refill_interval_ms"`
	// RedisLatencyMs 是模拟的 Redis 往返延迟。
	RedisLatencyMs float64 `yaml:"redis_latency_ms" json:"redis_latency_ms"`
	// GlobalLock 打开分配器全局锁（刻意瓶颈 #2）。
	GlobalLock bool `yaml:"global_lock" json:"global_lock"`
	// PoolHotKey 让空闲池的 key 全部落到同一个 Redis 分片（刻意瓶颈 #1）。
	PoolHotKey bool `yaml:"pool_hot_key" json:"pool_hot_key"`
}

// Defaults 填充默认值。
func (c *Config) Defaults() {
	if c.Mode == "" {
		c.Mode = "ranked5v5"
	}
	if c.PoolSize <= 0 {
		c.PoolSize = 64
	}
	if c.RefillIntervalMs <= 0 {
		c.RefillIntervalMs = 100
	}
	if c.RedisLatencyMs < 0 {
		c.RedisLatencyMs = 0
	}
}

// Request 是一次分配请求。
type Request struct {
	MatchID string
	Mode    string
	Seed    uint64
	Players []room.PlayerSpec
}

// Result 是分配结果。
type Result struct {
	RoomAddr  string
	RoomToken string
	// Dropped 表示本次分配走的是「池子空了现建」的慢路径。
	Dropped bool
	// Latency 是分配器内部耗时（含 Redis 与建房间）。
	Latency time.Duration
}

const poolKey = "rooms:free"

// Allocator 是分配器实现。
type Allocator struct {
	cfg    Config
	o      *obs.Server
	rooms  *room.Manager
	redis  *store.RedisLike
	mu     sync.Mutex // 全局锁（可开关）
	misses int64
}

// New 创建分配器。
func New(cfg Config, o *obs.Server, rooms *room.Manager, redis *store.RedisLike) *Allocator {
	cfg.Defaults()
	if cfg.PoolHotKey {
		redis.ForceHotShard()
	}
	redis.SetLatency(time.Duration(cfg.RedisLatencyMs * float64(time.Millisecond)))
	return &Allocator{cfg: cfg, o: o, rooms: rooms, redis: redis}
}

// Start 启动补池循环，阻塞到 ctx 取消。
func (a *Allocator) Start(ctx context.Context) {
	t := time.NewTicker(time.Duration(a.cfg.RefillIntervalMs) * time.Millisecond)
	defer t.Stop()
	a.refill(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.refill(ctx)
		}
	}
}

// refill 把空闲池补到 PoolSize。
//
// 注意：这里每条 SAdd 都是一次 Redis 往返。热键配置下它会和 Allocate 抢同一把锁，
// 于是「补池」本身成了分配的竞争对手——这正是真实事故里最反直觉的一种：
// 后台任务把前台接口的 p99 拖下水。
func (a *Allocator) refill(ctx context.Context) {
	for i := 0; i < a.cfg.PoolSize; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if a.redis.SCard(poolKey) >= a.cfg.PoolSize {
			return
		}
		a.redis.SAdd(poolKey, fmt.Sprintf("slot-%d", i))
	}
}

// Allocate 给一局匹配分配房间。
func (a *Allocator) Allocate(ctx context.Context, req Request) (Result, error) {
	start := time.Now()

	if a.cfg.GlobalLock {
		a.mu.Lock()
		defer a.mu.Unlock()
	}

	dropped := false
	_, ok := a.redis.SPop(poolKey)
	if !ok {
		// 池子空了：现建。这条路径比走池子慢一个数量级，正是我们要压出来的形态。
		dropped = true
		a.o.RoomAllocMiss.With("pool_empty").Inc()
	}

	token, err := a.rooms.Create(req.Mode, req.MatchID, req.Seed, req.Players)
	lat := time.Since(start)
	if err != nil {
		a.o.RoomAlloc.With("error").Observe(lat.Seconds())
		return Result{}, err
	}

	label := "pool"
	if dropped {
		label = "miss"
	}
	a.o.RoomAlloc.With(label).Observe(lat.Seconds())

	return Result{
		RoomAddr:  a.rooms.AdvertiseAddr(),
		RoomToken: token,
		Dropped:   dropped,
		Latency:   lat,
	}, nil
}

// PoolSize 返回当前空闲池大小（调试用）。
func (a *Allocator) PoolSize() int { return a.redis.SCard(poolKey) }

// RedisOps 返回累计 Redis 操作数。
func (a *Allocator) RedisOps() int64 { return a.redis.Ops() }
