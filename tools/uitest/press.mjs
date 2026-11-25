// The press layer (#135): every tappable kind shows a pressed look while a finger is down, at phone size, driven with
// real touch input (CDP Input.dispatchTouchEvent, which fires pointer events as a phone does). Checks, per kind: the
// layer is there while held, still there just after a quick release (the 100 ms minimum), gone after the fade. Then: a
// scroll gesture never shows it, the layer draws over a surface's own gradient (a blocked row's glow), a held row inside
// a scroller waits ~60 ms, reduced motion drops the fade, a redraw of the list under the finger keeps the layer, and no
// highlight stays on a card after a tap on a device without hover (every :hover rule sits in @media (hover: hover)).
//
//   node press.mjs BASE OUT_DIR      (env: SCHEME; COMEBYE_BIN + COMEBYE_STATE_DIR to publish a page for the Pages card)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const tag = dark ? "dark" : "light";
const { browser, page } = await launch({ dark });
page.setDefaultTimeout(8000);
const errors = [];
page.on("pageerror", (e) => errors.push("pageerror: " + e.message));
let failed = 0;
async function check(name, fn) {
  try { await fn(); console.log("ok   " + name); }
  catch (e) { failed++; console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const post = (p, body) => fetch(base + p, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) }).then((r) => r.json());

// Seed: a to-do (for the To do rows) and, when the CLI is at hand, a page (for the Pages card).
await post("/api/todos", { question: "Press test " + Date.now(), kind: "decision", no_wait: true, context: "seeded by press.mjs" });
if (process.env.COMEBYE_BIN && process.env.COMEBYE_STATE_DIR) {
  const live = (await (await fetch(base + "/api/agents")).json()).agents.filter((a) => a.pane_id);
  const work = fs.mkdtempSync(path.join(os.tmpdir(), "press-ui-"));
  const f = path.join(work, "p.html");
  fs.writeFileSync(f, "<!doctype html><title>Press test page</title><h1>Press test page</h1>");
  execFileSync(process.env.COMEBYE_BIN, ["publish", f, "--title", "Press test page"], { env: { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: process.env.COMEBYE_STATE_DIR, HERDR_PANE_ID: live[0].pane_id }, stdio: "ignore" });
}

const cdp = await page.context().newCDPSession(page);
const touch = (type, x, y) => cdp.send("Input.dispatchTouchEvent", { type, touchPoints: type === "touchEnd" || type === "touchCancel" ? [] : [{ x, y, id: 1 }] });
// Taps must not navigate or run their action, so each kind is pressed where it stands: a capture listener on window
// swallows the click the release makes. (The press layer works on pointer events, which this leaves alone.)
await page.addInitScript(() => addEventListener("click", (e) => { if (window.__noclick) { e.preventDefault(); e.stopImmediatePropagation(); } }, true));

// What the press layer looks like on an element now: is it drawn, and with which colour. One function in the page
// (__layerOf), used by layer(handle) and by readKey(key).
await page.addInitScript(() => {
  window.__layerOf = (el) => {
    const cs = getComputedStyle(el);
    const c = cs.getPropertyValue("--press-c").trim();
    const a = /rgba?\(([^)]+)\)/.exec(c)?.[1].split(/[ ,/]+/).filter(Boolean);
    const alpha = c.startsWith("color(") ? parseFloat(c.split("/")[1] ?? "1") : a ? (a.length > 3 ? parseFloat(a[3]) : 1) : 0;
    // A surface may have a gradient of its own (--grad: a blocked row's edge glow, web/index.html "Gradients"); the layer
    // is drawn over it, so the layer is one background layer more than the surface's own.
    // Layers that draw: "none" (the fallback when a surface has no --grad) is a layer that draws nothing.
    const layers = (v) => { const parts = []; let d = 0, cur = ""; for (const ch of v || "") { if (ch === "(") d++; else if (ch === ")") d--; if (ch === "," && !d) { parts.push(cur); cur = ""; } else cur += ch; } parts.push(cur); return parts.filter((x) => x.trim() && x.trim() !== "none").length; };
    const grad = cs.getPropertyValue("--grad").trim();
    return { pressed: el.classList.contains("is-pressed"), released: el.classList.contains("is-released"), img: cs.backgroundImage, n: layers(cs.backgroundImage), own: layers(grad), grad, alpha, c, transition: cs.transitionDuration, bg: cs.backgroundColor };
  };
});
const layer = (h) => h.evaluate((el) => window.__layerOf(el));
// readKey: the same, for the element that has key now, found and read in one evaluate (a poll's redraw can land between
// two calls on a loaded runner and detach the element the first one found). It waits up to ~2 s for the element.
const readKey = (key) => page.evaluate(async (key) => {
  for (let i = 0; i < 120; i++) {
    const el = document.querySelector(key);
    if (el) return window.__layerOf(el);
    await new Promise((r) => requestAnimationFrame(r));
  }
  return { pressed: false, released: false, img: "", n: -1, own: -1, alpha: 0, c: "", transition: "", bg: "", missing: key };
}, key);
const boxKey = (key, center) => page.evaluate(async ([key, center]) => {
  for (let i = 0; i < 120; i++) {
    const el = document.querySelector(key);
    if (el) {
      if (center) el.scrollIntoView({ block: "center" });
      const r = el.getBoundingClientRect();
      return { x: r.left, y: r.top, width: r.width, height: r.height };
    }
    await new Promise((r) => requestAnimationFrame(r));
  }
  return null;
}, [key, center]);
// A scroll during a held touch ends the press on purpose (pressEnd(true)): a poll's redraw that moves the layout can
// make one (scroll anchoring). scrolled() says whether any scroll happened since the last call; a gesture that failed
// with a scroll in it is that, not the helper, and is tried once more.
const scrolled = () => page.evaluate(() => {
  if (!window.__scN) { window.__scN = { n: 0 }; addEventListener("scroll", () => { window.__scN.n++; }, { capture: true, passive: true }); }
  const n = window.__scN.n; window.__scN.n = 0; return n > 0;
});
// fn calls scrolled() itself just before its touchStart, so a scroll it made to set up does not count.
async function gesture(fn) {
  try { return await fn(); }
  catch (e) {
    if (!(await scrolled())) throw e;
    console.log("     (a scroll during the gesture cancelled it, as it should: once more)");
    await page.waitForTimeout(300);
    return await fn();
  }
}
const shown = (l) => l.pressed && l.n === l.own + 1 && l.img.includes("gradient") && l.alpha > 0.05;
const gone = (l) => !l.pressed && !l.released && l.n === l.own;

async function go(hash, ready) {
  await page.goto("about:blank");
  await page.goto(base + "/" + hash, { waitUntil: "load" });
  await page.waitForSelector(ready, { state: "visible" });
  await page.evaluate(() => { window.__noclick = true; });
  await page.waitForTimeout(250);
}
// follow(h): a getter for the element h stands for now. The agent list re-renders its rows on every poll, so a handle
// taken before a poll points at a detached element (empty computed style, no box); the getter finds its replacement by
// the same key the press helper uses (tag, first class, data-id / data-name / data-key / id), or keeps h without one.
async function follow(h) {
  const key = await h.evaluate((el) => {
    for (const a of ["data-id", "data-name", "data-key", "id"]) {
      const v = el.getAttribute(a);
      if (v) return `${el.localName}${el.classList[0] && !el.classList[0].startsWith("is-") ? "." + CSS.escape(el.classList[0]) : ""}[${a}="${CSS.escape(v)}"]`;
    }
    return "";
  });
  // A redraw is one task, but a list waiting on a slow hub can be between rows for a moment: wait for it, briefly.
  const cur = async () => (key && (await page.waitForSelector(key, { state: "attached", timeout: 2000 }).catch(() => null))) || h;
  cur.read = () => (key ? readKey(key) : layer(h));
  // center(): scroll it to the middle; box(): its box. Each finds the element and acts in one evaluate, as read() does.
  cur.center = () => (key ? boxKey(key, true) : h.evaluate((el) => el.scrollIntoView({ block: "center" })));
  cur.box = () => (key ? boxKey(key, false) : h.boundingBox());
  return cur;
}
let shot = 0;
// press(h): touch the element's centre, hold ~130 ms, release; checks the layer through it.
async function press(name, sel, { opts = {} } = {}) {
  const h0 = await page.locator(sel).locator("visible=true").first().elementHandle();
  if (!h0) throw new Error("no visible " + sel);
  const cur = await follow(h0);
  await cur.center();
  await page.waitForTimeout(150);
  const b = await cur.box();
  const x = b.x + Math.min(b.width / 2, 40), y = b.y + b.height / 2;
  const rest = await cur.read();
  if (rest.pressed || rest.n !== rest.own) throw new Error("drawn before the press: " + JSON.stringify(rest));
  await scrolled();
  await touch("touchStart", x, y);
  await page.waitForTimeout(110);
  const held = await cur.read();
  if (shot < 99 && opts.shot !== false) {
    const pad = 16, vw = page.viewportSize();
    const clip = { x: Math.max(0, b.x - pad), y: Math.max(0, b.y - pad), width: Math.min(vw.width - Math.max(0, b.x - pad), b.width + 2 * pad), height: Math.min(b.height + 2 * pad, vw.height) };
    await page.screenshot({ path: `${out}/press-${tag}-${name.replace(/\W+/g, "-")}.png`, clip }).catch(() => {});
    shot++;
  }
  await touch("touchEnd");
  const after = await cur.read(); // just after the release: kept (100 ms minimum since it showed, or the fade)
  await page.waitForTimeout(450);
  const end = await cur.read();
  if (!shown(held)) throw new Error("not drawn while held: " + JSON.stringify(held));
  if (!(after.pressed || after.released)) throw new Error("gone at once on release: " + JSON.stringify(after));
  if (!gone(end)) throw new Error("still drawn 450 ms after release: " + JSON.stringify(end));
  return held;
}

// ---- every tappable kind in the research table (pg_2e34e37fcf) ----
const KINDS = [
  ["list", "#/list", ".agent", [
    ["agent row (idle)", ".agent.idle"], ["agent row (blocked)", ".agent.blocked"], ["agent row (working)", ".agent.working"],
    ["row kebab", ".row-more"], ["group header", "button.proj-head"], ["header icon", "#btn-cmds"], ["filter tab", ".lf-tab"],
    ["ready-to-remove banner", "#rtr-banner"], ["to-do toggle", ".td-toggle"],
  ]],
  ["todo", "#/todo", ".td-row", [["to-do row", ".td-row"]]],
  ["agent", "#/list", ".agent", [["back", "#back"], ["send", "#btn-send"], ["agent kebab", "#d-more"], ["side panel button", "#sp-btn"]], async () => {
    await page.evaluate(() => { window.__noclick = false; });
    await page.locator(".agent.idle").first().click();
    await page.waitForSelector("#btn-send", { state: "visible" });
    await page.evaluate(() => { window.__noclick = true; });
  }],
  ["side panel", "#/list", ".agent", [["side panel tab", ".sp-tab"]], async () => {
    await page.evaluate(() => { window.__noclick = false; });
    await page.locator(".agent.idle").first().click();
    await page.locator("#sp-btn").click();
    await page.waitForSelector(".sp-tab", { state: "visible" });
    await page.evaluate(() => { window.__noclick = true; });
  }],
  ["peers", "#/peers", ".pr-card", [["Peers card", ".pr-card"], ["Peers tab", "#pr-tab-rel"], ["screen back", "#pr-back"]]],
  ["releases", "#/peers", "#pr-tab-rel", [["Releases refresh", ".rel-refresh"]], async () => {
    await page.evaluate(() => { window.__noclick = false; });
    await page.locator("#pr-tab-rel").click();
    await page.waitForSelector(".rel-refresh", { state: "visible" });
    await page.evaluate(() => { window.__noclick = true; });
  }],
  ["pages", "#/pages", "#pgs-back", [["Pages card", ".pgs-card"]], null, () => !!process.env.COMEBYE_BIN],
  ["palette", "#/list", ".agent", [["palette item", ".pal-it"]], async () => {
    await page.evaluate(() => { window.__noclick = false; });
    await page.locator("#btn-cmds").click();
    await page.waitForSelector(".pal-it", { state: "visible" });
    await page.evaluate(() => { window.__noclick = true; });
  }],
  ["row menu", "#/list", ".agent", [["menu row", "#row-transcript"]], async () => {
    await page.evaluate(() => { window.__noclick = false; });
    await page.locator(".row-more").first().click();
    await page.waitForSelector("#row-transcript", { state: "visible" });
    await page.evaluate(() => { window.__noclick = true; });
  }],
];
for (const [screen, hash, ready, kinds, setup, when] of KINDS) {
  if (when && !when()) { console.log(`skip ${screen}: no COMEBYE_BIN to publish a page`); continue; }
  for (const [name, sel] of kinds) {
    await check(`press: ${name} (${screen})`, async () => {
      await go(hash, ready);
      if (setup) await setup();
      // The banner only shows while a session is marked ready to remove; another script on the shared server may have removed it.
      if (sel === "#rtr-banner" && !(await page.locator(sel).isVisible())) { console.log("     (no session is marked ready to remove: banner not shown)"); return; }
      await gesture(() => press(name, sel));
    });
  }
}

// The blocked row: its own orange background stays under the layer (the old .agent:active lost to .agent.blocked).
await check("press: a blocked row keeps its colour under the layer", async () => {
  await go("#/list", ".agent.blocked");
  const cur = await follow(await page.locator(".agent.blocked").first().elementHandle());
  await cur.center();
  await page.waitForTimeout(150);
  const bg0 = (await cur.read()).bg;
  const b = await cur.box();
  await touch("touchStart", b.x + 30, b.y + b.height / 2);
  await page.waitForTimeout(110);
  const l = await cur.read(), bg1 = l.bg;
  await touch("touchEnd");
  if (bg1 !== bg0) throw new Error(`background changed ${bg0} -> ${bg1}`);
  if (!shown(l)) throw new Error("no layer: " + JSON.stringify(l));
});

// A surface's own gradient (Surfaces: Layered): the layer draws over a blocked row's edge glow, it does not replace it,
// and the glow is still there after the release.
await check("press: a held blocked row shows the layer over its edge glow", () => gesture(async () => {
  await go("#/list", ".agent.blocked");
  const cur = await follow(await page.locator(".agent.blocked").first().elementHandle());
  await cur.center();
  await page.waitForTimeout(150);
  const rest = await cur.read();
  if (rest.own < 1 || !rest.img.startsWith("linear-gradient(90deg")) throw new Error("the blocked row has no glow at rest: " + JSON.stringify(rest));
  const b = await cur.box();
  await scrolled();
  await touch("touchStart", b.x + 30, b.y + b.height / 2);
  await page.waitForTimeout(110);
  const l = await cur.read();
  await page.screenshot({ path: `${out}/press-${tag}-blocked-glow.png`, clip: { x: 0, y: Math.max(0, b.y - 8), width: page.viewportSize().width, height: b.height + 16 } }).catch(() => {});
  await touch("touchEnd");
  await page.waitForTimeout(450);
  const end = await cur.read(); // the row as it is now (a poll may have re-rendered it)
  if (!shown(l)) throw new Error("no layer: " + JSON.stringify(l));
  if (!l.img.includes("linear-gradient(90deg")) throw new Error("the glow went under the layer: " + l.img);
  if (end.img !== rest.img) throw new Error(`the row did not come back as it was: ${rest.img} -> ${end.img}`);
}));

// A poll re-renders the agent rows (renderList replaces them): one that lands while a row is held, or fading after the
// release, must not drop the layer or leave it stuck on the new row. Forced here, so it is not left to a poll's timing.
await check("press: a list redraw while a row is held keeps the layer, and it still fades after", () => gesture(async () => {
  await go("#/list", ".agent.idle");
  const cur = await follow(await page.locator(".agent.idle").first().elementHandle());
  await cur.center();
  await page.waitForTimeout(150);
  const b = await cur.box();
  await scrolled();
  await touch("touchStart", b.x + 40, b.y + b.height / 2);
  let up = false;
  try {
    // Held: wait for the layer to show (60 ms in a scroller; longer on a loaded runner) before the redraw.
    await page.waitForSelector(".agent.is-pressed", { state: "attached", timeout: 1000 }).catch(() => { throw new Error("the held row never showed the layer"); });
    const [old, again] = await page.evaluate(async () => { const o = document.querySelector(".agent.is-pressed"); renderList(); await Promise.resolve(); return [o && !o.isConnected, !!document.querySelector(".agent.is-pressed")]; });
    if (!old) throw new Error("the redraw did not replace the held row (the check proves nothing)");
    if (!again) throw new Error("the layer was dropped by the redraw");
    const first = await cur.read();
    if (!shown(first)) throw new Error("not drawn on the new row: " + JSON.stringify({ first, then: await cur.read(), rows: await page.evaluate(() => [...document.querySelectorAll(".agent.is-pressed, .agent.is-released")].map((e) => e.className)) }));
    await touch("touchEnd"); up = true;
  } finally { if (!up) await touch("touchEnd"); } // a finger left down would break the checks after this one
  await page.waitForTimeout(30);
  await page.evaluate(async () => { renderList(); await Promise.resolve(); }); // and once more during the fade
  const mid = await cur.read();
  if (!(mid.pressed || mid.released)) throw new Error("the fade was dropped by a redraw: " + JSON.stringify(mid));
  await page.waitForTimeout(450);
  const end = await cur.read();
  if (!gone(end)) throw new Error("stuck on the new row 450 ms after release: " + JSON.stringify(end));
}));

// ---- a scroll is not a press ----
await check("scroll: a swipe over the rows never draws the layer, and the list scrolls", async () => {
  await page.setViewportSize({ width: 390, height: 520 }); // the fixture list must be taller than the screen
  await go("#/list", ".agent");
  const scrolls = await page.evaluate(() => document.scrollingElement.scrollHeight > innerHeight + 50);
  if (!scrolls) throw new Error("the list does not scroll at 390x520");
  await page.evaluate(() => {
    window.__seen = [];
    new MutationObserver((ms) => { for (const m of ms) if (m.target.classList?.contains("is-pressed")) window.__seen.push(m.target.className); })
      .observe(document.body, { subtree: true, attributes: true, attributeFilter: ["class"] });
  });
  const b = await (await follow(await page.locator(".agent").nth(1).elementHandle())).box();
  const x = b.x + 60; let y = b.y + b.height / 2;
  const y0 = await page.evaluate(() => scrollY);
  await touch("touchStart", x, y);
  for (let i = 0; i < 12; i++) { await page.waitForTimeout(8); y -= 18; await touch("touchMove", x, y); }
  await touch("touchEnd");
  await page.waitForTimeout(300);
  const [seen, y1] = await page.evaluate(() => [window.__seen, scrollY]);
  if (seen.length) throw new Error("the layer showed during a scroll on: " + seen.join(" | "));
  if (y1 === y0) throw new Error("the page did not scroll (scrollY stayed " + y0 + ")");
});

await check("scroll: inside a scroller a held row waits ~60 ms before the layer shows", () => gesture(async () => {
  const cur = await follow(await page.locator(".agent").nth(1).elementHandle());
  await cur.center();
  await page.waitForTimeout(150);
  const b = await cur.box();
  await scrolled();
  await touch("touchStart", b.x + 60, b.y + b.height / 2);
  const early = await cur.read(); // right after the touch: not yet
  await page.waitForTimeout(120);
  const late = await cur.read();
  await touch("touchEnd");
  await page.waitForTimeout(400);
  if (early.pressed) throw new Error("shown at once inside a scroller");
  if (!shown(late)) throw new Error("not shown after 120 ms: " + JSON.stringify(late));
}));

await check("scroll: a quick tap inside a scroller still shows the layer for its minimum", () => gesture(async () => {
  const cur = await follow(await page.locator(".agent").nth(1).elementHandle());
  const b = await cur.box();
  await scrolled();
  await touch("touchStart", b.x + 60, b.y + b.height / 2);
  await touch("touchEnd"); // released before the 60 ms delay
  const l = await cur.read();
  await page.waitForTimeout(450);
  const end = await cur.read();
  if (!shown(l)) throw new Error("a quick tap showed nothing: " + JSON.stringify(l));
  if (!gone(end)) throw new Error("still drawn after: " + JSON.stringify(end));
}));
await page.setViewportSize({ width: 390, height: 844 });

// ---- fade, and reduced motion ----
await check("fade: 180 ms normally, none with prefers-reduced-motion", async () => {
  await go("#/list", "#btn-cmds");
  const h = await page.locator("#btn-cmds").elementHandle();
  const b = await h.boundingBox();
  const fadeOf = async () => {
    await touch("touchStart", b.x + b.width / 2, b.y + b.height / 2);
    await page.waitForTimeout(250); // past the scroller delay (60 ms) and the minimum (100 ms)
    await touch("touchEnd");
    const l = await layer(h);
    await page.waitForTimeout(450);
    return l;
  };
  const normal = await fadeOf();
  if (!normal.released || normal.transition !== "0.18s") throw new Error("fade: " + JSON.stringify(normal));
  await page.emulateMedia({ reducedMotion: "reduce" });
  const reduced = await fadeOf();
  await page.emulateMedia({ reducedMotion: "no-preference" });
  if (reduced.transition !== "0s") throw new Error("reduced motion still fades: " + JSON.stringify(reduced));
});

await check("iOS: a passive touchstart listener is registered, so :active applies", async () => {
  const n = await cdp.send("Runtime.evaluate", { expression: "window", objectGroup: "press" })
    .then((r) => cdp.send("DOMDebugger.getEventListeners", { objectId: r.result.objectId }))
    .then((r) => r.listeners.filter((l) => l.type === "touchstart" && l.passive).length);
  if (!n) throw new Error("no passive touchstart listener on window");
});

// ---- hover does not stick (slice 2) ----
await check("hover: every :hover rule is inside @media (hover: hover)", async () => {
  const bare = await page.evaluate(() => {
    const out = [];
    const walk = (rules, gated) => {
      for (const r of rules) {
        if (r.media || r.conditionText !== undefined) { const g = gated || /\(\s*(any-)?hover\s*:\s*hover\s*\)/.test(r.conditionText || r.media?.mediaText || ""); walk(r.cssRules, g); continue; }
        if (r.cssRules && !r.selectorText) { walk(r.cssRules, gated); continue; }
        if (r.selectorText && /:hover/.test(r.selectorText) && !gated && !/::-webkit-scrollbar/.test(r.selectorText)) out.push(r.selectorText);
      }
    };
    for (const s of document.styleSheets) { try { walk(s.cssRules, false); } catch {} }
    return out;
  });
  if (bare.length) throw new Error("bare :hover rules (they stick after a tap):\n" + bare.join("\n"));
});
async function noStick(name, hash, sel) {
  await check(`hover: a tapped ${name} is not left highlighted`, async () => {
    await go(hash, sel);
    const h = await page.locator(sel).first().elementHandle();
    await h.evaluate((el) => el.scrollIntoView({ block: "center" }));
    await page.waitForTimeout(150);
    const bg0 = await h.evaluate((el) => getComputedStyle(el).backgroundColor);
    const b = await h.boundingBox();
    await page.touchscreen.tap(b.x + 40, b.y + b.height / 2);
    await page.waitForTimeout(500);
    const [bg1, hov] = await h.evaluate((el) => [getComputedStyle(el).backgroundColor, el.matches(":hover")]);
    if (bg1 !== bg0) throw new Error(`background ${bg0} at rest, ${bg1} after the tap (:hover ${hov})`);
  });
}
await noStick("Peers card", "#/peers", ".pr-card");
if (process.env.COMEBYE_BIN) await noStick("Pages card", "#/pages", ".pgs-card");

await check("hover: with a mouse (1280 px, hover: hover) a card still lights up under the pointer", async () => {
  const d = await launch({ width: 1280, height: 800, dark });
  try {
    await d.page.goto(base + "/#/peers", { waitUntil: "load" });
    const c = d.page.locator(".pr-card").first();
    await c.waitFor({ state: "visible" });
    await d.page.mouse.move(5, 5);
    const bg0 = await c.evaluate((el) => getComputedStyle(el).backgroundColor);
    await c.hover();
    await d.page.waitForTimeout(250);
    const bg1 = await c.evaluate((el) => getComputedStyle(el).backgroundColor);
    if (bg1 === bg0) throw new Error("no hover highlight with a mouse: " + bg0);
  } finally { await d.browser.close(); }
});

if (errors.length) { failed++; console.log("FAIL page errors:\n     " + errors.join("\n     ")); }
await browser.close();
console.log(failed ? `press: ${failed} failed` : "press: all passed");
process.exit(failed ? 1 : 0);
