package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// A page's skeleton is its preview in the Pages gallery: what a reader needs
// to tell pages apart, as text. The hub reads it out of the page's own source
// once per version, with no rendering and no browser, and keeps it next to
// the body (v<N>.preview.json, well under a kilobyte). The app draws it with
// its own theme tokens. The page is untrusted text: nothing here runs it, and
// only plain text and a fixed list of flags leave this file.
const (
	skelLeadMax  = 220 // runes
	skelHeadMax  = 60
	skelHeads    = 4
	skelLeadMin  = 40 // an HTML paragraph shorter than this is a label, not a lead
	skelVersion  = 1
	skelMaxBytes = 8 << 10
)

// pageSkeleton is the preview of one version.
type pageSkeleton struct {
	V     int      `json:"v"`
	Lead  string   `json:"lead,omitempty"`  // the first real paragraph
	Heads []string `json:"heads,omitempty"` // the first few section headings
	Flags []string `json:"flags,omitempty"` // what else the page holds: table, code, image, diagram, chart
}

// skelFlagOrder is the order flags are listed in.
var skelFlagOrder = []string{"diagram", "chart", "image", "table", "code"}

type skelFlags map[string]bool

func (f skelFlags) list() []string {
	var out []string
	for _, k := range skelFlagOrder {
		if f[k] {
			out = append(out, k)
		}
	}
	return out
}

// plain is s as one line of printable text, cut to max runes.
func plain(s string, max int) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range s {
		if r == utf8.RuneError || (unicode.IsControl(r) && !unicode.IsSpace(r)) || r == 0x200b || r == 0xfeff {
			continue
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
			n++
		}
		if n >= max {
			return strings.TrimSpace(b.String()) + "…"
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// makeSkeleton reads the preview out of a page body of the given kind.
func makeSkeleton(kind string, body []byte) pageSkeleton {
	sk := pageSkeleton{V: skelVersion}
	if kind == "md" {
		skeletonMarkdown(&sk, string(body))
	} else {
		skeletonHTML(&sk, body)
	}
	return sk
}

// skeletonHTML walks the token stream (the tokenizer is bounded by the body
// and builds no tree, so broken markup costs nothing extra). Text counts only
// outside script, style, svg and the like.
func skeletonHTML(sk *pageSkeleton, body []byte) {
	z := html.NewTokenizer(bytes.NewReader(body))
	flags := skelFlags{}
	skip := map[string]int{} // open script/style/template/noscript/svg/head/title elements
	skipping := func() bool {
		for _, n := range skip {
			if n > 0 {
				return true
			}
		}
		return false
	}
	var (
		cur    string // the element being read: p, h2, h3 or "" (also "anytext" for a loose first block)
		text   strings.Builder
		inBody = true          // text is read except inside <head> and the other skipped elements
		loose  strings.Builder // text outside any p/heading, a fallback for the lead
	)
	finish := func() {
		t := plain(text.String(), 400)
		text.Reset()
		k := cur
		cur = ""
		switch {
		case k == "p" && sk.Lead == "" && utf8.RuneCountInString(t) >= skelLeadMin && inBody:
			sk.Lead = plain(t, skelLeadMax)
		case (k == "h2" || k == "h3") && t != "" && len(sk.Heads) < skelHeads && inBody:
			sk.Heads = append(sk.Heads, plain(t, skelHeadMax))
		}
	}
	for steps := 0; steps < 400000; steps++ {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			cls := ""
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				if string(k) == "class" {
					cls = strings.ToLower(string(v))
				}
			}
			switch tag {
			case "body":
				skip["head"], skip["title"] = 0, 0 // a head that was never closed
			case "script", "style", "template", "noscript", "head", "title", "svg", "math", "iframe":
				if tt == html.StartTagToken {
					skip[tag]++
				}
				if tag == "svg" {
					flags["diagram"] = true
				}
			case "img", "picture":
				flags["image"] = true
			case "canvas":
				flags["chart"] = true
			case "table":
				flags["table"] = true
			case "pre":
				flags["code"] = true
			}
			if strings.Contains(cls, "mermaid") {
				flags["diagram"] = true
			}
			if skipping() {
				continue
			}
			switch tag {
			case "p", "h2", "h3":
				if cur != "" {
					finish()
				}
				cur = tag
				text.Reset()
			case "h1", "h4", "h5", "h6", "div", "li", "td", "section", "article", "ul", "ol", "tr", "blockquote":
				if cur != "" {
					finish()
				}
			case "br":
				text.WriteByte(' ')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch tag {
			case "script", "style", "template", "noscript", "head", "title", "svg", "math", "iframe":
				if skip[tag] > 0 {
					skip[tag]--
				}
				continue
			}
			if !skipping() && cur != "" && (tag == cur || tag == "div" || tag == "li" || tag == "section") {
				finish()
			}
		case html.TextToken:
			if skipping() {
				continue
			}
			if cur != "" {
				if text.Len() < 4096 {
					text.Write(z.Text())
				}
			} else if inBody && loose.Len() < 512 {
				loose.Write(z.Text())
				loose.WriteByte(' ')
			}
		}
		if sk.Lead != "" && len(sk.Heads) >= skelHeads && len(flags) >= len(skelFlagOrder) {
			break
		}
	}
	if cur != "" {
		finish()
	}
	if sk.Lead == "" {
		if t := plain(loose.String(), skelLeadMax); utf8.RuneCountInString(t) >= skelLeadMin {
			sk.Lead = t
		}
	}
	sk.Flags = flags.list()
}

var (
	skelLinkR = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	skelMarkR = regexp.MustCompile("[*_`~]+")
	skelNumR  = regexp.MustCompile(`^\s*(\d+[.)]|[-*+])\s+`)
)

func skeletonMarkdown(sk *pageSkeleton, src string) {
	flags := skelFlags{}
	fence := ""
	var para []string
	flush := func() {
		if len(para) > 0 && sk.Lead == "" {
			t := plain(skelMarkR.ReplaceAllString(skelLinkR.ReplaceAllString(strings.Join(para, " "), "$1"), ""), skelLeadMax)
			if utf8.RuneCountInString(t) >= 20 {
				sk.Lead = t
			}
		}
		para = nil
	}
	seenH1 := false
	for _, raw := range strings.Split(src, "\n") {
		ln := strings.TrimSpace(raw)
		if fence != "" {
			if strings.HasPrefix(ln, fence) {
				fence = ""
			}
			continue
		}
		switch {
		case strings.HasPrefix(ln, "```") || strings.HasPrefix(ln, "~~~"):
			flush()
			fence = ln[:3]
			if strings.Contains(strings.ToLower(ln), "mermaid") {
				flags["diagram"] = true
			} else {
				flags["code"] = true
			}
		case strings.HasPrefix(ln, "#"):
			flush()
			level := len(ln) - len(strings.TrimLeft(ln, "#"))
			t := plain(skelMarkR.ReplaceAllString(strings.TrimSpace(ln[level:]), ""), skelHeadMax)
			switch {
			case level == 1 && !seenH1:
				seenH1 = true
			case (level == 2 || level == 3) && t != "" && len(sk.Heads) < skelHeads:
				sk.Heads = append(sk.Heads, t)
			}
		case strings.HasPrefix(ln, "|"):
			flush()
			flags["table"] = true
		case strings.Contains(ln, "![") && strings.Contains(ln, "]("):
			flush()
			flags["image"] = true
		case ln == "":
			flush()
		case strings.HasPrefix(ln, ">") || skelNumR.MatchString(ln) || strings.HasPrefix(ln, "---") || strings.HasPrefix(ln, "<"):
			flush()
		default:
			para = append(para, ln)
		}
	}
	flush()
	sk.Flags = flags.list()
}

// ---- store: one skeleton per version, kept beside its body ----

func (st *pageStore) previewPath(id string, n int) string {
	return filepath.Join(st.dir, id, "v"+strconv.Itoa(n)+".preview.json")
}

// preview is the skeleton of a version (0 is the newest). It is read from the
// file kept at publish; a version that has none (published before previews
// existed) is made now and kept.
func (st *pageStore) preview(id string, n int) (pageSkeleton, int, error) {
	if n == 0 {
		p, ok := st.get(id)
		if !ok {
			return pageSkeleton{}, 0, errPageNotFound
		}
		n = p.Version
	}
	if st.dir != "" {
		if b, err := os.ReadFile(st.previewPath(id, n)); err == nil {
			var sk pageSkeleton
			if json.Unmarshal(b, &sk) == nil && sk.V == skelVersion {
				return sk, n, nil
			}
		}
	}
	_, v, body, err := st.body(id, n)
	if err != nil {
		return pageSkeleton{}, n, err
	}
	sk := makeSkeleton(v.Kind, body)
	st.keepPreview(id, n, sk)
	return sk, n, nil
}

func (st *pageStore) keepPreview(id string, n int, sk pageSkeleton) {
	if st.dir == "" {
		return
	}
	b, err := json.Marshal(sk)
	if err != nil || len(b) > skelMaxBytes {
		return
	}
	_ = writeBodyFile(st.previewPath(id, n), b)
}

// handlePagePreview: GET /api/pages/{id}/preview?v=N. The skeleton holds the
// page's own words, so it is read under the same rule as its body: the user,
// a coordinator, or an agent of the page's own host. A given version never
// changes, so the browser may keep it for good.
func (s *Server) handlePagePreview(w http.ResponseWriter, r *http.Request) {
	if s.pagesOff(w, r) {
		return
	}
	who := s.pageCaller(r)
	p, ok := s.pages.get(r.PathValue("id"))
	if !ok || !who.sees(p) {
		pageErr(w, errPageNotFound)
		return
	}
	if !who.user && !who.coord && !sameHost(p.Host, who.host) {
		readForbidden(w)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("v"))
	sk, n, err := s.pages.preview(p.ID, n)
	if err != nil {
		if errors.Is(err, errPageNoBody) {
			writeJSONStatus(w, http.StatusGone, map[string]string{"code": "body_gone", "message": "that version's file is missing on the hub"})
			return
		}
		pageErr(w, err)
		return
	}
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		// The browser replays this for a year: the build that served it is not the build serving later.
		w.Header().Del("X-App-Version")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	writeJSON(w, map[string]any{"id": p.ID, "version": n, "preview": sk})
}
