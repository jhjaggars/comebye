package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// kvWorker makes kv1 in a fake world as the cluster would leave it: the VM
// has a uid and a creation time, the boot claim is Bound and the data disk is
// still being prepared.
func kvWorker(t *testing.T, w *liveWorld) *env {
	t.Helper()
	e, _ := liveEnv(t, w)
	ctx := context.Background()
	p := e.prov()
	if err := p.EnsureDataDisk(ctx, "kv1", func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if err := p.Create(ctx, "kv1", fakePub); err != nil {
		t.Fatal(err)
	}
	md := w.objs["vm/kv1"]["metadata"].(map[string]any)
	md["uid"] = "6f1c2d3e-0000-4000-8000-00000000abcd"
	md["creationTimestamp"] = "2026-11-20T10:00:00Z"
	w.objs["pvc/kv1-boot"] = map[string]any{"metadata": map[string]any{"name": "kv1-boot"},
		"spec":   map[string]any{"storageClassName": "longhorn"},
		"status": map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": "40Gi"}}}
	w.objs["pvc/kv1-data"] = map[string]any{"metadata": map[string]any{"name": "kv1-data"},
		"spec":   map[string]any{"storageClassName": "longhorn"},
		"status": map[string]any{"phase": "Pending"}}
	w.objs["datavolume/kv1-data"]["status"] = map[string]any{"phase": "ImportInProgress", "progress": "12.5%"}
	return e
}

// An idle poweroff leaves a RerunOnFailure VM Stopped: status says so, names
// the archive host by the VM's uid (which survives stop and start) and shows
// both disks, with the data disk marked as kept.
func TestKubeVirtStatusStoppedShowsDisksAndArchiveHost(t *testing.T) {
	w := newLiveWorld()
	e := kvWorker(t, w)
	if !strings.Contains(w.stdins[2], `"runStrategy":"RerunOnFailure"`) {
		t.Fatalf("the VM is not RerunOnFailure, so an idle poweroff would be undone: %s", w.stdins[2])
	}
	w.vmState = "Stopped"
	var out strings.Builder
	e.Out = &out
	if err := e.status(context.Background(), "kv1"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"kubevirt    STOPPED",
		"boot disk   kv1-boot: claim Bound, 40Gi on longhorn (deleted with the VM)",
		"data disk   kv1-data: claim Pending on longhorn, DataVolume ImportInProgress 12.5% (kept by destroy unless --delete-data-disk)",
		"archive     host kv1.6f1c2d3e-0000-4000-8000-00000000abcd",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("status lacks %q:\n%s", want, s)
		}
	}
	// a disk that is gone says so, and a cluster error is shown, not hidden
	delete(w.objs, "pvc/kv1-boot")
	out.Reset()
	e.status(context.Background(), "kv1")
	if !strings.Contains(out.String(), "kv1-boot: no claim") {
		t.Errorf("missing boot claim:\n%s", out.String())
	}
	if l := e.prov().(*kubevirtProv).diskState(context.Background(), "kv1-data"); !strings.Contains(l, "Pending") {
		t.Errorf("%q", l)
	}
	w.fail = true
	if l := e.prov().(*kubevirtProv).diskState(context.Background(), "kv1-data"); !strings.Contains(l, "Unable to connect") {
		t.Errorf("a cluster error read as %q", l)
	}
}

// status --json: the instance id is the VM's uid, created comes from the VM,
// the node is the one it is pinned to, and there is no price mode.
func TestKubeVirtStatusJSON(t *testing.T) {
	w := newLiveWorld()
	e := kvWorker(t, w)
	w.vmState = "Stopped"
	for _, name := range []string{"kv1", ""} {
		var out strings.Builder
		e.Out = &out
		if err := e.statusJSON(context.Background(), name); err != nil {
			t.Fatal(err)
		}
		var d statusDoc
		if err := json.Unmarshal([]byte(out.String()), &d); err != nil {
			t.Fatalf("%v %s", err, out.String())
		}
		if d.Provider != "kubevirt" || len(d.Workers) != 1 {
			t.Fatalf("%q: %s", name, out.String())
		}
		got := d.Workers[0]
		if got.ID != "6f1c2d3e-0000-4000-8000-00000000abcd" || got.State != stateStopped || got.Mode != "" || !got.Managed ||
			got.CreatedAt == nil || *got.CreatedAt != "2026-11-20T10:00:00Z" || !strings.Contains(got.Type, "32Gi") {
			t.Errorf("%q: %+v", name, got)
		}
		if got.Node != "rozalin.x.cc" { // a stopped VM's node is its pin, for tools/ci's heat check
			t.Errorf("%q: node %q, want the pinned rozalin.x.cc", name, got.Node)
		}
		if strings.Contains(out.String(), `"mode"`) {
			t.Errorf("a KubeVirt worker has no price mode: %s", out.String())
		}
	}
}

// destroy of an idle-stopped VM refuses before anything is changed: its
// sessions are on its disk and the archive needs it running.
func TestKubeVirtDestroyStoppedRefusesWithNothingChanged(t *testing.T) {
	w := newLiveWorld()
	e := kvWorker(t, w)
	w.vmState = "Stopped"
	r := e.Run.(*fakeRunner)
	before := len(r.calls)
	err := e.destroy(context.Background(), "kv1", destroyOpts{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") || !strings.Contains(err.Error(), "worker start kv1") {
		t.Fatalf("destroy of a stopped VM: %v", err)
	}
	for _, c := range r.calls[before:] {
		if l := c.line(); strings.Contains(l, " delete ") || strings.Contains(l, " patch ") || strings.Contains(l, " apply ") {
			t.Errorf("destroy changed the cluster before refusing: %s", l)
		}
	}
	for _, k := range []string{"vm/kv1", "secret/kv1-cloudinit", "datavolume/kv1-data"} {
		if w.objs[k] == nil {
			t.Errorf("%s is gone", k)
		}
	}
}
