// Progress notes: a handoff worker's latest `comebye report --progress` note shows under its name
// in the Agents list, the opened agent's header and the Status page, with its age; it is muted
// when over 2 h old while the agent works; it is hidden when there is none; a working worker
// with no note for 90 min gets a "no update" hint.
//
//   node notes.mjs BASE OUT_DIR       (env: SCHEME)
// The ledger entry is edited in the page the way the hub would send it.
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

// Three workers: fresh note, an old note, none (and running 3 h).
const SETUP = () => {
  // The live polls would replace the edited agents and ledger: freeze them for this check.
  window.loadLedger = async () => {};
  window.setAgents = () => {};
  const ago = (m) => new Date(Date.now() - m * 60000).toISOString();
  const mk = (pane, name, entry) => {
    const a = { ...state.agents[0], pane_id: pane, id: (state.agents[0].host ? state.agents[0].host + "/" : "") + pane, name,
      agent_status: "working", status_since: ago(300), quiet_since: undefined, background: undefined, blocked_on: undefined };
    return [a, { id: "h-" + pane, agent_id: agentId(a), status: "running", created_at: ago(300), ...entry }];
  };
  const rows = [
    mk("w91:p1", "note-fresh", { progress: { text: "Writing the Go tests for the CLI; next the web checks.", at: ago(12) } }),
    mk("w92:p1", "note-old", { progress: { text: "Plan done, starting on the server side.", at: ago(200) } }),
    mk("w93:p1", "note-none", {}),
  ];
  state.agents = [...state.agents.filter((x) => !/^w9[123]:p1$/.test(x.pane_id)), ...rows.map((r) => r[0])];
  ledger.on = true;
  for (const [a, e] of rows) ledger.byAgent.set(agentId(a), e);
  ledger.entries = [...ledger.entries.filter((x) => !/^h-w9[123]/.test(x.id || "")), ...rows.map((r) => r[1])];
  renderList();
  return rows.map((r) => agentId(r[0]));
};

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent", { timeout: 15000 });
  const ids = await page.evaluate(SETUP);
  const row = (i) => `#agents .agent[data-id="${ids[i]}"]`;
  await check(`notes at ${width}px: the list shows the note with its age, inside the row`, async () => {
    await page.waitForSelector(row(0) + " .pnote");
    const g = await page.evaluate((q) => {
      const r = document.querySelector(q), n = r.querySelector(".pnote"), rb = r.getBoundingClientRect(), nb = n.getBoundingClientRect();
      return { text: n.textContent, inside: nb.left >= rb.left - 1 && nb.right <= rb.right + 1, scroll: document.documentElement.scrollWidth > innerWidth, title: n.title };
    }, row(0));
    if (!/Writing the Go tests/.test(g.text) || !/1[23]m ago/.test(g.text)) throw new Error("text " + JSON.stringify(g));
    if (!g.inside || g.scroll) throw new Error("geometry " + JSON.stringify(g));
    if (!/next the web checks/.test(g.title)) throw new Error("no full note on hover");
    if (await page.locator(row(0) + " .pnote.old").count()) throw new Error("fresh note is muted");
    if (await page.locator(row(0) + " .nou").count()) throw new Error("fresh note has a no-update hint");
    await page.locator(row(0)).screenshot({ path: `${out}/note-row-${width}.png` });
  });
  await check(`notes at ${width}px: a note over 2 h old is muted, and the worker shows "no update"`, async () => {
    const g = await page.evaluate((q) => {
      const n = document.querySelector(q + " .pnote"), f = document.querySelector(q.replace(/w92/, "w91") + " .pnote .pn-t");
      return { old: n.classList.contains("old"), color: getComputedStyle(n.querySelector(".pn-t")).color, fresh: getComputedStyle(f).color, nou: !!n.querySelector(".nou"), text: n.textContent };
    }, row(1));
    if (!g.old || g.color === g.fresh || !g.nou || !/3h ago/.test(g.text)) throw new Error(JSON.stringify(g));
    await page.locator(row(1)).screenshot({ path: `${out}/note-old-${width}.png` });
  });
  await check(`notes at ${width}px: no note and a worker that just started shows nothing; a long quiet one shows "no update" only`, async () => {
    const g = await page.evaluate((q) => {
      const n = document.querySelector(q + " .pnote");
      return n ? { text: n.textContent, hasT: !!n.querySelector(".pn-t") } : null;
    }, row(2));
    if (!g || g.hasT || g.text !== "no update") throw new Error(JSON.stringify(g));
    const none = await page.evaluate((id) => { const e = ledger.byAgent.get(id); e.created_at = new Date(Date.now() - 20 * 60000).toISOString(); renderList(); return document.querySelectorAll(`#agents .agent[data-id="${id}"] .pnote`).length; }, ids[2]);
    if (none) throw new Error("hint shown for a 20 min old worker");
    await page.evaluate((id) => { ledger.byAgent.get(id).created_at = new Date(Date.now() - 300 * 60000).toISOString(); renderList(); }, ids[2]);
  });
  await check(`notes at ${width}px: the opened agent's header shows the note`, async () => {
    // A real agent of the fixture fleet (the fake ones have no pane to open), given a ledger entry.
    const id = await page.evaluate(() => {
      const a = state.agents.find((x) => !/^w9[123]:p1$/.test(x.pane_id));
      a.agent_status = "working";
      ledger.byAgent.set(agentId(a), { id: "h-real", agent_id: agentId(a), status: "running", created_at: new Date(Date.now() - 300 * 60000).toISOString(),
        progress: { text: "Writing the Go tests for the CLI; next the web checks.", at: new Date(Date.now() - 12 * 60000).toISOString() } });
      renderList();
      return agentId(a);
    });
    await page.locator(`#agents .agent[data-id="${id}"]`).click();
    await page.waitForFunction((x) => state.selected === x, id, { timeout: 5000 });
    await page.evaluate(() => renderDetail());
    const g = await page.evaluate(() => { const n = document.querySelector("#d-note"); return { hidden: n.hidden, text: n.textContent, scroll: document.documentElement.scrollWidth > innerWidth }; });
    if (g.hidden || !/Writing the Go tests/.test(g.text) || !/1[23]m ago/.test(g.text) || g.scroll) throw new Error(JSON.stringify(g));
    await page.screenshot({ path: `${out}/note-open-${width}.png` });
    await page.evaluate(() => { ledger.byAgent.delete(state.selected); renderDetailNote(); });
    if (!(await page.evaluate(() => document.querySelector("#d-note").hidden))) throw new Error("header note shown with no ledger entry");
  });
  await check(`notes at ${width}px: the Status page shows the note, its age, and the hint`, async () => {
    await page.evaluate(SETUP);
    const rows = await page.evaluate(() => {
      const d = document.createElement("div"); d.innerHTML = statusHTML(true);
      return [...d.querySelectorAll(".st-note")].map((n) => ({ text: n.textContent, old: n.classList.contains("old") }));
    });
    if (!rows.some((t) => /Writing the Go tests.*1[23]m ago/.test(t.text) && !t.old)) throw new Error("fresh: " + JSON.stringify(rows));
    if (!rows.some((t) => /Plan done.*ago/.test(t.text) && t.old)) throw new Error("old: " + JSON.stringify(rows));
    // The real Status page, as a screenshot.
    if (width > 1100) {
      await page.evaluate(() => { closeDetail(); setView("status"); });
      await page.waitForSelector("#ps-body .st-note");
      await page.evaluate(() => document.querySelector("#ps-body .st-note").scrollIntoView({ block: "center" }));
      await page.screenshot({ path: `${out}/note-status-${width}.png` });
    }
  });
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`notes: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
