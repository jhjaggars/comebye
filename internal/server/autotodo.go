package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/dialog"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/question"
	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// Auto items: to-do items the hub files itself, so a need does not depend
// on an agent remembering `comebye todo add`. Slice 1 has one detector: an
// agent that has sat at a dialog (herdr status "blocked") for a while with
// nobody looking at it gets one action item, which closes itself when the
// agent leaves blocked.
//
// Everything here is deterministic: a status, a clock and one read of the
// pane. Nothing reads a screen before the threshold.

const (
	// SourceBlocked marks an item filed because an agent sat blocked.
	SourceBlocked = "hub-blocked"
	// SourceQuestion marks a suggestion: an agent sat idle on a question.
	SourceQuestion = "hub-question"
	// SourceAgent marks an item an agent added with `comebye todo add`.
	SourceAgent = "agent"
	// AutoCreator is the CreatedBy and ClosedBy of the hub's own items.
	AutoCreator = "hub"

	// DefaultAutoQuestion is how long an agent must sit idle (or done) on a
	// question before the hub suggests an item: long enough that a user
	// answering in the terminal never trips it.
	DefaultAutoQuestion = 3 * time.Minute

	// autoTurnWindow is how far before an agent's last message an item the
	// agent filed still counts as filed "during that turn" (the hub does not
	// see where a turn began).
	autoTurnWindow = 30 * time.Minute
	// autoReadsPerStep bounds the transcript reads one snapshot may start.
	autoReadsPerStep = 8

	// DefaultAutoBlocked is how long an agent must be blocked, unwatched,
	// before the hub files an item.
	DefaultAutoBlocked = 10 * time.Minute

	autoMaxActive   = 20 // active auto items, all agents
	autoMaxPerAgent = 5
	autoTick        = 30 * time.Second
	autoReadTimeout = 10 * time.Second
	autoDetailMax   = 1500
)

// AutoTodoConfig turns the detectors on. A zero value is everything off.
type AutoTodoConfig struct {
	// Blocked, when positive, is the blocked-agent threshold.
	Blocked time.Duration
	// Question, when positive, is how long an agent sits idle on a question
	// before the hub suggests an item for it.
	Question time.Duration
}

var (
	errAutoCap   = errors.New("auto_cap")
	errAutoHuman = errors.New("auto_human")
	errAutoDup   = errors.New("auto_dup")
)

type autoTodo struct {
	cfg AutoTodoConfig
	mu  sync.Mutex
	// muted agents get no auto items (set by id).
	muted map[string]bool
	// readDialog and viewing are replaced by tests.
	readDialog func(ctx context.Context, a herdr.AgentInfo) (*dialog.Dialog, error)
	viewing    func(a herdr.AgentInfo) bool
	latest     func(ctx context.Context, a herdr.AgentInfo) *transcript.Latest
	// checked is the idle episode (agent id -> key) whose last message was
	// already read and judged not to be a question, so a long idle agent
	// is read once, not every tick.
	checked map[string]string
}

func (s *Server) initAutoTodo(cfg AutoTodoConfig) {
	if (cfg.Blocked <= 0 && cfg.Question <= 0) || s.todos == nil {
		return
	}
	s.auto = &autoTodo{cfg: cfg, muted: map[string]bool{}, checked: map[string]string{}}
	s.auto.latest = func(ctx context.Context, a herdr.AgentInfo) *transcript.Latest {
		ctx, cancel := context.WithTimeout(ctx, autoReadTimeout)
		defer cancel()
		return s.latestOf(ctx, a.Host, a.PaneID)
	}
	s.auto.readDialog = s.autoReadDialog
	s.auto.viewing = func(a herdr.AgentInfo) bool {
		p := s.cfg.Presence
		return p.Viewing(state.AgentKey(&a)) || (s.isLocalAgent(a) && p.Viewing(a.PaneID))
	}
}

func (s *Server) isLocalAgent(a herdr.AgentInfo) bool {
	return a.Host == "" || a.Host == s.fleet.LocalName()
}

// MuteAutoTodo stops (or resumes) auto items for the agent with this id.
func (s *Server) MuteAutoTodo(id string, mute bool) {
	if s.auto == nil {
		return
	}
	s.auto.mu.Lock()
	defer s.auto.mu.Unlock()
	if mute {
		s.auto.muted[id] = true
	} else {
		delete(s.auto.muted, id)
	}
}

func (a *autoTodo) isMuted(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.muted[id]
}

// RunAutoTodo follows the fleet and a clock until ctx is done: the clock is
// what notices that ten minutes passed with no status change.
func (s *Server) RunAutoTodo(ctx context.Context) {
	if s.auto == nil || s.todos == nil {
		return
	}
	events, cancel := s.fleet.Hub().Subscribe()
	defer cancel()
	tick := time.NewTicker(autoTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Type == "agents" {
				s.autoTodoStep(ctx, ev.Agents, time.Now())
			}
		case <-tick.C:
			s.autoTodoStep(ctx, s.fleet.Agents(), time.Now())
		}
	}
}

// autoSince is when the agent's current status began, as far as known.
func autoSince(a herdr.AgentInfo) *time.Time {
	if a.StatusSince != nil {
		return a.StatusSince
	}
	return a.LastActive
}

func autoBlockedKey(a herdr.AgentInfo, since time.Time) string {
	return SourceBlocked + ":" + state.AgentKey(&a) + "@" + since.UTC().Format(time.RFC3339)
}

// autoTodoStep applies one fleet snapshot: it closes the items of agents
// that moved on, then files an item for each agent blocked past the
// threshold. It is serial (one caller), and the only slow part is the one
// pane read per new item.
func (s *Server) autoTodoStep(ctx context.Context, agents []herdr.AgentInfo, now time.Time) {
	au := s.auto
	if au == nil || s.todos == nil {
		return
	}
	byID := map[string]herdr.AgentInfo{}
	hosts := map[string]bool{}
	for _, a := range agents {
		byID[state.AgentKey(&a)] = a
		hosts[idHost(state.AgentKey(&a))] = true
	}
	// 1. Close what is over.
	for _, it := range s.todos.list("") {
		if it.State != TodoOpen || it.Waiting == nil {
			continue
		}
		a, ok := byID[it.Waiting.ID]
		reason := ""
		switch it.Source {
		case SourceBlocked:
			switch {
			case !ok:
				// A host that reports nobody may be down: only an agent that
				// vanished from a host that still lists others is gone.
				if hosts[idHost(it.Waiting.ID)] {
					reason = "the agent is gone"
				}
			case a.Status != herdr.StatusBlocked:
				reason = "the agent is no longer blocked"
			}
		case SourceQuestion:
			if !it.Suggested {
				continue // kept: it is the user's item now
			}
			switch {
			case !ok:
				if hosts[idHost(it.Waiting.ID)] {
					reason = "the agent is gone"
				}
			case a.Status != herdr.StatusIdle && a.Status != herdr.StatusDone:
				// The user answered, or the agent went on.
				reason = "the agent is no longer idle"
			default:
				if by := s.todos.filedBy(it.Waiting.ID, it.CreatedAt); by != "" {
					reason = "superseded by " + by // E2: it filed its own item after all
				}
			}
		default:
			continue
		}
		if reason != "" {
			if _, err := s.todos.close(it.ID, TodoDropped, AutoCreator, reason); err != nil && !errors.Is(err, errTodoClosed) {
				log.Printf("auto to-do: close %s: %v", it.ID, err)
			}
		}
	}
	// 2. File what is due.
	reads := 0
	for _, a := range agents {
		if ctx.Err() != nil {
			return
		}
		if (a.Status == herdr.StatusIdle || a.Status == herdr.StatusDone) && au.cfg.Question > 0 {
			if reads < autoReadsPerStep && s.autoQuestion(ctx, a, now, &reads) {
				continue
			}
		}
		if a.Status != herdr.StatusBlocked || au.cfg.Blocked <= 0 {
			continue
		}
		since := autoSince(a)
		if since == nil || now.Sub(*since) < au.cfg.Blocked {
			continue
		}
		id := state.AgentKey(&a)
		if au.isMuted(id) || (au.viewing != nil && au.viewing(a)) {
			continue
		}
		key := autoBlockedKey(a, *since)
		if !s.todos.wantsAuto(key, id) {
			continue // filed already (or closed, or a person has it): no screen read
		}
		it := s.autoBlockedItem(ctx, a, *since, now)
		it.AutoKey = key
		out, err := s.todos.addAuto(it)
		switch {
		case err == nil:
			log.Printf("auto to-do: %s for %s (blocked since %s)", out.ID, id, since.Format(time.RFC3339))
		case errors.Is(err, errAutoCap), errors.Is(err, errAutoHuman), errors.Is(err, errAutoDup), errors.Is(err, errTodoFull):
		default:
			log.Printf("auto to-do: %s: %v", id, err)
		}
	}
}

// autoReadDialog reads the dialog on a blocked agent's screen, on its host.
func (s *Server) autoReadDialog(ctx context.Context, a herdr.AgentInfo) (*dialog.Dialog, error) {
	ctx, cancel := context.WithTimeout(ctx, autoReadTimeout)
	defer cancel()
	if s.isLocalAgent(a) {
		return currentDialog(ctx, s.client, a, nil)
	}
	status, data, err := s.fleet.CallAgent(ctx, a.Host, a.PaneID, "dialog", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", a.Host, status)
	}
	var out struct {
		Dialog *dialog.Dialog `json:"dialog"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Dialog, nil
}

// autoAgentLink is the app link that opens the agent.
func (s *Server) autoAgentLink(a herdr.AgentInfo) string {
	if s.isLocalAgent(a) {
		return "/#/agent/" + url.PathEscape(a.PaneID)
	}
	return "/#/agent/" + url.PathEscape(a.Host) + "/" + url.PathEscape(a.PaneID)
}

func autoName(a herdr.AgentInfo) string {
	if a.Name != "" {
		return a.Name
	}
	return a.PaneID
}

// autoBlockedItem builds the item, reading the screen once.
func (s *Server) autoBlockedItem(ctx context.Context, a herdr.AgentInfo, since, now time.Time) Todo {
	name := autoName(a)
	var b strings.Builder
	var opts []TodoOption
	fmt.Fprintf(&b, "**%s** has been blocked for %s (since %s) and nobody has had it open.\n\n",
		mdSafe(name), roundMinutes(now.Sub(since)), since.Local().Format("15:04"))
	d, err := s.auto.readDialog(ctx, a)
	switch {
	case err != nil:
		fmt.Fprintf(&b, "The screen could not be read just now (%s). Open the agent to see what it is waiting for.", mdSafe(oneLine(err.Error(), 120)))
	case d == nil:
		b.WriteString("Blocked, no dialog recognised on its screen. Open the agent to see what it is waiting for.")
	default:
		b.WriteString(dialogContext(d))
		if opts = dialogOptions(d); len(opts) > 0 {
			// The same numbered list `todo add --option` writes, for readers of the text.
			b.WriteString("\n\n" + optionsMarkdown(opts, nil) + "\n\nThese are the dialog's own options. Answering here does not answer the dialog: open the agent (link above) and answer it in its pane.")
		}
	}
	it := Todo{
		Question:  oneLine(name+" is blocked at a dialog and waiting for you", todoMaxQuestion),
		Context:   block(b.String(), todoMaxContext),
		Options:   opts,
		Kind:      "action",
		Links:     []TodoLink{{Label: "Open " + oneLine(name, 60), URL: s.autoAgentLink(a)}},
		Waiting:   &TodoAgent{ID: state.AgentKey(&a), Name: a.Name, Instance: s.hostInstance(a.Host)},
		CreatedBy: AutoCreator,
		Source:    SourceBlocked,
	}
	return it
}

func roundMinutes(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

// mdSafe keeps an agent-supplied name from becoming Markdown structure.
func mdSafe(s string) string {
	return strings.NewReplacer("*", "", "`", "'", "_", " ", "[", "(", "]", ")", "<", "", ">", "").Replace(oneLine(s, 120))
}

// fenced puts text in a code fence it cannot close early.
func fenced(text string) string {
	text = strings.ReplaceAll(text, "```", "'''")
	r := []rune(text)
	if len(r) > autoDetailMax {
		text = string(r[:autoDetailMax]) + "\n…"
	}
	return "```\n" + strings.TrimRight(text, "\n") + "\n```"
}

// dialogContext is the Markdown for a recognised dialog: what it asks and
// what it is about (the command or tool waiting). Its options go in the
// item's Options (dialogOptions).
func dialogContext(d *dialog.Dialog) string {
	var b strings.Builder
	title := strings.TrimSpace(d.Title)
	if title == "" {
		title = strings.TrimSpace(d.Header)
	}
	if title != "" {
		fmt.Fprintf(&b, "**%s**\n\n", mdSafe(title))
	}
	if q := strings.TrimSpace(d.Question); q != "" {
		fmt.Fprintf(&b, "%s\n\n", oneLine(q, 400))
	}
	if det := strings.TrimSpace(d.Detail); det != "" {
		b.WriteString(fenced(det) + "\n\n")
	}
	if strings.TrimSpace(b.String()) == "" {
		return "A dialog is open, but it showed no text."
	}
	return b.String()
}

// dialogOptions are the dialog's own rows as the item's structured options
// (none when there are fewer than two or more than nine).
func dialogOptions(d *dialog.Dialog) []TodoOption {
	var in []TodoOption
	for _, o := range d.Options {
		if o.Role == dialog.RoleOther || o.Role == dialog.RoleChat || o.Role == dialog.RoleNext || strings.TrimSpace(o.Label) == "" {
			continue
		}
		in = append(in, TodoOption{Label: o.Label, Detail: o.Description})
	}
	opts, _, err := cleanOptions(in, nil)
	if err != nil {
		return nil
	}
	return opts
}

// ---- the store side ----

// wantsAuto reports whether an auto item for key may still be filed for the
// agent: nothing was filed under key before (in any state, so a closed item
// is not refiled), the agent has no active item of the hub's, and no person
// or agent has an active item waiting on it.
func (t *todoStore) wantsAuto(key, agentID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.data.Items {
		if e.AutoKey == key {
			return false
		}
		if e.active() && e.Waiting != nil && e.Waiting.ID == agentID {
			return false
		}
	}
	return true
}

// addAuto stores an auto item. It is exempt from the per-creator rate limit
// but not from the global cap, and it has caps of its own: autoMaxActive in
// all and autoMaxPerAgent for one agent. It refuses (errAutoHuman) when an
// item not made by the hub already waits on the same agent.
func (t *todoStore) addAuto(it Todo) (Todo, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	active, auto, perAgent := 0, 0, 0
	for _, e := range t.data.Items {
		if e.AutoKey != "" && e.AutoKey == it.AutoKey {
			return e, errAutoDup
		}
		if !e.active() {
			continue
		}
		active++
		if e.CreatedBy == AutoCreator {
			auto++
			if sameWaiting(e.Waiting, it.Waiting) {
				perAgent++
			}
		} else if e.Waiting != nil && it.Waiting != nil && e.Waiting.ID == it.Waiting.ID {
			return e, errAutoHuman
		}
	}
	switch {
	case active >= todoMaxActive:
		return it, errTodoFull
	case auto >= autoMaxActive, perAgent >= autoMaxPerAgent:
		return it, errAutoCap
	}
	now := t.now()
	it.ID, it.CreatedAt, it.UpdatedAt, it.State = newTodoID(), now, now, TodoOpen
	t.data.Items = append(t.data.Items, it)
	return it, t.save()
}

// ---- what agents may read ----

// autoStub is all an agent-role reader sees of an auto item about a session
// that is neither a handoff worker nor a coordinator: that it exists, not
// what the session said or ran. The app shows the full item.
func autoStub(it Todo) Todo {
	name := "a session"
	if it.Waiting != nil && it.Waiting.Name != "" {
		name = "session " + oneLine(it.Waiting.Name, 60)
	}
	out := Todo{ID: it.ID, Kind: it.Kind, State: it.State, Source: it.Source, Suggested: it.Suggested,
		CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt, CreatedBy: it.CreatedBy, ClosedBy: it.ClosedBy, ClosedAt: it.ClosedAt,
		Question: name + " is waiting on you (details are in the app)", Waiting: it.Waiting}
	return out
}

// autoShownToAgent: the item may go to an agent-role reader whole.
func (s *Server) autoShownToAgent(it Todo) bool {
	if it.Source == "" || it.Source == SourceAgent || it.Waiting == nil {
		return true
	}
	if s.isCoordinator(it.Waiting.ID, it.Waiting.Name) {
		return true
	}
	if s.ledger != nil {
		for _, e := range s.ledger.Entries() {
			if e.AgentID == it.Waiting.ID {
				return true
			}
		}
	}
	return false
}

// forReader prepares items for the caller: agent roles get stubs for the
// auto items about sessions that are not handoff workers or coordinators.
func (s *Server) todosForReader(r *http.Request, items []Todo) []Todo {
	if !callerOf(r).agent() {
		return items
	}
	out := make([]Todo, len(items))
	for i, it := range items {
		if s.autoShownToAgent(it) {
			out[i] = it
		} else {
			out[i] = autoStub(it)
		}
	}
	return out
}

// ---- B2/E2: an agent idle on a question ----

func autoQuestionKey(a herdr.AgentInfo, since time.Time) string {
	return SourceQuestion + ":" + state.AgentKey(&a) + "@" + since.UTC().Format(time.RFC3339)
}

// autoQuestion files a suggestion for an agent that has sat idle past the
// threshold with a question as its last message. It reports whether it
// handled the agent (read or decided something), so the caller can count
// the reads. It reads the transcript only when every cheap check passes,
// and once per idle episode.
func (s *Server) autoQuestion(ctx context.Context, a herdr.AgentInfo, now time.Time, reads *int) bool {
	au := s.auto
	since := autoSince(a)
	if since == nil || now.Sub(*since) < au.cfg.Question {
		return false
	}
	id := state.AgentKey(&a)
	if au.isMuted(id) || (au.viewing != nil && au.viewing(a)) {
		return false
	}
	key := autoQuestionKey(a, *since)
	au.mu.Lock()
	done := au.checked[id] == key
	au.mu.Unlock()
	if done || !s.todos.wantsAuto(key, id) {
		return false
	}
	*reads++
	latest := au.latest(ctx, a)
	var res question.Result
	if latest != nil {
		res = question.Classify(latest.Text)
	}
	au.mu.Lock()
	au.checked[id] = key // judged, whatever the verdict
	au.mu.Unlock()
	if !res.Ask {
		return true
	}
	// E2: an item the agent filed itself during that turn already says it.
	from := now
	if t, err := time.Parse(time.RFC3339Nano, latest.Time); err == nil {
		from = t
	} else if since != nil {
		from = *since
	}
	if by := s.todos.filedBy(id, from.Add(-autoTurnWindow)); by != "" {
		return true
	}
	name := autoName(a)
	it := Todo{
		Question: oneLine(name+" asks: "+res.Question, todoMaxQuestion),
		Context: block(fmt.Sprintf("Suggested by the hub: **%s** has been idle for %s with this as its last message.\n\n%s\n\n*Keep it to make it a real item; Dismiss it if it is not waiting on you.*",
			mdSafe(name), roundMinutes(now.Sub(*since)), quoteBlock(res.Block)), todoMaxContext),
		Kind:      "decision",
		Links:     []TodoLink{{Label: "Open " + oneLine(name, 60), URL: s.autoAgentLink(a)}},
		Waiting:   &TodoAgent{ID: id, Name: a.Name, Instance: s.hostInstance(a.Host)},
		CreatedBy: AutoCreator,
		Source:    SourceQuestion,
		AutoKey:   key,
		Suggested: true,
	}
	out, err := s.todos.addAuto(it)
	switch {
	case err == nil:
		log.Printf("auto to-do: suggested %s for %s (idle since %s)", out.ID, id, since.Format(time.RFC3339))
	case errors.Is(err, errAutoCap), errors.Is(err, errAutoHuman), errors.Is(err, errAutoDup), errors.Is(err, errTodoFull):
	default:
		log.Printf("auto to-do: %s: %v", id, err)
	}
	return true
}

func quoteBlock(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r", ""), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// filedBy returns the id of an item filed (not by the hub) by the agent at
// or after from, "" when there is none. Items of any state count: a
// coordinator that filed one and saw it answered has still said it.
func (t *todoStore) filedBy(agentID string, from time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.data.Items {
		if e.CreatedByID == agentID && e.CreatedBy != AutoCreator && !e.CreatedAt.Before(from) {
			return e.ID
		}
	}
	return ""
}

// ---- keep and dismiss ----

// autoStat counts what the user did with the hub's items, the tuning data
// for the thresholds and the classifier: kept:<source>, dismissed:<source>
// and dismissed:<source>:<why>. The counts outlive the items (which are
// pruned after todoRetention).
func (t *todoStore) countLocked(key string) {
	if t.data.Auto == nil {
		t.data.Auto = map[string]int{}
	}
	t.data.Auto[key]++
}

// keep turns a suggestion into a plain item. It reports errTodoClosed for
// an item that is closed.
func (t *todoStore) keep(id string) (Todo, error) {
	return t.mutate(id, func(it *Todo) error {
		if !it.active() {
			return errTodoClosed
		}
		if it.Suggested {
			it.Suggested = false
			t.countLocked("kept:" + it.Source)
		}
		return nil
	})
}

// dismissReasons are what the user may say about a suggestion.
var dismissReasons = map[string]string{
	"not_a_question":  "not a question for me",
	"already_handled": "already handled",
	"other":           "",
}

// dismiss drops a suggestion and counts it by source and reason.
func (t *todoStore) dismiss(id, by, why string) (Todo, error) {
	return t.mutate(id, func(it *Todo) error {
		if !it.active() {
			return errTodoClosed
		}
		now := t.now()
		reason := "dismissed"
		if txt := dismissReasons[why]; txt != "" {
			reason += ": " + txt
		}
		it.State, it.ClosedBy, it.ClosedAt, it.DropReason, it.Notice = TodoDropped, by, &now, reason, nil
		key := "dismissed:" + it.Source
		t.countLocked(key)
		if why != "" && why != "other" {
			t.countLocked(key + ":" + why)
		}
		return nil
	})
}

// autoStats copies the counters.
func (t *todoStore) autoStats() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int, len(t.data.Auto))
	for k, v := range t.data.Auto {
		out[k] = v
	}
	return out
}

// handleTodoKeep: POST /api/todos/{id}/keep makes a suggestion a real item.
func (s *Server) handleTodoKeep(w http.ResponseWriter, r *http.Request) {
	if s.todoOff(w, r) || !s.mayManage(w, r) {
		return
	}
	it, err := s.todos.keep(r.PathValue("id"))
	if err != nil {
		todoErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"todo": s.todoGone(it)})
}

// handleTodoDismiss: POST /api/todos/{id}/dismiss {reason} drops a
// suggestion and counts it. reason: not_a_question, already_handled or other.
func (s *Server) handleTodoDismiss(w http.ResponseWriter, r *http.Request) {
	if s.todoOff(w, r) || !s.mayManage(w, r) {
		return
	}
	var b struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b)
	}
	if _, ok := dismissReasons[b.Reason]; !ok && b.Reason != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_reason", "message": "reason is not_a_question, already_handled or other"})
		return
	}
	it, err := s.todos.dismiss(r.PathValue("id"), s.by(r, ""), b.Reason)
	if err != nil {
		todoErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"todo": s.todoGone(it)})
}
