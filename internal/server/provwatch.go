package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/provision"
)

// Adding a host, slice 4: the safety timers.
//
// On the provisioner (watchHosts, after every look at the cloud): at 80% of a
// host's budget cap the host's record says so; at 100% the host is stopped
// (disks kept, never destroyed); a host stopped for as long as it was asked to
// wait is started, has its sessions saved and is destroyed. The clock is the
// provisioner's own (a sleeping hub changes nothing); a provisioner that is
// offline leaves only the guest's own timers (idle stop, the longest run, and
// the cap-equivalent run the tool wrote into the host), which the hub reports.
//
// On the hub (hubProvisionStep): it reads what the provisioners have recorded
// and raises a to-do for each thing the user should know, once.

// watchHosts does what is due for the node's hosts. One thing at a time; a
// running job (a create, a destroy) holds the slot, and the check waits for the
// next look.
func (s *Server) watchHosts(ctx context.Context) {
	tool := s.cfg.Provision.Tool
	if s.phosts == nil || tool == nil || !tool.Real {
		return
	}
	for _, a := range s.phosts.Due() {
		switch a.Kind {
		case provision.ActWarn:
			s.phosts.MarkWarned(a.Name)
			s.auditBy(ledger.AuditEvent{Action: "provision.budget_warn", By: "provisioner", OK: true, Host: a.Name, Detail: a.Why})
		case provision.ActStop:
			release, ok := s.pjobs.hold()
			if !ok {
				continue
			}
			err := tool.RunStop(ctx, a.Name)
			release()
			if err != nil {
				s.auditBy(ledger.AuditEvent{Action: "provision.budget_stop", By: "provisioner", OK: false, Host: a.Name, Detail: a.Why + ": the stop failed: " + err.Error()})
				continue
			}
			s.phosts.MarkStopped(a.Name, true)
			s.auditBy(ledger.AuditEvent{Action: "provision.budget_stop", By: "provisioner", OK: true, Host: a.Name, Detail: a.Why + "; stopped, disks kept"})
		case provision.ActDestroy:
			j, err := s.pjobs.destroy(tool, a.Name, destroyReq{StartFirst: true, Why: a.Why})
			if err != nil {
				continue // busy: the next look tries again
			}
			s.phosts.MarkAutoDestroy(a.Name, a.Why)
			s.auditBy(ledger.AuditEvent{Action: "provision.auto_destroy", By: "provisioner", OK: true, Host: a.Name, Detail: "job " + j.ID + ": " + a.Why})
		}
	}
}

// ---- the hub's to-dos ----

// provSeen remembers which events already became a to-do, in a file, so that a
// restart of the hub does not raise them again.
type provSeen struct {
	mu   sync.Mutex
	path string
	seen map[string]string
	off  map[string]time.Time // node -> since when it has not answered (not kept)
}

func openProvSeen(stateDir string) *provSeen {
	p := &provSeen{seen: map[string]string{}, off: map[string]time.Time{}}
	if stateDir == "" {
		return p
	}
	p.path = filepath.Join(stateDir, "provision-todos.json")
	if b, err := os.ReadFile(p.path); err == nil {
		_ = json.Unmarshal(b, &p.seen)
	}
	return p
}

func (p *provSeen) saveLocked() {
	if p.path == "" {
		return
	}
	if len(p.seen) > 400 {
		keys := make([]string, 0, len(p.seen))
		for k := range p.seen {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys[:len(keys)-300] {
			delete(p.seen, k)
		}
	}
	b, _ := json.Marshal(p.seen)
	tmp := p.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, p.path)
	}
}

// once is true the first time it sees key.
func (p *provSeen) once(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.seen[key]; ok {
		return false
	}
	p.seen[key] = time.Now().UTC().Format(time.RFC3339)
	p.saveLocked()
	return true
}

// provOfflineAfter is how long a provisioner with hosts may stay silent before
// the hub says so.
const provOfflineAfter = 15 * time.Minute

type provNote struct{ key, question, ctx, kind string }

// provNotes are the to-dos the current picture calls for, each with a key that
// names the event, so it is raised once.
func (s *Server) provNotes(now time.Time) []provNote {
	var out []provNote
	s.prov.mu.Lock()
	nodes := make([]string, 0, len(s.prov.hosts))
	for n := range s.prov.hosts {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		c := s.prov.hosts[n]
		if c.View != nil {
			for _, h := range c.View.Hosts {
				if h.State == "destroyed" && h.AutoDestroy == "" {
					continue
				}
				cap := fmt.Sprintf("$%.2f", h.BudgetCapUSD)
				switch {
				case h.Warned80At != "" && h.State != "destroyed":
					out = append(out, provNote{key: h.Name + "|80|" + h.Warned80At, kind: "followup",
						question: fmt.Sprintf("%s is at 80%% of its %s budget cap", h.Name, cap),
						ctx:      fmt.Sprintf("%s (built by %s) has cost about $%.2f so far. At 100%% (%s) the provisioner stops it and keeps its disks; it never destroys a host for a cap.", h.Name, n, h.Spend.USD, cap)})
				}
				if h.CapStopped != "" && h.State != "destroyed" {
					out = append(out, provNote{key: h.Name + "|cap|" + h.CapStopped, kind: "action",
						question: fmt.Sprintf("%s was stopped at its %s budget cap", h.Name, cap),
						ctx:      fmt.Sprintf("%s reached its cap (about $%.2f spent) and %s stopped it; its disks are kept and cost about $%.4f an hour. Start it again or destroy it from Peers → Prices. A host that is still over its cap is stopped again at the next look.", h.Name, h.Spend.USD, n, h.Spend.USDHour)})
				}
				if h.State == "destroyed" && h.AutoDestroy != "" {
					out = append(out, provNote{key: h.Name + "|autod|" + h.Destroyed, kind: "followup",
						question: fmt.Sprintf("%s was destroyed after sitting stopped", h.Name),
						ctx:      fmt.Sprintf("%s: %s. Its sessions were saved first and its bill is final: about $%.2f.%s", n, h.AutoDestroy, h.Spend.USD, dataKeptNote(h))})
				}
				if h.DestroyFailed != "" && h.AutoDestroy != "" && h.State != "destroyed" {
					out = append(out, provNote{key: h.Name + "|autof|" + h.DestroyFailAt, kind: "action",
						question: fmt.Sprintf("The automatic destroy of %s did not finish", h.Name),
						ctx:      fmt.Sprintf("%s tried to destroy %s (%s) and stopped: %s. Nothing was destroyed. It tries again in about 6 hours, or destroy it yourself from Peers → Prices.", n, h.Name, h.AutoDestroy, h.DestroyFailed)})
				}
			}
		}
	}
	s.prov.mu.Unlock()
	return out
}

func dataKeptNote(h provision.HostView) string {
	if h.DataKept {
		return fmt.Sprintf(" Its %d GB data disk was kept and still costs a little each month.", h.DataDiskGB)
	}
	return ""
}

// hubProvisionStep reads each provisioner's hosts, notes how long one has been
// silent, and raises the to-dos that are due.
func (s *Server) hubProvisionStep(ctx context.Context) {
	if s.todos == nil || s.pseen == nil || s.ledger == nil {
		return
	}
	nodes := s.hostsNodes()
	if len(nodes) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n string) { defer wg.Done(); s.askHosts(ctx, n, false) }(n)
	}
	wg.Wait()
	s.raiseProvTodos(nodes)
}

// raiseProvTodos turns what the hub last read into to-dos, once each.
func (s *Server) raiseProvTodos(nodes []string) {
	now := s.prov.now()
	notes := s.provNotes(now)
	// a provisioner with live hosts that does not answer
	s.prov.mu.Lock()
	for _, n := range nodes {
		c := s.prov.hosts[n]
		if c == nil {
			continue
		}
		if c.Err == "" {
			delete(s.pseen.off, n)
			continue
		}
		since, ok := s.pseen.off[n]
		if !ok {
			since = now
			s.pseen.off[n] = since
		}
		live := 0
		if c.View != nil {
			live = c.View.Total.Live
		}
		if live > 0 && now.Sub(since) >= provOfflineAfter {
			notes = append(notes, provNote{key: n + "|off|" + since.UTC().Format(time.RFC3339), kind: "action",
				question: fmt.Sprintf("%s has been offline for %d minutes, %d host%s unwatched", n, int(now.Sub(since).Minutes()), live, sfx(live)),
				ctx:      fmt.Sprintf("%s builds and watches these hosts. While it is offline only the hosts' own timers apply (stop after idle, the longest run, and the run the budget pays for at the on-demand rate); the exact budget check and the destroy-after-stopped clock wait for it to come back. Last answer from it: %s", n, c.Err)})
		}
	}
	s.prov.mu.Unlock()
	for _, nt := range notes {
		if !s.pseen.once(nt.key) {
			continue
		}
		_, _, err := s.todos.add(Todo{Question: nt.question, Context: nt.ctx, Kind: nt.kind, CreatedBy: AutoCreator, Source: "hub-provision"})
		if err != nil {
			log.Printf("provision: to-do %q: %v", nt.question, err)
			continue
		}
		s.auditBy(ledger.AuditEvent{Action: "provision.todo", By: "hub", OK: true, Detail: nt.question})
	}
}

func sfx(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
