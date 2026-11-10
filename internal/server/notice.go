package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
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

// noticeTranscript is the target's session file, "" when it is on another
// host or cannot be resolved (then no arrival check is possible and a
// failed send is retried as before).
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
func (s *Server) promptNotice(ctx context.Context, a herdr.AgentInfo, text string) error {
	id := noticeID(text)
	tag := noticeTag(id)
	path := s.noticeTranscript(a)
	start := fileSize(path)
	if prev, ok := s.notices.get(id); ok && prev.path == path && path != "" {
		if noticeSeen(path, prev.off, tag) {
			s.notices.forget(id) // the earlier attempt arrived after all
			return nil
		}
		start = prev.off
	}
	err := s.promptAgent(ctx, a, text+tag)
	if err == nil {
		s.notices.forget(id)
		return nil
	}
	if isInputBusy(err) || path == "" {
		return err // not sent / cannot check
	}
	wait := s.noticeArriveWait
	if wait <= 0 {
		wait = noticeArriveWait
	}
	deadline := time.Now().Add(wait)
	for {
		if noticeSeen(path, start, tag) {
			s.notices.forget(id)
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.notices.put(id, noticeSent{path: path, off: start, at: time.Now()})
	return err
}
