package protocol

// 数据面（裸 TCP + 定长二进制）的消息体定义。
//
// 布局一览（大端）：
//
//	RoomJoinReq   token(str8) playerID(u64) reconn(u8)
//	RoomJoinResp  ok(u8) slot(u8) errCode(u8) players(u8) seed(u64) tickRateMs(u16)
//	              durationTicks(u32) startAtMs(u64) serverNowMs(u64)
//	Input         tick(u32) moveX(i16) moveY(i16) action(u8) flags(u8)
//	InputAck      seq(u32) accepted(u8) errCode(u8)
//	Snapshot      tick(u32) state(u8) score(u16) count(u8) [slot(u8) x(i16) y(i16) hp(u16) flags(u8)]*
//	Settle        tick(u32) result(u8) kills(u8) deaths(u8) score(u16) durationTicks(u32)
//	ReconnectReq  token(str8) playerID(u64) slot(u8)

// 房间规则常量（bot 必须遵守同一套规则：权威服务器，D1）。
const (
	// PlayersPerRoom 是 5v5 的总人数。
	PlayersPerRoom = 10
	// TeamSize 是单边人数。
	TeamSize = 5
	// WorldScale 是「1 米 = 100 单位」的定点缩放。
	WorldScale = 100
	// WorldSize 是世界边长（单位）。
	WorldSize = 100 * WorldScale
	// MaxMovePerTick 是服务端允许的单 tick 最大位移，越界即为非法输入（E8 的靶子）。
	//
	// 40 单位/tick：10Hz 下等于 4 m/s，接近真人的跑动速度。
	// 设成 15（1.5m/s 的快步走）会让短对局里两队根本走不到一起，
	// 结果所有对局都是 0:0 平局——压测跑得通，但胜负逻辑一行都没被执行。
	MaxMovePerTick = 40
	// AttackRange 是近战命中半径（单位）。bot 与房间服共用这一个值，
	// 避免「客户端以为能打到、服务端判不中」这类协议口径不一致的 bug。
	AttackRange = 120
	// AttackDamage 是单次命中的伤害。
	AttackDamage = 25
	// MaxHp 是初始血量。
	MaxHp = 100
)

// 房间状态（随快照下发，bot 用它对齐生命周期）。
const (
	RoomStateWaiting  uint8 = 0
	RoomStateRunning  uint8 = 1
	RoomStateSettling uint8 = 2
	RoomStateClosed   uint8 = 3
)

// 结算结果。
const (
	ResultDraw  uint8 = 0
	ResultTeam0 uint8 = 1
	ResultTeam1 uint8 = 2
)

// Input.Action 取值。
const (
	ActionNone   uint8 = 0
	ActionAttack uint8 = 1
	ActionSkill  uint8 = 2
	ActionPickup uint8 = 3
	ActionEmote  uint8 = 4
)

// Input.Flags 位标志。
const (
	// FlagIllegal 表示 bot 故意构造的非法输入（超射程 / 越界位移），用于验证服务端校验。
	FlagIllegal uint8 = 1 << 0
	// FlagDuplicate 表示重复投递，用于验证幂等。
	FlagDuplicate uint8 = 1 << 1
	// FlagOutOfOrder 表示乱序投递。
	FlagOutOfOrder uint8 = 1 << 2
)

// 进房错误码。
const (
	JoinOK            uint8 = 0
	JoinErrBadToken   uint8 = 1
	JoinErrRoomFull   uint8 = 2
	JoinErrRoomClosed uint8 = 3
	JoinErrDuplicate  uint8 = 4
	JoinErrExpired    uint8 = 5
)

// 输入被拒原因（服务端权威校验的返回值）。
const (
	InputOK                uint8 = 0
	InputErrOutOfRange     uint8 = 1
	InputErrRateLimited    uint8 = 2
	InputErrRoomNotRunning uint8 = 3
	InputErrBadTick        uint8 = 4
)

// RoomJoinReq 是 bot 进入房间服后的第一帧。
type RoomJoinReq struct {
	Token    string // 匹配器下发的 room_token
	PlayerID uint64
	Reconn   bool
}

// Encode 序列化。
func (m *RoomJoinReq) Encode() []byte {
	w := NewBinWriter(64)
	w.Str8(m.Token).U64(m.PlayerID)
	if m.Reconn {
		w.U8(1)
	} else {
		w.U8(0)
	}
	return w.Bytes()
}

// DecodeRoomJoinReq 反序列化。
func DecodeRoomJoinReq(b []byte) (RoomJoinReq, error) {
	r := NewBinReader(b)
	var m RoomJoinReq
	m.Token = r.Str8()
	m.PlayerID = r.U64()
	m.Reconn = r.U8() == 1
	return m, r.Err()
}

// RoomJoinResp 是进房结果。StartAtMs 是房间开局时刻（绝对墙钟毫秒），
// bot 用它对齐 tick，不用自己的本地时间瞎猜。
type RoomJoinResp struct {
	OK            bool
	Slot          uint8
	ErrCode       uint8
	Players       uint8
	Seed          uint64
	TickRateMs    uint16
	DurationTicks uint32
	StartAtMs     uint64
	ServerNowMs   uint64
}

// Encode 序列化。
func (m *RoomJoinResp) Encode() []byte {
	w := NewBinWriter(32)
	if m.OK {
		w.U8(1)
	} else {
		w.U8(0)
	}
	w.U8(m.Slot).U8(m.ErrCode).U8(m.Players).U64(m.Seed).
		U16(m.TickRateMs).U32(m.DurationTicks).U64(m.StartAtMs).U64(m.ServerNowMs)
	return w.Bytes()
}

// DecodeRoomJoinResp 反序列化。
func DecodeRoomJoinResp(b []byte) (RoomJoinResp, error) {
	r := NewBinReader(b)
	var m RoomJoinResp
	m.OK = r.U8() == 1
	m.Slot = r.U8()
	m.ErrCode = r.U8()
	m.Players = r.U8()
	m.Seed = r.U64()
	m.TickRateMs = r.U16()
	m.DurationTicks = r.U32()
	m.StartAtMs = r.U64()
	m.ServerNowMs = r.U64()
	return m, r.Err()
}

// Input 是一条玩家输入，固定 10 字节。
type Input struct {
	Tick   uint32
	MoveX  int16
	MoveY  int16
	Action uint8
	Flags  uint8
}

// Encode 序列化。
func (m *Input) Encode() []byte {
	w := NewBinWriter(10)
	w.U32(m.Tick).I16(m.MoveX).I16(m.MoveY).U8(m.Action).U8(m.Flags)
	return w.Bytes()
}

// DecodeInput 反序列化。
func DecodeInput(b []byte) (Input, error) {
	r := NewBinReader(b)
	var m Input
	m.Tick = r.U32()
	m.MoveX = r.I16()
	m.MoveY = r.I16()
	m.Action = r.U8()
	m.Flags = r.U8()
	return m, r.Err()
}

// InputAck 是输入确认（服务端权威校验结果）。
type InputAck struct {
	Seq      uint32
	Accepted bool
	ErrCode  uint8
}

// Encode 序列化。
func (m *InputAck) Encode() []byte {
	w := NewBinWriter(8)
	w.U32(m.Seq)
	if m.Accepted {
		w.U8(1)
	} else {
		w.U8(0)
	}
	w.U8(m.ErrCode)
	return w.Bytes()
}

// DecodeInputAck 反序列化。
func DecodeInputAck(b []byte) (InputAck, error) {
	r := NewBinReader(b)
	var m InputAck
	m.Seq = r.U32()
	m.Accepted = r.U8() == 1
	m.ErrCode = r.U8()
	return m, r.Err()
}

// SnapshotPlayer 是快照里的单个玩家状态。
//
// Score 是给客户端用的：真人客户端要能看见「谁在领先」，而服务端本来就记着
// 每个玩家的得分。少这一个字段，客户端只能显示总分，做不出像样的记分板。
type SnapshotPlayer struct {
	Slot  uint8
	X     int16
	Y     int16
	HP    uint16
	Flags uint8
	Score uint16
}

// Snapshot 是 10Hz 状态同步的整包快照（D3：MVP 用全量快照，
// 增量与 AOI 兴趣管理留作演进项，见 docs/METRICS.md）。
type Snapshot struct {
	Tick    uint32
	State   uint8
	Score   uint16
	Players []SnapshotPlayer
}

// MaxSnapshotPlayers 用于预分配。
const MaxSnapshotPlayers = PlayersPerRoom

// Encode 序列化。为省分配，调用方可复用 buf。
func (m *Snapshot) Encode() []byte {
	w := NewBinWriter(16 + len(m.Players)*10)
	w.U32(m.Tick).U8(m.State).U16(m.Score).U8(uint8(len(m.Players)))
	for i := range m.Players {
		p := &m.Players[i]
		w.U8(p.Slot).I16(p.X).I16(p.Y).U16(p.HP).U8(p.Flags).U16(p.Score)
	}
	return w.Bytes()
}

// DecodeSnapshot 反序列化到一个新分配的 Snapshot。
//
// 每次调用都会为 Players 分配一次底层数组——这是**刻意**的：调用方通常会把
// Snapshot 塞进 channel 交给别的 goroutine，若复用同一块内存，下一条快照会
// 覆盖还在排队的那条。需要在同一个循环里同步消费时，用 DecodeSnapshotInto。
func DecodeSnapshot(b []byte) (Snapshot, error) {
	var m Snapshot
	if err := DecodeSnapshotInto(b, &m); err != nil {
		return m, err
	}
	return m, nil
}

// DecodeSnapshotInto 解码到调用方提供的 dst，并复用 dst.Players 的底层数组。
//
// 使用前提：**同步消费**。解码完立刻读，读完了才解下一条；不要把 dst 放进
// 队列或 map 里长期持有。
func DecodeSnapshotInto(b []byte, dst *Snapshot) error {
	r := NewBinReader(b)
	dst.Tick = r.U32()
	dst.State = r.U8()
	dst.Score = r.U16()
	n := int(r.U8())
	if n > MaxSnapshotPlayers {
		return ErrBadBody
	}
	if cap(dst.Players) < n {
		dst.Players = make([]SnapshotPlayer, n)
	}
	dst.Players = dst.Players[:n]
	for i := 0; i < n; i++ {
		dst.Players[i] = SnapshotPlayer{
			Slot:  r.U8(),
			X:     r.I16(),
			Y:     r.I16(),
			HP:    r.U16(),
			Flags: r.U8(),
			Score: r.U16(),
		}
	}
	return r.Err()
}

// Settle 是结算推送。
type Settle struct {
	Tick          uint32
	Result        uint8
	Kills         uint8
	Deaths        uint8
	Score         uint16
	DurationTicks uint32
}

// Encode 序列化。
func (m *Settle) Encode() []byte {
	w := NewBinWriter(16)
	w.U32(m.Tick).U8(m.Result).U8(m.Kills).U8(m.Deaths).U16(m.Score).U32(m.DurationTicks)
	return w.Bytes()
}

// DecodeSettle 反序列化。
func DecodeSettle(b []byte) (Settle, error) {
	r := NewBinReader(b)
	var m Settle
	m.Tick = r.U32()
	m.Result = r.U8()
	m.Kills = r.U8()
	m.Deaths = r.U8()
	m.Score = r.U16()
	m.DurationTicks = r.U32()
	return m, r.Err()
}

// ReconnectReq 是断线重连请求（D6）。room_token 在有效窗口内可重复使用。
type ReconnectReq struct {
	Token    string
	PlayerID uint64
	Slot     uint8
}

// Encode 序列化。
func (m *ReconnectReq) Encode() []byte {
	w := NewBinWriter(64)
	w.Str8(m.Token).U64(m.PlayerID).U8(m.Slot)
	return w.Bytes()
}

// DecodeReconnectReq 反序列化。
func DecodeReconnectReq(b []byte) (ReconnectReq, error) {
	r := NewBinReader(b)
	var m ReconnectReq
	m.Token = r.Str8()
	m.PlayerID = r.U64()
	m.Slot = r.U8()
	return m, r.Err()
}

// SessionProgress 是服务端主动推送的对局进度，让 bot 不依赖本地墙钟判断结束时刻
// （墙钟与 tick 漂移会让 bot 提前或延后结束，压低有效对局吞吐的准确性）。
type SessionProgress struct {
	Tick       uint32
	TotalTicks uint32
	ElapsedMs  uint32
}

// Encode 序列化。
func (m *SessionProgress) Encode() []byte {
	w := NewBinWriter(16)
	w.U32(m.Tick).U32(m.TotalTicks).U32(m.ElapsedMs)
	return w.Bytes()
}

// DecodeSessionProgress 反序列化。
func DecodeSessionProgress(b []byte) (SessionProgress, error) {
	r := NewBinReader(b)
	var m SessionProgress
	m.Tick = r.U32()
	m.TotalTicks = r.U32()
	m.ElapsedMs = r.U32()
	return m, r.Err()
}
