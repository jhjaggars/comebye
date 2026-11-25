// Shared by drive.mjs (the scripted regression run) and shot.mjs (the dev
// loop's screenshot CLI): the headless Chrome setup and the mocked routes
// for the launch sheet, so neither depends on which agents the host has.
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
// serviceWorkers: "block" by default. sw.js answers the app shell (#136 slice 5), and a request a service worker
// lets fall through to the network is not always seen by page.route, so the scripts that hold or cut /api/ with
// routes would race it; shell.mjs, which tests the worker, passes "allow".
export async function launch({ width = 390, height = 844, dark, bypassCSP = true, serviceWorkers = "block" } = {}) {
  const real = await acquire();
  const mobile = width <= 500;
  let ctx;
  try {
    ctx = await real.newContext({
      viewport: { width, height }, deviceScaleFactor: mobile ? 2 : 1, isMobile: mobile, hasTouch: mobile, bypassCSP, serviceWorkers,
      ...(mobile ? { userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1" } : {}),
      ...(dark === undefined ? {} : { colorScheme: dark ? "dark" : "light" }),
    });
  } catch (e) { release(); throw e; }
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
      if (k === "newContext") return async (...a) => { const c = await t.newContext(...a); extra.push(c); return c; };
      const v = Reflect.get(t, k);
      return typeof v === "function" ? v.bind(t) : v;
    },
  });
  return { browser, ctx, page: await ctx.newPage() };
}

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
