// Pull to refresh (#136 slice 6), at 390 px with real touch input (CDP Input.dispatchTouchEvent):
//   - on the list, a pull short of the arm point fills part of the ring and does nothing; past it the ring pulses
//     (armed), letting go refreshes once, and the spinner shows for at least 500 ms (longer while the answer is held);
//   - Status, Pages, Peers, Releases, Usage and Archived pull too, each asking the hub for its own list;
//   - each of those screens has ⋮ › Refresh, which does the same;
//   - the chat and Settings never pull, and html.ptr-on (the browser's own bounce off) is set only on the screens that do;
//   - with Settings › Vibrate on, arming buzzes once (8 ms); off (the default), never;
//   - offline, a pull says it could not refresh and why, the saved rows stay, and the strip says Offline with Retry;
//   - at 1280 px nothing changes: no ⋮ on the screens, no ring, no ptr-on, and the list's ⋮ › Refresh is still there.
//
//   node pullrefresh.mjs BASE OUT_DIR      (env: SCHEME)
import { launch, makeCheck } from "./lib.mjs";
import fs from "node:fs";
import path from "node:path";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const tag = dark ? "dark" : "light";
const { browser, ctx, page } = await launch({ dark });
page.setDefaultTimeout(8000);
const errors = [];
page.on("pageerror", (e) => errors.push("pageerror: " + e.message));
const check = makeCheck({ page, out });
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };

// Requests the app makes, by path, so a check can count what one pull asked for.
const reqs = [];
page.on("request", (r) => { const u = new URL(r.url()); if (u.pathname.startsWith("/api/")) reqs.push({ path: u.pathname, at: Date.now() }); });
const asked = (p, since) => reqs.filter((r) => r.path === p && r.at >= since).length;

// The spinner's life, seen from the page: the moment #ptr starts and stops spinning, stamped in the very call that
// changed its class (classList.add/remove/toggle, patched here) or its hidden flag, so the time is the app's own and
// not across round trips. A MutationObserver catches any other change, but only at the end of the task: on a loaded
// machine the rest of that task (the refresh's own synchronous work) ran first, the start was stamped late and a
// 500 ms spin measured 483 ms. Sampling once a frame, before that, cut it short the same way.
await page.addInitScript(() => {
  window.__spin = [];
  let last = false;
  const note = () => {
    const on = !!document.querySelector("#ptr.spin:not([hidden])");
    if (on !== last) { last = on; window.__spin.push({ on, t: performance.now() }); }
  };
  for (const m of ["add", "remove", "toggle", "replace"]) {
    const orig = DOMTokenList.prototype[m];
    DOMTokenList.prototype[m] = function (...a) { const r = orig.apply(this, a); note(); return r; };
  }
  const hid = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "hidden");
  Object.defineProperty(HTMLElement.prototype, "hidden", { ...hid, set(v) { hid.set.call(this, v); note(); } });
  new MutationObserver(note).observe(document, { subtree: true, childList: true, attributes: true, attributeFilter: ["class", "hidden"] });
});
// navigator.vibrate stubbed (headless Chrome has none): each call is recorded, for the Settings › Vibrate check.
await page.addInitScript(() => {
  window.__vib = [];
  Object.defineProperty(Navigator.prototype, "vibrate", { configurable: true, writable: true, value(p) { window.__vib.push(p); return true; } });
});
const spins = () => page.evaluate(() => {
  const s = window.__spin, r = [];
  for (let i = 0; i < s.length; i++) if (s[i].on) r.push((s[i + 1] ? s[i + 1].t : performance.now()) - s[i].t);
  return r;
});

const cdp = await ctx.newCDPSession(page);
const touch = (type, x, y) => cdp.send("Input.dispatchTouchEvent", { type, touchPoints: type === "touchEnd" ? [] : [{ x, y, id: 1 }] });
// pull: a finger down at (x, y0), dragged dist px down in 8 px steps, then lifted. Returns what the ring showed just
// before the finger came up.
async function pull(x, y0, dist) {
  await touch("touchStart", x, y0);
  for (let d = 8; d < dist + 8; d += 8) { await touch("touchMove", x, y0 + Math.min(d, dist)); }
  const st = await page.evaluate(() => {
    const el = document.getElementById("ptr");
    return { state: el ? el.dataset.state : "", shown: !!el && !el.hidden, armed: !!el && el.classList.contains("armed"), off: ptr.off,
      fill: el ? Number(el.querySelector(".ptr-fill").style.strokeDashoffset) : 100 };
  });
  await touch("touchEnd", x, y0 + dist);
  return st;
}
const runs = () => page.evaluate(() => ptr.runs);
const idle = (n) => page.waitForFunction((n) => !ptr.busy && ptr.runs === n, n, { timeout: 15000 });
// Where to start a pull on a screen: a little below the top of the element that scrolls, at its top.
async function startOf(sel) {
  return page.evaluate(async (sel) => {
    for (let i = 0; i < 200; i++) {
      const el = document.querySelector(sel);
      if (el && el.getClientRects().length) {
        el.scrollTop = 0;
        // Clear of the screen's own controls (Pages' search box, say), by more than a finger's width: touch adjustment
        // moves a touch near one onto it, and a pull starts on the rows, not on an input.
        const r = el.getBoundingClientRect(), x = Math.round(r.left + r.width / 2);
        const clear = (y) => { const hit = document.elementFromPoint(x, y); return !!hit && el.contains(hit) && !hit.closest("input, select, textarea, button, a, [role='group'], [role='button']"); };
        for (let y = Math.round(r.top + 30); y < r.bottom - 40; y += 10) if (clear(y - 24) && clear(y) && clear(y + 24)) return { x, y };
      }
      await new Promise((ok) => requestAnimationFrame(ok));
    }
    return null;
  }, sel);
}
async function open(hash, sel) {
  await page.evaluate((h) => { location.hash = h; }, hash);
  await page.waitForFunction((sel) => { const el = document.querySelector(sel); return el && el.getClientRects().length > 0; }, sel);
  await page.waitForTimeout(300); // its first read
}
const toastText = () => page.evaluate(() => { const t = document.getElementById("toast"); return t && t.classList.contains("show") ? t.textContent : ""; });

await page.goto(base + "/", { waitUntil: "load" });
await page.waitForSelector("#agents .agent[data-id]");
await page.evaluate(() => scrollTo(0, 0));

await check("list: a pull short of the arm point fills part of the ring and does nothing", async () => {
  const n = await runs(), t0 = Date.now();
  const st = await pull(195, 320, 80); // 70 px past the slop: offset 140·(1−e^(−70/140)) ≈ 55 < 64
  assert(st.shown && st.state === "pull" && !st.armed, "the ring while pulling: " + JSON.stringify(st));
  assert(st.off > 40 && st.off < 64, "offset under the arm point: " + st.off);
  assert(st.fill > 0 && st.fill < 30, "the ring is partly filled (dashoffset " + st.fill + ")");
  await page.waitForTimeout(400);
  assert((await runs()) === n, "no refresh ran");
  assert(asked("/api/agents", t0) === 0, "the agents were not asked for");
  assert(await page.evaluate(() => document.getElementById("ptr").hidden), "the ring went away");
});

await check("list: past the arm point it pulses, refreshes once and spins at least 500 ms", async () => {
  const n = await runs(), t0 = Date.now();
  await page.evaluate(() => { window.__spin = []; });
  const st = await pull(195, 320, 200);
  assert(st.armed && st.state === "armed", "armed before letting go: " + JSON.stringify(st));
  assert(st.off > 64 && st.off < 140, "offset past the arm point and under the cap: " + st.off);
  await idle(n + 1);
  await page.waitForTimeout(300);
  assert((await runs()) === n + 1, "exactly one refresh ran");
  assert(asked("/api/agents", t0) >= 1, "the agents were asked for");
  const s = await spins();
  assert(s.length === 1 && s[0] >= 490, "one spin of at least 500 ms: " + JSON.stringify(s));
  assert(!(await toastText()).includes("Couldn"), "no failure said");
});

await check("list: html.ptr-on turns the browser's own overscroll off", async () => {
  const v = await page.evaluate(() => ({ on: document.documentElement.classList.contains("ptr-on"), html: getComputedStyle(document.documentElement).overscrollBehaviorY, body: getComputedStyle(document.body).overscrollBehaviorY }));
  assert(v.on && v.html === "none" && v.body === "none", JSON.stringify(v));
});

await check("a refresh waits on a held answer, and the ring holds at 48 px meanwhile", async () => {
  await open("#/pages", "#pages .pgs-list");
  let release;
  const held = new Promise((ok) => { release = ok; });
  await page.route(/\/api\/pages$/, async (r) => { await held; await r.continue().catch(() => {}); });
  const p = await startOf("#pages .pgs-list"), n = await runs(), t0 = Date.now();
  await page.evaluate(() => { window.__spin = []; });
  await pull(p.x, p.y, 200);
  await page.waitForTimeout(1200);
  const mid = await page.evaluate(() => { const el = document.getElementById("ptr"); return { spin: el.classList.contains("spin"), busy: ptr.busy, y: el.getBoundingClientRect().top, edge: document.querySelector("#pages .pgs-list").getBoundingClientRect().top }; });
  assert(mid.spin && mid.busy, "still spinning while the answer is held: " + JSON.stringify(mid));
  assert(Math.abs(mid.y - (mid.edge + 48 - 36)) <= 2, "held at 48 px under the edge: " + JSON.stringify(mid));
  release();
  await idle(n + 1);
  await page.unroute(/\/api\/pages$/);
  const s = await spins();
  assert(s.length === 1 && s[0] >= 1150, "the spin lasted until the answer: " + JSON.stringify(s));
  assert(asked("/api/pages", t0) === 1, "Pages was asked once: " + asked("/api/pages", t0));
});
await page.screenshot({ path: path.join(out, `pullrefresh-pages-${tag}.png`) }).catch(() => {});

// Each screen that pulls, the element that scrolls on it, and the read its pull must make.
const SCREENS = [
  { name: "Status", hash: "#/status", sel: "#ss-body", api: "/api/ledger" },
  { name: "Peers", hash: "#/peers", sel: "#peers .pr-list", api: "/api/agents" },
  { name: "Releases", hash: "#/peers/~releases", sel: "#pr-releases", api: "/api/releases" },
  { name: "Usage", hash: "#/usage", sel: "#us-body", api: "/api/usage" },
  { name: "Archived", hash: "#/archived", sel: "#ar-body", api: "/api/archived" },
];
for (const sc of SCREENS) {
  await check(`${sc.name}: a pull refreshes it`, async () => {
    await open(sc.hash, sc.sel);
    const p = await startOf(sc.sel), n = await runs(), t0 = Date.now();
    const st = await pull(p.x, p.y, 200);
    assert(st.armed, "armed: " + JSON.stringify(st));
    await idle(n + 1);
    assert(asked(sc.api, t0) >= 1, sc.api + " was asked for");
    assert(await page.evaluate(() => document.documentElement.classList.contains("ptr-on")), "ptr-on is set");
  });
  await check(`${sc.name}: ⋮ › Refresh does the same`, async () => {
    const n = await runs(), t0 = Date.now();
    const b = await page.evaluate((h) => {
      const scr = document.querySelector(h === "#/status" ? "#st-screen" : h.startsWith("#/peers") ? "#peers" : h === "#/usage" ? "#usage" : "#archived");
      const btn = scr.querySelector(".scr-more"), r = btn.getBoundingClientRect();
      return { x: r.left + r.width / 2, y: r.top + r.height / 2, w: r.width, h: r.height };
    }, sc.hash);
    assert(b.w >= 44 && b.h >= 44, "the ⋮ is a 44 px target: " + JSON.stringify(b));
    await touch("touchStart", b.x, b.y); await page.waitForTimeout(40); await touch("touchEnd", b.x, b.y);
    await page.waitForSelector("#scr-menu:not([hidden]) #scr-refresh");
    // The sheet slides up: wait until it is still, then tap Refresh where it stands.
    const r = await page.evaluate(async () => {
      const m = document.getElementById("scr-menu");
      let last = "";
      for (let i = 0; i < 120; i++) {
        await new Promise((ok) => requestAnimationFrame(ok));
        const e = document.getElementById("scr-refresh").getBoundingClientRect(), k = `${e.left},${e.top}`;
        if (k === last && !m.getAnimations({ subtree: true }).some((a) => a.playState === "running")) return { x: e.left + e.width / 2, y: e.top + e.height / 2 };
        last = k;
      }
      return null;
    });
    assert(r, "the menu never stood still");
    await touch("touchStart", r.x, r.y); await page.waitForTimeout(40); await touch("touchEnd", r.x, r.y);
    await idle(n + 1);
    assert(asked(sc.api, t0) >= 1, sc.api + " was asked for");
    assert(await page.evaluate(() => document.getElementById("scr-menu").hidden), "the menu closed");
  });
}
await check("Pages: ⋮ › Refresh is there too", async () => {
  await open("#/pages", "#pages .pgs-list");
  assert(await page.evaluate(() => !!document.querySelector("#pages header .scr-more")?.getClientRects().length), "a ⋮ in the Pages header");
});

await check("Settings and the chat never pull, and ptr-on is off there", async () => {
  await open("#/settings", "#sx-body");
  let n = await runs();
  let st = await pull(195, 200, 200);
  assert(!st.shown && (await runs()) === n, "Settings pulled: " + JSON.stringify(st));
  assert(!(await page.evaluate(() => document.documentElement.classList.contains("ptr-on"))), "ptr-on on Settings");
  await page.evaluate(() => { location.hash = ""; });
  await page.waitForSelector("#agents .agent[data-id]");
  await page.evaluate(() => document.querySelector("#agents .agent[data-id]").click());
  await page.waitForFunction(() => document.body.classList.contains("has-detail"));
  await page.waitForTimeout(500);
  n = await runs();
  st = await pull(195, 300, 200);
  await page.waitForTimeout(300);
  assert(!st.shown && (await runs()) === n, "the chat pulled: " + JSON.stringify(st));
  assert(!(await page.evaluate(() => document.documentElement.classList.contains("ptr-on"))), "ptr-on in the chat");
  await page.evaluate(() => history.back());
  await page.waitForFunction(() => !document.body.classList.contains("has-detail"));
});

await check("offline: a pull says it could not refresh, and the saved rows stay", async () => {
  await page.waitForSelector("#agents .agent[data-id]");
  await page.evaluate(() => scrollTo(0, 0));
  const rows = await page.evaluate(() => document.querySelectorAll("#agents .agent[data-id]").length);
  await ctx.setOffline(true);
  await page.evaluate(() => connectEvents());
  const n = await runs();
  await page.evaluate(() => { window.__spin = []; });
  await pull(195, 320, 200);
  await idle(n + 1);
  await page.waitForFunction(() => /Couldn’t refresh/.test(document.getElementById("toast").textContent));
  const t = await page.evaluate(() => document.getElementById("toast").textContent);
  assert(/offline/i.test(t), "says why: " + t);
  const after = await page.evaluate(() => ({ rows: document.querySelectorAll("#agents .agent[data-id]").length, stale: document.getElementById("agents").classList.contains("stale") }));
  assert(after.rows === rows && rows > 0, `rows kept: ${rows} -> ${after.rows}`);
  assert(after.stale, "the rows are marked old");
  await page.waitForSelector('#hub-strip[data-kind="offline"]:not([hidden]) .hs-retry');
  const s = await spins();
  assert(s.length === 1 && s[0] >= 490, "the spin still showed 500 ms: " + JSON.stringify(s));
  await page.screenshot({ path: path.join(out, `pullrefresh-offline-${tag}.png`) }).catch(() => {});
  await ctx.setOffline(false);
  await page.evaluate(() => stripRetry());
  await page.waitForFunction(() => !document.getElementById("agents").classList.contains("stale"), null, { timeout: 15000 });
});

await check("Vibrate: arming buzzes once (8 ms) only with Settings › Vibrate on", async () => {
  await page.evaluate(() => { scrollTo(0, 0); localStorage.removeItem("comebye-vibrate"); window.__vib = []; });
  let n = await runs();
  await pull(195, 320, 200);
  await idle(n + 1);
  assert((await page.evaluate(() => window.__vib.length)) === 0, "buzzed with the setting off");
  await page.evaluate(() => { localStorage.setItem("comebye-vibrate", "on"); window.__vib = []; });
  await pull(195, 320, 80); // short of the arm point: no buzz
  assert((await page.evaluate(() => window.__vib.length)) === 0, "buzzed short of the arm point");
  n = await runs();
  await pull(195, 320, 200);
  await idle(n + 1);
  const v = await page.evaluate(() => window.__vib);
  assert(JSON.stringify(v) === "[8]", "one 8 ms buzz: " + JSON.stringify(v));
  await page.evaluate(() => localStorage.removeItem("comebye-vibrate"));
});

// A still of the ring mid-pull and spinning, for the demo.
await check("stills: the ring while pulling and while spinning", async () => {
  await page.evaluate(() => scrollTo(0, 0));
  await touch("touchStart", 195, 320);
  for (let d = 8; d <= 200; d += 8) await touch("touchMove", 195, 320 + d);
  await page.screenshot({ path: path.join(out, `pullrefresh-armed-${tag}.png`) });
  const n = await runs();
  await touch("touchEnd", 195, 520);
  await page.waitForTimeout(150);
  await page.screenshot({ path: path.join(out, `pullrefresh-spin-${tag}.png`) });
  await idle(n + 1);
});
await browser.close();

// Desktop: unchanged.
const dk = await launch({ dark, width: 1280, height: 800 });
const dp = dk.page;
dp.on("pageerror", (e) => errors.push("desktop pageerror: " + e.message));
await check("desktop 1280: no ⋮ on the screens, no ring, no ptr-on; the list's Refresh stays", async () => {
  await dp.goto(base + "/", { waitUntil: "load" });
  await dp.waitForSelector("#agents .agent[data-id]");
  for (const h of ["#/pages", "#/usage", "#/archived", "#/peers"]) {
    await dp.evaluate((h) => { location.hash = h; }, h);
    await dp.waitForTimeout(300);
    const v = await dp.evaluate(() => ({ more: [...document.querySelectorAll(".scr-more")].filter((b) => b.getClientRects().length).length, on: document.documentElement.classList.contains("ptr-on"), ptr: !!document.getElementById("ptr") && !document.getElementById("ptr").hidden }));
    assert(!v.more && !v.on && !v.ptr, h + ": " + JSON.stringify(v));
  }
  assert(await dp.evaluate(() => !!document.getElementById("f-refresh")), "the list's ⋮ › Refresh");
  await dp.screenshot({ path: path.join(out, `pullrefresh-desktop-${tag}.png`) });
});
await dk.browser.close();

for (const e of errors) console.log("FAIL " + e);
process.exit(check.failed || errors.length ? 1 : 0);
