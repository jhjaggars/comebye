package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// The sweep loop. The hub runs the sweep (sweep.go) on a schedule, so finished
// sessions with nothing to lose are marked ready to remove without anyone
// typing `comebye sweep`. It lives in the hub because the hub already owns the
// ledger, the fleet and the clock, every install gets it, and there is no unit
// file to keep in step with the binary.
//
// What it does, and what it never does:
//
//   - It only marks. A mark is the ordinary one (the session is asked what is
//     left; the existing automatic removal, autoremove.go, or the user's tap
//     in the removals review does the removing). Nothing here removes.
//   - It is idempotent: a sweep skips what is already marked, so a second run
//     marks nothing new, and a run that marks nothing adds nothing to the log.
//   - It is cheap: one sweep an hour (setting), the same checks `comebye
//     sweep` makes, four hosts' worth at a time.
//   - It files at most one To do item a day: "N sessions are ready to remove",
//     linking the removals review, whose "Remove all safe" is the one confirm.
//     While that item is open the loop only keeps its count right, and it
//     closes it when nothing is left.
//
// Its state (the last run, what it marked, the item it filed) is sweeploop.json
// in the state directory, so a restart neither repeats a sweep nor files a
// second item the same day. The setting (on or off, how often) is in
// settings.json, edited in the app's Peers screen.

const (
	// SourceSweep marks the To do item the sweep loop filed.
	SourceSweep = "hub-sweep"
	evSweep     = "sweep" // a sweep marked sessions ready to remove

	defaultSweepEvery = time.Hour
	minSweepEvery     = 15 * time.Minute
	maxSweepEvery     = 24 * time.Hour
	sweepLoopTick     = time.Minute
	// sweepLoopIdle is the idle time after which a worker still recorded as
	// running (and an agent started by hand) counts as finished: longer than
	// `comebye sweep`'s 30 minutes, because nobody is watching this one.
	sweepLoopIdle = 2 * time.Hour
	// sweepTodoEvery is the most often a new item is filed.
	sweepTodoEvery = 24 * time.Hour
	// sweepTodoGrace is how long a mark waits before it counts: the session
	// is asked first, and the automatic removal gets its look.
	sweepTodoGrace = 20 * time.Minute
	// sweepTodoLook is how often an open item's count is checked again.
	sweepTodoLook = 20 * time.Minute
	// sweepTodoList is how many sessions the item's text names.
	sweepTodoList  = 20
	sweepKeepNames = 12
	// SweepReviewLink is the app's removals review.
	SweepReviewLink = "/#/remove"
)

// sweepLoopSettings is the "sweep_loop" part of settings.json. A nil Enabled
// is the default (on); a nil EveryMinutes is an hour.
type sweepLoopSettings struct {
	Enabled      *bool `json:"enabled,omitempty"`
	EveryMinutes *int  `json:"every_minutes,omitempty"`
}

func normalizeSweepLoop(in *sweepLoopSettings) *sweepLoopSettings {
	if in == nil {
		return nil
	}
	out := &sweepLoopSettings{}
	if in.Enabled != nil {
		v := *in.Enabled
		out.Enabled = &v
	}
	if in.EveryMinutes != nil {
		n := min(max(*in.EveryMinutes, int(minSweepEvery/time.Minute)), int(maxSweepEvery/time.Minute))
		out.EveryMinutes = &n
	}
	if out.Enabled == nil && out.EveryMinutes == nil {
		return nil
	}
	return out
}

// sweepLoopConf is the loop's setting in force.
type sweepLoopConf struct {
	On     bool          `json:"enabled"`
	Every  time.Duration `json:"-"`
	Source string        `json:"source"` // "setting" or "default"
}

func (s *Server) sweepLoopConf() sweepLoopConf {
	c := sweepLoopConf{On: true, Every: defaultSweepEvery, Source: "default"}
	if v := s.settings.get().SweepLoop; v != nil {
		if v.Enabled != nil {
			c.On, c.Source = *v.Enabled, "setting"
		}
		if v.EveryMinutes != nil {
			c.Every, c.Source = time.Duration(*v.EveryMinutes)*time.Minute, "setting"
		}
	}
	return c
}

// sweepSession is one session a sweep marked, as the board and the item name it.
type sweepSession struct {
	Name   string `json:"name"`
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// sweepLoopState is sweeploop.json.
type sweepLoopState struct {
	LastRunAt     time.Time      `json:"last_run_at"`
	Runs          int            `json:"runs"`
	LastMarked    int            `json:"last_marked"`
	LastNames     []sweepSession `json:"last_names,omitempty"`
	LastNeedsLook int            `json:"last_needs_look"`
	LastError     string         `json:"last_error,omitempty"`
	// TodoID and TodoAt are the item last filed and when.
	TodoID   string    `json:"todo_id,omitempty"`
	TodoAt   time.Time `json:"todo_at"`
	TodoLook time.Time `json:"todo_look"`
}

type sweepLoop struct {
	mu      sync.Mutex
	path    string // "" keeps it in memory
	st      sweepLoopState
	running bool
}

func openSweepLoop(path string) *sweepLoop {
	l := &sweepLoop{path: path}
	if path == "" {
		return l
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("sweep loop: %v (starting with none)", err)
		}
		return l
	}
	if err := json.Unmarshal(b, &l.st); err != nil {
		log.Printf("sweep loop: %s: %v (starting with none)", path, err)
		l.st = sweepLoopState{}
	}
	return l
}

// saveLocked writes the state (best effort: a failed write only means a
// restart may sweep once more).
func (l *sweepLoop) saveLocked() {
	if l.path == "" {
		return
	}
	b, err := json.MarshalIndent(l.st, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(l.path), 0o700)
	}
	if err == nil {
		tmp := l.path + ".tmp"
		if err = os.WriteFile(tmp, append(b, '\n'), 0o600); err == nil {
			err = os.Rename(tmp, l.path)
		}
	}
	if err != nil {
		log.Printf("sweep loop: save: %v", err)
	}
}

func (l *sweepLoop) get() sweepLoopState {
	if l == nil {
		return sweepLoopState{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.st
	st.LastNames = append([]sweepSession(nil), st.LastNames...)
	return st
}

// RunSweepLoop checks the clock every minute until ctx is done; the sweep and
// the item are decided in sweepLoopStep.
func (s *Server) RunSweepLoop(ctx context.Context) {
	if s.ledger == nil || s.sweeps == nil {
		return
	}
	tick := time.NewTicker(sweepLoopTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.sweepLoopStep(ctx, time.Now())
		}
	}
}

// sweepLoopStep is one look at the clock: sweep when one is due, then keep the
// To do item right. It reports whether a sweep ran. One step at a time.
func (s *Server) sweepLoopStep(ctx context.Context, now time.Time) bool {
	l := s.sweeps
	if l == nil || s.ledger == nil {
		return false
	}
	conf := s.sweepLoopConf()
	if !conf.On {
		return false
	}
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return false
	}
	l.running = true
	due := l.st.LastRunAt.IsZero() || now.Sub(l.st.LastRunAt) >= conf.Every
	l.mu.Unlock()
	defer func() { l.mu.Lock(); l.running = false; l.mu.Unlock() }()
	if due {
		s.sweepOnce(ctx, now)
	}
	s.sweepTodoStep(ctx, now, due)
	return due
}

// sweepOnce runs one sweep as the hub and records it.
func (s *Server) sweepOnce(ctx context.Context, now time.Time) sweepResult {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	res := s.sweep(ctx, sweepBody{IdleAfter: sweepLoopIdle.String()}, "", AutoCreator)
	var names []sweepSession
	for _, g := range [][]sweepItem{res.Ready, res.Gone} {
		for _, it := range g {
			if it.Marked {
				names = append(names, sweepSession{Name: orDefault(it.Name, it.ID), Repo: it.Repo, Branch: it.Branch})
			}
		}
	}
	l := s.sweeps
	l.mu.Lock()
	l.st.LastRunAt, l.st.LastError = now, ""
	l.st.Runs++
	l.st.LastMarked, l.st.LastNeedsLook = len(names), len(res.NeedsLook)
	l.st.LastNames = names[:min(len(names), sweepKeepNames)]
	l.saveLocked()
	l.mu.Unlock()
	if len(names) > 0 {
		list := make([]string, 0, len(names))
		for _, n := range names[:min(len(names), 6)] {
			list = append(list, safeText(n.Name))
		}
		more := ""
		if len(names) > len(list) {
			more = fmt.Sprintf(" and %d more", len(names)-len(list))
		}
		s.logEvent(evSweep, "", "", "", fmt.Sprintf("Sweep marked %d %s ready to remove: %s%s.", len(names), plural(len(names), "session", "sessions"), strings.Join(list, ", "), more))
	}
	return res
}

// sweepTodoPrefilter keeps the marked sessions that could be in the item: not a
// coordinator, not a proposal to discard work, not waiting on a session's
// answer or holding an UNSAFE one, and marked long enough ago for the ask and
// the automatic removal to have had their turn.
func (s *Server) sweepTodoPrefilter(now time.Time) []*removal {
	var out []*removal
	for _, it := range s.marked() {
		m := it.Mark
		if it.Coordinator || m.Discard || now.Sub(m.At) < sweepTodoGrace {
			continue
		}
		if m.Ask != nil && (m.Ask.State == ledger.AskAsking || m.Ask.State == ledger.AskUnsafe) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// sweepReady reviews the prefiltered sessions the way the removals review does
// and keeps the ones its "Remove all safe" would take.
func (s *Server) sweepReady(ctx context.Context, cands []*removal) []*removal {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, it := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.review(ctx, it)
		}()
	}
	wg.Wait()
	var out []*removal
	for _, it := range cands {
		if it.Safe && !it.DeliveredDiscard && !it.Coordinator {
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Mark.At.Before(out[j].Mark.At) })
	return out
}

// sweepTodoText is the item's question and Markdown context for the sessions.
func sweepTodoText(ready []*removal) (question, body string) {
	n := len(ready)
	if n == 1 {
		question = "1 session is ready to remove"
	} else {
		question = fmt.Sprintf("%d sessions are ready to remove", n)
	}
	var b strings.Builder
	b.WriteString("These sessions are marked ready to remove, and the hub's checks found nothing to lose in them: each is clean, its work is in the base branch, and nothing is unpushed. Nothing is removed until you confirm.\n\n")
	for i, it := range ready {
		if i >= sweepTodoList {
			fmt.Fprintf(&b, "- … and %d more\n", n-sweepTodoList)
			break
		}
		line := "- **" + mdSafe(orDefault(it.Name, it.ID)) + "**"
		if it.Repo != "" || it.Branch != "" {
			line += " · " + mdSafe(strings.Trim(it.Repo+" "+it.Branch, " "))
		}
		if r := strings.TrimSpace(strings.TrimPrefix(it.Mark.Reason, "Sweep: ")); r != "" {
			line += ": " + mdSafe(r)
		}
		b.WriteString(oneLine(line, 240) + "\n")
	}
	b.WriteString("\nOpen the removals review and tap **Remove all safe**, then confirm once. Sessions that need a look are not in this list; they stay where they are.")
	return question, block(b.String(), todoMaxContext)
}

// sweepActiveItem is the open item the loop filed, if there is one.
func (s *Server) sweepActiveItem() (Todo, bool) {
	for _, t := range s.todos.list("") {
		if t.Source == SourceSweep && t.active() {
			return t, true
		}
	}
	return Todo{}, false
}

// sweepTodoStep files the day's item, keeps an open one's count right, and
// closes it when nothing is left to remove. forced: a sweep just ran, so look
// now rather than at the next sweepTodoLook.
func (s *Server) sweepTodoStep(ctx context.Context, now time.Time, forced bool) {
	if s.todos == nil {
		return
	}
	l := s.sweeps
	st := l.get()
	open, have := s.sweepActiveItem()
	if !have && now.Sub(st.TodoAt) < sweepTodoEvery {
		return // filed a day ago or less, and the user dealt with it: one a day
	}
	if !forced && now.Sub(st.TodoLook) < sweepTodoLook {
		return
	}
	l.mu.Lock()
	l.st.TodoLook = now
	l.saveLocked()
	l.mu.Unlock()
	ready := s.sweepReady(ctx, s.sweepTodoPrefilter(now))
	switch {
	case have && len(ready) == 0:
		if _, err := s.todos.close(open.ID, TodoDone, AutoCreator, "nothing is left to remove"); err != nil {
			log.Printf("sweep loop: close %s: %v", open.ID, err)
		}
	case have:
		q, c := sweepTodoText(ready)
		if q == open.Question && c == open.Context {
			return
		}
		if _, err := s.todos.mutate(open.ID, func(it *Todo) error {
			if !it.active() {
				return errTodoClosed
			}
			it.Question, it.Context = q, c
			return nil
		}); err != nil {
			log.Printf("sweep loop: update %s: %v", open.ID, err)
		}
	case len(ready) > 0:
		q, c := sweepTodoText(ready)
		it, _, err := s.todos.add(Todo{
			Question: q, Context: c, Kind: "action",
			Links:     []TodoLink{{Label: "Open the removals review", URL: SweepReviewLink}},
			CreatedBy: AutoCreator, Source: SourceSweep, AutoKey: SourceSweep + ":" + now.UTC().Format("2006-01-02"),
		})
		if err != nil {
			log.Printf("sweep loop: file the item: %v", err)
			return
		}
		l.mu.Lock()
		l.st.TodoID, l.st.TodoAt = it.ID, now
		l.saveLocked()
		l.mu.Unlock()
	}
}

// sweepBoardLine is the "Hub did" lane's line for the loop: when it last ran
// and what it marked. ok is false when there is nothing to say (it has not
// run, or it is off and never ran).
func (s *Server) sweepBoardLine() (title string, at time.Time, ok bool) {
	st := s.sweeps.get()
	if st.LastRunAt.IsZero() {
		return "", time.Time{}, false
	}
	var parts []string
	if st.LastMarked == 0 {
		parts = append(parts, "marked nothing new")
	} else {
		names := make([]string, 0, 3)
		for _, n := range st.LastNames[:min(len(st.LastNames), 3)] {
			names = append(names, safeText(n.Name))
		}
		p := fmt.Sprintf("marked %d ready to remove (%s", st.LastMarked, strings.Join(names, ", "))
		if st.LastMarked > len(names) {
			p += fmt.Sprintf(", +%d", st.LastMarked-len(names))
		}
		parts = append(parts, p+")")
	}
	if st.LastNeedsLook > 0 {
		parts = append(parts, fmt.Sprintf("%d need a look", st.LastNeedsLook))
	}
	title = "Last sweep: " + strings.Join(parts, "; ")
	if !s.sweepLoopConf().On {
		title += " (the sweep is off)"
	}
	return title, st.LastRunAt, true
}

// sweepLoopView is what the settings answer says about the loop.
type sweepLoopView struct {
	Enabled      bool       `json:"enabled"`
	EveryMinutes int        `json:"every_minutes"`
	Source       string     `json:"source"`
	LastRunAt    *time.Time `json:"last_run_at,omitempty"`
	LastMarked   int        `json:"last_marked"`
	NeedsLook    int        `json:"last_needs_look"`
	NextRunAt    *time.Time `json:"next_run_at,omitempty"`
	TodoAt       *time.Time `json:"todo_at,omitempty"`
	// Hub: this host has no ledger, so it cannot sweep.
	Hub bool `json:"hub"`
}

func (s *Server) sweepLoopView() sweepLoopView {
	c := s.sweepLoopConf()
	v := sweepLoopView{Enabled: c.On, EveryMinutes: int(c.Every / time.Minute), Source: c.Source, Hub: s.ledger != nil}
	st := s.sweeps.get()
	if !st.LastRunAt.IsZero() {
		t := st.LastRunAt
		v.LastRunAt, v.LastMarked, v.NeedsLook = &t, st.LastMarked, st.LastNeedsLook
		if c.On {
			n := t.Add(c.Every)
			v.NextRunAt = &n
		}
	}
	if !st.TodoAt.IsZero() {
		t := st.TodoAt
		v.TodoAt = &t
	}
	return v
}
