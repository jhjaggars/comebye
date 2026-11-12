// The quiet mark: a working worker that has written nothing for a long time (the hub stamps
// quiet_since on the agent, quiet.go) shows "quiet 40m" under its name in the list and on the opened
// agent, in the yellow chip pair, inside the row at 390 and 1280px.
//
//   node quietmark.mjs BASE OUT_DIR       (env: SCHEME)
// The mark comes from the server's quiet monitor, which needs a ledger worker with an old session
// file; here the agent record is edited in the page the way the hub would send it.
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  await check(`quiet mark at ${width}px: row and opened agent say "quiet", legible, inside the row`, async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#agents .agent", { timeout: 15000 });
    // The shared fleet changes between runs: add a working worker of our own.
    const id = await page.evaluate(() => {
      const a = { ...state.agents[0], pane_id: "w98:p1", id: (state.agents[0].host ? state.agents[0].host + "/" : "") + "w98:p1", name: "quiet-worker",
        agent_status: "working", status_since: new Date(Date.now() - 90 * 60000).toISOString(), quiet_since: new Date(Date.now() - 47 * 60000).toISOString(), background: undefined, blocked_on: undefined };
      state.agents = [...state.agents.filter((x) => x.pane_id !== "w98:p1"), a];
      renderList();
      return agentId(a);
    });
    const sel = `#agents .agent[data-id="${id}"]`;
    await page.waitForFunction((q) => /^quiet 4[67]m/.test(document.querySelector(q + " .why.stuck")?.textContent || ""), sel, { timeout: 5000 });
    const g = await page.evaluate((q) => {
      const r = document.querySelector(q), w = r.querySelector(".why.stuck"), rb = r.getBoundingClientRect(), wb = w.getBoundingClientRect();
      const cs = getComputedStyle(w);
      return { inside: wb.left >= rb.left - 1 && wb.right <= rb.right + 1, scroll: document.documentElement.scrollWidth > innerWidth, color: cs.color, bg: cs.backgroundColor, title: w.title };
    }, sel);
    if (!g.inside || g.scroll) throw new Error("row geometry " + JSON.stringify(g));
    if (g.bg === "rgba(0, 0, 0, 0)" || g.color === g.bg) throw new Error("chip colours " + JSON.stringify(g));
    if (!/stuck|written nothing/.test(g.title)) throw new Error("no explanation on hover: " + g.title);
    await page.locator(sel).first().screenshot({ path: `${out}/quiet-row-${width}.png` });
  });
  await check(`quiet mark at ${width}px: the opened agent's header says "quiet" too`, async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#agents .agent", { timeout: 15000 });
    const row = page.locator("#agents .agent").first();
    await row.click();
    await page.waitForSelector("#detail", { state: "visible" });
    const got = await page.evaluate(() => {
      const a = state.agents.find((x) => agentId(x) === state.selected) || state.agents[0];
      a.agent_status = "working";
      a.quiet_since = new Date(Date.now() - 47 * 60000).toISOString();
      renderDetail();
      const w = document.querySelector("#d-why");
      return { hidden: w.hidden, text: w.textContent, stuck: w.classList.contains("stuck") };
    });
    if (got.hidden || !/^quiet 4[67]m/.test(got.text) || !got.stuck) throw new Error("header: " + JSON.stringify(got));
    await page.screenshot({ path: `${out}/quiet-open-${width}.png` });
  });
  await check(`quiet mark at ${width}px: an idle agent with quiet_since shows nothing`, async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#agents .agent", { timeout: 15000 });
    const n = await page.evaluate(() => {
      const mk = (pane, st) => ({ ...state.agents[0], pane_id: pane, id: (state.agents[0].host ? state.agents[0].host + "/" : "") + pane, name: pane,
        agent_status: st, quiet_since: new Date(Date.now() - 47 * 60000).toISOString(), background: undefined, blocked_on: undefined });
      state.agents = [mk("w96:p1", "working"), mk("w97:p1", "idle")];
      renderList();
      return document.querySelectorAll("#agents .why.stuck").length;
    });
    if (n !== 1) throw new Error(`${n} quiet marks; only the working agent's should show`);
  });
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`quiet-mark: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
