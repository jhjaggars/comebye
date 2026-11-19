// Decision sets: every decision has an "Other…" free-text answer, and the set has a note box
// (internal/server/todoset.go). One decision answered with text, one with an option, a note, Submit,
// then the answer the hub relays. Phone (390) and desktop, light and dark. Waits are on state
// (selectors, the item's state), never on time.
//
//   node setfreetext.mjs BASE_URL OUT_DIR
import fs from "node:fs";
import path from "node:path";
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const SET = (q) => ({
  question: q, kind: "decision", no_wait: true,
  set: {
    decisions: [
      { id: "D1", title: "Worker size", question: "How big is one worker?", recommend: 1, options: [{ label: "2 vCPU / 8 GiB" }, { label: "4 vCPU / 16 GiB" }, { label: "8 vCPU / 16 GiB" }] },
      { id: "D2", title: "Region", recommend: 1, options: [{ label: "us-east-1" }, { label: "eu-west-1" }] },
    ],
  },
});
const TEXT = "4 vCPU / 32 GiB", NOTE = "Check the quota first.";
const results = [];
const check = async (name, fn) => { try { await fn(); results.push([name, true, ""]); } catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); } };
const post = async (b) => (await fetch(base + "/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(b) }).then((r) => r.json())).todo;
const get = async (id) => (await fetch(base + "/api/todos/" + id).then((r) => r.json())).todo;
const drop = (id) => fetch(base + `/api/todos/${id}/drop`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
const eq = (a, b, what) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${what}: got ${JSON.stringify(a)}, want ${JSON.stringify(b)}`); };
// A save is done when the hub's draft says so.
const hubDraft = (id, pred) => async () => { for (let i = 0; i < 100; i++) { const t = await get(id); if (pred(t.set.draft)) return t; await new Promise((r) => setTimeout(r, 50)); } throw new Error("the hub never got the draft"); };

async function openSet(page, id, list) {
  await page.evaluate(() => { todos.loadedAt = 0; todos.openId = null; return loadTodos(); });
  await page.waitForSelector(`${list} .td-item[data-id="${id}"] .td-row`, { state: "visible" });
  await page.locator(`${list} .td-item[data-id="${id}"] .td-row`).click();
  await page.waitForSelector(`${list} .td-item.open[data-id="${id}"] .ds-set`);
}

for (const dark of [false, true]) {
  for (const width of [390, 1280]) {
    const tag = `${dark ? "dark" : "light"} ${width}`, phone = width < 500, L = phone ? "#todo" : "#p-todo";
    const { browser, page } = await launch({ width, height: phone ? 844 : 900, dark });
    const msgs = [];
    page.on("pageerror", (e) => msgs.push(String(e)));
    let id;
    try {
      await page.goto(base + (phone ? "/" : "/#/todo"), { waitUntil: "load" });
      await page.waitForSelector(L, { state: "attached" });
      id = (await post(SET("Free text " + tag))).id;
      if (phone) {
        await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
        await page.waitForSelector("#todo .td-toggle", { state: "visible" });
        if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
      }
      await openSet(page, id, L);
      const it = `${L} .td-item[data-id="${id}"]`;
      const dec = (d) => `${it} .ds-dec[data-dec="${d}"]`;
      const next = async () => { await page.locator(`${it} .ds-next`).click(); };
      const sw = () => page.evaluate(() => document.documentElement.scrollWidth > innerWidth);

      await check(`${tag}: every decision offers Other…`, async () => {
        if (phone) eq(await page.locator(`${it} .ds-other`).count(), 1, "Other on the step");
        else eq(await page.locator(`${it} .ds-other`).count(), 2, "Other on both decisions");
      });
      await check(`${tag}: Other… opens a box; typing and Use this answer confirms the decision`, async () => {
        if (phone) await next(); // D2 is the second step; D1 gets the option below
        const d = "D2";
        await page.locator(`${dec(d)} .ds-other`).click();
        await page.waitForSelector(`${dec(d)} .ds-text`, { state: "visible" });
        await page.locator(`${dec(d)} .ds-text`).fill(TEXT);
        await page.screenshot({ path: path.join(out, `setfreetext-box-${dark ? "dark" : "light"}-${width}.png`) });
        await page.locator(`${dec(d)} .ds-use`).click();
        await hubDraft(id, (dr) => dr.choices.D2 && dr.choices.D2.text === TEXT)();
        eq((await get(id)).set.draft.choices.D2, { option: 0, text: TEXT, confirmed: true }, "hub D2");
        await page.waitForSelector(`${dec(d)} .ds-other.on`);
        if (!(await page.locator(`${dec(d)} .ds-other`).innerText()).includes(TEXT)) throw new Error("the text is not shown on the choice");
        if (await sw()) throw new Error("the page scrolls sideways");
      });
      await check(`${tag}: an option on another decision, then the note, and the count reads 2/2`, async () => {
        if (phone) await page.locator(`${it} .ds-back`).click();
        await page.waitForSelector(`${dec("D1")}`);
        await page.locator(`${dec("D1")} .ds-opt[data-o="2"]`).click();
        await hubDraft(id, (dr) => dr.choices.D1 && dr.choices.D1.option === 2)();
        if (phone) { await page.locator(`${it} .ds-next`).click(); await page.locator(`${it} .ds-next`).click(); await page.waitForSelector(`${it} .ds-review`); }
        await page.waitForSelector(`${it} .ds-notebox`);
        await page.locator(`${it} .ds-notebox`).fill(NOTE);
        // Leaving the box keeps the note, and a tap on Submit right after must still land.
        await page.locator(`${it} .ds-notebox`).blur();
        await hubDraft(id, (dr) => dr.note === NOTE)();
        if (!(await page.locator(`${it} .ds-st`).innerText()).includes("2/2 decided")) throw new Error("status " + (await page.locator(`${it} .ds-st`).innerText()));
        if (phone && !(await page.locator(`${it} .ds-review`).innerText()).includes("Other: " + TEXT)) throw new Error("the review does not show the text");
        await page.screenshot({ path: path.join(out, `setfreetext-set-${dark ? "dark" : "light"}-${width}.png`) });
        if (await sw()) throw new Error("the page scrolls sideways");
      });
      await check(`${tag}: Accept all leaves the text answer alone`, async () => {
        await page.locator(`${it} .ds-all`).click();
        await hubDraft(id, (dr) => dr.rev > 0)();
        eq((await get(id)).set.draft.choices.D2.text, TEXT, "D2 text");
      });
      await check(`${tag}: Submit relays the plain answer: the option, the text and the note`, async () => {
        await page.locator(`${it} .ds-submit`).click();
        await page.waitForFunction((i) => { const t = todos.items.find((x) => x.id === i); return t && t.state === "answered"; }, id, { timeout: 5000 });
        const t = await get(id), a = t.answer.text;
        for (const w of ["**D1 Worker size**: 4 vCPU / 16 GiB", "**D2 Region**: Other: " + TEXT, NOTE]) if (!a.includes(w)) throw new Error(`answer lacks ${w}: ${a}`);
        eq(t.set.result.decisions.D2, { option: 0, label: TEXT, text: TEXT, suggested: false }, "result D2");
        eq(t.set.result.decisions.D1.option, 2, "result D1");
        eq(t.set.result.note, NOTE, "result note");
      });
      await check(`${tag}: no script errors`, async () => { if (msgs.length) throw new Error(msgs.join("; ")); });
    } finally { if (id) drop(id); await browser.close(); }
  }
}
for (const [n, ok, why] of results) console.log(`${ok ? "PASS" : "FAIL"}  ${n}${ok ? "" : "  -- " + why}`);
process.exit(results.some((r) => !r[1]) ? 1 : 0);
