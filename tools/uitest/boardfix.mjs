// A canned GET /api/board answer for the board: drive.mjs checks the
// board's rendering against it, and shot.mjs --board serves it in the dev
// server (so the screenshot shows every lane without arranging the state).
const ago = (min) => new Date(Date.now() - min * 60000).toISOString();

export function boardView(todoId = "tf7e1b09b86", question = "Press Bump hub on v0.5.20?") {
  const lane = (key, name, items, rule = "rule", count = items.length) => ({ key, name, rule, count, items });
  const h = (id, title, project, state, since, extra = {}) => ({ id, type: "handoff", title, project, state, since: ago(since), ...extra });
  return {
    at: ago(0),
    lanes: [
      lane("needs_you", "Needs you", [
        { id: todoId, type: "todo", kind: "decision", title: question, project: "Releases", state: "open", since: ago(40) },
        { id: "t0a1b2c3d4e", type: "todo", kind: "action", title: "Install the coordinator hook on laharl", project: "comebye", state: "open", since: ago(190) },
        h("h222222222222", "sandboxdriver-reader", "openclaw-enterprise", "running", 300, { type: "dialog", detail: "stuck on a dialog: it needs an answer" }),
      ]),
      lane("running", "Running", [
        h("h111111111111", "briefing-board", "comebye", "running", 25, { detail: "writing the lanes" }),
        h("h333333333333", "mods-build", "comebye", "running", 120, { detail: "status band, instant /todo" }),
      ]),
      lane("waiting", "Waiting on others", [
        { id: "trel0v0520a", type: "release", title: "Release v0.5.20", project: "Releases", state: "tagged", since: ago(35), detail: "tagged; rolling out to the hosts. The Releases tab shows the next step." },
        h("h444444444444", "oc-pr-4025", "openclaw-enterprise", "waiting", 600, { detail: "queued behind h555555555555" }),
      ]),
      lane("landing", "Ready to land, or landing", [
        h("h666666666666", "fresh-safety", "comebye", "finished", 90, { detail: "stale SAFE/UNSAFE answers re-checked" }),
        h("h777777777777", "coord-reset", "comebye", "landing", 10, { detail: "landing: tests running" }),
      ], "rule", 49),
      lane("reports", "Reports to read", [
        h("h121212121212", "etcd-op-lifecycle", "comebye", "finished", 20, { kind: "research", report: true, pages: [{ id: "pg_etcd1", title: "etcd operator lifecycle: findings" }], detail: "the operator's upgrade and restore paths" }),
        h("h131313131313", "helm-s0-spec", "comebye", "finished", 400, { kind: "plan", report: true }),
      ]),
      lane("landed", "Landed, recent", [
        h("h888888888888", "ci-speed", "comebye", "landed", 300, { detail: "main CI 7m34s" }),
        h("h999999999999", "oce-setup", "oce-ocp-setup", "landed", 900, { detail: "PR #17 merged" }),
      ]),
    ],
    loose: { reports: 0, ready_to_remove: 3, archived: 27 },
  };
}
