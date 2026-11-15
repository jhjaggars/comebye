// Screenshots of the Ready to remove review at 390 px for one app (the fixture is served to /api/removals).
// node leftshots.mjs BASE OUT_PREFIX before|after dark|light
import { launch } from "./lib.mjs";
const [base, prefix, mode, scheme] = process.argv.slice(2);
const ago = (h) => new Date(Date.now() - h * 3600e3).toISOString();
const mark = (at, ask) => ({ by: "coordinator", at: ago(at), reason: "landed; PR merged", ask });
const row = (id, name, branch, m, extra) => ({ id, name, host: "uitest", branch, repo: "webapp", safe: false, coordinator: false, live: { present: true, agent_status: "idle" },
  mark: m, checks: { exists: true, primary: false, dirty: false, unmerged_commits: 0, base_used: "origin/main", branch }, warnings: [], notes: [], ...extra });
const item = (type, label, state, now, text, more) => ({ type, label, state, now, text, ...more });
const after = mode === "after";
const T = after ? "the session says this is left: " : "the session says UNSAFE: ";
const rows = [
  row("uitest/w9:p4", "docs-fix", "docs/fix", mark(1, { state: "unsafe", at: ago(0.5) }), { warnings: [T + "delete branch docs/fix and its worktree (answered 30 min ago)", T + "close my PR #90 (answered 30 min ago)"],
    ask_review: { state: "unsafe", answered_at: ago(0.5), only_cleanup: after, items: [
      item(after ? "cleanup" : "other", after ? "cleanup" : "something else", "unchecked", "can't check: " + (after ? "the session can do this itself" : "free text"), "delete branch docs/fix and its worktree", {}),
      item(after ? "cleanup" : "other", after ? "cleanup" : "something else", "unchecked", "can't check: " + (after ? "the session can do this itself" : "free text"), "close my PR #90", {})] } }),
  row("uitest/w9:p5", "key-rotation", "feat/keys", mark(2, { state: "unsafe", at: ago(1.5) }), { warnings: [T + "which key to use (answered 90 min ago)"],
    ask_review: { state: "unsafe", answered_at: ago(1.5), items: after ? [
      item("decision", "decision (to-do t972236dfee)", "unresolved", "is still open", "which key to use", { id: "t972236dfee" }),
      item("cleanup", "cleanup", "unchecked", "can't check: the session can do this itself", "delete branch feat/keys", {})] : [
      item("todo", "to-do t972236dfee", "unresolved", "is still open", "waiting on the user: which key to use", { id: "t972236dfee" }),
      item("other", "something else", "unchecked", "can't check: free text", "delete branch feat/keys", {})] } }),
  row("uitest/w9:p1", "release-simple", "release/simple", mark(0.05, { state: "safe", reason: "both PRs merged", at: ago(0.005) }), { safe: true, ask_review: { state: "safe", answered_at: ago(0.005) } }),
];
const { browser, page } = await launch({ dark: scheme === "dark" });
await page.route(/\/api\/removals$/, (r) => r.fulfill({ json: { removals: rows } }));
await page.goto(base + "/#/remove", { waitUntil: "load" });
await page.waitForSelector("#rv-list .rv-item", { timeout: 8000 });
await new Promise((r) => setTimeout(r, 400));
await page.screenshot({ path: `${prefix}-${mode}-${scheme}.png`, fullPage: true });
await browser.close();
