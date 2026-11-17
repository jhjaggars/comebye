// Mocked transcripts for toolruns.mjs: a long claude one and a pi one.
export const T = (id, tool, args, result, extra) => ({ id, kind: "tool", tool, args, result, ms: 40, ...extra });
export const U = (id, text) => ({ id, kind: "user", text, time: "2026-11-17T10:00:00Z" });
export const A = (id, text) => ({ id, kind: "assistant", text, time: "2026-11-17T10:00:30Z" });

// A long claude transcript: a run of 10 calls with one failure in the middle, a short run whose last call failed, a
// run with a call still running, and a subagent card between two runs.
export const claude = [
  U("u1", "Fix the flaky upgrade test and tell me what you changed."),
  { ...A("a0", "On it. I'll start by reading the test."), think: "The test sleeps for a fixed 50ms.\n\nI should read it first.", think_ms: 12400 },
  T("c1", "Read", "internal/upgrade/upgrade_test.go", "package upgrade\n..."),
  T("c2", "Grep", "flaky|Sleep in internal/upgrade", "upgrade_test.go:44: time.Sleep(50 * time.Millisecond)"),
  T("c3", "Bash", "go test ./internal/upgrade/ -run Flaky -count=20", "--- FAIL: TestFlaky (0.05s)\n    upgrade_test.go:51: timeout", { err: true }),
  T("c4", "Read", "internal/upgrade/upgrade.go", "package upgrade\n..."),
  T("c5", "Edit", "internal/upgrade/upgrade_test.go", "The file has been updated."),
  T("c6", "Edit", "internal/upgrade/upgrade.go", "The file has been updated."),
  T("c7", "Bash", "go test ./internal/upgrade/ -count=20", "ok  \tgithub.com/x/upgrade\t1.2s"),
  T("c8", "Bash", "go vet ./...", "ok"),
  T("c9", "Read", "CHANGELOG.md", "# Changelog"),
  T("c10", "WebSearch", "go testing flaky timer", "10 results"),
  { ...A("a1", "Fixed: the test slept instead of waiting for the clock."), think: "Nothing to add." },
  U("u2", "Now run the whole suite."),
  T("d1", "Bash", "go test ./... 2>&1 | tail -20", "ok  \tgithub.com/x/a\n--- FAIL: TestRestart (0.3s)\n    restart_test.go:88: exit status 2: no such file\nFAIL", { err: true }),
  T("d2", "Bash", "go test ./internal/restart/ -v", "restart_test.go:88: exit status 2: no such file or directory\n--- FAIL: TestRestart", { err: true }),
  { id: "s1", kind: "tool", tool: "Agent", args: "review the diff", result: "Looks fine.", ms: 9000, subagent: { agent_id: "ag1", type: "reviewer", description: "Review the diff", status: "completed", report: "Looks fine.\n\nNo **blocking** issues.", dur_ms: 9000, tool_uses: 3 } },
  T("e1", "Read", "web/app.js", "// app"),
  T("e2", "Bash", "go build ./...", "", { ms: 0, result: "" }),
];
export const pi = [
  U("pu1", "List the failing checks."),
  T("p1", "bash", "gh run list --limit 5", "completed\tfailure\tCI"),
  T("p2", "read", "web/app.js", "// app"),
  T("p3", "grep", "toolcard in web", "web/index.html:455"),
  T("p4", "find", "*.mjs in tools/uitest", "tools/uitest/a.mjs"),
  T("p5", "edit", "web/app.js", "ok"),
  T("p6", "write", "notes.md", "ok"),
  T("p7", "ls", "web", "app.js"),
  A("pa1", "Done."),
];


// A run of edits: an Edit with unchanged lines around the change, a MultiEdit, a Write of a new file, and an Edit
// the server cut.
const body = (n) => Array.from({ length: n }, (_, i) => `line ${i + 1} of the file`).join("\n");
export const edits = [
  U("eu1", "Make the timer wait on the clock."),
  T("x1", "Edit", "internal/upgrade/upgrade_test.go", "The file has been updated.", {
    edit: { path: "/home/u/code/x/internal/upgrade/upgrade_test.go", edits: [{
      old: "func TestFlaky(t *testing.T) {\n\tsetup()\n\ttime.Sleep(50 * time.Millisecond)\n\tcheck(t)\n\tteardown()\n}\n",
      new: "func TestFlaky(t *testing.T) {\n\tsetup()\n\tclock.Advance(50 * time.Millisecond) // a very long line that has to wrap on a phone because it just keeps going and going past the edge\n\tcheck(t)\n\tteardown()\n}\n",
    }] },
  }),
  T("x2", "MultiEdit", "web/app.js", "Applied 2 edits.", {
    edit: { path: "/home/u/code/x/web/app.js", edits: [{ old: "let a = 1;", new: "let a = 2;" }, { old: "const b = 'x';\n", new: "", all: true }] },
  }),
  T("x3", "Write", "docs/notes.md", "File created.", { edit: { path: "/home/u/code/x/docs/notes.md", write: true, edits: [{ new: body(20) + "\n" }] } }),
  T("x4", "Edit", "big.txt", "ok", { edit: { path: "/home/u/code/x/big.txt", truncated: true, edits: [{ old: "aaa\nbbb", new: "aaa\nccc" }] } }),
  T("x5", "Bash", "go test ./...", "ok"),
  A("ea1", "Done."),
];
