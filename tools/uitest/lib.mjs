// Shared by drive.mjs (the scripted regression run) and shot.mjs (the dev
// loop's screenshot CLI): the headless Chrome setup and the mocked routes
// for the launch sheet, so neither depends on which agents the host has.
import { chromium } from "playwright-core";

export const CHROME = process.env.CHROME || "/usr/bin/google-chrome";

// Launches headless Chrome with a phone-style context (390x844 by default;
// width > 500 makes a desktop one: no touch, no mobile emulation). bypassCSP:
// false keeps the servers' Content-Security-Policy in force (the Pages checks
// test the sandbox it sets).
export async function launch({ width = 390, height = 844, dark, bypassCSP = true } = {}) {
  const browser = await chromium.launch({ executablePath: CHROME, headless: true, args: ["--no-sandbox"] });
  const mobile = width <= 500;
  const ctx = await browser.newContext({
    viewport: { width, height }, deviceScaleFactor: mobile ? 2 : 1, isMobile: mobile, hasTouch: mobile, bypassCSP,
    ...(mobile ? { userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1" } : {}),
    ...(dark === undefined ? {} : { colorScheme: dark ? "dark" : "light" }),
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
