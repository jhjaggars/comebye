package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
)

// Finding a Page's chat again (slice 3 of #22). A tap on "Chat about this"
// looks the Page up in chats.json first:
//
//	alive   the helper is in the fleet: open it
//	parked  a park record names it: resume it, then open it
//	gone    neither (or its scratch directory is gone): start a new one and
//	        say so
//
// "Start a new chat…" (body {"new":true}) skips the lookup and replaces the
// record; the old helper is left alone. Either way a newer Page version than
// the chat started on queues one "Page is now vN" line for the helper, and
// the record's version moves on so a later tap does not repeat it.

// chatDirRE is the name of a chat's scratch directory: <page id>-<digits>.
var chatDirRE = regexp.MustCompile(`^pg_[0-9a-f]{10}-[0-9]+$`)

// chatBase is the directory chat scratch directories live in, resolved.
func (s *Server) chatBase() string {
	if s.cfg.StateDir == "" {
		return ""
	}
	base := filepath.Join(s.cfg.StateDir, "chats")
	if real, err := filepath.EvalSymlinks(base); err == nil {
		return real
	}
	return base
}

// isChatDirName: p is, by its name alone, a chat scratch directory of this
// host (a direct child of <state>/chats named <page>-<n>). It need not exist.
func (s *Server) isChatDirName(p string) bool {
	base := s.chatBase()
	if base == "" || p == "" || !filepath.IsAbs(p) {
		return false
	}
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		p = filepath.Join(real, filepath.Base(p))
	}
	return filepath.Dir(p) == base && chatDirRE.MatchString(filepath.Base(p))
}

// chatDirOf is the real path of p when it is an existing chat scratch
// directory of this host, never a symlink out of chats/.
func (s *Server) chatDirOf(p string) (string, bool) {
	if !s.isChatDirName(p) {
		return "", false
	}
	if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
		return "", false
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil || !s.isChatDirName(real) {
		return "", false
	}
	return real, true
}

// removeChatDir deletes the scratch directory of a chat, and only that: a
// path that is not a direct <state>/chats/<page>-<n> child is left alone.
func (s *Server) removeChatDir(p string) (bool, error) {
	dir, ok := s.chatDirOf(p)
	if !ok {
		return false, nil
	}
	return true, os.RemoveAll(dir)
}

// update changes the Page's record in place (no earlier-chat entry).
func (s *chatStore) update(page string, fn func(*ChatRec)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.data[page]
	if !ok {
		return nil
	}
	fn(&r)
	s.data[page] = r
	return writeFileAtomic(s.path, s.data)
}

// dropAgent removes the records whose helper is agent (it was removed).
func (s *chatStore) dropAgent(agent string) {
	if s == nil || agent == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k, r := range s.data {
		if r.Agent == agent {
			delete(s.data, k)
			changed = true
		}
	}
	if changed {
		_ = writeFileAtomic(s.path, s.data)
	}
}

// chatVersionNote is the line queued for a helper whose Page has moved on.
func chatVersionNote(page string, now, was int) string {
	return fmt.Sprintf("Page %s is now version %d (you started on version %d). Read it again before you answer: comebye pages show %s --body --text", page, now, was, page)
}

// noteChatVersion queues the "Page is now vN" line for the helper when the
// Page has a newer version than the record's, once: the record moves on.
func (s *Server) noteChatVersion(p Page, rec ChatRec, agentID string) string {
	if p.Version <= rec.Version {
		return ""
	}
	note := chatVersionNote(p.ID, p.Version, rec.Version)
	if s.pq != nil {
		if _, _, err := s.pq.add(QueuedPrompt{Agent: agentID, Name: rec.Name, Text: note, From: "comebye"}); err != nil {
			return ""
		}
	} else if err := s.deliverPrompt(context.Background(), rec.Name, note); err != nil {
		return ""
	}
	_ = s.chats.update(p.ID, func(r *ChatRec) { r.Version = p.Version })
	return fmt.Sprintf("Page is now v%d: the chat was told to read it again.", p.Version)
}

// chatReuse is what a tap found.
type chatReuse struct {
	state string // "alive", "parked", "gone", "away"
	agent herdr.AgentInfo
	park  parkedRecord
	why   string
}

// findChat says what became of the Page's recorded helper.
func (s *Server) findChat(rec ChatRec) chatReuse {
	agents := s.fleet.Agents()
	for _, a := range agents {
		if a.ID == rec.Agent && (a.Name == "" || a.Name == rec.Name) {
			return chatReuse{state: "alive", agent: a}
		}
	}
	if pr, ok := parkLookup(s.parkSnapshot(), rec.Agent, rec.Name); ok {
		if pr.Rec.Parked() {
			if !pr.Online {
				return chatReuse{state: "away", why: rec.Host + " is not answering, so its parked chat cannot be resumed now"}
			}
			return chatReuse{state: "parked", park: pr}
		}
		// Resumed by hand since: it lives in another pane now.
		if id, live := resumedID(pr, agents); live {
			for _, a := range agents {
				if a.ID == id {
					return chatReuse{state: "alive", agent: a}
				}
			}
		}
	}
	if rec.Host != s.fleet.LocalName() {
		for _, h := range s.fleet.Hosts() {
			if h.Name == rec.Host && h.Status != fleet.StatusOK {
				return chatReuse{state: "away", why: rec.Host + " is not answering, so its chat cannot be found now"}
			}
		}
	}
	return chatReuse{state: "gone"}
}

// reuseChat answers a tap from the Page's recorded helper when there is one
// to open or resume. It reports whether it answered; false means start a new
// one (out then carries what to say about the one that is gone).
func (s *Server) reuseChat(ctx context.Context, w http.ResponseWriter, p Page, rec ChatRec, out map[string]any) bool {
	f := s.findChat(rec)
	switch f.state {
	case "away":
		writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "chat_host_away", "message": f.why + ". Try again in a moment, or start a new chat."})
		return true
	case "alive":
		ag := s.slimAgents([]herdr.AgentInfo{f.agent})[0]
		res := map[string]any{"agent": ag, "page": p.ID, "version": p.Version, "host": rec.Host, "name": rec.Name, "reused": "open",
			"writable": rec.Writable, "chat": map[string]any{"page": p.ID, "dir": rec.Dir}}
		if f.agent.ID != rec.Agent {
			rec.Agent = f.agent.ID
			_ = s.chats.update(p.ID, func(r *ChatRec) { r.Agent = f.agent.ID })
		}
		if n := s.noteChatVersion(p, rec, f.agent.ID); n != "" {
			res["page_note"] = n
		}
		writeJSON(w, res)
		return true
	case "parked":
		var res resumeResult
		body := resumeBody{ID: f.park.Rec.ID, NoNote: true}
		var lerr *launchError
		if f.park.Host == s.fleet.LocalName() {
			r, e := s.resumeAgent(ctx, body)
			if e != nil {
				lerr = e
			} else {
				res = *r
			}
		} else {
			lerr = s.callHost(ctx, f.park.Host, "resume", nil, body, &res)
		}
		if lerr != nil {
			switch lerr.code {
			case "not_found", "session_gone", "not_resumable", "no_such_record", "outside_roots":
				// Nothing to bring back: it counts as gone.
				out["gone_why"] = lerr.message
				return false
			}
			writeLaunchError(w, lerr)
			return true
		}
		id := res.Agent.ID
		if id == "" {
			id = state.QualifiedID(f.park.Host, res.Agent.PaneID)
		}
		_ = s.chats.update(p.ID, func(r *ChatRec) { r.Agent = id })
		if s.roles != nil {
			if err := s.roles.set(id, Role{Role: RoleChat, Name: rec.Name, Page: p.ID, By: rec.By, At: time.Now().UTC()}); err != nil {
				log.Printf("roles: %v", err)
			}
		}
		rec.Agent = id
		ag := res.Agent
		ag.ID, ag.Host = id, f.park.Host
		resp := map[string]any{"agent": ag, "page": p.ID, "version": p.Version, "host": rec.Host, "name": rec.Name, "reused": "resumed",
			"writable": rec.Writable, "chat": map[string]any{"page": p.ID, "dir": rec.Dir}}
		if res.Warn != "" {
			resp["warnings"] = []string{res.Warn}
		}
		if n := s.noteChatVersion(p, rec, id); n != "" {
			resp["page_note"] = n
		}
		writeJSON(w, resp)
		return true
	}
	return false
}

// dropGoneChatDir deletes the scratch directory of a chat that is gone, when
// it is on this host.
func (s *Server) dropGoneChatDir(rec ChatRec) {
	if rec.Host == s.fleet.LocalName() && rec.Dir != "" && !strings.Contains(rec.Dir, "..") {
		_, _ = s.removeChatDir(rec.Dir)
	}
}
