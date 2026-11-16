package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/scratch"
)

func (e *restartEnv) bulk(t *testing.T, path, body string) (int, bulkResult) {
	t.Helper()
	w := do(e.s, "POST", path, userHdr, body)
	var r bulkResult
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	return w.Code, r
}

func rowOf(r bulkResult, pane string) bulkRow {
	for _, row := range r.Results {
		if row.PaneID == pane {
			return row
		}
	}
	return bulkRow{}
}

// bulkServer runs: an eligible pi (w1), a working pi (w2), a pi coordinator
// (w3), a pi with a dialog open (w4), a second eligible pi (w5) and a claude
// (w6, another kind).
func bulkServer(t *testing.T) *restartEnv {
	sess := piSession(t)
	mk := func(name, pane, kind string, st herdr.AgentStatus) herdr.AgentInfo {
		return herdr.AgentInfo{Name: name, PaneID: pane, Agent: kind, Status: st, CWD: "/", AgentSession: sess}
	}
	e := restartServer(t,
		mk("one", "w1:p1", "pi", herdr.StatusIdle),
		mk("busy", "w2:p1", "pi", herdr.StatusWorking),
		mk("boss", "w3:p1", "pi", herdr.StatusIdle),
		mk("asking", "w4:p1", "pi", herdr.StatusIdle),
		mk("five", "w5:p1", "pi", herdr.StatusIdle),
		mk("other", "w6:p1", "claude", herdr.StatusIdle),
	)
	if err := e.s.roles.set("desk/w3:p1", Role{Role: RoleCoordinator, Name: "boss", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	showScreen(e.fh, "w4:p1", piGateScreen)
	return e
}

func TestRestartBulkMixedAgents(t *testing.T) {
	e := bulkServer(t)
	code, r := e.bulk(t, "/api/restart/bulk", `{"kind":"pi"}`)
	if code != 200 {
		t.Fatalf("bulk = %d", code)
	}
	if r.Host != "desk" || r.Kind != "pi" || len(r.Results) != 5 || r.Restarted != 2 || r.Refused != 2 || r.Skipped != 1 || r.Failed != 0 {
		t.Fatalf("totals: %+v", r)
	}
	// A refusal did not stop the agent after it.
	for _, pane := range []string{"w1:p1", "w5:p1"} {
		row := rowOf(r, pane)
		if row.Outcome != bulkRestarted || row.OldPID == 0 || row.NewPID == 0 || row.OldPID == row.NewPID || row.FlagsRecovered == nil {
			t.Fatalf("%s: %+v", pane, row)
		}
		// pi hides its flags: the session alone came back.
		if *row.FlagsRecovered {
			t.Fatalf("%s: pi flags cannot be recovered: %+v", pane, row)
		}
	}
	if row := rowOf(r, "w2:p1"); row.Outcome != bulkRefused || row.Code != "agent_working" || row.Message == "" {
		t.Fatalf("working: %+v", row)
	}
	if row := rowOf(r, "w4:p1"); row.Outcome != bulkRefused || row.Code != "dialog_open" {
		t.Fatalf("dialog: %+v", row)
	}
	if row := rowOf(r, "w3:p1"); row.Outcome != bulkSkipped || row.Code != "coordinator" {
		t.Fatalf("coordinator: %+v", row)
	}
	if row := rowOf(r, "w6:p1"); row.Agent != "" {
		t.Fatalf("another kind was listed: %+v", row)
	}
	// Only the two eligible agents were ended (pi: ctrl+d) and started.
	e.fh.mu.Lock()
	exits, starts := len(e.rf.exits), len(e.rf.starts)
	e.fh.mu.Unlock()
	if exits != 2 || starts != 2 {
		t.Fatalf("exits %d, starts %d", exits, starts)
	}
}

func TestRestartBulkDryRunChangesNothing(t *testing.T) {
	e := bulkServer(t)
	// The host-qualified route, with the host in the path.
	code, r := e.bulk(t, "/api/hosts/desk/restart/bulk", `{"kind":"pi","dry_run":true}`)
	if code != 200 || !r.DryRun {
		t.Fatalf("dry run = %d %+v", code, r)
	}
	if r.WouldRestart != 2 || r.Refused != 2 || r.Skipped != 1 || r.Restarted != 0 {
		t.Fatalf("totals: %+v", r)
	}
	if row := rowOf(r, "w1:p1"); row.Outcome != bulkWouldRestart || row.OldPID == 0 || row.NewPID != 0 {
		t.Fatalf("would restart: %+v", row)
	}
	e.closedNothing(t)
	if n := e.fh.call("agent.prompt"); n != nil {
		t.Fatalf("a dry run prompted an agent: %v", n)
	}
	if n := e.fh.call("agent.start"); n != nil {
		t.Fatalf("a dry run started an agent")
	}
}

func TestRestartBulkRequestErrors(t *testing.T) {
	e := bulkServer(t)
	for body, want := range map[string]int{`{}`: 400, `{"kind":"codex"}`: 400, `{"kind":"pi","host":"nowhere"}`: 404, `nope`: 400} {
		if code, _ := e.bulk(t, "/api/restart/bulk", body); code != want {
			t.Errorf("%s = %d, want %d", body, code, want)
		}
	}
	if code, r := e.bulk(t, "/api/hosts/nowhere/restart/bulk", `{"kind":"pi"}`); code != 404 {
		t.Errorf("unknown host = %d %+v", code, r)
	}
	e.closedNothing(t)
}

func TestRestartBulkIsTheUsersAlone(t *testing.T) {
	e := bulkServer(t)
	for _, path := range []string{"/api/restart/bulk", "/api/hosts/desk/restart/bulk"} {
		w := do(e.s, "POST", path, agentHdr("w1:p1"), `{"kind":"pi"}`)
		if w.Code != 403 {
			t.Errorf("%s with an agent token = %d %s", path, w.Code, w.Body)
		}
	}
	e.closedNothing(t)
}

func TestRestartBulkFailureDoesNotStopTheOthers(t *testing.T) {
	e := bulkServer(t)
	// The first start fails; the pane is left at its shell and the next
	// agents are still restarted.
	e.fh.set(func(f *fakeLaunchHerdr) { e.rf.startFail = 1 })
	_, r := e.bulk(t, "/api/restart/bulk", `{"kind":"pi"}`)
	if r.Failed != 1 || r.Restarted != 1 {
		t.Fatalf("totals: %+v", r)
	}
	row := rowOf(r, "w1:p1")
	if row.Outcome != bulkFailed || row.Code != "start_failed" {
		t.Fatalf("failed: %+v", row)
	}
}

// claudeScreens are claude's idle screen without background work and its
// "stop tasks" dialog, from captures of a real claude (claude_footer_shell
// has "· 1 shell" in its footer).
func claudeScreens(t *testing.T) (idle, withShell, dialog string) {
	withShell = capture(t, "claude_footer_shell.txt")
	idle = strings.Replace(withShell, " · 1 shell", "", 1)
	if idle == withShell {
		t.Fatal("the capture has no shell in its footer")
	}
	return idle, withShell, capture(t, "claude_exit_stop_tasks.txt")
}

func claudeRestartServer(t *testing.T) *restartEnv {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := t.TempDir()
	const sid = "c3eb8f54-0000-4000-8000-000000000009"
	proj := filepath.Join(cfg, "projects", scratch.ProjectDir(repo))
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, sid+".jsonl"), []byte("{}\n"), 0o644)
	return restartServer(t, herdr.AgentInfo{Name: "c", PaneID: "w1:p1", Agent: "claude", Status: herdr.StatusIdle, CWD: repo,
		AgentSession: &herdr.AgentSessionInfo{Agent: "claude", Kind: "id", Value: sid}})
}

func TestScreenBackgroundTask(t *testing.T) {
	idle, withShell, dialog := claudeScreens(t)
	if got := screenBackgroundTask(idle); got != "" {
		t.Errorf("idle screen: %q", got)
	}
	if got := screenBackgroundTask(withShell); got != "1 shell" {
		t.Errorf("footer with a shell: %q", got)
	}
	if !exitDialogOpen(dialog) || exitDialogOpen(withShell) || exitDialogOpen(idle) {
		t.Error("exit dialog detection")
	}
	// A turn's summary above the footer does not count once the shell ended.
	stale := "✻ Churned for 2s · 1 shell still running\n\n❯ \n   Sonnet 5.5 | scratch\n  -- INSERT -- ⏵⏵ auto mode on\n"
	if got := screenBackgroundTask(stale); got != "" {
		t.Errorf("stale summary: %q", got)
	}
}

func TestRestartRefusesAClaudeWhoseFooterShowsAShell(t *testing.T) {
	_, withShell, _ := claudeScreens(t)
	e := claudeRestartServer(t)
	showScreen(e.fh, "w1:p1", withShell)
	code, r := e.post(t, `{"agent":"w1:p1"}`)
	if code != 409 || r["code"] != "background_task" || !strings.Contains(r["message"].(string), "1 shell") {
		t.Fatalf("restart = %d %v", code, r)
	}
	e.closedNothing(t)
}

func TestRestartCancelsClaudesExitDialog(t *testing.T) {
	idle, _, dialog := claudeScreens(t)
	e := claudeRestartServer(t)
	showScreen(e.fh, "w1:p1", idle)
	e.rf.exitDialog, e.rf.clean = dialog, idle
	code, r := e.post(t, `{"agent":"w1:p1"}`)
	if code != 409 || r["code"] != "background_task" || !strings.Contains(r["message"].(string), "nothing was closed") {
		t.Fatalf("restart = %d %v", code, r)
	}
	e.fh.mu.Lock()
	defer e.fh.mu.Unlock()
	if e.rf.escs != 1 {
		t.Fatalf("Escape sent %d times, want once", e.rf.escs)
	}
	if exitDialogOpen(e.fh.screens["w1:p1"]) {
		t.Fatal("the exit dialog was left open")
	}
	// Never a confirming key; nothing ended or started; claude still listed.
	if len(e.rf.exits) != 0 || len(e.rf.starts) != 0 || len(e.fh.agents) != 1 {
		t.Fatalf("exits %v, starts %d, agents %d", e.rf.exits, len(e.rf.starts), len(e.fh.agents))
	}
}
