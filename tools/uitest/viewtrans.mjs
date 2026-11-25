// View Transitions between screens (#135 slice 5). document.startViewTransition is wrapped in the page, so every
// transition the app starts is recorded with its direction (html[data-vt]) and the animations it ran.
//   - 390 px with real touch input: a row tap slides forward (vt-in-right on the new screen, 200 ms), the app's Back and
//     the browser's back slide back (vt-out-right on the old one); Pages and its Back do the same through the hash;
//   - the return mark still shows on the row you came back from, and the press layer still shows on a row held down;
//   - a tap on Back while the forward slide is running lands: the list is back and the slide skipped to its end;
//   - 1280 px with a mouse: a row click cross-fades (120 ms, data-vt=fade); a click on another row mid-fade opens it;
//   - prefers-reduced-motion: reduce starts no transition, and the screens still change;
//   - no transition API (an older browser): the screens change as before.
//
//   node viewtrans.mjs BASE OUT_DIR      (env: SCHEME)
import { launch } from "./lib.mjs";
import fs from "node:fs";
import path from "node:path";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const tag = dark ? "dark" : "light";
let failed = 0;
const errors = [];
let page;
async function check(name, fn) {
  try { await fn(); console.log("ok   " + name); }
  catch (e) {
    failed++; console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     "));
    if (page) await page.screenshot({ path: path.join(out, `viewtrans-fail-${tag}-${failed}.png`) }).catch(() => {});
  }
}
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };

// In the page, before the app: record each transition (direction, when, the view-transition animations it ran, how it ended).
const RECORD = (off) => {
  window.__vts = [];
  if (off) { delete Document.prototype.startViewTransition; return; }
  const orig = Document.prototype.startViewTransition;
  if (!orig) return;
  Document.prototype.startViewTransition = function (cb) {
    const rec = { dir: document.documentElement.dataset.vt || "", at: performance.now(), anims: [], ready: "", end: 0 };
    window.__vts.push(rec);
    const t = orig.call(this, cb);
    t.ready.then(() => {
      rec.ready = "ok";
      rec.anims = document.getAnimations().filter((a) => (a.effect && a.effect.pseudoElement || "").startsWith("::view-transition"))
        .map((a) => ({ pe: a.effect.pseudoElement, name: a.animationName, dur: a.effect.getTiming().duration }));
    }, () => { rec.ready = "skipped"; });
    t.finished.then(() => { rec.end = performance.now(); }, () => { rec.end = performance.now(); });
    return t;
  };
};

async function open({ width, height, reduced = false, off = false }) {
  const l = await launch({ width, height, dark });
  await l.ctx.addInitScript(RECORD, off);
  page = l.page;
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => errors.push("pageerror: " + e.message));
  if (reduced) await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent[data-id]");
  return l;
}
const vts = () => page.evaluate(() => window.__vts.map((r) => ({ ...r })));
const ended = () => page.waitForFunction(() => window.__vts.every((r) => r.end));
const detailShown = () => page.evaluate(() => getComputedStyle(document.getElementById("detail")).display !== "none" && document.body.classList.contains("has-detail"));
const listShown = () => page.evaluate(() => getComputedStyle(document.getElementById("list")).display !== "none" && !document.body.classList.contains("has-detail"));

// A real touch (CDP) on the middle of the first visible match, or a mouse click at 1280.
let cdp;
async function boxOf(sel, nth = 0) {
  return page.evaluate(([sel, nth]) => {
    const el = [...document.querySelectorAll(sel)].filter((e) => e.getClientRects().length)[nth];
    if (!el) return null;
    const r = el.getBoundingClientRect();
    return { x: r.left + Math.min(r.width / 2, 120), y: r.top + r.height / 2, id: el.dataset.id || "" };
  }, [sel, nth]);
}
async function tap(sel, { hold = 40, nth = 0 } = {}) {
  const b = await boxOf(sel, nth);
  if (!b) throw new Error("nothing to tap: " + sel);
  const pt = [{ x: b.x, y: b.y, id: 1 }];
  await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: pt });
  await page.waitForTimeout(hold);
  await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  return b;
}
const anim = (r, name) => r.anims.find((a) => a.name === name);

await check("390: a row tap slides forward; Back and the browser's back slide back; the row is marked on return", async () => {
  const l = await open({ width: 390, height: 844 });
  cdp = await l.ctx.newCDPSession(page);
  try {
    assert(await page.evaluate(() => typeof Document.prototype.startViewTransition === "function"), "this Chrome has no View Transitions");
    const row = await tap("#agents .agent[data-id]");
    await page.waitForFunction(() => document.body.classList.contains("has-detail"));
    await ended();
    let v = await vts();
    assert(v.length === 1, "one transition for the tap, got " + JSON.stringify(v));
    assert(v[0].dir === "fwd", "the tap's transition is " + v[0].dir);
    const inR = anim(v[0], "vt-in-right");
    assert(inR && inR.pe === "::view-transition-new(root)" && inR.dur === 200, "no 200 ms slide in from the right: " + JSON.stringify(v[0].anims));
    assert(await page.evaluate(() => !document.documentElement.dataset.vt), "html[data-vt] stays after the transition");
    // The app's Back.
    await tap("#back");
    await page.waitForFunction(() => !document.body.classList.contains("has-detail"));
    await ended();
    v = await vts();
    assert(v.length === 2 && v[1].dir === "back" && anim(v[1], "vt-out-right"), "Back did not slide back: " + JSON.stringify(v[1]));
    await page.waitForFunction((id) => !!document.querySelector(`#agents .agent.was-open[data-id="${CSS.escape(id)}"]`), row.id, { timeout: 3000 })
      .catch(() => { throw new Error("the return mark did not show on the row after the transition"); });
    // The browser's back.
    await tap("#agents .agent[data-id]");
    await page.waitForFunction(() => document.body.classList.contains("has-detail"));
    await ended();
    await page.goBack();
    await page.waitForFunction(() => !document.body.classList.contains("has-detail"));
    await ended();
    v = await vts();
    assert(v.length === 4 && v[2].dir === "fwd" && v[3].dir === "back", "browser back: " + JSON.stringify(v.map((r) => r.dir)));
    assert(await listShown(), "the list is not shown after back");
    await page.screenshot({ path: path.join(out, `viewtrans-list-${tag}.png`) });
  } finally { await l.browser.close(); }
});

await check("390: Pages through the hash slides forward, its Back slides back", async () => {
  const l = await open({ width: 390, height: 844 });
  cdp = await l.ctx.newCDPSession(page);
  try {
    await page.evaluate(() => { location.hash = "#/pages"; });
    await page.waitForSelector("#pages:not([hidden])");
    await ended();
    let v = await vts();
    assert(v.length === 1 && v[0].dir === "fwd" && anim(v[0], "vt-in-right"), "Pages: " + JSON.stringify(v));
    await page.goBack();
    await page.waitForSelector("#pages", { state: "hidden" });
    await ended();
    v = await vts();
    assert(v.length === 2 && v[1].dir === "back" && anim(v[1], "vt-out-right"), "Pages back: " + JSON.stringify(v));
  } finally { await l.browser.close(); }
});

await check("390: the press layer shows on a held row with transitions on", async () => {
  const l = await open({ width: 390, height: 844 });
  cdp = await l.ctx.newCDPSession(page);
  try {
    const b = await boxOf("#agents .agent[data-id]");
    await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: b.x, y: b.y, id: 1 }] });
    await page.waitForTimeout(120);
    const pressed = await page.evaluate(() => !!document.querySelector("#agents .agent.is-pressed"));
    await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
    assert(pressed, "no .is-pressed on the held row");
    await page.waitForFunction(() => document.body.classList.contains("has-detail"));
  } finally { await l.browser.close(); }
});

await check("390: a tap on Back during the forward slide lands", async () => {
  const l = await open({ width: 390, height: 844 });
  cdp = await l.ctx.newCDPSession(page);
  try {
    await tap("#agents .agent[data-id]", { hold: 20 });
    // Mid-slide: the transition has started and not finished.
    await page.waitForFunction(() => window.__vts.length === 1 && window.__vts[0].ready === "ok" && !window.__vts[0].end, null, { timeout: 2000, polling: "raf" });
    const t0 = await page.evaluate(() => performance.now());
    await tap("#back", { hold: 20 });
    await page.waitForFunction(() => !document.body.classList.contains("has-detail"), null, { timeout: 2000 });
    await ended();
    const v = await vts();
    assert(v[0].end - v[0].at < 190, `the forward slide ran its full length (${Math.round(v[0].end - v[0].at)} ms) instead of skipping to its end`);
    assert(v[0].end <= t0 + 60, "the forward slide did not end at the tap");
    assert(await listShown(), "the list is not shown after Back");
  } finally { await l.browser.close(); }
});

await check("1280: a row click cross-fades for 120 ms; a click on another row mid-fade opens that one", async () => {
  const l = await open({ width: 1280, height: 860 });
  try {
    // The desktop may open to something on its own (an agent waiting on you, or Status): let that settle first.
    await page.waitForTimeout(400);
    await ended();
    const n0 = (await vts()).length;
    const rows = await page.evaluate(() => [...document.querySelectorAll("#agents .agent[data-id]:not(.selected)")].filter((e) => e.getClientRects().length).slice(0, 2).map((e) => e.dataset.id));
    assert(rows.length === 2, "two rows to click");
    const sel = (id) => `#agents .agent[data-id="${id}"]`;
    await page.click(sel(rows[0]));
    await page.waitForFunction((n) => window.__vts.length > n && window.__vts[n].ready === "ok", n0, { polling: "raf" });
    let v = await vts();
    const f = v[n0];
    assert(f.dir === "fade", "the desktop's transition is " + f.dir);
    const fades = f.anims.filter((a) => a.pe === "::view-transition-new(root)" || a.pe === "::view-transition-old(root)");
    assert(fades.length && fades.every((a) => a.dur === 120), "not a 120 ms cross-fade: " + JSON.stringify(f.anims));
    // Mid-fade: a second click lands on the second row.
    await page.click(sel(rows[1]));
    await page.waitForFunction((id) => document.querySelector(`#agents .agent.selected[data-id="${CSS.escape(id)}"]`), rows[1], { timeout: 2000 });
    await ended();
    assert(await detailShown(), "no detail after the second click");
    assert(await page.evaluate(() => state.selected) === rows[1], "the second row is not the one open");
  } finally { await l.browser.close(); }
});

await check("390 with reduced motion: no transition, and the screens still change", async () => {
  const l = await open({ width: 390, height: 844, reduced: true });
  cdp = await l.ctx.newCDPSession(page);
  try {
    await tap("#agents .agent[data-id]");
    await page.waitForFunction(() => document.body.classList.contains("has-detail"));
    await tap("#back");
    await page.waitForFunction(() => !document.body.classList.contains("has-detail"));
    await page.evaluate(() => { location.hash = "#/pages"; });
    await page.waitForSelector("#pages:not([hidden])");
    await page.goBack();
    await page.waitForSelector("#pages", { state: "hidden" });
    const v = await vts();
    assert(v.length === 0, "transitions ran under reduced motion: " + JSON.stringify(v));
  } finally { await l.browser.close(); }
});

await check("1280 with reduced motion: no transition on a row click", async () => {
  const l = await open({ width: 1280, height: 860, reduced: true });
  try {
    await page.waitForTimeout(300);
    const id = await page.evaluate(() => [...document.querySelectorAll("#agents .agent[data-id]:not(.selected)")].find((e) => e.getClientRects().length)?.dataset.id);
    await page.click(`#agents .agent[data-id="${id}"]`);
    await page.waitForFunction((id) => state.selected === id, id);
    assert((await vts()).length === 0, "a transition ran under reduced motion");
  } finally { await l.browser.close(); }
});

await check("390 without the API: the screens change as before", async () => {
  const l = await open({ width: 390, height: 844, off: true });
  cdp = await l.ctx.newCDPSession(page);
  try {
    assert(await page.evaluate(() => !document.startViewTransition), "the API is still there");
    await tap("#agents .agent[data-id]");
    await page.waitForFunction(() => document.body.classList.contains("has-detail"));
    await tap("#back");
    await page.waitForFunction(() => !document.body.classList.contains("has-detail"));
    assert(await listShown(), "the list is not shown");
  } finally { await l.browser.close(); }
});

await check("no page errors", async () => { assert(errors.length === 0, errors.join("\n")); });
process.exit(failed ? 1 : 0);
