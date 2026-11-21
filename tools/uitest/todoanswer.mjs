// Answering a hub-blocked to-do from the To do panel (internal/server/todoanswer.go): the hub files an
// item for an agent blocked at a dialog, and its option buttons answer the dialog in the pane.
// Then the hub's own relay (internal/server/relay.go): an answer to an item waiting on alpha is typed
// into alpha's pane, the closed item keeps a "delivered" record that `todo show` and the panel's row
// ("Sent to alpha") show, and a land answer picked from the context's numbered list is held for the coordinator.
//
//   node todoanswer.mjs BASE OUT_DIR       (env: SCHEME, COMEBYE_BIN, COMEBYE_STATE_DIR)
// BASE is a hub on its own fake herdr (-autoblock) with -auto-todo-blocked-after 2s.
import { execFileSync } from "node:child_process";
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const todos = async () => (await fetch(base + "/api/todos?state=all").then((r) => r.json())).todos;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, what, ms = 20000) {
  for (const end = Date.now() + ms; Date.now() < end; await sleep(200)) { const v = await fn(); if (v) return v; }
  throw new Error("timed out waiting for " + what);
}

let id;
await check("the hub files an item for the blocked agent, bound to its dialog", async () => {
  const it = await until(async () => (await todos()).find((t) => t.source === "hub-blocked" && t.state === "open"), "the auto item");
  id = it.id;
  if (!it.dialog || !it.dialog.fingerprint) throw new Error("no dialog binding: " + JSON.stringify(it));
  if (!it.options || it.options.length < 2 || !/^Yes/.test(it.options[0].label)) throw new Error("options: " + JSON.stringify(it.options));
});

for (const width of [390, 1280]) {
  if (width === 1280 && !id) break;
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  const msgs = [];
  page.on("pageerror", (e) => msgs.push(String(e)));
  try {
    if (width === 390) {
      await check(`to do at ${width}px: the card offers the dialog's options`, async () => {
        // On a phone the list sits under a toggle on the home screen.
        await page.goto(base + "/", { waitUntil: "load" });
        await page.waitForSelector("#todo .td-toggle", { state: "visible", timeout: 15000 });
        if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
        await page.waitForSelector(`#todo .td-item[data-id="${id}"] .td-row`, { state: "visible", timeout: 15000 });
        await page.locator(`#todo .td-item[data-id="${id}"] .td-row`).click();
        await page.waitForSelector(`#todo .td-item.open[data-id="${id}"] .td-opt[data-n="1"]`, { state: "visible" });
        const t = await page.locator(`#todo .td-item[data-id="${id}"]`).innerText();
        if (!/Choosing one here answers the dialog/.test(t)) throw new Error("card text: " + t.slice(0, 300));
        if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("page scrolls sideways");
        await page.screenshot({ path: `${out}/todoanswer-card-${width}.png` });
      });
    } else {
      await check(`to do at ${width}px: Yes answers the dialog in the pane and closes the item`, async () => {
        await page.goto(base + "/#/todo", { waitUntil: "load" });
        await page.evaluate(() => { CHOICE_UNDO_MS = 300; });
        await page.waitForSelector(`#p-todo .td-item[data-id="${id}"] .td-row`, { state: "visible", timeout: 15000 });
        await page.locator(`#p-todo .td-item[data-id="${id}"] .td-row`).click();
        await page.waitForSelector(`#p-todo .td-item.open[data-id="${id}"] .td-opt[data-n="1"]`, { state: "visible" });
        await page.screenshot({ path: `${out}/todoanswer-card-${width}.png` });
        await page.locator(`#p-todo .td-item[data-id="${id}"] .td-opt[data-n="1"]`).click();
        const it = await until(async () => (await todos()).find((t) => t.id === id && t.state !== "open"), "the item to close");
        if (it.state !== "done" || !it.answer || it.answer.via !== "dialog" || it.answer.text !== "ok 1") throw new Error("item: " + JSON.stringify(it));
        // The fake herdr writes every key it receives onto the pane's screen.
        const o = await fetch(base + "/api/agents/w11:p1/output?lines=40").then((r) => r.text());
        if (!/\[keys\] 1/.test(o)) throw new Error("the key did not reach the pane: " + o.slice(-300));
        await page.waitForTimeout(500);
        await page.screenshot({ path: `${out}/todoanswer-done-${width}.png` });
      });
    }
    await check(`to do at ${width}px: no page errors`, async () => { if (msgs.length) throw new Error(msgs.join("; ")); });
  } finally { await browser.close(); }
}
// The hub's own relay of an answer to a worker.
const post = (path, body) => fetch(base + path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }).then(async (r) => ({ code: r.status, j: await r.json().catch(() => ({})) }));
let rid, gid, alpha;
await check("relay: an answer to an item waiting on alpha reaches alpha's transcript", async () => {
  const ags = await fetch(base + "/api/agents").then((r) => r.json());
  alpha = (ags.agents || ags).find((a) => a.name === "alpha");
  if (!alpha) throw new Error("no alpha: " + JSON.stringify(ags).slice(0, 300));
  const r = await post("/api/todos", { question: "Which key should the service use?", kind: "decision", waiting: alpha.id, options: [{ label: "Mint a key", detail: "fresh, rotates" }, { label: "Reuse the old key" }] });
  if (r.code !== 201) throw new Error("add: " + r.code + " " + JSON.stringify(r.j));
  rid = r.j.todo.id;
  const a = await post(`/api/todos/${rid}/answer`, { text: "ok 2" });
  if (a.code !== 200) throw new Error("answer: " + a.code + " " + JSON.stringify(a.j));
  const it = await until(async () => (await todos()).find((t) => t.id === rid && t.state === "done"), "the relayed item to close");
  if (it.closed_by !== "hub" || !it.relay || it.relay.state !== "delivered" || it.relay.to !== "alpha" || !/^[0-9a-f]{6}$/.test(it.relay.notice || "")) throw new Error("item: " + JSON.stringify(it));
  const tr = await fetch(base + `/api/agents/${encodeURIComponent(alpha.id.split("/").pop())}/transcript?limit=20`).then((r) => r.text());
  if (!tr.includes("Comebye (hub relay)") || !tr.includes(rid) || !tr.includes(`[notice ${it.relay.notice}]`)) throw new Error("transcript: " + tr.slice(-600));
});
await check("relay: todo show says the hub relayed it", async () => {
  const bin = process.env.COMEBYE_BIN, sd = process.env.COMEBYE_STATE_DIR;
  if (!bin || !sd) throw new Error("set COMEBYE_BIN and COMEBYE_STATE_DIR");
  const o = execFileSync(bin, ["todo", "show", rid], { env: { PATH: "/usr/bin:/bin", HOME: sd, COMEBYE_URL: base, COMEBYE_STATE_DIR: sd }, encoding: "utf8" });
  if (!/Relayed by the hub to alpha at \d\d:\d\d UTC on \d{4}-\d\d-\d\d; it reached the agent's transcript \(notice [0-9a-f]{6}\)\./.test(o)) throw new Error("todo show: " + o);
  console.log("     " + o.trim().split("\n").filter((l) => l.startsWith("Relayed")).join(""));
});
await check("relay: a land answer chosen from the context's list is held for the coordinator", async () => {
  const r = await post("/api/todos", { question: "Next step for the sweep branch?", kind: "decision", waiting: alpha.id, context: "1. Land it on main\n2. Hold it." });
  if (r.code !== 201) throw new Error("add: " + r.code + " " + JSON.stringify(r.j));
  gid = r.j.todo.id;
  await post(`/api/todos/${gid}/answer`, { text: "ok 1" });
  await sleep(1500);
  const it = (await todos()).find((t) => t.id === gid);
  if (!it || it.state !== "answered" || (it.relay && it.relay.state === "delivered")) throw new Error("item: " + JSON.stringify(it));
  const tr = await fetch(base + `/api/agents/${encodeURIComponent(alpha.id.split("/").pop())}/transcript?limit=20`).then((r) => r.text());
  if (tr.includes(gid)) throw new Error("the gate was relayed to alpha");
});
for (const width of [390, 1280]) {
  if (!rid) break;
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  const msgs = [];
  page.on("pageerror", (e) => msgs.push(String(e)));
  try {
    await check(`relay at ${width}px: the closed row reads "Sent to alpha"`, async () => {
      const sel = width > 500 ? "#p-todo" : "#todo";
      await page.goto(base + "/#/todo/" + rid, { waitUntil: "load" });
      await page.waitForSelector(`${sel} .td-item[data-id="${rid}"] .td-row`, { state: "visible", timeout: 15000 });
      const row = await page.locator(`${sel} .td-item[data-id="${rid}"] .td-row`).innerText();
      if (!/Sent to alpha/.test(row)) throw new Error("row: " + row);
      const t = await page.locator(`${sel} .td-item[data-id="${rid}"]`).innerText();
      if (!/Sent to alpha by the hub .*ago; it reached the agent's transcript \(notice [0-9a-f]{6}\)\./.test(t)) throw new Error("card: " + t.slice(0, 600));
      if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("page scrolls sideways");
      await page.screenshot({ path: `${out}/todoanswer-relayed-${width}.png` });
    });
    await check(`relay at ${width}px: no page errors`, async () => { if (msgs.length) throw new Error(msgs.join("; ")); });
  } finally { await browser.close(); }
}

const bad = results.filter((r) => !r.ok).length;
console.log(`todoanswer: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
