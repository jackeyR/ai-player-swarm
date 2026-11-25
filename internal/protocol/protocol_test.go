package protocol

import (
	"reflect"
	"testing"
)

func TestBinBufRoundTrip(t *testing.T) {
	w := NewBinWriter(16)
	w.U8(1).U16(0x1234).U32(0xDEADBEEF).U64(0x0102030405060708).I16(-1234).Str8("abc")
	b := w.Bytes()

	r := NewBinReader(b)
	if got := r.U8(); got != 1 {
		t.Fatalf("U8 = %d", got)
	}
	if got := r.U16(); got != 0x1234 {
		t.Fatalf("U16 = %#x", got)
	}
	if got := r.U32(); got != 0xDEADBEEF {
		t.Fatalf("U32 = %#x", got)
	}
	if got := r.U64(); got != 0x0102030405060708 {
		t.Fatalf("U64 = %#x", got)
	}
	if got := r.I16(); got != -1234 {
		t.Fatalf("I16 = %d", got)
	}
	if got := r.Str8(); got != "abc" {
		t.Fatalf("Str8 = %q", got)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("不应有解析错误: %v", err)
	}
	if r.Remaining() != 0 {
		t.Fatalf("应当读完，剩余 %d 字节", r.Remaining())
	}
}

// TestBinReaderOverflow 验证越界读取不会 panic，而是置错误位。
func TestBinReaderOverflow(t *testing.T) {
	r := NewBinReader([]byte{1, 2})
	_ = r.U8()
	_ = r.U32() // 越界
	if err := r.Err(); err == nil {
		t.Fatal("越界读取应当置错误")
	}
	if r.Remaining() != 0 {
		t.Fatal("出错后 Remaining 应为 0")
	}
}

func TestRoomMessagesRoundTrip(t *testing.T) {
	join := RoomJoinReq{Token: "tok", PlayerID: 77, Reconn: true}
	got1, err := DecodeRoomJoinReq(join.Encode())
	if err != nil || !reflect.DeepEqual(got1, join) {
		t.Fatalf("RoomJoinReq 往返失败: %+v err=%v", got1, err)
	}

	resp := RoomJoinResp{OK: true, Slot: 3, Players: 10, Seed: 99, TickRateMs: 100,
		DurationTicks: 600, StartAtMs: 1700000000000, ServerNowMs: 1700000000500}
	got2, err := DecodeRoomJoinResp(resp.Encode())
	if err != nil || !reflect.DeepEqual(got2, resp) {
		t.Fatalf("RoomJoinResp 往返失败: %+v err=%v", got2, err)
	}

	in := Input{Tick: 42, MoveX: -13, MoveY: 14, Action: ActionAttack, Flags: FlagIllegal}
	got3, err := DecodeInput(in.Encode())
	if err != nil || got3 != in {
		t.Fatalf("Input 往返失败: %+v err=%v", got3, err)
	}
	if n := len(in.Encode()); n != 10 {
		t.Fatalf("Input 应为定长 10 字节，实际 %d", n)
	}

	ack := InputAck{Seq: 7, Accepted: false, ErrCode: InputErrOutOfRange}
	got4, err := DecodeInputAck(ack.Encode())
	if err != nil || got4 != ack {
		t.Fatalf("InputAck 往返失败: %+v err=%v", got4, err)
	}

	snap := Snapshot{Tick: 88, State: RoomStateRunning, Score: 300, Players: []SnapshotPlayer{
		{Slot: 0, X: -100, Y: 200, HP: 75, Flags: 1, Score: 200},
		{Slot: 9, X: 9000, Y: -50, HP: 0, Flags: 0, Score: 100},
	}}
	got5, err := DecodeSnapshot(snap.Encode())
	if err != nil {
		t.Fatalf("Snapshot 解码: %v", err)
	}
	if got5.Tick != snap.Tick || got5.Score != snap.Score || !reflect.DeepEqual(got5.Players, snap.Players) {
		t.Fatalf("Snapshot 往返失败: %+v", got5)
	}

	settle := Settle{Tick: 600, Result: ResultTeam1, Kills: 12, Deaths: 12, Score: 1200, DurationTicks: 600}
	got6, err := DecodeSettle(settle.Encode())
	if err != nil || got6 != settle {
		t.Fatalf("Settle 往返失败: %+v err=%v", got6, err)
	}

	reconn := ReconnectReq{Token: "t", PlayerID: 5, Slot: 2}
	got7, err := DecodeReconnectReq(reconn.Encode())
	if err != nil || got7 != reconn {
		t.Fatalf("ReconnectReq 往返失败: %+v err=%v", got7, err)
	}

	prog := SessionProgress{Tick: 10, TotalTicks: 600, ElapsedMs: 1000}
	got8, err := DecodeSessionProgress(prog.Encode())
	if err != nil || got8 != prog {
		t.Fatalf("SessionProgress 往返失败: %+v err=%v", got8, err)
	}
}

// TestSnapshotPlayerCap 验证快照人数上限：超限的数据包必须被拒绝，
// 否则恶意/错版本服务端可以让 bot 分配任意大的切片。
func TestSnapshotPlayerCap(t *testing.T) {
	body := []byte{0, 0, 0, 1, RoomStateRunning, 0, 0, 200} // count=200 > 10
	if _, err := DecodeSnapshot(body); err == nil {
		t.Fatal("超过房间容量的快照应当被拒绝")
	}
}

func TestMalformedBodies(t *testing.T) {
	for _, c := range []struct {
		name string
		fn   func() error
	}{
		{"RoomJoinReq", func() error { _, err := DecodeRoomJoinReq([]byte{9}); return err }},
		{"RoomJoinResp", func() error { _, err := DecodeRoomJoinResp(nil); return err }},
		{"Input", func() error { _, err := DecodeInput([]byte{1, 2, 3}); return err }},
		{"Settle", func() error { _, err := DecodeSettle([]byte{1}); return err }},
	} {
		if err := c.fn(); err == nil {
			t.Errorf("%s 截断消息应当报错", c.name)
		}
	}
}

func TestGatewayMarshalUnmarshal(t *testing.T) {
	mf := MatchFound{
		MatchID: "m1", Mode: "ranked5v5", RoomAddr: "127.0.0.1:8090", RoomToken: "tok",
		Players:   []MatchedPlayer{{PlayerID: 1, MMR: 1500, Team: 0, Slot: 0}},
		MMRSpread: 40, WaitMs: 120, MatchMs: 100, AllocMicros: 850,
		Seed: 7, TickRateMs: 100, DurationTicks: 600,
	}
	raw, err := Marshal(MsgMatchFound, 3, mf)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out MatchFound
	env, err := Unmarshal(raw, &out)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if env.MsgID != MsgMatchFound || env.Seq != 3 {
		t.Fatalf("信封错误: msg=%#x seq=%d", env.MsgID, env.Seq)
	}
	if !reflect.DeepEqual(out, mf) {
		t.Fatalf("消息往返不一致: %+v", out)
	}
	if MsgName(MsgMatchFound) != "MatchFound" {
		t.Fatalf("MsgName 错误: %s", MsgName(MsgMatchFound))
	}
}
