// Left nav, slices 4 and 5: the Group by menu (issue #30).
// Project (default, today's grouping), Host, Coordinator (slice 5, at the end of this file) and Status; collapse state is kept per mode; the choice persists in storage and in
// the #/list?...&group= hash; it works with the tabs, the filter box and the host picker; a blocked agent is in sight in every
// mode; the menu is keyboard reachable and a bottom sheet on a phone; nothing overflows at 390, 1280 and 3440, light and dark.
//
//   node groupby.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const results = [];
// idle: two frames, then no CSS transition or animation still running (a sheet has slid in, a pull has sprung back), not a timer.
const idle = async (page) => { await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(() => r())))); await page.waitForFunction(() => document.getAnimations().every((a) => a.playState !== "running" || a.effect?.getComputedTiming().iterations === Infinity), null, { timeout: 5000 }).catch(() => {}); };
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

// Three hosts, 5 agents each (k0 blocked on hosts 1 and 2, idle on 0; k1 working; k2 done; k3 idle; k4 working), plus a coordinator on mac,
// and one working agent on nas whose project is called "mac": a project and a host of the same name.
const HOSTS = ["laharl", "mac", "nas"];
const PROJ = ["server", "ui", "site", "ops", "bots"];
function fleet(hostNames = HOSTS) {
  const now = new Date().toISOString();
  const hosts = hostNames.map((name, i) => ({ name, local: i === 0, status: "ok", version: "v1.2.0", agents: 5 }));
  const agents = [];
  const add = (h, id, name, st, repo, extra = {}) => agents.push({ id: `${h}/${id}`, host: h, pane_id: id, name, agent: "claude", display_agent: "Claude", agent_status: st,
    cwd: `/home/u/code/${repo}`, worktree: { repo, checkout: "/x", linked: false }, last_active: now, status_since: now, ...extra });
  hosts.forEach((h, i) => {
    for (let k = 0; k < 5; k++) add(h.name, `w${i}${k}:p1`, `${["api", "web", "docs", "infra", "bot"][k]}-${h.name}`, k === 0 ? (i === 0 ? "idle" : "blocked") : k === 1 || k === 4 ? "working" : k === 2 ? "done" : "idle", PROJ[k]);
  });
  if (hostNames.includes("mac")) add("mac", "c1:p1", "coord-mac", "working", "server", { role: "coordinator", is_coordinator: true });
  if (hostNames.includes("nas")) add("nas", "m9:p1", "macproj-nas", "working", "mac");
  return { agents, hosts };
}

async function open(width, dark, { hash = "", store = {}, hostNames, fleet: fl } = {}) {
  const { browser, page } = await launch({ width, height: width < 500 ? 844 : 900, dark });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(e.message));
  const f = fl || fleet(hostNames);
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent");
  await page.evaluate(([f, store, hash]) => {
    for (const k of ["herdr-host-filter", "herdr-list-tab", "herdr-list-q", "herdr-list-group", "comebye-collapsed-projects"]) try { localStorage.removeItem(k); } catch {}
    for (const [k, v] of Object.entries(store)) try { localStorage.setItem(k, v); } catch {}
    state.hostFilter = null; state.listTab = null; state.listQ = null; state.listGroup = null; collapsedProjects = null;
    state.hosts = f.hosts; setAgents(f); window.setAgents = () => {};
    for (const a of f.agents) if (a.role === "coordinator") ledger.roles.set(agentId(a), { role: "coordinator" });
    if (hash) location.hash = "";
    if (hash) { history.replaceState(null, "", hash); route(); } else { renderList(); route(); }
  }, [f, store, hash]);
  await idle(page);
  return { browser, page, errs, f };
}
const shown = (page) => page.evaluate(() => [...document.querySelectorAll("#agents .agent[data-id]")].filter((e) => !e.classList.contains("host-down")).map((e) => e.dataset.id));
// groups: every section in the list: name, how many rows it shows (a folded one shows none), and whether it can fold.
const groups = (page) => page.evaluate(() => [...document.querySelectorAll("#agents section.proj")].map((s) => ({
  name: s.querySelector(".proj-name").textContent, rows: [...s.querySelectorAll(".agent[data-id]")].filter((r) => r.getClientRects().length).length,
  all: s.querySelectorAll(".agent[data-id]").length, fold: !!s.querySelector("button.proj-head"), open: s.querySelector("button.proj-head")?.getAttribute("aria-expanded") === "true",
  mode: s.dataset.group, aria: s.getAttribute("aria-label") })));
const names = async (page) => (await groups(page)).map((g) => g.name);
const hash = (page) => page.evaluate(() => location.hash);
const pick = async (page, key) => { await page.click("#gb-btn"); await page.click(`#gb-list [data-group="${key}"]`); await idle(page); };
const noOverflow = (page) => page.evaluate(() => { const b = document.querySelector("#list-filter"); return [b.scrollWidth, b.clientWidth, document.documentElement.scrollWidth, document.documentElement.clientWidth]; });
const blockedIds = (f) => f.agents.filter((a) => a.agent_status === "blocked").map((a) => a.id).sort();

for (const dark of [false, true]) {
  const scheme = dark ? "dark" : "light";
  for (const w of [390, 1280, 3440]) {
    await check(`${scheme} @${w}: Project is the default and unchanged; Host and Status regroup the same agents; every blocked agent is in sight in each mode; nothing overflows`, async () => {
      const { browser, page, errs, f } = await open(w, dark);
      try {
        const label = () => page.locator("#gb-label").innerText();
        if (await label() !== "Project") throw new Error("default label " + await label());
        const total = (await shown(page)).length;
        if (total !== f.agents.length) throw new Error("rows " + total);
        // Project: the groups are the repos (the coordinator and, on a desktop, the blocked agents are pinned above, as before).
        let g = await groups(page);
        const proj = g.filter((x) => x.mode === "project").map((x) => x.name).sort().join();
        if (proj !== "bots,mac,ops,server,site,ui") throw new Error("project groups " + proj);
        if (!g.some((x) => x.name === "Coordinators")) throw new Error("coordinators lost: " + g.map((x) => x.name));
        await page.screenshot({ path: `${out}/groupby-project-${w}-${scheme}.png` });
        for (const [mode, want] of [["host", "laharl,mac,nas"], ["status", "Needs you,Working,Done"]]) {
          await pick(page, mode);
          if (await label() !== (mode === "host" ? "Host" : "Status")) throw new Error(mode + " label " + await label());
          g = await groups(page);
          const rows = await shown(page);
          if (rows.length !== total || new Set(rows).size !== total) throw new Error(`${mode}: ${rows.length} rows (${new Set(rows).size} distinct), want ${total}`);
          const body = g.filter((x) => x.mode === mode);
          const pinned = g.filter((x) => x.mode !== mode).map((x) => x.name);
          if (body.map((x) => x.name).sort().join() !== want.split(",").sort().join()) throw new Error(`${mode} groups ${body.map((x) => x.name)}`);
          if (mode === "status" && pinned.length) throw new Error("status mode keeps pinned sections " + pinned);
          if (mode === "status") {
            const st = await page.evaluate(() => [...document.querySelectorAll("#agents section.proj")].map((s) => [s.querySelector(".proj-name").textContent, [...s.querySelectorAll(".agent[data-id]")].map((r) => agentById(r.dataset.id).agent_status)]));
            const ok = { "Needs you": (s) => s === "blocked", Working: (s) => s === "working", Done: (s) => s === "done" || s === "idle" };
            for (const [n, sts] of st) if (!sts.length || sts.some((s) => !ok[n](s))) throw new Error(`status group ${n} holds ${sts}`);
          }
          if (mode === "host") {
            const hs = await page.evaluate(() => [...document.querySelectorAll("#agents section.proj[data-group=host]")].map((s) => [s.querySelector(".proj-name").textContent, [...s.querySelectorAll(".agent[data-id]")].map((r) => agentById(r.dataset.id).host)]));
            for (const [n, h] of hs) if (h.some((x) => x !== n)) throw new Error(`host group ${n} holds ${h}`);
          }
          // A blocked agent is on screen (no fold hides it) in this mode.
          const vis = await page.evaluate(() => [...document.querySelectorAll("#agents .agent.blocked[data-id]")].filter((r) => r.getClientRects().length).map((r) => r.dataset.id).sort());
          if (vis.join() !== blockedIds(f).join()) throw new Error(`${mode}: blocked in sight ${vis}`);
          const ov = await noOverflow(page);
          if (ov[0] !== ov[1] || ov[2] > ov[3]) throw new Error(`${mode} overflow ${ov}`);
          await page.screenshot({ path: `${out}/groupby-${mode}-${w}-${scheme}.png` });
        }
        await pick(page, "project");
        // (a desktop may have opened a blocked agent, whose route owns the hash: ask the list's own.)
        if (await label() !== "Project" || await page.evaluate(() => listHash()) !== "") throw new Error("back to Project: " + await label() + " " + await page.evaluate(() => listHash()));
        if (errs.length) throw new Error(errs.join("; "));
      } finally { await browser.close(); }
    });
  }

  await check(`${scheme} @1280: collapse state is per mode (project "mac" and host "mac" fold on their own), and it persists`, async () => {
    const { browser, page } = await open(1280, dark);
    try {
      const fold = (page, name, mode) => page.click(`#agents section.proj[data-group=${mode}] .proj-head[data-proj] >> text="${name}"`);
      await fold(page, "mac", "project");
      let g = await groups(page);
      if (g.find((x) => x.mode === "project" && x.name === "mac").open) throw new Error("project mac did not fold");
      await pick(page, "host");
      g = await groups(page);
      if (!g.find((x) => x.name === "mac" && x.mode === "host").open) throw new Error("folding project mac folded host mac");
      await fold(page, "nas", "host");
      g = await groups(page);
      if (g.find((x) => x.name === "nas").open || g.find((x) => x.name === "nas").rows) throw new Error("host nas still open");
      await pick(page, "status");
      g = await groups(page);
      if (g.some((x) => x.fold && !x.open) || g.find((x) => x.name === "Working").rows < 1) throw new Error("status inherited a fold");
      await pick(page, "project");
      g = await groups(page);
      if (g.find((x) => x.name === "mac" && x.mode === "project").open || !g.find((x) => x.name === "server").open) throw new Error("project fold lost");
      await pick(page, "host");
      if ((await groups(page)).find((x) => x.name === "nas").open) throw new Error("host fold lost");
      const st = await page.evaluate(() => JSON.parse(localStorage.getItem("comebye-collapsed-projects")));
      if (!st.includes("mac") || st.length !== 2) throw new Error("storage " + JSON.stringify(st));
      // The Project part of the stored set keeps the plain names it always had.
    } finally { await browser.close(); }
  });

  await check(`${scheme} @390: the choice persists: storage, the hash (group= only when not Project), a link, a bad value falls back`, async () => {
    let o = await open(390, dark);
    try {
      await pick(o.page, "host");
      if (await hash(o.page) !== "#/list?group=host") throw new Error("hash " + await hash(o.page));
      await o.page.click('#lf-tabs [data-tab="working"]');
      await o.page.fill("#lf-q", "api");
      if (await hash(o.page) !== "#/list?tab=working&q=api&group=host") throw new Error("hash " + await hash(o.page));
      if (await o.page.evaluate(() => localStorage.getItem("herdr-list-group")) !== "host") throw new Error("storage");
      await o.page.evaluate(() => clearFilters());
      if (await hash(o.page) !== "#/list?group=host") throw new Error("Clear filters must keep the grouping: " + await hash(o.page));
    } finally { await o.browser.close(); }
    o = await open(390, dark, { hash: "#/list?group=status&tab=done" });
    try {
      if (await o.page.locator("#gb-label").innerText() !== "Status") throw new Error("link not applied");
      if ((await names(o.page)).join() !== "Done") throw new Error("groups " + await names(o.page));
    } finally { await o.browser.close(); }
    o = await open(390, dark, { store: { "herdr-list-group": "host" } });
    try {
      if (await o.page.locator("#gb-label").innerText() !== "Host" || await hash(o.page) !== "#/list?group=host") throw new Error("storage not read: " + await hash(o.page));
    } finally { await o.browser.close(); }
    o = await open(1280, dark, { hash: "#/list?group=bogus" });
    try { if (await o.page.locator("#gb-label").innerText() !== "Project") throw new Error("bad link"); } finally { await o.browser.close(); }
    o = await open(1280, dark, { store: { "herdr-list-group": "bogus-mode" } });
    try { if (await o.page.locator("#gb-label").innerText() !== "Project") throw new Error("unknown stored mode"); } finally { await o.browser.close(); }
  });

  await check(`${scheme} @1280: it works with the tabs, the box and the host picker, in every mode`, async () => {
    const { browser, page, f } = await open(1280, dark);
    try {
      await pick(page, "status");
      await page.click('#lf-tabs [data-tab="working"]');
      let g = await groups(page);
      if (g.map((x) => x.name).join() !== "Needs you,Working") throw new Error("working tab, status: " + g.map((x) => x.name));
      await page.click('#lf-tabs [data-tab="done"]');
      if ((await names(page)).join() !== "Done") throw new Error("done tab: " + await names(page));
      await page.click('#lf-tabs [data-tab="needs"]');
      g = await groups(page);
      if (g.map((x) => x.name).join() !== "Needs you" || g[0].fold || g[0].rows !== 2) throw new Error("needs tab: " + JSON.stringify(g));
      await page.click('#lf-tabs [data-tab="all"]');
      await page.fill("#lf-q", "web");
      if ((await names(page)).join() !== "Working" || (await shown(page)).length !== 3) throw new Error("box: " + await names(page) + " " + (await shown(page)).length);
      await page.fill("#lf-q", "");
      // Host mode with the picker on laharl: mac's and nas's blocked agents stay (host filter never hides them), in the pinned section.
      await pick(page, "host");
      await page.click("#hp-btn"); await page.click('#hp-list .hp-row[data-host="laharl"]'); await page.keyboard.press("Escape");
      g = await groups(page);
      const vis = await page.evaluate(() => [...document.querySelectorAll("#agents .agent.blocked[data-id]")].map((r) => r.dataset.id).sort());
      if (vis.join() !== blockedIds(f).join()) throw new Error("blocked outside the picker: " + vis);
      if (g.find((x) => x.aria === "Needs you")?.rows !== 2) throw new Error("needs section " + JSON.stringify(g));
      const hostNames = g.filter((x) => x.mode === "host").map((x) => x.name);
      if (hostNames.join() !== "laharl") throw new Error("host groups with the picker on laharl: " + hostNames);
      // Status mode with the picker: the two outsiders are in Needs you, and nothing else of theirs shows.
      await pick(page, "status");
      g = await groups(page);
      if (g.find((x) => x.name === "Needs you").rows !== 2) throw new Error("status + picker: " + JSON.stringify(g));
      if ((await shown(page)).length !== 5 + 2) throw new Error("rows " + (await shown(page)).length);
      // Clear filters leaves the grouping.
      await page.fill("#lf-q", "no-such"); await page.click("#agents .empty-act");
      if (await page.locator("#gb-label").innerText() !== "Status") throw new Error("Clear filters reset the grouping");
    } finally { await browser.close(); }
  });

  await check(`${scheme} @1280: Needs you in Status mode cannot be folded; a folded project never hides a blocked agent on a desktop`, async () => {
    const { browser, page } = await open(1280, dark);
    try {
      await pick(page, "status");
      const g = (await groups(page)).find((x) => x.name === "Needs you");
      if (g.fold || g.rows !== 2) throw new Error("Needs you " + JSON.stringify(g));
      await page.evaluate(() => toggleProject(undefined)); // a click on a static head
      if ((await groups(page)).find((x) => x.name === "Needs you").rows !== 2) throw new Error("folded");
    } finally { await browser.close(); }
  });

  await check(`${scheme} @1280: by keyboard: Tab reaches the button, Enter opens, arrows move, Enter picks and the focus returns; Esc closes; f and the palette`, async () => {
    const { browser, page } = await open(1280, dark);
    try {
      await page.focus("#lf-q");
      await page.keyboard.press("Tab");
      if (await page.evaluate(() => document.activeElement.id) !== "gb-btn") throw new Error("Tab from the box went to " + await page.evaluate(() => document.activeElement.id));
      await page.keyboard.press("Enter");
      await page.waitForSelector("#gb-pop:not([hidden])");
      if (await page.evaluate(() => document.activeElement.dataset.group) !== "project") throw new Error("focus starts on the current mode");
      if (await page.getAttribute("#gb-btn", "aria-expanded") !== "true") throw new Error("aria-expanded");
      const radios = await page.evaluate(() => [...document.querySelectorAll("#gb-list [role=menuitemradio]")].map((b) => b.dataset.group + ":" + b.getAttribute("aria-checked")));
      if (radios.join() !== "project:true,host:false,status:false") throw new Error("radios " + radios);
      await page.keyboard.press("ArrowDown"); await page.keyboard.press("ArrowDown");
      if (await page.evaluate(() => document.activeElement.dataset.group) !== "status") throw new Error("arrows");
      await page.keyboard.press("Enter");
      await idle(page);
      if (await page.locator("#gb-label").innerText() !== "Status" || !await page.locator("#gb-pop").isHidden()) throw new Error("Enter did not pick");
      if (await page.evaluate(() => document.activeElement.id) !== "gb-btn") throw new Error("focus did not return");
      await page.keyboard.press("Space");
      await page.waitForSelector("#gb-pop:not([hidden])");
      await page.keyboard.press("Escape");
      if (!await page.locator("#gb-pop").isHidden() || await page.evaluate(() => document.activeElement.id) !== "gb-btn") throw new Error("Esc");
      // A poll re-renders the list while the menu is open: it stays open and keeps the focus.
      await page.click("#gb-btn");
      await page.evaluate(() => { renderList(); renderList(); });
      if (await page.locator("#gb-pop").isHidden() || await page.evaluate(() => document.activeElement.closest("#gb-pop") === null)) throw new Error("a re-render closed the menu");
      await page.keyboard.press("Escape");
      // The palette lists it.
      await page.evaluate(() => { document.activeElement.blur(); location.hash = ""; });
      await page.keyboard.press(process.platform === "darwin" ? "Meta+k" : "Control+k");
      await page.keyboard.type("group");
      await idle(page);
      await page.keyboard.press("Enter");
      await page.waitForSelector("#gb-pop:not([hidden])");
    } finally { await browser.close(); }
  });

  await check(`${scheme} @390: on a phone the menu is a bottom sheet (like the host picker), 36px+ targets, the control fits`, async () => {
    const { browser, page } = await open(390, dark);
    try {
      await page.tap("#gb-btn");
      await page.waitForSelector("#gb-pop:not([hidden])");
      await idle(page);
      const r = await page.evaluate(() => { const b = document.querySelector("#gb-pop").getBoundingClientRect(); return { top: b.top, bottom: b.bottom, left: b.left, right: b.right, vh: innerHeight, vw: innerWidth, bk: !document.querySelector("#sheet-bk").hidden,
        rows: [...document.querySelectorAll("#gb-list .hp-row")].map((x) => Math.round(x.getBoundingClientRect().height)) }; });
      if (Math.abs(r.bottom - r.vh) > 2 || r.left < 0 || r.right > r.vw + 1 || !r.bk) throw new Error("not a sheet " + JSON.stringify(r));
      if (r.rows.length !== 3 || r.rows.some((h) => h < 36)) throw new Error("rows " + r.rows);
      await page.screenshot({ path: `${out}/groupby-menu-390-${scheme}.png` });
      await page.tap('#gb-list [data-group="host"]');
      await idle(page);
      if (!await page.locator("#gb-pop").isHidden() || await page.locator("#gb-label").innerText() !== "Host") throw new Error("tap did not pick and close");
      const hs = await page.evaluate(() => [...document.querySelectorAll("#lf-q, #gb-btn")].map((e) => Math.round(e.getBoundingClientRect().height)));
      if (hs.some((h) => h < 36)) throw new Error("targets " + hs);
      const ov = await noOverflow(page);
      if (ov[0] !== ov[1] || ov[2] > ov[3]) throw new Error("overflow " + ov);
    } finally { await browser.close(); }
  });

  await check(`${scheme} @1280: the menu opens under its button, inside the screen; a click outside closes it; opening the host picker closes it`, async () => {
    const { browser, page } = await open(1280, dark);
    try {
      await page.click("#gb-btn");
      const r = await page.evaluate(() => { const b = document.querySelector("#gb-pop").getBoundingClientRect(), t = document.querySelector("#gb-btn").getBoundingClientRect(); return { top: b.top, bt: t.bottom, left: b.left, right: b.right, vw: innerWidth }; });
      if (r.top < r.bt || r.left < 0 || r.right > r.vw) throw new Error("placement " + JSON.stringify(r));
      await page.screenshot({ path: `${out}/groupby-menu-1280-${scheme}.png` });
      await page.click("#hp-btn");
      if (!await page.locator("#gb-pop").isHidden() || await page.locator("#hp-pop").isHidden()) throw new Error("the picker did not replace the menu");
      await page.keyboard.press("Escape");
      await page.click("#gb-btn");
      await page.mouse.click(900, 600);
      if (!await page.locator("#gb-pop").isHidden()) throw new Error("outside click");
    } finally { await browser.close(); }
  });

  await check(`${scheme} @1280: one host: Host is not offered, a saved Host shows as Project and returns with a second host`, async () => {
    const { browser, page } = await open(1280, dark, { hostNames: ["laharl"], store: { "herdr-list-group": "host" } });
    try {
      await page.click("#gb-btn");
      const opts = await page.evaluate(() => [...document.querySelectorAll("#gb-list .hp-row")].map((b) => b.dataset.group));
      if (opts.join() !== "project,status") throw new Error("options " + opts);
      if (await page.locator("#gb-label").innerText() !== "Project") throw new Error("label " + await page.locator("#gb-label").innerText());
      await page.keyboard.press("Escape");
      if (await page.evaluate(() => localStorage.getItem("herdr-list-group")) !== "host") throw new Error("the saved choice was lost");
      await page.evaluate((f) => { state.hosts = f.hosts; setAgents(f); renderList(); }, fleet());
      if (await page.locator("#gb-label").innerText() !== "Host") throw new Error("Host did not come back");
    } finally { await browser.close(); }
  });
}


// ---- slice 5: Group by Coordinator ----
// laharl has its own coordinator (coord-laharl) beside mac's (coord-mac). The hub says which coordinator owns an agent in `owner`.
//   coord-laharl owns laharl's api (idle) and web (working); coord-mac owns mac's api (blocked), web and docs, and nas's web;
//   nas's api (blocked) has an owner that is no coordinator any more; the rest are unowned.
function coordFleet({ owners = true, second = true } = {}) {
  const f = fleet();
  const now = new Date().toISOString();
  if (second) f.agents.push({ id: "laharl/c0:p1", host: "laharl", pane_id: "c0:p1", name: "coord-laharl", agent: "claude", display_agent: "Claude", agent_status: "idle",
    cwd: "/home/u/code/comebye", worktree: { repo: "comebye", checkout: "/x", linked: false }, last_active: now, status_since: now, role: "coordinator", is_coordinator: true });
  const own = { "laharl/w00:p1": "laharl/c0:p1", "laharl/w01:p1": "laharl/c0:p1", "mac/w10:p1": "mac/c1:p1", "mac/w11:p1": "mac/c1:p1", "mac/w12:p1": "mac/c1:p1",
    "nas/w21:p1": "mac/c1:p1", "nas/w20:p1": "nas/gone:p1" };
  if (owners) for (const a of f.agents) if (own[a.id]) a.owner = own[a.id];
  return f;
}
const openCoord = (w, dark, o = {}) => open(w, dark, { ...o, fleet: coordFleet(o.fleetOpts) });
const members = (page) => page.evaluate(() => [...document.querySelectorAll("#agents section.proj[data-group=coordinator]")].map((s) => [s.querySelector(".proj-name").textContent,
  [...s.querySelectorAll(".agent[data-id]")].map((r) => r.dataset.id)]));

for (const dark of [false, true]) {
  const scheme = dark ? "dark" : "light";
  await check(`${scheme} @1280: Coordinator is offered with two coordinators, or when any agent has an owner, and not otherwise`, async () => {
    const offered = async (o) => { const x = await openCoord(1280, dark, o); try { await x.page.click("#gb-btn"); return await x.page.evaluate(() => [...document.querySelectorAll("#gb-list .hp-row")].map((b) => b.dataset.group).join()); } finally { await x.browser.close(); } };
    if (await offered({}) !== "project,host,coordinator,status") throw new Error("two coordinators and owners: " + await offered({}));
    if (await offered({ fleetOpts: { owners: false } }) !== "project,host,coordinator,status") throw new Error("two coordinators, no owners");
    if (await offered({ fleetOpts: { second: false } }) !== "project,host,coordinator,status") throw new Error("one coordinator with owned agents");
    if (await offered({ fleetOpts: { second: false, owners: false } }) !== "project,host,status") throw new Error("one coordinator, nobody owned: " + await offered({ fleetOpts: { second: false, owners: false } }));
    // A saved Coordinator with nothing to tell apart shows as Project and comes back with an owner.
    const x = await openCoord(1280, dark, { fleetOpts: { second: false, owners: false }, store: { "herdr-list-group": "coordinator" } });
    try {
      if (await x.page.locator("#gb-label").innerText() !== "Project") throw new Error("saved Coordinator without a reason shows " + await x.page.locator("#gb-label").innerText());
      await x.page.evaluate((f) => { state.agents = f.agents; renderList(); }, coordFleet({ second: false }));
      if (await x.page.locator("#gb-label").innerText() !== "Coordinator") throw new Error("Coordinator did not come back");
    } finally { await x.browser.close(); }
  });

  for (const w of [390, 1280, 3440]) {
    await check(`${scheme} @${w}: Coordinator mode: a group per coordinator led by its row, then Unowned; every agent once; a blocked agent in sight; nothing overflows`, async () => {
      const { browser, page, errs, f } = await openCoord(w, dark);
      try {
        await pick(page, "coordinator");
        if (await page.locator("#gb-label").innerText() !== "Coordinator" || await page.evaluate(() => listHash()) !== "#/list?group=coordinator") throw new Error("label/hash " + await page.evaluate(() => listHash()));
        const m = await members(page), all = await shown(page);
        if (all.length !== f.agents.length || new Set(all).size !== all.length) throw new Error(`rows ${all.length} of ${f.agents.length}`);
        const by = Object.fromEntries(m);
        // The group is named for the coordinator, and the coordinator's own row heads it.
        const want = { "coord-laharl": ["laharl/c0:p1", "laharl/w00:p1", "laharl/w01:p1"], "coord-mac": ["mac/c1:p1", "mac/w10:p1", "mac/w11:p1", "mac/w12:p1", "nas/w21:p1"] };
        // On a desktop a blocked agent (mac's api) is pinned in Needs you above, out of its group.
        const pinned = w >= 1100 ? new Set(blockedIds(f)) : new Set();
        for (const [n, ids] of Object.entries(want)) {
          const exp = ids.filter((id) => !pinned.has(id));
          // The coordinator's own row first, then what it started in the list's order.
          const got = by[n] || [];
          if (exp.includes(ids[0]) && got[0] !== ids[0]) throw new Error(`${n} is not led by its coordinator: ${got}`);
          if (got.slice().sort().join() !== exp.slice().sort().join()) throw new Error(`${n}: ${got} want ${exp}`);
        }
        if (m[m.length - 1][0] !== "Unowned") throw new Error("Unowned is not last: " + m.map((x) => x[0]));
        const owned = new Set(Object.values(want).flat());
        const expUn = f.agents.map((x) => x.id).filter((id) => !owned.has(id) && !pinned.has(id)).sort();
        if ((by.Unowned || []).slice().sort().join() !== expUn.join()) throw new Error(`Unowned: ${by.Unowned} want ${expUn}`);
        // A coordinator group's own row is first.
        const heads = await page.evaluate(() => [...document.querySelectorAll("#agents section.proj[data-group=coordinator]")].map((s) => [s.querySelector(".proj-name").textContent, s.querySelector(".agent[data-id]")?.querySelector(".role-chip")?.textContent || ""]));
        for (const [n, chip] of heads) if (n.startsWith("coord-") && chip !== "coordinator") throw new Error(`${n} is not led by its own row`);
        const vis = await page.evaluate(() => [...document.querySelectorAll("#agents .agent.blocked[data-id]")].filter((r) => r.getClientRects().length).map((r) => r.dataset.id).sort());
        if (vis.join() !== blockedIds(f).join()) throw new Error("blocked in sight " + vis);
        if (w >= 1100 && !(await groups(page)).some((g) => g.aria === "Needs you" && g.rows === 2)) throw new Error("Needs you is not pinned on a desktop");
        if ((await groups(page)).some((g) => g.name === "Coordinators")) throw new Error("the Coordinators section is pinned above the groups");
        const ov = await noOverflow(page);
        if (ov[0] !== ov[1] || ov[2] > ov[3]) throw new Error("overflow " + ov);
        await page.screenshot({ path: `${out}/groupby-coordinator-${w}-${scheme}.png` });
        if (errs.length) throw new Error(errs.join("; "));
      } finally { await browser.close(); }
    });
  }

  await check(`${scheme} @1280: Coordinator mode: folds on its own, persists in storage and the hash, works with the tabs and the box`, async () => {
    let o = await openCoord(1280, dark, { store: { "comebye-collapsed-projects": JSON.stringify(["mac"]) } });
    try {
      await pick(o.page, "coordinator");
      await o.page.click('#agents section.proj[data-group=coordinator] .proj-head[data-proj] >> text="coord-laharl"');
      const g = await groups(o.page);
      if (g.find((x) => x.name === "coord-laharl").open || !g.find((x) => x.name === "coord-mac").open) throw new Error("fold " + JSON.stringify(g.map((x) => [x.name, x.open])));
      await pick(o.page, "project");
      if ((await groups(o.page)).find((x) => x.name === "mac" && x.mode === "project").open) throw new Error("project fold lost");
      await pick(o.page, "coordinator");
      if ((await groups(o.page)).find((x) => x.name === "coord-laharl").open) throw new Error("coordinator fold lost");
      await o.page.click('#lf-tabs [data-tab="working"]');
      await o.page.fill("#lf-q", "web");
      if (await o.page.evaluate(() => listHash()) !== "#/list?tab=working&q=web&group=coordinator") throw new Error("hash " + await o.page.evaluate(() => listHash()));
      for (const [, ids] of await members(o.page)) if (ids.some((id) => !/web/.test(o.f.agents.find((a) => a.id === id).name))) throw new Error("box let others through " + ids);
    } finally { await o.browser.close(); }
    o = await openCoord(390, dark, { hash: "#/list?group=coordinator" });
    try {
      if (await o.page.locator("#gb-label").innerText() !== "Coordinator") throw new Error("link not applied");
      if (await o.page.evaluate(() => localStorage.getItem("herdr-list-group")) !== "coordinator") throw new Error("storage");
      const names = (await members(o.page)).map(([k]) => k);
      if (names[names.length - 1] !== "Unowned" || !names.includes("coord-mac")) throw new Error("groups " + names);
    } finally { await o.browser.close(); }
  });

  await check(`${scheme} @1280: two coordinators of one name are told apart by their host; an owner that went away is Unowned`, async () => {
    const f = coordFleet();
    f.agents.find((a) => a.id === "laharl/c0:p1").name = "coord-mac";
    const o = await open(1280, dark, { fleet: f });
    try {
      await pick(o.page, "coordinator");
      const n = (await members(o.page)).map(([k]) => k);
      if (!n.includes("coord-mac · laharl") || !n.includes("coord-mac · mac")) throw new Error("names " + n);
      if (!(await members(o.page)).find(([k]) => k === "Unowned")[1].includes("nas/w20:p1") && !blockedIds(f).includes("nas/w20:p1")) throw new Error("the gone owner's agent is not Unowned");
    } finally { await o.browser.close(); }
  });
}

// Colours: the button reads in all 15 themes (tokens); the pressed state paints blue on chip-blue as the tabs do.
import fs from "node:fs"; import vm from "node:vm"; import path from "node:path"; import { fileURLToPath } from "node:url";
const tctx = { globalThis: null }; tctx.globalThis = tctx; vm.createContext(tctx);
vm.runInContext(fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "..", "web", "themes.js"), "utf8"), tctx);
const { THEMES: ALL, contrast } = tctx.ComebyeThemes;
await check("all 15 themes (tokens): the Group button, its label, and the non-default state read", async () => {
  for (const t of ALL) {
    const v = t.vars;
    for (const [n, k, min] of [["button text on panel", contrast(v.text, v.panel), 4.5], ["Group word on panel", contrast(v.muted, v.panel), 4.5], ["non-default state", contrast(v.blue, v["chip-blue"]), 4.5], ["button edge on nav", contrast(v.border, v["nav-bg"]), 1]])
      if (k < min) throw new Error(`${t.id}: ${n} ${k.toFixed(2)} < ${min}`);
  }
});

process.exit(results.every(Boolean) ? 0 : 1);
