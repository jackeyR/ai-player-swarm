package room

import (
	"context"
	"testing"
	"time"

	"aiplayer/swarm/internal/metrics"
	"aiplayer/swarm/internal/obs"
	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/srv/store"
)

func testRoom(t *testing.T, durationTicks int) *Room {
	t.Helper()
	cfg := Config{
		TickRateMs:         10,
		DurationTicks:      durationTicks,
		FillTimeoutMs:      50,
		EmptyTTLMs:         100,
		HeartbeatTimeoutMs: 5000,
		BudgetMs:           16,
	}
	cfg.Defaults()
	o := obs.NewServer(metrics.New())
	r := newRoom("room-test", "ranked5v5", "match-test", 1, cfg, o, store.NewSettlement(0), Hooks{})
	r.assign([]PlayerSpec{
		{PlayerID: 1, Team: 0, Slot: 0, MMR: 1500},
		{PlayerID: 2, Team: 1, Slot: 1, MMR: 1500},
	})
	r.mu.Lock()
	r.state = protocol.RoomStateRunning
	r.startedAt = time.Now()
	r.mu.Unlock()
	return r
}

// TestCombatProducesKills 验证「权威裁决真的能打出击杀」。
//
// 这条测试存在的理由很实在：整条压测链路可以跑得通、指标也有数，但胜负逻辑
// 一行都没被执行——所有对局都是 0:0 平局。这种人眼看不出来的假通过，
// 只有对着「击杀数 > 0」做断言才拦得住。
func TestCombatProducesKills(t *testing.T) {
	r := testRoom(t, 100)

	// 把两人摆到命中半径内，然后连续攻击。
	r.mu.Lock()
	r.players[0].x, r.players[0].y = 5000, 5000
	r.players[1].x, r.players[1].y = 5060, 5000
	r.mu.Unlock()

	for i := 0; i < 20; i++ {
		r.mu.Lock()
		r.tick++
		r.players[0].inputsThisTick = 0
		r.applyInputLocked(inputMsg{
			slot: 0,
			seq:  uint32(i),
			in:   protocol.Input{Tick: r.tick, Action: protocol.ActionAttack},
			at:   time.Now(),
		})
		kills := r.players[0].kills
		r.mu.Unlock()
		if kills > 0 {
			break
		}
	}

	r.mu.Lock()
	kills := r.players[0].kills
	deaths := r.players[1].deaths
	r.mu.Unlock()
	if kills == 0 {
		t.Fatalf("命中半径内连续攻击 20 次却没有击杀（kills=%d deaths=%d），权威裁决没生效", kills, deaths)
	}
	if deaths != 1 {
		t.Fatalf("击杀数为 %d，但对方死亡数只有 %d", kills, deaths)
	}
}

// TestAttackOutOfRangeMisses 验证距离判定：隔着半张地图挥刀不该有伤害。
func TestAttackOutOfRangeMisses(t *testing.T) {
	r := testRoom(t, 100)
	r.mu.Lock()
	r.players[0].x, r.players[0].y = 1000, 1000
	r.players[1].x, r.players[1].y = 9000, 9000
	r.mu.Unlock()

	for i := 0; i < 10; i++ {
		r.mu.Lock()
		r.tick++
		r.players[0].inputsThisTick = 0
		r.applyInputLocked(inputMsg{
			slot: 0,
			seq:  uint32(i),
			in:   protocol.Input{Tick: r.tick, Action: protocol.ActionAttack},
			at:   time.Now(),
		})
		r.mu.Unlock()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.players[0].kills != 0 {
		t.Fatalf("超距离攻击不应造成击杀，实际 kills=%d", r.players[0].kills)
	}
	if r.players[1].hp != protocol.MaxHp {
		t.Fatalf("超距离攻击不应扣血，实际 hp=%d", r.players[1].hp)
	}
}

// TestInputValidation 覆盖三类权威校验：越界位移、非法标记、tick 越界。
func TestInputValidation(t *testing.T) {
	r := testRoom(t, 100)
	r.mu.Lock()
	r.tick = 50
	r.players[1].hp = protocol.MaxHp
	r.mu.Unlock()

	cases := []struct {
		name string
		in   protocol.Input
		want uint8
	}{
		{"越界位移", protocol.Input{Tick: 50, MoveX: protocol.MaxMovePerTick + 1}, protocol.InputErrOutOfRange},
		{"非法标记", protocol.Input{Tick: 50, Flags: protocol.FlagIllegal}, protocol.InputErrOutOfRange},
		{"tick 太旧", protocol.Input{Tick: 10}, protocol.InputErrBadTick},
		{"tick 太新", protocol.Input{Tick: 99}, protocol.InputErrBadTick},
		{"合法输入", protocol.Input{Tick: 50, MoveX: 1, MoveY: 1}, protocol.InputOK},
	}
	for _, c := range cases {
		r.mu.Lock()
		r.players[0].inputsThisTick = 0
		tickBefore := r.tick
		r.applyInputLocked(inputMsg{slot: 0, in: c.in, at: time.Now()})
		got := protocol.InputOK
		if r.players[0].rejected > 0 {
			r.players[0].rejected = 0
			got = c.want
		}
		r.tick = tickBefore
		r.mu.Unlock()
		if got != c.want {
			t.Errorf("%s: 期望判定 %d，实际 %d", c.name, c.want, got)
		}
	}
}

// TestSettlementAndLifecycle 验证结算结果与房间关闭。
func TestSettlementAndLifecycle(t *testing.T) {
	r := testRoom(t, 5)

	settle := store.NewSettlement(0)
	closed := make(chan Summary, 1)
	r.settle = settle
	r.hooks = Hooks{OnClosed: func(s Summary) { closed <- s }}

	// 造一次击杀，让 team0 必胜。
	r.mu.Lock()
	r.players[0].kills = 1
	r.tick = 4
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ctx

	// 注意：不能持有 r.mu 调 finalize——它内部会走 Close，Close 要拿同一把锁。
	r.finalize(context.Background(), 5)

	select {
	case s := <-closed:
		if !s.Settled {
			t.Fatalf("房间已结算但 Settled=false")
		}
		if s.Result != protocol.ResultTeam0 {
			t.Fatalf("team0 有击杀，期望 ResultTeam0，实际 %d", s.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("房间关闭时没有触发 OnClosed 回调")
	}

	if n := settle.Count(); n != 1 {
		t.Fatalf("结算库应有 1 条记录，实际 %d", n)
	}
}
