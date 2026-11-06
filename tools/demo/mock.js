/* The fake backend for the Comebye tour. It answers every /api route the web
   app calls, from invented data, with a little state: nothing leaves the page.
   Loaded before app.js. All names, repos and transcripts here are made up. */
(function () {
  "use strict";
  const MIN = 60000;
  const VERSION = "demo";
  const LOCAL = "studio";
  const ago = (m) => new Date(Date.now() - m * MIN).toISOString();
  const now = () => new Date().toISOString();
  const later = (ms, fn) => { const gen = S.gen; setTimeout(() => { if (gen === S.gen) fn(); }, ms); };
  let S = { gen: 0 };

  /* ---------- browser features a demo page cannot have ---------- */
  try { delete Navigator.prototype.serviceWorker; } catch (e) {}
  // The viewer's frame refuses dialogs. Everything here is fake, so a question
  // the app would ask ("Drop this item?") is answered yes.
  window.confirm = () => true;
  window.prompt = (msg, cur) => cur || "24h at 08:00";
  for (const fn of ["pushState", "replaceState"]) {
    const orig = history[fn];
    history[fn] = function () { try { return orig.apply(history, arguments); } catch (e) {} };
  }

  /* ---------- the seed ---------- */
  function agent(host, pane, name, kind, status, repo, opts = {}) {
    const linked = !!opts.branch;
    const cwd = opts.cwd || (linked ? `/home/dev/.worktrees/${repo}/${opts.branch}` : `/home/dev/code/${repo}`);
    const a = {
      id: `${host}/${pane}`, host, pane_id: pane, name, agent: kind,
      display_agent: { claude: "Claude", pi: "Pi", codex: "Codex", opencode: "OpenCode" }[kind] || kind,
      agent_status: status, cwd, last_active: ago(opts.active ?? 3), status_since: ago(opts.since ?? opts.active ?? 3),
    };
    if (repo) a.worktree = { repo, checkout: cwd, linked, ...(linked ? { branch: opts.branch } : {}) };
    return a;
  }
  const msg = (id, kind, mins, f) => ({ id, kind, time: ago(mins), ...f });
  const user = (id, mins, text) => msg(id, "user", mins, { text });
  const said = (id, mins, text, model = "sonnet") => msg(id, "assistant", mins, { text, model });
  const tool = (id, mins, name, args, result, ms = 240 + (mins * 83) % 1500, err) => msg(id, "tool", mins, { tool: name, args, result, ms, ...(err ? { err: true } : {}) });

  function seedTranscripts() {
    return {
      "studio/w1:p1": [
        user("u1", 95, "Morning. Rate limits for the public API are the priority this week. Split it up however you like, and tell me only when you need a decision."),
        said("a2", 94, "Understood. I read the tidepool issue and the current middleware. This splits into three steps:\n\n1. **rate-limit-store**: pick a store and build the limiter behind an interface\n2. **retry-webhooks**: our own webhook client has to back off when partners return 429\n3. **rollout-docs**: document the limits, once the other two are in\n\nI'll start the first two now and queue the third behind them."),
        tool("t3", 93, "Bash", "comebye plan new \"Rate limits for the public API\" --size 3", "plan p-41c0 created"),
        tool("t4", 93, "Bash", "comebye handoff --plan p-41c0 --host studio --repo ~/code/tidepool --branch rate-limit-store --brief store.md", "started rate-limit-store on studio (claude), branch rate-limit-store\nhandoff h-7c02"),
        tool("t5", 92, "Bash", "comebye handoff --plan p-41c0 --host buildbox --repo ~/code/tidepool --branch retry-webhooks --brief webhooks.md", "started retry-webhooks on buildbox (claude), branch retry-webhooks\nhandoff h-2f1a"),
        said("a6", 92, "Both are running, and **rollout-docs** is queued behind them."),
        tool("t7", 24, "Bash", "comebye read h-d440", "csv-export: finished 1 minute ago\nworktree clean, 3 commits, tests pass\nreport: streaming export with a row cap, see ledger"),
        tool("t8", 12, "Bash", "comebye todo add \"Should rate limiting be on by default in 2.4?\" --kind decision --waiting rate-limit-store", "todo td-3a1 added"),
        said("a9", 11, "One decision is on your **To do** list: whether limits ship on by default. Nothing else needs you right now.\n\n| Worker | State |\n| --- | --- |\n| retry-webhooks | running |\n| rate-limit-store | waiting on a question |\n| rollout-docs | queued behind both |"),
      ],
      "studio/w2:p1": [
        user("u1", 300, "How do we version the public API today? Short answer please."),
        said("a2", 299, "By path prefix: `/v2/...`. `v1` still serves two partners and is frozen. The router is in `internal/api/router.go`."),
      ],
      "studio/w4:p1": [
        user("u1", 92, "Build a rate limiter behind an interface in tidepool and pick the store. Work on your branch and finish with a report."),
        said("a2", 88, "The limiter is a token bucket behind a `Store` interface, with an in-memory version for tests:\n\n```go\ntype Store interface {\n\tTake(ctx context.Context, key string, n int) (Decision, error)\n}\n```\n\nThe production store is a real trade-off, so I'll ask."),
        tool("t3", 87, "Read", "internal/limit/bucket.go", "package limit\n\n// Bucket refills at Rate tokens per second, up to Burst.\ntype Bucket struct { ... }"),
        tool("t4", 86, "Bash", "go test ./internal/limit/", "ok  \texample.dev/tidepool/internal/limit\t0.214s"),
        said("a5", 14, "Tests pass with the in-memory store. I need one decision before going further."),
      ],
      "buildbox/w3:p1": [
        user("u1", 38, "Make webhook delivery back off when a partner returns 429 or 503, and honour Retry-After. Work on your branch and finish with a report."),
        said("a2", 37, "I'll start from the delivery loop.\n\n- retries today are a fixed 5 seconds\n- `Retry-After` is ignored\n- attempts are not capped per endpoint"),
        tool("t3", 36, "Read", "internal/webhooks/deliver.go", "func (d *Deliverer) attempt(ctx context.Context, ev Event) error {\n\tresp, err := d.client.Do(req)\n\t...\n\ttime.Sleep(5 * time.Second)\n}", 40),
        tool("t4", 31, "Edit", "internal/webhooks/deliver.go", "replaced the fixed sleep with backoff.Next(attempt, resp)", 25),
        tool("t5", 29, "Bash", "go test ./internal/webhooks/", "--- FAIL: TestRetryAfterSeconds (0.00s)\n    deliver_test.go:88: waited 2s, want 120s\nFAIL", 3400, true),
        said("a6", 28, "That failure is real: the header was parsed but the jitter step overwrote it. A `Retry-After` value now sets the **minimum** wait, and jitter only applies above it."),
        tool("t7", 26, "Edit", "internal/webhooks/backoff.go", "Next() keeps Retry-After as a floor", 22),
        tool("t8", 25, "Bash", "go test ./internal/webhooks/", "ok  \texample.dev/tidepool/internal/webhooks\t0.517s", 2900),
      ],
      "spot-7/w2:p1": [
        user("u1", 52, "Rebuild fernlog's search index in one pass instead of per file, and compare the speed on the large fixture."),
        said("a2", 51, "I'll split this: a subagent measures the current build while I write the new one.", "local/thinking"),
        msg("t3", "tool", 50, { tool: "Agent", args: "Measure the current index build", ms: 184000, result: "done",
          subagent: { agent_id: "sa-81f2", description: "Measure the current index build", type: "general", status: "completed", dur_ms: 184000, tool_uses: 6,
            report: "On the large fixture (41,000 log files):\n\n- **now:** 212 s, peak 1.9 GB\n- most of the time goes to reopening the writer per file",
            prompt: "Time the current index build three times on the large fixture and report the median." } }),
        tool("t4", 30, "Edit", "index/build.go", "one writer for the whole pass; files are streamed in batches of 500", 31),
        msg("t5", "tool", 4, { tool: "Bash", args: "go run ./cmd/bench -fixture large -runs 3" }),
      ],
      "buildbox/w5:p1": [
        user("u1", 70, "TestReplayOrder in fernlog fails about one run in twenty. Find out why and fix it."),
        said("a2", 66, "I reproduced it after 31 runs with `-race`. Two goroutines append to the same slice during replay. I have a fix, and I want to run the integration suite to be sure.", "local/fast"),
        tool("t3", 65, "Edit", "replay/replay.go", "guard the merge with one channel; order now comes from sequence numbers", 18),
      ],
      "studio/w6:p1": [
        user("u1", 180, "Add a CSV export to the bookings admin page. Stream it, don't build it in memory."),
        said("a2", 150, "Done. The export streams from a cursor and stops at a row cap of 500,000, with a note in the last row if it was cut off."),
        tool("t3", 149, "Bash", "go test ./internal/admin/...", "ok  \texample.dev/tidepool/internal/admin\t1.102s", 4100),
        said("a4", 148, "**Report:** three commits on `csv-export`, tests pass, worktree clean."),
      ],
      "spot-7/w3:p1": [
        user("u1", 240, "Rewrite the hero copy on the marigold site from the notes in copy.md."),
        said("a2", 230, "Done, and it fits on two lines down to 360px. The old tagline is kept in the commit message."),
      ],
      "buildbox/w7:p1": [
        user("u1", 400, "Nav links fail contrast in light mode. Fix them."),
        said("a2", 390, "Fixed: the link color was 3.1:1 and is now 5.2:1. Merged as `4be1d77`."),
      ],
      "studio/w8:p1": [
        user("u1", 2000, "Try replacing the booking cache with a materialized view. A spike only."),
        said("a2", 1990, "It works, but writes get about 30% slower. I don't think it's worth landing. Two commits are on the branch in case you want them."),
      ],
    };
  }

  function seedEntries() {
    const E = (id, repo, branch, status, f) => ({ id, host: f.host || LOCAL, repo: `/home/dev/code/${repo}`, repo_name: repo, branch, base: "main", kind: "claude", brief: f.brief || "", from: "quill", status, created_at: ago(f.age ?? 60), agent_name: branch, ...f });
    return [
      E("h-2f1a", "tidepool", "retry-webhooks", "running", { host: "buildbox", agent_id: "buildbox/w3:p1", pane_id: "w3:p1", age: 38, plan_id: "p-41c0", worktree: "/home/dev/.worktrees/tidepool/retry-webhooks" }),
      E("h-7c02", "tidepool", "rate-limit-store", "running", { agent_id: "studio/w4:p1", pane_id: "w4:p1", age: 92, plan_id: "p-41c0", worktree: "/home/dev/.worktrees/tidepool/rate-limit-store" }),
      E("h-91de", "fernlog", "flaky-replay-test", "running", { host: "buildbox", kind: "pi", agent_id: "buildbox/w5:p1", pane_id: "w5:p1", age: 70, worktree: "/home/dev/.worktrees/fernlog/flaky-replay-test" }),
      E("h-ac31", "fernlog", "search-index-v2", "running", { host: "spot-7", kind: "pi", agent_id: "spot-7/w2:p1", pane_id: "w2:p1", age: 52, worktree: "/home/dev/.worktrees/fernlog/search-index-v2" }),
      E("h-d440", "tidepool", "csv-export", "finished", { agent_id: "studio/w6:p1", pane_id: "w6:p1", age: 180, finished_at: ago(25), candidate: true, worktree: "/home/dev/.worktrees/tidepool/csv-export",
        final_report: "## CSV export\n\nThe bookings admin page has an **Export CSV** button. It streams from a database cursor, so memory stays flat.\n\n- row cap of 500,000, with a note in the last row when it cuts off\n- three commits on `csv-export`\n- `go test ./internal/admin/...` passes\n\n**Open question:** none." }),
      E("h-e512", "marigold-site", "hero-copy", "landed", { host: "spot-7", agent_id: "spot-7/w3:p1", pane_id: "w3:p1", age: 240, finished_at: ago(200), worktree: "/home/dev/.worktrees/marigold-site/hero-copy",
        land_check: { at: ago(200), ok: true, base: "origin/main", verdict: "landed", landed: ["9a31f0c2d1"] },
        final_report: "New hero copy, from `copy.md`. Fits on two lines down to 360px wide. Landed as `9a31f0c`." }),
      E("h-f622", "marigold-site", "nav-contrast", "ready_to_remove", { host: "buildbox", agent_id: "buildbox/w7:p1", pane_id: "w7:p1", age: 400, finished_at: ago(380), worktree: "/home/dev/.worktrees/marigold-site/nav-contrast",
        mark: { agent_id: "buildbox/w7:p1", host: "buildbox", pane_id: "w7:p1", name: "nav-contrast", by: "quill", reason: "merged as 4be1d77", at: ago(60), ask: { state: "safe", reason: "everything is on main and nothing is running", at: ago(59) } },
        marked_from: "finished", final_report: "Nav link contrast in light mode went from 3.1:1 to 5.2:1. Merged as `4be1d77`." }),
      E("h-0b77", "tidepool", "cache-view-spike", "ready_to_remove", { agent_id: "studio/w8:p1", pane_id: "w8:p1", age: 2100, finished_at: ago(1990), worktree: "/home/dev/.worktrees/tidepool/cache-view-spike",
        mark: { agent_id: "studio/w8:p1", host: LOCAL, pane_id: "w8:p1", name: "cache-view-spike", by: "quill", reason: "spike finished; not worth landing", at: ago(55), discard: true, ask: { state: "unsafe", reason: "two commits are on no remote", at: ago(54) } },
        marked_from: "finished", final_report: "A materialized view works, but writes get about 30% slower. Not worth landing." }),
      E("h-1c88", "tidepool", "rollout-docs", "waiting", { age: 90, agent_name: "", plan_id: "p-41c0", after: ["h-2f1a", "h-7c02"] }),
      E("h-2d99", "fernlog", "arm64-build", "failed", { host: "spot-7", age: 300, finished_at: ago(280), agent_name: "arm64-build", error: "The spot VM was reclaimed before the worker reported." }),
    ];
  }

  function seed() {
    const gen = S.gen + 1;
    S = {
      gen, nextPane: 20, seq: (S.seq || 1000) + 1000, // ids stay unique across a reset
      agents: [
        agent(LOCAL, "w1:p1", "quill", "claude", "idle", "", { cwd: "/home/dev", active: 11 }),
        agent(LOCAL, "w2:p1", "tidepool", "claude", "idle", "tidepool", { active: 300 }),
        agent(LOCAL, "w4:p1", "rate-limit-store", "claude", "blocked", "tidepool", { branch: "rate-limit-store", active: 14 }),
        agent(LOCAL, "w6:p1", "csv-export", "claude", "idle", "tidepool", { branch: "csv-export", active: 25 }),
        agent(LOCAL, "w8:p1", "cache-view-spike", "claude", "idle", "tidepool", { branch: "cache-view-spike", active: 1990 }),
        agent("buildbox", "w2:p1", "fernlog", "codex", "idle", "fernlog", { active: 130 }),
        agent("buildbox", "w3:p1", "retry-webhooks", "claude", "working", "tidepool", { branch: "retry-webhooks", active: 1, since: 38 }),
        agent("buildbox", "w5:p1", "flaky-replay-test", "pi", "blocked", "fernlog", { branch: "flaky-replay-test", active: 6 }),
        agent("buildbox", "w7:p1", "nav-contrast", "claude", "idle", "marigold-site", { branch: "nav-contrast", active: 380 }),
        agent("spot-7", "w2:p1", "search-index-v2", "pi", "working", "fernlog", { branch: "search-index-v2", active: 1, since: 52 }),
        agent("spot-7", "w3:p1", "hero-copy", "claude", "idle", "marigold-site", { branch: "hero-copy", active: 200 }),
      ],
      transcripts: seedTranscripts(),
      entries: seedEntries(),
      marks: [],
      roles: { "studio/w1:p1": { role: "coordinator", name: "quill", by: "the user", at: ago(100) } },
      plans: [{ id: "p-41c0", title: "Rate limits for the public API", size: 3, by: "quill", created_at: ago(93) }],
      dialogs: {
        "studio/w4:p1": { kind: "claude_question", source: "screen", agent: "claude", header: "Store", question: "Which store should the rate limiter use in production?",
          options: [
            { label: "Redis", description: "fast and already deployed; limits are lost on a flush" },
            { label: "Postgres", description: "no new parts; adds a write on every request" },
            { label: "In-process", description: "simplest; each replica counts on its own" },
            { label: "Type something.", role: "other" },
          ], selected: 0, keys: "arrows", answerable: true, cancel_label: "Cancel", fingerprint: "fp-store" },
        "buildbox/w5:p1": { kind: "pi_gate", source: "screen", agent: "pi", title: "Allow this command?", detail: "make integration TEST_FILTER=replay",
          options: [{ label: "Allow once" }, { label: "Allow for this session" }, { label: "Block" }], selected: 0, keys: "arrows", answerable: true, cancel_label: "Block", fingerprint: "fp-gate" },
      },
      todos: [
        { id: "td-3a1", kind: "decision", state: "open", question: "Should rate limiting be on by default in 2.4?", created_at: ago(12), updated_at: ago(12), created_by: "quill",
          waiting: { id: "studio/w4:p1", name: "rate-limit-store" },
          context: "Two options:\n\n1. **On by default:** protects us from day one, but partners with bursty imports may see 429s after upgrading.\n2. **Off, opt-in per key:** no surprises, but most keys will never turn it on.\n\nI'd go with on by default at a high limit (600 requests a minute), since only one partner exceeded that in the last month.",
          links: [{ label: "Traffic by key, last 30 days", url: "https://example.com/tidepool/traffic" }] },
        { id: "td-5c7", kind: "action", state: "open", question: "Rotate the staging webhook signing key", created_at: ago(44), updated_at: ago(44), created_by: "retry-webhooks",
          context: "The integration tests need the new key format. Only you have access to the staging secrets. When it's done, say so here and the worker will re-run them." },
        { id: "td-8e2", kind: "followup", state: "open", question: "CSV export: add a time zone option later?", created_at: ago(140), updated_at: ago(140), created_by: "csv-export",
          context: "Timestamps are in UTC for now. Someone in support asked for local time. Not needed to land this." },
        { id: "td-9b4", kind: "decision", state: "answered", question: "Keep the v1 API alive through Q3?", created_at: ago(500), updated_at: ago(8), created_by: "quill",
          answer: { text: "Yes, through Q3. Tell the two partners by Friday.", by: "you", via: "app", at: ago(8) } },
      ],
      brief: { markdown: "## tidepool\n- **retry-webhooks** is running: backoff for webhook delivery.\n- **rate-limit-store** is waiting on your answer about the store.\n- **csv-export** finished and is ready for you to look at.\n\n## fernlog\n- **search-index-v2** is measuring the new index build.\n- **arm64-build** failed: the spot VM was reclaimed. I'll retry on buildbox.", by: "quill", at: ago(45) },
      briefHistory: [
        { markdown: "## overnight\n- **hero-copy** landed on marigold-site.\n- Nothing is waiting on you.", by: "quill", at: ago(800) },
      ],
      schedule: { enabled: true, every: "24h", at: "08:00", tz: "UTC", state: "ok", updated: ago(5000) },
    };
    startScripts();
  }

  /* ---------- live changes ---------- */
  const find = (id) => S.agents.find((a) => a.id === id);
  const tx = (id) => (S.transcripts[id] = S.transcripts[id] || []);
  const nextId = (p) => p + (++S.seq);
  function setStatus(a, status) {
    if (a.agent_status !== status) a.status_since = now();
    a.agent_status = status;
    a.last_active = now();
  }
  function push(id, m) { tx(id).push({ id: nextId(m.kind[0]), time: now(), ...m }); const a = find(id); if (a) a.last_active = now(); }

  const LIVE = [
    ["Read", "internal/webhooks/deliver_test.go", "func TestRetryAfterSeconds(t *testing.T) { ... }\nfunc TestRetryAfterDate(t *testing.T) { ... }"],
    ["Edit", "internal/webhooks/deliver_test.go", "added TestBackoffCap and TestPerEndpointAttempts"],
    ["Bash", "go test -race ./internal/webhooks/", "ok  \texample.dev/tidepool/internal/webhooks\t1.904s"],
    ["Bash", "git commit -m \"webhooks: back off on 429 and 503, honour Retry-After\"", "[retry-webhooks 61ac0e2] webhooks: back off on 429 and 503, honour Retry-After\n 3 files changed, 148 insertions(+), 22 deletions(-)"],
  ];
  // retry-webhooks keeps working for as long as the page is open: a few steps,
  // then a long integration run that does not end, so the tour always finds it busy.
  function startScripts() {
    const id = "buildbox/w3:p1";
    let i = 0;
    const step = () => {
      const a = find(id);
      if (!a || a.agent_status !== "working") return;
      if (i >= LIVE.length) {
        push(id, { kind: "tool", tool: "Bash", args: "make integration" });
        broadcast();
        return;
      }
      const [name, args, result] = LIVE[i++];
      const m = { kind: "tool", tool: name, args };
      push(id, m);
      broadcast();
      later(3500, () => {
        const t = tx(id).filter((x) => x.kind === "tool").pop();
        t.result = result; t.ms = 3400;
        later(8000, step);
      });
    };
    later(6000, step);
  }

  const CANNED = [
    (t) => `I read that as: *${t}*. I'd start with the code nearest the change, keep it on this branch, and run the tests before reporting back.\n\nThis is a canned reply: no real agent is behind this demo. On a real install your words go straight into this agent's terminal pane, and you can see them there too.`,
    (t) => `Noted: *${t}*.\n\n- first I'll check what the tests cover today\n- then make the smallest change that does it\n\n(Canned reply. The real thing would now go and do the work.)`,
    (t) => `On it. For now this demo only pretends, so here is what you would see: tool cards as I work, then my answer here. You asked: *${t}*`,
  ];
  function reply(id, text) {
    const a = find(id);
    if (!a) return;
    const t = text.replace(/\s+/g, " ").trim().slice(0, 140);
    const stay = id === "buildbox/w3:p1"; // this one is busy with its own script
    if (a.agent === "codex") {
      S.terminal = S.terminal || {};
      S.terminal[id] = (S.terminal[id] || TERMINAL) + `\n> ${t}\n\n  Working on it. (Canned output: no real agent is behind this demo.)\n`;
      return;
    }
    push(id, { kind: "user", text });
    setStatus(a, "working");
    broadcast();
    later(1300, () => {
      push(id, { kind: "tool", tool: "Read", args: a.worktree ? "README.md" : "~/notes.md" });
      broadcast();
      later(1500, () => {
        const m = tx(id).filter((x) => x.kind === "tool").pop();
        m.result = "(a file the agent would read)"; m.ms = 60;
        later(900, () => {
          push(id, { kind: "assistant", text: CANNED[S.seq % CANNED.length](t), model: a.agent === "pi" ? "local/thinking" : "sonnet" });
          if (!stay) setStatus(a, "idle");
          broadcast();
        });
      });
    });
  }

  function finishWorker(id, text, report) {
    const a = find(id);
    push(id, { kind: "assistant", text, model: a && a.agent === "pi" ? "local/fast" : "sonnet" });
    if (a) setStatus(a, "idle");
    const e = S.entries.find((x) => x.agent_id === id && x.status === "running");
    if (e) { e.status = "finished"; e.finished_at = now(); e.final_report = report; }
    broadcast();
  }

  /* ---------- derived data ---------- */
  function hostsNow() {
    return ["studio", "buildbox", "spot-7"].map((name) => ({ name, ...(name === LOCAL ? { local: true } : { addr: "100.64.0." + (name === "buildbox" ? 12 : 37) + ":8443" }), status: "ok", version: VERSION, agents: S.agents.filter((a) => a.host === name).length }));
  }
  const agentsPayload = () => ({ agents: S.agents, hosts: hostsNow(), version: VERSION });
  function plansNow() {
    return S.plans.map((p) => {
      const members = S.entries.filter((e) => e.plan_id === p.id).map((e) => ({
        id: e.id, repo_name: e.repo_name, branch: e.branch, status: e.status,
        outcome: e.status === "landed" ? "landed" : e.status === "finished" ? "reported" : "pending",
      }));
      const done = members.filter((m) => m.outcome !== "pending").length;
      return { ...p, members, done, state: done === p.size ? "done" : "open" };
    });
  }
  function ledgerPayload() {
    return { entries: S.entries.filter((e) => e.status !== "removed"), marks: S.marks, plans: plansNow(), roles: S.roles };
  }
  function todosPayload() {
    return { todos: S.todos, open: S.todos.filter((t) => t.state === "open").length, answered: S.todos.filter((t) => t.state === "answered").length };
  }

  function removals() {
    const out = [];
    const item = (mark, e) => {
      const a = find(mark.agent_id);
      const ask = mark.ask || {};
      const base = {
        id: mark.agent_id, host: mark.host, pane_id: mark.pane_id, name: mark.name, repo: e ? e.repo_name : (a && a.worktree && a.worktree.repo) || "",
        branch: e ? e.branch : (a && a.worktree && a.worktree.branch) || "", worktree: e ? e.worktree : a && a.cwd, mark,
        live: a ? { present: true, agent_status: a.agent_status } : { present: false }, final_report: e && e.final_report,
        coordinator: !!S.roles[mark.agent_id],
      };
      if (e && e.id === "h-0b77") {
        return { ...base, safe: false, discardable: true,
          warnings: ["2 commits are not merged, and they are on no remote", "The session says it is not safe: two commits are on no remote"],
          checks: { exists: true, dirty: false, dirty_files: 0, unmerged_commits: 2, branch: e.branch, path: e.worktree, tip: "c81e44ab90",
            detail: { unpushed: 2, commits: [{ sha: "c81e44ab90f1", subject: "bookings: serve the list from a materialized view" }, { sha: "7fa2190b3c44", subject: "bench: compare write latency with the view" }], status: [] } } };
      }
      if (ask.state === "asking") return { ...base, safe: false, notes: ["asking the session whether it is safe to remove…"], checks: { exists: true, unmerged_commits: 0, base_used: "origin/main" } };
      return { ...base, safe: true, checks: { exists: true, dirty: false, unmerged_commits: 0, base_used: "origin/main", branch: base.branch, path: base.worktree } };
    };
    for (const e of S.entries) if (e.status === "ready_to_remove" && e.mark) out.push(item(e.mark, e));
    for (const m of S.marks) out.push(item(m, null));
    return out;
  }

  function setMark(id, reason, by) {
    const a = find(id);
    if (!a) return null;
    const mark = { agent_id: id, host: a.host, pane_id: a.pane_id, name: a.name, by, reason, at: now(), ask: { state: "asking", asked_at: now(), at: now() } };
    const e = S.entries.find((x) => x.agent_id === id && x.status !== "removed");
    if (e) { e.marked_from = e.status; e.status = "ready_to_remove"; e.mark = mark; } else S.marks.push(mark);
    later(4000, () => { mark.ask = { state: "safe", reason: "my work is on its branch and I'm idle", at: now() }; });
    return mark;
  }

  /* ---------- transcripts search ---------- */
  const R = (host, kind, id, title, repo, branch, startedMin, durMin, verdict, messages, extra = {}) => ({
    meta: { host, kind, id, title, repo, ...(branch ? { branch } : {}), started: ago(startedMin), ended: ago(startedMin - durMin), messages: messages.length, thread_id: "t-" + id.slice(0, 6), verdict, ...extra },
    messages: messages.map(([role, text, tool_id], i) => ({ seq: role === "handoff" ? -1 : i, role, ts: ago(startedMin - (i * durMin) / messages.length), text, ...(tool_id ? { tool_id } : {}) })),
  });
  const SESSIONS = [
    R("studio", "claude", "a1f0c2d4-1111-4aaa-8aaa-000000000001", "Exponential backoff for the partner sync client", "tidepool", "sync-backoff", 4300, 70, "landed", [
      ["handoff", "The partner sync client hammers a partner when it is down. Add exponential backoff with jitter and a cap."],
      ["user", "The partner sync client hammers a partner when it is down. Add exponential backoff with jitter and a cap."],
      ["assistant", "I'll read the client first. Retries are in `internal/sync/client.go`.\n\n- no **backoff** today, a retry every second\n- no cap on attempts\n\nI'll add both and a test for each."],
      ["tool", "Read internal/sync/client.go", "t1"], ["result", "for attempt := 0; attempt < 100; attempt++ {\n\tif err := c.push(ctx, batch); err == nil { return nil }\n\ttime.Sleep(time.Second)\n}", "t1"],
      ["tool", "Edit internal/sync/backoff.go", "t2"], ["result", "new file: Next(attempt) returns min(cap, base*2^attempt) with full jitter", "t2"],
      ["tool", "Bash go test ./internal/sync/", "t3"], ["result", "ok  \texample.dev/tidepool/internal/sync\t0.388s", "t3"],
      ["assistant", "Done. The backoff starts at 1 s, doubles, and stops at 5 minutes. With full jitter, two clients that failed together won't retry together."],
      ["user", "Why full jitter and not equal jitter?"],
      ["assistant", "With only a few clients it barely matters. Full jitter spreads retries the most and is one line of code, so I picked the simpler one."],
    ], { sub_messages: 3 }),
    R("buildbox", "claude", "b2e1d3c5-2222-4bbb-8bbb-000000000002", "Fix nav contrast in light mode", "marigold-site", "nav-contrast", 400, 20, "landed", [
      ["user", "Nav links fail contrast in light mode. Fix them."],
      ["assistant", "The link color is `#7d8a99` on white, which is 3.1:1. AA needs 4.5:1 for this size."],
      ["tool", "Edit src/styles/nav.css", "t1"], ["result", "--nav-link: #52606d", "t1"],
      ["assistant", "It's now 5.2:1. Hover still reads as different. Merged as `4be1d77`."],
    ]),
    R("spot-7", "pi", "c3d2e4f6-3333-4ccc-8ccc-000000000003", "Why is the search index build so slow?", "fernlog", "", 2900, 45, "reported", [
      ["user", "The search index build takes over three minutes on the large fixture. Why?"],
      ["assistant", "Let me profile it before guessing."],
      ["tool", "Bash go test -run xxx -bench BuildIndex -cpuprofile cpu.out ./index/", "t1"], ["result", "BenchmarkBuildIndex-16   1   212004881233 ns/op", "t1"],
      ["tool", "Bash go tool pprof -top cpu.out", "t2"], ["result", "  61.2%  index.(*Writer).open\n  14.8%  compress/flate.(*compressor).deflate", "t2"],
      ["assistant", "**61%** of the time is reopening the writer once per file. A single writer per pass should take most of that away."],
    ]),
    R("buildbox", "codex", "d4c3b2a1-4444-4ddd-8ddd-000000000004", "Retry queue drops events on shutdown", "tidepool", "drain-on-shutdown", 7300, 120, "abandoned", [
      ["user", "Events in the retry queue are lost when the server shuts down. Can we drain it first?"],
      ["assistant", "Draining works but can hold a deploy for as long as the longest backoff. Writing the queue to disk on shutdown would be simpler."],
      ["tool", "Bash git log --oneline -5 internal/queue/", "t1"], ["result", "e11a0c4 queue: persist on shutdown\n02bd9f7 queue: bound retry attempts", "t1"],
      ["assistant", "`e11a0c4` on main already does that. This branch isn't needed, so I'm stopping here."],
    ]),
    R("studio", "claude", "e5f4a3b2-5555-4eee-8eee-000000000005", "Why did the 2.3 deploy page at 02:00?", "tidepool", "", 1500, 35, "open", [
      ["user", "The 2.3 deploy paged at 02:00. What happened?"],
      ["assistant", "Reading the deploy log and the alert."],
      ["tool", "Bash deployctl logs tidepool --since 01:45 --until 02:15", "t1"], ["result", "01:58 migrate: bookings_idx started\n02:03 health: /readyz 503 (db pool exhausted)\n02:04 token=•••••••• rolled back by operator", "t1"],
      ["assistant", "The index migration held the connection pool, so the readiness check failed and paged. The fix is to build the index concurrently, and to give the health check its own connection."],
    ], { secrets: 1 }),
    R("studio", "claude", "f6a5b4c3-6666-4fff-8fff-000000000006", "Failed confirmation emails are never retried", "tidepool", "mail-backoff", 2300, 50, "landed", [
      ["handoff", "Booking confirmation emails are lost when the mail provider has a bad minute. Queue and retry them."],
      ["user", "Booking confirmation emails are lost when the mail provider has a bad minute. Queue and retry them."],
      ["assistant", "They're sent once, inline, and a failure is only logged. I'll move them to the outbox table and reuse the sync client's backoff instead of writing another one."],
      ["tool", "Edit internal/mail/outbox.go", "t1"], ["result", "Send() now writes to the outbox; a worker drains it with backoff.Next(attempt)", "t1"],
      ["tool", "Bash go test ./internal/mail/", "t2"], ["result", "ok  \texample.dev/tidepool/internal/mail\t0.644s", "t2"],
      ["assistant", "Done. A provider outage now delays an email instead of losing it, and after 24 hours the booking gets a flag in the admin page."],
    ]),
    R("buildbox", "pi", "a7b6c5d4-7777-4aaa-8bbb-000000000007", "Uploader gives up on the first 429", "fernlog", "upload-429", 5200, 40, "reported", [
      ["user", "fernlog upload drops the whole batch when the collector returns 429. It should wait and carry on."],
      ["assistant", "It treats every non-2xx as fatal. I'll retry 429 and 503 with a capped backoff, and resume from the last acknowledged chunk."],
      ["tool", "Edit upload/client.go", "t1"], ["result", "retryable(status) covers 429, 502, 503; resume uses the ack offset", "t1"],
      ["tool", "Bash go test ./upload/ -run TestResume", "t2"], ["result", "ok  \texample.dev/fernlog/upload\t0.291s", "t2"],
      ["assistant", "**Report:** a 2 GB upload against a collector that returns 429 half the time now finishes, with no duplicate chunks."],
    ]),
    R("spot-7", "claude", "b8c7d6e5-8888-4bbb-8ccc-000000000008", "Do partners actually send Retry-After?", "tidepool", "", 900, 25, "reported", [
      ["user", "Before we change webhook retries: how many partners send Retry-After with a 429?"],
      ["assistant", "I'll count it from last month's delivery log."],
      ["tool", "Bash deployctl logs webhooks --since 30d --grep ' 429 ' | cut -d' ' -f4,9 | sort | uniq -c", "t1"], ["result", "  8123 partner-a retry-after=120\n   941 partner-b -\n   212 partner-c retry-after=30", "t1"],
      ["assistant", "Two of the three do, and those two are 90% of the traffic. So honour the header first, and use backoff only when it's missing."],
    ]),
    R("studio", "codex", "c9d8e7f6-9999-4ccc-8ddd-000000000009", "Pick a backoff library or write our own?", "fernlog", "", 8800, 15, "abandoned", [
      ["user", "Is there a backoff library worth using here, or should this stay a few lines of ours?"],
      ["assistant", "The function is six lines, and the libraries bring contexts and timers we already have. I'd keep ours and test it."],
    ]),
    R("buildbox", "claude", "d0e9f8a7-aaaa-4ddd-8eee-00000000000a", "Contact form submits twice on slow networks", "marigold-site", "form-once", 3100, 18, "landed", [
      ["user", "The contact form sometimes sends two messages. Fix it."],
      ["assistant", "The button stays enabled while the request is in flight, so an impatient second tap sends again. I'll disable it and send an idempotency key too."],
      ["tool", "Edit src/components/ContactForm.tsx", "t1"], ["result", "disabled while pending; sends one key per form render", "t1"],
      ["assistant", "Fixed and landed. The server already drops repeated keys."],
    ]),
  ];
  const SUB = { "a1f0c2d4-1111-4aaa-8aaa-000000000001": { "sa-44d0e1a9": [
    { seq: 0, role: "user", ts: ago(4290), text: "Find every caller of the sync client that sets its own retry loop." },
    { seq: 1, role: "assistant", ts: ago(4289), text: "Two callers: `cmd/syncd/main.go` and `internal/admin/resync.go`. Neither needs its own loop once the client backs off." },
    { seq: 2, role: "tool", ts: ago(4289), text: "Grep c.push\\(", tool_id: "s1" },
  ] } };
  const ROLE_IN = { said: ["user", "assistant", "handoff"], commands: ["tool"], output: ["result"] };
  function sessionsFiltered(p) {
    const since = p.get("since");
    return SESSIONS.filter((s) => (!p.get("host") || s.meta.host === p.get("host")) && (!p.get("kind") || s.meta.kind === p.get("kind")) &&
      (!p.get("project") || s.meta.repo === p.get("project")) && (!since || s.meta.ended.slice(0, 10) >= since));
  }
  const words = (q) => q.toLowerCase().split(/\s+/).filter(Boolean);
  const matches = (text, ws) => { const t = String(text).toLowerCase(); return ws.every((w) => t.includes(w)); };
  function snippet(text, ws) {
    const t = String(text).replace(/\s+/g, " ");
    const at = Math.max(0, t.toLowerCase().indexOf(ws[0]) - 50);
    let s = (at ? "…" : "") + t.slice(at, at + 170) + (t.length > at + 170 ? "…" : "");
    for (const w of ws) s = s.replace(new RegExp(w.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "ig"), (m) => `\x02${m}\x03`);
    return s;
  }
  function recallSearch(p) {
    const ws = words(p.get("q") || "");
    const roles = ROLE_IN[p.get("role")];
    let hits = [];
    for (const s of sessionsFiltered(p)) {
      const ms = s.messages.filter((m) => matches(m.text, ws) && (!roles || roles.includes(m.role)));
      if (ms.length) hits.push({ ...s.meta, matches: ms.length, seq: ms[0].seq, sub: "", role: ms[0].role, snippet: snippet(ms[0].text, ws) });
    }
    hits.sort(p.get("sort") === "new" ? (a, b) => (a.started < b.started ? 1 : -1) : (a, b) => b.matches - a.matches);
    return { total: hits.length, hits };
  }
  function facets() {
    const f = (k) => { const c = {}; for (const s of SESSIONS) c[s.meta[k]] = (c[s.meta[k]] || 0) + 1; return Object.entries(c).map(([value, n]) => ({ value, n })); };
    return { hosts: f("host"), kinds: f("kind"), projects: f("repo") };
  }

  /* ---------- launching ---------- */
  const REPOS = { studio: ["tidepool", "marigold-site"], buildbox: ["tidepool", "fernlog", "marigold-site"], "spot-7": ["fernlog"] };
  const reposFor = (host) => (REPOS[host] || []).map((name) => ({ name, path: `/home/dev/code/${name}`, label: `~/code/${name}`, default_branch: "main" }));
  const CATALOGS = {
    pi: { kind: "pi", source: "pi --list-models", exact: true, default: "local/thinking", default_thinking: "minimal", thinking_levels: ["off", "minimal", "low", "medium", "high"],
      models: [{ id: "local/fast", provider: "local", name: "fast" }, { id: "local/thinking", provider: "local", name: "thinking", thinking: true }] },
    claude: { kind: "claude", source: "curated", models: [{ id: "opus", provider: "Aliases", note: "newest Opus" }, { id: "sonnet", provider: "Aliases", note: "newest Sonnet" }] },
    codex: { kind: "codex", source: "none", models: [] },
  };
  function launch(host, b) {
    const pane = `w${++S.nextPane}:p1`;
    const kind = b.kind || "claude";
    let a;
    if (b.coordinator) {
      a = agent(host, pane, "coordinator-" + S.nextPane, kind, "idle", "", { cwd: "/home/dev", active: 0 });
      S.roles[a.id] = { role: "coordinator", name: a.name, by: "the user", at: now() };
    } else {
      const repo = (b.dir || "").split("/").filter(Boolean).pop() || "scratch";
      a = agent(host, pane, b.name || `${repo}-${S.nextPane}`, kind, "idle", repo, { cwd: b.dir, active: 0 });
    }
    S.agents.push(a);
    tx(a.id);
    if (b.prompt) reply(a.id, b.prompt);
    return a;
  }
  function handoff(b) {
    const host = b.host || LOCAL;
    const repo = (b.repo || "").split("/").filter(Boolean).pop() || "tidepool";
    const pane = `w${++S.nextPane}:p1`;
    const a = agent(host, pane, b.branch.replace(/[^a-z0-9_-]/gi, "-").toLowerCase(), b.kind || "claude", "working", repo, { branch: b.branch, active: 0 });
    S.agents.push(a);
    const e = { id: "h-" + (S.seq++).toString(16), host, repo: b.repo, repo_name: repo, branch: b.branch, base: b.base || "main", kind: a.agent, agent_id: a.id, pane_id: pane, agent_name: a.name,
      worktree: a.cwd, brief: b.brief, from: "you", status: "running", created_at: now() };
    S.entries.unshift(e);
    push(a.id, { kind: "user", text: b.brief });
    later(2500, () => { push(a.id, { kind: "tool", tool: "Bash", args: "git status", result: `On branch ${b.branch}\nnothing to commit, working tree clean`, ms: 70 }); broadcast(); });
    later(9000, () => finishWorker(a.id, "Done. This worker is a demo, so no code changed, but this is where its report would appear.",
      `## ${b.branch}\n\nA real worker would report here: what it changed, the commits on its branch and how it tested them.\n\nYour brief was: *${String(b.brief).trim().slice(0, 160)}*`));
    return { entry: e, worker: { agent: { pane_id: pane, name: a.name } } };
  }

  /* ---------- the agent routes ---------- */
  const TERMINAL = "$ codex\n\n  Codex is ready. This agent has no chat view, so Comebye shows its terminal.\n\n> summarise what changed in the log parser last week\n\n  Three commits, all on the timestamp parser:\n    - accepts RFC 3339 with a space separator\n    - rejects dates past 9999\n    - benchmarks for the fast path\n";
  function agentRoute(id, rest, method, p, body) {
    const a = find(id);
    if (!a) return err(404, "no_agent", "that agent is gone");
    if (rest === "transcript" && method === "GET") {
      if (a.agent === "codex") return err(404, "no_transcript", "no chat view for this agent");
      let ms = tx(id);
      const from = p.get("from");
      let partial = false;
      if (from) { const i = ms.findIndex((m) => m.id === from); if (i >= 0) { ms = ms.slice(i); partial = true; } }
      const out = { messages: ms, total: tx(id).length, more: false, format: a.agent === "pi" ? "pi" : "claude", ...(partial ? { partial: true } : {}) };
      if (id === "spot-7/w2:p1") out.running_subagents = [];
      return out;
    }
    if (rest === "activity") return { activity: id === "buildbox/w3:p1" ? { text: "Working through the webhook tests", generic: false } : null };
    if (rest === "dialog") return { dialog: S.dialogs[id] || null };
    if (rest === "output") return { text: a.agent === "codex" ? (S.terminal && S.terminal[id]) || TERMINAL : `${a.name} is running in pane ${a.pane_id} on ${a.host}.\n\n(The raw terminal, as herdr sees it.)\n` };
    if (rest.startsWith("subagents/")) return { messages: [{ id: "s1", kind: "tool", tool: "Bash", args: "time ./bin/fernlog index --fixture large", result: "real 3m32s", ms: 212000, time: ago(46) }, { id: "s2", kind: "assistant", text: "Median of three runs: 212 s.", time: ago(45) }], total: 2, more: false, format: "pi" };
    if (rest === "prompt" && method === "POST") {
      if (S.dialogs[id]) return err(409, "blocked", "agent is waiting for input");
      reply(id, body.text || "");
      return { ok: true };
    }
    if (rest === "answer" && method === "POST") return answer(a, body);
    if (rest === "keys" && method === "POST") {
      if ((body.keys || []).includes("esc")) {
        if (S.dialogs[id]) { delete S.dialogs[id]; push(id, { kind: "note", note: "Question cancelled" }); setStatus(a, "idle"); broadcast(); }
        else if (a.agent_status === "working") { push(id, { kind: "note", note: "Interrupted" }); setStatus(a, "idle"); broadcast(); }
      }
      return { ok: true };
    }
    if (rest === "image") return { path: "/tmp/comebye-upload-demo.png" };
    if (rest === "model") { push(id, { kind: "note", note: `Model switched to ${body.model || "the same model"}` }); return { ok: true }; }
    return err(404, "not_found", "the demo has no route for " + rest);
  }
  function answer(a, body) {
    const id = a.id, d = S.dialogs[id];
    if (!d) return err(409, "no_dialog", "the dialog is gone");
    if (body.action === "cancel") {
      delete S.dialogs[id];
      push(id, { kind: "note", note: "Question cancelled" });
      setStatus(a, "idle");
      broadcast();
      return { keys: ["esc"] };
    }
    const label = (d.options[body.index] || {}).label || "";
    delete S.dialogs[id];
    setStatus(a, "working");
    broadcast();
    const store = id === "studio/w4:p1";
    if (store) {
      push(id, { kind: "user", text: label });
      later(1400, () => { push(id, { kind: "tool", tool: "Edit", args: "internal/limit/store.go", result: `added the ${label} store`, ms: 40 }); broadcast(); });
      later(3200, () => { push(id, { kind: "tool", tool: "Bash", args: "go test ./internal/limit/", result: "ok  \texample.dev/tidepool/internal/limit\t0.902s", ms: 1400 }); broadcast(); });
      later(5200, () => finishWorker(id, `Going with **${label}**. The limiter is in place and tests pass, so here is my report.`,
        `## Rate limiter\n\nA token bucket behind a \`Store\` interface, with **${label}** as the production store.\n\n- two commits on \`rate-limit-store\`\n- \`go test ./internal/limit/\` passes\n\n**Open question:** on by default? It's on your To do list.`));
    } else if (/block/i.test(label)) {
      later(1500, () => finishWorker(id, "Understood, I won't run the integration suite. The fix is committed, and only the unit tests have checked it.", "## Replay order\n\nThe fix is committed. The integration suite was **not** run, because you blocked it."));
    } else {
      later(1400, () => { push(id, { kind: "tool", tool: "Bash", args: "make integration TEST_FILTER=replay", result: "PASS  replay (41 runs, no failures)", ms: 2100 }); broadcast(); });
      later(3600, () => finishWorker(id, "The integration suite passes 41 runs in a row, so the race is gone.", "## Replay order\n\nTwo goroutines appended to one slice during replay. Replay now orders by sequence number.\n\n`make integration TEST_FILTER=replay` passes 41 runs in a row."));
    }
    return { keys: body.index ? [...Array(body.index).fill("down"), "enter"] : ["enter"] };
  }

  /* ---------- the router ---------- */
  function err(status, code, message) { const e = new Error(message); e.http = status; e.code = code; throw e; }
  function route(method, path, p, body) {
    if (path === "/api/agents") return agentsPayload();
    if (path === "/api/version") return { version: VERSION };
    if (path === "/api/ledger") return ledgerPayload();
    if (path === "/api/brief") return { brief: S.brief, schedule: S.schedule };
    if (path === "/api/brief/history") return { briefs: [S.brief, ...S.briefHistory], max: 48 };
    if (path === "/api/brief/schedule") {
      if (method === "POST") S.schedule = body.enabled ? { enabled: true, every: body.every, at: body.at, tz: body.tz, state: "ok", updated: now() } : { enabled: false, state: "off", updated: now() };
      return { schedule: S.schedule };
    }
    if (path === "/api/brief/schedule/dismiss") return { schedule: S.schedule };
    if (path === "/api/todos") return todosPayload();
    let m = path.match(/^\/api\/todos\/([^/]+)\/(answer|done|drop)$/);
    if (m && method === "POST") return todoAct(decodeURIComponent(m[1]), m[2], body);
    if (path === "/api/roles") {
      if (method === "POST") {
        const a = find(body.agent_id);
        if (body.role) S.roles[body.agent_id] = { role: body.role, name: a && a.name, by: "the user", at: now() };
        else delete S.roles[body.agent_id];
        broadcast();
      }
      return { roles: S.roles };
    }
    if (path === "/api/marks" && method === "POST") { const mark = setMark(body.agent_id, body.reason, "you"); broadcast(); return { mark }; }
    if (path === "/api/marks/clear" && method === "POST") {
      S.marks = S.marks.filter((x) => x.agent_id !== body.agent_id);
      const e = S.entries.find((x) => x.agent_id === body.agent_id && x.status === "ready_to_remove");
      if (e) { e.status = e.marked_from || "finished"; delete e.mark; }
      broadcast();
      return { ok: true };
    }
    if (path === "/api/removals") return { removals: removals() };
    if (path === "/api/removals/confirm" && method === "POST") return confirmRemovals(body);
    if (path === "/api/sweep" && method === "POST") return sweep(body);
    if (path === "/api/queue") return { queue: [] };
    if (path.startsWith("/api/queue/")) return { ok: true };
    if (path === "/api/handoffs" && method === "POST") { const r = handoff(body); broadcast(); return r; }
    if (path.startsWith("/api/push/")) return { enabled: false };
    if (path === "/api/recall/status") return { state: "ok", built_at: ago(35), checked_at: ago(4), hosts: ["studio", "buildbox", "spot-7"], sessions: SESSIONS.length, messages: SESSIONS.reduce((n, s) => n + s.meta.messages, 0) };
    if (path === "/api/recall/facets") return facets();
    if (path === "/api/recall/recent") { const hits = sessionsFiltered(p).map((s) => s.meta); return { total: hits.length, hits }; }
    if (path === "/api/recall/search") return recallSearch(p);
    m = path.match(/^\/api\/recall\/sessions\/([^/]+)\/([^/]+)\/([^/]+)(\/hits)?$/);
    if (m) {
      const s = SESSIONS.find((x) => x.meta.id === decodeURIComponent(m[3]));
      if (!s) return err(404, "no_session", "no such session");
      if (m[4]) { const ws = words(p.get("q") || ""); return { capped: false, hits: s.messages.filter((x) => matches(x.text, ws)).map((x) => ({ seq: x.seq, sub: "" })) }; }
      const subs = SUB[s.meta.id] || {};
      const sub = p.get("sub") || "";
      return { session: s.meta, sub, subs: Object.entries(subs).map(([id, ms]) => ({ id, messages: ms.length })), messages: sub ? subs[sub] || [] : s.messages, before: false, after: false };
    }
    m = path.match(/^\/api(?:\/hosts\/([^/]+))?\/agents\/([^/]+)\/(.+)$/);
    if (m) return agentRoute(`${m[1] ? decodeURIComponent(m[1]) : LOCAL}/${decodeURIComponent(m[2])}`, m[3], method, p, body);
    m = path.match(/^\/api(?:\/hosts\/([^/]+))?\/(launch|repos|models)(?:\/(options|dirs))?$/);
    if (m) {
      const host = m[1] ? decodeURIComponent(m[1]) : LOCAL;
      if (m[2] === "repos") return { repos: reposFor(host) };
      if (m[2] === "models") { const c = CATALOGS[p.get("kind")]; return { host, catalogs: c ? [c] : [] }; }
      if (m[3] === "options") return { enabled: true, coordinator_dir: "~", default_kind: "claude", kinds: ["claude", "pi", "codex"], model_kinds: ["claude", "pi", "codex"],
        roots: [{ name: "~/code", path: "/home/dev/code", label: "~/code", git: false }], recent: reposFor(host).slice(0, 1).map((r) => ({ ...r, git: true })) };
      if (m[3] === "dirs") {
        const path2 = p.get("path");
        return path2 === "/home/dev/code"
          ? { path: path2, label: "~/code", entries: reposFor(host).map((r) => ({ name: r.name, path: r.path, label: r.label, git: true })) }
          : { path: path2, label: path2.replace("/home/dev", "~"), parent: "/home/dev/code", entries: [] };
      }
      if (method === "POST") { const a = launch(host, body); broadcast(); return { agent: { pane_id: a.pane_id, name: a.name }, workspace_id: a.pane_id.split(":")[0] }; }
    }
    window.ComebyeDemo.unhandled.push(method + " " + path);
    return err(404, "not_found", "the demo has no route for " + path);
  }
  function todoAct(id, what, body) {
    const t = S.todos.find((x) => x.id === id);
    if (!t) return err(404, "no_todo", "that item is gone");
    if (what === "answer") {
      t.state = "answered"; t.updated_at = now();
      t.answer = { text: body.text, by: "you", via: "app", at: now() };
      push("studio/w1:p1", { kind: "user", text: `(from the To do list) ${t.question}\n\nAnswer: ${body.text}` });
      // The coordinator picks it up and passes it on, then the item closes.
      later(5000, () => {
        push("studio/w1:p1", { kind: "assistant", text: `Passing your answer on${t.waiting ? ` to **${t.waiting.name}**` : ""}: *${body.text}*`, model: "sonnet" });
        if (t.waiting) push(t.waiting.id, { kind: "note", note: `quill relayed your answer: ${body.text}` });
        S.todos = S.todos.filter((x) => x !== t);
        broadcast();
      });
    } else S.todos = S.todos.filter((x) => x !== t);
    broadcast();
    return { todo: t };
  }
  function confirmRemovals(b) {
    const results = (b.ids || []).map((id) => {
      const e = S.entries.find((x) => x.agent_id === id && x.status === "ready_to_remove");
      const safe = !(e && e.id === "h-0b77");
      const discard = !!(b.discard && b.discard[id]);
      S.agents = S.agents.filter((a) => a.id !== id);
      S.marks = S.marks.filter((m) => m.agent_id !== id);
      if (e) { e.status = "removed"; e.removed_at = now(); }
      return { id, ok: true, result: { closed: "workspace", worktree_removed: safe && !discard, ...(discard ? { discarded: true, branch_deleted: e && e.branch } : !safe ? { kept: "it has unmerged commits" } : {}) } };
    });
    broadcast();
    return { results };
  }
  function sweep(b) {
    const cand = S.entries.filter((e) => e.status === "finished" && e.agent_id);
    const row = (e, reason) => ({ id: e.agent_id, name: e.agent_name, host: e.host, branch: e.branch, reason });
    const ready = cand.filter((e) => e.candidate).map((e) => row(e, "finished, clean and merged"));
    const look = cand.filter((e) => !e.candidate).map((e) => row(e, "finished, but its branch is not merged yet"));
    if (!b.dry_run) { for (const r of ready) setMark(r.id, "finished, clean and merged", "sweep"); broadcast(); }
    return { ready, needs_look: look, skipped: [] };
  }

  /* ---------- fetch and EventSource ---------- */
  const realFetch = window.fetch.bind(window);
  window.fetch = function (input, init = {}) {
    const url = new URL(typeof input === "string" ? input : input.url, "http://demo.invalid/");
    if (!url.pathname.startsWith("/api/")) return realFetch(input, init);
    const method = (init.method || "GET").toUpperCase();
    const signal = init.signal;
    return new Promise((resolve, reject) => {
      if (signal && signal.aborted) return reject(signal.reason || new DOMException("aborted", "AbortError"));
      const onAbort = () => reject(signal.reason || new DOMException("aborted", "AbortError"));
      if (signal) signal.addEventListener("abort", onAbort, { once: true });
      setTimeout(() => {
        if (signal) signal.removeEventListener("abort", onAbort);
        let status = 200, data;
        try {
          data = route(method, url.pathname, url.searchParams, init.body ? JSON.parse(init.body) : {});
        } catch (e) {
          if (!e.http) console.warn("demo backend:", e);
          status = e.http || 500;
          data = { code: e.code || "demo_error", message: e.message };
        }
        // A deep copy, as the network would give: the app must not share objects with the mock.
        resolve(new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json", "X-App-Version": VERSION } }));
      }, 30 + Math.random() * 60);
    });
  };

  const streams = new Set();
  class FakeEventSource {
    constructor() {
      this.readyState = 0; this.listeners = {}; this.onopen = null; this.onerror = null;
      streams.add(this);
      setTimeout(() => {
        if (this.readyState === 2) return;
        this.readyState = 1;
        if (this.onopen) this.onopen({});
        this.emit("agents");
      }, 40);
      this.ping = setInterval(() => this.fire("ping", ""), 15000);
    }
    addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
    removeEventListener(type, fn) { this.listeners[type] = (this.listeners[type] || []).filter((f) => f !== fn); }
    fire(type, data) { for (const fn of this.listeners[type] || []) fn({ type, data }); }
    emit(type) { this.fire(type, JSON.stringify(agentsPayload())); }
    close() { this.readyState = 2; clearInterval(this.ping); streams.delete(this); }
  }
  FakeEventSource.CONNECTING = 0; FakeEventSource.OPEN = 1; FakeEventSource.CLOSED = 2;
  window.EventSource = FakeEventSource;
  function broadcast() { for (const s of streams) if (s.readyState === 1) s.emit("agents"); }

  window.ComebyeDemo = { unhandled: [], reset() { seed(); broadcast(); } };
  seed();
})();
