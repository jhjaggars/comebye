// Decision sets in the To do panel (internal/server/todoset.go): the card on
// desktop, the stepper and its review screen on a phone, Accept all with
// one-way doors held back, live conflicts and their fix, the draft kept on
// the hub across a reload, and Submit. Each check files its own set.
//
//   node decisionsets.mjs BASE_URL OUT_DIR
import fs from "node:fs";
import path from "node:path";
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const SET = (q) => ({
  question: q, kind: "decision", no_wait: true,
  set: {
    decided: ["S3 protocol, no vendor SDK"],
    risks: [{ id: "R1", text: "Old data lost or half-copied", decisions: ["D3"] }],
    decisions: [
      { id: "D1", title: "Where tenants live", surface: "store", release_early: true, recommend: 1, options: [{ label: "One bucket, a prefix per tenant", short: "1 bucket" }, { label: "A bucket per tenant", short: "bucket each" }] },
      { id: "D3", title: "How existing data moves", surface: "migrator", one_way: true, recommend: 1, options: [{ label: "Dual-write for one release", short: "dual-write" }, { label: "Big-bang cutover", short: "big-bang" }] },
      { id: "D6", title: "Rollout", surface: "rollout", recommend: 1, options: [{ label: "Flag per tenant", short: "per-tenant flag" }, { label: "All at once", short: "all at once" }] },
    ],
    facts: [{ id: "F1", text: "Strong read-after-write?", test: "loop on staging", affects: ["D1"] }],
    rules: [
      { id: "r1", level: "block", when: { D3: 2, D6: 1 }, say: "Big-bang cannot sit behind a per-tenant flag.", fix: { decision: "D3", option: 1 } },
      { id: "r2", level: "warn", when: { D1: 2, D6: 2 }, say: "All at once with a bucket each is heavy." },
    ],
  },
});
const results = [];
const check = async (name, fn) => { try { await fn(); results.push([name, true, ""]); } catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); } };
const post = async (b) => (await fetch(base + "/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(b) }).then((r) => r.json())).todo;
const get = async (id) => (await fetch(base + "/api/todos/" + id).then((r) => r.json())).todo;
const drop = (id) => fetch(base + `/api/todos/${id}/drop`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
const eq = (a, b, what) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${what}: got ${JSON.stringify(a)}, want ${JSON.stringify(b)}`); };

async function openSet(page, id, list) {
  await page.evaluate(() => { todos.loadedAt = 0; todos.openId = null; return loadTodos(); });
  await page.waitForSelector(`${list} .td-item[data-id="${id}"] .td-row`, { state: "visible" });
  await page.locator(`${list} .td-item[data-id="${id}"] .td-row`).click();
  await page.waitForSelector(`${list} .td-item.open[data-id="${id}"] .ds-set`);
}
const status = (page, list, id) => page.locator(`${list} .td-item[data-id="${id}"] .ds-st`).innerText();
const settle = (page) => page.waitForTimeout(250); // the save and the redraw

for (const dark of [false, true]) {
  const tag = dark ? "dark" : "light";
  const { browser, ctx, page } = await launch({ width: 1280, dark });
  const msgs = [];
  page.on("pageerror", (e) => msgs.push(String(e)));
  try {
    await page.goto(base + "/#/todo", { waitUntil: "load" });
    await page.waitForSelector("#p-todo", { state: "visible" });
    const L = "#p-todo";
    let id;
    await check(`${tag} desktop: the card shows the decided list, risks, every decision and the facts`, async () => {
      id = (await post(SET("Desktop set " + tag))).id;
      await openSet(page, id, L);
      const it = `${L} .td-item[data-id="${id}"]`;
      eq(await page.locator(`${it} .ds-dec`).count(), 3, "decisions");
      eq(await page.locator(`${it} .ds-risk`).count(), 1, "risks");
      eq(await page.locator(`${it} .ds-fact`).count(), 1, "facts");
      const txt = await page.locator(it).innerText();
      for (const w of ["S3 protocol", "UNBLOCKS NOW", "ONE-WAY DOOR", "0/3 decided", "Fact F1 unknown"]) if (!txt.toUpperCase().includes(w.toUpperCase())) throw new Error("missing " + w);
    });
    await check(`${tag} desktop: Accept all fills what is empty and holds back the one-way door`, async () => {
      const it = `${L} .td-item[data-id="${id}"]`;
      await page.locator(`${it} .ds-all`).click(); await settle(page);
      eq(await page.locator(`${it} .ds-opt.on:not(.pend)`).count(), 2, "confirmed picks");
      eq(await page.locator(`${it} .ds-opt.pend`).count(), 1, "pending (one-way) picks");
      if (!(await status(page, L, id)).includes("2/3 decided")) throw new Error("status " + (await status(page, L, id)));
      if (!(await page.locator(`${it} .ds-submit`).isDisabled())) throw new Error("Submit was open with a door unconfirmed");
      await page.screenshot({ path: path.join(out, `decisionset-${tag}-1280.png`) });
      await page.locator(`${it} .ds-confirm`).click(); await settle(page);
      eq(await page.locator(`${it} .ds-submit`).isDisabled(), false, "Submit enabled after confirming");
    });
    await check(`${tag} desktop: a blocking conflict shows, locks Submit, and its fix clears it`, async () => {
      const it = `${L} .td-item[data-id="${id}"]`;
      await page.locator(`${it} .ds-dec[data-dec="D3"] .ds-opt[data-o="2"]`).click(); await settle(page);
      if (!(await status(page, L, id)).includes("1 blocking")) throw new Error("no blocking chip: " + (await status(page, L, id)));
      eq(await page.locator(`${it} .ds-note.block`).count(), 2, "banner on both decisions");
      eq(await page.locator(`${it} .ds-submit`).isDisabled(), true, "Submit locked");
      await page.locator(`${it} .ds-dec[data-dec="D3"] .ds-fix`).click(); await settle(page);
      if (!(await status(page, L, id)).includes("0 conflicts")) throw new Error("not cleared: " + (await status(page, L, id)));
    });
    await check(`${tag} desktop: a warning shows but does not stop Submit`, async () => {
      const it = `${L} .td-item[data-id="${id}"]`;
      await page.locator(`${it} .ds-dec[data-dec="D1"] .ds-opt[data-o="2"]`).click();
      await page.locator(`${it} .ds-dec[data-dec="D6"] .ds-opt[data-o="2"]`).click(); await settle(page);
      if (!(await status(page, L, id)).includes("1 warning")) throw new Error("status " + (await status(page, L, id)));
      eq(await page.locator(`${it} .ds-submit`).isDisabled(), false, "Submit open");
    });
    await check(`${tag} desktop: the draft is on the hub and survives a reload`, async () => {
      const t = await get(id);
      eq(t.set.draft.choices.D1, { option: 2, confirmed: true }, "hub D1");
      await page.reload({ waitUntil: "load" });
      await openSet(page, id, L);
      eq(await page.locator(`${L} .td-item[data-id="${id}"] .ds-dec[data-dec="D1"] .ds-opt.on`).getAttribute("data-o"), "2", "D1 after reload");
    });
    await check(`${tag} desktop: Mark known is saved, and Submit answers the item with the structured result`, async () => {
      const it = `${L} .td-item[data-id="${id}"]`;
      await page.locator(`${it} .ds-fk`).click(); await settle(page);
      eq((await get(id)).set.draft.facts, { F1: true }, "fact");
      await page.locator(`${it} .ds-submit`).click();
      await page.waitForFunction((i) => { const t = todos.items.find((x) => x.id === i); return t && t.state === "answered"; }, id, { timeout: 5000 });
      const t = await get(id);
      eq(t.state, "answered", "state");
      eq(t.set.result.warnings.map((w) => w.rule), ["r2"], "recorded warnings");
      eq(t.set.result.decisions.D1.suggested, false, "D1 was an override");
      eq(t.set.result.facts.F1, "known", "fact in result");
    });
    drop(id);
    await check(`${tag} desktop: no script errors`, async () => { if (msgs.length) throw new Error(msgs.join("; ")); });
  } finally { await browser.close(); }

  const m = await launch({ width: 390, dark });
  const pmsgs = [];
  m.page.on("pageerror", (e) => pmsgs.push(String(e)));
  try {
    await m.page.goto(base + "/", { waitUntil: "load" });
    const L = "#todo";
    let id;
    await m.page.waitForSelector("#todo", { state: "attached" });
    await check(`${tag} phone: the set is a stepper, one decision at a time, with dots`, async () => {
      id = (await post(SET("Phone set " + tag))).id;
      await m.page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
      await m.page.waitForSelector("#todo .td-toggle", { state: "visible" });
      if ((await m.page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await m.page.locator("#todo .td-toggle").click();
      await openSet(m.page, id, L);
      const it = `${L} .td-item[data-id="${id}"]`;
      eq(await m.page.locator(`${it} .ds-dec`).count(), 1, "one decision shown");
      eq(await m.page.locator(`${it} .ds-dots button`).count(), 4, "dots (3 + review)");
      await m.page.screenshot({ path: path.join(out, `decisionset-${tag}-390.png`) });
    });
    await check(`${tag} phone: Next walks through, a pick is saved, and the review lists every answer`, async () => {
      const it = `${L} .td-item[data-id="${id}"]`;
      await m.page.locator(`${it} .ds-opt[data-o="1"]`).click(); await settle(m.page);
      await m.page.locator(`${it} .ds-next`).click(); await settle(m.page);
      eq(await m.page.locator(`${it} .ds-dec`).getAttribute("data-dec"), "D3", "step 2");
      await m.page.locator(`${it} .ds-next`).click(); await settle(m.page);
      await m.page.locator(`${it} .ds-next`).click(); await settle(m.page);
      eq(await m.page.locator(`${it} .ds-rv`).count(), 3, "review rows");
      if (!(await m.page.locator(`${it} .ds-review`).innerText()).includes("1 bucket")) throw new Error("D1 missing from the review");
      eq((await get(id)).set.draft.choices.D1, { option: 1, confirmed: true }, "hub D1");
      await m.page.screenshot({ path: path.join(out, `decisionset-${tag}-390-review.png`) });
    });
    await check(`${tag} phone: no horizontal scroll, and Accept all then Submit on the review screen`, async () => {
      const it = `${L} .td-item[data-id="${id}"]`;
      if (await m.page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("the page scrolls sideways");
      await m.page.locator(`${it} .ds-all`).click(); await settle(m.page);
      eq(await m.page.locator(`${it} .ds-submit`).isDisabled(), true, "one-way door held back");
      await m.page.locator(`${it} .ds-rv`).nth(1).click(); await settle(m.page);
      await m.page.locator(`${it} .ds-confirm`).click(); await settle(m.page);
      await m.page.locator(`${it} .ds-next`).click(); await settle(m.page); await m.page.locator(`${it} .ds-next`).click(); await settle(m.page);
      await m.page.locator(`${it} .ds-submit`).click();
      await m.page.waitForFunction((i) => { const t = todos.items.find((x) => x.id === i); return t && t.state === "answered"; }, id, { timeout: 5000 });
    });
    drop(id);
    await check(`${tag} phone: no script errors`, async () => { if (pmsgs.length) throw new Error(pmsgs.join("; ")); });
  } finally { await m.browser.close(); }
}
for (const [n, ok, why] of results) console.log(`${ok ? "PASS" : "FAIL"}  ${n}${ok ? "" : "  -- " + why}`);
process.exit(results.some((r) => !r[1]) ? 1 : 0);
