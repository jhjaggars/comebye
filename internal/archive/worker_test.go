package archive

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// piSession writes a pi session that ran in cwd, under the rig's pi directory.
func (r *rig) piSession(sub, id, cwd, start, text string) {
	r.t.Helper()
	slug := "--" + strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-") + "--"
	head, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": id, "timestamp": start, "cwd": cwd})
	msg, _ := json.Marshal(map[string]any{"type": "message", "id": "m1", "timestamp": start,
		"message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": text}}}})
	r.write(filepath.Join(r.pi, sub, slug, "2026-10-03T10-00-00-000Z_"+id+".jsonl"), string(head)+"\n"+string(msg)+"\n", false)
}

// uploadWorker pulls what is in pi/<sub> as the given worker VM would be pulled.
func (r *rig) uploadWorker(sub, name, id string, created time.Time) *Archiver {
	r.t.Helper()
	a := r.a.forHost(WorkerHost(name, id))
	if err := a.ClaimWorker(WorkerInfo{Name: name, Provider: "aws", InstanceID: id, Created: created}); err != nil {
		r.t.Fatal(err)
	}
	res, err := a.RunFinal([]Source{{Kind: "pi", Root: filepath.Join(r.pi, sub)}})
	if err != nil || len(res.Errors) > 0 {
		r.t.Fatalf("%v %v", err, res.Errors)
	}
	return a
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestWorkerHostsAreIndexedBesideThisHost(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	sid := "11111111-1111-4111-8111-111111111111"
	r.write(filepath.Join(r.claude, "-home-u-x", sid+".jsonl"), `{"type":"user","sessionId":"`+sid+`","cwd":"/home/u/x","timestamp":"2026-10-03T08:00:00Z","message":{"role":"user","content":"an ordinary session"}}`+"\n", false)
	if _, err := r.a.Run(r.sources()); err != nil {
		t.Fatal(err)
	}
	r.piSession("w1", "aaaa", "/data/worktrees/widgets/feat-a", "2026-10-03T10:00:00Z", "worker session about zebras")
	r.uploadWorker("w1", "w1", "i-1", mustTime(t, "2026-10-03T09:00:00Z"))

	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	links := []Link{{ID: "h-1", Host: "w1", AgentID: "w1/w1:p1", RepoName: "widgets", Branch: "feat/a", Worktree: "/data/worktrees/widgets/feat-a", Brief: "Do the zebras", CreatedAt: "2026-10-03T09:59:00Z"}}
	st, err := ix.Sync(r.a, r.op, links, SyncOptions{})
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("%v %v", err, st.Errors)
	}
	if st.Sessions != 2 || st.Threads != 2 {
		t.Fatalf("both hosts should keep their threads: %+v", st)
	}
	hits, err := ix.Search("zebras", Filter{})
	if err != nil || len(hits) == 0 {
		t.Fatalf("%v %v", err, hits)
	}
	if hits[0]["host"] != "w1.i-1" {
		t.Errorf("host = %v", hits[0]["host"])
	}
	// The handoff is joined on the ledger's name for the worker, not the archive's.
	if hits[0]["ledger_id"] != "h-1" {
		t.Errorf("not linked to its handoff: %v", hits[0])
	}
	// A rerun changes nothing and keeps both.
	st, err = ix.Sync(r.a, r.op, links, SyncOptions{})
	if err != nil || st.Unchanged != 2 || st.Indexed != 0 || st.Threads != 2 {
		t.Fatalf("%v %+v", err, st)
	}
	// The bare worker name finds it; a prefix of the name does not.
	for host, want := range map[string]int{"w1": 1, "w1.i-1": 1, "w": 0, "laharl": 1} {
		rows, err := ix.List(Filter{Host: host})
		if err != nil || len(rows) != want {
			t.Errorf("--host %s: %d rows, want %d (%v)", host, len(rows), want, err)
		}
	}
}

func TestAReusedWorkerNameDoesNotCrossLink(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	wt := "/data/worktrees/widgets/feat-a"
	r.piSession("old", "aaaa", wt, "2026-10-03T10:00:00Z", "first VM")
	r.uploadWorker("old", "w1", "i-old", mustTime(t, "2026-10-03T09:00:00Z"))
	r.piSession("new", "bbbb", wt, "2026-10-10T10:00:00Z", "second VM")
	r.uploadWorker("new", "w1", "i-new", mustTime(t, "2026-10-10T09:00:00Z"))

	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	// Only the second VM's handoff is left in the ledger: the first VM's session
	// must not take it, though the name and the path match.
	links := []Link{{ID: "h-new", Host: "w1", Worktree: wt, Brief: "second", CreatedAt: "2026-10-10T09:59:00Z"}}
	if _, err := ix.Sync(r.a, r.op, links, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	link := func(host string) any {
		rows, err := ix.List(Filter{Host: host})
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: %v %v", host, err, rows)
		}
		return rows[0]["ledger_id"]
	}
	if got := link("w1.i-old"); got != "" && got != nil {
		t.Errorf("the first VM's session linked to %v", got)
	}
	if got := link("w1.i-new"); got != "h-new" {
		t.Errorf("the second VM's session linked to %v", got)
	}

	// Only the first VM's handoff is left: the second VM's session must not take it.
	ix2 := &Index{Path: filepath.Join(r.dir, "i2.db")}
	links = []Link{{ID: "h-old", Host: "w1", Worktree: wt, Brief: "first", CreatedAt: "2026-10-03T09:59:00Z"}}
	if _, err := ix2.Sync(r.a, r.op, links, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	ix = ix2
	if got := link("w1.i-new"); got != "" && got != nil {
		t.Errorf("the second VM's session linked to %v", got)
	}
	if got := link("w1.i-old"); got != "h-old" {
		t.Errorf("the first VM's session linked to %v", got)
	}
}

func TestOnlyWorkersThisMachineUploadedAreIndexed(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	r.piSession("w1", "aaaa", "/data/x", "2026-10-03T10:00:00Z", "zebras")
	r.uploadWorker("w1", "w1", "i-1", time.Time{})
	body, _ := json.Marshal(hostClaim{Machine: "another", Kind: hostKindWorker, Worker: "w1"})
	if err := r.a.B.Delete(hostClaimKey("w1.i-1")); err != nil {
		t.Fatal(err)
	}
	if err := r.a.B.Put(hostClaimKey("w1.i-1"), body); err != nil {
		t.Fatal(err)
	}
	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	st, err := ix.Sync(r.a, r.op, nil, SyncOptions{})
	if err != nil || st.Sessions != 0 {
		t.Fatalf("%v %+v", err, st)
	}
}

func TestFailedHostsListStopsTheIndex(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	r.a.B = &flakyBackend{Backend: r.a.B, failList: func(p string) bool { return p == "hosts/" }}
	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{}); !errors.Is(err, errBlip) {
		t.Fatalf("err = %v", err)
	}
}

func TestClaimWorker(t *testing.T) {
	r := newRig(t)
	a := r.a.forHost("w1.i-1")
	info := WorkerInfo{Name: "w1", Provider: "aws", InstanceID: "i-1", Created: mustTime(t, "2026-10-03T09:00:00Z")}

	// A plain claim from this machine, left by a hand-run recovery, is upgraded.
	if err := a.claimHost(false); err != nil {
		t.Fatal(err)
	}
	if err := a.ClaimWorker(info); err != nil {
		t.Fatal(err)
	}
	cl, err := a.hostClaims()
	if err != nil {
		t.Fatal(err)
	}
	got := cl["w1.i-1"]
	if got.Kind != hostKindWorker || got.Worker != "w1" || got.InstanceID != "i-1" || got.Provider != "aws" || got.InstanceCreated != "2026-10-03T09:00:00Z" {
		t.Errorf("%+v", got)
	}
	// Again is fine, and the plain check still accepts it.
	if err := a.ClaimWorker(info); err != nil {
		t.Fatal(err)
	}
	if err := a.claimHost(false); err != nil {
		t.Fatal(err)
	}

	// Another machine's claim is refused and left alone.
	other := r.a.forHost("w2.i-2")
	body, _ := json.Marshal(hostClaim{Machine: "another", Hostname: "elsewhere"})
	if err := r.a.B.Put(hostClaimKey(other.Host), body); err != nil {
		t.Fatal(err)
	}
	err = other.ClaimWorker(WorkerInfo{Name: "w2", InstanceID: "i-2"})
	if err == nil || !strings.Contains(err.Error(), "another machine") {
		t.Fatalf("err = %v", err)
	}
	raw, err := r.a.B.Get(hostClaimKey(other.Host))
	if err != nil || string(raw) != string(body) {
		t.Errorf("claim changed: %s %v", raw, err)
	}
}

func TestRunFinalTakesTheUnterminatedLastLine(t *testing.T) {
	r := newRig(t)
	p := filepath.Join(r.pi, "--x--", "2026_s.jsonl")
	r.write(p, "{\"a\":1}\n{\"partial", false)
	res, err := r.a.Run(r.sources())
	if err != nil {
		t.Fatal(err)
	}
	if res.RawBytes != 8 {
		t.Fatalf("Run copied %d bytes, want the 8 up to the newline", res.RawBytes)
	}
	res, err = r.a.RunFinal(r.sources())
	if err != nil {
		t.Fatal(err)
	}
	if res.RawBytes != 9 {
		t.Fatalf("RunFinal copied %d bytes, want the 9 of the last line", res.RawBytes)
	}
	vr, err := r.a.Verify(r.op, r.sources())
	if err != nil || vr.Matched != 1 || vr.Behind != 0 {
		t.Fatalf("%v %+v", err, vr)
	}
}

func TestVerifyDoesNotCountTombstonesOrEmptyFilesAsMissing(t *testing.T) {
	r := newRig(t)
	r.write(filepath.Join(r.pi, "--x--", "2026_gone.jsonl"), "{\"a\":1}\n", false)
	r.write(filepath.Join(r.pi, "--x--", "2026_empty.jsonl"), "", false)
	r.now = r.now.Add(time.Hour)
	if _, err := r.a.Run(r.sources()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.DeleteSession("pi", "--x--", "2026_gone"); err != nil {
		t.Fatal(err)
	}
	r.write(filepath.Join(r.pi, "--x--", "2026_new.jsonl"), "{\"a\":1}\n", false)
	vr, err := r.a.Verify(r.op, r.sources())
	if err != nil {
		t.Fatal(err)
	}
	if vr.Tombstoned != 1 || vr.Empty != 1 || vr.Missing != 1 {
		t.Errorf("%+v", vr)
	}
	// A blip reading the tombstones is an error, not a pass.
	r.a.B = &flakyBackend{Backend: r.a.B, failList: failTombs}
	if _, err := r.a.Verify(r.op, r.sources()); !errors.Is(err, errBlip) {
		t.Errorf("err = %v", err)
	}
}
