// Honest loading and empty states (issue #136, slice 1): on a slow or dead link no screen claims "nothing" before the hub
// has said so. While /api/* is held, the screens show skeleton rows ("Reaching the hub…"), never "No agents running" or
// "Nothing is waiting on you"; after 10 s, or at once when the connection is reset, "Can't reach the hub" with Try again;
// Try again recovers when the hub is back; a failed poll keeps a list already loaded; no raw "Failed to fetch"; and the
// real empty states still show when the hub truly answers "none". Phone (390) and desktop (1280).
//
//   node offline.mjs BASE OUT_DIR       (env: SCHEME)
// The slow and dead links are Playwright routes on /api/ (they catch the event stream too), as the plan's audit measured them.
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const API = /\/api\//;
// hold: every /api/ request waits (a stalled link); it is let through when the page goes, or never answered.
const hold = (page) => page.route(API, async (r) => { await sleep(20000); r.continue().catch(() => {}); });
// reset: every /api/ request fails at the network level (a dead link), as "Failed to fetch" would.
const reset = (page) => page.route(API, (r) => r.abort("connectionreset"));
const textOf = (page, sel) => page.evaluate((s) => [...document.querySelectorAll(s)].filter((e) => e.offsetParent !== null).map((e) => e.innerText).join("\n"), sel);
const bodyText = (page) => page.evaluate(() => document.body.innerText);
async function noRaw(page) {
  const t = await bodyText(page);
  if (/Failed to fetch|NetworkError|Load failed/.test(t)) throw new Error("raw fetch error on screen: " + t.match(/.*(Failed to fetch|NetworkError|Load failed).*/)[0]);
}
const EMPTY_LEDGER = { plans: [], entries: [], marks: [], roles: {} };
const NO_TODOS = { todos: [], open: 0, answered: 0 };
const NO_AGENTS = { agents: [], hosts: [{ name: "uitest", local: true, status: "ok" }] };

async function phase(width) {
  const desk = width > 500, tag = desk ? "desktop" : "phone";
  const open = async () => {
    const { browser, page } = await launch({ width, height: desk ? 800 : 844, dark });
    return { browser, page };
  };

  // 1. A held link: skeleton rows, never an empty state, then "Can't reach the hub" after 10 s.
  await check(`${tag}: held link shows skeleton rows, not "No agents running"`, async () => {
    const { browser, page } = await open();
    try {
      await hold(page);
      await page.goto(base + "/");
      await sleep(2500);
      const list = await textOf(page, "#agents");
      if (/No agents running/.test(list)) throw new Error("the list says none before the hub answered");
      if (!(await page.locator("#agents .src-wait .skel-row").count())) throw new Error("no skeleton rows: " + JSON.stringify(list));
      if (!/Reaching the hub/.test(list)) throw new Error("no 'Reaching the hub…': " + JSON.stringify(list));
      if (!desk) {
        // The phone's To do card says it is waiting (it was hidden).
        if (!(await page.locator("#todo .src-wait").isVisible())) throw new Error("the phone's To do card is hidden while it waits");
      }
      await page.screenshot({ path: `${out}/offline-held-${tag}.png` });
      await page.waitForFunction(() => /Can.t reach the hub/.test(document.querySelector("#agents")?.innerText || ""), null, { timeout: 9000 });
      const t = await textOf(page, "#agents");
      if (!/did not answer|No answer in 10 seconds/.test(t)) throw new Error("no reason under Can't reach: " + JSON.stringify(t));
      if (!(await page.locator("#agents button.src-retry").isVisible())) throw new Error("no Try again");
      await noRaw(page);
      await page.screenshot({ path: `${out}/offline-held-10s-${tag}.png` });
    } finally { await browser.close(); }
  });

  await check(`${tag}: a link to an agent waits on "Opening …", not the empty list`, async () => {
    const { browser, page } = await open();
    try {
      await hold(page);
      await page.goto(base + "/#/agent/uitest/w9:p1");
      await sleep(1500);
      const t = await textOf(page, "#agents");
      if (/No agents running/.test(t)) throw new Error("empty list shown");
      if (!/Opening w9:p1/.test(t)) throw new Error("no 'Opening w9:p1…': " + JSON.stringify(t));
    } finally { await browser.close(); }
  });

  if (desk) {
    await check(`${tag}: held To do page is not "Nothing is waiting on you"`, async () => {
      const { browser, page } = await open();
      try {
        await hold(page);
        await page.goto(base + "/#/todo");
        await sleep(2000);
        const t = await textOf(page, "#pt-body");
        if (/Nothing is waiting on you/.test(t)) throw new Error("To do says nothing before the hub answered");
        if (!(await page.locator("#pt-body .src-wait").count())) throw new Error("no skeleton: " + JSON.stringify(t));
      } finally { await browser.close(); }
    });
  }

  // 2. A dead link (reset): every screen says it cannot reach the hub, with Try again, and none is stuck on "Loading…".
  await check(`${tag}: reset link: list, Status, Peers, Releases, Pages, Archived say "Can't reach the hub"`, async () => {
    const { browser, page } = await open();
    try {
      await page.goto(base + "/");
      await page.waitForFunction(() => typeof agentsSrc !== "undefined");
      // The app itself loaded; from here on the hub cannot be reached. Reload the data as a cold start would.
      await reset(page);
      await page.reload();
      await page.waitForFunction(() => /Can.t reach the hub/.test(document.querySelector("#agents")?.innerText || ""), null, { timeout: 5000 });
      await noRaw(page);
      // Status, To do and Archived wait on the ledger, which is read after the agents: their wait runs out at 10 s.
      await page.waitForTimeout(Math.max(0, 10500 - (await page.evaluate(() => Date.now() - ledgerSrc.since))));
      const screens = [["#/status", desk ? "#ps-body" : "#ss-body"], ["#/peers", "#pr-items"], [desk ? "#/peers/~releases" : "#/peers/~releases", "#pr-releases"],
        ["#/pages", "#pgs-items"], ["#/archived", "#ar-list"]];
      for (const [hash, sel] of screens) {
        await page.evaluate((h) => { location.hash = h; }, hash);
        try {
          await page.waitForFunction((s) => [...document.querySelectorAll(s)].some((e) => e.offsetParent !== null && /Can.t reach the hub/.test(e.innerText)), sel, { timeout: 3000 });
        } catch { throw new Error(`${hash}: no "Can't reach the hub" in ${sel}: ${JSON.stringify((await textOf(page, sel)).slice(0, 200))}`); }
        const t = await textOf(page, sel);
        if (/Loading…/.test(t)) throw new Error(`${hash}: still "Loading…"`);
        await noRaw(page);
        if (hash === "#/status" || hash === "#/pages") await page.screenshot({ path: `${out}/offline-reset-${hash.slice(2)}-${tag}.png` });
      }
      if (desk) {
        await page.evaluate(() => { location.hash = "#/todo"; });
        await page.waitForFunction(() => /Can.t reach the hub/.test(document.querySelector("#pt-body")?.innerText || ""), null, { timeout: 3000 });
        if (/Nothing is waiting/.test(await textOf(page, "#pt-body"))) throw new Error("To do says nothing");
      } else {
        await page.evaluate(() => { location.hash = ""; });
        await page.waitForFunction(() => /Can.t reach the hub/.test(document.querySelector("#todo")?.innerText || ""), null, { timeout: 3000 });
      }
    } finally { await browser.close(); }
  });

  // 3. Try again recovers once the hub answers.
  await check(`${tag}: Try again recovers the list when the hub is back`, async () => {
    const { browser, page } = await open();
    try {
      await reset(page);
      await page.goto(base + "/");
      await page.waitForSelector("#agents button.src-retry", { timeout: 5000 });
      await page.unroute(API);
      await page.click("#agents button.src-retry");
      await page.waitForSelector("#agents .agent[data-id]", { timeout: 8000 });
      if (await page.locator("#agents .src-down").count()) throw new Error("still says it cannot reach the hub");
    } finally { await browser.close(); }
  });

  await check(`${tag}: Try again recovers Pages`, async () => {
    const { browser, page } = await open();
    try {
      await page.goto(base + "/");
      await page.waitForSelector("#agents .agent[data-id]", { timeout: 8000 });
      await page.route(/\/api\/pages(\?|$)/, (r) => r.abort("connectionreset"));
      await page.evaluate(() => { location.hash = "#/pages"; });
      await page.waitForSelector("#pgs-items button.src-retry", { timeout: 5000 });
      await noRaw(page);
      await page.unroute(/\/api\/pages(\?|$)/);
      await page.click("#pgs-items button.src-retry");
      await page.waitForFunction(() => pgs.loaded && !document.querySelector("#pgs-items .src-down"), null, { timeout: 8000 });
    } finally { await browser.close(); }
  });

  // 4. A failed poll keeps what was loaded.
  await check(`${tag}: a failed poll keeps the loaded list (agents and Pages)`, async () => {
    const { browser, page } = await open();
    try {
      await page.route(/\/api\/pages(\?|$)/, (r) => r.fulfill({ json: { pages: [{ id: "pg_0000000001", title: "Kept page", by: "w", host: "uitest", version: 1, created_at: new Date().toISOString(), updated_at: new Date().toISOString() }] } }));
      await page.goto(base + "/#/pages");
      await page.waitForSelector("#agents .agent[data-id]", { timeout: 8000 });
      await page.waitForFunction(() => /Kept page/.test(document.body.innerText), null, { timeout: 8000 });
      const n = await page.locator("#agents .agent[data-id]").count();
      await page.unroute(/\/api\/pages(\?|$)/);
      await reset(page);
      await page.evaluate(() => pagesLoad());
      await page.evaluate(() => refreshAgents().catch(agentsFailed));
      await page.waitForTimeout(1500); // several test-mode polls fail meanwhile
      if (!/Kept page/.test(await bodyText(page))) throw new Error("a failed poll wiped the Pages list");
      if (await page.locator(".src-down").count()) throw new Error("a failed poll replaced a loaded list with Can't reach");
      const m = await page.locator("#agents .agent[data-id]").count();
      if (m !== n) throw new Error(`agents list changed from ${n} to ${m} rows`);
      await noRaw(page);
    } finally { await browser.close(); }
  });

  // 5. The real empty states, when the hub truly answers "none".
  await check(`${tag}: the hub's "none" still shows the real empty states`, async () => {
    const { browser, page } = await open();
    try {
      await page.route(/\/api\/agents$/, (r) => r.fulfill({ json: NO_AGENTS }));
      await page.route(/\/api\/events$/, (r) => r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream" }, body: `event: agents\ndata: ${JSON.stringify(NO_AGENTS)}\n\n` }));
      await page.route(/\/api\/ledger$/, (r) => r.fulfill({ json: EMPTY_LEDGER }));
      await page.route(/\/api\/todos$/, (r) => r.fulfill({ json: NO_TODOS }));
      await page.route(/\/api\/pages(\?|$)/, (r) => r.fulfill({ json: { pages: [] } }));
      await page.goto(base + "/" + (desk ? "#/todo" : ""));
      await page.waitForFunction(() => /No agents running/.test(document.querySelector("#agents")?.innerText || ""), null, { timeout: 5000 });
      if (desk) await page.waitForFunction(() => /Nothing is waiting on you/.test(document.querySelector("#pt-body")?.innerText || ""), null, { timeout: 5000 });
      else {
        await page.waitForTimeout(800);
        if (await page.locator("#todo").isVisible()) throw new Error("the phone's To do card shows with no to-dos");
      }
      await page.evaluate(() => { location.hash = "#/pages"; });
      await page.waitForFunction(() => /No pages yet/.test(document.body.innerText), null, { timeout: 5000 });
      if (await page.locator(".src-wait, .src-down").count()) throw new Error("a waiting state is left on screen");
    } finally { await browser.close(); }
  });

  // 6. The side panel: Doc is not "No document yet" while the hub has not answered for it.
  await check(`${tag}: side panel Doc and Diff wait, not "No document yet" / "No changes yet"`, async () => {
    const { browser, page } = await open();
    try {
      await page.goto(base + "/");
      await page.waitForSelector("#agents .agent[data-id]", { timeout: 8000 });
      await page.route(/\/(file|diff)(\?|$)/, async (r) => { await sleep(20000); r.continue().catch(() => {}); });
      await page.route(/\/panel\/events/, async (r) => { await sleep(20000); r.continue().catch(() => {}); });
      const id = await page.evaluate(() => state.agents[0] && agentId(state.agents[0]));
      await page.evaluate((x) => { localStorage.setItem("cb.sp." + x, JSON.stringify({ open: true, tab: "doc" })); openDetail(x); }, id);
      await page.waitForSelector("#sp-body", { state: "visible", timeout: 5000 });
      await sleep(1200);
      let t = await textOf(page, "#sp-body");
      if (/No document yet/.test(t)) throw new Error("Doc says 'No document yet' before the hub answered");
      if (!(await page.locator("#sp-body .src-wait").count())) throw new Error("no skeleton in Doc: " + JSON.stringify(t));
      await page.click("#sp-tab-diff");
      await sleep(600);
      t = await textOf(page, "#sp-body");
      if (/No changes yet/.test(t)) throw new Error("Diff says 'No changes yet' before the hub answered");
      if (!(await page.locator("#sp-body .src-wait").count())) throw new Error("no skeleton in Diff: " + JSON.stringify(t));
      await page.click("#sp-tab-doc");
      await page.waitForFunction(() => /Can.t reach the hub/.test(document.querySelector("#sp-body")?.innerText || ""), null, { timeout: 11000 });
      await page.screenshot({ path: `${out}/offline-sidepanel-${tag}.png` });
    } finally { await browser.close(); }
  });
}

for (const w of [390, 1280]) await phase(w);
const bad = results.filter((r) => !r.ok).length;
console.log(`offline: ${results.length - bad} passed, ${bad} failed`);
process.exit(bad ? 1 : 0);
