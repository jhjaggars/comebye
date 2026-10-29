package ledger

import (
	"time"
)

// Plan outcomes.
const (
	PlanOpen    = "open"    // members still working, or not all handed off yet
	PlanDone    = "done"    // every member finished
	PlanPartial = "partial" // all settled, but a member failed or was removed unfinished
)

// MaxPlans bounds the plans kept (the oldest settled ones go first).
const MaxPlans = 100

// Plan groups handoffs that make up one change, possibly in several
// repositories and on several hosts. It is settled when Size members
// exist and none is still pending; the hub then tells the user once.
type Plan struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Size      int       `json:"size"`
	By        string    `json:"by,omitempty"`       // display string of the creator
	ReplyTo   *Ref      `json:"reply_to,omitempty"` // creator, when an agent
	CreatedAt time.Time `json:"created_at"`
	// Outcome is "" while open, then PlanDone or PlanPartial.
	Outcome   string     `json:"outcome,omitempty"`
	SettledAt *time.Time `json:"settled_at,omitempty"`
	// Notified: the user was pushed for this settling. Callback is the
	// creator's notice (same delivery as an entry's).
	Notified bool      `json:"notified,omitempty"`
	Callback *Callback `json:"callback,omitempty"`
}

func copyPlan(p *Plan) Plan {
	c := *p
	if p.ReplyTo != nil {
		r := *p.ReplyTo
		c.ReplyTo = &r
	}
	if p.Callback != nil {
		cb := *p.Callback
		c.Callback = &cb
	}
	return c
}

// Member outcomes.
const (
	MemberPending = "pending"
	MemberOK      = "ok"
	MemberFailed  = "failed"
)

// MemberOutcome says where a plan member stands: ok once it finished
// (a finished worker that was marked or removed afterwards is still ok),
// failed when its handoff failed or it was removed without finishing,
// pending otherwise.
func MemberOutcome(e Entry) string {
	switch {
	case e.Status == Failed, e.Status == Removed && e.FinishedAt == nil:
		return MemberFailed
	case e.Status == Running:
		return MemberPending
	case e.FinishedAt != nil:
		return MemberOK
	}
	return MemberPending
}

// AddPlan records a new plan.
func (s *Store) AddPlan(p Plan) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.ID = newID()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	s.data.Plans = append(s.data.Plans, &p)
	if len(s.data.Plans) > MaxPlans {
		for i, q := range s.data.Plans {
			if q.Outcome != "" {
				s.data.Plans = append(s.data.Plans[:i], s.data.Plans[i+1:]...)
				break
			}
		}
	}
	return copyPlan(&p), s.save()
}

// Plans returns every plan, oldest first.
func (s *Store) Plans() []Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Plan, 0, len(s.data.Plans))
	for _, p := range s.data.Plans {
		out = append(out, copyPlan(p))
	}
	return out
}

// GetPlan returns a plan by id.
func (s *Store) GetPlan(id string) (Plan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.data.Plans {
		if p.ID == id {
			return copyPlan(p), true
		}
	}
	return Plan{}, false
}

// Members returns a plan's entries, oldest first.
func (s *Store) Members(planID string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.data.Entries {
		if e.PlanID == planID && !(e.Status == Failed && s.supersededLocked(e)) {
			out = append(out, copyEntry(e))
		}
	}
	return out
}

// EvalPlans settles plans whose members are all accounted for and
// returns those that just settled (each once; the user's push and the
// creator's callback are queued here). A settled plan whose member goes
// back to work opens again, and settles (and notifies) again later.
func (s *Store) EvalPlans(now time.Time) []Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	var settled []Plan
	changed := false
	for _, p := range s.data.Plans {
		members, pending, failed := 0, 0, 0
		for _, e := range s.data.Entries {
			if e.PlanID != p.ID {
				continue
			}
			if e.Status == Failed && s.supersededLocked(e) {
				continue // handed off again since; the new entry counts
			}
			members++
			switch MemberOutcome(*e) {
			case MemberPending:
				pending++
			case MemberFailed:
				failed++
			}
		}
		if members < p.Size || pending > 0 {
			if p.Outcome != "" {
				p.Outcome, p.SettledAt, p.Notified, p.Callback, changed = "", nil, false, nil, true
			}
			continue
		}
		outcome := PlanDone
		if failed > 0 {
			outcome = PlanPartial
		}
		if p.Outcome == outcome {
			continue
		}
		t := now.UTC()
		p.Outcome, p.SettledAt, p.Notified, changed = outcome, &t, true, true
		if p.ReplyTo != nil {
			p.Callback = &Callback{State: CallbackPending, At: t}
		}
		settled = append(settled, copyPlan(p))
	}
	if changed {
		_ = s.save()
	}
	return settled
}

// supersededLocked reports whether a failed entry has been replaced by a
// later, not failed handoff of the same plan, host, repository and branch.
func (s *Store) supersededLocked(f *Entry) bool {
	for _, e := range s.data.Entries {
		if e != f && e.PlanID == f.PlanID && e.Host == f.Host && e.RepoName == f.RepoName && e.Branch == f.Branch &&
			e.Status != Failed && e.CreatedAt.After(f.CreatedAt) {
			return true
		}
	}
	return false
}

// PendingPlanCallbacks lists settled plans whose creator has not been told.
func (s *Store) PendingPlanCallbacks() []Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Plan
	for _, p := range s.data.Plans {
		if p.Outcome != "" && p.Callback != nil && p.Callback.State == CallbackPending {
			out = append(out, copyPlan(p))
		}
	}
	return out
}

// PlanCallbackTried and PlanCallbackDone mirror the entry versions.
func (s *Store) PlanCallbackTried(id string, at time.Time) error {
	return s.setPlanCallback(id, at, func(c *Callback) { c.Tries++ })
}

func (s *Store) PlanCallbackDone(id string, at time.Time, state, to string, now time.Time) error {
	return s.setPlanCallback(id, at, func(c *Callback) { settleCallback(c, state, to, now) })
}

func (s *Store) setPlanCallback(id string, at time.Time, fn func(*Callback)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.data.Plans {
		if p.ID == id && p.Callback != nil && p.Callback.State == CallbackPending && p.Callback.At.Equal(at) {
			fn(p.Callback)
			return s.save()
		}
	}
	return nil
}

func settleCallback(c *Callback, state, to string, now time.Time) {
	c.State = state
	if state == CallbackDelivered {
		t := now.UTC()
		c.To, c.DeliveredAt = to, &t
		c.Tries++
	}
}
