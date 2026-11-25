package sut_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"aiplayer/swarm/internal/bot"
	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/srv/sut"
)

// humanOutcome 是一个真人客户端跑完一局后的结果。
type humanOutcome struct {
	settle protocol.Settle
	inputs int
	mmr    int
	err    error
}

// TestHumanClientAPI 端到端验证「真人客户端」用的那套公开 API。
//
// 这条测试覆盖的是 bot 不会走到的路径：
//   - ConnectPlayer（握手 + 签名登录的公开封装）
//   - EnterQueue / WaitMatch / JoinMatch
//   - 手动按快照节奏发输入（真人就是这样操作的：看到一个 tick 才动一下）
//   - **结算与关连接的竞态**：服务端先发结算帧再关连接，客户端必须仍能拿到结算
//
// 最后一点是这条测试存在的核心理由。这个竞态在 cmd/client 里真实出现过：
// 两边都打完了一局，服务端也记了 2 条 session，但客户端 select 随机选中了
// `<-Closed()`，把对局记成掉线，goodput 直接少一半。
func TestHumanClientAPI(t *testing.T) {
	cfg := smokeConfig()
	cfg.MetricsAddr = sut.MetricsDisabled

	sys, err := sut.New(cfg)
	if err != nil {
		t.Fatalf("组装 SUT: %v", err)
	}
	if err := sys.Start(context.Background()); err != nil {
		t.Fatalf("启动 SUT: %v", err)
	}
	defer sys.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	const players = 2
	results := make(chan humanOutcome, players)

	var wg sync.WaitGroup
	for i := 0; i < players; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results <- playLikeHuman(ctx, t, sys.GatewayWSURL(), cfg.Gateway.ClientSecret, idx)
		}(i)
	}
	wg.Wait()
	close(results)

	var got []humanOutcome
	for r := range results {
		if r.err != nil {
			t.Fatalf("客户端失败：%v", r.err)
		}
		got = append(got, r)
	}
	if len(got) != players {
		t.Fatalf("期望 %d 个客户端都拿到结算，实际 %d 个", players, len(got))
	}

	// 两个人必须拿到**同一局**的结算：tick 与结果一致。
	for i := 1; i < len(got); i++ {
		if got[i].settle.Tick != got[0].settle.Tick {
			t.Fatalf("两个客户端的结算 tick 不同：%d vs %d（不是同一局）",
				got[0].settle.Tick, got[i].settle.Tick)
		}
		if got[i].settle.Result != got[0].settle.Result {
			t.Fatalf("同一局的结果不一致：%d vs %d", got[0].settle.Result, got[i].settle.Result)
		}
	}
	for i, r := range got {
		if r.inputs == 0 {
			t.Errorf("客户端 %d 一条输入都没发出去", i)
		}
		if r.mmr <= 0 {
			t.Errorf("客户端 %d 的 MMR 异常：%d", i, r.mmr)
		}
	}
	if got[0].settle.Tick == 0 {
		t.Error("结算 tick 为 0，说明对局根本没跑")
	}
}

// playLikeHuman 用公开 API 打完一局，返回结算。
func playLikeHuman(ctx context.Context, t *testing.T, gatewayURL, secret string, idx int) (out humanOutcome) {
	p, err := bot.ConnectPlayer(ctx, bot.Config{
		GatewayURL:       gatewayURL,
		ClientSecret:     []byte(secret),
		DialTimeout:      5 * time.Second,
		MatchWaitTimeout: 20 * time.Second,
	}, fmt.Sprintf("human-test-%d", idx), 1500)
	if err != nil {
		out.err = fmt.Errorf("连接: %w", err)
		return out
	}
	defer p.Close()
	out.mmr = p.MMR()

	if err := p.EnterQueue(ctx, ""); err != nil {
		out.err = fmt.Errorf("入队: %w", err)
		return out
	}
	mf, err := p.WaitMatch(ctx, 20*time.Second)
	if err != nil {
		out.err = fmt.Errorf("等匹配: %w", err)
		return out
	}
	room, err := p.JoinMatch(ctx, mf)
	if err != nil {
		out.err = fmt.Errorf("进房: %w", err)
		return out
	}
	defer room.Close()

	var lastTick uint32
	for {
		select {
		case <-ctx.Done():
			out.err = ctx.Err()
			return out

		case snap, ok := <-room.Snapshots():
			if !ok {
				out.err = errors.New("快照通道关闭")
				return out
			}
			lastTick = snap.Tick
			// 真人每看到一个 tick 动一下：这里就按这个节奏发输入。
			in := protocol.Input{Tick: lastTick + 1, MoveX: protocol.MaxMovePerTick * 7 / 10, Action: protocol.ActionAttack}
			if _, err := room.SendInput(in); err != nil {
				out.err = fmt.Errorf("发输入: %w", err)
				return out
			}
			out.inputs++
			// 心跳必须发，否则 5 秒后被服务端判死连接踢掉。
			_ = room.Ping()

		case st := <-room.Settles():
			out.settle = st
			return out

		case <-room.Closed():
			// 关键路径：先补收一次结算，拿不到才算掉线。
			if st, ok := room.TrySettle(); ok {
				out.settle = st
				return out
			}
			out.err = errors.New("房间关闭且没有收到结算（结算帧被丢了）")
			return out
		}
	}
}
