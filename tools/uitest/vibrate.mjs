// Vibrate (#135 slice 6): a short buzz on a few key actions, Android only, behind a Settings choice that is off by default.
// navigator.vibrate is stubbed (headless Chrome has no motor), and the stub records every call.
//   - at 1280 with a mouse the Touch group is not in Settings, even with the API there (desktop Chrome has it, and it does
//     nothing);
//   - at 390 with touch and no API (Safari on iOS) the group is not there either;
//   - at 390 with touch and the API, Vibrate is shown and Off; haptic() then calls nothing;
//   - picking On saves it (it is still On after a reload), buzzes once to say so, and haptic() then calls vibrate(8);
//   - picking Off again stops it.
//
//   node vibrate.mjs BASE OUT_DIR      (env: SCHEME)
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

await check("no page errors", async () => { assert(errors.length === 0, errors.join("\n")); });
process.exit(failed ? 1 : 0);
