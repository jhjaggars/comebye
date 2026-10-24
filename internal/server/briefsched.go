package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// Scheduled briefings. Off by default; when on, the hub asks a live
// coordinator to publish a briefing at fixed slots (every N, optionally
// anchored to a time of day). The coordinator writes it; the hub only
// prompts. A slot is satisfied by any briefing published at or after it
// (one asked for by hand too). A slot that cannot be satisfied is missed:
// no coordinator was running (counted at once, so the app shows it), the
// coordinator stayed busy until the next slot, or it was prompted and
// published nothing. Misses form a streak that the app shows once (next
// to the stale briefing) until a briefing is published; dismissing it, on
// the hub, hides that streak only. Missed slots are never replayed, and
// nothing is pushed for them.

const (
	minBriefEvery = time.Hour
	maxBriefEvery = 30 * 24 * time.Hour
	// briefTickEvery paces the scheduler.
	briefTickEvery = time.Minute
)

// Why a scheduled briefing was missed.
const (
	missNoCoordinator = "no_coordinator"
	missBusy          = "busy"
	missNoBriefing    = "no_briefing"
)

// BriefSchedule is the schedule's configuration.
type BriefSchedule struct {
	Enabled bool `json:"enabled"`
	// Every is a Go duration ("24h"), 1h to 30 days.
	Every string `json:"every,omitempty"`
	// At anchors the slots to a time of day ("08:00") in TZ; without it
	// they count from when the schedule was set.
	At string `json:"at,omitempty"`
	// TZ is an IANA zone ("" is the hub's local time).
	TZ      string    `json:"tz,omitempty"`
	By      string    `json:"by,omitempty"`
	Updated time.Time `json:"updated"`
}

// briefMiss is a streak of missed scheduled briefings.
type briefMiss struct {
	ID     string    `json:"id"`    // the first missed slot, RFC 3339
	Since  time.Time `json:"since"` // the first missed slot
	Count  int       `json:"count"`
	Reason string    `json:"reason"` // the latest miss's
}

// briefRun is the scheduler's state.
type briefRun struct {
	// Floor: slots at or before it do not count (a schedule just set does not
	// fire for the slot already under way).
	Floor time.Time `json:"floor"`
	// Slot is the slot being worked on; Counted when it was already
	// counted as missed, Waiting why no coordinator was prompted yet.
	Slot       time.Time  `json:"slot,omitzero"`
	Counted    bool       `json:"counted,omitempty"`
	Waiting    string     `json:"waiting,omitempty"`
	PromptedAt *time.Time `json:"prompted_at,omitempty"`
	PromptedTo string     `json:"prompted_to,omitempty"`
	Missed     *briefMiss `json:"missed,omitempty"`
	// Dismissed is the id of the streak the user dismissed.
	Dismissed string `json:"dismissed,omitempty"`
}

type briefSchedFile struct {
	Schedule BriefSchedule `json:"schedule"`
	Run      briefRun      `json:"run"`
}

// briefSchedStore keeps brief-schedule.json in the state directory.
type briefSchedStore struct {
	mu   sync.Mutex
	path string
	data briefSchedFile
}

func openBriefSched(path string) (*briefSchedStore, error) {
	s := &briefSchedStore{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return s, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

func (s *briefSchedStore) get() briefSchedFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

// update changes the state under the lock and saves it when fn reports a
// change.
func (s *briefSchedStore) update(fn func(*briefSchedFile) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	if !fn(&next) {
		return nil
	}
	if err := writeFileAtomic(s.path, next); err != nil {
		return err
	}
	s.data = next
	return nil
}

var briefAtRE = regexp.MustCompile(`^([01]?[0-9]|2[0-3]):([0-5][0-9])$`)

// schedPlan is a validated schedule.
type schedPlan struct {
	every  time.Duration
	hasAt  bool
	hh, mm int
	loc    *time.Location
	since  time.Time // when it was set: the anchor without At
}

func planOf(c BriefSchedule) (schedPlan, error) {
	p := schedPlan{since: c.Updated}
	every, err := time.ParseDuration(c.Every)
	if err != nil {
		return p, fmt.Errorf("every must be a duration like 24h or 12h")
	}
	if every < minBriefEvery || every > maxBriefEvery {
		return p, fmt.Errorf("every must be between 1h and 720h (30 days)")
	}
	p.every = every.Truncate(time.Minute)
	if c.At != "" {
		m := briefAtRE.FindStringSubmatch(c.At)
		if m == nil {
			return p, fmt.Errorf("at must be a time of day like 08:00")
		}
		p.hasAt = true
		p.hh, _ = strconv.Atoi(m[1])
		p.mm, _ = strconv.Atoi(m[2])
	}
	p.loc = time.Local
	if c.TZ != "" {
		if p.loc, err = time.LoadLocation(c.TZ); err != nil {
			return p, fmt.Errorf("unknown time zone %q", c.TZ)
		}
	}
	return p, nil
}

// days is the interval in whole days when the slots follow the calendar
// (a time of day, every N days: 08:00 stays 08:00 across DST), else 0.
func (p schedPlan) days() int {
	if p.hasAt && p.every%(24*time.Hour) == 0 {
		return int(p.every / (24 * time.Hour))
	}
	return 0
}

// civilDay numbers t's calendar date in loc.
func civilDay(t time.Time, loc *time.Location) int {
	y, m, d := t.In(loc).Date()
	return int(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// slotAt is the latest slot at or before now.
func (p schedPlan) slotAt(now time.Time) time.Time {
	if n := p.days(); n > 0 {
		ref := p.since.In(p.loc)
		k := floorDiv(int64(civilDay(now, p.loc)-civilDay(ref, p.loc)), int64(n))
		y, m, d := ref.Date()
		slot := time.Date(y, m, d+int(k)*n, p.hh, p.mm, 0, 0, p.loc)
		if slot.After(now) {
			slot = time.Date(y, m, d+int(k-1)*n, p.hh, p.mm, 0, 0, p.loc)
		}
		return slot
	}
	anchor := p.since
	if p.hasAt {
		y, m, d := p.since.In(p.loc).Date()
		anchor = time.Date(y, m, d, p.hh, p.mm, 0, 0, p.loc)
	}
	k := floorDiv(int64(now.Sub(anchor)), int64(p.every))
	return anchor.Add(time.Duration(k) * p.every)
}

// nextAfter is the slot after slot.
func (p schedPlan) nextAfter(slot time.Time) time.Time {
	if n := p.days(); n > 0 {
		t := slot.In(p.loc)
		y, m, d := t.Date()
		return time.Date(y, m, d+n, p.hh, p.mm, 0, 0, p.loc)
	}
	return slot.Add(p.every)
}

// addMiss counts slot as missed for reason.
func (r *briefRun) addMiss(slot time.Time, reason string) {
	if r.Missed == nil {
		r.Missed = &briefMiss{ID: slot.UTC().Format(time.RFC3339), Since: slot.UTC()}
	}
	r.Missed.Count++
	r.Missed.Reason = reason
}

// RunBriefSchedule runs the scheduled briefings until ctx is done (the
// hub only; it returns at once elsewhere).
func (s *Server) RunBriefSchedule(ctx context.Context) {
	if s.briefSched == nil {
		return
	}
	t := time.NewTicker(briefTickEvery)
	defer t.Stop()
	for {
		s.briefTick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// briefTick does what the schedule asks for at now: close the previous
// slot, clear a streak a briefing ended, prompt a coordinator for the
// current slot or count it as missed.
func (s *Server) briefTick(ctx context.Context, now time.Time) {
	cur := s.briefSched.get()
	if !cur.Schedule.Enabled {
		return
	}
	plan, err := planOf(cur.Schedule)
	if err != nil {
		log.Printf("brief schedule: %v", err)
		return
	}
	var briefAt time.Time
	if b, err := s.brief.get(); err != nil {
		log.Printf("brief schedule: %v", err)
		return
	} else if b != nil {
		briefAt = b.At
	}
	slot := plan.slotAt(now)

	// The decision is made on a copy; the prompt goes out between two
	// updates so the lock is not held across the network.
	var target *herdr.AgentInfo
	err = s.briefSched.update(func(f *briefSchedFile) bool {
		r := &f.Run
		before, _ := json.Marshal(r)
		if !r.Slot.IsZero() && slot.After(r.Slot) {
			if briefAt.Before(r.Slot) && !r.Counted {
				reason := missBusy
				if r.PromptedAt != nil {
					reason = missNoBriefing
				}
				r.addMiss(r.Slot, reason)
			}
			r.Slot, r.Counted, r.Waiting, r.PromptedAt, r.PromptedTo = time.Time{}, false, "", nil, ""
		}
		if r.Missed != nil && !briefAt.Before(r.Missed.Since) {
			r.Missed = nil
		}
		// The slot under way when the schedule was set does not fire.
		early := !slot.After(r.Floor)
		if early || !briefAt.Before(slot) {
			if !early {
				// Done: a briefing came after the slot.
				r.Slot, r.Waiting, r.PromptedAt, r.PromptedTo = slot, "", nil, ""
				r.Counted = true
			}
			after, _ := json.Marshal(r)
			return string(before) != string(after)
		}
		r.Slot = slot
		if r.PromptedAt == nil {
			a, why := s.briefCoordinator()
			switch {
			case a != nil:
				target = a
			case why == missNoCoordinator && !r.Counted:
				r.addMiss(slot, missNoCoordinator)
				r.Counted = true
				r.Waiting = why
			default:
				r.Waiting = why
			}
		}
		after, _ := json.Marshal(r)
		return string(before) != string(after)
	})
	if err != nil {
		log.Printf("brief schedule: %v", err)
		return
	}
	if target == nil {
		return
	}
	if err := s.promptBrief(ctx, *target, plan, briefAt); err != nil {
		log.Printf("brief schedule: prompting %s: %v", target.ID, err)
		_ = s.briefSched.update(func(f *briefSchedFile) bool {
			f.Run.Waiting = missBusy
			return true
		})
		return
	}
	at := now.UTC()
	err = s.briefSched.update(func(f *briefSchedFile) bool {
		if !f.Run.Slot.Equal(slot) {
			return false
		}
		f.Run.PromptedAt, f.Run.PromptedTo, f.Run.Waiting = &at, target.DisplayName()+" ("+target.ID+")", ""
		return true
	})
	if err != nil {
		log.Printf("brief schedule: %v", err)
	}
}

// briefCoordinator picks the coordinator to prompt: a running one that is
// idle (or done), the hub's own first, then the most recently made one.
// Without one it says why: no coordinator running, or all busy.
func (s *Server) briefCoordinator() (*herdr.AgentInfo, string) {
	roles := s.liveRoles()
	type cand struct {
		a  herdr.AgentInfo
		at time.Time
	}
	var ready []cand
	running := false
	for _, a := range s.fleet.Agents() {
		r, ok := roles[a.ID]
		if !ok || r.Role != RoleCoordinator || (r.Name != "" && a.Name != r.Name) {
			continue
		}
		running = true
		if a.Status == herdr.StatusIdle || a.Status == herdr.StatusDone {
			ready = append(ready, cand{a, r.At})
		}
	}
	if len(ready) == 0 {
		if running {
			return nil, missBusy
		}
		return nil, missNoCoordinator
	}
	local := s.fleet.LocalName()
	sort.SliceStable(ready, func(i, j int) bool {
		if li, lj := ready[i].a.Host == local, ready[j].a.Host == local; li != lj {
			return li
		}
		return ready[i].at.After(ready[j].at)
	})
	return &ready[0].a, ""
}

// briefPrompt is what the scheduled run asks of the coordinator.
func briefPrompt(plan schedPlan, last time.Time) string {
	since := "No briefing has been published yet."
	if !last.IsZero() {
		since = "The last briefing was published " + last.In(plan.loc).Format("Mon Jan 2 15:04 MST") + "."
	}
	return "Scheduled briefing (sent by the Comebye hub, every " + fmtEvery(plan.every) + "). " + since +
		" Catch up from the ledger (`comebye ledger`) and the workers' reports since then, and publish a fresh" +
		" briefing with `comebye brief set` in the playbook's format. If nothing changed, publish a short one that says so."
}

// fmtEvery prints an interval the way people write it: 24h, 90m, 1h30m.
func fmtEvery(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// promptBrief sends the scheduled prompt to a coordinator on any host.
func (s *Server) promptBrief(ctx context.Context, a herdr.AgentInfo, plan schedPlan, last time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return s.promptAgent(ctx, a, briefPrompt(plan, last))
}

// promptAgent sends text to an agent on any host.
func (s *Server) promptAgent(ctx context.Context, a herdr.AgentInfo, text string) error {
	if a.Host == "" || a.Host == s.fleet.LocalName() {
		_, err := s.client.Prompt(ctx, a.PaneID, text, herdr.PromptOptions{})
		return err
	}
	body, _ := json.Marshal(promptBody{Text: text})
	status, data, err := s.fleet.CallAgent(ctx, a.Host, a.PaneID, "prompt", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%s answered %d: %s", a.Host, status, strings.TrimSpace(string(data)))
	}
	return nil
}

// briefSchedView is the schedule with its status, for the app and CLI.
type briefSchedView struct {
	BriefSchedule
	// State: off, ok (the current slot is done or not due), waiting (a
	// coordinator is busy), prompted (asked, not published yet).
	State      string     `json:"state"`
	Next       *time.Time `json:"next,omitempty"`
	PromptedAt *time.Time `json:"prompted_at,omitempty"`
	PromptedTo string     `json:"prompted_to,omitempty"`
	Missed     *missView  `json:"missed,omitempty"`
}

type missView struct {
	briefMiss
	Dismissed bool `json:"dismissed"`
}

func (s *Server) briefSchedView(now time.Time) briefSchedView {
	f := s.briefSched.get()
	v := briefSchedView{BriefSchedule: f.Schedule, State: "off"}
	if !f.Schedule.Enabled {
		return v
	}
	v.State = "ok"
	if plan, err := planOf(f.Schedule); err == nil {
		next := plan.nextAfter(plan.slotAt(now)).UTC()
		v.Next = &next
	}
	switch {
	case f.Run.PromptedAt != nil:
		v.State, v.PromptedAt, v.PromptedTo = "prompted", f.Run.PromptedAt, f.Run.PromptedTo
	case f.Run.Waiting == missBusy:
		v.State = "waiting"
	}
	if f.Run.Missed != nil {
		v.Missed = &missView{briefMiss: *f.Run.Missed, Dismissed: f.Run.Dismissed == f.Run.Missed.ID}
	}
	return v
}

// handleGetBriefSchedule answers {schedule}.
func (s *Server) handleGetBriefSchedule(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	writeJSON(w, map[string]any{"schedule": s.briefSchedView(time.Now())})
}

// handleSetBriefSchedule sets the schedule: POST {enabled, every, at,
// tz}. The user and coordinators alike.
func (s *Server) handleSetBriefSchedule(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b struct {
		Enabled bool   `json:"enabled"`
		Every   string `json:"every"`
		At      string `json:"at"`
		TZ      string `json:"tz"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	c := BriefSchedule{Enabled: b.Enabled, Every: strings.TrimSpace(b.Every), At: strings.TrimSpace(b.At), TZ: strings.TrimSpace(b.TZ), By: s.by(r, ""), Updated: now}
	// Turning it off with no interval keeps the last one for next time.
	keep := !c.Enabled && c.Every == "" && c.At == "" && c.TZ == ""
	if !keep {
		plan, err := planOf(c)
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_schedule", "message": err.Error()})
			return
		}
		c.Every = fmtEvery(plan.every)
		if plan.hasAt {
			c.At = fmt.Sprintf("%02d:%02d", plan.hh, plan.mm)
		}
	}
	err := s.briefSched.update(func(f *briefSchedFile) bool {
		if keep {
			c.Every, c.At, c.TZ = f.Schedule.Every, f.Schedule.At, f.Schedule.TZ
		}
		f.Schedule = c
		// A new schedule starts clean: no streak, and the slot under way
		// does not fire.
		f.Run = briefRun{Floor: now}
		return true
	})
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "brief_error", "message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"schedule": s.briefSchedView(now)})
}

// handleDismissBriefMiss hides the current streak of missed briefings
// (on the hub, so for every device): POST {id} with the streak's id.
func (s *Server) handleDismissBriefMiss(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	found := false
	err := s.briefSched.update(func(f *briefSchedFile) bool {
		m := f.Run.Missed
		if m == nil || (b.ID != "" && b.ID != m.ID) {
			return false
		}
		found = true
		f.Run.Dismissed = m.ID
		return true
	})
	switch {
	case err != nil:
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "brief_error", "message": err.Error()})
		return
	case !found:
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_missed_briefing", "message": "no missed scheduled briefing to dismiss"})
		return
	}
	writeJSON(w, map[string]any{"schedule": s.briefSchedView(time.Now())})
}
