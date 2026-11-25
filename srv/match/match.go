// Package match 实现 MMR 匹配器：桶 + 逐步放宽窗口。
//
// 对应知识清单 D9 / D10 / D11 / D13：
//   - 匹配是「先严格后放宽」的等待-质量折中，窗口随等待时长线性放宽；
//   - 匹配成功后立刻把匹配结果交给分配器，两段延迟分别度量（D5）；
//   - 冷启动/低在线时允许「不满开局」，否则凌晨 3 点的池子会永远凑不齐。
package match

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"aiplayer/swarm/internal/obs"
	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/srv/alloc"
	"aiplayer/swarm/srv/room"
)

// Config 是匹配器配置。
type Config struct {
	Mode string `yaml:"mode" json:"mode"`
	// TeamSize 是单边人数，5 => 5v5。
	TeamSize int `yaml:"team_size" json:"team_size"`
	// LoopIntervalMs 是匹配循环周期，默认 100ms。
	LoopIntervalMs int `yaml:"loop_interval_ms" json:"loop_interval_ms"`
	// InitialWindow 是初始可接受的 MMR 差。
	InitialWindow int `yaml:"initial_window" json:"initial_window"`
	// WindowGrowthPerSec 是每秒放宽的 MMR。
	WindowGrowthPerSec int `yaml:"window_growth_per_sec" json:"window_growth_per_sec"`
	// MaxWindow 是放宽上限。
	MaxWindow int `yaml:"max_window" json:"max_window"`
	// StarveAfterMs 是「等这么久还没凑齐就直接开」的阈值（不满足开局，D13）。
	StarveAfterMs int `yaml:"starve_after_ms" json:"starve_after_ms"`
}

// Defaults 填充默认值。
func (c *Config) Defaults() {
	if c.Mode == "" {
		c.Mode = "ranked5v5"
	}
	if c.TeamSize <= 0 {
		c.TeamSize = protocol.TeamSize
	}
	if c.LoopIntervalMs <= 0 {
		c.LoopIntervalMs = 100
	}
	if c.InitialWindow <= 0 {
		c.InitialWindow = 100
	}
	if c.WindowGrowthPerSec <= 0 {
		c.WindowGrowthPerSec = 50
	}
	if c.MaxWindow <= 0 {
		c.MaxWindow = 800
	}
	if c.StarveAfterMs <= 0 {
		c.StarveAfterMs = 30000
	}
}

// Assignment 是匹配器交给 bot 的最终结果。
type Assignment struct {
	MatchID       string
	RoomAddr      string
	RoomToken     string
	Players       []protocol.MatchedPlayer
	MMRSpread     int
	WaitMs        int64
	MatchMs       int64
	AllocMicros   int64
	Seed          uint64
	TickRateMs    int
	DurationTicks int
	Slot          uint8
	Team          uint8
	Err           error
}

// Ticket 是一张排队票据。
type Ticket struct {
	ID       string
	PlayerID uint64
	MMR      int
	JoinedAt time.Time
	// Result 缓冲为 1：匹配器推送结果绝不阻塞匹配循环。
	Result    chan Assignment
	cancelled atomic.Bool
}

// Matchmaker 是匹配器。
type Matchmaker struct {
	cfg     Config
	o       *obs.Server
	al      *alloc.Allocator
	roomCfg room.Config

	mu      sync.Mutex
	pending []*Ticket
	byID    map[string]*Ticket
	seq     atomic.Uint64

	ctx  context.Context
	stop chan struct{}
	wg   sync.WaitGroup

	matches atomic.Int64
}

// New 创建匹配器。
func New(cfg Config, o *obs.Server, al *alloc.Allocator, roomCfg room.Config) *Matchmaker {
	cfg.Defaults()
	return &Matchmaker{
		cfg:     cfg,
		o:       o,
		al:      al,
		roomCfg: roomCfg,
		byID:    make(map[string]*Ticket, 1024),
		stop:    make(chan struct{}),
	}
}

// Start 启动匹配循环，阻塞到 ctx 取消。
func (m *Matchmaker) Start(ctx context.Context) {
	m.ctx = ctx
	t := time.NewTicker(time.Duration(m.cfg.LoopIntervalMs) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-t.C:
			m.step()
		}
	}
}

// Stop 停止匹配器。
func (m *Matchmaker) Stop() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
}

// Enqueue 把玩家放进队列，返回票据。
func (m *Matchmaker) Enqueue(playerID uint64, mmr int) *Ticket {
	id := fmt.Sprintf("t%08d", m.seq.Add(1))
	t := &Ticket{
		ID:       id,
		PlayerID: playerID,
		MMR:      mmr,
		JoinedAt: time.Now(),
		Result:   make(chan Assignment, 1),
	}
	m.mu.Lock()
	m.pending = append(m.pending, t)
	m.byID[id] = t
	m.mu.Unlock()
	m.o.QueueJoin.With(m.cfg.Mode).Inc()
	return t
}

// Cancel 取消排队。返回是否成功取消（已经匹配成功就取消不了）。
func (m *Matchmaker) Cancel(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.byID[id]
	if !ok {
		return false
	}
	if !t.cancelled.CompareAndSwap(false, true) {
		return false
	}
	delete(m.byID, id)
	m.o.QueueCancel.With(m.cfg.Mode).Inc()
	return true
}

// Mode 返回匹配器的模式名。接入层用它校验客户端的入队请求。
func (m *Matchmaker) Mode() string { return m.cfg.Mode }

// Pending 返回当前排队人数。
func (m *Matchmaker) Pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}

// Matches 返回已成型局数。
func (m *Matchmaker) Matches() int64 { return m.matches.Load() }

// windowFor 计算等待了 waited 之后的可接受 MMR 窗口。
func (m *Matchmaker) windowFor(waited time.Duration) int {
	w := m.cfg.InitialWindow + int(waited.Seconds())*m.cfg.WindowGrowthPerSec
	if w > m.cfg.MaxWindow {
		w = m.cfg.MaxWindow
	}
	return w
}

type formation struct {
	matchID  string
	group    []*Ticket
	teams    []uint8
	spread   int
	formedAt time.Time
	seed     uint64
}

func (m *Matchmaker) step() {
	m.mu.Lock()
	all := m.pending
	m.pending = nil
	m.mu.Unlock()

	if len(all) == 0 {
		return
	}

	now := time.Now()
	// FIFO：先来先服务。按等待时长排序而不是按 MMR——否则冷门分段会被饿死（D13）。
	sort.SliceStable(all, func(i, j int) bool { return all[i].JoinedAt.Before(all[j].JoinedAt) })

	active := make([]*Ticket, 0, len(all))
	for _, t := range all {
		if t.cancelled.Load() {
			continue
		}
		active = append(active, t)
	}
	if len(active) == 0 {
		return
	}

	need := 2 * m.cfg.TeamSize
	used := make([]bool, len(active))
	var formations []*formation

	for i := range active {
		if used[i] || active[i].cancelled.Load() {
			continue
		}
		seed := active[i]
		window := m.windowFor(now.Sub(seed.JoinedAt))
		group := []int{i}
		for j := i + 1; j < len(active); j++ {
			if used[j] || active[j].cancelled.Load() {
				continue
			}
			if absInt(active[j].MMR-seed.MMR) <= window {
				group = append(group, j)
				if len(group) >= need {
					break
				}
			}
		}

		if len(group) < need {
			// 凑不齐：要么再等等，要么等太久就「不满开局」。
			if len(group) < 2 || now.Sub(seed.JoinedAt) < time.Duration(m.cfg.StarveAfterMs)*time.Millisecond {
				continue
			}
			m.o.MatchesImmediate.With(m.cfg.Mode).Inc()
		}

		f := m.form(active, group, now)
		for _, idx := range group {
			used[idx] = true
		}
		formations = append(formations, f)
	}

	// 没被选中的票据放回队列头部，保持 FIFO 顺序。
	var left []*Ticket
	for i := range active {
		if used[i] || active[i].cancelled.Load() {
			continue
		}
		left = append(left, active[i])
	}
	m.mu.Lock()
	if len(left) > 0 {
		m.pending = append(left, m.pending...)
	}
	m.mu.Unlock()

	for _, f := range formations {
		// 分配是阻塞的，放到独立 goroutine：匹配循环不能被分配拖慢（D5 解耦）。
		go m.allocate(f)
	}
}

// form 把一组票据分成两队并生成匹配。
func (m *Matchmaker) form(active []*Ticket, group []int, now time.Time) *formation {
	tickets := make([]*Ticket, 0, len(group))
	for _, idx := range group {
		tickets = append(tickets, active[idx])
	}
	// 按 MMR 降序，奇偶交替分边（1、3、5 名去一队，2、4、6 名去另一队），
	// 这是最简单的平衡方式，也让「碾压局」比例可观测（D11）。
	sort.SliceStable(tickets, func(i, j int) bool { return tickets[i].MMR > tickets[j].MMR })

	teams := make([]uint8, len(tickets))
	minMMR, maxMMR := math.MaxInt32, 0
	for i, t := range tickets {
		if i%2 == 0 {
			teams[i] = 0
		} else {
			teams[i] = 1
		}
		if t.MMR < minMMR {
			minMMR = t.MMR
		}
		if t.MMR > maxMMR {
			maxMMR = t.MMR
		}
	}
	spread := maxMMR - minMMR

	matchID := fmt.Sprintf("%s-%06d", m.cfg.Mode, m.seq.Add(1))
	seed := rand.Uint64()

	for _, t := range tickets {
		wait := now.Sub(t.JoinedAt)
		m.o.MatchWait.With(obs.MMRBucketLabel(t.MMR)).Observe(wait.Seconds())
	}
	m.o.MatchTotal.With(m.cfg.Mode).Inc()
	m.o.MatchMMRSpread.With(m.cfg.Mode).Observe(float64(spread))
	m.matches.Add(1)

	return &formation{
		matchID:  matchID,
		group:    tickets,
		teams:    teams,
		spread:   spread,
		formedAt: now,
		seed:     seed,
	}
}

// allocate 调用分配器并推送结果。
func (m *Matchmaker) allocate(f *formation) {
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	specs := make([]room.PlayerSpec, len(f.group))
	for i, t := range f.group {
		specs[i] = room.PlayerSpec{
			PlayerID: t.PlayerID,
			Team:     f.teams[i],
			Slot:     uint8(i),
			MMR:      t.MMR,
		}
	}

	res, err := m.al.Allocate(ctx, alloc.Request{
		MatchID: f.matchID,
		Mode:    m.cfg.Mode,
		Seed:    f.seed,
		Players: specs,
	})

	now := time.Now()
	players := make([]protocol.MatchedPlayer, len(f.group))
	for i, t := range f.group {
		players[i] = protocol.MatchedPlayer{
			PlayerID: t.PlayerID,
			MMR:      t.MMR,
			Team:     f.teams[i],
			Slot:     uint8(i),
		}
	}

	for i, t := range f.group {
		a := Assignment{
			MatchID:       f.matchID,
			Players:       players,
			MMRSpread:     f.spread,
			WaitMs:        now.Sub(t.JoinedAt).Milliseconds(),
			MatchMs:       f.formedAt.Sub(t.JoinedAt).Milliseconds(),
			Seed:          f.seed,
			TickRateMs:    m.roomCfg.TickRateMs,
			DurationTicks: m.roomCfg.DurationTicks,
			Slot:          uint8(i),
			Team:          f.teams[i],
		}
		if err != nil {
			a.Err = err
		} else {
			a.RoomAddr = res.RoomAddr
			a.RoomToken = res.RoomToken
			a.AllocMicros = res.Latency.Microseconds()
		}
		select {
		case t.Result <- a:
		default:
			// 票据已被取消：结果无人接收，丢弃。
		}
	}

	m.mu.Lock()
	for _, t := range f.group {
		delete(m.byID, t.ID)
	}
	m.mu.Unlock()
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
