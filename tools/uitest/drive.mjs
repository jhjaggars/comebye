// Drives the web app in headless Chrome (mobile viewport) against the fake
// herdr: after a prompt is sent, the reply must show without re-selecting
// the agent, in the chat view and in the raw terminal view, and switching
// agents must not show the previous agent's view.
import { launch, mockLaunchRoutes, dumpFailure, stamped } from "./lib.mjs";
import { releaseView } from "./relfix.mjs";
import { boardView } from "./boardfix.mjs";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
const WAIT = Number(process.env.REPLY_WAIT_MS || 9000);

const { browser, page } = await launch({ dark: process.env.SCHEME !== "light" });
const log = [];
const note = stamped(log);
page.on("console", (m) => note.push(`console.${m.type()}: ${m.text()}`));
page.on("pageerror", (e) => note.push(`pageerror: ${e.message}`));
page.on("response", (r) => { if (r.url().includes("/api/") && !r.url().includes("/api/events")) note.push(`${r.status()} ${r.request().method()} ${r.url().replace(base, "")}`); });
page.on("requestfailed", (r) => { if (r.url().includes("/api/")) note.push(`FAILED ${r.method()} ${r.url().replace(base, "")}: ${r.failure()?.errorText}`); });
page.on("framenavigated", (f) => { if (f === page.mainFrame()) note.push(`navigated ${f.url().replace(base, "")}`); });

// The smoke set (SMOKE=1, tools/ci/browser.sh smoke): whole groups of checks, by the start of their names, covering
// every screen's main action (about 90 of drive.mjs's 220 checks). Checks within a group, and some across groups, build on each other's state (the model
// picker check opens the sheet the one before it set up), so a smoke set must keep a group whole or leave it out.
const SMOKE_SET = [
  "chat:", "raw:", "switch:", "model:",                                      // chat, raw terminals, the launch sheet, model switch
  "mark:", "fleet menu:", "brief:", "status:", "board:",                      // marks, the fleet menu, the briefing, Status, the board
  "desktop", "waiting:", "keys:", "phone:", "prompt bar:",                // the desktop and phone layouts, keys, the prompt bar
  "to-do:", "to-do accordion", "to-do choices", "to-do markdown",             // To do
  "search page:", "host chips:", "releases live at", "theme:",                // search, hosts, releases, themes
];
const results = [];
async function check(name, fn) {
  if (process.env.ONLY && !name.includes(process.env.ONLY)) return; // ONLY=latency: run one group while iterating
  if (process.env.SMOKE && !SMOKE_SET.some((p) => name.startsWith(p))) return; // SMOKE=1: the pre-submit set, one check per screen
  const t0 = Date.now();
  try { await fn(); results.push([name, true, ""]); }
  catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); await dumpFailure({ page, out, name, err: e, log }); }
  if (process.env.TIMES) console.log(`  ${((Date.now() - t0) / 1000).toFixed(1)}s ${name.slice(0, 110)}`); // TIMES=1: how long each check took
  // A screenshot after every check (SHOTS=1, for looking around); a failed check already dumped its own in dumpFailure.
  if (process.env.SHOTS) await page.screenshot({ path: `${out}/${name.replace(/\W+/g, "-")}.png` }).catch(() => {});
}
const visible = (sel) => page.locator(sel).isVisible();
const PANES = { alpha: "w1:p1", bravo: "w2:p1" };
// Opens an agent by its deep link, as tapping its row would.
async function openAgent(name) {
  await page.goto(base + "/#/agent/" + encodeURIComponent(PANES[name]), { waitUntil: "load" });
  await page.waitForSelector("#detail", { state: "visible" });
}
async function send(text) {
  await page.locator("#prompt-text").click();
  await page.keyboard.type(text);
  await page.locator("#btn-send").click();
}

await check("chat: reply shows after send without re-selecting", async () => {
  await openAgent("alpha");
  await page.waitForFunction(() => document.querySelector("#chat")?.innerText.includes("alpha says hi"), null, { timeout: 5000 });
  await send("ping1");
  await page.waitForFunction(() => document.querySelector("#chat")?.innerText.includes("reply to: ping1"), null, { timeout: WAIT });
});

await check("chat: Send to To do sits under the last reply only, and says what it found", async () => {
  await openAgent("alpha");
  await page.waitForSelector("#chat .msg.assistant .msg-todo", { timeout: 5000 });
  const n = await page.locator("#chat .msg-todo").count();
  if (n !== 1) throw new Error(`${n} Send to To do buttons, want 1 (the last reply's)`);
  const underLast = await page.evaluate(() => {
    const replies = document.querySelectorAll("#chat .msg.assistant");
    return !!replies[replies.length - 1].querySelector(".msg-todo");
  });
  if (!underLast) throw new Error("the button is not under the last reply");
  // The fake agent's replies ask nothing: the hub says so.
  await page.locator("#chat .msg-todo").click();
  await page.waitForFunction(() => /No questions/.test(document.querySelector("#toast")?.textContent || ""), null, { timeout: WAIT });
});

await check("raw: reply shows after send without re-selecting", async () => {
  await openAgent("bravo");
  await page.waitForFunction(() => document.querySelector("#output")?.innerText.includes("bravo ready"), null, { timeout: 5000 });
  await send("ping2");
  await page.waitForFunction(() => document.querySelector("#output")?.innerText.includes("reply to: ping2"), null, { timeout: WAIT });
});

await check("switch: chat agent after a raw agent shows its own chat", async () => {
  // still on bravo (raw view): go back and open alpha
  await page.locator("#back").click();
  await page.waitForSelector("#list", { state: "visible" });
  await page.evaluate((h) => { location.hash = h; }, "#/agent/" + encodeURIComponent(PANES.alpha));
  await page.waitForSelector("#detail", { state: "visible" });
  await page.waitForFunction(() => document.querySelector("#chat")?.innerText.includes("alpha says hi"), null, { timeout: 5000 });
  if (!(await visible("#chat"))) throw new Error("#chat is hidden");
  if (await visible("#output")) throw new Error("#output (raw view) still visible");
});

// ---- launch sheet: model picker (mocked launch options and model lists, so
// the checks do not depend on which agents the host has installed) ----
let launchPosts = [];
const modelUrls = [];
await mockLaunchRoutes(page, base, { onModels: (u) => modelUrls.push(u), onLaunch: (b) => launchPosts.push(b) });
const modelVal = () => page.locator("#ln-model-val").innerText();
async function openSheet() {
  await page.goto(base, { waitUntil: "load" });
  await page.waitForSelector("#btn-launch", { state: "visible" });
  await page.locator("#btn-launch").click();
  await page.waitForSelector("#launch", { state: "visible" });
  await page.waitForSelector("#ln-model-block", { state: "visible" });
  await page.locator('#ln-list .ln-dir.recent').first().click(); // choose the recent directory
}
const noHScroll = async () => {
  const w = await page.evaluate(() => { const b = document.querySelector("#launch .ln-body"); return [b.scrollWidth, b.clientWidth, document.documentElement.scrollWidth, innerWidth]; });
  if (w[0] > w[1] || w[2] > w[3]) throw new Error("horizontal overflow " + w.join("/"));
};

await page.evaluate(() => localStorage.clear()).catch(() => {});
await check("model: default is shown and marked, no flag", async () => {
  await openSheet();
  await page.waitForFunction(() => document.querySelector("#ln-model-val")?.innerText === "Default (homelab/thinking)", null, { timeout: 5000 });
  if (!(await page.locator("#ln-model-note").innerText()).includes("no flag")) throw new Error("no default note");
  if (modelUrls[0] !== "/api/models?kind=pi") throw new Error("models url " + modelUrls[0]);
  await noHScroll();
});

await check("model: picker lists models grouped by provider with the default marked", async () => {
  await page.locator("#ln-model-btn").click();
  await page.waitForSelector("#ln-picker", { state: "visible" });
  const heads = await page.locator("#ln-mlist .ln-head").allInnerTexts();
  if (heads.join().toLowerCase() !== "homelab,openai-codex") throw new Error("groups " + heads);
  const dflt = await page.locator("#ln-mlist .ln-model", { has: page.locator(".ln-badge.dflt") }).innerText();
  if (!dflt.includes("thinking") || !dflt.includes("default")) throw new Error("default row " + dflt);
  if (!(await page.locator('#ln-mlist [data-model=""]').innerText()).includes("No flag is passed")) throw new Error("no default row");
  // phone usability: every row is a comfortable tap target, nothing overflows
  const hs = await page.locator("#ln-mlist .ln-model").evaluateAll((els) => els.map((e) => e.getBoundingClientRect().height));
  if (Math.min(...hs) < 44) throw new Error("row too small " + Math.min(...hs));
  await noHScroll();
});

await check("model: search filters, picking sets the model", async () => {
  await page.locator("#ln-model-q").fill("luna");
  const rows = await page.locator("#ln-mlist .ln-model").allInnerTexts();
  if (rows.length !== 1 || !rows[0].includes("gpt-5.6-luna")) throw new Error("rows " + JSON.stringify(rows));
  await page.locator("#ln-model-q").fill("zzz");
  if (!(await page.locator("#ln-mlist .ln-empty").innerText()).includes("No model matches")) throw new Error("no empty state");
  await page.locator("#ln-model-q").fill("gpt");
  await page.locator('#ln-mlist [data-model="openai-codex/gpt-5.5"]').click();
  if ((await modelVal()) !== "openai-codex/gpt-5.5") throw new Error("value " + (await modelVal()));
  if (await visible("#ln-picker")) throw new Error("picker stayed open");
});

await check("model: pi has a thinking selector", async () => {
  const chips = await page.locator("#ln-think-chips button").allInnerTexts();
  if (chips[0] !== "Default (minimal)" || !chips.includes("xhigh")) throw new Error("chips " + chips);
  await page.locator('#ln-think-chips [data-think="high"]').click();
  if ((await page.locator("#ln-model-think").innerText()) !== "high") throw new Error("level not shown beside the model");
  if ((await page.locator('#ln-think-chips [aria-pressed="true"]').innerText()) !== "high") throw new Error("not selected");
});

await check("model: launch sends model and thinking", async () => {
  launchPosts = [];
  await page.locator("#ln-go").click();
  await page.waitForFunction(() => document.querySelector("#launch")?.hidden, null, { timeout: 5000 });
  const b = launchPosts[0];
  if (!b || b.kind !== "pi" || b.model !== "openai-codex/gpt-5.5" || b.thinking !== "high" || b.dir !== "/home/u/code/proj") throw new Error("body " + JSON.stringify(b));
});

await check("model: the last choice per kind is remembered on the device", async () => {
  await openSheet();
  await page.waitForFunction(() => document.querySelector("#ln-model-val")?.innerText === "openai-codex/gpt-5.5", null, { timeout: 5000 });
  if ((await page.locator('#ln-think-chips [aria-pressed="true"]').innerText()) !== "high") throw new Error("thinking not remembered");
  // another kind starts from its own default, and keeps its own choice
  await page.locator('#ln-kinds [data-kind="claude"]').click();
  await page.waitForFunction(() => document.querySelector("#ln-model-val")?.innerText === "Default", null, { timeout: 5000 });
  if (await visible("#ln-thinking")) throw new Error("thinking selector shown for claude");
  await page.locator("#ln-model-btn").click();
  await page.locator('#ln-mlist [data-model="sonnet"]').click();
  await page.locator('#ln-kinds [data-kind="pi"]').click();
  await page.waitForFunction(() => document.querySelector("#ln-model-val")?.innerText === "openai-codex/gpt-5.5", null, { timeout: 5000 });
  await page.locator('#ln-kinds [data-kind="claude"]').click();
  await page.waitForFunction(() => document.querySelector("#ln-model-val")?.innerText === "sonnet", null, { timeout: 5000 });
});

await check("model: a kind with no list accepts a typed name; exact lists do not", async () => {
  await page.locator("#ln-model-btn").click();
  await page.locator("#ln-model-q").fill("my-model-1");
  await page.locator('#ln-mlist [data-model="my-model-1"]').click();
  if ((await modelVal()) !== "my-model-1") throw new Error("typed name not used");
  await page.locator('#ln-kinds [data-kind="pi"]').click();
  await page.locator("#ln-model-btn").click();
  await page.locator("#ln-model-q").fill("not-a-model");
  if (await page.locator('#ln-mlist [data-model="not-a-model"]').count()) throw new Error("offered a typed name for pi's exact list");
});

await check("model: Escape closes the list before the sheet", async () => {
  await page.keyboard.press("Escape");
  if (await visible("#ln-picker")) throw new Error("list still open");
  if (!(await visible("#launch"))) throw new Error("sheet closed too");
});

await check("model: a kind without model support hides the picker and sends none", async () => {
  await page.locator('#ln-kinds [data-kind="cursor"]').click();
  if (await visible("#ln-model-block")) throw new Error("picker shown for cursor");
  launchPosts = [];
  await page.locator("#ln-go").click();
  await page.waitForFunction(() => document.querySelector("#launch")?.hidden, null, { timeout: 5000 });
  const b = launchPosts[0];
  if (!b || b.kind !== "cursor" || "model" in b || "thinking" in b) throw new Error("body " + JSON.stringify(b));
});

// ---- switching a running agent's model (alpha is an idle claude agent) ----
let switchPosts = [];
await page.route(/\/agents\/[^/]+\/model$/, (r) => {
  switchPosts.push({ url: r.request().url().replace(base, ""), body: JSON.parse(r.request().postData() || "{}") });
  r.fulfill({ json: { agent: { pane_id: "w1:p1" }, commands: ["/model sonnet"] } });
});
await check("switch: the agent menu offers Switch model and opens the list", async () => {
  await openAgent("alpha");
  await page.locator("#d-more").click();
  await page.locator("#d-model").click();
  await page.waitForSelector("#switch", { state: "visible" });
  await page.waitForFunction(() => document.querySelectorAll("#sw-list .ln-model").length > 0, null, { timeout: 5000 });
  if (!(await page.locator("#sw-note").innerText()).includes("idle")) throw new Error("note " + (await page.locator("#sw-note").innerText()));
  if (!(await page.locator("#sw-go").isDisabled())) throw new Error("Switch enabled before choosing");
  if (await visible("#sw-thinking")) throw new Error("thinking shown for claude");
  const heads = (await page.locator("#sw-list .ln-head").allInnerTexts()).join().toLowerCase();
  if (heads !== "aliases,full ids") throw new Error("groups " + heads);
  if (await page.locator('#sw-list [data-model=""]').count()) throw new Error("a Default row for a running agent");
  const hs = await page.locator("#sw-list .ln-model").evaluateAll((els) => els.map((e) => e.getBoundingClientRect().height));
  if (Math.min(...hs) < 44) throw new Error("row too small");
  const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
  if (w[0] > w[1]) throw new Error("horizontal overflow");
});

await check("switch: choosing a model sends it to the agent", async () => {
  await page.locator("#sw-q").fill("son");
  await page.locator('#sw-list [data-model="sonnet"]').click();
  if ((await page.locator("#sw-go").innerText()) !== "Switch to sonnet") throw new Error("button " + (await page.locator("#sw-go").innerText()));
  await page.locator("#sw-go").click();
  await page.waitForFunction(() => document.querySelector("#switch")?.hidden, null, { timeout: 5000 });
  const p = switchPosts[0];
  if (!p || !/agents\/.*\/model$/.test(p.url) || p.body.model !== "sonnet" || "thinking" in p.body) throw new Error("post " + JSON.stringify(p));
});

await check("switch: Escape closes the sheet and the chat is still open", async () => {
  await page.locator("#d-more").click();
  await page.locator("#d-model").click();
  await page.waitForSelector("#switch", { state: "visible" });
  await page.keyboard.press("Escape");
  await page.waitForFunction(() => document.querySelector("#switch")?.hidden, null, { timeout: 3000 });
  if (!(await visible("#detail"))) throw new Error("detail closed too");
});

// ---- marking a session ready to remove, and the sweep ----
const noOverflow = async () => {
  const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
  if (w[0] > w[1]) throw new Error("horizontal overflow");
};
let nativeDialogs = 0;
page.on("dialog", (d) => { nativeDialogs++; d.dismiss().catch(() => {}); });
const rowOf = (pane) => page.locator(`#agents .agent[data-id$="${pane}"]`);
async function toList() {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent", { state: "visible" });
  // The ledger is known once the hub has answered.
  await page.waitForFunction(() => ledger.on === true, null, { timeout: 8000 });
}
async function fleetMenuOpen() {
  await page.locator("#btn-fleet").click();
  await page.waitForSelector("#f-menu", { state: "visible", timeout: 3000 });
}

await check("mark: the list row has a visible menu button of tap size", async () => {
  await toList();
  const b = rowOf(PANES.alpha).locator(".row-more");
  if (!(await b.isVisible())) throw new Error("no menu button on the row");
  const box = await b.boundingBox();
  if (box.width < 44 || box.height < 44) throw new Error("tap target " + box.width + "x" + box.height);
  await noOverflow();
});

await check("mark: row menu opens an in-app sheet (no window.prompt) and marks with the reason", async () => {
  await rowOf(PANES.alpha).locator(".row-more").click();
  await page.waitForSelector("#row-menu", { state: "visible" });
  if ((await page.locator("#row-mark").innerText()).trim() !== "Mark ready to remove…") throw new Error("menu item " + (await page.locator("#row-mark").innerText()));
  await page.locator("#row-mark").click();
  await page.waitForSelector("#mk", { state: "visible" });
  await noOverflow();
  await page.locator("#mk-reason").fill("merged and deployed");
  await page.locator("#mk-go").click();
  await page.waitForFunction(() => document.querySelector("#mk")?.hidden, null, { timeout: 5000 });
  await page.waitForFunction(() => /Ready to remove \(1\)/.test(document.querySelector("#rtr-banner")?.textContent || ""), null, { timeout: 8000 });
  await page.waitForFunction((p) => document.querySelector(`#agents .agent[data-id$="${p}"]`)?.classList.contains("marked"), PANES.alpha, { timeout: 8000 });
  const lbl = await rowOf(PANES.alpha).locator(".row-more").getAttribute("aria-label");
  if (!/^Unmark/.test(lbl)) throw new Error("aria-label " + lbl);
  if (nativeDialogs) throw new Error("a native dialog opened");
});

await check("mark: the detail shows a marked badge with the reason and an Unmark button", async () => {
  await openAgent("alpha");
  await page.waitForSelector("#d-markbar .lg-chip.rtr", { state: "visible", timeout: 8000 });
  const t = await page.locator("#d-markbar").innerText();
  if (!t.includes("merged and deployed")) throw new Error("bar text " + t);
  await noOverflow();
  await page.locator('#d-markbar [data-mb="unmark"]').click();
  await page.waitForFunction(() => document.querySelector("#d-markbar")?.hidden, null, { timeout: 8000 });
  if (await page.locator("#d-markbar .lg-chip.rtr").count()) throw new Error("badge still shown");
  if (await page.locator('#d-markbar [data-mb="mark"]').count()) throw new Error("persistent mark button still present");
});

await check("mark: the detail's ⋯ menu marks and the row menu unmarks", async () => {
  await page.locator("#d-more").click();
  if ((await page.locator("#d-mark").innerText()).trim() !== "Mark ready to remove…") throw new Error("menu item " + (await page.locator("#d-mark").innerText()));
  await page.locator("#d-mark").click();
  await page.waitForSelector("#mk", { state: "visible" });
  await page.locator("#mk-go").click(); // no reason: optional
  await page.waitForSelector("#d-markbar .lg-chip.rtr", { state: "visible", timeout: 8000 });
  await page.locator("#back").click();
  await page.waitForSelector("#list", { state: "visible" });
  await rowOf(PANES.alpha).locator(".row-more").click();
  if ((await page.locator("#row-mark").innerText()).trim() !== "Unmark (keep this session)") throw new Error("menu item " + (await page.locator("#row-mark").innerText()));
  await page.locator("#row-mark").click();
  await page.waitForFunction((p) => !document.querySelector(`#agents .agent[data-id$="${p}"]`)?.classList.contains("marked"), PANES.alpha, { timeout: 8000 });
  await page.waitForFunction(() => /^Look for finished agents/.test(document.querySelector("#rtr-banner")?.textContent || ""), null, { timeout: 8000 }); // at zero the phone banner offers the sweep
});

// Unmark tapped while the mark is still being saved (the badge already shows from the optimistic state) must
// wait for the save and then unmark, not be dropped. The save is held for 600 ms so the tap always lands inside it.
await check("mark: Unmark tapped while the mark is still being saved is not dropped", async () => {
  const slow = async (r) => { if (r.request().method() === "POST") await new Promise((x) => setTimeout(x, 600)); r.fallback(); };
  await page.route(/\/api\/marks$/, slow);
  try {
    await toList();
    await rowOf(PANES.alpha).locator(".row-more").click();
    await page.locator("#row-mark").click();
    await page.locator("#mk-go").click();
    await page.waitForFunction(() => /Ready to remove \(1\)/.test(document.querySelector("#rtr-banner")?.textContent || ""), null, { timeout: 8000 });
    await page.evaluate((p) => { location.hash = "#/agent/" + encodeURIComponent(p); }, PANES.alpha);
    await page.waitForSelector("#d-markbar .lg-chip.rtr", { state: "visible", timeout: 8000 });
    if (!(await page.evaluate(() => ledgerOpt.busyKeys().length))) throw new Error("the save was not on the wire: the check does not test what it says");
    await page.locator('#d-markbar [data-mb="unmark"]').click();
    await page.waitForFunction(() => document.querySelector("#d-markbar")?.hidden, null, { timeout: 8000 });
  } finally { await page.unroute(/\/api\/marks$/, slow); }
  await page.waitForFunction(() => !ledger.marked.size, null, { timeout: 8000 });
});

const sweepPosts = [];
await page.route(/\/api\/sweep/, (r) => {
  const body = JSON.parse(r.request().postData() || "{}");
  sweepPosts.push(body);
  const ready = { id: "uitest/w9:p1", host: "uitest", name: "fix-login", branch: "fix/login", class: "ready", reason: "Sweep: finished; worktree clean; every commit of fix/login is in origin/main; nothing unpushed" };
  const look = { id: "uitest/w8:p1", host: "uitest", name: "feat-search", branch: "feat/search", class: "needs_look", reason: "pushed but not merged: 2 commits not in origin/main (an open PR?)" };
  const delivered = { id: "uitest/w7:p1", host: "uitest", name: "stt-research", branch: "research/stt", class: "delivered", reason: "work delivered in the report; branch has 5 local commits (on no remote)" };
  const gone = { id: "arch1/w1:p1", host: "arch1", name: "smoke", branch: "smoke/x", class: "gone", reason: "host arch1 is not in this hub's fleet any more", ad_hoc: true };
  r.fulfill({ json: { dry_run: !!body.dry_run, ready: [{ ...ready, marked: !body.dry_run }], needs_look: [look], delivered: [delivered], gone: [{ ...gone, marked: !body.dry_run }], skipped: [{ id: "x" }] } });
});

await check("remove page: sweep: the button on Ready to remove runs a dry run listing ready and needs-a-look", async () => {
  await page.goto(base + "/#/remove", { waitUntil: "load" });
  await page.waitForSelector("#review", { state: "visible" });
  await page.locator("#rv-sweep").click();
  await page.waitForFunction(() => document.querySelector("#rv-sweepbox")?.innerText.includes("fix-login"), null, { timeout: 5000 });
  const t = await page.locator("#rv-sweepbox").innerText();
  if (!t.includes("feat-search") || !t.includes("open PR")) throw new Error("list " + t);
  if (!sweepPosts.length || sweepPosts[0].dry_run !== true) throw new Error("first call was not a dry run: " + JSON.stringify(sweepPosts));
  // ready + gone: a plain sweep marks both; delivered is never in that count
  if ((await page.locator("#sw-mark").innerText()).trim() !== "Mark 2 ready to remove") throw new Error("button " + (await page.locator("#sw-mark").innerText()));
  await noOverflow();
});

await check("remove page: sweep: delivered rows have their own button (a discard mark); gone rows are listed with the ready ones", async () => {
  const t = (await page.locator("#rv-sweepbox").innerText()).toLowerCase(); // status chips are upper-cased by CSS
  if (!t.includes("stt-research") || !t.includes("5 local commits") || !t.includes("smoke") || !t.includes("no handoff")) throw new Error("list " + t.replace(/\s+/g, " "));
  if (!/gone: nothing left to lose or check, marked with the ready ones/.test(t)) throw new Error("gone heading " + t.replace(/\s+/g, " "));
  if (await page.locator('#rv-sweepbox [data-act="sw-gone"]').count()) throw new Error("gone has a button of its own");
  const marks = [];
  await page.route(/\/api\/marks$/, (r) => {
    const b = JSON.parse(r.request().postData() || "{}");
    if (b.agent_id !== "uitest/w7:p1") return r.fallback();
    marks.push(b);
    r.fulfill({ json: { mark: {} } });
  });
  const btn = page.locator('#rv-sweepbox [data-act="sw-delivered"]');
  const bb = await btn.boundingBox();
  if (!bb || bb.height < 30) throw new Error("button too small " + JSON.stringify(bb));
  await noOverflow();
  const before = sweepPosts.length;
  await btn.click();
  await page.waitForTimeout(800);
  if (marks.length !== 1 || marks[0].discard !== true || !marks[0].reason.startsWith("Sweep: work delivered")) throw new Error("marks " + JSON.stringify(marks));
  if (sweepPosts.length !== before + 1 || sweepPosts[before].dry_run !== true) throw new Error("no refresh after marking: " + JSON.stringify(sweepPosts));
});

await check("remove page: sweep: confirming marks, and the result stays on the page", async () => {
  await page.locator("#sw-mark").click();
  await page.waitForFunction(() => document.querySelector("#rv-sweepbox")?.innerText.includes("Marked 2 ready to remove"), null, { timeout: 5000 });
  if (sweepPosts.length !== 3 || sweepPosts[2].dry_run !== false) throw new Error("marking call " + JSON.stringify(sweepPosts));
  if (!(await page.locator("#rv-sweepbox").innerText()).toLowerCase().includes("marked ready to remove")) throw new Error("no marked heading");
  if (!(await page.evaluate(() => location.hash === "#/remove"))) throw new Error("left the page");
  await page.locator("#rv-close").click();
  await page.waitForSelector("#review", { state: "hidden" });
});

// A marked "gone" row (host not in the fleet): the review says there is
// nothing left to check, counts it as safe, and removing it posts a confirm.
await check("remove page: a gone row is safe, says it only clears the record, and removing it confirms", async () => {
  const gone = { id: "arch1/w1:p1", host: "arch1", name: "smoke", branch: "smoke/x", worktree: "/w/smoke", mark: { agent_id: "arch1/w1:p1", by: "sweep", reason: "Sweep: host arch1 is not in this hub's fleet any more", at: new Date().toISOString() },
    live: { present: false }, warnings: [], notes: ["host arch1 is not in this hub's fleet: nothing there can be checked or removed; confirming only clears the record"], safe: true };
  const confirms = [];
  let removed = false;
  await page.route(/\/api\/removals(\/confirm)?$/, (r) => {
    if (r.request().method() === "POST") { confirms.push(JSON.parse(r.request().postData() || "{}")); removed = true; return r.fulfill({ json: { results: [{ id: gone.id, ok: true, result: { closed: "", worktree_removed: false, kept: "host arch1 is not in this hub's fleet: only the record was cleared" } }] } }); }
    r.fulfill({ json: { removals: removed ? [] : [gone] } });
  });
  await page.goto(base + "/#/remove", { waitUntil: "load" });
  await page.waitForFunction(() => document.querySelector("#rv-list")?.innerText.includes("smoke"), null, { timeout: 5000 });
  const t = await page.locator("#rv-list").innerText();
  if (!t.includes("confirming only clears the record")) throw new Error("no note: " + t);
  if (t.toLowerCase().includes("checks failed")) throw new Error("checks failed shown: " + t);
  if (!/Remove all safe \(1\)/.test(await page.locator("#rv-all-safe").innerText())) throw new Error("not counted as safe");
  await noOverflow();
  await page.locator('#rv-list [data-act="remove"]').first().click();
  await page.locator('#rv-list [data-act="confirm-yes"]').first().click();
  await page.waitForFunction(() => /removed|Removed/.test(document.querySelector("#rv-list")?.innerText || "") || !document.querySelector("#rv-list")?.innerText.includes("smoke/x"), null, { timeout: 5000 });
  if (confirms.length !== 1 || confirms[0].ids?.[0] !== gone.id) throw new Error("confirm " + JSON.stringify(confirms));
  await page.unroute(/\/api\/removals(\/confirm)?$/);
});

// The briefing card: collapsed by default, a new briefing keeps it collapsed,
// and expanding is a persistent choice that survives new briefings.
async function publishBrief(text) {
  const r = await fetch(base + "/api/brief", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ markdown: text }) });
  if (!r.ok) throw new Error("publish brief: " + r.status);
  await new Promise((r) => setTimeout(r, 1100)); // briefings have a timestamp to the second
}
const briefState = () => page.evaluate(() => ({
  hidden: document.querySelector("#briefing").hidden,
  expanded: document.querySelector("#briefing .bf-toggle")?.getAttribute("aria-expanded"),
  body: !!document.querySelector("#briefing .bf-body") && !document.querySelector("#briefing .bf-body").hidden,
  preview: document.querySelector("#briefing .bf-preview")?.textContent || "",
  text: document.querySelector("#briefing")?.innerText || "",
}));
async function reloadBrief() {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#briefing .bf-toggle", { state: "visible", timeout: 8000 });
}
await check("brief: collapsed by default, with a preview, and stays collapsed on a new briefing", async () => {
  await publishBrief("## Status\n\n**All quiet** on the western front\n\nlong detail one");
  await page.goto(base + "/", { waitUntil: "load" });
  await page.evaluate(() => { try { localStorage.clear(); } catch {} });
  await reloadBrief();
  let st = await briefState();
  if (st.expanded !== "false" || st.body) throw new Error("not collapsed by default: " + JSON.stringify(st));
  if (st.preview !== "Status") throw new Error("preview is " + JSON.stringify(st.preview));
  await publishBrief("**Second update** is in\n\nlong detail two");
  await reloadBrief();
  st = await briefState();
  if (st.expanded !== "false" || st.body) throw new Error("new briefing expanded the card: " + JSON.stringify(st));
  if (st.preview !== "Second update is in") throw new Error("preview is " + JSON.stringify(st.preview));
  if (st.text.includes("long detail two")) throw new Error("body text shown while collapsed");
});
await check("brief: expanding persists across a reload and a new briefing; collapsing sticks too", async () => {
  await page.locator("#briefing .bf-toggle").click();
  let st = await briefState();
  if (st.expanded !== "true" || !st.body || st.preview) throw new Error("tap did not expand: " + JSON.stringify(st));
  await publishBrief("Third update\n\nlong detail three");
  await reloadBrief();
  st = await briefState();
  if (st.expanded !== "true" || !st.body || !st.text.includes("long detail three")) throw new Error("new briefing collapsed an expanded card: " + JSON.stringify(st));
  await page.locator("#briefing .bf-toggle").click();
  await publishBrief("Fourth update\n\nlong detail four");
  await reloadBrief();
  st = await briefState();
  if (st.expanded !== "false" || st.body) throw new Error("collapse did not stick: " + JSON.stringify(st));
});

// ---- the header's fleet menu ("⋯", top right of the list) ----
await check("fleet menu: the ⋯ is visible at 390px, on top, inside the header, without overflow", async () => {
  await toList();
  const r = await page.evaluate(() => {
    const b = document.querySelector("#btn-fleet"), h = document.querySelector("#list header");
    const r = b.getBoundingClientRect(), hr = h.getBoundingClientRect();
    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    const c = document.querySelector("#count").getBoundingClientRect();
    const bell = document.querySelector("#btn-notify").getBoundingClientRect();
    return { x: r.x, right: r.right, w: r.width, h: r.height, top: r.top, bottom: r.bottom, hb: hr.bottom, onTop: hit === b || b.contains(hit), vw: innerWidth,
      countRight: c.right, bellLeft: bell.left, bellRight: bell.right, countText: document.querySelector("#count").innerText };
  });
  if (!r.onTop || r.x < 0 || r.right > r.vw || r.w < 40 || r.h < 40 || r.bottom > r.hb + 1) throw new Error("⋯ not usable: " + JSON.stringify(r));
  if (r.bellRight > r.x + 1) throw new Error("⋯ overlaps the bell: " + JSON.stringify(r));
  if (r.countRight > r.bellLeft + 1) throw new Error("count runs under the buttons: " + JSON.stringify(r));
  if (r.h > 60) throw new Error("header wrapped: " + JSON.stringify(r));
  await noOverflow();
});
await check("fleet menu: opens with its items and the right aria; Escape closes it", async () => {
  await fleetMenuOpen();
  if (await page.locator("#btn-fleet").getAttribute("aria-expanded") !== "true") throw new Error("aria-expanded not true");
  if (await page.locator("#btn-fleet").getAttribute("aria-haspopup") !== "menu") throw new Error("aria-haspopup");
  const t = (await page.locator("#f-menu button:visible:not(.pop-x)").allInnerTexts()).map((x) => x.trim().replace(/\s+/g, " "));
  // View (Theme, Font, Notifications), Hub (the briefing item when there is one, History, Pages, Usage, Refresh): the schedule lives on Status; sweep and removals are the Ready to remove page.
  const want = [/^Theme /, /^Font /, /^Notifications/, /briefing/i, /^History/, /^Pages/, /^Usage/, /^Refresh/];
  if (t.length !== want.length || want.some((re, i) => !re.test(t[i]))) throw new Error("items " + JSON.stringify(t));
  if (/schedule/i.test(t.join("|"))) throw new Error("the schedule is still in the menu");
  const grp = await page.locator("#f-menu .m-grp").allInnerTexts();
  if (grp.join(",").toLowerCase() !== "view,hub") throw new Error("groups " + grp);
  const box = await page.locator("#f-menu").boundingBox();
  if (box.x < 0 || box.x + box.width > 390) throw new Error("menu off-screen " + JSON.stringify(box));
  await page.keyboard.press("Escape");
  await page.waitForSelector("#f-menu", { state: "hidden", timeout: 2000 });
  if (await page.locator("#btn-fleet").getAttribute("aria-expanded") !== "false") throw new Error("aria-expanded not reset");
});
await check("fleet menu: Pages and Usage open their screens on a phone", async () => {
  for (const [id, hash, screen] of [["f-pages", "#/pages", "#pages"], ["f-usage", "#/usage", "#usage"]]) {
    await fleetMenuOpen();
    await page.locator("#" + id).click();
    await page.waitForSelector("#f-menu", { state: "hidden", timeout: 2000 });
    await page.waitForSelector(screen, { state: "visible", timeout: 3000 });
    if (!(await page.evaluate(() => location.hash)).startsWith(hash)) throw new Error(id + ": hash " + await page.evaluate(() => location.hash));
    await toList();
  }
});
await check("theme: System paints the OS scheme (Comebye Light/Dark) on load; the menu item opens the theme panel", async () => {
  const bg = () => page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  const attr = () => page.evaluate(() => document.documentElement.getAttribute("data-theme"));
  const dark = "rgb(16, 18, 24)", light = "rgb(243, 244, 247)";
  const light_os = process.env.SCHEME === "light";
  await page.evaluate(() => { try { localStorage.removeItem("comebye.theme"); } catch {} });
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForSelector("#btn-fleet");
  if (await attr() !== (light_os ? "light" : "dark") || await bg() !== (light_os ? light : dark)) throw new Error("system: " + await attr() + " " + await bg());
  await fleetMenuOpen();
  if (!/Theme\s+System/.test(await page.locator("#f-theme").innerText())) throw new Error("label " + await page.locator("#f-theme").innerText());
  await page.locator("#f-theme").click();
  await page.waitForSelector("#theme-panel", { state: "visible" });
  if ((await page.locator("#tp-list .tp-opt").count()) < 3) throw new Error("no options");
  await page.keyboard.press("Escape");
  await page.waitForSelector("#theme-panel", { state: "hidden" });
});
await check("fleet menu: the sweep and review items are gone; the removals banner opens #/remove", async () => {
  if (await page.locator("#sweep-open, #f-sweep, #f-review").count()) throw new Error("a sweep/review item is still in the page");
  if ((await page.locator("#agents, main").first().innerText()).includes("Sweep for finished agents")) throw new Error("sweep row text still on the main screen");
  // Mark one agent so the banner shows.
  await rowOf(PANES.alpha).locator(".row-more").click();
  await page.locator("#row-mark").click();
  await page.waitForSelector("#mk", { state: "visible" });
  await page.locator("#mk-go").click();
  await page.waitForFunction(() => /Ready to remove \(1\)/.test(document.querySelector("#rtr-banner")?.textContent || ""), null, { timeout: 8000 });
  if (!(await page.locator("#rtr-banner").innerText()).includes("Ready to remove (1)")) throw new Error("banner text");
  await page.locator("#rtr-banner").click();
  await page.waitForSelector("#review", { state: "visible", timeout: 5000 });
  if ((await page.evaluate(() => location.hash)) !== "#/remove") throw new Error("hash " + (await page.evaluate(() => location.hash)));
  await page.locator("#rv-close").click();
  await page.waitForSelector("#review", { state: "hidden" });
  await rowOf(PANES.alpha).locator(".row-more").click();
  await page.locator("#row-mark").click(); // unmark
  await page.waitForFunction(() => /^Look for finished agents/.test(document.querySelector("#rtr-banner")?.textContent || ""), null, { timeout: 8000 }); // at zero the phone banner offers the sweep
});
await check("remove page: at zero the phone banner reads \"Look for finished agents\" and opens #/remove", async () => {
  await toList();
  const t = (await page.locator("#rtr-banner").innerText()).trim().replace(/\s*\u203a$/, "");
  if (t !== "Look for finished agents") throw new Error("banner " + t);
  await page.locator("#rtr-banner").click();
  await page.waitForSelector("#review", { state: "visible", timeout: 5000 });
  if ((await page.evaluate(() => location.hash)) !== "#/remove") throw new Error("hash");
  await page.locator("#rv-close").click();
  await page.waitForSelector("#review", { state: "hidden" });
});
await check("fleet menu: Briefing item expands, collapses and shows a dismissed briefing again", async () => {
  await publishBrief("Menu briefing\n\nlong detail five");
  await reloadBrief();
  await page.evaluate(() => { try { localStorage.clear(); } catch {} });
  await reloadBrief();
  await page.waitForFunction(() => !document.querySelector("#f-brief").disabled, null, { timeout: 8000 });
  await fleetMenuOpen();
  if (!/^Expand/.test(await page.locator("#f-brief").innerText())) throw new Error("item " + await page.locator("#f-brief").innerText());
  await page.locator("#f-brief").click();
  if ((await briefState()).expanded !== "true") throw new Error("not expanded");
  await page.locator("#briefing .bf-dismiss").click();
  await page.waitForSelector("#briefing", { state: "hidden" });
  await fleetMenuOpen();
  if (!/dismissed/.test(await page.locator("#f-brief").innerText())) throw new Error("item " + await page.locator("#f-brief").innerText());
  await page.locator("#f-brief").click();
  await page.waitForSelector("#briefing", { state: "visible" });
});

// ---- the collapsed briefing preview: exactly two lines, none cut ----
// 1000px: wide, but below the desktop breakpoint (1100px), so still the card.
for (const width of [390, 1000]) {
  await check(`brief: a long first line clamps to exactly two lines, inside the card, at ${width}px`, async () => {
    await page.setViewportSize({ width, height: 844 });
    await publishBrief('fix-header-menu landed on main as 8ea6a16 after the coordinator marked "landed on main as" the worker finished, the sweep verified every commit of the branch is in origin/main and nothing is unpushed, so it is safe to remove the worktree and its branch.\n\nmore');
    await page.evaluate(() => { try { localStorage.clear(); } catch {} });
    await reloadBrief();
    const r = await page.evaluate(() => {
      const p = document.querySelector("#briefing .bf-preview"), card = document.querySelector("#briefing");
      const cs = getComputedStyle(p), lh = parseFloat(cs.lineHeight);
      const pr = p.getBoundingClientRect(), cr = card.getBoundingClientRect();
      // the preview's own box: padding included; no bottom padding may exist, or a third line shows in it
      return { lines: Math.round(pr.height / lh), pad: parseFloat(cs.paddingBottom), h: pr.height, lh, bottom: pr.bottom, cardBottom: cr.bottom,
        clamp: cs.webkitLineClamp, scrollH: p.scrollHeight, meta: document.querySelector(".bf-meta").innerText, metaCut: document.querySelector(".bf-meta").scrollWidth > document.querySelector(".bf-meta").clientWidth };
    });
    if (r.lines !== 2 || r.pad !== 0 || r.clamp !== "2") throw new Error("not a clean two-line clamp: " + JSON.stringify(r));
    if (r.scrollH <= r.h + 1) throw new Error("text did not overflow two lines; the fixture is too short: " + JSON.stringify(r));
    if (r.bottom > r.cardBottom) throw new Error("preview extends past the card: " + JSON.stringify(r));
    if (!/^\d+\w+ ago/.test(r.meta)) throw new Error("age is not first in the meta: " + r.meta);
    if (r.metaCut && width > 500) throw new Error("meta is truncated at " + width + "px: " + JSON.stringify(r));
  });
}
await page.setViewportSize({ width: 390, height: 844 });

// ---- agent view "⋯" menu: always at the top right, whatever the ledger says ----
async function moreBox() {
  return page.evaluate(() => {
    const b = document.querySelector("#d-more");
    const r = b.getBoundingClientRect(), cs = getComputedStyle(b);
    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    return { x: r.x, y: r.y, w: r.width, h: r.height, shown: cs.display !== "none" && cs.visibility !== "hidden", hidden: b.hidden, onTop: hit === b || b.contains(hit), vw: innerWidth };
  });
}
async function expectMore(label) {
  await page.waitForSelector("#d-more", { state: "visible", timeout: 3000 }).catch(() => { throw new Error(label + ": #d-more not visible"); });
  const b = await moreBox();
  if (!b.shown || b.hidden || b.w < 40 || b.h < 40) throw new Error(label + ": too small/hidden " + JSON.stringify(b));
  if (b.x < 0 || b.x + b.w > b.vw + 0.5 || b.y < 0) throw new Error(label + ": off-screen " + JSON.stringify(b));
  if (b.x + b.w < b.vw - 24) throw new Error(label + ": not at the right edge " + JSON.stringify(b));
  if (!b.onTop) throw new Error(label + ": covered by another element");
  return b;
}
async function menuItems() {
  await page.locator("#d-more").click();
  await page.waitForSelector("#d-menu", { state: "visible" });
  const items = await page.evaluate(() => [...document.querySelectorAll("#d-menu [role=menuitem], #d-menu .mark-state")]
    .filter((e) => e.offsetParent !== null).map((e) => ({ id: e.id, text: e.textContent.trim(), disabled: !!e.disabled })));
  await page.locator("#d-more").click();
  return items;
}
// A cold load of the agent's deep link, so the ledger starts unknown.
async function coldOpen(name) { await page.goto("about:blank"); await openAgent(name); }
async function dropLedger(handler) { await page.route(/\/api\/ledger$/, handler); }

await check("more: visible at phone width for a switchable agent (ledger on), and menu opens", async () => {
  await openAgent("alpha");
  await expectMore("alpha");
  const ids = (await menuItems()).map((i) => i.id);
  if (!ids.includes("d-model") || !ids.includes("d-mark") || !ids.includes("d-role")) throw new Error("items: " + ids);
});
await check("more: visible for a non-switchable agent (codex)", async () => {
  await openAgent("bravo");
  await expectMore("bravo");
  await page.waitForFunction(() => !document.querySelector("#d-mark").disabled, null, { timeout: 5000 });
  const ids = (await menuItems()).map((i) => i.id);
  if (ids.includes("d-model") || !ids.includes("d-mark")) throw new Error("items: " + ids);
});
await check("more: stays put after scrolling the output and the page", async () => {
  await openAgent("alpha");
  const before = await expectMore("before scroll");
  await page.evaluate(() => {
    const c = document.querySelector("#chat");
    c.insertAdjacentHTML("beforeend", "<div style='height:4000px'>pad</div>");
    c.scrollTop = c.scrollHeight; window.scrollTo(0, 99999);
  });
  const after = await expectMore("after scroll");
  if (Math.abs(after.y - before.y) > 1 || Math.abs(after.x - before.x) > 1) throw new Error("moved " + JSON.stringify([before, after]));
});
await check("more: present before the ledger loads, menu opens with items waiting, then enabled", async () => {
  let release;
  const gate = new Promise((r) => { release = r; });
  await dropLedger(async (r) => { await gate; r.continue(); });
  await coldOpen("bravo");
  await expectMore("ledger pending");
  const pending = await menuItems();
  const mark = pending.find((i) => i.id === "d-mark");
  if (!mark || !mark.disabled) throw new Error("mark should wait (disabled): " + JSON.stringify(pending));
  release();
  await page.waitForFunction(() => !document.querySelector("#d-mark").disabled, null, { timeout: 5000 });
  await expectMore("ledger loaded");
  await page.unroute(/\/api\/ledger$/);
});
await check("more: present when the ledger fails transiently (500) and the menu still opens", async () => {
  await dropLedger((r) => r.fulfill({ status: 500, body: "boom" }));
  await coldOpen("bravo");
  await expectMore("ledger 500");
  const items = await menuItems();
  if (!items.some((i) => i.id === "d-mark" && i.disabled)) throw new Error(JSON.stringify(items));
  await page.unroute(/\/api\/ledger$/);
});
await check("more: present when the hub keeps no ledger (503): switch model for claude, a note for codex", async () => {
  await dropLedger((r) => r.fulfill({ status: 503, body: "no state dir" }));
  await coldOpen("alpha");
  await expectMore("503 claude");
  let items = await menuItems();
  if (!items.some((i) => i.id === "d-model")) throw new Error("no model item: " + JSON.stringify(items));
  if (items.some((i) => i.id === "d-mark")) throw new Error("mark shown without a ledger");
  await coldOpen("bravo");
  await expectMore("503 codex");
  items = await menuItems();
  if (!items.some((i) => i.id === "d-mark-state" && /not available/.test(i.text))) throw new Error("no note: " + JSON.stringify(items));
  await page.unroute(/\/api\/ledger$/);
});
await check("more: stays visible with a very long name and title at phone width", async () => {
  const long = "an-extremely-long-agent-name-that-goes-on-and-on-and-on-without-any-spaces-at-all-0123456789";
  await coldOpen("alpha");
  await page.evaluate((t) => { document.querySelector("#d-title").textContent = t; document.querySelector("#d-subtitle").textContent = t + t; document.querySelector("#d-status").textContent = "blocked"; }, long);
  await expectMore("long title");
  const w = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth);
  if (w > 0) throw new Error("page scrolls horizontally by " + w);
});

// ---- the briefings screen: swipe left from the list, with history ----
// A real touch swipe: CDP touch events (press, moves, release) at a pace.
const cdp = await page.context().newCDPSession(page);
async function swipe(x0, y0, x1, y1, { steps = 8, ms = 240, hold = false } = {}) {
  const pt = (x, y) => [{ x, y, id: 1 }];
  await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: pt(x0, y0) });
  for (let i = 1; i <= steps; i++) {
    await page.waitForTimeout(ms / steps);
    await cdp.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: pt(x0 + ((x1 - x0) * i) / steps, y0 + ((y1 - y0) * i) / steps) });
  }
  if (hold) return;
  await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
}
const release = () => cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
const briefsState = () => page.evaluate(() => ({
  open: document.querySelector("#briefs").classList.contains("open"),
  inert: document.querySelector("#briefs").inert,
  x: Math.round(document.querySelector("#briefs").getBoundingClientRect().x),
  hash: location.hash,
  listY: Math.round(window.scrollY),
}));
async function settle() { await page.waitForTimeout(450); }
async function freshList() {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#pager:not([hidden])", { timeout: 8000 });
  await page.waitForSelector("#agents .agent", { timeout: 8000 });
}
await check("briefings: history is kept and listed, newest first, the latest on top", async () => {
  // The hub merges briefings published within 15 minutes unless the Needs you
  // lines differ, so each of these says something different there.
  await publishBrief("## History one\n\nbody one\n\n## Needs you\nNothing open.");
  await publishBrief("## History two\n\nbody two\n\n## Needs you\nNone.");
  await publishBrief("## History three\n\nbody three\n\n## Needs you\nAll clear.");
  const h = await (await fetch(base + "/api/brief/history")).json();
  if (h.briefs.length < 3 || !h.briefs[0].markdown.includes("three") || !h.briefs[1].markdown.includes("two")) throw new Error(JSON.stringify(h).slice(0, 300));
  await freshList();
  await page.locator("#pager").click();
  await page.waitForFunction(() => document.querySelectorAll("#br-body .br-item").length >= 2, null, { timeout: 5000 });
  const t = await page.locator("#br-body").innerText();
  if (!/History three/.test(t.split("EARLIER")[0] || t) || !t.includes("History two") || !t.includes("History one")) throw new Error(t.slice(0, 300));
  await page.locator("#br-body .br-item summary").first().click();
  await page.waitForFunction(() => document.querySelector("#br-body .br-item[open] .md")?.innerText.includes("body two"), null, { timeout: 3000 });
});
await check("briefings: the pill opens it, the back button returns (and the URL follows)", async () => {
  await freshList();
  let st = await briefsState();
  if (st.open || !st.inert || st.x < 380) throw new Error("not parked off-screen: " + JSON.stringify(st));
  await page.locator("#pager").click();
  await settle();
  st = await briefsState();
  if (!st.open || st.inert || st.x !== 0 || st.hash !== "#/briefings") throw new Error("pill did not open it: " + JSON.stringify(st));
  await page.locator("#br-back").click();
  await settle();
  st = await briefsState();
  if (st.open || !st.inert || st.x < 380 || st.hash !== "") throw new Error("back did not return: " + JSON.stringify(st));
});
await check("briefings: the menu item opens it; the browser's back returns", async () => {
  await freshList();
  await page.locator("#btn-fleet").click();
  await page.locator("#f-briefs").click();
  await settle();
  let st = await briefsState();
  if (!st.open) throw new Error("menu did not open it: " + JSON.stringify(st));
  await page.goBack();
  await settle();
  st = await briefsState();
  if (st.open || st.x < 380) throw new Error("history back did not return: " + JSON.stringify(st));
});
await check("briefings: a cold start on #/briefings shows it without a slide", async () => {
  await page.goto(base + "/#/briefings", { waitUntil: "load" });
  await page.waitForTimeout(300);
  const st = await briefsState();
  if (!st.open || st.x !== 0) throw new Error(JSON.stringify(st));
});
await check("briefings: swipe left opens it, swipe right returns", async () => {
  await freshList();
  await swipe(330, 500, 60, 505);
  await settle();
  let st = await briefsState();
  if (!st.open || st.x !== 0 || st.hash !== "#/briefings") throw new Error("swipe left did not open it: " + JSON.stringify(st));
  await swipe(60, 500, 330, 505);
  await settle();
  st = await briefsState();
  if (st.open || st.x < 380 || st.hash !== "") throw new Error("swipe right did not return: " + JSON.stringify(st));
});
await check("briefings: mid-swipe the screen follows the finger", async () => {
  await freshList();
  await swipe(330, 500, 130, 504, { hold: true });
  await page.waitForTimeout(100);
  const x = (await briefsState()).x;
  await page.screenshot({ path: `${out}/briefings-mid-swipe.png` });
  await release();
  // 200px of finger: the left edge sits near 390 - 200 (the lock distance is not counted).
  if (x < 190 || x > 260) throw new Error("screen at x=" + x + " with the finger 200px along");
  await settle();
});
await check("briefings: a short slow drag snaps back; a short fast flick goes through", async () => {
  await freshList();
  await swipe(300, 500, 230, 503, { ms: 600 }); // 70px, slow
  await settle();
  let st = await briefsState();
  if (st.open || st.x < 380) throw new Error("a short slow drag opened it: " + JSON.stringify(st));
  await swipe(300, 500, 240, 502, { steps: 3, ms: 45 }); // 60px, fast
  await settle();
  st = await briefsState();
  if (!st.open) throw new Error("a fast flick did not open it: " + JSON.stringify(st));
});
await check("briefings: a flick back the other way mid-drag cancels the open", async () => {
  await freshList();
  await swipe(330, 500, 150, 504, { steps: 6, ms: 300, hold: true });
  for (const x of [165, 200, 260, 330]) { await cdp.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x, y: 504, id: 1 }] }); await page.waitForTimeout(8); }
  await release();
  await settle();
  const st = await briefsState();
  if (st.open || st.x < 380) throw new Error("a reversal still opened it: " + JSON.stringify(st));
});
await check("briefings: vertical scrolling on the list does not open it", async () => {
  await freshList();
  await page.evaluate(() => { document.querySelector("#agents").style.minHeight = "2400px"; });
  await swipe(200, 600, 190, 200, { steps: 10, ms: 200 }); // up, with a little drift left
  await settle();
  const st = await briefsState();
  if (st.open || st.x < 380) throw new Error("a vertical swipe opened it: " + JSON.stringify(st));
  if (st.listY < 100) throw new Error("the list did not scroll: " + JSON.stringify(st));
});
await check("briefings: a swipe that starts on a sideways-scrolling row scrolls the row, not the page", async () => {
  await freshList();
  await page.evaluate(() => {
    const d = document.createElement("div");
    d.id = "hscroll"; d.className = "host-filter"; d.style.cssText = "overflow-x:auto;white-space:nowrap;height:60px;background:#334";
    d.innerHTML = '<div style="flex:none;width:1200px;height:40px">a wide row that scrolls sideways</div>';
    document.querySelector("#agents").prepend(d);
  });
  const r = await page.evaluate(() => { const b = document.querySelector("#hscroll").getBoundingClientRect(); return { y: b.y + 20 }; });
  await swipe(330, r.y, 60, r.y + 2);
  await settle();
  const st = await briefsState();
  const sl = await page.evaluate(() => document.querySelector("#hscroll").scrollLeft);
  if (st.open) throw new Error("opened from a scrolling row");
  if (sl < 50) throw new Error("the row did not scroll (scrollLeft " + sl + ")");
});
await check("briefings: touches at the screen edges are left to the OS", async () => {
  await freshList();
  await swipe(383, 500, 100, 504); // starts at the right edge
  await settle();
  let st = await briefsState();
  if (st.open) throw new Error("opened from the right edge");
  await page.locator("#pager").click();
  await settle();
  await swipe(8, 500, 340, 504); // starts at the left edge: iOS back swipe territory
  await settle();
  st = await briefsState();
  if (!st.open || st.x !== 0) throw new Error("a left-edge start was taken by the page: " + JSON.stringify(st));
});
await check("briefings: no swipe while an agent or the launch sheet is open", async () => {
  await freshList();
  await page.locator("#btn-launch").click();
  await page.waitForSelector("#launch", { state: "visible" });
  await swipe(330, 500, 60, 504);
  await settle();
  let st = await briefsState();
  if (st.open) throw new Error("opened under the launch sheet");
  await page.locator("#ln-close").click();
  await openAgent("alpha");
  await swipe(330, 500, 60, 504);
  await settle();
  st = await briefsState();
  if (st.open) throw new Error("opened over an agent");
});

// ---- the Briefings chip stays at the bottom of the screen (phone) ----
await check("pager: at 390px the chip stays at the bottom of the screen while a long To do item scrolls, clear of its last button", async () => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(base + "/", { waitUntil: "load" });
  const ctx = Array.from({ length: 40 }, (_, i) => `- line ${i} of the context, long enough to wrap on a narrow screen`).join("\n");
  const id = await page.evaluate(async (c) => {
    const r = await fetch("/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ question: "Pager check: pick one?", kind: "decision", context: c, no_wait: true }) }).then((x) => x.json());
    return r.todo.id;
  }, ctx);
  try {
    await page.reload({ waitUntil: "load" });
    await page.waitForSelector("#pager", { state: "visible" });
    await page.waitForSelector("#todo .td-toggle", { state: "visible" });
    if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
    await page.locator(`#todo .td-item[data-id="${id}"] .td-row`).click();
    await page.waitForSelector(`#todo .td-item.open[data-id="${id}"] .td-acts button`);
    const geoOf = () => page.evaluate((i) => {
      const r = document.getElementById("pager").getBoundingClientRect();
      const bs = [...document.querySelectorAll(`#todo .td-item[data-id="${i}"] button`)], b = bs[bs.length - 1].getBoundingClientRect();
      return { top: r.top, bottom: r.bottom, left: r.left, right: r.right, vh: innerHeight, vw: innerWidth, btnTop: b.top, btnBottom: b.bottom, sy: scrollY, max: document.documentElement.scrollHeight - innerHeight };
    }, id);
    const pinned = (g, at) => {
      if (g.bottom > g.vh + 0.5 || g.bottom < g.vh - 60) throw new Error(`${at}: the chip is not at the bottom: ${JSON.stringify(g)}`);
      if (Math.abs((g.left + g.right) / 2 - g.vw / 2) > 1.5) throw new Error(`${at}: the chip is not centred: ${JSON.stringify(g)}`);
    };
    await page.evaluate(() => scrollTo(0, 0));
    await page.evaluate((i) => document.querySelector(`#todo .td-item[data-id="${i}"]`).scrollIntoView(), id);
    pinned(await geoOf(), "top of the item");
    await page.evaluate(() => scrollBy(0, 500));
    const mid = await geoOf(); pinned(mid, "mid-scroll");
    await page.screenshot({ path: `${out}/pager-mid.png` });
    await page.evaluate(() => scrollTo(0, 1e7));
    await page.waitForTimeout(150);
    const end = await geoOf(); pinned(end, "end of the page");
    if (end.sy < end.max - 2) throw new Error("did not reach the end: " + JSON.stringify(end));
    await page.screenshot({ path: `${out}/pager-end.png` });
    // The last button scrolls clear of the chip: it ends above the chip's top edge.
    if (end.btnBottom > end.top) throw new Error("the chip covers the last button: " + JSON.stringify(end));
  } finally {
    await page.evaluate((i) => fetch("/api/todos/" + i + "/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }), id).catch(() => {});
  }
});
await check("pager: at 390px an opened To do item's last option button ends above the chip at the end of the page, and can be tapped", async () => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(base + "/", { waitUntil: "load" });
  const ctx = "Ways:\n\n1. **Rotate now.** wait\n2. **Rotate later.** no wait\n3. **Both.** why not\n\nMy recommendation: ok 1+3.";
  const id = await page.evaluate(async (c) => (await fetch("/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ question: "Pager options: which?", kind: "decision", context: c, no_wait: true }) }).then((x) => x.json())).todo.id, ctx);
  try {
    await page.reload({ waitUntil: "load" });
    await page.waitForSelector("#pager", { state: "visible" });
    await page.waitForSelector("#todo .td-toggle", { state: "visible" });
    if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
    await page.locator(`#todo .td-item[data-id="${id}"] .td-row`).click();
    await page.waitForSelector(`#todo .td-item.open[data-id="${id}"] .td-opt`);
    await page.evaluate(() => scrollTo(0, 1e7));
    await page.waitForTimeout(150);
    const g = await page.evaluate((i) => {
      const p = document.getElementById("pager").getBoundingClientRect();
      const bs = [...document.querySelectorAll(`#todo .td-item[data-id="${i}"] .td-opt, #todo .td-item[data-id="${i}"] button`)];
      const b = bs.map((x) => x.getBoundingClientRect()).sort((x, y) => y.bottom - x.bottom)[0];
      const hit = document.elementFromPoint(b.left + b.width / 2, b.bottom - 4);
      return { pagerTop: p.top, btnBottom: b.bottom, vh: innerHeight, coveredBy: hit && hit.closest("#pager") ? "pager" : "" };
    }, id);
    await page.screenshot({ path: `${out}/pager-options-end.png` });
    if (g.btnBottom > g.pagerTop || g.coveredBy) throw new Error("the chip covers the last button: " + JSON.stringify(g));
  } finally {
    await page.evaluate((i) => fetch("/api/todos/" + i + "/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }), id).catch(() => {});
  }
});
await check("pager: when the visual viewport differs from the layout one (pinch-zoomed), the chip follows the visible bottom", async () => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#pager", { state: "visible" });
  const cdp = await page.context().newCDPSession(page);
  try {
    await cdp.send("Emulation.setPageScaleFactor", { pageScaleFactor: 1.5 });
    await page.evaluate(() => { visualViewport.dispatchEvent(new Event("resize")); });
    await page.waitForTimeout(150);
    const g = await page.evaluate(() => { const r = document.getElementById("pager").getBoundingClientRect(), v = visualViewport; return { bottom: r.bottom, vvBottom: v.offsetTop + v.height, scale: v.scale, mid: (r.left + r.right) / 2, vvMid: v.offsetLeft + v.width / 2 }; });
    if (Math.abs(g.scale - 1.5) > 0.01) throw new Error("page scale not applied: " + JSON.stringify(g)); // emulation broke
    if (g.bottom > g.vvBottom + 0.5 || g.bottom < g.vvBottom - 20) throw new Error("the chip is not at the visible bottom: " + JSON.stringify(g));
    if (Math.abs(g.mid - g.vvMid) > 2) throw new Error("the chip is not centred in the visible area: " + JSON.stringify(g));
  } finally {
    await cdp.send("Emulation.setPageScaleFactor", { pageScaleFactor: 1 }).catch(() => {});
    await cdp.detach().catch(() => {});
  }
});

// ---- desktop view (>= 1100px): two columns, the list and one pane ----
const geo = (sel) => page.evaluate((s) => {
  const e = document.querySelector(s); if (!e) return null;
  const r = e.getBoundingClientRect(), cs = getComputedStyle(e);
  return { x: r.x, y: r.y, w: r.width, h: r.height, shown: cs.display !== "none" && cs.visibility !== "hidden" && r.width > 0 };
}, sel);
// start: the hash to load. The Briefing page by default, so what is in the pane does not
// depend on which agents happen to be waiting (the desktop's own default has its own checks).
async function desktopList(width, start = "#/briefings") {
  await page.setViewportSize({ width, height: 800 });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.evaluate(() => { try { localStorage.clear(); } catch {} });
  await page.goto(base + "/" + (width >= 1100 ? start : ""), { waitUntil: "load" });
  await page.waitForSelector("#agents .agent[data-id]", { state: "visible" });
}
for (const width of [1280, 1920]) {
  await check(`desktop: at ${width}px the list and the Briefing page sit side by side, two columns`, async () => {
    await publishBrief("## Status\n\nDesktop panel check\n\nlong detail\n\n## Needs you\nNo decisions.");
    await desktopList(width);
    await page.waitForSelector("body.view-briefing #briefs.open .br-card");
    const [l, d, e, b, c, pg, td] = await Promise.all([geo("#agents"), geo("#detail"), geo("#detail-empty"), geo("#briefs"), geo("#briefing"), geo("#pager"), geo("#todo")]);
    if (!l.shown || l.x !== 0 || l.w < 300 || l.w > 400) throw new Error("list column: " + JSON.stringify(l));
    if (d.shown) throw new Error("agent pane shown with a page open");
    if (e.shown) throw new Error("the empty state shows over a page");
    if (!b.shown || Math.abs(b.x - (l.x + l.w)) > 2 || Math.abs(b.x + b.w - width) > 1 || b.h < 600) throw new Error("the Briefing page does not fill the pane beside the list: " + JSON.stringify(b));
    if (c.shown || (td && td.shown)) throw new Error("the latest-briefing or to-do card shows in the list column");
    if (pg.shown) throw new Error("the pill shows on desktop");
    const t = await page.locator("#briefs").innerText();
    if (!t.includes("Desktop panel check")) throw new Error("latest briefing missing from the page: " + t.slice(0, 200));
    if (!(await page.locator("#briefs .br-item").count())) throw new Error("no history on the page");
    if (await page.locator("#br-back").isVisible()) throw new Error("a back button on the page");
    if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("page scrolls horizontally");
    if (await page.evaluate(() => getComputedStyle(document.body).gridTemplateColumns.split(" ").length) !== 2) throw new Error("not two grid columns");
  });
}
// A multi-host worker row is the tightest case: host, repo and branch, and sometimes a state chip.
for (const width of [1280, 390]) {
  await check(`agent rows: at ${width}px a worker keeps its repo whole, room for its branch and its state`, async () => {
    await desktopList(width);
    const m = await page.evaluate(() => {
      state.hosts = [{ name: "hub", local: true, status: "ok" }, { name: "buildbox", status: "ok" }];
      const a = { id: "buildbox/w9:p1", host: "buildbox", pane_id: "w9:p1", name: "a-worker", agent: "claude", agent_status: "idle", cwd: "/x",
        worktree: { repo: "tidepool", checkout: "/x", linked: true, branch: "a-rather-long-branch-name-for-this-one-worker" } };
      const wrap = document.getElementById("agents");
      const measure = () => {
        wrap.insertAdjacentHTML("afterbegin", agentRow(a, true));
        const row = wrap.firstElementChild, sub = row.querySelector(".sub"), box = (e) => e.getBoundingClientRect();
        const repo = sub.querySelector(".repo"), chip = sub.querySelector(".wt-chip"), lg = sub.querySelector(".lg-chip");
        const out = { repoCut: repo.scrollWidth > repo.clientWidth, branch: box(chip).width, branchEnd: box(chip).right, subEnd: box(sub).right,
          state: lg && getComputedStyle(lg).display !== "none" ? { end: box(lg).right, cut: lg.scrollWidth > lg.clientWidth } : null, subH: box(sub).height };
        row.remove();
        return out;
      };
      const running = (ledger.byAgent.set(a.id, { status: "running" }), measure());
      const finished = (ledger.byAgent.set(a.id, { status: "finished", candidate: true }), measure());
      ledger.byAgent.delete(a.id);
      return { running, finished };
    });
    for (const [what, r] of Object.entries(m)) {
      if (r.repoCut) throw new Error(`${what}: the repo is cut: ` + JSON.stringify(r));
      if (r.branch < 45) throw new Error(`${what}: no room for the branch: ` + JSON.stringify(r));
      if (r.branchEnd > r.subEnd + 0.5) throw new Error(`${what}: the branch runs off the row: ` + JSON.stringify(r));
    }
    if (!m.finished.state || m.finished.state.cut || m.finished.state.end > m.finished.subEnd + 0.5) throw new Error("the finished chip is cut: " + JSON.stringify(m.finished));
    if (width >= 1100 && m.running.subH > 24) throw new Error("a running worker's detail takes more than one line on desktop: " + JSON.stringify(m.running));
  });
}

// Landing cards say only what is true: a progress note, an unpushed branch, a background wait, CI of a landed
// commit, a failure with its reason. Each state is drawn as a worker row, in both themes, phone and desktop.
for (const width of [390, 1280]) {
  await check(`landing states: at ${width}px each card says what is true, in both themes`, async () => {
    await desktopList(width);
    const rows = await page.evaluate(() => {
      const iso0 = (ms) => new Date(Date.now() - ms).toISOString();
      const mk = (i, status, over = {}, ag = {}) => ({
        a: { id: `uit/w${i}:p1`, host: "uit", pane_id: `w${i}:p1`, name: `worker-${i}`, agent: "claude", agent_status: "idle", cwd: "/x",
          worktree: { repo: "proj", checkout: "/x", linked: true, branch: `fix/state-${i}` }, ...ag },
        e: { status, approval: { by: "coordinator", note: "ok" }, finished_at: iso0(3600e3), live: { present: true }, ...over },
      });
      const lc = (o) => ({ at: iso0(5e3), base: "origin/main", verdict: "1 commits not in origin/main", unmerged: 1, unpushed: 0, ...o });
      const bg = [{ kind: "bash", label: "full UI suite", since: iso0(240e3) }];
      const cases = [
        ["landing", mk(1, "landing", { finished_at: undefined })],
        ["landing: tests running", mk(2, "landing", { finished_at: undefined, progress: { text: "the full UI suite is running", phase: "tests", at: iso0(60e3) } }, { agent_status: "working" })],
        ["landing: not pushed yet", mk(3, "landing", { finished_at: undefined, land_check: lc({ state: "pending", unpushed: 1, reason: "the worker is working" }) }, { agent_status: "working" })],
        ["landing: not on main yet", mk(4, "landing", { finished_at: undefined, land_check: lc({ state: "pending" }) }, { agent_status: "working" })],
        ["landing: waiting on a background task", mk(5, "landing", { finished_at: undefined, live: { present: true }, land_check: lc({ state: "pending", unpushed: 1 }) }, { background: bg })],
        ["landed", mk(6, "landed", { land_check: lc({ ok: true, state: "landed", landed: ["64c1774aaaa"] }) })],
        ["landed, CI running", mk(7, "landed", { land_check: lc({ ok: true, state: "landed", ci: { state: "running", runs: 2 } }) })],
        ["landed, CI green", mk(8, "landed", { land_check: lc({ ok: true, state: "landed", ci: { state: "success", runs: 2 } }) })],
        ["landed, CI failed", mk(9, "landed", { land_check: lc({ ok: true, state: "landed", ci: { state: "failure", runs: 2, url: "https://ci/9" } }) })],
        ["land failed", mk(10, "finished", { finished_at: iso0(60e3), land_check: lc({ state: "failed", reason: "the worker said it was done, but its work is not on the base" }) })],
        ["finished", mk(11, "finished", { finished_at: iso0(60e3), approval: undefined })],
        ["running", mk(12, "running", { finished_at: undefined, approval: undefined }, { agent_status: "working" })],
        ["queued #2", mk(14, "landing", { finished_at: undefined, queue: { position: 2, ahead: 1, total: 3, turn: false } })],
        ["queued #3", mk(15, "landing", { finished_at: undefined, queue: { position: 3, ahead: 2, total: 3, turn: false } })],
        ["landing now", mk(16, "landing", { finished_at: undefined, queue: { position: 1, ahead: 0, total: 3, turn: true, turn_at: iso0(60e3), expires_at: iso0(-44 * 60e3) } })],
        ["landing: tests running", mk(17, "landing", { finished_at: undefined, queue: { position: 1, ahead: 0, total: 2, turn: true }, progress: { text: "tests", phase: "tests", at: iso0(60e3) } }, { agent_status: "working" })],
        ["bg", mk(13, "running", { finished_at: undefined, approval: undefined, live: { present: true } }, { background: bg })],
      ];
      for (const [, c] of cases) ledger.byAgent.set(c.a.id, c.e);
      const wrap = document.getElementById("agents");
      wrap.innerHTML = cases.map(([, c]) => agentRow(c.a, true)).join("");
      return cases.map(([want, c]) => {
        const row = wrap.querySelector(`[data-id="${c.a.id}"]`), chip = row.querySelector(".lg-chip");
        const st = row.querySelector(".status");
        return { want, chip: chip && getComputedStyle(chip).display !== "none" ? chip.textContent : null, status: st.textContent, statusBg: st.classList.contains("waiting"),
          dotWaiting: row.querySelector(".dot").classList.contains("waiting"), cut: chip ? chip.scrollWidth > chip.clientWidth + 1 || chip.getBoundingClientRect().right > row.querySelector('.sub').getBoundingClientRect().right + 1 : false, rowEnd: row.getBoundingClientRect().right };
      });
    });
    for (const r of rows) {
      if (r.want === "bg") {
        if (!/^waiting/.test(r.status) || !r.dotWaiting) throw new Error("an idle agent with a background task is not shown waiting: " + JSON.stringify(r));
        continue;
      }
      if (r.want === "running") { if (!/^working/.test(r.status)) throw new Error("a running worker: " + JSON.stringify(r)); continue; }
      if (r.want === "finished") { if (r.chip !== "finished") throw new Error("finished: " + JSON.stringify(r)); continue; }
      const ok = r.want === "land failed" ? r.chip && r.chip.startsWith("land failed: the worker said it was done") : r.chip === r.want;
      if (!ok) throw new Error(`want chip "${r.want}", got ${JSON.stringify(r)}`);
      if (r.cut) throw new Error("a chip is cut: " + JSON.stringify(r));
      if (r.rowEnd > width + 1) throw new Error("a row runs off the screen: " + JSON.stringify(r));
    }
    const bgRow = rows.find((r) => r.want === "landing: waiting on a background task");
    if (!/^waiting/.test(bgRow.status) || !bgRow.statusBg) throw new Error("the waiting worker's status is not waiting: " + JSON.stringify(bgRow));
    if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("page scrolls horizontally");
    for (const cs of ["dark", "light"]) {
      await page.emulateMedia({ colorScheme: cs });
      await page.locator("#agents").screenshot({ path: `${out}/landing-states-${width}-${cs}.png` });
    }
    await page.emulateMedia({ colorScheme: process.env.SCHEME === "light" ? "light" : "dark" });
    await page.evaluate(() => { for (let i = 1; i <= 17; i++) ledger.byAgent.delete(`uit/w${i}:p1`); });
  });
}

// An idle or done agent with a task running in the background reads "waiting" with the task's label and
// age, not "done"; it sorts between working and done, is not counted as working or as needing you, and
// goes back to normal when the task ends. A working agent is unchanged.
for (const width of [390, 1280]) {
  await check(`background: at ${width}px an idle agent with a running task shows waiting, and clears when it ends`, async () => {
    await desktopList(width);
    const r = await page.evaluate(() => {
      const iso0 = (ms) => new Date(Date.now() - ms).toISOString();
      const saved = state.agents;
      const mk = (i, st, bg) => ({ id: `uit/w${i}:p1`, host: "uit", pane_id: `w${i}:p1`, name: `agent-${i}`, agent: "claude", agent_status: st, cwd: "/x", ...(bg ? { background: bg } : {}) });
      const wait = [{ kind: "bash", label: "race + uitest", since: iso0(4 * 60e3 + 5e3) }, { kind: "monitor", label: "gh run watch", since: iso0(60e3) }];
      state.agents = [mk(1, "done"), mk(2, "done", wait), mk(3, "idle", [wait[0]]), mk(4, "working", wait), mk(5, "working")];
      renderListRows();
      const row = (i) => document.querySelector(`#agents [data-id="uit/w${i}:p1"]`);
      const read = (i) => { const r = row(i); return { status: [...r.querySelector(".status").childNodes].map((n) => n.textContent).join(" ").replace(/\s+/g, " ").trim(), cls: r.querySelector(".status").className, dot: r.querySelector(".dot").className, why: (r.querySelector(".why") || {}).textContent || "", title: r.querySelector(".status").title, end: r.getBoundingClientRect().right }; };
      const out = { w2: read(2), w3: read(3), w1: read(1), w4: read(4), count: document.getElementById("count").textContent,
        order: [...document.querySelectorAll("#agents .agent")].map((e) => e.dataset.id.split("/")[1].split(":")[0]).join(",") };
      state.agents = state.agents.map((a) => (a.id === "uit/w2:p1" ? { ...a, background: undefined } : a));
      renderListRows();
      out.cleared = read(2);
      state.agents = saved;
      return out;
    });
    if (!/^waiting 4m$/i.test(r.w2.status) || !/waiting/.test(r.w2.dot) || !/waiting/.test(r.w2.cls)) throw new Error("done + tasks is not waiting 4m: " + JSON.stringify(r.w2));
    if (r.w2.why !== "race + uitest +1" || !/race \+ uitest/.test(r.w2.title) || !/gh run watch/.test(r.w2.title)) throw new Error("label/title: " + JSON.stringify(r.w2));
    if (!/^waiting/.test(r.w3.status)) throw new Error("idle + task is not waiting: " + JSON.stringify(r.w3));
    if (r.w1.status !== "done" || /waiting/.test(r.w1.dot)) throw new Error("a plain done agent changed: " + JSON.stringify(r.w1));
    if (r.w4.status.startsWith("waiting") || !/working/.test(r.w4.cls)) throw new Error("a working agent changed: " + JSON.stringify(r.w4));
    if (!/\b1\b.*working|2 working/.test(r.count) || /waiting|need/.test(r.count)) throw new Error("count line: " + r.count);
    if (!/^w[45],w[45],w[23],w[23],w1$/.test(r.order)) throw new Error("order, want working, waiting, done: " + r.order);
    if (r.cleared.status !== "done" || /waiting/.test(r.cleared.dot) || r.cleared.why) throw new Error("did not clear when the task ended: " + JSON.stringify(r.cleared));
    if (r.w2.end > width + 1) throw new Error("row runs off the screen");
    for (const cs of ["dark", "light"]) {
      await page.emulateMedia({ colorScheme: cs });
      await page.evaluate(() => { const iso0 = (ms) => new Date(Date.now() - ms).toISOString(); state.agents = [
        { id: "uit/w2:p1", host: "uit", pane_id: "w2:p1", name: "feat-add-host-slice2", agent: "claude", agent_status: "done", cwd: "/x", background: [{ kind: "bash", label: "race + uitest", since: iso0(4 * 60e3) }] },
        { id: "uit/w1:p1", host: "uit", pane_id: "w1:p1", name: "feat-other", agent: "claude", agent_status: "done", cwd: "/x" },
        { id: "uit/w4:p1", host: "uit", pane_id: "w4:p1", name: "feat-busy", agent: "claude", agent_status: "working", cwd: "/x" }]; renderListRows(); });
      await page.locator("#agents").screenshot({ path: `${out}/background-waiting-${width}-${cs}.png` });
    }
    await page.emulateMedia({ colorScheme: process.env.SCHEME === "light" ? "light" : "dark" });
  });
}

for (const width of [390, 1280]) {
  await check(`background: at ${width}px the agent's detail says waiting, with its label and age`, async () => {
    await page.setViewportSize({ width, height: width < 600 ? 780 : 800 });
    await openAgent("alpha");
    const r = await page.evaluate(() => {
      const a = state.agents.find((x) => x.pane_id === "w1:p1");
      a.agent_status = "done";
      a.background = [{ kind: "bash", label: "race + uitest", since: new Date(Date.now() - 4 * 60e3).toISOString() }];
      renderDetail();
      const on = { status: document.getElementById("d-status").textContent, cls: document.getElementById("d-status").className, why: document.getElementById("d-why").textContent, whyHidden: document.getElementById("d-why").hidden };
      return on;
    });
    if (!/^waiting · 4m$/.test(r.status) || !/waiting/.test(r.cls) || r.why !== "race + uitest" || r.whyHidden) throw new Error("detail: " + JSON.stringify(r));
    for (const cs of ["dark", "light"]) {
      await page.emulateMedia({ colorScheme: cs });
      await page.locator("#detail header").first().screenshot({ path: `${out}/background-detail-${width}-${cs}.png` });
    }
    await page.emulateMedia({ colorScheme: process.env.SCHEME === "light" ? "light" : "dark" });
    const off = await page.evaluate(() => { const a = state.agents.find((x) => x.pane_id === "w1:p1"); a.background = undefined; renderDetail(); return document.getElementById("d-status").textContent; });
    if (/waiting/.test(off)) throw new Error("detail still waiting after the task ended: " + off);
  });
}

// A hub-only hub (no herdr): its own host is "no agents here", not an error, and the launch form starts elsewhere.
await check("hub-only: the hub's own host reads 'no agents here' and the launch form skips it", async () => {
  await desktopList(1280);
  const r = await page.evaluate(() => {
    const saved = { agents: state.agents, hosts: state.hosts };
    state.hosts = [{ name: "hub", local: true, status: "ok", no_agents: true, agents: 0 }, { name: "buildbox", status: "ok", agents: 0 }];
    state.agents = [];
    renderList();
    const chips = document.getElementById("host-filter").innerText;
    const empty = document.querySelector("#agents .empty")?.innerText || "";
    renderLaunchHosts();
    const launch = [...document.querySelectorAll("#ln-hosts .hf-chip")].map((b) => [b.dataset.host, b.disabled]);
    state.agents = saved.agents; state.hosts = saved.hosts;
    renderList();
    return { chips, empty, launch };
  });
  if (!/hub\s+no agents here/.test(r.chips)) throw new Error("the host chip does not say 'no agents here': " + JSON.stringify(r.chips));
  if (!/hosts none/.test(r.empty)) throw new Error("the empty state does not explain: " + JSON.stringify(r.empty));
  const byHost = Object.fromEntries(r.launch);
  if (byHost.hub !== true || byHost.buildbox !== false) throw new Error("launch form hosts: " + JSON.stringify(r.launch));
});

await check("desktop: selecting an agent keeps the list visible and opens it beside it", async () => {
  await desktopList(1280);
  const before = await geo("#agents");
  await page.locator('#agents .agent[data-id$="w1:p1"]').first().click();
  await page.waitForSelector("#detail", { state: "visible" });
  await page.waitForFunction(() => document.querySelector("#chat")?.innerText.includes("alpha says hi"), null, { timeout: 5000 });
  const [l, d, back] = await Promise.all([geo("#agents"), geo("#detail"), geo("#back")]);
  if (!l.shown || l.x !== before.x || l.w !== before.w) throw new Error("list moved or hid: " + JSON.stringify([before, l]));
  if (d.x < l.x + l.w - 1 || d.h < 600) throw new Error("agent pane not beside the list: " + JSON.stringify(d));
  if (back.shown) throw new Error("back button shown on desktop");
  const sel = await page.evaluate(() => [...document.querySelectorAll("#agents .agent.selected")].map((r) => r.dataset.id));
  if (sel.length !== 1 || !sel[0].endsWith("w1:p1")) throw new Error("selected row marked: " + JSON.stringify(sel));
  if (!(await visible("#detail-empty")) === false) throw new Error("empty state still shown");
});
for (const width of [1280, 1600, 1920, 2560]) {
  await check(`desktop: at ${width}px the agent pane fills all the space beside the list`, async () => {
    await desktopList(width);
    await page.locator('#agents .agent[data-id$="w1:p1"]').first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.waitForSelector("#chat", { state: "visible" });
    const [l, d, m, c, h, b] = await Promise.all([geo("#agents"), geo("#detail"), geo("#detail main"), geo("#chat"), geo("#detail header"), geo("#briefs")]);
    if (Math.abs(d.x - 360) > 2 || Math.abs(d.x + d.w - width) > 1.5) throw new Error("pane does not span list..window edge: " + JSON.stringify([l, d]));
    if (b.shown) throw new Error("the Briefing page stays over an agent");
    if (m.w < d.w - 2 || h.w < d.w - 2) throw new Error("main/header narrower than the pane: " + JSON.stringify([d, m, h]));
    if (c.w < d.w - 40) throw new Error("chat leaves empty space at the sides: " + JSON.stringify([d, c]));
  });
}
await check("desktop: up/down in the list moves through the agents and opens each", async () => {
  await desktopList(1280);
  await page.locator("#agents .agent[data-id]").first().focus();
  // The rows can reorder as agents change status: look up the neighbour just before each key.
  const neighbour = (dir) => page.evaluate((d) => {
    const rows = [...document.querySelectorAll("#agents .agent[data-id]")].filter((r) => r.offsetParent);
    return rows[rows.indexOf(document.activeElement.closest(".agent[data-id]")) + d]?.dataset.id;
  }, dir);
  for (const [key, dir] of [["ArrowDown", 1], ["ArrowUp", -1], ["ArrowDown", 1]]) {
    const want = await neighbour(dir);
    await page.keyboard.press(key);
    await page.waitForFunction((id) => document.activeElement?.dataset.id === id && document.querySelector("#agents .agent.selected")?.dataset.id === id && location.hash.startsWith("#/agent/"), want, { timeout: 4000 });
  }
  // a re-render of the rows (the fleet menu's Refresh) must keep the focus on the same row
  const cur = await page.evaluate(() => document.activeElement.dataset.id);
  await page.evaluate(() => document.querySelector("#f-refresh").click());
  await new Promise((r) => setTimeout(r, 800));
  const id = await page.evaluate(() => document.activeElement?.dataset?.id);
  if (id !== cur) throw new Error("focus lost on re-render: " + id + " vs " + cur);
});
await check("desktop: the fleet menu opens beside the list and has no Briefing items", async () => {
  await desktopList(1280);
  await page.waitForFunction(() => ledger.on === true, null, { timeout: 8000 });
  await page.locator("#btn-fleet").click();
  const m = await geo("#f-menu");
  if (!m.shown || m.x < 0 || m.x + m.w > 361) throw new Error("menu outside the list column: " + JSON.stringify(m));
  if (await page.locator("#f-briefs").isVisible() || await page.locator("#f-brief").isVisible()) throw new Error("a Briefing item shows in the fleet menu on desktop");
  await page.keyboard.press("Escape");
});
// ---- waiting agents: the tab title, a Needs you group on desktop, the n key ----
// (w9:p1 is the Pi agent whose trust prompt makes it blocked.)
await check("waiting: the tab title counts them, the host filter never hides one, and the phone list has no Needs you group", async () => {
  await desktopList(390);
  await page.waitForFunction(() => /blocked/i.test(document.querySelector('#agents .agent[data-id$="w9:p1"]')?.innerText || ""), null, { timeout: 10000 });
  if (!/^\(\d+\) Comebye$/.test(await page.title())) throw new Error("title " + (await page.title()));
  if (await page.locator(".needs-sec").count()) throw new Error("a Needs you group on the phone");
  await page.evaluate(() => { setHostFilter("nowhere"); });
  if (!(await page.locator('#agents .agent[data-id$="w9:p1"]').count())) throw new Error("the host filter hid a waiting agent");
  await page.evaluate(() => { setHostFilter("all"); });
});
await check("desktop: waiting agents sit in a Needs you group at the top, and n opens the next one", async () => {
  await desktopList(1280);
  await page.waitForSelector('.needs-sec .agent[data-id$="w9:p1"]', { timeout: 10000 });
  const first = await page.evaluate(() => document.querySelector("#agents .agent[data-id]").closest(".needs-sec") !== null);
  if (!first) throw new Error("the Needs you group is not first");
  if (await page.locator('.proj:not(.needs-sec) .agent[data-id$="w9:p1"]').count()) throw new Error("the waiting agent is listed twice");
  await page.locator("body").press("n");
  await page.waitForFunction(() => document.querySelector("#agents .agent.selected")?.dataset.id.endsWith("w9:p1") && location.hash.startsWith("#/agent/"), null, { timeout: 5000 });
});

// ---- what a blocked agent is blocked on, on its card (blockedrelay.go) ----
// The hub reads the dialog of every blocked agent; the list row and the opened agent say "blocked: <what>"
// instead of a bare "blocked". (w9:p1 sits at Pi's "Trust project folder?" selector.)
for (const width of [390, 1280]) {
  await check(`blocked card at ${width}px: the row and the opened agent say what it is blocked on, inside their boxes`, async () => {
    await desktopList(width);
    await page.waitForFunction(() => /^blocked: /.test(document.querySelector('#agents .agent[data-id$="w9:p1"] .why')?.textContent || ""), null, { timeout: 15000 });
    const row = await page.evaluate(() => {
      const r = document.querySelector('#agents .agent[data-id$="w9:p1"]'), w = r.querySelector(".why");
      const rb = r.getBoundingClientRect(), wb = w.getBoundingClientRect();
      return { text: w.textContent, inside: wb.left >= rb.left - 1 && wb.right <= rb.right + 1, scroll: document.documentElement.scrollWidth > innerWidth };
    });
    if (!/choice \(|question \(|permission prompt/.test(row.text) || /undefined|null/.test(row.text)) throw new Error("row text: " + row.text);
    if (!row.inside || row.scroll) throw new Error("row geometry " + JSON.stringify(row));
    await page.locator('#agents .agent[data-id$="w9:p1"]').first().screenshot({ path: `${out}/blocked-card-row-${width}.png` });
    await page.locator('#agents .agent[data-id$="w9:p1"]').first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.waitForFunction(() => { const w = document.querySelector("#d-why"); return w && !w.hidden && /^blocked: /.test(w.textContent); }, null, { timeout: 8000 });
    const head = await page.evaluate(() => {
      const w = document.querySelector("#d-why"), h = document.querySelector("#detail header");
      const wb = w.getBoundingClientRect(), hb = h.getBoundingClientRect();
      return { text: w.textContent, inside: wb.right <= hb.right + 1 && wb.left >= hb.left - 1, scroll: document.documentElement.scrollWidth > innerWidth };
    });
    if (!head.inside || head.scroll) throw new Error("header geometry " + JSON.stringify(head));
    await page.screenshot({ path: `${out}/blocked-card-open-${width}.png` });
  });
}
await check("blocked card: an agent that is not blocked shows no reason", async () => {
  await desktopList(1280);
  const n = await page.evaluate(() => [...document.querySelectorAll("#agents .agent:not(.blocked) .why:not(.quiet)")].length);
  if (n) throw new Error(n + " rows that are not blocked show a blocked reason");
});

// ---- pages in the router: one selection fills the main pane ----
await check("pages: opening one leaves the agent through closeDetail (presence clears), and its hash is the route", async () => {
  await desktopList(1280);
  const posts = [];
  const onReq = (r) => { if (r.url().includes("/api/push/presence")) posts.push(JSON.parse(r.postData() || "{}")); };
  page.on("request", onReq);
  try {
    await page.locator('#agents .agent[data-id$="w1:p1"]').first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.waitForFunction(() => state.selected && state.selected.endsWith("w1:p1"));
    await page.waitForTimeout(300);
    if (!posts.some((p) => p.pane && p.visible)) throw new Error("no presence for the open agent: " + JSON.stringify(posts));
    posts.length = 0;
    await page.evaluate(() => openPage("todo"));
    await page.waitForTimeout(300);
    const r = await page.evaluate(() => ({ sel: state.selected, view: state.view, hash: location.hash, poll: state.pollTimer, cls: document.body.className }));
    if (r.sel !== null || r.view !== "todo" || r.hash !== "#/todo" || r.poll || !/view-todo/.test(r.cls) || /has-detail/.test(r.cls)) throw new Error("state " + JSON.stringify(r));
    if (!posts.length || posts[posts.length - 1].pane !== "") throw new Error("presence not cleared: " + JSON.stringify(posts));
    // Opening an agent from a page drops the page.
    await page.evaluate(() => openDetail(state.agents[0].id || state.agents[0].pane_id));
    const v = await page.evaluate(() => state.view);
    if (v !== null) throw new Error("page still set over an agent: " + v);
    // Back returns to the page, and a cold start on its hash opens it.
    await page.goto(base + "/#/todo", { waitUntil: "load" });
    await page.waitForFunction(() => state.view === "todo");
  } finally { page.off("request", onReq); }
});
// The g <x> jumps leave focus on the agent row that was in use; the To do page's own keys must still work.
await check("pages: g t then j/k move the To do selection, even with focus left on an agent row; so after a reload", async () => {
  await desktopList(1280);
  const ids = await page.evaluate(async () => {
    const out = [];
    for (const q of ["keys check one", "keys check two", "keys check three"]) {
      const r = await fetch("/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ question: q, kind: "decision", no_wait: true }) }).then((x) => x.json());
      out.push(r.todo.id);
    }
    return out;
  });
  const open = () => page.evaluate(() => todos.openId);
  const rowsOpen = async (want, what) => { await page.waitForTimeout(150); const got = await open(); if (got !== want) throw new Error(what + ": open item " + got + ", want " + want + " rows " + JSON.stringify(await page.evaluate(() => [...document.querySelectorAll("#p-todo .td-item")].map((e) => e.dataset.id)))); };
  try {
    const gt = async () => { await page.keyboard.press("g"); await page.keyboard.press("t"); await page.waitForSelector("#p-todo", { state: "visible" }); };
    const fresh = async () => { await page.evaluate(() => { todos.loadedAt = 0; todos.openId = null; todos.lastId = null; return loadTodos(); }); await page.waitForSelector("#p-todo .td-item", { state: "visible" }); };
    // Focus on an agent row (the case that broke), then g t.
    await page.locator('#agents .agent[data-id$="w1:p1"]').first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.locator('#agents .agent[data-id$="w1:p1"]').first().focus();
    await gt();
    await fresh();
    const ord = () => page.evaluate(() => [...document.querySelectorAll("#p-todo .td-item")].map((e) => e.dataset.id));
    let order = await ord();
    await page.keyboard.press("j"); await rowsOpen(order[0], "g t, j");
    await page.keyboard.press("j"); await rowsOpen(order[1], "g t, j j");
    await page.keyboard.press("k"); await rowsOpen(order[0], "g t, j j k");
    await page.keyboard.press("Escape"); await rowsOpen(null, "Esc closes");
    // g s, then g t again from the Status page.
    await page.keyboard.press("g"); await page.keyboard.press("s");
    await page.waitForSelector("#p-status", { state: "visible" });
    await gt(); await fresh(); order = await ord();
    await page.keyboard.press("j"); await rowsOpen(order[0], "status, g t, j");
    // A reload on #/todo.
    await page.goto(base + "/#/todo", { waitUntil: "load" });
    await page.reload({ waitUntil: "load" });
    await page.waitForSelector("#p-todo .td-item", { state: "visible" });
    await page.waitForTimeout(500);
    order = await ord();
    // The page may already show the first item; j then goes to the next.
    const was = await open();
    await page.keyboard.press("j"); await rowsOpen(was ? order[order.indexOf(was) + 1] : order[0], "reload, j");
  } finally {
    await page.evaluate((ids) => Promise.all(ids.map((i) => fetch("/api/todos/" + i + "/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }))), ids);
  }
});
// What the desktop opens to on a cold start. The agent list is rewritten on its way (and live
// events are cut) so the answer does not depend on which agents the fake herdr has waiting.
async function coldStartWith(blocked) {
  const rewrite = (j) => {
    for (const a of j.agents) if (a.agent_status === "blocked") a.agent_status = "idle";
    if (blocked) { const a = j.agents.find((x) => x.pane_id === "w1:p1"); if (a) a.agent_status = "blocked"; }
    return j;
  };
  const list = async (r) => { const res = await r.fetch(); await r.fulfill({ response: res, json: rewrite(await res.json()) }); };
  // The first list arrives as an SSE "agents" event: serve one (the app reconnects and gets it again).
  const events = async (r) => {
    const res = await page.request.get(base + "/api/agents");
    await r.fulfill({ status: 200, headers: { "content-type": "text/event-stream", "cache-control": "no-cache" }, body: `event: agents\ndata: ${JSON.stringify(rewrite(await res.json()))}\n\n` });
  };
  await page.route(/\/api\/events/, events);
  await page.route(/\/api\/agents$/, list);
  try {
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto(base + "/", { waitUntil: "load" });
    await page.evaluate(() => { try { localStorage.clear(); } catch {} });
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#agents .agent[data-id]", { state: "visible" });
    await page.waitForTimeout(600);
    return await page.evaluate(() => ({ sel: state.selected, view: state.view, hash: location.hash }));
  } finally {
    await page.unroute(/\/api\/agents$/, list);
    await page.unroute(/\/api\/events/, events);
  }
}
await check("desktop opens to a waiting agent when there is one", async () => {
  const r = await coldStartWith(true);
  if (!r.sel || !r.sel.endsWith("w1:p1") || r.view !== null) throw new Error("opened " + JSON.stringify(r));
});
await check("desktop opens to the Status page when nothing is waiting", async () => {
  const r = await coldStartWith(false);
  if (r.sel !== null || r.view !== "status" || r.hash !== "#/status") throw new Error("opened " + JSON.stringify(r));
});
await check("desktop: a deep link to an agent wins over the default", async () => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(base + "/#/agent/" + encodeURIComponent(PANES.bravo), { waitUntil: "load" });
  await page.waitForSelector("#detail", { state: "visible" });
  const r = await page.evaluate(() => ({ sel: state.selected, view: state.view }));
  if (!r.sel || !r.sel.endsWith(PANES.bravo) || r.view !== null) throw new Error(JSON.stringify(r));
});
await check("pages: the To do page fills the pane and answers from it; the list column has no to-do card", async () => {
  await desktopList(1280, "#/todo");
  const id = await page.evaluate(async () => {
    const r = await fetch("/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ question: "page check: keep the old key?", kind: "decision", context: "Some **context**" }) }).then((x) => x.json());
    await loadTodos();
    return r.todo.id;
  });
  const item = `#p-todo .td-item[data-id="${id}"]`;
  await page.waitForSelector(item);
  const [l, p, card] = await Promise.all([geo("#agents"), geo("#p-todo"), geo("#todo")]);
  if (!p.shown || Math.abs(p.x - (l.x + l.w)) > 2 || Math.abs(p.x + p.w - 1280) > 1 || p.h < 600) throw new Error("page not filling the pane: " + JSON.stringify(p));
  if (card && card.shown) throw new Error("the to-do card shows in the list column");
  await page.locator(item + " > .td-row").click();
  const box = page.locator(item + " .td-text");
  if ((await box.boundingBox()).width < 500) throw new Error("the answer box is cramped: " + JSON.stringify(await box.boundingBox()));
  // A typed answer survives a poll (the re-render guard).
  await box.fill("draft answer");
  await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
  if ((await box.inputValue()) !== "draft answer") throw new Error("draft lost on re-render");
  await page.locator(item + " .td-answer-btn").click();
  // Answered items fold into their group at the bottom.
  await page.waitForFunction((s) => !document.querySelector(s), item, { timeout: 5000 });
  await page.locator("#pt-body .td-g").click();
  await page.waitForSelector(item + ".answered", { timeout: 5000 });
  await page.evaluate((i) => fetch("/api/todos/" + i + "/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }), id);
  await page.evaluate(() => loadTodos());
  await page.waitForFunction((s) => !document.querySelector(s), item, { timeout: 5000 });
  if (!(await page.locator("#p-todo").innerText()).includes("Nothing is waiting on you")) throw new Error("no empty note");
});
await check("pages: an open To do item uses the panel's width (code and tables wide, prose capped at ~90ch), without horizontal scroll, at 1280/2000 and on the phone", async () => {
  const cmd = "cd ~/code/comebye && git fetch origin && git pull --ff-only && tools/release/tag v0.5.11 38ccfe8079bafda0972880e6b93197991f7667af";
  const prose = "Mark Done after the tag; I then start the rollout. This is a longer paragraph of running prose that must keep a readable measure however wide the window is. ".repeat(4);
  const ctx = "Main is at `38ccfe8`:\n\n```\n" + cmd + "\n```\n\nThree lines:\n\n```\ngit push origin --delete a\ngit worktree remove b\ngit branch -D c\n```\n\n" + prose + "\n\n| a | b |\n|---|---|\n| 1 | 2 |\n";
  const post = (b) => page.evaluate(async (b) => (await fetch("/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ no_wait: true, ...b }) }).then((x) => x.json())).todo.id, b);
  const drop = (i) => page.evaluate((i) => fetch("/api/todos/" + i + "/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }), i);
  await desktopList(2000, "#/todo");
  const id = await post({ question: "width check: a long question that should use the panel's width rather than stop at a narrow column, with `code` in it", kind: "decision", context: ctx });
  await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
  const item = `#p-todo .td-item[data-id="${id}"]`;
  await page.waitForSelector(item);
  await page.locator(item + " > .td-row").click();
  await page.waitForSelector(item + ".open .td-ctx pre");
  const measure = () => page.evaluate((item) => {
    const it = document.querySelector(item);
    const pre = it.querySelector(".td-ctx pre"), ctx = it.querySelector(".td-ctx"), p = [...it.querySelectorAll(".td-ctx p")].sort((a, b) => b.textContent.length - a.textContent.length)[0];
    const zero = document.createElement("span"); zero.textContent = "0"; zero.style.cssText = "position:absolute;visibility:hidden;white-space:pre"; p.append(zero);
    const chPx = zero.getBoundingClientRect().width; zero.remove();
    const tab = it.querySelector(".td-ctx table"), q = it.querySelector(".td-dtitle"), txt = it.querySelector(".td-text"), acts = it.querySelector(".td-acts");
    return { chPx, pre: pre.getBoundingClientRect().width, preScroll: pre.scrollWidth - pre.clientWidth, ctxW: ctx.getBoundingClientRect().width, p: p.getBoundingClientRect().width,
      table: tab.getBoundingClientRect().right <= ctx.getBoundingClientRect().right + 1, qW: q.getBoundingClientRect().width,
      txtL: txt.getBoundingClientRect().left, ctxL: ctx.getBoundingClientRect().left, txtR: txt.getBoundingClientRect().right, preR: pre.getBoundingClientRect().right, actsR: acts.getBoundingClientRect().right,
      pageOver: document.documentElement.scrollWidth - innerWidth };
  }, item);
  let m = await measure();
  if (m.pre <= 90 * m.chPx + 20 || m.preScroll > 0) throw new Error("2000: the code block should be wider than 90ch with no scroll: " + JSON.stringify(m));
  if (m.p > 72 * m.chPx + 2) throw new Error("2000: prose wider than 72ch: " + JSON.stringify(m));
  if (m.qW < 500) throw new Error("2000: the open item title is cramped: " + JSON.stringify(m));
  if (m.txtL < m.preR + 20 || m.txtR > 2001 || m.actsR > m.txtR + 1) throw new Error("2000: the answer should sit beside the content, inside the pane: " + JSON.stringify(m));
  if (m.pageOver > 0) throw new Error("2000: page scrolls horizontally");
  // Nothing inside an open item scrolls sideways, at any width.
  const hscroll = (sel) => page.evaluate((sel) => [...document.querySelectorAll(sel + " *")].filter((e) => e.offsetParent && e.scrollWidth > e.clientWidth + 1 && getComputedStyle(e).display !== "inline").map((e) => e.tagName + "." + e.className + " " + e.scrollWidth + ">" + e.clientWidth), sel);
  for (const w of [1280, 1600, 2000]) {
    await page.setViewportSize({ width: w, height: 800 });
    await page.waitForTimeout(150);
    const bad = await hscroll(item);
    if (bad.length) throw new Error(w + ": horizontal scroll inside the open To do item: " + bad.join("; "));
  }
  await page.setViewportSize({ width: 2000, height: 800 });
  // A short body: the answer form follows it instead of being pinned far below.
  await drop(id);
  const sid = await post({ question: "short one", kind: "decision", context: "Just a line." });
  await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
  const sitem = `#p-todo .td-item[data-id="${sid}"]`;
  await page.waitForSelector(sitem);
  if (!(await page.locator(sitem + ".open").count())) await page.locator(sitem + " > .td-row").click();
  await page.waitForSelector(sitem + ".open .td-text");
  const gap = await page.evaluate((s) => { const i = document.querySelector(s); return i.querySelector(".td-form").getBoundingClientRect().top - i.querySelector(".td-ctx").getBoundingClientRect().bottom; }, sitem);
  if (gap > 60) throw new Error("a short body leaves a " + gap + "px gap before the answer form");
  await drop(sid);
  // The phone: no page overflow with a long one-line command.
  const pid = await post({ question: "phone width check", kind: "decision", context: ctx });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent");
  await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
  const pitem = `#todo .td-item[data-id="${pid}"]`;
  await page.waitForSelector(pitem);
  if (!(await page.locator(pitem + ".open").count())) await page.locator(pitem + " > .td-row").click();
  await page.waitForSelector(pitem + ".open .td-ctx pre");
  const o = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
  if (o[0] > o[1]) throw new Error("390: page scrolls horizontally " + o);
  const bad390 = await hscroll(pitem);
  if (bad390.length) throw new Error("390: horizontal scroll inside the open To do item: " + bad390.join("; "));
  await drop(pid);
});
await check("pages: the rows above the list open the pages; the open one is marked; leaving an agent clears presence", async () => {
  await desktopList(1280);
  await page.waitForSelector("#nav .nv");
  const posts = [];
  const onReq = (r) => { if (r.url().includes("/api/push/presence")) posts.push(JSON.parse(r.postData() || "{}")); };
  page.on("request", onReq);
  try {
    const rows = await page.locator("#nav .nv span:first-of-type").allInnerTexts();
    for (const want of ["Status", "To do", "Search"]) if (!rows.includes(want)) throw new Error("rows " + JSON.stringify(rows));
    const [n, l] = await Promise.all([geo("#nav"), geo("#agents")]);
    if (!n.shown || n.x !== 0 || n.y > l.y) throw new Error("the rows are not above the agent list: " + JSON.stringify([n, l]));
    if (await page.locator("#rtr-banner").isVisible()) throw new Error("the removal banner shows beside its row");
    await page.locator('#agents .agent[data-id$="w1:p1"]').first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.waitForTimeout(300);
    if (await page.locator("#nav .nv.on").count()) throw new Error("a page row marked over an agent");
    posts.length = 0;
    await page.locator('#nav .nv[data-page="todo"]').click();
    await page.waitForSelector("#p-todo", { state: "visible" });
    if (await visible("#detail")) throw new Error("the agent stays under the page");
    if ((await page.locator("#nav .nv.on").getAttribute("data-page")) !== "todo") throw new Error("row not marked");
    await page.waitForTimeout(300);
    if (!posts.length || posts[posts.length - 1].pane !== "") throw new Error("presence not cleared: " + JSON.stringify(posts));
    await page.locator('#nav .nv[data-page="status"]').click();
    await page.waitForSelector("#p-status", { state: "visible" });
    // Search is a page beside the list: the row is marked, the sidebar stays, Esc does nothing with an empty query.
    await page.locator('#nav .nv[data-page="search"]').click();
    await page.waitForSelector("#recall", { state: "visible" });
    if (!(await page.evaluate(() => location.hash)).startsWith("#/search")) throw new Error("hash " + (await page.evaluate(() => location.hash)));
    if ((await page.locator("#nav .nv.on").getAttribute("data-page")) !== "search") throw new Error("search row not marked");
    if (!(await visible("#agents"))) throw new Error("the sidebar went away");
    if (await visible("#p-status")) throw new Error("the Status page stays under Search");
    await page.keyboard.press("Escape");
    await page.waitForTimeout(150);
    if (!(await visible("#recall"))) throw new Error("Esc left the Search page");
    await page.locator('#nav .nv[data-page="status"]').click();
    await page.waitForSelector("#p-status", { state: "visible" });
    await page.waitForSelector("#recall", { state: "hidden" });
    // Opening Search over an agent leaves it through closeDetail too.
    await page.locator('#agents .agent[data-id$="w1:p1"]').first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    posts.length = 0;
    await page.locator('#nav .nv[data-page="search"]').click();
    await page.waitForSelector("#recall", { state: "visible" });
    await page.waitForTimeout(300);
    if (await page.evaluate(() => state.selected) !== null || !posts.length || posts[posts.length - 1].pane !== "") throw new Error("the agent stays open under Search: " + JSON.stringify(posts));
    await page.locator('#nav .nv[data-page="status"]').click();
    await page.waitForSelector("#p-status", { state: "visible" });
    await page.waitForSelector("#recall", { state: "hidden" });
    // The removals row follows the ledger's marks.
    const marks = await page.evaluate(() => ledger.marked.size);
    const row = await page.locator('#nav .nv[data-page="removals"]').count();
    if ((marks > 0) !== (row === 1)) throw new Error(`removals row ${row} for ${marks} marks`);
    if (row) {
      await page.locator('#nav .nv[data-page="removals"]').click();
      await page.waitForSelector("#review", { state: "visible" });
      if ((await page.locator("#nav .nv.on").getAttribute("data-page")) !== "removals") throw new Error("removals row not marked");
      await page.locator('#nav .nv[data-page="status"]').click();
      await page.waitForSelector("#review", { state: "hidden" });
    }
    if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("page scrolls horizontally");
  } finally { page.off("request", onReq); }
});
// ---- Status: generated from the ledger, the to-do list and the agents (synthetic data, mocked routes) ----
const ago = (min) => new Date(Date.now() - min * 60000).toISOString();
const SYN_LEDGER = { plans: [], marks: [], roles: {}, entries: [
  { id: "h1", repo_name: "webapp", branch: "fix/login-timeout", agent_name: "fix-login", agent_id: "uitest/w1:p1", status: "running", created_at: ago(12), brief: "x" },
  { id: "h2", repo_name: "webapp", branch: "feat/after-login", agent_name: "after-login", status: "waiting", after: ["h1"], created_at: ago(11), brief: "x" },
  { id: "h3", repo_name: "webapp", branch: "chore/old-worker", agent_name: "old-worker", status: "ready_to_remove", finished_at: ago(60), created_at: ago(300), final_report: "Fixed the **header** overlap.\n\nTests pass.", brief: "x" },
  { id: "h4", repo_name: "ci-tools", branch: "feat/arm64", agent_name: "arm64", status: "landed", finished_at: ago(200), created_at: ago(400), final_report: "Added arm64.", brief: "x" },
  { id: "h5", repo_name: "ci-tools", branch: "fix/old", agent_name: "ancient", status: "finished", finished_at: ago(60 * 24 * 5), created_at: ago(60 * 24 * 6), final_report: "Too old to show.", brief: "x" },
  { id: "h6", repo_name: "webapp", branch: "fix/never", agent_name: "never", status: "failed", error: "the worker never started", created_at: ago(120), brief: "x" },
] };
const SYN_TODOS = { open: 1, answered: 0, todos: [{ id: "t1", kind: "decision", question: "Keep old session cookies for 30 days?", state: "open", created_at: ago(25), updated_at: ago(25), waiting: { id: "uitest/w1:p1", name: "fix-login" } }] };
async function mockStatusData() {
  await page.route(/\/api\/ledger$/, (r) => r.fulfill({ json: SYN_LEDGER }));
  await page.route(/\/api\/todos$/, (r) => r.fulfill({ json: SYN_TODOS }));
  await page.route(/\/api\/board$/, (r) => r.fulfill({ json: boardView("t1", "Keep old session cookies for 30 days?") }));
}
async function unmockStatusData() { await page.unroute(/\/api\/ledger$/); await page.unroute(/\/api\/todos$/); await page.unroute(/\/api\/board$/); }
await check("status: the page groups the ledger, to-dos and agents by project, with reports and no Needs you section", async () => {
  await mockStatusData();
  try {
    await desktopList(1280, "#/status");
    await page.waitForSelector("#ps-body .st-proj", { timeout: 8000 });
    const t = await page.locator("#ps-body").innerText();
    for (const want of ["webapp", "ci-tools", "fix-login", "fix/login-timeout", "running 12m", "queued behind 1 step", "Keep old session cookies", "ready to remove", "landed", "failed"]) if (!t.includes(want)) throw new Error(`missing "${want}" in: ${t.slice(0, 400)}`);
    if (t.includes("Too old to show") || t.includes("ancient")) throw new Error("an entry older than 2 days is shown");
    const cards = await page.locator("#ps-body .st-proj").allInnerTexts();
    if (cards.some((c) => /needs you/i.test(c))) throw new Error("a Needs you section in the project cards");
    if (!(await page.locator("#ps-body .st-g.blocked").count()) || !(await page.locator("#ps-body .st-g.you").count()) || !(await page.locator("#ps-body .st-g.prog").count())) throw new Error("groups missing");
    // A report opens in place and stays open across a re-render.
    await page.locator('#ps-body details[data-id="h3"] summary').click();
    await page.waitForSelector("#ps-body .st-rep strong");
    await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
    if (!(await page.locator('#ps-body details[data-id="h3"]').evaluate((d) => d.open))) throw new Error("report closed by a re-render");
    // The links: to-do goes to the To do page; the old briefing is still readable.
    await page.locator('#ps-body [data-go="todo"]').click();
    await page.waitForSelector("#p-todo", { state: "visible" });
    await page.locator('#nav .nv[data-page="status"]').click();
    await page.locator('#ps-body [data-go="briefing"]').click();
    await page.waitForSelector("#briefs.open", { state: "visible" });
    if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("page scrolls horizontally");
    await page.locator('#nav .nv[data-page="status"]').click();
    await page.screenshot({ path: `${out}/status-1280.png` });
    await page.setViewportSize({ width: 1600, height: 800 });
    await page.screenshot({ path: `${out}/status-1600.png` });
  } finally { await unmockStatusData(); }
});
await check("status: readable on the phone, at the top of the briefings screen, with no sideways scroll", async () => {
  await mockStatusData();
  try {
    await desktopList(390);
    await page.goto(base + "/#/briefings", { waitUntil: "load" });
    await page.waitForSelector("#br-status .st-proj", { timeout: 8000 });
    const t = await page.locator("#br-status").innerText();
    if (!t.includes("fix-login") || !t.includes("queued behind")) throw new Error(t.slice(0, 300));
    if (await page.locator("#br-status [data-go]").count()) throw new Error("desktop-only links on the phone");
    await noOverflow();
    await page.screenshot({ path: `${out}/status-390.png` });
  } finally { await unmockStatusData(); }
});

// ---- the briefing board (mocked GET /api/board): lanes with counts, the switch, Needs you linking to its to-do ----
const boardSummary = (box) => page.evaluate((b) => {
  const root = document.querySelector(b + " .bd");
  if (!root) return null;
  return {
    lanes: [...root.querySelectorAll(".bd-lane")].map((d) => ({ lane: d.dataset.lane, count: d.querySelector(".bd-chip").textContent, open: d.open })),
    head: (root.querySelector(".bd-head") || {}).textContent || "",
    pressed: [...root.querySelectorAll("[data-bd-mode]")].filter((x) => x.getAttribute("aria-pressed") === "true").map((x) => x.dataset.bdMode),
    wide: document.documentElement.scrollWidth > innerWidth,
    todo: root.querySelector("[data-bd-todo]") ? root.querySelector("[data-bd-todo]").dataset.bdTodo : "",
    h: root.querySelector("[data-bd-todo]") ? root.querySelector("[data-bd-todo]").getBoundingClientRect().height : 0,
  };
}, box);
for (const [width, box, start] of [[1280, "#ps-body", "#/status"], [390, "#br-status", "#/briefings"]]) {
  await check(`board: lanes, counts, the switch and the Needs you link at ${width}px`, async () => {
    await mockStatusData();
    try {
      await page.evaluate(() => { try { localStorage.removeItem("comebye-board-mode"); } catch {} });
      await desktopList(width, start);
      if (width < 1100) await page.goto(base + start, { waitUntil: "load" });
      await page.waitForSelector(`${box} .bd .bd-lane`, { timeout: 8000 });
      let b = await boardSummary(box);
      const want = ["needs_you:3", "running:2", "waiting:2", "landing:2", "landed:2"];
      if (b.lanes.map((l) => l.lane + ":" + l.count).join() !== want.join()) throw new Error("lanes " + JSON.stringify(b.lanes));
      if (!b.lanes[0].open || b.lanes.slice(1).some((l) => l.open)) throw new Error("only Needs you is open at first: " + JSON.stringify(b.lanes));
      if (!b.head.startsWith("Tue 20:11. v0.5.20 is rolling out")) throw new Error("headline " + b.head);
      if (b.pressed.join() !== "status" || b.wide) throw new Error(JSON.stringify(b));
      if (b.h < 44) throw new Error("the Needs you row is only " + b.h + "px tall");
      // A lane opens, and stays open across a re-render.
      await page.locator(`${box} .bd-lane[data-lane="waiting"] summary`).click();
      await page.evaluate(() => { board.at = 0; return loadBoard(); });
      if (!(await page.locator(`${box} .bd-lane[data-lane="waiting"]`).evaluate((d) => d.open))) throw new Error("a lane closed on re-render");
      const t = await page.locator(`${box} .bd`).innerText();
      for (const w of ["Release v0.5.20", "Waits for the RFC, on purpose.", "the rollout is waiting on the hub."]) if (!t.includes(w)) throw new Error("no " + w);
      await page.screenshot({ path: `${out}/board-status-${width}.png` });
      // By project: Needs you stays, the other lanes regroup with a lane tag per item.
      await page.locator(`${box} [data-bd-mode="project"]`).click();
      b = await boardSummary(box);
      const keys = b.lanes.map((l) => l.lane);
      if (keys[0] !== "needs_you" || !keys.includes("p:comebye") || !keys.includes("p:openclaw-enterprise") || keys.includes("running")) throw new Error("projects " + keys);
      const comebye = b.lanes.find((l) => l.lane === "p:comebye");
      if (comebye.count !== "5") throw new Error("comebye count " + comebye.count);
      if (b.pressed.join() !== "project") throw new Error("pressed " + b.pressed);
      await page.locator(`${box} .bd-lane[data-lane="p:comebye"] summary`).click();
      if (!(await page.locator(`${box} .bd-lane[data-lane="p:comebye"] .bd-tag`).count())) throw new Error("no lane tags by project");
      await page.screenshot({ path: `${out}/board-project-${width}.png` });
      await page.reload({ waitUntil: "load" });
      await page.waitForSelector(`${box} .bd .bd-lane`, { timeout: 8000 });
      if ((await boardSummary(box)).pressed.join() !== "project") throw new Error("the grouping is not remembered");
      await page.locator(`${box} [data-bd-mode="status"]`).click();
      // Needs you links to its to-do.
      await page.locator(`${box} [data-bd-todo="t1"]`).click();
      if (width >= 1100) {
        await page.waitForSelector("#p-todo", { state: "visible" });
        if (!(await page.locator("#pt-body .td-item.open").count())) throw new Error("the to-do is not open on the To do page");
        const q = await page.locator("#pt-body .td-item.open").innerText();
        if (!q.includes("Keep old session cookies")) throw new Error("opened " + q.slice(0, 80));
      } else {
        await page.waitForSelector("#todo:not([hidden])", { timeout: 5000 });
        if (await page.evaluate(() => document.querySelector("#briefs").classList.contains("open"))) throw new Error("the briefings screen stays open");
      }
    } finally { await unmockStatusData(); }
  });
}
for (const [width, box, start] of [[1280, "#ps-body", "#/status"], [390, "#br-status", "#/briefings"]]) {
  await check(`board: the new mark: none on a first visit, new and moved after a change, cleared by Mark seen, at ${width}px`, async () => {
    await mockStatusData();
    try {
      const marks = () => page.evaluate((b) => [...document.querySelectorAll(b + " .bd .bd-new")].map((x) => x.textContent).join("|"), box);
      const go = async () => { await desktopList(width, start); if (width < 1100) await page.goto(base + start, { waitUntil: "load" }); await page.waitForSelector(`${box} .bd .bd-lane`, { timeout: 8000 }); };
      await page.evaluate(() => { try { localStorage.removeItem("comebye-board-seen"); localStorage.removeItem("comebye-board-mode"); } catch {} });
      await go();
      if (await marks()) throw new Error("marks on a first visit: " + await marks());
      if (await page.locator(`${box} [data-bd-seen]`).count()) throw new Error("Mark seen on a first visit");
      const snap = await page.evaluate(() => JSON.parse(localStorage.getItem("comebye-board-seen")));
      if (!snap || snap.items.t1 !== "needs_you" || snap.items.h888888888888 !== "landed") throw new Error("snapshot " + JSON.stringify(snap));
      // An older snapshot: one item missing (new), one in another lane (moved).
      await page.evaluate(() => { const v = JSON.parse(localStorage.getItem("comebye-board-seen")); delete v.items.h333333333333; v.items.h111111111111 = "waiting"; localStorage.setItem("comebye-board-seen", JSON.stringify(v)); });
      await page.reload({ waitUntil: "load" }); // desktopList would clear the snapshot just written
      await page.waitForSelector(`${box} .bd .bd-lane`, { timeout: 8000 });
      const m = await marks();
      if (!/new/.test(m) || !/moved/.test(m)) throw new Error("marks " + m);
      const btn = await page.locator(`${box} [data-bd-seen]`).innerText();
      if (!btn.startsWith("2 new or moved")) throw new Error("button " + btn);
      if ((await page.locator(`${box} [data-bd-seen]`).evaluate((b) => b.getBoundingClientRect().height)) < 44) throw new Error("Mark seen is under 44px");
      await page.locator(`${box} .bd-lane[data-lane="running"] summary`).click();
      await page.screenshot({ path: `${out}/board-new-${width}.png` });
      if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("page scrolls sideways");
      // By project keeps the marks.
      await page.locator(`${box} [data-bd-mode="project"]`).click();
      if (!/new/.test(await marks())) throw new Error("no marks by project");
      await page.locator(`${box} [data-bd-mode="status"]`).click();
      await page.locator(`${box} [data-bd-seen]`).click();
      if (await marks() || (await page.locator(`${box} [data-bd-seen]`).count())) throw new Error("Mark seen left marks: " + await marks());
      await page.reload({ waitUntil: "load" });
      await page.waitForSelector(`${box} .bd .bd-lane`, { timeout: 8000 });
      if (await marks()) throw new Error("marks came back after a reload");
    } finally { await unmockStatusData(); }
  });
}
await check("board: empty lanes are left out, and no board shows without one", async () => {
  await page.route(/\/api\/board$/, (r) => r.fulfill({ json: { at: ago(0), lanes: [{ key: "needs_you", name: "Needs you", rule: "r", count: 0, items: [] }, { key: "running", name: "Running", rule: "r", count: 0, items: [] }], loose: {} } }));
  try {
    await desktopList(1280, "#/status");
    await page.waitForSelector("#ps-body .bd", { timeout: 8000 });
    const t = await page.locator("#ps-body .bd").innerText();
    if (!t.includes("Nothing needs you") || !t.includes("Nothing else in flight") || (await page.locator("#ps-body .bd-lane").count())) throw new Error(t);
  } finally { await page.unroute(/\/api\/board$/); }
});

// A host reports a damaged repository: the Status page says how to repair it, and
// the agents working in it are marked. Runs in both schemes (run.sh), at both widths.
// ---- Plan card: members link to their agents ----
const PLAN_LEDGER = { marks: [], roles: {}, entries: [
  { id: "m1", host: "uitest", repo_name: "webapp", branch: "feat/openshell-sandbox", agent_id: "uitest/w1:p1", pane_id: "w1:p1", status: "landing", finished_at: null, created_at: ago(90), brief: "x", plan_id: "pl1", final_report: "Done **it**." },
  { id: "m2", host: "uitest", repo_name: "infra", branch: "feat/removed-one", agent_id: "uitest/w77:p1", pane_id: "w77:p1", status: "removed", finished_at: ago(30), created_at: ago(80), brief: "x", plan_id: "pl1" },
], plans: [{ id: "pl1", title: "OpenShell sandbox with a long enough title that it cannot fit on a single line of the card", size: 2, state: "done", done: 2, landing: 1, created_at: ago(95), settled_at: ago(30), members: [
  { id: "m1", host: "uitest", repo_name: "webapp", branch: "feat/openshell-sandbox", agent_id: "uitest/w1:p1", pane_id: "w1:p1", status: "landing", outcome: "ok", followup: "landing", reported: true },
  { id: "m2", host: "uitest", repo_name: "infra", branch: "feat/removed-one", agent_id: "uitest/w77:p1", pane_id: "w77:p1", status: "removed", outcome: "ok", reported: false },
] }] };
for (const width of [1280, 390]) {
  await check(`plan card: at ${width}px a member opens its agent, a gone one opens its transcript, a long title stays on one line, keys work`, async () => {
    await page.route(/\/api\/ledger$/, (r) => r.fulfill({ json: PLAN_LEDGER }));
    try {
      await desktopList(width, "#/status");
      await page.waitForSelector("#plans .plan-title", { state: "visible", timeout: 8000 });
      const title = page.locator("#plans .plan-title");
      const one = await title.evaluate((e) => ({ lines: Math.round(e.getBoundingClientRect().height / parseFloat(getComputedStyle(e).lineHeight)), cut: e.scrollWidth > e.clientWidth, ell: getComputedStyle(e).textOverflow, tip: e.title }));
      if (one.lines !== 1 || !one.cut || one.ell !== "ellipsis" || !one.tip.includes("a single line of the card")) throw new Error("title is not one truncated line with the full title as tooltip: " + JSON.stringify(one));
      const t = await page.locator("#plans").innerText();
      if (!/done · 1 landing/i.test(t)) throw new Error("no landing shown: " + t);
      if (await page.locator("#plans .pm").count()) throw new Error("a done plan shows its members before it is opened");
      if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("page scrolls sideways");
      await title.click(); // opens the done plan
      await page.waitForSelector("#plans .plan .pm", { state: "visible" });
      // Opened above: the report is there and the card keeps working.
      await page.waitForSelector("#plans .pm-rep", { state: "visible" });
      // Keyboard: focus the first member, Enter opens its agent.
      await page.locator('#plans a.pm').first().focus();
      await page.keyboard.press("Enter");
      await page.waitForSelector("#detail", { state: "visible" });
      if (!/#\/agent\/w1(:|%3A)p1/.test(await page.evaluate(() => location.hash))) throw new Error("hash " + await page.evaluate(() => location.hash));
      // The gone one: a tooltip says so and the click lands on that handoff's transcript.
      await page.goto(base + "/#/status", { waitUntil: "load" });
      await page.waitForSelector("#plans a.pm.gone", { state: "visible" });
      const tip = await page.locator("#plans a.pm.gone").getAttribute("title");
      if (!/gone/.test(tip)) throw new Error("tooltip " + tip);
      await page.locator("#plans a.pm.gone").click();
      await page.waitForSelector("#recall", { state: "visible" });
      const h = await page.evaluate(() => decodeURIComponent(location.hash));
      if (!h.startsWith("#/search/for?ledger=")) throw new Error("hash " + h);
      await page.waitForFunction(() => /isn't in the transcript index yet/.test(document.querySelector("#rc-session")?.textContent || ""), null, { timeout: 5000 });
    } finally { await page.unroute(/\/api\/ledger$/); }
  });
}
const PLAN_SHORT = { marks: [], roles: {}, entries: [], plans: [{ id: "pl2", title: "Structural to-dos, slice 1", size: 2, state: "open", done: 0, created_at: ago(20), members: [
  { id: "n1", host: "uitest", repo_name: "comebye", branch: "feat/todo-asks", agent_id: "uitest/w1:p1", pane_id: "w1:p1", status: "running", outcome: "pending", reported: false },
  { id: "n2", host: "uitest", repo_name: "comebye", branch: "feat/todo-auto", agent_id: "uitest/w2:p1", pane_id: "w2:p1", status: "running", outcome: "pending", reported: false },
] }] };
for (const width of [1280, 390]) {
  await check(`plan card: at ${width}px a title stays on one line, the state sits under it, and the focus ring is for the keyboard only`, async () => {
    await page.route(/\/api\/ledger$/, (r) => r.fulfill({ json: PLAN_SHORT }));
    try {
      await desktopList(width, "#/status");
      await page.waitForSelector("#plans .plan .pm", { state: "visible", timeout: 8000 });
      const title = page.locator("#plans .plan-title");
      const lines = await title.evaluate((e) => Math.round(e.getBoundingClientRect().height / parseFloat(getComputedStyle(e).lineHeight)));
      if (lines !== 1) throw new Error(`title takes ${lines} lines`);
      const geo = await page.evaluate(() => { const t = document.querySelector("#plans .plan-title").getBoundingClientRect(), s = document.querySelector("#plans .plan-state").getBoundingClientRect(); return { below: s.top >= t.bottom - 1 }; });
      if (!geo.below) throw new Error("state is not under the title");
      const ring = () => page.evaluate(() => { const a = document.activeElement; if (!a || !a.closest("#plans")) return "none"; const c = getComputedStyle(a); return c.outlineStyle !== "none" && parseFloat(c.outlineWidth) > 0 ? "ring" : "none"; });
      // A mouse click leaves no ring on the title (the card re-renders under it).
      await title.click();
      await page.waitForTimeout(150);
      if ((await ring()) !== "none") throw new Error("a ring after a mouse click on the title");
      await page.locator("#plans a.pm").first().focus();
      await page.keyboard.press("Shift+Tab"); // keyboard moves to the title
      if (!(await page.evaluate(() => document.activeElement.classList.contains("plan-title")))) throw new Error("Shift+Tab did not reach the title");
      if ((await ring()) !== "ring") throw new Error("no ring after Tab");
      await page.keyboard.press("Enter"); // collapses (the click opened it); focus stays and keeps its ring
      await page.waitForSelector("#plans .plan-meta", { state: "detached" });
      if (!(await page.evaluate(() => document.activeElement.classList.contains("plan-title"))) || (await ring()) !== "ring") throw new Error("focus lost on a keyboard toggle");
      await page.locator("#plans").screenshot({ path: `${out}/plan-card-polish-${width}.png` });
    } finally { await page.unroute(/\/api\/ledger$/); }
  });
}
const mk = (id, status, outcome = "ok") => ({ id, host: "uitest", repo_name: "comebye", branch: "feat/" + id, status, outcome, reported: true });
const PLAN_MANY = { marks: [], roles: {}, entries: [], plans: [
  { id: "d1", title: "Migrate the billing service to the new queue", size: 3, state: "done", done: 3, created_at: ago(200), settled_at: ago(30), members: [mk("a1", "finished"), mk("a2", "ready_to_remove"), mk("a3", "landed")] },
  { id: "d2", title: "Everything landed already", size: 2, state: "done", done: 2, created_at: ago(200), settled_at: ago(30), members: [mk("b1", "landed"), mk("b2", "removed")] },
  { id: "p1", title: "One worker failed", size: 2, state: "partial", done: 1, created_at: ago(200), settled_at: ago(30), members: [mk("c1", "landed"), mk("c2", "removed", "failed")] },
  { id: "o1", title: "Still going", size: 2, state: "open", done: 0, created_at: ago(20), members: [mk("e1", "running", "pending"), mk("e2", "running", "pending")] },
] };
for (const width of [1280, 390]) {
  await check(`plan card: at ${width}px a done plan collapses to a line, dismisses per browser, hides once landed; a partial one never does`, async () => {
    await page.route(/\/api\/ledger$/, (r) => r.fulfill({ json: PLAN_MANY }));
    try {
      await desktopList(width, "#/status");
      await page.waitForSelector("#plans .plan", { state: "visible", timeout: 8000 });
      const ids = () => page.locator("#plans .plan-title").evaluateAll((es) => es.map((e) => e.dataset.plan));
      if (JSON.stringify(await ids()) !== JSON.stringify(["d1", "p1", "o1"])) throw new Error("plans shown: " + JSON.stringify(await ids()) + " (d2, every member landed or removed, must be hidden)");
      const d1 = page.locator('#plans .plan:has([data-plan="d1"])');
      const p1 = page.locator('#plans .plan:has([data-plan="p1"])');
      const o1 = page.locator('#plans .plan:has([data-plan="o1"])');
      if (!(await d1.evaluate((e) => e.classList.contains("collapsed"))) || (await d1.locator(".pm").count())) throw new Error("done plan is not collapsed");
      const dt = await d1.innerText();
      if (!dt.includes("✓") || !/done · 3 of 3/i.test(dt)) throw new Error("collapsed line: " + dt);
      const dh = await d1.evaluate((e) => e.getBoundingClientRect().height);
      if (dh > 60) throw new Error("collapsed plan is " + dh + "px tall");
      if (await p1.evaluate((e) => e.classList.contains("collapsed")) || !(await p1.locator(".pm").count())) throw new Error("a partial plan collapsed");
      if (await o1.locator(".plan-x").count()) throw new Error("an open plan can be dismissed");
      await page.locator("#plans").screenshot({ path: `${out}/plan-card-collapsed-${width}.png` });
      // Click opens, click closes.
      await d1.locator(".plan-title").click();
      await page.waitForSelector('#plans .plan:has([data-plan="d1"]) .pm', { state: "visible" });
      await page.locator("#plans").screenshot({ path: `${out}/plan-card-expanded-${width}.png` });
      await page.locator('#plans [data-plan="d1"]').click();
      await page.waitForFunction(() => document.querySelector('#plans .plan [data-plan="d1"]').closest(".plan").classList.contains("collapsed"));
      // Dismiss hides it, and it stays hidden after a reload; the partial plan is still there.
      await d1.locator(".plan-x").click();
      await page.waitForFunction(() => !document.querySelector('#plans [data-plan="d1"]'));
      if (await page.evaluate(() => document.activeElement && document.activeElement.matches && document.activeElement.closest("#plans") ? true : false)) throw new Error("focus left inside a removed card");
      await page.reload({ waitUntil: "load" });
      await page.waitForSelector("#plans .plan", { state: "visible", timeout: 8000 });
      if (JSON.stringify(await ids()) !== JSON.stringify(["p1", "o1"])) throw new Error("after reload: " + JSON.stringify(await ids()));
      // A partial plan is dismissable by hand but is never hidden on its own: its members are all landed or removed, and it is still shown.
      await page.locator('#plans .plan:has([data-plan="p1"]) .plan-x').click();
      await page.waitForFunction(() => !document.querySelector('#plans [data-plan="p1"]'));
      await page.screenshot({ path: `${out}/plan-card-collapse-${width}.png` });
    } finally { await page.unroute(/\/api\/ledger$/); }
  });
  await check(`plan card: at ${width}px a ~40-character title stays on one line`, async () => {
    const title40 = "Rate limits for the public API gateway!!"; // 40 characters
    await page.route(/\/api\/ledger$/, (r) => r.fulfill({ json: { ...PLAN_SHORT, plans: [{ ...PLAN_SHORT.plans[0], title: title40 }, { ...PLAN_SHORT.plans[0], id: "pl3", title: title40 + " and then some more words to push it past the edge of the card" }] } }));
    try {
      await desktopList(width, "#/status");
      await page.waitForSelector("#plans .plan-title", { state: "visible", timeout: 8000 });
      const info = await page.locator("#plans .plan-title").evaluateAll((es) => es.map((e) => ({ lines: Math.round(e.getBoundingClientRect().height / parseFloat(getComputedStyle(e).lineHeight)), cut: e.scrollWidth > e.clientWidth, tip: e.title.split("\n")[0] })));
      if (info.length !== 2 || info.some((i) => i.lines !== 1)) throw new Error("not one line: " + JSON.stringify(info));
      if (!info[1].cut || !info[1].tip.includes("push it past")) throw new Error("long title not cut with a tooltip: " + JSON.stringify(info));
      await page.locator("#plans").screenshot({ path: `${out}/plan-card-longtitle-${width}.png` });
    } finally { await page.unroute(/\/api\/ledger$/); }
  });
}
async function injectRepoDamage() {
  return page.evaluate(() => {
    const a = state.agents.find((x) => (x.worktree && x.worktree.checkout) || x.cwd);
    const dir = (a.worktree && a.worktree.checkout) || a.cwd;
    const local = state.hosts.find((h) => h.local) || state.hosts[0];
    local.repo_damage = [{
      repo: "OpenShell", path: "/home/dev/code/OpenShell", source: "fetch", since: new Date(Date.now() - 3600e3).toISOString(),
      reason: "the local repository has missing or damaged objects", ref: "refs/remotes/origin/pull-request/4789",
      hint: "Delete the broken remote-tracking ref and fetch again: git -C /home/dev/code/OpenShell update-ref -d refs/remotes/origin/pull-request/4789 && git -C /home/dev/code/OpenShell fetch origin. Then check with git -C /home/dev/code/OpenShell fsck --connectivity-only.",
      worktrees: ["/home/dev/code/OpenShell", dir],
    }];
    renderList(); renderStatus();
    return agentId(a);
  });
}
for (const width of [1280, 390]) {
  await check(`repo damage: Status says how to repair it and the agents in it are marked (${width})`, async () => {
    await mockStatusData();
    try {
      await desktopList(width, "#/status");
      if (width < 1100) await page.goto(base + "/#/briefings", { waitUntil: "load" });
      await page.waitForSelector(width >= 1100 ? "#ps-body .st-proj" : "#br-status .st-proj", { timeout: 8000 });
      const id = await injectRepoDamage();
      const box = width >= 1100 ? "#ps-body" : "#br-status";
      const r = await page.evaluate(([box, id]) => {
        const sec = document.querySelector(box + " .st-repair");
        const row = document.querySelector(`#agents .agent[data-id="${CSS.escape(id)}"] .rd-chip`);
        return { text: sec ? sec.innerText : "", first: document.querySelector(box + " .st").firstElementChild.nextElementSibling === sec,
          sum: document.querySelector(box + " .st-sum").textContent, chip: row ? row.textContent : "", title: row ? row.title : "",
          others: document.querySelectorAll("#agents .rd-chip").length };
      }, [box, id]);
      if (!r.text.includes("Repositories need repair") || !r.text.includes("OpenShell") || !r.text.includes("update-ref -d refs/remotes/origin/pull-request/4789") || !r.text.includes("1 agent works in it")) throw new Error(r.text.slice(0, 300));
      if (!r.first) throw new Error("the repair section is not first");
      if (!r.sum.startsWith("1 repository needs repair")) throw new Error(r.sum);
      if (r.chip !== "repo damaged" || !r.title.includes("missing or damaged objects")) throw new Error(JSON.stringify(r));
      if (r.others !== 1) throw new Error(`${r.others} agents marked`);
      await noOverflow();
      await page.locator(box + " .st-repair").screenshot({ path: `${out}/repo-damage-${width}.png` });
    } finally { await unmockStatusData(); }
  });
}

await check("pages: below the breakpoint the page hashes do nothing and openPage is a no-op", async () => {
  await desktopList(1099);
  await page.goto(base + "/#/todo", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent[data-id]", { state: "visible" });
  await page.evaluate(() => openPage("briefing"));
  const r = await page.evaluate(() => ({ view: state.view, hash: location.hash, cls: document.body.className }));
  if (r.view !== null || r.hash !== "#/todo" || /view-|has-page/.test(r.cls)) throw new Error("state " + JSON.stringify(r));
});

await check("desktop: below the breakpoint (1099px) it is still the phone layout", async () => {
  await desktopList(1099);
  const [l, a] = await Promise.all([geo("#list"), geo("#agents")]);
  if (a.x < 100 || a.w > 640) throw new Error("not the centred single column: " + JSON.stringify(a));
  if (await visible("#detail-empty")) throw new Error("desktop empty state shown");
  const st = await briefState();
  if (st.expanded !== "false") throw new Error("briefing not collapsed: " + JSON.stringify(st));
  if (await page.evaluate(() => document.body.classList.contains("has-briefs") || document.querySelector("#briefs").classList.contains("open"))) throw new Error("the briefings panel stays open below the breakpoint");
});
await check("phone: 390px is unchanged: one column, back button, collapsed briefing, no desktop pane", async () => {
  await desktopList(390);
  const a = await geo("#agents");
  if (a.x > 20 || a.w < 340 || a.w > 390) throw new Error("agents column: " + JSON.stringify(a));
  if (await visible("#detail-empty") || await visible("#detail")) throw new Error("desktop pane visible");
  const st = await briefState();
  if (st.expanded !== "false" || st.body) throw new Error("briefing not collapsed: " + JSON.stringify(st));
  const f = await geo("#f-menu"); // hidden until opened
  await page.locator('#agents .agent[data-id$="w1:p1"]').first().click();
  await page.waitForSelector("#detail", { state: "visible" });
  if (await visible("#list") && (await geo("#agents")).shown) throw new Error("the list stays visible under the agent on a phone");
  const [d, back] = await Promise.all([geo("#detail"), geo("#back")]);
  if (d.x !== 0 || d.w !== 390 || !back.shown) throw new Error("detail not full-screen with a back button: " + JSON.stringify([d, back]));
  await page.locator("#back").click();
  await page.waitForSelector("#agents .agent", { state: "visible" });
});

// ---- the key controls on a phone: an idle agent at a prompt ----
const inView = async (sel, vh) => { const g = await geo(sel); return g && g.shown && g.y >= 0 && g.y + g.h <= vh && g.x >= 0 && g.x + g.w <= 390 ? null : `${sel} not in the viewport: ${JSON.stringify(g)}`; };
const SCREEN_RE = /\/output\?/;
for (const vh of [844, 664]) {
  await check(`keys: 390x${vh}, idle agent: + -> Keys... shows the pad; Esc and Enter are on screen and arrive`, async () => {
    await page.setViewportSize({ width: 390, height: vh });
    await coldOpen("bravo");
    await page.locator("#btn-attach").tap();
    await page.locator("#attach-keys").tap();
    if (!(await visible("#keypad"))) throw new Error("the keypad did not show");
    for (const k of ["esc", "enter"]) {
      const bad = await inView(`#keypad [data-key=${k}]`, vh);
      if (bad) throw new Error(bad);
    }
    await page.locator("#keypad [data-key=esc]").tap();
    await page.locator("#keypad [data-key=enter]").tap();
    await page.waitForFunction(() => /\[keys\] esc[\s\S]*\[keys\] enter/.test(document.querySelector("#output").textContent), null, { timeout: 4000 });
  });
}
await check("keys: a screen ending in a prompt footer shows an Esc/Enter strip with no taps; Keys... widens it", async () => {
  await page.setViewportSize({ width: 390, height: 664 });
  await page.route(SCREEN_RE, (r) => r.fulfill({ json: { text: "Trust project folder?\n  /home/u/code/x\n\n  ↑↓ navigate  enter select  escape/ctrl+c cancel", revision: 1 } }), { times: 20 });
  await coldOpen("bravo");
  await page.waitForFunction(() => !document.querySelector("#keypad").hidden, null, { timeout: 4000 });
  for (const k of ["esc", "enter", "up", "down"]) {
    const bad = await inView(`#keypad [data-key=${k}]`, 664);
    if (bad) throw new Error(bad);
  }
  if (await visible("#keypad [data-key=tab]")) throw new Error("the auto strip shows the full pad");
  await page.locator("#btn-attach").tap();
  await page.locator("#attach-keys").tap();
  if (!(await visible("#keypad [data-key=tab]"))) throw new Error("Keys... did not widen the strip");
  await page.unroute(SCREEN_RE);
});
await page.setViewportSize({ width: 390, height: 844 });

// ---- inputs answer within a frame, whatever the network does ----
//
// Every request below is held for a second or more (or refused). The tap must
// show its result in the next frame, a refresh while the request is out must
// not undo it, and a failed request must roll it back with an error.
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const SLOW = 600, FRAME_MS = 120; // a frame is ~16ms; this leaves room for a loaded CI box
// tapThen runs clickJs in the page, waits one frame and returns [snapJs(), ms since the tap].
const tapThen = (clickJs, snapJs) => page.evaluate(([c, s]) => {
  const t0 = performance.now();
  new Function(c)();
  return new Promise((r) => requestAnimationFrame(() => r([new Function("return (" + s + ")")(), Math.round(performance.now() - t0)])));
}, [clickJs, snapJs]);
const inFrame = ([, ms], what) => { if (ms > FRAME_MS) throw new Error(`${what}: took ${ms}ms, not one frame (the request is held ${SLOW}ms)`); };
// hold routes a request: after ms it is refused (fail: message), answered (json) or sent on.
async function hold(re, method, ms, { fail, json } = {}) {
  const h = async (r) => {
    if (r.request().method() !== method) return r.fallback();
    await sleep(ms);
    if (fail) return r.fulfill({ status: 500, json: { message: fail } });
    if (json) return r.fulfill({ json });
    return r.continue();
  };
  await page.route(re, h);
  return () => page.unroute(re, h);
}
const jpost = (u, b) => page.evaluate(([u, b]) => fetch(u, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(b) }).then((r) => r.json()), [u, b]);
const toastNow = () => page.evaluate(() => [document.querySelector("#toast").className, document.querySelector("#toast").textContent]);

async function openBlocked() { // the Pi trust prompt: five options as buttons
  await page.goto(base + "/", { waitUntil: "load" });
  const row = page.locator('#agents .agent[data-id$="w9:p1"]');
  await row.waitFor({ state: "visible" });
  await page.waitForFunction(() => /blocked/i.test(document.querySelector('#agents .agent[data-id$="w9:p1"]')?.innerText || ""), null, { timeout: 8000 });
  await row.click();
  await page.locator(".ans-opts button.opt").first().waitFor({ state: "visible", timeout: 8000 });
}
const OPT = `document.querySelectorAll('.ans-opts button.opt:not(.cancel):not(.submit)')[1]`;

await check("latency: an option tap shows chosen in a frame; a refresh in flight does not undo it; it stays until the screen moves", async () => {
  await openBlocked();
  const off = await hold(/\/answer$/, "POST", SLOW, { json: { ok: true, keys: ["down", "enter"] } });
  try {
    const t = await tapThen(`${OPT}.click()`, `[${OPT}.className, document.querySelectorAll('.ans-opts button.opt.locked').length, document.querySelectorAll('.ans-opts button.opt:disabled').length].join("|")`);
    inFrame(t, "option tap");
    const [cls, locked, disabled] = t[0].split("|");
    if (!/\bchosen\b/.test(cls) || !/\bpend\b/.test(cls)) throw new Error("the tapped option does not show as chosen/pending: " + cls);
    if (Number(locked) < 3) throw new Error("the other options do not hold still: " + locked);
    if (disabled !== "0") throw new Error("buttons were disabled (the card feels dead): " + disabled);
    await sleep(300);
    // A poll and an SSE redraw land while the answer is out.
    await page.evaluate(() => { refreshDialog(); renderAnswerCard(agentById(state.selected)); });
    await sleep(200);
    if (!/\bchosen\b/.test(await page.evaluate(`${OPT}.className`))) throw new Error("a refresh undid the tap");
    // A second tap while it is out is refused, not sent.
    await page.evaluate(`document.querySelectorAll('.ans-opts button.opt')[3].click()`);
    await sleep(SLOW);
    // Confirmed: the screen still shows the old dialog (nothing really moved, the answer was mocked): that is stale, not a rollback.
    await page.evaluate(() => refreshDialog());
    await sleep(500);
    if (!/\bchosen\b/.test(await page.evaluate(`${OPT}.className`))) throw new Error("a refresh that still shows the old dialog undid a confirmed tap");
    if (!/sent/.test((await toastNow())[1])) throw new Error("no confirmation toast");
  } finally { await off(); }
});

await check("latency: a refused answer rolls the card back with the error, and a second tap sends it", async () => {
  await openBlocked();
  const posts = [];
  const spy = async (r) => { if (r.request().method() === "POST") posts.push(JSON.parse(r.request().postData() || "{}")); await r.fallback(); };
  await page.route(/\/answer$/, spy);
  const off = await hold(/\/answer$/, "POST", 600, { fail: "herdr is down" });
  try {
    const t = await tapThen(`${OPT}.click()`, `${OPT}.className`);
    inFrame(t, "option tap");
    if (!/\bchosen\b/.test(t[0])) throw new Error("not chosen at once: " + t[0]);
    await page.waitForSelector("#answer .ans-err", { timeout: 4000 });
    const r = await page.evaluate(`[${OPT}.className, document.querySelector('#answer .ans-err').textContent, document.querySelectorAll('.ans-opts button.opt.locked, .ans-opts button.opt:disabled').length]`);
    if (/\bchosen\b|\bpend\b/.test(r[0])) throw new Error("still chosen after the failure: " + r[0]);
    if (!/herdr is down/.test(r[1])) throw new Error("error not shown: " + r[1]);
    if (r[2] !== 0) throw new Error("buttons stay locked after a failure");
    const [tc, tx] = await toastNow();
    if (!/error/.test(tc) || !/herdr is down/.test(tx)) throw new Error("toast " + tc + " " + tx);
  } finally { await off(); }
  const ok = await hold(/\/answer$/, "POST", 200, { json: { ok: true, keys: ["enter"] } });
  try {
    const t = await tapThen(`${OPT}.click()`, `${OPT}.className`); // retry
    inFrame(t, "retry tap");
    if (!/\bchosen\b/.test(t[0])) throw new Error("retry not chosen: " + t[0]);
    await sleep(500);
    if (await page.locator("#answer .ans-err").count()) throw new Error("the old error stays after a retry");
  } finally { await ok(); await page.unroute(/\/answer$/, spy); }
});

await check("latency: ticking several boxes quickly shows each at once and sends them in order", async () => {
  await openBlocked();
  const checked = [false, false, false];
  let n = 0;
  const bodies = [];
  const dialog = () => ({ kind: "claude_question", agent: "claude", header: "Pick", question: "Which?", multi: true, answerable: true, selected: 0, cancel_label: "Cancel",
    fingerprint: "fp" + n, options: [...checked.map((c, i) => ({ label: "Opt " + "ABC"[i], checked: c })), { label: "Next", role: "next" }] });
  const rd = async (r) => { if (r.request().method() === "GET") return r.fulfill({ json: { dialog: dialog() } }); r.fallback(); };
  const ra = async (r) => {
    if (r.request().method() !== "POST") return r.fallback();
    const b = JSON.parse(r.request().postData());
    bodies.push(b);
    await sleep(700);
    checked[b.index] = !checked[b.index]; n++;
    r.fulfill({ json: { ok: true, keys: ["space"] } });
  };
  await page.route(/\/dialog$/, rd);
  await page.route(/\/answer$/, ra);
  try {
    await page.evaluate(() => refreshDialog());
    await page.waitForFunction(() => document.querySelectorAll(".ans-opts .opt-box").length === 3, null, { timeout: 5000 });
    const t = await tapThen(`const b = document.querySelectorAll('.ans-opts button.opt'); b[0].click(); b[1].click();`,
      `[...document.querySelectorAll('.ans-opts .opt-box')].map((e) => e.textContent).join("")`);
    inFrame(t, "toggle taps");
    if (t[0] !== "☑☑☐") throw new Error("boxes not ticked at once: " + t[0]);
    await sleep(900);
    await page.evaluate(() => refreshDialog()); // mid-queue: the server has ticked A only
    await sleep(100);
    if ((await page.evaluate(`[...document.querySelectorAll('.ans-opts .opt-box')].map((e) => e.textContent).join("")`)) !== "☑☑☐") throw new Error("a refresh flipped a box back");
    await page.waitForFunction(() => !answer.inflight, null, { timeout: 8000 });
    if (bodies.length !== 2 || bodies[0].index !== 0 || bodies[1].index !== 1) throw new Error("sent " + JSON.stringify(bodies));
    if (bodies[0].fingerprint !== "fp0" || bodies[1].fingerprint !== "fp1") throw new Error("the second tap did not quote the dialog the first one produced: " + JSON.stringify(bodies));
    await page.evaluate(() => refreshDialog());
    await sleep(300);
    const end = await page.evaluate(`[...document.querySelectorAll('.ans-opts .opt-box')].map((e) => e.textContent).join("")`);
    if (end !== "☑☑☐") throw new Error("final " + end);
  } finally { await page.unroute(/\/dialog$/, rd); await page.unroute(/\/answer$/, ra); }
});

await check("latency: a sent prompt leaves the box and shows as sending in a frame, survives a transcript poll, and comes back if refused", async () => {
  await openAgent("alpha");
  await page.waitForFunction(() => document.querySelector("#chat")?.innerText.includes("alpha says hi"), null, { timeout: 5000 });
  const off = await hold(/\/prompt$/, "POST", SLOW);
  try {
    await page.locator("#prompt-text").click(); await page.keyboard.type("lat one");
    const t = await tapThen(`document.querySelector('#btn-send').click()`, `[document.querySelector('#prompt-text').innerText, document.querySelector('#chat .msg.pending .msgtime')?.textContent, document.querySelector('#btn-send').disabled].join("|")`);
    inFrame(t, "send");
    const [box, mt, dis] = t[0].split("|");
    if (box !== "") throw new Error("the box still holds the text: " + box);
    if (mt !== "sending…") throw new Error("no sending bubble: " + mt);
    if (dis === "true") throw new Error("the send button went dead");
    await sleep(300);
    await page.evaluate(() => loadChat(false));
    await sleep(300);
    if (!(await page.locator("#chat .msg.pending").count())) throw new Error("the transcript poll removed the pending bubble");
    // Another message while the first is out: shown at once too.
    await page.locator("#prompt-text").click(); await page.keyboard.type("lat two");
    const t2 = await tapThen(`document.querySelector('#btn-send').click()`, `document.querySelectorAll('#chat .msg.pending').length + "|" + document.querySelector('#prompt-text').innerText`);
    inFrame(t2, "second send");
    if (t2[0] !== "2|") throw new Error("second message not shown at once: " + t2[0]);
    await sleep(SLOW * 2 + 500);
  } finally { await off(); }
  await page.waitForFunction(() => document.querySelector("#chat")?.innerText.includes("reply to: lat two"), null, { timeout: 12000 });
  // Refused: the text is back in the box, the bubble is gone, the error is shown.
  const bad = await hold(/\/prompt$/, "POST", 500, { fail: "agent unreachable" });
  try {
    await page.locator("#prompt-text").click(); await page.keyboard.type("lat three");
    const t = await tapThen(`document.querySelector('#btn-send').click()`, `document.querySelector('#prompt-text').innerText`);
    inFrame(t, "send");
    if (t[0] !== "") throw new Error("not cleared at once");
    await page.waitForFunction(() => document.querySelector("#prompt-text").innerText.trim() === "lat three", null, { timeout: 4000 });
    if (await page.locator("#chat .msg.pending").count()) throw new Error("the bubble stays after a failure");
    const [tc, tx] = await toastNow();
    if (!/error/.test(tc) || !/agent unreachable/.test(tx)) throw new Error("toast " + tc + " " + tx);
  } finally { await bad(); }
  await page.evaluate(() => setPromptValue(""));
});

await check("latency: To do answer, done and drop leave the open list in a frame, a refresh in flight keeps them gone, a refusal brings them back", async () => {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent");
  const ids = await page.evaluate(async () => {
    const post = (b) => fetch("/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ no_wait: true, ...b }) }).then((r) => r.json());
    return [(await post({ question: "lat done", kind: "decision" })).todo.id, (await post({ question: "lat answer", kind: "decision" })).todo.id, (await post({ question: "lat drop", kind: "decision" })).todo.id];
  });
  await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
  await page.waitForSelector("#todo:not([hidden]) .td-item");
  await page.evaluate(() => window.confirm = () => true);
  const q = (id) => `#todo .td-item[data-id="${id}"]`;
  const open = async (id) => { if (!(await page.locator(q(id) + ".open").count())) await page.locator(q(id) + " > .td-row").click(); await page.waitForSelector(q(id) + ".open"); };
  const gone = (id) => `!document.querySelector('${q(id)}:not(.answered)')`;
  const count = () => page.locator("#todo .td-count").innerText();
  const c0 = await count();
  // Done, held.
  const off = await hold(/\/api\/todos\/[^/]+\/(done|answer|drop)$/, "POST", SLOW);
  try {
    await open(ids[0]);
    const t = await tapThen(`document.querySelector('${q(ids[0])} .td-done').click()`, `${gone(ids[0])}`);
    inFrame(t, "Done");
    if (t[0] !== true) throw new Error("the item is still in the open list");
    if ((await count()) === c0) throw new Error("the count did not move: " + c0);
    await sleep(300);
    await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); }); // the server has not heard of it yet
    await sleep(300);
    if (!(await page.evaluate(`${gone(ids[0])}`))) throw new Error("a refresh brought the done item back");
    // Answer, held: into the answered group, with the pending marker.
    await open(ids[1]);
    await page.locator(q(ids[1]) + " .td-text").fill("lat yes");
    const a = await tapThen(`document.querySelector('${q(ids[1])} .td-answer-btn').click()`, `${gone(ids[1])} + "|" + document.querySelector('#todo .td-gn')?.textContent`);
    inFrame(a, "Answer");
    if (!a[0].startsWith("true|") || !/[1-9]/.test(a[0].split("|")[1] || "")) throw new Error("not in the answered group: " + a[0]);
    await sleep(SLOW * 2 + 500);
  } finally { await off(); }
  const st = await page.evaluate((i) => fetch("/api/todos/" + i).then((r) => r.json()).then((x) => x.todo.state), ids[0]);
  if (st === "open") throw new Error("the held done never reached the server");
  await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
  if (!(await page.evaluate(`${gone(ids[0])}`))) throw new Error("done item reappeared after the server confirmed");
  // Refused: back, open, with the typed answer.
  await open(ids[2]);
  await page.locator(q(ids[2]) + " .td-text").fill("lat typed");
  const bad = await hold(/\/api\/todos\/[^/]+\/(done|answer|drop)$/, "POST", 400, { fail: "ledger is busy" });
  try {
    const t = await tapThen(`document.querySelector('${q(ids[2])} .td-answer-btn').click()`, `${gone(ids[2])}`);
    inFrame(t, "Answer");
    if (t[0] !== true) throw new Error("not moved at once");
    await page.waitForSelector(q(ids[2]) + ".open", { timeout: 4000 });
    if ((await page.locator(q(ids[2]) + " .td-text").inputValue()) !== "lat typed") throw new Error("the typed answer was lost on rollback");
    const [tc, tx] = await toastNow();
    if (!/error/.test(tc) || !/ledger is busy/.test(tx)) throw new Error("toast " + tc + " " + tx);
    const t2 = await tapThen(`document.querySelector('${q(ids[2])} .td-drop').click()`, `${gone(ids[2])}`);
    inFrame(t2, "Drop");
    if (t2[0] !== true) throw new Error("drop not at once");
    await page.waitForSelector(q(ids[2]) + ".open", { timeout: 4000 });
  } finally { await bad(); }
  await page.evaluate((ids) => Promise.all(ids.map((i) => fetch("/api/todos/" + i + "/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }))), ids);
});

await check("latency: unmark and the coordinator role show in a frame, stay through a refresh, and roll back on a refusal", async () => {
  await openAgent("alpha");
  await jpost("/api/marks", { agent_id: "uitest/w1:p1", reason: "lat" });
  await page.evaluate(() => { ledger.loadedAt = 0; return loadLedger(); });
  await page.waitForSelector('#d-markbar [data-mb="unmark"]', { timeout: 5000 });
  const bad = await hold(/\/api\/marks\/clear$/, "POST", 500, { fail: "no ledger" });
  try {
    const t = await tapThen(`document.querySelector('#d-markbar [data-mb="unmark"]').click()`, `document.querySelector('#d-markbar').hidden`);
    inFrame(t, "Unmark");
    if (t[0] !== true) throw new Error("the bar is still there");
    await page.waitForSelector('#d-markbar [data-mb="unmark"]', { timeout: 4000 }); // refused: back
    const [tc, tx] = await toastNow();
    if (!/error/.test(tc) || !/no ledger/.test(tx)) throw new Error("toast " + tc + " " + tx);
  } finally { await bad(); }
  const slow = await hold(/\/api\/marks\/clear$/, "POST", SLOW);
  try {
    const t = await tapThen(`document.querySelector('#d-markbar [data-mb="unmark"]').click()`, `document.querySelector('#d-markbar').hidden`);
    inFrame(t, "Unmark");
    await sleep(300);
    await page.evaluate(() => { ledger.loadedAt = 0; return loadLedger(); }); // still marked on the server
    await sleep(300);
    if (!(await page.evaluate(() => document.querySelector("#d-markbar").hidden))) throw new Error("a refresh brought the mark back");
    await sleep(SLOW);
  } finally { await slow(); }
  // The role.
  await page.locator("#d-more").click();
  const roleSlow = await hold(/\/api\/roles$/, "POST", SLOW);
  try {
    const t = await tapThen(`document.querySelector('#d-role').click()`, `isCoordinator(agentById(state.selected)) + "|" + document.querySelector('#d-menu').hidden`);
    inFrame(t, "role");
    if (t[0] !== "true|true") throw new Error("role not applied at once: " + t[0]);
    await sleep(300);
    await page.evaluate(() => { ledger.loadedAt = 0; return loadLedger(); });
    await sleep(300);
    if (!(await page.evaluate(() => isCoordinator(agentById(state.selected))))) throw new Error("a refresh undid the role");
    await sleep(SLOW);
  } finally { await roleSlow(); }
  await jpost("/api/roles", { agent_id: "uitest/w1:p1", role: "" });
  await page.evaluate(() => { ledgerOpt.m.clear(); ledger.loadedAt = 0; return loadLedger(); });
});

// ---- Fresh start: the coordinator's in-place context reset ----
// The real hub answers (alpha is a Claude agent with a session file that has no
// usage lines and no hook), so the refusal path is the real one; the success
// path is mocked, since the fake herdr has no Claude to clear.
const freshCtx = (over = {}) => ({ threshold: 150000, last: { at: "2026-11-08T09:00:00Z", trigger: "briefing", tokens: 158000 }, coordinators: [{ id: "uitest/w1:p1", name: "alpha", context: { tokens: 163000, hook_active: true, ...over } }] });
async function openFresh() {
  await page.locator("#d-more").click();
  await page.locator("#d-fresh").click();
  await page.waitForSelector("#fs", { state: "visible" });
}
for (const [width, height] of [[390, 844], [1440, 900]]) {
  await check(`fresh start: the sheet in a coordinator's menu reads, fits and works at ${width}px`, async () => {
    await page.setViewportSize({ width, height });
    await page.goto(base + "/", { waitUntil: "load" });
    await jpost("/api/roles", { agent_id: "uitest/w1:p1", role: "coordinator" });
    await openAgent("alpha");
    await page.evaluate(() => { ledger.loadedAt = 0; return loadLedger(); });
    await page.waitForFunction(() => isCoordinator(agentById(state.selected)), null, { timeout: 8000 });
    // Not offered to an agent that is no coordinator (checked below, after the role is dropped).
    await page.locator("#d-more").click();
    const it = await page.locator("#d-fresh").boundingBox();
    if (!it || it.height < 40) throw new Error("menu item box " + JSON.stringify(it));
    await page.keyboard.press("Escape");
    // The real hub: no usage in the fixture's session, so it cannot read the context.
    await openFresh();
    await page.waitForFunction(() => /Could not read its context/.test(document.querySelector("#fs-ctx")?.textContent || ""), null, { timeout: 8000 });
    await page.locator("#fs-go").click();
    await page.waitForFunction(() => (document.querySelector("#fs-status")?.textContent || "").length > 5 && !/^Clearing/.test(document.querySelector("#fs-status").textContent), null, { timeout: 8000 });
    const refused = await page.locator("#fs-status").innerText();
    if (!/usage|session|hook/i.test(refused)) throw new Error("the refusal is not shown: " + refused);
    if (await page.locator("#fs-go").isDisabled()) throw new Error("the button stayed disabled after a refusal");
    if (!(await page.evaluate(() => !document.querySelector("#fs").hidden))) throw new Error("the sheet closed on a refusal");
    await page.locator("#fs-close").click();
    await page.waitForFunction(() => document.querySelector("#fs").hidden, null, { timeout: 3000 });

    // Mocked: a large context, the hook there; the reset succeeds.
    let posts = [];
    await page.route(/\/api\/coordinator(\?.*)?$/, (r) => r.fulfill({ json: freshCtx() }));
    await page.route(/\/api\/coordinator\/reset$/, async (r) => { posts.push(JSON.parse(r.request().postData() || "{}")); await sleep(600); r.fulfill({ json: { tokens: 163000, session_after: "s2", digest_landed: true, record: { agent_id: "uitest/w1:p1", name: "alpha" } } }); });
    try {
      await openFresh();
      await page.waitForFunction(() => /163k tokens/.test(document.querySelector("#fs-ctx")?.textContent || ""), null, { timeout: 5000 });
      const ctx = await page.locator("#fs-ctx").innerText();
      if (!/150k tokens/.test(ctx) || !/Last fresh start/.test(ctx)) throw new Error("context note: " + ctx);
      await noOverflow();
      const box = await page.locator("#fs-go").boundingBox();
      if (box.height < 40 || box.x < 0 || box.x + box.width > width) throw new Error("button box " + JSON.stringify(box));
      await page.screenshot({ path: `${out}/fresh-start-sheet-${width}.png` });
      await page.locator("#fs-go").click();
      await page.waitForFunction(() => document.querySelector("#fs").classList.contains("busy"), null, { timeout: 2000 });
      await page.screenshot({ path: `${out}/fresh-start-busy-${width}.png` });
      await page.waitForFunction(() => document.querySelector("#fs").hidden, null, { timeout: 5000 });
      const [tc, tx] = await toastNow();
      if (/error/.test(tc) || !/Fresh start done \(was 163k tokens\)/.test(tx)) throw new Error("toast " + tc + " " + tx);
      if (posts.length !== 1 || posts[0].agent_id !== "uitest/w1:p1") throw new Error("posts " + JSON.stringify(posts));
      if (nativeDialogs) throw new Error("a native dialog opened");
    } finally { await page.unroute(/\/api\/coordinator\/reset$/); await page.unroute(/\/api\/coordinator(\?.*)?$/); }

    // Mocked: the session has no hook: the button is off and says what to do.
    await page.route(/\/api\/coordinator(\?.*)?$/, (r) => r.fulfill({ json: freshCtx({ hook_active: false }) }));
    try {
      await openFresh();
      await page.waitForFunction(() => /install-hook/.test(document.querySelector("#fs-ctx")?.textContent || ""), null, { timeout: 5000 });
      if (!(await page.locator("#fs-go").isDisabled())) throw new Error("the button is on without the hook");
      await page.screenshot({ path: `${out}/fresh-start-nohook-${width}.png` });
      await noOverflow();
      await page.locator("#fs-close").click();
      await page.waitForFunction(() => document.querySelector("#fs").hidden, null, { timeout: 3000 });
    } finally { await page.unroute(/\/api\/coordinator(\?.*)?$/); }

    // Back (the browser's) closes the sheet; and a non-coordinator is not offered it.
    await openFresh();
    await page.goBack();
    await page.waitForFunction(() => document.querySelector("#fs").hidden, null, { timeout: 3000 });
    await jpost("/api/roles", { agent_id: "uitest/w1:p1", role: "" });
    await page.evaluate(() => { ledgerOpt.m.clear(); ledger.loadedAt = 0; return loadLedger(); });
    await page.waitForFunction(() => !isCoordinator(agentById(state.selected)), null, { timeout: 8000 });
    await page.locator("#d-more").click();
    if (await page.locator("#d-fresh").isVisible()) throw new Error("Fresh start offered for a non-coordinator");
    await page.keyboard.press("Escape");
  });
}
await page.setViewportSize({ width: 390, height: 844 });

await check("latency: cancelling a queued prompt removes it in a frame; a refresh in flight does not bring it back; a refusal does", async () => {
  await openAgent("alpha");
  let alive = true;
  const rq = async (r) => {
    const m = r.request().method();
    if (m === "GET") return r.fulfill({ json: { queue: alive ? [{ id: "q1", text: "wait for me", from: "you" }] : [] } });
    r.fallback();
  };
  await page.route(/\/api\/queue(\?.*)?$/, rq);
  const rd = async (r) => { if (r.request().method() !== "DELETE") return r.fallback(); await sleep(SLOW); alive = false; r.fulfill({ json: { ok: true } }); };
  await page.route(/\/api\/queue\/q1$/, rd);
  try {
    await page.evaluate(() => refreshQueued(true));
    await page.waitForSelector("#queued .q-cancel", { timeout: 5000 });
    const t = await tapThen(`document.querySelector('#queued .q-cancel').click()`, `document.querySelector('#queued').hidden`);
    inFrame(t, "Cancel");
    if (t[0] !== true) throw new Error("the queued prompt is still listed");
    await sleep(300);
    await page.evaluate(() => refreshQueued(true)); // the server still lists it
    await sleep(300);
    if (!(await page.evaluate(() => document.querySelector("#queued").hidden))) throw new Error("a refresh brought the cancelled prompt back");
    await sleep(SLOW);
  } finally { await page.unroute(/\/api\/queue\/q1$/, rd); }
  alive = true;
  const bad = async (r) => { if (r.request().method() !== "DELETE") return r.fallback(); await sleep(300); r.fulfill({ status: 500, json: { message: "queue busy" } }); };
  await page.route(/\/api\/queue\/q1$/, bad);
  try {
    await page.evaluate(() => { queued.gone.clear(); return refreshQueued(true); });
    await page.waitForSelector("#queued .q-cancel", { timeout: 5000 });
    const t = await tapThen(`document.querySelector('#queued .q-cancel').click()`, `document.querySelector('#queued').hidden`);
    inFrame(t, "Cancel");
    await page.waitForSelector("#queued .q-cancel", { timeout: 4000 });
    const [tc, tx] = await toastNow();
    if (!/error/.test(tc) || !/queue busy/.test(tx)) throw new Error("toast " + tc + " " + tx);
  } finally { await page.unroute(/\/api\/queue\/q1$/, bad); await page.unroute(/\/api\/queue(\?.*)?$/, rq); }
});

// ---- Pi's "Trust project folder?" prompt ----
// herdr reports the agent idle while it is up; the app must read the screen,
// show it as blocked and offer the five options as buttons.
await check("pi trust prompt: an idle-reported Pi agent shows as blocked with its options as buttons, and an answer clears it", async () => {
  await page.goto(base + "/", { waitUntil: "load" });
  const row = page.locator('#agents .agent[data-id$="w9:p1"]');
  await row.waitFor({ state: "visible" });
  await page.waitForFunction(() => /blocked/i.test(document.querySelector('#agents .agent[data-id$="w9:p1"]')?.innerText || ""), null, { timeout: 8000 });
  await row.click();
  await page.waitForSelector("#detail", { state: "visible" });
  const card = page.locator(".ans-opts");
  await card.waitFor({ state: "visible", timeout: 8000 });
  if ((await page.locator(".ans-title").innerText()).trim() !== "Trust project folder?") throw new Error("title " + (await page.locator(".ans-title").innerText()));
  const labels = await card.locator("button.opt").allInnerTexts();
  if (labels.length !== 5 || labels[0].trim() !== "Trust" || labels[4].trim() !== "Do not trust (this session only)") throw new Error("options " + JSON.stringify(labels));
  if (!(await card.locator("button.opt").first().getAttribute("aria-current"))) throw new Error("the highlighted row is not marked");
  if (!(await page.locator("button.opt.cancel").isVisible())) throw new Error("no cancel button");
  await card.locator("button.opt").nth(4).click();
  await page.waitForFunction(() => !document.querySelector(".ans-opts"), null, { timeout: 8000 });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForFunction(() => /idle/i.test(document.querySelector('#agents .agent[data-id$="w9:p1"]')?.innerText || ""), null, { timeout: 40000 });
});

// ---- version badge: a peer that runs another version than the hub ----
await check("host chips: a peer on another version shows a badge, a matching or unknown one does not", async () => {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent");
  const r = await page.evaluate(() => {
    const local = state.hosts.find((h) => h.local);
    state.hosts = [
      { ...local, version: "v1.2.0" },
      { name: "behind", status: "ok", agents: 0, version: "v1.1.0", version_mismatch: true },
      { name: "same", status: "ok", agents: 0, version: "v1.2.0" },
      { name: "old", status: "ok", agents: 0 },
    ];
    renderHostFilter();
    const nav = document.getElementById("host-filter");
    const badges = [...nav.querySelectorAll(".ver")].map((e) => ({ text: e.textContent, host: e.closest(".hf-chip").dataset.host, title: e.title }));
    return { badges, hidden: nav.hidden, overflowX: document.documentElement.scrollWidth > document.documentElement.clientWidth };
  });
  if (r.hidden) throw new Error("host filter hidden");
  if (r.badges.length !== 1 || r.badges[0].host !== "behind" || r.badges[0].text !== "v1.1.0") throw new Error("badges " + JSON.stringify(r.badges));
  if (!/runs v1\.1\.0; this hub runs v1\.2\.0/.test(r.badges[0].title)) throw new Error("title " + r.badges[0].title);
  if (r.overflowX) throw new Error("the page scrolls sideways");
});

// ---- the to-do panel: read the context, answer, finish, drop ----
await check("to-do: items show with their context; answer, done and drop work from the app", async () => {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent");
  const ids = await page.evaluate(async () => {
    const post = (b) => fetch("/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(b) }).then((r) => r.json());
    const a = await post({ question: "may the worker delete <i>the old key</i>?", kind: "decision", context: "## Why\nunused\n\n<script>window.__xss=1</script>", links: [{ label: "PR", url: "https://example.com/pr/1" }] });
    const b = await post({ question: "run aws login", kind: "action" });
    const c = await post({ question: "close the old ticket", kind: "followup" });
    return [a.todo.id, b.todo.id, c.todo.id];
  });
  await page.evaluate(() => loadTodos());
  await page.waitForSelector("#todo:not([hidden]) .td-item");
  if ((await page.locator("#todo .td-item").count()) < 3) throw new Error("items missing");
  if ((await page.locator("#todo .td-count").innerText()).indexOf("3 open") < 0) throw new Error("count " + (await page.locator("#todo .td-count").innerText()));
  await page.evaluate(() => window.confirm = () => true); // a native dialog would block; the check below is about the buttons
  const q = (id) => `#todo .td-item[data-id="${id}"]`;
  // Opens an item unless it is open already (focus moves on to the next one by itself).
  const openIt = async (id) => { if (!(await page.locator(q(id) + ".open").count())) await page.locator(q(id) + " > .td-row").click(); await page.waitForSelector(q(id) + ".open"); };
  await openIt(ids[0]);
  if (!(await page.locator(q(ids[0]) + " .td-ctx h2").count())) throw new Error("context not rendered as markdown");
  if (await page.evaluate(() => window.__xss)) throw new Error("script in the context ran");
  if ((await page.locator(q(ids[0]) + " .td-q").innerText()).indexOf("<i>") < 0) throw new Error("question was not escaped");
  const href = await page.locator(q(ids[0]) + " .td-links a").getAttribute("href");
  if (href !== "https://example.com/pr/1") throw new Error("link " + href);
  await noOverflow();
  // Answer.
  await page.locator(q(ids[0]) + " .td-text").fill("yes, delete it");
  await page.locator(q(ids[0]) + " .td-answer-btn").click();
  await page.waitForFunction((s) => !document.querySelector(s), q(ids[0]), { timeout: 5000 }); // into the folded Answered group
  const ans = await page.evaluate((id) => fetch("/api/todos/" + id).then((r) => r.json()), ids[0]);
  if (ans.todo.state !== "answered" || ans.todo.answer.text !== "yes, delete it" || ans.todo.answer.via !== "app") throw new Error("answer " + JSON.stringify(ans));
  // Done, a single tap.
  await openIt(ids[1]);
  await page.locator(q(ids[1]) + " .td-done").click();
  await page.waitForFunction((s) => !document.querySelector(s), q(ids[1]), { timeout: 5000 });
  // Drop asks first: a refusal keeps the item.
  await openIt(ids[2]);
  await page.evaluate(() => window.confirm = () => false);
  await page.locator(q(ids[2]) + " .td-drop").click();
  await page.waitForTimeout(300);
  if (!(await page.locator(q(ids[2])).count())) throw new Error("dropped without the user confirming");
  await page.evaluate(() => window.confirm = () => true);
  await page.locator(q(ids[2]) + " .td-drop").click();
  await page.waitForFunction((s) => !document.querySelector(s), q(ids[2]), { timeout: 5000 });
  await page.screenshot({ path: `${out}/todo.png` });
});


// ---- the to-do accordion: one item open, keys, focus after an answer, drafts across refreshes ----
const postTodos = (list) => page.evaluate(async (list) => {
  const out = [];
  for (const b of list) {
    // The server allows 20 adds a minute: when a run is over that, wait the window out.
    for (let tries = 0; ; tries++) {
      const r = await fetch("/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ no_wait: true, ...b }) });
      if (r.status === 429 && tries < 20) { await new Promise((ok) => setTimeout(ok, 4000)); continue; }
      out.push((await r.json()).todo.id);
      break;
    }
  }
  await loadTodos();
  return out;
}, list);
const dropTodos = (ids) => page.evaluate(async (ids) => {
  for (const i of ids) await fetch("/api/todos/" + i + "/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
  await loadTodos();
}, ids);
const openIds = (list) => page.evaluate((l) => [...document.querySelectorAll(l + " .td-item.open")].map((e) => e.dataset.id), list);
const focusedItem = () => page.evaluate(() => { const a = document.activeElement; return { cls: a.className, id: a.closest(".td-item")?.dataset.id || null }; });

const todoMdCheck = async (root, setup) => {
  await setup();
  const ids = await postTodos([
    { question: "md `code` and **bold** <script>window.__tq=1</script> [x](https://example.com)", kind: "decision", context: "line one\nline two\nline three\n\n```\n" + "long-command-".repeat(30) + "\n```" },
  ]);
  const ans = await page.evaluate(async (id) => (await fetch("/api/todos/" + id + "/answer", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ text: "use **bubblewrap**\nthen `make clean` <img src=x onerror=\"window.__ta=1\"> [l](https://example.com/a)" }) })).status, ids[0]);
  if (ans >= 300) throw new Error("answer status " + ans);
  await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
  const item = `${root} .td-item[data-id="${ids[0]}"]`;
  // answered items fold into their group
  if (!(await page.locator(item).count())) await page.locator(`${root} .td-g`).first().click();
  await page.waitForSelector(item);
  const row = page.locator(item + " > .td-row");
  const q = item + " .td-q";
  // collapsed: markdown renders, still one line, no link, no script
  if ((await page.locator(item + ".open").count())) await row.click();
  const c = await page.evaluate((s) => { const e = document.querySelector(s); return { code: !!e.querySelector("code"), bold: !!e.querySelector("strong"), a: !!e.querySelector("a"), script: !!e.querySelector("script"), text: e.textContent, h: e.getBoundingClientRect().height, ell: getComputedStyle(e).textOverflow, ws: getComputedStyle(e).whiteSpace }; }, q);
  if (!c.code || !c.bold || c.a || c.script || c.text.indexOf("`") >= 0 || c.text.indexOf("**") >= 0 || (root === "#pt-body" ? c.ws !== "normal" || c.h > 40 : c.ell !== "ellipsis" || c.ws !== "nowrap" || c.h > 24)) throw new Error("collapsed title " + JSON.stringify(c));
  await row.click();
  await page.waitForSelector(item + ".open");
  const o = await page.evaluate((s) => { const t = document.querySelector(s + " .td-q"), a = document.querySelector(s + " .td-answer-t"), x = document.querySelector(s + " .td-ctx"); return {
    tcode: !!t.querySelector("code"), tbold: !!t.querySelector("strong"), tscript: !!t.querySelector("script"), tlit: t.textContent.indexOf("<script>") >= 0,
    abold: !!a.querySelector("strong"), acode: !!a.querySelector("code"), abr: a.querySelectorAll("br").length, alink: a.querySelector("a")?.getAttribute("href"), aimg: !!a.querySelector("img"), alit: a.textContent.indexOf("<img") >= 0,
    ctxbr: x.querySelectorAll("br").length, prew: getComputedStyle(x.querySelector("pre")).whiteSpace, prow: x.querySelector("pre").scrollWidth <= x.querySelector("pre").clientWidth + 1 }; }, item);
  if (!o.tcode || !o.tbold || o.tscript || !o.tlit) throw new Error("open title " + JSON.stringify(o));
  if (!o.abold || !o.acode || o.abr < 1 || o.alink !== "https://example.com/a" || o.aimg || !o.alit) throw new Error("answer " + JSON.stringify(o));
  if (o.ctxbr < 2) throw new Error("single newlines lost their line breaks in the context: " + JSON.stringify(o));
  if (o.prew !== "pre-wrap" || !o.prow) throw new Error("code block in the context scrolls sideways: " + JSON.stringify(o));
  if (await page.evaluate(() => window.__tq || window.__ta)) throw new Error("script ran");
  await noOverflow();
  await page.screenshot({ path: `${out}/todo-md-${root.replace(/\W/g, "")}.png` });
  await dropTodos(ids);
};
await check("to-do markdown (desktop): title and answer render inline markdown, sanitized; context keeps single newlines; no sideways overflow", async () => {
  await todoMdCheck("#pt-body", () => desktopList(1280, "#/todo"));
});
await check("to-do markdown (phone): title and answer render inline markdown, sanitized; context keeps single newlines; no sideways overflow", async () => {
  await todoMdCheck("#todo", async () => { await page.setViewportSize({ width: 390, height: 844 }); await page.goto(base + "/", { waitUntil: "load" }); await page.waitForSelector("#agents .agent"); });
});

await check("to-do accordion (desktop): one item open at a time; keys move, answer and close; focus moves on; answered items fold; the badge counts open items", async () => {
  await desktopList(1280, "#/todo");
  const ids = await postTodos([{ question: "accordion one: keep the old key?", kind: "decision", context: "Some *context*" }, { question: "accordion two: run the migration", kind: "action" }, { question: "accordion three: check the backup", kind: "followup" }]);
  const row = (id) => `#pt-body .td-item[data-id="${id}"] > .td-row`;
  await page.waitForSelector(row(ids[2]));
  await page.locator(row(ids[0])).click();
  if ((await openIds("#pt-body")).join() !== ids[0]) throw new Error("open after a click: " + await openIds("#pt-body"));
  await page.locator(row(ids[1])).click();
  if ((await openIds("#pt-body")).join() !== ids[1]) throw new Error("two open: " + await openIds("#pt-body"));
  if (await page.locator(row(ids[0])).getAttribute("aria-expanded") !== "false" || await page.locator(row(ids[1])).getAttribute("aria-expanded") !== "true") throw new Error("aria-expanded");
  const order = await page.evaluate(() => [...document.querySelectorAll("#pt-body .td-row")].map((r) => r.closest(".td-item").dataset.id));
  const at = order.indexOf(ids[1]);
  // ↓ and k move and open; Enter goes to the box; Esc leaves it, then closes the item.
  await page.keyboard.press("ArrowDown");
  if ((await openIds("#pt-body")).join() !== order[at + 1] || (await focusedItem()).id !== order[at + 1]) throw new Error("ArrowDown: " + JSON.stringify(await focusedItem()));
  await page.keyboard.press("k");
  if ((await openIds("#pt-body")).join() !== ids[1]) throw new Error("k did not move back");
  await page.keyboard.press("Enter");
  if (!/td-text/.test((await focusedItem()).cls)) throw new Error("Enter did not go to the box: " + JSON.stringify(await focusedItem()));
  await page.keyboard.type("jk typed");
  if ((await openIds("#pt-body")).join() !== ids[1]) throw new Error("j/k in the box moved the list");
  await page.keyboard.press("Escape");
  const f = await focusedItem();
  if (!/td-row/.test(f.cls) || f.id !== ids[1] || (await openIds("#pt-body")).join() !== ids[1]) throw new Error("first Esc: " + JSON.stringify(f));
  await page.keyboard.press("Escape");
  if ((await openIds("#pt-body")).length) throw new Error("second Esc did not close the item");
  if (!(await page.locator(row(ids[1]) + " .td-draft").isVisible())) throw new Error("no draft mark on the closed row");
  await page.keyboard.press("Enter");
  if ((await page.locator(`#pt-body .td-item[data-id="${ids[1]}"] .td-text`).inputValue()) !== "jk typed") throw new Error("draft lost on closing");
  // Ctrl+Enter sends; the next open item opens, its row focused; the answered one folds into the group.
  await page.keyboard.press("Enter");
  await page.keyboard.press("Control+Enter");
  const next = order[(at + 1) % order.length];
  await page.waitForFunction((n) => document.activeElement?.closest(".td-item")?.dataset.id === n && document.querySelector("#pt-body .td-item.open")?.dataset.id === n, next, { timeout: 5000 });
  const st = await page.evaluate((i) => fetch("/api/todos/" + i).then((r) => r.json()), ids[1]);
  if (st.todo.state !== "answered" || st.todo.answer.text !== "jk typed") throw new Error("answer " + JSON.stringify(st.todo));
  if (await page.locator(row(ids[1])).count()) throw new Error("the answered item is still in the open list");
  if (await page.locator("#pt-body .td-g").getAttribute("aria-expanded") !== "false") throw new Error("the Answered group is not folded");
  const counts = await page.evaluate(() => ({ open: todos.open, badge: document.querySelector('#nav .nv[data-page="todo"] .badge')?.textContent, pill: document.querySelector("#pager-todo").textContent }));
  if (counts.badge !== String(counts.open) || counts.pill !== String(counts.open)) throw new Error("badge counts more than open items: " + JSON.stringify(counts));
  await page.screenshot({ path: `${out}/todo-accordion-1280.png` });
  await page.locator("#pt-body .td-g").click();
  await page.waitForSelector(row(ids[1]));
  await page.locator("#pt-body .td-g").click();
  await page.waitForFunction((s) => !document.querySelector(s), row(ids[1]));
  await noOverflow();
  await dropTodos(ids);
});

const CHOICE_CTX = "Ways:\n\n1. **Rotate now.** wait\n2. **Rotate later.** no wait\n3. **Both.** why not\n\nMy recommendation: ok 1+3.";
// What the server holds for a to-do (its state and answer text), not the page's overlay.
const serverTodo = (id) => page.evaluate(async (id) => { const d = await fetch("/api/todos").then((r) => r.json()); const t = d.todos.find((x) => x.id === id); return t ? { state: t.state, text: t.answer && t.answer.text } : null; }, id);
// One to-do per call (the server rate-limits adds to 20 a minute); two when the queue's next line is under test.
const openChoices = async (q, two = false) => {
  await desktopList(1280, "#/todo");
  await page.evaluate(() => { CHOICE_UNDO_MS = 1200; });
  const ids = await postTodos([{ question: q, kind: "decision", context: CHOICE_CTX }, ...(two ? [{ question: q + " (other)", kind: "action" }] : [])]);
  const it = `#pt-body .td-item[data-id="${ids[0]}"]`;
  await page.locator(it + " > .td-row").click();
  await page.waitForSelector(it + ".open .td-opt");
  return { ids, it, box: page.locator(it + " .td-text") };
};

await check("to-do choices (desktop): a numbered list in the context becomes choice buttons; the recommendation is marked; Shift-click builds a pending combination in the answer box without sending; typed text is kept (the choice goes in front); the queue shows what is next", async () => {
  const { ids, it, box } = await openChoices("choices one: which way?", true);
  if ((await page.locator(it + " .td-opt[data-n]").count()) !== 3) throw new Error("expected 3 choices");
  if (!(await page.locator(it + " .td-opt.rec[data-n='1']").count()) || !(await page.locator(it + " .td-opt.rec[data-n='3']").count()) || (await page.locator(it + " .td-opt.rec[data-n='2']").count())) throw new Error("recommendation marks wrong");
  if (!/Send the recommendation: ok 1\+3/.test(await page.locator(it + " .td-rec").innerText())) throw new Error("recommendation button text");
  await page.locator(it + " .td-opt[data-n='2']").click({ modifiers: ["Shift"] });
  if ((await box.inputValue()) !== "ok 2") throw new Error("shift pick: " + await box.inputValue());
  await page.locator(it + " .td-opt[data-n='1']").click({ modifiers: ["Shift"] });
  if ((await box.inputValue()) !== "ok 1+2" || (await page.locator(it + " .td-opt.on").count()) !== 2) throw new Error("two picks: " + await box.inputValue());
  await page.locator(it + " .td-opt[data-n='2']").click({ modifiers: ["Shift"] });
  if ((await box.inputValue()) !== "ok 1") throw new Error("unpick: " + await box.inputValue());
  await page.waitForTimeout(1600);
  if ((await serverTodo(ids[0])).state !== "open") throw new Error("a shift-click sent the answer");
  // Typed text that did not come from a button is not replaced and not sent.
  await box.fill("because the old key leaks");
  await page.locator(it + " .td-opt[data-n='3']").click();
  if ((await box.inputValue()) !== "ok 3 because the old key leaks") throw new Error("front: " + await box.inputValue());
  if (!/to the front of your answer/.test(await page.locator("#toast").innerText())) throw new Error("no toast saying so: " + await page.locator("#toast").innerText());
  await page.waitForTimeout(1600);
  if ((await serverTodo(ids[0])).state !== "open") throw new Error("typed text was sent by a choice click");
  await box.fill("");
  if (!(await page.locator("#pt-body .td-nx").count())) throw new Error("no Next up mark");
  if (!/Next:/.test(await page.locator(it + " .td-after").innerText())) throw new Error("no next line");
  await dropTodos(ids);
});

await check("to-do choices: a click sends at once (the item is answered on screen, a toast offers Undo) and the request goes out when the window ends", async () => {
  const { ids, it } = await openChoices("choices send: which way?");
  await page.locator(it + " .td-opt[data-n='2']").click();
  await page.waitForFunction((i) => todos.items.find((x) => x.id === i)?.state === "answered" && !document.querySelector(`#pt-body .td-item.open[data-id="${i}"]`), ids[0], { timeout: 1000 });
  const toast = await page.locator("#toast").innerText();
  if (!/Sent: ok 2/.test(toast) || !/Undo/.test(toast)) throw new Error("toast: " + toast);
  if ((await serverTodo(ids[0])).state !== "open") throw new Error("the request went out before the window ended");
  await page.waitForFunction(async (i) => { const d = await fetch("/api/todos").then((r) => r.json()); const t = d.todos.find((x) => x.id === i); return t && t.state !== "open" && t.answer.text === "ok 2"; }, ids[0], { timeout: 6000 });
  await dropTodos(ids);
});

await check("to-do choices: Undo inside the window puts the item back, with its draft, and nothing is ever sent", async () => {
  const { ids, it, box } = await openChoices("choices undo: which way?");
  await page.locator(it + " .td-opt[data-n='1']").click({ modifiers: ["Shift"] }); // a pending pick is the draft
  await page.locator(it + " .td-opt[data-n='3']").click();                          // adds 3 and sends "ok 1+3"
  await page.waitForFunction((i) => todos.items.find((x) => x.id === i)?.state === "answered" && !document.querySelector(`#pt-body .td-item.open[data-id="${i}"]`), ids[0], { timeout: 1000 });
  if (!/Sent: ok 1\+3/.test(await page.locator("#toast").innerText())) throw new Error("toast: " + await page.locator("#toast").innerText());
  await page.locator("#toast .toast-act").click();
  await page.waitForFunction((i) => { const e = document.querySelector(`#pt-body .td-item[data-id="${i}"]`); return todos.items.find((x) => x.id === i)?.state === "open" && e && !e.classList.contains("answered"); }, ids[0], { timeout: 2000 });
  await page.locator(it + " > .td-row").evaluate((r) => { if (!r.closest(".td-item").classList.contains("open")) r.click(); });
  if ((await page.locator(it + " .td-text").inputValue()) !== "ok 1") throw new Error("draft after undo: " + await page.locator(it + " .td-text").inputValue());
  await page.waitForTimeout(2000);
  if ((await serverTodo(ids[0])).state !== "open") throw new Error("an undone choice was sent");
  await dropTodos(ids);
});

await check("to-do choices: the recommendation, a digit key and Shift+digit then Answer all send; leaving the page inside the window still sends", async () => {
  // The answer requests are captured (and answered 200) so one to-do serves all three: the server
  // keeps it open and a reset puts it back, which also keeps under the server's 20 adds a minute.
  const sent = [];
  const cap = async (route) => { sent.push(JSON.parse(route.request().postData() || "{}").text); await route.fulfill({ json: {} }); };
  const c = await openChoices("choices keys: which way?");
  await page.route(/\/api\/todos\/[^/]+\/answer$/, cap);
  const reset = async () => {
    await page.evaluate(async (id) => { todos.pend.drop(id); todos.openId = id; todos.loadedAt = 0; await loadTodos(); }, c.ids[0]);
    await page.waitForSelector(c.it + ".open .td-opt");
  };
  const waitSent = (n) => page.waitForFunction(() => true).then(async () => { for (let k = 0; k < 80 && sent.length < n; k++) await page.waitForTimeout(100); if (sent.length < n) throw new Error(`only ${sent.length} sent: ` + JSON.stringify(sent)); });
  // The recommendation button sends ok 1+3.
  await page.locator(c.it + " .td-rec").click();
  await waitSent(1);
  await reset();
  // A digit key sends that choice.
  await page.locator(c.it + " > .td-row").focus();
  await page.keyboard.press("2");
  await waitSent(2);
  await reset();
  // Shift+digit builds a combination, shown in the box (the keys stay with the list); Answer sends it.
  await page.locator(c.it + " > .td-row").focus();
  await page.keyboard.press("Shift+Digit1");
  await page.keyboard.press("Shift+Digit2");
  if ((await c.box.inputValue()) !== "ok 1+2") throw new Error("combo in the box: " + await c.box.inputValue());
  await page.locator(c.it + " .td-answer-btn").click();
  await waitSent(3);
  if (JSON.stringify(sent) !== JSON.stringify(["ok 1+3", "ok 2", "ok 1+2"])) throw new Error("sent: " + JSON.stringify(sent));
  await page.unroute(/\/api\/todos\/[^/]+\/answer$/, cap);
  await dropTodos(c.ids);
  // Reloading inside the window sends it, for real.
  const d = await openChoices("choices leave: which way?");
  await page.evaluate(() => { CHOICE_UNDO_MS = 30000; });
  await page.locator(d.it + " .td-opt[data-n='2']").click();
  await page.waitForFunction((i) => todos.items.find((x) => x.id === i)?.state === "answered", d.ids[0], { timeout: 1000 });
  await page.reload({ waitUntil: "load" });
  await page.waitForFunction(async (i) => { const r = await fetch("/api/todos").then((x) => x.json()); const t = r.todos.find((x) => x.id === i); return t && t.state !== "open" && t.answer.text === "ok 2"; }, d.ids[0], { timeout: 6000 });
  await dropTodos(d.ids);
});

await check("to-do accordion (phone): one item open; a draft survives a poll and a reload; a refresh that closes the open item opens the next and keeps the draft; a refresh never takes focus", async () => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.evaluate(() => { try { localStorage.clear(); } catch {} });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent[data-id]");
  const ids = await postTodos([{ question: "phone one: which region?", kind: "decision" }, { question: "phone two: rotate the token", kind: "action" }]);
  const it = (id) => `#todo .td-item[data-id="${id}"]`;
  await page.waitForSelector(it(ids[1]));
  await page.locator(it(ids[0]) + " > .td-row").click();
  await page.locator(it(ids[1]) + " > .td-row").click();
  if ((await openIds("#todo")).join() !== ids[1]) throw new Error("open: " + await openIds("#todo"));
  await page.locator(it(ids[0]) + " > .td-row").click();
  const box = page.locator(it(ids[0]) + " .td-text");
  await box.fill("eu-west, keep it");
  await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
  if ((await box.inputValue()) !== "eu-west, keep it") throw new Error("draft lost to a poll");
  await page.reload({ waitUntil: "load" });
  await page.waitForSelector(it(ids[0]) + " .td-draft:not([hidden])", { timeout: 8000 });
  await page.locator(it(ids[0]) + " > .td-row").click();
  if ((await page.locator(it(ids[0]) + " .td-text").inputValue()) !== "eu-west, keep it") throw new Error("draft lost to a reload");
  // Closed elsewhere while open here: the next open item opens and takes the focus; the draft is kept.
  await page.locator(it(ids[0]) + " .td-text").focus();
  await page.evaluate((i) => fetch("/api/todos/" + i + "/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }), ids[0]);
  await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
  await page.waitForFunction((s) => !document.querySelector(s), it(ids[0]), { timeout: 5000 });
  const open = await openIds("#todo");
  if (open.length !== 1 || open[0] === ids[0]) throw new Error("open after the refresh: " + open);
  if ((await focusedItem()).id !== open[0]) throw new Error("focus did not move on: " + JSON.stringify(await focusedItem()));
  const kept = page.locator(`#todo [data-kept="${ids[0]}"] .td-kept-t`);
  if ((await kept.inputValue()) !== "eu-west, keep it") throw new Error("the unsent answer was not kept");
  await page.screenshot({ path: `${out}/todo-accordion-390-kept.png` });
  await page.locator(`#todo [data-kept="${ids[0]}"] .td-discard`).click();
  if (await page.locator(`#todo [data-kept="${ids[0]}"]`).count()) throw new Error("discard left the kept answer");
  if (await page.evaluate((i) => (localStorage.getItem("comebye.todo.drafts") || "").includes(i), ids[0])) throw new Error("discard left the draft in storage");
  // A refresh that does not touch the open item keeps it open and leaves the focus alone.
  await page.locator(it(ids[1]) + ".open, " + it(ids[1])).first().waitFor();
  if (!(await page.locator(it(ids[1]) + ".open").count())) await page.locator(it(ids[1]) + " > .td-row").click();
  await page.evaluate(() => document.getElementById("btn-recall").focus());
  const more = await postTodos([{ question: "phone three: a new item arrives", kind: "followup" }]);
  if ((await openIds("#todo")).join() !== ids[1]) throw new Error("a refresh changed the open item: " + await openIds("#todo"));
  if (await page.evaluate(() => document.activeElement.id) !== "btn-recall") throw new Error("a refresh took the focus");
  await noOverflow();
  await dropTodos([ids[1], ...more]);
});


/* ---------- transcripts (a synthetic index: tools/devserver/recall.go) ---------- */
const SMOKE = "#/search/devbox/claude/11111111-aaaa-4aaa-8aaa-111111111111";
const LONGS = "#/search/devbox/claude/22222222-bbbb-4bbb-8bbb-222222222222";
const noSideways = () => page.evaluate(() => {
  const el = document.querySelector("#recall");
  const over = [...el.querySelectorAll("*")].filter((x) => x.getBoundingClientRect().right > innerWidth + 1 && !x.closest(".rc-filters, .rc-subs, .rc-targs"));
  if (over.length || document.documentElement.scrollWidth > innerWidth) throw new Error("wider than the screen: " + (over[0]?.className || over[0]?.tagName));
});

await check("transcripts: before typing, the newest sessions, with filters that narrow them", async () => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.evaluate(() => localStorage.removeItem("comebye.recall.filters"));
  await page.locator("#btn-recall").click();
  await page.waitForSelector("#recall", { state: "visible" });
  await page.waitForFunction(() => document.querySelectorAll(".rc-card").length === 3, null, { timeout: 5000 });
  const titles = await page.locator(".rc-card .rc-title").allInnerTexts();
  if (!titles[0].includes("Provision") || !titles[1].includes("Long session") || !titles[2].includes("Retry")) throw new Error("not newest first: " + titles);
  const metas = await page.locator(".rc-card > .rc-meta:not(.rc-foot)").allInnerTexts();
  if (!metas[0].includes("w-spot-3") || !metas[1].includes("devbox") || !metas[1].includes("homelab")) throw new Error("no host or project: " + metas);
  if (!metas.every((m) => /\d:\d\d/.test(m))) throw new Error("no time of day: " + metas);
  if (!/3 sessions, newest first/.test(await page.locator("#rc-results").innerText())) throw new Error("no count");
  if (await page.locator(".rc-card mark").count()) throw new Error("marks with no query");
  if (await visible('#rc-filters select[data-key="role"]') || await visible('#rc-filters select[data-key="sort"]')) throw new Error("word filters with no words");
  await page.screenshot({ path: `${out}/search-before-typing.png` });
  await page.waitForFunction(() => document.querySelector('#rc-filters select[data-key="host"] option[value="devbox"]'), null, { timeout: 5000 });
  await page.locator('#rc-filters select[data-key="host"]').selectOption("devbox");
  await page.waitForFunction(() => document.querySelectorAll(".rc-card").length === 2, null, { timeout: 5000 });
  await page.locator('#rc-filters select[data-key="host"]').selectOption("");
  await page.waitForFunction(() => document.querySelectorAll(".rc-card").length === 3, null, { timeout: 5000 });
  // Opens at the start of the session: no match to jump to.
  const href = await page.locator(".rc-card").first().getAttribute("href");
  if (/[?&](seq|q)=/.test(href)) throw new Error("a recent card links to a match: " + href);
  await noSideways();
  // A query takes over, and clearing it brings the list back.
  await page.locator("#rc-q").fill("\"connection refused\"");
  await page.waitForSelector(".rc-card .rc-snip mark", { timeout: 5000 });
  if (!await visible('#rc-filters select[data-key="role"]')) throw new Error("no word filter with words");
  await page.locator("#rc-q").fill("");
  await page.waitForFunction(() => /newest first/.test(document.querySelector("#rc-results").textContent), null, { timeout: 5000 });
  if (await page.locator(".rc-card mark").count()) throw new Error("marks after clearing");
});

await check("transcripts: the header icon searches, one card per session, real marks", async () => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.locator("#btn-recall").click();
  await page.waitForSelector("#recall", { state: "visible" });
  await page.waitForFunction(() => /indexed/.test(document.querySelector("#rc-fresh").textContent), null, { timeout: 5000 });
  await page.locator("#rc-q").fill("\"connection refused\"");
  await page.waitForSelector(".rc-card .rc-snip mark", { timeout: 5000 });
  const n = await page.locator(".rc-card").count();
  if (n !== 3) throw new Error(`${n} cards`);
  const marks = await page.locator(".rc-card .rc-snip mark").count();
  if (!marks) throw new Error("no <mark> in the snippets");
  const raw = await page.locator(".rc-card").first().innerText();
  if (/[\x02\x03]/.test(raw)) throw new Error("control characters on screen");
  await page.getByText("possible secret").first().waitFor();
  await noSideways();
  await page.setViewportSize({ width: 360, height: 700 });
  await noSideways();
  await page.setViewportSize({ width: 390, height: 844 });
});

await check("transcripts: filters narrow, are remembered, and the query is not stored", async () => {
  await page.locator('#rc-filters select[data-key="role"]').selectOption("commands");
  await page.waitForFunction(() => document.querySelectorAll(".rc-card").length === 1, null, { timeout: 5000 });
  await page.reload({ waitUntil: "load" });
  await page.waitForSelector(".rc-card", { timeout: 5000 });
  if (await page.locator('#rc-filters select[data-key="role"]').inputValue() !== "commands") throw new Error("the filter was not remembered");
  const stored = await page.evaluate(() => JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage }));
  if (/connection refused/i.test(stored)) throw new Error("the query is in browser storage");
  await page.locator('#rc-filters select[data-key="role"]').selectOption("");
  await page.waitForFunction(() => document.querySelectorAll(".rc-card").length === 3, null, { timeout: 5000 });
});

await check("transcripts: a card opens the session at its match; back returns to the results", async () => {
  await page.locator(".rc-card", { hasText: "Long session" }).click();
  await page.waitForSelector("#rc-find", { timeout: 5000 });
  await page.waitForFunction(() => /of 8/.test(document.querySelector(".rc-find-n").textContent), null, { timeout: 5000 });
  const cur = page.locator(".rc-out.cur");
  await cur.waitFor();
  if (!(await cur.innerText()).toLowerCase().includes("connection refused")) throw new Error("the current match is another message");
  if (await visible(".rc-list")) throw new Error("the results pane still shows on a phone");
  await page.goBack();
  await page.waitForSelector(".rc-card", { timeout: 5000 });
  if (await visible("#rc-find")) throw new Error("still in the session");
});

await check("transcripts: arrows step through matches across pages", async () => {
  await page.goto(base + "/" + LONGS + "?q=" + encodeURIComponent("connection refused"), { waitUntil: "load" });
  await page.waitForFunction(() => /of 8/.test(document.querySelector(".rc-find-n")?.textContent || ""), null, { timeout: 5000 });
  const seen = new Set();
  for (let i = 0; i < 8; i++) {
    await page.locator("#rc-next").click();
    await page.waitForFunction((n) => document.querySelector(".rc-find-n").textContent.startsWith(n + " of"), i + 1, { timeout: 5000 });
    const step = `step ${(i + 1) * 20}:`;
    await page.waitForFunction((s) => document.querySelector(".rc-out.cur")?.textContent.startsWith(s), step, { timeout: 5000 });
    const t = await page.locator(".rc-out.cur").innerText();
    if (!/connection refused/i.test(t)) throw new Error("match " + i + " shows " + t);
    const box = await page.locator(".rc-out.cur").boundingBox();
    if (!box || box.y < 0 || box.y > 844) throw new Error("match " + i + " is off screen");
    seen.add(t);
  }
  if (seen.size !== 8) throw new Error(`${seen.size} different matches`);
  await page.locator("#rc-next").click(); // wraps
  await page.waitForFunction(() => document.querySelector(".rc-find-n").textContent.startsWith("1 of"), null, { timeout: 5000 });
  await page.locator("#rc-prev").click();
  await page.waitForFunction(() => document.querySelector(".rc-find-n").textContent.startsWith("8 of"), null, { timeout: 5000 });
});

await check("transcripts: paging by Earlier and Later messages", async () => {
  await page.goto(base + "/" + LONGS + "?seq=160", { waitUntil: "load" });
  await page.waitForSelector("#rc-earlier", { timeout: 5000 });
  const count = () => page.locator("#rc-msgs [data-seq]").count();
  const a = await count();
  await page.locator("#rc-earlier").click();
  await page.waitForFunction((n) => document.querySelectorAll("#rc-msgs [data-seq]").length > n, a, { timeout: 5000 });
  const b = await count();
  await page.locator("#rc-later").click();
  await page.waitForFunction((n) => document.querySelectorAll("#rc-msgs [data-seq]").length > n, b, { timeout: 5000 });
});

await check("transcripts: agent text cannot run; markdown still renders; output stays text", async () => {
  await page.goto(base + "/" + SMOKE, { waitUntil: "load" });
  await page.waitForSelector(".rc-tool", { timeout: 5000 });
  await page.locator(".rc-tool", { hasText: "printf" }).locator("summary").click();
  await page.waitForTimeout(200);
  const r = await page.evaluate(() => ({
    xss: window.__xss,
    imgs: document.querySelectorAll("#recall img").length,
    scripts: document.querySelectorAll("#recall script").length,
    bolds: [...document.querySelectorAll("#recall .rc-out b")].length,
    strong: document.querySelector("#recall .rc-asst strong")?.textContent,
    lib: !!(window.marked && window.marked.Marked),
    out: [...document.querySelectorAll("#recall .rc-out")].map((x) => x.textContent).join(""),
  }));
  if (r.xss !== undefined || r.imgs || r.scripts || r.bolds) throw new Error(JSON.stringify(r));
  if (r.strong !== "reproduce" || !r.lib) throw new Error("markdown did not render: " + JSON.stringify(r));
  if (!r.out.includes("<img src=x onerror=window.__xss=1>")) throw new Error("the output was not shown as text");
  await page.screenshot({ path: `${out}/transcripts-session.png` });
});

await check("transcripts: secrets are masked, with a warning, and clipped output says so", async () => {
  await page.goto(base + "/" + SMOKE, { waitUntil: "load" });
  await page.waitForSelector(".rc-secret", { timeout: 5000 });
  await page.locator(".rc-tool", { hasText: "Authorization" }).locator("summary").click();
  await page.locator(".rc-tool", { hasText: "journalctl" }).locator("summary").click();
  const text = await page.locator("#recall").innerText();
  if (/Tok3n|ghp_/.test(text)) throw new Error("a secret is on screen");
  if (!text.includes("••••••")) throw new Error("no mask mark");
  if (!/possible secrets? (was|were) masked/.test(text)) throw new Error("no warning");
  if (!/Clipped: the first and last 1,500 of [\d,]+ bytes kept/.test(text)) throw new Error("no clip notice");
  if (!text.includes("gave up: connection refused")) throw new Error("the tail of long output is missing");
  await noSideways();
});

await check("transcripts: a subagent opens as its own thread", async () => {
  await page.goto(base + "/#/search?q=" + encodeURIComponent("connection refused"), { waitUntil: "load" });
  await page.locator(".rc-card", { hasText: "Retry the upgrade" }).click();
  await page.waitForSelector(".rc-subs", { timeout: 5000 });
  const on = await page.locator(".rc-subs .rc-chip.on").innerText();
  if (!on.startsWith("Subagent")) throw new Error("opened on " + on);
  await page.locator(".rc-subs .rc-chip", { hasText: "Main thread" }).click();
  await page.waitForFunction(() => document.querySelector(".rc-subs .rc-chip.on")?.textContent === "Main thread", null, { timeout: 5000 });
  await page.waitForFunction(() => /reproduce/.test(document.querySelector("#rc-msgs")?.innerText || ""), null, { timeout: 5000 });
});

await check("transcripts: a commit's SHA finds the session", async () => {
  await page.goto(base + "/#/search?q=1a2b3c4d", { waitUntil: "load" });
  await page.waitForSelector(".rc-card", { timeout: 5000 });
  const t = await page.locator("#rc-results").innerText();
  if (!/PRODUCED/i.test(t) || !t.includes("1a2b3c4d")) throw new Error("no commit in the Produced section: " + t);
});

await check("transcripts: Produced section above the sessions, chip lines, Open at turn N, chips in the transcript", async () => {
  for (const [w, h, tag] of [[390, 844, "phone"], [1280, 800, "desktop"]]) {
    await page.setViewportSize({ width: w, height: h });
    // A Page's id finds the Page and the turn that published it.
    await page.goto(base + "/#/search?q=pg_5a0e7e1b2c", { waitUntil: "load" });
    await page.waitForSelector(".rc-prod", { timeout: 5000 });
    const sec = await page.locator("#rc-produced").innerText();
    if (!/produced/i.test(sec)) throw new Error("section " + sec);
    const card = await page.locator(".rc-prod").first().innerText();
    if (!/Page/.test(card) || !card.includes("pg_5a0e7e1b2c") || !/turn 22/.test(card) || !card.includes("Open at turn 22")) throw new Error("card " + card);
    if (await page.locator("#rc-produced ~ .rc-card").evaluateAll((l) => l.length && l[0].classList.contains("rc-prod")) !== true) throw new Error("the Produced card is not under its heading");
    await noSideways();
    await page.screenshot({ path: `${out}/search-produced-${tag}.png` });
    // A PR ref (owner/repo#N) too, and the session rows carry what they made.
    await page.locator("#rc-q").fill("acme/comebye-fixture#42");
    await page.waitForFunction(() => /turn 26/.test(document.querySelector(".rc-prod")?.textContent || ""), null, { timeout: 5000 });
    await page.locator("#rc-q").fill("smoke");
    await page.waitForSelector(".rc-card .rc-made", { timeout: 5000 });
    const chips = await page.locator(".rc-card .rc-made").first().innerText();
    for (const want of ["1 Page", "1 PR", "1 issue", "1 commit"]) if (!chips.includes(want)) throw new Error("chip line " + chips);
    if (await page.locator(".rc-prod").count()) throw new Error("a Produced card for plain words");
    await noSideways();
    await page.screenshot({ path: `${out}/search-chipline-${tag}.png` });
    // Open at turn N lands on the turn, whose chips say what it made.
    await page.locator("#rc-q").fill("pg_5a0e7e1b2c");
    await page.waitForSelector(".rc-prod", { timeout: 5000 });
    await page.locator(".rc-prod").first().click();
    await page.waitForSelector(".rc-pchip", { timeout: 5000 });
    await page.waitForFunction(() => document.querySelector("#rc-session .rc-out.cur, #rc-session .rc-args.cur"), null, { timeout: 5000 });
    const line = await page.locator(".rc-pchip").allInnerTexts();
    if (!line.some((x) => x.includes("Page pg_5a0e7e1b2c"))) throw new Error("chips " + line);
    const href = await page.locator("a.rc-pchip", { hasText: "pg_5a0e7e1b2c" }).getAttribute("href");
    if (href !== "#/pages/pg_5a0e7e1b2c") throw new Error("page chip href " + href);
    await noSideways();
    await page.screenshot({ path: `${out}/transcript-chips-${tag}.png` });
  }
  // The other turns of that session: a PR and an issue link to GitHub, a commit says where it stands.
  await page.goto(base + "/" + SMOKE + "?seq=26", { waitUntil: "load" });
  await page.waitForSelector(".rc-pchip", { timeout: 5000 });
  const all = await page.locator(".rc-pchip").allInnerTexts();
  if (!all.some((x) => x.startsWith("PR acme/comebye-fixture#42"))) throw new Error("chips " + all);
  const gh = await page.locator("a.rc-pchip", { hasText: "#42" }).getAttribute("href");
  if (gh !== "https://github.com/acme/comebye-fixture/pull/42") throw new Error("pr chip " + gh);
  // Nothing in the index for a ref: a notice, not a blank.
  await page.goto(base + "/#/search?q=pg_0000000000", { waitUntil: "load" });
  await page.waitForFunction(() => /Nothing matches/.test(document.querySelector("#rc-results")?.textContent || ""), null, { timeout: 5000 });
  await page.setViewportSize({ width: 390, height: 844 });
});

await check("transcripts: at desktop width, results and session side by side", async () => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(base + "/#/search?q=" + encodeURIComponent("connection refused"), { waitUntil: "load" });
  await page.waitForSelector(".rc-card", { timeout: 5000 });
  await page.locator(".rc-card", { hasText: "Provision" }).click();
  await page.waitForFunction(() => /Provision/.test(document.querySelector(".rc-stitle")?.textContent || ""), null, { timeout: 5000 });
  if (!(await visible(".rc-list"))) throw new Error("the results went away");
  if (!(await page.locator(".rc-card.current").count())) throw new Error("the open session is not marked");
  const a = await page.locator(".rc-list").boundingBox(), b = await page.locator("#rc-session").boundingBox();
  if (a.x + a.width > b.x + 1 || b.width < 500) throw new Error(JSON.stringify({ a, b }));
  await page.setViewportSize({ width: 390, height: 844 });
});

await check("transcripts: no index, an old index, a stale one, a deleted session", async () => {
  const withStatus = async (body, want) => {
    await page.route(/\/api\/recall\/status$/, (r) => r.fulfill({ json: body }));
    await page.goto("about:blank");
    await page.goto(base + "/#/search", { waitUntil: "load" });
    await page.waitForFunction((w) => new RegExp(w).test(document.querySelector("#rc-results").textContent), want, { timeout: 5000 });
    await page.unroute(/\/api\/recall\/status$/);
  };
  await withStatus({ state: "missing", sessions: 0, messages: 0 }, "no transcript index yet");
  await withStatus({ state: "outdated", sessions: 0, messages: 0 }, "older version");
  await withStatus({ state: "stale", built_at: new Date(Date.now() - 86400000 * 2).toISOString(), sessions: 3, messages: 9 }, "last built 2d ago");
  // Content days old, with the indexer still looking: the age shown is the look's.
  await page.route(/\/api\/recall\/status$/, (r) => r.fulfill({ json: { state: "ok", built_at: new Date(Date.now() - 86400000 * 3).toISOString(), checked_at: new Date(Date.now() - 600000).toISOString(), sessions: 3, messages: 9 } }));
  await page.goto("about:blank");
  await page.goto(base + "/#/search", { waitUntil: "load" });
  await page.waitForFunction(() => /indexed 10m ago/.test(document.querySelector("#rc-fresh").textContent), null, { timeout: 5000 });
  if (await page.locator("#rc-fresh.warn").count()) throw new Error("warns about a copy whose indexer is alive");
  await page.unroute(/\/api\/recall\/status$/);
  await page.goto(base + "/#/search/devbox/claude/gone-session", { waitUntil: "load" });
  await page.waitForFunction(() => /isn't in the index/.test(document.querySelector("#rc-session").textContent), null, { timeout: 5000 });
});

/* ---------- threads and links (R2) ---------- */
const THREAD_HASH = async () => {
  const d = await page.evaluate(() => fetch("/api/recall/search?mode=work&q=smoke").then((r) => r.json()));
  return "#/search/thread/" + d.threads.find((t) => /smoke/i.test(t.title)).id;
};

for (const [w, h, tag] of [[390, 844, "phone"], [1280, 800, "desktop"]]) {
  await check(`threads: the Work tab finds a piece of work, and its page says how it ended with sources, at ${w}px`, async () => {
    await page.setViewportSize({ width: w, height: h });
    await page.goto("about:blank");
    await page.goto(base + "/#/search", { waitUntil: "load" });
    await page.evaluate(() => localStorage.removeItem("comebye.recall.filters"));
    await page.goto("about:blank");
    await page.goto(base + "/#/search?q=smoke", { waitUntil: "load" });
    await page.waitForSelector(".rc-card", { timeout: 5000 });
    if (!(await visible("#rc-tabs"))) throw new Error("no Messages/Work tabs with words");
    if ((await page.locator('#rc-tabs [aria-selected="true"]').innerText()) !== "Messages") throw new Error("Messages is not the default");
    await page.locator('#rc-tabs [data-mode="work"]').click();
    await page.waitForSelector(".rc-thread", { timeout: 5000 });
    if (await visible('#rc-filters select[data-key="role"]')) throw new Error("the where-the-words-are filter shows for threads");
    if (!/piece(s)? of work/.test(await page.locator(".rc-count").innerText())) throw new Error("count " + await page.locator(".rc-count").innerText());
    const card = await page.locator(".rc-thread").first().innerText();
    if (!/Retry the upgrade smoke check/.test(card) || !/1 session/.test(card) || !/reported/.test(card)) throw new Error("card " + card);
    await noSideways();
    await page.screenshot({ path: `${out}/threads-work-${tag}.png` });
    // The choice is remembered.
    await page.goto("about:blank");
    await page.goto(base + "/#/search?q=smoke", { waitUntil: "load" });
    await page.waitForSelector(".rc-thread", { timeout: 5000 });
    await page.locator(".rc-thread").first().click();
    await page.waitForSelector(".rc-threadpage .rc-outcomes", { timeout: 5000 });
    const t = await page.locator("#rc-session").innerText();
    for (const want of ["Retry the upgrade smoke check", "BRIEF", "tolerate a server that is still restarting", "HOW IT ENDED", "source: ledger", "SESSION"]) {
      if (!t.toUpperCase().includes(want.toUpperCase())) throw new Error("the thread page lacks " + want + ": " + t);
    }
    if (!/source: git not consulted/.test(t)) throw new Error("a commit outcome has no source: " + t);
    await page.locator(".rc-report summary").click();
    await page.waitForFunction(() => /Three attempts with backoff/.test(document.querySelector(".rc-report")?.textContent || ""), null, { timeout: 3000 });
    await noSideways();
    await page.screenshot({ path: `${out}/threads-page-${tag}.png` });
    // From the page to the session, and from the session back to the whole piece of work.
    await page.locator("#rc-session .rc-card", { hasText: "Retry the upgrade" }).click();
    await page.waitForSelector("#rc-thread-link", { timeout: 5000 });
    await page.locator("#rc-thread-link").click();
    await page.waitForSelector(".rc-threadpage", { timeout: 5000 });
    // Back to Messages.
    if (w >= 1100) {
      await page.locator('#rc-tabs [data-mode="messages"]').click();
      await page.waitForSelector(".rc-card:not(.rc-thread)", { timeout: 5000 });
      if (await page.locator(".rc-thread").count()) throw new Error("threads still listed under Messages");
    }
    await page.setViewportSize({ width: 390, height: 844 });
  });
}

await check("threads: a handoff or an agent leads to its session; none yet says so; several lead to the thread", async () => {
  await page.setViewportSize({ width: 390, height: 844 });
  for (const q of ["ledger=h-fixture1", "agent=" + encodeURIComponent("devbox/w3:p1")]) {
    await page.goto(base + "/#/search/for?" + q, { waitUntil: "load" });
    await page.waitForFunction(() => location.hash.startsWith("#/search/devbox/claude/11111111-aaaa"), null, { timeout: 5000 });
    await page.waitForSelector(".rc-stitle", { timeout: 5000 });
  }
  // The link's own history entry was replaced: Back leaves the screen it came from.
  await page.goto(base + "/#/status", { waitUntil: "load" });
  await page.evaluate(() => { location.hash = "#/search/for?ledger=h-fixture1"; });
  await page.waitForSelector(".rc-stitle", { timeout: 5000 });
  await page.goBack();
  await page.waitForFunction(() => location.hash === "#/status", null, { timeout: 5000 });
  // No session yet.
  await page.goto(base + "/#/search/for?ledger=h-nothing-yet", { waitUntil: "load" });
  await page.waitForFunction(() => /isn't in the transcript index yet/.test(document.querySelector("#rc-session")?.textContent || ""), null, { timeout: 5000 });
  await noSideways();
  // Several sessions with a thread: the thread.
  const th = await THREAD_HASH();
  const tid = th.split("/").pop();
  await page.route(/\/api\/recall\/sessions\?ledger=h-two$/, (r) => r.fulfill({ json: { thread_id: tid, sessions: [
    { host: "devbox", kind: "claude", id: "a", title: "A" }, { host: "devbox", kind: "claude", id: "b", title: "B" }] } }));
  await page.goto(base + "/#/search/for?ledger=h-two", { waitUntil: "load" });
  await page.waitForFunction((h) => location.hash === h, th, { timeout: 5000 });
  await page.waitForSelector(".rc-threadpage", { timeout: 5000 });
  await page.unroute(/\/api\/recall\/sessions\?ledger=h-two$/);
  // A thread that is gone.
  await page.goto(base + "/#/search/thread/t-00000000", { waitUntil: "load" });
  await page.waitForFunction(() => /isn't in the index/.test(document.querySelector("#rc-session")?.textContent || ""), null, { timeout: 5000 });
});

await check("threads: a list narrowed to one handoff says so, and the chip lifts it", async () => {
  await page.goto(base + "/#/search?ledger=h-fixture1", { waitUntil: "load" });
  await page.waitForFunction(() => document.querySelectorAll(".rc-card").length === 1, null, { timeout: 5000 });
  if (!/This handoff only/.test(await page.locator("#rc-unscope").innerText())) throw new Error("no chip");
  await page.locator("#rc-unscope").click();
  await page.waitForFunction(() => document.querySelectorAll(".rc-card").length >= 3 && !document.querySelector("#rc-unscope"), null, { timeout: 5000 });
  const hash = await page.evaluate(() => location.hash);
  if (hash.includes("ledger")) throw new Error("hash " + hash);
});

await check("threads: View transcript on an agent's row menu, and the to-do link, go to its transcript", async () => {
  for (const [w, h] of [[390, 844], [1280, 800]]) {
    await page.setViewportSize({ width: w, height: h });
    await toList();
    await rowOf(PANES.alpha).locator(".row-more").click();
    await page.waitForSelector("#row-menu", { state: "visible" });
    await page.locator("#row-transcript").click();
    await page.waitForFunction(() => location.hash.startsWith("#/search/for?"), null, { timeout: 5000 });
    // This agent has no archived session: the page says so and offers the live one.
    await page.waitForFunction(() => /isn't in the transcript index yet/.test(document.querySelector("#rc-session")?.textContent || ""), null, { timeout: 5000 });
    await page.waitForSelector("#rc-open", { timeout: 5000 });
    await page.screenshot({ path: `${out}/threads-link-${w}.png` });
    await page.locator("#rc-open").click();
    await page.waitForFunction(() => location.hash.startsWith("#/agent/"), null, { timeout: 5000 });
  }
  // A to-do link that carries a handoff id leads to that handoff's transcript.
  const href = await page.evaluate(() => {
    const t = { id: "tx", created_at: new Date().toISOString(), state: "open", kind: "decision", question: "q?", links: [{ handoff: "h-fixture1" }], waiting: null };
    todos.openId = "tx";
    const d = document.createElement("div");
    d.innerHTML = todoItemHtml(t, "x");
    return d.querySelector("a.td-tr")?.getAttribute("href");
  });
  if (href !== "#/search/for?ledger=h-fixture1") throw new Error("to-do link " + href);
  await page.setViewportSize({ width: 390, height: 844 });
});

for (const [width, box, start] of [[1280, "#ps-body", "#/status"], [390, "#br-status", "#/briefings"]]) {
  await check(`status: the transcript index line says ok, stale, missing, and hides when unreadable, at ${width}px`, async () => {
    const card = () => page.evaluate((b) => (document.querySelector(b + ' [aria-label="Transcript index"]') || {}).innerText || "", box);
    const show = async (route) => {
      await page.route(/\/api\/recall\/status$/, route);
      await desktopList(width, start);
      if (width < 1100) await page.goto(base + start, { waitUntil: "load" });
      await page.waitForSelector(`${box} .st-proj`, { timeout: 8000 });
    };
    const ago = (ms) => new Date(Date.now() - ms).toISOString();
    try {
      await show((r) => r.fulfill({ json: { state: "ok", built_at: ago(3 * 86400000), checked_at: ago(240000), sessions: 12, messages: 90, hosts: ["devbox", "kube"] } }));
      await page.waitForFunction((b) => /indexed 4m ago/.test((document.querySelector(b + ' [aria-label="Transcript index"]') || {}).innerText || ""), box, { timeout: 6000 });
      let t = await card();
      if (!t.includes("12 sessions on 2 hosts") || await page.locator(`${box} [aria-label="Transcript index"].hot`).count()) throw new Error("ok: " + t);
      await page.unroute(/\/api\/recall\/status$/);
      await show((r) => r.fulfill({ json: { state: "stale", built_at: ago(5 * 3600000), sessions: 12, messages: 90 } }));
      await page.waitForFunction((b) => /timer may have stopped/.test((document.querySelector(b + ' [aria-label="Transcript index"]') || {}).innerText || ""), box, { timeout: 6000 });
      t = await card();
      if (!/last indexed 5h ago/.test(t) || !(await page.locator(`${box} [aria-label="Transcript index"].hot`).count())) throw new Error("stale: " + t);
      await page.screenshot({ path: `${out}/status-index-stale-${width}.png` });
      await page.unroute(/\/api\/recall\/status$/);
      await show((r) => r.fulfill({ json: { state: "missing", sessions: 0, messages: 0 } }));
      await page.waitForFunction((b) => /No transcript index/.test((document.querySelector(b + ' [aria-label="Transcript index"]') || {}).innerText || ""), box, { timeout: 6000 });
      await page.unroute(/\/api\/recall\/status$/);
      await show((r) => r.fulfill({ status: 500, json: {} }));
      await page.waitForTimeout(800);
      if (await card()) throw new Error("a card shows with no status");
      if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("sideways scroll");
    } finally { await page.unroute(/\/api\/recall\/status$/).catch(() => {}); }
  });
}

// ---- Search and Ready to remove as pages (#/search, #/remove) ----
await check("search page: old #/transcripts and #/session links redirect to #/search", async () => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(base + "/#/transcripts?q=" + encodeURIComponent("connection refused"), { waitUntil: "load" });
  await page.waitForSelector("#recall", { state: "visible" });
  const h1 = await page.evaluate(() => decodeURIComponent(location.hash));
  if (h1 !== "#/search?q=connection refused") throw new Error("transcripts -> " + h1);
  if ((await page.locator("#rc-q").inputValue()) !== "connection refused") throw new Error("query not carried");
  await page.goto(base + "/#/session/devbox/claude/22222222-bbbb-4bbb-8bbb-222222222222?q=retry", { waitUntil: "load" });
  await page.waitForFunction(() => location.hash.startsWith("#/search/devbox/claude/"), null, { timeout: 5000 });
  if (!(await page.evaluate(() => location.hash)).includes("q=retry")) throw new Error("q lost: " + (await page.evaluate(() => location.hash)));
  // The redirect replaces the entry: Back leaves the page rather than bouncing off it.
  await page.waitForSelector(".rc-stitle", { timeout: 5000 });
});

await check("search page: sidebar kept, query box in the header, Esc clears then does nothing", async () => {
  await desktopList(1440, "#/search?q=" + encodeURIComponent("connection refused"));
  await page.waitForSelector("#recall", { state: "visible" });
  await page.waitForSelector(".rc-card", { timeout: 5000 });
  const [side, rc, q, hd] = await Promise.all([geo("#agents"), geo("#recall"), geo("#rc-q"), geo("#recall header")]);
  if (!side.shown || rc.x < side.x + side.w - 1) throw new Error("not beside the sidebar: " + JSON.stringify([side, rc]));
  if (q.y < hd.y || q.y + q.h > hd.y + hd.h + 1) throw new Error("the query box is not in the page header: " + JSON.stringify([q, hd]));
  if (await visible("#rc-back")) throw new Error("a Back arrow on the desktop page");
  if ((await page.locator("#nav .nv.on").getAttribute("data-page")) !== "search") throw new Error("nav not marked");
  await page.locator("#rc-q").focus();
  await page.keyboard.press("Escape");
  await page.waitForFunction(() => document.querySelector("#rc-q").value === "", null, { timeout: 3000 });
  await page.waitForFunction(() => /newest first/.test(document.querySelector("#rc-results").textContent), null, { timeout: 5000 });
  if ((await page.evaluate(() => location.hash)) !== "#/search") throw new Error("hash " + (await page.evaluate(() => location.hash)));
  await page.keyboard.press("Escape");
  await page.waitForTimeout(200);
  if (!(await visible("#recall"))) throw new Error("a second Esc left the page");
  await page.locator("#rc-q").fill("connection refused");
  await page.waitForSelector(".rc-card .rc-snip mark", { timeout: 5000 });
  await page.locator(".rc-card", { hasText: "Provision" }).click();
  await page.waitForFunction(() => /Provision/.test(document.querySelector(".rc-stitle")?.textContent || ""), null, { timeout: 5000 });
  const [l, ss] = await Promise.all([geo(".rc-list"), geo("#rc-session")]);
  if (l.x + l.w > ss.x + 1) throw new Error("results and session do not sit side by side at 1440: " + JSON.stringify([l, ss]));
  await page.screenshot({ path: `${out}/search-page-1440.png` });
});

await check("search page: the results stack above the session when the pane is narrow", async () => {
  await page.setViewportSize({ width: 1100, height: 800 });
  await page.waitForTimeout(300);
  const [l, ss] = await Promise.all([geo(".rc-list"), geo("#rc-session")]);
  if (ss.y < l.y + l.h - 1 || Math.abs(ss.x - l.x) > 2) throw new Error("not stacked at 1100: " + JSON.stringify([l, ss]));
  await page.screenshot({ path: `${out}/search-page-1100.png` });
  await page.setViewportSize({ width: 1440, height: 800 });
});

await check("search page: Open agent leaves for the agent, and Back restores the same query and session", async () => {
  await desktopList(1440, "#/search?q=" + encodeURIComponent("connection refused"));
  await page.waitForSelector(".rc-card", { timeout: 5000 });
  await page.locator(".rc-card", { hasText: "Long session" }).click();
  await page.waitForSelector(".rc-stitle", { timeout: 5000 });
  if (await visible("#rc-open")) throw new Error("Open agent on a session with no live agent");
  const want = await page.evaluate(() => location.hash);
  // Make one live agent hold this session (the fixture's agents hold other sessions).
  await page.evaluate(() => {
    const a = state.agents[0];
    a.host = "devbox"; a.agent_session = { kind: "id", value: "22222222-bbbb-4bbb-8bbb-222222222222" };
    renderRecallSession();
  });
  await page.waitForSelector("#rc-open", { timeout: 3000 });
  await page.locator("#rc-open").click();
  await page.waitForSelector("#detail", { state: "visible" });
  await page.waitForSelector("#recall", { state: "hidden" });
  await page.goBack();
  await page.waitForSelector("#recall", { state: "visible" });
  await page.waitForSelector(".rc-stitle", { timeout: 5000 });
  if ((await page.evaluate(() => location.hash)) !== want) throw new Error("hash " + (await page.evaluate(() => location.hash)));
  if ((await page.locator("#rc-q").inputValue()) !== "connection refused") throw new Error("query lost: " + (await page.locator("#rc-q").inputValue()));
  if (!/Long session/.test(await page.locator(".rc-stitle").innerText())) throw new Error("another session");
  if (!(await page.locator(".rc-card.current").count())) throw new Error("the open session is not marked");
});

await check("search page: on the phone it is a full page with a Back arrow", async () => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.locator("#btn-recall").click();
  await page.waitForSelector("#recall", { state: "visible" });
  if ((await page.evaluate(() => location.hash)) !== "#/search") throw new Error("hash " + (await page.evaluate(() => location.hash)));
  const b = await geo("#rc-back");
  if (!b.shown) throw new Error("no Back arrow");
  await page.locator("#rc-q").fill("retry");
  await page.waitForSelector(".rc-card", { timeout: 5000 });
  await page.locator("#rc-back").click();
  await page.waitForSelector("#recall", { state: "hidden" });
  await noSideways();
  await page.screenshot({ path: `${out}/search-page-390.png` });
});

const RM_ITEMS = () => [
  { id: "uitest/w7:p1", name: "tidy-docs", host: "uitest", branch: "docs/tidy", repo: "webapp", safe: true, coordinator: false,
    live: { present: false }, mark: { by: "coordinator", at: new Date().toISOString() }, checks: { exists: true, primary: false, dirty: false, unmerged_commits: 0, base_used: "origin/main" }, warnings: [], notes: [] },
  { id: "uitest/w7:p2", name: "spike-sse", host: "uitest", branch: "spike/sse", repo: "webapp", safe: false, coordinator: false, discardable: false,
    live: { present: false }, mark: { by: "you", at: new Date().toISOString() }, checks: { exists: true, dirty: true, dirty_files: 1 }, warnings: ["1 uncommitted file: README.md"], notes: [] },
  { id: "uitest/w7:p3", name: "bump-go", host: "uitest", branch: "chore/go", repo: "webapp", safe: true, coordinator: false,
    live: { present: false }, mark: { by: "coordinator", at: new Date().toISOString() }, checks: { exists: true, primary: false, dirty: false, unmerged_commits: 0, base_used: "origin/main" }, warnings: [], notes: [] },
];
async function mockRemovals() {
  const posts = { confirm: [], clear: [] };
  let items = RM_ITEMS();
  await page.route(/\/api\/removals$/, (r) => r.fulfill({ json: { removals: items } }));
  await page.route(/\/api\/removals\/confirm$/, (r) => {
    const b = JSON.parse(r.request().postData() || "{}");
    posts.confirm.push(b);
    r.fulfill({ json: { results: b.ids.map((id) => ({ id, ok: true, result: { closed: "workspace", worktree_removed: items.find((x) => x.id === id).safe } })) } });
  });
  await page.route(/\/api\/marks\/clear$/, (r) => { posts.clear.push(JSON.parse(r.request().postData() || "{}")); r.fulfill({ json: {} }); });
  return posts;
}
async function unmockRemovals() {
  for (const re of [/\/api\/removals$/, /\/api\/removals\/confirm$/, /\/api\/marks\/clear$/]) await page.unroute(re);
}

await check("remove page: sidebar entry hides at zero except on the page; select-all; inline confirms; Keep", async () => {
  const dialogs0 = nativeDialogs;
  const posts = await mockRemovals();
  try {
    await desktopList(1440, "#/status");
    await page.waitForFunction(() => ledger.on === true, null, { timeout: 8000 });
    if (await page.locator('#nav .nv[data-page="removals"]').count()) throw new Error("the entry shows with no marks");
    await page.goto(base + "/#/remove", { waitUntil: "load" });
    await page.waitForSelector("#review", { state: "visible" });
    await page.waitForSelector("#rv-list .rv-item", { timeout: 5000 });
    if ((await page.locator('#nav .nv[data-page="removals"].on').count()) !== 1) throw new Error("the entry is not shown and marked on the page");
    const [side, pg] = await Promise.all([geo("#agents"), geo("#review")]);
    if (pg.x < side.x + side.w - 1) throw new Error("not beside the sidebar");
    if (await visible("#rv-close")) throw new Error("a Back arrow on the desktop page");
    if ((await page.locator("#rv-list .rv-item").count()) !== 3) throw new Error("rows");
    // select-all, then the bulk button counts the safe rows only
    await page.locator("#rv-selall").check();
    if ((await page.locator("#rv-list .rv-chk:checked").count()) !== 3) throw new Error("select-all did not select every row");
    if (!/Remove selected \(2\)/.test(await page.locator("#rv-all").innerText())) throw new Error("bulk label " + (await page.locator("#rv-all").innerText()));
    await page.locator("#rv-all").click();
    await page.waitForSelector("#rv-bulk .rv-confirm", { timeout: 3000 });
    if (!/left for Remove anyway/.test(await page.locator("#rv-bulk .rv-confirm").innerText())) throw new Error("bulk confirm does not name what it leaves");
    await page.locator('#rv-bulk [data-act="bulk-no"]').click();
    if (posts.confirm.length) throw new Error("cancel sent a removal");
    // an unsafe row: "Remove anyway…" confirms in the row and lists what stays
    const row2 = page.locator('#rv-list .rv-item[data-id="uitest/w7:p2"]');
    await row2.locator('[data-act="remove"]').click();
    const cf = row2.locator(".rv-confirm");
    await cf.waitFor({ timeout: 3000 });
    const ct = await cf.innerText();
    if (!ct.includes("README.md") || !/stays/.test(ct)) throw new Error("confirm text: " + ct);
    await cf.locator('[data-act="confirm-no"]').click();
    if (await row2.locator(".rv-confirm").count()) throw new Error("Cancel left the confirm");
    // a safe row: Remove asks, then removes
    const row1 = page.locator('#rv-list .rv-item[data-id="uitest/w7:p1"]');
    await row1.locator('[data-act="remove"]').click();
    await row1.locator(".rv-confirm").waitFor({ timeout: 3000 });
    if (posts.confirm.length) throw new Error("Remove did not wait for the confirm");
    await page.screenshot({ path: `${out}/remove-page-1440.png` });
    await row1.locator('[data-act="confirm-yes"]').click();
    await row1.locator(".rv-result.ok").waitFor({ timeout: 5000 });
    if (JSON.stringify(posts.confirm[0].ids) !== '["uitest/w7:p1"]' || posts.confirm[0].allow_working) throw new Error("body " + JSON.stringify(posts.confirm));
    // Keep un-marks
    await row2.locator('[data-act="unmark"]').click();
    await page.waitForFunction(() => !document.querySelector('#rv-list .rv-item[data-id="uitest/w7:p2"]'), null, { timeout: 3000 });
    if (posts.clear[0].agent_id !== "uitest/w7:p2") throw new Error("clear " + JSON.stringify(posts.clear));
    if (nativeDialogs !== dialogs0) throw new Error("a native dialog opened");
    // leaving the page by the sidebar: Esc did nothing, the entry goes with the page
    await page.keyboard.press("Escape");
    if (!(await visible("#review"))) throw new Error("Esc left the page");
  } finally { await unmockRemovals(); }
});

await check("remove page: the sweep is a button on the page; its result shows above the list and marking adds rows", async () => {
  await desktopList(1440, "#/remove");
  await page.waitForSelector("#review", { state: "visible" });
  await page.waitForFunction(() => ledger.on === true, null, { timeout: 8000 });
  const calls = sweepPosts.length;
  await page.locator("#rv-sweep").click();
  await page.waitForFunction(() => /fix-login/.test(document.querySelector("#rv-sweepbox")?.innerText || ""), null, { timeout: 5000 });
  if (sweepPosts.length !== calls + 1 || sweepPosts[calls].dry_run !== true) throw new Error("not a dry run");
  const [sw, list] = await Promise.all([geo("#rv-sweepbox"), geo("#rv-list")]);
  if (sw.y > list.y) throw new Error("the sweep result is not above the list");
  await page.locator('#rv-sweepbox [data-act="sw-dismiss"]').click();
  if (await visible("#rv-sweepbox")) throw new Error("Not now left the box");
});

await check("remove page: Remove all safe (N) asks once, lists the rows, removes only the safe ones, and says what it skipped", async () => {
  const dialogs0 = nativeDialogs;
  const posts = await mockRemovals();
  try {
    await desktopList(1440, "#/remove");
    await page.waitForSelector("#rv-list .rv-item", { timeout: 5000 });
    // N=2 safe (tidy-docs, bump-go), M=1 unsafe (spike-sse); nothing is selected
    const b = page.locator("#rv-all-safe");
    if (!/^Remove all safe \(2\)$/.test((await b.innerText()).trim())) throw new Error("label " + (await b.innerText()));
    if ((await page.locator("#rv-list .rv-chk:checked").count()) !== 0) throw new Error("the action needs no selection");
    if (!/1 not safe/.test(await page.locator("#rv-bulk").innerText())) throw new Error("no note about the skipped row");
    await b.click();
    const cf = page.locator("#rv-bulk .rv-confirm");
    await cf.waitFor({ timeout: 3000 });
    const t = await cf.innerText();
    if (!/2 sessions/.test(t) || !t.includes("tidy-docs") || !t.includes("bump-go") || t.includes("spike-sse") || !/1 not safe/.test(t)) throw new Error("confirm text: " + t);
    if ((await page.locator("#rv-list .rv-confirm").count()) !== 0) throw new Error("a second confirm opened in a row");
    if (posts.confirm.length) throw new Error("removed before the confirm");
    await page.screenshot({ path: `${out}/remove-all-safe-confirm.png` });
    await cf.locator('[data-act="bulk-no"]').click();
    if (posts.confirm.length) throw new Error("Cancel sent a removal");
    await b.click();
    await page.locator('#rv-bulk [data-act="bulk-yes"]').click();
    await page.waitForFunction(() => document.querySelectorAll("#rv-list .rv-result.ok").length === 2, null, { timeout: 5000 });
    if (posts.confirm.length !== 1) throw new Error("one request expected: " + JSON.stringify(posts.confirm));
    if (JSON.stringify(posts.confirm[0].ids.slice().sort()) !== '["uitest/w7:p1","uitest/w7:p3"]' || posts.confirm[0].allow_working) throw new Error("body " + JSON.stringify(posts.confirm[0]));
    if ((await page.locator('#rv-list .rv-item[data-id="uitest/w7:p2"] .rv-result').count()) !== 0) throw new Error("the unsafe row was touched");
    if (nativeDialogs !== dialogs0) throw new Error("a native dialog opened");
  } finally { await unmockRemovals(); }
});

// Work captured in Pages: its dirty files and local commits are notes, and one confirmation discards every such row.
const DV_ITEMS = () => {
  const mark = { by: "coordinator", at: new Date().toISOString(), discard: true, reason: "published as pages" };
  const delivered = (id, name, branch, pages, files, commits) => ({
    id, name, host: "uitest", branch, repo: "webapp", safe: true, coordinator: false, discardable: true, delivered_discard: true,
    live: { present: false }, mark, checks: { exists: true, primary: false, dirty: true, dirty_files: files.length, unmerged_commits: commits.length, base_used: "origin/main", branch, tip: "tip-" + name,
      detail: { status: files, commits: commits.map((c, i) => ({ sha: "a1b2c3d4e" + i, subject: c })), unpushed: commits.length } },
    warnings: [], notes: [`captured in ${pages.map((p) => "\u201c" + p.title + "\u201d").join(", ")}; discarding drops ${files.length} uncommitted files and ${commits.length} unmerged commits`],
    delivered: { pages, holds: true, files: files.length, file_list: files, commits: commits.length, commit_list: commits.map((c, i) => "a1b2c3d4e" + i + " " + c) } });
  return [
    delivered("uitest/w8:p1", "credential-bridge-review", "review/credential-bridge", [{ id: "pg_0123456789", title: "Credential bridge review" }], ["?? notes/", "?? docs/investigations/"], ["docs: investigation notes"]),
    delivered("uitest/w8:p2", "stt-research", "research/stt", [{ id: "pg_abcdef0123", title: "Speech to text options" }, { id: "pg_abcdef0124", title: "STT benchmark table" }], ["?? notes/stt.md", " M README.md"], ["docs: stt notes", "docs: benchmark numbers"]),
    { id: "uitest/w8:p3", name: "pr4941-review", host: "uitest", branch: "review/pr4941", repo: "webapp", safe: false, coordinator: false, discardable: true,
      live: { present: false }, mark, checks: { exists: true, dirty: true, dirty_files: 1, unmerged_commits: 0, tip: "tip-pr", base_used: "origin/main", detail: { status: [" M src/fix.go"], commits: [], unpushed: 0 } },
      warnings: ["the session says UNSAFE: there is code in src/", "uncommitted changes (1 files): the worktree will be kept"], notes: [],
      delivered: { pages: [{ id: "pg_0123456790", title: "PR 4941 review" }], holds: false, why: "the session answered UNSAFE after its pages were recorded", files: 0, commits: 0 } },
    { id: "uitest/w8:p4", name: "tidy-docs", host: "uitest", branch: "docs/tidy", repo: "webapp", safe: true, coordinator: false,
      live: { present: false }, mark: { by: "coordinator", at: new Date().toISOString() }, checks: { exists: true, primary: false, dirty: false, unmerged_commits: 0, base_used: "origin/main" }, warnings: [], notes: [] },
  ];
};
const reviewFits = () => page.evaluate(() => {
  const over = [...document.querySelectorAll("#review *")].filter((x) => x.getBoundingClientRect().right > innerWidth + 1 && x.offsetParent);
  if (over.length || document.documentElement.scrollWidth > innerWidth) throw new Error("wider than the screen: " + (over[0]?.className || over[0]?.tagName));
});
await check("remove page: Discard all delivered (N) asks once, lists each item with its Pages and what is dropped, sends each tip, and Remove all safe leaves them alone", async () => {
  const dialogs0 = nativeDialogs;
  const posts = [];
  const items = DV_ITEMS();
  await page.route(/\/api\/removals$/, (r) => r.fulfill({ json: { removals: items } }));
  await page.route(/\/api\/removals\/confirm$/, (r) => {
    const b = JSON.parse(r.request().postData() || "{}");
    posts.push(b);
    r.fulfill({ json: { results: b.ids.map((id) => ({ id, ok: true, result: { closed: "workspace", worktree_removed: true, discarded: !!(b.discard || {})[id], branch_deleted: "x" } })) } });
  });
  try {
    for (const w of [1440, 390]) {
      posts.length = 0;
      await page.setViewportSize({ width: w, height: w < 500 ? 844 : 900 });
      await page.goto("about:blank");
      await page.goto(base + "/#/remove", { waitUntil: "load" });
      await page.waitForSelector("#rv-list .rv-item", { timeout: 5000 });
      const row1 = page.locator('#rv-list .rv-item[data-id="uitest/w8:p1"]');
      const rt = await row1.innerText();
      if (!/Captured in/.test(rt) || !rt.includes("Credential bridge review") || !rt.includes("docs/investigations") || !rt.includes("docs: investigation notes")) throw new Error("row text: " + rt);
      if (!(await row1.locator('[data-act="discard"]').count()) || (await row1.locator('[data-act="remove"]').innerText()).trim() !== "Close session only…") throw new Error("row actions");
      if ((await row1.locator(".rv-list.warn li").count()) !== 0) throw new Error("a delivered row warns");
      if ((await page.locator('#rv-list .rv-item[data-id="uitest/w8:p3"] .rv-list.warn li').count()) !== 2) throw new Error("the UNSAFE-after row lost its warnings");
      const b = page.locator("#rv-all-deliv");
      if (!/^Discard all delivered \(2\)$/.test((await b.innerText()).trim())) throw new Error("label " + (await b.innerText()));
      if (!/^Remove all safe \(1\)$/.test((await page.locator("#rv-all-safe").innerText()).trim())) throw new Error("Remove all safe must count only tidy-docs: " + (await page.locator("#rv-all-safe").innerText()));
      await reviewFits();
      await page.screenshot({ path: `${out}/remove-delivered-${w}.png` });
      await b.click();
      const cf = page.locator("#rv-bulk .rv-dconfirm");
      await cf.waitFor({ timeout: 3000 });
      const t = await cf.innerText();
      for (const want of ["2 delivered sessions", "credential-bridge-review", "stt-research", "Credential bridge review", "Speech to text options", "STT benchmark table",
        "2 uncommitted files", "1 unmerged commit", "docs: stt notes", "cannot be undone"]) if (!t.includes(want)) throw new Error(`confirm lacks "${want}": ${t}`);
      if (t.includes("pr4941-review") || t.includes("tidy-docs")) throw new Error("confirm lists a row that is not delivered: " + t);
      if (posts.length) throw new Error("discarded before the confirm");
      for (const sel of ['#rv-bulk [data-act="bulk-yes"]', '#rv-bulk [data-act="bulk-no"]']) {
        const g = await geo(sel);
        if (!g.shown || g.x < 0 || g.x + g.w > w + 0.5 || g.y < 0 || g.y + g.h > (w < 500 ? 844 : 900) + 0.5) throw new Error(`${sel} is not on screen at ${w}: ${JSON.stringify(g)}`);
      }
      await reviewFits();
      await page.screenshot({ path: `${out}/remove-delivered-confirm-${w}.png` });
      await cf.locator('[data-act="bulk-no"]').click();
      if (posts.length || (await page.locator("#rv-bulk .rv-dconfirm").count())) throw new Error("Cancel sent a request or left the confirm");
      await b.click();
      await page.locator('#rv-bulk [data-act="bulk-yes"]').click();
      await page.waitForFunction(() => document.querySelectorAll("#rv-list .rv-result.ok").length === 2, null, { timeout: 5000 });
      if (posts.length !== 1) throw new Error("one request expected: " + JSON.stringify(posts));
      const body = posts[0];
      if (JSON.stringify(body.ids.slice().sort()) !== '["uitest/w8:p1","uitest/w8:p2"]' || body.allow_working) throw new Error("body " + JSON.stringify(body));
      if (body.discard["uitest/w8:p1"] !== "tip-credential-bridge-review" || body.discard["uitest/w8:p2"] !== "tip-stt-research" || Object.keys(body.discard).length !== 2) throw new Error("tips " + JSON.stringify(body.discard));
      if (!/Discarded the worktree/.test(await page.locator('#rv-list .rv-item[data-id="uitest/w8:p1"] .rv-result').innerText())) throw new Error("no result line");
      if ((await page.locator('#rv-list .rv-item[data-id="uitest/w8:p3"] .rv-result').count()) !== 0) throw new Error("the UNSAFE-after row was touched");
    }
    if (nativeDialogs !== dialogs0) throw new Error("a native dialog opened");
  } finally { for (const re of [/\/api\/removals$/, /\/api\/removals\/confirm$/]) await page.unroute(re); }
});

// The safety answers: how old each is, what an UNSAFE named as the hub re-checked it, and a Re-ask per item.
const ASK_ITEMS = () => {
  const hoursAgo = (h) => new Date(Date.now() - h * 3600e3).toISOString();
  const mark = (by, at, ask) => ({ by, at: hoursAgo(at), reason: "both PRs merged at 14:02", ask });
  const base = (id, name, branch, m, extra) => ({ id, name, host: "uitest", branch, repo: "webapp", safe: true, coordinator: false,
    live: { present: true, agent_status: "idle" }, mark: m, checks: { exists: true, primary: false, dirty: false, unmerged_commits: 0, base_used: "origin/main", branch }, warnings: [], notes: [], ...extra });
  const item = (type, label, state, now, text, more) => ({ type, label, state, now, text, ...more });
  return [
    // Fresh: the session re-checked and said SAFE two minutes ago.
    base("uitest/w9:p1", "release-simple", "release/simple", mark("coordinator", 0.05, { state: "safe", reason: "both PRs merged, nothing running", at: hoursAgo(0.005) }),
      { ask_review: { state: "safe", answered_at: hoursAgo(0.005) } }),
    // Stale, and everything it named has since resolved: a note, counted as safe.
    base("uitest/w9:p2", "rollout-v0-5-16", "rollout/v0.5.16", mark("coordinator", 7, { state: "unsafe", reason: "PR homelab#343; to-do t972236dfee", at: hoursAgo(6) }),
      { notes: ["the session said UNSAFE at 08:12 UTC (6 h ago), but PR jhjaggars/homelab#343 merged 5 h ago and to-do t972236dfee is done 2 h ago"],
        ask_review: { state: "unsafe", answered_at: hoursAgo(6), resolved: true, event: true, items: [
          item("pr", "PR jhjaggars/homelab#343", "resolved", "merged 5 h ago", "I am watching it until it merges", { repo: "jhjaggars/homelab", number: 343 }),
          item("todo", "to-do t972236dfee", "resolved", "is done 2 h ago", "asked the user which cluster", { id: "t972236dfee" })] } }),
    // Still UNSAFE: an open PR, a cluster nobody can check, and free text, each its own warning.
    base("uitest/w9:p3", "metrics-spike", "spike/metrics", mark("coordinator", 3, { state: "unsafe", reason: "open PR", at: hoursAgo(2.5) }),
      { safe: false, warnings: ["the session says UNSAFE: PR jhjaggars/webapp#88 is still open (waiting for review) (answered 2 h ago)", "the session says UNSAFE: a loop I run every 10 minutes (answered 2 h ago)"],
        notes: ["the session said UNSAFE about PR jhjaggars/webapp#87, but it merged 1 h ago"],
        ask_review: { state: "unsafe", answered_at: hoursAgo(2.5), items: [
          item("pr", "PR jhjaggars/webapp#88", "unresolved", "is still open", "waiting for review", { repo: "jhjaggars/webapp", number: 88 }),
          item("pr", "PR jhjaggars/webapp#87", "resolved", "merged 1 h ago", "", { repo: "jhjaggars/webapp", number: 87 }),
          item("resource", "k3d-cluster dev on uitest", "unchecked", "can't check: k3d is not installed", "", { kind: "k3d-cluster", name: "dev" }),
          item("other", "something else", "unchecked", "can't check: free text", "a loop I run every 10 minutes", {})] } }),
  ];
};
await check("remove page: the safety answer shows its age, each item's re-checked state and a Re-ask; a resolved UNSAFE is a note", async () => {
  const items = ASK_ITEMS();
  const reasks = [];
  await page.route(/\/api\/removals$/, (r) => r.fulfill({ json: { removals: items } }));
  await page.route(/\/api\/marks\/reask$/, (r) => { reasks.push(JSON.parse(r.request().postData() || "{}")); r.fulfill({ json: { asking: true } }); });
  try {
    for (const w of [1280, 390]) {
      reasks.length = 0;
      await page.setViewportSize({ width: w, height: w < 500 ? 844 : 900 });
      await page.goto("about:blank");
      await page.goto(base + "/#/remove", { waitUntil: "load" });
      await page.waitForSelector("#rv-list .rv-item", { timeout: 5000 });
      const row = (n) => page.locator(`#rv-list .rv-item[data-id="uitest/w9:p${n}"]`);
      // fresh
      const t1 = await row(1).innerText();
      if (!/The session says SAFE/.test(t1) || !/answered just now/.test(t1) || !(await row(1).locator('[data-act="reask"]').count())) throw new Error("fresh row: " + JSON.stringify(t1));
      if ((await row(1).locator(".rv-list.warn li").count()) !== 0) throw new Error("the fresh row warns");
      // stale and resolved
      const t2 = await row(2).innerText();
      for (const want of ["UNSAFE answered 6 h ago", "every item has since resolved", "merged 5 h ago", "is done 2 h ago", "I am watching it until it merges", "08:12 UTC (6 h ago), but PR"]) if (!t2.includes(want)) throw new Error(`stale row lacks "${want}": ${t2}`);
      if ((await row(2).locator(".rv-list.warn li").count()) !== 0 || (await row(2).locator(".rv-ai.resolved").count()) !== 2) throw new Error("the resolved row warns or lacks resolved items");
      if (!/Remove all safe \(2\)/.test(await page.locator("#rv-all-safe").innerText())) throw new Error("the resolved row is not counted safe: " + (await page.locator("#rv-all-safe").innerText()));
      // still unsafe
      const r3 = row(3);
      const t3 = await r3.innerText();
      for (const want of ["UNSAFE answered 2 h ago", "still holds", "PR jhjaggars/webapp#88 is still open", "waiting for review", "k3d is not installed", "a loop I run every 10 minutes"]) if (!t3.includes(want)) throw new Error(`unsafe row lacks "${want}": ${t3}`);
      if ((await r3.locator(".rv-ai").count()) !== 4 || (await r3.locator(".rv-ai.unresolved").count()) !== 1 || (await r3.locator(".rv-ai.unchecked").count()) !== 2 || (await r3.locator(".rv-ai.resolved").count()) !== 1) throw new Error("item states");
      if ((await r3.locator("button.rv-reask").count()) !== 4) throw new Error("one Re-ask per item");
      await reviewFits();
      for (const sel of ["#rv-list .rv-item[data-id=\"uitest/w9:p3\"] .rv-ai:nth-child(1) button.rv-reask", "#rv-list .rv-item[data-id=\"uitest/w9:p1\"] button.rv-reask"]) {
        const g = await geo(sel);
        if (!g.shown || g.x < 0 || g.x + g.w > w + 0.5) throw new Error(`${sel} is not on screen at ${w}: ${JSON.stringify(g)}`);
      }
      await page.screenshot({ path: `${out}/remove-asks-${w}.png`, fullPage: false });
      for (const n of [1, 2, 3]) await row(n).screenshot({ path: `${out}/remove-asks-row${n}-${w}.png` });
      await r3.locator(".rv-ai:nth-child(3) button.rv-reask").click();
      await page.waitForFunction(() => true);
      await new Promise((ok) => setTimeout(ok, 300));
      if (reasks.length !== 1 || reasks[0].agent_id !== "uitest/w9:p3" || reasks[0].item !== 3) throw new Error("reask body " + JSON.stringify(reasks));
    }
  } finally { for (const re of [/\/api\/removals$/, /\/api\/marks\/reask$/]) await page.unroute(re); }
});

// The prompt bar and the keybar keep one row at every width (a long placeholder is cut, not wrapped). run.sh runs this in both colour schemes.
await check("prompt bar: one row at 390, 1100, 1280, 1440 and 2000, for both placeholders, with the hint on one line", async () => {
  for (const w of [390, 1100, 1280, 1440, 2000]) {
    await page.setViewportSize({ width: w, height: w < 500 ? 844 : 800 });
    await page.goto("about:blank");
    await openAgent("alpha");
    await page.waitForSelector("#prompt-text", { state: "visible" });
    for (const ph of ["Queue a message for its next step…", "Send a prompt…"]) {
      await page.evaluate((ph) => { const p = document.getElementById("prompt-text"); p.textContent = ""; p.dataset.placeholder = ph; }, ph);
      const m = await page.evaluate(() => {
        const r = (s) => document.querySelector(s)?.getBoundingClientRect();
        const c = document.getElementById("prompt-text");
        const lh = parseFloat(getComputedStyle(c).lineHeight);
        const kh = document.getElementById("kb-hint");
        return { prompt: r(".prompt").height, composer: c.getBoundingClientRect().height, line: lh,
          kb: kh && kh.offsetParent ? kh.getBoundingClientRect().height : 0, kbLine: kh ? parseFloat(getComputedStyle(kh).lineHeight) || 20 : 0 };
      });
      if (m.composer > 44 || m.prompt > 54) throw new Error(`${w}: the prompt bar is more than one row with "${ph}": ${JSON.stringify(m)}`);
      if (m.kb && m.kb > m.kbLine * 1.5) throw new Error(`${w}: the keybar hint wraps: ${JSON.stringify(m)}`);
    }
    if (w === 390) await page.screenshot({ path: `${out}/prompt-bar-390.png` });
  }
});

await check("remove page: Back leaves at once even while the check on each session is still running", async () => {
  let release;
  const gate = new Promise((r) => { release = r; });
  await page.route(/\/api\/removals$/, async (r) => { await gate; r.fulfill({ json: { removals: [] } }); });
  try {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("about:blank");
    await page.goto(base + "/#/remove", { waitUntil: "load" });
    await page.waitForSelector("#review", { state: "visible" });
    await page.waitForFunction(() => review.busy === true, null, { timeout: 5000 });
    await page.locator("#rv-close").click();
    await page.waitForSelector("#review", { state: "hidden", timeout: 3000 });
    if ((await page.evaluate(() => location.hash)) !== "") throw new Error("hash " + (await page.evaluate(() => location.hash)));
  } finally { release(); await page.unroute(/\/api\/removals$/); }
});

await check("remove page: on the phone it is a full page with a Back arrow, no overflow", async () => {
  const posts = await mockRemovals();
  try {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("about:blank");
    await page.goto(base + "/#/remove", { waitUntil: "load" });
    await page.waitForSelector("#rv-list .rv-item", { timeout: 5000 });
    if (!(await geo("#rv-close")).shown) throw new Error("no Back arrow");
    await noSideways();
    await page.screenshot({ path: `${out}/remove-page-390.png` });
    await page.locator("#rv-close").click();
    await page.waitForSelector("#review", { state: "hidden" });
    if ((await page.evaluate(() => location.hash)) !== "") throw new Error("hash " + (await page.evaluate(() => location.hash)));
  } finally { await unmockRemovals(); }
});

// ---- the keyboard layer (web/keys.js): palette, shortcuts, chips, cheat sheet ----
{
  const palOpen = () => visible("#palette");
  const act = () => page.evaluate(() => { const e = document.activeElement; return e ? (e.id || e.dataset.id || e.tagName) : ""; });
  const sel = () => page.evaluate(() => state.selected);
  const chip = () => page.locator("#kb-mode").innerText();
  const items = () => page.locator("#pal-list .pal-it .pal-main").allTextContents();
  const secs = () => page.locator("#pal-list .pal-sec").allTextContents();
  async function keysDesk(start = "#/status") {
    await desktopList(1440, start);
    await page.waitForFunction(() => ledger.on === true && document.querySelectorAll("#agents .agent[data-id]").length >= 3, null, { timeout: 8000 });
  }
  await page.route(/\/api\/models/, (r) => r.fulfill({ json: { host: "uitest", catalogs: [{ kind: "claude", source: "curated", models: [{ id: "sonnet", provider: "Aliases" }, { id: "opus", provider: "Aliases" }] }] } }));
  const keyPosts = [];
  await page.route(/\/api\/agents\/[^/]+\/keys$/, (r) => { keyPosts.push(JSON.parse(r.request().postData() || "{}")); r.fulfill({ json: { ok: true } }); });

  await check("keys: Ctrl+K opens the palette with needs-you agents first, pages and actions; Esc closes and restores the focus", async () => {
    await keysDesk();
    await page.evaluate(() => rowFor(state.agents[0] && agentId(state.agents[0]))?.focus());
    const before = await act();
    if ((await chip()) !== "NAV") throw new Error("chip " + (await chip()));
    await page.keyboard.press("Control+k");
    await page.waitForSelector("#palette", { state: "visible" });
    if ((await act()) !== "pal-q") throw new Error("the field is not focused: " + (await act()));
    if ((await chip()) !== "PALETTE") throw new Error("chip " + (await chip()));
    const sc = await secs();
    if (!/^Agents: needs you first/.test(sc[0])) throw new Error("sections " + sc);
    const first = (await items())[0];
    if (!/trust-prompt/.test(first)) throw new Error("the agent that needs you is not first: " + first);
    if (!sc.includes("Go to") || !sc.includes("Fleet")) throw new Error("sections " + sc);
    const all = (await items()).join("|");
    for (const w of ["Status", "To do", "Search", "Ready to remove"]) if (!all.includes(w)) throw new Error("no page " + w + ": " + all);
    await page.screenshot({ path: `${out}/palette-1440.png` });
    await page.keyboard.press("Escape");
    await page.waitForSelector("#palette", { state: "hidden" });
    if ((await act()) !== before) throw new Error(`focus not restored: ${before} -> ${await act()}`);
    if ((await chip()) !== "NAV") throw new Error("chip after " + (await chip()));
    await page.keyboard.press("Meta+k");
    await page.waitForSelector("#palette", { state: "visible" });
    await page.keyboard.press("Control+k");
    await page.waitForSelector("#palette", { state: "hidden" });
  });

  await check("keys: the palette's prefixes @ > # narrow it; typing filters; Enter on an agent opens it", async () => {
    await page.route(/\/api\/todos$/, (r) => r.fulfill({ json: { todos: [{ id: "t1", kind: "question", question: "Which retry policy?", state: "open", context: "", agent_name: "w", created_at: new Date().toISOString() }], open: 1, answered: 0 } }));
    try { // the mock must not outlive this check, whatever happens in it: the release check posts to the real /api/todos
    await keysDesk();
    await page.waitForFunction(() => todos.items.length === 1, null, { timeout: 8000 });
    await page.keyboard.press("Control+k");
    await page.keyboard.type("@");
    if ((await page.locator("#pal-pfx").innerText()) !== "@") throw new Error("no prefix chip");
    if ((await secs()).join() !== "Agents") throw new Error("@ sections " + (await secs()));
    await page.keyboard.press("Backspace"); await page.keyboard.press("Backspace"); // the prefix goes with the text
    await page.keyboard.type(">");
    if (!(await secs()).every((x) => !/^Agents|^To do/.test(x)) || !(await secs()).length) throw new Error("> sections " + (await secs()));
    await page.keyboard.press("Backspace");
    await page.keyboard.type("#");
    if ((await secs()).join() !== "To do") throw new Error("# sections " + (await secs()));
    if (!/Which retry policy/.test((await items())[0])) throw new Error("to-do item " + (await items()));
    await page.keyboard.press("Enter");
    await page.waitForSelector("#p-todo", { state: "visible" });
    await page.waitForSelector('#p-todo .td-item.open[data-id="t1"]', { timeout: 5000 }).catch(() => { throw new Error("the to-do did not open"); });
    await page.keyboard.press("Control+k");
    await page.keyboard.type("brav");
    if (!/bravo/.test((await items())[0])) throw new Error("fuzzy: " + (await items()));
    await page.keyboard.press("Enter");
    await page.waitForSelector("#detail", { state: "visible" });
    if (!/w2:p1$/.test(await sel())) throw new Error("opened " + (await sel()));
    if (await palOpen()) throw new Error("the palette stayed open");
    } finally { await page.unroute(/\/api\/todos$/); }
  });

  await check("keys: the palette finds an agent by its workspace (repo) name, hints why, and ranks it below a direct name match", async () => {
    await keysDesk();
    // One agent whose own name matches, next to agents that only live in the "notes" workspace.
    await page.evaluate(() => { const b = state.agents.find((x) => x.name === "bravo"); state.agents.push({ ...b, name: "notes-taker", pane_id: "w99:p1", id: undefined, worktree: undefined, cwd: "/tmp/elsewhere" }); });
    await page.keyboard.press("Control+k");
    await page.keyboard.type("notes");
    const rows = await page.locator("#pal-list .pal-it").evaluateAll((els) => els.filter((e) => e.querySelector(".dot")).map((e) => ({ name: e.querySelector(".pal-main").innerText, meta: e.querySelector(".pal-meta").innerText, why: e.querySelector(".pal-meta").classList.contains("ws-hit") })));
    await page.screenshot({ path: `${out}/palette-workspace-${page.viewportSize().width}.png` });
    if (rows.length < 2 || rows[0].name !== "notes-taker" || rows[0].why) throw new Error("a direct name match is not first, unhinted: " + JSON.stringify(rows));
    const ws = rows.slice(1);
    if (!ws.every((r) => r.why && /notes/.test(r.meta))) throw new Error("workspace matches have no hint: " + JSON.stringify(ws));
    await page.keyboard.press("Escape");
    await page.evaluate(() => { state.agents = state.agents.filter((x) => x.name !== "notes-taker"); });
  });

  await check("keys: second steps (rename, model, role) and Esc goes back one step, then closes", async () => {
    await keysDesk();
    await page.locator('#agents .agent[data-id$="w1:p1"]').first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.evaluate(() => rowFor(state.selected).focus());
    const before = await act();
    await page.keyboard.press("Control+k");
    if (!(await secs()).some((x) => /^Current agent/.test(x))) throw new Error("no current-agent group: " + (await secs()));
    await page.keyboard.type("rename");
    await page.keyboard.press("Enter");
    if ((await page.locator("#pal-q").getAttribute("placeholder")) !== "Rename alpha") throw new Error("placeholder " + (await page.locator("#pal-q").getAttribute("placeholder")));
    await page.keyboard.press("Escape"); // back to the list, still open
    if (!(await palOpen())) throw new Error("Esc closed the palette from a second step");
    if ((await page.locator("#pal-q").inputValue()) !== "") throw new Error("the field was not cleared");
    await page.keyboard.type("switch model");
    await page.keyboard.press("Enter");
    await page.waitForFunction(() => /sonnet/.test(document.querySelector("#pal-list")?.innerText || ""), null, { timeout: 4000 });
    await page.keyboard.press("Escape");
    await page.keyboard.type("role");
    await page.keyboard.press("Enter");
    const roles = await items();
    if (roles.join() !== "worker,coordinator") throw new Error("roles " + roles);
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await page.waitForSelector("#palette", { state: "hidden" });
    if ((await act()) !== before) throw new Error(`focus not restored: ${before} -> ${await act()}`);
  });

  await check("keys: for an agent that is not Claude the palette says Rename is Claude-only, and r says so", async () => {
  await keysDesk();
  await page.locator('#agents .agent[data-id$="w2:p1"]').first().click(); // bravo, a codex agent
  await page.waitForSelector("#detail", { state: "visible" });
  await page.evaluate(() => rowFor(state.selected).focus());
  await page.keyboard.press("r");
  await page.waitForFunction(() => /only available for Claude/.test(document.querySelector("#toast")?.textContent || ""), null, { timeout: 3000 });
  if (await visible("#palette")) throw new Error("r opened a rename step for a non-Claude agent");
  await page.keyboard.press("Control+k");
  await page.keyboard.type("rename");
  if (!/Claude agents only/.test((await items())[0] || "")) throw new Error("palette: " + (await items()));
  await page.keyboard.press("Enter");
  await page.waitForSelector("#palette", { state: "hidden" });
});

await check("keys: stop and interrupt are in the palette only, behind a confirm", async () => {
    keyPosts.length = 0;
    const el = await page.locator("#agents .agent.selected, #agents .agent[aria-current]").first().getAttribute("data-id");
    await page.keyboard.press("Control+k");
    await page.keyboard.type("stop");
    await page.keyboard.press("Enter");
    const t = await items();
    if (t.length !== 2 || !/^Stop/.test(t[0]) || t[1] !== "Cancel") throw new Error("confirm " + t);
    if (keyPosts.length) throw new Error("sent before the confirm");
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("Enter"); // Cancel
    if (keyPosts.length || !(await palOpen())) throw new Error("Cancel sent keys or closed");
    await page.keyboard.type("interrupt");
    await page.keyboard.press("Enter");
    await page.keyboard.press("Enter"); // the confirm
    await page.waitForFunction(() => document.querySelector("#palette").hidden);
    await page.waitForTimeout(300);
    if (keyPosts.length !== 1 || keyPosts[0].keys.join() !== "esc") throw new Error("keys " + JSON.stringify(keyPosts));
    // and no single key does it
    keyPosts.length = 0;
    for (const k of ["s", "x", "Escape", "Control+c"]) await page.keyboard.press(k);
    await page.waitForTimeout(200);
    if (keyPosts.length) throw new Error("a bare key sent " + JSON.stringify(keyPosts));
  });

  await check("keys: i focuses the prompt, nothing fires while typing, Esc leaves it for the agent row", async () => {
    await keysDesk();
    await page.locator('#agents .agent[data-id$="w1:p1"]').first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.evaluate(() => rowFor(state.selected).focus());
    const id = await sel();
    await page.keyboard.press("i");
    if ((await act()) !== "prompt-text") throw new Error("not focused: " + (await act()));
    if ((await page.evaluate(() => promptValue())) !== "") throw new Error("the i was typed into the box");
    if ((await chip()) !== "TYPING") throw new Error("chip " + (await chip()));
    await page.keyboard.type("njkgtm?/i hello n");
    if ((await page.evaluate(() => promptValue())) !== "njkgtm?/i hello n") throw new Error("typed: " + (await page.evaluate(() => promptValue())));
    if ((await sel()) !== id || (await palOpen()) || (await visible("#cheats")) || (await page.evaluate(() => location.hash.startsWith("#/search")))) throw new Error("a key fired while typing");
    await page.keyboard.press("Escape");
    if ((await act()) !== id) throw new Error("focus did not go to the row: " + (await act()));
    if ((await chip()) !== "NAV") throw new Error("chip " + (await chip()));
    await page.evaluate(() => { $("prompt-text").textContent = ""; });
    // Ctrl+K works while typing
    await page.keyboard.press("i");
    await page.keyboard.press("Control+k");
    await page.waitForSelector("#palette", { state: "visible" });
    await page.keyboard.press("Escape");
    if ((await act()) !== "prompt-text") throw new Error("focus not restored to the prompt: " + (await act()));
    await page.keyboard.press("Escape");
    await page.screenshot({ path: `${out}/keys-nav-1440.png` });
  });

  await check("keys: / opens Search; j k n Shift+N Alt+digit move between agents; g chords go to pages; ? opens the cheat sheet", async () => {
    let step = "start";
    const S = (n) => { step = n; };
    try {
    await keysDesk();
    S("slash"); await page.keyboard.press("/");
    await page.waitForSelector("#recall", { state: "visible" });
    if (!(await page.evaluate(() => location.hash.startsWith("#/search")))) throw new Error("hash");
    await page.waitForFunction(() => document.activeElement && document.activeElement.id === "rc-q", null, { timeout: 3000 });
    if ((await page.locator("#rc-q").inputValue()) !== "") throw new Error("the slash was typed");
    await page.keyboard.press("Escape"); await page.keyboard.press("Escape");
    await page.locator("#agents").focus().catch(() => {});
    await page.evaluate(() => document.activeElement && document.activeElement.blur());
    const ids = await page.evaluate(() => [...document.querySelectorAll("#agents .agent[data-id]")].filter((r) => r.offsetParent).map((r) => r.dataset.id));
    S("alt2 " + JSON.stringify(ids)); await page.keyboard.press("Alt+2");
    await page.waitForFunction((x) => state.selected === x, ids[1], { timeout: 3000 });
    S("j " + (await sel())); await page.keyboard.press("j");
    await page.waitForFunction((x) => state.selected === x, ids[2], { timeout: 3000 });
    S("k " + (await sel())); await page.keyboard.press("k");
    await page.waitForFunction((x) => state.selected === x, ids[1], { timeout: 3000 });
    // Two agents that need you, whatever the earlier checks left answered (the next list update would undo it, so press at once).
    const blocked = await page.evaluate(() => {
      const rs = [...document.querySelectorAll("#agents .agent[data-id]")].filter((r) => r.offsetParent);
      for (const r of [rs[0], rs[2]]) { const a = agentById(r.dataset.id); if (a) a.agent_status = "blocked"; }
      renderList();
      return [...document.querySelectorAll("#agents .agent.blocked[data-id]")].map((r) => r.dataset.id);
    });
    if (blocked.length < 2) throw new Error("could not make two agents need you");
    await page.evaluate(() => document.activeElement && document.activeElement.blur());
    S("n " + JSON.stringify(blocked)); await page.keyboard.press("n");
    await page.waitForFunction((x) => state.selected === x, blocked[0], { timeout: 3000 });
    S("N"); await page.keyboard.press("Shift+N");
    await page.waitForFunction((x) => state.selected === x, blocked[blocked.length - 1], { timeout: 3000 });
    // g chords
    await page.keyboard.press("g"); await page.keyboard.press("t");
    await page.waitForSelector("#p-todo", { state: "visible" });
    await page.keyboard.press("g"); await page.keyboard.press("s");
    await page.waitForSelector("#p-status", { state: "visible" });
    await page.keyboard.press("g"); await page.keyboard.press("r");
    await page.waitForSelector("#review", { state: "visible" });
    await page.keyboard.press("g"); await page.keyboard.press("t");
    await page.waitForSelector("#p-todo", { state: "visible" });
    // the chord times out after about a second
    await page.keyboard.press("g");
    await page.waitForTimeout(1500);
    await page.keyboard.press("s");
    await page.waitForTimeout(250);
    if (!(await visible("#p-todo")) || (await visible("#p-status"))) throw new Error("a stale g still chorded");
    // ? opens the cheat sheet; Esc closes it and the focus comes back
    await page.evaluate(() => document.querySelector("#nav .nv")?.focus());
    const before = await act();
    await page.keyboard.press("?");
    await page.waitForSelector("#cheats", { state: "visible" });
    if ((await chip()) !== "SHEET") throw new Error("chip " + (await chip()));
    const txt = await page.locator("#cheat-box").textContent();
    for (const w of ["Command palette", "Next agent that needs you", "g", "To do page"]) if (!txt.includes(w)) throw new Error("cheat sheet lacks " + w);
    await page.screenshot({ path: `${out}/cheats-1440.png` });
    await page.keyboard.press("Escape");
    await page.waitForSelector("#cheats", { state: "hidden" });
    if ((await act()) !== before) throw new Error(`focus not restored: ${before} -> ${await act()}`);
    } catch (e) { throw new Error(step + ": " + String(e.message).split("\n")[0]); }
  });

  await check("keys: m marks the open agent; the existing To do keys still work", async () => {
    await keysDesk();
    await page.locator('#agents .agent[data-id$="w1:p1"]').first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.evaluate(() => rowFor(state.selected).focus());
    await page.keyboard.press("m");
    await page.waitForSelector("#mk", { state: "visible", timeout: 3000 });
    await page.keyboard.press("Escape");
    await page.waitForSelector("#mk", { state: "hidden" });
  });

  await check("keys: on the phone the Commands button opens the palette; no chip or hint shows", async () => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("about:blank");
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#agents .agent[data-id]", { state: "visible" });
    if (!(await visible("#btn-cmds"))) throw new Error("no Commands button");
    if (await visible("#keybar")) throw new Error("the key bar shows on the phone");
    const fl = await page.locator("#btn-fleet").boundingBox();
    if (fl.x + fl.width > 390) throw new Error("the header's last button is cut off: " + JSON.stringify(fl));
    const b = await page.locator("#btn-cmds").boundingBox();
    if (b.width < 38 || b.height < 36) throw new Error("tap target " + b.width + "x" + b.height);
    await page.locator("#btn-cmds").click();
    await page.waitForSelector("#palette", { state: "visible" });
    await noSideways();
    const pb = await page.locator("#palette .pal").boundingBox();
    if (pb.x < 0 || pb.x + pb.width > 390) throw new Error("the palette is off screen: " + JSON.stringify(pb));
    await page.screenshot({ path: `${out}/palette-390.png` });
    await page.keyboard.press("Escape");
    await page.waitForSelector("#palette", { state: "hidden" });
    if ((await act()) !== "btn-cmds") throw new Error("focus not restored to the button: " + (await act()));
  });
  await page.unroute(/\/api\/models/);
  await page.unroute(/\/api\/agents\/[^/]+\/keys$/);
}

await check("briefing: a skipped scheduled run shows \"checked HH:MM, no change\" with the briefing", async () => {
  await publishBrief("## Status\n\nchecked-note briefing");
  const checked = new Date(Date.now() + 1000).toISOString();
  await page.route(/\/api\/brief$/, async (r) => {
    const res = await r.fetch();
    const j = await res.json();
    j.schedule = { ...(j.schedule || {}), enabled: true, every: "1h", state: "ok", checked_at: checked };
    await r.fulfill({ response: res, json: j });
  });
  try {
    await page.goto("about:blank");
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForFunction(() => /checked \d{1,2}:\d{2}.*no change/.test(document.querySelector("#briefing .bf-meta")?.textContent || ""), null, { timeout: 8000 });
    const t = await page.locator("#briefing .bf-meta").innerText();
    if (!/ago · from .* · checked/.test(t)) throw new Error("meta: " + t);
  } finally {
    await page.unroute(/\/api\/brief$/);
  }
  await page.goto("about:blank");
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#briefing .bf-toggle", { state: "visible", timeout: 8000 });
  if (/no change/.test(await page.locator("#briefing").innerText())) throw new Error("note shown without the hub saying so");
});

// ---- themes (web/themes.js): contrast, picker, persistence, no flash ----
await check("themes: every theme passes the contrast check (tools/uitest/contrast.mjs)", async () => {
  const { execFileSync } = await import("node:child_process");
  try { execFileSync("node", [new URL("./contrast.mjs", import.meta.url).pathname], { encoding: "utf8", stdio: "pipe" }); }
  catch (e) { throw new Error(String(e.stdout || e.message).split("\n").filter((l) => /FAIL|<|missing|not defined/.test(l)).slice(0, 4).join(" | ")); }
});

{
  const tctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, colorScheme: "light", bypassCSP: true });
  const tp = await tctx.newPage();
  // Record every data-theme <html> ever had, and whether <body> existed yet:
  // the right theme must be there before the body (so before first paint).
  await tctx.addInitScript(() => {
    window.__themeLog = [];
    const set = Element.prototype.setAttribute;
    Element.prototype.setAttribute = function (n, v) {
      if (this === document.documentElement && n === "data-theme") window.__themeLog.push([v, document.body === null]);
      return set.call(this, n, v);
    };
  });
  const tok = (name) => tp.evaluate((n) => getComputedStyle(document.documentElement).getPropertyValue(n).trim(), name);
  const bodyBg = () => tp.evaluate(() => getComputedStyle(document.body).backgroundColor);
  const cur = () => tp.evaluate(() => document.documentElement.getAttribute("data-theme"));

  await check("themes: the picker switches the tokens instantly and lists every theme", async () => {
    await tp.goto(base + "/", { waitUntil: "load" });
    await tp.evaluate(() => localStorage.clear());
    await tp.reload({ waitUntil: "load" });
    if ((await cur()) !== "light") throw new Error("System + light OS should be the Comebye Light theme, got " + (await cur()));
    if ((await tok("--bg")) !== "#f3f4f7") throw new Error("light --bg " + (await tok("--bg")));
    const openPicker = async () => {
      if (await tp.locator("#theme-panel").isVisible()) return;
      await tp.locator("#btn-fleet").click();
      await tp.locator("#f-theme").click();
      await tp.waitForSelector("#theme-panel", { state: "visible" });
    };
    await openPicker();
    const n = await tp.locator("#tp-list .tp-opt").count();
    if (n !== 16) throw new Error("expected System + 15 themes, got " + n);
    await tp.locator('#tp-list [data-theme-id="gruvbox-dark"]').click();
    await tp.waitForSelector("#theme-panel", { state: "hidden" }); // picking closes the picker
    if ((await cur()) !== "gruvbox-dark") throw new Error("data-theme not applied");
    if ((await tok("--bg")) !== "#1d2021") throw new Error("--bg " + (await tok("--bg")));
    if ((await bodyBg()) !== "rgb(29, 32, 33)") throw new Error("body background " + (await bodyBg()));
    await openPicker();
    if ((await tp.locator('#tp-list [data-theme-id="gruvbox-dark"]').getAttribute("aria-checked")) !== "true") throw new Error("option not marked checked");
    // A swatch previews ITS theme, not the active one.
    const sw = await tp.locator('#tp-list [data-theme-id="dracula"] .tp-sw').evaluate((e) => getComputedStyle(e).backgroundColor);
    if (sw !== "rgb(33, 34, 44)") throw new Error("dracula swatch background " + sw);
    for (const id of ["dracula", "tokyonight-day", "solarized-light"]) {
      await openPicker();
      await tp.locator(`#tp-list [data-theme-id="${id}"]`).click();
      const want = await tp.evaluate((i) => ComebyeThemes.byId[i].vars.bg, id);
      if ((await tok("--bg")) !== want) throw new Error(id + " --bg " + (await tok("--bg")) + " != " + want);
    }
    await tp.keyboard.press("Escape").catch(() => {});
    await tp.locator("#btn-fleet").click();
    if (!/Solarized Light/.test(await tp.locator("#f-theme").innerText())) throw new Error("menu label: " + (await tp.locator("#f-theme").innerText()));
  });

  await check("themes: the choice persists across reload with no flash of another theme", async () => {
    await tp.evaluate(() => ComebyeThemes.set("tokyonight-storm"));
    await tp.reload({ waitUntil: "load" });
    const log = await tp.evaluate(() => window.__themeLog);
    const set = log;
    if (!set.length || set.some(([t]) => t !== "tokyonight-storm")) throw new Error("theme sequence " + JSON.stringify(log));
    if (!set[0][1]) throw new Error("data-theme was set only after <body> existed (could flash): " + JSON.stringify(log));
    if ((await tok("--bg")) !== "#1f2335") throw new Error("--bg " + (await tok("--bg")));
  });

  await check("themes: System follows the OS, with a chosen theme for each of light and dark", async () => {
    await tp.evaluate(() => { localStorage.setItem("comebye.theme", "system"); localStorage.setItem("comebye.theme.light", "catppuccin-latte"); localStorage.setItem("comebye.theme.dark", "nord"); });
    await tp.reload({ waitUntil: "load" });
    if ((await cur()) !== "catppuccin-latte") throw new Error("light OS gave " + (await cur()));
    await tp.emulateMedia({ colorScheme: "dark" });
    await tp.waitForFunction(() => document.documentElement.getAttribute("data-theme") === "nord", null, { timeout: 3000 });
    // The pickers in the panel set those two slots.
    await tp.locator("#btn-fleet").click();
    await tp.locator("#f-theme").click();
    await tp.locator("#tp-more").click(); // System's light and dark themes are a second step
    await tp.selectOption('#tp-sys select[data-sys="dark"]', "dracula");
    if ((await cur()) !== "dracula") throw new Error("dark slot change not applied: " + (await cur()));
    if ((await tp.evaluate(() => localStorage.getItem("comebye.theme.dark"))) !== "dracula") throw new Error("slot not stored");
    await tp.emulateMedia({ colorScheme: "light" });
    await tp.waitForFunction(() => document.documentElement.getAttribute("data-theme") === "catppuccin-latte", null, { timeout: 3000 });
  });

  await check("themes: an old stored value (light, dark) and a junk one still work", async () => {
    await tp.evaluate(() => { localStorage.removeItem("comebye.theme.light"); localStorage.removeItem("comebye.theme.dark"); });
    for (const [v, want] of [["dark", "dark"], ["light", "light"], ["nonsense", "light"]]) {
      await tp.evaluate((x) => localStorage.setItem("comebye.theme", x), v);
      await tp.reload({ waitUntil: "load" });
      const got = await cur();
      if (got !== want) throw new Error(`${v} -> ${got}, want ${want}`);
    }
  });
  await tctx.close();
}

// Empty states and option controls, at desktop and phone widths in a few themes.
{
  const EMPTY_LEDGER = { plans: [], entries: [], marks: [], roles: {} };
  const NO_TODOS = { todos: [], open: 0, answered: 0 };
  for (const [w, theme] of [[1280, "dark"], [1280, "light"], [2000, "nord"], [390, "dark"], [390, "light"], [390, "gruvbox-dark"]]) {
    await check(`empty states: padded and centred at ${w} in ${theme}`, async () => {
      const mobile = w <= 500;
      const ctx = await browser.newContext({ viewport: { width: w, height: mobile ? 844 : 800 }, colorScheme: theme === "light" ? "light" : "dark", bypassCSP: true, isMobile: mobile, hasTouch: mobile });
      await ctx.addInitScript((t) => { if (t !== "light" && t !== "dark") localStorage.setItem("comebye.theme", t); }, theme);
      const p = await ctx.newPage();
      try {
        await p.route(/\/api\/ledger$/, (r) => r.fulfill({ json: EMPTY_LEDGER }));
        await p.route(/\/api\/todos$/, (r) => r.fulfill({ json: NO_TODOS }));
        await p.goto(base + "/" + (mobile ? "" : "#/todo"), { waitUntil: "load" });
        // Wait for the fleet to arrive (the list drawn, the event stream open): the first "agents" event can land any time
        // after load, and one landing after the list is forced empty below redraws it with the fleet.
        await p.waitForFunction(() => document.querySelector("#agents .agent") && state.es && state.es.readyState === 1);
        await p.waitForTimeout(300);
        // Each state is forced, then its box is measured: non-zero left padding, text centred, inside its pane.
        const states = [
          ["list", "state.agents = []; renderList()", "#agents .empty", true], // measured in the same task: an "agents" event (or a resume refresh) landing in a wait would redraw the fleet
          ["search none", "location.hash = '#/search?q=zzzqqqnomatch'", ".rc-empty"],
          ["ready to remove", "openReview(); await new Promise(r => setTimeout(r, 500)); review.items = []; renderReview()", "#rv-list .ln-empty"],
          ["sweep", "openReview(); await new Promise(r => setTimeout(r, 500)); Object.assign(sweep, { shown: true, busy: false, data: { ready: [], needs_look: [] } }); renderSweep()", "#rv-sweepbox .ln-empty"],
          ...(mobile ? [] : [["To do none", "location.hash = '#/todo'; todos.loadedAt = 0; await loadTodos()", "#pt-body .empty"]]),
        ];
        const bad = [];
        const measure = (sel) => {
          const el = [...document.querySelectorAll(sel)].find((e) => e.offsetParent !== null);
          if (!el) return null;
          const cs = getComputedStyle(el), r = el.getBoundingClientRect(), pr = el.parentElement.getBoundingClientRect();
          return { padL: parseFloat(cs.paddingLeft), padR: parseFloat(cs.paddingRight), align: cs.textAlign, inside: r.left >= pr.left - 0.5 && r.right <= pr.right + 0.5 };
        };
        for (const [name, js, sel, now] of states) {
          let m;
          if (now) m = await p.evaluate(`(async () => { ${js}; return (${measure.toString()})(${JSON.stringify(sel)}); })()`);
          else {
            await p.evaluate(`(async () => { ${js} })()`);
            await p.waitForTimeout(700);
            m = await p.evaluate(measure, sel);
          }
          if (!m) bad.push(`${name}: not shown (${sel})`);
          else if (!(m.padL > 0 && m.padR > 0) || m.align !== "center" || !m.inside) bad.push(`${name}: ${JSON.stringify(m)}`);
          await p.evaluate(() => { document.getElementById("rv-close")?.click(); });
        }
        if (bad.length) throw new Error(bad.join("; "));
      } finally { await ctx.close(); }
    });
  }

  // Option-like controls: hover and pressed look different from rest, the selected look differs from hover,
  // and hover is limited to devices that can hover. Checked in two themes.
  for (const theme of ["dark", "nord"]) {
    await check(`option controls: hover and pressed styles differ from rest (${theme})`, async () => {
      const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, colorScheme: "dark", bypassCSP: true });
      await ctx.addInitScript((t) => { if (t !== "dark") localStorage.setItem("comebye.theme", t); }, theme);
      const p = await ctx.newPage();
      try {
        await p.goto(base + "/#/todo", { waitUntil: "load" });
        await p.waitForTimeout(500);
        // A To do item with choices, drawn from the app's own markup.
        await p.evaluate(() => {
          document.body.insertAdjacentHTML("beforeend", `<div id="zz" style="position:fixed;left:20px;top:20px;z-index:99999;background:var(--bg);padding:10px">
            <div class="td-opts"><button type="button" class="td-opt" id="zz-a"><kbd>1</kbd><span>Plain</span></button><button type="button" class="td-opt on" id="zz-b"><kbd>2</kbd><span>Selected</span></button></div>
            <button type="button" class="hf-chip" id="zz-c">chip</button></div>`);
        });
        const look = (sel) => p.evaluate((sel) => { const cs = getComputedStyle(document.querySelector(sel)); return [cs.backgroundColor, cs.borderTopColor, cs.transform, cs.cursor].join("|"); }, sel);
        const bad = [];
        for (const sel of ["#zz-a", "#zz-b", "#zz-c"]) {
          await p.mouse.move(700, 600); await p.waitForTimeout(250);
          const rest = await look(sel);
          await p.hover(sel); await p.waitForTimeout(250);
          const hover = await look(sel);
          await p.mouse.down(); await p.waitForTimeout(250);
          const press = await look(sel);
          await p.mouse.up();
          if (hover === rest) bad.push(`${sel}: hover looks like rest`);
          if (press === hover) bad.push(`${sel}: pressed looks like hover`);
          if (!rest.endsWith("|pointer")) bad.push(`${sel}: cursor is not pointer`);
        }
        await p.mouse.move(700, 600); await p.waitForTimeout(250);
        const selRest = await look("#zz-b");
        await p.hover("#zz-a"); await p.waitForTimeout(250);
        const plainHover = await look("#zz-a");
        if (plainHover.split("|").slice(0, 2).join() === selRest.split("|").slice(0, 2).join()) bad.push("hovered plain option looks like the selected one");
        // Only (hover: hover) devices get hover rules: none outside such a media block.
        const ok = await p.evaluate(() => {
          let outside = [];
          const walk = (rules, inHover) => { for (const r of rules) { if (r.cssRules && r.media) walk(r.cssRules, inHover || /hover:\s*hover/.test(r.media.mediaText)); else if (r.selectorText && /\.(td-opt|hf-chip|tp-opt)[^,{]*:hover/.test(r.selectorText) && !inHover) outside.push(r.selectorText); } };
          for (const sh of document.styleSheets) { try { walk(sh.cssRules, false); } catch {} }
          return outside;
        });
        if (ok.length) bad.push("hover outside (hover: hover): " + ok.join(", "));
        if (bad.length) throw new Error(bad.join("; "));
      } finally { await ctx.close(); }
    });
  }

  await check("todo choices: a recommendation is found in the forms people write", async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    const r = await page.evaluate(() => [
      todoRecommended(["I recommend ok 2"], 3),
      todoRecommended(["Recommendation: **1**"], 3),
      todoRecommended(["My recommendation: ok 1+3 here"], 3),
      todoRecommended(["recommend 1+3"], 3),
      todoRecommended(["**Recommend option 2**"], 3),
      todoRecommended(["recommend option 5"], 3),
      todoRecommended(["I recommend waiting for a bit"], 3),
      todoRecommended(["no opinion"], 3),
      todoOptions("1. **A** x\n2. **B** y\nRecommend option 2 because it is cheaper").rec,
    ]);
    const want = [[2], [1], [1, 3], [1, 3], [2], [], [], [], [2]];
    if (JSON.stringify(r) !== JSON.stringify(want)) throw new Error(JSON.stringify(r) + " != " + JSON.stringify(want));
  });

  await check("todo choices: buttons come from the options and recommend fields; the context's list is only the fallback", async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    const r = await page.evaluate(() => {
      const labels = (h) => [...new DOMParser().parseFromString(h, "text/html").querySelectorAll(".td-opt[data-n] span")].map((x) => x.textContent);
      const withFields = todoOptsHtml({ id: "tzz00000001", context: "1. **Wrong** a\n2. **Also wrong** b", options: [{ label: "Mint a key", detail: "fresh, rotates" }, { label: "Reuse the old key" }, { label: "Skip" }], recommend: [3, 1, 9] });
      const fallback = todoOptsHtml({ id: "tzz00000002", context: "1. **A** x\n2. **B** y\nRecommend option 2" });
      const one = todoOptsHtml({ id: "tzz00000003", context: "", options: [{ label: "only" }] });
      return { f: labels(withFields), fh: withFields, b: labels(fallback), bh: fallback, one };
    });
    if (JSON.stringify(r.f) !== JSON.stringify(["Mint a key", "Reuse the old key", "Skip"])) throw new Error("field labels: " + JSON.stringify(r.f));
    if (!r.fh.includes('data-fill="ok 1+3"') || !r.fh.includes("fresh, rotates")) throw new Error("recommendation or detail missing: " + r.fh);
    if (JSON.stringify(r.b) !== JSON.stringify(["A", "B"]) || !r.bh.includes('data-fill="ok 2"')) throw new Error("fallback: " + r.bh);
    if (r.one !== "") throw new Error("one option drew buttons");
  });
}

// ---- menus: one set of rules for every ⋯ popover; the theme picker; the ☀/☾ toggle; the Briefings card ----
{
  const vis = (sel) => page.locator(sel).isVisible();
  const active = () => page.evaluate(() => document.activeElement?.id || document.activeElement?.className || "");
  const resetTheme = () => page.evaluate(() => { try { localStorage.clear(); } catch {} });
  const OPEN = {
    fleet: async () => { await page.locator("#btn-fleet").click(); await page.waitForSelector("#f-menu", { state: "visible" }); },
    // The pointer is parked in a corner afterwards: a mouse resting over a row would preview that row
    // when the browser next re-hit-tests (slow CI), over whatever the keyboard previewed.
    theme: async () => { await page.locator("#btn-fleet").click(); await page.locator("#f-theme").click(); await page.waitForSelector("#theme-panel", { state: "visible" }); await page.mouse.move(page.viewportSize().width - 3, 3); await page.waitForTimeout(150); },
    notify: async () => { await page.locator("#btn-notify").click(); await page.waitForSelector("#notify-panel", { state: "visible" }); },
    row: async () => { await rowOf(PANES.alpha).locator(".row-more").click(); await page.waitForSelector("#row-menu", { state: "visible" }); },
  };
  const BOX = { fleet: "#f-menu", theme: "#theme-panel", notify: "#notify-panel", row: "#row-menu" };
  const TRIG = { fleet: "btn-fleet", theme: "btn-fleet", notify: "btn-notify", row: "row-more" };
  const openIds = () => page.evaluate(() => ["f-menu", "d-menu", "row-menu", "theme-panel", "notify-panel"].filter((i) => !document.getElementById(i).hidden));

  for (const width of [1280, 390]) {
    await check(`menus: Escape, a click outside and the × close the fleet menu, the theme picker, the notifications and a row's menu, with the focus back on the button, at ${width}px`, async () => {
      await desktopList(width);
      await page.waitForFunction(() => ledger.on === true, null, { timeout: 8000 });
      for (const name of ["fleet", "theme", "notify", "row"]) {
        await OPEN[name]();
        await page.keyboard.press("Escape");
        await page.waitForSelector(BOX[name], { state: "hidden", timeout: 2000 });
        if (!(await active()).includes(TRIG[name])) throw new Error(`${name}: Escape left the focus on "${await active()}"`);
        await OPEN[name]();
        await page.mouse.click(width - 8, 8); // empty corner (on a phone, the sheet's backdrop)
        await page.waitForSelector(BOX[name], { state: "hidden", timeout: 2000 }).catch(() => { throw new Error(`${name}: a click outside did not close it`); });
        await OPEN[name]();
        await page.locator(BOX[name] + " .pop-x").click();
        await page.waitForSelector(BOX[name], { state: "hidden", timeout: 2000 });
        if (!(await active()).includes(TRIG[name])) throw new Error(`${name}: × left the focus on "${await active()}"`);
      }
      if (nativeDialogs) throw new Error("a native dialog opened");
      await noOverflow();
    });
  }

  await check("menus: only one is open at a time", async () => {
    await desktopList(1280);
    await page.waitForFunction(() => ledger.on === true, null, { timeout: 8000 });
    await OPEN.row();
    await page.locator("#btn-notify").click();
    if ((await openIds()).join() !== "notify-panel") throw new Error("row then bell: " + await openIds());
    await page.locator("#btn-fleet").click();
    if ((await openIds()).join() !== "f-menu") throw new Error("bell then ⋯: " + await openIds());
    await page.locator("#f-theme").click();
    if ((await openIds()).join() !== "theme-panel") throw new Error("Theme item: " + await openIds());
    await page.locator("#btn-notify").click();
    if ((await openIds()).join() !== "notify-panel") throw new Error("theme then bell: " + await openIds());
    await page.locator("#btn-notify").click();
    if ((await openIds()).length) throw new Error("the bell did not toggle it shut: " + await openIds());
  });

  await check("menus: ↑ ↓ Home End move between the items (the × is not one) and wrap", async () => {
    await OPEN.fleet();
    const items = await page.evaluate(() => [...document.querySelectorAll("#f-menu button:not(.pop-x)")].filter((b) => !b.disabled && !b.hidden && b.getClientRects().length).map((b) => b.id));
    if (items.length < 3) throw new Error("few items " + items);
    if ((await active()) !== items[0]) throw new Error("first item not focused: " + await active());
    await page.keyboard.press("ArrowDown");
    if ((await active()) !== items[1]) throw new Error("ArrowDown: " + await active());
    await page.keyboard.press("End");
    if ((await active()) !== items[items.length - 1]) throw new Error("End: " + await active());
    await page.keyboard.press("ArrowDown");
    if ((await active()) !== items[0]) throw new Error("ArrowDown did not wrap: " + await active());
    await page.keyboard.press("ArrowUp");
    if ((await active()) !== items[items.length - 1]) throw new Error("ArrowUp did not wrap: " + await active());
    await page.keyboard.press("Home");
    if ((await active()) !== items[0]) throw new Error("Home: " + await active());
    await page.keyboard.press("Escape");
    // The notifications and the theme picker answer the arrows too.
    await OPEN.notify();
    const nb = await page.evaluate(() => document.activeElement.id);
    if (!/^notify-/.test(nb) && nb !== "") throw new Error("notifications focus " + nb);
    await page.keyboard.press("Escape");
    await OPEN.theme();
    await page.keyboard.press("ArrowDown");
    const a1 = await page.evaluate(() => document.activeElement.dataset.themeId || document.activeElement.id);
    if (!a1) throw new Error("theme ArrowDown went nowhere");
    await page.keyboard.press("Escape");
    // ArrowDown on the ⋯ itself opens the menu.
    await page.locator("#btn-fleet").focus();
    await page.keyboard.press("ArrowDown");
    await page.waitForSelector("#f-menu", { state: "visible" });
    await page.keyboard.press("Escape");
  });

  await check("theme: the picker is a popover under ⋯; hovering previews, leaving reverts, a click keeps it and closes it", async () => {
    await desktopList(1280);
    await resetTheme();
    await page.reload({ waitUntil: "load" });
    await page.waitForFunction(() => ledger.on === true, null, { timeout: 8000 });
    const attr = () => page.evaluate(() => document.documentElement.getAttribute("data-theme"));
    const saved = await attr();
    await OPEN.theme();
    const g = await page.evaluate(() => { const p = document.querySelector("#theme-panel").getBoundingClientRect(), b = document.querySelector("#btn-fleet").getBoundingClientRect(); return { top: p.top, bottom: p.bottom, right: p.right, btnBottom: b.bottom, btnRight: b.right, vh: innerHeight }; });
    if (g.top < g.btnBottom - 1 || g.right > g.btnRight + 24 || g.bottom > g.vh) throw new Error("not under ⋯: " + JSON.stringify(g));
    await page.locator('#tp-list [data-theme-id="dracula"]').hover();
    if ((await attr()) !== "dracula") throw new Error("hover did not preview: " + await attr());
    if (!/Previewing Dracula/.test(await page.locator("#tp-hint").innerText())) throw new Error("hint " + await page.locator("#tp-hint").innerText());
    if ((await page.evaluate(() => localStorage.getItem("comebye.theme"))) === "dracula") throw new Error("a preview was saved");
    await page.mouse.move(5, 400); // off the list
    await page.waitForFunction((t) => document.documentElement.getAttribute("data-theme") === t, saved, { timeout: 2000 }).catch(() => { throw new Error("leaving the list did not revert"); });
    await page.locator('#tp-list [data-theme-id="nord"]').hover();
    await page.locator('#tp-list [data-theme-id="nord"]').click();
    await page.waitForSelector("#theme-panel", { state: "hidden" });
    if ((await attr()) !== "nord" || (await page.evaluate(() => localStorage.getItem("comebye.theme"))) !== "nord") throw new Error("click did not keep nord: " + await attr());
    if ((await active()) !== "btn-fleet") throw new Error("focus after picking: " + await active());
  });

  await check("theme: arrowing previews, Escape reverts and closes, Enter keeps", async () => {
    const attr = () => page.evaluate(() => document.documentElement.getAttribute("data-theme"));
    await page.evaluate(() => ComebyeThemes.set("gruvbox-dark"));
    await OPEN.theme();
    if ((await page.evaluate(() => document.activeElement.dataset.themeId)) !== "gruvbox-dark") throw new Error("the saved theme is not focused: " + await page.evaluate(() => document.activeElement.dataset.themeId));
    await page.keyboard.press("ArrowDown");
    const next = await page.evaluate(() => document.activeElement.dataset.themeId);
    const want = await page.evaluate((i) => ComebyeThemes.byId[ComebyeThemes.resolve(i)].id, next);
    if ((await attr()) !== want || want === "gruvbox-dark") throw new Error("arrow did not preview " + next + ": " + await attr());
    await page.keyboard.press("Escape");
    await page.waitForSelector("#theme-panel", { state: "hidden" });
    if ((await attr()) !== "gruvbox-dark") throw new Error("Escape did not revert: " + await attr());
    if ((await page.evaluate(() => localStorage.getItem("comebye.theme"))) !== "gruvbox-dark") throw new Error("Escape changed the saved theme");
    await OPEN.theme();
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("ArrowDown");
    const pick = await page.evaluate(() => document.activeElement.dataset.themeId);
    await page.keyboard.press("Enter");
    await page.waitForSelector("#theme-panel", { state: "hidden" });
    if ((await page.evaluate(() => localStorage.getItem("comebye.theme"))) !== pick || (await attr()) !== (await page.evaluate((i) => ComebyeThemes.resolve(i), pick))) throw new Error("Enter did not keep " + pick);
    if ((await active()) !== "btn-fleet") throw new Error("focus after Enter: " + await active());
  });

  await check("theme: System's light and dark themes are a second step", async () => {
    await OPEN.theme();
    if (await vis("#tp-sys")) throw new Error("the system step shows up front");
    await page.locator("#tp-more").click();
    await page.waitForSelector("#tp-sys", { state: "visible" });
    if (await vis("#tp-list")) throw new Error("the list stays beside the step");
    await page.selectOption('#tp-sys select[data-sys="light"]', "catppuccin-latte");
    if ((await page.evaluate(() => localStorage.getItem("comebye.theme.light"))) !== "catppuccin-latte") throw new Error("slot not stored");
    await page.locator("#tp-back").click();
    await page.waitForSelector("#tp-list", { state: "visible" });
    await page.keyboard.press("Escape");
  });

  await check("theme: the ☀/☾ button flips within the family and says when there is no other half", async () => {
    const attr = () => page.evaluate(() => document.documentElement.getAttribute("data-theme"));
    const q = page.locator("#btn-theme-q");
    if (!(await q.isVisible())) throw new Error("no ☀/☾ button");
    const bb = await q.boundingBox();
    if (bb.width < 36 || bb.height < 36) throw new Error("tap target " + bb.width + "x" + bb.height);
    await page.evaluate(() => ComebyeThemes.set("gruvbox-dark"));
    await page.evaluate(() => window.dispatchEvent(new Event("storage")));
    if (!/Gruvbox Light/.test(await q.getAttribute("aria-label"))) throw new Error("label " + await q.getAttribute("aria-label"));
    await q.click();
    if ((await attr()) !== "gruvbox-light") throw new Error("☀ gave " + await attr());
    await q.click();
    if ((await attr()) !== "gruvbox-dark") throw new Error("☾ gave " + await attr());
    await page.evaluate(() => { ComebyeThemes.set("tokyonight-storm"); window.dispatchEvent(new Event("storage")); });
    await q.click();
    if ((await attr()) !== "tokyonight-day") throw new Error("storm -> " + await attr());
    await page.evaluate(() => { ComebyeThemes.set("dracula"); window.dispatchEvent(new Event("storage")); });
    await page.evaluate(() => document.querySelector("#btn-theme-q").click()); // aria-disabled: it still answers, with a toast
    if ((await attr()) !== "dracula") throw new Error("Dracula changed to " + await attr());
    await page.waitForFunction(() => /no light version/.test(document.querySelector("#toast").textContent), null, { timeout: 2000 });
    if ((await q.getAttribute("aria-disabled")) !== "true") throw new Error("not marked unavailable");
    // System with a dark OS flips to an explicit light theme of the same family.
    await page.evaluate(() => { ComebyeThemes.set("system"); window.dispatchEvent(new Event("storage")); });
    const first = await attr();
    await q.click();
    if ((await attr()) === first) throw new Error("System did not flip");
    await page.evaluate(() => localStorage.clear());
  });

  await check("sheet: on a phone every ⋯ menu is a bottom sheet with a handle; the backdrop and a swipe down close it", async () => {
    await desktopList(390);
    await page.waitForFunction(() => ledger.on === true, null, { timeout: 8000 });
    for (const name of ["fleet", "theme", "notify", "row"]) {
      await OPEN[name]();
      await page.waitForTimeout(400); // the slide-in
      const g = await page.evaluate((sel) => { const e = document.querySelector(sel), r = e.getBoundingClientRect(), gr = e.querySelector(".grab").getBoundingClientRect(), bk = document.querySelector("#sheet-bk"); return { x: r.x, w: r.width, bottom: r.bottom, top: r.top, vh: innerHeight, vw: innerWidth, grab: gr.height, bk: !bk.hidden && getComputedStyle(bk).display !== "none" }; }, BOX[name]);
      if (g.x !== 0 || Math.abs(g.w - g.vw) > 1 || Math.abs(g.bottom - g.vh) > 1 || g.grab < 10 || !g.bk || g.top < g.vh * 0.15) throw new Error(`${name}: not a bottom sheet ` + JSON.stringify(g));
      // A short pull springs back; a long one closes.
      const hb = await page.locator(BOX[name] + " .grab").boundingBox();
      const cx = hb.x + hb.width / 2, cy = hb.y + hb.height / 2;
      await page.mouse.move(cx, cy); await page.mouse.down(); await page.mouse.move(cx, cy + 20, { steps: 3 }); await page.mouse.up();
      await page.waitForTimeout(350);
      if (!(await vis(BOX[name]))) throw new Error(`${name}: a 20px pull closed it`);
      // The spring-back must not replay the slide-in (it used to, 180 ms later, which moved the handle under the next pull).
      if (await page.evaluate((sel) => document.querySelector(sel).getAnimations().filter((a) => a.animationName === "sheet-in").length, BOX[name])) throw new Error(`${name}: the slide-in replayed after a short pull`);
      const hb2 = await page.locator(BOX[name] + " .grab").boundingBox();
      await page.mouse.move(hb2.x + hb2.width / 2, hb2.y + hb2.height / 2); await page.mouse.down();
      await page.mouse.move(hb2.x + hb2.width / 2, hb2.y + 160, { steps: 8 }); await page.mouse.up();
      await page.waitForSelector(BOX[name], { state: "hidden", timeout: 2000 }).catch(() => { throw new Error(`${name}: a swipe down did not close it`); });
      if (await vis("#sheet-bk")) throw new Error(`${name}: the backdrop stayed`);
      await OPEN[name]();
      await page.mouse.click(195, 60); // the backdrop
      await page.waitForSelector(BOX[name], { state: "hidden", timeout: 2000 }).catch(() => { throw new Error(`${name}: the backdrop did not close it`); });
    }
    await noOverflow();
  });

  // screenshots of the popovers, in two themes, at 1280 and 390
  for (const width of [1280, 390]) {
    await check(`menus: the theme picker and the fleet menu in two themes at ${width}px (screenshots)`, async () => {
      for (const th of ["dark", "gruvbox-light"]) {
        await desktopList(width);
        await page.evaluate((t) => ComebyeThemes.set(t), th);
        await page.waitForFunction(() => ledger.on === true, null, { timeout: 8000 });
        await OPEN.fleet();
        await page.waitForTimeout(450); // the open animation
        await page.screenshot({ path: `${out}/menus-fleet-${th}-${width}.png` });
        await page.locator("#f-theme").click();
        await page.waitForSelector("#theme-panel", { state: "visible" });
        await page.waitForTimeout(450);
        await page.screenshot({ path: `${out}/menus-theme-${th}-${width}.png` });
        const bad = await page.evaluate(() => [...document.querySelectorAll("#theme-panel *")].filter((e) => e.getClientRects().length && e.scrollWidth > e.clientWidth + 1 && getComputedStyle(e).overflowX === "visible").map((e) => e.tagName + "." + e.className));
        if (bad.length) throw new Error("clipped: " + bad.join(","));
        await page.keyboard.press("Escape");
      }
      await noOverflow();
      await page.evaluate(() => localStorage.clear());
    });
  }

  // ---- the Briefings card on Status ----
  const schedule = () => page.evaluate(async () => (await (await fetch("/api/brief/schedule")).json()).schedule);
  const cardSteps = async (card) => {
    const c = (s) => page.locator(`${card} ${s}`);
    await page.waitForSelector(`${card} .bcard`, { state: "visible", timeout: 8000 });
    const t = await c(".bcard").innerText();
    for (const w of ["Last briefing", "Next briefing", "Skip when nothing has changed", "Max quiet", "Publish now", "History"]) if (!t.includes(w)) throw new Error("card lacks " + w + ": " + t);
    for (const l of ["Off", "1 h", "3 h", "6 h"]) if (!(await c(".bc-seg button").filter({ hasText: new RegExp("^" + l + "$") }).count())) throw new Error("no " + l + " button");
    await c('[data-bi="3h"]').click();
    await page.waitForFunction(() => /Briefing every 3 h/.test(document.querySelector("#toast").textContent), null, { timeout: 5000 });
    let sc = await schedule();
    if (!sc.enabled || sc.every !== "3h") throw new Error("3 h not saved: " + JSON.stringify(sc));
    if ((await c('[data-bi="3h"]').getAttribute("aria-pressed")) !== "true") throw new Error("3 h not pressed");
    if (!/Next briefing\s*\n?.*\S/.test(await c(".bcard").innerText()) || /Next briefing\s+off/.test(await c(".bcard").innerText())) throw new Error("next slot missing: " + await c(".bcard").innerText());
    await page.waitForFunction((cd) => document.activeElement?.dataset?.bk === "int-3h" && document.activeElement.closest(cd), card, { timeout: 2000 }).catch(() => { throw new Error("focus lost after saving"); });
    await c('[data-bskip]').uncheck();
    await page.waitForFunction(() => /every slot/.test(document.querySelector("#toast").textContent), null, { timeout: 5000 });
    sc = await schedule();
    if (!sc.always) throw new Error("skip off not saved: " + JSON.stringify(sc));
    if (!(await c("[data-bquiet]").isDisabled())) throw new Error("max quiet should wait for the skip");
    await c('[data-bskip]').check();
    await page.waitForFunction(() => /Skipping when nothing/.test(document.querySelector("#toast").textContent), null, { timeout: 5000 });
    await c("[data-bquiet]").selectOption("12h");
    await page.waitForFunction(() => /Max quiet 12 h/.test(document.querySelector("#toast").textContent), null, { timeout: 5000 });
    sc = await schedule();
    if (sc.always || sc.max_quiet !== "12h" || sc.every !== "3h") throw new Error("quiet not saved: " + JSON.stringify(sc));
    await c('[data-bi=""]').click();
    await page.waitForFunction(() => /briefings off/.test(document.querySelector("#toast").textContent), null, { timeout: 5000 });
    sc = await schedule();
    if (sc.enabled) throw new Error("Off not saved");
    if (!(await c("[data-bskip]").isDisabled())) throw new Error("the options stay live while off");
    await c('[data-bi="1h"]').click();
    await page.waitForFunction(() => /every 1 h/.test(document.querySelector("#toast").textContent), null, { timeout: 5000 });
    if ((await schedule()).every !== "1h") throw new Error("1 h not saved");
    await c("[data-bpub]").click();
    await page.waitForFunction(() => /No coordinator|Asked/.test(document.querySelector("#toast").textContent), null, { timeout: 5000 });
  };
  await check("briefings card: on the desktop Status page it shows the times and applies each change through the schedule API", async () => {
    await desktopList(1280, "#/status");
    await page.waitForSelector("#p-status", { state: "visible" });
    await cardSteps("#ps-body");
    await page.screenshot({ path: `${out}/briefings-card-1280.png` });
    const bad = await page.evaluate(() => [...document.querySelectorAll("#ps-body .bcard *")].filter((e) => e.getClientRects().length && e.scrollWidth > e.clientWidth + 1 && getComputedStyle(e).display !== "inline").map((e) => e.tagName + "." + e.className));
    if (bad.length) throw new Error("clipped: " + bad.join(","));
    await page.locator("#ps-body [data-bhist]").click();
    await page.waitForSelector("#p-briefing, #briefs", { state: "visible" });
  });
  await check("briefings card: on the phone it sits at the top of the briefings screen and the schedule is gone from ⋯", async () => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#agents .agent");
    await page.waitForFunction(() => ledger.on === true, null, { timeout: 8000 });
    await page.evaluate(() => { location.hash = "#/briefings"; });
    await page.waitForSelector("#br-status", { state: "visible" });
    await cardSteps("#br-status");
    await page.screenshot({ path: `${out}/briefings-card-390.png` });
    await noOverflow();
    const bad = await page.evaluate(() => [...document.querySelectorAll("#br-status .bcard *")].filter((e) => e.getClientRects().length && e.scrollWidth > e.clientWidth + 1 && getComputedStyle(e).display !== "inline").map((e) => e.tagName + "." + e.className));
    if (bad.length) throw new Error("clipped: " + bad.join(","));
    if (nativeDialogs) throw new Error("a native dialog opened");
    if (await page.locator("#f-sched").count()) throw new Error("#f-sched is still in the page");
  });
}

// Collapsing the left column (desktop): a rail with the expand button and the counts, the
// choice kept per browser, the "[" key, and the phone untouched.
await check("collapse: the list folds to a rail, keeps its state, and expands again", async () => {
  await desktopList(1280, "");
  await page.locator("#agents .agent[data-id]").first().click();
  await page.waitForSelector("body.has-detail #detail");
  await page.locator("#prompt-text").fill("half typed");
  const before = await geo("#detail");
  await page.evaluate(() => { document.getElementById("agents").scrollTop = 7; });
  await page.locator("#btn-collapse").click();
  const [r, d, l] = await Promise.all([geo("#list-rail"), geo("#detail"), geo("#agents")]);
  if (!r.shown || r.w > 80) throw new Error("rail: " + JSON.stringify(r));
  if (l.shown) throw new Error("the list still shows");
  if (d.w < before.w + 250 || Math.abs(d.x + d.w - 1280) > 1) throw new Error("detail did not take the width: " + JSON.stringify([before, d]));
  if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("scrolls sideways collapsed");
  if (await page.locator("#prompt-text").innerText() !== "half typed") throw new Error("prompt lost");
  if (await page.evaluate(() => localStorage.getItem("comebye-list-collapsed")) !== "1") throw new Error("not remembered");
  await page.reload({ waitUntil: "load" });
  await page.waitForSelector("body.list-collapsed");
  if (!(await visible("#list-rail"))) throw new Error("rail missing after reload");
  await page.locator("#list-rail").click();
  if (await page.evaluate(() => document.body.classList.contains("list-collapsed"))) throw new Error("still collapsed");
  const l2 = await geo("#agents");
  if (!l2.shown || l2.w < 300) throw new Error("list not back: " + JSON.stringify(l2));
  if (!(await page.locator("#agents .agent.selected").count())) throw new Error("selection lost");
  if (await page.evaluate(() => localStorage.getItem("comebye-list-collapsed")) !== "0") throw new Error("expanded not remembered");
});
await check("collapse: the rail shows how many agents need input and open to-dos", async () => {
  await desktopList(1280, "");
  await page.evaluate(() => { todos.on = true; todos.open = 3; state.agents = state.agents.map((a, i) => i < 2 ? { ...a, agent_status: "blocked" } : a); renderList(); renderTodos(true); });
  await page.locator("#btn-collapse").click();
  const need = await page.locator("#lr-need").innerText(), td = await page.locator("#lr-todo").innerText();
  if (need !== "2" || td !== "3") throw new Error("badges: " + need + "/" + td);
});
await check("collapse: [ toggles it, but not while typing", async () => {
  await desktopList(1280, "");
  const col = () => page.evaluate(() => document.body.classList.contains("list-collapsed"));
  await page.locator("body").press("[");
  if (!(await col())) throw new Error("[ did not collapse");
  await page.locator("body").press("[");
  if (await col()) throw new Error("[ did not expand");
  await page.locator("#agents .agent[data-id]").first().click();
  await page.locator("#prompt-text").click();
  await page.keyboard.type("a[b");
  if (await col()) throw new Error("[ toggled while typing");
  if (await page.locator("#prompt-text").innerText() !== "a[b") throw new Error("[ not typed");
});
await check("collapse: the palette has Collapse/Expand the list and the shortcut sheet lists [", async () => {
  await desktopList(1280, "");
  const col = () => page.evaluate(() => document.body.classList.contains("list-collapsed"));
  await page.locator("body").press("Control+k");
  await page.keyboard.type("collapse the list");
  await page.waitForSelector("#palette .pal-it");
  await page.keyboard.press("Enter");
  if (!(await col())) throw new Error("palette command did not collapse");
  await page.locator("body").press("Control+k");
  await page.keyboard.type("expand the list");
  await page.waitForSelector("#palette .pal-it");
  await page.keyboard.press("Enter");
  if (await col()) throw new Error("palette command did not expand");
  await page.locator("body").press("?");
  const t = await page.locator("#cheats").innerText();
  if (!/Collapse \/ expand the list/.test(t)) throw new Error("sheet: " + t.slice(0, 200));
  await page.keyboard.press("Escape");
});
await check("collapse: with the Diff panel open the chat and panel share the full width", async () => {
  await desktopList(1280, "");
  await page.locator("#agents .agent[data-id]").first().click();
  await page.waitForSelector("body.has-detail #detail");
  await page.locator("body").press("d");
  await page.waitForSelector("#detail.sp-open #sp");
  await page.locator("body").press("[");
  const [sp, d] = await Promise.all([geo("#sp"), geo("#detail")]);
  if (!sp.shown || Math.abs(sp.x + sp.w - 1280) > 1 || d.x > 60) throw new Error("layout: " + JSON.stringify([sp, d]));
  if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("scrolls sideways");
  await page.locator("body").press("[");
  await page.locator("body").press("d");
});
await check("collapse: the phone keeps the list as it was, even with the choice stored", async () => {
  await desktopList(1280, "");
  await page.locator("#btn-collapse").click();
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent[data-id]", { state: "visible" });
  if (await visible("#btn-collapse") || await visible("#list-rail")) throw new Error("collapse controls on the phone");
  if (!(await visible("#agents")) || !(await visible("#list header"))) throw new Error("list hidden on the phone");
  await page.evaluate(() => { try { localStorage.clear(); } catch {} });
});

fs.writeFileSync(`${out}/browser.log`, log.join("\n") + "\n");
// ---- the Releases tab on Peers ----
const REL_SHA = "0123456789abcdef0123456789abcdef01234567";
const relPost = async (b) => {
  // The server allows 20 to-do adds a minute and the checks before use them: wait for room.
  for (let i = 0; i < 14; i++) {
    const r = await page.evaluate(async (b) => { const x = await fetch("/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(b) }); return { code: x.status, body: await x.json() }; }, b);
    if (r.code !== 429) return r.body.todo;
    await page.waitForTimeout(5000);
  }
  return undefined;
};
const relDrop = (id) => page.evaluate((id) => fetch("/api/todos/" + id + "/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }), id).catch(() => {});
const relHscroll = () => page.evaluate(() => {
  const sel = "#pr-releases";
  const bad = [...document.querySelectorAll(sel + " *")].filter((e) => e.offsetParent && e.scrollWidth > e.clientWidth + 1 && getComputedStyle(e).display !== "inline" && !e.closest("pre")).map((e) => e.tagName + "." + e.className + " " + e.scrollWidth + ">" + e.clientWidth);
  return { bad, page: [document.documentElement.scrollWidth, innerWidth], pane: [document.querySelector(sel).scrollWidth, document.querySelector(sel).clientWidth] };
});
const openReleases = async (width) => {
  await page.setViewportSize({ width, height: width < 500 ? 844 : 800 });
  await page.goto(base + "/#/peers/~releases", { waitUntil: "load" });
  await page.reload({ waitUntil: "load" });
  await page.waitForSelector("#pr-releases .rl-card", { timeout: 20000 });
};

// Every state of the tab, from canned answers (relfix.mjs), at both widths.
for (const width of [1280, 390]) {
await check(`releases tab at ${width}: tabs and badge, and the card and fleet table in every state; nothing scrolls sideways`, async () => {
  let state = "none";
  await page.route(/\/api\/releases(\?.*)?$/, (r) => r.fulfill({ json: releaseView(state) }));
  try {
    const txt = () => page.locator("#pr-releases").innerText();
    const reload = async (s) => { state = s; await page.evaluate(() => loadReleases(false)); await page.waitForFunction((v) => document.querySelector("#pr-releases").innerText.length > 0 && rls.data && rls.data.history, null); await page.waitForTimeout(100); };
    await openReleases(width);
    // the tabs
    if ((await page.locator("#pr-tabs [role=tab]:not([hidden])").allInnerTexts()).map((t) => t.trim().replace(/\s+\d$/, "")).join("|") !== "Hosts|Releases") throw new Error("tabs");
    if ((await page.locator("#pr-tab-rel").getAttribute("aria-selected")) !== "true") throw new Error("the deep link did not select Releases");
    // nothing pending
    let t = await txt();
    if (!/No release pending/.test(t) || !/Everything runs v0\.5\.21/.test(t) || (await page.locator("#pr-dot:visible").count())) throw new Error("none: " + t);
    if ((await page.locator("#pr-releases .rl-hist tr").count()) !== 5) throw new Error("history is not capped at 5");
    for (const s of ["ci", "green", "red"]) {
      await reload(s); t = await txt();
      const dis = await page.locator("#pr-releases .rel-approve").isDisabled();
      if (!t.includes("v0.5.22") || !t.includes("a3f9c1e")) throw new Error(s + ": " + t);
      if (s === "ci" && !(/CI in progress/.test(t) && dis)) throw new Error("ci running: " + t);
      if (s === "ci" && (await page.locator("#pr-dot:visible").count())) throw new Error("badge while CI runs");
      if (s === "green" && (dis || !/CI passed/.test(t) || !/4 changes since v0\.5\.21/.test(t) || !(await page.locator("#pr-dot:visible").count()))) throw new Error("green: " + t);
      if (s === "red" && (!dis || !/CI failure/.test(t) || !/Fix CI first/.test(t) || !(await page.locator("#pr-dot:visible").count()))) throw new Error("red: " + t);
      if (!(await page.locator("#pr-releases .rl-host").count())) throw new Error(s + ": no fleet");
      if (await page.locator("#pr-releases .rl-host button:not(.cpb)").count()) throw new Error(s + ": a host button before the tag");
    }
    // tagged: the hub is behind, every row offers its button
    await reload("tagged"); t = await txt();
    const up = await page.locator("#pr-releases .rl-host button:not(.cpb)").allInnerTexts();
    if (!/0 of 4/.test(t) || up.join("|") !== "Bump hub|Upgrade|Upgrade|Upgrade") throw new Error("tagged: " + up + " " + t);
    if ((await page.locator("#pr-releases .rl-host").first().innerText()).indexOf("hub") < 0) throw new Error("hub row");
    // partial, with an offline host: 2 of 4, 1 offline; the offline row's button is disabled; no "all upgraded"
    await reload("partial"); t = await txt();
    if (!/2 of 4, 1 offline/.test(t) || /all upgraded/i.test(t)) throw new Error("partial: " + t);
    const off = page.locator('#pr-releases .rl-host[data-host="mac-mini"]');
    if (!(await off.locator("button:not(.cpb)").isDisabled()) || !/not reachable since 2d ago/.test(await off.innerText())) throw new Error("offline row: " + (await off.innerText()));
    if (!(await page.locator('#pr-releases .rl-host[data-host="kubevirt-1"] .rel-up:not(:disabled)').count())) throw new Error("kubevirt-1 has no Upgrade");
    if ((await page.locator("#pr-releases .rl-prog").getAttribute("aria-valuenow")) !== "2") throw new Error("progress");
    await page.screenshot({ path: `${out}/releases-partial-${width}.png` });
    let h = await relHscroll(); if (h.bad.length || h.page[0] > h.page[1] || h.pane[0] > h.pane[1]) throw new Error("partial: sideways scroll " + JSON.stringify(h));
    await reload("partial3"); t = await txt();
    if (!/3 of 4, 1 offline/.test(t) || /all upgraded/i.test(t)) throw new Error("partial3: " + t);
    // deploy failed: its error and a Retry
    await reload("failed"); t = await txt();
    const hub = page.locator('#pr-releases .rl-host[data-host="laharl"]');
    if (!/FAILED phase=reconcile/.test(await hub.innerText()) || (await hub.locator("button:not(.cpb)").innerText()).trim() !== "Retry") throw new Error("deploy failed: " + (await hub.innerText()));
    const pb = page.locator('#pr-releases .rl-host[data-host="peerbox"]');
    if (!/rolled back/.test(await pb.innerText()) || (await pb.locator("button:not(.cpb)").innerText()).trim() !== "Retry") throw new Error("upgrade failed: " + (await pb.innerText()));
    if (!(await page.locator("#pr-dot:visible").count())) throw new Error("no badge on a failure");
    await page.screenshot({ path: `${out}/releases-failed-${width}.png` });
    h = await relHscroll(); if (h.bad.length || h.page[0] > h.page[1] || h.pane[0] > h.pane[1]) throw new Error("failed: sideways scroll " + JSON.stringify(h));
    // all done: green end state, no buttons but Roll back, no badge
    await reload("done"); t = await txt();
    if (!/All upgraded/.test(t) || !/4 of 4/.test(t) || (await page.locator("#pr-dot:visible").count())) throw new Error("done: " + t);
    const left = (await page.locator("#pr-releases .rl-host button").allInnerTexts()).filter((x) => !/Roll back/.test(x));
    if (left.length) throw new Error("done: buttons left " + left);
    await page.screenshot({ path: `${out}/releases-done-${width}.png` });
    h = await relHscroll(); if (h.bad.length || h.page[0] > h.page[1] || h.pane[0] > h.pane[1]) throw new Error("done: sideways scroll " + JSON.stringify(h));
    // the Hosts tab still lists the hosts, and nudges while a release needs you
    await reload("tagged");
    await page.locator("#pr-tab-hosts").click();
    await page.waitForSelector("#pr-items .pr-card");
    if (!(await page.locator("#pr-items .pr-nudge").count()) || (await page.locator("#pr-releases:visible").count())) throw new Error("hosts tab");
    h = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
    if (h[0] > h[1]) throw new Error("hosts tab: sideways scroll " + h);
  } finally { await page.unroute(/\/api\/releases(\?.*)?$/); }
});
}

// The palette opens the tab; the to-do item is a one-line ping that links to it.
await check("releases: the command palette has an entry for the tab, and the release to-do is a ping with Open in Peers", async () => {
  await desktopList(1280, "#/todo");
  await page.keyboard.press("Control+k");
  await page.locator("#pal-q").fill("releases");
  await page.waitForSelector("#palette .pal-item, #palette [role=option]");
  await page.keyboard.press("Enter");
  await page.waitForFunction(() => location.hash === "#/peers/~releases");
  const t = await relPost({ question: "", kind: "release", no_wait: true, release: { version: "v9.1.0", commit: REL_SHA } });
  if (!t) throw new Error("not raised");
  try {
    await desktopList(1280, "#/todo");
    await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
    const item = `#p-todo .td-item[data-id="${t.id}"]`;
    await page.waitForSelector(item);
    if (!(await page.locator(item + ".open").count())) await page.locator(item + " > .td-row").click();
    const ping = item + " .rel-ping";
    await page.waitForSelector(ping);
    if (await page.locator(item + " .rel-approve, " + item + " .rel-up, " + item + " .rel-dep-btn").count()) throw new Error("the ping carries release buttons");
    await page.locator(ping + " .rel-open").click();
    await page.waitForFunction(() => location.hash === "#/peers/~releases");
    await page.waitForSelector('#pr-releases [data-card=release]');
  } finally { await relDrop(t.id); }
});

// The real flow against the simulated server: raise, Approve with its confirm, tag, build, Bump hub (running, passed,
// failed, retry), the item's ping, and the hub closing the item itself.
for (const width of [1280, 390]) {
await check(`releases live at ${width}: Approve needs a confirm naming the version and commit; Bump hub is offered once tagged, runs, fails and retries; history lists the tagged release`, async () => {
  const OK = width === 1280 ? "v9.8.1" : "v9.7.1", BAD = width === 1280 ? "v9.8.13" : "v9.7.13";
  const items = [];
  try {
    const a = await relPost({ question: "", kind: "release", no_wait: true, release: { version: OK, commit: REL_SHA } });
    if (!a) throw new Error("not raised"); items.push(a);
    await openReleases(width);
    const card = "#pr-releases [data-card=release]", fleet = "#pr-releases [data-fleet]";
    await page.waitForSelector(card + " .rel-facts");
    const pane = () => page.locator("#pr-releases").innerText();
    let tx = await pane();
    if (!tx.includes(REL_SHA) || !/CI passed/.test(tx) || !/2 changes since v0\.0\.0/.test(tx)) throw new Error("facts: " + tx);
    if (!(await page.locator("#pr-dot:visible").count())) throw new Error("no badge for a release waiting for approval");
    await page.locator(card + " .rel-approve").click();
    let ct = await page.locator("#pr-releases .rel-confirm").innerText();
    if (!ct.includes(OK) || !ct.includes(REL_SHA)) throw new Error("confirm: " + ct);
    await page.locator("#pr-releases .rel-confirm [data-rel=cancel]").click();
    await page.waitForSelector("#pr-releases .rel-confirm", { state: "detached" });
    const noTok = await page.evaluate(async ([id, v, c]) => (await fetch(`/api/release/${id}/approve`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ version: v, commit: c }) })).status, [a.id, OK, REL_SHA]);
    if (noTok !== 403) throw new Error("approve without the CSRF token = " + noTok);
    await page.locator(card + " .rel-approve").click();
    await page.locator("#pr-releases .rel-confirm .rel-go").click();
    await page.waitForSelector(card + " .rel-s-tagged", { timeout: 15000 });
    // tagged: the Bump hub button, never run by Approve
    const dep = fleet + " .rl-host[data-deploy]";
    await page.waitForSelector(dep + " .rel-dep-btn", { timeout: 15000 });
    if (!/Bump hub/.test(await page.locator(dep + " .rel-dep-btn").innerText())) throw new Error("label");
    await page.locator(dep + " .rel-dep-btn").click();
    ct = await page.locator("#pr-releases .rel-confirm").innerText();
    if (!ct.includes(OK) || !/separate step/.test(ct)) throw new Error("deploy confirm: " + ct);
    await page.locator("#pr-releases .rel-confirm [data-rel=cancel]").click();
    const noTok2 = await page.evaluate(async ([id, v, c]) => (await fetch(`/api/release/${id}/deploy`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ version: v, commit: c }) })).status, [a.id, OK, REL_SHA]);
    if (noTok2 !== 403) throw new Error("deploy without the token = " + noTok2);
    await page.locator(dep + " .rel-dep-btn").click();
    await page.locator("#pr-releases .rel-confirm .rel-go").click();
    await page.waitForFunction((s) => /running/.test(document.querySelector(s).innerText), dep, { timeout: 10000 });
    if (await page.locator(dep + " .rel-dep-btn").count()) throw new Error("button offered while it runs");
    await page.waitForFunction((s) => /done/.test(document.querySelector(s).innerText), dep, { timeout: 20000 });
    let h = await relHscroll(); if (h.bad.length || h.page[0] > h.page[1]) throw new Error("sideways scroll " + JSON.stringify(h));
    await page.screenshot({ path: `${out}/releases-live-${width}.png` });
    // the hub is not on the release's version (a dev build), so the item stays open and the tab keeps it
    const st = await page.evaluate(async (id) => (await (await fetch("/api/todos/" + id)).json()).todo.state, a.id);
    if (st !== "open") throw new Error("the item closed although the hub does not run " + OK + ": " + st);
    // dropping the ping does not take the release away from the tab
    await relDrop(a.id);
    await page.evaluate(() => loadReleases(true));
    await page.waitForFunction((v) => rls.data && rls.data.current && rls.data.todo.release.version === v && rls.data.todo.state === "dropped", OK, { timeout: 10000 });
    tx = await pane();
    if (!tx.includes(OK) || !/to-do item is dropped/.test(tx) || !(await page.locator(dep).count())) throw new Error("dropped item took the release away: " + tx);
    // a failing deploy shows its words and a Retry
    const b = await relPost({ question: "", kind: "release", no_wait: true, release: { version: BAD, commit: REL_SHA } });
    if (!b) throw new Error("not raised b"); items.push(b);
    await page.evaluate(async ([id, v, c]) => {
      const d = await (await fetch("/api/release/" + id)).json();
      await fetch("/api/release/" + id + "/approve", { method: "POST", headers: { "Content-Type": "application/json", "X-Comebye-Csrf": d.csrf }, body: JSON.stringify({ version: v, commit: c }) });
    }, [b.id, BAD, REL_SHA]);
    await page.waitForFunction(async (id) => (await (await fetch("/api/todos/" + id)).json()).todo.release.state === "tagged", b.id, { timeout: 15000 });
    await page.evaluate(() => loadReleases(true));
    await page.waitForFunction((v) => rls.data && rls.data.todo.release.version === v && document.querySelector("#pr-releases .rel-dep-btn"), BAD, { timeout: 10000 });
    await page.locator(dep + " .rel-dep-btn").click();
    await page.locator("#pr-releases .rel-confirm .rel-go").click();
    await page.waitForFunction((s) => /failed/.test(document.querySelector(s).innerText), dep, { timeout: 20000 });
    if ((await page.locator(dep + " .rel-dep-btn").innerText()).trim() !== "Retry") throw new Error("no Retry");
    if ((await page.locator(dep + " .rel-dephead").innerText()).trim() !== "bump: FAILED phase=reconcile reason=flux: command not found") throw new Error("failed headline");
    h = await relHscroll(); if (h.bad.length || h.page[0] > h.page[1]) throw new Error("failed: sideways scroll " + JSON.stringify(h));
    // the history lists the earlier release (not the current one)
    if (!(await page.locator("#pr-releases .rl-hist").innerText()).includes(OK)) throw new Error("history lacks " + OK);
  } finally {
    for (const t of items) await relDrop(t.id);
  }
});
}
// Each host's row is its own: the Bump hub deploy sits on the hub's row, the release host keeps its own Upgrade, and a failure
// from before a host reached the version is not its status (the v0.5.24 rollout screenshot, relfix.mjs "rollout").
for (const width of [1280, 390]) {
await check(`releases fleet rows at ${width}: Bump hub on the hub's row, Upgrade on the release host's, no stale failure beside a match, one "by"`, async () => {
  await page.route(/\/api\/releases(\?.*)?$/, (r) => r.fulfill({ json: releaseView("rollout") }));
  try {
    await openReleases(width);
    await page.waitForSelector("#pr-releases [data-fleet] .rl-host");
    const row = (n) => page.locator(`#pr-releases [data-fleet] .rl-host[data-host="${n}"]`);
    const txt = (n) => row(n).innerText();
    // the hub's row: the deploy, its output, and where it runs; nothing doubled
    const hub = await txt("comebye-hub");
    if (!(await row("comebye-hub").getAttribute("data-deploy") !== null)) throw new Error("the deploy is not on the hub's row");
    if (!/v0\.5\.24 done/.test(hub) || !/runs on laharl/.test(hub) || /hub says/.test(hub)) throw new Error("hub row: " + hub);
    if (!(await row("comebye-hub").locator("[data-deploy-out]").count())) throw new Error("the bump output is not on the hub's row");
    // laharl: still behind, with its own Upgrade, and none of the deploy
    const lh = await txt("laharl");
    if (!/v0\.5\.23/.test(lh) || !(await row("laharl").locator('[data-rel=up]').count()) || (await row("laharl").locator('[data-rel=deploy]').count())) throw new Error("laharl row: " + lh);
    if (/bump|done by/.test(lh) || (await row("laharl").getAttribute("data-deploy")) !== null) throw new Error("laharl carries the deploy: " + lh);
    // the Mac runs the version: no failure beside the check
    const mac = await txt("mac");
    if (!/✓ v0\.5\.24/.test(mac) || /failed|systemd-run/.test(mac)) throw new Error("mac row: " + mac);
    // a host behind with no record has a plain Upgrade
    if (!(await row("kubevirt-1").locator('[data-rel=up]').count())) throw new Error("kubevirt-1 has no Upgrade");
    const h = await relHscroll(); if (h.bad.length || h.page[0] > h.page[1]) throw new Error("sideways scroll " + JSON.stringify(h));
  } finally {
    await page.unroute(/\/api\/releases(\?.*)?$/);
  }
});
}
// Dismiss: the user abandons a tagged release's rollout. A confirm first, then it is out of the tab and the badge.
for (const width of [1280, 390]) {
await check(`releases dismiss at ${width}: Dismiss release asks for a confirm, then the release leaves the tab and the badge, its item is dropped, and it stays in the history`, async () => {
  const V = width === 1280 ? "v9.6.1" : "v9.5.1";
  // Earlier checks left tagged releases behind (their items dropped, the hub not on their version): dismiss them, as a user would.
  await page.evaluate(async () => {
    const all = (await (await fetch("/api/todos?state=all")).json()).todos.filter((x) => x.release && x.release.state === "tagged" && !x.release.dismissed);
    for (const x of all) {
      const d = await (await fetch("/api/release/" + x.id)).json();
      await fetch(`/api/release/${x.id}/dismiss`, { method: "POST", headers: { "Content-Type": "application/json", "X-Comebye-Csrf": d.csrf }, body: JSON.stringify({ version: x.release.version, commit: x.release.commit }) });
    }
  });
  const t = await relPost({ question: "", kind: "release", no_wait: true, release: { version: V, commit: REL_SHA } });
  if (!t) throw new Error("not raised");
  try {
    await page.evaluate(async ([id, v, c]) => {
      const d = await (await fetch("/api/release/" + id)).json();
      await fetch("/api/release/" + id + "/approve", { method: "POST", headers: { "Content-Type": "application/json", "X-Comebye-Csrf": d.csrf }, body: JSON.stringify({ version: v, commit: c }) });
    }, [t.id, V, REL_SHA]);
    await page.waitForFunction(async (id) => (await (await fetch("/api/todos/" + id)).json()).todo.release.state === "tagged", t.id, { timeout: 15000 });
    await openReleases(width);
    await page.waitForSelector("#pr-releases [data-rel=dismiss]");
    // no token, no dismissal
    const noTok = await page.evaluate(async ([id, v, c]) => (await fetch(`/api/release/${id}/dismiss`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ version: v, commit: c }) })).status, [t.id, V, REL_SHA]);
    if (noTok !== 403) throw new Error("dismiss without the CSRF token = " + noTok);
    await page.locator("#pr-releases [data-rel=dismiss]").click();
    const ct = await page.locator("#pr-releases .rel-confirm").innerText();
    if (!ct.includes(V) || !/Nothing is changed on any host/.test(ct)) throw new Error("confirm: " + ct);
    await page.locator("#pr-releases .rel-confirm [data-rel=cancel]").click();
    await page.waitForSelector("#pr-releases .rel-confirm", { state: "detached" });
    if ((await page.evaluate(async (id) => (await (await fetch("/api/todos/" + id)).json()).todo.release.dismissed, t.id))) throw new Error("Cancel dismissed it");
    await page.locator("#pr-releases [data-rel=dismiss]").click();
    await page.locator("#pr-releases .rel-confirm .rel-go").click();
    await page.waitForFunction((v) => /No release pending/.test(document.querySelector("#pr-releases").innerText) && !document.querySelector("#pr-releases").innerText.includes("Dismiss release"), V, { timeout: 10000 });
    if (await page.locator("#pr-dot:visible").count()) throw new Error("the badge stayed");
    const it = await page.evaluate(async (id) => (await (await fetch("/api/todos/" + id)).json()).todo, t.id);
    if (!it.release.dismissed || it.state !== "dropped") throw new Error("item: " + JSON.stringify(it.release) + it.state);
    if (!(await page.locator("#pr-releases .rl-hist").innerText()).includes(V)) throw new Error("history lacks " + V);
    const au = await page.evaluate(async (id) => JSON.stringify((await (await fetch("/api/release/" + id)).json()).audit), t.id);
    if (!au.includes("release.dismiss")) throw new Error("not in the audit log");
    const h = await relHscroll(); if (h.bad.length || h.page[0] > h.page[1]) throw new Error("sideways scroll " + JSON.stringify(h));
  } finally { await relDrop(t.id); }
});
}
await browser.close();
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"}  ${name}${ok ? "" : "  -- " + why}`); if (!ok) failed++; }
console.log(`artifacts: ${out}`);
process.exit(failed ? 1 : 0);
