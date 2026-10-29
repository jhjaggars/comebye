package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// The prompt queue (`comebye prompt --queue`, docs/proposals/prompt-queue.md):
// the hub holds a prompt for an agent that cannot take it now (a dialog
// is open, it is working or blocked) and sends it once the agent is idle
// or done with no dialog showing. It reuses the callback's rules:
// idle-only delivery, 15 s spacing, a 2 h lifetime, and an identity check
// (agent id plus the name it had) so a reused pane id never gets a stranger's
// message.

const (
	// queueTTL bounds how long a prompt waits.
	queueTTL = 2 * time.Hour
	// queueRetry spaces delivery attempts per target (same as callbacks).
	queueRetry = callbackRetry
	// queueMaxPerTarget, queueMaxText and queueMaxTotal cap what agents can
	// park on the hub.
	queueMaxPerTarget = 5
	queueMaxText      = 4 << 10
	queueMaxTotal     = 50
	// queueRate limits how many prompts one sender may queue per minute.
	queueRate = 10
	// queueKeep is how many settled prompts (and for how long) stay
	// visible after they leave the queue.
	queueKeep    = 20
	queueKeepFor = 24 * time.Hour
)

// Queue states.
const (
	QueuePending   = "pending"
	QueueDelivered = "delivered"
	QueueExpired   = "expired"   // not deliverable within queueTTL
	QueueGone      = "gone"      // the agent's pane now has another name
	QueueCancelled = "cancelled" // withdrawn by the user or the sender
)

// QueuedPrompt is one prompt waiting for (or settled after) delivery.
type QueuedPrompt struct {
	ID string `json:"id"`
	// Agent and Name identify the target as it was when queued.
	Agent string `json:"agent_id"`
	Name  string `json:"name,omitempty"`
	// Text is the sender's text, without the stamp added at delivery.
	Text string `json:"text"`
	// From is a display string; FromID is the sending agent's id ("" for
	// the user).
	From   string    `json:"from"`
	FromID string    `json:"from_id,omitempty"`
	At     time.Time `json:"at"`
	State  string    `json:"state"`
	Tries  int       `json:"tries,omitempty"`
	// SettledAt is when it was delivered, expired, dropped or cancelled.
	SettledAt *time.Time `json:"settled_at,omitempty"`
	// Notice is set while the sender still has to be told it was not
	// delivered (expired or gone); the notice is fixed text.
	Notice bool `json:"notice,omitempty"`
}

type queueFile struct {
	Version int            `json:"version"`
	Items   []QueuedPrompt `json:"items"`
}

type promptQueue struct {
	mu   sync.Mutex
	path string // "" keeps it in memory
	data queueFile
	now  func() time.Time
	// last: target id -> last send attempt; sending: ids in flight;
	// rate: sender -> recent queue times. In memory only.
	last    map[string]time.Time
	sending map[string]bool
	rate    map[string][]time.Time
	// send delivers text to an agent (default Server.promptAgent; tests
	// replace it).
	send func(ctx context.Context, a herdr.AgentInfo, text string) error
}

func newPromptQueue(path string) *promptQueue {
	return &promptQueue{path: path, data: queueFile{Version: 1}, now: time.Now,
		last: map[string]time.Time{}, sending: map[string]bool{}, rate: map[string][]time.Time{}}
}

func openPromptQueue(path string) (*promptQueue, error) {
	q := newPromptQueue(path)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return q, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &q.data); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	q.data.Version = 1
	return q, nil
}

// save persists the queue; callers hold mu.
func (q *promptQueue) save() error {
	if q.path == "" {
		return nil
	}
	return writeFileAtomic(q.path, &q.data)
}

func newQueueID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "q" + hex.EncodeToString(b)
}

// Errors for add; the handler maps them to statuses.
var (
	errQueueFull    = errors.New("queue_full")
	errQueueRate    = errors.New("queue_rate")
	errQueueTooBig  = errors.New("text_too_large")
	errQueueNoMatch = errors.New("not_found")
)

// add queues p. An identical pending prompt (same target, text and
// sender) is returned instead of a second copy (dup is true).
func (q *promptQueue) add(p QueuedPrompt) (out QueuedPrompt, dup bool, err error) {
	if len(p.Text) > queueMaxText {
		return p, false, errQueueTooBig
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	total, perTarget := 0, 0
	for _, it := range q.data.Items {
		if it.State != QueuePending {
			continue
		}
		if it.Agent == p.Agent && it.Text == p.Text && it.FromID == p.FromID && it.From == p.From {
			return it, true, nil
		}
		total++
		if it.Agent == p.Agent {
			perTarget++
		}
	}
	if perTarget >= queueMaxPerTarget || total >= queueMaxTotal {
		return p, false, errQueueFull
	}
	key := p.FromID
	if key == "" {
		key = "user"
	}
	recent := q.rate[key][:0]
	for _, t := range q.rate[key] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= queueRate {
		q.rate[key] = recent
		return p, false, errQueueRate
	}
	q.rate[key] = append(recent, now)
	p.ID, p.At, p.State = newQueueID(), now, QueuePending
	q.data.Items = append(q.data.Items, p)
	return p, false, q.save()
}

// list returns the queue newest-last; agent filters by target id when set,
// settledToo includes the recently settled ones.
func (q *promptQueue) list(agent string, settledToo bool) []QueuedPrompt {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := []QueuedPrompt{}
	for _, it := range q.data.Items {
		if agent != "" && it.Agent != agent {
			continue
		}
		if it.State != QueuePending && !settledToo {
			continue
		}
		out = append(out, it)
	}
	return out
}

// settle moves pending id to state, queueing the sender notice when
// notice is set.
func (q *promptQueue) settle(id, state string, notice bool) (QueuedPrompt, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.data.Items {
		it := &q.data.Items[i]
		if it.ID != id || it.State != QueuePending {
			continue
		}
		now := q.now()
		it.State, it.SettledAt, it.Notice = state, &now, notice && it.FromID != ""
		delete(q.sending, id)
		return *it, q.save()
	}
	return QueuedPrompt{}, errQueueNoMatch
}

func (q *promptQueue) tried(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.data.Items {
		if q.data.Items[i].ID == id {
			q.data.Items[i].Tries++
		}
	}
	delete(q.sending, id)
	if err := q.save(); err != nil {
		log.Printf("prompt queue: %v", err)
	}
}

func (q *promptQueue) clearNotice(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.data.Items {
		if q.data.Items[i].ID == id {
			q.data.Items[i].Notice = false
		}
	}
	if err := q.save(); err != nil {
		log.Printf("prompt queue: %v", err)
	}
}

// prune drops settled items that are old or beyond queueKeep.
func (q *promptQueue) prune() {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	var settled int
	for _, it := range q.data.Items {
		if it.State != QueuePending {
			settled++
		}
	}
	kept := q.data.Items[:0]
	changed := false
	for _, it := range q.data.Items {
		if it.State != QueuePending && !it.Notice {
			if (it.SettledAt != nil && now.Sub(*it.SettledAt) > queueKeepFor) || settled > queueKeep {
				settled--
				changed = true
				continue
			}
		}
		kept = append(kept, it)
	}
	q.data.Items = kept
	if changed {
		if err := q.save(); err != nil {
			log.Printf("prompt queue: %v", err)
		}
	}
}

// ---- delivery ----

// findAgent returns the agent with id from the fleet. replaced is true
// when the pane is there under another name than name (pane id reused).
func findAgent(agents []herdr.AgentInfo, id, name string) (a *herdr.AgentInfo, replaced bool) {
	for i := range agents {
		if agents[i].ID != id {
			continue
		}
		if name != "" && agents[i].Name != "" && agents[i].Name != name {
			return nil, true
		}
		return &agents[i], false
	}
	return nil, false
}

// canTakePrompt is the status a hub message may be sent to.
func canTakePrompt(a herdr.AgentInfo) bool {
	return a.Status == herdr.StatusIdle || a.Status == herdr.StatusDone
}

// queueText is what the agent receives: the sender's text under a stamp
// naming who queued it and when, so it is not mistaken for the user.
func queueText(p QueuedPrompt) string {
	from := "the user"
	if p.FromID != "" {
		from = safeText(p.From)
	}
	return fmt.Sprintf("[Queued by %s via comebye at %s UTC; held while you could not take it]\n%s", from, p.At.UTC().Format("15:04"), p.Text)
}

// noticeText is the fixed note to a sender whose prompt did not arrive.
func noticeText(p QueuedPrompt) string {
	why := "expired after 2 hours"
	if p.State == QueueGone {
		why = "was dropped because the agent is gone or its pane has another name now"
	}
	return fmt.Sprintf("Comebye (hub notice): your queued prompt %s to %s %s and was not delivered. Send it again if it still matters.", safeText(p.ID), safeText(p.Name), why)
}

// deliverQueue runs on every fleet update: expire old prompts, drop ones
// whose pane was reused, and send the head of each target's queue to an
// agent that is idle or done (the send itself refuses when a dialog shows).
func (s *Server) deliverQueue(agents []herdr.AgentInfo) {
	q := s.pq
	if q == nil {
		return
	}
	now := q.now()
	pending := q.list("", true)
	headDone := map[string]bool{}
	for _, it := range pending {
		if it.State != QueuePending {
			if it.Notice {
				s.sendNotice(agents, it, now)
			}
			continue
		}
		if now.Sub(it.At) > queueTTL {
			if p, err := q.settle(it.ID, QueueExpired, true); err != nil {
				logQueueErr(err)
			} else {
				s.sendNotice(agents, p, now)
			}
			continue
		}
		a, replaced := findAgent(agents, it.Agent, it.Name)
		if replaced {
			if p, err := q.settle(it.ID, QueueGone, true); err != nil {
				logQueueErr(err)
			} else {
				s.sendNotice(agents, p, now)
			}
			continue
		}
		if headDone[it.Agent] { // FIFO: one per target per pass
			continue
		}
		headDone[it.Agent] = true
		if a == nil || !canTakePrompt(*a) {
			continue
		}
		q.mu.Lock()
		if q.sending[it.ID] || now.Sub(q.last[it.Agent]) < queueRetry {
			q.mu.Unlock()
			continue
		}
		q.sending[it.ID], q.last[it.Agent] = true, now
		q.mu.Unlock()
		go s.sendQueued(it, *a)
	}
	q.prune()
}

// logQueueErr logs a queue failure; losing a race to settle the same
// prompt (another pass or a cancel got there first) is not one.
func logQueueErr(err error) {
	if !errors.Is(err, errQueueNoMatch) {
		log.Printf("prompt queue: %v", err)
	}
}

func (s *Server) sendQueued(it QueuedPrompt, target herdr.AgentInfo) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	send := s.pq.send
	if send == nil {
		send = s.promptAgent
	}
	err := send(ctx, target, queueText(it))
	if err == nil {
		if _, lerr := s.pq.settle(it.ID, QueueDelivered, false); lerr != nil {
			logQueueErr(lerr)
		}
		return
	}
	var he *herdr.Error
	// A dialog or a blocked agent is the expected reason to wait; anything
	// else is logged.
	if !(errors.As(err, &he) && (he.Code == "dialog_open" || he.Code == "agent_blocked")) && !strings.Contains(err.Error(), "answered 409") {
		log.Printf("queued prompt %s to %s: %v", it.ID, target.ID, err)
	}
	s.pq.tried(it.ID)
}

// sendNotice tells the sending agent its prompt did not arrive, once, when
// it is idle or done; it gives up with the item's retention.
func (s *Server) sendNotice(agents []herdr.AgentInfo, it QueuedPrompt, now time.Time) {
	if it.FromID == "" {
		s.pq.clearNotice(it.ID)
		return
	}
	if it.SettledAt != nil && now.Sub(*it.SettledAt) > queueTTL {
		s.pq.clearNotice(it.ID)
		return
	}
	var from *herdr.AgentInfo
	for i := range agents {
		if agents[i].ID == it.FromID {
			from = &agents[i]
		}
	}
	if from == nil || !canTakePrompt(*from) {
		return
	}
	s.pq.mu.Lock()
	key := "notice/" + it.ID
	if s.pq.sending[key] || now.Sub(s.pq.last[key]) < queueRetry {
		s.pq.mu.Unlock()
		return
	}
	s.pq.sending[key], s.pq.last[key] = true, now
	s.pq.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		send := s.pq.send
		if send == nil {
			send = s.promptAgent
		}
		err := send(ctx, *from, noticeText(it))
		s.pq.mu.Lock()
		delete(s.pq.sending, key)
		s.pq.mu.Unlock()
		if err == nil {
			s.pq.clearNotice(it.ID)
		}
	}()
}

// ---- API ----

type queueBody struct {
	// Agent is the target's id (host/pane); Name is the name it had when
	// the caller resolved it.
	Agent string `json:"agent"`
	Name  string `json:"name"`
	Text  string `json:"text"`
}

func (s *Server) queueOff(w http.ResponseWriter, r *http.Request) bool {
	if s.ledgerOff(w, r) {
		return true
	}
	if s.pq == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"code": "queue_disabled", "message": "the prompt queue is off"})
		return true
	}
	return false
}

func queueErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errQueueFull):
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"code": "queue_full", "message": fmt.Sprintf("too many queued prompts for this agent (max %d)", queueMaxPerTarget)})
	case errors.Is(err, errQueueRate):
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"code": "queue_rate", "message": "queueing too fast: wait a minute"})
	case errors.Is(err, errQueueTooBig):
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "text_too_large", "message": fmt.Sprintf("queued prompts are limited to %d bytes; send longer text with a plain prompt", queueMaxText)})
	case errors.Is(err, errQueueNoMatch):
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "not_found", "message": "no such pending prompt"})
	default:
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "queue_error", "message": err.Error()})
	}
}

// handleQueueAdd queues a prompt (POST /api/queue). The agent must be in
// the fleet now; whether it can take the prompt is decided at delivery.
func (s *Server) handleQueueAdd(w http.ResponseWriter, r *http.Request) {
	if s.queueOff(w, r) {
		return
	}
	var b queueBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(b.Text) == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	var target *herdr.AgentInfo
	agents := s.fleet.Agents()
	for i := range agents {
		if agents[i].ID == b.Agent {
			target = &agents[i]
		}
	}
	if target == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "agent_not_found", "message": "unknown agent"})
		return
	}
	if b.Name != "" && target.Name != "" && target.Name != b.Name {
		writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "agent_changed", "message": "the agent's name changed: look it up again"})
		return
	}
	c := callerOf(r)
	if c.agent() && s.isSelf(c, target.Host, target.PaneID) {
		writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "self", "message": "an agent cannot do this to itself"})
		return
	}
	p := QueuedPrompt{Agent: target.ID, Name: target.Name, Text: b.Text, From: s.by(r, "")}
	if c.agent() {
		p.FromID = s.replyID(c)
	}
	out, dup, err := s.pq.add(p)
	if err != nil {
		queueErr(w, err)
		return
	}
	s.deliverQueue(agents)
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"queued": out, "duplicate": dup})
}

// replyID is the calling agent's fleet id (host/pane).
func (s *Server) replyID(c caller) string {
	host := c.host
	if host == "" {
		host = s.fleet.LocalName()
	}
	return host + "/" + c.pane
}

// handleQueueList lists pending prompts (GET /api/queue?agent=ID&all=1).
func (s *Server) handleQueueList(w http.ResponseWriter, r *http.Request) {
	if s.queueOff(w, r) {
		return
	}
	writeJSON(w, map[string]any{"queue": s.pq.list(r.URL.Query().Get("agent"), r.URL.Query().Get("all") != "")})
}

// handleQueueCancel withdraws a pending prompt (DELETE /api/queue/{id}).
func (s *Server) handleQueueCancel(w http.ResponseWriter, r *http.Request) {
	if s.queueOff(w, r) {
		return
	}
	p, err := s.pq.settle(r.PathValue("id"), QueueCancelled, false)
	if err != nil {
		queueErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"queued": p})
}

func queuePath(dir string) string { return filepath.Join(dir, "queue.json") }
