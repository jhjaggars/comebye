// The scheduled sweep against a throwaway hub (see sweeploop.sh): what it marked, the one To do item, a
// second sweep that marks nothing new, one confirm that removes them, and the screenshots.
//
//   node sweeploop.mjs BASE OUT_DIR HARNESS_DIR
import { launch } from "./lib.mjs";
import fs from "node:fs";

const [base, out, harness] = process.argv.slice(2);
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
const until = async (what, fn, ms = 30000) => {
  const end = Date.now() + ms;
  for (;;) {
    const v = await fn();
    if (v) return v;
    if (Date.now() > end) throw new Error("timed out waiting for " + what);
    await new Promise((r) => setTimeout(r, 250));
  }
};
const sweepTodos = async () => (await api("GET", "/api/todos?state=all")).todos?.filter((t) => t.source === "hub-sweep") ?? [];

const MERGED = ["add-cache", "bump-deps", "drop-flag", "fix-footer", "fix-header", "rename-api"];
const LEFT = ["half-done", "local-only", "open-pr-a", "open-pr-b"];
let todoId = "";

await check("the loop ran by itself and marked exactly the merged, clean sessions", async () => {
  const s = await until("the first scheduled sweep", async () => (await api("GET", "/api/settings")).sweep_loop.last_run_at);
  const v = (await api("GET", "/api/settings")).sweep_loop;
  if (!v.enabled || v.every_minutes !== 15) throw new Error("setting " + JSON.stringify(v));
  const marked = (await api("GET", "/api/removals")).removals.map((r) => r.name).sort();
  if (!marked.length) for (const x of (await api("POST", "/api/sweep", { dry_run: true })).needs_look) console.log(`     needs a look: ${x.name}: ${x.reason}`);
  console.log("     marked: " + marked.join(", ") + "; last sweep: marked " + v.last_marked + ", " + v.last_needs_look + " need a look");
  if (JSON.stringify(marked) !== JSON.stringify(MERGED)) throw new Error("marked " + marked + ", want " + MERGED);
  const by = (await api("GET", "/api/removals")).removals.map((r) => r.mark.by);
  if (!by.every((b) => b === "hub")) throw new Error("marked by " + by);
});

await check("one batched To do item, linking the removals review", async () => {
  const items = await until("the item", async () => { const t = await sweepTodos(); return t.length ? t : null; });
  if (items.length !== 1) throw new Error(items.length + " items");
  const t = items[0];
  todoId = t.id;
  console.log("     " + t.question);
  if (t.question !== "6 sessions are ready to remove" || t.kind !== "action" || !t.links?.some((l) => l.url === "/#/remove")) throw new Error(JSON.stringify(t));
  for (const n of MERGED) if (!t.context.includes(n)) throw new Error("context lacks " + n);
  for (const n of LEFT) if (t.context.includes(n)) throw new Error("context names " + n);
});

await check("a second sweep marks nothing new and files no second item", async () => {
  const r = await api("POST", "/api/sweep", { idle_after: "2h" });
  if (r.ready.length) throw new Error("marked again: " + r.ready.map((x) => x.name));
  await new Promise((r) => setTimeout(r, 6000)); // the loop looks every 2 s in test mode
  const t = await sweepTodos();
  if (t.length !== 1) throw new Error(t.length + " items");
  const left = r.needs_look.map((x) => x.name).sort();
  if (JSON.stringify(left) !== JSON.stringify(LEFT)) throw new Error("needs a look: " + left);
  for (const x of r.needs_look) console.log(`     needs a look: ${x.name}: ${x.reason}`);
});

await check("the board line and the event say what the sweep did", async () => {
  const b = await api("GET", "/api/board");
  const lane = b.lanes.find((l) => l.key === "hub_did");
  if (!lane) throw new Error("no Hub did lane");
  const first = lane.items[0];
  console.log("     " + first.title);
  if (!/^Last sweep: marked 6 ready to remove \(/.test(first.title) || !/4 need a look/.test(first.title)) throw new Error(first.title);
  if (!lane.items.some((i) => i.kind === "sweep" && /Sweep marked 6 sessions ready to remove/.test(i.title))) throw new Error("no sweep event");
});

// Screenshots before the confirm: the item and the board line, and the setting.
for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark: true });
  await check(`screenshots at ${width}px: the To do item, the board line and the setting`, async () => {
    await page.goto(`${base}/#/todo/${todoId}`, { waitUntil: "load" });
    await page.waitForSelector(".td-item, .td-open, [data-todo-id]", { timeout: 15000 }).catch(() => {});
    await page.waitForTimeout(800);
    await page.screenshot({ path: `${out}/sweep-todo-${width}.png` });
    const txt = await page.evaluate(() => document.body.innerText);
    if (!/6 sessions are ready to remove/.test(txt)) throw new Error("the item is not on screen");
    await page.goto(`${base}/#/briefings`, { waitUntil: "load" });
    await page.waitForTimeout(1500);
    const lane = page.locator('[data-lane="hub_did"]');
    if (await lane.count()) { await lane.first().evaluate((el) => { el.open = true; el.scrollIntoView(); }); }
    await page.waitForTimeout(400);
    await page.screenshot({ path: `${out}/sweep-board-${width}.png` });
    const bt = await page.evaluate(() => document.body.innerText);
    if (!/Last sweep: marked 6/.test(bt)) throw new Error("the board line is not on screen");
    await page.goto(`${base}/#/peers/devbox`, { waitUntil: "load" });
    await page.waitForSelector("#pr-sweep-on", { timeout: 15000 });
    await page.locator("#pr-sweep-on").scrollIntoViewIfNeeded();
    await page.screenshot({ path: `${out}/sweep-setting-${width}.png` });
  });
  await browser.close();
}

await check("one confirm in the removals review removes them all", async () => {
  const { browser, page } = await launch({ width: 390, height: 844, dark: true });
  try {
    await page.goto(`${base}/#/remove`, { waitUntil: "load" });
    await page.waitForSelector("#rv-all-safe:not([disabled])", { timeout: 20000 });
    await page.screenshot({ path: `${out}/sweep-review-390.png` });
    const label = await page.locator("#rv-all-safe").innerText();
    if (!/Remove all safe \(6\)/.test(label)) throw new Error("button says " + label);
    await page.locator("#rv-all-safe").click();
    await page.getByRole("button", { name: /^Remove 6$/ }).click(); // the one confirm
    await until("the removals", async () => (await api("GET", "/api/removals")).removals.length === 0, 40000);
  } finally { await browser.close(); }
  const entries = (await api("GET", "/api/ledger")).entries;
  const st = Object.fromEntries(entries.map((e) => [e.agent_name, e.status]));
  console.log("     " + JSON.stringify(st));
  for (const n of MERGED) if (st[n] !== "removed") throw new Error(n + " is " + st[n]);
  for (const n of LEFT) if (st[n] !== "finished") throw new Error(n + " is " + st[n]);
  for (const n of LEFT) if (!fs.existsSync(`${harness}/worktrees/proj/${n}`)) throw new Error(n + " worktree is gone");
  for (const n of MERGED) if (fs.existsSync(`${harness}/worktrees/proj/${n}`)) throw new Error(n + " worktree is still there");
});

await check("the item closes itself once nothing is left to remove", async () => {
  await until("the item to close", async () => { const t = await sweepTodos(); return t.length === 1 && t[0].state === "done"; }, 20000);
});

console.log(failed ? `${failed} failed` : "all passed");
process.exit(failed ? 1 : 0);
