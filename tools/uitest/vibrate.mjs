// Vibrate (#135 slice 6): a short buzz on a few key actions, Android only, behind a Settings choice that is off by default.
// navigator.vibrate is stubbed (headless Chrome has no motor), and the stub records every call.
//   - at 1280 with a mouse the Touch group is not in Settings, even with the API there (desktop Chrome has it, and it does
//     nothing);
//   - at 390 with touch and no API (Safari on iOS) the group is not there either;
//   - at 390 with touch and the API, Vibrate is shown and Off; haptic() then calls nothing;
//   - picking On saves it (it is still On after a reload), buzzes once to say so, and haptic() then calls vibrate(8);
//   - picking Off again stops it;
//   - with touch input on a fixture hub: a status tab switch, Send and an answer option each call vibrate(8) with the
//     setting on, and nothing else does (a load, the tab already selected, opening an agent); with it off, none do.
//
//   node vibrate.mjs BASE OUT_DIR      (env: SCHEME; BASE is a fixture hub: alpha w1:p1 idle, needs-input w6:p1 blocked)
import { launch, clickStable } from "./lib.mjs";
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
    if (page) await page.screenshot({ path: path.join(out, `vibrate-fail-${tag}-${failed}.png`) }).catch(() => {});
  }
}
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };

// The stub: every call lands in window.__vib. stub false takes the API away (Safari).
const STUB = (on) => {
  window.__vib = [];
  if (on) Object.defineProperty(Navigator.prototype, "vibrate", { configurable: true, writable: true, value(p) { window.__vib.push(p); return true; } });
  else delete Navigator.prototype.vibrate;
};

async function open({ width, height, stub }) {
  const l = await launch({ width, height, dark });
  await l.ctx.addInitScript(STUB, stub);
  page = l.page;
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => errors.push("pageerror: " + e.message));
  await page.goto(base + "/#/settings", { waitUntil: "load" });
  await page.waitForSelector("#settings:not([hidden]) #sx-view h2");
  return l;
}
const vib = () => page.evaluate(() => window.__vib.slice());
const group = () => page.locator("#sx-vibrate-lbl").count();

await check("1280 with a mouse: no Vibrate in Settings, though the API is there", async () => {
  const l = await open({ width: 1280, height: 860, stub: true });
  try {
    assert(await page.evaluate(() => typeof navigator.vibrate === "function"), "the stub is not in place");
    assert((await group()) === 0, "Vibrate is shown on a desktop");
  } finally { await l.browser.close(); }
});

await check("390 with touch, no API (iOS): no Vibrate in Settings", async () => {
  const l = await open({ width: 390, height: 844, stub: false });
  try {
    assert(await page.evaluate(() => !("vibrate" in navigator)), "the API is still there");
    assert((await group()) === 0, "Vibrate is shown without the API");
  } finally { await l.browser.close(); }
});

await check("390 with touch and the API: Vibrate is Off by default; On buzzes, is saved and makes haptic() buzz; Off stops it", async () => {
  const l = await open({ width: 390, height: 844, stub: true });
  try {
    assert((await group()) === 1, "Vibrate is not shown");
    assert((await page.getAttribute("#sx-vibrate-default", "aria-checked")) === "true", "Off is not the default");
    await page.evaluate(() => haptic());
    assert((await vib()).length === 0, "haptic() buzzed with the setting off: " + JSON.stringify(await vib()));
    await clickStable(page, "#sx-vibrate-on");
    assert((await page.getAttribute("#sx-vibrate-on", "aria-checked")) === "true", "On is not checked after the pick");
    assert(JSON.stringify(await vib()) === "[8]", "picking On did not buzz once: " + JSON.stringify(await vib()));
    await page.reload({ waitUntil: "load" });
    await page.waitForSelector("#sx-vibrate-on");
    assert((await page.getAttribute("#sx-vibrate-on", "aria-checked")) === "true", "On was not kept over a reload");
    await page.evaluate(() => haptic());
    assert(JSON.stringify(await vib()) === "[8]", "haptic() did not buzz 8 ms with the setting on: " + JSON.stringify(await vib()));
    await clickStable(page, "#sx-vibrate-default");
    await page.evaluate(() => { window.__vib.length = 0; haptic(); });
    assert((await vib()).length === 0, "haptic() buzzed after Off");
    await page.screenshot({ path: path.join(out, `vibrate-settings-${tag}.png`) });
  } finally { await l.browser.close(); }
});

// The key actions, with the setting on and then off, at 390 with real touch input: a status tab switch (and a tap on
// the tab already selected), Send in an agent, and an answer option on a blocked agent.
async function actions(on) {
  const l = await launch({ width: 390, height: 844, dark });
  await l.ctx.addInitScript(STUB, true);
  await l.ctx.addInitScript((on) => { try { if (on) localStorage.setItem("comebye-vibrate", "on"); else localStorage.removeItem("comebye-vibrate"); } catch {} }, on);
  page = l.page;
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => errors.push("pageerror: " + e.message));
  const cdp = await l.ctx.newCDPSession(page);
  const tap = async (sel) => {
    const b = await page.evaluate((sel) => {
      const el = [...document.querySelectorAll(sel)].find((e) => e.getClientRects().length);
      if (!el) return null;
      el.scrollIntoView({ block: "center" });
      const r = el.getBoundingClientRect();
      return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
    }, sel);
    if (!b) throw new Error("nothing to tap: " + sel);
    await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: b.x, y: b.y, id: 1 }] });
    await page.waitForTimeout(30);
    await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
    await page.waitForTimeout(60);
  };
  const got = {};
  const take = async (k) => { got[k] = await page.evaluate(() => window.__vib.splice(0)); };
  try {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#agents .agent[data-id]");
    await take("load");
    await tap('#lf-tabs .lf-tab[aria-pressed="false"]');
    await take("tab");
    await tap('#lf-tabs .lf-tab[aria-pressed="true"]');
    await take("same tab");
    // Send, in the idle agent alpha.
    await page.evaluate(() => { location.hash = "#/agent/" + encodeURIComponent("w1:p1"); });
    await page.waitForFunction(() => document.body.classList.contains("has-detail"));
    await take("open");
    await page.locator("#prompt-text").click();
    await page.keyboard.type("hello from vibrate");
    await page.evaluate(() => window.__vib.splice(0));
    await tap("#btn-send");
    await take("send");
    // An answer: the blocked agent needs-input shows its options.
    await page.evaluate(() => { location.hash = "#/agent/" + encodeURIComponent("w6:p1"); });
    await page.waitForSelector("#answer button.opt:not(:disabled)");
    await take("open blocked");
    await tap("#answer button.opt:not(:disabled)");
    await take("answer");
  } finally { await l.browser.close(); }
  return got;
}

await check("390, Vibrate On: a tab switch, Send and an answer each buzz 8 ms; the tab already selected, a load and opening an agent do not", async () => {
  const g = await actions(true);
  const want = { load: [], tab: [8], "same tab": [], open: [], send: [8], "open blocked": [], answer: [8] };
  for (const [k, v] of Object.entries(want)) assert(JSON.stringify(g[k]) === JSON.stringify(v), `${k}: vibrate got ${JSON.stringify(g[k])}, want ${JSON.stringify(v)}`);
});

await check("390, Vibrate Off (the default): the same actions never call vibrate", async () => {
  const g = await actions(false);
  const all = Object.values(g).flat();
  assert(all.length === 0, "vibrate was called with the setting off: " + JSON.stringify(g));
  assert(g.answer !== undefined, "the run did not reach the answer");
});

await check("no page errors", async () => { assert(errors.length === 0, errors.join("\n")); });
process.exit(failed ? 1 : 0);
