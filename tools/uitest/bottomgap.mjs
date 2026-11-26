// The space at the foot of the phone's agent chat (web/app.js viewportHeal): under the composer only the design's
// margin over the home indicator (index.html "#detail main": max(8px, inset - 12px)), and between the last row (the
// queued message, "Thinking…", the activity row) and the composer only the chat's own padding, in every state.
//
// iOS's installed app (navigator.standalone) can be left with a short layout viewport once the keyboard closes:
// innerHeight and 100dvh drop (~59pt on a Pro Max) until the app is quit, and the chat ended above an empty band. Chrome
// replays that here: the viewport is made shorter while the composer has focus (the keyboard), and stays short after
// the composer lets go. The screen is still the full height, so each check measures against it.
//
// At 390x844 and 844x390 with an iPhone's insets, with a busy agent and a queued message, through: at rest, keyboard
// out, keyboard in, keyboard in with the short viewport left behind, a screen change and back, a background and
// foreground, and the viewport coming back. Each time:
//   - the gap under the composer is the margin, within 2px (the composer's own 8px while it has the keyboard);
//   - the gap between the last row and the composer is the one at rest, within 2px.
// Also: Safari's tab (no navigator.standalone) and a desktop window are left as they were.
//
//   node bottomgap.mjs BASE OUT_DIR       (env: SCHEME)
import { launch } from "./lib.mjs";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const scheme = dark ? "dark" : "light";
const IPHONE = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1";
let failed = 0;
const ok = (n) => console.log("ok   " + n);
const fail = (n, why) => { failed++; console.log("FAIL " + n + "\n     " + why); };

const { agents } = await (await fetch(base + "/api/agents")).json();
const alpha = agents.find((a) => a.name === "alpha");
if (!alpha) { console.error("bottomgap.mjs: fixture agent alpha missing"); process.exit(2); }
const agentUrl = base + "/#/agent/" + encodeURIComponent(alpha.pane_id);

const SIZES = [
  { name: "portrait", w: 390, h: 844, ins: { top: 47, bottom: 34, left: 0, right: 0 }, kb: 336 },
  { name: "landscape", w: 844, h: 390, ins: { top: 0, bottom: 21, left: 47, right: 47 }, kb: 170 },
];
const SHRINK = 59; // what iOS leaves behind on a Pro Max (the top inset)

// The agent is busy, with a queued message and the live "Thinking…" line, and enough above them to scroll. Set again
// before each reading: a poll can redraw the chat from the hub's state.
async function busy(page) {
  await page.evaluate(() => {
    const a = agentById(state.selected); a.agent_status = "working";
    state.chat.thinking = { text: "working out where the space at the bottom comes from" };
    if (!state.chat.pending.some((p) => p.test)) state.chat.pending.push({ text: "/home/u/.local/state/comebye/uploads/img-20261126-075826-b670e7fb.png", at: Date.now(), test: true });
    const c = $("chat");
    if (!c.querySelector(".bg-fill")) for (let i = 0; i < 14; i++) { const d = document.createElement("div"); d.className = "msg assistant bg-fill"; d.textContent = "earlier reply " + i; c.insertBefore(d, c.querySelector(".msg")); }
    renderPending(); renderActivity();
    c.scrollTop = c.scrollHeight;
  });
  await page.waitForTimeout(250);
  await page.evaluate(() => { const c = $("chat"); c.scrollTop = c.scrollHeight; });
}

// screenH: the screen's height (the viewport's, unless iOS's short viewport is being replayed).
async function read(page, screenH) {
  return page.evaluate((screenH) => {
    const p = document.querySelector("#detail main > .prompt").getBoundingClientRect();
    const c = $("chat");
    const rows = [...c.children].filter((e) => !e.classList.contains("jump-anchor") && e.getClientRects().length);
    const last = rows[rows.length - 1];
    return {
      under: screenH - p.bottom, row: p.top - last.getBoundingClientRect().bottom, last: last.className,
      appH: document.documentElement.style.getPropertyValue("--app-h"), inner: innerHeight,
      detailH: $("detail").getBoundingClientRect().height, scrollY,
      focused: !!document.querySelector("#detail .composer:focus"),
    };
  }, screenH);
}

const focusComposer = (page) => page.evaluate(() => { const e = $("prompt-text"); e.focus(); });
const blurComposer = (page) => page.evaluate(() => { document.activeElement?.blur(); });
const settle = (page) => page.waitForTimeout(450); // viewportHeal runs 150ms after the event

// A picture of the whole screen: what the page draws, on the screen's height (iOS shows the page's background below
// a short viewport: the band the user saw).
async function shot(page, browser, s, file) {
  const png = await page.screenshot({ fullPage: true });
  const bg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  const p2 = await browser.newPage();
  await p2.setViewportSize({ width: s.w, height: s.h });
  await p2.setContent(`<body style="margin:0;background:${bg};height:${s.h}px;overflow:hidden"><img style="display:block;width:${s.w}px" src="data:image/png;base64,${png.toString("base64")}"></body>`);
  await p2.screenshot({ path: file });
  await p2.close();
}

const { browser } = await launch({ dark });
try {
  for (const s of SIZES) {
    const tag = `${s.name} ${s.w}x${s.h} (${scheme})`;
    const margin = Math.max(8, s.ins.bottom - 12);
    const ctx = await browser.newContext({ viewport: { width: s.w, height: s.h }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, colorScheme: scheme, userAgent: IPHONE });
    await ctx.addInitScript(() => { Object.defineProperty(Navigator.prototype, "standalone", { get: () => true, configurable: true }); });
    const page = await ctx.newPage();
    const errs = [];
    page.on("pageerror", (e) => errs.push(String(e)));
    page.setDefaultTimeout(8000);
    const cdp = await ctx.newCDPSession(page);
    await cdp.send("Emulation.setSafeAreaInsetsOverride", { insets: s.ins });
    await page.goto(agentUrl, { waitUntil: "load" });
    await page.waitForSelector("#detail #chat .msg", { state: "visible", timeout: 15000 });
    await page.waitForTimeout(400);
    await busy(page);
    const rest = await read(page, s.h);
    const rowGap = rest.row;
    await shot(page, browser, s, `${out}/bottomgap-${s.name}-rest-${scheme}.png`);

    const expect = (state, g, { under = margin, screenH = s.h } = {}) => {
      const why = [];
      if (Math.abs(g.under - under) > 2) why.push(`gap under the composer ${g.under.toFixed(1)}px, want ${under}px`);
      if (Math.abs(g.row - rowGap) > 2) why.push(`gap from the last row (${g.last}) to the composer ${g.row.toFixed(1)}px, want ${rowGap.toFixed(1)}px as at rest`);
      if (!g.focused && Math.abs(g.detailH - screenH) > 1) why.push(`the chat screen is ${g.detailH}px tall on a ${screenH}px screen`);
      if (g.scrollY) why.push(`the window is scrolled by ${g.scrollY}px`);
      if (why.length) fail(`${tag}: ${state}`, why.join("; ") + ` (innerHeight ${g.inner}, --app-h "${g.appH}")`);
      else ok(`${tag}: ${state} (under ${g.under.toFixed(1)}px, row gap ${g.row.toFixed(1)}px)`);
    };
    expect("at rest, busy with a queued message", rest);
    if (rowGap > 48) fail(`${tag}: the last row's gap`, `${rowGap.toFixed(1)}px between the last row and the composer at rest: a blank band`);

    // The keyboard out (the viewport shorter, as Android's resizes-content does), then back in.
    await focusComposer(page);
    await page.setViewportSize({ width: s.w, height: s.h - s.kb });
    await settle(page); await busy(page); await focusComposer(page);
    { const g = await read(page, s.h - s.kb); expect("keyboard out", g, { under: 8, screenH: s.h - s.kb }); if (g.appH) fail(`${tag}: keyboard out`, `--app-h set while typing: ${g.appH}`); }
    await blurComposer(page);
    await page.setViewportSize({ width: s.w, height: s.h });
    await settle(page); await busy(page);
    expect("keyboard in", await read(page, s.h));

    // iOS's leftover: the keyboard goes, the viewport stays SHRINK shorter.
    await focusComposer(page);
    await page.setViewportSize({ width: s.w, height: s.h - SHRINK });
    await settle(page);
    await blurComposer(page);
    await settle(page); await busy(page);
    expect("keyboard in, short viewport left behind", await read(page, s.h));
    await shot(page, browser, s, `${out}/bottomgap-${s.name}-short-${scheme}.png`);

    // A screen change and back (the list by the back button, the chat by its route), still short.
    await page.click("#back");
    await page.waitForSelector("#detail", { state: "hidden" }).catch(() => {});
    await settle(page);
    await page.evaluate((h) => { location.hash = h; }, new URL(agentUrl).hash);
    await page.waitForSelector("#detail #chat .msg", { state: "visible" });
    await settle(page); await busy(page);
    expect("short viewport, after the list and back", await read(page, s.h));

    // Background and foreground.
    await page.evaluate(() => {
      Object.defineProperty(document, "hidden", { get: () => true, configurable: true });
      document.dispatchEvent(new Event("visibilitychange"));
      Object.defineProperty(document, "hidden", { get: () => false, configurable: true });
      document.dispatchEvent(new Event("visibilitychange"));
      window.dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true }));
    });
    await settle(page); await busy(page);
    expect("short viewport, after a background and foreground", await read(page, s.h));

    // The viewport comes back: --app-h goes.
    await page.setViewportSize({ width: s.w, height: s.h });
    await settle(page); await busy(page);
    { const g = await read(page, s.h); expect("viewport back to full", g); if (g.appH) fail(`${tag}: viewport back`, `--app-h left set: ${g.appH}`); }
    await page.evaluate(() => { location.hash = "#/"; }); await settle(page);
    await page.evaluate((h) => { location.hash = h; }, new URL(agentUrl).hash);
    await page.waitForSelector("#detail #chat .msg", { state: "visible" });
    await settle(page); await busy(page);
    expect("after the list and back", await read(page, s.h));

    if (errs.length) fail(`${tag}: page errors`, errs.join("; ")); else ok(`${tag}: no page errors`);
    await ctx.close();
  }

  // Safari's tab (no navigator.standalone) and a desktop window: a shorter viewport is the screen, nothing is healed.
  for (const s of [{ name: "safari-tab", w: 390, h: 844, mobile: true }, { name: "desktop", w: 1280, h: 800, mobile: false }]) {
    const tag = `${s.name} ${s.w}x${s.h} (${scheme})`;
    const ctx = await browser.newContext({ viewport: { width: s.w, height: s.h }, deviceScaleFactor: s.mobile ? 2 : 1, isMobile: s.mobile, hasTouch: s.mobile, colorScheme: scheme, ...(s.mobile ? { userAgent: IPHONE } : {}) });
    const page = await ctx.newPage();
    await page.goto(agentUrl, { waitUntil: "load" });
    await page.waitForSelector("#detail #chat .msg", { state: "visible", timeout: 15000 });
    await focusComposer(page);
    await page.setViewportSize({ width: s.w, height: s.h - SHRINK });
    await settle(page); await blurComposer(page); await settle(page);
    const g = await page.evaluate(() => ({ appH: document.documentElement.style.getPropertyValue("--app-h"), h: $("detail").getBoundingClientRect().height, inner: innerHeight }));
    if (g.appH || (s.mobile && Math.abs(g.h - g.inner) > 1)) fail(`${tag}: left as it was`, `--app-h "${g.appH}", chat screen ${g.h}px in a ${g.inner}px viewport`);
    else ok(`${tag}: left as it was (no --app-h, chat screen ${g.h}px in a ${g.inner}px viewport)`);
    await ctx.close();
  }
} finally {
  await browser.close();
}
console.log(failed ? `bottomgap: ${failed} failed` : "bottomgap: all ok");
process.exit(failed ? 1 : 0);
