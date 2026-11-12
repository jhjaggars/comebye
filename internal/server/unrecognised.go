package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/dialog"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// Capture of screens the dialog detector does not recognise.
//
// When an agent herdr calls blocked shows a screen no dialog is read from,
// the raw visible text is saved on the agent's own host, so the cause can be
// found later (it is not sent anywhere: not through the hub, not in
// notices). A failed pane read is recorded in its own file, so "could not
// read" and "read but not recognised" can be told apart. One file of each
// kind per agent per occurrence: the entry is cleared once the agent is no
// longer blocked, or a dialog is recognised.
const (
	unrecDirName   = "dialog-unrecognised"
	unrecMaxLines  = 80
	unrecMaxBytes  = 8 << 10
	unrecKeepFiles = 20
	unrecKeepAge   = 7 * 24 * time.Hour
)

type unrecCapture struct {
	dir  string
	host string
	now  func() time.Time

	mu     sync.Mutex
	screen map[string]string // agent key → saved screen file name (this occurrence)
	rerr   map[string]string // agent key → saved read-error file name
}

func newUnrecCapture(stateDir, host string) *unrecCapture {
	if stateDir == "" {
		return nil
	}
	return &unrecCapture{dir: filepath.Join(stateDir, unrecDirName), host: host, now: time.Now,
		screen: map[string]string{}, rerr: map[string]string{}}
}

// savedScreen is the file saved for the agent's current occurrence, or "".
func (u *unrecCapture) savedScreen(key string) string {
	if u == nil {
		return ""
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.screen[key]
}

// recordingIO remembers what the last pane read returned.
type recordingIO struct {
	paneIO
	read bool
	text string
	err  error
}

func (r *recordingIO) ReadPane(ctx context.Context, paneID, source string, lines int) (herdr.ReadResult, error) {
	res, err := r.paneIO.ReadPane(ctx, paneID, source, lines)
	r.read, r.text, r.err = true, res.Text, err
	return res, err
}

// ReadPaneStyled keeps readDialogScreen's styled read working through the
// wrapper (and records what it read like ReadPane does).
func (r *recordingIO) ReadPaneStyled(ctx context.Context, paneID, source string, lines int) (herdr.ReadResult, error) {
	sp, ok := r.paneIO.(styledPaneIO)
	if !ok {
		return r.ReadPane(ctx, paneID, source, lines)
	}
	res, err := sp.ReadPaneStyled(ctx, paneID, source, lines)
	r.read, r.text, r.err = true, res.Text, err
	return res, err
}

// currentDialogCaptured is currentDialog plus the capture above.
func (s *Server) currentDialogCaptured(ctx context.Context, agent herdr.AgentInfo, ask *transcript.Ask) (*dialog.Dialog, error) {
	if s.unrec == nil {
		return currentDialog(ctx, s.client, agent, ask)
	}
	rec := &recordingIO{paneIO: s.client}
	d, err := currentDialog(ctx, rec, agent, ask)
	s.unrec.observe(agent, rec, d)
	return d, err
}

func (u *unrecCapture) observe(agent herdr.AgentInfo, rec *recordingIO, d *dialog.Dialog) {
	key := state.AgentKey(&agent)
	u.mu.Lock()
	defer u.mu.Unlock()
	if agent.Status != herdr.StatusBlocked || d != nil || !rec.read {
		if agent.Status != herdr.StatusBlocked || d != nil {
			delete(u.screen, key)
			delete(u.rerr, key)
		}
		return
	}
	if rec.err != nil {
		if u.rerr[key] == "" {
			u.rerr[key] = u.save(agent, "readerror", "the pane read failed: "+rec.err.Error()+"\n")
		}
		return
	}
	delete(u.rerr, key)
	if u.screen[key] == "" {
		u.screen[key] = u.save(agent, "screen", tailClip(rec.text))
	}
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// save writes one capture file and prunes the directory; it returns the
// file name, or "" when it could not be written.
func (u *unrecCapture) save(agent herdr.AgentInfo, what, body string) string {
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		return ""
	}
	t := u.now()
	name := fmt.Sprintf("%s-%s-%s.txt", t.UTC().Format("20060102T150405Z"), unsafeName.ReplaceAllString(state.AgentKey(&agent), "_"), what)
	head := fmt.Sprintf("agent: %s\nagent kind: %s\nstatus: %s\nhost: %s\ntime: %s\ncapture: %s\n---\n",
		state.AgentKey(&agent), agent.Agent, agent.Status, u.host, t.UTC().Format(time.RFC3339), what)
	if err := os.WriteFile(filepath.Join(u.dir, name), []byte(head+body), 0o600); err != nil {
		return ""
	}
	u.prune(t)
	return name
}

func (u *unrecCapture) prune(now time.Time) {
	ents, err := os.ReadDir(u.dir)
	if err != nil {
		return
	}
	type f struct {
		name string
		mod  time.Time
	}
	var fs []f
	for _, e := range ents {
		if info, err := e.Info(); err == nil && !e.IsDir() && strings.HasSuffix(e.Name(), ".txt") {
			fs = append(fs, f{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].name > fs[j].name }) // newest first: names start with the time
	for i, x := range fs {
		if i >= unrecKeepFiles || now.Sub(x.mod) > unrecKeepAge {
			_ = os.Remove(filepath.Join(u.dir, x.name))
		}
	}
}

// tailClip keeps the last unrecMaxLines lines and at most unrecMaxBytes.
func tailClip(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n "), "\n")
	if len(lines) > unrecMaxLines {
		lines = lines[len(lines)-unrecMaxLines:]
	}
	out := strings.Join(lines, "\n") + "\n"
	if len(out) > unrecMaxBytes {
		out = out[len(out)-unrecMaxBytes:]
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
	}
	return out
}
