package server

import (
	"html"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Page body search: GET /api/pages?q=WORDS. The bodies are read inside the hub,
// for the signed-in user alone, and what comes back is text: a snippet with
// \x02 and \x03 around the matched words (stored text holds neither), which a
// client splits on and never treats as markup. Only each page's newest version
// is searched, so the work is one file per page.

const (
	pageSearchMaxQ       = 200             // bytes of query text
	pageSearchMaxWords   = 8               // words that must all appear
	pageSearchScan       = 1 << 20         // bytes of one body that are searched
	pageSearchBudget     = 3 * time.Second // wall time for all the bodies
	pageSearchMaxResults = 50
	pageSearchRate       = 30 // searches per minute
	pageSnipRunes        = 160
)

var (
	pageScriptRE = regexp.MustCompile(`(?is)<(script|style)\b.*?</(script|style)\s*>`)
	pageTagRE    = regexp.MustCompile(`(?s)<[^>]*>`)
)

// PageMatch is where a search words appear in one page.
type PageMatch struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Count   int    `json:"count"`
	Snippet string `json:"snippet"`
}

// pageText is what a reader sees of a body: for HTML, its text without markup,
// scripts and styles.
func pageText(kind string, b []byte) string {
	s := string(b)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	if kind == "html" {
		s = pageScriptRE.ReplaceAllString(s, " ")
		s = html.UnescapeString(pageTagRE.ReplaceAllString(s, " "))
	}
	s = strings.NewReplacer("\x02", "", "\x03", "").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// pageSearchWords splits a query into the lowercase words that must all appear.
func pageSearchWords(q string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(q)) {
		w = strings.Trim(w, `"'`)
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
		if len(out) == pageSearchMaxWords {
			break
		}
	}
	return out
}

// matchPageText reports how often the first word appears and a snippet around the
// earliest match, or ok=false if a word is missing.
func matchPageText(text string, words []string) (count int, snippet string, ok bool) {
	low := strings.ToLower(text)
	// ToLower can change a string's length (some non-ASCII letters): then offsets in
	// low do not map onto text, so search the text as it is.
	if len(low) != len(text) {
		low = text
	}
	first := -1
	for i, w := range words {
		at := strings.Index(low, w)
		if at < 0 {
			return 0, "", false
		}
		if i == 0 {
			count = strings.Count(low, w)
		}
		if first < 0 || at < first {
			first = at
		}
	}
	// A window of whole runes around the first match.
	start := first
	for n := 0; n < pageSnipRunes/4 && start > 0; n++ {
		_, sz := utf8.DecodeLastRuneInString(text[:start])
		start -= sz
	}
	end := start
	for n := 0; n < pageSnipRunes && end < len(text); n++ {
		_, sz := utf8.DecodeRuneInString(text[end:])
		end += sz
	}
	win, wl := text[start:end], low[start:end]
	// Mark every word's occurrences in the window, once each, left to right.
	type span struct{ a, b int }
	var spans []span
	for _, w := range words {
		for from := 0; ; {
			i := strings.Index(wl[from:], w)
			if i < 0 {
				break
			}
			spans = append(spans, span{from + i, from + i + len(w)})
			from += i + len(w)
		}
	}
	for i := 1; i < len(spans); i++ { // insertion sort: a handful of spans
		for j := i; j > 0 && spans[j].a < spans[j-1].a; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	at := 0
	for _, sp := range spans {
		if sp.a < at {
			continue
		}
		b.WriteString(win[at:sp.a])
		b.WriteString("\x02" + win[sp.a:sp.b] + "\x03")
		at = sp.b
	}
	b.WriteString(win[at:])
	if end < len(text) {
		b.WriteString("…")
	}
	return count, b.String(), true
}

// searchRateOK counts one body search against the minute's allowance.
func (st *pageStore) searchRateOK() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	recent := st.rate["search"][:0]
	for _, at := range st.rate["search"] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	if len(recent) >= pageSearchRate {
		st.rate["search"] = recent
		return false
	}
	st.rate["search"] = append(recent, now)
	return true
}

// search reads the newest version of each of the pages and returns those whose text
// holds every word, newest first. partial is true if it ran out of time or results.
func (st *pageStore) search(pages []Page, words []string) (matches []PageMatch, partial bool) {
	matches = []PageMatch{}
	deadline := time.Now().Add(pageSearchBudget)
	for _, p := range pages {
		if time.Now().After(deadline) {
			return matches, true
		}
		var v *PageVersion
		for i := range p.History {
			if p.History[i].N == p.Version {
				v = &p.History[i]
			}
		}
		if v == nil {
			continue
		}
		f, err := os.Open(st.bodyPath(p.ID, v.N, v.Kind))
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(f, pageSearchScan))
		f.Close()
		if n, snip, ok := matchPageText(pageText(v.Kind, b), words); ok {
			if len(matches) == pageSearchMaxResults {
				return matches, true
			}
			matches = append(matches, PageMatch{ID: p.ID, Version: v.N, Count: n, Snippet: snip})
		}
	}
	return matches, false
}

// pageSearchError answers a search the caller may not make or made badly.
func pageSearchBad(w http.ResponseWriter, code, msg string, status int) {
	writeJSONStatus(w, status, map[string]string{"code": code, "message": msg})
}
