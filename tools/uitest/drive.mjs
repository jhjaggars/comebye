// Drives the web app in headless Chrome (mobile viewport) against the fake
// herdr: after a prompt is sent, the reply must show without re-selecting
// the agent, in the chat view and in the raw terminal view, and switching
// agents must not show the previous agent's view.
import { launch, mockLaunchRoutes } from "./lib.mjs";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
const WAIT = Number(process.env.REPLY_WAIT_MS || 9000);

const { browser, page } = await launch({ dark: process.env.SCHEME !== "light" });
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
  const want = [/^Sweep for finished agents/, /^Review removals/, /briefing/i, /^Briefings screen/, /^Briefing schedule/, /^Notifications/, /^Theme:/, /^Refresh/];
  if (t.length !== want.length || want.some((re, i) => !re.test(t[i]))) throw new Error("items " + JSON.stringify(t));
  const box = await page.locator("#f-menu").boundingBox();
  if (box.x < 0 || box.x + box.width > 390) throw new Error("menu off-screen " + JSON.stringify(box));
  await page.keyboard.press("Escape");
  await page.waitForSelector("#f-menu", { state: "hidden", timeout: 2000 });
  if (await page.locator("#btn-fleet").getAttribute("aria-expanded") !== "false") throw new Error("aria-expanded not reset");
});
await check("theme: the menu cycles System / Light / Dark, it is stored, and a reload applies it before first paint", async () => {
  const bg = () => page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  const attr = () => page.evaluate(() => document.documentElement.getAttribute("data-theme"));
  const dark = "rgb(16, 18, 24)", light = "rgb(243, 244, 247)";
  const sys = process.env.SCHEME === "light" ? light : dark;
  await page.evaluate(() => { try { localStorage.removeItem("comebye.theme"); } catch {} });
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForSelector("#btn-fleet");
  if (await attr() !== null || await bg() !== sys) throw new Error("system: " + await attr() + " " + await bg());
  const step = async (label, want, color) => {
    await fleetMenuOpen();
    await page.locator("#f-theme").click();
    if (await attr() !== want) throw new Error(label + ": data-theme " + await attr());
    if (await bg() !== color) throw new Error(label + ": body " + await bg());
    const saved = await page.evaluate(() => localStorage.getItem("comebye.theme"));
    if (saved !== label) throw new Error("stored " + saved);
  };
  await step("light", "light", light);
  await step("dark", "dark", dark);
  // Forced dark survives a reload and is painted by the <head> script, before the app runs.
  await page.reload({ waitUntil: "domcontentloaded" });
  if (await attr() !== "dark" || await bg() !== dark) throw new Error("reload: " + await attr());
  await step("system", null, sys);
  await fleetMenuOpen();
  if (!/Theme: System/.test(await page.locator("#f-theme").innerText())) throw new Error("label");
  await page.keyboard.press("Escape");
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
    await publishBrief("## Status\n\nDesktop panel check\n\nlong detail");
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
  await page.waitForFunction(() => !document.querySelector("#f-sweep").disabled, null, { timeout: 8000 });
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
    // Search opens the Transcripts screen (openRecall), over the pages; closing it leaves no page behind.
    await page.locator('#nav .nv[data-page="search"]').click();
    await page.waitForSelector("#recall", { state: "visible" });
    if (!(await page.evaluate(() => location.hash)).startsWith("#/transcripts")) throw new Error("hash " + (await page.evaluate(() => location.hash)));
    await page.keyboard.press("Escape");
    await page.waitForSelector("#recall", { state: "hidden" });
    if (await page.evaluate(() => state.view) !== null) throw new Error("a page stays after closing Transcripts");
    // Opening Transcripts over an agent leaves it through closeDetail too.
    await page.locator('#agents .agent[data-id$="w1:p1"]').first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    posts.length = 0;
    await page.locator('#nav .nv[data-page="search"]').click();
    await page.waitForSelector("#recall", { state: "visible" });
    await page.waitForTimeout(300);
    if (await page.evaluate(() => state.selected) !== null || !posts.length || posts[posts.length - 1].pane !== "") throw new Error("the agent stays open under Transcripts: " + JSON.stringify(posts));
    await page.keyboard.press("Escape");
    await page.waitForSelector("#recall", { state: "hidden" });
    await page.locator('#nav .nv[data-page="status"]').click();
    await page.waitForSelector("#p-status", { state: "visible" });
    // The removals row follows the ledger's marks.
    const marks = await page.evaluate(() => ledger.marked.size);
    const row = await page.locator('#nav .nv[data-page="removals"]').count();
    if ((marks > 0) !== (row === 1)) throw new Error(`removals row ${row} for ${marks} marks`);
    if (row) {
      await page.locator('#nav .nv[data-page="removals"]').click();
      await page.waitForSelector("#review", { state: "visible" });
      await page.locator("#rv-close").click();
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
}
async function unmockStatusData() { await page.unroute(/\/api\/ledger$/); await page.unroute(/\/api\/todos$/); }
await check("status: the page groups the ledger, to-dos and agents by project, with reports and no Needs you section", async () => {
  await mockStatusData();
  try {
    await desktopList(1280, "#/status");
    await page.waitForSelector("#ps-body .st-proj", { timeout: 8000 });
    const t = await page.locator("#ps-body").innerText();
    for (const want of ["webapp", "ci-tools", "fix-login", "fix/login-timeout", "running 12m", "queued behind 1 step", "Keep old session cookies", "ready to remove", "landed", "failed"]) if (!t.includes(want)) throw new Error(`missing "${want}" in: ${t.slice(0, 400)}`);
    if (t.includes("Too old to show") || t.includes("ancient")) throw new Error("an entry older than 2 days is shown");
    if (/needs you/i.test(t)) throw new Error("a Needs you section");
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

// A host reports a damaged repository: the Status page says how to repair it, and
// the agents working in it are marked. Runs in both schemes (run.sh), at both widths.
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
  for (const b of list) out.push((await fetch("/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ no_wait: true, ...b }) }).then((r) => r.json())).todo.id);
  await loadTodos();
  return out;
}, list);
const dropTodos = (ids) => page.evaluate(async (ids) => {
  for (const i of ids) await fetch("/api/todos/" + i + "/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
  await loadTodos();
}, ids);
const openIds = (list) => page.evaluate((l) => [...document.querySelectorAll(l + " .td-item.open")].map((e) => e.dataset.id), list);
const focusedItem = () => page.evaluate(() => { const a = document.activeElement; return { cls: a.className, id: a.closest(".td-item")?.dataset.id || null }; });

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
const SMOKE = "#/session/devbox/claude/11111111-aaaa-4aaa-8aaa-111111111111";
const LONGS = "#/session/devbox/claude/22222222-bbbb-4bbb-8bbb-222222222222";
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
  // Content days old, with the indexer still looking: the age shown is the look's.
  await page.route(/\/api\/recall\/status$/, (r) => r.fulfill({ json: { state: "ok", built_at: new Date(Date.now() - 86400000 * 3).toISOString(), checked_at: new Date(Date.now() - 600000).toISOString(), sessions: 3, messages: 9 } }));
  await page.goto("about:blank");
  await page.goto(base + "/#/transcripts", { waitUntil: "load" });
  await page.waitForFunction(() => /indexed 10m ago/.test(document.querySelector("#rc-fresh").textContent), null, { timeout: 5000 });
  if (await page.locator("#rc-fresh.warn").count()) throw new Error("warns about a copy whose indexer is alive");
  await page.unroute(/\/api\/recall\/status$/);
  await page.goto(base + "/#/session/devbox/claude/gone-session", { waitUntil: "load" });
  await page.waitForFunction(() => /isn't in the index/.test(document.querySelector("#rc-session").textContent), null, { timeout: 5000 });
});

fs.writeFileSync(`${out}/browser.log`, log.join("\n") + "\n");
await browser.close();
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"}  ${name}${ok ? "" : "  -- " + why}`); if (!ok) failed++; }
console.log(`artifacts: ${out}`);
process.exit(failed ? 1 : 0);
