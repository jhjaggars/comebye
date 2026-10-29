package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/push"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// Pusher delivers Web Push notifications (implemented by *push.Service).
// Notify must not block: the state manager calls it inline.
type Pusher interface {
	Active() bool
	ExcerptChars() int
	Notify(key string, msg push.Message, opts push.Options)
}

// Viewer reports whether some client is looking at an agent right now,
// and when one last did (implemented by *push.Presence). The key is what
// the client reported: the host-qualified agent ID (older clients: the
// bare pane ID).
type Viewer interface {
	Viewing(key string) bool
	LastViewed(key string) time.Time
}

// DefaultHoldFor bounds how long an unanswered notification holds back
// further ones for the same agent, so a notification swiped away without
// opening the app does not silence that agent forever.
const DefaultHoldFor = 12 * time.Hour

// Notifier sends notifications via ntfy (the optional fallback channel).
type Notifier struct {
	// BaseURL is the ntfy root, e.g. https://ntfy.sh. Empty disables notifications.
	BaseURL string
	// Topic is the ntfy topic to publish to.
	Topic string
	// ClickURL is the phone app URL. Notifications deep-link to
	// ClickURL + "/#/agent/<route>" so a tap opens that agent's chat.
	ClickURL string
	// MinInterval throttles repeat notifications for the same agent+kind.
	MinInterval time.Duration
	// ExcerptChars caps the last-assistant-message excerpt included in the
	// body. 0 sends no message text (only agent and repo names).
	ExcerptChars int

	client *http.Client
	mu     sync.Mutex // guards last; notify runs from several goroutines
	last   map[string]time.Time
}

// ntfyMessage is ntfy's JSON publish format (POST to the server root).
type ntfyMessage struct {
	Topic    string   `json:"topic"`
	Title    string   `json:"title,omitempty"`
	Message  string   `json:"message"`
	Tags     []string `json:"tags,omitempty"`
	Click    string   `json:"click,omitempty"`
	Priority int      `json:"priority,omitempty"`
}

func (n *Notifier) enabled() bool { return n != nil && n.BaseURL != "" && n.Topic != "" }

// allow applies per-agent+kind throttling and records the send.
func (n *Notifier) allow(key string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if t, ok := n.last[key]; ok && now.Sub(t) < n.MinInterval {
		return false
	}
	if n.last == nil {
		n.last = map[string]time.Time{}
	}
	if len(n.last) >= pruneAt {
		pruneBefore(n.last, now.Add(-n.MinInterval))
	}
	n.last[key] = now
	return true
}

// pruneAt is the size at which the per-agent time maps (throttle and hold)
// drop entries that no longer affect anything, so agents that come and go
// (or a misbehaving peer inventing pane ids) cannot grow them for good.
const pruneAt = 1024

// pruneBefore deletes the entries of m recorded before cutoff.
func pruneBefore(m map[string]time.Time, cutoff time.Time) {
	for k, t := range m {
		if t.Before(cutoff) {
			delete(m, k)
		}
	}
}

// agentURL is the deep link to one agent's detail view; route is the
// already-escaped part after "#/agent/".
func (n *Notifier) agentURL(route string) string {
	if n.ClickURL == "" {
		return ""
	}
	return strings.TrimRight(n.ClickURL, "/") + "/#/agent/" + route
}

// notify publishes one notification, subject to per-agent+kind throttling.
func (n *Notifier) notify(agent, kind string, msg ntfyMessage) {
	if !n.enabled() || !n.allow(agent+"/"+kind, time.Now()) {
		return
	}
	msg.Topic = n.Topic
	body, err := json.Marshal(msg)
	if err != nil {
		log.Printf("ntfy: encode: %v", err)
		return
	}
	// JSON publishing (rather than Title/Tags headers) keeps non-ASCII
	// agent names and message text intact.
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(n.BaseURL, "/")+"/", bytes.NewReader(body))
	if err != nil {
		log.Printf("ntfy: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	n.mu.Lock()
	if n.client == nil {
		n.client = &http.Client{Timeout: 10 * time.Second}
	}
	client := n.client
	n.mu.Unlock()
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("ntfy: send: %v", err)
		return
	}
	if resp.StatusCode >= 300 {
		log.Printf("ntfy: send: HTTP %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Ref identifies an agent for one notification.
type Ref struct {
	// Key is the host-qualified agent ID: the throttle bucket, the
	// notification tag, and what presence reports are matched against.
	Key string
	// Route is the escaped deep-link path after "#/agent/": "<pane>" for
	// this instance's own agents (old links keep working), "<host>/<pane>"
	// for a peer's.
	Route string
	// Local marks this instance's own agents, for which a presence report
	// of the bare pane ID (from an older client) also suppresses.
	Local bool
}

// LocalRef is the notification identity of one of this instance's agents.
func LocalRef(a *herdr.AgentInfo) Ref {
	return Ref{Key: AgentKey(a), Route: url.PathEscape(a.PaneID), Local: true}
}

// PeerRef is the notification identity of an agent on a peer host.
func PeerRef(a *herdr.AgentInfo) Ref {
	return Ref{Key: AgentKey(a), Route: url.PathEscape(a.Host) + "/" + url.PathEscape(a.PaneID)}
}

// LatestFunc fetches the tail of an agent's session for the notification
// excerpt; nil or a nil result means no excerpt.
type LatestFunc func(ctx context.Context) *transcript.Latest

// Dispatcher turns status transitions into notifications on every enabled
// channel (Web Push, ntfy), applying foreground suppression, per-agent
// collation (one unanswered notification per agent) and, through the
// channels, per-agent+kind throttling. One Dispatcher is shared by the
// local state manager and the hub's peers so throttling and presence are
// common to all of them.
type Dispatcher struct {
	Ntfy   *Notifier
	Push   Pusher
	Viewer Viewer
	// ShowHost prefixes the body with the agent's host (set when the
	// instance aggregates several hosts).
	ShowHost bool
	// ExcerptTimeout bounds the LatestFunc call (default 5s).
	ExcerptTimeout time.Duration
	// HoldFor is how long an agent's unanswered notification holds back
	// its next ones (default DefaultHoldFor; negative disables holding).
	HoldFor time.Duration
	// Kinds selects the notifications sent (KindBlocked, KindQuestion,
	// KindDone); nil sends all of them.
	Kinds map[string]bool
	// DoneDigest, when positive, collects "done" notifications for that
	// long after the first one and sends them as one (a single agent's
	// as usual). Questions are never delayed.
	DoneDigest time.Duration
	// BlockedDelay, when positive, holds a "needs input" notification
	// that long and drops it if the agent left blocked meanwhile (it
	// worked through the block on its own, or someone answered).
	BlockedDelay time.Duration
	// Quiet, when set, reports agents (by key) that Comebye itself is
	// talking to, such as a session asked whether it is safe to remove:
	// they get no notification for answering.
	Quiet func(key string) bool

	mu sync.Mutex
	// held maps an agent key to when it was last notified. Until the user
	// opens that agent in the app (a presence report after that time) or
	// it goes back to working, it gets no further notifications: one per
	// agent, not one per prompt.
	held map[string]time.Time
	now  func() time.Time
	// digest holds the "done" notifications waiting for the digest, by
	// agent key; digestArmed while its timer runs. after starts the timer
	// (time.AfterFunc; tests replace it).
	digest      map[string]digestEntry
	digestArmed bool
	// blocked holds the "needs input" notifications waiting out
	// BlockedDelay, by agent key; blockedSeq tells a timer whether its
	// entry is still the one it was started for.
	blocked    map[string]blockedEntry
	blockedSeq uint64
	after      func(time.Duration, func())
}

// blockedEntry is a "needs input" waiting out BlockedDelay.
type blockedEntry struct {
	a      herdr.AgentInfo
	ref    Ref
	latest LatestFunc
	seq    uint64
}

// afterFunc runs f after t (time.AfterFunc unless a test replaced it).
func (d *Dispatcher) afterFunc(t time.Duration, f func()) {
	if d.after != nil {
		d.after(t, f)
		return
	}
	time.AfterFunc(t, f)
}

// The kinds of notification, for Dispatcher.Kinds.
const (
	KindBlocked  = "blocked"  // the agent needs input
	KindQuestion = "question" // it finished on a question
	KindDone     = "done"     // it finished
	KindPlan     = "plan"     // every member of a plan finished, or the plan needs a look
)

// ParseKinds reads a comma-separated list of notification kinds
// ("blocked,question,done"; "none" for no notifications).
func ParseKinds(s string) (map[string]bool, error) {
	out := map[string]bool{}
	if strings.TrimSpace(s) == "none" {
		return out, nil
	}
	for _, k := range strings.Split(s, ",") {
		switch k = strings.ToLower(strings.TrimSpace(k)); k {
		case KindBlocked, KindQuestion, KindDone, KindPlan:
			out[k] = true
		case "":
		default:
			return nil, fmt.Errorf("unknown notification kind %q (want blocked, question, done, plan or none)", k)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no notification kinds (use none to turn them all off)")
	}
	return out, nil
}

// NotifyPlan tells the user a plan settled. It has its own throttle key
// and none of the per-agent holds: it is about the change, not about one
// agent the user may already have a notification for.
func (d *Dispatcher) NotifyPlan(id, title, body string) {
	if d == nil || !d.kindOn(KindPlan) {
		return
	}
	ntfyOn, pushOn := d.channels()
	key := "plan/" + id
	if pushOn {
		d.Push.Notify(key+"/"+KindPlan, push.Message{Title: title, Body: body, URL: "/", Tag: key},
			push.Options{Urgency: "normal", TTL: 12 * time.Hour, Topic: push.TopicFor(key)})
	}
	if ntfyOn {
		msg := ntfyMessage{Title: title, Message: body, Tags: []string{"white_check_mark"}}
		if d.Ntfy.ClickURL != "" {
			msg.Click = strings.TrimRight(d.Ntfy.ClickURL, "/") + "/"
		}
		d.Ntfy.notify(key, KindPlan, msg)
	}
}

func (d *Dispatcher) kindOn(k string) bool { return d.Kinds == nil || d.Kinds[k] }

// digestEntry is one "done" waiting for the digest.
type digestEntry struct {
	a      herdr.AgentInfo
	ref    Ref
	latest *transcript.Latest
	at     time.Time
}

func (d *Dispatcher) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d *Dispatcher) holdFor() time.Duration {
	if d.HoldFor == 0 {
		return DefaultHoldFor
	}
	return d.HoldFor
}

// lastViewed is the latest time the user had this agent open.
func (d *Dispatcher) lastViewed(a *herdr.AgentInfo, ref Ref) time.Time {
	if d.Viewer == nil {
		return time.Time{}
	}
	t := d.Viewer.LastViewed(ref.Key)
	if ref.Local {
		if bare := d.Viewer.LastViewed(a.PaneID); bare.After(t) {
			t = bare
		}
	}
	return t
}

// heldLocked reports whether an earlier notification for ref is still
// unanswered. Callers hold d.mu.
func (d *Dispatcher) heldLocked(a *herdr.AgentInfo, ref Ref, now time.Time) bool {
	sent, ok := d.held[ref.Key]
	if !ok {
		return false
	}
	if hold := d.holdFor(); hold < 0 || now.Sub(sent) >= hold || d.lastViewed(a, ref).After(sent) {
		delete(d.held, ref.Key)
		return false
	}
	return true
}

// claim marks ref as notified unless an earlier notification is still
// unanswered, reporting whether the caller may send and the claim's time
// (for unclaim).
func (d *Dispatcher) claim(a *herdr.AgentInfo, ref Ref) (bool, time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	if d.heldLocked(a, ref, now) {
		return false, now
	}
	if d.holdFor() < 0 {
		return true, now
	}
	if d.held == nil {
		d.held = map[string]time.Time{}
	}
	if len(d.held) >= pruneAt {
		// Holds older than holdFor have lapsed (heldLocked drops them on
		// the next lookup anyway).
		pruneBefore(d.held, now.Add(-d.holdFor()))
	}
	d.held[ref.Key] = now
	return true, now
}

// unclaim takes back a claim made at at that sent nothing.
func (d *Dispatcher) unclaim(ref Ref, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.held[ref.Key]; ok && t.Equal(at) {
		delete(d.held, ref.Key)
	}
}

func (d *Dispatcher) channels() (ntfyOn, pushOn bool) {
	if d == nil {
		return false, false
	}
	return d.Ntfy.enabled(), d.Push != nil && d.Push.Active()
}

// Observe records a status transition of a. Leaving blocked drops a
// "needs input" still waiting out BlockedDelay. An agent going back to
// working was answered (from the app or its terminal), which releases
// its held notifications. Call it for every transition, before Wants.
func (d *Dispatcher) Observe(a *herdr.AgentInfo, ref Ref) {
	if d == nil {
		return
	}
	if a.Status != herdr.StatusBlocked {
		d.mu.Lock()
		delete(d.blocked, ref.Key)
		d.mu.Unlock()
	}
	if a.Status != herdr.StatusWorking {
		return
	}
	d.mu.Lock()
	delete(d.held, ref.Key)
	delete(d.digest, ref.Key) // answered before the digest went out
	d.mu.Unlock()
}

// Wants reports whether a transition of a into its current status would
// produce a notification right now (before throttling). Callers use it to
// skip fetching an excerpt that would be thrown away.
func (d *Dispatcher) Wants(a *herdr.AgentInfo, ref Ref) bool {
	switch a.Status {
	case herdr.StatusBlocked:
		if !d.kindOn(KindBlocked) {
			return false
		}
	case herdr.StatusDone:
		if !d.kindOn(KindDone) && !d.kindOn(KindQuestion) {
			return false
		}
	default:
		return false
	}
	ntfyOn, pushOn := d.channels()
	if !ntfyOn && !pushOn {
		return false
	}
	if d.Quiet != nil && d.Quiet(ref.Key) {
		return false
	}
	// Foreground suppression: the user is looking at this agent right now.
	if d.Viewer != nil && (d.Viewer.Viewing(ref.Key) || (ref.Local && d.Viewer.Viewing(a.PaneID))) {
		return false
	}
	// Collation: an earlier notification for this agent is still unanswered.
	d.mu.Lock()
	held := d.heldLocked(a, ref, d.clock())
	d.mu.Unlock()
	return !held
}

// Notify sends the notifications for a's transition into blocked or done
// (a "done" may wait for the digest). It may block on latest and on the
// ntfy post; run it off any lock.
func (d *Dispatcher) Notify(a *herdr.AgentInfo, ref Ref, latestFn LatestFunc) {
	if !d.Wants(a, ref) {
		return
	}
	if a.Status == herdr.StatusBlocked && d.BlockedDelay > 0 {
		d.delayBlocked(a, ref, latestFn)
		return
	}
	d.deliver(a, ref, latestFn)
}

// delayBlocked waits out BlockedDelay before notifying that a needs
// input; Observe drops the wait when a leaves blocked. A wait under way
// is not restarted.
func (d *Dispatcher) delayBlocked(a *herdr.AgentInfo, ref Ref, latestFn LatestFunc) {
	d.mu.Lock()
	if _, waiting := d.blocked[ref.Key]; waiting {
		d.mu.Unlock()
		return
	}
	if d.blocked == nil {
		d.blocked = map[string]blockedEntry{}
	}
	d.blockedSeq++
	seq := d.blockedSeq
	d.blocked[ref.Key] = blockedEntry{a: *a, ref: ref, latest: latestFn, seq: seq}
	d.mu.Unlock()
	d.afterFunc(d.BlockedDelay, func() {
		d.mu.Lock()
		e, ok := d.blocked[ref.Key]
		if ok && e.seq == seq {
			delete(d.blocked, ref.Key)
		}
		d.mu.Unlock()
		if !ok || e.seq != seq || !d.Wants(&e.a, e.ref) {
			return
		}
		d.deliver(&e.a, e.ref, e.latest)
	})
}

// deliver claims a's notification and sends it (a "done" may wait for
// the digest).
func (d *Dispatcher) deliver(a *herdr.AgentInfo, ref Ref, latestFn LatestFunc) {
	ok, claimed := d.claim(a, ref)
	if !ok {
		return
	}
	ntfyOn, pushOn := d.channels()
	excerptChars := 0
	if ntfyOn {
		excerptChars = d.Ntfy.ExcerptChars
	}
	if pushOn {
		excerptChars = max(excerptChars, d.Push.ExcerptChars())
	}
	// Telling a question from a plain "done" takes the last message even
	// when no excerpt is shown.
	classify := a.Status == herdr.StatusDone && d.kindOn(KindQuestion) != d.kindOn(KindDone)
	var latest *transcript.Latest
	if (excerptChars > 0 || classify) && latestFn != nil {
		timeout := d.ExcerptTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		latest = latestFn(ctx)
		cancel()
	}
	kind := KindBlocked
	if a.Status == herdr.StatusDone {
		kind = KindDone
		if endsOnQuestion(latest) {
			kind = KindQuestion
		}
	}
	if !d.kindOn(kind) {
		d.unclaim(ref, claimed)
		return
	}
	if kind == KindDone && d.DoneDigest > 0 {
		d.queueDigest(digestEntry{a: *a, ref: ref, latest: latest, at: claimed})
		return
	}
	d.send(a, ref, latest)
}

// send delivers one agent's notification on every channel.
func (d *Dispatcher) send(a *herdr.AgentInfo, ref Ref, latest *transcript.Latest) {
	ntfyOn, pushOn := d.channels()
	host := ""
	if d.ShowHost {
		host = a.Host
	}
	if pushOn {
		// Web Push payloads are end-to-end encrypted to the browser, so they
		// get their own (by default non-zero) excerpt length.
		kind, msg := buildNotification(a, host, latest, d.Push.ExcerptChars())
		blocked := a.Status == herdr.StatusBlocked
		// "done" can wait for the phone's next convenient wake-up (low
		// urgency lets the push service hold it, e.g. in Low Power Mode);
		// "needs input" is delivered right away.
		opts := push.Options{Urgency: "low", TTL: 4 * time.Hour, Topic: push.TopicFor(ref.Key)}
		if blocked {
			opts.Urgency, opts.TTL = "high", 12*time.Hour
		}
		d.Push.Notify(ref.Key+"/"+kind, push.Message{
			Title:    msg.Title,
			Body:     msg.Message,
			URL:      "/#/agent/" + ref.Route,
			Tag:      ref.Key,
			Renotify: blocked,
		}, opts)
	}
	if ntfyOn {
		kind, msg := buildNotification(a, host, latest, d.Ntfy.ExcerptChars)
		msg.Click = d.Ntfy.agentURL(ref.Route)
		d.Ntfy.notify(ref.Key, kind, msg)
	}
}

// queueDigest adds a "done" to the digest, starting its timer with the
// first one. The window does not slide: nothing waits longer than
// DoneDigest.
func (d *Dispatcher) queueDigest(e digestEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.digest == nil {
		d.digest = map[string]digestEntry{}
	}
	d.digest[e.ref.Key] = e
	if d.digestArmed {
		return
	}
	d.digestArmed = true
	d.afterFunc(d.DoneDigest, d.flushDigest)
}

// flushDigest sends the digest: the agents the user has not opened (or
// is not looking at) since they finished, one notification for all.
func (d *Dispatcher) flushDigest() {
	d.mu.Lock()
	var keep []digestEntry
	for _, e := range d.digest {
		viewing := d.Viewer != nil && (d.Viewer.Viewing(e.ref.Key) || (e.ref.Local && d.Viewer.Viewing(e.a.PaneID)))
		if viewing || d.lastViewed(&e.a, e.ref).After(e.at) {
			continue
		}
		keep = append(keep, e)
	}
	d.digest, d.digestArmed = nil, false
	d.mu.Unlock()
	switch len(keep) {
	case 0:
		return
	case 1:
		d.send(&keep[0].a, keep[0].ref, keep[0].latest)
		return
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].at.Before(keep[j].at) })
	msg := buildDigest(keep, d.ShowHost)
	ntfyOn, pushOn := d.channels()
	if pushOn {
		d.Push.Notify(digestKey+"/"+KindDone, push.Message{
			Title: msg.Title,
			Body:  msg.Message,
			URL:   "/",
			Tag:   digestKey,
		}, push.Options{Urgency: "low", TTL: 4 * time.Hour, Topic: push.TopicFor(digestKey)})
	}
	if ntfyOn {
		msg.Click = strings.TrimRight(d.Ntfy.ClickURL, "/")
		if msg.Click != "" {
			msg.Click += "/"
		}
		d.Ntfy.notify(digestKey, KindDone, msg)
	}
}

// digestKey is the digest's throttle bucket and notification tag.
const digestKey = "done-digest"

// maxDigestLines caps the agents listed in a digest.
const maxDigestLines = 6

// buildDigest renders a digest of several finished agents: no message
// text, only names and where they work.
func buildDigest(es []digestEntry, showHost bool) ntfyMessage {
	msg := ntfyMessage{Title: fmt.Sprintf("%d agents finished", len(es)), Tags: []string{"white_check_mark"}}
	var lines []string
	for i, e := range es {
		if i == maxDigestLines-1 && len(es) > maxDigestLines {
			lines = append(lines, fmt.Sprintf("and %d more", len(es)-i))
			break
		}
		where := repoLabel(&e.a)
		if showHost && e.a.Host != "" {
			where = e.a.Host + " · " + where
		}
		lines = append(lines, notifyName(&e.a)+" · "+where)
	}
	msg.Message = strings.Join(lines, "\n")
	return msg
}

// endsOnQuestion reports whether the agent's last message asks the user
// something (it ends with a question mark).
func endsOnQuestion(latest *transcript.Latest) bool {
	return latest != nil && strings.HasSuffix(strings.TrimSpace(latest.Text), "?")
}

// localLatest reads the excerpt straight from a local agent's session file.
func localLatest(a *herdr.AgentInfo, all []herdr.AgentInfo) LatestFunc {
	// SessionPath also resolves Claude Code agents that report no session
	// path (by cwd), so their notifications get an excerpt too.
	path, err := transcript.SessionPath(*a, all)
	if err != nil || path == "" {
		return nil
	}
	return func(context.Context) *transcript.Latest {
		l, err := transcript.LatestOf(path)
		if err != nil {
			return nil
		}
		return l
	}
}

// buildNotification renders the title and body for a transition into
// blocked or done. kind is the throttle bucket. host, when set, prefixes
// the repository line.
func buildNotification(a *herdr.AgentInfo, host string, latest *transcript.Latest, excerptChars int) (string, ntfyMessage) {
	name := notifyName(a)
	excerpt := ""
	if latest != nil {
		excerpt = transcript.Excerpt(latest.Text, excerptChars)
	}
	where := repoLabel(a)
	if host != "" {
		where = host + " · " + where
	}
	lines := []string{where}
	var msg ntfyMessage
	switch a.Status {
	case herdr.StatusBlocked:
		msg.Title = name + " needs input"
		msg.Tags = []string{"warning"}
		msg.Priority = 4
		switch {
		case latest != nil && len(latest.Pending) > 0:
			// Pi's permission gate blocks on a pending tool call.
			p := latest.Pending[len(latest.Pending)-1]
			line := "wants to run " + p.Tool
			if args := transcript.Excerpt(p.Args, excerptChars); args != "" {
				line += ": " + args
			}
			lines = append(lines, line)
		case excerpt != "":
			lines = append(lines, excerpt)
		default:
			lines = append(lines, "waiting for a response")
		}
	default: // done
		msg.Title = name + " is done"
		msg.Tags = []string{"white_check_mark"}
		if endsOnQuestion(latest) {
			// The turn ended on a question: the agent is waiting on you.
			msg.Title = name + " has a question"
			msg.Tags = []string{"question"}
		}
		switch {
		case excerpt != "":
			lines = append(lines, excerpt)
		case endsOnQuestion(latest):
			lines = append(lines, "waiting for your answer")
		default:
			lines = append(lines, "completed work")
		}
	}
	msg.Message = strings.Join(lines, "\n")
	return string(a.Status), msg
}

// repoLabel names where the agent works: repo (and branch for a linked
// worktree), else the cwd basename, else the pane ID.
// notifyName labels an agent in a notification title. Unlike DisplayName
// it never uses the terminal title, which any program running in the pane
// can set with an escape sequence (and so fake a title like "Security
// alert"): the agent's name, else its type, else its pane.
func notifyName(a *herdr.AgentInfo) string {
	switch {
	case a.Name != "":
		return a.Name
	case a.DisplayAgent != "":
		return a.DisplayAgent
	case a.Agent != "":
		return a.Agent
	default:
		return a.PaneID
	}
}

func repoLabel(a *herdr.AgentInfo) string {
	if wt := a.Worktree; wt != nil && wt.RepoName != "" {
		if wt.IsLinked && wt.Branch != "" {
			return wt.RepoName + " ⑂ " + wt.Branch
		}
		return wt.RepoName
	}
	if a.CWD != "" {
		return filepath.Base(a.CWD)
	}
	return a.PaneID
}
