// Open by default (Settings > Appearance, web/themes.js comebye.open -> <html data-open>): "" (default), "think",
// "tools", "both" set only the FIRST state of a thinking line and of a tool run list. With it on, lines render
// aria-expanded="true" on first paint with no click, at 390, 1280 and 3440; a line the user closed by hand stays
// closed through polls; Up/Down still reaches the rows; an unknown value shows the default, the stored value is on
// <html> by DOMContentLoaded, a storage event updates an open chat, the split view follows, and the two Appearance
// toggles work by keyboard with a preview that follows. Light and dark.
//
//   node openbydefault.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";
import { claude } from "./fixtures/toolruns.mjs";

const [base, out] = process.argv.slice(2);
const results = [], log = [];
const same = (got, want, what) => { if (got !== want) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
async function session(width, height, dark) {
  const { browser, page } = await launch({ width, height, dark });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: claude, total: claude.length, more: false, format: "claude" } }));
  await page.addInitScript(() => {
    document.addEventListener("DOMContentLoaded", () => { window.__openAtDCL = document.documentElement.getAttribute("data-open"); });
  });
  return { browser, page };
}
const store = (page, v) => page.evaluate((v) => { if (v === null) localStorage.removeItem("comebye.open"); else localStorage.setItem("comebye.open", v); }, v);
async function openChat(page, v) {
  await page.goto(base + "/#/settings", { waitUntil: "load" });
  await store(page, v);
  await page.goto("about:blank");
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#chat .toolrun .tr-line");
}
// aria-expanded of every thinking line and every plain tool run line, as "true"/"false" lists
const states = (page, root = "#chat") => page.evaluate((root) => ({
  think: [...document.querySelectorAll(root + " .thinking .tr-line")].map((l) => l.getAttribute("aria-expanded")),
  runs: [...document.querySelectorAll(root + " .toolrun .tr-line")].map((l) => l.getAttribute("aria-expanded")),
  runVis: [...document.querySelectorAll(root + " .toolrun .tr-list")].map((l) => !l.hidden),
  thinkVis: [...document.querySelectorAll(root + " .thinking .think-body")].map((l) => !l.hidden && l.textContent.length > 0),
}), root);
const all = (a, v) => a.length > 0 && a.every((x) => x === v);
const poll = (page) => page.evaluate(() => loadChat(false));

for (const dark of [true, false]) {
  const tag = dark ? "dark" : "light";
  for (const [w, hgt] of [[3440, 1200], [1280, 800], [390, 844]]) {
    const { browser, page } = await session(w, hgt, dark);
    await check(`${w} ${tag}: both on, every thinking line and tool run is open on first paint with no click`, async () => {
      await openChat(page, "both");
      const s = await states(page);
      same(all(s.think, "true"), true, "thinking aria-expanded " + s.think);
      same(all(s.runs, "true"), true, "runs aria-expanded " + s.runs);
      same(s.runVis.every(Boolean), true, "run lists visible");
      same(s.thinkVis.every(Boolean), true, "thinking bodies visible and filled");
      same(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth <= 0), true, "no sideways scroll");
      await page.screenshot({ path: `${out}/open-${w}-${tag}-both.png` });
    });
    await check(`${w} ${tag}: default (off), everything folded as today`, async () => {
      await openChat(page, null);
      const s = await states(page);
      same(all(s.think, "false"), true, "thinking " + s.think);
      // runs: the live one (the last, still running) opens by the live rule, the rest are folded
      same(s.runs.filter((x) => x === "true").length <= 1, true, "at most the live run is open: " + s.runs);
    });
    await check(`${w} ${tag}: think only opens thinking, tools only opens runs`, async () => {
      await openChat(page, "think");
      let s = await states(page);
      same(all(s.think, "true"), true, "think: thinking");
      same(s.runs.filter((x) => x === "true").length <= 1, true, "think: runs folded " + s.runs);
      await openChat(page, "tools");
      s = await states(page);
      same(all(s.think, "false"), true, "tools: thinking folded");
      same(all(s.runs, "true"), true, "tools: runs open");
    });
    await check(`${w} ${tag}: a line closed by hand stays closed through a poll, and one opened by hand stays open`, async () => {
      await openChat(page, "both");
      await page.locator("#chat .thinking .tr-line").first().click();
      await page.locator("#chat .toolrun .tr-line").first().click();
      same(await page.locator("#chat .thinking .tr-line").first().getAttribute("aria-expanded"), "false", "thought closed");
      same(await page.locator("#chat .toolrun .tr-line").first().getAttribute("aria-expanded"), "false", "run closed");
      await poll(page); await poll(page);
      let s = await states(page);
      same(s.think[0], "false", "thought still closed after polls");
      same(s.runs[0], "false", "run still closed after polls");
      same(s.think.slice(1).every((x) => x === "true"), true, "the others stay open " + s.think);
      same(s.runs.slice(1).every((x) => x === "true"), true, "the other runs stay open " + s.runs);
      // and with the default off, a hand-opened line stays open
      await openChat(page, null);
      await page.locator("#chat .thinking .tr-line").first().click();
      await poll(page);
      same((await states(page)).think[0], "true", "hand-opened thought stays open");
    });
    if (w === 1280) {
      await check(`${w} ${tag}: keyboard navigation still reaches the rows with the lists open`, async () => {
        await openChat(page, "both");
        await page.evaluate(() => document.querySelector("#chat .toolrun .tr-line").focus());
        await page.keyboard.press("ArrowDown");
        const on = await page.evaluate(() => document.activeElement.className + "|" + (document.activeElement.closest(".tr-item") || {}).dataset?.id);
        if (!/tr-row/.test(on)) throw new Error("ArrowDown from an open run did not reach a row: " + on);
        await page.keyboard.press("ArrowUp");
        same(await page.evaluate(() => document.activeElement.classList.contains("tr-line")), true, "ArrowUp returns to the line");
      });
    }
    if (w === 3440) {
      await check(`${w} ${tag}: an unknown stored value shows the default`, async () => {
        await openChat(page, "yes");
        const s = await states(page);
        same(all(s.think, "false"), true, "thinking folded");
        same(await page.evaluate(() => document.documentElement.getAttribute("data-open")), null, "data-open");
      });
      await check(`${w} ${tag}: the stored value is on <html> by DOMContentLoaded`, async () => {
        await openChat(page, "both");
        same(await page.evaluate(() => window.__openAtDCL), "both", "data-open at DOMContentLoaded");
      });
      await check(`${w} ${tag}: a storage event from another tab opens lines that are already on screen, but not one closed by hand`, async () => {
        await openChat(page, "");
        await page.locator("#chat .thinking .tr-line").nth(1).click(); // open by hand
        await page.locator("#chat .thinking .tr-line").nth(1).click(); // and close by hand
        const other = await page.context().newPage();
        await other.goto(base + "/#/settings", { waitUntil: "load" });
        await other.evaluate(() => localStorage.setItem("comebye.open", "both"));
        await page.waitForFunction(() => document.documentElement.getAttribute("data-open") === "both");
        await page.waitForFunction(() => document.querySelector("#chat .thinking .tr-line").getAttribute("aria-expanded") === "true");
        const s = await states(page);
        same(s.think[0], "true", "first thought opened");
        same(s.think[1], "false", "hand-closed thought stays closed");
        await other.close();
      });
      await check(`${w} ${tag}: the split view (#sv-chat) opens its subagent thoughts and runs`, async () => {
        await openChat(page, "both");
        await page.locator("#chat .subrun .sa-line").click();
        await page.locator("#chat .subrun .sa-steps").click();
        await page.waitForFunction(() => !document.getElementById("subview").hidden);
        await page.waitForTimeout(500);
        const s = await states(page, "#sv-chat");
        if (s.runs.length) same(all(s.runs, "true"), true, "#sv-chat runs " + s.runs);
        if (s.think.length) same(all(s.think, "true"), true, "#sv-chat thoughts " + s.think);
      });
      await check(`${w} ${tag}: Settings > Appearance toggles work by keyboard and click, the preview follows`, async () => {
        await page.goto(base + "/#/settings", { waitUntil: "load" });
        await store(page, null);
        await page.reload({ waitUntil: "load" });
        await page.waitForSelector("#sx-openthink-default");
        same(await page.locator("#sx-view [role=radiogroup]").count(), 8, "eight radio groups");
        const sel = (id) => `[aria-labelledby="${id}"] .sx-radio[aria-checked="true"] .sx-rn`;
        same(await page.locator(sel("sx-openthink-lbl")).textContent(), "Off", "think default");
        same(await page.locator(sel("sx-opentools-lbl")).textContent(), "Off", "tools default");
        const prev = () => page.evaluate(() => ({
          t: document.querySelector(".sx-prev .thinking .tr-line").getAttribute("aria-expanded"),
          r: document.querySelector(".sx-prev .toolrun .tr-line").getAttribute("aria-expanded"),
          tv: !document.querySelector(".sx-prev .think-body").hidden, rv: !document.querySelector(".sx-prev .tr-list").hidden }));
        same(JSON.stringify(await prev()), JSON.stringify({ t: "false", r: "false", tv: false, rv: false }), "preview at Off");
        await page.locator("#sx-openthink-default").focus();
        await page.keyboard.press("ArrowRight");
        same(await page.locator(sel("sx-openthink-lbl")).textContent(), "On", "think On");
        same(await page.evaluate(() => localStorage.getItem("comebye.open")), "think", "stored think");
        same(await page.evaluate(() => document.activeElement.id), "sx-openthink-on", "focus stays");
        same((await prev()).tv, true, "preview thought open");
        await page.locator("#sx-opentools-on").click();
        same(await page.evaluate(() => localStorage.getItem("comebye.open")), "both", "stored both");
        same(JSON.stringify(await prev()), JSON.stringify({ t: "true", r: "true", tv: true, rv: true }), "preview at both");
        await page.screenshot({ path: `${out}/open-settings-${w}-${tag}.png` });
        await page.locator("#sx-openthink-default").click();
        same(await page.evaluate(() => localStorage.getItem("comebye.open")), "tools", "stored tools");
        await page.locator("#sx-opentools-default").click();
        same(await page.evaluate(() => localStorage.getItem("comebye.open")), "", "stored default");
      });
    }
    await browser.close();
  }
}
if (log.length) console.log(log.join("\n"));
const bad = results.filter((r) => !r).length;
console.log(`${results.length - bad}/${results.length} checks passed`);
process.exit(bad || log.length ? 1 : 0);
