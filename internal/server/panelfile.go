package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The Doc panel's host op: read one text document of an agent's, under
// rules that keep it from being a way to read the host's files.
//
//   - the path is made absolute and its symlinks resolved first (realpath),
//     and only then judged: a link out of the worktree is out;
//   - it must be inside the agent's worktree (or working directory), or be
//     the document the agent declared (`comebye show`);
//   - and inside the user's home, with no dot-directory (or dot-file) below
//     the worktree (or, for a declared file elsewhere, below home);
//   - a .md, .markdown, .txt or .html file, up to 1 MB, valid UTF-8;
//   - never a secret file (.env, *.pem, id_*).
const (
	panelDocMax  = 1 << 20
	panelListMax = 300
)

var panelDocExt = map[string]string{".md": "markdown", ".markdown": "markdown", ".txt": "text", ".html": "html"}

type fileErr struct {
	status  int
	code    string
	message string
}

func (e *fileErr) Error() string { return e.message }

func ferr(status int, code, msg string) *fileErr { return &fileErr{status, code, msg} }

func writeFileErr(w http.ResponseWriter, e *fileErr) {
	writeJSONStatus(w, e.status, map[string]string{"code": e.code, "message": e.message})
}

// panelRoots are what a request is judged against, all resolved.
type panelRoots struct {
	home, dir, pin string
}

func realDir(p string) (string, bool) {
	if p == "" || !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
		return "", false
	}
	r, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return "", false
	}
	return r, true
}

func newPanelRoots(dir, pin string) (panelRoots, *fileErr) {
	var pr panelRoots
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return pr, ferr(http.StatusForbidden, "no_home", "this host has no home directory to read documents from")
	}
	home, ok := realDir(h)
	if !ok {
		return pr, ferr(http.StatusForbidden, "no_home", "this host has no home directory to read documents from")
	}
	pr.home = home
	if d, ok := realDir(dir); ok && within(d, home) {
		pr.dir = d
	}
	if pin != "" {
		if p, ok := realDir(pin); ok {
			pr.pin = p
		}
	}
	return pr, nil
}

// hasDot reports whether any element of rel (a slash path below a root)
// starts with a dot.
func hasDot(rel string) bool {
	for _, e := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.HasPrefix(e, ".") && e != "." {
			return true
		}
	}
	return false
}

// checkDocPath applies the rules to path (absolute, or relative to the
// worktree) and returns its resolved name and kind.
func (pr panelRoots) checkDocPath(path string) (abs, kind string, e *fileErr) {
	if path == "" || len(path) > 4096 || strings.ContainsRune(path, 0) {
		return "", "", ferr(http.StatusBadRequest, "bad_path", "name a file")
	}
	if !filepath.IsAbs(path) {
		if pr.dir == "" {
			return "", "", ferr(http.StatusForbidden, "outside_allowed", "this agent has no worktree here to read from")
		}
		path = filepath.Join(pr.dir, path)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		// Not found, but say nothing about places that are not allowed.
		if !within(filepath.Clean(path), pr.home) {
			return "", "", ferr(http.StatusForbidden, "outside_allowed", "that file is not one this panel may read")
		}
		return "", "", ferr(http.StatusNotFound, "not_found", "no such file")
	}
	if !within(real, pr.home) {
		return "", "", ferr(http.StatusForbidden, "outside_allowed", "that file is not one this panel may read (outside the home directory)")
	}
	switch {
	case pr.dir != "" && within(real, pr.dir):
		if rel, _ := filepath.Rel(pr.dir, real); hasDot(rel) {
			return "", "", ferr(http.StatusForbidden, "dot_dir", "files in hidden (dot) directories are not read")
		}
	case pr.pin != "" && real == pr.pin:
		if rel, _ := filepath.Rel(pr.home, real); hasDot(rel) {
			return "", "", ferr(http.StatusForbidden, "dot_dir", "files in hidden (dot) directories are not read")
		}
	default:
		return "", "", ferr(http.StatusForbidden, "outside_allowed", "that file is neither in this agent's worktree nor the document it declared")
	}
	if secretFileName(real) != "" {
		return "", "", ferr(http.StatusForbidden, "secret_file", "secret files are not shown")
	}
	kind = panelDocExt[strings.ToLower(filepath.Ext(real))]
	if kind == "" {
		return "", "", ferr(http.StatusUnsupportedMediaType, "bad_type", "only .md, .markdown, .txt and .html files are shown")
	}
	return real, kind, nil
}

type panelDoc struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Size    int64  `json:"size"`
	MTime   int64  `json:"mtime"`
	Sig     string `json:"sig"`
	Content string `json:"content,omitempty"`
	// Unchanged answers a request whose since= matched.
	Unchanged bool `json:"unchanged,omitempty"`
	// SectionFound: for a request that named a section of a Markdown file.
	SectionFound *bool `json:"section_found,omitempty"`
}

// readPanelDoc reads path under the rules. meta: no content. since: the sig
// of what the caller has.
func (pr panelRoots) readPanelDoc(path, since, section string, meta bool) (*panelDoc, *fileErr) {
	abs, kind, e := pr.checkDocPath(path)
	if e != nil {
		return nil, e
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, ferr(http.StatusNotFound, "not_found", "no such file")
	}
	if fi.Size() > panelDocMax {
		return nil, ferr(http.StatusRequestEntityTooLarge, "too_big", "the file is over 1 MB ("+strconv.FormatInt(fi.Size(), 10)+" bytes)")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, ferr(http.StatusNotFound, "not_found", "could not read the file")
	}
	if len(data) > panelDocMax {
		return nil, ferr(http.StatusRequestEntityTooLarge, "too_big", "the file is over 1 MB")
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data[:min(len(data), 8000)]), 0) {
		return nil, ferr(http.StatusUnsupportedMediaType, "bad_type", "the file is not text")
	}
	h := sha256.Sum256(data)
	d := &panelDoc{Path: abs, Name: filepath.Base(abs), Kind: kind, Size: fi.Size(), MTime: fi.ModTime().UnixMilli(), Sig: hex.EncodeToString(h[:8])}
	if section != "" && kind == "markdown" {
		found := false
		for _, h := range headingsOf(string(data)) {
			if headingMatches(h, section) {
				found = true
				break
			}
		}
		d.SectionFound = &found
	}
	switch {
	case since != "" && since == d.Sig:
		d.Unchanged = true
	case !meta:
		d.Content = string(data)
	}
	return d, nil
}

var headingRE = regexp.MustCompile(`(?m)^ {0,3}#{1,6}[ \t]+(.+?)[ \t#]*$`)

func headingsOf(md string) []string {
	var out []string
	fence := false
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") || strings.HasPrefix(strings.TrimSpace(l), "~~~") {
			fence = !fence
			continue
		}
		if fence {
			continue
		}
		if m := headingRE.FindStringSubmatch(l); m != nil {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	return out
}

// headingMatches: the cited section is the heading's text, ignoring case,
// a leading "#" marks, and surrounding punctuation or numbering.
func headingMatches(heading, want string) bool {
	norm := func(s string) string {
		return strings.ToLower(strings.TrimSpace(strings.Trim(strings.TrimLeft(strings.TrimSpace(s), "# "), " .:)")))
	}
	h, w := norm(heading), norm(want)
	return w != "" && (h == w || strings.HasPrefix(h, w))
}

type panelListEntry struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

// listPanelDocs lists the documents in the worktree a user may pick: .md,
// .markdown, .txt and .html files below no dot-directory, newest first.
func (pr panelRoots) listPanelDocs(ctx context.Context) []panelListEntry {
	if pr.dir == "" {
		return []panelListEntry{}
	}
	var rels []string
	if out, _, err := panelGit(ctx, pr.dir, 8<<20, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "."); err == nil {
		for _, r := range strings.Split(string(out), "\x00") {
			if r != "" {
				rels = append(rels, r)
			}
		}
	} else {
		filepath.WalkDir(pr.dir, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(pr.dir, p)
			if de.IsDir() {
				if rel != "." && (strings.HasPrefix(de.Name(), ".") || de.Name() == "node_modules" || strings.Count(rel, string(filepath.Separator)) >= 4) {
					return filepath.SkipDir
				}
				return nil
			}
			rels = append(rels, filepath.ToSlash(rel))
			return nil
		})
	}
	out := []panelListEntry{}
	for _, rel := range rels {
		if panelDocExt[strings.ToLower(filepath.Ext(rel))] == "" || hasDot(rel) || secretFileName(rel) != "" {
			continue
		}
		if len(out) >= 4*panelListMax {
			break
		}
		fi, err := os.Lstat(filepath.Join(pr.dir, filepath.FromSlash(rel)))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > panelDocMax {
			continue
		}
		out = append(out, panelListEntry{Path: rel, Size: fi.Size(), MTime: fi.ModTime().UnixMilli()})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].MTime > out[j].MTime })
	if len(out) > panelListMax {
		out = out[:panelListMax]
	}
	return out
}
