package protocol

// 控制面（WebSocket + JSON）的消息体定义。
//
// 保留字段与不兼容变更靠 ProtoVersion 拦住；新增可选字段靠 omitempty 向后兼容。

// Hello 是 bot 连上 gateway 后的第一条消息。
type Hello struct {
	ProtoVersion int    `json:"proto_version"`
	ClientID     string `json:"client_id"` // 形如 "runner-1/bot-42"
	Platform     string `json:"platform"`  // "windows" / "linux" / "headless"
	Seed         uint64 `json:"seed"`      // bot 的随机种子，服务端可回放用
	ClientBuild  string `json:"client_build,omitempty"`
}

// Welcome 是握手应答。Nonce 参与登录签名，防止重放（A12 / D6）。
type Welcome struct {
	OK           bool   `json:"ok"`
	ProtoVersion int    `json:"proto_version"`
	ServerName   string `json:"server_name"`
	ServerNowMs  int64  `json:"server_now_ms"`
	Nonce        string `json:"nonce"`
	Err          *Error `json:"err,omitempty"`
}

// LoginReq 登录请求。Sign = hex(HMAC-SHA256(accountSecret, accountID|clientTsMs|nonce))。
type LoginReq struct {
	AccountID  string `json:"account_id"`
	ClientTsMs int64  `json:"client_ts_ms"`
	Nonce      string `json:"nonce"`
	Sign       string `json:"sign"`
	InitMMR    int    `json:"init_mmr,omitempty"` // 新建账号的初始 MMR（bot 用人为分布，§2.9 诚实标注）
}

// LoginResp 登录应答。Token 是后续排队与进房的凭据。
type LoginResp struct {
	OK          bool   `json:"ok"`
	Token       string `json:"token,omitempty"`
	PlayerID    uint64 `json:"player_id,omitempty"`
	MMR         int    `json:"mmr,omitempty"`
	ExpiresAtMs int64  `json:"expires_at_ms,omitempty"`
	Err         *Error `json:"err,omitempty"`
}

// EnterQueueReq 进入匹配队列。
type EnterQueueReq struct {
	Mode  string   `json:"mode"`            // 默认 "ranked5v5"
	Party []uint64 `json:"party,omitempty"` // 组队成员（D14），单人留空
}

// EnterQueueResp 入队结果。Position 是该 MMR 桶内的排队位次，用于压测观察饥饿（D13）。
type EnterQueueResp struct {
	OK       bool   `json:"ok"`
	Ticket   string `json:"ticket,omitempty"`
	Bucket   int    `json:"bucket,omitempty"`
	Position int    `json:"position,omitempty"`
	Err      *Error `json:"err,omitempty"`
}

// CancelQueueResp 取消排队结果。
type CancelQueueResp struct {
	OK  bool   `json:"ok"`
	Err *Error `json:"err,omitempty"`
}

// MatchedPlayer 是匹配结果里的单个玩家。
type MatchedPlayer struct {
	PlayerID uint64 `json:"player_id"`
	MMR      int    `json:"mmr"`
	Team     uint8  `json:"team"` // 0 / 1
	Slot     uint8  `json:"slot"` // 房间内槽位 0..9
}

// MatchFound 是匹配成功推进。
//
// 分层延迟全部随包下发（D5：匹配与分配必须分开度量）：
//   - WaitMs       排队到匹配成功（match_wait_seconds）
//   - MatchMs      匹配器内部耗时
//   - AllocMicros  分配器找到房间耗时（room_alloc_seconds，微秒精度，
//     因为分配的正常值本来就在亚毫秒量级，用毫秒会把分布压成一根柱子）
type MatchFound struct {
	MatchID       string          `json:"match_id"`
	Mode          string          `json:"mode"`
	RoomAddr      string          `json:"room_addr"` // 房间服 TCP 地址
	RoomToken     string          `json:"room_token"`
	Players       []MatchedPlayer `json:"players"`
	MMRSpread     int             `json:"mmr_spread"`
	WaitMs        int64           `json:"wait_ms"`
	MatchMs       int64           `json:"match_ms"`
	AllocMicros   int64           `json:"alloc_micros"`
	Seed          uint64          `json:"seed"`
	TickRateMs    int             `json:"tick_rate_ms"`
	DurationTicks int             `json:"duration_ticks"`
	ServerNowMs   int64           `json:"server_now_ms"`
}

// Ping / Pong 用于应用层心跳（A8：应用层心跳比 TCP keepalive 更能判定「死连接」）。
type Ping struct {
	ClientNowMs int64  `json:"client_now_ms"`
	Seq         uint32 `json:"seq"`
}

// Pong 回带客户端时间戳，bot 侧直接得出 RTT。
type Pong struct {
	ClientNowMs int64 `json:"client_now_ms"`
	ServerNowMs int64 `json:"server_now_ms"`
}

// Bye 主动断开。
type Bye struct {
	Reason string `json:"reason"`
}
