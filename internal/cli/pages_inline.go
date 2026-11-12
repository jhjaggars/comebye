package cli

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Inlining local images into an HTML page at publish. A page is one file in
// a sandbox that cannot fetch anything, so an image next to it is only
// useful as a data: URI. The CLI does that for what the page references by a
// relative path (img src, srcset, source src, CSS url()), and only for
// images: png, jpeg, webp, gif and svg. It reads under the page's own
// directory and nowhere else.

var inlineMIME = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".webp": "image/webp", ".gif": "image/gif", ".svg": "image/svg+xml",
}

var (
	reTagSrc    = regexp.MustCompile(`(?is)(<(?:img|source)\b[^>]*?\s(?:src)\s*=\s*)("[^"]*"|'[^']*')`)
	reTagSrcset = regexp.MustCompile(`(?is)(<(?:img|source)\b[^>]*?\ssrcset\s*=\s*)("[^"]*"|'[^']*')`)
	reCSSURL    = regexp.MustCompile(`(?i)(url\(\s*)("[^"]*"|'[^']*'|[^)\s"']+)(\s*\))`)
	reSrcsetEnt = regexp.MustCompile(`([^\s,]+)(\s+[^,]*)?`)
	reScheme    = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

type inliner struct {
	root    string // the page's directory, symlinks resolved
	cache   map[string]string
	done    []string // what was inlined, in order, once each
	notes   []string // references left alone, and why
	failure error
}

// inlineImages returns html with its local image references replaced by
// data: URIs. dir is the page's directory. It fails on a reference that
// leaves dir (".." or a symlink out); a missing or non-image file is left as
// it was and reported in notes.
func inlineImages(html, dir string) (out string, inlined, notes []string, err error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", nil, nil, err
	}
	in := &inliner{root: root, cache: map[string]string{}}
	rep := func(re *regexp.Regexp, f func(ref string) string) {
		html = re.ReplaceAllStringFunc(html, func(m string) string {
			g := re.FindStringSubmatch(m)
			q, end := g[2], ""
			if len(g) > 3 {
				end = g[3]
			}
			if len(q) >= 2 && (q[0] == '"' || q[0] == '\'') {
				return g[1] + string(q[0]) + f(q[1:len(q)-1]) + string(q[0]) + end
			}
			return g[1] + f(q) + end
		})
	}
	rep(reTagSrc, in.uri)
	rep(reCSSURL, in.uri)
	html = reTagSrcset.ReplaceAllStringFunc(html, func(m string) string {
		g := reTagSrcset.FindStringSubmatch(m)
		q := g[2]
		body := reSrcsetEnt.ReplaceAllStringFunc(q[1:len(q)-1], func(e string) string {
			p := reSrcsetEnt.FindStringSubmatch(e)
			return in.uri(p[1]) + p[2]
		})
		return g[1] + string(q[0]) + body + string(q[0])
	})
	if in.failure != nil {
		return "", nil, nil, in.failure
	}
	return html, in.done, in.notes, nil
}

// uri returns ref's data: URI when it is a local image, else ref unchanged.
func (in *inliner) uri(ref string) string {
	if in.failure != nil {
		return ref
	}
	r := strings.TrimSpace(ref)
	if r == "" || strings.HasPrefix(r, "#") || strings.HasPrefix(r, "//") || reScheme.MatchString(r) {
		return ref
	}
	p := r
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}
	if filepath.IsAbs(p) {
		in.note(fmt.Sprintf("%s: an absolute path is not inlined", r))
		return ref
	}
	if got, ok := in.cache[p]; ok {
		return got
	}
	mime, ok := inlineMIME[strings.ToLower(filepath.Ext(p))]
	full := filepath.Join(in.root, filepath.FromSlash(p))
	if rel, err := filepath.Rel(in.root, full); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		in.failure = fmt.Errorf("%s points outside the page's directory; images are read only from beside the page", r)
		return ref
	}
	if !ok {
		in.note(fmt.Sprintf("%s: not an image (png, jpeg, webp, gif, svg); left as it is", r))
		return ref
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		in.note(fmt.Sprintf("%s: %s; left as it is", r, humanErr(err)))
		return ref
	}
	if rel, err := filepath.Rel(in.root, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		in.failure = fmt.Errorf("%s is a link to a file outside the page's directory; images are read only from beside the page", r)
		return ref
	}
	b, err := os.ReadFile(real)
	if err != nil {
		in.note(fmt.Sprintf("%s: %s; left as it is", r, humanErr(err)))
		return ref
	}
	if mime == "image/svg+xml" {
		if !strings.Contains(strings.ToLower(string(b)), "<svg") {
			in.note(fmt.Sprintf("%s: not an SVG; left as it is", r))
			return ref
		}
	} else if !strings.HasPrefix(http.DetectContentType(b), "image/") {
		in.note(fmt.Sprintf("%s: not an image; left as it is", r))
		return ref
	}
	uri := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
	in.cache[p] = uri
	in.done = append(in.done, fmt.Sprintf("%s (%s)", p, humanSize(int64(len(b)))))
	return uri
}

func (in *inliner) note(s string) { in.notes = append(in.notes, s) }

func humanErr(err error) string {
	if os.IsNotExist(err) {
		return "no such file"
	}
	return err.Error()
}
