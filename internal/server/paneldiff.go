package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The Diff panel's host op: what an agent's checkout changed, file by file.
// Four sections, each a list of files with their own unified diff:
//
//	unstaged   git diff
//	staged     git diff --cached
//	untracked  files git does not track yet (shown as all-added)
//	commits    commits since the base branch (git diff MERGE-BASE HEAD)
//
// Secrets never leave the host: .env, *.pem and id_* files are withheld (a
// line count only) and obvious secrets in other lines are replaced.

const (
	// panelFileCap is the largest diff shown for one file; a bigger one is
	// a "too big, N lines" placeholder.
	panelFileCap = 48 << 10
	// panelTotalCap is the most diff text one answer carries.
	panelTotalCap = 768 << 10
	// panelLoadCap is the most diff text one "Load diff" fetch returns.
	panelLoadCap = 1 << 20
	// panelMaxFiles is the most files listed per section.
	panelMaxFiles = 300
	// panelGitRead is the most output read from one git command.
	panelGitRead = 12 << 20
	// panelUntrackedMax is the most untracked files read.
	panelUntrackedMax = 100
)

type panelCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

type panelFile struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	// Status: added, modified, deleted, renamed or untracked.
	Status string `json:"status"`
	Add    int    `json:"add"`
	Del    int    `json:"del"`
	Binary bool   `json:"binary,omitempty"`
	// Withheld says why the contents are not here (a secret file); Lines is
	// then the size of the change. TooBig: the text is not here; Skip says
	// why: "size" (over the per-file cap), "total" (the answer's budget was
	// spent on other files) or "load" (over even the one-off cap of a fetch).
	// Bytes is the size of the diff text, 0 when not known. Both kinds can be
	// fetched on their own (GET <agent>/diff?file=).
	Withheld string `json:"withheld,omitempty"`
	TooBig   bool   `json:"too_big,omitempty"`
	Skip     string `json:"skip,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
	Lines    int    `json:"lines,omitempty"`
	// Generated marks output that is not hand-written (see generatedPath):
	// it is served last when the budget runs out and starts collapsed.
	Generated bool `json:"generated,omitempty"`
	// Redacted counts the secrets replaced in Diff.
	Redacted int `json:"redacted,omitempty"`
	// Diff holds the hunks (from the first @@), "" when withheld or too big.
	Diff string `json:"diff,omitempty"`
}

type panelSection struct {
	Key   string      `json:"key"`
	Title string      `json:"title"`
	Files []panelFile `json:"files"`
}

type panelDiff struct {
	Sig       string         `json:"sig"`
	Unchanged bool           `json:"unchanged,omitempty"`
	NotGit    bool           `json:"not_git,omitempty"`
	Dir       string         `json:"dir,omitempty"`
	Branch    string         `json:"branch,omitempty"`
	Base      string         `json:"base,omitempty"`
	Commits   []panelCommit  `json:"commits,omitempty"`
	Sections  []panelSection `json:"sections,omitempty"`
	Files     int            `json:"files"`
	Add       int            `json:"add"`
	Del       int            `json:"del"`
	Redacted  int            `json:"redacted,omitempty"`
	Note      string         `json:"note,omitempty"`
}

// panelGit runs git in dir and reads up to max bytes of its output;
// truncated says there was more.
func panelGit(ctx context.Context, dir string, max int, args ...string) (out []byte, truncated bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.quotepath=false", "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err = cmd.Start(); err != nil {
		return nil, false, err
	}
	out, _ = io.ReadAll(io.LimitReader(pipe, int64(max)+1))
	if len(out) > max {
		truncated, out = true, out[:max]
		cancel()
	}
	err = cmd.Wait()
	if truncated {
		err = nil
	}
	return out, truncated, err
}

// secretFileName reports whether a file's contents are never shown: private
// keys and environment files. Examples (.env.example) are fine.
func secretFileName(p string) string {
	b := strings.ToLower(path.Base(filepath.ToSlash(p)))
	switch {
	case b == ".env.example" || b == ".env.sample" || b == ".env.template" || b == ".env.dist":
		return ""
	case b == ".env" || strings.HasPrefix(b, ".env."):
		return "environment file"
	case strings.HasSuffix(b, ".pem") || strings.HasSuffix(b, ".key") || strings.HasSuffix(b, ".p12") || strings.HasSuffix(b, ".pfx"):
		return "key file"
	case strings.HasPrefix(b, "id_"):
		return "SSH key"
	}
	return ""
}

var (
	secretTokenRE = regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|sk-[A-Za-z0-9_-]{20,}|AIza[0-9A-Za-z_-]{30,}|eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
	// secretAssignRE: name = value for names that hold secrets; the value
	// (8+ characters, no spaces) is replaced and the name kept.
	secretAssignRE = regexp.MustCompile(`(?i)((?:secret|token|passw(?:or)?d|passwd|api[_-]?key|access[_-]?key|private[_-]?key|authorization|bearer)[\w.-]*["']?\s*[:=]\s*["']?)([^\s"',;]{8,})`)
	pemBeginRE     = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	pemEndRE       = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	b64LineRE      = regexp.MustCompile(`^[A-Za-z0-9+/=]{32,}$`)
)

const redactedMark = "«redacted»"

// redactDiff replaces obvious secrets in a diff body, line by line, and
// counts the replacements. A private key block is replaced as a whole.
func redactDiff(body string) (string, int) {
	n, inKey := 0, false
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if len(l) == 0 || l[0] == '@' || l[0] == '\\' {
			continue
		}
		text := l[1:]
		switch {
		case pemBeginRE.MatchString(text):
			inKey = true
			lines[i] = l[:1] + pemBeginRE.ReplaceAllString(text, "-----BEGIN PRIVATE KEY----- "+redactedMark)
			n++
			continue
		case inKey && pemEndRE.MatchString(text):
			inKey = false
			continue
		case inKey || (b64LineRE.MatchString(strings.TrimSpace(text)) && len(strings.TrimSpace(text)) >= 64 && strings.Contains(body, "PRIVATE KEY")):
			lines[i] = l[:1] + redactedMark
			if inKey {
				continue
			}
			n++
			continue
		}
		t := secretTokenRE.ReplaceAllStringFunc(text, func(string) string { n++; return redactedMark })
		t = secretAssignRE.ReplaceAllStringFunc(t, func(m string) string {
			sub := secretAssignRE.FindStringSubmatch(m)
			if strings.Contains(sub[2], redactedMark) {
				return m
			}
			n++
			return sub[1] + redactedMark
		})
		lines[i] = l[:1] + t
	}
	return strings.Join(lines, "\n"), n
}

var gitHeaderRE = regexp.MustCompile(`^diff --git a/(.*) b/(.*)$`)

// splitDiff cuts git's unified output into files.
func splitDiff(raw []byte) []panelFile {
	var files []panelFile
	var cur *panelFile
	var body []string
	var oldP, newP string
	flush := func() {
		if cur == nil {
			return
		}
		cur.Diff = strings.Join(body, "\n")
		if newP != "" {
			cur.Path = newP
		}
		if cur.Status == "renamed" {
			cur.OldPath = oldP
		}
		files = append(files, *cur)
		cur, body, oldP, newP = nil, nil, "", ""
	}
	inHunk := false
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			cur = &panelFile{Status: "modified"}
			inHunk = false
			rest := line[len("diff --git "):]
			if m := gitHeaderRE.FindStringSubmatch(line); m != nil {
				cur.Path = m[2]
				if m[1] != m[2] {
					// a/X b/Y with X != Y is a rename, or a path that holds
					// " b/"; the ---/+++ and rename lines settle it.
					cur.Path = m[2]
				}
			} else {
				cur.Path = rest
			}
			continue
		}
		if cur == nil {
			continue
		}
		if !inHunk {
			switch {
			case strings.HasPrefix(line, "@@"):
				inHunk = true
				body = append(body, line)
			case strings.HasPrefix(line, "new file mode"):
				cur.Status = "added"
			case strings.HasPrefix(line, "deleted file mode"):
				cur.Status = "deleted"
			case strings.HasPrefix(line, "rename from "):
				cur.Status, oldP = "renamed", line[len("rename from "):]
			case strings.HasPrefix(line, "rename to "):
				cur.Status, newP = "renamed", line[len("rename to "):]
			case strings.HasPrefix(line, "+++ b/"):
				newP = line[len("+++ b/"):]
			case strings.HasPrefix(line, "--- a/"):
				if oldP == "" {
					oldP = line[len("--- a/"):]
				}
			case strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch"):
				cur.Binary = true
			}
			continue
		}
		body = append(body, line)
		switch {
		case strings.HasPrefix(line, "+"):
			cur.Add++
		case strings.HasPrefix(line, "-"):
			cur.Del++
		}
	}
	flush()
	return files
}

// generatedPath reports whether a path is machine output that people rarely
// read line by line: generated code, vendored code, CRD manifests, lock files
// and the like. They spend the answer's size budget last and start collapsed.
func generatedPath(p string) bool {
	p = strings.ToLower(filepath.ToSlash(p))
	b := path.Base(p)
	switch {
	case strings.Contains(p, "zz_generated"), strings.Contains(b, ".generated."), strings.HasSuffix(b, "_generated.go"),
		strings.HasSuffix(b, ".pb.go"), strings.HasSuffix(b, ".pb.gw.go"), strings.HasSuffix(b, "_gen.go"), strings.HasSuffix(b, ".gen.go"),
		strings.HasSuffix(b, ".min.js"), strings.HasSuffix(b, ".min.css"), strings.HasSuffix(b, ".map"),
		b == "go.sum", b == "package-lock.json", b == "yarn.lock", b == "pnpm-lock.yaml", b == "cargo.lock", b == "poetry.lock",
		strings.HasSuffix(p, "docs/content/reference/aggregated-docs.md"):
		return true
	}
	for _, seg := range strings.Split(path.Dir(p), "/") {
		switch seg {
		case "vendor", "node_modules", "generated", "crds":
			return true
		}
	}
	return false
}

// shapeFile applies the secret rules and a cap to one file. total is the diff
// text already carried and budget the most the answer may carry (0: no limit).
// It returns the secrets replaced.
func shapeFile(f *panelFile, total *int, budget, fileCap int) (redacted int) {
	lines := f.Add + f.Del
	f.Generated = generatedPath(f.Path)
	switch {
	case secretFileName(f.Path) != "":
		f.Withheld, f.Lines, f.Diff = secretFileName(f.Path), lines, ""
	case f.Binary:
		f.Diff = ""
	case len(f.Diff) > fileCap:
		f.TooBig, f.Skip, f.Bytes, f.Lines, f.Diff = true, "size", len(f.Diff), strings.Count(f.Diff, "\n")+1, ""
	case budget > 0 && *total+len(f.Diff) > budget:
		f.TooBig, f.Skip, f.Bytes, f.Lines, f.Diff = true, "total", len(f.Diff), strings.Count(f.Diff, "\n")+1, ""
	default:
		var n int
		f.Diff, n = redactDiff(f.Diff)
		f.Redacted = n
		redacted += n
		*total += len(f.Diff)
	}
	return redacted
}

// shapeAll shapes every file of every section. The budget goes to the files
// people read first: hand-written before generated, then in the order shown
// (sections as listed, files as listed). The lists keep their order.
func shapeAll(secs []panelSection) (redacted, skipped int) {
	type ref struct{ s, i int }
	var hand, gen []ref
	for si := range secs {
		for i := range secs[si].Files {
			if generatedPath(secs[si].Files[i].Path) {
				gen = append(gen, ref{si, i})
			} else {
				hand = append(hand, ref{si, i})
			}
		}
	}
	total := 0
	for _, r := range append(hand, gen...) {
		f := &secs[r.s].Files[r.i]
		if f.TooBig { // untrackedFile already set it aside
			f.Generated = generatedPath(f.Path)
			continue
		}
		redacted += shapeFile(f, &total, panelTotalCap, panelFileCap)
		if f.Skip == "total" {
			skipped++
		}
	}
	return redacted, skipped
}

// untrackedFile builds an all-added entry for a file git does not track.
func untrackedFile(top, rel string) panelFile { return untrackedFileCap(top, rel, panelFileCap) }

// untrackedFileCap is untrackedFile with its own size cap.
func untrackedFileCap(top, rel string, fileCap int) panelFile {
	f := panelFile{Path: rel, Status: "untracked"}
	abs := filepath.Join(top, filepath.FromSlash(rel))
	fi, err := os.Lstat(abs)
	if err != nil {
		f.Diff = ""
		return f
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		tgt, _ := os.Readlink(abs)
		f.Add, f.Diff = 1, "@@ -0,0 +1 @@\n+symbolic link to "+tgt
		return f
	}
	if !fi.Mode().IsRegular() {
		return f
	}
	file, err := os.Open(abs)
	if err != nil {
		return f
	}
	defer file.Close()
	secret := secretFileName(rel) != ""
	r := bufio.NewReaderSize(file, 64<<10)
	head, _ := r.Peek(8000)
	if bytes.IndexByte(head, 0) >= 0 {
		f.Binary = true
		return f
	}
	var sb strings.Builder
	n := 0
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			n++
			if !secret && sb.Len() <= fileCap {
				sb.WriteString("+" + strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r") + "\n")
			}
		}
		if err != nil || n > 2_000_000 {
			break
		}
	}
	f.Add = n
	switch {
	case secret:
		f.Withheld, f.Lines = secretFileName(rel), n
	case sb.Len() > fileCap:
		f.TooBig, f.Skip, f.Bytes, f.Lines = true, "size", int(fi.Size())+n, n
	default:
		f.Diff = "@@ -0,0 +1," + strconv.Itoa(n) + " @@\n" + strings.TrimSuffix(sb.String(), "\n")
	}
	return f
}

// panelDiffOf reads dir's changes.
func panelDiffOf(ctx context.Context, dir, since string) panelDiff {
	d := panelDiff{Dir: dir}
	top, _, _, err := gitDirs(ctx, dir)
	if err != nil {
		d.NotGit = true
		d.Sig = panelSig(&d)
		return d
	}
	d.Dir = top
	base, mb := panelBase(ctx, top)
	// Nothing changed since the last answer: say so without running the diffs.
	fp, fpOK := panelFingerprint(ctx, top, base, mb)
	if fpOK {
		if c, hit := panelCacheGet(top, fp); hit {
			if since != "" && since == c.Sig {
				return panelDiff{Sig: c.Sig, Unchanged: true}
			}
			return c
		}
	} else {
		panelCacheDrop(top)
	}
	d.Branch, _ = runGit(ctx, top, "rev-parse", "--abbrev-ref", "HEAD")
	var note string
	section := func(key, title string, args ...string) {
		out, trunc, err := panelGit(ctx, top, panelGitRead, append([]string{"diff", "--no-color", "--no-ext-diff", "-M", "--src-prefix=a/", "--dst-prefix=b/"}, args...)...)
		if err != nil {
			return
		}
		files := splitDiff(out)
		if files == nil {
			files = []panelFile{} // JSON [], not null: the browser maps over it
		}
		if trunc && len(files) > 0 {
			files = files[:len(files)-1]
			note = "the diff is large: some files are not listed"
		}
		if len(files) > panelMaxFiles {
			note = "only the first " + strconv.Itoa(panelMaxFiles) + " files of a section are listed"
			files = files[:panelMaxFiles]
		}
		d.Sections = append(d.Sections, panelSection{Key: key, Title: title, Files: files})
	}
	section("unstaged", "Unstaged")
	section("staged", "Staged", "--cached")
	if out, _, err := panelGit(ctx, top, 4<<20, "ls-files", "--others", "--exclude-standard", "-z"); err == nil {
		files := []panelFile{} // JSON [], not null
		skipped := 0
		for _, rel := range strings.Split(string(out), "\x00") {
			if rel == "" || strings.HasSuffix(rel, "/") {
				continue
			}
			if len(files) >= panelUntrackedMax {
				skipped++
				continue
			}
			files = append(files, untrackedFile(top, rel))
		}
		if skipped > 0 {
			note = strconv.Itoa(skipped) + " more untracked files are not shown"
		}
		d.Sections = append(d.Sections, panelSection{Key: "untracked", Title: "Untracked", Files: files})
	}
	// Commits since the base.
	if base != "" {
		d.Base = base
		if lg, err := runGit(ctx, top, "log", "--format=%h%x09%s", "-n", "50", mb+"..HEAD"); err == nil && lg != "" {
			for _, l := range strings.Split(lg, "\n") {
				if sha, subj, ok := strings.Cut(l, "\t"); ok {
					d.Commits = append(d.Commits, panelCommit{SHA: sha, Subject: subj})
				}
			}
		}
		if len(d.Commits) > 0 {
			section("commits", "Commits since "+base, mb, "HEAD")
		}
	}
	redacted, skipped := shapeAll(d.Sections)
	d.Redacted += redacted
	if skipped > 0 && note == "" {
		note = strconv.Itoa(skipped) + " " + plural(skipped, "file", "files") + " not loaded: the change set is large (over " + strconv.Itoa(panelTotalCap>>10) + " KB). Each has a Load diff button."
	}
	d.Note = note
	seen := map[string]bool{}
	for _, s := range d.Sections {
		for _, f := range s.Files {
			d.Add += f.Add
			d.Del += f.Del
			seen[f.Path] = true
		}
	}
	d.Files = len(seen)
	d.Sig = panelSig(&d)
	if fpOK { // the fingerprint was taken before the read, so an edit during it shows at the next poll
		panelCachePut(top, fp, d)
	}
	if since != "" && since == d.Sig {
		return panelDiff{Sig: d.Sig, Unchanged: true}
	}
	return d
}

// panelBase is the branch a checkout's commits are counted from (origin's, when
// it has one) and the merge base with it; "" when there is none.
func panelBase(ctx context.Context, top string) (base, mb string) {
	b := defaultBranch(ctx, top)
	if b == "" {
		return "", ""
	}
	base = b
	if refExists(ctx, top, "origin/"+b) {
		base = "origin/" + b
	}
	mb, err := runGit(ctx, top, "merge-base", "HEAD", base)
	if err != nil || mb == "" {
		return "", ""
	}
	return base, mb
}

func panelSig(d *panelDiff) string {
	b, _ := json.Marshal(d)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// panelFileOf is the "Load diff" fetch: one file of one section, read again
// with the one-off cap (panelLoadCap) and the same secret rules and redaction
// as the whole answer. old is the file's former path when it was renamed.
// An error has the HTTP status and code to answer with.
func panelFileOf(ctx context.Context, dir, section, file, old string) (panelFile, *fileErr) {
	clean := func(p string) bool {
		p = filepath.ToSlash(p)
		return p != "" && !strings.HasPrefix(p, "/") && !strings.ContainsRune(p, 0) && !strings.HasPrefix(p, ":") &&
			path.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../")
	}
	if !clean(file) || (old != "" && !clean(old)) {
		return panelFile{}, ferr(http.StatusBadRequest, "bad_path", "not a path inside the checkout")
	}
	top, _, _, err := gitDirs(ctx, dir)
	if err != nil {
		return panelFile{}, ferr(http.StatusNotFound, "not_git", "this directory is not a git checkout")
	}
	gone := ferr(http.StatusNotFound, "no_change", "this file has no change here any more")
	lit := func(p string) string { return ":(top,literal)" + p }
	var f panelFile
	switch section {
	case "untracked":
		out, _, err := panelGit(ctx, top, 4<<20, "ls-files", "--others", "--exclude-standard", "-z", "--", lit(file))
		if err != nil || strings.TrimRight(string(out), "\x00") != file {
			return panelFile{}, gone
		}
		f = untrackedFileCap(top, file, panelLoadCap)
	case "unstaged", "staged", "commits":
		args := []string{"diff", "--no-color", "--no-ext-diff", "-M", "--src-prefix=a/", "--dst-prefix=b/"}
		switch section {
		case "staged":
			args = append(args, "--cached")
		case "commits":
			_, mb := panelBase(ctx, top)
			if mb == "" {
				return panelFile{}, gone
			}
			args = append(args, mb, "HEAD")
		}
		args = append(args, "--", lit(file))
		if old != "" && old != file {
			args = append(args, lit(old))
		}
		out, trunc, err := panelGit(ctx, top, panelLoadCap*4, args...)
		if err != nil {
			return panelFile{}, ferr(http.StatusBadGateway, "git_failed", "git could not read this file's diff")
		}
		files := splitDiff(out)
		for i := range files {
			if files[i].Path == file {
				f = files[i]
				break
			}
		}
		if f.Path == "" {
			return panelFile{}, gone
		}
		if trunc {
			f.Diff = strings.Repeat("x", panelLoadCap+1) // over the read limit: shapeFile calls it too big
		}
	default:
		return panelFile{}, ferr(http.StatusBadRequest, "bad_section", "unknown section")
	}
	total := 0
	shapeFile(&f, &total, 0, panelLoadCap)
	if f.TooBig {
		f.Skip = "load"
	}
	return f, nil
}
