// Left nav, slice 2: the host chips are one host picker (issue #30).
// A button names the selection ("All hosts", "laharl + mac", "3 hosts") and opens a checklist (a bottom sheet on a phone).
// With 5, 8 and 14 hosts the control never overflows; two hosts show only their agents plus every blocked one, with a
// "+N outside this filter" note; the old single stored value still reads and the new value is an array; h, the arrows,
// Space, Enter and Esc work and focus returns to the button; the sheet traps focus and closes on a swipe down; a down
// host is listed and cannot be picked; light and dark at 390, 1280 and 3440.
//
//   (a part of navjourney.mjs) hostpicker BASE OUT_DIR
//
// A part of navjourney.mjs (journey.mjs): the script as it was, as the body of run(process); `process` is the one the
// journey gives it (its own argv and env), and process.exit ends the part.
import { launch, settled } from "./lib.mjs";
import fs from "node:fs"; import vm from "node:vm"; import path from "node:path"; import { fileURLToPath } from "node:url";

export async function run(process) {
const [base, out] = process.argv.slice(2);
const results = [];
// idle: two frames, then no CSS transition or animation still running (a sheet has slid in, a pull has sprung back), not a timer.
const idle = (page) => settled(page); // lib.mjs: no fetch in flight, no redraw pending, no finite animation running
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

const NAMES = ["laharl", "mac", "jjaggars-kubevirt", "comebye-hub", "gpu-box", "build-farm-eu-west-2", "pi-zero", "nas", "edge-1", "edge-2", "edge-3", "edge-4", "edge-5", "edge-6"];
// n hosts, 4 agents each (the first agent on hosts 3 and 4 is blocked); host 5 is down; host 2 runs another version.
function fleet(n) {
  const now = new Date().toISOString();
  const hosts = NAMES.slice(0, n).map((name, i) => ({ name, local: i === 0, status: i === 4 ? "down" : "ok", version: i === 1 ? "v1.1.0" : "v1.2.0", version_mismatch: i === 1, since: i === 4 ? new Date(Date.now() - 7200e3).toISOString() : undefined, agents: 4 }));
  const agents = [];
  hosts.forEach((h, i) => { for (let k = 0; k < 4; k++) agents.push({ id: `${h.name}/w${i}${k}:p1`, host: h.name, pane_id: `w${i}${k}:p1`, name: `a-${h.name.slice(0, 6)}-${k}`, agent: "claude", display_agent: "Claude", agent_status: k === 0 && (i === 2 || i === 3) ? "blocked" : k === 1 ? "working" : "idle", cwd: "/home/u/code/p" + k, worktree: { repo: "p" + k, checkout: "/home/u/code/p" + k, linked: false }, last_active: now, status_since: now }); });
  return { agents, hosts };
}

async function open(width, dark, n = 5, store) {
  const { browser, page } = await launch({ width, height: width < 500 ? 844 : 900, dark });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(e.message));
  // The fleet is the server's answer, and the app stops taking the live feed's, so a poll cannot put the real two hosts back.
  const f = fleet(n);
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent");
  await page.evaluate(([f, st]) => {
    try { st === undefined ? localStorage.removeItem("herdr-host-filter") : localStorage.setItem("herdr-host-filter", st); } catch {}
    state.hostFilter = null; state.hosts = f.hosts; setAgents(f); window.setAgents = () => {}; renderList();
  }, [f, store]);
  await idle(page);
  return { browser, page, errs };
}
const shown = (page) => page.evaluate(() => [...document.querySelectorAll("#agents .agent[data-id]")].map((e) => e.dataset.id));
const stored = (page) => page.evaluate(() => localStorage.getItem("herdr-host-filter"));
const label = (page) => page.locator("#hp-btn .hp-label").innerText();

for (const dark of [false, true]) {
  const scheme = dark ? "dark" : "light";
  for (const w of [390, 1280, 3440]) {
    for (const n of [5, 8, 14]) {
      await check(`${scheme} @${w}: ${n} hosts: one button, no chips, the host control and the page never overflow (open or closed)`, async () => {
        const { browser, page, errs } = await open(w, dark, n);
        try {
          if (await page.locator("#host-filter .hf-chip").count()) throw new Error("host chips are still drawn");
          const m = () => page.evaluate(() => { const b = document.querySelector("#host-filter"); const p = document.querySelector("#hp-pop"); return { bar: [b.scrollWidth, b.clientWidth], pop: [p.scrollWidth, p.clientWidth], list: [document.querySelector("#hp-list").scrollWidth, document.querySelector("#hp-list").clientWidth], page: [document.documentElement.scrollWidth, document.documentElement.clientWidth], btnH: document.querySelector("#hp-btn").getBoundingClientRect().height, need: !!document.querySelector("#hp-btn .hp-need") }; });
          let g = await m();
          if (g.bar[0] !== g.bar[1]) throw new Error("bar overflows " + g.bar);
          if (g.page[0] > g.page[1]) throw new Error("page scrolls sideways " + g.page);
          if (g.btnH < 36) throw new Error("button " + g.btnH);
          if (!g.need) throw new Error("no need-you dot on the button (two hosts have blocked agents)");
          await page.click("#hp-btn");
          await page.waitForSelector("#hp-pop:not([hidden])");
          g = await m();
          if (g.pop[0] !== g.pop[1] || g.list[0] !== g.list[1]) throw new Error("popover overflows " + JSON.stringify(g));
          if (g.page[0] > g.page[1]) throw new Error("page scrolls sideways with the picker open");
          const rows = await page.locator("#hp-list .hp-row").count();
          if (rows !== n + 1) throw new Error(`${rows} rows, want ${n + 1}`);
          if ((n >= 8) !== await page.locator("#hp-q").isVisible()) throw new Error("the find box should show from 8 hosts");
          await idle(page);
          await page.screenshot({ path: `${out}/hostpicker-${w}-${n}-${scheme}.png` });
          if (errs.length) throw new Error(errs.join("; "));
        } finally { await browser.close(); }
      });
    }
  }

  await check(`${scheme} @1280: the rows carry a need dot, a version badge, a down state and a count; down is listed but not selectable`, async () => {
    const { browser, page } = await open(1280, dark, 5);
    try {
      await page.click("#hp-btn");
      const rows = await page.locator("#hp-list .hp-row[data-host]:not([data-host=''])").evaluateAll((els) => els.map((e) => ({ h: e.dataset.host, dis: e.disabled, need: !!e.querySelector(".hp-need:not(.off)"), ver: e.querySelector(".ver")?.textContent || "", n: e.querySelector(".n").textContent })));
      const by = Object.fromEntries(rows.map((r) => [r.h, r]));
      if (!by["jjaggars-kubevirt"].need || !by["comebye-hub"].need || by.laharl.need) throw new Error("need dots " + JSON.stringify(rows));
      if (by.mac.ver !== "v1.1.0" || by.laharl.ver) throw new Error("version badges " + JSON.stringify(rows));
      if (!by["gpu-box"].dis || !/^down 2h$/.test(by["gpu-box"].n)) throw new Error("gpu-box " + JSON.stringify(by["gpu-box"]));
      if (by.laharl.n !== "4" || by.laharl.dis) throw new Error("laharl " + JSON.stringify(by.laharl));
      await page.locator('#hp-list .hp-row[data-host="gpu-box"]').click({ force: true });
      if (await stored(page) !== null && JSON.parse(await stored(page)).includes("gpu-box")) throw new Error("a down host was picked");
      if (await page.locator('#hp-list .hp-row[data-host="gpu-box"][aria-checked="true"]').count()) throw new Error("down host shows checked");
    } finally { await browser.close(); }
  });

  await check(`${scheme} @1280: two hosts: the button says "laharl + mac", only their agents and the blocked ones show, "+2 outside this filter"; stored as an array`, async () => {
    const { browser, page } = await open(1280, dark, 5);
    try {
      if (await label(page) !== "All hosts") throw new Error("label " + await label(page));
      await page.click("#hp-btn");
      await page.click('#hp-list .hp-row[data-host="laharl"]');
      await page.click('#hp-list .hp-row[data-host="mac"]');
      const ids = await shown(page);
      const hosts = new Set(ids.map((i) => i.split("/")[0]));
      if ([...hosts].sort().join() !== "comebye-hub,jjaggars-kubevirt,laharl,mac") throw new Error("hosts shown: " + [...hosts]);
      const other = await page.evaluate(() => state.agents.filter((a) => !["laharl", "mac"].includes(a.host)));
      for (const a of other) if ((a.agent_status === "blocked") !== ids.includes(a.id)) throw new Error(`${a.id} (${a.agent_status}) shown=${ids.includes(a.id)}: only blocked agents ignore the filter`);
      if (ids.length !== 8 + 2) throw new Error(ids.length + " rows, want 10");
      if (await label(page) !== "laharl + mac") throw new Error("label " + await label(page));
      if (!await page.locator("#hp-btn[data-narrow]").count()) throw new Error("the button does not show it narrows");
      const note = (await page.locator("#host-filter .hp-out").innerText()).trim();
      if (note !== "+2 outside this filter") throw new Error("note " + JSON.stringify(note));
      if (stored(page) && JSON.stringify(JSON.parse(await stored(page))) !== '["laharl","mac"]') throw new Error("stored " + await stored(page));
      await page.click('#hp-list .hp-row[data-host="jjaggars-kubevirt"]');
      if (await label(page) !== "3 hosts") throw new Error("label " + await label(page));
      await page.click('#hp-list .hp-row[data-host=""]');
      if (await label(page) !== "All hosts" || (await shown(page)).length !== 20) throw new Error("clear: " + await label(page) + ", " + (await shown(page)).length + " rows");
      if (await page.locator("#host-filter .hp-out:not([hidden])").count()) throw new Error("the outside note stays with all hosts");
      await page.keyboard.press("Escape");
      await page.screenshot({ path: `${out}/hostpicker-two-${scheme}.png` });
    } finally { await browser.close(); }
  });

  await check(`${scheme}: the old single stored value still works (a name, "all", garbage) and a reload keeps an array`, async () => {
    for (const [st, want, shownHosts] of [["mac", "mac", "comebye-hub,jjaggars-kubevirt,mac"], ["all", "All hosts", null], ["nowhere", "All hosts", null], ['["laharl","gone","mac"]', "laharl + mac", "comebye-hub,jjaggars-kubevirt,laharl,mac"], ["[not json", "All hosts", null]]) {
      const { browser, page } = await open(1280, dark, 5, st);
      try {
        if (await label(page) !== want) throw new Error(`${JSON.stringify(st)}: label ${await label(page)}, want ${want}`);
        if (shownHosts) { const h = [...new Set((await shown(page)).map((i) => i.split("/")[0]))].sort().join(); if (h !== shownHosts) throw new Error(`${st}: hosts ${h}`); }
      } finally { await browser.close(); }
    }
    const { browser, page } = await open(1280, dark, 5, "mac");
    try {
      await page.click("#hp-btn"); await page.click('#hp-list .hp-row[data-host="laharl"]');
      if (await stored(page) !== '["mac","laharl"]') throw new Error("stored " + await stored(page));
      await page.keyboard.press("Escape");
      const re = await page.evaluate(() => { state.hostFilter = null; return hostSel(); });
      if (re.join() !== "mac,laharl") throw new Error("reload " + re);
    } finally { await browser.close(); }
  });

  await check(`${scheme} @1280: keyboard: h opens, arrows move, Space toggles, Enter applies and closes, Esc closes, focus returns to the button; / stays Search`, async () => {
    const { browser, page } = await open(1280, dark, 5);
    try {
      await page.locator("body").click({ position: { x: 700, y: 400 } });
      await page.keyboard.press("h");
      await page.waitForSelector("#hp-pop:not([hidden])");
      const first = await page.evaluate(() => document.activeElement.dataset.host);
      if (first === undefined) throw new Error("focus is not on a row");
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("ArrowDown");
      const at = await page.evaluate(() => document.activeElement.dataset.host);
      if (at === first) throw new Error("the arrows did not move");
      await page.keyboard.press("Space");
      if ((await page.evaluate(() => hostSel())).join() !== at) throw new Error("Space did not pick " + at);
      if (await page.evaluate(() => document.activeElement.dataset.host) !== at) throw new Error("focus jumped off the row after a toggle");
      await page.keyboard.press("Space");
      if ((await page.evaluate(() => hostSel())).length) throw new Error("Space did not untick");
      await page.keyboard.press("Space");
      await page.keyboard.press("Enter");
      if (!await page.locator("#hp-pop").isHidden()) throw new Error("Enter did not close");
      if (await page.evaluate(() => document.activeElement.id) !== "hp-btn") throw new Error("focus not on the button after Enter");
      if ((await page.evaluate(() => hostSel())).join() !== at) throw new Error("Enter dropped the selection");
      await page.keyboard.press("h");
      await page.waitForSelector("#hp-pop:not([hidden])");
      await page.keyboard.press("Escape");
      if (!await page.locator("#hp-pop").isHidden()) throw new Error("Esc did not close");
      if (await page.evaluate(() => document.activeElement.id) !== "hp-btn") throw new Error("focus not on the button after Esc");
      // h does not fire while typing, / still opens Search.
      await page.evaluate(() => document.activeElement.blur());
      await page.keyboard.press("/");
      await page.waitForSelector("#recall:not([hidden])");
      if (!await page.locator("#hp-pop").isHidden()) throw new Error("/ opened the picker");
    } finally { await browser.close(); }
  });

  await check(`${scheme} @1280: with 14 hosts h focuses the find box, typing narrows the list, Esc closes`, async () => {
    const { browser, page } = await open(1280, dark, 14);
    try {
      await page.locator("body").click({ position: { x: 700, y: 400 } });
      await page.keyboard.press("h");
      await page.waitForSelector("#hp-pop:not([hidden])");
      if (await page.evaluate(() => document.activeElement.id) !== "hp-q") throw new Error("focus not on the find box");
      await page.keyboard.type("edge-1");
      const vis = await page.locator("#hp-list .hp-row:visible").count();
      if (vis !== 1) throw new Error(vis + " rows match edge-1");
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Space");
      if ((await page.evaluate(() => hostSel())).join() !== "edge-1") throw new Error("could not pick from the filtered list");
      await page.keyboard.press("Escape");
      if (await page.evaluate(() => document.activeElement.id) !== "hp-btn") throw new Error("focus not on the button");
    } finally { await browser.close(); }
  });

  await check(`${scheme} @390: the phone gets a bottom sheet that traps focus, closes on Esc, the x, a tap outside and a swipe down`, async () => {
    const { browser, page } = await open(390, dark, 8);
    try {
      await page.click("#hp-btn");
      await page.waitForSelector("#hp-pop:not([hidden])");
      await idle(page);
      const g = await page.evaluate(() => { const r = document.querySelector("#hp-pop").getBoundingClientRect(); return { l: r.left, r: r.right, b: r.bottom, h: r.height, vh: innerHeight, bk: !document.querySelector("#sheet-bk").hidden, grab: getComputedStyle(document.querySelector("#hp-pop .grab")).display }; });
      if (g.l !== 0 || g.r !== 390 || Math.abs(g.b - g.vh) > 1 || !g.bk || g.grab === "none") throw new Error("not a bottom sheet " + JSON.stringify(g));
      // Tab wraps inside the sheet, both ways.
      const inside = () => page.evaluate(() => document.querySelector("#hp-pop").contains(document.activeElement));
      for (let i = 0; i < 30; i++) { await page.keyboard.press("Tab"); if (!await inside()) throw new Error("Tab left the sheet after " + (i + 1)); }
      for (let i = 0; i < 30; i++) { await page.keyboard.press("Shift+Tab"); if (!await inside()) throw new Error("Shift+Tab left the sheet after " + (i + 1)); }
      await page.screenshot({ path: `${out}/hostpicker-sheet-${scheme}.png` });
      await page.keyboard.press("Escape");
      if (!await page.locator("#hp-pop").isHidden() || await page.evaluate(() => document.activeElement.id) !== "hp-btn") throw new Error("Esc");
      await page.click("#hp-btn"); await page.click("#hp-pop .pop-x");
      if (!await page.locator("#hp-pop").isHidden()) throw new Error("the x");
      await page.click("#hp-btn"); await idle(page);
      await page.touchscreen.tap(195, 40);
      if (!await page.locator("#hp-pop").isHidden()) throw new Error("tap outside");
      // A short pull springs back; a long one from the handle closes.
      await page.click("#hp-btn"); await idle(page);
      const hb = await page.locator("#hp-pop .grab").boundingBox(), cx = hb.x + hb.width / 2, cy = hb.y + hb.height / 2;
      await page.mouse.move(cx, cy); await page.mouse.down(); await page.mouse.move(cx, cy + 20, { steps: 3 }); await page.mouse.up();
      await idle(page);
      if (await page.locator("#hp-pop").isHidden()) throw new Error("a 20px pull closed the sheet");
      const hb2 = await page.locator("#hp-pop .grab").boundingBox();
      await page.mouse.move(hb2.x + hb2.width / 2, hb2.y + hb2.height / 2); await page.mouse.down();
      await page.mouse.move(hb2.x + hb2.width / 2, hb2.y + 160, { steps: 8 }); await page.mouse.up();
      await page.waitForSelector("#hp-pop", { state: "hidden", timeout: 2000 }).catch(() => { throw new Error("the swipe down did not close the sheet"); });
      if (await page.evaluate(() => document.activeElement.id) !== "hp-btn") throw new Error("focus not back on the button after the swipe");
    } finally { await browser.close(); }
  });

  await check(`${scheme} @390: the phone sheet picks two hosts and the list follows`, async () => {
    const { browser, page } = await open(390, dark, 5);
    try {
      await page.click("#hp-btn"); await idle(page);
      await page.tap('#hp-list .hp-row[data-host="laharl"]'); await page.tap('#hp-list .hp-row[data-host="mac"]');
      if (await label(page) !== "laharl + mac") throw new Error("label " + await label(page));
      if (!(await page.locator("#hp-pop").isVisible())) throw new Error("the sheet closed on a pick");
      await page.keyboard.press("Escape");
      await page.screenshot({ path: `${out}/hostpicker-phone-two-${scheme}.png` });
      if ((await shown(page)).length !== 10) throw new Error("rows " + (await shown(page)).length);
    } finally { await browser.close(); }
  });
}

// Colours: the button, the popover and the note read in all 15 themes, both from the tokens and on the painted column.
const tctx = { globalThis: null }; tctx.globalThis = tctx; vm.createContext(tctx);
vm.runInContext(fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "..", "web", "themes.js"), "utf8"), tctx);
const { THEMES: ALL, contrast } = tctx.ComebyeThemes;
const rgb = (s) => (s.match(/[\d.]+/g) || []).slice(0, 4).map(Number);
const lum = (c) => { const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]); };
const ratio = (a, b) => { const x = lum(rgb(a)), y = lum(rgb(b)); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
await check("all 15 themes (tokens): the button, the narrowed button, the rows, the note and the box read", async () => {
  if (ALL.length !== 15) throw new Error(ALL.length + " themes");
  for (const t of ALL) {
    const v = t.vars;
    for (const [n, k, min] of [["button text on panel", contrast(v.text, v.panel), 4.5], ["narrowed text on chip-purple", contrast(v.purple, v["chip-purple"]), 4.5], ["row text on panel2", contrast(v.text, v.panel2), 4.5], ["count and note on panel2", contrast(v.muted, v.panel2), 4.5], ["note on nav", contrast(v.muted, v["nav-bg"]), 4.5], ["version badge on panel2", contrast(v.orange, v.panel2), 4.5], ["ticked box on chip-purple", contrast(v.purple, v["chip-purple"]), 3], ["box edge on panel2", contrast(v.muted, v.panel2), 3], ["need dot on panel", contrast(v.orange, v.panel), 3]])
      if (k < min) throw new Error(`${t.id}: ${n} ${k.toFixed(2)} < ${min}`);
  }
});
for (const t of ALL) {
  await check(`${t.id} @1280: the painted button, popover rows, note and ticked box read`, async () => {
    const { browser, page } = await launch({ width: 1280, height: 900, dark: t.mode === "dark" });
    page.setDefaultTimeout(8000);
    await page.addInitScript((id) => { try { localStorage.setItem("comebye.theme", id); } catch {} }, t.id);
    try {
      await page.goto(base + "/", { waitUntil: "load" });
      await page.waitForSelector("#agents .agent");
      const f = fleet(8);
      await page.evaluate(([f]) => { try { localStorage.setItem("herdr-host-filter", '["laharl","mac"]'); } catch {} state.hostFilter = null; state.hosts = f.hosts; setAgents(f); window.setAgents = () => {}; renderList(); }, [f]);
      const read = () => page.evaluate(() => {
        const cs = (e) => getComputedStyle(typeof e === "string" ? document.querySelector(e) : e);
        const row = document.querySelector('#hp-list .hp-row[aria-checked="true"]:not(.hp-all)');
        return { btnFg: cs("#hp-btn").color, btnBg: cs("#hp-btn").backgroundColor, btnEdge: cs("#hp-btn").borderTopColor, outFg: cs("#host-filter .hp-out").color, rowFg: cs(row).color, nFg: cs(row.querySelector(".n")).color, verFg: cs('#hp-list .hp-row[data-host="mac"] .ver').color };
      });
      await page.click("#hp-btn");
      const o = await read(), g = await page.evaluate(() => ({ nav: getComputedStyle(document.body).backgroundColor, pop: getComputedStyle(document.querySelector("#hp-pop")).backgroundColor }));
      for (const [n, a, b, min] of [["narrowed button text", o.btnFg, o.btnBg, 4.5], ["note", o.outFg, g.nav, 4.5], ["row text", o.rowFg, g.pop, 4.5], ["count", o.nFg, g.pop, 4.5], ["version badge", o.verFg, g.pop, 4.5]]) {
        const k = ratio(a, b); if (k < min) throw new Error(`${n} ${a} on ${b} = ${k.toFixed(2)} < ${min}`);
      }
      // The narrowed button is a purple tint with a purple edge: the edge is what stands out from the nav.
      const kb = ratio(o.btnEdge, g.nav); if (kb < 3) throw new Error(`the button edge ${o.btnEdge} on the nav ${g.nav} is ${kb.toFixed(2)} < 3`);
      if (process.env.HP_SHOTS) await page.screenshot({ path: `${out}/hostpicker-theme-${t.id}.png` });
    } finally { await browser.close(); }
  });
}

process.exit(results.every(Boolean) ? 0 : 1);
}
