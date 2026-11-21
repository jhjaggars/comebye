package idle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A hold keeps a host busy until a time, whatever its agents are doing. The
// server on the host sets one when work arrives that an agent has not picked
// up yet (a prompt or a launch: a handoff in flight), and when it starts an
// upgrade. The idle check, `comebye idle` and the VM's idle-check.sh, honours
// every hold that has not expired.
//
// A hold is one file, <dir>/<name>, holding "<unix seconds until> <reason>".
// Every hold expires on its own, so a server that dies holding one cannot
// keep the host up for ever. Setting a hold with the same name again
// replaces it (the later time wins only if it is later).

// HoldsDir is where holds live under a state directory.
func HoldsDir(stateDir string) string { return filepath.Join(stateDir, "idle-holds") }

// Hold is one hold that has not expired.
type Hold struct {
	Name   string    `json:"name"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

var holdName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// SetHold keeps the host busy until until. A hold already there with a later
// time is kept.
func SetHold(dir, name string, until time.Time, reason string) error {
	if !holdName.MatchString(name) {
		return fmt.Errorf("bad hold name %q", name)
	}
	path := filepath.Join(dir, name)
	if h, ok := readHold(path); ok && !h.Until.Before(until) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) > 200 {
		reason = reason[:200]
	}
	tmp := path + ".tmp"
	// World-readable: the VM's idle check runs as root, a sandbox's as the
	// user; neither needs more than to read it.
	if err := os.WriteFile(tmp, fmt.Appendf(nil, "%d %s\n", until.Unix(), reason), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ClearHold removes a hold; a missing one is not an error.
func ClearHold(dir, name string) error {
	err := os.Remove(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ActiveHolds lists the holds in dir that have not expired at now, soonest
// to expire last. An unreadable or malformed file is not a hold.
func ActiveHolds(dir string, now time.Time) []Hold {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Hold
	for _, e := range ents {
		if e.IsDir() || !holdName.MatchString(e.Name()) {
			continue
		}
		h, ok := readHold(filepath.Join(dir, e.Name()))
		if ok && h.Until.After(now) {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Until.After(out[j].Until) })
	return out
}

func readHold(path string) (Hold, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Hold{}, false
	}
	secs, reason, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	n, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return Hold{}, false
	}
	return Hold{Name: filepath.Base(path), Until: time.Unix(n, 0), Reason: reason}, true
}
