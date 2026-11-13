package server

import (
	"regexp"
	"strings"
)

// jargon maps internal terms to the plain words the user reads. The same
// list is in the coordinator playbook ("Plain words for the user").
var jargon = []struct{ term, plain string }{
	{"worktree", "local copy"},
	{"ledger", "the record"},
	{"teardown", "cleanup"},
	{"handoff", "task"},
	{"ready_to_remove", "ready to remove"},
	{"fast-forward", "push to main"},
	{"whole-module", "the full test suite"},
	{"-race", "the full test suite"},
	{"rebase", "bring up to date with main"},
}

var jargonRE = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(jargon))
	for i, j := range jargon {
		out[i] = regexp.MustCompile(`(?i)(^|[^\w-])` + regexp.QuoteMeta(j.term) + `s?([^\w-]|$)`)
	}
	return out
}()

// jargonWarning names listed jargon words in the text a to-do shows the
// user. It only warns: the item is filed either way.
func jargonWarning(texts ...string) string {
	all := strings.Join(texts, "\n")
	var found []string
	for i, re := range jargonRE {
		if re.MatchString(all) {
			found = append(found, "\""+jargon[i].term+"\" (say \""+jargon[i].plain+"\")")
		}
	}
	if len(found) == 0 {
		return ""
	}
	return "the user reads this on a phone: plain words please, instead of " + strings.Join(found, ", ")
}
