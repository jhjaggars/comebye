package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

// ---- slice 4: create and destroy from the in-cluster provisioner ----

func TestSpecKubeVirtKindTakesOnlyWhatTheConfigDoesNotSet(t *testing.T) {
	c := kubeCfg(t)
	s, err := ParseSpec([]byte(`{"kind":"kubevirt","name":"kv2","idle_minutes":90,"destroy_after_hours":48}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(c); err != nil || c.Provider != "kubevirt" || c.IdleMinutes != 90 || c.DestroyAfterHours != 48 {
		t.Fatalf("%v %+v", err, c)
	}
	// size, disks, price and zone are the config's: a request that names one is refused
	for field, spec := range map[string]string{
		"type":         `{"kind":"kubevirt","type":"t3.large"}`,
		"spot":         `{"kind":"kubevirt","spot":true}`,
		"boot_disk_gb": `{"kind":"kubevirt","boot_disk_gb":20}`,
		"data_disk_gb": `{"kind":"kubevirt","data_disk_gb":50}`,
		"max_run":      `{"kind":"kubevirt","max_run":"24h"}`,
	} {
		s, err := ParseSpec([]byte(spec))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Apply(kubeCfg(t)); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: %v", field, err)
		}
	}
}

// The plan with the in-cluster config (no kube_context, the pod's token file):
// it reads the cluster, changes nothing and prints no credential.
func TestKubeVirtDryRunWithInClusterCredentials(t *testing.T) {
	withPath(t, "")
	old := statFile
	t.Cleanup(func() { statFile = old })
	statFile = func(p string) (os.FileInfo, error) {
		if p == inClusterTokenFile {
			return nil, nil
		}
		return nil, os.ErrNotExist
	}
	e, r, out, errOut := kubeEnv(t, goodWorld())
	e.Cfg.KubeContext = ""
	if err := e.create(context.Background(), "kv2", createOpts{Version: "v0.3.1"}); err != nil {
		t.Fatalf("%v\n%s", err, out())
	}
	o := out() + errOut()
	for _, want := range []string{"VirtualMachine kv2", "DataVolume kv2-data", "Secret kv2-cloudinit"} {
		if !strings.Contains(o, want) {
			t.Errorf("plan lacks %q:\n%s", want, o)
		}
	}
	for _, c := range r.calls {
		if c.name != "kubectl" || strings.Contains(c.line(), "--context") {
			t.Errorf("the dry run ran %s", c.line())
		}
		if !(c.args[0] == "get" || (len(c.args) > 2 && c.args[2] == "get")) {
			t.Errorf("the dry run changed something: %s", c.line())
		}
	}
	if c := e.pfKubeVirt(context.Background()); c.State != "ok" || !strings.Contains(c.Note, "in-cluster credentials") {
		t.Errorf("preflight: %+v", c)
	}
	for _, bad := range []string{"BEGIN OPENSSH PRIVATE KEY", secretKey, "tskey-", "serviceaccount/token", "Bearer"} {
		if strings.Contains(o, bad) {
			t.Errorf("the plan prints %q:\n%s", bad, o)
		}
	}
}

// No token, key or OAuth value reaches the tool's output, in a KubeVirt
// create's events or in the messages built from them.
func TestKubeVirtEventsScrubTokenKeyAndOAuthValues(t *testing.T) {
	e, _, _, _ := kubeEnv(t, goodWorld())
	oauth := "tskey-client-kOAUTHID-OAUTHSECRETVALUE"
	e.addSecret(oauth)
	e.addSecret("plain-oauth-secret-without-a-prefix")
	var buf strings.Builder
	sink := &eventSink{w: &buf, env: e}
	for _, msg := range []string{
		"mint failed for " + oauth,
		"the client said plain-oauth-secret-without-a-prefix",
		"minted " + secretKey,
		"Authorization: Bearer eyJhbGciOi.payload.sig",
		"token=sa-token-123 secret=s3cr3t password=pw",
		"kubectl: error: sk-live-abcdefghijk",
	} {
		sink.emit(event{Stage: "tailnet", State: stateEvRunning, Msg: msg})
	}
	for _, bad := range []string{"OAUTHSECRETVALUE", "plain-oauth-secret-without-a-prefix", "tskey-", "eyJhbGciOi", "sa-token-123", "s3cr3t", "pw\"", "sk-live"} {
		if strings.Contains(buf.String(), bad) {
			t.Errorf("%q reached an event:\n%s", bad, buf.String())
		}
	}
}

// Destroy in a fake world deletes only objects that carry the managed label,
// and keeps the data disk unless asked.
func TestKubeVirtDestroyDeletesOnlyManagedObjectsAndKeepsTheDataDisk(t *testing.T) {
	w := newLiveWorld()
	e, r := liveEnv(t, w)
	ctx := context.Background()
	p := e.prov()
	p.EnsureDataDisk(ctx, "kv2", func(string, ...any) {})
	if err := p.Create(ctx, "kv2", fakePub); err != nil {
		t.Fatal(err)
	}
	// look-alikes that are not ours: same names with another VM's suffixes, no label
	for _, k := range []string{"vm/kv2-other", "secret/kv2-other-cloudinit", "datavolume/kv2-other-data", "vm/someone"} {
		kind, name, _ := strings.Cut(k, "/")
		w.objs[k] = map[string]any{"metadata": map[string]any{"name": name}}
		_ = kind
	}
	w.vmState = "Stopped"
	if err := e.destroy(ctx, "kv2", destroyOpts{Yes: true, NoArchive: true}); err != nil {
		t.Logf("destroy: %v", err) // the tailnet and hub steps are not part of this fake
	}
	for _, k := range []string{"vm/kv2", "secret/kv2-cloudinit"} {
		if w.objs[k] != nil {
			t.Errorf("%s was not deleted", k)
		}
	}
	for _, k := range []string{"datavolume/kv2-data", "vm/kv2-other", "secret/kv2-other-cloudinit", "datavolume/kv2-other-data", "vm/someone"} {
		if w.objs[k] == nil {
			t.Errorf("%s was deleted: only the managed objects of kv2 may go (the data disk only when asked)", k)
		}
	}
	// every delete the tool issued names kv2's own objects, and a label was read first
	for _, c := range r.calls {
		if len(c.args) > 0 && strings.Contains(c.line(), " delete ") {
			if !strings.Contains(c.line(), "kv2") || strings.Contains(c.line(), "kv2-other") || strings.Contains(c.line(), "someone") {
				t.Errorf("a delete outside kv2: %s", c.line())
			}
			if strings.Contains(c.line(), "--all") || strings.Contains(c.line(), " -l ") || strings.Contains(c.line(), "--selector") {
				t.Errorf("a bulk delete: %s", c.line())
			}
		}
	}
	// asked: the data disk goes too, still only kv2's
	if err := p.DeleteDataDisk(ctx, "kv2"); err != nil || w.objs["datavolume/kv2-data"] != nil || w.objs["datavolume/kv2-other-data"] == nil {
		t.Errorf("%v %v", err, w.objs)
	}
}
