// Package testruns reads the test runs going on this host: one record per busy
// slot of a test VM, which tools/ci/remote.sh writes into the VM's comebye state
// directory (<state-dir>/test-runs/<run id>.json) when its suite takes a slot,
// renews with the run's idle hold, and removes when the run ends. A record
// carries its own expiry, so one left behind by a run that died is ignored once
// that passes.
//
// When a run ends, remote.sh leaves its done record in test-runs/done/<run id>.json
// (the same fields, and the end, seconds, exit status and temperatures), with the
// last 256 KiB of the suite's output beside it (<run id>.log). The VM keeps the last
// 50; the host reports the last 20 (History) and serves a log (Log).
//
// The host reports them on its own HostStatus (TestRuns); the hub keeps what a
// peer reports, bounded by Clip. A peer too old to report them sends none, and a
// hub too old to know the field drops it: either way no runs are shown.
package testruns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhjaggars/comebye/internal/state"
)

// Limits on what a host reports (its own, and what the hub keeps of a peer's).
const (
	MaxRuns    = 16  // a VM has at most 8 slots (tools/ci/remote.sh)
	MaxText    = 200 // each text field, in bytes
	MaxSlots   = 64
	MaxHistory = 20        // finished runs reported (the VM keeps 50)
	MaxLog     = 256 << 10 // the most of a run's log served
	maxRecord  = 4 << 10   // a record file bigger than this is not one
	maxEntries = 256       // directory entries looked at
)

// Dir is where the records live under a state directory.
func Dir(stateDir string) string { return filepath.Join(stateDir, "test-runs") }

// DoneDir is where finished runs' records and logs live under a state directory.
func DoneDir(stateDir string) string { return filepath.Join(Dir(stateDir), "done") }

// List reads the unexpired records in dir at now, in slot order. A missing
// directory, or a file that is not a well-formed record, is no run.
func List(dir string, now time.Time) []state.TestRun {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	return Clip(readRecords(dir, ents, maxEntries, nil), now)
}

// History reads the finished runs in dir (DoneDir), newest first, at most
// MaxHistory; each says whether its log is there. A missing directory is none.
func History(dir string) []state.TestRun {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	logs := map[string]bool{}
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".log"); ok && e.Type().IsRegular() {
			logs[n] = true
		}
	}
	// The VM keeps 50 records and their logs: twice that, and some, is plenty.
	return ClipHistory(readRecords(dir, ents, 4*maxEntries, logs))
}

// readRecords reads the well-formed records among ents (at most limit
// entries looked at). With logs, each record's Log is whether its name is in
// it (a record never says so itself).
func readRecords(dir string, ents []os.DirEntry, limit int, logs map[string]bool) []state.TestRun {
	var out []state.TestRun
	for i, e := range ents {
		if i == limit {
			break
		}
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !e.Type().IsRegular() || !ok {
			continue
		}
		b, err := readSmall(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r state.TestRun
		if json.Unmarshal(b, &r) != nil || r.ID == "" {
			continue
		}
		r.Log = logs != nil && logs[name] && r.ID == name
		out = append(out, r)
	}
	return out
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, maxRecord+1)
	n, _ := f.Read(b)
	if n > maxRecord {
		return nil, os.ErrInvalid
	}
	return b[:n], nil
}

// Clip bounds a list of runs: the expired ones and those without an id or a
// start go, each text field is cut to MaxText, the slots are made sane, and at
// most MaxRuns are kept, in slot order (then start).
func Clip(in []state.TestRun, now time.Time) state.TestRuns {
	var out []state.TestRun
	for _, r := range in {
		if r.ID == "" || r.Started.IsZero() || r.Expires.IsZero() || !r.Expires.After(now) {
			continue
		}
		r.ID, r.Suite, r.Branch, r.Head, r.Agent, r.From = clip(r.ID), clip(r.Suite), clip(r.Branch), clip(r.Head), clip(r.Agent), clip(r.From)
		r.Slots = min(max(r.Slots, 1), MaxSlots)
		r.Slot = min(max(r.Slot, 1), r.Slots)
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Slot != out[j].Slot {
			return out[i].Slot < out[j].Slot
		}
		return out[i].Started.Before(out[j].Started)
	})
	if len(out) > MaxRuns {
		out = out[:MaxRuns]
	}
	return out
}

// ClipHistory bounds a list of finished runs: those without an id, a start, an
// end or an exit status go, the text and numbers are made sane like Clip's (a
// temperature outside -50..200 °C is no reading), and the newest MaxHistory
// are kept, newest first. A live run's expiry means nothing here and is dropped.
func ClipHistory(in []state.TestRun) state.TestRuns {
	var out []state.TestRun
	for _, r := range in {
		if r.ID == "" || r.Started.IsZero() || r.Ended.IsZero() || r.RC == nil {
			continue
		}
		r.ID, r.Suite, r.Branch, r.Head, r.Agent, r.From = clip(r.ID), clip(r.Suite), clip(r.Branch), clip(r.Head), clip(r.Agent), clip(r.From)
		r.Slots = min(max(r.Slots, 1), MaxSlots)
		r.Slot = min(max(r.Slot, 1), r.Slots)
		r.Secs = min(max(r.Secs, 0), 366*24*3600)
		r.Expires = time.Time{}
		rc := min(max(*r.RC, 0), 255)
		r.RC = &rc
		r.CPUStart, r.NVMeStart, r.CPUEnd, r.NVMeEnd = temp(r.CPUStart), temp(r.NVMeStart), temp(r.CPUEnd), temp(r.NVMeEnd)
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Ended.Equal(out[j].Ended) {
			return out[i].Ended.After(out[j].Ended)
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > MaxHistory {
		out = out[:MaxHistory]
	}
	return out
}

func temp(t *int) *int {
	if t == nil || *t < -50 || *t > 200 {
		return nil
	}
	v := *t
	return &v
}

// ValidID reports whether id can name a run's files: what remote.sh makes
// (lower-case letters, digits and '-'), at most 128 bytes.
func ValidID(id string) bool {
	if id == "" || len(id) > 128 || id[0] == '-' {
		return false
	}
	for _, c := range []byte(id) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// ErrNoLog is a run with no log here (or an id that cannot be one).
var ErrNoLog = errors.New("no log for that run")

// Log reads the log of the finished run id in dir (DoneDir): at most its last
// MaxLog bytes, cut to start on a whole line when it was cut.
func Log(dir, id string) ([]byte, error) {
	if !ValidID(id) {
		return nil, ErrNoLog
	}
	f, err := os.Open(filepath.Join(dir, id+".log"))
	if err != nil {
		return nil, ErrNoLog
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, ErrNoLog
	}
	cut := fi.Size() > MaxLog
	if cut {
		if _, err := f.Seek(-MaxLog, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxLog))
	if err != nil {
		return nil, err
	}
	if cut {
		if i := bytes.IndexByte(b, '\n'); i >= 0 && i < len(b)-1 {
			b = b[i+1:]
		}
	}
	return b, nil
}

func clip(s string) string {
	if len(s) > MaxText {
		s = s[:MaxText]
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	return s
}

// Watch calls changed whenever the set of runs in dir changes (one starts, ends
// or expires) or a finished one is added to its done records, looking every
// interval until ctx ends. The host's status is sent to browsers and the hub
// only on changes, and a run is not an agent change.
func Watch(ctx context.Context, dir string, every time.Duration, changed func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	sig := func() string {
		return signature(List(dir, time.Now())) + "|" + signature(History(filepath.Join(dir, "done")))
	}
	last := sig()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s := sig(); s != last {
			last = s
			changed()
		}
	}
}

func signature(runs []state.TestRun) string {
	var b strings.Builder
	for _, r := range runs {
		b.WriteString(r.ID)
		b.WriteByte(0)
		b.WriteString(r.Started.String())
		b.WriteByte(0)
		if r.RC != nil { // a finished run: its temperatures and log can come later
			fmt.Fprintf(&b, "%d %v %v %v %v %v", *r.RC, r.Log, r.CPUStart != nil, r.NVMeStart != nil, r.CPUEnd != nil, r.NVMeEnd != nil)
			b.WriteByte(0)
		}
	}
	return b.String()
}
