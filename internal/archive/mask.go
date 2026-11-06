package archive

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Secret-looking text is masked before it reaches the index, so that snippets,
// session pages and any copy of the index file never carry it. The archive
// itself stays byte-exact: the mask is a safety net for readers of the index,
// and the patterns will miss things, so the index is still treated as holding
// plaintext secrets (see docs/archive.md).

// MaskMark replaces a masked value.
const MaskMark = "••••••"

// tokenShapes are whole values recognisable by their own shape.
var tokenShapes = regexp.MustCompile(`(?s)` + strings.Join([]string{
	`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|\z)`,
	// The end of a key whose beginning was left outside the window (see maskWindow).
	`(?:[A-Za-z0-9+/=]{16,}\s+)+-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`,
	`\btskey-[A-Za-z0-9-]{10,}`,
	`\bgh[pousr]_[A-Za-z0-9]{20,}`,
	`\bgithub_pat_[A-Za-z0-9_]{20,}`,
	`\bglpat-[A-Za-z0-9_-]{16,}`,
	`\bsk-ant-[A-Za-z0-9_-]{10,}`,
	`\bsk-[A-Za-z0-9_-]{32,}`,
	`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`,
	`\bAGE-SECRET-KEY-1[0-9A-Z]{10,}`,
	`\bxox[abprs]-[A-Za-z0-9-]{10,}`,
	`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
}, "|"))

// keyedValues are values recognisable by what labels them; the label stays.
var keyedValues = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(\bbearer\s+)([A-Za-z0-9._~+/-]{12,}=*)`),
	regexp.MustCompile(`(?i)(\b(?:password|passwd|pwd|secret_?key|secret_?access_?key|api_?key|access_?key|auth_?token|access_?token|client_?secret)["']?\s*[=:]\s*["']?)([^\s"'<>{}$&;,]{8,})`),
	regexp.MustCompile(`(\b[a-z][a-z0-9+.-]*://[^\s/:@]{1,64}:)([^\s/@]{4,})(@)`),
}

// Mask replaces what looks like a secret and says whether it did.
func Mask(s string) (string, bool) {
	hit := false
	out := tokenShapes.ReplaceAllStringFunc(s, func(string) string {
		hit = true
		return MaskMark
	})
	for _, re := range keyedValues {
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			p := re.FindStringSubmatch(m)
			if !looksLikeValue(p[2]) {
				return m
			}
			hit = true
			r := p[1] + MaskMark
			if len(p) > 3 {
				r += p[3]
			}
			return r
		})
	}
	return out, hit
}

// looksLikeValue tells a value from an ordinary word after a label ("Bearer
// authentication", "secret_key = os.environ"): a value holds a digit, an
// upper-case letter or a symbol, and is not a placeholder.
func looksLikeValue(v string) bool {
	if v == MaskMark || strings.Contains(v, "••") {
		return false
	}
	switch v {
	case "PASSWORD", "YOUR_PASSWORD", "YOUR_API_KEY", "REDACTED", "CHANGEME":
		return false
	}
	return strings.ContainsAny(v, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ!@#%^*+=/")
}

// maskMargin is how much text beyond the kept part is masked with it. A value
// that crosses the point where the text is clipped is then masked whole, not
// left half-visible. It is larger than any PEM key the patterns name.
const maskMargin = 8192

// maskHead masks the start of s and keeps at most n bytes of it. Very long
// text is only masked as far as it is kept, so a tool's huge output costs
// nothing beyond the window.
func maskHead(s string, n int) (string, bool) {
	w := s
	if len(w) > n+maskMargin {
		w = runeHead(w, n+maskMargin)
	}
	m, hit := Mask(w)
	if len(m) <= n && len(w) < len(s) {
		return m + "…", hit // masking shrank it, but there was more
	}
	return clip(m, n), hit
}

// maskEnds masks s and keeps at most n bytes from each end, with a marker where
// the middle was left out. Errors tend to come last, so a long tool output's
// tail is as worth finding as its head. clipped reports whether anything went.
func maskEnds(s string, n int) (out string, hit, clipped bool) {
	head, tail := s, s
	if len(s) > 2*(n+maskMargin) {
		var h1, h2 bool
		head, h1 = Mask(runeHead(s, n+maskMargin))
		tail, h2 = Mask(runeTail(s, n+maskMargin))
		hit = h1 || h2
	} else {
		// Short enough to mask whole, so no value is seen in two halves.
		if out, hit = Mask(s); len(out) <= 2*n {
			return out, hit, false
		}
		head, tail = out, out
	}
	head, tail = runeHead(head, n), runeTail(tail, n)
	return head + "\n… [" + itoa(len(s)-2*n) + " bytes left out] …\n" + tail, hit, true
}

func itoa(n int) string { return qn(int64(n)) }

func runeHead(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func runeTail(s string, n int) string {
	if n >= len(s) {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// maskPlain masks a short field and drops the flag.
func maskPlain(s string) string {
	m, _ := Mask(s)
	return m
}

// maskLinks returns a copy of the handoff links with their free text masked:
// briefs, reports and discard reasons are written by agents and can quote
// anything. It records which briefs held something.
func maskLinks(links []Link) []Link {
	out := make([]Link, len(links))
	copy(out, links)
	for i := range out {
		l := &out[i]
		l.Brief, l.briefMasked = Mask(l.Brief)
		l.FinalReport = maskPlain(l.FinalReport)
		if l.Mark != nil {
			mk := *l.Mark
			mk.Reason = maskPlain(mk.Reason)
			l.Mark = &mk
		}
	}
	return out
}
