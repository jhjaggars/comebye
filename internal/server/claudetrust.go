package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Claude Code asks "Do you trust the files in this folder?" the first time
// it starts in a directory neither it nor a parent has accepted, and holds
// the session (and a brief typed at it) until someone answers. A freshly
// provisioned host (a worker VM, a new machine) has no accepted folders, so
// every worker's worktree would stop there. Before it starts claude in a
// worker's worktree, comebye records that one directory as trusted in
// Claude's own config, the same entry Claude writes when a person answers
// "Yes". Only the worktree comebye just created or reopened is marked, never
// its parent or the worktree root, and only after it was checked to lie
// inside the launch and worktree roots.

var claudeTrustMu sync.Mutex

// claudeConfigPath is Claude Code's global config file: $CLAUDE_CONFIG_DIR/
// .claude.json when that is set, else ~/.claude.json.
func claudeConfigPath() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("home directory unknown")
	}
	return filepath.Join(home, ".claude.json"), nil
}

// trustClaudeFolder marks dir trusted in the Claude config (dir and, when
// it differs, its symlink-resolved form). Every other key of the file and of
// the project's entry is kept as it was. A config that exists but is not a
// JSON object is left alone and reported.
func trustClaudeFolder(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%q is not an absolute path", dir)
	}
	paths := []string{filepath.Clean(dir)}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != paths[0] {
		paths = append(paths, real)
	}
	cfg, err := claudeConfigPath()
	if err != nil {
		return err
	}
	claudeTrustMu.Lock()
	defer claudeTrustMu.Unlock()

	top := map[string]json.RawMessage{}
	mode := os.FileMode(0o600)
	raw, err := os.ReadFile(cfg)
	switch {
	case err == nil:
		if fi, serr := os.Stat(cfg); serr == nil {
			mode = fi.Mode().Perm()
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &top); err != nil {
				return fmt.Errorf("%s is not a JSON object: %w", cfg, err)
			}
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	projects := map[string]json.RawMessage{}
	if p, ok := top["projects"]; ok && string(p) != "null" {
		if err := json.Unmarshal(p, &projects); err != nil {
			return fmt.Errorf("%s: projects is not an object: %w", cfg, err)
		}
	}
	changed := false
	for _, p := range paths {
		entry := map[string]json.RawMessage{}
		if e, ok := projects[p]; ok && string(e) != "null" {
			if err := json.Unmarshal(e, &entry); err != nil {
				return fmt.Errorf("%s: the entry for %s is not an object: %w", cfg, p, err)
			}
		}
		if string(entry["hasTrustDialogAccepted"]) == "true" {
			continue
		}
		entry["hasTrustDialogAccepted"] = json.RawMessage("true")
		b, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		projects[p] = b
		changed = true
	}
	if !changed {
		return nil
	}
	pb, err := json.Marshal(projects)
	if err != nil {
		return err
	}
	top["projects"] = pb
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		return err
	}
	// Write beside the file and rename, so Claude never reads a torn file.
	tmp, err := os.CreateTemp(filepath.Dir(cfg), ".claude.json.comebye-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), cfg)
}
