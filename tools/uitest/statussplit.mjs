// Status in the side panel: the Doc / Diff split beside an agent's chat has a third tab, Status, drawn by the same code as
// the full-screen Status page (renderStatus / statusHTML), live on the hub's events. Like Doc and Diff it has no URL of its
// own: the agent's route stays, Back leaves the agent as before, and the panel's open tab is remembered per agent (so a
// reload, or coming back to the agent, puts it up again). Unlike them it is not about one agent, so it stays open on
// Status when another agent is opened. On a phone it is the third tab of the bottom sheet. The full-screen Status is
// checked unchanged at both widths.
//
//   node statussplit.mjs BASE OUT_DIR      (env: SCHEME, WIDTHS)
import { launch } from "./lib.mjs";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const tag0 = dark ? "dark" : "light";
const J = { "Content-Type": "application/json" };
const add = async (b) => { const j = await (await fetch(base + "/api/todos", { method: "POST", headers: J, body: JSON.stringify({ no_wait: true, kind: "decision", ...b }) })).json(); if (!j.todo) throw new Error("add: " + JSON.stringify(j)); return j.todo; };
const drop = (id) => fetch(base + `/api/todos/${id}/drop`, { method: "POST", headers: J, body: "{}" });
const live = (await (await fetch(base + "/api/agents")).json()).agents.filter((a) => a.pane_id);
if (live.length < 2) { console.error("statussplit.mjs: the server needs two agents"); process.exit(2); }
const [A, B] = live;
const route = (a) => "#/agent/" + a.pane_id;

// Polls a page function with evaluate: waitForFunction compiles its predicate with eval, which the app's CSP can refuse.
async function until(page, fn, arg, opts = {}) {
  const end = Date.now() + (opts.timeout || 8000);
  for (;;) {
    if (await page.evaluate(fn, arg)) return;
    if (Date.now() > end) throw new Error((opts.what || "timed out waiting for ") + String(fn).slice(0, 140));
    await page.waitForTimeout(50);
  }
}
const same = (got, want, what) => { if (JSON.stringify(got) !== JSON.stringify(want)) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

for (const width of (process.env.WIDTHS || "1280,390").split(",").map(Number)) {
  const phone = width < 500, tag = `${width}px ${tag0}`;
  const { browser, page } = await launch({ width, height: phone ? 844 : 800, dark, bypassCSP: false });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(String(e)));
  const shot = (n) => page.screenshot({ path: `${out}/statussplit-${n}-${width}-${tag0}.png` }).catch(() => {});
  const openAgent = async (a) => {
    await page.goto("about:blank");
    await page.goto(base + "/" + route(a), { waitUntil: "load" });
    await page.waitForSelector("#sp-btn", { state: "visible", timeout: 15000 });
  };
  const panel = () => page.evaluate(() => ({
    open: !!document.querySelector("#detail.sp-open"),
    tab: document.querySelector("#sp .sp-tab[aria-selected=true]")?.dataset.tab || "",
    st: !!document.querySelector("#sp-st .st"),
    hash: decodeURIComponent(location.hash),
  }));
  const ids = [];
  try {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.evaluate(() => { try { for (const k of Object.keys(localStorage)) if (k.startsWith("cb.sp.")) localStorage.removeItem(k); } catch {} });

    await check(`${tag}: the panel has a Status tab and shows the hub's Status beside the chat`, async () => {
      await openAgent(A);
      same((await page.locator("#sp .sp-tab").allInnerTexts()).map((t) => t.trim()), ["Doc", "Diff", "Status"], "tabs");
      await page.click("#sp-btn");
      await page.click("#sp-tab-status");
      await page.waitForSelector("#sp-st .st", { state: "visible" });
      const p = await panel();
      same([p.open, p.tab, p.st], [true, "status", true], "open on Status");
      same(p.hash, route(A), "the agent's route stays");
      // The chat is still there, beside it (desktop) or under the sheet (phone).
      if (!(await page.locator("#prompt-text").count())) throw new Error("the chat's prompt box is gone");
      const g = await page.evaluate(() => {
        const sp = document.querySelector("#sp").getBoundingClientRect(), m = document.querySelector("#detail > main").getBoundingClientRect();
        return { over: document.documentElement.scrollWidth - innerWidth, body: (() => { const b = document.querySelector("#sp-body"); return b.scrollWidth - b.clientWidth; })(), spL: sp.left, mR: m.right, spTop: sp.top, h: innerHeight };
      });
      same(g.over, 0, "page overflows sideways by");
      same(g.body, 0, "panel body overflows sideways by");
      if (!phone && g.spL < g.mR - 1) throw new Error(`the split overlaps the chat: panel left ${g.spL}, chat right ${g.mR}`);
      if (phone && g.spTop < g.h * 0.2) throw new Error("the sheet is not a bottom sheet: top " + g.spTop);
      // The desktop's split has the desktop's links and a way to the full page; the phone's sheet has the phone screen's.
      same(await page.locator("#sp-full").count(), phone ? 0 : 1, "Full screen button");
      await shot("open");
    });

    await check(`${tag}: it is drawn by the Status page's own code (same as #/status renders)`, async () => {
      const mine = await page.evaluate((ph) => { renderStatus(); const el = document.querySelector("#sp-st"); return el._html === statusHTML(!ph); }, phone);
      same(mine, true, "panel html equals statusHTML()");
    });

    await check(`${tag}: it updates live when the hub files an item`, async () => {
      const q = `Status split live ${width} ${tag0} ${Date.now()}`;
      const t = await add({ question: q });
      ids.push(t.id);
      // The same path as the full-screen page: an agents event schedules the ledger (5 s at most), which reloads the items (8 s).
      const t0 = Date.now();
      await until(page, (s) => (document.querySelector("#sp-st")?.textContent || "").includes(s), q, { timeout: 45000, what: "the new item never reached the panel: " });
      console.log(`     (in the panel after ${((Date.now() - t0) / 1000).toFixed(1)} s, no reload)`);
      await shot("live");
      await drop(t.id); // its leaving takes the same path again: not waited for, to keep the script short
    });

    await check(`${tag}: it closes (×, then D or the button reopens it on Status), and the URL never moves`, async () => {
      const len0 = await page.evaluate(() => history.length);
      await page.click("#sp .sp-x");
      same((await panel()).open, false, "closed");
      if (phone) await page.click("#sp-btn"); else { await page.locator("#prompt-text").evaluate((x) => x.blur()); await page.keyboard.press("d"); }
      const p = await panel();
      same([p.open, p.tab, p.st, p.hash], [true, "status", true, route(A)], "reopened on Status");
      // Escape inside the panel closes it, as for Doc and Diff.
      await page.click("#sp-tab-status");
      await page.keyboard.press("Escape");
      same((await panel()).open, false, "Escape closed it");
      await page.click("#sp-btn");
      same((await panel()).tab, "status", "the button reopens it on Status");
      same(await page.evaluate(() => history.length), len0, "history entries added by the panel");
    });

    await check(`${tag}: a reload keeps it open on Status; ${phone ? "the sheet stays with its agent" : "the next agent keeps Status beside it; Back returns to the first"}`, async () => {
      await page.reload({ waitUntil: "load" });
      await page.waitForSelector("#sp-st .st", { state: "visible", timeout: 15000 });
      same((await panel()).tab, "status", "after reload");
      // Another agent: open it the way a user does (its row in the list; on a phone, Back to the list first).
      if (phone) { await page.click("#back"); await page.waitForSelector("#agents .agent[data-id]:visible"); }
      const rows = await page.locator("#agents .agent[data-id]:visible").evaluateAll((xs, a) => xs.map((x) => x.dataset.id).filter((id) => id !== a), A.id);
      if (!rows.length) throw new Error("no other agent row on screen");
      await page.click(`#agents .agent[data-id="${rows[0]}"]`);
      await until(page, (h) => decodeURIComponent(location.hash) !== h && decodeURIComponent(location.hash).startsWith("#/agent/"), route(A));
      if (phone) {
        // On a phone the list comes between the two agents: the sheet does not follow (it would cover every chat opened);
        // the second agent's own memory says (closed here). The first agent still remembers its Status sheet.
        await page.waitForSelector("#sp-btn", { state: "visible" });
        same((await panel()).open, false, "the sheet followed through the list");
        await page.click("#back");
        await page.click(`#agents .agent[data-id="${A.id}"]`);
        await page.waitForSelector("#sp-st .st", { state: "visible" });
        same((await panel()).tab, "status", "the first agent's sheet");
        return;
      }
      await page.waitForSelector("#sp-st .st", { state: "visible" });
      const p = await panel();
      same([p.open, p.tab], [true, "status"], "beside the second agent");
      {
        await page.goBack();
        await until(page, (h) => decodeURIComponent(location.hash) === h, route(A));
        await page.waitForSelector("#sp-st .st", { state: "visible" });
        same((await panel()).tab, "status", "Back to the first agent");
      }
    });

    await check(`${tag}: Doc and Diff still work, and the tabs step with the arrow keys`, async () => {
      await page.click("#sp-tab-doc");
      same((await panel()).tab, "doc", "Doc");
      same(await page.locator("#sp-st").count(), 0, "Status gone from the body");
      await page.focus("#sp-tab-doc");
      await page.keyboard.press("ArrowLeft");
      same((await panel()).tab, "status", "ArrowLeft from Doc wraps to Status");
      await page.waitForSelector("#sp-st .st", { state: "visible" });
      await page.keyboard.press("ArrowRight");
      same((await panel()).tab, "doc", "ArrowRight from Status wraps to Doc");
      await page.click("#sp-tab-status");
    });

    if (!phone) {
      await check(`${tag}: an agent's "open" link and Full screen work from the panel`, async () => {
        await page.click("#sp-full");
        await until(page, () => location.hash === "#/status");
        await page.waitForSelector("#ps-body .st", { state: "visible" });
        same(await page.evaluate(() => !!document.querySelector("#detail.sp-open") && !!document.querySelector("#sp-st")), false, "the panel left with the agent");
      });
    }

    await check(`${tag}: the full-screen Status is unchanged`, async () => {
      await page.goto("about:blank");
      await page.goto(base + "/#/status", { waitUntil: "load" });
      if (phone) {
        await page.waitForSelector("#st-screen.open #ss-body .st", { state: "visible", timeout: 15000 });
        same(await page.evaluate(() => { renderStatus(); return document.querySelector("#ss-body")._html === statusHTML(false); }), true, "phone screen html");
      } else {
        await page.waitForSelector("#ps-body .st", { state: "visible", timeout: 15000 });
        same(await page.evaluate(() => { renderStatus(); return document.querySelector("#ps-body")._html === statusHTML(true); }), true, "page html");
      }
      same(await page.locator("#sp-st").count(), 0, "no panel Status on the Status page");
      await shot("fullscreen");
    });

    await check(`${tag}: no page errors`, async () => { same(errs, [], "page errors"); });
  } finally {
    for (const id of ids) await drop(id).catch(() => {});
    await browser.close();
  }
}

const bad = results.filter((r) => !r.ok);
console.log(`statussplit: ${results.length - bad.length}/${results.length} ok`);
process.exit(bad.length ? 1 : 0);
