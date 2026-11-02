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
  const want = [/^Sweep for finished agents/, /^Review removals/, /briefing/i, /^Briefing schedule/, /^Notifications/, /^Refresh/];
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
for (const width of [390, 1280]) {
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

fs.writeFileSync(`${out}/browser.log`, log.join("\n") + "\n");
await browser.close();
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"}  ${name}${ok ? "" : "  -- " + why}`); if (!ok) failed++; }
console.log(`artifacts: ${out}`);
process.exit(failed ? 1 : 0);
