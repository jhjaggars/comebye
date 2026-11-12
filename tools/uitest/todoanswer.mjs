// Answering a hub-blocked to-do from the To do panel (internal/server/todoanswer.go): the hub files an
// item for an agent blocked at a dialog, and its option buttons answer the dialog in the pane.
//
//   node todoanswer.mjs BASE OUT_DIR       (env: SCHEME)
// BASE is a hub on its own fake herdr (-autoblock) with -auto-todo-blocked-after 2s.
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
const bad = results.filter((r) => !r.ok).length;
console.log(`todoanswer: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
