// The "Open session" button on a To do item: it opens the agent the item is about (the agent in Waiting, else the one that filed
// it, else the one running a handoff it links to) in the same detail view as the list does, with the prompt box focused, and
// leaves the item open and unanswered. No agent named: no button. An agent that has gone: the button is greyed with the reason,
// and a click says so. Phone and desktop, browser Back, the keyboard, a decision set, and the split view beside a Page.
//
// The agents are the server's own (/api/agents), not the test fleet's: the app drops a detail whose agent is not in its list.
//
//   node todoopen.mjs BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME, WIDTHS)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("todoopen.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const tag0 = dark ? "dark" : "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "todoopen-ui-"));
const f = path.join(work, "open.html");
fs.writeFileSync(f, `<!doctype html><html><head><meta charset="utf-8"><title>Open demo</title></head><body style="background:#ffeedd;color:#222;font:32px system-ui"><h1>Open demo</h1></body></html>`);
const pid = execFileSync(bin, ["publish", f, "--key", "todoopen", "--title", "Open demo"], { env: { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w4:p1" }, encoding: "utf8" }).match(/pg_[0-9a-f]{10}/)[0];
const J = { "Content-Type": "application/json" };
const add = async (b) => { const j = await (await fetch(base + "/api/todos", { method: "POST", headers: J, body: JSON.stringify({ no_wait: true, kind: "decision", ...b }) })).json(); if (!j.todo) throw new Error("add: " + JSON.stringify(j)); return j.todo; };
const get = async (id) => (await (await fetch(base + "/api/todos/" + id)).json()).todo;
const drop = (id) => fetch(base + `/api/todos/${id}/drop`, { method: "POST", headers: J, body: "{}" });
const live = (await (await fetch(base + "/api/agents")).json()).agents;
if (!live.length) { console.error("todoopen.mjs: the server has no agents"); process.exit(2); }
const AG = live[0]; // { id: "uitest/w1:p1", name, pane_id }
const GONE = "uitest/w97:p9";
const SET = { decisions: [
  { id: "D1", title: "Where", surface: "store", recommend: 1, options: [{ label: "One", short: "one" }, { label: "Two", short: "two" }] },
  { id: "D2", title: "When", surface: "rollout", recommend: 1, options: [{ label: "Now", short: "now" }, { label: "Later", short: "later" }] } ] };

// Polls a page function with evaluate: waitForFunction compiles its predicate with eval, which the app's CSP can refuse.
async function until(page, fn, arg, opts = {}) {
  const end = Date.now() + (opts.timeout || 8000);
  for (;;) {
    if (await page.evaluate(fn, arg)) return;
    if (Date.now() > end) throw new Error("timed out waiting for " + String(fn).slice(0, 120));
    await page.waitForTimeout(50);
  }
}
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

for (const width of (process.env.WIDTHS || "390,1280,3440").split(",").map(Number)) {
  const phone = width < 500, tag = `${width}px ${tag0}`;
  const LIST = phone ? "#todo" : "#p-todo";
  const ids = [];
  const { browser, page } = await launch({ width, height: phone ? 844 : width > 2000 ? 1440 : 800, dark, bypassCSP: false });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(String(e)));
  const mk = async (b) => { const t = await add(b); ids.push(t.id); return t; };
  const item = (t) => `${LIST} .td-item[data-id="${t.id}"]`;
  const load = async () => {
    await page.goto("about:blank");
    await page.goto(base + (phone ? "/" : "/#/todo"), { waitUntil: "load" });
    if (phone) {
      await page.waitForSelector("#todo .td-toggle", { state: "visible", timeout: 15000 });
      if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
    } else await page.waitForSelector("#p-todo", { state: "visible" });
    await until(page, () => state.agents.length > 0);
  };
  // Back at the To do list with the item expanded.
  const openItem = async (t, fresh) => {
    if (fresh) await load();
    else { await page.evaluate((p) => { location.hash = p ? "" : "#/todo"; }, phone); await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); }); }
    await page.waitForSelector(`${item(t)} .td-row`, { state: "visible" });
    if (!(await page.locator(`${item(t)}.open`).count())) await page.locator(`${item(t)} .td-row`).click();
    await page.waitForSelector(`${item(t)}.open .td-panel`, { state: "visible" });
  };
  const noOverflow = () => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1);
  try {
    const withAgent = await mk({ question: `Open with an agent ${width}?`, context: "ctx", waiting: AG.id });
    const noAgent = await mk({ question: `Open without an agent ${width}?`, context: "ctx" });
    const goneAgent = await mk({ question: `Open with a gone agent ${width}?`, context: "ctx", waiting: GONE });
    const setItem = await mk({ question: `Open on a set ${width}?`, waiting: AG.id, set: SET });
    let first = true;

    await check(`an item with an agent has the button, enabled, in the action row: ${tag}`, async () => {
      await openItem(withAgent, first); first = false;
      const b = page.locator(`${item(withAgent)} .td-acts .td-open`);
      if ((await b.count()) !== 1) throw new Error("buttons: " + await b.count());
      if ((await b.innerText()).trim() !== "Open session") throw new Error("label " + await b.innerText());
      if ((await b.getAttribute("aria-disabled")) === "true") throw new Error("disabled");
      const bb = await b.boundingBox();
      if (!bb || bb.x < 0 || bb.x + bb.width > width + 0.5) throw new Error("outside the viewport: " + JSON.stringify(bb));
      // Not on top of its neighbours.
      const rects = await page.locator(`${item(withAgent)} .td-acts button`).evaluateAll((bs) => bs.map((x) => { const r = x.getBoundingClientRect(); return [x.className, r.x, r.y, r.width, r.height]; }));
      for (const a of rects) for (const c of rects) if (a !== c && a[1] < c[1] + c[3] - 1 && c[1] < a[1] + a[3] - 1 && a[2] < c[2] + c[4] - 1 && c[2] < a[2] + a[4] - 1) throw new Error(`${a[0]} overlaps ${c[0]}`);
      if (!(await noOverflow())) throw new Error("the page scrolls sideways");
      await page.screenshot({ path: `${out}/todoopen-${width}-${tag0}-item.png` });
    });

    await check(`Open session opens the agent with the prompt box focused; the item stays open; Back returns to it: ${tag}`, async () => {
      await openItem(withAgent);
      await page.locator(`${item(withAgent)} .td-open`).click();
      await until(page, (h) => location.hash === h, "#/agent/" + encodeURIComponent(AG.pane_id));
      await until(page, (id) => state.selected === id, AG.id);
      await page.waitForSelector("#detail", { state: "visible" });
      await until(page, () => document.activeElement && document.activeElement.id === "prompt-text", null, { timeout: 4000 }).catch(async () => {
        throw new Error("the prompt box is not focused: " + await page.evaluate(() => [document.activeElement && (document.activeElement.id || document.activeElement.className), document.hasFocus(), location.hash].join(" ")));
      });
      await page.screenshot({ path: `${out}/todoopen-${width}-${tag0}-agent.png` });
      const t = await get(withAgent.id);
      if (t.state !== "open" || t.answer) throw new Error("the item changed: " + JSON.stringify([t.state, t.answer]));
      await page.goBack();
      await until(page, () => !location.hash.startsWith("#/agent/"));
      await page.waitForSelector(`${item(withAgent)}.open .td-open`, { state: "visible", timeout: 5000 });
      if (await page.evaluate(() => !!state.selected)) throw new Error("the detail is still selected after Back");
      if (!page.url().startsWith(base)) throw new Error("Back left the app: " + page.url());
    });

    await check(`the keyboard reaches the button and Enter opens the session: ${tag}`, async () => {
      await openItem(withAgent);
      const b = page.locator(`${item(withAgent)} .td-open`);
      await b.focus();
      await page.keyboard.press("Shift+Tab"); await page.keyboard.press("Tab"); // keyboard modality, so :focus-visible applies
      if (!(await b.evaluate((e) => e === document.activeElement))) throw new Error("not reached by Tab");
      if (!(await page.evaluate(() => { const s = getComputedStyle(document.activeElement); return s.outlineStyle !== "none" || s.boxShadow !== "none"; }))) throw new Error("no focus ring");
      await page.keyboard.press("Enter");
      await until(page, (id) => state.selected === id, AG.id);
      await until(page, () => document.activeElement && document.activeElement.id === "prompt-text", null, { timeout: 4000 });
      await page.goBack();
      await until(page, () => !location.hash.startsWith("#/agent/"));
    });

    await check(`an item that names no agent has no button: ${tag}`, async () => {
      await openItem(noAgent);
      if (await page.locator(`${item(noAgent)} .td-open`).count()) throw new Error("a button on an item with no agent");
    });

    await check(`an item whose agent has gone: the button is greyed with the reason; a click says so and opens nothing: ${tag}`, async () => {
      await openItem(goneAgent);
      const b = page.locator(`${item(goneAgent)} .td-open`);
      if ((await b.count()) !== 1) throw new Error("no button");
      if ((await b.getAttribute("aria-disabled")) !== "true") throw new Error("not disabled");
      if (!/no longer running/.test((await b.getAttribute("title")) || "")) throw new Error("tooltip " + (await b.getAttribute("title")));
      const h = await page.evaluate(() => location.hash);
      await b.click({ force: true }); // aria-disabled, not disabled: Playwright would wait for it to be enabled
      await until(page, () => /no longer running/.test(document.getElementById("toast").textContent));
      if ((await page.evaluate(() => location.hash)) !== h) throw new Error("the hash moved");
      if (await page.evaluate(() => !!state.selected)) throw new Error("a detail opened");
      if (!(await page.locator(`${item(goneAgent)}.open`).count())) throw new Error("the item closed");
      await page.screenshot({ path: `${out}/todoopen-${width}-${tag0}-gone.png` });
    });

    await check(`an agent that goes between the draw and the click: a message, not a broken view: ${tag}`, async () => {
      await openItem(withAgent);
      await page.evaluate(() => { document.getElementById("toast").className = ""; document.getElementById("toast").textContent = ""; });
      await page.locator(`${item(withAgent)} .td-open`).evaluate((b) => { b.dataset.agent = "uitest/w96:p9"; });
      await page.locator(`${item(withAgent)} .td-open`).click();
      await until(page, () => /no longer running/.test(document.getElementById("toast").textContent));
      if (await page.evaluate(() => !!state.selected)) throw new Error("a detail opened");
    });

    await check(`a decision set has the button in its bar: ${tag}`, async () => {
      await openItem(setItem);
      const b = page.locator(`${item(setItem)} .ds-bar .td-open`);
      if ((await b.count()) !== 1) throw new Error("buttons: " + await b.count());
      const bb = await b.boundingBox();
      if (!bb || bb.x < 0 || bb.x + bb.width > width + 0.5) throw new Error("outside the viewport");
      if (!(await noOverflow())) throw new Error("the page scrolls sideways");
      await page.screenshot({ path: `${out}/todoopen-${width}-${tag0}-set.png` });
      await b.click();
      await until(page, (id) => state.selected === id, AG.id);
      const t = await get(setItem.id);
      if (t.state !== "open" || (t.set && t.set.submitted_at)) throw new Error("the set changed");
      await page.goBack();
      await until(page, () => !location.hash.startsWith("#/agent/"));
    });

    await check(`the agent is found from who filed it, or from a handoff it links to: ${tag}`, async () => {
      const r = await page.evaluate((ag) => {
        const keep = ledger.entries;
        ledger.entries = [{ id: "h-test", agent_id: ag.id, name: "x" }];
        const o = {
          filed: todoSessionAgent({ created_by_id: ag.id, created_by: "somebody" }),
          goneWaitingFiledLive: todoSessionAgent({ waiting: { id: "uitest/w95:p1", name: "old", gone: true }, created_by_id: ag.id }),
          handoff: todoSessionAgent({ links: [{ handoff: "h-test" }] }),
          none: todoSessionAgent({ links: [{ label: "x", url: "https://example.com" }] }),
          goneOnly: todoSessionAgent({ waiting: { id: "uitest/w95:p1", name: "old", gone: true } }),
        };
        ledger.entries = keep;
        return o;
      }, AG);
      for (const k of ["filed", "goneWaitingFiledLive", "handoff"]) if (!r[k] || !r[k].live || r[k].id !== AG.id) throw new Error(`${k}: ${JSON.stringify(r[k])}`);
      if (r.none !== null) throw new Error("none: " + JSON.stringify(r.none));
      if (!r.goneOnly || r.goneOnly.live || !/no longer running/.test(r.goneOnly.why)) throw new Error("goneOnly: " + JSON.stringify(r.goneOnly));
    });

    await check(`beside a Page (the split view): the button opens the agent and closes the split: ${tag}`, async () => {
      const sp = await mk({ question: `Open beside a page ${width}?`, page: { id: pid }, context: "ctx", waiting: AG.id });
      await page.evaluate((p) => { location.hash = p ? "" : "#/todo"; }, phone);
      await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
      await page.waitForSelector(`${item(sp)} .td-row`, { state: "visible" });
      await page.locator(`${item(sp)} .td-row`).click();
      await page.waitForSelector("#rs", { state: "visible" });
      if (phone) await page.locator('#rs [data-rs-tab="decide"]').click();
      const b = page.locator(`#rs .td-item[data-id="${sp.id}"] .td-open`);
      await b.waitFor({ state: "visible" });
      await page.screenshot({ path: `${out}/todoopen-${width}-${tag0}-split.png` });
      await b.click();
      await until(page, (id) => state.selected === id, AG.id);
      await until(page, () => document.activeElement && document.activeElement.id === "prompt-text", null, { timeout: 4000 });
      if (await page.locator("#rs").isVisible()) throw new Error("the split is still up over the agent");
      const t = await get(sp.id);
      if (t.state !== "open") throw new Error("the item changed");
      await page.goBack();
      await until(page, () => !location.hash.startsWith("#/agent/"));
      if (!page.url().startsWith(base)) throw new Error("Back left the app");
    });

    if (errs.length) throw new Error("page errors: " + errs.join("; "));
  } catch (e) {
    results.push({ name: `todoopen ${tag}: ${e.message.split("\n")[0]}`, ok: false }); console.log("FAIL " + tag + " " + e.message);
  } finally {
    for (const id of ids) await drop(id).catch(() => {});
    await browser.close();
  }
}
const bad = results.filter((r) => !r.ok).length;
console.log(`\n${results.length - bad}/${results.length} open-session checks passed`);
process.exit(bad ? 1 : 0);
