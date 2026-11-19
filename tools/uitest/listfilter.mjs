// Left nav, slice 3: status tabs and a filter box (issue #30).
// The tabs (All, Needs you, Working, Done) carry live counts that equal their rows; the box matches name, branch and repo
// and f focuses it; blocked agents stay in All and Working; both persist and live in the URL hash (#/list?tab=..&q=..&host=..);
// an empty result says so and "Clear filters" clears the box, the tab and the host selection; the host picker's
// "+N outside this filter" is a button that clears the host filter; light and dark at 390, 1280 and 3440.
//
//   node listfilter.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const results = [];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

// Three hosts, 5 agents each: k0 blocked on hosts 1 and 2, k1 working, k2 done, k3 idle, k4 working. Names carry a branch and a repo.
const HOSTS = ["laharl", "mac", "nas"];
function fleet() {
  const now = new Date().toISOString();
  const hosts = HOSTS.map((name, i) => ({ name, local: i === 0, status: "ok", version: "v1.2.0", agents: 5 }));
  const agents = [];
  hosts.forEach((h, i) => {
    for (let k = 0; k < 5; k++) {
      const st = k === 0 ? (i === 0 ? "idle" : "blocked") : k === 1 || k === 4 ? "working" : k === 2 ? "done" : "idle";
      agents.push({ id: `${h.name}/w${i}${k}:p1`, host: h.name, pane_id: `w${i}${k}:p1`, name: `${["api", "web", "docs", "infra", "bot"][k]}-${h.name}`, agent: "claude", display_agent: "Claude", agent_status: st,
        cwd: `/home/u/code/${["server", "ui", "site", "ops", "bots"][k]}`, worktree: { repo: ["server", "ui", "site", "ops", "bots"][k], checkout: "/x", linked: k === 1, branch: k === 1 ? `feat-${h.name}` : undefined }, last_active: now, status_since: now });
    }
  });
  return { agents, hosts };
}
const count = (f, fn) => f.agents.filter(fn).length;

async function open(width, dark, { hash = "", store = {} } = {}) {
  const { browser, page } = await launch({ width, height: width < 500 ? 844 : 900, dark });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(e.message));
  const f = fleet();
  await page.goto(base + "/", { waitUntil: "load" }); // the link is applied below, once the fleet is ours: a cold start on a filter would hide the real one
  await page.waitForSelector("#agents .agent");
  await page.evaluate(([f, store, hash]) => {
    for (const k of ["herdr-host-filter", "herdr-list-tab", "herdr-list-q"]) try { localStorage.removeItem(k); } catch {}
    for (const [k, v] of Object.entries(store)) try { localStorage.setItem(k, v); } catch {}
    state.hostFilter = null; state.listTab = null; state.listQ = null;
    state.hosts = f.hosts; setAgents(f); window.setAgents = () => {};
    if (hash) location.hash = ""; // a clean slate, then the link
    if (hash) { history.replaceState(null, "", hash); route(); } else { renderList(); route(); }
  }, [f, store, hash]);
  await page.waitForTimeout(150);
  return { browser, page, errs, f };
}
const shown = (page) => page.evaluate(() => [...document.querySelectorAll("#agents .agent[data-id]")].filter((e) => !e.classList.contains("host-down")).map((e) => e.dataset.id));
const tabs = (page) => page.evaluate(() => Object.fromEntries([...document.querySelectorAll("#lf-tabs .lf-tab")].map((b) => [b.dataset.tab, { n: +b.querySelector(".n").textContent, on: b.getAttribute("aria-pressed") === "true" }])));
const hash = (page) => page.evaluate(() => location.hash);
const noScroll = (page) => page.evaluate(() => { const b = document.querySelector("#list-filter"); return [b.scrollWidth, b.clientWidth, document.documentElement.scrollWidth, document.documentElement.clientWidth]; });

for (const dark of [false, true]) {
  const scheme = dark ? "dark" : "light";
  for (const w of [390, 1280, 3440]) {
    await check(`${scheme} @${w}: four tabs with live counts equal to their rows; nothing overflows; blocked agents are in All and Working`, async () => {
      const { browser, page, errs, f } = await open(w, dark);
      try {
        const want = { all: f.agents.length, needs: count(f, (a) => a.agent_status === "blocked"), working: count(f, (a) => a.agent_status === "working" || a.agent_status === "blocked"), done: count(f, (a) => a.agent_status === "done" || a.agent_status === "idle") };
        const t = await tabs(page);
        if (Object.keys(t).join() !== "all,needs,working,done") throw new Error("tabs " + Object.keys(t));
        if (!t.all.on) throw new Error("All is not the default");
        for (const k of Object.keys(want)) if (t[k].n !== want[k]) throw new Error(`${k} count ${t[k].n}, want ${want[k]}`);
        for (const k of ["all", "needs", "working", "done"]) {
          await page.click(`#lf-tabs [data-tab="${k}"]`);
          const rows = await shown(page);
          if (rows.length !== want[k]) throw new Error(`${k}: ${rows.length} rows, count says ${want[k]}`);
          const st = await page.evaluate((ids) => ids.map((i) => agentById(i).agent_status), rows);
          if (k === "needs" && st.some((s) => s !== "blocked")) throw new Error("needs shows " + st);
          if (k === "working" && (st.some((s) => s !== "working" && s !== "blocked") || !st.includes("blocked"))) throw new Error("working must hold the blocked agents: " + st);
          if (k === "done" && st.some((s) => s !== "done" && s !== "idle")) throw new Error("done shows " + st);
          if (k === "all" && !st.includes("blocked")) throw new Error("all lost the blocked agents");
          const tt = await tabs(page);
          if (!tt[k].on || Object.entries(tt).filter(([, v]) => v.on).length !== 1) throw new Error("pressed state " + JSON.stringify(tt));
          for (const j of Object.keys(want)) if (tt[j].n !== want[j]) throw new Error(`count of ${j} moved to ${tt[j].n} when ${k} was picked`);
        }
        const g = await noScroll(page);
        if (g[0] !== g[1] || g[2] > g[3]) throw new Error("overflow " + g);
        const hs = await page.evaluate(() => [...document.querySelectorAll("#lf-q, .lf-tab")].map((e) => Math.round(e.getBoundingClientRect().height)));
        if (hs.some((h) => h < 36)) throw new Error("targets " + hs);
        await page.click('#lf-tabs [data-tab="working"]');
        await page.waitForTimeout(150);
        await page.screenshot({ path: `${out}/listfilter-tabs-${w}-${scheme}.png` });
        if (errs.length) throw new Error(errs.join("; "));
      } finally { await browser.close(); }
    });
  }

  await check(`${scheme} @1280: typing narrows by name, repo and branch (words are ANDed), clearing restores, counts follow`, async () => {
    const { browser, page, f } = await open(1280, dark);
    try {
      await page.fill("#lf-q", "infra");
      let rows = await shown(page);
      if (rows.length !== 3 || rows.some((i) => !/infra/.test(i) && !/w\d3/.test(i))) throw new Error("name: " + rows);
      let t = await tabs(page);
      if (t.all.n !== 3) throw new Error("the All count did not follow the box: " + t.all.n);
      await page.fill("#lf-q", "SERVER");
      rows = await shown(page);
      if (rows.length !== 3 || !rows.every((i) => /w\d0/.test(i))) throw new Error("repo, any case: " + rows);
      await page.fill("#lf-q", "feat-mac");
      rows = await shown(page);
      if (rows.join() !== "mac/w11:p1") throw new Error("branch: " + rows);
      await page.fill("#lf-q", "ui mac");
      if ((await shown(page)).join() !== "mac/w11:p1") throw new Error("two words: " + await shown(page));
      await page.fill("#lf-q", "api  ");
      if ((await shown(page)).length !== 3) throw new Error("trailing space changed the match");
      // The box and a tab combine.
      await page.fill("#lf-q", "web");
      await page.click('#lf-tabs [data-tab="done"]');
      if ((await shown(page)).length !== 0) throw new Error("web is working, not done: " + await shown(page));
      await page.click('#lf-tabs [data-tab="working"]');
      if ((await shown(page)).length !== 3) throw new Error("web in Working");
      await page.fill("#lf-q", "");
      await page.click('#lf-tabs [data-tab="all"]');
      if ((await shown(page)).length !== f.agents.length) throw new Error("clearing did not restore");
      await page.fill("#lf-q", "zzz");
      await page.click("#lf-x");
      if ((await shown(page)).length !== f.agents.length || await page.inputValue("#lf-q") !== "") throw new Error("the x did not clear");
      if (!await page.locator("#lf-x").isHidden()) throw new Error("the x shows on an empty box");
      // The poll re-renders the list: the box keeps its text and its focus.
      await page.fill("#lf-q", "bot");
      await page.focus("#lf-q");
      await page.evaluate(() => { renderList(); renderList(); });
      if (await page.evaluate(() => document.activeElement.id) !== "lf-q" || await page.inputValue("#lf-q") !== "bot") throw new Error("a re-render took the box");
      // Esc clears, then leaves; Enter and ArrowDown go to the first row.
      await page.keyboard.press("Escape");
      if (await page.inputValue("#lf-q") !== "") throw new Error("Esc did not clear");
      await page.keyboard.press("Escape");
      if (await page.evaluate(() => document.activeElement.id) === "lf-q") throw new Error("Esc did not leave");
      await page.focus("#lf-q"); await page.keyboard.type("api"); await page.keyboard.press("ArrowDown");
      if (!await page.evaluate(() => document.activeElement.classList.contains("agent"))) throw new Error("ArrowDown did not reach the list");
    } finally { await browser.close(); }
  });

  await check(`${scheme} @1280: f focuses the box, does not fire while typing, / stays Search, h stays the host picker, g f still goes to Search`, async () => {
    const { browser, page } = await open(1280, dark);
    try {
      await page.evaluate(() => { document.activeElement && document.activeElement.blur(); });
      await page.keyboard.press("f");
      if (await page.evaluate(() => document.activeElement.id) !== "lf-q") throw new Error("f did not focus the box");
      if (await page.inputValue("#lf-q") !== "") throw new Error("f was typed into the box");
      await page.keyboard.type("fff h / g");
      if (await page.inputValue("#lf-q") !== "fff h / g") throw new Error("typing was taken: " + await page.inputValue("#lf-q"));
      if (!await page.locator("#hp-pop").isHidden() || await page.locator("#recall:not([hidden])").count()) throw new Error("a typed key opened something");
      await page.keyboard.press("Escape"); await page.keyboard.press("Escape");
      await page.keyboard.press("h");
      await page.waitForSelector("#hp-pop:not([hidden])");
      await page.keyboard.press("Escape");
      await page.evaluate(() => document.activeElement.blur());
      await page.keyboard.press("/");
      await page.waitForSelector("#recall:not([hidden])");
      await page.evaluate(() => { location.hash = ""; document.activeElement.blur(); });
      await page.keyboard.press("g"); await page.keyboard.press("f");
      if (await page.evaluate(() => document.activeElement.id) === "lf-q") throw new Error("g f must stay Search");
      // The cheat sheet and the palette list it.
      await page.evaluate(() => { location.hash = ""; document.activeElement.blur(); });
      await page.keyboard.press("?");
      const sheet = await page.locator("#cheat-box").innerText();
      if (!/Filter the agent list/.test(sheet)) throw new Error("the cheat sheet does not list f");
    } finally { await browser.close(); }
  });

  await check(`${scheme}: the view is in the hash and in storage; a reload (a fresh load of the link) keeps tab, text and hosts`, async () => {
    const { browser, page } = await open(390, dark);
    try {
      await page.fill("#lf-q", "api");
      await page.click('#lf-tabs [data-tab="working"]');
      if (await hash(page) !== "#/list?tab=working&q=api") throw new Error("hash " + await hash(page));
      await page.click("#hp-btn"); await page.tap('#hp-list .hp-row[data-host="mac"]'); await page.keyboard.press("Escape");
      if (await hash(page) !== "#/list?tab=working&q=api&host=mac") throw new Error("hash with a host " + await hash(page));
      const st = await page.evaluate(() => [localStorage.getItem("herdr-list-tab"), localStorage.getItem("herdr-list-q"), localStorage.getItem("herdr-host-filter")]);
      if (st.join("|") !== 'working|api|["mac"]') throw new Error("storage " + st);
      // Back to the defaults: the hash goes away.
      await page.evaluate(() => clearFilters());
      if (await hash(page) !== "") throw new Error("hash after Clear " + await hash(page));
    } finally { await browser.close(); }
    // A link: the hash alone sets the view.
    let o = await open(390, dark, { hash: "#/list?tab=needs&host=mac,nas" });
    try {
      const t = await tabs(o.page);
      if (!t.needs.on) throw new Error("the link's tab was not applied");
      const rows = await shown(o.page);
      if (rows.length !== 2 || !rows.every((i) => /w[12]0/.test(i))) throw new Error("rows " + rows);
      if (await o.page.locator("#hp-btn .hp-label").innerText() !== "mac + nas") throw new Error("host label");
    } finally { await o.browser.close(); }
    // Storage alone (a reload with no hash): the hash is written back.
    o = await open(390, dark, { store: { "herdr-list-tab": "done", "herdr-list-q": "docs" } });
    try {
      const t = await tabs(o.page);
      if (!t.done.on || await o.page.inputValue("#lf-q") !== "docs") throw new Error("storage not read");
      if (await hash(o.page) !== "#/list?tab=done&q=docs") throw new Error("hash from storage " + await hash(o.page));
      if ((await shown(o.page)).length !== 3) throw new Error("rows " + (await shown(o.page)).length);
    } finally { await o.browser.close(); }
    // Garbage in the link falls back to the defaults.
    o = await open(390, dark, { hash: "#/list?tab=bogus&q=&host=" });
    try { if (!(await tabs(o.page)).all.on || (await shown(o.page)).length !== 15) throw new Error("garbage"); } finally { await o.browser.close(); }
  });

  await check(`${scheme} @390: opening an agent leaves the hash to its route; coming back restores the list's own`, async () => {
    // The server's own agents here: the test fleet's do not exist there, and the app drops a detail whose agent is gone.
    const { browser, page } = await launch({ width: 390, height: 844, dark });
    page.setDefaultTimeout(8000);
    try {
      await page.goto(base + "/", { waitUntil: "load" });
      await page.waitForSelector("#agents .agent[data-id]");
      await page.evaluate(() => { for (const k of ["herdr-host-filter", "herdr-list-tab", "herdr-list-q"]) localStorage.removeItem(k); });
      const nm = await page.evaluate(() => agentById(document.querySelector("#agents .agent[data-id]").dataset.id).name);
      const q = nm.slice(0, 4).trim();
      await page.fill("#lf-q", q);
      const h0 = await hash(page);
      if (h0 !== "#/list?q=" + encodeURIComponent(q).replace(/%20/g, "+")) throw new Error("hash with a filter " + h0);
      await page.click("#agents .agent[data-id]");
      await page.waitForFunction(() => location.hash.startsWith("#/agent/"));
      await page.evaluate(() => history.back());
      await page.waitForFunction(() => !location.hash.startsWith("#/agent/"));
      await page.waitForTimeout(150);
      if (await hash(page) !== h0) throw new Error("hash after back " + await hash(page));
      if (await page.inputValue("#lf-q") !== q) throw new Error("box lost");
    } finally { await browser.close(); }
  });

  await check(`${scheme} @390: nothing matches: a message and "Clear filters", which also clears the host selection`, async () => {
    const { browser, page } = await open(390, dark);
    try {
      await page.click("#hp-btn"); await page.tap('#hp-list .hp-row[data-host="mac"]'); await page.keyboard.press("Escape");
      await page.click('#lf-tabs [data-tab="done"]');
      await page.fill("#lf-q", "no-such-agent");
      if ((await shown(page)).length) throw new Error("rows still show");
      const e = await page.locator("#agents .empty").innerText();
      if (!/No agents match/.test(e) || !/no-such-agent/.test(e) || !/Done/.test(e) || !/mac/.test(e)) throw new Error("message " + e);
      // The tabs still read: they count what the host and the box leave (none).
      const t = await tabs(page);
      if (t.all.n !== 0) throw new Error("count " + t.all.n);
      await page.waitForTimeout(100);
      await page.screenshot({ path: `${out}/listfilter-empty-${scheme}.png` });
      await page.click("#agents .empty-act");
      const rows = await shown(page);
      if (rows.length !== 15) throw new Error("rows after Clear filters " + rows.length);
      if (await page.inputValue("#lf-q") !== "" || !(await tabs(page)).all.on) throw new Error("box or tab not cleared");
      if (await page.locator("#hp-btn .hp-label").innerText() !== "All hosts") throw new Error("host not cleared");
      if (await hash(page) !== "") throw new Error("hash " + await hash(page));
      const st = await page.evaluate(() => [localStorage.getItem("herdr-list-tab"), localStorage.getItem("herdr-list-q"), localStorage.getItem("herdr-host-filter")]);
      if (st.join("|") !== 'all||[]') throw new Error("storage " + st);
    } finally { await browser.close(); }
  });

  await check(`${scheme} @1280: with the host picker: blocked agents on other hosts stay, the note is a button that clears the host filter, the box and tabs still apply`, async () => {
    const { browser, page } = await open(1280, dark);
    try {
      await page.click("#hp-btn"); await page.click('#hp-list .hp-row[data-host="laharl"]'); await page.keyboard.press("Escape");
      // laharl: 5 agents, none blocked; mac and nas each have one blocked, shown anyway.
      let rows = await shown(page);
      if (rows.length !== 7) throw new Error("rows " + rows.length);
      let t = await tabs(page);
      if (t.needs.n !== 2 || t.all.n !== 7) throw new Error("counts " + JSON.stringify(t));
      await page.click('#lf-tabs [data-tab="needs"]');
      if ((await shown(page)).length !== 2) throw new Error("needs on laharl: " + (await shown(page)).length);
      const note = page.locator("#host-filter .hp-out button");
      if ((await note.innerText()).trim() !== "+2 outside this filter") throw new Error("note " + await note.innerText());
      await page.fill("#lf-q", "web");
      // The box is explicit: it hides a blocked agent that does not match (the host filter never does).
      if ((await shown(page)).length !== 0) throw new Error("box + needs: " + await shown(page));
      await page.fill("#lf-q", "");
      await page.click('#lf-tabs [data-tab="working"]');
      await note.click();
      if (await page.locator("#hp-btn .hp-label").innerText() !== "All hosts") throw new Error("the note did not clear the host filter");
      if (await page.locator("#host-filter .hp-out button").count()) throw new Error("the note stays");
      if (!(await tabs(page)).working.on) throw new Error("clearing the host cleared the tab too");
      rows = await shown(page);
      if (rows.length !== 8) throw new Error("working across hosts " + rows.length);
      await page.screenshot({ path: `${out}/listfilter-hosts-${scheme}.png` });
    } finally { await browser.close(); }
  });
}

// Colours: the box, the tabs, the pressed tab and the note read in all 15 themes (tokens) and as painted.
import fs from "node:fs"; import vm from "node:vm"; import path from "node:path"; import { fileURLToPath } from "node:url";
const tctx = { globalThis: null }; tctx.globalThis = tctx; vm.createContext(tctx);
vm.runInContext(fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "..", "web", "themes.js"), "utf8"), tctx);
const { THEMES: ALL, contrast } = tctx.ComebyeThemes;
await check("all 15 themes (tokens): the box text and placeholder, the tabs, the pressed tab and the count read", async () => {
  for (const t of ALL) {
    const v = t.vars;
    for (const [n, k, min] of [["box text on panel", contrast(v.text, v.panel), 4.5], ["placeholder on panel", contrast(v.muted, v.panel), 4.5], ["tab on nav", contrast(v.muted, v["nav-bg"]), 4.5], ["pressed tab", contrast(v.blue, v["chip-blue"]), 4.5], ["needs count on nav", contrast(v.orange, v["nav-bg"]), 3], ["box edge on nav", contrast(v.border, v["nav-bg"]), 1]])
      if (k < min) throw new Error(`${t.id}: ${n} ${k.toFixed(2)} < ${min}`);
  }
});

process.exit(results.every(Boolean) ? 0 : 1);
