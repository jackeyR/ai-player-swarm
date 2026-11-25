package account

import (
	"fmt"
	"testing"
	"time"
)

func newTestService(ttl time.Duration) (*Service, []byte) {
	secret := []byte("test-secret")
	s := New(secret, ttl)
	return s, secret
}

func doLogin(t *testing.T, s *Service, secret []byte, accountID string) *Session {
	t.Helper()
	ts := time.Now().UnixMilli()
	nonce := "n-" + accountID
	sess, _, err := s.Login(accountID, ts, nonce, Sign(secret, accountID, ts, nonce), 1500)
	if err != nil {
		t.Fatalf("登录 %s: %v", accountID, err)
	}
	return sess
}

// TestReloginInvalidatesOldToken 守住一条行为：同一账号重新登录后，旧 token 立即失效。
//
// 这条语义必须保留——它是「一个账号同时只有一个有效会话」的保证，
// 也是我们能把清理从「遍历全表」优化成「查索引」的前提。
func TestReloginInvalidatesOldToken(t *testing.T) {
	s, secret := newTestService(time.Hour)
	first := doLogin(t, s, secret, "alice")
	if _, err := s.Validate(first.Token); err != nil {
		t.Fatalf("首次登录的 token 应当有效: %v", err)
	}

	second := doLogin(t, s, secret, "alice")
	if second.Token == first.Token {
		t.Fatal("重新登录应当签发新 token")
	}
	if _, err := s.Validate(first.Token); err != ErrUnknownToken {
		t.Fatalf("旧 token 应当失效（ErrUnknownToken），实际 %v", err)
	}
	if _, err := s.Validate(second.Token); err != nil {
		t.Fatalf("新 token 应当有效: %v", err)
	}
	if n := len(s.sessions); n != 1 {
		t.Fatalf("同一账号只应保留 1 个 session，实际 %d 个（token 表在泄漏）", n)
	}
}

// TestOtherAccountsSessionsSurvive 确认优化没有误伤：A 重新登录不能影响 B 的会话。
func TestOtherAccountsSessionsSurvive(t *testing.T) {
	s, secret := newTestService(time.Hour)
	alice := doLogin(t, s, secret, "alice")
	bob := doLogin(t, s, secret, "bob")
	_ = doLogin(t, s, secret, "alice") // alice 重登

	if _, err := s.Validate(bob.Token); err != nil {
		t.Fatalf("bob 的会话被 alice 的重登误删了: %v", err)
	}
	if _, err := s.Validate(alice.Token); err != ErrUnknownToken {
		t.Fatalf("alice 的旧 token 应失效，实际 %v", err)
	}
}

// TestSweepDropsExpired 确认摊还清理真的会回收过期会话。
func TestSweepDropsExpired(t *testing.T) {
	s, secret := newTestService(time.Millisecond)
	for i := 0; i < 50; i++ {
		doLogin(t, s, secret, fmt.Sprintf("u%d", i))
	}
	time.Sleep(5 * time.Millisecond) // 让所有 token 过期

	// 触发一次清理：把阈值压低到当前表长，下一次登录就会 sweep。
	s.mu.Lock()
	s.sweepAt = len(s.sessions)
	s.mu.Unlock()

	doLogin(t, s, secret, "trigger")
	s.mu.RLock()
	n := len(s.sessions)
	s.mu.RUnlock()
	if n > 10 {
		t.Fatalf("过期会话没有被清理，表里仍有 %d 个", n)
	}
}

// TestExpiredTokenRejected 过期 token 必须直接拒（不依赖是否被清理过）。
func TestExpiredTokenRejected(t *testing.T) {
	s, secret := newTestService(20 * time.Millisecond)
	sess := doLogin(t, s, secret, "carol")
	time.Sleep(40 * time.Millisecond)
	if _, err := s.Validate(sess.Token); err != ErrExpired {
		t.Fatalf("过期 token 应返回 ErrExpired，实际 %v", err)
	}
}

// TestBadSignatureRejected 签名不对必须拒（顺带确认时钟偏差检查可被调小）。
func TestBadSignatureRejected(t *testing.T) {
	s, secret := newTestService(time.Hour)
	ts := time.Now().UnixMilli()
	if _, _, err := s.Login("dave", ts, "n", "deadbeef", 1500); err != ErrBadSign {
		t.Fatalf("错误签名应返回 ErrBadSign，实际 %v", err)
	}
	// 时钟偏差过大也要挡住（签名里含时间戳，防重放）。
	s2 := New(secret, time.Hour)
	if _, _, err := s2.Login("dave", ts-10*time.Minute.Milliseconds(), "n", Sign(secret, "dave", ts-600000, "n"), 1500); err != ErrClockSkew {
		t.Fatalf("时钟偏差过大应返回 ErrClockSkew，实际 %v", err)
	}
}

// TestMMREloUpdate 验证 Elo-lite：赢了涨、输了跌、有上下界。
func TestMMREloUpdate(t *testing.T) {
	s, secret := newTestService(time.Hour)
	doLogin(t, s, secret, "winner")
	doLogin(t, s, secret, "loser")

	winner := s.byID["winner"].PlayerID
	loser := s.byID["loser"].PlayerID
	before := s.MMR(winner)

	s.ApplyMatch([]MatchOutcome{
		{PlayerID: winner, Win: true, TeamMMR: 1500},
		{PlayerID: loser, Win: false, TeamMMR: 1500},
	})
	if s.MMR(winner) <= before {
		t.Fatalf("赢的人 MMR 应当上涨：%d -> %d", before, s.MMR(winner))
	}
	if s.MMR(loser) >= 1500 {
		t.Fatalf("输的人 MMR 应当下跌，实际 %d", s.MMR(loser))
	}
	// 上下界：连续赢 200 局不会突破 4000。
	for i := 0; i < 200; i++ {
		s.ApplyMatch([]MatchOutcome{{PlayerID: winner, Win: true, TeamMMR: 100}})
	}
	if got := s.MMR(winner); got > 4000 {
		t.Fatalf("MMR 上界失效：%d", got)
	}
}

// loginOldAlgorithm 复刻修复前的实现：每次登录遍历整张 sessions 表。
//
// 保留它在这里是为了让「为什么要改」这件事**可测量**，而不是只在 commit message 里说一句。
func loginOldAlgorithm(sessions map[string]*Session, accountID string, now time.Time) {
	for k, v := range sessions {
		if v.AccountID == accountID || v.ExpiresAt.Before(now) {
			delete(sessions, k)
		}
	}
}

// BenchmarkLogin 测「表里已有 5000 个别的账号」时单次登录的成本。
func BenchmarkLogin(b *testing.B) {
	s := New([]byte("bench-secret"), time.Hour)
	secret := []byte("bench-secret")
	now := time.Now()
	// 预热：5000 个其他账号的会话
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("bench-%d", i)
		sess := &Session{Token: fmt.Sprintf("tok-%d", i), AccountID: id, PlayerID: uint64(i), ExpiresAt: now.Add(time.Hour)}
		s.sessions[sess.Token] = sess
		s.accountToken[id] = sess.Token
		s.byID[id] = &Account{ID: id, PlayerID: uint64(i), MMR: 1500}
		s.byPlayer[uint64(i)] = s.byID[id]
	}
	ts := now.UnixMilli()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 每次用不同账号，模拟 bot 集群并发登录（不能走同账号的 O(1) 分支）
		id := fmt.Sprintf("bench-%d", i%5000)
		if _, _, err := s.Login(id, ts, "n", Sign(secret, id, ts, "n"), 1500); err != nil {
			b.Fatalf("登录: %v", err)
		}
	}
}

// BenchmarkLoginOldAlgorithm 对照：同样的表，用修复前的遍历清理。
func BenchmarkLoginOldAlgorithm(b *testing.B) {
	sessions := make(map[string]*Session, 5000)
	now := time.Now()
	for i := 0; i < 5000; i++ {
		sess := &Session{Token: fmt.Sprintf("tok-%d", i), AccountID: fmt.Sprintf("bench-%d", i), ExpiresAt: now.Add(time.Hour)}
		sessions[sess.Token] = sess
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("bench-%d", i%5000)
		loginOldAlgorithm(sessions, id, now)
		sessions[fmt.Sprintf("new-%d", i)] = &Session{
			Token: fmt.Sprintf("new-%d", i), AccountID: id, ExpiresAt: now.Add(time.Hour),
		}
	}
}
