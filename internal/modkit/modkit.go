// Package modkit is comebye's Claude Code mods: embedded sources, and
// `comebye mod install|status|uninstall`, which writes them to a folder and
// registers that folder as a local Claude Code marketplace. Claude reads a
// plugin whose marketplace entry is a relative path from the folder itself,
// so a new comebye only rewrites files there; no version bump, no reinstall.
//
// It only ever touches its own folder, the marketplace "comebye-mods" and the
// plugins "…@comebye-mods". Another plugin or marketplace is never listed
// for change, updated or removed.
package modkit

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed all:mods
var embedded embed.FS

// Marketplace is the name of the local marketplace the folder holds.
const Marketplace = "comebye-mods"

// Plugin names. Hub is installed by default; the coordinator window is early
// access and only ever installed on request (--with coord-window).
const (
	Hub         = "comebye-hub"
	CoordWindow = "comebye-coord-window"
)

// Plugins are the plugin names the marketplace lists.
var Plugins = []string{Hub, CoordWindow}

const recordName = ".comebye-mods.json"

// File is one embedded file, relative to the mods folder.
type File struct {
	Rel  string
	Data []byte
}

// Files are the embedded mod files. Test files are not shipped.
func Files() ([]File, error) {
	var out []File
	err := fs.WalkDir(embedded, "mods", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.Contains(d.Name(), ".test.") {
			return nil
		}
		b, err := embedded.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, File{Rel: strings.TrimPrefix(p, "mods/"), Data: b})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, err
}

// DefaultDir is where the mods folder goes: $XDG_DATA_HOME/comebye/mods, else
// ~/.local/share/comebye/mods.
func DefaultDir(getenv func(string) string) (string, error) {
	if x := getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "comebye", "mods"), nil
	}
	home := getenv("HOME")
	if home == "" {
		return "", errors.New("HOME is not set")
	}
	return filepath.Join(home, ".local", "share", "comebye", "mods"), nil
}

// File states.
const (
	Missing  = "missing"
	Current  = "current"
	Outdated = "outdated" // comebye wrote an older one, unedited
	Edited   = "edited"   // not what comebye wrote: the user's
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type record struct {
	Files map[string]string `json:"files"` // rel -> sha256 of what comebye last wrote
}

func readRecord(dir string) record {
	r := record{Files: map[string]string{}}
	if b, err := os.ReadFile(filepath.Join(dir, recordName)); err == nil {
		json.Unmarshal(b, &r)
		if r.Files == nil {
			r.Files = map[string]string{}
		}
	}
	return r
}

func writeRecord(dir string, r record) error {
	b, _ := json.MarshalIndent(r, "", "  ")
	return os.WriteFile(filepath.Join(dir, recordName), append(b, '\n'), 0o644)
}

func state(dir string, f File, rec record) (string, error) {
	have, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Rel)))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Missing, nil
	case err != nil:
		return "", err
	}
	switch h := sum(have); {
	case h == sum(f.Data):
		return Current, nil
	case rec.Files[f.Rel] == h:
		return Outdated, nil
	}
	return Edited, nil
}

// FileState is one file's state in the folder.
type FileState struct {
	Rel    string `json:"file"`
	State  string `json:"state"`
	Backup string `json:"backup,omitempty"`
}

// Claude runs the `claude` program. Tests replace it.
type Claude interface {
	Run(args ...string) (stdout string, err error)
}

// ExecClaude runs `claude` from PATH.
type ExecClaude struct{}

func (ExecClaude) Run(args ...string) (string, error) {
	cmd := exec.Command("claude", args...)
	out, err := cmd.Output()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && len(ee.Stderr) > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return string(out), err
}

// Options for Install, Status and Uninstall.
type Options struct {
	Dir      string   // the mods folder
	Claude   Claude   // nil: ExecClaude
	With     []string // plugins to install besides the default; Install only
	Force    bool     // Install: replace an edited file (kept as .bak)
	NoClaude bool     // write the folder only; do not run claude at all
	// IfInstalled: Install refreshes the files of a folder comebye wrote
	// before and does nothing where there is none; it never runs claude.
	// `comebye upgrade` uses it.
	IfInstalled bool
}

func (o *Options) claude() Claude {
	if o.Claude == nil {
		return ExecClaude{}
	}
	return o.Claude
}

// Report is what Install, Status and Uninstall found or did.
type Report struct {
	Dir         string      `json:"dir"`
	Files       []FileState `json:"files"`
	Marketplace string      `json:"marketplace"` // registered, registered-elsewhere, missing, unknown
	Installed   []string    `json:"installed"`   // plugin names installed from the marketplace
	Notes       []string    `json:"notes,omitempty"`
}

type marketJSON struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type pluginJSON struct {
	ID string `json:"id"`
}

func wantPlugins(with []string) ([]string, error) {
	out := []string{Hub}
	for _, w := range with {
		w = strings.TrimPrefix(w, "comebye-")
		name := "comebye-" + w
		if w == "coord-window" || w == "hub" {
			if name != Hub {
				out = append(out, name)
			}
			continue
		}
		return nil, fmt.Errorf("unknown mod %q (want coord-window)", w)
	}
	return out, nil
}

func registered(c Claude, dir string, r *Report) {
	out, err := c.Run("plugin", "marketplace", "list", "--json")
	if err != nil {
		r.Marketplace = "unknown"
		r.Notes = append(r.Notes, "could not ask claude: "+err.Error())
		return
	}
	var ms []marketJSON
	if err := json.Unmarshal([]byte(out), &ms); err != nil {
		r.Marketplace = "unknown"
		return
	}
	r.Marketplace = "missing"
	for _, m := range ms {
		if m.Name == Marketplace {
			r.Marketplace = "registered"
			if filepath.Clean(m.Path) != filepath.Clean(dir) {
				r.Marketplace = "registered-elsewhere"
				r.Notes = append(r.Notes, fmt.Sprintf("the marketplace %s points at %s, not %s", Marketplace, m.Path, dir))
			}
		}
	}
}

func installedPlugins(c Claude) ([]string, error) {
	out, err := c.Run("plugin", "list", "--json")
	if err != nil {
		return nil, err
	}
	var ps []pluginJSON
	if err := json.Unmarshal([]byte(out), &ps); err != nil {
		return nil, err
	}
	var names []string
	for _, p := range ps {
		if n, m, ok := strings.Cut(p.ID, "@"); ok && m == Marketplace {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Status reads the folder and, unless NoClaude, claude's registration.
func Status(o Options) (*Report, error) {
	files, err := Files()
	if err != nil {
		return nil, err
	}
	r := &Report{Dir: o.Dir, Marketplace: "unknown"}
	rec := readRecord(o.Dir)
	for _, f := range files {
		st, err := state(o.Dir, f, rec)
		if err != nil {
			return nil, err
		}
		r.Files = append(r.Files, FileState{Rel: f.Rel, State: st})
	}
	if !o.NoClaude {
		registered(o.claude(), o.Dir, r)
		if names, err := installedPlugins(o.claude()); err == nil {
			r.Installed = names
		}
	}
	return r, nil
}

// Install writes the folder, then registers it as a marketplace and installs
// the default plugin and those asked for (With). It is idempotent, updates
// files it wrote before, and leaves a file the user edited (unless Force,
// which keeps the edited one as .bak).
func Install(o Options) (*Report, error) {
	if o.IfInstalled {
		if _, err := os.Stat(filepath.Join(o.Dir, recordName)); err != nil {
			return &Report{Dir: o.Dir, Marketplace: "unknown", Notes: []string{"mods are not installed here; nothing done"}}, nil
		}
		o.NoClaude = true
	}
	plugins, err := wantPlugins(o.With)
	if err != nil {
		return nil, err
	}
	files, err := Files()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, err
	}
	r := &Report{Dir: o.Dir, Marketplace: "unknown"}
	rec := readRecord(o.Dir)
	for _, f := range files {
		st, err := state(o.Dir, f, rec)
		if err != nil {
			return nil, err
		}
		fsr := FileState{Rel: f.Rel, State: st}
		target := filepath.Join(o.Dir, filepath.FromSlash(f.Rel))
		write := func() error {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(target, f.Data, 0o644); err != nil {
				return err
			}
			rec.Files[f.Rel] = sum(f.Data)
			return nil
		}
		switch st {
		case Missing, Outdated:
			if err := write(); err != nil {
				return nil, err
			}
			fsr.State = map[string]string{Missing: "installed", Outdated: "updated"}[st]
		case Current:
			rec.Files[f.Rel] = sum(f.Data)
		case Edited:
			if o.Force {
				if b, err := os.ReadFile(target); err == nil {
					if err := os.WriteFile(target+".bak", b, 0o644); err != nil {
						return nil, err
					}
				}
				fsr.Backup = f.Rel + ".bak"
				if err := write(); err != nil {
					return nil, err
				}
				fsr.State = "replaced"
			}
		}
		r.Files = append(r.Files, fsr)
	}
	if err := writeRecord(o.Dir, rec); err != nil {
		return nil, err
	}
	if o.NoClaude {
		return r, nil
	}
	c := o.claude()
	registered(c, o.Dir, r)
	switch r.Marketplace {
	case "missing":
		if _, err := c.Run("plugin", "marketplace", "add", o.Dir); err != nil {
			return r, fmt.Errorf("claude plugin marketplace add: %w", err)
		}
		r.Marketplace = "registered"
	case "registered-elsewhere":
		return r, errors.New("a marketplace named " + Marketplace + " already points elsewhere: run `comebye mod uninstall` for it first")
	case "unknown":
		return r, errors.New("could not register the marketplace with claude (is `claude` on PATH?)")
	}
	have, err := installedPlugins(c)
	if err != nil {
		return r, fmt.Errorf("claude plugin list: %w", err)
	}
	for _, p := range plugins {
		if !contains(have, p) {
			if _, err := c.Run("plugin", "install", p+"@"+Marketplace); err != nil {
				return r, fmt.Errorf("claude plugin install %s: %w", p, err)
			}
			have = append(have, p)
		}
	}
	sort.Strings(have)
	r.Installed = have
	return r, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Uninstall uninstalls the plugins "…@comebye-mods", removes the marketplace
// "comebye-mods" and deletes the files comebye wrote that were not edited.
// An edited file, and anything else in the folder, stays.
func Uninstall(o Options) (*Report, error) {
	files, err := Files()
	if err != nil {
		return nil, err
	}
	r := &Report{Dir: o.Dir, Marketplace: "unknown"}
	if !o.NoClaude {
		c := o.claude()
		if names, err := installedPlugins(c); err == nil {
			for _, p := range names {
				if _, err := c.Run("plugin", "uninstall", p+"@"+Marketplace); err != nil {
					return r, fmt.Errorf("claude plugin uninstall %s: %w", p, err)
				}
			}
		} else {
			r.Notes = append(r.Notes, "could not list plugins: "+err.Error())
		}
		registered(c, o.Dir, r)
		if r.Marketplace == "registered" || r.Marketplace == "registered-elsewhere" {
			if _, err := c.Run("plugin", "marketplace", "remove", Marketplace); err != nil {
				return r, fmt.Errorf("claude plugin marketplace remove: %w", err)
			}
			r.Marketplace = "missing"
		}
	}
	rec := readRecord(o.Dir)
	for _, f := range files {
		st, err := state(o.Dir, f, rec)
		if err != nil {
			return nil, err
		}
		fsr := FileState{Rel: f.Rel, State: st}
		switch st {
		case Current, Outdated:
			if err := os.Remove(filepath.Join(o.Dir, filepath.FromSlash(f.Rel))); err != nil {
				return nil, err
			}
			fsr.State = "removed"
			delete(rec.Files, f.Rel)
		case Edited:
			fsr.State = "kept (edited)"
		}
		r.Files = append(r.Files, fsr)
	}
	// Empty directories, deepest first; then the record when nothing is left.
	var dirs []string
	filepath.WalkDir(o.Dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != o.Dir {
			dirs = append(dirs, p)
		}
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		os.Remove(d) // only succeeds when empty
	}
	if len(rec.Files) == 0 {
		os.Remove(filepath.Join(o.Dir, recordName))
		os.Remove(o.Dir)
	} else {
		writeRecord(o.Dir, rec)
	}
	return r, nil
}
