package protocol

import (
	"testing"
)

// TestDecodeSnapshotAllocs 把「快照解码要不要分配」这件事钉成可执行的断言。
//
// 背景：DecodeSnapshot 的注释曾经写着「复用调用方传入的底层数组」，
// 但它返回的是新建的 Snapshot，Players 永远是 nil，cap < n 恒成立 → 每次分配。
// 注释与实现不符比单纯的慢更危险：读代码的人会据此认为热路径没有分配。
func TestDecodeSnapshotAllocs(t *testing.T) {
	snap := Snapshot{Tick: 100, State: RoomStateRunning, Score: 42}
	for i := 0; i < PlayersPerRoom; i++ {
		snap.Players = append(snap.Players, SnapshotPlayer{Slot: uint8(i), X: int16(i * 100), Y: int16(i * 50), HP: 100})
	}
	body := snap.Encode()

	// 1) DecodeSnapshot 每次都分配：这是**有意的**，因为它返回的值可能被塞进 channel。
	allocs := testing.AllocsPerRun(200, func() {
		if _, err := DecodeSnapshot(body); err != nil {
			t.Fatalf("解码: %v", err)
		}
	})
	if allocs < 1 {
		t.Fatalf("DecodeSnapshot 应当至少分配一次（返回独立副本），实际 %.2f", allocs)
	}

	// 2) DecodeSnapshotInto 在 dst 已预热的情况下必须零分配。
	dst := Snapshot{Players: make([]SnapshotPlayer, 0, MaxSnapshotPlayers)}
	// 先解一次把底层数组准备好
	if err := DecodeSnapshotInto(body, &dst); err != nil {
		t.Fatalf("解码: %v", err)
	}
	intoAllocs := testing.AllocsPerRun(200, func() {
		if err := DecodeSnapshotInto(body, &dst); err != nil {
			t.Fatalf("解码: %v", err)
		}
	})
	if intoAllocs != 0 {
		t.Fatalf("DecodeSnapshotInto 应当零分配（dst 已预热），实际 %.2f 次/op", intoAllocs)
	}
}

// TestDecodeSnapshotIntoOverwrites 确认复用 dst 时内容被完整覆盖，不会残留上一帧数据。
func TestDecodeSnapshotIntoOverwrites(t *testing.T) {
	full := Snapshot{Tick: 1, Players: make([]SnapshotPlayer, PlayersPerRoom)}
	for i := range full.Players {
		full.Players[i] = SnapshotPlayer{Slot: uint8(i), X: 1, Y: 2, HP: 3}
	}
	dst := Snapshot{Players: make([]SnapshotPlayer, MaxSnapshotPlayers)}
	if err := DecodeSnapshotInto(full.Encode(), &dst); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if len(dst.Players) != PlayersPerRoom {
		t.Fatalf("人数应为 %d，实际 %d", PlayersPerRoom, len(dst.Players))
	}

	// 换一条只有 2 个人的快照，dst 必须收缩到 2 条而不是残留 10 条。
	small := Snapshot{Tick: 2, Players: []SnapshotPlayer{{Slot: 0, X: 9, Y: 9, HP: 9}, {Slot: 1}}}
	if err := DecodeSnapshotInto(small.Encode(), &dst); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if len(dst.Players) != 2 {
		t.Fatalf("人数应收缩到 2，实际 %d（残留了上一帧的数据）", len(dst.Players))
	}
	if dst.Players[0].X != 9 {
		t.Fatalf("第 0 个玩家没被覆盖：%+v", dst.Players[0])
	}
}
