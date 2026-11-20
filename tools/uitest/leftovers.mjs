// The sweep's leftovers against a throwaway hub (see leftovers.sh). Phase "confirm" (the default setting): the
// example card is safe to remove with its branch deletion named, nothing is deleted by the loop, the batched
// item lists the deletion, a branch with a commit main lacks is kept, one confirm deletes only the proven
// branch. Phase "auto": the loop deletes a proven branch itself, says so on the card, and does it once.
//
//   node leftovers.mjs PHASE BASE OUT_DIR HARNESS_DIR EXAMPLE_BRANCH
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";

const [phase, base, out, harness, B1] = process.argv.slice(2);
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
  for (;;) { const v = await fn(); if (v) return v; if (Date.now() > end) throw new Error("timed out waiting for " + what); await new Promise((r) => setTimeout(r, 250)); }
};
const onOrigin = (b) => execFileSync("git", ["-C", harness + "/proj", "ls-remote", "--heads", harness + "/origin.git", "refs/heads/" + b]).toString().trim() !== "";
const removals = async () => (await api("GET", "/api/removals")).removals;
const byName = async (n) => (await removals()).find((r) => r.name === n);
const sweepTodos = async () => (await api("GET", "/api/todos?state=all")).todos?.filter((t) => t.source === "hub-sweep") ?? [];

async function shots(tag, sel) {
  for (const width of [390, 1280]) {
    const { browser, page } = await launch({ width, height: width > 500 ? 900 : 844, dark: true });
    try {
      await page.goto(`${base}/#/remove`, { waitUntil: "load" });
      await page.waitForSelector(".rv-item", { timeout: 20000 });
      await page.waitForTimeout(700);
      await page.screenshot({ path: `${out}/${tag}-review-${width}.png`, fullPage: true });
      if (sel) { const el = page.locator(sel).first(); if (await el.count()) { await el.scrollIntoViewIfNeeded(); await page.screenshot({ path: `${out}/${tag}-card-${width}.png` }); } }
      const w = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
      if (w > 1) throw new Error(`horizontal overflow of ${w}px at ${width}px`);
    } finally { await browser.close(); }
  }
}

if (phase === "confirm") {
  await check("the example card: safe to remove, the branch deletion named, nothing deleted by looking", async () => {
    const it = await until("the review", () => byName("tool-runs-s6-keys"));
    if (!it.safe || !it.ask_review?.stale_only) throw new Error(JSON.stringify({ safe: it.safe, ar: it.ask_review, w: it.warnings }));
    const v = it.ask_review.items[0];
    console.log("     " + v.now);
    if (!v.stale || !/git cherry/.test(v.proof)) throw new Error(JSON.stringify(v));
    if (!onOrigin(B1)) throw new Error("the branch is gone from origin");
  });
  await check("a backup with a commit main lacks, and a process to stop, stay left to the user", async () => {
    const a = await byName("extra-commit"), b = await byName("stop-server");
    if (a.safe || b.safe) throw new Error("safe: " + a.safe + " " + b.safe);
    console.log("     extra-commit: " + a.warnings.join(" | "));
    console.log("     stop-server: " + b.warnings.join(" | "));
    if (!/is kept: 1 commit is not in origin\/main/.test(a.warnings.join(" "))) throw new Error("warning: " + a.warnings);
  });
  await check("the loop ran and deleted nothing (confirm mode), and filed one item naming the deletion", async () => {
    await until("a sweep", async () => (await api("GET", "/api/settings")).sweep_loop.last_run_at);
    const t = await until("the item", async () => { const x = await sweepTodos(); return x.length ? x[0] : null; });
    console.log("     " + t.question + "\n     " + t.context.split("\n").filter((l) => l.startsWith("- ") || /also deletes/.test(l)).join("\n     "));
    if (t.question !== "1 session is ready to remove" || !t.context.includes("delete* origin/" + B1)) throw new Error(t.context);
    // The sessions with something left nobody can finish are listed apart, after the ready ones, for the user.
    const [readyPart, stuckPart = ""] = t.context.split("no one is going to finish it");
    if (readyPart.includes("extra-commit") || readyPart.includes("stop-server")) throw new Error("a session that is not safe is listed as ready");
    if (!stuckPart.includes("stop-server") || !stuckPart.includes("agent gone") || !stuckPart.includes("pid 4242")) throw new Error("the stuck leftover is not listed: " + stuckPart);
    if (!onOrigin(B1) || !onOrigin("feat/extra")) throw new Error("the loop deleted a branch");
    if ((await api("GET", "/api/settings")).sweep_loop.auto_delete_branches) throw new Error("auto is on by default");
  });
  await check("screenshots of the batched item at 390 and 1280", async () => {
    const t = (await sweepTodos())[0];
    for (const width of [390, 1280]) {
      const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark: true });
      try {
        await page.goto(`${base}/#/todo/${t.id}`, { waitUntil: "load" });
        await page.waitForTimeout(1500);
        await page.screenshot({ path: `${out}/item-${width}.png` });
        if (!/delete\s*origin\/feat\/tool-runs-6-keyboard/.test(await page.evaluate(() => document.body.innerText))) throw new Error(`the deletion is not on screen at ${width}px`);
      } finally { await browser.close(); }
    }
  });
  await shots("confirm", '.rv-item[data-id$="w50:p1"]');
  await check("one confirm deletes exactly the proven branch and removes the session", async () => {
    const { browser, page } = await launch({ width: 390, height: 844, dark: true });
    try {
      await page.goto(`${base}/#/remove`, { waitUntil: "load" });
      await page.waitForSelector("#rv-all-safe:not([disabled])", { timeout: 20000 });
      const label = await page.locator("#rv-all-safe").innerText();
      if (!/Remove all safe \(1\)/.test(label)) throw new Error("button says " + label);
      await page.locator("#rv-all-safe").click();
      await page.getByRole("button", { name: /^Remove 1$/ }).click();
      await until("the removal", async () => !(await byName("tool-runs-s6-keys")));
    } finally { await browser.close(); }
    if (onOrigin(B1)) throw new Error("the proven branch is still on origin");
    if (!onOrigin("feat/extra")) throw new Error("a branch with unmerged work was deleted");
    if (!onOrigin("main")) throw new Error("main was touched");
    if (!(await byName("extra-commit"))) throw new Error("extra-commit is gone");
  });
  await check("the Peers setting is off by default and saves on", async () => {
    const { browser, page } = await launch({ width: 390, height: 844, dark: true });
    try {
      await page.goto(`${base}/#/peers/devbox`, { waitUntil: "load" });
      await page.waitForSelector("#pr-sweep-del", { timeout: 15000 });
      if (await page.locator("#pr-sweep-del").isChecked()) throw new Error("on by default");
      await page.locator("#pr-sweep-del").scrollIntoViewIfNeeded();
      await page.screenshot({ path: `${out}/setting-390.png` });
    } finally { await browser.close(); }
  });
} else {
  await check("auto mode: the loop deleted the proven branch itself, once, and the card says when", async () => {
    const it = await until("the loop's delete", async () => { const x = await byName("auto-case"); return x?.ask_review?.loop ? x : null; }, 45000);
    console.log("     " + it.ask_review.loop.text);
    if (onOrigin("feat/auto")) throw new Error("still on origin");
    if (!onOrigin("feat/extra")) throw new Error("a branch with unmerged work was deleted");
    if (!it.safe || it.mark.ask.state !== "safe") throw new Error("not settled: " + JSON.stringify(it.mark.ask));
    const st = (await api("GET", "/api/board")).lanes.find((l) => l.key === "hub_did")?.items[0]?.title;
    console.log("     " + st);
    if (!/deleted \d+ stale remote branch/.test(st || "")) throw new Error("board line: " + st);
  });
  await check("a LIVE agent re-asked 12 times: the loop gives the go-ahead itself, and nobody asks it again", async () => {
    const it = await until("the loop's delete", async () => { const x = await byName("layered-surfaces"); return x?.ask_review?.loop ? x : null; }, 45000);
    console.log("     live " + it.live.present + ", reasks " + it.mark.ask.reasks + ", state " + it.mark.ask.state + ": " + it.ask_review.loop.text);
    if (!it.live.present) throw new Error("the agent is not live");
    if (onOrigin("ui/layered-surfaces")) throw new Error("still on origin");
    if (it.mark.ask.reasks !== 12 || it.mark.ask.state !== "safe" || it.mark.ask.reasked_at) throw new Error(JSON.stringify(it.mark.ask));
  });
  await check("a second run does nothing new", async () => {
    const before = (await byName("auto-case")).mark.ask.loop.at;
    await new Promise((r) => setTimeout(r, 4000));
    if ((await byName("auto-case")).mark.ask.loop.at !== before) throw new Error("the note changed");
  });
  await shots("auto", '.rv-item[data-id$="w60:p1"]');
  await shots("live", '.rv-item[data-id$="w2:p1"]');
  await check("the Peers setting shows on", async () => {
    const { browser, page } = await launch({ width: 1280, height: 800, dark: true });
    try {
      await page.goto(`${base}/#/peers/devbox`, { waitUntil: "load" });
      await page.waitForSelector("#pr-sweep-del", { timeout: 15000 });
      if (!(await page.locator("#pr-sweep-del").isChecked())) throw new Error("not on");
      await page.locator("#pr-sweep-del").scrollIntoViewIfNeeded();
      await page.screenshot({ path: `${out}/setting-1280.png` });
    } finally { await browser.close(); }
  });
}
console.log(failed ? `${failed} failed` : "all passed");
process.exit(failed ? 1 : 0);
