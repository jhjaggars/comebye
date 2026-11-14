// An expanded To do item does not trap navigation: the header, the fleet menu, an agent, a Page link and browser Back all leave
// it, and the target opens. Two modes: an ordinary expanded item, and the split review mode (an item about a Page, with the Page
// beside it; a full-width card under the app's header on a phone). A typed, unsent answer is still there when you come back.
//
//   node todonav.mjs BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("todonav.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "todonav-ui-"));
const results = [];
const f = path.join(work, "nav.html");
fs.writeFileSync(f, `<!doctype html><html><head><meta charset="utf-8"><title>Nav demo</title></head><body style="background:#ffeedd;color:#222;font:32px system-ui"><h1>Nav demo</h1></body></html>`);
const pid = execFileSync(bin, ["publish", f, "--key", "todonav", "--title", "Nav demo"], { env: { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w4:p1" }, encoding: "utf8" }).match(/pg_[0-9a-f]{10}/)[0];
const J = { "Content-Type": "application/json" };
const add = async (b) => { const j = await (await fetch(base + "/api/todos", { method: "POST", headers: J, body: JSON.stringify({ no_wait: true, kind: "decision", ...b }) })).json(); if (!j.todo) throw new Error("add: " + JSON.stringify(j)); return j.todo; };
const drop = (id) => fetch(base + `/api/todos/${id}/drop`, { method: "POST", headers: J, body: "{}" });
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

// Each route: how to go, and what must then be showing.
const SCREEN = { pages: "#pages", usage: "#usage", settings: "#settings", search: "#recall" };
const ROUTES = (phone) => [
  ...["pages", "usage", "settings", "search"].map((k) => ({
    name: phone ? `header, fleet menu: ${k}` : `nav: ${k}`, screen: SCREEN[k],
    go: async (page) => {
      if (!phone) return page.locator(`#nav button[data-page="${k}"]`).click({ timeout: 4000 });
      if (k === "search") return page.locator("#btn-recall").click({ timeout: 4000 });
      await page.locator("#btn-fleet").click({ timeout: 4000 });
      await page.locator(`#f-${k}`).click({ timeout: 4000 });
    },
  })),
  { name: "an agent", agent: true, go: async (page) => {
    if (!phone) return page.locator("#agents .agent[data-id]").first().click({ timeout: 4000 });
    await page.evaluate(() => { location.hash = agentHash(state.agents[0]); }); // a notification tap or a link
  } },
  { name: "the status page", status: true, go: async (page) => {
    if (!phone) return page.locator('#nav button[data-page="status"]').click({ timeout: 4000 });
    await page.evaluate(() => { location.hash = "#/status"; });
  } },
];

for (const width of (process.env.WIDTHS || "390,1280").split(",").map(Number)) for (const mode of ["plain", "split"]) {
  const phone = width < 500, tag = `${mode} item at ${width}px ${dark ? "dark" : "light"}`;
  const LIST = phone ? "#todo" : "#p-todo";
  const ids = [];
  const { browser, page } = await launch({ width, height: phone ? 844 : 800, dark, bypassCSP: false });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(String(e)));
  const scope = mode === "split" ? "#rs" : LIST;
  const open = async (it) => {
    await page.goto("about:blank"); // a fresh load: the same URL again would not reload, and the new item would not be in the list
    await page.goto(base + (phone ? "/" : "/#/todo"), { waitUntil: "load" });
    if (phone) {
      await page.waitForSelector("#todo .td-toggle", { state: "visible", timeout: 15000 });
      if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
    } else await page.waitForSelector("#p-todo", { state: "visible" });
    await page.locator(`${LIST} .td-item[data-id="${it.id}"] .td-row`).click();
    await page.waitForSelector(`${scope} .td-item.open[data-id="${it.id}"]`, { state: "attached" });
    if (mode === "split" && phone) await page.locator('#rs [data-rs-tab="decide"]').click();
    await page.waitForSelector(`${scope} .td-item.open[data-id="${it.id}"]`, { state: "visible" });
    await page.locator(`${scope} .td-item[data-id="${it.id}"] .td-text`).fill("keep me");
  };
  try {
    const mk = async (q) => { const t = await add(mode === "split" ? { question: q, page: { id: pid }, context: "ctx" } : { question: q, context: "ctx" }); ids.push(t.id); return t; };
    for (const r of ROUTES(phone)) {
      await check(`${r.name} leaves an expanded item: ${tag}`, async () => {
        const it = await mk(`Nav ${r.name} ${mode} ${width}?`);
        await open(it);
        if (mode === "split" && (await page.locator("#rs").isHidden())) throw new Error("the split did not open");
        await page.screenshot({ path: `${out}/todonav-${mode}-${width}-${dark ? "dark" : "light"}-open.png` });
        await r.go(page);
        if (r.screen) await page.waitForSelector(`${r.screen}:not([hidden])`, { timeout: 5000 });
        if (r.agent) await page.waitForFunction(() => state.selected, null, { timeout: 5000 });
        if (r.status) await page.waitForFunction(() => location.hash === "#/status", null, { timeout: 5000 });
        await page.waitForTimeout(300);
        if (mode === "split") {
          if (await page.locator("#rs").isVisible()) throw new Error("the split is still up");
          if (await page.evaluate((id) => todos.openId === id, it.id)) throw new Error("the item is still open behind the target");
        }
        if (r.screen && !(await page.locator(r.screen).isVisible())) throw new Error("the target is not showing");
        await page.screenshot({ path: `${out}/todonav-${mode}-${width}-${dark ? "dark" : "light"}-${r.name.replace(/\W+/g, "_")}.png` });
        // Back to To do: the unsent answer is still in the box.
        await page.evaluate((p) => { location.hash = p ? "" : "#/todo"; }, phone);
        await page.waitForSelector(`${LIST} .td-item[data-id="${it.id}"] .td-row`, { state: "visible", timeout: 5000 });
        if (!(await page.locator(`${scope} .td-item.open[data-id="${it.id}"]`).count())) await page.locator(`${LIST} .td-item[data-id="${it.id}"] .td-row`).click();
        if (mode === "split" && phone) await page.locator('#rs [data-rs-tab="decide"]').click();
        const box = page.locator(`${scope} .td-item[data-id="${it.id}"] .td-text`);
        await box.waitFor({ state: "visible", timeout: 5000 });
        if ((await box.inputValue()) !== "keep me") throw new Error("the unsent answer is gone: " + JSON.stringify(await box.inputValue()));
      });
    }
    if (mode === "split") {
      await check(`the Page link leaves the split: ${tag}`, async () => {
        const it = await mk(`Nav page link ${width}?`);
        await open(it);
        await page.evaluate(() => { document.activeElement && document.activeElement.blur(); });
        await page.locator('#rs a.td-pglink:visible').first().click({ timeout: 4000 });
        await page.waitForFunction(() => /#\/pages\/pg_/.test(location.hash), null, { timeout: 5000 });
        await page.waitForTimeout(300);
        if (await page.locator("#rs").isVisible()) throw new Error("the split is still up over the Page");
        if (!(await page.locator("#pages").isVisible())) throw new Error("the Pages screen is not showing");
      });
      await check(`browser Back closes the split once and stays in the app: ${tag}`, async () => {
        const it = await mk(`Nav back ${width}?`);
        await open(it);
        await page.goBack();
        await page.waitForSelector("#rs", { state: "hidden", timeout: 5000 });
        if (!page.url().startsWith(base)) throw new Error("Back left the app: " + page.url());
      });
      await check(`a cold start with the Pages not yet loaded still has the one history entry: ${tag}`, async () => {
        const it = await mk(`Nav cold ${width}?`);
        await page.route(/\/api\/pages(\?|$)/, async (r) => { await new Promise((x) => setTimeout(x, 800)); r.continue(); });
        await open(it).catch(() => {});
        await page.waitForTimeout(1500);
        if (!(await page.evaluate(() => !!(history.state && history.state.rs)))) throw new Error("no history entry for the split");
        await page.unroute(/\/api\/pages(\?|$)/);
      });
    }
    if (errs.length) throw new Error("page errors: " + errs.join("; "));
  } catch (e) {
    results.push({ name: `todonav ${tag}: ${e.message.split("\n")[0]}`, ok: false }); console.log("FAIL " + tag + " " + e.message);
  } finally {
    for (const id of ids) await drop(id).catch(() => {});
    await browser.close();
  }
}
const bad = results.filter((r) => !r.ok).length;
console.log(`\n${results.length - bad}/${results.length} to do navigation checks passed`);
process.exit(bad ? 1 : 0);
