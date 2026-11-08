// Measures click -> first visible change for each input of the web app, with
// latency added to every /api call (except the live stream): the number the
// optimistic-UI work is about.
//
//   node tools/uitest/measure.mjs BASE [LATENCY_MS ...]      (default 0 300 1500)
//
// Run it against a dev server (tools/uitest/dev.sh start). Each input lists
// the moments that matter, e.g. "ack" (something shows the tap was taken) and
// "result" (the state the tap asked for is on screen), in ms after the click.
import { launch } from "./lib.mjs";

const [base, ...lat] = process.argv.slice(2);
const LATS = lat.length ? lat.map(Number) : [0, 300, 1500];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const post = (page, url, body) => page.evaluate(([u, b]) => fetch(u, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(b) }).then((r) => r.json()).catch(() => ({})), [url, body]);

const { browser, page } = await launch({ width: 1280, height: 800, dark: true });
let delay = 0;
await page.route(/\/api\//, async (r) => {
  if (!/\/api\/events/.test(r.request().url()) && delay) await sleep(delay);
  await r.continue().catch(() => {});
});

// timeIt clicks in the page and returns, per named snapshot expression, the ms
// until it first differs from its value before the click (null: not in 5s).
async function timeIt(click, snaps) {
  return page.evaluate(async ([click, snaps]) => {
    const fns = Object.fromEntries(Object.entries(snaps).map(([k, v]) => [k, new Function("return (" + v + ")")]));
    const before = Object.fromEntries(Object.entries(fns).map(([k, f]) => [k, f()]));
    const out = Object.fromEntries(Object.keys(fns).map((k) => [k, null]));
    const t0 = performance.now();
    new Function(click)();
    return await new Promise((res) => {
      const tick = () => {
        let open = 0;
        for (const k in fns) {
          if (out[k] === null) { if (fns[k]() !== before[k]) out[k] = Math.round(performance.now() - t0); else open++; }
        }
        if (!open || performance.now() - t0 > 5000) return res(out);
        requestAnimationFrame(tick);
      };
      tick();
    });
  }, [click, snaps]);
}
const noDis = (s) => `(${s}).replace(/ disabled=""/g, "")`;

const rows = {};
const rec = (name, L, m) => { (rows[name] ||= {})[L] = m; };
async function open(path) { await page.goto(base + path, { waitUntil: "load" }); await page.waitForSelector("#agents .agent"); }
const W6 = "/#/agent/" + encodeURIComponent("w6:p1");

for (const L of LATS) {
  delay = 0;
  // --- dialog option tap (Claude permission dialog on w6:p1) ---
  await open(W6);
  await page.waitForSelector("#answer button.opt", { timeout: 8000 });
  await sleep(500);
  delay = L;
  const A = `document.querySelector('#answer').innerHTML`;
  rec("dialog option tap", L, await timeIt(`document.querySelector('#answer button.opt:not(.cur):not(.cancel):not(.submit)').click()`, {
    anyChange: A, choiceShown: `document.querySelector('#answer').innerHTML.replace(/ disabled=""/g, "") + document.querySelector('#toast').className`,
  }));
  await sleep(L * 3 + 1200); delay = 0;

  // --- send prompt (chat view, alpha) ---
  await open("/#/agent/" + encodeURIComponent("w1:p1"));
  await page.waitForFunction(() => document.querySelector("#chat")?.innerText.includes("alpha says hi"));
  await page.locator("#prompt-text").click(); await page.keyboard.type("hello m" + L);
  delay = L;
  rec("send prompt", L, await timeIt(`document.querySelector('#btn-send').click()`, {
    bubble: `document.querySelectorAll('#chat .msg.pending').length`, boxCleared: `document.querySelector('#prompt-text').innerText`,
  }));
  await sleep(L * 3 + 3500); delay = 0;

  // --- to-do Done / Answer ---
  await open("/#/todo");
  const ids = [];
  for (const q of ["m-a " + L, "m-b " + L]) ids.push((await post(page, "/api/todos", { no_wait: true, question: q, kind: "decision" })).todo.id);
  await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
  const item = (id) => `#pt-body .td-item[data-id="${id}"]`;
  const row = (id) => `(document.querySelector('${item(id)}')||{outerHTML:'GONE'}).outerHTML`;
  const rowCls = (id) => `(document.querySelector('${item(id)}')||{className:'GONE'}).className + ((document.querySelector('${item(id)}')||{}).querySelector?.('.td-done')||{}).hidden`;
  await page.waitForSelector(item(ids[0]));
  await page.evaluate((id) => todoExpand(id), ids[0]);
  delay = L;
  rec("to-do Done", L, await timeIt(`document.querySelector('${item(ids[0])} .td-done').click()`, {
    anyChange: row(ids[0]), leavesOpenList: `!!document.querySelector('${item(ids[0])}.answered') + '|' + !document.querySelector('${item(ids[0])}')`,
    }));
  await sleep(L * 3 + 1200); delay = 0;
  await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
  await page.evaluate((id) => todoExpand(id), ids[1]);
  await page.locator(item(ids[1]) + " .td-text").fill("an answer");
  delay = L;
  rec("to-do Answer", L, await timeIt(`document.querySelector('${item(ids[1])} .td-answer-btn').click()`, {
    anyChange: row(ids[1]), leavesOpenList: `!!document.querySelector('${item(ids[1])}.answered') + '|' + !document.querySelector('${item(ids[1])}')`,
  }));
  await sleep(L * 3 + 1200); delay = 0;
  for (const i of ids) await post(page, `/api/todos/${i}/drop`, {});

  // --- unmark (w7 is marked) ---
  await open("/#/agent/" + encodeURIComponent("w7:p1"));
  await post(page, "/api/marks", { agent_id: "devbox/w7:p1", reason: "m" });
  await page.evaluate(() => { ledger.loadedAt = 0; return loadLedger(); });
  await page.waitForSelector("#d-markbar [data-mb=unmark]");
  delay = L;
  rec("unmark", L, await timeIt(`document.querySelector('#d-markbar [data-mb=unmark]').click()`, { barGone: `document.querySelector('#d-markbar').innerHTML` }));
  await sleep(L * 3 + 1200); delay = 0;

  // --- make coordinator (menu) ---
  await open("/#/agent/" + encodeURIComponent("w5:p1"));
  await post(page, "/api/roles", { agent_id: "devbox/w5:p1", role: "" });
  await page.evaluate(() => { ledger.loadedAt = 0; return loadLedger(); });
  await page.locator("#d-more").click(); delay = L;
  rec("make coordinator", L, await timeIt(`document.querySelector('#d-role').click()`, {
    menuCloses: `document.querySelector('#d-menu').hidden`, roleShown: `document.querySelector('#agents').innerHTML + document.querySelector('#d-role').textContent`,
  }));
  await sleep(L * 3 + 1200); delay = 0;
  await post(page, "/api/roles", { agent_id: "devbox/w5:p1", role: "" });

  // --- mark sheet submit ---
  await open("/#/agent/" + encodeURIComponent("w1:p1"));
  await page.locator("#d-more").click(); await page.locator("#d-mark").click();
  await page.waitForSelector("#mk", { state: "visible" });
  delay = L;
  rec("mark sheet Mark", L, await timeIt(`document.querySelector('#mk-go').click()`, {
    sheetBusy: `document.querySelector('#mk-status').textContent`, sheetCloses: `document.querySelector('#mk').hidden`, barShown: `document.querySelector('#d-markbar').hidden`,
  }));
  await sleep(L * 3 + 1200); delay = 0;
  await post(page, "/api/marks/clear", { agent_id: "devbox/w1:p1" });

  // --- queue cancel ---
  await open("/#/agent/" + encodeURIComponent("w3:p1"));
  await post(page, "/api/queue", { agent: "devbox/w3:p1", text: "queued m" + L });
  await page.evaluate(() => refreshQueued(true));
  await page.waitForSelector("#queued .q-cancel").catch(() => {});
  if (await page.locator("#queued .q-cancel").count()) {
    delay = L;
    rec("queue Cancel", L, await timeIt(`document.querySelector('#queued .q-cancel').click()`, {
      anyChange: `document.querySelector('#queued').innerHTML.replace(/ disabled=""/g, "")`,
    }));
    await sleep(L * 3 + 1200); delay = 0;
  }

  // --- local-only: theme ---
  await open("/");
  delay = L;
  rec("theme toggle", L, await timeIt(`document.querySelector('#f-theme').click()`, { changed: `document.documentElement.getAttribute('data-theme')` }));
  delay = 0;
}
await browser.close();
const f = (v) => (v === null ? "never" : String(v));
console.log("input (metric) | " + LATS.map((l) => `${l}ms`).join(" | "));
for (const [n, r] of Object.entries(rows)) {
  for (const k of Object.keys(Object.values(r)[0])) console.log(`${n} (${k}) | ` + LATS.map((l) => (r[l] ? f(r[l][k]) : "-")).join(" | "));
}
