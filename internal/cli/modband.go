package cli

import (
	"net/url"
	"strings"
)

// bandState is what the Claude Code band mod shows above the prompt: a few
// counts and one line of state, small enough to fetch every half minute.
// Every section is best-effort: one that cannot be read is left out.
type bandState struct {
	Agent       string       `json:"agent"`
	Coordinator bool         `json:"coordinator"`
	Todos       int          `json:"todos"` // open to-dos waiting on this agent
	FirstTodo   string       `json:"first_todo,omitempty"`
	Queued      int          `json:"queued"` // hub notices and prompts held for this agent
	Handoff     *bandHandoff `json:"handoff,omitempty"`
	Headline    string       `json:"headline,omitempty"`   // a coordinator's briefing, first line
	OpenTodos   int          `json:"open_todos,omitempty"` // a coordinator: all open to-dos
}

type bandHandoff struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Progress string `json:"progress,omitempty"`
	Phase    string `json:"phase,omitempty"`
	Landing  string `json:"landing,omitempty"` // landed, pending, failed
	CI       string `json:"ci,omitempty"`      // running, success, failure, none
}

// headline is a briefing's first line of prose, without Markdown marks.
func headline(md string) string {
	for _, l := range strings.Split(md, "\n") {
		l = strings.TrimSpace(strings.NewReplacer("**", "", "__", "", "`", "").Replace(strings.TrimLeft(strings.TrimSpace(l), "# ")))
		if l == "" {
			continue
		}
		return clip(l, 160)
	}
	return ""
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func collectBand(c *client) (*bandState, error) {
	w, err := c.getWhoami()
	if err != nil {
		return nil, err
	}
	b := &bandState{Agent: w.Agent}
	var led struct {
		Entries []struct {
			ID       string `json:"id"`
			AgentID  string `json:"agent_id"`
			Status   string `json:"status"`
			Progress *struct {
				Text  string `json:"text"`
				Phase string `json:"phase"`
			} `json:"progress"`
			LandCheck *struct {
				State string `json:"state"`
				OK    bool   `json:"ok"`
				CI    *struct {
					State string `json:"state"`
				} `json:"ci"`
			} `json:"land_check"`
		} `json:"entries"`
		Roles map[string]struct {
			Role string `json:"role"`
		} `json:"roles"`
	}
	if err := c.call("GET", "/api/ledger", nil, nil, &led); err == nil {
		b.Coordinator = led.Roles[w.Agent].Role == "coordinator"
		for i := len(led.Entries) - 1; i >= 0; i-- {
			en := led.Entries[i]
			if en.AgentID != w.Agent || en.Status == "removed" {
				continue
			}
			h := &bandHandoff{ID: en.ID, Status: en.Status}
			if en.Progress != nil {
				h.Progress, h.Phase = clip(en.Progress.Text, 120), en.Progress.Phase
			}
			if lc := en.LandCheck; lc != nil {
				h.Landing = lc.State
				if h.Landing == "" {
					h.Landing = map[bool]string{true: "landed", false: "failed"}[lc.OK]
				}
				if lc.CI != nil {
					h.CI = lc.CI.State
				}
			}
			b.Handoff = h
			break
		}
	}
	var td struct {
		Todos []apiTodo `json:"todos"`
	}
	if err := c.call("GET", "/api/todos", nil, nil, &td); err == nil {
		for _, t := range td.Todos {
			if t.Waiting != nil && t.Waiting.ID == w.Agent {
				if b.Todos == 0 {
					b.FirstTodo = clip(t.Question, 100)
				}
				b.Todos++
			}
		}
		if b.Coordinator {
			b.OpenTodos = len(td.Todos)
		}
	}
	var q struct {
		Queue []struct {
			State string `json:"state"`
		} `json:"queue"`
	}
	if err := c.call("GET", "/api/queue", url.Values{"agent": {w.Agent}}, nil, &q); err == nil {
		for _, it := range q.Queue {
			if it.State == "pending" {
				b.Queued++
			}
		}
	}
	if b.Coordinator {
		var br struct {
			Brief *struct {
				Markdown string `json:"markdown"`
			} `json:"brief"`
		}
		if err := c.call("GET", "/api/brief", nil, nil, &br); err == nil && br.Brief != nil {
			b.Headline = headline(br.Brief.Markdown)
		}
	}
	return b, nil
}
