package bot

// 本文件给「真客户端」（cmd/client）提供一套对外公开的会话 API。
//
// 为什么让真客户端复用 bot 的协议实现，而不是另写一份客户端：
//
//	压测工具最怕的失败模式是「bot 和服务器一起错」——两边由同一套错误的假设写出来，
//	测试全绿、线上崩。真人客户端是唯一能打破这种共谋的东西：它能连上、能打完一局，
//	协议层就等于被独立实现方验证过一次。
//
// 所以这里的每个方法都只是薄封装，**不含任何客户端专属逻辑**，
// 真正的字节编解码仍然是 wire/protocol 那一套。

import (
	"context"
	"fmt"
	"time"

	"aiplayer/swarm/internal/protocol"
)

// Player 是一个已登录的玩家会话（接入层连接 + 身份）。
type Player struct {
	cfg      Config
	gw       *gwClient
	account  string
	playerID uint64
	mmr      int
	token    string
	server   string
	proto    int
}

// ConnectPlayer 连接接入层，完成 Hello→Welcome 握手与签名登录。
//
// accountID 是账号名（不是昵称）：同一个名字重复登录会拿到同一个 playerID，
// 但会让旧 token 立刻失效——这正是「客户端崩溃后重连」需要的行为。
func ConnectPlayer(ctx context.Context, cfg Config, accountID string, initMMR int) (*Player, error) {
	cfg.Defaults()
	if accountID == "" {
		return nil, fmt.Errorf("client: 账号不能为空")
	}
	gcfg := gwConfig{
		URL:             cfg.GatewayURL,
		DialTimeout:     cfg.DialTimeout,
		ClientID:        "human/" + accountID,
		Platform:        cfg.Platform,
		Seed:            uint64(time.Now().UnixNano()),
		ProtoVersion:    protocol.ProtoVersion,
		MaxMessageBytes: cfg.MaxMessageBytes,
	}
	gw, err := dialGateway(ctx, gcfg)
	if err != nil {
		return nil, err
	}
	welcome, err := gw.handshake(ctx, gcfg)
	if err != nil {
		gw.close()
		return nil, err
	}
	login, err := gw.login(ctx, accountID, initMMR, welcome.Nonce, cfg.ClientSecret)
	if err != nil {
		gw.close()
		return nil, err
	}
	return &Player{
		cfg:      cfg,
		gw:       gw,
		account:  accountID,
		playerID: login.PlayerID,
		mmr:      login.MMR,
		token:    login.Token,
		server:   welcome.ServerName,
		proto:    welcome.ProtoVersion,
	}, nil
}

// Account 返回账号名。
func (p *Player) Account() string { return p.account }

// PlayerID 返回服务端分配的数字 ID。
func (p *Player) PlayerID() uint64 { return p.playerID }

// MMR 返回登录时的 MMR（进入新对局后会变，服务端不回推，重连才刷新）。
func (p *Player) MMR() int { return p.mmr }

// ServerName 返回握手时服务端自报的名字，用来确认连的是哪个环境。
func (p *Player) ServerName() string { return p.server }

// ProtoVersion 返回双方协商出的协议版本。
func (p *Player) ProtoVersion() int { return p.proto }

// EnterQueue 进入匹配队列。
func (p *Player) EnterQueue(ctx context.Context, mode string) error {
	_, err := p.gw.enterQueue(ctx, mode)
	if err != nil {
		return fmt.Errorf("client: 入队被拒: %w", err)
	}
	return nil
}

// CancelQueue 取消排队。已经匹配成功时服务端会返回 ok=false。
func (p *Player) CancelQueue(ctx context.Context) (bool, error) {
	resp, err := p.gw.cancelQueue(ctx)
	if err != nil {
		return false, err
	}
	return resp.OK, nil
}

// WaitMatch 等待匹配结果推送。
func (p *Player) WaitMatch(ctx context.Context, timeout time.Duration) (protocol.MatchFound, error) {
	return p.gw.waitMatch(ctx, timeout)
}

// Ping 发一次应用层心跳，返回 RTT。
func (p *Player) Ping(ctx context.Context) (time.Duration, error) { return p.gw.ping(ctx) }

// Done 在底层连接断开时关闭，供 UI 感知「掉线」。
func (p *Player) Done() <-chan struct{} { return p.gw.closed }

// Close 断开接入层连接。
func (p *Player) Close() error {
	p.gw.close()
	return nil
}

// RoomSession 是一次进房后的对局会话。
type RoomSession struct {
	rc   *roomClient
	join protocol.RoomJoinResp
	mf   protocol.MatchFound
}

// JoinMatch 按匹配结果连接房间服并完成进房握手。
func (p *Player) JoinMatch(ctx context.Context, mf protocol.MatchFound) (*RoomSession, error) {
	rc, err := dialRoom(ctx, mf.RoomAddr, p.cfg.DialTimeout)
	if err != nil {
		return nil, err
	}
	join, err := rc.join(ctx, mf.RoomToken, p.playerID, false)
	if err != nil {
		rc.close()
		return nil, err
	}
	if !join.OK {
		rc.close()
		return nil, fmt.Errorf("client: 进房被拒，错误码 %d", join.ErrCode)
	}
	return &RoomSession{rc: rc, join: join, mf: mf}, nil
}

// Slot 返回自己在房间里的槽位。
func (r *RoomSession) Slot() uint8 { return r.join.Slot }

// TickRateMs 返回房间的 tick 步长。
func (r *RoomSession) TickRateMs() int { return int(r.join.TickRateMs) }

// DurationTicks 返回一局的总 tick 数。
func (r *RoomSession) DurationTicks() uint32 { return r.join.DurationTicks }

// Match 返回这场对局的匹配信息（含全部玩家与分队）。
func (r *RoomSession) Match() protocol.MatchFound { return r.mf }

// Snapshots 返回快照通道。
func (r *RoomSession) Snapshots() <-chan protocol.Snapshot { return r.rc.snapshots }

// Acks 返回输入确认通道。
func (r *RoomSession) Acks() <-chan protocol.InputAck { return r.rc.acks }

// Settles 返回结算通道。
func (r *RoomSession) Settles() <-chan protocol.Settle { return r.rc.settles }

// TrySettle 非阻塞地取一次结算。
//
// 存在理由：服务端是「先发结算帧，再关连接」，两条消息几乎同时到达，
// 客户端 select 里 `<-Closed()` 与 `<-Settles()` 会同时就绪，随机选一个。
// 选中 Closed 就会把**已经打完的对局**误记成掉线，goodput 直接少算。
// 所以任何客户端在 Closed 分支里都应该先 TrySettle 补收一次。
func (r *RoomSession) TrySettle() (protocol.Settle, bool) {
	select {
	case st := <-r.rc.settles:
		return st, true
	default:
		return protocol.Settle{}, false
	}
}

// Progress 返回服务端主动推送的对局进度（每 10 tick 一条）。
func (r *RoomSession) Progress() <-chan protocol.SessionProgress { return r.rc.progress }

// Closed 在房间连接断开时关闭。
func (r *RoomSession) Closed() <-chan struct{} { return r.rc.closed }

// SendInput 发送一条输入，返回用于和服务端 ack 配对的序号。
func (r *RoomSession) SendInput(in protocol.Input) (uint32, error) { return r.rc.sendInput(in) }

// Ping 发房间层心跳。**即使挂机也必须发**：服务端按「多久没收到任何帧」判定死连接，
// 不发心跳的客户端会在 5 秒后被踢出房间。
func (r *RoomSession) Ping() error { return r.rc.ping() }

// Leave 主动离房（对手会立刻看到你掉线）。
func (r *RoomSession) Leave() error { return r.rc.leave() }

// Close 断开房间连接。
func (r *RoomSession) Close() error {
	r.rc.close()
	return nil
}
