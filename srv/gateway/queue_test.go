package gateway

import (
	"testing"

	"aiplayer/swarm/internal/protocol"
)

// TestCheckQueueRequest 守住「协议字段必须被处理」这条线。
//
// 回归背景：EnterQueueReq 里的 Mode 与 Party 曾经被完全忽略——客户端请求组队，
// 服务端静默按单人排队处理并且回 OK。这类「不报错的错误」比报错难查一个数量级，
// 所以校验逻辑单独抽成函数并在这里钉死。
func TestCheckQueueRequest(t *testing.T) {
	const serverMode = "ranked5v5"

	cases := []struct {
		name string
		req  protocol.EnterQueueReq
		want string // 期望的错误码，空表示应当通过
	}{
		{"空 mode 走默认", protocol.EnterQueueReq{}, ""},
		{"显式同 mode", protocol.EnterQueueReq{Mode: serverMode}, ""},
		{"只带自己算单人", protocol.EnterQueueReq{Mode: serverMode, Party: []uint64{7}}, ""},
		{"不认识的 mode", protocol.EnterQueueReq{Mode: "casual5v5"}, protocol.CodeModeUnsupported},
		{"大小写不同不算同一个", protocol.EnterQueueReq{Mode: "Ranked5v5"}, protocol.CodeModeUnsupported},
		{"两人队", protocol.EnterQueueReq{Party: []uint64{1, 2}}, protocol.CodePartyUnsupported},
		{"五人队", protocol.EnterQueueReq{Mode: serverMode, Party: []uint64{1, 2, 3, 4, 5}}, protocol.CodePartyUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := c.req
			got := CheckQueueRequest(serverMode, &req)
			if c.want == "" {
				if got != nil {
					t.Fatalf("期望通过，实际被拒：%s: %s", got.Code, got.Message)
				}
				return
			}
			if got == nil {
				t.Fatalf("期望错误码 %s，实际通过（这个参数被静默忽略了）", c.want)
			}
			if got.Code != c.want {
				t.Fatalf("期望错误码 %s，实际 %s", c.want, got.Code)
			}
			if got.Message == "" {
				t.Fatal("拒绝时必须带上可读的原因，否则客户端不知道怎么办")
			}
		})
	}
}
