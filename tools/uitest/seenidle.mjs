// A finished agent reads "done" until the user has looked at it; opening it with its last message
// on screen (in the visible tab) turns it into "idle" in the list and the header, and a new turn
// makes it working, then done again. A background tab does not count.
//
//   node seenidle.mjs BASE OUT_DIR       (env: SCHEME)
// The fake herdr ends a turn in "done" when the prompt starts with "[done]".
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

const PANE = "w1:p1"; // alpha: a Claude agent with a transcript
for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  const status = () => page.evaluate((pane) => { const a = state.agents.find((x) => x.pane_id === pane); return a && a.agent_status; }, PANE);
  const listStatus = () => page.evaluate((pane) => { const a = state.agents.find((x) => x.pane_id === pane); const r = a && document.querySelector(`#agents .agent[data-id="${agentId(a)}"]`); return r ? (r.querySelector(".status")?.textContent || r.className) : null; }, PANE);
  const turn = (text) => page.evaluate(async ([pane, text]) => { const r = await fetch(`/api/agents/${encodeURIComponent(pane)}/prompt`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ text }) }); return r.status; }, [PANE, text]);
  const waitStatus = (want, ms = 12000) => page.waitForFunction(([pane, want]) => (state.agents.find((x) => x.pane_id === pane) || {}).agent_status === want, [PANE, want], { timeout: ms });

  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent", { timeout: 15000 });
  await waitStatus("idle");

  await check(`seen at ${width}px: a background tab does not count`, async () => {
    if (![200, 202].includes(await turn("[done] first"))) throw new Error("prompt refused");
    await waitStatus("done");
    await page.evaluate((pane) => {
      Object.defineProperty(document, "hidden", { get: () => true, configurable: true });
      openDetail(state.agents.find((x) => x.pane_id === pane).id || pane);
    }, PANE);
    await page.waitForSelector("#detail", { state: "visible" });
    await page.waitForTimeout(2500);
    const s = await status();
    if (s !== "done") throw new Error("hidden tab changed the status to " + s);
    await page.evaluate(() => { delete document.hidden; });
  });

  await check(`seen at ${width}px: opening it in the visible tab turns done into idle in the header and the list`, async () => {
    await page.evaluate((pane) => { closeDetail(); }, PANE);
    await page.evaluate((pane) => openDetail(state.agents.find((x) => x.pane_id === pane).id || pane), PANE);
    await waitStatus("idle", 8000);
    const head = await page.evaluate(() => document.querySelector("#d-status").textContent);
    if (head !== "idle") throw new Error("header says " + head);
    await page.screenshot({ path: `${out}/seen-open-${width}.png` });
    await page.evaluate(() => closeDetail());
    const ls = await listStatus();
    if (!/idle/.test(ls || "")) throw new Error("list row says " + ls);
  });

  await check(`seen at ${width}px: a new turn is working, then done again`, async () => {
    await turn("[done] second");
    await waitStatus("working");
    await waitStatus("done");
    const ls = await listStatus();
    if (!/done/.test(ls || "")) throw new Error("list row says " + ls);
    await page.screenshot({ path: `${out}/seen-done-${width}.png` });
  });
  await turn("settle"); // leave the shared fleet idle for the next script
  await waitStatus("idle");
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`seen-idle: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
