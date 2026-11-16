// Restart in place from the app (issue #20, slice 4): "Restart…" in an agent's ⋯ menu, only for an idle or
// done claude, pi or opencode; the confirm sheet says what happens; progress while it runs; the result:
// success, flags that could not be recovered, or the refusal's plain sentence. At 390 and 1280px.
//
//   node restart.mjs BASE OUT_DIR       (env: SCHEME)
// The hub's answers are mocked (a real restart needs a real agent process), so the check does not depend on
// the shared fleet. The server side is covered by internal/server/restart_test.go.
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

const FLEET = [
  ["w90:p1", "calm-claude", "claude", "idle"],
  ["w91:p1", "busy-claude", "claude", "working"],
  ["w92:p1", "quiet-pi", "pi", "done"],
  ["w93:p1", "codex-thing", "codex", "idle"],
  ["w94:p1", "loopy-claude", "claude", "idle"],
];

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  const posts = [];
  let hold = null; // a promise the mock waits on, to look at the progress state
  await page.route(/\/api\/hosts\/[^/]+\/restart$/, async (r) => {
    const body = r.request().postDataJSON();
    posts.push({ url: new URL(r.request().url()).pathname, body });
    if (hold) await hold;
    if (body.agent === "w91:p1") return r.fulfill({ status: 409, json: { code: "agent_working", message: "the agent is working; restart it when it is idle" } });
    if (body.agent === "w94:p1" && !body.force_schedule) return r.fulfill({ status: 409, json: { code: "schedule_pending", message: "the session has a loop or wakeup scheduled, which a restart ends (force_schedule restarts anyway)" } });
    const pi = body.agent === "w92:p1";
    return r.fulfill({ json: { agent: { pane_id: body.agent }, session: "s", old_pid: 11, new_pid: 12, args: [], args_from: pi ? "session" : "recorded" } });
  });

  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent", { timeout: 15000 });
  // Our own agents, so the shared fleet does not matter.
  await page.evaluate((fleet) => {
    const t = state.agents[0];
    state.agents = fleet.map(([pane, name, kind, st]) => ({ ...t, pane_id: pane, id: (t.host ? t.host + "/" : "") + pane, name, agent: kind, agent_status: st,
      background: undefined, blocked_on: undefined, quiet_since: undefined, park_hint: undefined }));
    renderList();
    // The shared fleet's updates must not replace ours half way through.
    window.setAgents = () => {};
  }, FLEET);
  const menu = async (name) => {
    await page.click(`#agents .agent:has(.name:text-is("${name}")) .row-more`);
    await page.waitForSelector("#row-menu:not([hidden])");
  };
  // A shot waits out the menu's slide-in and the last toast, so it shows the settled screen.
  const shot = async (name) => { await page.waitForTimeout(500); await page.evaluate(() => { $("toast").className = ""; }); await page.waitForTimeout(400); await page.screenshot({ path: `${out}/${name}-${width}.png` }); };
  const closeMenu = () => page.evaluate(() => closeRowMenu());

  await check(`menu at ${width}px: Restart… only for an idle or done claude, pi or opencode`, async () => {
    const seen = {};
    for (const [, name] of FLEET) {
      await menu(name);
      seen[name] = await page.evaluate(() => !document.querySelector("#row-restart").hidden);
      if (name === "calm-claude") await shot("restart-menu");
      await closeMenu();
    }
    const want = { "calm-claude": true, "busy-claude": false, "quiet-pi": true, "codex-thing": false, "loopy-claude": true };
    if (JSON.stringify(seen) !== JSON.stringify(want)) throw new Error("rows " + JSON.stringify(seen));
  });

  await check(`sheet at ${width}px: says what happens, the button is tall enough, no sideways scroll`, async () => {
    await menu("calm-claude");
    await page.click("#row-restart");
    await page.waitForSelector("#rt:not([hidden])");
    const g = await page.evaluate(() => ({ name: document.querySelector("#rt-name").textContent, note: document.querySelector("#rt-note").textContent,
      goH: document.querySelector("#rt-go").getBoundingClientRect().height, scroll: document.documentElement.scrollWidth > innerWidth }));
    if (g.name !== "calm-claude" || !/same pane/.test(g.note) || !/same conversation/.test(g.note) || !/background stops/.test(g.note) || g.scroll) throw new Error("sheet " + JSON.stringify(g));
    if (width < 500 && g.goH < 44) throw new Error("Restart is " + g.goH + "px tall on a phone");
    await shot("restart-sheet");
  });

  await check(`success at ${width}px: progress while it runs, then the result; the request names the pane on its host`, async () => {
    let release; hold = new Promise((res) => { release = res; });
    await page.click("#rt-go");
    await page.waitForFunction(() => document.querySelector("#rt").classList.contains("busy") && /Ending it/.test(document.querySelector("#rt-status").textContent));
    const dis = await page.evaluate(() => document.querySelector("#rt-go").disabled);
    if (!dis) throw new Error("Restart is still pressable while it runs");
    await shot("restart-progress");
    hold = null; release();
    await page.waitForFunction(() => /Restarted in the same pane/.test(document.querySelector("#rt-result").textContent));
    const g = await page.evaluate(() => ({ go: document.querySelector("#rt-go").textContent, cls: document.querySelector("#rt-result").className, text: document.querySelector("#rt-result").textContent }));
    if (g.go !== "Done" || !/rt-ok/.test(g.cls) || /could not be recovered/.test(g.text)) throw new Error("result " + JSON.stringify(g));
    const p = posts.filter((x) => x.body.agent === "w90:p1");
    if (p.length !== 1 || !/\/api\/hosts\/[^/]+\/restart$/.test(p[0].url) || p[0].body.name !== "calm-claude") throw new Error("posts " + JSON.stringify(posts));
    await shot("restart-result");
    await page.click("#rt-go");
    await page.waitForSelector("#rt", { state: "hidden" });
  });

  await check(`flags at ${width}px: a pi whose flags could not be recovered says so`, async () => {
    await menu("quiet-pi");
    await page.click("#row-restart");
    await page.click("#rt-go");
    await page.waitForFunction(() => /Restarted/.test(document.querySelector("#rt-result").textContent));
    const g = await page.evaluate(() => ({ text: document.querySelector("#rt-result").textContent, cls: document.querySelector("#rt-result").className }));
    if (!/start flags could not be recovered/.test(g.text) || !/rt-warn/.test(g.cls)) throw new Error("result " + JSON.stringify(g));
    await shot("restart-flags");
    await page.click("#rt-go");
    await page.waitForSelector("#rt", { state: "hidden" });
  });

  await check(`refusal at ${width}px: the plain sentence, the sheet stays open and can be closed`, async () => {
    // The menu hides the row for a working agent, so open the sheet the way a stale list would.
    await page.evaluate(() => { const a = state.agents.find((x) => x.pane_id === "w91:p1"); a.agent_status = "idle"; openRestart(agentId(a)); });
    await page.waitForSelector("#rt:not([hidden])");
    await page.click("#rt-go");
    await page.waitForFunction(() => /restart it when it is idle/.test(document.querySelector("#rt-result").textContent));
    const g = await page.evaluate(() => ({ text: document.querySelector("#rt-result").textContent, cls: document.querySelector("#rt-result").className, go: document.querySelector("#rt-go").textContent, dis: document.querySelector("#rt-go").disabled }));
    if (g.text !== "It is working; restart it when it is idle" || !/rt-err/.test(g.cls) || g.go !== "Restart" || g.dis) throw new Error("refusal " + JSON.stringify(g));
    await shot("restart-refused");
    await page.click("#rt-close");
    await page.waitForSelector("#rt", { state: "hidden" });
  });

  await check(`schedule at ${width}px: a loop or wakeup is refused, then "Restart anyway" sends force_schedule`, async () => {
    await menu("loopy-claude");
    await page.click("#row-restart");
    await page.click("#rt-go");
    await page.waitForFunction(() => document.querySelector("#rt-go").textContent === "Restart anyway");
    await page.click("#rt-go");
    await page.waitForFunction(() => /Restarted/.test(document.querySelector("#rt-result").textContent));
    const p = posts.filter((x) => x.body.agent === "w94:p1");
    if (p.length !== 2 || p[0].body.force_schedule || p[1].body.force_schedule !== true) throw new Error("posts " + JSON.stringify(p));
    await page.click("#rt-go");
    await page.waitForSelector("#rt", { state: "hidden" });
  });

  await check(`back at ${width}px: the browser's back closes the sheet`, async () => {
    await menu("calm-claude");
    await page.click("#row-restart");
    await page.waitForSelector("#rt:not([hidden])");
    await page.goBack();
    await page.waitForSelector("#rt", { state: "hidden" });
  });
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`restart: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
