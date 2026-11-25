// Honest loading and empty states (issue #136, slice 1): on a slow or dead link no screen claims "nothing" before the hub
// has said so. While /api/* is held, the screens show skeleton rows ("Reaching the hub…"), never "No agents running" or
// "Nothing is waiting on you"; after 10 s, or at once when the connection is reset, "Can't reach the hub" with Try again;
// Try again recovers when the hub is back; a failed poll keeps a list already loaded; no raw "Failed to fetch"; and the
// real empty states still show when the hub truly answers "none". Phone (390) and desktop (1280).
// Slice 2 (phase2): the offline strip ("Offline · list from 12 min ago · Retry"), its age, Retry, its folding away when
// the link is back, old rows marked (and live ones not), the side panel bar, and Usage's waiting and failed states.
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
        // The phone's To do card says it is waiting (it was hidden), without repeating the list's "Reaching the hub…".
        if (!(await page.locator("#todo .src-wait").isVisible())) throw new Error("the phone's To do card is hidden while it waits");
        const n = ((await bodyText(page)).match(/Reaching the hub/g) || []).length;
        if (n !== 1) throw new Error(`"Reaching the hub…" shows ${n} times`);
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
        // The phone's To do card sits above the list, which says it once for both: the card shows a still row, no words.
        await page.evaluate(() => { location.hash = ""; });
        await page.waitForSelector('#todo [data-src-state="down"]', { timeout: 3000 });
        if (/Can.t reach the hub|Reaching the hub/.test(await textOf(page, "#todo"))) throw new Error("the To do card repeats the list's words");
        if (!/Can.t reach the hub/.test(await textOf(page, "#agents"))) throw new Error("the list does not say it");
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

// Slice 2: the offline strip and old rows. Rows from an answer that is not live lose their colour and their times freeze;
// one strip under the header says why and how old they are, with Retry; it clears when the live connection is back.
const stripText = (page) => page.evaluate(() => { const e = document.getElementById("hub-strip"); return e && !e.hidden ? e.innerText.replace(/\s+/g, " ").trim() : ""; });
const stripKind = (page) => page.evaluate(() => { const e = document.getElementById("hub-strip"); return e && !e.hidden ? e.dataset.kind : ""; });
// rowsLive: the agents list is not marked old, its dots keep their colour and no time is frozen.
async function rowState(page) {
  return page.evaluate(() => {
    const list = document.getElementById("agents");
    const dot = list.querySelector(".agent[data-id] .dot.idle, .agent[data-id] .dot.blocked, .agent[data-id] .dot.done");
    const bg = dot ? getComputedStyle(dot).backgroundColor : "";
    const agos = [...list.querySelectorAll(".agent[data-id] .ago")].map((e) => e.textContent);
    return { stale: list.classList.contains("stale"), hollow: bg === "rgba(0, 0, 0, 0)" || bg === "transparent", frozen: agos.filter((t) => /^at /.test(t)).length, agos: agos.length };
  });
}
async function waitLive(page) {
  await page.waitForSelector("#agents .agent[data-id]", { timeout: 8000 });
  await page.waitForFunction(() => document.getElementById("conn-list").classList.contains("ok"), null, { timeout: 8000 });
}
// cut: the live connection drops and cannot come back (every /api/ request is reset, and the stream reconnects into that).
async function cut(page) { await reset(page); await page.evaluate(() => connectEvents()); }

async function phase2(width) {
  const desk = width > 500, tag = desk ? "desktop" : "phone";
  const open = async (clock) => {
    const { browser, page } = await launch({ width, height: desk ? 800 : 844, dark });
    if (clock) await page.clock.install();
    return { browser, page };
  };

  await check(`${tag}: offline: the strip says so, with the list's age and Retry; rows go plain and their times freeze`, async () => {
    const { browser, page } = await open(true);
    try {
      await page.goto(base + "/");
      await waitLive(page);
      let r = await rowState(page);
      if (r.stale || r.hollow || r.frozen) throw new Error("live rows are marked old: " + JSON.stringify(r));
      if (await stripText(page)) throw new Error("a strip while live: " + (await stripText(page)));
      await page.context().setOffline(true);
      await page.waitForFunction(() => !document.getElementById("hub-strip")?.hidden && document.getElementById("hub-strip")?.dataset.kind === "offline", null, { timeout: 3000 });
      const t = await stripText(page);
      if (!/^Offline · list from just now Retry$/.test(t)) throw new Error("strip: " + JSON.stringify(t));
      // It sits right under the header of the screen in front.
      const under = await page.evaluate(() => { const e = document.getElementById("hub-strip"); return e.previousElementSibling && e.previousElementSibling.tagName; });
      if (under !== "HEADER") throw new Error("the strip is not under a header: after " + under);
      if (!(await page.evaluate(() => document.getElementById("conn-list").getAttribute("aria-label"))).match(/Offline/)) throw new Error("the dot's label does not say Offline");
      r = await rowState(page);
      if (!r.stale || !r.hollow) throw new Error("old rows keep their live look: " + JSON.stringify(r));
      if (r.agos && r.frozen !== r.agos) throw new Error(`times still count: ${r.frozen} of ${r.agos} frozen`);
      await page.screenshot({ path: `${out}/offline-strip-${tag}.png` });
      // The age counts.
      await page.clock.fastForward("12:00");
      await page.waitForFunction(() => /list from 12 min ago/.test(document.getElementById("hub-strip")?.innerText || ""), null, { timeout: 3000 });
      await page.clock.fastForward("02:00");
      await page.waitForFunction(() => /list from 14 min ago/.test(document.getElementById("hub-strip")?.innerText || ""), null, { timeout: 3000 });
      await page.screenshot({ path: `${out}/offline-strip-12min-${tag}.png` });
      // Back online: "Updated", then no strip and no old marking.
      await page.context().setOffline(false);
      await page.waitForFunction(() => document.getElementById("hub-strip")?.dataset.kind === "back" || document.getElementById("hub-strip")?.hidden, null, { timeout: 5000 });
      const k = await stripKind(page);
      if (k && k !== "back") throw new Error("strip after the link came back: " + k);
      await page.clock.fastForward("00:02");
      await page.waitForFunction(() => document.getElementById("hub-strip")?.hidden, null, { timeout: 5000 });
      r = await rowState(page);
      if (r.stale || r.hollow || r.frozen) throw new Error("rows still marked old: " + JSON.stringify(r));
    } finally { await browser.close(); }
  });

  await check(`${tag}: a blip under 8 s shows no strip; a longer loss says "Can't reach the hub", and Retry brings it back`, async () => {
    const { browser, page } = await open();
    try {
      await page.goto(base + "/");
      await waitLive(page);
      await cut(page);
      await sleep(3000);
      if (await stripText(page)) throw new Error("a strip 3 s into a drop: " + (await stripText(page)));
      if ((await rowState(page)).stale) throw new Error("rows marked old during a blip");
      await page.waitForFunction(() => document.getElementById("hub-strip")?.dataset.kind === "down", null, { timeout: 7000 });
      const t = await stripText(page);
      if (!/^Can.t reach the hub · list from just now Retry$/.test(t)) throw new Error("strip: " + JSON.stringify(t));
      if (!(await rowState(page)).stale) throw new Error("rows not marked old");
      // Retry while the hub is still out: it tries, and the strip stays.
      await page.click("#hub-strip .hs-retry");
      if (!/Trying/.test(await stripText(page))) throw new Error("Retry does not say it is trying: " + (await stripText(page)));
      await sleep(2500);
      if ((await stripKind(page)) !== "down") throw new Error("the strip left while the hub is still out: " + (await stripKind(page)));
      // The hub is back; Retry reconnects at once (no backoff wait), and the strip folds away.
      await page.unroute(API);
      await page.click("#hub-strip .hs-retry");
      await page.waitForFunction(() => document.getElementById("hub-strip")?.hidden, null, { timeout: 6000 });
      if ((await rowState(page)).stale) throw new Error("rows still marked old");
    } finally { await browser.close(); }
  });

  await check(`${tag}: the side panel bar says "live" only while its body is live`, async () => {
    const { browser, page } = await open();
    try {
      await page.goto(base + "/");
      await waitLive(page);
      const id = await page.evaluate(() => state.agents[0] && agentId(state.agents[0]));
      await page.evaluate((x) => { localStorage.setItem("cb.sp." + x, JSON.stringify({ open: true, tab: "pages" })); openDetail(x); }, id);
      await page.waitForFunction(() => /^live/.test(document.getElementById("sp-live")?.textContent || ""), null, { timeout: 8000 });
      await page.context().setOffline(true);
      await page.waitForFunction(() => { const l = document.getElementById("sp-live"); return l && l.textContent === "offline" && l.classList.contains("off"); }, null, { timeout: 3000 });
      // The strip is under the header in front (the agent's), with the panel's bar saying the same.
      if ((await stripKind(page)) !== "offline") throw new Error("no strip beside the panel: " + (await stripKind(page)));
      await page.screenshot({ path: `${out}/offline-sidepanel-bar-${tag}.png` });
      await page.context().setOffline(false);
      await page.waitForFunction(() => /^live/.test(document.getElementById("sp-live")?.textContent || ""), null, { timeout: 8000 });
      // A Doc the hub never answers: after 10 s the body says "Can't reach the hub", and the bar does not say "live".
      await page.route(/\/(file)(\?|$)/, async (r) => { await sleep(20000); r.continue().catch(() => {}); });
      await page.route(/\/panel\/events/, async (r) => { await sleep(20000); r.continue().catch(() => {}); });
      const id2 = await page.evaluate(() => state.agents[1] && agentId(state.agents[1]));
      await page.evaluate((x) => { localStorage.setItem("cb.sp." + x, JSON.stringify({ open: true, tab: "doc" })); openDetail(x); }, id2);
      await page.waitForFunction(() => /Can.t reach the hub/.test(document.getElementById("sp-body")?.innerText || ""), null, { timeout: 11000 });
      const bar = await page.evaluate(() => { const l = document.getElementById("sp-live"); return l && [l.textContent, l.classList.contains("off")]; });
      if (!bar || /^live/.test(bar[0]) || !bar[1]) throw new Error("the bar says " + JSON.stringify(bar) + " while the body can't reach the hub");
    } finally { await browser.close(); }
  });

  await check(`${tag}: other screens: the strip names their list; a failed read after an answer says "Not updating"`, async () => {
    const { browser, page } = await open();
    try {
      await page.route(/\/api\/pages(\?|$)/, (r) => r.fulfill({ json: { pages: [{ id: "pg_0000000001", title: "Kept page", by: "w", host: "uitest", version: 1, created_at: new Date().toISOString(), updated_at: new Date().toISOString() }] } }));
      await page.goto(base + "/#/pages");
      await waitLive(page);
      await page.waitForFunction(() => /Kept page/.test(document.body.innerText), null, { timeout: 8000 });
      if (await stripText(page)) throw new Error("a strip while live");
      await page.unroute(/\/api\/pages(\?|$)/);
      await page.route(/\/api\/pages(\?|$)/, (r) => r.abort("connectionreset"));
      await page.evaluate(() => pagesLoad());
      await page.waitForFunction(() => document.getElementById("hub-strip")?.dataset.kind === "stuck", null, { timeout: 3000 });
      const t = await stripText(page);
      if (!/^Not updating · pages from just now Retry$/.test(t)) throw new Error("strip: " + JSON.stringify(t));
      if (!(await page.evaluate(() => document.getElementById("pgs-items").classList.contains("stale")))) throw new Error("Pages not marked old");
      if (await page.evaluate(() => document.getElementById("agents").classList.contains("stale"))) throw new Error("the live agents list is marked old");
      await page.unroute(/\/api\/pages(\?|$)/);
      await page.click("#hub-strip .hs-retry");
      await page.waitForFunction(() => document.getElementById("hub-strip")?.hidden, null, { timeout: 6000 });
      if (await page.evaluate(() => document.getElementById("pgs-items").classList.contains("stale"))) throw new Error("Pages still marked old");
    } finally { await browser.close(); }
  });

  await check(`${tag}: Usage waits with skeleton rows, then "Can't reach the hub" with Try again, never "Loading…" or a raw error`, async () => {
    const { browser, page } = await open();
    try {
      await page.goto(base + "/");
      await waitLive(page);
      await page.route(/\/api\/usage/, async (r) => { await sleep(20000); r.continue().catch(() => {}); });
      await page.evaluate(() => { location.hash = "#/usage"; });
      await sleep(1200);
      let t = await textOf(page, "#us-view");
      if (/Loading…/.test(t)) throw new Error("Usage says Loading…");
      if (!(await page.locator("#us-view .src-wait .skel-row").count())) throw new Error("no skeleton: " + JSON.stringify(t));
      await page.unroute(/\/api\/usage/);
      await page.route(/\/api\/usage/, (r) => r.abort("connectionreset"));
      await page.evaluate(() => { us.seq++; usageLoad(); });
      await page.waitForFunction(() => /Can.t reach the hub/.test(document.getElementById("us-view")?.innerText || ""), null, { timeout: 5000 });
      await noRaw(page);
      await page.screenshot({ path: `${out}/offline-usage-${tag}.png` });
      await page.unroute(/\/api\/usage/);
      await page.click("#us-view button.src-retry");
      await page.waitForFunction(() => us.status !== "loading" && !document.querySelector("#us-view .src-wait, #us-view .src-down"), null, { timeout: 10000 });
      // A failed read after the numbers came keeps them.
      await page.route(/\/api\/usage/, (r) => r.abort("connectionreset"));
      const before = await textOf(page, "#us-view");
      await page.evaluate(() => usageLoad());
      await page.waitForFunction(() => usageSrc.err, null, { timeout: 5000 });
      if ((await textOf(page, "#us-view")).length < before.length / 2 || await page.locator("#us-view .src-down").count()) throw new Error("a failed read wiped Usage");
      if ((await stripKind(page)) !== "stuck") throw new Error("no 'Not updating' strip on Usage: " + (await stripKind(page)));
    } finally { await browser.close(); }
  });
}

for (const w of [390, 1280]) await phase(w);
for (const w of [390, 1280]) await phase2(w);
const bad = results.filter((r) => !r.ok).length;
console.log(`offline: ${results.length - bad} passed, ${bad} failed`);
process.exit(bad ? 1 : 0);
