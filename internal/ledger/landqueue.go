package ledger

import (
	"sort"
	"time"
)

// The landing queue. Landings into one base branch of one repository
// (on one host) take turns, so a worker does not rebase and retest because
// another landing pushed to the base in the middle of its test run.
//
// There is no separate store: the queue is the open landings (status
// landing, Approval.Queued) of one repo+base, in Approval.QueueSeq order,
// so it survives a restart of the hub with the ledger. The first one holds
// the turn once the hub has told its worker (Approval.TurnAt); the turn
// ends when the entry leaves landing (landed, failed, marked, cancelled), or
// when the holder is skipped, or when it has lasted too long (AdvanceQueues).

// QueueKey identifies one queue: a repository's primary checkout on a host,
// and the branch landed on.
type QueueKey struct {
	Host string `json:"host"`
	Repo string `json:"repo"`
	Base string `json:"base"`
}

// QueueKeyOf is the queue the entry's landing belongs to.
func QueueKeyOf(e Entry) QueueKey {
	base := e.Base
	if e.Approval != nil && e.Approval.Base != "" {
		base = e.Approval.Base
	}
	if base == "" {
		base = "main"
	}
	return QueueKey{Host: e.Host, Repo: e.Repo, Base: base}
}

func inQueue(e *Entry) bool {
	return e.Status == Landing && e.Approval != nil && e.Approval.Queued
}

// QueueItem is one place in a queue.
type QueueItem struct {
	AgentID   string     `json:"agent_id"`
	AgentName string     `json:"agent_name,omitempty"`
	EntryID   string     `json:"entry_id"`
	Branch    string     `json:"branch,omitempty"`
	Position  int        `json:"position"` // 1 is the front
	Turn      bool       `json:"turn"`     // the worker has been told it is up
	TurnAt    *time.Time `json:"turn_at,omitempty"`
	Since     time.Time  `json:"since"`
}

// QueueView is a queue as the ledger and the app show it.
type QueueView struct {
	QueueKey
	Items []QueueItem `json:"items"`
}

// members are the queue's entries, front first. s.mu is held.
func (s *Store) queueMembers(k QueueKey) []*Entry {
	var ms []*Entry
	for _, e := range s.data.Entries {
		if inQueue(e) && QueueKeyOf(*e) == k {
			ms = append(ms, e)
		}
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Approval.QueueSeq < ms[j].Approval.QueueSeq })
	return ms
}

func (s *Store) nextSeq() int64 {
	var m int64
	for _, e := range s.data.Entries {
		if e.Approval != nil && e.Approval.QueueSeq > m {
			m = e.Approval.QueueSeq
		}
	}
	return m + 1
}

// grantLocked gives the turn to the front of e's queue when nobody holds
// it. It reports the entry that got it (nil: none).
func (s *Store) grantLocked(_ string, e *Entry, now time.Time) *Entry {
	return s.grantQueue(QueueKeyOf(*e), now)
}

func (s *Store) grantQueue(k QueueKey, now time.Time) *Entry {
	ms := s.queueMembers(k)
	if len(ms) == 0 {
		return nil
	}
	for _, m := range ms {
		if m.Approval.TurnAt != nil {
			return nil
		}
	}
	t := now.UTC()
	ms[0].Approval.TurnAt = &t
	return ms[0]
}

// Queues lists every non-empty queue.
func (s *Store) Queues() []QueueView {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[QueueKey]bool{}
	var out []QueueView
	for _, e := range s.data.Entries {
		if !inQueue(e) {
			continue
		}
		k := QueueKeyOf(*e)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s.viewOf(k))
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].QueueKey, out[j].QueueKey
		return a.Host+"\x00"+a.Repo+"\x00"+a.Base < b.Host+"\x00"+b.Repo+"\x00"+b.Base
	})
	return out
}

func (s *Store) viewOf(k QueueKey) QueueView {
	v := QueueView{QueueKey: k, Items: []QueueItem{}}
	for i, m := range s.queueMembers(k) {
		it := QueueItem{AgentID: m.AgentID, AgentName: m.AgentName, EntryID: m.ID, Branch: m.Branch,
			Position: i + 1, Turn: m.Approval.TurnAt != nil, Since: m.Approval.At}
		if m.Approval.TurnAt != nil {
			t := *m.Approval.TurnAt
			it.TurnAt = &t
		}
		if m.Approval.Branch != "" {
			it.Branch = m.Approval.Branch
		}
		v.Items = append(v.Items, it)
	}
	return v
}

// QueueOf is the queue the agent's landing is in, and its place there
// (0: it is not in a queue).
func (s *Store) QueueOf(agentID string) (QueueView, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byAgent(agentID)
	if e == nil || !inQueue(e) {
		return QueueView{}, 0
	}
	v := s.viewOf(QueueKeyOf(*e))
	for _, it := range v.Items {
		if it.AgentID == agentID {
			return v, it.Position
		}
	}
	return v, 0
}

// QueueChange is what a reorder, skip or advance did to the queue: who now
// holds the turn without having been told, and who lost it.
type QueueChange struct {
	Granted []Entry // newly up
	Lost    []Entry // had the turn and no longer do
	Ended   []Entry // left the queue by timeout
}

// AdvanceQueues hands the turn to the front of every queue that nobody
// holds, and takes it from a holder whose turn has lasted longer than
// timeout (0: never), which leaves the queue (its landing stays open and is
// still checked).
func (s *Store) AdvanceQueues(now time.Time, timeout time.Duration) (QueueChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ch QueueChange
	keys := map[QueueKey]bool{}
	for _, e := range s.data.Entries {
		if inQueue(e) {
			keys[QueueKeyOf(*e)] = true
		}
	}
	for k := range keys {
		for _, m := range s.queueMembers(k) {
			if t := m.Approval.TurnAt; t != nil && timeout > 0 && now.Sub(*t) > timeout {
				m.Approval.Queued, m.Approval.QueueEnd = false, "turn timed out"
				ch.Ended = append(ch.Ended, copyEntry(m))
			}
		}
		if g := s.grantQueue(k, now); g != nil {
			ch.Granted = append(ch.Granted, copyEntry(g))
		}
	}
	if len(ch.Granted)+len(ch.Ended) == 0 {
		return ch, nil
	}
	return ch, s.save()
}

// MoveInQueue puts the agent's landing at place `to` (1 is the front) in its
// queue. The holder of the turn keeps the front: others go no further
// forward than second, and the holder itself is moved by SkipInQueue.
// ok is false when the agent is not in a queue or holds the turn.
func (s *Store) MoveInQueue(agentID string, to int) (QueueView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byAgent(agentID)
	if e == nil || !inQueue(e) || e.Approval.TurnAt != nil {
		return QueueView{}, false, nil
	}
	k := QueueKeyOf(*e)
	ms := s.queueMembers(k)
	if to < 1 {
		to = 1
	}
	if len(ms) > 0 && ms[0].Approval.TurnAt != nil && to < 2 {
		to = 2
	}
	if to > len(ms) {
		to = len(ms)
	}
	seqs := make([]int64, len(ms))
	var order []*Entry
	for i, m := range ms {
		seqs[i] = m.Approval.QueueSeq
		if m != e {
			order = append(order, m)
		}
	}
	order = append(order[:to-1], append([]*Entry{e}, order[to-1:]...)...)
	for i, m := range order {
		m.Approval.QueueSeq = seqs[i]
	}
	return s.viewOf(k), true, s.save()
}

// SkipInQueue sends the agent's landing to the back of its queue; if it held
// the turn the turn passes to the next in line. had says it held the turn;
// next is the entry that was handed it (nil: nobody else is waiting).
func (s *Store) SkipInQueue(agentID string, now time.Time) (had bool, next *Entry, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byAgent(agentID)
	if e == nil || !inQueue(e) {
		return false, nil, false, nil
	}
	had = e.Approval.TurnAt != nil
	e.Approval.TurnAt = nil
	e.Approval.QueueSeq = s.nextSeq()
	if g := s.grantQueue(QueueKeyOf(*e), now); g != nil && g != e {
		c := copyEntry(g)
		next = &c
	} else if g == e {
		// alone in the queue: it simply holds the turn again
		had = false
	}
	return had, next, true, s.save()
}

// SetTurnAtForTest backdates an entry's turn (tests of the timeout).
func (s *Store) SetTurnAtForTest(id string, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.data.Entries {
		if e.ID == id && e.Approval != nil && e.Approval.TurnAt != nil {
			e.Approval.TurnAt = &at
			return true, s.save()
		}
	}
	return false, nil
}
