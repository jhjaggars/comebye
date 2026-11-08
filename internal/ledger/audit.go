package ledger

import "time"

// MaxAudit bounds the audit log kept in the ledger file.
const MaxAudit = 2000

// AuditEvent is one action the user took through the app that changes
// something outside it (a release tagged, a host upgraded): who, what, when
// and how it went. It is the ledger's record, beside the journal line the
// server logs for the same action. Nothing in it is written by an agent.
type AuditEvent struct {
	At     time.Time `json:"at"`
	By     string    `json:"by"`               // the Tailscale login (or "the user" without one)
	Action string    `json:"action"`           // e.g. release.approve, release.tagged, upgrade.start
	Todo   string    `json:"todo,omitempty"`   // the to-do item this belongs to
	Host   string    `json:"host,omitempty"`   // for host actions
	Detail string    `json:"detail,omitempty"` // version, commit, result
	OK     bool      `json:"ok"`
}

// AddAudit appends an event, dropping the oldest past MaxAudit.
func (s *Store) AddAudit(e AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	s.data.Audit = append(s.data.Audit, &e)
	if n := len(s.data.Audit); n > MaxAudit {
		s.data.Audit = append([]*AuditEvent(nil), s.data.Audit[n-MaxAudit:]...)
	}
	return s.save()
}

// AuditLog returns the events, oldest first; todo "" means all of them.
func (s *Store) AuditLog(todo string) []AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []AuditEvent{}
	for _, e := range s.data.Audit {
		if todo == "" || e.Todo == todo {
			out = append(out, *e)
		}
	}
	return out
}
