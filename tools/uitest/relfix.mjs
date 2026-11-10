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
//   done      tagged and every host upgraded; the item was closed by the hub

const V = "v0.5.22", PREV = "v0.5.21";
const SHA = "a3f9c1e5d2b7480cf6e1a9b34d70c8e2f15a6b09";
const ago = (ms) => new Date(Date.now() - ms).toISOString();
const HIST = [
  ["v0.5.21", 1, "hub bumped by hand after the item was closed"], ["v0.5.20", 3, "Peers show unreachable hosts"],
  ["v0.5.19", 6, "Answered box readable in light mode"], ["v0.5.18", 9, "Mark button reads Unmark"], ["v0.5.17", 12, "Briefings skip a slot when nothing changed"],
];

export const RELEASE_STATES = ["none", "ci", "green", "red", "tagged", "partial", "partial3", "failed", "done"];

export function releaseView(name) {
  const tagged = ["tagged", "partial", "partial3", "failed", "done"].includes(name);
  const vers = {
    none: [PREV, PREV, PREV, PREV], ci: [PREV, PREV, PREV, PREV], green: [PREV, PREV, PREV, PREV], red: [PREV, PREV, PREV, PREV],
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
