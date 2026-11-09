// Fixture token usage for the Usage screen: Claude Code and Pi session files
// (counts only: no message text, only ids, models, usage and a directory),
// for the dev hub ("devbox") and its peer ("peerbox"), plus ledger entries for
// the handoffs the workers belong to. Deterministic: the same files every run.
//
//   node usagefix.mjs OUT_DIR [--days N]
//     OUT_DIR/hub/claude/<project>/<session>.jsonl   and .../pi/<dir>/<ts>_<uuid>.jsonl
//     OUT_DIR/peer/claude, OUT_DIR/peer/pi
//     OUT_DIR/ledger.json                            entries for the hub's workers
//
// dev.sh and the uitest point the hub and the peer at these with
// COMEBYE_ARCHIVE_CLAUDE_DIR / COMEBYE_ARCHIVE_PI_DIR.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const out = process.argv[2];
const days = +(process.argv.includes("--days") ? process.argv[process.argv.indexOf("--days") + 1] : 21);
if (!out) { console.error("usage: usagefix.mjs OUT_DIR [--days N]"); process.exit(2); }

let seed = 7;
const rnd = () => { seed = (seed * 1664525 + 1013904223) % 4294967296; return seed / 4294967296; };
const pick = (a) => a[Math.floor(rnd() * a.length)];
const now = Date.now();
const day = 86400e3;

const files = [];
let msg = 0;
// claudeSession writes one session: replies spread over the given days and hours.
function claudeSession(root, project, sid, cwd, branch, model, spec) {
  const lines = [];
  for (const d of spec.days) {
    const n = spec.perDay;
    for (let k = 0; k < n; k++) {
      const t = new Date(now - d * day + (9 + k * (spec.span / n)) * 3600e3 * 0.5 + rnd() * 600e3);
      if (t.getTime() > now) continue;
      const ctx = spec.ctx * (0.7 + rnd() * 0.6);
      const cw = Math.round(ctx * spec.write * (0.5 + rnd()));
      const u = { input_tokens: Math.round(2 + rnd() * 30), output_tokens: Math.round(spec.out * (0.3 + rnd() * 1.4)), cache_creation_input_tokens: cw, cache_read_input_tokens: Math.round(ctx - cw) };
      const id = "msg_" + (++msg);
      // a reply is written once per content block, with the usage so far: the last line wins
      lines.push({ type: "assistant", timestamp: t.toISOString(), cwd, gitBranch: branch, isSidechain: false, message: { id, model, usage: { ...u, output_tokens: 3 } } });
      lines.push({ type: "assistant", timestamp: new Date(t.getTime() + 1500).toISOString(), cwd, gitBranch: branch, isSidechain: false, message: { id, model, usage: u } });
    }
  }
  lines.sort((a, b) => a.timestamp.localeCompare(b.timestamp));
  files.push([path.join(root, "claude", project, sid + ".jsonl"), lines]);
}
function piSession(root, dir, sid, cwd, model, spec) {
  const lines = [{ type: "session", version: 3, id: sid, timestamp: new Date(now - spec.days[0] * day).toISOString(), cwd }];
  for (const d of spec.days) {
    for (let k = 0; k < spec.perDay; k++) {
      const t = new Date(now - d * day + (10 + k) * 3600e3 * 0.6);
      if (t.getTime() > now) continue;
      lines.push({ type: "message", id: "p" + (++msg), timestamp: t.toISOString(), message: { role: "assistant", provider: "homelab", model, usage: { input: Math.round(spec.ctx * 0.1), output: Math.round(spec.out * (0.5 + rnd())), cacheRead: Math.round(spec.ctx * 0.9), cacheWrite: 0, cost: { total: 0 } } } });
    }
  }
  files.push([path.join(root, "pi", dir, `${new Date(now - spec.days[0] * day).toISOString().replace(/[:.]/g, "-")}_${sid}.jsonl`), lines]);
}
const range = (a, b) => Array.from({ length: Math.max(0, b - a + 1) }, (_, i) => a + i);

const hub = path.join(out, "hub"), peer = path.join(out, "peer");
const HOME = os.homedir(); // the hub is this machine, so "home" sessions are in its real home directory
// the coordinator: every day, big contexts (laharl in the fixtures runs in ~/code/comebye)
for (const d of range(0, days - 1)) {
  claudeSession(hub, "-home-u-code-comebye", `c0000000-0000-4000-8000-${String(d).padStart(12, "0")}`, "/home/u/code/comebye", "main", "claude-opus-5-5", { days: [d], perDay: 14, span: 14, ctx: 190000, write: 0.03, out: 700 });
}
// workers with handoffs (ledger entries below)
const handoffs = [
  ["usage-collector", "comebye", "feat/usage-collector", range(1, 3), 22, 70000, "claude-opus-5-5", ""],
  ["usage-screen", "comebye", "feat/usage-screen", range(0, 2), 18, 90000, "claude-opus-5-5", ""],
  ["fix-detail-menu", "comebye", "fix/detail-menu", range(4, 6), 12, 60000, "claude-sonnet-5-5", ""],
  ["research-cache", "comebye", "research/cache", range(8, 9), 16, 80000, "claude-opus-5-5", "review"],
  ["flma-ingest", "flma", "feat/ingest", range(2, 5), 20, 75000, "claude-sonnet-5-5", ""],
  ["hs-upgrade", "hypershift", "deploy/upgrade", range(6, 10), 10, 65000, "claude-opus-4-8", ""],
  ["old-plan", "flma", "plan/schema", range(12, 13), 9, 50000, "claude-haiku-5-5", ""],
];
const entries = [];
handoffs.forEach(([name, repo, branch, ds, per, ctx, model, purpose], i) => {
  const wt = `${HOME}/.worktrees/${repo}/${branch.replace("/", "-")}`;
  claudeSession(hub, `-home-u--worktrees-${repo}-${branch.replace("/", "-")}`, `a${i}000000-0000-4000-8000-000000000000`, wt, branch, model, { days: ds, perDay: per, span: 12, ctx, write: 0.06, out: 900 });
  entries.push({ id: `h${i}${(i * 7919).toString(16).padStart(11, "0")}`.slice(0, 12), host: "devbox", repo: `${HOME}/code/${repo}`, repo_name: repo, branch, kind: "claude", purpose, agent_name: name, agent_id: `devbox/wU${i}:p1`,
    worktree: wt, brief: "fixture", status: "removed", created_at: new Date(now - (ds[ds.length - 1] + 1) * day).toISOString() });
});
// worktrees with no handoff, a home session, scratch, and Pi
claudeSession(hub, "-home-u--worktrees-flma-spike-x", "b0000000-0000-4000-8000-000000000001", `${HOME}/.worktrees/flma/spike-x`, "spike/x", "claude-sonnet-5-5", { days: [3, 4], perDay: 8, span: 10, ctx: 40000, write: 0.05, out: 500 });
claudeSession(hub, "-home-u", "b0000000-0000-4000-8000-000000000002", HOME, "", "claude-sonnet-5-5", { days: range(0, 9), perDay: 4, span: 8, ctx: 30000, write: 0.05, out: 400 });
claudeSession(hub, "-tmp-scratch", "b0000000-0000-4000-8000-000000000003", "/tmp/scratch-1", "", "claude-haiku-5-5", { days: [1, 2], perDay: 5, span: 6, ctx: 20000, write: 0.08, out: 300 });
piSession(hub, "--home-u-code-notes--", "019e9e7c-bfc2-7970-b8c7-1a8c1107ae56", `${HOME}/code/notes`, "glm-5-3", { days: range(0, 6), perDay: 6, ctx: 30000, out: 500 });
piSession(hub, "--home-u--", "019e9e7c-bfc2-7970-b8c7-1a8c1107ae57", HOME, "deepseek-v4", { days: range(2, 5), perDay: 5, ctx: 25000, out: 400 });
// the peer
claudeSession(peer, "-home-p--worktrees-hypershell-feat-gitops", "d0000000-0000-4000-8000-000000000001", "/home/p/.worktrees/hypershell/feat-gitops", "feat/gitops", "claude-sonnet-5-5", { days: range(0, 4), perDay: 12, span: 10, ctx: 70000, write: 0.05, out: 700 });
piSession(peer, "--home-p--", "019e9e7c-bfc2-7970-b8c7-1a8c1107ae58", HOME, "glm-5-3", { days: range(0, 8), perDay: 8, ctx: 35000, out: 500 });

for (const [f, lines] of files) {
  fs.mkdirSync(path.dirname(f), { recursive: true });
  fs.writeFileSync(f, lines.map((l) => JSON.stringify(l)).join("\n") + "\n");
}
fs.writeFileSync(path.join(out, "ledger.json"), JSON.stringify({ version: 1, entries, marks: [] }));
console.log(`usage fixtures: ${files.length} session files, ${entries.length} handoffs in ${out}`);
