package fleet

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/state"
)

// The peer registry: peers added and removed while the hub runs. They are
// kept in a Store (peers.json) and merged with the static -peers at
// startup. Static peers are listed but never removed through the registry.

// Refusals of Add and Remove (the server maps them to HTTP statuses).
var (
	ErrNotHub     = errors.New("this instance is not a hub")
	ErrStaticPeer = errors.New("this peer is set in -peers / COMEBYE_PEERS; edit the hub's environment and restart to change it")
	ErrLocalName  = errors.New("that is this host's own name")
	ErrNoStore    = errors.New("the peer registry is not available")
)

// InvalidError is bad input to Add (a 400).
type InvalidError struct{ Msg string }

func (e InvalidError) Error() string { return e.Msg }

// registry is the Fleet's attachment to a Store.
type registry struct {
	store *Store
	opt   AddrOptions
}

// AttachStore makes the fleet's registry persist to store, validating
// runtime addresses with opt.
func (f *Fleet) AttachStore(store *Store, opt AddrOptions) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reg = &registry{store: store, opt: opt}
}

// LoadDynamic converts the store's records to peers for fleet.New:
// records that clash with a static name or the local name, or whose
// addresses no longer validate, are skipped and reported by logf (the
// static configuration wins; a bad file entry must not stop the hub).
func LoadDynamic(store *Store, static []Peer, localName string, opt AddrOptions, logf func(string, ...any)) []Peer {
	taken := map[string]bool{localName: true}
	for _, p := range static {
		taken[p.Name] = true
	}
	var out []Peer
	for _, r := range store.List() {
		if taken[r.Name] {
			logf("peers: ignoring stored peer %q: the name is also a static peer or this host", r.Name)
			continue
		}
		p, err := r.Peer(opt)
		if err != nil {
			logf("peers: ignoring stored peer %q: %v", r.Name, err)
			continue
		}
		p.Source, p.AddedBy, p.AddedAt, p.Tag = SourceDynamic, r.AddedBy, r.AddedAt, r.Tag
		out = append(out, p)
	}
	return out
}

// PeerInfo is one peer as listed by the registry.
type PeerInfo struct {
	Name    string     `json:"name"`
	URLs    []string   `json:"urls"`
	Source  string     `json:"source"`
	AddedBy string     `json:"added_by,omitempty"`
	AddedAt *time.Time `json:"added_at,omitempty"`
	Tag     string     `json:"tag,omitempty"`
	state.HostStatus
}

// List describes every peer (static first, then dynamic by name).
func (f *Fleet) List() []PeerInfo {
	out := []PeerInfo{}
	for _, src := range []string{SourceStatic, SourceDynamic} {
		for _, p := range f.peerList() {
			if p.meta.Source != src {
				continue
			}
			hs := p.hostStatus()
			hs.VersionMismatch = f.opts.Version != "" && hs.Version != "" && hs.Version != f.opts.Version
			info := PeerInfo{Name: p.name, Source: p.meta.Source, AddedBy: p.meta.AddedBy, Tag: p.meta.Tag, HostStatus: hs}
			for _, u := range p.addrs {
				info.URLs = append(info.URLs, u.String())
			}
			if !p.meta.AddedAt.IsZero() {
				t := p.meta.AddedAt
				info.AddedAt = &t
			}
			out = append(out, info)
		}
	}
	return out
}

// Add registers a dynamic peer: validates it, stores it, starts its
// stream and health check at once. The caller has checked the caller's
// authority; by records who asked.
func (f *Fleet) Add(name string, urls []string, by, tag string) (PeerInfo, error) {
	f.mu.Lock()
	reg := f.reg
	if reg == nil {
		f.mu.Unlock()
		return PeerInfo{}, ErrNoStore
	}
	if !f.opts.HubMode && len(f.peers) == 0 {
		f.mu.Unlock()
		return PeerInfo{}, ErrNotHub
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if !ValidName(name) {
		f.mu.Unlock()
		return PeerInfo{}, InvalidError{fmt.Sprintf("name %q must be a lowercase DNS label (a-z, 0-9, -)", name)}
	}
	if name == f.localName {
		f.mu.Unlock()
		return PeerInfo{}, ErrLocalName
	}
	if tag = strings.TrimSpace(tag); len(tag) > 64 || strings.ContainsAny(tag, " \t\r\n") {
		f.mu.Unlock()
		return PeerInfo{}, InvalidError{"tag must be one word of at most 64 characters (tag:worker)"}
	}
	if len(by) > 128 {
		by = by[:128]
	}
	if old := f.byName[name]; old != nil {
		f.mu.Unlock()
		if old.meta.Source == SourceStatic {
			return PeerInfo{}, fmt.Errorf("%w: %s", ErrExists, ErrStaticPeer.Error())
		}
		return PeerInfo{}, ErrExists
	}
	addrs, err := ParseAddrs(urls, reg.opt)
	if err != nil {
		f.mu.Unlock()
		return PeerInfo{}, InvalidError{err.Error()}
	}
	taken := map[string]string{}
	for _, p := range f.peers {
		for _, u := range p.addrs {
			taken[u.String()] = p.name
		}
	}
	for _, u := range addrs {
		if other, ok := taken[u.String()]; ok {
			f.mu.Unlock()
			return PeerInfo{}, InvalidError{fmt.Sprintf("address %s already belongs to peer %q", u, other)}
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	rec := Record{Name: name, AddedBy: by, AddedAt: now, Tag: tag}
	for _, u := range addrs {
		rec.URLs = append(rec.URLs, u.String())
	}
	if err := reg.store.Add(rec); err != nil {
		f.mu.Unlock()
		return PeerInfo{}, err
	}
	p := f.addLocked(Peer{Name: name, URLs: addrs, Source: SourceDynamic, AddedBy: by, AddedAt: now, Tag: tag})
	if f.runCtx != nil {
		p.start(f.runCtx)
	}
	f.mu.Unlock()
	f.opts.Logf("peers: added %s at %s (by %s, tag %q)", name, p.meta.Addrs(), by, tag)
	f.broadcast()
	for _, i := range f.List() {
		if i.Name == name {
			return i, nil
		}
	}
	return PeerInfo{}, errors.New("peer vanished")
}

// Remove drops a dynamic peer: its stream, probes and in-flight forwards
// are cancelled, its agents leave the merged view. A static peer is refused.
func (f *Fleet) Remove(name, by string) error {
	f.mu.Lock()
	reg := f.reg
	if reg == nil {
		f.mu.Unlock()
		return ErrNoStore
	}
	if !f.opts.HubMode && len(f.peers) == 0 {
		f.mu.Unlock()
		return ErrNotHub
	}
	p := f.byName[name]
	switch {
	case p == nil:
		f.mu.Unlock()
		return ErrNotFound
	case p.meta.Source == SourceStatic:
		f.mu.Unlock()
		return ErrStaticPeer
	}
	if err := reg.store.Remove(name); err != nil && !errors.Is(err, ErrNotFound) {
		f.mu.Unlock()
		return err
	}
	delete(f.byName, name)
	for i, q := range f.peers {
		if q == p {
			f.peers = append(f.peers[:i:i], f.peers[i+1:]...)
			break
		}
	}
	p.cancel()
	f.mu.Unlock()
	f.opts.Logf("peers: removed %s (by %s)", name, by)
	f.broadcast()
	return nil
}
