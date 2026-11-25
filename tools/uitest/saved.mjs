// Saved state (issue #136, slice 4): the last good answer of each list is kept on the device (IndexedDB), so opening
// the app shows it at once, marked old, until the hub's fresh answer replaces it. Phone (390) and desktop (1280):
// - load, then reload with /api/* held: the saved lists (agents, To do, Status, Pages, hosts, releases, archived,
//   usage) show at once, marked old ("Updating… · list from …"), then go live when the hub answers;
// - /api/version naming another viewer, or a 401, empties the store and takes the saved rows away;
// - Settings › Forget saved data empties it;
// - the caps: a list over 512 KB (3 MB of agents) is not kept, the store stays under 2 MB (the board goes first, then
//   the oldest record), a record older than 7 days is deleted and not shown;
// - IndexedDB missing, throwing, never answering, or out of quota: the app still works (nothing is saved).
//
//   node saved.mjs BASE OUT_DIR       (env: SCHEME)
// The slow and dead links are Playwright routes on /api/ (they catch the event stream too); the hub is never changed
// except for one to-do filed through its API.
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  if (process.env.ONLY && !new RegExp(process.env.ONLY).test(name)) return; // ONLY=regex: just those checks, for a quick look
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const API = /\/api\//;
// gate: every /api/ request waits until open() is called (a stalled link that comes back).
function gate(page) {
  let open;
  const until = new Promise((r) => { open = r; });
  const ready = page.route(API, async (r) => { await until; r.continue().catch(() => {}); });
  return { ready, open: async () => { open(); await page.unroute(API); } };
}
// stored: what the store holds, by name ({size, at, viewer}); null when it cannot be read.
const stored = (page) => page.evaluate(() => new Promise((res) => {
  try {
    const rq = indexedDB.open("comebye-cache");
    rq.onerror = () => res(null);
    rq.onsuccess = () => {
      const db = rq.result;
      if (!db.objectStoreNames.contains("src")) { db.close(); res({}); return; }
      const g = db.transaction("src").objectStore("src").getAll();
      g.onsuccess = () => { const o = {}; for (const r of g.result) o[r.name] = { size: r.size, at: r.at, viewer: r.viewer }; db.close(); res(o); };
      g.onerror = () => { db.close(); res(null); };
    };
  } catch { res(null); }
}));
const total = (o) => Object.values(o || {}).reduce((a, r) => a + r.size, 0);
async function waitStored(page, names, ms = 15000) {
  const end = Date.now() + ms;
  let got = {};
  for (;;) {
    got = (await stored(page)) || {};
    if (names.every((n) => got[n])) return got;
    if (Date.now() > end) throw new Error(`not saved: ${names.filter((n) => !got[n]).join(", ")} (store has ${Object.keys(got).join(", ") || "nothing"})`);
    await sleep(250);
  }
}
const stripText = (page) => page.evaluate(() => { const e = document.querySelector("#hub-strip"); return e && !e.hidden ? e.innerText.replace(/\s+/g, " ").trim() : ""; });
const rows = (page) => page.locator("#agents .agent").count();
const J = { "Content-Type": "application/json" };
let todoQ = "";
async function fileTodo() {
  todoQ = "Saved on the device: keep this list? " + Date.now();
  const r = await fetch(base + "/api/todos", { method: "POST", headers: J, body: JSON.stringify({ question: todoQ, kind: "decision", no_wait: true, options: [{ label: "Yes" }, { label: "No" }] }) });
  const d = await r.json();
  if (!d.todo || !d.todo.id) throw new Error("could not file a to-do: " + JSON.stringify(d));
}
// visitAll opens each screen once, so each list is read (and saved).
const SCREENS = [["#/status", "ledger"], ["#/pages", "pages"], ["#/peers", "peers"], ["#/peers/~releases", "releases"], ["#/archived", "archived"], ["#/usage", "usage"]];
async function visitAll(page) {
  for (const [h] of SCREENS) { await page.evaluate((x) => { location.hash = x; }, h); await sleep(700); }
  await page.evaluate(() => { location.hash = ""; });
}

async function phase(width) {
  const desk = width > 500, tag = desk ? "desktop" : "phone";
  const open = async (init) => {
    const { browser, page } = await launch({ width, height: desk ? 800 : 844, dark });
    const errs = [];
    page.on("pageerror", (e) => errs.push(e.message));
    if (init) await page.addInitScript(init);
    return { browser, page, errs };
  };
  // load: open the app and wait until the agents (and whatever else is named) are saved.
  const load = async (page, names = ["agents"]) => {
    await page.goto(base + "/");
    await page.waitForFunction(() => typeof agentsSrc !== "undefined" && agentsSrc.at > 0 && !agentsSrc.saved, null, { timeout: 10000 });
    return waitStored(page, names);
  };

  await check(`${tag}: reload with /api/* held shows the saved lists at once, marked old, then they go live`, async () => {
    const { browser, page, errs } = await open();
    try {
      // The fixture hub has no Pages: its list answers one page here (the list only is saved, never a body).
      const at = new Date(Date.now() - 3600000).toISOString();
      const pagesList = { pages: [{ id: "pg_5a7ed0c0de", title: "Saved list check", kind: "markdown", by: "saved.mjs", agent: "uitest/w1:p1", host: "uitest", created_at: at, updated_at: at, version: 1, size: 120, versions: [{ version: 1, size: 120, created_at: at }] }] };
      await page.route(/\/api\/pages$/, (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(pagesList) }));
      await page.goto(base + "/");
      await page.waitForFunction(() => typeof agentsSrc !== "undefined" && agentsSrc.at > 0);
      await visitAll(page);
      await page.unroute(/\/api\/pages$/);
      // Every list this hub has something in (an empty one is never saved): the ledger and peers depend on the hub.
      const has = await page.evaluate(() => Promise.all([fetch("/api/ledger").then((r) => r.json()), fetch("/api/peers").then((r) => (r.ok ? r.json() : {}))])
        .then(([l, p]) => ({ ledger: !!((l.entries || []).length || (l.marks || []).length), peers: !!(p.peers || []).length })));
      const names = ["agents", "todos", "board", "pages", "releases", "usage"].concat(has.ledger ? ["ledger"] : [], has.peers ? ["peers"] : []);
      const got = await waitStored(page, names);
      const viewer = await page.evaluate(() => fetch("/api/version").then((r) => r.json()).then((d) => d.viewer));
      if (!/^[0-9a-f]{16}$/.test(viewer || "")) throw new Error("/api/version names no viewer: " + viewer);
      for (const [n, r] of Object.entries(got)) if (r.viewer !== viewer) throw new Error(`${n} saved for ${r.viewer}, not ${viewer}`);
      if (total(got) > 2 << 20) throw new Error("over 2 MB: " + total(got));
      // No transcripts or page bodies: only the lists.
      const extra = Object.keys(got).filter((n) => !["agents", "todos", "ledger", "board", "pages", "peers", "releases", "archived", "usage"].includes(n));
      if (extra.length) throw new Error("saved something else: " + extra);
      const g = gate(page);
      await g.ready;
      const t0 = Date.now();
      await page.reload();
      await page.waitForFunction(() => document.querySelectorAll("#agents .agent").length > 0, null, { timeout: 3000 });
      const ms = Date.now() - t0;
      await page.waitForFunction(() => { const e = document.querySelector("#hub-strip"); return e && !e.hidden; }, null, { timeout: 2000 });
      const s = await stripText(page);
      if (!/Updating… · list from just now/.test(s)) throw new Error("strip: " + JSON.stringify(s));
      if (!(await page.evaluate(() => document.querySelector("#agents").classList.contains("stale")))) throw new Error("the saved rows are not marked old");
      // The To do list (the phone's card, the desktop's page) shows the saved question.
      if (desk) await page.evaluate(() => { location.hash = "#/todo"; });
      await page.waitForFunction((q) => document.body.innerText.includes(q), todoQ, { timeout: 3000 }).catch(() => { throw new Error("the saved to-do is not shown"); });
      await page.screenshot({ path: `${out}/saved-held-${tag}.png` });
      // Every other screen draws its saved list too, marked old, with no skeleton and no "Can't reach".
      // (Status draws from the ledger: a hub whose ledger is empty has none saved, and Status waits for the hub.)
      const want = [...(has.ledger ? [["#/status", desk ? "#ps-body" : "#ss-body"]] : []), ["#/pages", "#pgs-items"], ["#/peers", "#pr-items"], ["#/peers/~releases", "#pr-releases"], ["#/usage", "#us-view"]];
      for (const [h, sel] of want) {
        await page.evaluate((x) => { location.hash = x; }, h);
        try {
          await page.waitForFunction((q) => { const e = [...document.querySelectorAll(q)].find((x) => x.offsetParent !== null); return e && e.innerText.trim().length > 20 && !e.querySelector(".src-wait, .src-down"); }, sel, { timeout: 3000 });
        } catch { throw new Error(`${h}: no saved list in ${sel}: ${JSON.stringify(await page.evaluate((q) => document.querySelector(q)?.innerText.slice(0, 160), sel))}`); }
        const st = await stripText(page);
        if (!/^Updating…/.test(st)) throw new Error(`${h}: strip ${JSON.stringify(st)}`);
        if (h === "#/status" || h === "#/pages") await page.screenshot({ path: `${out}/saved-held-${h.slice(2)}-${tag}.png` });
      }
      await page.evaluate(() => { location.hash = ""; });
      if (ms > 2000) throw new Error(`saved rows took ${ms} ms`);
      // The hub answers: fresh lists replace them, "Updated", then no strip and no marking.
      await g.open();
      await page.waitForFunction(() => !agentsSrc.saved && !document.querySelector("#agents").classList.contains("stale"), null, { timeout: 25000 });
      await page.waitForFunction(() => { const e = document.querySelector("#hub-strip"); return !e || e.hidden; }, null, { timeout: 5000 });
      if (errs.length) throw new Error("page errors: " + errs.join("; "));
    } finally { await browser.close(); }
  });

  await check(`${tag}: another viewer in /api/version empties the store and takes the saved rows away`, async () => {
    const { browser, page } = await open();
    try {
      await load(page);
      const v = await page.evaluate(() => fetch("/api/version").then((r) => r.json()));
      await page.route(API, (r) => {
        if (/\/api\/version$/.test(r.request().url())) return r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ version: v.version, viewer: "0123456789abcdef" }) });
        return r.abort("connectionreset");
      });
      await page.reload();
      await page.waitForFunction(() => /Can.t reach the hub/.test(document.querySelector("#agents")?.innerText || ""), null, { timeout: 5000 });
      if (await rows(page)) throw new Error("another viewer's rows are still shown");
      const got = await stored(page);
      if (got && Object.keys(got).length) throw new Error("the store still holds " + Object.keys(got));
    } finally { await browser.close(); }
  });

  await check(`${tag}: a 401 empties the store and takes the saved rows away`, async () => {
    const { browser, page } = await open();
    try {
      await load(page);
      await page.route(API, (r) => r.fulfill({ status: 401, contentType: "application/json", body: '{"message":"not allowed"}' }));
      await page.reload();
      await page.waitForFunction(() => state.unauthorized, null, { timeout: 5000 });
      await sleep(500);
      if (await rows(page)) throw new Error("saved rows shown to a login that is not allowed");
      const got = await stored(page);
      if (got && Object.keys(got).length) throw new Error("the store still holds " + Object.keys(got));
    } finally { await browser.close(); }
  });

  await check(`${tag}: Settings › Forget saved data empties the store`, async () => {
    const { browser, page } = await open();
    try {
      await load(page);
      await page.evaluate(() => { location.hash = "#/settings"; });
      await page.waitForSelector("#sx-forget", { state: "visible", timeout: 5000 });
      const before = await page.locator("#sx-view").innerText();
      if (!/Saved on this device[\s\S]*About \d+ KB is saved now/.test(before)) throw new Error("Settings does not say what is saved: " + before.slice(0, 300));
      if (!/Messages and page bodies are never saved/.test(before)) throw new Error("no word on what is never saved");
      await page.locator("#sx-forget").click();
      await page.waitForFunction(() => /Forgotten/.test(document.querySelector("#sx-view").innerText), null, { timeout: 3000 });
      const got = await stored(page);
      if (got && Object.keys(got).length) throw new Error("the store still holds " + Object.keys(got));
      if (!/Nothing is saved now/.test(await page.locator("#sx-view").innerText())) throw new Error("Settings still counts saved data");
      await page.screenshot({ path: `${out}/saved-forget-${tag}.png` });
    } finally { await browser.close(); }
  });

  await check(`${tag}: caps: 3 MB of agents is not kept, the store stays under 2 MB, a week-old record is not shown`, async () => {
    const { browser, page } = await open();
    try {
      const got = await load(page, ["agents", "todos"]);
      const viewer = got.agents.viewer;
      // Fill the store to 2.1 MB with records of lists this screen does not read again (data their screens refuse to
      // show), the board the newest of them.
      const fill = await page.evaluate((viewer) => new Promise((res) => {
        const pad = "x".repeat(540 * 1024), now = Date.now(), H = 3600000;
        const recs = [["board", now - H], ["peers", now - 5 * H], ["usage", now - 4 * H], ["pages", now - 2 * H]]
          .map(([name, at]) => { const json = JSON.stringify({ pad }); return { name, viewer, at, json, size: json.length }; });
        const rq = indexedDB.open("comebye-cache");
        rq.onsuccess = () => { const tx = rq.result.transaction("src", "readwrite"); for (const r of recs) tx.objectStore("src").put(r); tx.oncomplete = () => { rq.result.close(); res(recs.length); }; };
      }), viewer);
      if (fill !== 4) throw new Error("could not fill the store");
      // Reload: the app reads the filled store; its next write must bring it under 2 MB. The agents answer is 3 MB.
      const big = await page.evaluate(() => fetch("/api/agents").then((r) => r.json()));
      const a0 = big.agents[0];
      big.agents = Array.from({ length: 1500 }, (_, i) => ({ ...a0, id: a0.host + "/w" + (900 + i) + ":p1", pane_id: "w" + (900 + i) + ":p1", name: "big-" + i, cwd: "/home/u/" + "d".repeat(1900) }));
      const bigJson = JSON.stringify(big);
      if (bigJson.length < 3e6) throw new Error("the big answer is only " + bigJson.length);
      await page.route(/\/api\/events/, () => {}); // never answered: the list comes from /api/agents alone
      await page.route(/\/api\/agents$/, (r) => r.fulfill({ status: 200, contentType: "application/json", body: bigJson }));
      await page.reload();
      await page.waitForFunction(() => state.agents.length >= 1500 && !agentsSrc.saved, null, { timeout: 10000 });
      if ((await rows(page)) < 100) throw new Error("the big list is not drawn");
      await page.evaluate(() => { cacheMark("todos"); cacheFlush(true); }); // a write: the eviction runs
      let after = {};
      // The filled board is what must go; a fresh board answer saved again afterwards (a few KB) is fine.
      const filler = (o) => !!o.board && o.board.size >= 500 * 1024;
      for (let i = 0; i < 40; i++) { after = (await stored(page)) || {}; if (total(after) <= 2 << 20 && !filler(after)) break; await sleep(250); }
      if (total(after) > 2 << 20) throw new Error("store over 2 MB: " + total(after) + " " + JSON.stringify(Object.keys(after)));
      if (filler(after)) throw new Error("the board was not the first to go: " + JSON.stringify(after) + " app: " + await page.evaluate(() => JSON.stringify({ sizes: [...cache.sizes], viewer: cache.viewer, dirty: [...cache.dirty], off: cache.off, flushing: cache.flushing })));
      if (after.agents && after.agents.size > 512 * 1024) throw new Error("a 3 MB agents list was kept: " + after.agents.size);
      if (!after.todos) throw new Error("the to-dos went, not only the board: " + Object.keys(after));
      if (!after.peers) throw new Error("more went than needed: " + Object.keys(after));
      await page.unroute(/\/api\/agents$/);
      await page.unroute(/\/api\/events/);
      // 7 days: a week-old record is deleted, not shown.
      await page.goto(base + "/");
      await waitStored(page, ["agents"]);
      await page.evaluate(() => new Promise((res) => {
        const rq = indexedDB.open("comebye-cache");
        rq.onsuccess = () => {
          const st = rq.result.transaction("src", "readwrite").objectStore("src"), g = st.get("agents");
          g.onsuccess = () => { const r = g.result; r.at = Date.now() - 8 * 86400000; st.put(r).onsuccess = () => { rq.result.close(); res(); }; };
        };
      }));
      const g = gate(page);
      await g.ready;
      await page.reload();
      await sleep(1500);
      if (await rows(page)) throw new Error("a week-old list is shown");
      const left = await stored(page);
      if (left && left.agents) throw new Error("the week-old record was not deleted");
      await g.open();
    } finally { await browser.close(); }
  });

  const broken = {
    missing: () => { Object.defineProperty(window, "indexedDB", { value: undefined, configurable: true }); },
    throws: () => { IDBFactory.prototype.open = function () { throw new DOMException("The operation is insecure.", "SecurityError"); }; },
    hangs: () => { IDBFactory.prototype.open = function () { return { set onsuccess(f) {}, set onerror(f) {}, set onupgradeneeded(f) {} }; }; },
    quota: () => {
      IDBObjectStore.prototype.put = function () { throw new DOMException("Quota exceeded", "QuotaExceededError"); };
    },
  };
  for (const [kind, init] of Object.entries(broken)) {
    if (desk && kind !== "throws" && kind !== "hangs") continue; // the phone runs all four; the desktop two
    await check(`${tag}: IndexedDB ${kind}: the app still works`, async () => {
      const { browser, page, errs } = await open(init);
      try {
        await page.goto(base + "/");
        await page.waitForFunction(() => document.querySelectorAll("#agents .agent").length > 0 && agentsSrc.at > 0, null, { timeout: 10000 });
        await sleep(kind === "hangs" ? 3500 : 800);
        await page.evaluate(() => cacheFlush(true));
        // The live strip and the screens behave as before; Settings says nothing is saved (or what is).
        await page.evaluate(() => { location.hash = "#/status"; });
        await page.waitForFunction((d) => { const e = document.querySelector(d ? "#ps-body" : "#ss-body"); return e && e.innerText.trim().length > 20 && !e.querySelector(".src-wait"); }, desk, { timeout: 8000 });
        await page.evaluate(() => { location.hash = "#/settings"; });
        await page.waitForSelector("#sx-forget", { state: "visible", timeout: 5000 });
        const t = await page.locator("#sx-view").innerText();
        if (kind !== "quota" && !/does not let the app keep anything here/.test(t)) throw new Error("Settings does not say nothing can be saved: " + t.slice(0, 400));
        await page.locator("#sx-forget").click();
        await page.waitForFunction(() => /Forgotten/.test(document.querySelector("#sx-view").innerText), null, { timeout: 5000 });
        await page.evaluate(() => { location.hash = ""; });
        // A reload with the link held: skeleton rows, as without saved state; no page errors.
        const g = gate(page);
        await g.ready;
        await page.reload();
        await sleep(1200);
        if (await rows(page)) throw new Error("rows shown without a store");
        if (!(await page.locator("#agents .src-wait").count())) throw new Error("no skeleton rows");
        await g.open();
        await page.waitForFunction(() => document.querySelectorAll("#agents .agent").length > 0, null, { timeout: 25000 });
        if (errs.length) throw new Error("page errors: " + errs.join("; "));
      } finally { await browser.close(); }
    });
  }
}

await fileTodo();
for (const w of (process.env.WIDTHS || "390 1280").split(/\s+/).map(Number)) await phase(w);
const bad = results.filter((r) => !r.ok).length;
console.log(`saved: ${results.length - bad} passed, ${bad} failed`);
process.exit(bad ? 1 : 0);
