package server

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// The removal safety ask is answered in a small line format so the hub can
// re-check what the session names (see askcheck.go):
//
//	SAFE
//
//	UNSAFE
//	- pr jhjaggars/homelab#343 open, I am watching it
//	- todo t972236dfee waiting on the user's answer
//	- commits feat/x pushed to origin
//	- resource k3d-cluster dev on desk
//	- resource pid 4242 on desk
//	- other a loop I run every 10 minutes
//
// The parser is lenient: bullets or numbers, markdown around the words, a
// reason on the UNSAFE line itself, a PR as a URL, a to-do id anywhere in a
// line. Whatever it cannot type becomes an "other" item.

const (
	maxAskItems    = 20
	maxItemText    = 300
	maxReasonShown = 500
)

// askFormat is the part of the prompt that fixes the reply format.
const askFormat = "Reply in exactly this format. First line: SAFE, or UNSAFE. After UNSAFE, one line per thing that must be dealt with before this session goes, each starting with \"- \" and a type:\n" +
	"- pr OWNER/REPO#NUMBER <your note>\n" +
	"- todo TODO_ID <your note>\n" +
	"- commits BRANCH [pushed to REMOTE] <your note>\n" +
	"- resource KIND NAME_OR_ID [on HOST] <your note>   (KIND: pid, k3d-cluster, container, ...)\n" +
	"- other <anything else: a loop you run, a request or follow-up you owe the user>\n" +
	"No other text."

// parseReply reads a session's reply to the ask: the state (SAFE, UNSAFE,
// or unclear with the reply as its reason), the one-line reason, and the
// typed items of an UNSAFE answer.
func parseReply(text string) (state, reason string, items []ledger.AskItem) {
	state, reason = parseAnswer(text)
	if state != ledger.AskUnsafe {
		return state, reason, nil
	}
	first, body := splitFirst(text)
	// The reason is what the first line says after UNSAFE; a list below it
	// is the items, summed up when the line says nothing.
	_, reason = parseAnswer(first)
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "```") {
			continue
		}
		cont := (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && !hasBullet(trim) && len(items) > 0
		if cont {
			last := &items[len(items)-1]
			last.Text = clip(last.Text+" "+trim, maxItemText)
			continue
		}
		if len(items) >= maxAskItems {
			break
		}
		if it, ok := parseItem(trim); ok {
			items = append(items, it)
		}
	}
	if len(items) == 0 && reason != "" {
		items = []ledger.AskItem{{Type: ledger.ItemOther, Text: reason}}
	}
	if reason == "" && len(items) > 0 {
		var parts []string
		for _, it := range items {
			parts = append(parts, strings.TrimSpace(itemLabel(it)+" "+it.Text))
		}
		reason = clip(strings.Join(parts, "; "), maxAskReason)
	}
	return state, reason, items
}

// splitFirst cuts text after its first non-empty line.
func splitFirst(text string) (first, rest string) {
	t := strings.TrimSpace(text)
	first, rest, _ = strings.Cut(t, "\n")
	return first, rest
}

var bulletRe = regexp.MustCompile(`^(?:[-*•–+]|\d{1,2}[.)])\s+`)

func hasBullet(s string) bool { return bulletRe.MatchString(s) }

var (
	prRefRe  = regexp.MustCompile(`([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(?:#|/pull/)(\d+)`)
	prNumRe  = regexp.MustCompile(`^#?(\d+)$`)
	todoIDRe = regexp.MustCompile(`\bt[0-9a-f]{10}\b`)
)

// parseItem turns one reply line into an item; ok is false for a line with
// nothing in it.
func parseItem(line string) (ledger.AskItem, bool) {
	line = bulletRe.ReplaceAllString(line, "")
	line = strings.TrimSpace(strings.Trim(line, "*_ "))
	if line == "" {
		return ledger.AskItem{}, false
	}
	word, rest, _ := strings.Cut(line, " ")
	kind := strings.ToLower(strings.TrimFunc(word, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }))
	rest = strings.TrimSpace(strings.Trim(rest, "*_ "))
	other := func() (ledger.AskItem, bool) {
		return ledger.AskItem{Type: ledger.ItemOther, Text: clip(line, maxItemText)}, true
	}
	switch kind {
	case "pr", "pullrequest":
		if it, ok := parsePR(rest); ok {
			return it, true
		}
		return other()
	case "todo", "to-do":
		if id := todoIDRe.FindString(rest); id != "" {
			return ledger.AskItem{Type: ledger.ItemTodo, ID: id, Text: noteAfter(rest, id)}, true
		}
		return other()
	case "commit", "commits":
		return parseCommits(rest), true
	case "resource":
		if it, ok := parseResource(rest); ok {
			return it, true
		}
		return other()
	case "other":
		if rest == "" {
			return ledger.AskItem{}, false
		}
		return ledger.AskItem{Type: ledger.ItemOther, Text: clip(rest, maxItemText)}, true
	}
	// An untyped line: a PR reference or a to-do id still types it.
	if m := prRefRe.FindStringSubmatch(line); m != nil {
		n, _ := strconv.Atoi(m[2])
		return ledger.AskItem{Type: ledger.ItemPR, Repo: m[1], Number: n, Text: clip(line, maxItemText)}, true
	}
	if id := todoIDRe.FindString(line); id != "" {
		return ledger.AskItem{Type: ledger.ItemTodo, ID: id, Text: clip(line, maxItemText)}, true
	}
	return other()
}

// noteAfter is the session's note: rest without the reference it started with.
func noteAfter(rest, ref string) string {
	i := strings.Index(rest, ref)
	if i < 0 {
		return clip(rest, maxItemText)
	}
	n := strings.TrimLeftFunc(rest[i+len(ref):], func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(":-–—,.)", r) })
	return clip(n, maxItemText)
}

func parsePR(rest string) (ledger.AskItem, bool) {
	rest = strings.ReplaceAll(rest, "`", "")
	if m := prRefRe.FindStringSubmatch(rest); m != nil {
		n, _ := strconv.Atoi(m[2])
		return ledger.AskItem{Type: ledger.ItemPR, Repo: m[1], Number: n, Text: noteAfter(rest, m[0])}, true
	}
	tok, _, _ := strings.Cut(rest, " ")
	if m := prNumRe.FindStringSubmatch(strings.Trim(tok, ",.:;")); m != nil {
		n, _ := strconv.Atoi(m[1])
		return ledger.AskItem{Type: ledger.ItemPR, Number: n, Text: noteAfter(rest, tok)}, true
	}
	return ledger.AskItem{}, false
}

func parseCommits(rest string) ledger.AskItem {
	rest = strings.ReplaceAll(rest, "`", "")
	it := ledger.AskItem{Type: ledger.ItemCommits}
	toks := strings.Fields(rest)
	if len(toks) == 0 {
		return it
	}
	it.Branch = strings.Trim(toks[0], ",.:;()")
	i := 1
	for ; i < len(toks); i++ {
		w := strings.ToLower(strings.Trim(toks[i], ",.:;()"))
		if (w == "on" || w == "to" || w == "at") && i+1 < len(toks) {
			it.Remote = strings.Trim(toks[i+1], ",.:;()")
			i += 2
			break
		}
		if w == "pushed" || w == "unpushed" {
			continue
		}
		break
	}
	if i > len(toks) {
		i = len(toks)
	}
	it.Text = clip(strings.Join(toks[i:], " "), maxItemText)
	if it.Remote == "" {
		// "pushed to origin" later in the note, or the words alone.
		if m := regexp.MustCompile(`(?i)\bpushed to (\S+)`).FindStringSubmatch(rest); m != nil {
			it.Remote = strings.Trim(m[1], ",.:;()")
		}
	}
	it.Text = strings.TrimLeftFunc(it.Text, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("-–—:,", r) })
	return it
}

// resourceKinds normalizes the kind a session names.
var resourceKinds = map[string]string{
	"pid": "pid", "process": "pid",
	"k3d": "k3d-cluster", "k3d-cluster": "k3d-cluster", "cluster": "k3d-cluster", "k3dcluster": "k3d-cluster",
}

func parseResource(rest string) (ledger.AskItem, bool) {
	rest = strings.ReplaceAll(rest, "`", "")
	toks := strings.Fields(rest)
	if len(toks) < 2 {
		return ledger.AskItem{}, false
	}
	kind := strings.ToLower(strings.Trim(toks[0], ",.:;()"))
	i := 1
	if (kind == "k3d" || kind == "kind") && strings.EqualFold(toks[1], "cluster") && len(toks) > 2 {
		kind, i = kind+"-cluster", 2
	}
	if k, ok := resourceKinds[kind]; ok {
		kind = k
	}
	it := ledger.AskItem{Type: ledger.ItemResource, Kind: kind, Name: strings.Trim(toks[i], ",.:;()")}
	tail := toks[i+1:]
	if len(tail) >= 2 && strings.EqualFold(tail[0], "on") {
		it.Host = strings.Trim(tail[1], ",.:;()")
		tail = tail[2:]
	}
	it.Text = strings.TrimLeftFunc(clip(strings.Join(tail, " "), maxItemText), func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("-–—:,", r) })
	return it, it.Name != ""
}

// label names an item in one phrase ("PR o/r#343").
func itemLabel(it ledger.AskItem) string {
	switch it.Type {
	case ledger.ItemPR:
		if it.Repo == "" {
			return fmt.Sprintf("PR #%d", it.Number)
		}
		return fmt.Sprintf("PR %s#%d", it.Repo, it.Number)
	case ledger.ItemTodo:
		return "to-do " + it.ID
	case ledger.ItemCommits:
		s := "commits on " + orDefault(it.Branch, "its branch")
		if it.Remote != "" {
			s += " (pushed to " + it.Remote + ")"
		}
		return s
	case ledger.ItemResource:
		s := it.Kind + " " + it.Name
		if it.Host != "" {
			s += " on " + it.Host
		}
		return s
	}
	return "something else"
}
