package server

import (
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/upgrade"
)

// The rollout alarm: when a hub bump (the deploy command) or a host upgrade
// of a released version fails, the hub files one action to-do on its own, so
// a failure nobody was watching is not lost on a page nobody reopens.
//
// It hangs off the two places where the hub already settles a run's verdict,
// once per run (settleDeployAudit and settleUpgradeAudit), so there is no
// poller of its own: the page, the release sweep and the rollout loop all
// read the results through them.
//
// One open item per release and target (the hub, or a host): a second
// failure while it is open adds nothing, and a later run that succeeds
// closes it. Its AutoKey names the failed run, so the same run settled again
// (a restart forgets which runs it saw) never refiles an item the user
// dismissed; a new failure after the item closed files a new one.

// SourceRollout marks an item the hub filed because a rollout step failed.
const SourceRollout = "hub-rollout"

// rolloutAlarmTargetHub is the target of a deploy (the hub bump).
const rolloutAlarmTargetHub = "hub"

// rolloutAlarmLines is how many of the run's last output lines the item quotes.
const rolloutAlarmLines = 12

var phaseRE = regexp.MustCompile(`\bphase=([A-Za-z0-9_.-]{1,40})`)

func rolloutAlarmPrefix(release, target string) string {
	return SourceRollout + ":" + release + ":" + target + "@"
}

// rolloutFailed files the item for a failed run, unless one is open for the
// release and target already. run names the run (its id or start time).
func (s *Server) rolloutFailed(rel Todo, target, run, what, headline, errText, output string) {
	if s.todos == nil || rel.Release == nil {
		return
	}
	prefix := rolloutAlarmPrefix(rel.ID, target)
	for _, it := range s.todos.list("") {
		if it.active() && it.Source == SourceRollout && strings.HasPrefix(it.AutoKey, prefix) {
			return // one open item per release and target
		}
	}
	version := rel.Release.Version
	reason := headline
	if reason == "" {
		reason = errText
	}
	phase := ""
	if m := phaseRE.FindStringSubmatch(reason); m != nil {
		phase = m[1]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s for %s failed.\n\n", what, mdSafe(version))
	if phase != "" {
		fmt.Fprintf(&b, "- Phase: `%s`\n", mdSafe(phase))
	}
	if reason != "" {
		fmt.Fprintf(&b, "- Reason: %s\n", mdSafe(oneLine(reason, 300)))
	}
	if tail := lastLines(cleanDeployOutput(output), rolloutAlarmLines); tail != "" {
		fmt.Fprintf(&b, "\nThe last lines of its output:\n\n```\n%s\n```\n", strings.ReplaceAll(tail, "```", "'''"))
	}
	b.WriteString("\nFix the cause, then retry from the release item. This item closes itself when a retry succeeds.")
	title := "The " + version + " hub rollout failed"
	if target != rolloutAlarmTargetHub {
		title = "The " + version + " upgrade of " + target + " failed"
	}
	it := Todo{
		Question:  oneLine(title, todoMaxQuestion),
		Context:   block(b.String(), todoMaxContext),
		Kind:      "action",
		Links:     []TodoLink{{Label: "Open the " + oneLine(version, 20) + " release", URL: "/#/todo/" + rel.ID}},
		CreatedBy: AutoCreator,
		Source:    SourceRollout,
		AutoKey:   prefix + run,
	}
	out, err := s.todos.addAuto(it)
	switch {
	case err == nil:
		log.Printf("rollout alarm: %s for %s %s (%s)", out.ID, version, target, oneLine(reason, 120))
	case errors.Is(err, errAutoDup):
	default:
		log.Printf("rollout alarm: %s %s: %v", version, target, err)
	}
}

// rolloutSucceeded closes the open item for the release and target.
func (s *Server) rolloutSucceeded(rel Todo, target string) {
	if s.todos == nil {
		return
	}
	prefix := rolloutAlarmPrefix(rel.ID, target)
	for _, it := range s.todos.list("") {
		if !it.active() || it.Source != SourceRollout || !strings.HasPrefix(it.AutoKey, prefix) {
			continue
		}
		if _, err := s.todos.close(it.ID, TodoDone, AutoCreator, "a retry succeeded"); err != nil && !errors.Is(err, errTodoClosed) {
			log.Printf("rollout alarm: close %s: %v", it.ID, err)
		}
	}
}

// alarmDeploy is the alarm's side of a settled deploy record.
func (s *Server) alarmDeploy(rec DeployRecord) {
	if s.todos == nil || rec.Todo == "" {
		return
	}
	rel, ok := s.todos.get(rec.Todo)
	if !ok || rel.Release == nil || rel.Release.Version != rec.Version {
		return
	}
	switch rec.State {
	case depOK:
		s.rolloutSucceeded(rel, rolloutAlarmTargetHub)
	case depFailed:
		label := s.cfg.Release.DeployLabel
		if label == "" {
			label = "Deploy"
		}
		what := fmt.Sprintf("The **%s** command on %s", mdSafe(label), mdSafe(s.releaseHost()))
		s.rolloutFailed(rel, rolloutAlarmTargetHub, rec.Started.UTC().Format(time.RFC3339Nano), what, rec.Headline, rec.Error, rec.Output)
	}
}

// alarmUpgrade is the alarm's side of a settled upgrade record: only an
// upgrade (not a rollback, not a dry run) to a released version counts.
func (s *Server) alarmUpgrade(host string, rec upgrade.Record) {
	if s.todos == nil || rec.Rollback || host == "" {
		return
	}
	var rel Todo
	found := false
	for _, it := range s.todos.list("all") { // a tagged release rolls out whether or not its item is open
		if it.Release != nil && it.Release.State == relTagged && it.Release.Version == rec.Version {
			rel, found = it, true
			break
		}
	}
	if !found {
		return
	}
	switch rec.State {
	case upgrade.RecOK:
		s.rolloutSucceeded(rel, host)
	case upgrade.RecFailed, upgrade.RecRolledBack:
		why := rec.Error
		if why == "" {
			why = rec.State
		}
		s.rolloutFailed(rel, host, rec.ID, "The upgrade of "+mdSafe(host), "", why, rec.Output)
	}
}

// alarmHubStep files the hub's To do when the rollout's hub step stopped or
// was skipped. A deploy that ran and failed is filed by alarmDeploy with its
// output; this covers the rest (the deploy never started, the hub could not
// be planned), which used to leave a quiet "skipped" and nothing else.
func (s *Server) alarmHubStep(j Rollout, st RolloutStep) {
	if s.todos == nil || j.Todo == "" {
		return
	}
	rel, ok := s.todos.get(j.Todo)
	if !ok || rel.Release == nil || rel.Release.Version != j.Version {
		return
	}
	what := "The rollout's hub step (" + mdSafe(st.Host) + ")"
	if st.Phase == roSkipped {
		what = "Bumping the hub (" + mdSafe(st.Host) + ") in the rollout"
	}
	s.rolloutFailed(rel, rolloutAlarmTargetHub, fmt.Sprintf("%s:%s:%d", j.ID, st.Phase, st.Attempt), what, "", st.Detail, st.Output)
}

// lastLines is the last n non-blank lines of s.
func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
