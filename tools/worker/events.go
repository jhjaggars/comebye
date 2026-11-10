package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
)

// Events are the tool's machine-readable progress: with --events, stdout
// carries one JSON object per line, one per stage transition, and the human
// text moves to stderr. The app reads these; a person reads stderr.
//
//	{"v":1,"seq":12,"stage":"instance","step":"4/9","state":"running","msg":"..."}
//
// Only the fields of event can appear: there is no free-form map, so nothing
// outside this list can leak into a line, and every message passes sanitize.

const eventsVersion = 1

// Event states.
const (
	stateEvRunning = "running"
	stateEvOK      = "ok"
	stateEvFailed  = "failed"
	stateEvPlanned = "planned" // dry run: this stage would run
)

// Failure classes, the ones the app can act on.
const (
	classSpotCapacity = "spot_capacity"
	classCredsExpired = "creds_expired"
	classQuota        = "quota"
	classBootstrap    = "bootstrap_failed"
	classNeedsInput   = "needs_input"
	classOther        = "other"
)

type event struct {
	V     int    `json:"v"`
	Seq   int    `json:"seq"`
	Stage string `json:"stage"`
	Step  string `json:"step,omitempty"`
	State string `json:"state"`
	Msg   string `json:"msg,omitempty"`
	Class string `json:"class,omitempty"` // failures only
	Zone  string `json:"zone,omitempty"`  // spot_capacity only: the zone that had no capacity
}

// eventSink writes events. It is safe for concurrent use.
type eventSink struct {
	mu  sync.Mutex
	w   io.Writer
	seq int
	env *env
}

func (s *eventSink) emit(ev event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ev.V, ev.Seq = eventsVersion, s.seq
	ev.Msg = s.env.sanitize(ev.Msg)
	b, _ := json.Marshal(ev)
	s.w.Write(append(b, '\n'))
}

// Stage names, in order, per kind. The human lines keep their "N/8" numbering
// for people; events add the final hub stage.
var (
	vmStages      = []string{"network", "ssh_key", "data_disk", "instance", "ssh", "bootstrap", "tailnet", "agents", "hub"}
	sandboxStages = []string{"sandbox", "tailnet", "settings", "hub"}
)

// stageTracker remembers the stage in flight so that a later error can be
// attached to it.
type stageTracker struct {
	names []string
	cur   int // 1-based; 0 before the first stage
}

func (e *env) emit(ev event) {
	if e.Events != nil {
		e.Events.emit(ev)
	}
}

// begin starts stage n (1-based) of the kind being built: it ends the previous
// one, emits the transition and prints the human line.
func (e *env) begin(n int, format string, a ...any) {
	e.endStage()
	t := e.stages
	if t == nil || n < 1 || n > len(t.names) {
		e.printf(format, a...)
		return
	}
	t.cur = n
	e.emit(event{Stage: t.names[n-1], Step: fmt.Sprintf("%d/%d", n, len(t.names)), State: stateEvRunning})
	e.printf(format, a...)
}

// endStage reports the stage in flight as done.
func (e *env) endStage() {
	t := e.stages
	if t == nil || t.cur == 0 {
		return
	}
	e.emit(event{Stage: t.names[t.cur-1], Step: fmt.Sprintf("%d/%d", t.cur, len(t.names)), State: stateEvOK})
	t.cur = 0
}

// failStage reports err against the stage in flight, with its class.
func (e *env) failStage(err error) {
	t := e.stages
	stage, step := "create", ""
	if t != nil && t.cur > 0 {
		stage, step = t.names[t.cur-1], fmt.Sprintf("%d/%d", t.cur, len(t.names))
	}
	class := classify(err, stage)
	ev := event{Stage: stage, Step: step, State: stateEvFailed, Msg: err.Error(), Class: class}
	if class == classSpotCapacity && e.Cfg != nil {
		ev.Zone = e.zone()
	}
	e.emit(ev)
	if t != nil {
		t.cur = 0
	}
}

// zone is the availability zone the request builds in.
func (e *env) zone() string {
	switch e.Cfg.Provider {
	case "aws":
		return e.Cfg.AWSAZ
	case "gcp":
		return e.Cfg.Zone
	}
	return ""
}

// track runs one whole command that is staged: it sets up the stage list, and
// turns its outcome into events.
func (e *env) track(names []string, f func() error) error {
	if e.Events == nil {
		return f()
	}
	e.stages = &stageTracker{names: names}
	err := f()
	if err != nil {
		e.failStage(err)
	} else {
		e.endStage()
	}
	return err
}

// planEvents emits a "planned" event for every stage, for --dry-run.
func (e *env) planEvents(names []string) {
	for i, n := range names {
		e.emit(event{Stage: n, Step: fmt.Sprintf("%d/%d", i+1, len(names)), State: stateEvPlanned})
	}
}

// needsInputError is returned where the tool would have asked a person.
type needsInputError struct{ what string }

func (n *needsInputError) Error() string { return "needs input: " + n.what }

func needsInput(format string, a ...any) error {
	return &needsInputError{what: fmt.Sprintf(format, a...)}
}

var (
	reCapacity = regexp.MustCompile(`(?i)InsufficientInstanceCapacity|capacity-not-available|SpotMaxPriceTooLow|insufficient capacity|no capacity|ZONE_RESOURCE_POOL_EXHAUSTED|not enough resources`)
	reExpired  = regexp.MustCompile(`(?i)ExpiredToken|RequestExpired|token has expired|session has expired|reauthenticat|aws (sso )?login|unable to locate credentials|invalid_grant|ExpiredTokenException|credentials.*(expired|not found)|gcloud auth login|Reauthentication`)
	reQuota    = regexp.MustCompile(`(?i)VcpuLimitExceeded|InstanceLimitExceeded|MaxSpotInstanceCountExceeded|LimitExceeded|quota|QUOTA_EXCEEDED`)
)

// classify names the kind of failure, for the app. The stage it happened in
// matters only for bootstrap, which is an ssh script failing on the new host.
func classify(err error, stage string) string {
	var ni *needsInputError
	switch msg := err.Error(); {
	case errors.As(err, &ni):
		return classNeedsInput
	case reExpired.MatchString(msg):
		return classCredsExpired
	case reCapacity.MatchString(msg):
		return classSpotCapacity
	case reQuota.MatchString(msg):
		return classQuota
	case stage == "bootstrap" || stage == "agents":
		return classBootstrap
	}
	return classOther
}

var secretShapes = regexp.MustCompile(`tskey-[A-Za-z0-9_-]+|sk-[A-Za-z0-9_-]{8,}|\b(AKIA|ASIA)[0-9A-Z]{16}\b|Bearer [A-Za-z0-9._~+/=-]+|(?i:secret|token|password)=\S+`)

// sanitize is the one place a message passes through before it leaves the
// process on an event: secrets this run learned of are removed, then anything
// shaped like a key, then it is cut to one short line.
func (e *env) sanitize(s string) string {
	for _, sec := range e.secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	s = secretShapes.ReplaceAllString(s, "[redacted]")
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "..."
	}
	return s
}

// addSecret registers a value that must never appear in an event or a
// message built from one.
func (e *env) addSecret(s string) {
	if s != "" {
		e.secrets = append(e.secrets, s)
	}
}
