// Package usage counts the tokens agents spend. Each host scans its agents'
// session transcripts (Claude Code and Pi) and keeps counts and labels only:
// never message text, tool output or prompts. The hub gathers the hosts'
// counts, labels each session with its project, handoff and role, and
// answers questions about them (query.go).
//
// The headline unit is "fresh" tokens: input + output + cache writes. Cache
// reads are counted and shown apart, because they are 10x cheaper and
// would drown everything else.
package usage

import "time"

// Tokens is a bag of counts. Calls is the number of model replies; Sub is
// how many of those came from subagents (it is included in Calls).
type Tokens struct {
	In    int64 `json:"in,omitempty"`
	Out   int64 `json:"out,omitempty"`
	CW    int64 `json:"cw,omitempty"` // cache writes
	CR    int64 `json:"cr,omitempty"` // cache reads
	Calls int64 `json:"calls,omitempty"`
	Sub   int64 `json:"sub,omitempty"`
}

// Add adds o to t.
func (t *Tokens) Add(o Tokens) {
	t.In += o.In
	t.Out += o.Out
	t.CW += o.CW
	t.CR += o.CR
	t.Calls += o.Calls
	t.Sub += o.Sub
}

// Minus returns t - o, field by field.
func (t Tokens) Minus(o Tokens) Tokens {
	return Tokens{t.In - o.In, t.Out - o.Out, t.CW - o.CW, t.CR - o.CR, t.Calls - o.Calls, t.Sub - o.Sub}
}

// Fresh is the headline number: what the model had to read or write anew.
func (t Tokens) Fresh() int64 { return t.In + t.Out + t.CW }

// IsZero reports whether every count is zero.
func (t Tokens) IsZero() bool { return t == Tokens{} }

// Row is one session's tokens for one hour (UTC, as unix seconds / 3600)
// and one model.
type Row struct {
	H int64  `json:"h"`
	M string `json:"m"`
	T Tokens `json:"t"`
}

// Label sources: a label from the ledger is never replaced by a guess.
const (
	SrcGuess  = 0
	SrcLedger = 1
)

// Labels say whose work a session was. The hub sets them; a peer's
// sessions arrive without.
type Labels struct {
	Project string `json:"project,omitempty"`
	Role    string `json:"role,omitempty"`    // coordinator, worker or home
	Purpose string `json:"purpose,omitempty"` // build, research, plan, review, rollout, coordination, ad-hoc, other
	Handoff string `json:"handoff,omitempty"` // the ledger entry's id
	Name    string `json:"name,omitempty"`    // the handoff's agent name or branch
	Agent   string `json:"agent,omitempty"`   // "<host>/<pane>" when known
	Src     int    `json:"src,omitempty"`
}

// Session is one agent session's counts. Subagent transcripts count toward
// their parent session.
type Session struct {
	Host   string `json:"host"`
	Kind   string `json:"kind"` // claude or pi
	ID     string `json:"id"`
	Cwd    string `json:"cwd,omitempty"`
	Branch string `json:"branch,omitempty"`
	First  int64  `json:"first"` // unix seconds of the first and last counted reply
	Last   int64  `json:"last"`
	// Upd is when the owning host last changed this record (unix nanos on
	// that host): the cursor a hub gathers by.
	Upd  int64  `json:"upd"`
	Rows []Row  `json:"rows"`
	L    Labels `json:"l"`

	idx map[rowKey]int
}

type rowKey struct {
	h int64
	m string
}

// Key identifies the session across hosts.
func (s *Session) Key() string { return s.Host + "/" + s.Kind + "/" + s.ID }

// Total is the sum over the session's rows.
func (s *Session) Total() Tokens {
	var t Tokens
	for _, r := range s.Rows {
		t.Add(r.T)
	}
	return t
}

func (s *Session) index() {
	s.idx = make(map[rowKey]int, len(s.Rows))
	for i, r := range s.Rows {
		s.idx[rowKey{r.H, r.M}] = i
	}
}

// add applies a delta at (hour, model).
func (s *Session) add(h int64, model string, d Tokens) {
	if s.idx == nil {
		s.index()
	}
	k := rowKey{h, model}
	i, ok := s.idx[k]
	if !ok {
		s.Rows = append(s.Rows, Row{H: h, M: model})
		i = len(s.Rows) - 1
		s.idx[k] = i
	}
	s.Rows[i].T.Add(d)
}

// HourOf is the hour index (UTC) of t.
func HourOf(t time.Time) int64 { return t.Unix() / 3600 }

// Retention: session rows are kept this long after the session's last
// reply; their hourly rollups are kept forever.
const SessionRetention = 180 * 24 * time.Hour
