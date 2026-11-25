// Package protocol 定义 bot 与被测系统之间的应用层消息。
//
// 分为两段（对应知识清单 A5 / A6 / D1）：
//
//	控制面 gateway：WebSocket 上的 JSON 消息（登录、排队、匹配结果）——低频、易调试
//	数据面 room   ：裸 TCP 上的定长二进制消息（输入、快照、结算）——10Hz、省 CPU 与带宽
//
// 两段共用 wire.Envelope 的「消息号 + 序列号」语义。
package protocol

import (
	"encoding/json"
	"fmt"

	"aiplayer/swarm/internal/wire"
)

// ProtoVersion 是当前协议版本。bot 与服务器版本不一致时握手直接失败，
// 避免「改了字段没改版本」造成的哑失败。
const ProtoVersion = 1

// 控制面消息号（0x01xx）。请求/响应用连续的偶数/奇数号，便于扫日志。
const (
	MsgHello        uint16 = 0x0101 // c -> s 握手
	MsgWelcome      uint16 = 0x0102 // s -> c 握手应答
	MsgLoginReq     uint16 = 0x0103 // c -> s
	MsgLoginResp    uint16 = 0x0104 // s -> c
	MsgEnterQueue   uint16 = 0x0105 // c -> s
	MsgEnterQueueOK uint16 = 0x0106 // s -> c
	MsgCancelQueue  uint16 = 0x0107 // c -> s
	MsgCancelOK     uint16 = 0x0108 // s -> c
	MsgMatchFound   uint16 = 0x0109 // s -> c 匹配成功推进
	MsgPing         uint16 = 0x010A // c -> s
	MsgPong         uint16 = 0x010B // s -> c
	MsgError        uint16 = 0x010C // s -> c
	MsgBye          uint16 = 0x010D // 双向主动断开
)

// 数据面消息号（0x02xx）。
const (
	MsgRoomJoinReq     uint16 = 0x0201 // c -> s
	MsgRoomJoinResp    uint16 = 0x0202 // s -> c
	MsgInput           uint16 = 0x0203 // c -> s
	MsgInputAck        uint16 = 0x0204 // s -> c
	MsgSnapshot        uint16 = 0x0205 // s -> c 10Hz 状态同步
	MsgSettle          uint16 = 0x0206 // s -> c 结算
	MsgRoomLeave       uint16 = 0x0207 // c -> s
	MsgRoomPing        uint16 = 0x0208 // c -> s
	MsgRoomPong        uint16 = 0x0209 // s -> c
	MsgReconnectReq    uint16 = 0x020A // c -> s 断线重连
	MsgReconnectResp   uint16 = 0x020B // s -> c
	MsgRoomClose       uint16 = 0x020C // s -> c 房间回收
	MsgSessionProgress uint16 = 0x020D // s -> c 进度广播（避免 bot 只依赖本地计时）
)

var msgNames = map[uint16]string{
	MsgHello:        "Hello",
	MsgWelcome:      "Welcome",
	MsgLoginReq:     "LoginReq",
	MsgLoginResp:    "LoginResp",
	MsgEnterQueue:   "EnterQueue",
	MsgEnterQueueOK: "EnterQueueOK",
	MsgCancelQueue:  "CancelQueue",
	MsgCancelOK:     "CancelOK",
	MsgMatchFound:   "MatchFound",
	MsgPing:         "Ping",
	MsgPong:         "Pong",
	MsgError:        "Error",
	MsgBye:          "Bye",

	MsgRoomJoinReq:     "RoomJoinReq",
	MsgRoomJoinResp:    "RoomJoinResp",
	MsgInput:           "Input",
	MsgInputAck:        "InputAck",
	MsgSnapshot:        "Snapshot",
	MsgSettle:          "Settle",
	MsgRoomLeave:       "RoomLeave",
	MsgRoomPing:        "RoomPing",
	MsgRoomPong:        "RoomPong",
	MsgReconnectReq:    "ReconnectReq",
	MsgReconnectResp:   "ReconnectResp",
	MsgRoomClose:       "RoomClose",
	MsgSessionProgress: "SessionProgress",
}

// MsgName 返回消息号的可读名，未知号返回十六进制。
func MsgName(id uint16) string {
	if n, ok := msgNames[id]; ok {
		return n
	}
	return fmt.Sprintf("0x%04x", id)
}

// Error 是控制面统一的错误载荷。
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Code + ": " + e.Message
}

// 控制面错误码（写进日志与结果文件，便于按类聚合）。
const (
	CodeBadProto      = "bad_proto"
	CodeBadSign       = "bad_sign"
	CodeExpired       = "expired"
	CodeRateLimited   = "rate_limited"
	CodeBadToken      = "bad_token"
	CodeRoomFull      = "room_full"
	CodeRoomClosed    = "room_closed"
	CodeAlreadyQueued = "already_queued"
	CodeUnknown       = "unknown"
	// CodeModeUnsupported / CodePartyUnsupported 是「协议里有字段、服务端还没实现」
	// 的显式拒绝码。
	//
	// 为什么要专门加这两个码：EnterQueueReq 里早就有 Mode 和 Party 字段，
	// 但服务端一直**静默忽略**——客户端点「组队匹配」会被当成单人排队并且不报错，
	// 这比直接报错恶劣得多：用户以为组队成功了，直到进了房间才发现队友不在。
	// 协议字段既然对外暴露了，就必须要么实现、要么明确拒绝。
	CodeModeUnsupported  = "mode_unsupported"
	CodePartyUnsupported = "party_unsupported"
)

// Marshal 把消息体编码为 WebSocket 形态的信封。
func Marshal(msgID uint16, seq uint32, v any) ([]byte, error) {
	var body []byte
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("protocol: marshal msg 0x%04x: %w", msgID, err)
		}
		body = b
	}
	return wire.EncodeEnvelope(msgID, seq, body), nil
}

// Envelope 是一次解包结果：消息号、序列号与原始消息体。
type Envelope struct {
	MsgID uint16
	Seq   uint32
	Body  []byte
}

// Unmarshal 拆信封并把 JSON 消息体解到 v（v 为 nil 时只拆信封）。
func Unmarshal(b []byte, v any) (Envelope, error) {
	msgID, seq, body, err := wire.DecodeEnvelope(b)
	if err != nil {
		return Envelope{}, err
	}
	env := Envelope{MsgID: msgID, Seq: seq, Body: body}
	if v != nil && len(body) > 0 {
		if err := json.Unmarshal(body, v); err != nil {
			return env, fmt.Errorf("protocol: unmarshal msg %s: %w", MsgName(msgID), err)
		}
	}
	return env, nil
}
