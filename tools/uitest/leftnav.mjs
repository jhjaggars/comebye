// Left nav, slice 1: the eight full-width nav rows above the agent list are one row of icon buttons (issue #30).
// With a fleet like the research's (5 hosts, 36 agents) at 1280 the first agent row is above y=300 (it was ~452);
// each button opens its screen, carries its count as a badge, a tooltip, an aria-label and aria-current, and is at
// least 36px; Pages, Usage, Peers and Settings are gone from the fleet menu on a desktop (the row has them) and
// stay in it on a phone, where the row is hidden and every destination is still reached; light and dark; the icons
// and badges read on --nav-bg in all 15 themes.
//
//   node leftnav.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";
import fs from "node:fs"; import vm from "node:vm"; import path from "node:path"; import { fileURLToPath } from "node:url";

const [base, out] = process.argv.slice(2);
const results = [];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

// The research's fleet: 5 hosts (one down), 36 agents over 9 projects, 4 of them blocked.
const HOSTS = ["laharl", "mac", "jjaggars-kubevirt", "comebye-hub", "gpu-box"];
const PROJECTS = ["comebye", "webapp", "infra", "docs", "billing", "mobile", "ml-lab", "tools", "site"];
const STATUS = ["working", "working", "idle", "done", "working", "idle", "blocked", "working", "done"];
function fleet() {
  const now = new Date().toISOString();
  const agents = Array.from({ length: 36 }, (_, i) => {
    const host = HOSTS[i % 4], repo = PROJECTS[i % 9], st = i < 4 ? "blocked" : STATUS[(i * 5) % 9] === "blocked" ? "working" : STATUS[(i * 5) % 9];
    return { id: `${host}/w${i + 1}:p1`, host, pane_id: `w${i + 1}:p1`, name: `agent-${i + 1}`, agent: "claude", display_agent: "Claude", agent_status: st,
      cwd: `/home/u/code/${repo}`, worktree: { repo, checkout: `/home/u/code/${repo}`, linked: false }, last_active: now, status_since: now };
  });
  const hosts = HOSTS.map((name, i) => ({ name, local: i === 0, status: name === "gpu-box" ? "down" : "ok", version: "dev", agents: agents.filter((a) => a.host === name).length }));
  return { agents, hosts };
}
const SCREENS = [["status", "#p-status"], ["todo", "#p-todo"], ["search", "#recall"], ["pages", "#pages"], ["usage", "#usage"], ["peers", "#peers"], ["settings", "#settings"], ["removals", "#review"]];
const LABEL = { status: "Status", todo: "To do", search: "Search", pages: "Pages", usage: "Usage", peers: "Peers", settings: "Settings", removals: "Ready to remove" };

async function open(width, dark) {
  const { browser, page } = await launch({ width, height: width < 500 ? 844 : 900, dark });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(e.message));
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent");
  // The app's first loads (the ledger and the to-do list) must be over, or one lands after the state below and replaces it
  // (the single host of the test server hides the Peers button; an empty ledger hides Ready to remove).
  await page.waitForFunction(() => ledger.on === true && !ledger.inflight && ledger.loadedAt > 0 && !todos.inflight && todos.loadedAt > 0);
  // The fleet and the counts go in through the app's own state, then it draws. The app refreshes that state from the
  // server's events and every few seconds, and the next refresh (on a slow runner, one that lands mid-check) would put
  // the server's one-host fleet back: the loaders that set it are stubbed, as the other left-nav scripts stub setAgents.
  await page.evaluate((f) => {
    state.hosts = f.hosts; setAgents(f);
    for (const fn of ["setAgents", "setLedger", "loadLedger", "loadTodos"]) window[fn] = () => {};
    todos.on = true; todos.open = 3; ledger.on = true;
    ledger.marked = new Set(["a", "b", "c", "d", "e"]); renderList(); renderNav(); renderFleetMenu();
  }, fleet());
  await page.waitForTimeout(200);
  return { browser, page, errs };
}

for (const dark of [false, true]) {
  const scheme = dark ? "dark" : "light";
  await check(`${scheme} @1280: the first agent row is above y=300 with 36 agents on 5 hosts`, async () => {
    const { browser, page, errs } = await open(1280, dark);
    try {
      const g = await page.evaluate(() => {
        const r = (s) => { const e = document.querySelector(s); const b = e.getBoundingClientRect(); return { top: b.top, bottom: b.bottom, h: b.height, w: b.width }; };
        return { n: document.querySelectorAll("#agents .agent").length, nav: r("#nav"), first: r("#agents .agent"), navFlex: getComputedStyle(document.querySelector("#nav")).flexDirection };
      });
      if (g.n < 30) throw new Error(`only ${g.n} agent rows drawn`);
      if (g.first.top >= 300) throw new Error(`first agent row top ${g.first.top} (was ~452 with eight rows)`);
      if (g.nav.h > 70) throw new Error(`the nav is ${g.nav.h}px tall, want one row`);
      if (g.navFlex !== "row") throw new Error("nav is " + g.navFlex);
      await page.screenshot({ path: `${out}/leftnav-1280-${scheme}.png` });
      if (errs.length) throw new Error(errs.join("; "));
      console.log(`     first row top ${Math.round(g.first.top)}, nav ${Math.round(g.nav.h)}px tall`);
    } finally { await browser.close(); }
  });

  await check(`${scheme} @1280: eight buttons, >= 36px, tooltip, aria-label, badges (To do 3, Ready to remove 5)`, async () => {
    const { browser, page } = await open(1280, dark);
    try {
      const bs = await page.locator("#nav button.nv").evaluateAll((els) => els.map((e) => { const b = e.getBoundingClientRect(); return { key: e.dataset.page, w: b.width, h: b.height, title: e.title, aria: e.getAttribute("aria-label"), badge: e.querySelector(".badge")?.textContent || "", text: e.textContent.trim(), cur: e.getAttribute("aria-current") }; }));
      // Peers shows with more than one host: the fleet has five.
      const keys = bs.map((b) => b.key).join(",");
      if (keys !== SCREENS.map((s) => s[0]).join(",")) throw new Error("buttons: " + keys);
      for (const b of bs) {
        if (b.w < 36 || b.h < 36) throw new Error(`${b.key} is ${b.w}x${b.h}, want >= 36`);
        if (b.title !== LABEL[b.key]) throw new Error(`${b.key} tooltip ${JSON.stringify(b.title)}`);
        const want = b.key === "todo" ? "To do, 3 open" : b.key === "removals" ? "Ready to remove, 5 open" : LABEL[b.key];
        if (b.aria !== want) throw new Error(`${b.key} aria-label ${JSON.stringify(b.aria)}, want ${JSON.stringify(want)}`);
        if (b.key !== "todo" && b.key !== "removals" && b.badge) throw new Error(`${b.key} has a badge ${b.badge}`);
        if (b.text !== (b.badge || "")) throw new Error(`${b.key} shows text ${JSON.stringify(b.text)}: the button is an icon`);
      }
      const t = bs.find((b) => b.key === "todo").badge, r = bs.find((b) => b.key === "removals").badge;
      if (t !== "3" || r !== "5") throw new Error(`badges ${t} / ${r}`);
      // Opening Status marks it and only it (a cold start may already show a page or an agent).
      await page.click('#nav .nv[data-page="status"]');
      await page.waitForSelector('#nav .nv[data-page="status"][aria-current="page"]');
      if (await page.locator("#nav .nv[aria-current]").count() !== 1) throw new Error("more than one button marked");
      // The row fits the column: nothing scrolls sideways, every button inside it.
      const fit = await page.evaluate(() => { const n = document.querySelector("#nav"); return n.scrollWidth <= n.clientWidth && [...n.children].every((c) => c.getBoundingClientRect().right <= n.getBoundingClientRect().right + 0.5); });
      if (!fit) throw new Error("the row overflows the column");
    } finally { await browser.close(); }
  });

  await check(`${scheme} @1280: each button opens its screen and is marked aria-current`, async () => {
    const { browser, page } = await open(1280, dark);
    try {
      for (const [key, sel] of SCREENS) {
        await page.locator(`#nav .nv[data-page="${key}"]`).click();
        await page.waitForSelector(`${sel}:not([hidden])`, { state: "visible" });
        await page.waitForFunction((k) => document.querySelector(`#nav .nv[data-page="${k}"]`)?.getAttribute("aria-current") === "page", key);
        const marked = await page.locator("#nav .nv[aria-current]").count();
        if (marked !== 1) throw new Error(`${key}: ${marked} buttons marked`);
        // The mark means the screen is showing, not just a state flag.
        if (!(await page.locator(`#nav .nv[data-page="${key}"]`).evaluate((e) => e.classList.contains("on")))) throw new Error(`${key}: no .on`);
      }
      await page.screenshot({ path: `${out}/leftnav-removals-${scheme}.png` });
    } finally { await browser.close(); }
  });

  await check(`${scheme} @1280: the fleet menu has no Pages, Usage, Peers, Settings or Status; Refresh stays`, async () => {
    const { browser, page } = await open(1280, dark);
    try {
      await page.click("#btn-fleet");
      await page.waitForSelector("#f-menu:not([hidden])");
      for (const id of ["f-pages", "f-usage", "f-peers", "f-settings", "f-status"]) if (await page.locator("#" + id).isVisible()) throw new Error(id + " is in the menu");
      for (const id of ["f-refresh", "f-theme"]) if (!(await page.locator("#" + id).isVisible())) throw new Error(id + " is missing");
    } finally { await browser.close(); }
  });

  await check(`${scheme} @390: no icon row; Pages, Usage, Peers, Settings stay in the fleet menu; Search stays`, async () => {
    const { browser, page } = await open(390, dark);
    try {
      if (await page.locator("#nav").isVisible()) throw new Error("the icon row shows on a phone");
      if (!(await page.locator("#btn-recall").isVisible())) throw new Error("no Search button on the phone list");
      await page.click("#btn-fleet");
      await page.waitForSelector("#f-menu:not([hidden])");
      for (const id of ["f-pages", "f-usage", "f-peers", "f-settings"]) {
        if (!(await page.locator("#" + id).isVisible())) throw new Error(id + " is not in the phone menu");
        if (await page.locator("#" + id).isDisabled()) throw new Error(id + " is disabled");
      }
      await page.click("#f-usage");
      await page.waitForFunction(() => location.hash === "#/usage");
      await page.screenshot({ path: `${out}/leftnav-390-${scheme}.png` });
    } finally { await browser.close(); }
  });
}

// Colours: the icons (--muted, --text when on) and the badge read on the nav, in all 15 themes (themes.js is the one source).
const tctx = { globalThis: null }; tctx.globalThis = tctx; vm.createContext(tctx);
vm.runInContext(fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "..", "web", "themes.js"), "utf8"), tctx);
const { THEMES: ALL, contrast } = tctx.ComebyeThemes;
await check("all 15 themes: icon (--muted on --nav-bg and on the hover/on --panel2) >= 3, on icon (--text) >= 4.5, badge text on orange >= 4.5", async () => {
  if (ALL.length !== 15) throw new Error(ALL.length + " themes");
  for (const t of ALL) {
    const v = t.vars;
    const c = [["muted icon on nav", contrast(v.muted, v["nav-bg"]), 3], ["muted icon on panel2", contrast(v.muted, v.panel2), 3], ["on icon on panel2", contrast(v.text, v.panel2), 4.5], ["badge", contrast(v["on-orange"], v.orange), 4.5], ["badge edge vs nav", contrast(v.orange, v["nav-bg"]), 3]];
    for (const [n, k, min] of c) if (k < min) throw new Error(`${t.id}: ${n} ${k.toFixed(2)} < ${min}`);
  }
});

// And in the browser, on the real recessed column, light and dark: the computed icon stroke against the painted nav.
for (const dark of [false, true]) {
  await check(`${dark ? "dark" : "light"} @1280: the icon stroke and the badge read on the painted nav`, async () => {
    const { browser, page } = await open(1280, dark);
    try {
      const m = await page.evaluate(() => {
        const s = (e) => getComputedStyle(e);
        return { nav: s(document.body).backgroundColor, icon: s(document.querySelector("#nav .nv svg")).stroke, badge: s(document.querySelector("#nav .badge")).color, badgeBg: s(document.querySelector("#nav .badge")).backgroundColor };
      });
      const rgb = (x) => x.match(/[\d.]+/g).slice(0, 3).map(Number);
      const lum = (c) => { const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]); };
      const ratio = (a, b) => { const x = lum(rgb(a)), y = lum(rgb(b)); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
      if (ratio(m.icon, m.nav) < 3) throw new Error(`icon ${m.icon} on ${m.nav} = ${ratio(m.icon, m.nav).toFixed(2)}`);
      if (ratio(m.badge, m.badgeBg) < 4.5) throw new Error(`badge ${m.badge} on ${m.badgeBg} = ${ratio(m.badge, m.badgeBg).toFixed(2)}`);
    } finally { await browser.close(); }
  });
}

// Slice 6: the list column follows the window: 360 at 1280 and 1920, 480 at 3440, a phone unchanged; nothing in it overflows.
const WANT_W = { 1280: 360, 1920: 360, 3440: 480 };
for (const dark of [false, true]) for (const width of [390, 1280, 1920, 3440]) {
  await check(`${dark ? "dark" : "light"} @${width}: the list column is ${WANT_W[width] ? WANT_W[width] + "px" : "the phone layout"} and nothing in it overflows`, async () => {
    const { browser, page, errs } = await open(width, dark);
    try {
      const g = await page.evaluate(() => {
        const col = document.querySelector("#agents").getBoundingClientRect();
        const lw = parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--list-w")) || 0;
        const bad = [];
        const sels = ["#list header", "#nav", "#host-filter", "#list-filter", "#agents"];
        for (const s of sels) {
          const e = document.querySelector(s); if (!e || e.hidden || !e.getClientRects().length) continue;
          if (e.scrollWidth > e.clientWidth + 1) bad.push(`${s} scrolls ${e.scrollWidth}>${e.clientWidth}`);
          for (const c of e.querySelectorAll("*")) {
            const b = c.getBoundingClientRect();
            if (b.width && b.right > e.getBoundingClientRect().right + 1) bad.push(`${s} > ${c.tagName}.${c.className} right ${Math.round(b.right)} past ${Math.round(e.getBoundingClientRect().right)}`);
          }
        }
        const d = document.querySelector("#detail-empty"), de = d && d.getClientRects().length ? d.getBoundingClientRect() : null;
        return { colW: Math.round(col.width), colLeft: Math.round(col.left), grid: getComputedStyle(document.body).display, cols: getComputedStyle(document.body).gridTemplateColumns, detailLeft: de && Math.round(de.left), docOver: document.documentElement.scrollWidth > innerWidth, bad: bad.slice(0, 5), lw, ww: innerWidth };
      });
      if (g.docOver) throw new Error("the page scrolls sideways");
      if (g.bad.length) throw new Error(g.bad.join("; "));
      if (width === 390) { if (g.grid === "grid") throw new Error("the phone got the two-column grid"); }
      else {
        const first = parseFloat(g.cols);
        if (Math.round(first) !== WANT_W[width]) throw new Error(`grid column ${g.cols}, want ${WANT_W[width]}px`);
        if (g.detailLeft != null && Math.abs(g.detailLeft - WANT_W[width]) > 1) throw new Error(`the detail pane starts at ${g.detailLeft}, want ${WANT_W[width]}`);
      }
      await page.screenshot({ path: `${out}/leftnav-wide-${width}-${dark ? "dark" : "light"}.png` });
      if (errs.length) throw new Error(errs.join("; "));
    } finally { await browser.close(); }
  });
}

process.exit(results.every(Boolean) ? 0 : 1);
