package server

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/park"
)

// The hub's view of parked agents. Each host owns its records; the hub asks the
// peers (GET /api/parked?all=1) and keeps the last answer, so it can list a host
// that is down (the mirror, parked-mirror.json), keep queued prompts and waiting
// to-dos for a parked agent, and hand them to its new pane when it resumes.

const (
	// parkQueueTTL is how long a prompt to a parked agent waits for it to resume.
	parkQueueTTL = 7 * 24 * time.Hour
	// parkRefreshEvery is how often the hub re-asks the peers, at most.
	parkRefreshEvery = 20 * time.Second
)

// parkedRecord is a record with where it was read.
type parkedRecord struct {
	Host   string
	Online bool
	Rec    park.Record
}

// parkCache is the peers' records as last fetched.
type parkCache struct {
	mu       sync.Mutex
	hosts    map[string]parkedHost // peers only; the local store is read live
	at       time.Time
	inflight bool
}

// parkedHost is one host's section of the hub's list of parked agents.
type parkedHost struct {
	Host string `json:"host"`
	// Online is false when the host could not be asked just now: its entries
	// are the hub's last copy (Seen) and cannot be resumed until it is back.
	Online bool          `json:"online"`
	Local  bool          `json:"local,omitempty"`
	Seen   *time.Time    `json:"seen,omitempty"`
	Error  string        `json:"error,omitempty"`
	Parked []park.Record `json:"parked"`
}

// parkMirror is the hub's copy of the peers' records (parked-mirror.json),
// the answer for a host that is down. The host stays the source of truth.
type parkMirror struct {
	Hosts map[string]parkMirrorHost `json:"hosts"`
}

type parkMirrorHost struct {
	Seen   time.Time     `json:"seen"`
	Parked []park.Record `json:"parked"`
}

// fetchParkedAll asks every host for its records, resumed ones included. A peer
// that does not answer gets the hub's last copy from the mirror.
func (s *Server) fetchParkedAll(ctx context.Context) []parkedHost {
	hosts := s.fleet.Hosts()
	out := make([]parkedHost, len(hosts))
	mirror := s.loadParkMirror()
	var mu sync.Mutex
	var wg sync.WaitGroup
	changed := false
	for i, h := range hosts {
		out[i] = parkedHost{Host: h.Name, Local: h.Local, Parked: []park.Record{}}
		if h.Local {
			out[i].Online = true
			if s.parkStore() != nil {
				out[i].Parked = append(out[i].Parked, s.parkStore().List(false)...)
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var res struct {
				Parked []park.Record `json:"parked"`
			}
			// Ask even a host reported down: it may be back, and a failure is quick.
			hctx, hcancel := context.WithTimeout(ctx, 8*time.Second)
			defer hcancel()
			lerr := s.callHost(hctx, h.Name, "parked", url.Values{"all": {"1"}}, nil, &res)
			mu.Lock()
			defer mu.Unlock()
			if lerr == nil {
				out[i].Online, out[i].Parked = true, append(out[i].Parked, res.Parked...)
				mirror.Hosts[h.Name] = parkMirrorHost{Seen: time.Now().UTC(), Parked: res.Parked}
				changed = true
				return
			}
			out[i].Error = lerr.message
			if m, ok := mirror.Hosts[h.Name]; ok {
				seen := m.Seen
				out[i].Seen, out[i].Parked = &seen, append(out[i].Parked, m.Parked...)
			}
		}()
	}
	wg.Wait()
	if changed {
		s.saveParkMirror(mirror)
	}
	c := &s.pcache
	c.mu.Lock()
	c.hosts = map[string]parkedHost{}
	for _, h := range out {
		if !h.Local {
			c.hosts[h.Host] = h
		}
	}
	c.at = time.Now()
	c.mu.Unlock()
	return out
}

// handleParkedAll is GET /api/parked/all: every host's parked agents (the ones
// already resumed are history and left out).
func (s *Server) handleParkedAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	hosts := s.fetchParkedAll(ctx)
	for i := range hosts {
		only := []park.Record{}
		for _, rec := range hosts[i].Parked {
			if rec.Parked() {
				only = append(only, rec)
			}
		}
		hosts[i].Parked = only
	}
	writeJSON(w, map[string]any{"hosts": hosts})
}

// refreshParkCache re-asks the peers in the background, at most every
// parkRefreshEvery (a hub with no peers has nothing to ask).
func (s *Server) refreshParkCache() {
	if !s.fleet.HasPeers() {
		return
	}
	c := &s.pcache
	c.mu.Lock()
	if c.inflight || time.Since(c.at) < parkRefreshEvery {
		c.mu.Unlock()
		return
	}
	c.inflight = true
	c.mu.Unlock()
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.fetchParkedAll(ctx)
		c.mu.Lock()
		c.inflight = false
		c.mu.Unlock()
	}()
}

// parkSnapshot is every known record: this host's, read live, and the peers' as
// last fetched.
func (s *Server) parkSnapshot() []parkedRecord {
	var out []parkedRecord
	if ps := s.parkStore(); ps != nil {
		for _, r := range ps.List(false) {
			out = append(out, parkedRecord{Host: s.fleet.LocalName(), Online: true, Rec: r})
		}
	}
	c := &s.pcache
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, h := range c.hosts {
		for _, r := range h.Parked {
			out = append(out, parkedRecord{Host: h.Host, Online: h.Online, Rec: r})
		}
	}
	return out
}

func paneOf(id string) string {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' {
			return id[i+1:]
		}
	}
	return id
}

// parkLookup finds the record an agent id (host/pane, as it was when something was
// queued or filed for it) and name refer to: the newest one whose pane at park time
// is that pane and whose name matches. ok is false when none does.
func parkLookup(recs []parkedRecord, id, name string) (parkedRecord, bool) {
	host, pane := idHost(id), paneOf(id)
	var best parkedRecord
	found := false
	for _, pr := range recs {
		r := pr.Rec
		if pr.Host != host || r.PaneID != pane || (name != "" && r.Name != name) {
			continue
		}
		if !found || r.ParkedAt.After(best.Rec.ParkedAt) {
			best, found = pr, true
		}
	}
	return best, found
}

// resumedID is the fleet id a resumed record's agent has now, when it is live.
func resumedID(pr parkedRecord, agents []herdr.AgentInfo) (string, bool) {
	if pr.Rec.Parked() || pr.Rec.ResumedPane == "" {
		return "", false
	}
	id := pr.Host + "/" + pr.Rec.ResumedPane
	for _, a := range agents {
		if a.ID == id && (a.Name == "" || a.Name == pr.Rec.Name) {
			return id, true
		}
	}
	return "", false
}

// parkedTarget resolves what a caller named (a park id pk_…, or host/pane) to a
// parked record.
func (s *Server) parkedTarget(ref, name string) (parkedRecord, bool) {
	recs := s.parkSnapshot()
	for _, pr := range recs {
		if pr.Rec.ID == ref && pr.Rec.Parked() {
			return pr, true
		}
	}
	if pr, ok := parkLookup(recs, ref, name); ok && pr.Rec.Parked() {
		return pr, true
	}
	return parkedRecord{}, false
}

// rebindParked runs on every fleet update: it keeps the peers' records fresh and
// moves what waited for a parked agent (queued prompts, to-dos waiting on it) to
// the pane it resumed in.
func (s *Server) rebindParked(agents []herdr.AgentInfo) {
	s.refreshParkCache()
	recs := s.parkSnapshot()
	if len(recs) == 0 {
		return
	}
	s.parkLedger(recs, agents)
	if s.pq != nil {
		for _, it := range s.pq.list("", false) {
			if pr, ok := parkLookup(recs, it.Agent, it.Name); ok {
				if id, live := resumedID(pr, agents); live && id != it.Agent {
					if err := s.pq.rebind(it.ID, id); err != nil {
						logQueueErr(err)
					}
				}
			}
		}
	}
	if s.todos != nil {
		for _, t := range s.todos.list("") {
			if t.Waiting == nil {
				continue
			}
			pr, ok := parkLookup(recs, t.Waiting.ID, t.Waiting.Name)
			if !ok || pr.Rec.ResumedAt == nil || t.CreatedAt.After(*pr.Rec.ResumedAt) {
				continue
			}
			if id, live := resumedID(pr, agents); live && id != t.Waiting.ID {
				_, _ = s.todos.mutate(t.ID, func(it *Todo) error {
					if it.Waiting != nil {
						w := *it.Waiting // copy on write: served copies share the pointer
						w.ID = id
						it.Waiting = &w
					}
					return nil
				})
			}
		}
	}
}

// parkLedger records parks and resumes on the ledger entry of the worker that
// went through them, and points the entry at the pane the worker resumed in
// (an entry follows its agent's id, and a resumed agent has a new one).
func (s *Server) parkLedger(recs []parkedRecord, agents []herdr.AgentInfo) {
	if s.ledger == nil {
		return
	}
	for _, pr := range recs {
		r := pr.Rec
		old := pr.Host + "/" + r.PaneID
		if _, ok := s.ledger.ByAgent(old); !ok {
			continue
		}
		var err error
		if r.Parked() {
			_, err = s.ledger.NotePark(old, ledger.ParkEvent{ID: r.ID, Event: "parked", At: r.ParkedAt, Pane: r.PaneID}, nil)
		} else if id, live := resumedID(pr, agents); live {
			to := &ledger.Where{AgentID: id, PaneID: r.ResumedPane}
			for _, a := range agents {
				if a.ID == id {
					to.WorkspaceID = a.WorkspaceID
				}
			}
			// The parked event first, so the history reads in order whatever the hub saw.
			_, err = s.ledger.NotePark(old, ledger.ParkEvent{ID: r.ID, Event: "parked", At: r.ParkedAt, Pane: r.PaneID}, nil)
			if err == nil {
				_, err = s.ledger.NotePark(old, ledger.ParkEvent{ID: r.ID, Event: "resumed", At: *r.ResumedAt, Pane: r.ResumedPane}, to)
			}
		}
		if err != nil {
			log.Printf("ledger: park history: %v", err)
		}
	}
}
