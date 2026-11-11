package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// The outbox holds the short lines the hub has for the coordinator that
// need no judgement (a worker's work was landed, a handoff was cleaned up
// by the hub itself). They are sent together in one hub notice, not one
// message each. Lines survive a hub restart.

const (
	outboxMaxLines = 200
	outboxLineMax  = 400
)

// outLine is one routine line.
type outLine struct {
	// Key identifies the line: the same key is not queued twice.
	Key   string    `json:"key"`
	Kind  string    `json:"kind"`
	Entry string    `json:"entry,omitempty"`
	Text  string    `json:"text"`
	At    time.Time `json:"at"`
}

type outboxFile struct {
	Lines []outLine `json:"lines"`
	// Sent are the keys already sent (newest last), so a repeat of a line
	// is not sent again.
	Sent []string `json:"sent,omitempty"`
}

type outbox struct {
	mu      sync.Mutex
	path    string
	data    outboxFile
	sending bool
	last    time.Time // the last send attempt
}

func openOutbox(path string) *outbox {
	o := &outbox{path: path}
	if path == "" {
		return o
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("notice outbox: %v (starting empty)", err)
		}
		return o
	}
	if err := json.Unmarshal(b, &o.data); err != nil {
		log.Printf("notice outbox: %v (starting empty)", err)
		o.data = outboxFile{}
	}
	return o
}

func (o *outbox) saveLocked() {
	if o.path == "" {
		return
	}
	b, _ := json.Marshal(o.data)
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("notice outbox: %v", err)
		return
	}
	if err := os.Rename(tmp, o.path); err != nil {
		log.Printf("notice outbox: %v", err)
	}
}

// add queues a line unless its key is already queued or was sent.
func (o *outbox) add(l outLine) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, x := range o.data.Lines {
		if x.Key == l.Key {
			// A repeat (landed, then CI green) updates the waiting line.
			o.data.Lines[i].Text = l.Text
			o.saveLocked()
			return false
		}
	}
	for _, k := range o.data.Sent {
		if k == l.Key {
			return false
		}
	}
	// A cleanup line stands in for the entry's earlier "landed" line.
	if l.Kind == "cleanup" {
		kept := o.data.Lines[:0]
		for _, x := range o.data.Lines {
			if !(x.Entry == l.Entry && x.Kind == "landed") {
				kept = append(kept, x)
			}
		}
		o.data.Lines = kept
	}
	if len(l.Text) > outboxLineMax {
		l.Text = l.Text[:outboxLineMax]
	}
	if len(o.data.Lines) >= outboxMaxLines {
		o.data.Lines = o.data.Lines[1:]
	}
	o.data.Lines = append(o.data.Lines, l)
	o.saveLocked()
	return true
}

func (o *outbox) pending() []outLine {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]outLine(nil), o.data.Lines...)
}

// settle drops the lines that were sent.
func (o *outbox) settle(sent []outLine) {
	o.mu.Lock()
	defer o.mu.Unlock()
	gone := map[string]bool{}
	for _, l := range sent {
		gone[l.Key] = true
		o.data.Sent = append(o.data.Sent, l.Key)
	}
	if n := len(o.data.Sent); n > 1000 {
		o.data.Sent = o.data.Sent[n-1000:]
	}
	kept := o.data.Lines[:0]
	for _, l := range o.data.Lines {
		if !gone[l.Key] {
			kept = append(kept, l)
		}
	}
	o.data.Lines = kept
	o.saveLocked()
}

// queueLine adds a routine line for the coordinator.
func (s *Server) queueLine(kind, entry, key, text string) {
	if s.outbox == nil {
		return
	}
	s.outbox.add(outLine{Key: kind + "/" + key, Kind: kind, Entry: entry, Text: text, At: time.Now().UTC()})
}

// outboxText is the notice for a batch of lines. The lines are written by
// the hub from safe fields only.
func outboxText(lines []outLine) string {
	var b strings.Builder
	if len(lines) == 1 {
		b.WriteString("Comebye (hub notice): " + lines[0].Text)
		return b.String()
	}
	b.WriteString("Comebye (hub notice): digest of routine updates; nothing here needs a reply unless you want to follow up.\n")
	for _, l := range lines {
		b.WriteString("- " + l.Text + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// flushOutbox sends the queued lines to a ready coordinator in one notice
// when they are due.
func (s *Server) flushOutbox(agents []herdr.AgentInfo) {
	o := s.outbox
	if o == nil {
		return
	}
	lines := o.pending()
	if len(lines) == 0 || !s.outboxDue(lines, time.Now()) {
		return
	}
	target, _ := s.briefCoordinator()
	if target == nil {
		return
	}
	o.mu.Lock()
	if o.sending || time.Since(o.last) < callbackRetry {
		o.mu.Unlock()
		return
	}
	o.sending, o.last = true, time.Now()
	o.mu.Unlock()
	go func(target herdr.AgentInfo) {
		ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
		defer cancel()
		err := s.promptNotice(ctx, target, outboxText(lines))
		o.mu.Lock()
		o.sending = false
		o.mu.Unlock()
		if err != nil {
			if !isInputBusy(err) {
				log.Printf("notice outbox to %s: %v", target.ID, err)
			}
			return
		}
		o.settle(lines)
		s.recordDigest(target.ID, lines)
	}(*target)
}

// outboxDue says whether the queued lines go out now: with no digest
// interval at once, else when the oldest has waited that long.
func (s *Server) outboxDue(lines []outLine, now time.Time) bool {
	if s.digestEvery <= 0 {
		return true
	}
	return now.Sub(lines[0].At) >= s.digestEvery
}

func outboxPath(dir string) string { return filepath.Join(dir, "notice-outbox.json") }
