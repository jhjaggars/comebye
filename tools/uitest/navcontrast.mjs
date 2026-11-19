// The list column is recessed from the conversation (Nav contrast, slice 1): from 1100px the list column paints
// --nav-bg (the body, since #list is display:contents; the list header, #agents and the rest are transparent over
// it) and #chat, #sv-chat, the detail and the pages stay on --bg. At 1280 and 3440, in Comebye Light, Comebye Dark
// and Gruvbox Dark, the list's effective background is --nav-bg, a step from #chat inside the tuned bounds
// (slice 3: dark themes 1.09-1.16:1, light 1.09-1.12:1, in all 15 themes), and the detail and the empty state are
// on --bg; at 390 the two are equal.
//
// Slice 2 carries it through the rest of the column: the keybar footer is transparent on the nav (not a lighter
// --panel strip) with its text and key caps readable; the collapsed 56px rail and its hover; the host filter's
// pills (--panel, one step above the nav, text 4.5:1); the list header stays transparent (one surface); and every
// screen that opens as the right-hand cell (Settings, Peers, Usage, Search, Pages, Status, To do, Briefing) is
// --bg with a 1px --border left edge exactly where the list column ends, so the step reads as a clean edge.
//
//   node navcontrast.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";
import { claude } from "./fixtures/toolruns.mjs";

const [base, out] = process.argv.slice(2);
const results = [], log = [];
const SCREENS = [["settings", "#settings"], ["peers", "#peers"], ["usage", "#usage"], ["pages", "#pages"], ["status", "#p-status"], ["todo", "#p-todo"], ["briefings", "#briefs"]];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
// Slice 3: the step between --bg and --nav-bg, read clearly but calmly: [min, max] luminance contrast.
const STEP = { dark: [1.09, 1.16], light: [1.09, 1.12] };
const THEMES = [{ id: "light", dark: false }, { id: "dark", dark: true }, { id: "gruvbox-dark", dark: true }];
const rgb = (s) => (s.match(/[\d.]+/g) || []).slice(0, 4).map(Number);
const hex = (h) => [1, 3, 5].map((i) => parseInt(h.slice(i, i + 2), 16));
const lum = (c) => { const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]); };
const ratio = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
const near = (a, b) => a.every((v, i) => Math.abs(v - b[i]) <= 1);

// All 15 themes' tokens, no browser: the step is inside the bounds and text, muted and the accents hold 4.5:1 on the nav,
// the selected row outline and the focus ring 3:1 (themes.js is the one source of the tokens).
import fs from "node:fs"; import vm from "node:vm"; import path from "node:path"; import { fileURLToPath } from "node:url";
const tctx = { globalThis: null }; tctx.globalThis = tctx; vm.createContext(tctx);
vm.runInContext(fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "..", "web", "themes.js"), "utf8"), tctx);
const { THEMES: ALL, contrast: tcontrast } = tctx.ComebyeThemes;
await check("all 15 themes: nav step in bounds, text/muted/accents >= 4.5 and focus/selected outline >= 3 on the nav", async () => {
  const rows = [];
  for (const t of ALL) {
    const v = t.vars, step = tcontrast(v.bg, v["nav-bg"]), [lo, hi] = STEP[t.mode];
    if (step < lo || step > hi) throw new Error(`${t.id}: step ${step.toFixed(3)} (bg ${v.bg}, nav ${v["nav-bg"]}) outside ${lo}-${hi}`);
    for (const k of ["text", "muted", "quote", "blue", "orange", "green", "purple", "red", "yellow", "teal"]) if (tcontrast(v[k], v["nav-bg"]) < 4.5) throw new Error(`${t.id}: ${k} on nav ${tcontrast(v[k], v["nav-bg"]).toFixed(2)} < 4.5`);
    for (const bgk of ["nav-bg", "panel2"]) if (tcontrast(v.focus, v[bgk]) < 3) throw new Error(`${t.id}: focus ring/selected outline on ${bgk} ${tcontrast(v.focus, v[bgk]).toFixed(2)} < 3`);
    rows.push(`${t.id} ${step.toFixed(2)}`);
  }
  if (ALL.length !== 15) throw new Error(`${ALL.length} themes, expected 15`);
  console.log("     steps: " + rows.join(", "));
});

async function read(width, theme) {
  const { browser, page } = await launch({ width, height: width < 500 ? 844 : 900, dark: theme.dark });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await page.addInitScript((id) => { try { localStorage.setItem("comebye.theme", id); } catch {} }, theme.id);
  await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: claude, total: claude.length, more: false, format: "claude" } }));
  try {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#agents .agent");
    // A second host, so the host filter shows (it is hidden on a single-host instance).
    await page.evaluate(() => { state.hosts = [{ name: "hub", local: true, status: "ok" }, { name: "buildbox", status: "ok" }]; renderHostFilter(); });
    const empty = width >= 1100 ? await page.evaluate(() => getComputedStyle(document.querySelector("#detail-empty")).backgroundColor) : "";
    const list = await page.evaluate(() => ({
      body: getComputedStyle(document.body).backgroundColor, agents: getComputedStyle(document.querySelector("#agents")).backgroundColor,
      head: getComputedStyle(document.querySelector("#list header")).backgroundColor,
      bg: getComputedStyle(document.documentElement).getPropertyValue("--bg").trim(), nav: getComputedStyle(document.documentElement).getPropertyValue("--nav-bg").trim() }));
    await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
    await page.waitForSelector("#chat .msg");
    const main = await page.evaluate(() => ({ chat: getComputedStyle(document.querySelector("#chat")).backgroundColor, detail: getComputedStyle(document.querySelector("#detail")).backgroundColor }));
    const parts = await page.evaluate(() => {
      const cs = (sel) => { const e = document.querySelector(sel); return e ? getComputedStyle(e) : null; };
      const kb = cs("#keybar"), hint = cs("#keybar .kb-hint"), mode = cs("#kb-mode"), hf = document.querySelector("#host-filter .hp-btn:not([data-narrow])");
      const kbd = cs("#keybar kbd");
      return { keybar: kb && { bg: kb.backgroundColor, color: kb.color, top: kb.borderTopColor, topw: kb.borderTopWidth, hint: hint && hint.color, kbdBg: kbd && kbd.backgroundColor, kbdFg: kbd && kbd.color },
        hfBox: cs("#host-filter") && cs("#host-filter").backgroundColor,
        hf: hf && { bg: getComputedStyle(hf).backgroundColor, fg: getComputedStyle(hf).color }, listw: document.querySelector("#detail").getBoundingClientRect().left };
    });
    if (process.env.NAV_SHOTS) await page.screenshot({ path: `${out}/navcontrast-${theme.id}-${width}.png` });
    return { ...list, ...main, empty, ...parts };
  } finally { await browser.close(); }
}

for (const th of THEMES) {
  for (const w of [1280, 3440]) {
    await check(`${th.id} @${w}: list recessed from #chat, detail on bg`, async () => {
      const r = await read(w, th);
      if (!near(rgb(r.body), hex(r.nav))) throw new Error(`list ${r.body} is not --nav-bg ${r.nav}`);
      if (!near(rgb(r.chat), hex(r.bg))) throw new Error(`#chat ${r.chat}, want --bg ${r.bg}`);
      if (!near(rgb(r.detail), hex(r.bg))) throw new Error(`#detail ${r.detail}, want --bg ${r.bg}`);
      if (!near(rgb(r.empty), hex(r.bg))) throw new Error(`#detail-empty ${r.empty}, want --bg ${r.bg}`);
      if (rgb(r.agents)[3] !== 0 || rgb(r.head)[3] !== 0) throw new Error(`#agents ${r.agents} / list header ${r.head} must be transparent so the column is one surface`);
      const k = ratio(rgb(r.body), rgb(r.chat));
      const [lo, hi] = STEP[th.dark ? 'dark' : 'light'];
      if (k < lo || k > hi) throw new Error(`list vs #chat step ${k.toFixed(3)} outside ${lo}-${hi}`);
      console.log(`     ${th.id} ${w}: list ${r.body} vs #chat ${r.chat} = ${k.toFixed(2)}:1`);
    });
  }
  await check(`${th.id} @390: list and #chat are the same colour`, async () => {
    const r = await read(390, th);
    if (!near(rgb(r.body), hex(r.bg)) || !near(rgb(r.body), rgb(r.chat))) throw new Error(`list ${r.body} vs #chat ${r.chat}, want both --bg ${r.bg}`);
  });
}

// The footer, host filter and header are part of the one list surface.
for (const th of THEMES) {
  for (const w of [1280, 3440]) {
    await check(`${th.id} @${w}: keybar, host filter and header sit on the nav`, async () => {
      const r = await read(w, th), nav = rgb(r.body);
      if (!r.keybar) throw new Error("no #keybar");
      if (rgb(r.keybar.bg)[3] !== 0) throw new Error(`keybar ${r.keybar.bg} must be transparent (the column is one surface, not a lighter --panel strip)`);
      if (r.keybar.topw !== "1px") throw new Error(`keybar top border ${r.keybar.topw}, want 1px`);
      for (const [what, fg] of [["footer text", r.keybar.color], ["footer hint", r.keybar.hint]]) {
        const k = ratio(rgb(fg), nav); if (k < 4.5) throw new Error(`${what} ${fg} on nav ${r.body} = ${k.toFixed(2)}:1 < 4.5`);
      }
      const kc = ratio(rgb(r.keybar.kbdFg), rgb(r.keybar.kbdBg)); if (kc < 4.5) throw new Error(`key cap text ${r.keybar.kbdFg} on ${r.keybar.kbdBg} = ${kc.toFixed(2)}:1 < 4.5`);
      if (rgb(r.hfBox)[3] !== 0) throw new Error(`#host-filter ${r.hfBox} must be transparent`);
      if (!r.hf) throw new Error("no host filter pill (fixtures have a second host)");
      const k = ratio(rgb(r.hf.fg), rgb(r.hf.bg)); if (k < 4.5) throw new Error(`host pill text ${r.hf.fg} on ${r.hf.bg} = ${k.toFixed(2)}:1 < 4.5`);
      if (near(rgb(r.hf.bg), nav)) throw new Error(`host pill ${r.hf.bg} is invisible against the nav ${r.body}`);
      console.log(`     ${th.id} ${w}: footer text ${ratio(rgb(r.keybar.color), nav).toFixed(2)}:1, host pill vs nav ${ratio(rgb(r.hf.bg), nav).toFixed(2)}:1`);
    });
  }
}

async function readRail(width, theme) {
  const { browser, page } = await launch({ width, height: 900, dark: theme.dark });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await page.addInitScript((id) => { try { localStorage.setItem("comebye.theme", id); localStorage.setItem("comebye-list-collapsed", "1"); } catch {} }, theme.id);
  await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: claude, total: claude.length, more: false, format: "claude" } }));
  try {
    await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
    await page.waitForSelector("#chat .msg");
    await page.waitForSelector("body.list-collapsed #list-rail");
    const get = () => page.evaluate(() => { const r = document.querySelector("#list-rail"), c = getComputedStyle(r), b = r.getBoundingClientRect();
      return { bg: c.backgroundColor, fg: c.color, w: b.width, body: getComputedStyle(document.body).backgroundColor, chat: getComputedStyle(document.querySelector("#chat")).backgroundColor, panel: getComputedStyle(document.documentElement).getPropertyValue("--panel").trim() }; });
    const rest = await get();
    await page.hover("#list-rail"); await page.waitForTimeout(250);
    const hover = await get();
    if (process.env.NAV_SHOTS) await page.screenshot({ path: `${out}/navrail-${theme.id}-${width}.png` });
    return { rest, hover };
  } finally { await browser.close(); }
}
for (const th of THEMES) {
  for (const w of [1280, 3440]) {
    await check(`${th.id} @${w}: collapsed rail is the nav surface, hover steps up to --panel`, async () => {
      const { rest, hover } = await readRail(w, th);
      if (Math.round(rest.w) !== 56) throw new Error(`rail ${rest.w}px wide, want 56`);
      if (!near(rgb(rest.bg), rgb(rest.body))) throw new Error(`rail ${rest.bg} is not the nav surface ${rest.body}`);
      if (ratio(rgb(rest.bg), rgb(rest.chat)) < 1.09) throw new Error(`rail vs #chat ${ratio(rgb(rest.bg), rgb(rest.chat)).toFixed(3)} < 1.09`);
      if (!near(rgb(hover.bg), hex(hover.panel))) throw new Error(`rail hover ${hover.bg}, want --panel ${hover.panel}`);
      const k = ratio(rgb(hover.fg), rgb(hover.bg)); if (k < 4.5) throw new Error(`rail icon ${hover.fg} on hover ${hover.bg} = ${k.toFixed(2)}:1 < 4.5`);
      const k2 = ratio(rgb(rest.fg), rgb(rest.bg)); if (k2 < 4.5) throw new Error(`rail icon ${rest.fg} on ${rest.bg} = ${k2.toFixed(2)}:1 < 4.5`);
    });
  }
}

async function readScreen(width, theme, route, sel) {
  const { browser, page } = await launch({ width, height: 900, dark: theme.dark });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await page.addInitScript((id) => { try { localStorage.setItem("comebye.theme", id); } catch {} }, theme.id);
  try {
    await page.goto(base + "/#/" + route, { waitUntil: "load" });
    await page.waitForSelector(sel, { state: "visible" });
    await page.waitForTimeout(300);
    const r = await page.evaluate((sel) => { const e = document.querySelector(sel), c = getComputedStyle(e), b = e.getBoundingClientRect(), root = getComputedStyle(document.documentElement);
      const h = e.querySelector("header") && getComputedStyle(e.querySelector("header"));
      return { bg: c.backgroundColor, left: b.left, bw: c.borderLeftWidth, bc: c.borderLeftColor, bgvar: root.getPropertyValue("--bg").trim(), border: root.getPropertyValue("--border").trim(), nav: getComputedStyle(document.body).backgroundColor,
        listw: parseFloat(getComputedStyle(document.body).gridTemplateColumns.split(" ")[0]), head: h && h.backgroundColor }; }, sel);
    if (process.env.NAV_SHOTS) await page.screenshot({ path: `${out}/navscreen-${route.split("/")[0]}-${theme.id}-${width}.png` });
    return r;
  } finally { await browser.close(); }
}
for (const th of THEMES) {
  for (const w of [1280, 3440]) {
    for (const [route, sel] of SCREENS) {
      await check(`${th.id} @${w}: ${route} cell is on --bg with a clean 1px edge at the list column`, async () => {
        const r = await readScreen(w, th, route, sel);
        if (!near(rgb(r.bg), hex(r.bgvar))) throw new Error(`${sel} ${r.bg}, want --bg ${r.bgvar}`);
        if (r.head && rgb(r.head)[3] !== 0 && !near(rgb(r.head), hex(r.bgvar))) throw new Error(`${sel} header ${r.head}, want --bg ${r.bgvar}`);
        if (Math.abs(r.left - r.listw) > 1) throw new Error(`${sel} starts at x=${r.left}, the list column ends at ${r.listw}`);
        if (r.bw !== "1px" || !near(rgb(r.bc), hex(r.border))) throw new Error(`${sel} left edge ${r.bw} ${r.bc}, want 1px --border ${r.border}`);
        if (near(rgb(r.nav), rgb(r.bg))) throw new Error(`the list column ${r.nav} is not distinct from the ${sel} cell ${r.bg}`);
      });
    }
  }
}
if (log.length) console.log(log.join("\n"));
process.exit(results.every(Boolean) && !log.length ? 0 : 1);
