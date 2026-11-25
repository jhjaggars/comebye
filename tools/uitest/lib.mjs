// Shared by the uitest scripts: the headless Chrome setup, the mocked routes for the launch sheet, the failure dump, and
// the helpers every check should use against a page that redraws (clickStable, readInPage, makeCheck).
//
// THE RULE: never hold an element handle (or a value read from one) across a redraw.
// The app redraws its panels on every poll (300 ms in test mode) and on every event. The hot ones (the agent list, To do,
// Status, Peers, Releases, the side panel's bar) are patched (app.js patchHTML: a row that did not change stays the same
// node), but many others are still rebuilt with innerHTML: there a handle taken before a redraw points at a detached
// node (empty computed style, no box), and a click whose press and release land on two different nodes fires nothing.
// A patched row can still be replaced (its key or tag changed). So:
//   - find an element by selector each time you use it (a locator, or a selector string passed to the helpers below),
//     never by a handle kept from an earlier step;
//   - read what you assert (text, attributes, box, computed style) in one evaluate inside the page (readInPage), not
//     in several round trips that a redraw can land between;
//   - click with clickStable, which waits until the element is attached, still and on top before it clicks;
//   - wait on a condition (waitForFunction, until), or for the app to be settled (settled()), never on a fixed sleep
//     sized for a fast laptop.
// When a check is about a redraw under the user's finger, force the redraw (call the app's render function between
// the press and the release, as reviewcard.mjs does) instead of hoping a poll lands there.
import { chromium } from "playwright-core";

export const CHROME = process.env.CHROME || "/usr/bin/google-chrome";

// One Chrome per script process: the first launch() starts it, every launch() gets a context of its own (its own
// cookies, storage, cache and permissions, as a fresh browser had), and the returned browser's close() closes that
// context. Chrome itself closes a moment after its last context does, so a script that ends without process.exit
// still ends. A script that opens a context several times (widths, themes) no longer starts Chrome each time
// (300 starts in the rest suite; a cold start on a CI runner took seconds).
// UITEST_PROFILE=FILE appends one line per Chrome start ("launch SCRIPT MS") to FILE, for profiling a run.
let shared = null, users = 0, idleClose = null;
async function acquire() {
  if (idleClose) { clearTimeout(idleClose); idleClose = null; }
  users++;
  if (!shared) {
    const t0 = Date.now();
    shared = chromium.launch({ executablePath: CHROME, headless: true, args: ["--no-sandbox"] });
    shared.then(async () => { if (process.env.UITEST_PROFILE) (await import("node:fs")).appendFileSync(process.env.UITEST_PROFILE, `launch ${process.argv[1].split("/").pop()} ${Date.now() - t0}\n`); }, () => {});
  }
  try {
    const b = await shared;
    if (b.isConnected()) return b;
    shared = null; users--; return acquire(); // it crashed or was closed: start another
  } catch (e) { shared = null; users--; throw e; }
}
function release() {
  if (--users > 0) return;
  const b = shared;
  idleClose = setTimeout(() => { idleClose = null; if (shared === b) shared = null; b?.then((x) => x.close()).catch(() => {}); }, 200);
}

// Opens a headless Chrome context with a phone-style viewport (390x844 by default;
// width > 500 makes a desktop one: no touch, no mobile emulation). bypassCSP:
// false keeps the servers' Content-Security-Policy in force (the Pages checks
// test the sandbox it sets). Returns { browser, ctx, page }: browser.close() closes
// this context (and Chrome once no context is left); browser.newContext() is the shared Chrome's.
// serviceWorkers: "block" by default: the app's sw.js is never registered. It answers the app shell (#136 slice 5),
// and a request a service worker lets fall through to the network is not always seen by page.route, so the scripts
// that hold or cut /api/ with routes would race it. (Not Playwright's own serviceWorkers: "block": it reads
// navigator.serviceWorker in every frame, which throws a page error in a sandboxed Pages frame.) shell.mjs, which
// tests the worker, passes "allow".
export async function launch({ width = 390, height = 844, dark, bypassCSP = true, serviceWorkers = "block" } = {}) {
  const real = await acquire();
  const mobile = width <= 500;
  let ctx;
  try {
    ctx = await real.newContext({
      viewport: { width, height }, deviceScaleFactor: mobile ? 2 : 1, isMobile: mobile, hasTouch: mobile, bypassCSP,
      ...(mobile ? { userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1" } : {}),
      ...(dark === undefined ? {} : { colorScheme: dark ? "dark" : "light" }),
    });
  } catch (e) { release(); throw e; }
  await ctx.addInitScript(settleFlag);
  if (serviceWorkers === "block") {
    await ctx.addInitScript(() => {
      try {
        const sw = navigator.serviceWorker; // throws in a sandboxed frame: nothing to block there
        if (sw) sw.register = () => Promise.reject(new DOMException("service workers are off in this test", "SecurityError"));
      } catch {}
    });
  }
  const extra = []; // contexts the script made itself with browser.newContext(): closed with this one
  let closed = false;
  const close = async () => {
    if (closed) return;
    closed = true;
    await Promise.all([ctx, ...extra].map((c) => c.close().catch(() => {})));
    release();
  };
  const browser = new Proxy(real, {
    get(t, k) {
      if (k === "close") return close;
      if (k === "newContext") return async (...a) => { const c = await t.newContext(...a); extra.push(c); await c.addInitScript(settleFlag); return c; };
      const v = Reflect.get(t, k);
      return typeof v === "function" ? v.bind(t) : v;
    },
  });
  return { browser, ctx, page: await ctx.newPage() };
}

// The flag that turns on the app's "settled" signal (web/app.js, settle): set in every page before its scripts run.
// Only the app's own page reads it; a Page's frame and a plain site ignore it.
const settleFlag = () => { window.__cbSettle = 1; };

// settled waits until the app says it is settled: no fetch in flight, no redraw pending, no finite animation running
// (web/app.js, window.__cb). It resolves true then, at once when it already is; false after timeout ms (a screen that
// never stops, an offline retry loop), so a caller can still go on and let its own assert say what is wrong. On a page
// without the signal (not the app, or a context made outside launch()) it waits two animation frames and resolves
// true. A navigation during the wait is waited out once. Use it in place of a sleep before a read or a click; for a
// value that only changes with the next poll, wait on that value (waitForFunction), not on settled.
export async function settled(page, { timeout = 5000, why = "settled" } = {}) {
  const t0 = Date.now();
  const wait = () => page.evaluate((timeout) => window.__cb
    ? window.__cb.settled(timeout)
    : new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(() => r(true)))), timeout);
  let ok;
  try { ok = await wait(); }
  catch { await page.waitForLoadState("domcontentloaded").catch(() => {}); ok = await wait().catch(() => false); }
  await noteSettle(page, why, Date.now() - t0, ok);
  return ok;
}

// Every settled() wait is counted (settleStats): how many, how long in all, and each one that timed out, with the
// screen (location.hash) and what kept the app from settling (window.__cb.state(): the fetches held, the last DOM
// change, the finite animations running). The journey prints a part's summary when it ends, a script its own at exit,
// so a CI log shows a screen that never settles instead of a script that is only slower.
// UITEST_PROFILE=FILE also appends one line per wait ("settle PART WHY MS ok|TIMEOUT HASH [STATE]") to FILE.
const PROFILE = process.env.UITEST_PROFILE;
const stats = { n: 0, ms: 0, timeouts: [] };
async function noteSettle(page, why, ms, ok) {
  stats.n++; stats.ms += ms;
  if (ok && !PROFILE) return;
  let info = { hash: "?", st: null };
  try { info = await page.evaluate(() => ({ hash: location.hash || "-", st: window.__cb ? window.__cb.state() : null })); } catch {}
  if (!ok) stats.timeouts.push(`${why} ${ms} ms on ${info.hash} ${JSON.stringify(info.st)}`);
  if (PROFILE) {
    try {
      const part = globalThis.__uitestPart || process.argv[1].split("/").pop().replace(/\.mjs$/, "");
      (await import("node:fs")).appendFileSync(PROFILE, `settle ${part} ${why} ${ms} ${ok ? "ok" : "TIMEOUT"} ${info.hash.replace(/\s/g, "_")}${ok ? "" : " " + JSON.stringify(info.st)}\n`);
    } catch {}
  }
}
// settleStats: the summary lines since the last call (and starts counting again), or [] when nothing waited.
export function settleStats() {
  if (!stats.n) return [];
  const lines = [`settled: ${stats.n} waits, ${(stats.ms / 1000).toFixed(1)} s in all, ${stats.timeouts.length} timed out`,
    ...stats.timeouts.map((t) => `settled TIMEOUT ${t}`)];
  stats.n = 0; stats.ms = 0; stats.timeouts = [];
  return lines;
}
process.on("exit", () => { for (const l of settleStats()) console.log(l); });

export const CATALOGS = {
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

// Mocks the launch sheet's routes. hooks.onModels(path) and
// hooks.onLaunch(body) see what the app asked for.
export async function mockLaunchRoutes(page, base, hooks = {}) {
  await page.route(/\/api\/launch\/options/, (r) => r.fulfill({ json: {
    enabled: true, kinds: ["pi", "claude", "codex", "cursor"], default_kind: "pi", model_kinds: ["pi", "claude", "codex"],
    roots: [{ name: "~/code", path: "/home/u/code", label: "~/code", git: false }],
    recent: [{ name: "proj", path: "/home/u/code/proj", label: "~/code/proj", git: true }] } }));
  await page.route(/\/api\/launch\/dirs/, (r) => r.fulfill({ json: { path: "/home/u/code", label: "~/code", dirs: [] } }));
  await page.route(/\/api\/models/, (r) => {
    hooks.onModels?.(r.request().url().replace(base, ""));
    const kind = new URL(r.request().url()).searchParams.get("kind");
    r.fulfill({ json: { host: "uitest", catalogs: CATALOGS[kind] ? [CATALOGS[kind]] : [] } });
  });
  await page.route(/\/api\/launch$/, (r) => {
    if (r.request().method() !== "POST") return r.continue();
    hooks.onLaunch?.(JSON.parse(r.request().postData() || "{}"));
    r.fulfill({ json: { agent: { pane_id: "w1:p1", name: "alpha" }, workspace_id: "w9" } });
  });
}

// dumpFailure writes what is needed to see why a check failed (a wait that timed out, say) to
// OUT/failures/<check>.txt: the whole error with Playwright's call log, the page's URL and hash, the app's
// state (ledger, review, removals, agents, the open detail), the DOM of the screens, and the last console and
// network lines. It runs only after a failure, so passing runs pay nothing, and it never throws.
// `log` is the driver's list of console and network lines; `now` stamps when each was added (see stamped()).
export async function dumpFailure({ page, out, name, err, log = [] }) {
  try {
    const dir = `${out}/failures`;
    const fs = await import("node:fs");
    fs.mkdirSync(dir, { recursive: true });
    const snap = await page.evaluate(() => {
      const t = (f) => { try { return f(); } catch (e) { return "(unavailable: " + e.message + ")"; } };
      const html = (sel, n = 3000) => t(() => { const e = document.querySelector(sel); return e ? (e.hidden ? "[hidden] " : "") + e.outerHTML.slice(0, n) : "(none)"; });
      return {
        url: location.href, hash: location.hash, visibility: document.visibilityState, active: t(() => document.activeElement && (document.activeElement.id || document.activeElement.tagName)),
        state: t(() => ({ selected: state.selected, view: state.view, agents: state.agents.map((a) => agentId(a)), es: state.es && state.es.readyState, pendingRoute: state.pendingRoute })),
        ledger: t(() => ({ on: ledger.on, inflight: ledger.inflight, loadedAt: ledger.loadedAt, timer: !!ledger.timer, marked: [...ledger.marked.keys()], entries: ledger.entries.map((e) => `${e.agent_id}:${e.status}`), optimistic: [...ledgerOpt.m].map(([k, e]) => `${k} until=${e.until}`) })),
        review: t(() => ({ open: review.open, busy: review.busy, seq: review.seq, items: review.items.map((i) => i.id), results: [...review.results.keys()], confirm: review.confirm, sel: [...review.sel] })),
        sweep: t(() => ({ busy: sweep.busy, shown: sweep.shown, marked: sweep.marked, err: sweep.err })),
        mark: t(() => ({ open: mk.open, id: mk.id, busy: mk.busy })),
        dom: { banner: html("#rtr-banner"), review: html("#review", 6000), markbar: html("#d-markbar"), markSheet: html("#mk"), detail: html("#detail", 1500), agents: html("#agents", 4000), usage: html("#usage", 1500) },
        body: t(() => document.body.className),
        toasts: t(() => [...document.querySelectorAll(".toast, #toast")].map((e) => e.textContent)),
      };
    }).catch((e) => ({ unavailable: "page.evaluate failed: " + e.message }));
    const lines = [
      `check: ${name}`, `at: ${new Date().toISOString()}`, "",
      "error:", String((err && (err.stack || err.message)) || err), "",
      "page and app state:", JSON.stringify(snap, null, 2), "",
      `last ${Math.min(log.length, 80)} console and network lines (of ${log.length}):`, ...log.slice(-80),
    ];
    fs.writeFileSync(`${dir}/${name.replace(/\W+/g, "-")}.txt`, lines.join("\n") + "\n");
  } catch { /* a failed dump must never fail the run */ }
}

// stamped wraps a log array so every line carries the time it was added (ms since `t0`), to line the
// console and network up with a timeout.
export function stamped(log, t0 = Date.now()) {
  return { push: (s) => log.push(`+${String(Date.now() - t0).padStart(6)}ms ${s}`) };
}

// A screenshot of what one selector covers, taken as a clip of the page. locator.screenshot holds an element handle,
// and the app redraws its lists on every refresh: a redraw between the handle and the shot fails the check with "Element
// is not attached to the DOM". The box is read and shot in one go instead.
export async function shotOf(page, selector, path) {
  const box = await page.evaluate((sel) => { const b = document.querySelector(sel).getBoundingClientRect(); return { x: b.left + scrollX, y: b.top + scrollY, width: b.width, height: b.height }; }, selector);
  await page.screenshot({ path, clip: box, fullPage: true });
}

// readInPage finds the first element that matches selector and reads it in one evaluate inside the page, so a redraw
// cannot land between finding it and reading it. It first waits for the app to be settled (see settled(); a page that
// does not settle in timeout ms is read anyway), then (one animation frame at a time, up to timeout ms) for the
// element to exist. Returns null when there is none by then, else { text, attrs, box, style, connected } with:
//   text   innerText, trimmed;
//   attrs  { name: value or null } for each name in opts.attrs, plus "class" when asked for;
//   box    the bounding client rect { x, y, width, height };
//   style  { prop: computed value } for each name in opts.style (CSS property names or custom properties).
// opts.all: read every match instead (an array, [] when there is none by timeout).
export async function readInPage(page, selector, { attrs = [], style = [], all = false, timeout = 2000 } = {}) {
  // settled first (within the same timeout), so what is read is not a frame from the middle of a redraw
  const t0 = Date.now();
  await settled(page, { timeout, why: "read" });
  timeout = Math.max(0, timeout - (Date.now() - t0));
  return page.evaluate(async ({ selector, attrs, style, all, timeout }) => {
    const read = (el) => {
      const r = el.getBoundingClientRect(), cs = style.length ? getComputedStyle(el) : null;
      return {
        text: (el.innerText ?? el.textContent ?? "").trim(),
        attrs: Object.fromEntries(attrs.map((a) => [a, el.getAttribute(a)])),
        box: { x: r.x, y: r.y, width: r.width, height: r.height },
        style: Object.fromEntries(style.map((p) => [p, p.startsWith("--") ? cs.getPropertyValue(p).trim() : cs[p]])),
        connected: el.isConnected,
      };
    };
    const end = performance.now() + timeout;
    for (;;) {
      const els = [...document.querySelectorAll(selector)];
      if (els.length) return all ? els.map(read) : read(els[0]);
      if (performance.now() > end) return all ? [] : null;
      await new Promise((r) => requestAnimationFrame(r));
    }
  }, { selector, attrs, style, all, timeout });
}

// clickStable clicks the first visible element that matches selector once the app is settled (settled(), up to 3 s a
// try), and the element is attached, has kept the same node and
// the same box for two animation frames, and is what the browser hits at its centre (not covered, not mid-redraw).
// The element is found again by selector on every try, never kept as a handle across a redraw. It waits up to timeout
// ms for that, then clicks with Playwright's own locator click (which re-resolves the selector again). opts.position
// and opts.button pass through. Throws, naming what it saw last, when the element never settles.
export async function clickStable(page, selector, { timeout = 8000, position, button } = {}) {
  const end = Date.now() + timeout;
  let last = "";
  for (;;) {
    await settled(page, { timeout: Math.max(100, Math.min(3000, end - Date.now())), why: "click" });
    const r = await page.evaluate(async (selector) => {
      const frame = () => new Promise((res) => requestAnimationFrame(() => res()));
      const pick = () => [...document.querySelectorAll(selector)].find((e) => e.getClientRects().length && getComputedStyle(e).visibility !== "hidden");
      const a = pick();
      if (!a) return { ok: false, why: "no visible element" };
      const r0 = a.getBoundingClientRect(), cx = r0.x + r0.width / 2, cy = r0.y + r0.height / 2;
      if (cx < 0 || cy < 0 || cx >= innerWidth || cy >= innerHeight) { a.scrollIntoView({ block: "center", inline: "center" }); return { ok: false, why: "scrolled into view" }; }
      const ra = a.getBoundingClientRect();
      await frame(); await frame();
      const b = pick();
      if (b !== a) return { ok: false, why: "redrawn (another node) while waiting for it to settle" };
      const rb = b.getBoundingClientRect();
      if (ra.x !== rb.x || ra.y !== rb.y || ra.width !== rb.width || ra.height !== rb.height) return { ok: false, why: "still moving" };
      const hit = document.elementFromPoint(rb.x + rb.width / 2, rb.y + rb.height / 2);
      if (!hit || !(hit === b || b.contains(hit))) return { ok: false, why: "covered by " + (hit ? hit.tagName + (hit.id ? "#" + hit.id : "") + (hit.className && typeof hit.className === "string" ? "." + hit.className.split(" ")[0] : "") : "nothing") };
      return { ok: true };
    }, selector).catch((e) => ({ ok: false, why: String(e.message).split("\n")[0] })); // a navigation mid-evaluate
    if (r.ok) break;
    last = r.why;
    if (Date.now() > end) throw new Error(`clickStable ${selector}: never settled (${last})`);
    await new Promise((res) => setTimeout(res, 50));
  }
  await page.locator(selector).locator("visible=true").first().click({ timeout: Math.max(1000, end - Date.now()), position, button });
}

// makeCheck returns check(name, fn), the one every script uses: it runs fn, prints "ok   NAME" or "FAIL NAME" with the
// error's lines indented under it (tools/ci/flakes.sh and run.sh read these lines), and keeps the count. On a failure
// it calls opts.onFail(name, err) (a screenshot, say), and with opts.page and opts.out it writes dumpFailure's file
// too; neither may fail the run. check.failed is the number of failed checks; check.results the [name, ok, why] list.
// ONLY=regex in the environment runs only the checks whose names match (a quick look at one).
export function makeCheck({ page, out, log, onFail } = {}) {
  const results = [];
  const check = async (name, fn) => {
    if (process.env.ONLY && !new RegExp(process.env.ONLY).test(name)) return; // ONLY=regex: just those checks
    try {
      await fn();
      results.push([name, true, ""]);
      console.log("ok   " + name);
    } catch (e) {
      check.failed++;
      const why = String((e && e.message) || e);
      results.push([name, false, why.split("\n")[0]]);
      console.log("FAIL " + name + "\n     " + why.split("\n").join("\n     "));
      const pg = typeof page === "function" ? page() : page;
      if (onFail) await Promise.resolve().then(() => onFail(name, e)).catch(() => {});
      if (pg && out) await dumpFailure({ page: pg, out, name, err: e, log: log || [] });
    }
  };
  check.failed = 0;
  check.results = results;
  return check;
}
