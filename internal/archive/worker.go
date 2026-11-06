package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// A spot worker holds no archive key. The machine that runs tools/worker pulls
// its sessions over ssh and uploads them under a host name of the worker's own,
// so this file is what tells that host apart from an ordinary one.

// WorkerInfo says which VM an archive host belongs to.
type WorkerInfo struct {
	Name       string // the worker's name, which is the host name in the ledger
	Provider   string
	InstanceID string
	Created    time.Time // when the VM was first created; zero if unknown
}

// WorkerHost is the archive host of a worker: its name plus its instance id.
// Worker names get reused and instance ids do not, and a stop, a start or a
// spot reclaim keeps the id. A worker name has no dots, so it can be read back.
func WorkerHost(name, instanceID string) string { return name + "." + instanceID }

// ClaimWorker records this machine as the uploader of a worker's sessions under
// a.Host. Another machine's claim is refused, as for any host: creating objects
// safely depends on one writer per host. A plain claim by this machine, left by
// `archive run --host` during a recovery, is replaced so the index can find it.
func (a *Archiver) ClaimWorker(w WorkerInfo) error {
	key := hostClaimKey(a.Host)
	mine := machineID()
	first := a.now().UTC().Format(time.RFC3339)
	raw, err := a.B.Get(key)
	switch {
	case err == nil:
		var cl hostClaim
		if err := json.Unmarshal(raw, &cl); err != nil {
			return fmt.Errorf("archive: reading %s: %w", key, err)
		}
		if cl.Machine != mine {
			return fmt.Errorf("archive: worker host %q is uploaded by another machine (%s, since %s): run this from that machine", a.Host, cl.Hostname, cl.FirstSeen)
		}
		if cl.Kind == hostKindWorker {
			return nil
		}
		first = cl.FirstSeen
		if err := a.B.Delete(key); err != nil {
			return fmt.Errorf("archive: replacing %s: %w", key, err)
		}
	case !errors.Is(err, ErrNotExist):
		return fmt.Errorf("archive: reading %s: %w", key, err)
	}
	name, _ := os.Hostname()
	cl := hostClaim{Machine: mine, Hostname: name, FirstSeen: first, Kind: hostKindWorker,
		Worker: w.Name, Provider: w.Provider, InstanceID: w.InstanceID}
	if !w.Created.IsZero() {
		cl.InstanceCreated = w.Created.UTC().Format(time.RFC3339)
	}
	body, _ := json.Marshal(cl)
	if err := a.B.Put(key, body); err != nil && !errors.Is(err, ErrExists) {
		return err
	}
	return nil
}

// forHost is this archiver reading and writing another host's objects.
func (a *Archiver) forHost(host string) *Archiver {
	c := *a
	c.Host = host
	c.beforeWrite = nil
	return &c
}

// hostClaims reads every hosts/<host>.json, keyed by host. A failure is an
// error, never an empty set: an index run must not forget a worker's sessions
// because of a network blip.
func (a *Archiver) hostClaims() (map[string]hostClaim, error) {
	keys, err := a.B.List("hosts/")
	if err != nil {
		return nil, fmt.Errorf("archive: listing hosts: %w", err)
	}
	out := make(map[string]hostClaim, len(keys))
	for _, k := range keys {
		if !strings.HasSuffix(k, ".json") {
			continue
		}
		raw, err := a.B.Get(k)
		if errors.Is(err, ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("archive: reading %s: %w", k, err)
		}
		var cl hostClaim
		if err := json.Unmarshal(raw, &cl); err != nil {
			return nil, fmt.Errorf("archive: reading %s: %w", k, err)
		}
		out[strings.TrimSuffix(strings.TrimPrefix(k, "hosts/"), ".json")] = cl
	}
	return out, nil
}

// maxLinkAfterStart is how long after a worker session starts a handoff can
// still be the one it ran for, allowing for clock differences.
const maxLinkAfterStart = 5 * time.Minute

// linkScope is who a host is in the ledger. An ordinary host is its own name. A
// worker is its bare name, shared by every VM that ever had that name, so its
// links must also fall inside this VM's life.
type linkScope struct {
	host      string
	worker    bool
	notBefore time.Time
}

func scopeOf(host string, claims map[string]hostClaim) linkScope {
	cl, ok := claims[host]
	if !ok || cl.Kind != hostKindWorker {
		return linkScope{host: host}
	}
	s := linkScope{host: cl.Worker, worker: true}
	s.notBefore, _ = time.Parse(time.RFC3339, cl.InstanceCreated)
	return s
}

// admits reports whether a handoff created at ct could belong to a session of
// this scope that started at st.
func (s linkScope) admits(ct, st time.Time) bool {
	if !s.worker {
		return true
	}
	if !s.notBefore.IsZero() && ct.Before(s.notBefore) {
		return false
	}
	return st.IsZero() || !ct.After(st.Add(maxLinkAfterStart))
}
