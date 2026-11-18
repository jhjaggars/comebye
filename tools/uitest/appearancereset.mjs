// Settings > Appearance, last slice: the Theme and Font rows and Reset to defaults. Reset removes every layout
// choice (comebye.col, rs, sp, wrap, open) and not the theme or font; the page follows at once with no reload;
// the conversation then has the default layout at 390, 1280 and 3440 (column 820 or the pane, text 15 px, line
// height 1.55, wrap off, nothing open); another tab follows by the storage event; the Theme and Font rows open
// the same panels as the fleet menu (anchored at the row, Esc and a pick return to the row) and show the
// current value; everything is reachable by keyboard. Light and dark.
//
//   node appearancereset.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";
import { claude } from "./fixtures/toolruns.mjs";

const [base, out] = process.argv.slice(2);
const results = [], log = [];
const same = (got, want, what) => { if (JSON.stringify(got) !== JSON.stringify(want)) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const KEYS = ["col", "rs", "sp", "wrap", "open"];
const ATTRS = KEYS.map((k) => "data-" + k);
async function session(width, height, dark) {
  const { browser, ctx, page } = await launch({ width, height, dark });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: claude, total: claude.length, more: false, format: "claude" } }));
  return { browser, ctx, page };
}
const stored = (page) => page.evaluate((ks) => Object.fromEntries(ks.map((k) => [k, localStorage.getItem("comebye." + k)])), [...KEYS, "theme", "font"]);
const attrs = (page) => page.evaluate((as) => Object.fromEntries(as.map((a) => [a, document.documentElement.getAttribute(a)])), [...ATTRS, "data-theme", "data-font"]);
async function settings(page) {
  await page.goto(base + "/#/settings", { waitUntil: "load" });
  await page.waitForSelector("#sx-reset");
}
const radio = (page, kind, val) => page.locator(`#sx-${kind}-${val}`).click();
async function changeAll(page) {
  await radio(page, "col", "wider"); await radio(page, "rs", "larger"); await radio(page, "sp", "roomy");
  await radio(page, "wrap", "on"); await radio(page, "openthink", "on"); await radio(page, "opentools", "on");
}
// What the conversation looks like now (first assistant reply, a code block, the open state of the lines).
async function chat(page) {
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#chat .toolrun .tr-line");
  return page.evaluate(() => {
    const b = document.querySelector("#chat .msg.assistant .bubble"), cs = getComputedStyle(b);
    const pane = document.querySelector("#chat").clientWidth;
    const pre = document.querySelector("#chat .md pre");
    return { w: Math.round(b.getBoundingClientRect().width), pane, fs: parseFloat(cs.fontSize), lh: Math.round(parseFloat(cs.lineHeight) * 100 / parseFloat(cs.fontSize)) / 100,
      wrap: pre ? getComputedStyle(pre).whiteSpace : "pre", open: [...document.querySelectorAll("#chat .thinking .tr-line, #chat .toolrun .tr-line")].map((l) => l.getAttribute("aria-expanded")).join(),
      sx: document.documentElement.scrollWidth - innerWidth };
  });
}

for (const dark of [true, false]) {
  const tag = dark ? "dark" : "light";
  for (const [w, hgt] of [[390, 844], [1280, 800], [3440, 1200]]) {
    const { browser, page } = await session(w, hgt, dark);
    await check(`${w} ${tag}: change everything, Reset restores the default layout at once; theme and font stay; no reload`, async () => {
      await settings(page);
      await page.evaluate(() => { localStorage.setItem("comebye.font", "geist"); localStorage.setItem("comebye.theme", "dracula"); });
      await page.reload({ waitUntil: "load" }); await page.waitForSelector("#sx-reset");
      await changeAll(page);
      const before = await attrs(page);
      same(before["data-col"], "wider", "col set"); same(before["data-open"], "both", "open set");
      await page.evaluate(() => { window.__mark = 1; });
      await page.locator("#sx-reset").click();
      const a = await attrs(page);
      for (const k of ATTRS) same(a[k], null, k + " removed at once");
      same(a["data-font"], "geist", "font stays"); same(a["data-theme"] !== null, true, "theme stays");
      const s = await stored(page);
      for (const k of KEYS) same(s[k], null, "comebye." + k + " removed");
      same(s.font, "geist", "stored font"); same(s.theme, "dracula", "stored theme");
      same(await page.evaluate(() => window.__mark), 1, "no reload");
      for (const [k, v] of [["col", ""], ["rs", ""], ["sp", ""], ["wrap", ""], ["openthink", ""], ["opentools", ""]]) {
        same(await page.locator(`#sx-${k}-default`).getAttribute("aria-checked"), "true", k + " shows the default");
      }
      same(await page.evaluate(() => document.activeElement.id), "sx-col-default", "focus lands on the first group");
      await page.screenshot({ path: `${out}/areset-${w}-${tag}-after.png` });
      const c = await chat(page); let d0;
      { const fresh = await session(w, hgt, dark); d0 = await chat(fresh.page); const d = d0; await fresh.browser.close(); same(c.w, d.w, "column equals a never-touched browser's"); same(d.w <= 820, true, "default column at most 820"); }
      same(c.fs, 15, "text size"); same(c.lh, 1.55, "line height"); same(c.wrap !== "pre-wrap", true, "wrap off " + c.wrap); same(c.open, d0.open, "same lines open as a never-touched browser"); 
      same(c.sx <= 0, true, "no sideways scroll");
      // and it stays after a reload
      await page.reload({ waitUntil: "load" }); await page.waitForSelector("#chat .tr-line");
      for (const k of ATTRS) same(await page.evaluate((k) => document.documentElement.getAttribute(k), k), null, k + " after reload");
      same((await attrs(page))["data-font"], "geist", "font after reload");
    });
    await check(`${w} ${tag}: the changed layout really differs before Reset (so the reset proves something)`, async () => {
      await settings(page);
      await changeAll(page);
      const c = await chat(page);
      same(c.fs, 15 * 1.3, "larger text"); same(c.lh, 1.75, "roomy"); same(c.open.includes("true"), true, "open");
      if (w > 1400) same(c.w, 1480, "wider column");
      await page.evaluate(() => { for (const k of ["col", "rs", "sp", "wrap", "open"]) localStorage.removeItem("comebye." + k); });
      await browser.close();
    });
  }

  // Rows: same panels, values, anchored at the row, focus back; keyboard.
  for (const [w, hgt] of [[390, 844], [1280, 800]]) {
    const { browser, page } = await session(w, hgt, dark);
    await check(`${w} ${tag}: Theme and Font rows show the current value and open the fleet menu's panels; a pick updates the row and focus returns to it`, async () => {
      await settings(page);
      await page.evaluate(() => { localStorage.removeItem("comebye.font"); });
      await page.reload({ waitUntil: "load" }); await page.waitForSelector("#sx-reset");
      same(await page.locator("#sx-font-val").textContent(), "System sans", "font value");
      await page.locator("#sx-row-font").click();
      await page.locator("#font-panel").waitFor({ state: "visible" });
      same(await page.locator("#font-panel .tp-opt").count(), 4, "same four fonts");
      if (w > 600) { const r = await page.evaluate(() => { const a = document.getElementById("sx-row-font").getBoundingClientRect(), p = document.getElementById("font-panel").getBoundingClientRect(); return p.top >= a.bottom - 1; }); same(r, true, "panel under the row"); }
      await page.screenshot({ path: `${out}/areset-${w}-${tag}-fontpanel.png` });
      await page.locator('#font-panel .tp-opt:has(.tp-name:text-is("Serif reading"))').click();
      await page.locator("#font-panel").waitFor({ state: "hidden" });
      same(await page.locator("#sx-font-val").textContent(), "Serif reading", "row value follows");
      same(await page.evaluate(() => document.documentElement.getAttribute("data-font")), "serif-reading", "applied");
      same(await page.evaluate(() => document.activeElement.id), "sx-row-font", "focus back on the row");
      // theme
      await page.locator("#sx-row-theme").click();
      await page.locator("#theme-panel").waitFor({ state: "visible" });
      const n = await page.locator("#tp-list [data-theme-id]").count();
      same(n >= 15, true, "the theme list (" + n + ")");
      await page.keyboard.press("Escape");
      await page.locator("#theme-panel").waitFor({ state: "hidden" });
      same(await page.evaluate(() => document.activeElement.id), "sx-row-theme", "Esc returns to the row");
      await page.locator("#sx-row-theme").click();
      await page.locator('#tp-list [data-theme-id="dracula"]').click();
      await page.locator("#theme-panel").waitFor({ state: "hidden" });
      same(await page.evaluate(() => document.documentElement.getAttribute("data-theme")), "dracula", "theme applied");
      same((await page.locator("#sx-theme-val").textContent()).includes("Dracula"), true, "row shows Dracula");
      // Reset leaves both
      await page.locator("#sx-reset").click();
      same(await page.evaluate(() => [document.documentElement.getAttribute("data-theme"), document.documentElement.getAttribute("data-font")]), ["dracula", "serif-reading"], "theme+font survive reset");
      same(await page.locator("#sx-font-val").textContent(), "Serif reading", "font row after reset");
    });
    await check(`${w} ${tag}: the fleet menu keeps Theme and Font`, async () => {
      await page.goto(base + "/#/", { waitUntil: "load" });
      await page.locator("#btn-fleet").click();
      await page.locator("#f-font").click();
      await page.locator("#font-panel").waitFor({ state: "visible" });
      await page.keyboard.press("Escape");
      same(await page.locator("#f-theme").count(), 1, "f-theme exists");
    });
    await check(`${w} ${tag}: keyboard reaches the rows and Reset; Enter opens a picker, Reset works from the keyboard`, async () => {
      await settings(page);
      await radio(page, "sp", "roomy");
      await page.locator("#sx-opentools-on").focus();
      const seen = [];
      for (let i = 0; i < 6 && seen.indexOf("sx-reset") < 0; i++) { await page.keyboard.press("Tab"); seen.push(await page.evaluate(() => document.activeElement.id)); }
      same(seen.includes("sx-row-theme") && seen.includes("sx-row-font") && seen.includes("sx-reset"), true, "Tab reaches both rows and Reset: " + seen);
      await page.keyboard.press("Enter");
      same(await page.evaluate(() => document.documentElement.getAttribute("data-sp")), null, "Enter on Reset resets");
      const h = await page.evaluate(() => document.getElementById("sx-reset").getBoundingClientRect().height);
      same(h >= 36, true, "Reset is a real tap target (" + h + ")");
    });
    await browser.close();
  }

  // Another tab follows.
  {
    const { browser, ctx, page } = await session(1280, 800, dark);
    await check(`1280 ${tag}: Reset in one tab updates a second tab by the storage event`, async () => {
      await settings(page);
      await changeAll(page);
      const p2 = await ctx.newPage();
      await p2.goto(base + "/#/", { waitUntil: "load" });
      same(await p2.evaluate(() => document.documentElement.getAttribute("data-col")), "wider", "tab 2 has the choice");
      await page.locator("#sx-reset").click();
      await p2.waitForFunction(() => !document.documentElement.hasAttribute("data-col") && !document.documentElement.hasAttribute("data-open") && !document.documentElement.hasAttribute("data-rs"));
      same(await p2.evaluate(() => ["data-sp", "data-wrap"].map((a) => document.documentElement.getAttribute(a))), [null, null], "tab 2 sp, wrap");
    });
    await browser.close();
  }
}

if (log.length) { console.log("console:\n  " + log.join("\n  ")); results.push(false); }
process.exit(results.every(Boolean) ? 0 : 1);
