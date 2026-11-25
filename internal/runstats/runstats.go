// Package runstats 收集一次实验内的跨 bot 计数。
//
// 为什么不全都走 Prometheus 计数器：报告里要的「有效对局吞吐」（goodput）是
// 「多少局真的打完了」，这是一个**业务口径**的数字，需要能从 run summary 里
// 直接读出来，而不是让读报告的人去写 PromQL 求和。
// 所以这些计数同时进 Prometheus（看板）和 run summary（报告）。
package runstats

import "sync/atomic"

// Stats 是一次实验的计数集合，可被成百上千个 bot 并发累加。
type Stats struct {
	Sessions       atomic.Int64
	Matches        atomic.Int64
	MatchesWin     atomic.Int64
	MatchesLoss    atomic.Int64
	MatchesDraw    atomic.Int64
	Abandoned      atomic.Int64
	QueueCancelled atomic.Int64
	InputsSent     atomic.Int64
	InputsRejected atomic.Int64
	InvalidInputs  atomic.Int64
	Disconnects    atomic.Int64
	Reconnects     atomic.Int64
}

// Snapshot 是可序列化的计数快照。
type Snapshot struct {
	Sessions       int64 `json:"sessions"`
	Matches        int64 `json:"matches_completed"`
	MatchesWin     int64 `json:"matches_win"`
	MatchesLoss    int64 `json:"matches_loss"`
	MatchesDraw    int64 `json:"matches_draw"`
	Abandoned      int64 `json:"abandoned"`
	QueueCancelled int64 `json:"queue_cancelled"`
	InputsSent     int64 `json:"inputs_sent"`
	InputsRejected int64 `json:"inputs_rejected"`
	InvalidInputs  int64 `json:"invalid_inputs"`
	Disconnects    int64 `json:"disconnects"`
	Reconnects     int64 `json:"reconnects"`
}

// Take 取一份快照。
func (s *Stats) Take() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	return Snapshot{
		Sessions:       s.Sessions.Load(),
		Matches:        s.Matches.Load(),
		MatchesWin:     s.MatchesWin.Load(),
		MatchesLoss:    s.MatchesLoss.Load(),
		MatchesDraw:    s.MatchesDraw.Load(),
		Abandoned:      s.Abandoned.Load(),
		QueueCancelled: s.QueueCancelled.Load(),
		InputsSent:     s.InputsSent.Load(),
		InputsRejected: s.InputsRejected.Load(),
		InvalidInputs:  s.InvalidInputs.Load(),
		Disconnects:    s.Disconnects.Load(),
		Reconnects:     s.Reconnects.Load(),
	}
}
