package server

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// The removal check asks "what is left to finish this work?". It is answered
// in a small line format so the hub can re-check what the session names (see
// askcheck.go):
//
//	NOTHING LEFT
//
//	LEFT
//	- pr jhjaggars/homelab#343 open, waiting on review
//	- decision t972236dfee waiting on the user: which key to use
//	- cleanup delete branch feat/x and its worktree (me, once told to go ahead)
//	- todo t972236dfee waiting on the user's answer
//	- commits feat/x pushed to origin
//	- resource k3d-cluster dev on desk
//	- resource pid 4242 on desk
//	- path /tmp/fetch-cache on desk
//	- branch origin/feat/x the pre-rebase backup
//	- issue jhjaggars/comebye#78 tick the box once it lands
//	- followup post the summary to the ticket (only the user can)
//	- other a loop I run every 10 minutes
//
// NOTHING LEFT is the old SAFE, and LEFT the old UNSAFE: replies in the old
// format (SAFE, UNSAFE) still parse, and the ledger keeps its "safe" and
// "unsafe" states. The parser is lenient: bullets or numbers, markdown around the words, a
// reason on the UNSAFE line itself, a PR as a URL, a to-do id anywhere in a
// line. Whatever it cannot type becomes an "other" item.

const (
	maxAskItems    = 20
	maxItemText    = 300
	maxReasonShown = 500
)

// askFormat is the part of the prompt that fixes the reply format.
const askFormat = "Reply in exactly this format. First line: NOTHING LEFT, or LEFT. After LEFT, one line per thing still left to finish this work, " +
	"each starting with \"- \" and a type, then in plain words what remains and who it waits on (you, the user, a reviewer, CI):\n" +
	"- decision TODO_ID <what the answer decides; waiting on the user>\n" +
	"- cleanup <what you can do yourself if told to go ahead: delete a branch or worktree, stop a process you started, close your PR, tick an issue box>\n" +
	"- pr OWNER/REPO#NUMBER <your note>\n" +
	"- todo TODO_ID <your note>\n" +
	"- commits BRANCH [pushed to REMOTE] <your note>\n" +
	"- resource KIND NAME_OR_ID [on HOST] <your note: say \"mine\" if you started it, \"keep\" if the user asked to keep it>   (KIND: pid, k3d-cluster, container, ...)\n" +
	"- path ABSOLUTE_PATH [on HOST] <your note: say \"mine\" if you created it>   (a file or directory left behind)\n" +
	"- branch REMOTE/BRANCH <your note>   (a remote branch left behind, such as origin/feat/x)\n" +
	"- issue OWNER/REPO#NUMBER <your note>\n" +
	"- followup <something only the user can do: post a comment, run a command, try a login>\n" +
	"- other <anything else: a loop you run, a request you owe the user>\n" +
	"Apart from your own scratch, do not do the cleanup now: only list it. Say NOTHING LEFT only when nothing remains. No other text."

// parseReply reads a session's reply to the ask: the state (safe: nothing
// left; unsafe: something is left; or unclear with the reply as its reason),
// the one-line reason, and the typed items of a "left" answer.
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
	case "decision":
		if id := todoIDRe.FindString(rest); id != "" {
			return ledger.AskItem{Type: ledger.ItemDecision, ID: id, Text: noteAfter(rest, id)}, true
		}
		if rest == "" {
			return ledger.AskItem{}, false
		}
		return ledger.AskItem{Type: ledger.ItemDecision, Text: clip(rest, maxItemText)}, true
	case "cleanup", "clean-up":
		if rest == "" {
			return ledger.AskItem{}, false
		}
		return ledger.AskItem{Type: ledger.ItemCleanup, Text: clip(rest, maxItemText)}, true
	case "path", "file", "dir", "directory":
		if it, ok := parsePath(rest); ok {
			return it, true
		}
		return other()
	case "branch":
		if it, ok := parseBranch(rest); ok {
			return it, true
		}
		return other()
	case "issue":
		if it, ok := parseIssue(rest); ok {
			return it, true
		}
		return other()
	case "followup", "follow-up", "followups":
		if rest == "" {
			return ledger.AskItem{}, false
		}
		return ledger.AskItem{Type: ledger.ItemFollowup, Text: clip(rest, maxItemText)}, true
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

// trimTok trims the punctuation a reference picks up in a sentence.
func trimTok(s string) string { return strings.Trim(s, ",.:;()\"'") }

// trimNote trims the dash or colon a note starts with.
func trimNote(toks []string) string {
	return strings.TrimLeftFunc(clip(strings.Join(toks, " "), maxItemText), func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("-–—:,", r) })
}

// parsePath reads "ABSOLUTE_PATH [on HOST] <note>".
func parsePath(rest string) (ledger.AskItem, bool) {
	toks := strings.Fields(strings.ReplaceAll(rest, "`", ""))
	if len(toks) == 0 {
		return ledger.AskItem{}, false
	}
	p := strings.TrimRight(trimTok(toks[0]), ".")
	if !strings.HasPrefix(p, "/") {
		return ledger.AskItem{}, false
	}
	it := ledger.AskItem{Type: ledger.ItemPath, Name: p}
	tail := toks[1:]
	if len(tail) >= 2 && strings.EqualFold(tail[0], "on") {
		it.Host = trimTok(tail[1])
		tail = tail[2:]
	}
	it.Text = trimNote(tail)
	return it, true
}

// remoteNames are the remotes a branch reference may start with.
var remoteNames = map[string]bool{"origin": true, "upstream": true}

// splitRemote cuts "origin/feat/x" into its remote and branch; a reference
// that names no remote is on origin.
func splitRemote(ref string) (remote, branch string) {
	ref = strings.TrimPrefix(strings.TrimPrefix(ref, "refs/remotes/"), "refs/heads/")
	if r, b, ok := strings.Cut(ref, "/"); ok && remoteNames[r] && b != "" {
		return r, b
	}
	return "origin", ref
}

// parseBranch reads "REMOTE/BRANCH <note>".
func parseBranch(rest string) (ledger.AskItem, bool) {
	toks := strings.Fields(strings.ReplaceAll(rest, "`", ""))
	if len(toks) == 0 {
		return ledger.AskItem{}, false
	}
	ref := strings.TrimRight(trimTok(toks[0]), ".")
	if ref == "" {
		return ledger.AskItem{}, false
	}
	remote, branch := splitRemote(ref)
	return ledger.AskItem{Type: ledger.ItemBranch, Remote: remote, Branch: branch, Text: trimNote(toks[1:])}, true
}

var issueRefRe = regexp.MustCompile(`([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(?:#|/issues/)(\d+)`)

// parseIssue reads "OWNER/REPO#NUMBER <note>" (or an issue's URL).
func parseIssue(rest string) (ledger.AskItem, bool) {
	rest = strings.ReplaceAll(rest, "`", "")
	if m := issueRefRe.FindStringSubmatch(rest); m != nil {
		n, _ := strconv.Atoi(m[2])
		return ledger.AskItem{Type: ledger.ItemIssue, Repo: m[1], Number: n, Text: noteAfter(rest, m[0])}, true
	}
	tok, _, _ := strings.Cut(rest, " ")
	if m := prNumRe.FindStringSubmatch(trimTok(tok)); m != nil {
		n, _ := strconv.Atoi(m[1])
		return ledger.AskItem{Type: ledger.ItemIssue, Number: n, Text: noteAfter(rest, tok)}, true
	}
	return ledger.AskItem{}, false
}

// Re-typing. Answers given before the path, branch, issue and followup types
// (and sessions that still write "cleanup" or "other") name things the hub
// can check in their own words. retype reads a cleanup or other item's words
// and, when they are plainly about one checkable thing, gives it that type,
// keeping the words and the old type (Was). It runs on the review's copy of
// an answer, so stored answers benefit without a re-ask.
//
// It is careful: settling a re-typed item can let the sweep remove the
// session, so a line that strings several steps together ("stop pid 4242,
// then close my PR") or names two things of a kind is left as it was. Only
// the words that put the step on the user make a followup whatever else
// the line says: a followup is never settled by the hub.
var (
	retypePIDRe     = regexp.MustCompile(`(?i)\bpid[ :#]*(\d{2,7})\b`)
	retypeRemoteRe  = regexp.MustCompile("(?:^|[\\s`'\"(])((?:origin|upstream)/[A-Za-z0-9][\\w.-]*(?:/[\\w.-]+)*)")
	retypeIssueWord = regexp.MustCompile(`(?i)\bissues?\b`)
	retypePRNumRe   = regexp.MustCompile(`(?i)\b(?:PR|pull request)\s*#?(\d+)\b`)
	retypeCommitsRe = regexp.MustCompile(`(?i)\bcommits? ahead\b|\bunpushed commits?\b|\bcommits? (?:not|never) pushed\b`)
	retypePathRe    = regexp.MustCompile("(?:^|[\\s`'\"(=])(/[A-Za-z0-9_.@+-]+(?:/[A-Za-z0-9_.@+-]+)*)/?")
	retypeURLRe     = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	retypeUserRe    = regexp.MustCompile(`(?i)\bwaiting (?:on|for) (?:the )?user\b|\b(?:the )?user (?:has|needs|must|will need) to\b|\bonly the user can\b|\bfor the user to (?:do|run|post|try)\b`)
	retypeSeveralRe = regexp.MustCompile(`(?i);|,\s*then\b|\bthen\b|\band also\b|\bas well as\b|\bplus\b`)
)

// own is the session's branch: words about deleting its remote copy keep the
// reading remoteBranchItem gives them, whatever else they mention.
func retype(item ledger.AskItem, own string) ledger.AskItem {
	if item.Type != ledger.ItemCleanup && item.Type != ledger.ItemOther {
		return item
	}
	t := strings.ReplaceAll(item.Text, "`", " ")
	as := func(n ledger.AskItem) ledger.AskItem {
		n.Text, n.Was, n.Done = item.Text, item.Type, item.Done
		return n
	}
	if b, ok := remoteBranchItem(item.Text, own); ok {
		return as(ledger.AskItem{Type: ledger.ItemBranch, Remote: "origin", Branch: b})
	}
	if ownBranchDeletion(item.Text, own) && !retypePathRe.MatchString(retypeURLRe.ReplaceAllString(t, " ")) {
		return item // the local branch goes with the worktree (recheck)
	}
	if !retypeSeveralRe.MatchString(t) {
		pids := distinct(retypePIDRe.FindAllStringSubmatch(t, -1))
		var remotes []string
		for _, r := range distinct(retypeRemoteRe.FindAllStringSubmatch(t, -1)) {
			if _, b := splitRemote(r); !protectedBranches[b] {
				remotes = append(remotes, r)
			}
		}
		noURL := retypeURLRe.ReplaceAllString(t, " ")
		issues := distinct(issueRefRe.FindAllStringSubmatch(t, -1))
		prs := distinct(prRefRe.FindAllStringSubmatch(t, -1))
		prNums := distinct(retypePRNumRe.FindAllStringSubmatch(t, -1))
		del := deleteVerbRe.MatchString(t)
		var paths []string
		for _, p := range distinct(retypePathRe.FindAllStringSubmatch(noURL, -1)) {
			if strings.Count(p, "/") >= 2 || p == "/tmp" {
				paths = append(paths, p)
			}
		}
		refs := len(pids) + len(remotes) + len(paths)
		switch {
		case len(pids) == 1 && refs == 1:
			return as(ledger.AskItem{Type: ledger.ItemResource, Kind: "pid", Name: pids[0]})
		case len(remotes) == 1 && refs == 1 && del:
			r, b := splitRemote(remotes[0])
			return as(ledger.AskItem{Type: ledger.ItemBranch, Remote: r, Branch: strings.TrimRight(b, ".")})
		case refs == 0 && retypeIssueWord.MatchString(t) && len(issues) == 1 && !strings.Contains(t, "/pull/"):
			m := issueRefRe.FindStringSubmatch(t)
			n, _ := strconv.Atoi(m[2])
			return as(ledger.AskItem{Type: ledger.ItemIssue, Repo: m[1], Number: n})
		// A PR named with no repository is left as words: the hub could not
		// check it anyway.
		case refs == 0 && !del && !retypeIssueWord.MatchString(t) && len(prs) == 1:
			m := prRefRe.FindStringSubmatch(t)
			n, _ := strconv.Atoi(m[2])
			return as(ledger.AskItem{Type: ledger.ItemPR, Repo: m[1], Number: n})
		case refs == 0 && !del && len(prs)+len(issues)+len(prNums) == 0 && retypeCommitsRe.MatchString(t):
			return as(ledger.AskItem{Type: ledger.ItemCommits})
		}
		if !retypeUserRe.MatchString(t) && len(paths) == 1 && refs == 1 && len(prs)+len(issues)+len(prNums) == 0 {
			return as(ledger.AskItem{Type: ledger.ItemPath, Name: strings.TrimRight(paths[0], ".")})
		}
	}
	if retypeUserRe.MatchString(t) {
		return as(ledger.AskItem{Type: ledger.ItemFollowup})
	}
	return item
}

// distinct is the first group of each match, once each.
func distinct(ms [][]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range ms {
		v := m[len(m)-1]
		if len(m) > 2 {
			v = m[1] + "#" + m[2]
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
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
	case ledger.ItemDecision:
		if it.ID == "" {
			return "a decision"
		}
		return "decision (to-do " + it.ID + ")"
	case ledger.ItemCleanup:
		return "cleanup"
	case ledger.ItemPath:
		s := "path " + it.Name
		if it.Host != "" {
			s += " on " + it.Host
		}
		return s
	case ledger.ItemBranch:
		return "branch " + orDefault(it.Remote, "origin") + "/" + it.Branch
	case ledger.ItemIssue:
		if it.Repo == "" {
			return fmt.Sprintf("issue #%d", it.Number)
		}
		return fmt.Sprintf("issue %s#%d", it.Repo, it.Number)
	case ledger.ItemFollowup:
		return "a follow-up for you"
	case ledger.ItemResource:
		s := it.Kind + " " + it.Name
		if it.Host != "" {
			s += " on " + it.Host
		}
		return s
	}
	return "something else"
}
