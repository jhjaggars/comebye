package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
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
	// then the size of the change. TooBig: over the per-file or total cap.
	Withheld string `json:"withheld,omitempty"`
	TooBig   bool   `json:"too_big,omitempty"`
	Lines    int    `json:"lines,omitempty"`
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

// shapeFiles applies the secret rules and the size caps to files; *total is
// the diff text already carried. It returns the count of secrets replaced.
func shapeFiles(files []panelFile, total *int, note *string) (redacted int) {
	if len(files) > panelMaxFiles {
		*note = "only the first " + strconv.Itoa(panelMaxFiles) + " files of a section are listed"
		files = files[:panelMaxFiles]
	}
	for i := range files {
		f := &files[i]
		lines := f.Add + f.Del
		switch {
		case secretFileName(f.Path) != "":
			f.Withheld, f.Lines, f.Diff = secretFileName(f.Path), lines, ""
		case f.Binary:
			f.Diff = ""
		case len(f.Diff) > panelFileCap:
			f.TooBig, f.Lines, f.Diff = true, strings.Count(f.Diff, "\n")+1, ""
		case *total+len(f.Diff) > panelTotalCap:
			f.TooBig, f.Lines, f.Diff = true, strings.Count(f.Diff, "\n")+1, ""
			*note = "the diff is large: some files are listed without their text"
		default:
			var n int
			f.Diff, n = redactDiff(f.Diff)
			f.Redacted = n
			redacted += n
			*total += len(f.Diff)
		}
	}
	return redacted
}

// untrackedFile builds an all-added entry for a file git does not track.
func untrackedFile(top, rel string) panelFile {
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
			if !secret && sb.Len() <= panelFileCap {
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
	case sb.Len() > panelFileCap:
		f.TooBig, f.Lines = true, n
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
	d.Branch, _ = runGit(ctx, top, "rev-parse", "--abbrev-ref", "HEAD")
	total := 0
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
		d.Redacted += shapeFiles(files, &total, &note)
		if len(files) > panelMaxFiles {
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
		// Untracked files go through the same caps and redaction as the rest.
		for i := range files {
			f := &files[i]
			if f.Diff == "" {
				continue
			}
			if total+len(f.Diff) > panelTotalCap {
				f.TooBig, f.Lines, f.Diff = true, strings.Count(f.Diff, "\n")+1, ""
				note = "the diff is large: some files are listed without their text"
				continue
			}
			var n int
			f.Diff, n = redactDiff(f.Diff)
			f.Redacted, d.Redacted = n, d.Redacted+n
			total += len(f.Diff)
		}
		d.Sections = append(d.Sections, panelSection{Key: "untracked", Title: "Untracked", Files: files})
	}
	// Commits since the base.
	if b := defaultBranch(ctx, top); b != "" {
		base := b
		if refExists(ctx, top, "origin/"+b) {
			base = "origin/" + b
		}
		if mb, err := runGit(ctx, top, "merge-base", "HEAD", base); err == nil && mb != "" {
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
	if since != "" && since == d.Sig {
		return panelDiff{Sig: d.Sig, Unchanged: true}
	}
	return d
}

func panelSig(d *panelDiff) string {
	b, _ := json.Marshal(d)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}
