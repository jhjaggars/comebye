// Drives the web app in headless Chrome (mobile viewport) against the fake
// herdr: after a prompt is sent, the reply must show without re-selecting
// the agent, in the chat view and in the raw terminal view, and switching
// agents must not show the previous agent's view.
import { chromium } from "playwright-core";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
const CHROME = process.env.CHROME || "/usr/bin/google-chrome";
const WAIT = Number(process.env.REPLY_WAIT_MS || 9000);

const browser = await chromium.launch({ executablePath: CHROME, headless: true, args: ["--no-sandbox"] });
const ctx = await browser.newContext({
  viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, bypassCSP: true,
  userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
});
const page = await ctx.newPage();
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
const CATALOGS = {
  pi: { kind: "pi", source: "pi --list-models", exact: true, default: "homelab/thinking", default_thinking: "minimal",
    thinking_levels: ["off", "minimal", "low", "medium", "high", "xhigh", "max"],
    models: [
      { id: "homelab/fast", provider: "homelab", name: "fast" },
      { id: "homelab/thinking", provider: "homelab", name: "thinking", thinking: true },
      { id: "openai-codex/gpt-5.5", provider: "openai-codex", name: "gpt-5.5", thinking: true },
      { id: "openai-codex/gpt-5.6-luna", provider: "openai-codex", name: "gpt-5.6-luna", thinking: true },
    ] },
  claude: { kind: "claude", source: "curated", models: [
    { id: "opus", provider: "Aliases", note: "newest Opus" }, { id: "sonnet", provider: "Aliases", note: "newest Sonnet" },
    { id: "claude-opus-5-5", provider: "Full ids" } ] },
  codex: { kind: "codex", source: "none", models: [] },
};
let launchPosts = [];
const modelUrls = [];
await page.route(/\/api\/launch\/options/, (r) => r.fulfill({ json: {
  enabled: true, kinds: ["pi", "claude", "codex", "cursor"], default_kind: "pi", model_kinds: ["pi", "claude", "codex"],
  roots: [{ name: "~/code", path: "/home/u/code", label: "~/code", git: false }],
  recent: [{ name: "proj", path: "/home/u/code/proj", label: "~/code/proj", git: true }] } }));
await page.route(/\/api\/launch\/dirs/, (r) => r.fulfill({ json: { path: "/home/u/code", label: "~/code", dirs: [] } }));
await page.route(/\/api\/models/, (r) => {
  modelUrls.push(r.request().url().replace(base, ""));
  const kind = new URL(r.request().url()).searchParams.get("kind");
  r.fulfill({ json: { host: "uitest", catalogs: CATALOGS[kind] ? [CATALOGS[kind]] : [] } });
});
await page.route(/\/api\/launch$/, (r) => {
  if (r.request().method() !== "POST") return r.continue();
  launchPosts.push(JSON.parse(r.request().postData() || "{}"));
  r.fulfill({ json: { agent: { pane_id: "w1:p1", name: "alpha" }, workspace_id: "w9" } });
});
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

fs.writeFileSync(`${out}/browser.log`, log.join("\n") + "\n");
await browser.close();
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"}  ${name}${ok ? "" : "  -- " + why}`); if (!ok) failed++; }
console.log(`artifacts: ${out}`);
process.exit(failed ? 1 : 0);
