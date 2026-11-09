package server

import (
	"regexp"
	"strings"
)

// What the page shows of a deploy command's output.
//
// The output can come from any operator command, so it is untrusted text, and
// the deploy command must not print secrets: this is a safety net, not a
// licence. It strips terminal escapes, blanks the obvious token shapes, caps
// the size, and picks one line to lead with (Headline).

// deployOutMax bounds the output kept in the record and sent to the page.
const deployOutMax = 4000

var (
	ansiRE = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[@-Z\\\\-_]")
	// private key blocks, whole or cut off by the tail cap
	pemRE = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)
	// well-known token shapes
	tokenRE = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{16,}|xox[abprs]-[A-Za-z0-9-]{10,}|sk-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})\b`)
	// Authorization: Bearer x / Basic x
	bearerRE = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// user:password@ in a URL
	urlCredRE = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^\s/:@]*):[^\s/@]+@`)
	// token=..., "password": "...", SECRET: ...
	kvRE = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:token|secret|passw(?:or)?d|passwd|api[_-]?key|apikey|private[_-]?key|credential)[A-Za-z0-9_.-]*["']?\s*[:=]\s*)("[^"\n]*"|'[^'\n]*'|[^\s"',;]+)`)
)

const redacted = "[redacted]"

// cleanDeployOutput makes a command's output safe to keep and show: valid
// text without control characters or ANSI escapes, secrets blanked, and only
// its last deployOutMax bytes (whole lines when it can).
func cleanDeployOutput(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// a progress bar redraws a line with \r: the last redraw is what was on screen
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if j := strings.LastIndexByte(l, '\r'); j >= 0 {
			lines[i] = l[j+1:]
		}
	}
	s = strings.Join(lines, "\n")
	s = ansiRE.ReplaceAllString(s, "")
	s = pemRE.ReplaceAllString(s, redacted)
	s = tokenRE.ReplaceAllString(s, redacted)
	s = bearerRE.ReplaceAllString(s, "$1 "+redacted)
	s = urlCredRE.ReplaceAllString(s, "$1:"+redacted+"@")
	s = kvRE.ReplaceAllString(s, "$1"+redacted)
	// block cuts from the front of the string; keep the tail instead
	if len(s) > deployOutMax {
		s = s[len(s)-deployOutMax:]
		if i := strings.IndexByte(s, '\n'); i >= 0 && i < 200 {
			s = s[i+1:] // drop the line the cut went through
		}
	}
	return block(s, deployOutMax)
}

// deployHeadline is the one line that says how a run went.
func deployHeadline(rec DeployRecord) string {
	var lines []string
	for _, l := range strings.Split(rec.Output, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	find := func(sub string) string {
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.Contains(lines[i], sub) {
				return lines[i]
			}
		}
		return ""
	}
	last := ""
	if len(lines) > 0 {
		last = lines[len(lines)-1]
	}
	var h string
	switch rec.State {
	case depFailed:
		if h = find("FAILED"); h == "" {
			h = rec.Error
		}
	case depOK:
		if h = find("bump: ok"); h == "" {
			h = last
		}
	case depRunning:
		h = last
	}
	return oneLine(h, 300)
}

// cleanDeployRecord bounds and cleans every text a record carries (a peer's
// are not trusted either) and adds the headline. Idempotent.
func cleanDeployRecord(rec *DeployRecord) {
	rec.Output = cleanDeployOutput(rec.Output)
	rec.Error = oneLine(rec.Error, 300)
	rec.Headline = deployHeadline(*rec)
}
