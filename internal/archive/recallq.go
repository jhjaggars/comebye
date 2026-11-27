package archive

import (
	"errors"
	"strings"
)

// One `comebye recall` question, answered from an index. The CLI runs it on a
// local index when one is asked for (--index, COMEBYE_RECALL_INDEX), and
// otherwise sends it to its server, which answers from the hub's index
// (POST /api/recall/query): the hub builds the one all-hosts index, and a peer
// reads it over the API.

// RecallOps are the questions recall asks.
var RecallOps = map[string]bool{"ask": true, "threads": true, "todo": true, "list": true, "search": true, "show": true, "commit": true, "produced": true}

// RecallQuery is one question.
type RecallQuery struct {
	Op     string   `json:"op"`
	Args   []string `json:"args,omitempty"`
	Filter Filter   `json:"filter"`
	// Text: show also returns the first Text indexed messages.
	Text int `json:"text,omitempty"`
}

// RecallAnswer holds what one question found; which fields are set depends
// on the op.
type RecallAnswer struct {
	Threads []ThreadView     `json:"threads,omitempty"` // ask, threads, todo
	Rows    []map[string]any `json:"rows,omitempty"`    // list, search, commit
	// produced
	Items []ProducedItem `json:"items,omitempty"`
	Known bool           `json:"known,omitempty"` // the reference was one produced understands
	// show of a thread
	Thread *ThreadView `json:"thread,omitempty"`
	// show of a session
	Session  map[string]any   `json:"session,omitempty"`
	Commits  []map[string]any `json:"commits,omitempty"`
	Messages []map[string]any `json:"messages,omitempty"`
	ThreadOf string           `json:"thread_of,omitempty"`
	Produced []ProducedItem   `json:"produced,omitempty"`
}

// ErrAmbiguous is an id prefix that matches more than one session or thread.
var ErrAmbiguous = errors.New("matches several; give more of the id")

// ErrRecallUsage is a question without the arguments its op needs.
var ErrRecallUsage = errors.New("recall: bad question")

// Recall answers q.
func (ix *Index) Recall(q RecallQuery) (*RecallAnswer, error) {
	a := &RecallAnswer{}
	var err error
	words := strings.Join(q.Args, " ")
	switch q.Op {
	case "ask":
		if words == "" {
			return nil, ErrRecallUsage
		}
		a.Threads, err = ix.Ask(words, q.Filter, q.Filter.Limit)
	case "threads", "todo":
		a.Threads, err = ix.Threads(q.Filter, q.Op == "todo")
	case "list":
		a.Rows, err = ix.List(q.Filter)
	case "search":
		if words == "" {
			return nil, ErrRecallUsage
		}
		a.Rows, err = ix.Search(words, q.Filter)
	case "commit":
		if len(q.Args) != 1 {
			return nil, ErrRecallUsage
		}
		a.Rows, err = ix.ByCommit(q.Args[0])
	case "produced":
		if len(q.Args) != 1 {
			return nil, ErrRecallUsage
		}
		a.Items, a.Known, err = ix.ProducedRef(q.Args[0], q.Filter, max(q.Filter.Limit, 20))
	case "show":
		if len(q.Args) != 1 {
			return nil, ErrRecallUsage
		}
		id := q.Args[0]
		if strings.HasPrefix(id, "t-") {
			a.Thread, err = ix.Thread(id)
			break
		}
		a.Session, a.Commits, err = ix.Show(id)
		if err != nil {
			break
		}
		host, kind, sid := recallStr(a.Session["host"]), recallStr(a.Session["kind"]), recallStr(a.Session["id"])
		if q.Text > 0 {
			if a.Messages, err = ix.Text(host, kind, sid, q.Text); err != nil {
				break
			}
		}
		a.ThreadOf = ix.ThreadOf(host, kind, sid)
		a.Produced, _ = ix.ProducedBySession(host, kind, sid)
	default:
		return nil, ErrRecallUsage
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

func recallStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
