// Tap targets (#135 slice 4): on a touch device (pointer: coarse) every tappable thing has a hit area of at least
// 44 x 44, and no two hit areas overlap. The hit area may be bigger than what is drawn (a ::before hit box, web/index.html
// "Tap targets"); the drawn shape, the press layer and the return mark stay as they were. A desktop with a mouse is
// pixel-identical: at 1280 the geometry of every tappable thing is the same with the tap-target rules taken out.
//
// Per screen, at 390 x 844 and 844 x 390 with touch:
//   - every visible tappable element (the press helper's kinds, web/app.js PRESS_SEL, and selects) is found;
//   - its hit box is measured by hit-testing: elementFromPoint outwards from its edges until something else answers;
//     the box must be at least 44 x 44 and its four corners must answer to it;
//   - its declared hit box (its border box with any absolutely placed ::before / ::after) must not overlap another's;
//   - its drawn box must answer to it everywhere (no neighbour's hit box lies over it).
// Inline links in running text (chat Markdown, a page) are left out: WCAG 2.5.8 exempts them, and they can't grow.
//
//   node taptargets.mjs BASE OUT_DIR      (env: SCHEME; COMEBYE_BIN + COMEBYE_STATE_DIR to publish pages for the
//                                          Pages screen; TAPTARGETS_DUMP=FILE writes what was measured as JSON)
import { launch, clickStable, settled as settledApp } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const tag = dark ? "dark" : "light";
let failed = 0;
async function check(name, fn) {
  try { await fn(); console.log("ok   " + name); }
  catch (e) { failed++; console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const MIN = 44;
const IPHONE = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1";
const SIZES = [
  { name: "portrait", w: 390, h: 844 },
  { name: "landscape", w: 844, h: 390 },
];

// In the page: every visible tappable element, with its drawn box, its declared hit box and its probed hit box.
// Runs in one evaluate, so a poll's redraw can't detach what it measured half way.
const MEASURE = ([MIN, scope]) => {
  const SEL = 'button, a[href], summary, [role="button"], [role="tab"], [role="menuitem"], [role="option"], .agent, select, input:not([type=hidden]), textarea';
  const vw = innerWidth, vh = innerHeight;
  const at = (x, y) => document.elementFromPoint(x, y); // integer x, y: the pixel [x, x+1) x [y, y+1)
  const owns = (el, x, y) => { const t = at(x, y); return !!t && (t === el || el.contains(t)); };
  const label = (el) => {
    const id = el.id ? "#" + el.id : "";
    const cls = [...el.classList].filter((c) => !c.startsWith("is-") && c !== "was-open").slice(0, 2).map((c) => "." + c).join("");
    const txt = (el.getAttribute("aria-label") || el.textContent || "").trim().replace(/\s+/g, " ").slice(0, 24);
    return `${el.localName}${id}${cls}${txt ? ` "${txt}"` : ""}`;
  };
  const inside = (r, x, y) => x >= r.left && x < r.right && y >= r.top && y < r.bottom;
  const inlineText = (el) => el.localName === "a" && getComputedStyle(el).display === "inline";
  const root = scope ? document.querySelector(scope) : document;
  const skipped = [];
  const els = [...root.querySelectorAll(SEL)].filter((el) => {
    if (!el.getClientRects().length) return false;
    const cs = getComputedStyle(el);
    if (cs.visibility === "hidden" || cs.pointerEvents === "none" || parseFloat(cs.opacity) === 0) return false;
    if (el.closest("[inert], [aria-hidden='true']")) return false;
    if (inlineText(el)) return false; // a link in running text (WCAG 2.5.8 exempts it)
    const r = el.getBoundingClientRect();
    if (r.width < 1 || r.height < 1) return false;
    if (r.left < -0.5 || r.top < -0.5 || r.right > vw + 0.5 || r.bottom > vh + 0.5) return false; // scrolled partly away
    return true;
  });
  // What answers at a spot of el's drawn box, when it is not el: a nested tappable (fine), something drawn over el (el
  // is under a sheet, a sticky header or the composer: not measured), or another tappable's hit box reaching over el
  // (an overlap: reported).
  const nestedOf = (el) => [...el.querySelectorAll(SEL)].filter((n) => n.getClientRects().length);
  const survey = (el) => {
    const r = el.getBoundingClientRect(), nested = nestedOf(el), stolen = [];
    let under = false;
    for (const fx of [0.1, 0.5, 0.9]) for (const fy of [0.1, 0.5, 0.9]) {
      const x = Math.floor(r.left + r.width * fx), y = Math.floor(r.top + r.height * fy);
      const t = at(x, y);
      if (t && (t === el || el.contains(t) || nested.some((n) => n.contains(t)))) continue;
      if (t && t.contains(el)) continue; // a rounded corner: the spot belongs to what el sits in
      const by = t?.closest(SEL);
      if (by && !by.contains(el) && !inside(by.getBoundingClientRect(), x, y)) stolen.push(label(by));
      else under = true;
    }
    return { under, stolen: [...new Set(stolen)] };
  };
  const pseudo = (el, which) => {
    const cs = getComputedStyle(el, which);
    if (cs.content === "none" || cs.content === "normal" || cs.position !== "absolute" || cs.display === "none" || cs.pointerEvents === "none") return null;
    const host = getComputedStyle(el);
    if (host.position === "static") return null; // its box belongs to another containing block: not a hit box of el
    const r = el.getBoundingClientRect();
    const bl = parseFloat(host.borderLeftWidth), bt = parseFloat(host.borderTopWidth);
    const pl = r.left + bl, pt = r.top + bt, pw = r.width - bl - parseFloat(host.borderRightWidth), ph = r.height - bt - parseFloat(host.borderBottomWidth);
    const px = (v) => (v === "auto" ? null : parseFloat(v));
    const L = px(cs.left), R = px(cs.right), T = px(cs.top), B = px(cs.bottom), W = parseFloat(cs.width), H = parseFloat(cs.height);
    const l = L !== null ? pl + L : R !== null ? pl + pw - R - W : pl;
    const t = T !== null ? pt + T : B !== null ? pt + ph - B - H : pt;
    return { l, t, r: l + W, b: t + H };
  };
  // The hit box, by hit-testing whole pixels outwards from the drawn box along its centre lines.
  const probe = (el, r) => {
    const cx = Math.floor(r.left + r.width / 2), cy = Math.floor(r.top + r.height / 2), far = MIN;
    let l = Math.ceil(r.left); while (l > 0 && r.left - l < far && owns(el, l - 1, cy)) l--;
    let rr = Math.ceil(r.right) - 1; while (rr < vw - 1 && rr + 1 - r.right < far && owns(el, rr + 1, cy)) rr++;
    let t = Math.ceil(r.top); while (t > 0 && r.top - t < far && owns(el, cx, t - 1)) t--;
    let b = Math.ceil(r.bottom) - 1; while (b < vh - 1 && b + 1 - r.bottom < far && owns(el, cx, b + 1)) b++;
    return { l, t, r: rr + 1, b: b + 1 };
  };
  const res = [];
  els.forEach((el, i) => { el.dataset.ttIdx = String(i); });
  for (const [i, el] of els.entries()) {
    const sv = survey(el);
    if (sv.under) { skipped.push(label(el)); continue; }
    const r = el.getBoundingClientRect();
    const drawn = { l: r.left, t: r.top, r: r.right, b: r.bottom };
    const decl = { ...drawn };
    for (const w of ["::before", "::after"]) {
      const p = pseudo(el, w);
      if (p) { decl.l = Math.min(decl.l, p.l); decl.t = Math.min(decl.t, p.t); decl.r = Math.max(decl.r, p.r); decl.b = Math.max(decl.b, p.b); }
    }
    const anc = []; for (let p = el.parentElement; p; p = p.parentElement) if (p.dataset.ttIdx !== undefined) anc.push(+p.dataset.ttIdx);
    // A hit box must not reach past the control's right edge: that is scrollable overflow, so the control (and its
    // row) would scroll sideways. drive.mjs's sideways checks and a phone both notice. To the left it may grow.
    const out = decl.r > drawn.r + 0.5 ? `${Math.round(decl.r - drawn.r)}px past its right edge` : "";
    res.push({ i, label: label(el), drawn, decl, hit: probe(el, r), stolen: sv.stolen, anc, out });
  }
  els.forEach((el) => { delete el.dataset.ttIdx; });
  return { ms: res, skipped };
};

const fmt = (b) => `${Math.round(b.r - b.l)}x${Math.round(b.b - b.t)}`;
const overlap = (a, b) => Math.max(0, Math.min(a.r, b.r) - Math.max(a.l, b.l)) * Math.max(0, Math.min(a.b, b.b) - Math.max(a.t, b.t));

function verdict(ms, vw, vh) {
  const why = [];
  for (const m of ms) {
    // At a screen edge a finger can't go further: the hit box is as big as it can be there.
    const w = m.hit.r - m.hit.l + (m.hit.l <= 0 || m.hit.r >= vw ? MIN : 0);
    const h = m.hit.b - m.hit.t + (m.hit.t <= 0 || m.hit.b >= vh ? MIN : 0);
    if (w < MIN || h < MIN) why.push(`${m.label}: hit box ${fmt(m.hit)} (drawn ${fmt(m.drawn)}), under ${MIN}x${MIN}`);
    if (m.out) why.push(`${m.label}: its hit box sticks out sideways, ${m.out}`);
    if (m.stolen.length) why.push(`${m.label}: its drawn box answers to ${m.stolen.join(", ")}`);
  }
  for (let i = 0; i < ms.length; i++) for (let j = i + 1; j < ms.length; j++) {
    const a = ms[i], b = ms[j];
    if (a.anc.includes(b.i) || b.anc.includes(a.i)) continue;
    const o = overlap(a.decl, b.decl);
    if (o > 1) why.push(`hit boxes overlap (${Math.round(o)} px²): ${a.label} ${fmt(a.decl)} and ${b.label} ${fmt(b.decl)}`);
  }
  return why;
}

// settled: every finite animation (a sheet sliding in, a fade) has ended, and no fetch or redraw is pending.
const settled = (page) => settledApp(page); // lib.mjs: no fetch in flight, no redraw pending, no finite animation running

// ---- screens ----
// Each: [name, hash, ready selector, setup?, scope?]. setup runs after the screen is ready (a sheet opened, a tab
// picked); scope, for a sheet or a dialog, measures only what is in it (what is under it is not tappable then).
const firstAgent = async (page) => { await clickStable(page, ".agent.idle"); await page.waitForSelector("#btn-send", { state: "visible" }); };
const SCREENS = [
  ["list", "#/list", ".agent"],
  ["list (bottom)", "#/list", ".agent", async (page) => { await page.evaluate(() => { const s = document.scrollingElement; s.scrollTop = s.scrollHeight; for (const e of document.querySelectorAll("*")) if (e.scrollHeight > e.clientHeight + 40 && /auto|scroll/.test(getComputedStyle(e).overflowY)) e.scrollTop = e.scrollHeight; }); }],
  ["row menu", "#/list", ".agent", async (page) => { await clickStable(page, ".row-more"); await page.waitForSelector("#row-transcript", { state: "visible" }); }, "#row-menu"],
  ["palette", "#/list", ".agent", async (page) => { await page.locator("#btn-cmds").click(); await page.waitForSelector(".pal-it", { state: "visible" }); }, "#palette"],
  ["agent", "#/list", ".agent", firstAgent],
  ["agent menu", "#/list", ".agent", async (page) => { await firstAgent(page); await page.locator("#d-more").click(); await page.waitForSelector("#d-menu:not([hidden])"); }, "#d-menu"],
  ["side panel", "#/list", ".agent", async (page) => { await firstAgent(page); await page.locator("#sp-btn").click(); await page.waitForSelector(".sp-tab", { state: "visible" }); }],
  ["todo", "#/todo", ".td-row"],
  ["to-do item", "#/list", "#todo .td-toggle", async (page) => {
    if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await clickStable(page, "#todo .td-toggle");
    await clickStable(page, "#todo .td-row");
    await page.waitForSelector("#todo .td-item.open", { state: "visible" });
    await page.evaluate(() => document.querySelector("#todo .td-item.open").scrollIntoView({ block: "start" }));
  }],
  ["new agent", "#/list", ".agent", async (page) => { await page.locator("#btn-launch").click(); await page.waitForSelector("#launch", { state: "visible" }); }, "#launch"],
  ["status", "#/status", "#status, .bd-lane, main"],
  ["pages", "#/pages", "#pgs-back"],
  ["peers", "#/peers", "#pr-back"],
  ["releases", "#/peers", "#pr-tab-rel", async (page) => { await page.locator("#pr-tab-rel").click(); await page.waitForSelector(".rel-refresh", { state: "visible" }); }],
  ["settings", "#/settings", "#back, .back, main"],
  ["archived", "#/archived", ".back, main"],
  ["offline", "#/list", ".agent", async (page, ctx) => { await ctx.setOffline(true); await page.waitForSelector("#hub-strip:not([hidden]) .hs-retry", { state: "visible", timeout: 15000 }); }],
];

// Pages, so the Pages screen has cards and chips (with the CLI at hand).
if (process.env.COMEBYE_BIN && process.env.COMEBYE_STATE_DIR) {
  const live = (await (await fetch(base + "/api/agents")).json()).agents.filter((a) => a.pane_id);
  const work = fs.mkdtempSync(path.join(os.tmpdir(), "taptargets-ui-"));
  for (const t of ["Tap target page", "Another tap target page"]) {
    const f = path.join(work, "p.html");
    fs.writeFileSync(f, `<!doctype html><title>${t}</title><h1>${t}</h1>`);
    execFileSync(process.env.COMEBYE_BIN, ["publish", f, "--title", t], { env: { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: process.env.COMEBYE_STATE_DIR, HERDR_PANE_ID: live[0].pane_id }, stdio: "ignore" });
  }
  fs.rmSync(work, { recursive: true, force: true });
}
// A to-do, so the To do screen has rows (the hub may have none).
await fetch(base + "/api/todos", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ question: "Tap target test " + Date.now(), kind: "decision", no_wait: true, context: "seeded by taptargets.mjs" }) });

const dump = [];
const { browser, ctx: first } = await launch({ dark });
await first.close().catch(() => {});
try {
  // ---- touch: every hit box at least 44 x 44, none overlapping ----
  for (const s of SIZES) {
    for (const [screen, hash, ready, setup, scope] of SCREENS) {
      await check(`${s.name} ${s.w}x${s.h} ${tag}: ${screen}: every tappable thing has a 44x44 hit box, and none overlap`, async () => {
        const ctx = await browser.newContext({ viewport: { width: s.w, height: s.h }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, bypassCSP: true, colorScheme: dark ? "dark" : "light", userAgent: IPHONE });
        try {
          const page = await ctx.newPage();
          page.setDefaultTimeout(8000);
          await page.goto(base + "/" + hash, { waitUntil: "load" });
          await page.waitForSelector(ready, { state: "visible" });
          if (setup) await setup(page, ctx);
          await settled(page);
          const coarse = await page.evaluate(() => matchMedia("(pointer: coarse)").matches);
          if (!coarse) throw new Error("this context is not (pointer: coarse)");
          const { ms, skipped } = await page.evaluate(MEASURE, [MIN, scope || ""]);
          dump.push({ size: s.name, screen, ms, skipped });
          if (!ms.length) throw new Error("nothing tappable found: the screen did not load");
          const why = verdict(ms, s.w, s.h);
          if (why.length) {
            await page.screenshot({ path: `${out}/taptargets-${s.name}-${screen.replace(/\W+/g, "-")}-${tag}.png` }).catch(() => {});
            throw new Error(why.join("\n"));
          }
        } finally { await ctx.close().catch(() => {}); }
      });
    }
  }

  // ---- a mouse at 1280: nothing changed ----
  // The tap-target rules all sit in one @media (pointer: coarse) block (the one that sets --tap-hit). With a mouse the
  // geometry of every tappable thing, and the page's size, are the same with that block taken out of the stylesheet.
  const DESK = [
    ...SCREENS.filter(([n]) => ["list", "status", "releases", "archived", "new agent"].includes(n)),
    ["agent", "#/list", ".agent"], ["side panel", "#/list", ".agent"],
    ["todo", "#/todo", "#p-todo"], ["peers", "#/peers", ".pr-card"], ["pages", "#/pages", ".pgs-card, .pgs-zero"],
  ];
  for (const [screen, hash, ready, setup] of DESK) {
    await check(`desktop 1280x800 ${tag}: ${screen}: with a mouse the tap-target rules change nothing`, async () => {
      const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, deviceScaleFactor: 1, bypassCSP: true, colorScheme: dark ? "dark" : "light" });
      try {
        const page = await ctx.newPage();
        page.setDefaultTimeout(8000);
        await page.goto(base + "/" + hash, { waitUntil: "load" });
        await page.waitForSelector(ready, { state: "visible" });
        if (setup) await setup(page, ctx);
        if (screen === "agent" || screen === "side panel") {
          await clickStable(page, ".agent.idle");
          await page.waitForSelector("#btn-send", { state: "visible" });
          if (screen === "side panel") { await page.locator("#sp-btn").click(); await page.waitForSelector(".sp-tab", { state: "visible" }); }
        }
        await settled(page);
        const geo = () => page.evaluate(() => {
          const SEL = 'button, a[href], summary, [role="button"], [role="tab"], [role="menuitem"], [role="option"], .agent, select, input:not([type=hidden]), textarea';
          const r = (e) => { const b = e.getBoundingClientRect(); return [b.left, b.top, b.width, b.height].map((v) => Math.round(v * 100) / 100).join(","); };
          const pse = (e) => { const c = getComputedStyle(e, "::before"); return c.content === "none" ? "" : `${c.position}:${c.top},${c.left},${c.width},${c.height}`; };
          return { coarse: matchMedia("(pointer: coarse)").matches, size: `${document.documentElement.scrollWidth}x${document.documentElement.scrollHeight}`,
            els: [...document.querySelectorAll(SEL)].filter((e) => e.getClientRects().length).map((e) => `${e.localName}#${e.id}.${e.className}|${r(e)}|${pse(e)}|${getComputedStyle(e).position}`) };
        });
        // Take the block out and put it back: with, without, with. A page still filling in (a list arriving) changes
        // between the two "with" reads: then it is read again, until it holds still.
        const toggle = (on) => page.evaluate((on) => {
          for (const sh of document.styleSheets) {
            let rules; try { rules = sh.cssRules; } catch { continue; }
            if (on) { if (window.__tapRule && window.__tapRule.sh === sh) { sh.insertRule(window.__tapRule.text, window.__tapRule.i); return true; } continue; }
            for (let i = rules.length - 1; i >= 0; i--) {
              const r = rules[i];
              if (r instanceof CSSMediaRule && /pointer:\s*coarse/.test(r.conditionText) && r.cssText.includes("--tap-hit:")) { window.__tapRule = { sh, i, text: r.cssText }; sh.deleteRule(i); return true; }
            }
          }
          return false;
        }, on);
        const frames = () => page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
        let a, b;
        for (let tries = 0; ; tries++) {
          a = await geo();
          if (a.coarse) throw new Error("a mouse context matched (pointer: coarse)");
          if (!(await toggle(false))) throw new Error("the tap-target block (@media (pointer: coarse) with --tap-hit) is not in the stylesheet");
          await frames();
          b = await geo();
          await toggle(true);
          await frames();
          const c = await geo();
          if (JSON.stringify(a) === JSON.stringify(c)) break;
          if (tries === 4) throw new Error("the page never held still for two reads");
          await page.waitForTimeout(300);
        }
        if (a.size !== b.size) throw new Error(`page size ${a.size} with the rules, ${b.size} without`);
        const diff = a.els.filter((x, i) => x !== b.els[i]);
        if (a.els.length !== b.els.length || diff.length) throw new Error(`geometry differs:\n${diff.slice(0, 8).map((x) => x + "\n  without: " + b.els[a.els.indexOf(x)]).join("\n")}`);
        if (a.els.length < 3) throw new Error("only " + a.els.length + " tappable things: the screen did not load");
      } finally { await ctx.close().catch(() => {}); }
    });
  }
} finally {
  if (process.env.TAPTARGETS_DUMP) fs.writeFileSync(process.env.TAPTARGETS_DUMP, JSON.stringify(dump, null, 1));
  await browser.close();
}
console.log(failed ? `taptargets: ${failed} failed` : "taptargets: all passed");
process.exit(failed ? 1 : 0);
