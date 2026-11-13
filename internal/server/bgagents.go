package server

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// Background work, shown on the agent list. herdr calls an agent idle or
// done whenever Claude is not mid-turn, though a test run, a CI watch or a
// Monitor it started may still be going and will wake it. The app shows
// such an agent as waiting rather than finished, from what the session file
// says it has open (transcript.BackgroundTasks, the same read the "finished"
// notice uses). Only a short label and the start time leave the server,
// never output.

type bgBrief = herdr.BackgroundTask

const (
	bgLabelMax = 40
	// bgAgentTTL is how long a read stands for the same idle stretch: a
	// task can only end by waking the session, which changes the status.
	bgAgentTTL = 2 * time.Minute
)

// bgCmdSkip are words that start a command line without saying what it is.
var bgCmdSkip = map[string]bool{"cd": true, "env": true, "sudo": true, "time": true, "nohup": true, "export": true, "set": true}

var bgSecretRE = regexp.MustCompile(`(?i)(token|secret|passw|key|bearer|auth)`)

// bgLabel is a short, harmless name for a task: its description, or for a
// task with only a command line the first words of it (the program, never
// its flags, values or assignments).
func bgLabel(t transcript.BackgroundTask) string {
	l := strings.TrimSpace(t.Desc)
	if t.Cmd {
		l = ""
		for _, seg := range strings.FieldsFunc(t.Desc, func(r rune) bool { return r == ';' || r == '&' || r == '|' || r == '\n' }) {
			f := strings.Fields(seg)
			if len(f) == 0 || bgCmdSkip[f[0]] {
				continue
			}
			var words []string
			for _, w := range f {
				if strings.ContainsAny(w, "=$`\"'/~") || strings.HasPrefix(w, "-") || bgCmdSkip[w] {
					if len(words) > 0 {
						break
					}
					continue
				}
				words = append(words, w)
				if len(words) == 2 {
					break
				}
			}
			l = strings.Join(words, " ")
			break
		}
		if bgSecretRE.MatchString(l) {
			l = ""
		}
	}
	return transcript.Excerpt(l, bgLabelMax)
}

func briefOf(tasks []transcript.BackgroundTask) []bgBrief {
	var out []bgBrief
	for _, t := range tasks {
		out = append(out, bgBrief{Kind: strings.ToLower(t.Tool), Label: bgLabel(t), Since: t.Since})
	}
	return out
}

type bgAgentEntry struct {
	stamp  string
	briefs []bgBrief
	at     time.Time
	busy   bool
}

type bgAgents struct {
	mu sync.Mutex
	m  map[string]*bgAgentEntry
}

// isLocalHost: the agent runs on this instance (a hub reads a peer's
// background tasks from the peer, which alone can read its session).
func (s *Server) isLocalHost(host string) bool {
	return host == "" || s.fleet == nil || host == s.fleet.LocalName()
}

// withBackground stamps the full records a hub reads from this instance
// with the background tasks of its own idle agents.
func (s *Server) withBackground(agents []herdr.AgentInfo) []herdr.AgentInfo {
	out := make([]herdr.AgentInfo, len(agents))
	copy(out, agents)
	for i := range out {
		if s.isLocalHost(out[i].Host) {
			out[i].Background = s.backgroundOfAgent(out[i])
		}
	}
	s.stampQuiet(out)
	s.stampPark(out)
	return out
}

// backgroundOfAgent returns what an idle or done agent has running in the
// background, from the last read; it never waits for one. A read starts when
// there is none for this stretch of idleness, and the browsers get a fresh
// agent list when it finds the answer changed.
func (s *Server) backgroundOfAgent(a herdr.AgentInfo) []bgBrief {
	if !s.isLocalHost(a.Host) {
		if a.Status != herdr.StatusIdle && a.Status != herdr.StatusDone {
			return nil
		}
		return a.Background // read by the peer that owns the agent
	}
	if a.Status != herdr.StatusIdle && a.Status != herdr.StatusDone {
		return nil
	}
	key := state.AgentKey(&a)
	stamp := ""
	if a.StatusSince != nil {
		stamp = a.StatusSince.UTC().Format(time.RFC3339Nano)
	}
	s.bgAgents.mu.Lock()
	if s.bgAgents.m == nil {
		s.bgAgents.m = map[string]*bgAgentEntry{}
	}
	e := s.bgAgents.m[key]
	if e == nil {
		e = &bgAgentEntry{}
		s.bgAgents.m[key] = e
	}
	stale := e.at.IsZero() || e.stamp != stamp || time.Since(e.at) > bgAgentTTL
	var out []bgBrief
	if e.stamp == stamp {
		out = e.briefs
	}
	if stale && !e.busy {
		e.busy = true
		go s.readBackground(a, key, stamp)
	}
	s.bgAgents.mu.Unlock()
	return out
}

func (s *Server) readBackground(a herdr.AgentInfo, key, stamp string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var briefs []bgBrief
	if l := s.latestOf(ctx, a.Host, a.PaneID); l != nil && l.BackgroundKnown {
		briefs = briefOf(l.Background)
	}
	s.bgAgents.mu.Lock()
	e := s.bgAgents.m[key]
	changed := false
	if e != nil {
		changed = !sameBriefs(e.briefs, briefs) || e.stamp != stamp && len(briefs) > 0
		e.stamp, e.briefs, e.at, e.busy = stamp, briefs, time.Now(), false
	}
	s.bgAgents.mu.Unlock()
	if changed && s.fleet != nil {
		s.fleet.Changed()
	}
}

func sameBriefs(a, b []bgBrief) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
