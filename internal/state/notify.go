package state

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/herdr-phone/internal/herdr"
	"github.com/jhjaggars/herdr-phone/internal/push"
	"github.com/jhjaggars/herdr-phone/internal/transcript"
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
	n.last[key] = now
	return true
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

	mu sync.Mutex
	// held maps an agent key to when it was last notified. Until the user
	// opens that agent in the app (a presence report after that time) or
	// it goes back to working, it gets no further notifications: one per
	// agent, not one per prompt.
	held map[string]time.Time
	now  func() time.Time
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
// unanswered, reporting whether the caller may send.
func (d *Dispatcher) claim(a *herdr.AgentInfo, ref Ref) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	if d.heldLocked(a, ref, now) {
		return false
	}
	if d.holdFor() < 0 {
		return true
	}
	if d.held == nil {
		d.held = map[string]time.Time{}
	}
	d.held[ref.Key] = now
	return true
}

func (d *Dispatcher) channels() (ntfyOn, pushOn bool) {
	if d == nil {
		return false, false
	}
	return d.Ntfy.enabled(), d.Push != nil && d.Push.Active()
}

// Observe records a status transition of a. An agent going back to
// working was answered (from the app or its terminal), which releases
// its held notifications. Call it for every transition, before Wants.
func (d *Dispatcher) Observe(a *herdr.AgentInfo, ref Ref) {
	if d == nil || a.Status != herdr.StatusWorking {
		return
	}
	d.mu.Lock()
	delete(d.held, ref.Key)
	d.mu.Unlock()
}

// Wants reports whether a transition of a into its current status would
// produce a notification right now (before throttling). Callers use it to
// skip fetching an excerpt that would be thrown away.
func (d *Dispatcher) Wants(a *herdr.AgentInfo, ref Ref) bool {
	if a.Status != herdr.StatusBlocked && a.Status != herdr.StatusDone {
		return false
	}
	ntfyOn, pushOn := d.channels()
	if !ntfyOn && !pushOn {
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

// Notify sends the notifications for a's transition into blocked or done.
// It may block on latest and on the ntfy post; run it off any lock.
func (d *Dispatcher) Notify(a *herdr.AgentInfo, ref Ref, latestFn LatestFunc) {
	if !d.Wants(a, ref) || !d.claim(a, ref) {
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
	var latest *transcript.Latest
	if excerptChars > 0 && latestFn != nil {
		timeout := d.ExcerptTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		latest = latestFn(ctx)
		cancel()
	}
	host := ""
	if d.ShowHost {
		host = a.Host
	}
	if pushOn {
		// Web Push payloads are end-to-end encrypted to the browser, so they
		// get their own (by default non-zero) excerpt length.
		kind, msg := buildNotification(a, host, latest, d.Push.ExcerptChars())
		blocked := a.Status == herdr.StatusBlocked
		opts := push.Options{Urgency: "normal", TTL: 4 * time.Hour, Topic: push.TopicFor(ref.Key)}
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
	name := a.DisplayName()
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
		if strings.HasSuffix(excerpt, "?") {
			// The turn ended on a question: the agent is waiting on you.
			msg.Title = name + " has a question"
			msg.Tags = []string{"question"}
		}
		if excerpt != "" {
			lines = append(lines, excerpt)
		} else {
			lines = append(lines, "completed work")
		}
	}
	msg.Message = strings.Join(lines, "\n")
	return string(a.Status), msg
}

// repoLabel names where the agent works: repo (and branch for a linked
// worktree), else the cwd basename, else the pane ID.
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
