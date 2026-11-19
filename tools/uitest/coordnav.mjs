// `g c` opens the main coordinator's session (the same view as clicking its row, the prompt focused), from a Page, from To do and from
// a worker's session; Back returns. The command is in the palette and the ? help, hidden with no coordinator, and says so (a toast)
// when the main coordinator has gone. Phone and desktop. The fleet and roles are injected (the shared server has no coordinator).
//
//   node coordnav.mjs BASE OUT_DIR      (env: SCHEME, WIDTHS)
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const same = (got, want, what) => { if (got !== want) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };
// Polls with evaluate (waitForFunction compiles with eval, which the app's CSP can refuse), with no sleeps beyond the poll tick.
async function until(page, fn, arg, what) {
  const end = Date.now() + 8000;
  for (;;) {
    if (await page.evaluate(fn, arg)) return;
    if (Date.now() > end) throw new Error("timed out waiting for " + what);
    await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => r())));
  }
}

for (const width of (process.env.WIDTHS || "390,1280").split(",").map(Number)) {
  const phone = width < 500, tag = `${width}px`;
  const { browser, page } = await launch({ width, height: phone ? 844 : 900, dark });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(e.message));
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent");
  // The server's own agents (the detail view polls them, so injected ones would be closed as gone): the first two are made
  // coordinators (A main), the third is the worker. gone: A's role stays but its agent is not in the list.
  const ids = await page.evaluate(() => state.agents.map(agentId));
  if (ids.length < 3) throw new Error("coordnav.mjs: the server needs 3 agents, has " + ids.length);
  const [A, B, W] = ids;
  const hashOf = (id) => page.evaluate((i) => agentHash(agentById(i)), id);
  const setup = (o) => page.evaluate(([A, B, o]) => {
    window.__agents = window.__agents || state.agents;
    state.agents = o.gone ? window.__agents.filter((a) => agentId(a) !== A) : window.__agents;
    const roles = new Map();
    if (o.coord) { roles.set(B, { role: "coordinator", name: "second", at: "2026-01-02T00:00:00Z" }); roles.set(A, { role: "coordinator", name: "main", main: true, at: "2026-01-03T00:00:00Z" }); }
    ledger.roles = roles;
    if (!window.__frozen) { window.__frozen = 1; window.setAgents = () => {}; window.setLedger = () => {}; }
    renderList();
  }, [A, B, o]);
  const go = async (hash) => {
    await page.evaluate((h) => { history.pushState(null, "", h); route(); }, hash);
  };
  const sel = () => page.evaluate(() => state.selected);
  const gc = async () => { await page.evaluate(() => document.activeElement && document.activeElement !== document.body && document.activeElement.blur()); await page.keyboard.press("g"); await page.keyboard.press("c"); };
  const landed = async (what) => {
    await until(page, (id) => state.selected === id && document.activeElement && document.activeElement.id === "prompt-text", A, what + ": main coordinator's session, prompt focused");
    same(await page.evaluate(() => location.hash), await hashOf(A), what + " hash");
  };
  const back = async (hash, what) => {
    await page.goBack();
    await until(page, (h) => location.hash === h, hash, what + ": Back returns to " + hash);
  };

  await setup({ coord: true });

  for (let [name, hash] of [["a Page", "#/pages"], ["To do", "#/todo"], ["a worker's session", "W"]]) {
    if (phone && name === "To do") continue; // the To do screen is a desktop page; on a phone the same check runs from the list
    await check(`${tag}: g c from ${name} opens the main coordinator's session; Back returns`, async () => {
      if (hash === "W") hash = await hashOf(W);
      await go(hash);
      if (hash.includes("agent")) await until(page, (w) => state.selected === w, W, "worker open");
      await gc();
      await landed(name);
      await back(hash, name);
    });
  }
  if (phone) await check(`${tag}: g c from the list opens it; Back returns to the list`, async () => {
    await go("#/list");
    await gc(); await landed("list");
    await page.goBack();
    await until(page, () => state.selected == null, null, "list again");
  });

  await check(`${tag}: it is in the palette and the ? help`, async () => {
    await go("#/list");
    await page.keyboard.press("Control+k");
    await page.keyboard.type("main coord");
    await until(page, () => !$("palette").hidden && /Main coordinator/.test($("palette").innerText), null, "palette item");
    await page.keyboard.press("Escape");
    await until(page, () => $("palette").hidden, null, "palette closed");
    await page.evaluate(() => document.activeElement && document.activeElement.blur());
    await page.keyboard.press("?");
    await until(page, () => /Main coordinator/.test(document.body.innerText), null, "help row");
    await page.keyboard.press("Escape");
  });

  await check(`${tag}: with the main coordinator gone, g c toasts and stays put`, async () => {
    await setup({ coord: true, gone: true });
    await go(await hashOf(W));
    await until(page, (w) => state.selected === w, W, "worker open");
    const h = await page.evaluate(() => location.hash);
    await gc();
    await until(page, () => /no longer running/.test(document.getElementById("toast").textContent) && document.getElementById("toast").classList.contains("show"), null, "toast");
    same(await page.evaluate(() => location.hash), h, "hash");
    same(await sel(), W, "still on the worker");
  });

  await check(`${tag}: with no coordinator the command is hidden`, async () => {
    await setup({ coord: false });
    await go("#/list");
    await page.keyboard.press("Control+k");
    await page.keyboard.type("main coord");
    await until(page, () => !$("palette").hidden && /No match|No results|nothing/i.test($("palette").innerText) || ($("palette").innerText.length > 0 && !/Main coordinator/.test($("palette").innerText)), null, "palette settled");
    same(await page.evaluate(() => /Main coordinator/.test($("palette").innerText)), false, "palette lists it");
    await page.keyboard.press("Escape");
  });
  if (errs.length) { results.push(false); console.log("FAIL page errors: " + errs.join("; ")); }
  await browser.close();
}
const bad = results.filter((r) => !r).length;
console.log(bad ? `${bad} FAILED` : "all ok");
process.exit(bad ? 1 : 0);
