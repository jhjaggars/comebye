// The return mark (#135 slice 3): back on a list from a row you opened, that row is briefly marked (.was-open, a 16%
// --blue layer held a moment and faded), then the mark clears. At 390 px with real touch input (CDP touch events):
//   - agents: the app's Back and the browser's back both mark the row; the list keeps its scroll position, and a row
//     scrolled off the list's top is scrolled into view;
//   - a re-render of the list (a poll) during the mark or after it never marks the row again;
//   - Pages (a card, its page, Back) and the palette (a command with a step, the step's Back) mark theirs too;
//   - reduced motion shows the mark without the fade and still takes it away;
//   - at 1280 px, where the list and the detail stand side by side, no row is marked.
//
//   node returnmark.mjs BASE OUT_DIR      (env: SCHEME; COMEBYE_BIN + COMEBYE_STATE_DIR to publish a page for Pages)
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
  catch (e) {
    failed++; console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     "));
    await page.screenshot({ path: path.join(out, `returnmark-fail-${tag}-${failed}.png`) }).catch(() => {});
    // Start the next check on a fresh list, so one failure cannot leave a detail open under the ones after it.
    await page.goto("about:blank").then(() => page.goto(base + "/", { waitUntil: "load" })).then(() => page.waitForSelector("#agents .agent[data-id]")).catch(() => {});
  }
}
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };

// A page for the Pages list, when the CLI is at hand.
// And a to-do filed by a live agent, so its item has Open session (a way off the list and back).
let haveCli = false;
const todoQ = "Return mark test " + Date.now();
if (process.env.COMEBYE_BIN && process.env.COMEBYE_STATE_DIR) {
  const live = (await (await fetch(base + "/api/agents")).json()).agents.filter((a) => a.pane_id);
  const work = fs.mkdtempSync(path.join(os.tmpdir(), "retmark-ui-"));
  const f = path.join(work, "p.html");
  fs.writeFileSync(f, "<!doctype html><title>Return mark page</title><h1>Return mark page</h1>");
  const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: process.env.COMEBYE_STATE_DIR, HERDR_PANE_ID: live[0].pane_id };
  execFileSync(process.env.COMEBYE_BIN, ["publish", f, "--title", "Return mark page"], { env, stdio: "ignore" });
  execFileSync(process.env.COMEBYE_BIN, ["todo", "add", todoQ, "--kind", "followup"], { env, stdio: "ignore" });
  haveCli = true;
}

const cdp = await page.context().newCDPSession(page);
const touch = (type, x, y) => cdp.send("Input.dispatchTouchEvent", { type, touchPoints: type === "touchEnd" ? [] : [{ x, y, id: 1 }] });
// tap: a real touch on the middle of the first visible element matching sel, scrolled into view first as a finger would
// find it. Found, scrolled and measured in one evaluate: the agent list redraws its rows on its polls, and a handle taken
// in one call can be detached by the next.
async function tap(sel) {
  const b = await page.evaluate(async (sel) => {
    for (let i = 0; i < 300; i++) {
      const el = [...document.querySelectorAll(sel)].find((e) => e.getClientRects().length && getComputedStyle(e).visibility !== "hidden");
      if (el) {
        // Scroll only when a finger could not reach it where it is: off screen, or clipped by a scroller (the palette).
        const r0 = el.getBoundingClientRect(), hit = document.elementFromPoint(r0.left + Math.min(r0.width / 2, 120), r0.top + r0.height / 2);
        if (!hit || !el.contains(hit)) el.scrollIntoView({ block: "center" });
        const r = el.getBoundingClientRect();
        return { x: r.left, y: r.top, width: r.width, height: r.height };
      }
      await new Promise((r) => requestAnimationFrame(r));
    }
    return null;
  }, sel);
  if (!b) throw new Error("nothing visible to tap: " + sel);
  const x = b.x + Math.min(b.width / 2, 120), y = b.y + b.height / 2;
  await touch("touchStart", x, y);
  await page.waitForTimeout(40);
  await touch("touchEnd", x, y);
}
const box = (sel) => page.evaluate((sel) => { const r = document.querySelector(sel)?.getBoundingClientRect(); return r && { y: r.top, height: r.height }; }, sel);
// The marks seen on the page: which elements carry .was-open, and the layer's alpha and animation on each.
const marks = () => page.evaluate(() => [...document.querySelectorAll(".was-open")].map((el) => {
  const cs = getComputedStyle(el);
  const c = cs.getPropertyValue("--ret-c").trim();
  const img = cs.backgroundImage, slash = /\/\s*([\d.]+)\)/.exec(img), rgba = /rgba\([^)]*,\s*([\d.]+)\)/.exec(img);
  const alpha = !img.includes("gradient") ? 0 : slash ? parseFloat(slash[1]) : rgba ? parseFloat(rgba[1]) : 1;
  return { c, id: el.dataset.id || el.closest("[data-id]")?.dataset.id || el.dataset.key || el.dataset.name || "", cls: el.className, img: cs.backgroundImage, alpha, anim: cs.animationName, dur: cs.animationDuration };
}));
// Watch .was-open from now: every element that gets it, with the time, until stop() is called.
async function watch() {
  await page.evaluate(() => {
    window.__ret = [];
    const t0 = performance.now();
    window.__retMo?.disconnect();
    window.__retMo = new MutationObserver((ms) => {
      for (const m of ms) if (m.type === "attributes" && m.target.classList.contains("was-open") && !(m.oldValue || "").includes("was-open"))
        window.__ret.push({ t: Math.round(performance.now() - t0), id: m.target.dataset.id || m.target.closest("[data-id]")?.dataset.id || m.target.dataset.key || "" });
    });
    window.__retMo.observe(document.body, { attributes: true, attributeFilter: ["class"], attributeOldValue: true, subtree: true });
  });
}
const seen = () => page.evaluate(() => window.__ret);
// The mark comes from a check one or two frames after the step back (or a timer at 150 ms): wait for it, never a fixed sleep.
const markAppears = () => page.waitForSelector(".was-open", { state: "attached", timeout: 1500 }).catch(() => {});
const clears = () => page.waitForFunction(() => !document.querySelector(".was-open"), null, { timeout: 2500 });

async function go(hash, ready) {
  await page.goto("about:blank");
  await page.goto(base + "/" + hash, { waitUntil: "load" });
  await page.waitForSelector(ready, { state: "visible" });
}
const listScroll = () => page.evaluate(() => {
  const s = [document.scrollingElement, document.getElementById("list"), document.querySelector("#list main")].filter(Boolean);
  return s.map((e) => e.scrollTop).join(",");
});
const rowSel = (id) => `#agents .agent[data-id="${id}"]`;

// ---- agents ----
await go("", "#agents .agent[data-id]");
const ids = await page.$$eval("#agents .agent[data-id]", (els) => els.map((e) => e.dataset.id));
// A row low enough down that the list scrolled to reach it (its position must survive the round trip).
const pick = ids[Math.min(ids.length - 1, Math.max(2, Math.floor(ids.length * 0.6)))];
const scrollY = () => page.evaluate(() => Math.round(document.scrollingElement.scrollTop));

// openAndBack: tap the row, wait for the detail, come back with back(); return the scroll position before and after.
async function openAndBack(id, back) {
  for (let i = 0; ; i++) {
    await tap(rowSel(id));
    await page.waitForSelector("#detail", { state: "visible" });
    await page.waitForTimeout(250);
    // The list can move between finding the row and the touch (a redraw when the hub answers), so the finger lands on
    // its neighbour. That opens the neighbour (and marks it on the way back, rightly): go back and touch again.
    if (await page.evaluate((id) => state.selected === id, id)) break;
    if (i) throw new Error(`the tap opened ${await page.evaluate(() => state.selected)}, not ${id}`);
    console.log("     (the list moved under the touch: once more)");
    await page.goBack();
    await page.waitForSelector(rowSel(id), { state: "visible" });
    await page.waitForTimeout(1200); // let that mark run out
  }
  assert(!(await marks()).length, "a row is marked while the detail is open");
  await watch();
  await back();
  await page.waitForSelector(rowSel(id), { state: "visible" });
  await markAppears();
}
async function marked(id) {
  const m = await marks();
  assert(m.length === 1 && m[0].id === id, "marked: " + JSON.stringify(m));
  assert(m[0].alpha > 0.1, "the layer is not drawn: " + JSON.stringify(m[0]));
  return m[0];
}

for (const [how, back] of [["the app's Back", () => tap("#back")], ["the browser's back", () => page.goBack()]]) {
  await check(`agents, ${how}: the row you opened is marked, the scroll position kept, then the mark clears`, async () => {
    await page.evaluate(() => { scrollTo(0, 0); });
    await page.evaluate((sel) => { const el = document.querySelector(sel); const r = el.getBoundingClientRect(); if (r.top < 0 || r.bottom > innerHeight) el.scrollIntoView({ block: "center" }); }, rowSel(pick));
    const y0 = await scrollY();
    await openAndBack(pick, back);
    const m = await marked(pick);
    assert(m.anim === "was-open" && m.dur === "1s", "no fade: " + m.anim + " " + m.dur);
    assert(await scrollY() === y0, `scroll moved: ${y0} -> ${await scrollY()}`);
    await page.screenshot({ path: path.join(out, `returnmark-agents-${how.includes("app") ? "app" : "browser"}-${tag}.png`) });
    await clears();
    // A poll during the fade carries the mark over to the new row element; nothing may start a mark after it ends.
    const ss = await seen();
    assert(ss.length >= 1 && ss.every((x) => x.id === pick && x.t <= ss[0].t + 1050), "marked again: " + JSON.stringify(ss));
  });
}

await check("agents: the mark fades (held, then falling to nothing by about 1 s)", async () => {
  await openAndBack(pick, () => tap("#back"));
  const a1 = (await marks())[0]?.alpha ?? 0;
  await page.waitForTimeout(500);
  const a2 = (await marks())[0]?.alpha ?? 0;
  assert(a1 > 0.12 && a2 < a1 && a2 > 0, `alpha ${a1} then ${a2}`);
  await clears();
});

await check("agents: a re-render or a poll during the mark keeps one mark, and one after it marks nothing", async () => {
  await openAndBack(pick, () => tap("#back"));
  await page.evaluate(() => { document.getElementById("agents").innerHTML = ""; renderList(); }); // a full redraw: new row elements
  await page.waitForTimeout(80);
  const m = await marked(pick);
  assert(m.alpha < 0.17, "the fade restarted: " + m.alpha);
  await clears();
  await watch();
  await page.evaluate(() => { document.getElementById("agents").innerHTML = ""; renderList(); });
  await page.waitForTimeout(2200); // a real poll or two
  assert(!(await seen()).length, "marked again after a re-render: " + JSON.stringify(await seen()));
});

await check("agents: a row scrolled away while open is scrolled into view; one on screen is not moved", async () => {
  await page.evaluate(() => scrollTo(0, 0));
  const top = ids[0];
  await openAndBack(top, async () => { await page.evaluate(() => scrollTo(0, document.scrollingElement.scrollHeight)); await tap("#back"); });
  // the list came back at the bottom; the first row is off screen, so it is brought into view
  await marked(top);
  const b = await box(rowSel(top));
  assert(b.y >= 0 && b.y + b.height <= 844, "the row is still off screen: " + JSON.stringify(b));
  await clears();
});

await check("agents: back from a blocked row, the mark is drawn over its edge glow, which stays", async () => {
  const id = await page.$eval("#agents .agent.blocked[data-id]", (e) => e.dataset.id);
  const glow = () => page.evaluate((sel) => getComputedStyle(document.querySelector(sel)).backgroundImage, rowSel(id));
  const rest = await glow();
  if (!rest.includes("linear-gradient(90deg")) { console.log("     (no edge glow on a blocked row in this surface style: skipped)"); return; }
  await openAndBack(id, () => tap("#back"));
  const m = await marked(id);
  assert(m.img.includes("linear-gradient(90deg"), "the glow went under the mark: " + m.img);
  await clears();
  assert((await glow()) === rest, "the row did not come back as it was");
});

await check("agents, reduced motion: the mark shows without a fade and still clears", async () => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await openAndBack(pick, () => tap("#back"));
  const m = await marked(pick);
  assert(m.anim === "none", "animated under reduced motion: " + m.anim);
  await page.waitForTimeout(350);
  const m2 = await marked(pick);
  assert(Math.abs(m2.alpha - m.alpha) < 0.01, `faded: ${m.alpha} -> ${m2.alpha}`);
  await clears();
  await page.emulateMedia({ reducedMotion: "no-preference" });
});

await check("agents: a tap that never leaves the list marks nothing (a group header)", async () => {
  await watch();
  const h = await page.$("#agents button.proj-head");
  assert(h, "no group header");
  await tap("#agents button.proj-head"); await page.waitForTimeout(300); await tap("#agents button.proj-head");
  await page.waitForTimeout(1200);
  assert(!(await seen()).length, "marked: " + JSON.stringify(await seen()));
});

// ---- the palette: a command that asks a second question, then that question's Cancel ----
await check("palette: back from a step marks the command you came from", async () => {
  await tap(rowSel(pick));
  await page.waitForSelector("#detail", { state: "visible" });
  await page.keyboard.press("Control+k");
  await page.waitForSelector("#palette:not([hidden]) .pal-it[data-key='k:a-int']", { state: "visible" });
  await tap("#pal-list .pal-it[data-key='k:a-int']");
  await page.waitForSelector("#pal-list .pal-it[data-key='c:Cancel']", { state: "visible" });
  await page.waitForTimeout(200);
  await watch();
  await tap("#pal-list .pal-it[data-key='c:Cancel']");
  await page.waitForSelector("#pal-list .pal-it[data-key='k:a-int']", { state: "visible" });
  await markAppears();
  const m = await marks();
  assert(m.length === 1 && m[0].id === "k:a-int", "marked: " + JSON.stringify(m));
  await page.screenshot({ path: path.join(out, `returnmark-palette-${tag}.png`) });
  await clears();
  // Closing the palette from a step forgets the command: opening the palette again marks nothing.
  await tap("#pal-list .pal-it[data-key='k:a-int']");
  await page.waitForSelector("#pal-list .pal-it[data-key='c:Cancel']", { state: "visible" });
  await page.waitForTimeout(200);
  await page.keyboard.press("Escape"); // back a step with the keyboard: marked as well
  await markAppears();
  assert((await marks()).map((x) => x.id).join() === "k:a-int", "Esc back: marked " + JSON.stringify(await marks()));
  await page.keyboard.press("Escape"); // closed during the mark
  await page.waitForSelector("#palette", { state: "hidden" });
  await page.waitForTimeout(100);
  await watch();
  await page.keyboard.press("Control+k");
  await page.waitForSelector("#pal-list .pal-it[data-key='k:a-int']", { state: "attached" });
  await page.waitForTimeout(1200);
  assert(!(await seen()).length, "reopening the palette marked: " + JSON.stringify(await seen()));
  await page.keyboard.press("Escape");
});

// ---- To do: an item opened in place marks nothing; its Open session and Back mark the item ----
if (haveCli) {
  await check("To do: opening an item in place marks nothing; back from its session marks the item", async () => {
    await go("", "#todo .td-item[data-id]");
    const id = await page.$$eval("#todo .td-item[data-id]", (els, q) => els.find((e) => e.textContent.includes(q))?.dataset.id, todoQ);
    assert(id, "the seeded to-do is not on the To do card");
    const row = `#todo .td-item[data-id="${id}"] > .td-row`;
    await watch();
    await tap(row);
    await page.waitForSelector(`#todo .td-item[data-id="${id}"].open .td-open:not([aria-disabled])`, { state: "visible" });
    await page.waitForTimeout(1200);
    assert(!(await seen()).length, "opening in place marked: " + JSON.stringify(await seen()));
    await tap(`#todo .td-item[data-id="${id}"] .td-open`);
    await page.waitForSelector("#detail", { state: "visible" });
    await page.waitForTimeout(300);
    await tap("#back");
    await page.waitForSelector(row, { state: "visible" });
    await markAppears();
    const m = await marks();
    assert(m.length === 1 && m[0].id === id && m[0].cls.includes("td-row"), "marked: " + JSON.stringify(m));
    await page.screenshot({ path: path.join(out, `returnmark-todo-${tag}.png`) });
    await clears();
  });
}

// ---- Peers: a host's card, its page, Back (when the hub has a peer) ----
await check("Peers: back from a host marks its card", async () => {
  await go("#/peers", "#peers, .pr-card");
  const name = await page.$$eval(".pr-card[data-name]", (els) => els.at(-1)?.dataset.name);
  if (!name) { console.log("     (no peer cards here: skipped)"); return; }
  await tap(`.pr-card[data-name="${name}"]`);
  await page.waitForSelector("#pr-back", { state: "visible" });
  await page.waitForTimeout(400);
  assert(!(await marks()).length, "marked while the host is open");
  await tap("#pr-back");
  await page.waitForSelector(`.pr-card[data-name="${name}"]`, { state: "visible" });
  await markAppears();
  const m = await marks();
  assert(m.length === 1 && m[0].id === name, "marked: " + JSON.stringify(m));
  await page.screenshot({ path: path.join(out, `returnmark-peers-${tag}.png`) });
  await clears();
});

// ---- Pages: a card, its page, Back ----
if (haveCli) {
  await check("Pages: back from a page marks its card", async () => {
    await go("#/pages", ".pgs-card[data-id]");
    const id = await page.$eval(".pgs-card[data-id]", (e) => e.dataset.id);
    await tap(`.pgs-card[data-id="${id}"]`);
    const back = page.locator("#pgs-back, #pgs-vback").locator("visible=true").first();
    await back.waitFor();
    await page.waitForTimeout(400);
    await watch();
    await tap("#pgs-back, #pgs-vback");
    await page.waitForSelector(`.pgs-card[data-id="${id}"]`, { state: "visible" });
    await markAppears();
    const m = await marks();
    assert(m.length === 1 && m[0].id === id, "marked: " + JSON.stringify(m));
    await page.screenshot({ path: path.join(out, `returnmark-pages-${tag}.png`) });
    await clears();
  });
}

// ---- wide: the list and the detail side by side ----
await check("1280 px: opening a row beside the list and going back marks nothing", async () => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await go("", "#agents .agent[data-id]");
  await page.click(rowSel(ids[1]));
  await page.waitForSelector(rowSel(ids[1]) + ".selected");
  await watch();
  await page.goBack();
  await page.waitForTimeout(1200);
  assert(!(await seen()).length, "marked: " + JSON.stringify(await seen()));
});

if (errors.length) { failed++; console.log("FAIL page errors\n     " + errors.join("\n     ")); }
await browser.close();
if (failed) { console.log(`returnmark (${tag}): ${failed} failed`); process.exit(1); }
console.log(`returnmark (${tag}): all passed`);
