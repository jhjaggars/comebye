package fleet

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
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
		if r.Instance == "" { // a peers.json from before instances: the peer present now is the one
			r.Instance = NewInstance()
			if err := store.SetInstance(r.Name, r.Instance); err != nil {
				logf("peers: could not save the instance of %q: %v", r.Name, err)
			}
		}
		p.Instance = r.Instance
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
	// Instance is this incarnation of the peer (see Record.Instance).
	Instance string `json:"instance,omitempty"`
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
			info := PeerInfo{Name: p.name, Source: p.meta.Source, AddedBy: p.meta.AddedBy, Tag: p.meta.Tag, Instance: p.instance(), HostStatus: hs}
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
	rec := Record{Name: name, AddedBy: by, AddedAt: now, Tag: tag, Instance: NewInstance()}
	for _, u := range addrs {
		rec.URLs = append(rec.URLs, u.String())
	}
	if err := reg.store.Add(rec); err != nil {
		f.mu.Unlock()
		return PeerInfo{}, err
	}
	p := f.addLocked(Peer{Name: name, URLs: addrs, Source: SourceDynamic, AddedBy: by, AddedAt: now, Tag: tag, Instance: rec.Instance})
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
	old := p.instance()
	f.mu.Unlock()
	f.opts.Logf("peers: removed %s (by %s)", name, by)
	f.ended(name, old, "its host "+name+" was removed from the hub")
	f.broadcast()
	return nil
}

// NewInstance makes a fresh instance id (a random UUID).
func NewInstance() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

var instanceRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidInstance reports whether s is usable as an instance id (a UUID or a
// cloud instance id such as i-0abc123).
func ValidInstance(s string) bool { return instanceRE.MatchString(s) }

// OnInstanceEnd sets fn, called (outside the fleet's locks) when a peer's
// incarnation ends: the peer was removed, or its instance replaced. host and
// instance name the incarnation that ended.
func (f *Fleet) OnInstanceEnd(fn func(host, instance, why string)) {
	f.mu.Lock()
	f.instanceEnd = fn
	f.mu.Unlock()
}

func (f *Fleet) ended(host, instance, why string) {
	f.mu.RLock()
	fn := f.instanceEnd
	f.mu.RUnlock()
	if fn != nil && instance != "" {
		fn(host, instance, why)
	}
}

// Instances maps each peer that has an instance to it. The local host and
// static peers have none.
func (f *Fleet) Instances() map[string]string {
	out := map[string]string{}
	for _, p := range f.peerList() {
		if in := p.instance(); in != "" {
			out[p.name] = in
		}
	}
	return out
}

// SetInstance replaces a dynamic peer's instance (the cloud's id, set by
// `worker create`). A different value ends the old incarnation: its ledger
// entries are lost. The same value is a no-op.
func (f *Fleet) SetInstance(name, instance, by string) (PeerInfo, error) {
	if !ValidInstance(instance) {
		return PeerInfo{}, InvalidError{"instance must be 1-64 letters, digits, '.', '_' or '-'"}
	}
	f.mu.Lock()
	reg := f.reg
	if reg == nil {
		f.mu.Unlock()
		return PeerInfo{}, ErrNoStore
	}
	p := f.byName[name]
	switch {
	case p == nil:
		f.mu.Unlock()
		return PeerInfo{}, ErrNotFound
	case p.meta.Source == SourceStatic:
		f.mu.Unlock()
		return PeerInfo{}, ErrStaticPeer
	}
	old := p.instance()
	if old == instance {
		f.mu.Unlock()
		return f.info(name), nil
	}
	if err := reg.store.SetInstance(name, instance); err != nil {
		f.mu.Unlock()
		return PeerInfo{}, err
	}
	p.setInstance(instance)
	f.mu.Unlock()
	f.opts.Logf("peers: %s is now instance %s (was %s; by %s)", name, instance, old, by)
	f.ended(name, old, "its host "+name+" was replaced by another machine ("+instance+")")
	f.broadcast()
	return f.info(name), nil
}

// Refusals of Announce.
var (
	// ErrNameTaken: the name belongs to a peer on a different node.
	ErrNameTaken = errors.New("that peer name belongs to another node")
	// ErrNodeRegistered: the node is already registered under another name.
	ErrNodeRegistered = errors.New("this node is already registered under another name")
)

func hostOf(u interface{ Hostname() string }) string {
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}

// FindByHost returns the peer one of whose addresses is on node (a
// MagicDNS name, lowercase, no trailing dot): its name, recorded tag and
// source.
func (f *Fleet) FindByHost(node string) (name, tag string, ok bool) {
	node = strings.ToLower(strings.TrimSuffix(node, "."))
	for _, p := range f.peerList() {
		for _, u := range p.addrs {
			if hostOf(u) == node {
				return p.name, p.meta.Tag, true
			}
		}
	}
	return "", "", false
}

// Announce registers (or re-confirms) the peer a tagged node announced.
// The caller has verified node and tag with WhoIs. rawURL, when given,
// must be on that very node: a worker can register only itself. The same
// node announcing again is a no-op (created is false); a name held by a
// different node, or a node already registered under another name, is
// refused.
func (f *Fleet) Announce(name, rawURL, node, tag string) (info PeerInfo, created bool, err error) {
	node = strings.ToLower(strings.TrimSuffix(node, "."))
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name, _, _ = strings.Cut(node, ".")
	}
	if rawURL = strings.TrimSpace(rawURL); rawURL == "" {
		rawURL = "https://" + node
	}
	f.announceMu.Lock()
	defer f.announceMu.Unlock()
	f.mu.RLock()
	reg := f.reg
	f.mu.RUnlock()
	if reg == nil {
		return PeerInfo{}, false, ErrNoStore
	}
	addrs, err := ParseAddrs([]string{rawURL}, reg.opt)
	if err != nil {
		return PeerInfo{}, false, InvalidError{err.Error()}
	}
	if h := hostOf(addrs[0]); h != node {
		return PeerInfo{}, false, InvalidError{fmt.Sprintf("the announced address must be this node (%s), not %s", node, h)}
	}
	if have, _, ok := f.FindByHost(node); ok {
		if have == name {
			return f.info(name), false, nil
		}
		return PeerInfo{}, false, fmt.Errorf("%w (%q)", ErrNodeRegistered, have)
	}
	if name != "" && f.lookup(name) != nil {
		return PeerInfo{}, false, ErrNameTaken
	}
	by := tag + "@" + node
	info, err = f.Add(name, []string{addrs[0].String()}, by, tag)
	if err != nil {
		return PeerInfo{}, false, err
	}
	f.opts.Logf("peers: self-registered %s at %s (node %s, tag %s)", name, addrs[0], node, tag)
	return info, true, nil
}

func (f *Fleet) info(name string) PeerInfo {
	for _, i := range f.List() {
		if i.Name == name {
			return i
		}
	}
	return PeerInfo{}
}

// Logf logs through the fleet's logger.
func (f *Fleet) Logf(format string, args ...any) { f.opts.Logf(format, args...) }
