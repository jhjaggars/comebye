// The press layer (#135): every tappable kind shows a pressed look while a finger is down, at phone size, driven with
// real touch input (CDP Input.dispatchTouchEvent, which fires pointer events as a phone does). Checks, per kind: the
// layer is there while held, still there just after a quick release (the 100 ms minimum), gone after the fade. Then: a
// scroll gesture never shows it, a held row inside a scroller waits ~60 ms, and reduced motion drops the fade.
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

// What the press layer looks like on an element now: is it drawn, and with which colour.
const layer = (h) => h.evaluate((el) => {
  const cs = getComputedStyle(el);
  const c = cs.getPropertyValue("--press-c").trim();
  const a = /rgba?\(([^)]+)\)/.exec(c)?.[1].split(/[ ,/]+/).filter(Boolean);
  const alpha = c.startsWith("color(") ? parseFloat(c.split("/")[1] ?? "1") : a ? (a.length > 3 ? parseFloat(a[3]) : 1) : 0;
  return { pressed: el.classList.contains("is-pressed"), released: el.classList.contains("is-released"), img: cs.backgroundImage, alpha, c, transition: cs.transitionDuration };
});
const shown = (l) => l.pressed && l.img.includes("gradient") && l.alpha > 0.05;
const gone = (l) => !l.pressed && !l.released && !l.img.includes("gradient");

async function go(hash, ready) {
  await page.goto("about:blank");
  await page.goto(base + "/" + hash, { waitUntil: "load" });
  await page.waitForSelector(ready, { state: "visible" });
  await page.evaluate(() => { window.__noclick = true; });
  await page.waitForTimeout(250);
}
let shot = 0;
// press(h): touch the element's centre, hold ~130 ms, release; checks the layer through it.
async function press(name, sel, { opts = {} } = {}) {
  const h = await page.locator(sel).locator("visible=true").first().elementHandle();
  if (!h) throw new Error("no visible " + sel);
  await h.evaluate((el) => el.scrollIntoView({ block: "center" }));
  await page.waitForTimeout(150);
  const b = await h.boundingBox();
  const x = b.x + Math.min(b.width / 2, 40), y = b.y + b.height / 2;
  const rest = await layer(h);
  if (rest.pressed || rest.img.includes("gradient")) throw new Error("drawn before the press: " + JSON.stringify(rest));
  await touch("touchStart", x, y);
  await page.waitForTimeout(110);
  const held = await layer(h);
  if (shot < 99 && opts.shot !== false) {
    const pad = 16, vw = page.viewportSize();
    const clip = { x: Math.max(0, b.x - pad), y: Math.max(0, b.y - pad), width: Math.min(vw.width - Math.max(0, b.x - pad), b.width + 2 * pad), height: Math.min(b.height + 2 * pad, vw.height) };
    await page.screenshot({ path: `${out}/press-${tag}-${name.replace(/\W+/g, "-")}.png`, clip }).catch(() => {});
    shot++;
  }
  await touch("touchEnd");
  const after = await layer(h); // just after the release: kept (100 ms minimum since it showed, or the fade)
  await page.waitForTimeout(450);
  const end = await layer(h);
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
      await press(name, sel);
    });
  }
}

// The blocked row: its own orange background stays under the layer (the old .agent:active lost to .agent.blocked).
await check("press: a blocked row keeps its colour under the layer", async () => {
  await go("#/list", ".agent.blocked");
  const h = await page.locator(".agent.blocked").first().elementHandle();
  await h.evaluate((el) => el.scrollIntoView({ block: "center" }));
  const bg0 = await h.evaluate((el) => getComputedStyle(el).backgroundColor);
  const b = await h.boundingBox();
  await touch("touchStart", b.x + 30, b.y + b.height / 2);
  await page.waitForTimeout(110);
  const [bg1, l] = [await h.evaluate((el) => getComputedStyle(el).backgroundColor), await layer(h)];
  await touch("touchEnd");
  if (bg1 !== bg0) throw new Error(`background changed ${bg0} -> ${bg1}`);
  if (!shown(l)) throw new Error("no layer: " + JSON.stringify(l));
});

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
  const h = await page.locator(".agent").nth(1).elementHandle();
  const b = await h.boundingBox();
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

await check("scroll: inside a scroller a held row waits ~60 ms before the layer shows", async () => {
  const h = await page.locator(".agent").nth(1).elementHandle();
  await h.evaluate((el) => el.scrollIntoView({ block: "center" }));
  await page.waitForTimeout(150);
  const b = await h.boundingBox();
  await touch("touchStart", b.x + 60, b.y + b.height / 2);
  const early = await layer(h); // right after the touch: not yet
  await page.waitForTimeout(120);
  const late = await layer(h);
  await touch("touchEnd");
  await page.waitForTimeout(400);
  if (early.pressed) throw new Error("shown at once inside a scroller");
  if (!shown(late)) throw new Error("not shown after 120 ms: " + JSON.stringify(late));
});

await check("scroll: a quick tap inside a scroller still shows the layer for its minimum", async () => {
  const h = await page.locator(".agent").nth(1).elementHandle();
  const b = await h.boundingBox();
  await touch("touchStart", b.x + 60, b.y + b.height / 2);
  await touch("touchEnd"); // released before the 60 ms delay
  const l = await layer(h);
  await page.waitForTimeout(450);
  const end = await layer(h);
  if (!shown(l)) throw new Error("a quick tap showed nothing: " + JSON.stringify(l));
  if (!gone(end)) throw new Error("still drawn after: " + JSON.stringify(end));
  await page.setViewportSize({ width: 390, height: 844 });
});

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

if (errors.length) { failed++; console.log("FAIL page errors:\n     " + errors.join("\n     ")); }
await browser.close();
console.log(failed ? `press: ${failed} failed` : "press: all passed");
process.exit(failed ? 1 : 0);
