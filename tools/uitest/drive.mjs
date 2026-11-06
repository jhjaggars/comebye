// Drives the web app in headless Chrome (mobile viewport) against the fake
// herdr: after a prompt is sent, the reply must show without re-selecting
// the agent, in the chat view and in the raw terminal view, and switching
// agents must not show the previous agent's view.
import { launch, mockLaunchRoutes } from "./lib.mjs";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
const WAIT = Number(process.env.REPLY_WAIT_MS || 9000);

const { browser, page } = await launch();
const log = [];
page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
page.on("response", (r) => { if (r.url().includes("/api/") && !r.url().includes("/api/events")) log.push(`${r.status()} ${r.request().method()} ${r.url().replace(base, "")}`); });

const results = [];
async function check(name, fn) {
  try { await fn(); results.push([name, true, ""]); }
  catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); }
  await page.screenshot({ path: `${out}/${name.replace(/\W+/g, "-")}.png` }).catch(() => {});
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
  // The ledger is known once the fleet menu's ledger items are enabled.
  await page.waitForFunction(() => !document.querySelector("#f-sweep").disabled && !document.querySelector("#f-sweep").hidden, null, { timeout: 8000 });
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
  await page.waitForFunction(() => document.querySelector("#rtr-banner") && !document.querySelector("#rtr-banner").hidden, null, { timeout: 8000 });
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
  await page.waitForFunction(() => document.querySelector("#rtr-banner")?.hidden, null, { timeout: 8000 });
});

const sweepPosts = [];
await page.route(/\/api\/sweep/, (r) => {
  const body = JSON.parse(r.request().postData() || "{}");
  sweepPosts.push(body);
  const ready = { id: "uitest/w9:p1", host: "uitest", name: "fix-login", branch: "fix/login", class: "ready", reason: "Sweep: finished; worktree clean; every commit of fix/login is in origin/main; nothing unpushed" };
  const look = { id: "uitest/w8:p1", host: "uitest", name: "feat-search", branch: "feat/search", class: "needs_look", reason: "pushed but not merged: 2 commits not in origin/main (an open PR?)" };
  r.fulfill({ json: { dry_run: !!body.dry_run, ready: [{ ...ready, marked: !body.dry_run }], needs_look: [look], skipped: [{ id: "x" }] } });
});

await check("sweep: the fleet menu item opens a dry run listing ready and needs-a-look", async () => {
  await toList();
  await fleetMenuOpen();
  await page.locator("#f-sweep").click();
  await page.waitForSelector("#sweep", { state: "visible" });
  await page.waitForFunction(() => document.querySelector("#sweep-list")?.innerText.includes("fix-login"), null, { timeout: 5000 });
  const t = await page.locator("#sweep-list").innerText();
  if (!t.includes("feat-search") || !t.includes("open PR")) throw new Error("list " + t);
  if (!sweepPosts.length || sweepPosts[0].dry_run !== true) throw new Error("first call was not a dry run: " + JSON.stringify(sweepPosts));
  if ((await page.locator("#sweep-go").innerText()).trim() !== "Mark 1 ready to remove") throw new Error("button " + (await page.locator("#sweep-go").innerText()));
  await noOverflow();
});

await check("sweep: confirming marks, then offers the removals review", async () => {
  await page.locator("#sweep-go").click();
  await page.waitForFunction(() => document.querySelector("#sweep-go")?.innerText.trim() === "Review removals", null, { timeout: 5000 });
  if (sweepPosts.length !== 2 || sweepPosts[1].dry_run !== false) throw new Error("second call " + JSON.stringify(sweepPosts));
  if (!(await page.locator("#sweep-list").innerText()).toLowerCase().includes("marked ready to remove")) throw new Error("no marked heading");
  await page.locator("#sweep-go").click();
  await page.waitForSelector("#review", { state: "visible", timeout: 5000 });
  await page.locator("#rv-close").click();
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
  const t = (await page.locator("#f-menu button:visible").allInnerTexts()).map((x) => x.trim());
  const want = [/^Sweep for finished agents/, /^Review removals/, /briefing/i, /^Briefings screen/, /^Briefing schedule/, /^Notifications/, /^Refresh/];
  if (t.length !== want.length || want.some((re, i) => !re.test(t[i]))) throw new Error("items " + JSON.stringify(t));
  const box = await page.locator("#f-menu").boundingBox();
  if (box.x < 0 || box.x + box.width > 390) throw new Error("menu off-screen " + JSON.stringify(box));
  await page.keyboard.press("Escape");
  await page.waitForSelector("#f-menu", { state: "hidden", timeout: 2000 });
  if (await page.locator("#btn-fleet").getAttribute("aria-expanded") !== "false") throw new Error("aria-expanded not reset");
});
await check("fleet menu: the dashed sweep row is gone and the removals banner stays", async () => {
  if (await page.locator("#sweep-open").count()) throw new Error("#sweep-open still in the page");
  if ((await page.locator("#agents, main").first().innerText()).includes("Sweep for finished agents")) throw new Error("sweep row text still on the main screen");
  // Mark one agent so the banner shows, then check the menu count too.
  await rowOf(PANES.alpha).locator(".row-more").click();
  await page.locator("#row-mark").click();
  await page.waitForSelector("#mk", { state: "visible" });
  await page.locator("#mk-go").click();
  await page.waitForFunction(() => document.querySelector("#rtr-banner") && !document.querySelector("#rtr-banner").hidden, null, { timeout: 8000 });
  if (!(await page.locator("#rtr-banner").innerText()).includes("Ready to remove (1)")) throw new Error("banner text");
  await fleetMenuOpen();
  if (!(await page.locator("#f-review").innerText()).includes("Review removals (1)")) throw new Error("menu count " + (await page.locator("#f-review").innerText()));
  await page.locator("#f-review").click();
  await page.waitForSelector("#review", { state: "visible", timeout: 5000 });
  await page.locator("#rv-close").click();
  await page.waitForSelector("#review", { state: "hidden" });
  await rowOf(PANES.alpha).locator(".row-more").click();
  await page.locator("#row-mark").click(); // unmark
  await page.waitForFunction(() => document.querySelector("#rtr-banner")?.hidden, null, { timeout: 8000 });
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
  await publishBrief("## History one\n\nbody one");
  await publishBrief("## History two\n\nbody two");
  await publishBrief("## History three\n\nbody three");
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

// ---- desktop view (>= 1100px): list | agent | briefing, side by side ----
const geo = (sel) => page.evaluate((s) => {
  const e = document.querySelector(s); if (!e) return null;
  const r = e.getBoundingClientRect(), cs = getComputedStyle(e);
  return { x: r.x, y: r.y, w: r.width, h: r.height, shown: cs.display !== "none" && cs.visibility !== "hidden" && r.width > 0 };
}, sel);
async function desktopList(width) {
  await page.setViewportSize({ width, height: 800 });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.evaluate(() => { try { localStorage.clear(); } catch {} });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent[data-id]", { state: "visible" });
}
for (const width of [1280, 1920]) {
  await check(`desktop: at ${width}px the list, an empty pane and the briefing panel sit side by side`, async () => {
    await publishBrief("## Status\n\nDesktop panel check\n\nlong detail");
    await desktopList(width);
    await page.waitForSelector("body.has-briefs #briefs.open .br-card");
    const [l, d, e, b, c, pg] = await Promise.all([geo("#agents"), geo("#detail"), geo("#detail-empty"), geo("#briefs"), geo("#briefing"), geo("#pager")]);
    if (!l.shown || l.x !== 0 || l.w < 300 || l.w > 400) throw new Error("list column: " + JSON.stringify(l));
    if (d.shown) throw new Error("agent pane shown with nothing selected");
    if (!e.shown || e.x < l.x + l.w - 1) throw new Error("empty state not beside the list: " + JSON.stringify(e));
    if (!b.shown || b.x < e.x + e.w - 1 || Math.abs(b.x + b.w - width) > 1 || b.h < 600) throw new Error("briefings panel not at the right edge: " + JSON.stringify(b));
    if (c.shown) throw new Error("the latest-briefing card shows next to the briefings panel");
    if (pg.shown) throw new Error("the pill shows on desktop");
    const t = await page.locator("#briefs").innerText();
    if (!t.includes("Desktop panel check")) throw new Error("latest briefing missing from the panel: " + t.slice(0, 200));
    if (!(await page.locator("#briefs .br-item").count())) throw new Error("no history in the panel");
    if (await page.locator("#br-back").isVisible()) throw new Error("a back button in the panel");
    if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("page scrolls horizontally");
  });
}
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
  await check(`desktop: at ${width}px the agent pane fills the space between the list and the briefings panel`, async () => {
    await desktopList(width);
    await page.locator('#agents .agent[data-id$="w1:p1"]').first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.waitForSelector("#chat", { state: "visible" });
    const [l, d, m, c, h, b] = await Promise.all([geo("#agents"), geo("#detail"), geo("#detail main"), geo("#chat"), geo("#detail header"), geo("#briefs")]);
    if (Math.abs(d.x - 360) > 2 || Math.abs(d.x + d.w - b.x) > 1.5) throw new Error("pane does not span list..panel: " + JSON.stringify([l, d, b]));
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
await check("desktop: the fleet menu opens beside the list and its briefing item points at the panel", async () => {
  await desktopList(1280);
  await page.waitForFunction(() => !document.querySelector("#f-sweep").disabled, null, { timeout: 8000 });
  await page.locator("#btn-fleet").click();
  const m = await geo("#f-menu");
  if (!m.shown || m.x < 0 || m.x + m.w > 361) throw new Error("menu outside the list column: " + JSON.stringify(m));
  const t = await page.evaluate(() => ({ text: document.querySelector("#f-brief").textContent, dis: document.querySelector("#f-brief").disabled }));
  if (!/side panel/.test(t.text) || !t.dis) throw new Error("brief item: " + JSON.stringify(t));
  if (await page.locator("#f-briefs").isVisible()) throw new Error("the Briefings screen item shows on desktop");
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
  await page.locator(q(ids[0]) + " summary").click();
  if (!(await page.locator(q(ids[0]) + " .td-ctx h2").count())) throw new Error("context not rendered as markdown");
  if (await page.evaluate(() => window.__xss)) throw new Error("script in the context ran");
  if ((await page.locator(q(ids[0]) + " .td-q").innerText()).indexOf("<i>") < 0) throw new Error("question was not escaped");
  const href = await page.locator(q(ids[0]) + " .td-links a").getAttribute("href");
  if (href !== "https://example.com/pr/1") throw new Error("link " + href);
  await noOverflow();
  // Answer.
  await page.locator(q(ids[0]) + " .td-text").fill("yes, delete it");
  await page.locator(q(ids[0]) + " .td-answer-btn").click();
  await page.waitForSelector(`${q(ids[0])}.answered`, { timeout: 5000 });
  const ans = await page.evaluate((id) => fetch("/api/todos/" + id).then((r) => r.json()), ids[0]);
  if (ans.todo.state !== "answered" || ans.todo.answer.text !== "yes, delete it" || ans.todo.answer.via !== "app") throw new Error("answer " + JSON.stringify(ans));
  // Done, a single tap.
  await page.locator(q(ids[1]) + " summary").click();
  await page.locator(q(ids[1]) + " .td-done").click();
  await page.waitForFunction((s) => !document.querySelector(s), q(ids[1]), { timeout: 5000 });
  // Drop asks first: a refusal keeps the item.
  await page.locator(q(ids[2]) + " summary").click();
  await page.evaluate(() => window.confirm = () => false);
  await page.locator(q(ids[2]) + " .td-drop").click();
  await page.waitForTimeout(300);
  if (!(await page.locator(q(ids[2])).count())) throw new Error("dropped without the user confirming");
  await page.evaluate(() => window.confirm = () => true);
  await page.locator(q(ids[2]) + " .td-drop").click();
  await page.waitForFunction((s) => !document.querySelector(s), q(ids[2]), { timeout: 5000 });
  await page.screenshot({ path: `${out}/todo.png` });
});


/* ---------- transcripts (a synthetic index: tools/devserver/recall.go) ---------- */
const SMOKE = "#/session/devbox/claude/11111111-aaaa-4aaa-8aaa-111111111111";
const LONGS = "#/session/devbox/claude/22222222-bbbb-4bbb-8bbb-222222222222";
const noSideways = () => page.evaluate(() => {
  const el = document.querySelector("#recall");
  const over = [...el.querySelectorAll("*")].filter((x) => x.getBoundingClientRect().right > innerWidth + 1 && !x.closest(".rc-filters, .rc-subs, .rc-targs"));
  if (over.length || document.documentElement.scrollWidth > innerWidth) throw new Error("wider than the screen: " + (over[0]?.className || over[0]?.tagName));
});

await check("transcripts: the header icon searches, one card per session, real marks", async () => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.locator("#btn-recall").click();
  await page.waitForSelector("#recall", { state: "visible" });
  await page.waitForFunction(() => /indexed/.test(document.querySelector("#rc-fresh").textContent), null, { timeout: 5000 });
  await page.locator("#rc-q").fill("\"connection refused\"");
  await page.waitForSelector(".rc-card", { timeout: 5000 });
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
  await page.goto(base + "/#/transcripts?q=" + encodeURIComponent("connection refused"), { waitUntil: "load" });
  await page.locator(".rc-card", { hasText: "Retry the upgrade" }).click();
  await page.waitForSelector(".rc-subs", { timeout: 5000 });
  const on = await page.locator(".rc-subs .rc-chip.on").innerText();
  if (!on.startsWith("Subagent")) throw new Error("opened on " + on);
  await page.locator(".rc-subs .rc-chip", { hasText: "Main thread" }).click();
  await page.waitForFunction(() => document.querySelector(".rc-subs .rc-chip.on")?.textContent === "Main thread", null, { timeout: 5000 });
  await page.waitForFunction(() => /reproduce/.test(document.querySelector("#rc-msgs")?.innerText || ""), null, { timeout: 5000 });
});

await check("transcripts: a commit's SHA finds the session", async () => {
  await page.goto(base + "/#/transcripts?q=1a2b3c4d", { waitUntil: "load" });
  await page.waitForSelector(".rc-card", { timeout: 5000 });
  if (!(await page.locator("#rc-results").innerText()).includes("made 1a2b3c4d")) throw new Error("no commit card");
});

await check("transcripts: at desktop width, results and session side by side", async () => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(base + "/#/transcripts?q=" + encodeURIComponent("connection refused"), { waitUntil: "load" });
  await page.waitForSelector(".rc-card", { timeout: 5000 });
  await page.locator(".rc-card", { hasText: "Provision" }).click();
  await page.waitForFunction(() => /Provision/.test(document.querySelector(".rc-stitle")?.textContent || ""), null, { timeout: 5000 });
  if (!(await visible(".rc-list"))) throw new Error("the results went away");
  if (!(await page.locator(".rc-card.current").count())) throw new Error("the open session is not marked");
  const a = await page.locator(".rc-list").boundingBox(), b = await page.locator("#rc-session").boundingBox();
  if (a.x + a.width > b.x + 1 || b.width < 500) throw new Error(JSON.stringify({ a, b }));
  await page.keyboard.press("Escape");
  await page.waitForSelector("#recall", { state: "hidden" });
  await page.setViewportSize({ width: 390, height: 844 });
});

await check("transcripts: no index, an old index, a stale one, a deleted session", async () => {
  const withStatus = async (body, want) => {
    await page.route(/\/api\/recall\/status$/, (r) => r.fulfill({ json: body }));
    await page.goto("about:blank");
    await page.goto(base + "/#/transcripts", { waitUntil: "load" });
    await page.waitForFunction((w) => new RegExp(w).test(document.querySelector("#rc-results").textContent), want, { timeout: 5000 });
    await page.unroute(/\/api\/recall\/status$/);
  };
  await withStatus({ state: "missing", sessions: 0, messages: 0 }, "no transcript index yet");
  await withStatus({ state: "outdated", sessions: 0, messages: 0 }, "older version");
  await withStatus({ state: "stale", built_at: new Date(Date.now() - 86400000 * 2).toISOString(), sessions: 3, messages: 9 }, "last built 2d ago");
  await page.goto(base + "/#/session/devbox/claude/gone-session", { waitUntil: "load" });
  await page.waitForFunction(() => /isn't in the index/.test(document.querySelector("#rc-session").textContent), null, { timeout: 5000 });
});

fs.writeFileSync(`${out}/browser.log`, log.join("\n") + "\n");
await browser.close();
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"}  ${name}${ok ? "" : "  -- " + why}`); if (!ok) failed++; }
console.log(`artifacts: ${out}`);
process.exit(failed ? 1 : 0);
