// The review card in the To do panel (internal/server/review.go): a kind "review" item about a Page shows the
// hub's thumbnail of that version, what to try and two buttons. Looks good closes at once with an Undo toast
// driven by the hub's undo_until; Needs changes takes a kind and a note and files a Fix item; keys 1 and 2 on
// desktop. Pages are published with the real CLI so the thumbnails are Chrome's own.
//
//   node reviewcard.mjs BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("reviewcard.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "reviewcard-ui-"));
const results = [];
const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w4:p1" };
const publish = (key, title, noThumb) => {
  const f = path.join(work, key + ".html");
  fs.writeFileSync(f, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>${title}</title></head><body style="background:#ffeedd;color:#222;font:32px system-ui;margin:24px"><h1>${title}</h1><p>Each card shows a picture of its page.</p></body></html>`);
  const e = noThumb ? { ...env, COMEBYE_NO_THUMBNAILS: "1" } : env;
  return execFileSync(bin, ["publish", f, "--key", key, "--title", title], { env: e, encoding: "utf8" }).match(/pg_[0-9a-f]{10}/)[0];
};
const J = { "Content-Type": "application/json" };
const addReview = async (q, page, ctx) => {
  const r = await fetch(base + "/api/todos", { method: "POST", headers: J, body: JSON.stringify({ question: q, kind: "review", no_wait: true, context: ctx || "Open Pages: each card shows a picture of its page.\n\n- Publish a new revision: the picture updates.", page: { id: page } }) });
  const j = await r.json();
  if (!j.todo) throw new Error("add review: " + JSON.stringify(j));
  return j.todo;
};
const get = async (id) => (await fetch(base + "/api/todos/" + id).then((r) => r.json())).todo;
const all = async () => (await fetch(base + "/api/todos?state=all").then((r) => r.json())).todos;
const drop = (id) => fetch(base + `/api/todos/${id}/drop`, { method: "POST", headers: J, body: "{}" });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
async function until(fn, what, ms = 8000) {
  for (const end = Date.now() + ms; Date.now() < end; await sleep(150)) { const v = await fn(); if (v) return v; }
  throw new Error("timed out waiting for " + what);
}

const withPic = publish("rc-pic", "Gallery thumbnails demo"), noPic = publish("rc-none", "Plain demo", true);

for (const width of [1280, 390]) {
  const tag = `${width}px ${dark ? "dark" : "light"}`;
  const L = width > 500 ? "#p-todo" : "#todo";
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark, bypassCSP: false });
  const errs = [];
  page.on("pageerror", (e) => errs.push(String(e)));
  const ids = [];
  const item = (id) => page.locator(`${L} .td-item[data-id="${id}"]`);
  const open = async (id) => {
    await page.evaluate(async () => { todos.loadedAt = 0; todos.openId = null; await loadTodos(); renderTodos(true); });
    await page.waitForSelector(`${L} .td-item[data-id="${id}"] .td-row`, { state: "visible" });
    if (!(await item(id).evaluate((e) => e.classList.contains("open")))) await item(id).locator(".td-row").click();
    await page.waitForSelector(`${L} .td-item.open[data-id="${id}"] .rv-bar`);
  };
  try {
    const r1 = await addReview(`Review: Gallery thumbnails ${width}`, withPic); ids.push(r1.id);
    if (width > 500) { await page.goto(base + "/#/todo", { waitUntil: "load" }); await page.waitForSelector("#p-todo", { state: "visible" }); }
    else {
      await page.goto(base + "/", { waitUntil: "load" });
      await page.waitForSelector("#todo .td-toggle", { state: "visible", timeout: 15000 });
      if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
    }
    await check(`review card at ${tag}: thumbnail, what to try, two buttons, where it goes`, async () => {
      await open(r1.id);
      const g = await page.evaluate(async (sel) => {
        const it = document.querySelector(sel);
        const im = it.querySelector("img.rv-img");
        if (im) for (let i = 0; i < 60 && !(im.complete && im.naturalWidth); i++) await new Promise((r) => setTimeout(r, 100));
        return { src: im && im.getAttribute("src"), w: im && im.naturalWidth, text: it.innerText, btns: [...it.querySelectorAll(".rv-btns button")].map((b) => ({ t: b.innerText, h: b.getBoundingClientRect().height })),
          open: !!it.querySelector("a.rv-open"), noAnswerBox: !it.querySelector(".td-answer-btn"), scroll: document.documentElement.scrollWidth > innerWidth };
      }, `${L} .td-item[data-id="${r1.id}"]`);
      const want = dark ? "dark" : "light";
      if (!g.src || !g.src.includes(`scheme=${want}`) || !g.w) throw new Error("thumbnail: " + JSON.stringify(g.src) + " w=" + g.w);
      if (!/what to try/i.test(g.text) || !/Gallery thumbnails demo/i.test(g.text) || !/Open demo/i.test(g.text)) throw new Error("text: " + g.text.slice(0, 300));
      if (g.btns.length !== 2 || !/Looks good/.test(g.btns[0].t) || !/Needs changes/.test(g.btns[1].t)) throw new Error("buttons: " + JSON.stringify(g.btns));
      if (g.btns.some((b) => b.h < 44 - 0.5)) throw new Error("buttons under 44px: " + JSON.stringify(g.btns));
      if (!/goes to|no longer running/i.test(g.text)) throw new Error("no routing line: " + g.text.slice(0, 400));
      if (!g.noAnswerBox) throw new Error("the free-text Answer button is on a review");
      if (g.scroll) throw new Error("page scrolls sideways");
      await page.screenshot({ path: `${out}/reviewcard-${width}-${dark ? "dark" : "light"}.png` });
    });
    await check(`review card at ${tag}: the row says review; a page with no thumbnail shows a placeholder and the link`, async () => {
      const r2 = await addReview(`Review: Plain demo ${width}`, noPic); ids.push(r2.id);
      await open(r2.id);
      const t = await item(r2.id).innerText();
      if (!/no preview yet/i.test(t) || !/Open demo/i.test(t)) throw new Error(t.slice(0, 300));
      if (await item(r2.id).locator("img.rv-img").count()) throw new Error("an image with no thumbnail");
      const kind = await item(r2.id).locator(".td-row .td-kind").innerText();
      if (!/review/i.test(kind)) throw new Error("kind label: " + kind);
      const pl = await item(r2.id).evaluate((e, id) => {
        const a = [...e.querySelectorAll(".td-links a, .td-dlinks a")].find((x) => x.offsetParent);
        return { first: a && a.innerText, href: a && a.getAttribute("href"), n: e.querySelectorAll('a[href^="/#/pages/"].td-pglink').length, ids: (e.innerText.match(/\bt[0-9a-f]{10}\b/g) || []).length, cp: e.querySelectorAll("[aria-label='Copy item id']").length };
      }, r2.id);
      if (!/^Page: Plain demo/.test(pl.first || "") || !/^\/#\/pages\/pg_[0-9a-f]{10}$/.test(pl.href || "") || pl.n < 1) throw new Error("page link: " + JSON.stringify(pl));
      if (pl.ids || pl.cp) throw new Error("item id still shown: " + JSON.stringify(pl));
      await page.screenshot({ path: `${out}/reviewcard-nopic-${width}-${dark ? "dark" : "light"}.png` });
      await item(r2.id).locator("a.td-pglink:visible").first().click();
      await page.waitForFunction(() => /#\/pages\/pg_/.test(location.hash), null, { timeout: 5000 });
      await page.goto(base + (width > 500 ? "/#/todo" : "/"), { waitUntil: "load" });
      await drop(r2.id);
    });
    await check(`review card at ${tag}: Looks good closes it with an Undo toast; Undo opens it again`, async () => {
      await open(r1.id);
      await item(r1.id).locator(".rv-good").click();
      const t = await until(async () => { const x = await get(r1.id); return x.state === "done" ? x : null; }, "the review to close");
      if (!t.answer || t.answer.via !== "verdict" || !t.review.verdict || t.review.verdict.verdict !== "good" || !t.review.verdict.undo_until) throw new Error(JSON.stringify(t.review));
      await page.waitForSelector("#toast.show.act .toast-act", { timeout: 3000 });
      await page.screenshot({ path: `${out}/reviewcard-undo-${width}-${dark ? "dark" : "light"}.png` });
      await page.locator("#toast .toast-act").click();
      const u = await until(async () => { const x = await get(r1.id); return x.state === "open" ? x : null; }, "Undo to reopen it");
      if (u.answer) throw new Error("answer still there after Undo");
    });
    await check(`review card at ${tag}: Needs changes sends a kind and a note and files a Fix item`, async () => {
      await open(r1.id);
      await item(r1.id).locator(".rv-chg").click();
      await item(r1.id).locator('.rv-kind[data-k="idea"]').click();
      const box = item(r1.id).locator(".rv-text");
      await box.fill("Bigger thumbnails, later");
      await page.screenshot({ path: `${out}/reviewcard-note-${width}-${dark ? "dark" : "light"}.png` });
      await item(r1.id).locator(".rv-send").click();
      const t = await until(async () => { const x = await get(r1.id); return x.state === "done" ? x : null; }, "the review to close");
      const v = t.review.verdict;
      if (v.verdict !== "changes" || v.kind !== "idea" || v.note !== "Bigger thumbnails, later" || !t.review.fix_id) throw new Error(JSON.stringify(t.review));
      const fix = (await all()).find((x) => x.id === t.review.fix_id);
      if (!fix || fix.review_of !== r1.id || fix.kind !== "followup") throw new Error("fix item: " + JSON.stringify(fix));
      await drop(fix.id);
    });
    if (width > 500) {
      await check(`review card at ${tag}: key 1 is Looks good, key 2 opens the note`, async () => {
        const r3 = await addReview("Review: keys A", withPic), r4 = await addReview("Review: keys B", withPic); ids.push(r3.id, r4.id);
        await open(r3.id);
        await page.locator("body").click({ position: { x: 1, y: 1 } }).catch(() => {});
        await page.keyboard.press("2");
        await page.waitForSelector(`${L} .td-item[data-id="${r3.id}"] .rv-text`);
        await item(r3.id).locator(".rv-cancel").click();
        await page.locator(`${L} .td-item[data-id="${r3.id}"] .td-row`).focus();
        await page.keyboard.press("1");
        await until(async () => (await get(r3.id)).state === "done", "key 1 to close the review");
      });
    }
    if (errs.length) throw new Error("page errors: " + errs.join("; "));
  } catch (e) {
    results.push({ name: `review card at ${tag}: ${e.message.split("\n")[0]}`, ok: false }); console.log("FAIL " + tag + " " + e.message);
  } finally {
    for (const id of ids) await drop(id).catch(() => {});
    await browser.close();
  }
}
const bad = results.filter((r) => !r.ok).length;
console.log(`\n${results.length - bad}/${results.length} review card checks passed`);
process.exit(bad ? 1 : 0);
