package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// Notices the hub sends to an agent (a worker finished, a to-do answered,
// a blocked agent relayed, the scheduled briefing) are sent once. herdr's
// agent.prompt can fail after the text and Enter were already written (a
// timeout, a dropped socket); the callers used to read any error as "not
// sent" and send the notice again on the next pass, so the agent got it
// twice. promptNotice tags each notice with a short id and, when the send
// errors, looks in the tail of the target's session transcript (Claude and
// pi alike) for that id before the error is reported as a failure.

const (
	// noticeArriveWait is how long, after a failed send, the transcript is
	// watched for the notice (the harness may take a moment to write it).
	noticeArriveWait = 3 * time.Second
	// noticeRead caps how much of a transcript's tail is searched.
	noticeRead = 1 << 20
	// noticeKeep is how long an unverified attempt is remembered.
	noticeKeep = time.Hour
)

// noticeSent is an earlier send of a notice that errored without being seen
// to arrive: where the target's transcript ended just before it.
type noticeSent struct {
	path string
	off  int64
	at   time.Time
}

type noticeBook struct {
	mu   sync.Mutex
	sent map[string]noticeSent
}

func (b *noticeBook) get(id string) (noticeSent, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.sent[id]
	if ok && time.Since(n.at) > noticeKeep {
		delete(b.sent, id)
		return noticeSent{}, false
	}
	return n, ok
}

func (b *noticeBook) put(id string, n noticeSent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sent == nil {
		b.sent = map[string]noticeSent{}
	}
	for k, v := range b.sent {
		if time.Since(v.at) > noticeKeep {
			delete(b.sent, k)
		}
	}
	b.sent[id] = n
}

func (b *noticeBook) forget(id string) {
	b.mu.Lock()
	delete(b.sent, id)
	b.mu.Unlock()
}

// noticeID is a short stable tag for a notice's text.
func noticeID(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:3])
}

// noticeTag is what promptNotice appends to a notice.
func noticeTag(id string) string { return " [notice " + id + "]" }

// noticeTranscript is the target's session file on this host, "" when the
// agent is on another host or its session cannot be resolved.
func (s *Server) noticeTranscript(a herdr.AgentInfo) string {
	if a.Host != "" && a.Host != s.fleet.LocalName() {
		return ""
	}
	p, err := transcript.SessionPath(a, s.fleet.Agents())
	if err != nil {
		return ""
	}
	return p
}

// noticeProbe is the answer of a transcript look-up: whether tag follows
// offset, and where the transcript ends now. ok is false when the
// transcript cannot be looked at (no session, a peer that did not answer).
type noticeProbe struct {
	Seen bool  `json:"seen"`
	Size int64 `json:"size"`
	OK   bool  `json:"ok"`
}

// probeNotice looks at the agent's transcript, here or on its peer. off < 0
// asks only for the size.
func (s *Server) probeNotice(ctx context.Context, a herdr.AgentInfo, id string, off int64) noticeProbe {
	if s.isLocalAgent(a) {
		return probeFile(s.noticeTranscript(a), id, off)
	}
	body, _ := json.Marshal(noticeSeenBody{ID: id, Offset: off})
	status, data, err := s.fleet.CallAgent(ctx, a.Host, a.PaneID, "noticeseen", body)
	var p noticeProbe
	if err != nil || status != http.StatusOK || json.Unmarshal(data, &p) != nil {
		return noticeProbe{}
	}
	return p
}

func probeFile(path, id string, off int64) noticeProbe {
	if path == "" {
		return noticeProbe{}
	}
	return noticeProbe{OK: true, Size: fileSize(path), Seen: off >= 0 && noticeSeen(path, off, noticeTag(id))}
}

// noticeSeenBody is the request of a peer's noticeseen route.
type noticeSeenBody struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset"`
}

var noticeIDRe = regexp.MustCompile(`^[0-9a-f]{6}$`)

// handleNoticeSeen is the hub asking this host whether a notice's id is in
// the transcript of one local agent since an offset. Read-only; the answer
// is a boolean and a size, never transcript text.
func (s *Server) handleNoticeSeen(w http.ResponseWriter, r *http.Request) {
	var b noticeSeenBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil || !noticeIDRe.MatchString(b.ID) {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	a := s.resolveAgent(r.PathValue("target"))
	if a == nil {
		writeJSON(w, noticeProbe{})
		return
	}
	writeJSON(w, probeFile(s.noticeTranscript(*a), b.ID, b.Offset))
}

func fileSize(path string) int64 {
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// noticeSeen reports whether the transcript holds tag after offset off.
func noticeSeen(path string, off int64, tag string) bool {
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	if off > fi.Size() {
		off = 0 // the file was replaced by a shorter one
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(f, noticeRead))
	return err == nil && bytes.Contains(b, []byte(tag))
}

// promptNotice is promptAgent for a hub notice: it returns nil when the
// text reached the agent even though herdr's answer was an error, so the
// caller does not send it again. A notice that did not arrive returns the
// error, and a later attempt first checks whether this one arrived late.
// The look-up is made where the agent runs (a peer answers from its own
// transcript); where none is possible the error stands and the send is
// retried as before.
func (s *Server) promptNotice(ctx context.Context, a herdr.AgentInfo, text string) error {
	id := noticeID(text)
	tag := noticeTag(id)
	where := a.ID
	start := int64(-1)
	if prev, ok := s.notices.get(id); ok && prev.path == where {
		if p := s.probeNotice(ctx, a, id, prev.off); p.Seen {
			s.notices.forget(id) // the earlier attempt arrived after all
			return nil
		}
		start = prev.off
	} else if p := s.probeNotice(ctx, a, id, -1); p.OK {
		start = p.Size
	}
	err := s.promptAgent(ctx, a, text+tag)
	if err == nil {
		s.notices.forget(id)
		return nil
	}
	if isInputBusy(err) || start < 0 {
		return err // not sent / cannot check
	}
	wait := s.noticeArriveWait
	if wait <= 0 {
		wait = noticeArriveWait
	}
	deadline := time.Now().Add(wait)
	for {
		if s.probeNotice(ctx, a, id, start).Seen {
			s.notices.forget(id)
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.notices.put(id, noticeSent{path: where, off: start, at: time.Now()})
	return err
}
