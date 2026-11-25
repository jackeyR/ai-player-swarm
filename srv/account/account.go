// Package account 负责账号、登录鉴权与会话 token。
//
// 对应知识清单 A12（TLS/握手开销之外的另一半：鉴权本身的成本）、D6（会话与重连）、
// 以及 §2.9「bot 的 MMR 分布是人为的」这句诚实标注的落地处：MMR 存在账号上，
// 由对局结果驱动 Elo-lite 更新，bot 的初始值来自 persona 的 MMR 分布。
package account

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// 错误定义。
var (
	// ErrBadSign 表示签名不匹配（密钥错、字段错或伪造）。
	ErrBadSign = errors.New("account: bad signature")
	// ErrExpired 表示 token 过期。
	ErrExpired = errors.New("account: session expired")
	// ErrUnknownToken 表示 token 不存在。
	ErrUnknownToken = errors.New("account: unknown session token")
	// ErrClockSkew 表示客户端时间戳与服务器偏差过大（防重放）。
	ErrClockSkew = errors.New("account: client clock skew too large")
)

// DefaultMaxSkew 是允许的客户端时间偏差。
const DefaultMaxSkew = 5 * time.Minute

// Account 是一个玩家账号。
type Account struct {
	ID       string
	PlayerID uint64
	MMR      int
	Created  time.Time
	Matches  uint64
	Wins     uint64
	Losses   uint64
}

// Session 是一次登录会话。
type Session struct {
	Token     string
	AccountID string
	PlayerID  uint64
	ExpiresAt time.Time
}

// Service 是账号服务。全部内存态，进程重启即清空（压测场景不需要持久化）。
type Service struct {
	mu       sync.RWMutex
	byID     map[string]*Account
	byPlayer map[uint64]*Account
	sessions map[string]*Session
	// accountToken 记录「每个账号当前有效的那一个 token」。
	//
	// 为什么需要它：登出/重登要让旧 token 立刻失效，过去靠「每次登录遍历整个
	// sessions map，删掉同账号与过期的」。那是 O(全表) 且全程持写锁——
	// 3000 个 bot 同时登录时，所有登录被这把锁串行化，登录 p99 会随账号数平方增长。
	// 有了这张索引，同账号旧 token 的失效是 O(1)。
	accountToken map[string]string
	// sweepAt 是下一次全表清理的触发阈值（摊还清理，见 Login）。
	sweepAt    int
	secret     []byte
	sessionTTL time.Duration
	maxSkew    time.Duration
	nextID     atomic.Uint64
	now        func() time.Time
	logins     atomic.Int64
	failures   atomic.Int64
}

// New 创建账号服务。secret 是登录签名与 token 生成共用的密钥。
func New(secret []byte, sessionTTL time.Duration) *Service {
	if len(secret) == 0 {
		secret = []byte("dsh-swarm-dev-secret")
	}
	if sessionTTL <= 0 {
		sessionTTL = 30 * time.Minute
	}
	return &Service{
		byID:         make(map[string]*Account, 4096),
		byPlayer:     make(map[uint64]*Account, 4096),
		sessions:     make(map[string]*Session, 4096),
		accountToken: make(map[string]string, 4096),
		sweepAt:      4096,
		secret:       secret,
		sessionTTL:   sessionTTL,
		maxSkew:      DefaultMaxSkew,
		now:          time.Now,
	}
}

// sweepLocked 清掉已过期的 session。调用方必须持有写锁。
//
// 只在 sessions 表长到阈值时才跑一次（见 Login），把 O(n) 的成本摊到 n 次登录上。
// 不做这一步也不会错——Validate 会拒掉过期 token——但表会一直涨。
func (s *Service) sweepLocked(now time.Time) {
	for tok, sess := range s.sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.sessions, tok)
			if cur, ok := s.accountToken[sess.AccountID]; ok && cur == tok {
				delete(s.accountToken, sess.AccountID)
			}
		}
	}
}

// SetClock 注入时钟，测试用。
func (s *Service) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// SetMaxSkew 调整允许的客户端时钟偏差。
func (s *Service) SetMaxSkew(d time.Duration) { s.maxSkew = d }

// Sign 计算登录签名：hex(HMAC-SHA256(secret, accountID|tsMs|nonce))。
//
// bot 侧用同一个函数算，服务器用同一个函数验——这条「同一个函数」是
// 协议契约测试（K2）能成立的前提。
func Sign(secret []byte, accountID string, tsMs int64, nonce string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(accountID))
	m.Write([]byte("|"))
	m.Write([]byte(strconv.FormatInt(tsMs, 10)))
	m.Write([]byte("|"))
	m.Write([]byte(nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// Login 校验签名并签发 token。账号不存在时自动注册（压测需要大量一次性账号）。
//
// initMMR <= 0 时使用 1500。
func (s *Service) Login(accountID string, tsMs int64, nonce, sign string, initMMR int) (*Session, *Account, error) {
	if accountID == "" {
		s.failures.Add(1)
		return nil, nil, ErrBadSign
	}
	// 时间偏差检查放在验签之前：签名里含时间戳，先挡住明显过期的重放。
	serverMs := s.now().UnixMilli()
	skew := time.Duration(serverMs-tsMs) * time.Millisecond
	if skew < 0 {
		skew = -skew
	}
	if skew > s.maxSkew {
		s.failures.Add(1)
		return nil, nil, ErrClockSkew
	}
	want := Sign(s.secret, accountID, tsMs, nonce)
	if subtle.ConstantTimeCompare([]byte(want), []byte(sign)) != 1 {
		s.failures.Add(1)
		return nil, nil, ErrBadSign
	}

	s.mu.Lock()
	acc, ok := s.byID[accountID]
	if !ok {
		if initMMR <= 0 {
			initMMR = 1500
		}
		acc = &Account{
			ID:       accountID,
			PlayerID: s.nextID.Add(1),
			MMR:      initMMR,
			Created:  s.now(),
		}
		s.byID[accountID] = acc
		s.byPlayer[acc.PlayerID] = acc
	}

	tokBytes := make([]byte, 16)
	if _, err := rand.Read(tokBytes); err != nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("account: 生成 token: %w", err)
	}
	sess := &Session{
		Token:     hex.EncodeToString(tokBytes),
		AccountID: accountID,
		PlayerID:  acc.PlayerID,
		ExpiresAt: s.now().Add(s.sessionTTL),
	}
	// 同一账号重新登录时，旧 token 立刻失效——O(1)，不再遍历整张表。
	if old, ok := s.accountToken[accountID]; ok {
		delete(s.sessions, old)
	}
	s.sessions[sess.Token] = sess
	s.accountToken[accountID] = sess.Token

	// 过期会话改摊还清理：表长到阈值才全表扫一次，成本摊到 n 次登录上。
	// 不清理也不会错（Validate 会拒掉过期 token），但表会无界增长。
	if len(s.sessions) >= s.sweepAt {
		s.sweepLocked(s.now())
		s.sweepAt = len(s.sessions)*2 + 64
	}
	s.mu.Unlock()

	s.logins.Add(1)
	return sess, acc, nil
}

// Validate 校验 token 并返回账号快照。
func (s *Service) Validate(token string) (*Account, error) {
	s.mu.RLock()
	sess, ok := s.sessions[token]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrUnknownToken
	}
	if s.now().After(sess.ExpiresAt) {
		return nil, ErrExpired
	}
	s.mu.RLock()
	acc, ok := s.byPlayer[sess.PlayerID]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrUnknownToken
	}
	// 返回副本，调用方拿到的快照不会被并发更新改掉。
	cp := *acc
	return &cp, nil
}

// Account 返回账号快照。
func (s *Service) Account(playerID uint64) (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.byPlayer[playerID]
	if !ok {
		return nil, false
	}
	cp := *a
	return &cp, true
}

// MMR 返回玩家当前 MMR。
func (s *Service) MMR(playerID uint64) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a, ok := s.byPlayer[playerID]; ok {
		return a.MMR
	}
	return 0
}

// MatchOutcome 是结算后要回写到账号的结果。
type MatchOutcome struct {
	PlayerID uint64
	Win      bool
	TeamMMR  int // 对手队伍平均 MMR
}

// ApplyMatch 用 Elo-lite 更新 MMR，返回每个玩家的新 MMR。
//
// K 因子随局数衰减：新号波动大、老号稳定，这是真实匹配系统的常见做法，
// 也让「碾压局」在压测中能自然出现（D11 匹配质量指标的数据来源）。
func (s *Service) ApplyMatch(outcomes []MatchOutcome) map[uint64]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := make(map[uint64]int, len(outcomes))
	for _, o := range outcomes {
		a, ok := s.byPlayer[o.PlayerID]
		if !ok {
			continue
		}
		k := 32.0
		if a.Matches > 30 {
			k = 16.0
		}
		expected := 1 / (1 + math.Pow(10, float64(o.TeamMMR-a.MMR)/400))
		score := 0.0
		if o.Win {
			score = 1
		}
		delta := int(math.Round(k * (score - expected)))
		a.MMR += delta
		if a.MMR < 0 {
			a.MMR = 0
		}
		if a.MMR > 4000 {
			a.MMR = 4000
		}
		a.Matches++
		if o.Win {
			a.Wins++
		} else {
			a.Losses++
		}
		res[o.PlayerID] = a.MMR
	}
	return res
}

// LoginCount 返回成功登录次数。
func (s *Service) LoginCount() int64 { return s.logins.Load() }

// FailureCount 返回登录失败次数。
func (s *Service) FailureCount() int64 { return s.failures.Load() }

// Size 返回账号数与会话数。
func (s *Service) Size() (accounts, sessions int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID), len(s.sessions)
}
