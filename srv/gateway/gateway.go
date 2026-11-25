// Package gateway 实现被测系统的接入层：WebSocket 长连接上的握手、鉴权、排队与匹配推进。
//
// 对应知识清单 A5（WebSocket）、A8（应用层心跳超时判定）、D6（会话与重连）、
// 以及 §2.8 里 match_wait_seconds 这个指标的采集位置。
//
// 接入层是「长连接 + 有状态会话」，所以不能拿 k6 之类的 HTTP 工具压——
// 那正是本项目要自己写 bot 的原因（§1.3）。
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"aiplayer/swarm/internal/metrics"
	"aiplayer/swarm/internal/obs"
	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/internal/ws"
	"aiplayer/swarm/srv/account"
	"aiplayer/swarm/srv/match"
)

// Config 是接入层配置。
type Config struct {
	Addr string `yaml:"addr" json:"addr"`
	// ServerName 会出现在握手应答里，压测时用来确认连的是哪个环境（防止压错集群）。
	ServerName string `yaml:"server_name" json:"server_name"`
	// ClientSecret 是登录签名用的共享密钥。生产环境应当由注册流程下发，
	// 这里是自建压测环境，客户端（bot）与服务端共用同一份开发密钥。
	ClientSecret string `yaml:"client_secret" json:"client_secret"`
	// SessionTTLMinutes 是登录 token 有效期。
	SessionTTLMinutes int `yaml:"session_ttl_minutes" json:"session_ttl_minutes"`
	// HeartbeatTimeoutMs 是应用层心跳超时（A8）。
	HeartbeatTimeoutMs int `yaml:"heartbeat_timeout_ms" json:"heartbeat_timeout_ms"`
	// HandshakeTimeoutMs 是握手阶段（Hello/Login）的总时限。
	HandshakeTimeoutMs int `yaml:"handshake_timeout_ms" json:"handshake_timeout_ms"`
	// MaxMessageBytes 是单条 WS 消息上限。
	MaxMessageBytes int64 `yaml:"max_message_bytes" json:"max_message_bytes"`
}

// Defaults 填充默认值。
func (c *Config) Defaults() {
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	if c.ServerName == "" {
		c.ServerName = "swarm-sut/1"
	}
	if c.ClientSecret == "" {
		c.ClientSecret = "dsh-swarm-dev-secret"
	}
	if c.SessionTTLMinutes <= 0 {
		c.SessionTTLMinutes = 30
	}
	if c.HeartbeatTimeoutMs <= 0 {
		c.HeartbeatTimeoutMs = 15000
	}
	if c.HandshakeTimeoutMs <= 0 {
		c.HandshakeTimeoutMs = 5000
	}
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = 64 << 10
	}
}

// Server 是接入层服务器。
type Server struct {
	cfg  Config
	o    *obs.Server
	acct *account.Service
	mm   *match.Matchmaker
	reg  *metrics.Registry

	up         ws.Upgrader
	httpSrv    *http.Server
	addr       string
	conns      atomic.Int64
	handshakes atomic.Int64
}

// New 创建接入层服务器。
func New(cfg Config, o *obs.Server, reg *metrics.Registry, mm *match.Matchmaker) *Server {
	cfg.Defaults()
	acct := account.New([]byte(cfg.ClientSecret), time.Duration(cfg.SessionTTLMinutes)*time.Minute)
	s := &Server{
		cfg:  cfg,
		o:    o,
		acct: acct,
		mm:   mm,
		reg:  reg,
		up: ws.Upgrader{
			HandshakeTimeout: time.Duration(cfg.HandshakeTimeoutMs) * time.Millisecond,
			MaxMessageSize:   cfg.MaxMessageBytes,
			ReadBufferSize:   4096,
			WriteBufferSize:  4096,
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.Handle("/metrics", reg.Handler())
	mux.HandleFunc("/debug/state", s.handleState)
	s.httpSrv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.Background() },
	}
	return s
}

// AccountService 暴露账号服务（房间结算回写 MMR 用）。
func (s *Server) AccountService() *account.Service { return s.acct }

// Addr 返回监听地址。
func (s *Server) Addr() string {
	if s.addr != "" {
		return s.addr
	}
	return s.httpSrv.Addr
}

// Start 在 addr 上监听（addr 允许 ":0" 取随机端口），然后后台提供服务。
// 返回实际监听地址，便于测试与多实例。
func (s *Server) Start(addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("gateway: 监听 %s: %w", addr, err)
	}
	s.addr = ln.Addr().String()
	s.httpSrv.Addr = s.addr
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// 服务退出不是致命错误，Shutdown 也会走到这里。
		}
	}()
	return s.addr, nil
}

// ListenAndServe 启动 HTTP 服务。
func (s *Server) ListenAndServe() error {
	return s.httpSrv.ListenAndServe()
}

// Shutdown 优雅退出（B12）：先停止接收新连接，再给在途请求一个超时窗口。
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	accs, sess := s.acct.Size()
	state := map[string]any{
		"server":        s.cfg.ServerName,
		"connections":   s.conns.Load(),
		"handshakes":    s.handshakes.Load(),
		"accounts":      accs,
		"sessions":      sess,
		"logins":        s.acct.LoginCount(),
		"login_errors":  s.acct.FailureCount(),
		"queue_pending": s.mm.Pending(),
		"matches":       s.mm.Matches(),
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(state)
}

// connState 是一条连接上的会话状态。
type connState struct {
	c *ws.Conn
	o *obs.Server

	mu       sync.Mutex
	ticket   *match.Ticket
	account  *account.Account
	token    string
	nonce    string
	welcome  bool
	loggedIn bool

	closed chan struct{}
	once   sync.Once
}

func (st *connState) close() { st.once.Do(func() { close(st.closed) }) }

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.up.Upgrade(w, r)
	if err != nil {
		// Upgrade 已经写过响应了，这里只记日志意图。
		return
	}
	s.conns.Add(1)
	defer s.conns.Add(-1)
	defer conn.Close()

	st := &connState{c: conn, o: s.o, closed: make(chan struct{})}
	defer st.close()
	defer func() {
		// 连接结束时把还在排队的票据撤掉，否则队列里会留一堆「幽灵玩家」，
		// 它们永远不会被匹配到，却一直占着队伍（这是压测里最常见的 queue 泄漏）。
		st.mu.Lock()
		if st.ticket != nil {
			s.mm.Cancel(st.ticket.ID)
			st.ticket = nil
		}
		st.mu.Unlock()
	}()

	// 握手阶段：限时，避免空连接占 fd。
	_ = conn.SetReadDeadline(time.Now().Add(time.Duration(s.cfg.HandshakeTimeoutMs) * time.Millisecond))
	if err := s.doHandshake(st); err != nil {
		return
	}
	s.handshakes.Add(1)

	// 主循环：应用层心跳判定靠读超时（A8）。
	hb := time.Duration(s.cfg.HeartbeatTimeoutMs) * time.Millisecond
	for {
		_ = conn.SetReadDeadline(time.Now().Add(hb))
		op, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if op != ws.BinaryMessage {
			continue
		}
		if err := s.dispatch(st, payload); err != nil {
			if errors.Is(err, errCloseConn) {
				return
			}
		}
	}
}

var errCloseConn = errors.New("gateway: close connection")

// doHandshake 处理 Hello → Welcome → Login → LoginResp。
func (s *Server) doHandshake(st *connState) error {
	_, payload, err := st.c.ReadMessage()
	if err != nil {
		return err
	}
	var hello protocol.Hello
	env, err := protocol.Unmarshal(payload, &hello)
	if err != nil {
		return err
	}
	if env.MsgID != protocol.MsgHello {
		return s.sendError(st, env.Seq, protocol.CodeBadProto, "expected Hello")
	}
	if hello.ProtoVersion != protocol.ProtoVersion {
		return s.sendError(st, env.Seq, protocol.CodeBadProto,
			fmt.Sprintf("proto version mismatch: client=%d server=%d", hello.ProtoVersion, protocol.ProtoVersion))
	}

	nonceBytes := make([]byte, 8)
	_, _ = rand.Read(nonceBytes)
	st.nonce = hex.EncodeToString(nonceBytes)

	welcome := protocol.Welcome{
		OK:           true,
		ProtoVersion: protocol.ProtoVersion,
		ServerName:   s.cfg.ServerName,
		ServerNowMs:  time.Now().UnixMilli(),
		Nonce:        st.nonce,
	}
	body, _ := protocol.Marshal(protocol.MsgWelcome, env.Seq, welcome)
	if err := st.c.WriteMessage(ws.BinaryMessage, body); err != nil {
		return err
	}
	st.welcome = true

	_, payload, err = st.c.ReadMessage()
	if err != nil {
		return err
	}
	var login protocol.LoginReq
	env, err = protocol.Unmarshal(payload, &login)
	if err != nil {
		return err
	}
	if env.MsgID != protocol.MsgLoginReq {
		return s.sendError(st, env.Seq, protocol.CodeBadProto, "expected LoginReq")
	}
	if login.Nonce != st.nonce {
		s.o.LoginErrors.With("bad_nonce").Inc()
		s.o.LoginTotal.With("fail").Inc()
		return s.sendError(st, env.Seq, protocol.CodeBadSign, "nonce mismatch")
	}

	sess, acc, err := s.acct.Login(login.AccountID, login.ClientTsMs, login.Nonce, login.Sign, login.InitMMR)
	if err != nil {
		code := protocol.CodeBadSign
		if errors.Is(err, account.ErrClockSkew) {
			code = protocol.CodeExpired
		}
		s.o.LoginErrors.With(code).Inc()
		s.o.LoginTotal.With("fail").Inc()
		return s.sendError(st, env.Seq, code, err.Error())
	}

	st.mu.Lock()
	st.account = acc
	st.token = sess.Token
	st.loggedIn = true
	st.mu.Unlock()

	s.o.LoginTotal.With("ok").Inc()
	resp := protocol.LoginResp{
		OK:          true,
		Token:       sess.Token,
		PlayerID:    acc.PlayerID,
		MMR:         acc.MMR,
		ExpiresAtMs: sess.ExpiresAt.UnixMilli(),
	}
	body, _ = protocol.Marshal(protocol.MsgLoginResp, env.Seq, resp)
	return st.c.WriteMessage(ws.BinaryMessage, body)
}

func (s *Server) dispatch(st *connState, payload []byte) error {
	env, err := protocol.Unmarshal(payload, nil)
	if err != nil {
		return nil
	}
	switch env.MsgID {
	case protocol.MsgEnterQueue:
		var req protocol.EnterQueueReq
		if _, err := protocol.Unmarshal(payload, &req); err != nil {
			return s.sendError(st, env.Seq, protocol.CodeBadProto, err.Error())
		}
		// 协议里有 Mode 与 Party 字段，就必须处理它们——静默忽略比报错恶劣得多。
		if perr := CheckQueueRequest(s.mm.Mode(), &req); perr != nil {
			return s.sendError(st, env.Seq, perr.Code, perr.Message)
		}
		return s.enterQueue(st, env.Seq)
	case protocol.MsgCancelQueue:
		st.mu.Lock()
		t := st.ticket
		st.ticket = nil
		st.mu.Unlock()
		ok := false
		if t != nil {
			ok = s.mm.Cancel(t.ID)
		}
		body, _ := protocol.Marshal(protocol.MsgCancelOK, env.Seq, protocol.CancelQueueResp{OK: ok})
		return st.c.WriteMessage(ws.BinaryMessage, body)
	case protocol.MsgPing:
		var p protocol.Ping
		if _, err := protocol.Unmarshal(payload, &p); err != nil {
			return nil
		}
		body, _ := protocol.Marshal(protocol.MsgPong, env.Seq, protocol.Pong{
			ClientNowMs: p.ClientNowMs,
			ServerNowMs: time.Now().UnixMilli(),
		})
		return st.c.WriteMessage(ws.BinaryMessage, body)
	case protocol.MsgBye:
		return errCloseConn
	default:
		// 未知消息号：容错忽略，保证协议演进时老客户端不会被打断。
		return nil
	}
}

// CheckQueueRequest 校验入队请求。
//
// 单独抽成函数是为了能直接单测（不需要起整个 gateway），也让「哪些参数被支持」
// 这件事集中在一个地方，而不是散在 dispatch 里靠读代码猜。
//
// 背景：EnterQueueReq 一直有 Mode 与 Party 两个字段，但服务端过去**完全忽略**它们。
// 客户端点「组队匹配」会被静默当成单人排队，直到进了房间才发现队友不在——
// 这类「协议有字段、实现没跟上、还不报错」的问题最难排查。
func CheckQueueRequest(serverMode string, req *protocol.EnterQueueReq) *protocol.Error {
	mode := req.Mode
	if mode == "" {
		mode = serverMode
	}
	if mode != serverMode {
		return &protocol.Error{
			Code:    protocol.CodeModeUnsupported,
			Message: fmt.Sprintf("服务端只支持模式 %q，收到 %q", serverMode, req.Mode),
		}
	}
	// 只放自己（或留空）等价于单人排队，允许；真的带队友就明确拒绝。
	if len(req.Party) > 1 {
		return &protocol.Error{
			Code:    protocol.CodePartyUnsupported,
			Message: "组队匹配尚未实现：当前只接受单人排队，party 请留空或只包含自己",
		}
	}
	return nil
}

// enterQueue 把玩家放进匹配队列并回执。mode 已在 CheckQueueRequest 里校验过。
func (s *Server) enterQueue(st *connState, seq uint32) error {
	st.mu.Lock()
	acc := st.account
	if acc == nil {
		st.mu.Unlock()
		return s.sendError(st, seq, protocol.CodeBadToken, "not logged in")
	}
	if st.ticket != nil {
		st.mu.Unlock()
		return s.sendError(st, seq, protocol.CodeAlreadyQueued, "already queued")
	}
	t := s.mm.Enqueue(acc.PlayerID, acc.MMR)
	st.ticket = t
	st.mu.Unlock()

	body, _ := protocol.Marshal(protocol.MsgEnterQueueOK, seq, protocol.EnterQueueResp{
		OK:       true,
		Ticket:   t.ID,
		Bucket:   0,
		Position: s.mm.Pending(),
	})
	if err := st.c.WriteMessage(ws.BinaryMessage, body); err != nil {
		return err
	}

	// 等匹配结果：独立 goroutine，绝不阻塞读循环（否则连接上就再也收不到取消排队）。
	go func() {
		select {
		case a := <-t.Result:
			st.mu.Lock()
			st.ticket = nil
			st.mu.Unlock()
			s.pushMatch(st, a)
		case <-st.closed:
			return
		}
	}()
	return nil
}

func (s *Server) pushMatch(st *connState, a match.Assignment) {
	if a.Err != nil {
		_ = s.sendError(st, 0, protocol.CodeUnknown, a.Err.Error())
		return
	}
	mf := protocol.MatchFound{
		MatchID:       a.MatchID,
		Mode:          "ranked5v5",
		RoomAddr:      a.RoomAddr,
		RoomToken:     a.RoomToken,
		Players:       a.Players,
		MMRSpread:     a.MMRSpread,
		WaitMs:        a.WaitMs,
		MatchMs:       a.MatchMs,
		AllocMicros:   a.AllocMicros,
		Seed:          a.Seed,
		TickRateMs:    a.TickRateMs,
		DurationTicks: a.DurationTicks,
		ServerNowMs:   time.Now().UnixMilli(),
	}
	body, err := protocol.Marshal(protocol.MsgMatchFound, 0, mf)
	if err != nil {
		return
	}
	_ = st.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := st.c.WriteMessage(ws.BinaryMessage, body); err != nil {
		st.close()
	}
}

func (s *Server) sendError(st *connState, seq uint32, code, msg string) error {
	body, _ := protocol.Marshal(protocol.MsgError, seq, protocol.Error{Code: code, Message: msg})
	_ = st.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = st.c.WriteMessage(ws.BinaryMessage, body)
	return nil
}
