// The list column is recessed from the conversation (Nav contrast, slice 1): from 1100px the list column paints
// --nav-bg (the body, since #list is display:contents; the list header, #agents and the rest are transparent over
// it) and #chat, #sv-chat, the detail and the pages stay on --bg. At 1280 and 3440, in Comebye Light, Comebye Dark
// and Gruvbox Dark, the list's effective background differs from #chat by the planned step (4.5% black over bg in
// light themes, 50% in dark ones) and the detail and the empty state are on --bg; at 390 the two are equal.
//
//   node navcontrast.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";
import { claude } from "./fixtures/toolruns.mjs";

const [base, out] = process.argv.slice(2);
const results = [], log = [];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const THEMES = [{ id: "light", dark: false }, { id: "dark", dark: true }, { id: "gruvbox-dark", dark: true }];
const rgb = (s) => (s.match(/[\d.]+/g) || []).slice(0, 4).map(Number);
const hex = (h) => [1, 3, 5].map((i) => parseInt(h.slice(i, i + 2), 16));
const planned = (bg, dark) => hex(bg).map((v) => Math.round(v * (1 - (dark ? 0.5 : 0.045))));
const lum = (c) => { const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]); };
const ratio = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
const near = (a, b) => a.every((v, i) => Math.abs(v - b[i]) <= 1);

async function read(width, theme) {
  const { browser, page } = await launch({ width, height: width < 500 ? 844 : 900, dark: theme.dark });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await page.addInitScript((id) => { try { localStorage.setItem("comebye.theme", id); } catch {} }, theme.id);
  await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: claude, total: claude.length, more: false, format: "claude" } }));
  try {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#agents .agent");
    const empty = width >= 1100 ? await page.evaluate(() => getComputedStyle(document.querySelector("#detail-empty")).backgroundColor) : "";
    const list = await page.evaluate(() => ({
      body: getComputedStyle(document.body).backgroundColor, agents: getComputedStyle(document.querySelector("#agents")).backgroundColor,
      head: getComputedStyle(document.querySelector("#list header")).backgroundColor,
      bg: getComputedStyle(document.documentElement).getPropertyValue("--bg").trim(), nav: getComputedStyle(document.documentElement).getPropertyValue("--nav-bg").trim() }));
    await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
    await page.waitForSelector("#chat .msg");
    const main = await page.evaluate(() => ({ chat: getComputedStyle(document.querySelector("#chat")).backgroundColor, detail: getComputedStyle(document.querySelector("#detail")).backgroundColor }));
    if (process.env.NAV_SHOTS) await page.screenshot({ path: `${out}/navcontrast-${theme.id}-${width}.png` });
    return { ...list, ...main, empty };
  } finally { await browser.close(); }
}

for (const th of THEMES) {
  for (const w of [1280, 3440]) {
    await check(`${th.id} @${w}: list recessed from #chat, detail on bg`, async () => {
      const r = await read(w, th);
      const want = planned(r.bg, th.dark);
      if (!near(rgb(r.body), want)) throw new Error(`list (body) ${r.body}, want ${want} (${th.dark ? "50%" : "4.5%"} black over ${r.bg})`);
      if (!near(rgb(r.body), hex(r.nav))) throw new Error(`list ${r.body} is not --nav-bg ${r.nav}`);
      if (!near(rgb(r.chat), hex(r.bg))) throw new Error(`#chat ${r.chat}, want --bg ${r.bg}`);
      if (!near(rgb(r.detail), hex(r.bg))) throw new Error(`#detail ${r.detail}, want --bg ${r.bg}`);
      if (!near(rgb(r.empty), hex(r.bg))) throw new Error(`#detail-empty ${r.empty}, want --bg ${r.bg}`);
      if (rgb(r.agents)[3] !== 0 || rgb(r.head)[3] !== 0) throw new Error(`#agents ${r.agents} / list header ${r.head} must be transparent so the column is one surface`);
      const k = ratio(rgb(r.body), rgb(r.chat));
      if (k < 1.04) throw new Error(`list vs #chat contrast ${k.toFixed(3)} < 1.04`);
      console.log(`     ${th.id} ${w}: list ${r.body} vs #chat ${r.chat} = ${k.toFixed(2)}:1`);
    });
  }
  await check(`${th.id} @390: list and #chat are the same colour`, async () => {
    const r = await read(390, th);
    if (!near(rgb(r.body), hex(r.bg)) || !near(rgb(r.body), rgb(r.chat))) throw new Error(`list ${r.body} vs #chat ${r.chat}, want both --bg ${r.bg}`);
  });
}
if (log.length) console.log(log.join("\n"));
process.exit(results.every(Boolean) && !log.length ? 0 : 1);
