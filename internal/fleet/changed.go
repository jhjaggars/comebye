package fleet

import (
	"encoding/json"
	"strings"

	"github.com/jhjaggars/comebye/internal/state"
)

// Changes is the fan-out of "changed" events to the browsers' streams (see
// state.Changes). Every server write path that changes a store a screen shows
// calls ChangedKind; a peer's own kinds come in on its stream and go out here.
func (f *Fleet) Changes() *state.Changes { return f.changes }

// ChangedKind says kind has new data on this instance (see state.Changes).
func (f *Fleet) ChangedKind(kind string) { f.changes.Changed(kind) }

// OnPeerChanged sets fn, called (outside the fleet's locks) with each kind a
// peer reports after it was relayed: the hub maps a provisioner's job id to
// its own copy of the job.
func (f *Fleet) OnPeerChanged(fn func(host, kind string)) {
	f.mu.Lock()
	f.peerChanged = fn
	f.mu.Unlock()
}

// relayKind is what a peer's "changed" kind means on this instance: the
// peer's own stores only (its parked agents, its upgrade, a transcript of one
// of its agents, a job it runs). A transcript is named by the hub's name for
// the peer, never the peer's own. "" drops it.
func relayKind(peer, kind string) string {
	if !state.ValidChangedKind(kind) {
		return ""
	}
	switch {
	case kind == state.ChangedParked, kind == state.ChangedReleases:
		return kind
	case strings.HasPrefix(kind, state.ChangedJobPrefix):
		return kind
	case strings.HasPrefix(kind, state.ChangedTranscriptPrefix):
		id := strings.TrimPrefix(kind, state.ChangedTranscriptPrefix)
		pane := id[strings.LastIndexByte(id, '/')+1:]
		if pane == "" || len(pane) > maxPaneID {
			return ""
		}
		return state.ChangedTranscriptPrefix + state.QualifiedID(peer, pane)
	}
	return "" // ledger, todos, pages, usage: the hub's own
}

// changed relays one "changed" event from the peer's stream.
func (p *peer) changed(data string) {
	var ev struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal([]byte(data), &ev) != nil {
		return
	}
	kind := relayKind(p.name, ev.Kind)
	if kind == "" {
		return
	}
	p.f.changes.Changed(kind)
	p.f.mu.RLock()
	fn := p.f.peerChanged
	p.f.mu.RUnlock()
	if fn != nil {
		fn(p.name, kind)
	}
}
