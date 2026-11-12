// Canned GET /api/releases answers for the Releases tab, one per state of the
// mockup (docs: the Peers screen's Releases tab): used by drive.mjs to check
// the tab's rendering in every state, and by shot.mjs --release STATE to look
// at a state in the dev server without arranging it for real.
//
//   none      nothing pending; everything on v0.5.21
//   ci        v0.5.22 proposed, CI running
//   green     proposed, CI green: Approve release
//   red       proposed, CI red
//   tagged    tagged, build green, nothing upgraded yet (the hub is behind)
//   partial   tagged; hub and peerbox upgraded, kubevirt-1 behind, mac-mini offline
//   partial3  tagged; three of four upgraded, mac-mini offline (the user's "3 of 4, 1 offline")
//   failed    tagged; the hub's deploy failed, peerbox's upgrade was rolled back
//   dismissed the user abandoned v0.5.22: nothing pending, v0.5.22 first in the history
//   rollout   the v0.5.24 rollout screenshot: hub comebye-hub bumped by laharl's deploy, laharl (the release host) still on v0.5.23, a Mac upgraded by hand after a failed app upgrade
//   done      tagged and every host upgraded; the item was closed by the hub
//   ro-before   tagged, build green: the "Roll out to all" button and the plan it would confirm
//   ro-running  a rollout: the canary is done, the next peer is upgrading, one waits
//   ro-paused   paused after the canary
//   ro-failed   stopped at the second peer (its upgrade was rolled back); the third was not touched
//   ro-skipped  done, one peer skipped (offline): "Roll out again"

const V = "v0.5.22", PREV = "v0.5.21";
const SHA = "a3f9c1e5d2b7480cf6e1a9b34d70c8e2f15a6b09";
const ago = (ms) => new Date(Date.now() - ms).toISOString();
const HIST = [
  ["v0.5.21", 1, "hub bumped by hand after the item was closed"], ["v0.5.20", 3, "Peers show unreachable hosts"],
  ["v0.5.19", 6, "Answered box readable in light mode"], ["v0.5.18", 9, "Mark button reads Unmark"], ["v0.5.17", 12, "Briefings skip a slot when nothing changed"],
];

export const RELEASE_STATES = ["none", "ci", "green", "red", "tagged", "partial", "partial3", "failed", "rollout", "done", "dismissed", "ro-before", "ro-running", "ro-paused", "ro-failed", "ro-skipped"];

// The v0.5.24 rollout as the user saw it. The deploy ran from laharl for the hub (comebye-hub);
// laharl itself is on v0.5.23 and the Mac was upgraded by hand after its app upgrade failed.
function rolloutView() {
  const N = "v0.5.24", P = "v0.5.23", by = "jhjaggars@gmail.com";
  const mk = (name, version, extra = {}) => ({ name, local: name === "comebye-hub", hub: name === "comebye-hub", status: "ok", version, enabled: true, offline: false, current: version === N, ...extra });
  const hosts = [
    mk("comebye-hub", N),
    mk("laharl", P),
    mk("mac", N, { record: { id: "m1", version: N, state: "failed", rollback: false, by, started: ago(120000), finished: ago(60000), error: "could not start: systemd-run is needed to upgrade from the app" } }),
    mk("kubevirt-1", P),
  ];
  const cur = hosts.filter((h) => h.current).length;
  const rollout = { total: 4, current: cur, offline: 0, behind: 4 - cur, complete: false };
  const base = releaseView("tagged");
  base.release_host = "laharl";
  base.hosts = hosts;
  base.rollout = rollout;
  base.todo.release.version = N;
  base.todo.question = `Release ${N} (a3f9c1e)?`;
  base.deploy.record = { version: N, state: "ok", by: `${by} (hub says: by ${by})`, started: ago(900000), finished: ago(840000), headline: "bump: ok version=" + N + " is serving",
    output: "bump: version=" + N + "\nbump: ok version=" + N + " is serving" };
  base.needs_text = `Roll out ${N}: 2 hosts behind`;
  return base;
}

// The rolling rollout on the Fleet card: laharl is the hub, three peers behind it.
function rolloutJobView(name) {
  const out = releaseView("tagged");
  const hosts = out.hosts;
  const set = (n, o) => Object.assign(hosts.find((h) => h.name === n), o);
  const step = (host, wave, phase, extra = {}) => ({ host, wave, phase, at: ago(60000), ...extra });
  out.rollout_enabled = true;
  out.csrf = "fixture";
  const hub = () => step("laharl", "hub", "pending", { mode: "deploy" });
  const plan = [step("peerbox", "canary", "pending"), step("kubevirt-1", "peers", "pending"), step("mac-mini", "peers", "pending"), hub()];
  out.rollout_plan = plan;
  out.rollout_hub_label = "Bump hub";
  if (name === "ro-before") return out;
  delete out.rollout_plan;
  const job = { id: "r7f3a1", todo: out.todo.id, version: V, commit: SHA, state: "running", by: "me", canary: "peerbox", plan, started: ago(300000), updated: ago(2000), rev: 4, note: "" };
  out.rollout_job = job;
  const done = (host, wave, from) => step(host, wave, "ok", { from, to: V, run: "run-" + host, done: ago(120000), detail: "" });
  set("peerbox", { version: V, current: true, record: { id: "run-peerbox", version: V, state: "ok", from: PREV, to: V, by: "me", started: ago(280000), finished: ago(240000) } });
  const n = (c) => { out.rollout = { total: 4, current: c, offline: 0, behind: 4 - c, complete: false }; };
  if (name === "ro-running") {
    job.plan = [done("peerbox", "canary", PREV), step("kubevirt-1", "peers", "upgrading", { run: "run-kv", from: PREV }), step("mac-mini", "peers", "pending"), hub()];
    job.note = "upgrading kubevirt-1";
    set("kubevirt-1", { record: { id: "run-kv", version: V, state: "running", by: "me", started: ago(30000) } });
    n(1);
    out.needs_you = false; out.needs_text = `Rolling out ${V}: upgrading kubevirt-1`;
  } else if (name === "ro-paused") {
    job.state = "paused"; job.note = "paused after peerbox";
    job.plan = [done("peerbox", "canary", PREV), step("kubevirt-1", "peers", "pending"), step("mac-mini", "peers", "pending"), hub()];
    n(1);
    out.needs_you = true; out.needs_text = `Rollout of ${V} is paused`;
  } else if (name === "ro-failed") {
    job.state = "stopped"; job.note = "stopped at kubevirt-1";
    const why = "rolled_back: smoke check failed: journal has errors";
    job.stop = { host: "kubevirt-1", reason: why, at: ago(40000) };
    job.plan = [done("peerbox", "canary", PREV), step("kubevirt-1", "peers", "failed", { run: "run-kv", from: PREV, detail: why, output: "fetch ok\nverify ok\ninstall ok\nrestart ok\nsmoke: journal has errors\nrolled back to " + PREV }), step("mac-mini", "peers", "pending"), hub()];
    set("kubevirt-1", { record: { id: "run-kv", version: V, state: "rolled_back", by: "me", started: ago(200000), finished: ago(60000), error: "smoke check failed: journal has errors" } });
    n(1);
    out.needs_you = true; out.needs_text = `Rollout of ${V} stopped at kubevirt-1`;
  } else if (name === "ro-skipped") {
    job.state = "done"; job.finished = ago(20000);
    job.plan = [done("peerbox", "canary", PREV), done("kubevirt-1", "peers", PREV), step("mac-mini", "peers", "skipped", { detail: "offline for 2m0s" }), done("laharl", "hub", PREV)];
    set("laharl", { version: V, current: true });
    set("kubevirt-1", { version: V, current: true });
    set("mac-mini", { status: "down", offline: true, unknown: true, enabled: false, since: ago(3 * 3600000) });
    n(3); out.rollout.offline = 1; out.rollout.behind = 0;
    out.rollout_plan = [step("mac-mini", "peers", "pending")];
    delete out.rollout_hub_label;
    out.needs_you = false; out.needs_text = `${V}: 3 of 4, 1 offline`;
  }
  return out;
}

export function releaseView(name) {
  if (name.startsWith("ro-")) return rolloutJobView(name);
  if (name === "rollout") return rolloutView();
  const tagged = ["tagged", "partial", "partial3", "failed", "done"].includes(name);
  const vers = {
    none: [PREV, PREV, PREV, PREV], dismissed: [PREV, PREV, PREV, PREV], ci: [PREV, PREV, PREV, PREV], green: [PREV, PREV, PREV, PREV], red: [PREV, PREV, PREV, PREV],
    tagged: [PREV, PREV, PREV, PREV], partial: [V, V, PREV, "v0.5.20"], partial3: [V, V, V, "v0.5.20"],
    failed: [PREV, PREV, V, V], done: [V, V, V, V],
  }[name];
  const names = ["laharl", "peerbox", "kubevirt-1", "mac-mini"];
  const offline = name === "partial" || name === "partial3" ? 3 : -1;
  const hosts = names.map((n, i) => ({
    name: n, local: i === 0, hub: i === 0, status: i === offline ? "down" : "ok", version: vers[i], enabled: i !== offline,
    offline: i === offline, current: tagged && vers[i] === V,
    ...(i === offline ? { since: ago(2 * 86400000), unknown: true } : {}),
  }));
  if (name === "failed") {
    hosts[1].record = { id: "r1", version: V, state: "rolled_back", rollback: false, by: "me", started: ago(300000), finished: ago(240000), error: "peerbox did not report in 60s" };
  }
  if (name === "partial" || name === "partial3") {
    hosts[1].record = { id: "r2", version: V, state: "ok", from: PREV, to: V, by: "me", started: ago(900000), finished: ago(840000) };
  }
  const cur = hosts.filter((h) => h.current).length, off = hosts.filter((h) => h.offline && !h.current).length;
  const rollout = { total: 4, current: cur, offline: off, behind: 4 - cur - off, complete: cur === 4 };
  const rel = { version: V, commit: SHA, state: tagged ? "tagged" : "pending" };
  if (tagged) Object.assign(rel, { approved_by: "me", approved_at: ago(3600000), tagged_at: ago(3000000), host: "laharl", run_url: "https://github.com/jhjaggars/comebye/actions/runs/123456" });
  const todo = { id: "tfixture01", question: `Release ${V} (a3f9c1e)?`, kind: "release", state: name === "done" ? "done" : "open", created_by: "coordinator", created_at: ago(7200000), updated_at: ago(60000), release: rel,
    ...(name === "done" ? { closed_by: "comebye", closed_at: ago(60000) } : {}) };
  const history = HIST.map(([version, d]) => ({ version, commit: SHA.replace(/^.{7}/, "b" + version.slice(-2).replace(".", "0") + "0000").slice(0, 40), tagged_at: ago(d * 86400000), notes_url: `https://github.com/jhjaggars/comebye/releases/tag/${version}`, todo: "t" + version.slice(1).replace(/\./g, "") }));
  const out = { csrf: "fixture", can_tag: true, simulated: false, dry_run: false, release_host: "laharl", hosts, history, audit: [] };
  if (name === "dismissed") {
    out.history = [{ version: V, commit: SHA, tagged_at: ago(3000000), notes_url: `https://github.com/jhjaggars/comebye/releases/tag/${V}`, todo: "tfixture01" }, ...out.history].slice(0, 5);
    return { ...out, current: false, needs_you: false, needs_text: "" };
  }
  if (name === "none") return { ...out, current: false, needs_you: false, needs_text: "" };
  out.current = true; out.todo = todo;
  if (!tagged) {
    const base = { version: V, commit: SHA, short: "a3f9c1e", subject: "To do titles and answers render inline Markdown", previous: PREV, ci_id: "9", ci_url: "https://github.com/jhjaggars/comebye/actions/runs/9",
      changes: ["a3f9c1e To do titles and answers render inline Markdown", "9c1d2e3 Briefings skip a slot when nothing changed", "77ab5f1 Ledger notifies the requester only on a new report", "5e0a9d4 Open To do item uses the panel width"], at: ago(5000) };
    out.check = name === "ci" ? { ...base, ok: false, problem: "CI for a3f9c1e is still in_progress", ci_status: "in_progress" }
      : name === "red" ? { ...base, ok: false, problem: "CI for a3f9c1e finished with failure", ci_status: "completed", ci_conclusion: "failure" }
        : { ...base, ok: true, ci_status: "completed", ci_conclusion: "success" };
    out.needs_you = name === "green" || name === "red";
    out.needs_text = name === "green" ? "Approve " + V : name === "red" ? V + " is blocked: CI for a3f9c1e finished with failure" : V + " waits for CI";
    return out;
  }
  out.build = { state: "success", url: "https://github.com/jhjaggars/comebye/actions/runs/123456", at: ago(1000) };
  out.notes_url = `https://github.com/jhjaggars/comebye/releases/tag/${V}`;
  out.rollout = rollout;
  out.deploy = { enabled: true, label: "Bump hub", command: "bump-hub.sh" };
  if (name === "failed") {
    out.deploy.record = { version: V, state: "failed", by: "me", started: ago(200000), finished: ago(150000), error: "exit status 1", headline: "bump: FAILED phase=reconcile reason=flux: command not found",
      output: "bump: version=" + V + "\nbump: push\nbump: reconcile\nflux: command not found\nbump: FAILED phase=reconcile reason=flux: command not found" };
  }
  if (name === "done") out.deploy.record = { version: V, state: "ok", by: "me", started: ago(1500000), finished: ago(1440000), headline: "bump: ok version=" + V + " is serving", output: "bump: ok version=" + V + " is serving" };
  out.needs_you = !rollout.complete && (name === "tagged" || name === "partial" || name === "failed");
  out.needs_text = name === "done" ? "All upgraded to " + V : name === "failed" ? "Upgrade failed on peerbox" : name === "tagged" ? `Roll out ${V}: 4 hosts behind`
    : name === "partial" ? `Roll out ${V}: 1 host behind` : `${V}: 3 of 4, 1 offline`;
  return out;
}
