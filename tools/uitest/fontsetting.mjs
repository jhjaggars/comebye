// The Font setting (web/themes.js FONTS, <html data-font>, Settings > Appearance): two choices, Sans (the default,
// the system sans stack) and Serif (one system serif stack for the reading text; code stays monospace; the key bar, nav tabs and fields stay sans). At 390, 1280 and 3440 in the run's colour scheme: each choice's computed font-family on a
// conversation reply, a To do body, the briefing's Markdown and a Markdown Page (its own frame); the control is a
// two-radio row in Settings > Appearance, operated by keyboard; the stored value is on <html> by DOMContentLoaded;
// every old stored value migrates (serif-heads and serif-reading -> serif, geist and unknown -> sans); another tab's
// change arrives by the storage event; no font file is fetched; the fleet menu's Font entry toggles.
//
//   node fontsetting.mjs BASE OUT_DIR      (env: SCHEME, COMEBYE_BIN, COMEBYE_STATE_DIR)
import { launch } from "./lib.mjs";
import { U, A } from "./fixtures/toolruns.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("fontsetting.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const results = [];
const log = [];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const same = (got, want, what) => { if (got !== want) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };

const SANS = /^-apple-system, "Segoe UI"/;
const SERIF = /^"Iowan Old Style", Charter/;
const MONO = /^ui-monospace/;
const CTX = "## A heading\n\nSome **bold** and *italic* reading text with `inline code` that wraps over a couple of lines.\n\n```sh\nls -la /etc\n```\n";
const MD = "# A serif page\n\nSome **bold** and *italic* text with `inline code`.\n\n```sh\nls -la\n```\n";
const todo = await fetch(base + "/api/todos", { method: "POST", headers: { "content-type": "application/json" },
  body: JSON.stringify({ question: "Which font reads better? " + Date.now(), kind: "decision", no_wait: true, context: CTX }) }).then((r) => r.json());
const tid = todo.todo.id;
const work = fs.mkdtempSync(path.join(os.tmpdir(), "fontsetting-ui-"));
const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w5:p1" };
const mdFile = path.join(work, "serif.md");
fs.writeFileSync(mdFile, MD);
const pageUrl = execFileSync(bin, ["publish", mdFile, "--key", "fontsetting-" + Date.now()], { env, encoding: "utf8" }).trim();
const pageId = (pageUrl.match(/pg_[0-9a-z]+/) || [])[0];
if (!pageId) throw new Error("no page id in: " + pageUrl);

const MSGS = [U("u1", "Which font?"), A("a1", "Here is **bold**, *italic* and `code`.\n\n```go\nfunc F() {}\n```\n\nA second paragraph of the reply.")];
async function session(width) {
  const { browser, ctx, page } = await launch({ width, height: width < 500 ? 844 : 900, dark, bypassCSP: false });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  page.on("console", (m) => { if (/Content Security Policy|violat|Refused/i.test(m.text())) log.push("csp: " + m.text()); });
  await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: MSGS, total: MSGS.length, more: false, format: "claude" } }));
  await page.addInitScript(() => {
    document.addEventListener("DOMContentLoaded", () => { window.__fontAtDCL = document.documentElement.getAttribute("data-font"); });
  });
  return { browser, ctx, page };
}
// page.waitForFunction evals a string, which the app's CSP forbids: poll with evaluate instead.
async function until(page, fn, what) {
  for (let i = 0; i < 60; i++) { if (await page.evaluate(fn)) return; await page.waitForTimeout(50); }
  throw new Error("timed out waiting for " + what);
}
const attr = (page) => page.evaluate(() => document.documentElement.getAttribute("data-font"));
const stored = (page) => page.evaluate(() => localStorage.getItem("comebye.font") || null); // "" is how the default is saved
const fam = (page, sel) => page.evaluate((sel) => { const el = document.querySelector(sel); return el ? getComputedStyle(el).fontFamily : null; }, sel);
const woff = (page) => page.evaluate(() => performance.getEntriesByType("resource").map((e) => e.name).filter((n) => /\.(woff2?|ttf|otf)/.test(n)));
async function setStored(page, v) {
  await page.goto(base + "/#/settings", { waitUntil: "load" });
  await page.evaluate((v) => { if (v === null) localStorage.removeItem("comebye.font"); else localStorage.setItem("comebye.font", v); }, v);
  await page.goto("about:blank");
}
async function openSettings(page) {
  await page.goto(base + "/#/settings", { waitUntil: "load" });
  await page.waitForSelector("#sx-font-default");
}
async function openTodo(page) {
  await page.goto(base + "/#/todo", { waitUntil: "load" });
  await page.waitForSelector(".td-item", { state: "attached" });
  if (!(await page.locator(`.td-item[data-id="${tid}"].open .td-ctx:visible`).count())) await page.locator(`.td-item[data-id="${tid}"] > .td-row:visible`).first().click();
  await page.locator(`.td-item[data-id="${tid}"] .td-ctx:visible`).first().waitFor();
}
async function openChat(page) {
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#chat .msg.assistant");
}
const pageFam = async (page) => {
  await page.goto(base + "/#/pages/" + pageId, { waitUntil: "load" });
  let fr = null;
  for (let i = 0; i < 100 && !fr; i++) { fr = page.frames().find((f) => f.url().includes("/p/" + pageId)); if (!fr) await page.waitForTimeout(50); }
  if (!fr) throw new Error("the Page's frame never appeared");
  await fr.waitForSelector("#out h1");
  await page.waitForTimeout(250);
  return fr.evaluate(() => { const f = (s) => getComputedStyle(document.querySelector(s)).fontFamily; return { body: f("#out p"), h1: f("#out h1"), code: f("#out pre code") }; });
};

for (const width of [390, 1280, 3440]) {
  const tag = (dark ? "dark" : "light") + " " + width;
  const { browser, page } = await session(width);
  await check(`${tag}: Appearance has a two-radio Font row, no picker panel, Sans checked by default`, async () => {
    await setStored(page, null);
    await openSettings(page);
    same(await page.locator("#sx-view [role=radiogroup][aria-labelledby=sx-font-lbl] [role=radio]").count(), 2, "radios");
    same(await page.locator("#sx-font-default").getAttribute("aria-checked"), "true", "Sans checked");
    same(await page.locator("#font-panel").count(), 0, "no font panel");
    same(await page.locator("#sx-row-font").count(), 0, "no font row");
    same(await attr(page), null, "data-font");
  });
  for (const o of [{ id: "", name: "Sans", re: SANS }, { id: "serif", name: "Serif", re: SERIF }]) {
    await check(`${tag}: ${o.name}: reply, To do body, briefing Markdown and a Markdown Page; code mono; key bar sans; persists`, async () => {
      await setStored(page, o.id || null);
      await openChat(page);
      same(await page.evaluate(() => window.__fontAtDCL), o.id || null, "data-font by DOMContentLoaded");
      const want = (what, got, re) => { if (!re.test(got || "")) throw new Error(`${what}: ${got}`); };
      want("reply", await fam(page, "#chat .msg.assistant .md p"), o.re);
      want("reply code", await fam(page, "#chat .msg.assistant .md pre code"), MONO);
      want("reply inline code", await fam(page, "#chat .msg.assistant .md p code"), MONO);
      want("composer", await fam(page, "#detail .prompt .composer"), o.re);
      await page.screenshot({ path: `${out}/font-${o.name.toLowerCase()}-chat-${width}.png` });
      await openTodo(page);
      want("To do body", await fam(page, ".td-item.open .td-ctx"), o.re);
      want("To do body code", await fam(page, ".td-item.open .td-ctx pre code"), MONO);
      want("To do question", await fam(page, ".td-item.open .td-q"), o.re);
      want("header title", await fam(page, "header h1"), o.re);
      want("key bar", await fam(page, ".keybar"), SANS);
      await page.screenshot({ path: `${out}/font-${o.name.toLowerCase()}-todo-${width}.png` });
      const p = await pageFam(page);
      want("Markdown Page body", p.body, o.id ? o.re : /^system-ui/); want("Markdown Page h1", p.h1, o.id ? o.re : /^system-ui/); want("Markdown Page code", p.code, /mono/);
      await page.screenshot({ path: `${out}/font-${o.name.toLowerCase()}-page-${width}.png` });
      same((await woff(page)).length, 0, "font files fetched");
      same(await stored(page), o.id || null, "stored");
      await page.reload({ waitUntil: "load" });
      same(await attr(page), o.id || null, "data-font after reload");
    });
  }
  await check(`${tag}: picking Serif then Sans in Settings by click saves and applies; the page follows without a reload`, async () => {
    await setStored(page, null);
    await openSettings(page);
    await page.locator("#sx-font-serif").click();
    same(await attr(page), "serif", "applied"); same(await stored(page), "serif", "stored");
    same(await page.locator("#sx-font-serif").getAttribute("aria-checked"), "true", "Serif checked");
    if (!SERIF.test(await fam(page, "#sx-view .sx-note"))) throw new Error("the settings text did not follow");
    await page.screenshot({ path: `${out}/font-settings-serif-${width}.png` });
    await page.locator("#sx-font-default").click();
    same(await attr(page), null, "applied"); same(await stored(page), null, "stored");
  });
  await check(`${tag}: keyboard: arrows move and select within the radiogroup, Home/End jump, Tab leaves it`, async () => {
    await setStored(page, null);
    await openSettings(page);
    await page.locator("#sx-font-default").focus();
    await page.keyboard.press("ArrowRight");
    same(await page.evaluate(() => document.activeElement.id), "sx-font-serif", "focus after ArrowRight");
    same(await attr(page), "serif", "ArrowRight selects Serif");
    await page.keyboard.press("ArrowRight");
    same(await page.evaluate(() => document.activeElement.id), "sx-font-default", "wraps to Sans");
    same(await attr(page), null, "Sans again");
    await page.keyboard.press("End");
    same(await attr(page), "serif", "End");
    await page.keyboard.press("Home");
    same(await attr(page), null, "Home");
    same(await page.locator("#sx-font-serif").getAttribute("tabindex"), "-1", "roving tabindex");
    await page.locator("#sx-font-default").focus();
    await page.keyboard.press("Tab");
    const id = await page.evaluate(() => document.activeElement.id);
    if (id === "sx-font-serif" || id === "sx-font-default") throw new Error("Tab stayed in the group: " + id);
  });
  await check(`${tag}: fleet menu Font entry toggles Sans/Serif and shows the value`, async () => {
    await setStored(page, null);
    await page.goto(base + "/", { waitUntil: "load" });
    await page.locator("#btn-fleet").click();
    same((await page.locator("#f-font-val").textContent()).trim(), "Sans", "menu value");
    await page.locator("#f-font").click();
    same(await attr(page), "serif", "toggled to serif");
    await page.locator("#btn-fleet").click();
    same((await page.locator("#f-font-val").textContent()).trim(), "Serif", "menu value");
    await page.locator("#f-font").click();
    same(await attr(page), null, "toggled back");
    same(await page.locator("#font-panel").count(), 0, "no panel");
  });
  await check(`${tag}: another tab's change arrives by the storage event (page, Settings radios, menu)`, async () => {
    await setStored(page, null);
    await openSettings(page);
    const other = await page.context().newPage();
    await other.goto(base + "/#/settings", { waitUntil: "load" });
    await other.evaluate(() => localStorage.setItem("comebye.font", "serif"));
    await until(page, () => document.documentElement.getAttribute("data-font") === "serif", "serif applied");
    await until(page, () => document.getElementById("sx-font-serif")?.getAttribute("aria-checked") === "true", "Serif radio");
    await other.evaluate(() => localStorage.removeItem("comebye.font"));
    await until(page, () => document.documentElement.getAttribute("data-font") === null, "sans applied");
    await until(page, () => document.getElementById("sx-font-default")?.getAttribute("aria-checked") === "true", "Sans radio");
    await other.close();
  });
  await check(`${tag}: a Markdown Page open in the viewer follows a change at once`, async () => {
    await setStored(page, null);
    const p0 = await pageFam(page);
    if (!SANS.test(p0.body) && !/system-ui/.test(p0.body)) throw new Error("not sans at first: " + p0.body);
    await page.evaluate(() => { localStorage.setItem("comebye.font", "serif"); window.dispatchEvent(new StorageEvent("storage", { key: "comebye.font" })); });
    const fr = page.frames().find((f) => f.url().includes("/p/" + pageId));
    let got = "";
    for (let i = 0; i < 40 && !SERIF.test(got); i++) { await page.waitForTimeout(50); got = await fr.evaluate(() => getComputedStyle(document.querySelector("#out p")).fontFamily); }
    if (!SERIF.test(got)) throw new Error("the Page stayed " + got);
  });
  await page.evaluate(() => localStorage.removeItem("comebye.font"));
  await browser.close();
}

// Migration: every value an older build could have stored.
{
  const { browser, page } = await session(1280);
  for (const [old, want] of [["serif-heads", "serif"], ["serif-reading", "serif"], ["geist", null], ["", null], ["comic-sans", null], ["SERIF", null], ["serif", "serif"]]) {
    await check(`migration: stored ${JSON.stringify(old)} shows ${want ? "Serif" : "Sans"}`, async () => {
      await setStored(page, old);
      await openSettings(page);
      same(await attr(page), want, "data-font");
      same(await page.locator("#sx-font-" + (want || "default")).getAttribute("aria-checked"), "true", "radio");
      same(await stored(page), want, "stored value rewritten clean");
    });
  }
  await check("migration: the other Appearance values and Reset to defaults leave the font alone", async () => {
    await setStored(page, "serif-reading");
    await openSettings(page);
    await page.locator("#sx-wrap-on").click();
    await page.locator("#sx-reset").click();
    same(await attr(page), "serif", "font stays after Reset");
    same(await stored(page), "serif", "stored after Reset");
  });
  await page.evaluate(() => localStorage.removeItem("comebye.font"));
  await browser.close();
}

await fetch(base + `/api/todos/${tid}/drop`, { method: "POST", headers: { "content-type": "application/json" }, body: "{}" }).catch(() => {});
fs.rmSync(work, { recursive: true, force: true });
const bad = results.filter((r) => !r).length + (log.length ? 1 : 0);
if (log.length) console.log("page errors:\n" + log.join("\n"));
console.log(`${results.filter(Boolean).length}/${results.length} fontsetting checks passed`);
process.exit(bad ? 1 : 0);
