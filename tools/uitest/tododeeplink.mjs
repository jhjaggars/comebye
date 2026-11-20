// The #/todo/<id> link: it opens that one To do item, on a phone and on a desktop, on a cold load and from inside the app.
// An item that is answered, done or dropped opens read only with a note; an unknown id shows a notice card (not a toast);
// a decision set opens its set card; an item with a Page opens the split view. Collapsing an item the link opened takes the
// hash back to the plain page. On a phone the bare #/todo scrolls the To do card into view.
//
//   node tododeeplink.mjs BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME, WIDTHS)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("tododeeplink.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const tag0 = dark ? "dark" : "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "tododeeplink-ui-"));
const f = path.join(work, "dl.html");
fs.writeFileSync(f, `<!doctype html><html><head><meta charset="utf-8"><title>Deep link demo</title></head><body style="background:#e8f0ff;color:#222;font:32px system-ui"><h1>Deep link demo</h1></body></html>`);
const pid = execFileSync(bin, ["publish", f, "--key", "tododeeplink", "--title", "Deep link demo"], { env: { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w4:p1" }, encoding: "utf8" }).match(/pg_[0-9a-f]{10}/)[0];
const J = { "Content-Type": "application/json" };
const add = async (b) => { const j = await (await fetch(base + "/api/todos", { method: "POST", headers: J, body: JSON.stringify({ no_wait: true, kind: "decision", ...b }) })).json(); if (!j.todo) throw new Error("add: " + JSON.stringify(j)); return j.todo; };
const post = (id, what, body = {}) => fetch(base + `/api/todos/${id}/${what}`, { method: "POST", headers: J, body: JSON.stringify(body) });
const SET = { decisions: [
  { id: "D1", title: "Where", surface: "store", recommend: 1, options: [{ label: "One", short: "one" }, { label: "Two", short: "two" }] },
  { id: "D2", title: "When", surface: "rollout", recommend: 1, options: [{ label: "Now", short: "now" }, { label: "Later", short: "later" }] } ] };

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

for (const width of (process.env.WIDTHS || "390,1280").split(",").map(Number)) {
  const phone = width < 500, tag = `${width}px ${tag0}`;
  const LIST = phone ? "#todo" : "#p-todo";
  const ids = [];
  const { browser, page } = await launch({ width, height: phone ? 844 : 800, dark, bypassCSP: false });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(String(e)));
  const mk = async (b) => { const t = await add(b); ids.push(t.id); return t; };
  const item = (t) => `${LIST} .td-item[data-id="${t.id}"]`;
  const hash = () => page.evaluate(() => location.hash);
  const cold = async (h) => {
    await page.goto("about:blank");
    await page.goto(base + "/" + h, { waitUntil: "load" });
  };
  const openIs = (t) => until(page, ([sel]) => { const e = document.querySelector(sel); return !!e && e.classList.contains("open"); }, [item(t)]);
  const onlyOpen = async (t) => {
    const n = await page.locator(`${LIST} .td-item.open`).count();
    if (n !== 1) throw new Error("open items: " + n);
    const inView = await page.locator(item(t)).evaluate((e) => { const r = e.getBoundingClientRect(); return r.bottom > 0 && r.top < innerHeight; });
    if (!inView) throw new Error("the item is not in the viewport");
  };
  const noOverflow = () => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1);
  try {
    const a = await mk({ question: `Deep one ${width}?`, context: "ctx a" });
    const b = await mk({ question: `Deep two ${width}?`, context: "ctx b" });
    const ansd = await mk({ question: `Deep answered ${width}?`, context: "ctx c" });
    const done = await mk({ question: `Deep done ${width}?`, context: "ctx d" });
    const drop = await mk({ question: `Deep dropped ${width}?`, context: "ctx e" });
    const setItem = await mk({ question: `Deep set ${width}?`, set: SET });
    const withPage = await mk({ question: `Deep page ${width}?`, page: { id: pid }, context: "ctx f" });
    await post(ansd.id, "answer", { text: "the user said yes" });
    await post(done.id, "done");
    await post(drop.id, "drop", { reason: "not needed" });

    await check(`a cold load of #/todo/<id> opens that item, in view, the others collapsed: ${tag}`, async () => {
      await cold("#/todo/" + b.id);
      await openIs(b);
      await page.waitForTimeout(300);
      await onlyOpen(b);
      if (!(await page.locator(`${item(b)} .td-text`).isVisible())) throw new Error("no answer box");
      if (!(await noOverflow())) throw new Error("the page scrolls sideways");
      await page.screenshot({ path: `${out}/tododeeplink-${width}-${tag0}-cold.png` });
    });

    await check(`a slow first load still opens the item once the list arrives: ${tag}`, async () => {
      await page.route("**/api/todos", async (r) => { await new Promise((x) => setTimeout(x, 1500)); await r.continue(); });
      await cold("#/todo/" + a.id);
      await page.waitForTimeout(400);
      if (await page.locator(`${LIST} .td-item.open`).count()) throw new Error("opened before the list arrived");
      await openIs(a);
      await page.unroute("**/api/todos");
      await onlyOpen(a);
    });

    await check(`in-app navigation: a hash change and a clicked link open the item: ${tag}`, async () => {
      await cold(phone ? "" : "#/todo");
      await page.waitForSelector(`${LIST} .td-row`, { state: "visible" });
      await page.evaluate((id) => { location.hash = "#/todo/" + id; }, a.id);
      await openIs(a); await onlyOpen(a);
      await page.evaluate((id) => { location.hash = "#/todo/" + id; }, b.id);
      await openIs(b); await onlyOpen(b);
      if ((await page.locator(item(a) + ".open").count())) throw new Error("the first item stayed open");
      // A plain in-app link (<a href="#/todo/ID">, as a page or the board can make): a click goes through the same route.
      await page.evaluate((id) => { const a = document.createElement("a"); a.id = "dl-link"; a.href = "#/todo/" + id; a.textContent = "go"; a.style.cssText = "position:fixed;left:0;top:0;z-index:99999;background:#fff;padding:8px"; document.body.append(a); }, a.id);
      await page.locator("#dl-link").click();
      await openIs(a); await onlyOpen(a);
      await page.evaluate(() => document.getElementById("dl-link").remove());
    });

    await check(`collapsing the item the link opened takes the hash back; Back/reload do not reopen it: ${tag}`, async () => {
      await cold("#/todo/" + a.id);
      await openIs(a);
      await page.locator(`${item(a)} .td-row`).click();
      await until(page, ([s]) => !document.querySelector(s).classList.contains("open"), [item(a)]);
      const h = await hash();
      if (phone ? h !== "" : h !== "#/todo") throw new Error("hash after collapse: " + JSON.stringify(h));
      await page.reload({ waitUntil: "load" });
      await page.waitForSelector(`${LIST} .td-row`, { state: "visible" });
      await page.waitForTimeout(300);
      if (await page.locator(`${LIST} .td-item.open`).count()) throw new Error("a reload reopened it");
    });

    if (!phone) await check(`expanding an item on the desktop does not change the URL: ${tag}`, async () => {
      await cold("#/todo");
      await page.waitForSelector(`${item(a)} .td-row`, { state: "visible" });
      await page.locator(`${item(a)} .td-row`).click();
      await openIs(a);
      if ((await hash()) !== "#/todo") throw new Error("hash: " + await hash());
    });

    await check(`an answered item opens read only with the note, the answered group open: ${tag}`, async () => {
      await cold("#/todo/" + ansd.id);
      await openIs(ansd);
      await page.waitForSelector(`${item(ansd)} .td-deepnote`, { state: "visible" });
      const note = await page.locator(`${item(ansd)} .td-deepnote`).innerText();
      if (!/Already answered/.test(note)) throw new Error("note: " + note);
      if (await page.locator(`${item(ansd)} .td-answer-btn, ${item(ansd)} .td-text`).count()) throw new Error("answer controls on an answered item");
      if (!/the user said yes/.test(await page.locator(`${item(ansd)} .td-answer-t`).innerText())) throw new Error("the answer is not shown");
      if ((await page.locator(`${LIST} .td-g`).getAttribute("aria-expanded")) !== "true") throw new Error("the answered group is closed");
      await page.screenshot({ path: `${out}/tododeeplink-${width}-${tag0}-answered.png` });
    });

    await check(`a done item (not in the list) opens read only: ${tag}`, async () => {
      await cold("#/todo/" + done.id);
      await openIs(done);
      await page.waitForSelector(`${item(done)} .td-deepnote`, { state: "visible" });
      if (!/Done/.test(await page.locator(`${item(done)} .td-deepnote`).innerText())) throw new Error("note");
      if (await page.locator(`${item(done)} .td-answer-btn, ${item(done)} .td-done, ${item(done)} .td-drop, ${item(done)} .td-text`).count()) throw new Error("answer controls on a done item");
      await page.waitForTimeout(500);
      await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); }); // a poll keeps it while it is open
      await page.waitForTimeout(300);
      if (!(await page.locator(`${item(done)}.open`).count())) throw new Error("a poll dropped the item");
      await page.screenshot({ path: `${out}/tododeeplink-${width}-${tag0}-done.png` });
    });

    await check(`a dropped item shows the reason: ${tag}`, async () => {
      await cold("#/todo/" + drop.id);
      await openIs(drop);
      await page.waitForSelector(`${item(drop)} .td-deepnote`, { state: "visible" });
      const n = await page.locator(`${item(drop)} .td-deepnote`).innerText();
      if (!/Dropped: not needed/.test(n)) throw new Error("note: " + n);
    });

    for (const [name, h] of [["a well-formed id nobody has", "#/todo/t0000000000"], ["a malformed id", "#/todo/nonsense"]]) {
      await check(`${name}: a notice card, no toast, the hash cleaned, the list usable: ${tag}`, async () => {
        await cold(h);
        await page.waitForSelector(`${LIST} .td-notice`, { state: "visible" });
        const txt = await page.locator(`${LIST} .td-notice`).innerText();
        if (!/No to-do/.test(txt)) throw new Error("notice: " + txt);
        if (await page.evaluate(() => /show/.test(document.getElementById("toast").className))) throw new Error("a toast");
        const hh = await hash();
        if (phone ? hh !== "" : hh !== "#/todo") throw new Error("hash: " + JSON.stringify(hh));
        if (await page.locator(`${LIST} .td-item.open`).count()) throw new Error("an item is open");
        await page.screenshot({ path: `${out}/tododeeplink-${width}-${tag0}-unknown.png` });
        await page.locator(`${item(b)} .td-row`).click();
        await openIs(b);
        await page.locator(`${LIST} .td-notice-x`).click().catch(() => {});
      });
    }

    await check(`a decision set opens its set card: ${tag}`, async () => {
      await cold("#/todo/" + setItem.id);
      await openIs(setItem);
      await page.waitForSelector(`${item(setItem)} .ds-bar, ${item(setItem)} .ds-dec`, { state: "visible" });
      if (!(await page.locator(`${item(setItem)} .ds-dec, ${item(setItem)} .ds-rk`).count())) throw new Error("no decisions");
      await onlyOpen(setItem);
      await page.screenshot({ path: `${out}/tododeeplink-${width}-${tag0}-set.png` });
    });

    await check(`an item with a Page opens the split view; one Back leaves it: ${tag}`, async () => {
      await cold(phone ? "" : "#/todo");
      await page.waitForSelector(`${LIST} .td-row`, { state: "visible" });
      const len0 = await page.evaluate(() => history.length);
      await cold("#/todo/" + withPage.id);
      await page.waitForSelector("#rs", { state: "visible" });
      if (!(await page.locator(`#rs .td-item[data-id="${withPage.id}"]`).count())) throw new Error("the item is not in the split");
      await page.screenshot({ path: `${out}/tododeeplink-${width}-${tag0}-split.png` });
      await page.goBack();
      await until(page, () => { const r = document.getElementById("rs"); return !r || r.hidden; });
      if (!page.url().startsWith(base)) throw new Error("Back left the app: " + page.url());
      await page.waitForTimeout(300);
      if (await page.locator("#rs:not([hidden])").count()) throw new Error("the split came back");
    });

    if (phone) await check(`the bare #/todo on a phone scrolls the To do card into view: ${tag}`, async () => {
      await cold("");
      await page.waitForSelector("#todo .td-toggle", { state: "visible" });
      await page.evaluate(() => { window.scrollTo(0, 0); document.querySelector("#list")?.scrollTo?.(0, 0); });
      await page.evaluate(() => { location.hash = "#/todo"; });
      await until(page, () => { const r = document.getElementById("todo").getBoundingClientRect(); return r.top >= -2 && r.top < 120; }).catch(async () => {
        throw new Error("the card is at " + JSON.stringify(await page.evaluate(() => document.getElementById("todo").getBoundingClientRect().top)));
      });
    });

    if (errs.length) throw new Error("page errors: " + errs.join("; "));
  } catch (e) {
    results.push({ name: `tododeeplink ${tag}: ${e.message.split("\n")[0]}`, ok: false }); console.log("FAIL " + tag + " " + e.message);
  } finally {
    for (const id of ids) await post(id, "drop").catch(() => {});
    await browser.close();
  }
}
const bad = results.filter((r) => !r.ok).length;
console.log(`\n${results.length - bad}/${results.length} todo deep-link checks passed`);
process.exit(bad ? 1 : 0);
