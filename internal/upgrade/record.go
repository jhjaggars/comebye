package upgrade

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Record states.
const (
	RecRunning    = "running"
	RecOK         = "ok"
	RecFailed     = "failed"
	RecRolledBack = "rolled_back" // the new binary failed its smoke check; the old one is back
	RecDryRun     = "dry_run"     // fetched and verified, installed nothing
)

// StaleAfter is how long a "running" record is believed: the unit doing the
// upgrade was killed past it.
const StaleAfter = 15 * time.Minute

// Record is the account of one upgrade (or rollback) run by the app. The
// server writes it as "running" before it starts the run, and the run
// finishes it, since the run restarts the very service that started it: the
// new server reads the verdict from the file.
type Record struct {
	ID       string    `json:"id"`
	Version  string    `json:"version"`
	Rollback bool      `json:"rollback,omitempty"`
	From     string    `json:"from,omitempty"` // the version running when it began
	To       string    `json:"to,omitempty"`   // the version running when it ended
	State    string    `json:"state"`
	By       string    `json:"by,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Error    string    `json:"error,omitempty"`
	Output   string    `json:"output,omitempty"` // the tail of the run's output
}

// Live reports whether the record is a run still believed to be going.
func (r Record) Live(now time.Time) bool {
	return r.State == RecRunning && now.Sub(r.Started) < StaleAfter
}

// ReadRecord reads a record file; a missing file is an empty record.
func ReadRecord(path string) (Record, error) {
	var r Record
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

// WriteRecord replaces the record file atomically.
func WriteRecord(path string, r Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RunRecorded is Run, and finishes the record at path (started by the
// server) with the verdict and the tail of the output.
func RunRecorded(o Options, path string) error {
	rec, err := ReadRecord(path)
	if err != nil {
		return err
	}
	if rec.Started.IsZero() {
		rec.Started = time.Now().UTC()
	}
	rec.State = RecRunning
	capped := &tailWriter{max: 6000}
	var w io.Writer = capped
	if o.Out != nil {
		o.Out = io.MultiWriter(o.Out, w)
	} else {
		o.Out = w
	}
	if o.Err != nil {
		o.Err = io.MultiWriter(o.Err, w)
	} else {
		o.Err = w
	}
	o.defaults()
	rec.From, _ = binVersion(&o, o.Bin)
	rec.Rollback = o.Rollback
	_ = WriteRecord(path, rec)

	runErr := Run(o)
	rec.Finished = time.Now().UTC()
	rec.To, _ = binVersion(&o, o.Bin)
	rec.Output = strings.TrimSpace(capped.String())
	switch {
	case runErr == nil && o.DryRun:
		rec.State = RecDryRun
	case runErr == nil:
		rec.State = RecOK
	case errors.Is(runErr, ErrRolledBack):
		rec.State, rec.Error = RecRolledBack, runErr.Error()
	default:
		rec.State, rec.Error = RecFailed, runErr.Error()
	}
	if werr := WriteRecord(path, rec); werr != nil && runErr == nil {
		return werr
	}
	return runErr
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	max int
	b   []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string { return strings.ToValidUTF8(string(t.b), "?") }
