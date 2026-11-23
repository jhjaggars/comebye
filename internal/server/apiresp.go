package server

import "github.com/jhjaggars/comebye/internal/ledger"

// The answers of the read routes, as named types: `comebye api` generates
// its Python records from them (apitypes_test.go), so a field added here
// shows up in scripts, and one renamed fails the golden test.

// agentsView answers GET /api/agents (the slim view).
type agentsView struct {
	Agents []browserAgent `json:"agents"`
	Hosts  []browserHost  `json:"hosts"`
}

// ledgerView answers GET /api/ledger.
type ledgerView struct {
	Entries []entryView           `json:"entries"`
	Marks   []ledger.Mark         `json:"marks"`
	Plans   []planView            `json:"plans"`
	Roles   map[string]Role       `json:"roles"`
	Asks    map[string]*askReview `json:"asks,omitempty"` // not for a tagged node
}

// todoListView answers GET /api/todos.
type todoListView struct {
	Todos    []Todo         `json:"todos"`
	Open     int            `json:"open"`
	Answered int            `json:"answered"`
	Auto     map[string]int `json:"auto"`
}

// todoView answers GET /api/todos/{id}.
type todoView struct {
	Todo Todo `json:"todo"`
}

// queueView answers GET /api/queue.
type queueView struct {
	Queue []QueuedPrompt `json:"queue"`
}

// pageListView answers GET /api/pages. Matches and Partial come with ?q=
// (and then always, an empty list included: the app reads them).
type pageListView struct {
	Pages     []Page       `json:"pages"`
	PublicURL string       `json:"public_url"`
	PagesURL  string       `json:"pages_url"`
	Matches   *[]PageMatch `json:"matches,omitempty"`
	Partial   *bool        `json:"partial,omitempty"`
}

// pageView is a page as the API shows it: GET /api/pages/{id} (with the
// body on ?body=1) and the answer to a publish (with Created).
type pageView struct {
	Page        Page    `json:"page"`
	Path        string  `json:"path"`
	URL         string  `json:"url,omitempty"`
	Body        *string `json:"body,omitempty"`
	BodyVersion int     `json:"body_version,omitempty"`
	BodyKind    string  `json:"body_kind,omitempty"`
	Created     *bool   `json:"created,omitempty"`
}

// logView answers GET /api/log.
type logView struct {
	Events []Event `json:"events"`
}

// removalsView answers GET /api/removals (the user's).
type removalsView struct {
	Removals      []*removal        `json:"removals"`
	AutoRemoved   []autoRemovedView `json:"auto_removed"`
	AutoRemove    bool              `json:"auto_remove"`
	SweepBranches []sweepDone       `json:"sweep_branches"`
}

// whoamiView answers GET /api/whoami. Agent and By are an agent caller's.
type whoamiView struct {
	Host      string `json:"host"`
	Role      string `json:"role"`
	Ledger    bool   `json:"ledger"`
	Version   string `json:"version"`
	HubMode   bool   `json:"hub_mode"`
	Hub       string `json:"hub"`
	PublicURL string `json:"public_url,omitempty"`
	Agent     string `json:"agent,omitempty"`
	By        string `json:"by,omitempty"`
}

// coordinatorsView answers GET /api/coordinators.
type coordinatorsView struct {
	Coordinators []coordinatorInfo `json:"coordinators"`
}
