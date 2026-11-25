// Package store 模拟被测系统依赖的两个外部存储：
//
//   - RedisLike：分片的内存 KV/Set，可注入网络往返延迟。分配器的空闲房间池
//     故意全部落在一个「热分片」上，用来复现「单个 Redis 热键打满」这个最常见的
//     分配器瓶颈（方案 §2.1「故意留三个可被压垮的点」之一）。
//   - Settlement：结算写库，可注入 fsync 延迟，并且是同步调用（第三个可被压垮的点）。
//
// 这两处都可以用 config 开关关掉，用来做「修复前后对比」的实验（findings 的核心素材）。
package store

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
)

// RedisLike 是一个带分片与延迟注入的内存 Redis 模拟。
//
// 注意：这不是「模拟 Redis 的 bug」，而是模拟**客户端用法**造成的热点——
// 真实事故里 Redis 本身没坏，是所有人都访问同一个 key。
type RedisLike struct {
	shards  []*shard
	latency time.Duration
	ops     atomic.Int64
	// forceShard >= 0 时所有 key 强制落到该分片（复现热键）。
	forceShard int
}

type shard struct {
	mu   sync.RWMutex
	kv   map[string]string
	sets map[string]map[string]struct{}
}

// NewRedisLike 创建模拟 Redis。分片数 <=0 时取 8；latency 是每次操作的模拟往返时间。
func NewRedisLike(shards int, latency time.Duration) *RedisLike {
	if shards <= 0 {
		shards = 8
	}
	r := &RedisLike{latency: latency, forceShard: -1, shards: make([]*shard, shards)}
	for i := range r.shards {
		r.shards[i] = &shard{kv: map[string]string{}, sets: map[string]map[string]struct{}{}}
	}
	return r
}

// ForceHotShard 让所有 key 落到 0 号分片。返回自身便于链式配置。
func (r *RedisLike) ForceHotShard() *RedisLike {
	r.forceShard = 0
	return r
}

// SetLatency 调整模拟往返延迟（用于慢依赖故障注入，§2.7）。
func (r *RedisLike) SetLatency(d time.Duration) {
	if d < 0 {
		d = 0
	}
	r.latency = d
}

// Ops 返回累计操作数，用于报告「每个匹配要访问几次 Redis」。
func (r *RedisLike) Ops() int64 { return r.ops.Load() }

func (r *RedisLike) pick(key string) *shard {
	r.ops.Add(1)
	if r.latency > 0 {
		time.Sleep(r.latency)
	}
	if r.forceShard >= 0 {
		return r.shards[r.forceShard]
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return r.shards[int(h.Sum32())%len(r.shards)]
}

// Set 设置一个字符串键。
func (r *RedisLike) Set(key, val string) {
	s := r.pick(key)
	s.mu.Lock()
	s.kv[key] = val
	s.mu.Unlock()
}

// Get 读取一个字符串键。
func (r *RedisLike) Get(key string) (string, bool) {
	s := r.pick(key)
	s.mu.RLock()
	v, ok := s.kv[key]
	s.mu.RUnlock()
	return v, ok
}

// Del 删除一个字符串键。
func (r *RedisLike) Del(key string) {
	s := r.pick(key)
	s.mu.Lock()
	delete(s.kv, key)
	s.mu.Unlock()
}

// SAdd 向集合添加成员。
func (r *RedisLike) SAdd(key, member string) {
	s := r.pick(key)
	s.mu.Lock()
	m, ok := s.sets[key]
	if !ok {
		m = make(map[string]struct{}, 64)
		s.sets[key] = m
	}
	m[member] = struct{}{}
	s.mu.Unlock()
}

// SRem 从集合移除成员，返回是否确实存在。
func (r *RedisLike) SRem(key, member string) bool {
	s := r.pick(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.sets[key]
	if !ok {
		return false
	}
	if _, exists := m[member]; !exists {
		return false
	}
	delete(m, member)
	return true
}

// SPop 弹出任意一个成员（模拟非阻塞分配：池子空了就是空了）。
func (r *RedisLike) SPop(key string) (string, bool) {
	s := r.pick(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.sets[key]
	if !ok {
		return "", false
	}
	for k := range m {
		delete(m, k)
		return k, true
	}
	return "", false
}

// SCard 返回集合大小。
func (r *RedisLike) SCard(key string) int {
	s := r.pick(key)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sets[key])
}

// SettlementRecord 是一条结算记录。
type SettlementRecord struct {
	MatchID       string
	Mode          string
	Result        uint8
	DurationTicks int
	Players       []SettlementPlayer
	FinishedAt    time.Time
}

// SettlementPlayer 是结算记录里的单个玩家。
type SettlementPlayer struct {
	PlayerID  uint64
	Slot      uint8
	Team      uint8
	Kills     uint8
	Deaths    uint8
	Score     uint16
	MMRBefore int
	MMRAfter  int
}

// Settlement 模拟结算写库。Write 是同步加锁 + 模拟 fsync——
// 这是方案里故意留下的第三个可被压垮的点：结算写在 tick goroutine 里，
// 一旦写库变慢，房间 tick 直接被拖爆（tick 超预算率飙升）。
type Settlement struct {
	mu      sync.Mutex
	fsync   time.Duration
	records []SettlementRecord
	count   atomic.Int64
	bytes   atomic.Int64
}

// NewSettlement 创建结算库，fsync 为模拟落盘延迟。
func NewSettlement(fsync time.Duration) *Settlement {
	return &Settlement{fsync: fsync, records: make([]SettlementRecord, 0, 4096)}
}

// SetFsync 调整模拟落盘延迟（慢依赖故障注入）。
func (s *Settlement) SetFsync(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.fsync = d
}

// Write 同步写入一条结算记录。
func (s *Settlement) Write(ctx context.Context, rec SettlementRecord) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fsync > 0 {
		// 模拟 fsync：持锁睡眠，正是现实里最糟的写法，也正是我们要压出来的那类瓶颈。
		time.Sleep(s.fsync)
	}
	s.records = append(s.records, rec)
	s.count.Add(1)
	s.bytes.Add(int64(64 + 32*len(rec.Players)))
	return nil
}

// Count 返回已写入记录数（goodput 的服务端口径）。
func (s *Settlement) Count() int64 { return s.count.Load() }

// Bytes 返回已写入字节数估计。
func (s *Settlement) Bytes() int64 { return s.bytes.Load() }

// Len 返回内存中保留的记录数。
func (s *Settlement) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// Tail 返回最近 n 条记录（只读快照，给控制面/调试接口用）。
func (s *Settlement) Tail(n int) []SettlementRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n > len(s.records) {
		n = len(s.records)
	}
	out := make([]SettlementRecord, n)
	copy(out, s.records[len(s.records)-n:])
	return out
}

// String 便于日志。
func (r SettlementRecord) String() string {
	return fmt.Sprintf("settle{match=%s players=%d result=%d ticks=%d}", r.MatchID, len(r.Players), r.Result, r.DurationTicks)
}
