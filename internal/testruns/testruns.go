// Package testruns reads the test runs going on this host: one record per busy
// slot of a test VM, which tools/ci/remote.sh writes into the VM's comebye state
// directory (<state-dir>/test-runs/<run id>.json) when its suite takes a slot,
// renews with the run's idle hold, and removes when the run ends. A record
// carries its own expiry, so one left behind by a run that died is ignored once
// that passes.
//
// The host reports them on its own HostStatus (TestRuns); the hub keeps what a
// peer reports, bounded by Clip. A peer too old to report them sends none, and a
// hub too old to know the field drops it: either way no runs are shown.
package testruns

import (
	"context"
	"encoding/json"
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
	maxRecord  = 4 << 10 // a record file bigger than this is not one
	maxEntries = 256     // directory entries looked at
)

// Dir is where the records live under a state directory.
func Dir(stateDir string) string { return filepath.Join(stateDir, "test-runs") }

// List reads the unexpired records in dir at now, in slot order. A missing
// directory, or a file that is not a well-formed record, is no run.
func List(dir string, now time.Time) []state.TestRun {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []state.TestRun
	for i, e := range ents {
		if i == maxEntries {
			break
		}
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json") {
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
		out = append(out, r)
	}
	return Clip(out, now)
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
// or expires), looking every interval until ctx ends. The host's status is sent
// to browsers and the hub only on changes, and a run is not an agent change.
func Watch(ctx context.Context, dir string, every time.Duration, changed func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	last := signature(List(dir, time.Now()))
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s := signature(List(dir, time.Now())); s != last {
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
	}
	return b.String()
}
