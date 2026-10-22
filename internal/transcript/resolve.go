package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// ErrNoSession means no session file could be found for the agent.
var ErrNoSession = errors.New("agent has no session file")

// ErrAmbiguousSession means several Claude agents share a working
// directory and none of them reported its session, so the transcript in
// that project directory cannot be attributed to one pane.
var ErrAmbiguousSession = errors.New("several Claude Code agents share this directory and herdr reports no session for them; with `herdr integration install claude`, each session reports its own transcript when it starts, resumes, or is cleared")

// claudeAgent is herdr's agent id for Claude Code.
const claudeAgent = "claude"

// maxSlugLen is where Claude Code truncates long project directory names
// (it then appends a hash suffix this package does not reproduce).
const maxSlugLen = 200

// Resolver maps herdr agents to session files.
type Resolver struct {
	// ProjectsDir is Claude Code's projects directory. Empty means
	// $CLAUDE_CONFIG_DIR/projects, else ~/.claude/projects.
	ProjectsDir string
}

// DefaultResolver is used by SessionPath.
var DefaultResolver = &Resolver{}

// SessionPath resolves a's session file with DefaultResolver.
func SessionPath(a herdr.AgentInfo, live []herdr.AgentInfo) (string, error) {
	return DefaultResolver.SessionPath(a, live)
}

func (r *Resolver) projectsDir() string {
	if r.ProjectsDir != "" {
		return r.ProjectsDir
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// ClaudeProjectSlug is Claude Code's project directory name for cwd: every
// character other than an ASCII letter or digit becomes '-'
// ("/home/me/x.y" -> "-home-me-x-y"). Claude does this with a JavaScript
// regexp over UTF-16 code units, so a non-BMP rune becomes two dashes.
func ClaudeProjectSlug(cwd string) string {
	var sb strings.Builder
	for _, c := range cwd {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			sb.WriteRune(c)
		case c > 0xFFFF:
			sb.WriteString("--")
		default:
			sb.WriteByte('-')
		}
	}
	return sb.String()
}

// SessionPath returns the session file for agent a. live is the full list
// of live agents, used to detect Claude agents sharing a directory.
//
// Order: a session path reported by herdr (any agent); a session id
// reported for a Claude agent, looked up as <projects>/*/<id>.jsonl; and
// for Claude agents with nothing reported (the herdr Claude integration is
// not installed, or the session started before it was), the newest
// transcript in the project directory of the agent's cwd, provided no
// other unreported Claude agent shares that cwd.
func (r *Resolver) SessionPath(a herdr.AgentInfo, live []herdr.AgentInfo) (string, error) {
	if p, ok := r.reported(a); ok {
		// herdr passes on whatever path the agent reported: refuse
		// anything but a regular file (opening a FIFO blocks, a device may
		// never end). A file not written yet is the caller's ENOENT.
		if fi, err := os.Stat(p); err == nil && !fi.Mode().IsRegular() {
			return "", ErrNoSession
		}
		return p, nil
	}
	if a.Agent != claudeAgent || a.CWD == "" {
		return "", ErrNoSession
	}
	cwd := filepath.Clean(a.CWD)
	exclude := map[string]bool{}
	for _, o := range live {
		if o.PaneID == a.PaneID || o.Agent != claudeAgent || o.CWD == "" || filepath.Clean(o.CWD) != cwd {
			continue
		}
		p, ok := r.reported(o)
		if !ok {
			return "", ErrAmbiguousSession
		}
		exclude[filepath.Clean(p)] = true // that transcript belongs to the other pane
	}
	dir := r.projectDir(cwd)
	if dir == "" {
		return "", ErrNoSession
	}
	if p := newestJSONL(dir, exclude); p != "" {
		return p, nil
	}
	return "", ErrNoSession
}

// reported returns the session file herdr reported for a, if any.
func (r *Resolver) reported(a herdr.AgentInfo) (string, bool) {
	s := a.AgentSession
	if s == nil || s.Value == "" {
		return "", false
	}
	switch s.Kind {
	case "path":
		return s.Value, true
	case "id":
		if a.Agent != claudeAgent && s.Agent != claudeAgent {
			return "", false
		}
		if p := r.claudeSessionByID(s.Value, a.CWD); p != "" {
			return p, true
		}
	}
	return "", false
}

// claudeSessionByID finds <projects>/<slug>/<id>.jsonl, trying the cwd's
// project directory first.
func (r *Resolver) claudeSessionByID(id, cwd string) string {
	if id == "" || strings.ContainsAny(id, `/\*?[`) || id == "." || id == ".." {
		return ""
	}
	name := id + ".jsonl"
	if cwd != "" {
		if dir := r.projectDir(filepath.Clean(cwd)); dir != "" {
			p := filepath.Join(dir, name)
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				return p
			}
		}
	}
	root := r.projectsDir()
	if root == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*", name))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

// projectDir returns the existing project directory for cwd, or "".
func (r *Resolver) projectDir(cwd string) string {
	root := r.projectsDir()
	if root == "" {
		return ""
	}
	slug := ClaudeProjectSlug(cwd)
	dir := filepath.Join(root, slug)
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir
	}
	if len(slug) > maxSlugLen {
		// Long paths are truncated and suffixed with a hash.
		matches, _ := filepath.Glob(filepath.Join(root, slug[:maxSlugLen]+"-*"))
		if len(matches) == 1 {
			return matches[0]
		}
	}
	return ""
}

// newestJSONL returns the most recently modified *.jsonl directly in dir.
func newestJSONL(dir string, exclude map[string]bool) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestTime time.Time
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if exclude[p] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = p, info.ModTime()
		}
	}
	return best
}
