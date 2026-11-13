// The review card (internal/server/review.go) in the split review mode (issue #17, slice 3b): a kind "review"
// item about a Page opens with the Page in a frame beside it (tabs on a phone), at the version it was filed with;
// the side has what to try and two buttons. Looks good closes at once with an Undo toast driven by the hub's
// undo_until; Needs changes takes a kind and a note and files a Fix item; comments selected on the Page go with the
// verdict; keys 1 and 2 and Escape on desktop. Pages are published with the real CLI.
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
  const LIST = width > 500 ? "#p-todo" : "#todo";
  const L = "#rs"; // the split view holds the open review
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark, bypassCSP: false });
  const errs = [];
  page.on("pageerror", (e) => errs.push(String(e)));
  const ids = [];
  const item = (id) => page.locator(`${L} .td-item[data-id="${id}"]`);
  const open = async (id) => {
    await page.evaluate(async () => { todos.loadedAt = 0; todos.openId = null; await loadTodos(); renderTodos(true); });
    await page.waitForSelector(`${LIST} .td-item[data-id="${id}"] .td-row`, { state: "visible" });
    if (!(await page.locator(`${L} .td-item.open[data-id="${id}"]`).count())) {
      if (await page.locator("#rs:not([hidden])").count()) await page.locator("#rs .rs-back").click();
      await page.locator(`${LIST} .td-item[data-id="${id}"] .td-row`).click();
    }
    await page.waitForSelector(`${L} .td-item.open[data-id="${id}"] .rv-bar`, { state: "attached" });
    if (width < 500) await page.locator('#rs [data-rs-tab="decide"]').click();
    await page.waitForSelector(`${L} .td-item.open[data-id="${id}"] .rv-bar`, { state: "visible" });
  };
  try {
    const r1 = await addReview(`Review: Gallery thumbnails ${width}`, withPic); ids.push(r1.id);
    if (width > 500) { await page.goto(base + "/#/todo", { waitUntil: "load" }); await page.waitForSelector("#p-todo", { state: "visible" }); }
    else {
      await page.goto(base + "/", { waitUntil: "load" });
      await page.waitForSelector("#todo .td-toggle", { state: "visible", timeout: 15000 });
      if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
    }
    await check(`review card at ${tag}: the Page beside it at the version it was filed with, what to try, two buttons, where it goes`, async () => {
      await open(r1.id);
      const g = await page.evaluate((sel) => {
        const it = document.querySelector(sel), fr = document.querySelector("#rs iframe.pgs-frame");
        return { src: fr && fr.getAttribute("src"), text: it.innerText, btns: [...it.querySelectorAll(".rv-btns button")].map((b) => ({ t: b.innerText, h: b.getBoundingClientRect().height })),
          noThumb: !it.querySelector(".rv-thumb"), noAnswerBox: !it.querySelector(".td-answer-btn"), scroll: document.documentElement.scrollWidth > innerWidth,
          bar: document.querySelector("#rs .rs-title")?.textContent, listPanel: !!document.querySelector("#pt-body .td-panel, #todo .td-panel") };
      }, `${L} .td-item[data-id="${r1.id}"]`);
      if (!g.src || !new RegExp(`/p/${withPic}/1$`).test(g.src)) throw new Error("frame src: " + g.src);
      if (!/what to try/i.test(g.text)) throw new Error("text: " + g.text.slice(0, 300));
      if (!/Gallery thumbnails demo/.test(g.bar || "")) throw new Error("title in the bar: " + g.bar);
      if (g.btns.length !== 2 || !/Looks good/.test(g.btns[0].t) || !/Needs changes/.test(g.btns[1].t)) throw new Error("buttons: " + JSON.stringify(g.btns));
      if (g.btns.some((b) => b.h < 44 - 0.5)) throw new Error("buttons under 44px: " + JSON.stringify(g.btns));
      if (!/goes to|no longer running/i.test(g.text)) throw new Error("no routing line: " + g.text.slice(0, 400));
      if (!g.noThumb) throw new Error("a thumbnail next to the real Page");
      if (!g.noAnswerBox) throw new Error("the free-text Answer button is on a review");
      if (g.listPanel) throw new Error("the panel is in the list too");
      if (g.scroll) throw new Error("page scrolls sideways");
      await page.screenshot({ path: `${out}/split-${width}-decide-${dark ? "dark" : "light"}.png` });
      if (width < 500) {
        await page.locator('#rs [data-rs-tab="page"]').click();
        const v = await page.evaluate(() => { const f = document.querySelector("#rs iframe.pgs-frame").getBoundingClientRect(), s = document.querySelector("#rs-side").getBoundingClientRect(); return { fh: f.height, fw: f.width, sideShown: s.width > 0 }; });
        if (v.sideShown || v.fw < 300 || v.fh < 300) throw new Error("page tab: " + JSON.stringify(v));
      } else {
        const v = await page.evaluate(() => { const f = document.querySelector("#rs iframe.pgs-frame").getBoundingClientRect(), s = document.querySelector("#rs-side").getBoundingClientRect(); return { fl: f.left, fr: f.right, fw: f.width, sl: s.left, sw: s.width }; });
        if (v.fw < 500 || v.sw < 330 || v.fr > v.sl + 1) throw new Error("not side by side: " + JSON.stringify(v));
      }
      await page.screenshot({ path: `${out}/split-${width}-page-${dark ? "dark" : "light"}.png` });
    });
    await check(`review card at ${tag}: a newer version of the Page does not change the version under review`, async () => {
      const key = "rc-ver-" + width, pid = publish(key, "Versioned demo");
      const r = await addReview(`Review: Versioned ${width}`, pid); ids.push(r.id);
      publish(key, "Versioned demo two"); // v2
      await open(r.id);
      await page.locator('#rs [data-rs-tab="page"]').click().catch(() => {});
      const src = await page.locator("#rs iframe.pgs-frame").getAttribute("src");
      if (!new RegExp(`/p/${pid}/1$`).test(src)) throw new Error("frame src: " + src);
      await drop(r.id);
    });
    await check(`review card at ${tag}: Back closes the split and the list is as it was`, async () => {
      await open(r1.id);
      await page.locator("#rs .rs-back").click();
      await page.waitForSelector("#rs", { state: "hidden" });
      if (await page.locator("#rs iframe").count()) throw new Error("the frame stays after Back");
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
    await check(`review card at ${tag}: comments selected on the Page go with the verdict`, async () => {
      const r5 = await addReview(`Review: comments ${width}`, withPic); ids.push(r5.id);
      await open(r5.id);
      if (width < 500) await page.locator('#rs [data-rs-tab="page"]').click();
      let fr; for (let i = 0; i < 60 && !fr; i++) { fr = page.frames().find((x) => x.url().includes("/p/" + withPic + "/")); if (!fr) await page.waitForTimeout(100); }
      if (!fr) throw new Error("no frame");
      await fr.evaluate(() => parent.postMessage({ type: "comebye-selection", v: 1, quote: "Each card shows a picture", before: "", after: " of its page." }, "*"));
      await page.locator("#pgs-addc").click();
      await page.locator("#pgs-note").fill("Make the picture bigger");
      await page.locator("#pgs-noteadd").click();
      await page.waitForSelector("#pgs-tray .pgs-cmt", { state: "attached" });
      await page.screenshot({ path: `${out}/split-${width}-comment-${dark ? "dark" : "light"}.png` });
      if (width < 500) await page.locator('#rs [data-rs-tab="decide"]').click();
      const note = await page.locator("#rs-notes").innerText();
      if (!/1 comment on the Page goes with your answer/.test(note)) throw new Error("notes line: " + note);
      await item(r5.id).locator(".rv-chg").click();
      await item(r5.id).locator(".rv-text").fill("Also fix the colours");
      await item(r5.id).locator(".rv-send").click();
      const t = await until(async () => { const x = await get(r5.id); return x.state === "done" ? x : null; }, "the review to close");
      const n = t.review.verdict.note;
      if (!/Also fix the colours/.test(n) || !/On the Page \(v1\)/.test(n) || !/Each card shows a picture/.test(n) || !/Make the picture bigger/.test(n)) throw new Error("note: " + n);
      const c = await (await fetch(base + "/api/pages/" + withPic + "/comments")).json();
      if ((c.comments || []).length) throw new Error("the comments were also filed on their own");
    });
    if (width > 500) {
      await check(`split at ${tag}: a decision set about a Page opens beside it; Submit sends the comments in a note`, async () => {
        const set = { decisions: [
          { id: "D1", title: "Where tenants live", recommend: 1, options: [{ label: "One bucket", short: "1 bucket" }, { label: "A bucket each", short: "each" }] },
          { id: "D2", title: "Rollout", recommend: 1, options: [{ label: "Flag per tenant", short: "flag" }, { label: "All at once", short: "all" }] }] };
        const r = await fetch(base + "/api/todos", { method: "POST", headers: J, body: JSON.stringify({ question: "Decisions from the report", kind: "decision", no_wait: true, set, page: { id: withPic } }) });
        const t = (await r.json()).todo; ids.push(t.id);
        await fetch(base + `/api/todos/${t.id}/draft`, { method: "PUT", headers: J, body: JSON.stringify({ accept_suggestions: true, choices: { D1: { option: 1, confirmed: true }, D2: { option: 1, confirmed: true } } }) });
        await page.evaluate(async () => { todos.loadedAt = 0; todos.openId = null; await loadTodos(); renderTodos(true); });
        await page.locator(`${LIST} .td-item[data-id="${t.id}"] .td-row`).click();
        await page.waitForSelector(`#rs .td-item.open[data-id="${t.id}"] .ds-set`);
        let fr; for (let i = 0; i < 60 && !fr; i++) { fr = page.frames().find((x) => x.url().includes("/p/" + withPic + "/")); if (!fr) await page.waitForTimeout(100); }
        await fr.evaluate(() => parent.postMessage({ type: "comebye-selection", v: 1, quote: "Each card shows", before: "", after: " a picture" }, "*"));
        await page.locator("#pgs-addc").click();
        await page.locator("#pgs-note").fill("Is one bucket enough?");
        await page.locator("#pgs-noteadd").click();
        await page.waitForSelector("#rs-notes:not([hidden])");
        await page.screenshot({ path: `${out}/split-set-${width}-${dark ? "dark" : "light"}.png` });
        await page.locator("#rs .ds-submit").click();
        const x = await until(async () => { const y = await get(t.id); return y.state === "answered" ? y : null; }, "the set to be submitted");
        if (!/Is one bucket enough\?/.test(x.answer.text) || !/own words/.test(x.answer.text)) throw new Error("answer: " + x.answer.text);
      });
      await check(`review card at ${tag}: Escape leaves the note box, then closes the split`, async () => {
        const r6 = await addReview("Review: escape", withPic); ids.push(r6.id);
        await open(r6.id);
        await item(r6.id).locator(".rv-chg").click();
        await page.waitForSelector("#rs .rv-text");
        await page.keyboard.press("Escape");
        if (!(await page.evaluate(() => document.activeElement.classList.contains("rs-back")))) throw new Error("focus after the first Escape: " + (await page.evaluate(() => document.activeElement.className)));
        if (await page.locator("#rs").isHidden()) throw new Error("closed on the first Escape");
        await page.keyboard.press("Escape");
        await page.waitForSelector("#rs", { state: "hidden" });
      });
      await check(`review card at ${tag}: key 1 is Looks good, key 2 opens the note`, async () => {
        const r3 = await addReview("Review: keys A", withPic), r4 = await addReview("Review: keys B", withPic); ids.push(r3.id, r4.id);
        await open(r3.id);
        await page.locator("body").click({ position: { x: 1, y: 1 } }).catch(() => {});
        await page.keyboard.press("2");
        await page.waitForSelector(`${L} .td-item[data-id="${r3.id}"] .rv-text`);
        await item(r3.id).locator(".rv-cancel").click();
        await page.locator(`${LIST} .td-item[data-id="${r3.id}"] .td-row`).focus();
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
