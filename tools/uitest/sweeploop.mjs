// The scheduled sweep and its clean-up against a throwaway hub (see sweeploop.sh): the loop removes the
// merged, clean sessions and deletes the one provably stale remote branch by itself, keeps everything else,
// files no To do, says what it did on the board and in the removals review (with the branch's SHA), and does
// nothing the second time. Screenshots at 390 and 1280 px.
//
//   node sweeploop.mjs BASE OUT_DIR HARNESS_DIR EXAMPLE_BRANCH
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";

const [base, out, harness, B1] = process.argv.slice(2);
let failed = 0;
async function check(name, fn) {
  try { await fn(); console.log("ok   " + name); } catch (e) { failed++; console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const api = async (method, path, body) => {
  const r = await fetch(base + path, { method, headers: { "content-type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await r.text();
  if (!r.ok) throw new Error(`${method} ${path}: ${r.status} ${text}`);
  return text ? JSON.parse(text) : {};
};
const until = async (what, fn, ms = 45000) => {
  const end = Date.now() + ms;
  for (;;) {
    const v = await fn();
    if (v) return v;
    if (Date.now() > end) throw new Error("timed out waiting for " + what);
    await new Promise((r) => setTimeout(r, 250));
  }
};
const git = (...a) => execFileSync("git", ["-C", harness + "/proj", ...a]).toString().trim();
const onOrigin = (b) => git("ls-remote", "--heads", harness + "/origin.git", "refs/heads/" + b) !== "";
const statuses = async () => Object.fromEntries((await api("GET", "/api/ledger")).entries.map((e) => [e.agent_name, e.status]));
const allTodos = async () => (await api("GET", "/api/todos?state=all")).todos ?? [];

const MERGED = ["add-cache", "bump-deps", "drop-flag", "fix-footer", "fix-header", "rename-api"];
const NEEDS_LOOK = ["half-done", "local-only", "open-pr-a", "open-pr-b"];
const KEPT = ["extra-commit", "stop-server"];
const DELIVERED = ["poc/demo", "eval/bench"];
let tip = "";

await check("the loop ran by itself and removed the merged, clean sessions and the one with a provably stale branch", async () => {
  tip = git("rev-parse", "refs/remotes/origin/" + B1);
  const want = [...MERGED, "tool-runs-s6-keys", "left-path-branch"];
  const st = await until("the removals", async () => { const s = await statuses(); return want.every((n) => s[n] === "removed") ? s : null; });
  console.log("     " + JSON.stringify(st));
  for (const n of [...NEEDS_LOOK]) if (st[n] !== "finished") throw new Error(n + " is " + st[n]);
  for (const n of KEPT) if (st[n] !== "ready_to_remove") throw new Error(n + " is " + st[n]);
  for (const n of MERGED) {
    if (fs.existsSync(`${harness}/worktrees/proj/${n}`)) throw new Error(n + " worktree is still there");
    if (git("branch", "--list", n) !== "") throw new Error(n + " local branch is still there");
  }
  for (const n of [...NEEDS_LOOK, "extra", "server"]) if (!fs.existsSync(`${harness}/worktrees/proj/${n}`)) throw new Error(n + " worktree is gone");
  if (fs.existsSync(`${harness}/worktrees/proj/s6`)) throw new Error("s6 worktree is still there");
  if (fs.existsSync(`${harness}/worktrees/proj/lp`)) throw new Error("lp worktree is still there");
});

await check("the typed leftovers settle: the path in the scratchpad goes with the removal, the stale branch item is deleted", async () => {
  if (onOrigin("feat/leftovers")) throw new Error("feat/leftovers is still on origin");
  const lp = fs.readFileSync(harness + "/lp_path", "utf8").trim();
  if (fs.existsSync(lp)) throw new Error("the scratchpad file is still there: " + lp);
  const e = (await api("GET", "/api/ledger")).entries.find((x) => x.agent_name === "left-path-branch");
  const ask = e && e.mark && e.mark.ask;
  console.log("     " + (ask ? ask.state + ": " + ask.reason : JSON.stringify(e && e.mark)));
  if (!ask || ask.state !== "safe" || !/deleted origin\/feat\/leftovers/.test(ask.reason)) throw new Error("answer not settled by the sweep: " + JSON.stringify(ask));
});

await check("a dry-run sweep classes the poc/ and eval/ workers as delivered by their branch prefix", async () => {
  const st = await statuses();
  for (const n of DELIVERED) if (st[n] !== "finished") throw new Error(n + " is " + st[n]);
  const r = await api("POST", "/api/sweep", { dry_run: true });
  const cls = Object.fromEntries(["ready", "needs_look", "delivered", "gone", "skipped"].flatMap((k) => (r[k] || []).map((i) => [i.name, k + ": " + i.reason])));
  console.log("     " + JSON.stringify(Object.fromEntries([...DELIVERED, "local-only"].map((n) => [n, cls[n]]))));
  for (const n of DELIVERED) if (!String(cls[n]).startsWith("delivered: work delivered in the report")) throw new Error(n + " is " + cls[n]);
  if (!String(cls["local-only"]).startsWith("needs_look")) throw new Error("local-only is " + cls["local-only"]);
});

await check("the board puts the poc/ and eval/ workers in Reports to read, and local-only in Ready to land", async () => {
  const b = await api("GET", "/api/board");
  const lane = (k) => b.lanes.find((l) => l.key === k)?.items ?? [];
  const reports = lane("reports").map((i) => i.title + "|" + i.kind).join(", "), landing = lane("landing").map((i) => i.title).join(", ");
  console.log("     reports: " + reports + "; landing: " + landing);
  for (const [n, k] of [["poc/demo", "poc"], ["eval/bench", "eval"]]) if (!reports.includes(n + "|" + k) || landing.includes(n)) throw new Error(n + " not a " + k + " report");
  if (!landing.includes("local-only")) throw new Error("local-only is not in landing");
});

await check("the proven backup branch is deleted from origin; the one with a commit main lacks is kept", async () => {
  if (onOrigin(B1)) throw new Error(B1 + " is still on origin");
  if (!onOrigin("feat/extra")) throw new Error("a branch with unmerged work was deleted");
  if (!onOrigin("main")) throw new Error("main was touched");
});

await check("no To do is filed, and the sessions that need a look stay in the review with none", async () => {
  await new Promise((r) => setTimeout(r, 4000));
  const t = await allTodos();
  if (t.length) throw new Error("to-dos: " + t.map((x) => x.question));
  const names = (await api("GET", "/api/removals")).removals.map((r) => r.name).sort();
  if (JSON.stringify(names) !== JSON.stringify(KEPT)) throw new Error("review has " + names);
});

await check("a second run does nothing", async () => {
  const before = JSON.stringify((await api("GET", "/api/removals")).sweep_branches) + (await api("GET", "/api/removals")).auto_removed.length;
  await new Promise((r) => setTimeout(r, 6000)); // the loop looks every 2 s in test mode
  const after = JSON.stringify((await api("GET", "/api/removals")).sweep_branches) + (await api("GET", "/api/removals")).auto_removed.length;
  if (before !== after) throw new Error(before + " -> " + after);
});

await check("the board line and the log say what the loop removed and deleted, with the SHA", async () => {
  const b = await api("GET", "/api/board");
  const lane = b.lanes.find((l) => l.key === "hub_did");
  if (!lane) throw new Error("no Hub did lane");
  const first = lane.items[0];
  console.log("     " + first.title);
  if (!/^Sweep: removed 8 sessions \(/.test(first.title) || !/deleted 2 stale remote branches/.test(first.title) || !first.title.includes("origin/" + B1 + " @ " + tip.slice(0, 7))) throw new Error(first.title);
  const log = await api("GET", "/api/log?since=1h&kind=sweep");
  const ev = (log.events || []).find((i) => /Sweep deleted 2 stale remote branches/.test(i.text));
  if (!ev || !ev.text.includes(tip)) throw new Error("no deletion event with the SHA: " + JSON.stringify(log).slice(0, 800));
  console.log("     " + ev.text);
});

await check("the review lists what was removed and the deleted branch with its SHA and restore command, and the command works", async () => {
  const d = await api("GET", "/api/removals");
  if (d.auto_removed.length !== 8) throw new Error("auto_removed " + d.auto_removed.length);
  if (!d.auto_removed.every((a) => /nothing to lose/.test(a.reason))) throw new Error(d.auto_removed.map((a) => a.reason));
  const b = d.sweep_branches.find((x) => x.branch === B1);
  if (d.sweep_branches.length !== 2 || !b || !d.sweep_branches.some((x) => x.branch === "feat/leftovers") || b.tip !== tip || b.restore !== `git push origin ${tip}:refs/heads/${B1}`) throw new Error(JSON.stringify(d.sweep_branches));
  git(...b.restore.split(" ").slice(1));
  if (!onOrigin(B1) || git("ls-remote", "--heads", harness + "/origin.git", "refs/heads/" + B1).split(/\s/)[0] !== tip) throw new Error("the branch did not come back at its tip");
  git("push", "-q", "origin", ":refs/heads/" + B1); // delete it again for the screenshots
});

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 900 : 844, dark: true });
  await check(`screenshots at ${width}px: the board line, the removals review and the setting`, async () => {
    await page.goto(`${base}/#/briefings`, { waitUntil: "load" });
    await page.waitForTimeout(1500);
    const lane = page.locator('[data-lane="hub_did"]');
    if (await lane.count()) { await lane.first().evaluate((el) => { el.open = true; el.scrollIntoView(); }); }
    await page.waitForTimeout(400);
    await page.screenshot({ path: `${out}/sweep-board-${width}.png` });
    const bt = await page.evaluate(() => document.body.innerText);
    if (!/Sweep: removed 8 sessions/.test(bt)) throw new Error("the board line is not on screen");
    await page.goto(`${base}/#/remove`, { waitUntil: "load" });
    await page.waitForSelector(".rv-auto-item", { timeout: 20000 });
    await page.waitForTimeout(700);
    await page.screenshot({ path: `${out}/sweep-review-${width}.png`, fullPage: true });
    await page.locator("#rv-auto").scrollIntoViewIfNeeded();
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${out}/sweep-review-removed-${width}.png` });
    await page.locator(".rv-branch").first().scrollIntoViewIfNeeded();
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${out}/sweep-review-branch-${width}.png` });
    const rt = await page.locator("#rv-auto").innerText();
    if (!/Removed automatically/i.test(rt) || !/Remote branches deleted/i.test(rt) || !rt.includes(tip) || !rt.includes("git push origin " + tip)) throw new Error("the review lacks the removals or the branch SHA; tail: " + rt.slice(-700));
    const w = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    if (w > 1) throw new Error(`horizontal overflow of ${w}px at ${width}px`);
    await page.goto(`${base}/#/peers/devbox`, { waitUntil: "load" });
    await page.waitForSelector("#pr-sweep-on", { timeout: 15000 });
    if (!(await page.locator("#pr-sweep-on").isChecked())) throw new Error("not on");
    if (await page.locator("#pr-sweep-del").count()) throw new Error("the old branch switch is still there");
    await page.locator("#pr-sweep-on").scrollIntoViewIfNeeded();
    await page.screenshot({ path: `${out}/sweep-setting-${width}.png` });
  });
  await browser.close();
}

console.log(failed ? `${failed} failed` : "all passed");
process.exit(failed ? 1 : 0);
