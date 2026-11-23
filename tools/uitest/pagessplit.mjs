// The Pages tab of the side panel beside an agent's chat: the Pages that agent published (app.js's agentPages), newest first,
// as the Pages list's own cards; one of them open in the panel in the Pages screen's own frame (pageFrameHtml) with a way
// back to the list and to the full-screen Pages viewer; live when the agent publishes a Page or a version (the Pages
// screen's list, read again as often as that screen reads it); a quiet empty state. Doc, Diff and Status keep their
// tabs and arrow keys; on a phone Pages is the sheet's fourth tab. Pages are published with the real CLI as the agents.
//
//   node pagessplit.mjs BASE OUT_DIR      (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME, WIDTHS)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("pagessplit.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const tag0 = dark ? "dark" : "light";
const live = (await (await fetch(base + "/api/agents")).json()).agents.filter((a) => a.pane_id);
if (live.length < 2) { console.error("pagessplit.mjs: the server needs two agents"); process.exit(2); }
const [A, B] = live;
const route = (a) => "#/agent/" + a.pane_id;

const work = fs.mkdtempSync(path.join(os.tmpdir(), "pagessplit-ui-"));
let seq = 0;
// publish runs `comebye publish` as the agent in pane `as` (the hub stamps the page with that agent's "host/pane").
const publish = (as, title, extra = []) => {
  const f = path.join(work, `p${++seq}.html`);
  fs.writeFileSync(f, `<!doctype html><title>${title}</title><h1>${title}</h1><p>seq ${seq}</p>`);
  const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: as.pane_id };
  return execFileSync(bin, ["publish", f, "--title", title, ...extra], { env, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).match(/pg_[0-9a-f]{10}/)[0];
};
const allPages = async () => (await (await fetch(base + "/api/pages")).json()).pages || [];
const del = (id) => fetch(base + "/api/pages/" + id, { method: "DELETE" });

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

try {
  for (const width of (process.env.WIDTHS || "1280,390").split(",").map(Number)) {
    const phone = width < 500, tag = `${width}px ${tag0}`;
    // Each width starts with no Pages from A or B (a throwaway hub: these are the script's own and other scripts' test pages).
    for (const p of await allPages()) if (p.agent === A.id || p.agent === B.id) await del(p.id);
    const { browser, page } = await launch({ width, height: phone ? 844 : 800, dark, bypassCSP: false });
    page.setDefaultTimeout(8000);
    const errs = [];
    page.on("pageerror", (e) => errs.push(String(e)));
    const shot = (n) => page.screenshot({ path: `${out}/pagessplit-${n}-${width}-${tag0}.png` }).catch(() => {});
    const panel = () => page.evaluate(() => ({
      open: !!document.querySelector("#detail.sp-open"),
      tab: document.querySelector("#sp .sp-tab[aria-selected=true]")?.dataset.tab || "",
      rows: [...document.querySelectorAll("#sp-pgl a.pgs-card")].map((x) => x.dataset.id),
      frame: document.querySelector("#sp-body iframe.pgs-frame")?.getAttribute("src") || "",
      empty: !!document.querySelector("#sp-body .sp-pg0"),
      badge: document.querySelector("#sp-pgn")?.hidden ? "" : document.querySelector("#sp-pgn")?.textContent || "",
      hash: decodeURIComponent(location.hash),
    }));
    const frameText = async (id, ver) => {
      const end = Date.now() + 8000;
      for (;;) {
        const f = page.frames().find((x) => x.url().includes(`/p/${id}/${ver}`));
        const t = f ? await f.locator("h1").textContent({ timeout: 1000 }).catch(() => "") : "";
        if (t) return t;
        if (Date.now() > end) throw new Error(`the frame of ${id} v${ver} never showed its page`);
        await page.waitForTimeout(100);
      }
    };
    let P1, P2;
    try {
      await page.goto(base + "/", { waitUntil: "load" });
      await page.evaluate(() => { try { for (const k of Object.keys(localStorage)) if (k.startsWith("cb.sp.")) localStorage.removeItem(k); } catch {} });
      await page.goto(base + "/" + route(A), { waitUntil: "load" });
      await page.waitForSelector("#sp-btn", { state: "visible", timeout: 15000 });

      await check(`${tag}: Pages is the fourth tab; with no Pages it says so quietly`, async () => {
        same((await page.locator("#sp .sp-tab").allInnerTexts()).map((t) => t.trim()), ["Doc", "Diff", "Status", "Pages"], "tabs");
        await page.click("#sp-btn");
        await page.click("#sp-tab-pages");
        await page.waitForSelector("#sp-body .sp-pg0", { state: "visible" });
        const p = await panel();
        same([p.open, p.tab, p.empty, p.rows.length, p.badge, p.hash], [true, "pages", true, 0, "", route(A)], "empty Pages tab");
        const txt = await page.locator("#sp-body .sp-pg0").innerText();
        if (!/No Pages from this agent yet/.test(txt) || !/comebye publish/.test(txt)) throw new Error("empty state text: " + txt);
        const g = await page.evaluate(() => ({ over: document.documentElement.scrollWidth - innerWidth, body: (() => { const b = document.querySelector("#sp-body"); return b.scrollWidth - b.clientWidth; })() }));
        same([g.over, g.body], [0, 0], "sideways overflow (page, panel body)");
        await shot("empty");
      });

      await check(`${tag}: a Page the agent publishes shows up live; another agent's does not`, async () => {
        const t0 = Date.now();
        P1 = publish(A, `Split plan ${width} ${tag0}`);
        publish(B, `Someone else ${width} ${tag0}`);
        await until(page, (id) => [...document.querySelectorAll("#sp-pgl a.pgs-card")].some((x) => x.dataset.id === id), P1, { timeout: 30000, what: "the new Page never reached the panel: " });
        console.log(`     (in the panel after ${((Date.now() - t0) / 1000).toFixed(1)} s, no reload)`);
        P2 = publish(A, `Split report ${width} ${tag0}`, ["--key", `splitrep${width}`]);
        await until(page, (ids) => JSON.stringify([...document.querySelectorAll("#sp-pgl a.pgs-card")].map((x) => x.dataset.id)) === JSON.stringify(ids), [P2, P1], { timeout: 30000, what: "the second Page never came first: " });
        const p = await panel();
        same([p.rows, p.badge, p.empty], [[P2, P1], "2", false], "the agent's Pages, newest first");
      });

      await check(`${tag}: each row is the Pages list's card: title, age and version`, async () => {
        const r = await page.locator(`#sp-pgl a.pgs-card[data-id="${P2}"]`).evaluate((c) => ({
          title: c.querySelector(".pgs-title")?.textContent, ver: [...c.querySelectorAll(".pgs-pill")].map((x) => x.textContent).pop(),
          age: c.querySelectorAll(".pgs-line .pgs-meta")[1]?.textContent || "", href: c.getAttribute("href"), rows: !!c.closest(".pgs-rows"),
        }));
        same([r.title, r.ver, r.href, r.rows], [`Split report ${width} ${tag0}`, "v1", "#/pages/" + P2, true], "card");
        if (!/ago|now/.test(r.age)) throw new Error("no age on the card: " + r.age);
        // The thumbnail fills in after the draw (preview, image); the rest is the Pages list's card as drawn.
        const want = await page.evaluate((id) => { const t = document.createElement("template"); t.innerHTML = pgsCardHtml(pgs.items.find((x) => x.id === id), false); return t.content.querySelector(".pgs-txt").outerHTML; }, P2);
        same(await page.locator(`#sp-pgl a.pgs-card[data-id="${P2}"] .pgs-txt`).evaluate((x) => x.outerHTML), want, "same markup as the Pages list's card");
        await shot("list");
      });

      await check(`${tag}: matching: the agent's handoff follows it across panes; an earlier agent in the same pane is left out`, async () => {
        // The test hub has no handoffs, so the ledger entry is put in the app's state: a handoff that started a minute ago.
        const H = "t" + Date.now().toString(16).slice(-10).padStart(10, "0");
        const fromB = publish(B, `Handoff page ${width} ${tag0}`, ["--handoff", H]);
        const got = await page.evaluate(([aid, h, fb, p1, p2]) => {
          const a = agentById(aid);
          const e = { id: h, agent_id: aid, status: "running", created_at: new Date(Date.now() - 60000).toISOString() };
          return pagesLoad().then(() => {
            // All in one task, so a ledger refresh cannot land in between.
            const keep = { entries: ledger.entries, byAgent: ledger.byAgent };
            ledger.entries = [...(ledger.entries || []), e];
            ledger.byAgent = new Map(ledger.byAgent); ledger.byAgent.set(aid, e);
            const old = pgs.items.map((p) => ({ ...p }));
            const withH = agentPages(a).map((p) => p.id);
            // P1 made before the handoff started (moved back two minutes): an earlier agent's in the same pane.
            for (const p of pgs.items) if (p.id === p1) p.created_at = new Date(Date.now() - 120000).toISOString();
            const reused = agentPages(a).map((p) => p.id);
            ledger.entries = keep.entries; ledger.byAgent = keep.byAgent;
            const none = agentPages(a).map((p) => p.id);
            pgs.items = old;
            return { withH: withH.includes(fb) && withH.includes(p1) && withH.includes(p2), reused: !reused.includes(p1) && reused.includes(fb) && reused.includes(p2), none: !none.includes(fb) };
          });
        }, [A.id, H, fromB, P1, P2]);
        same(got, { withH: true, reused: true, none: true }, "handoff page in, earlier occupant's page out, no handoff: by pane only");
        await del(fromB);
        await page.evaluate(() => pagesLoad());
      });

      await check(`${tag}: a click opens the Page in the panel, in the Pages screen's frame`, async () => {
        await page.click(`#sp-pgl a.pgs-card[data-id="${P2}"]`);
        await page.waitForSelector("#sp-body iframe.pgs-frame");
        const p = await panel();
        same([p.hash, p.tab], [route(A), "pages"], "the agent's route stays");
        if (!p.frame.endsWith(`/p/${P2}/1`)) throw new Error("frame src " + p.frame);
        same(await page.locator("#sp-body iframe.pgs-frame").getAttribute("sandbox"), "allow-scripts allow-popups allow-popups-to-escape-sandbox", "sandbox (no allow-same-origin)");
        same(await frameText(P2, 1), `Split report ${width} ${tag0}`, "the page in the frame");
        same(await page.locator("#sp-bar .sp-pgt").innerText(), `Split report ${width} ${tag0}`, "title in the bar");
        same(await page.locator("#sp-pgv").innerText(), "v1", "version pill");
        same(await page.evaluate((id) => pgc.id === id && pgcFrame() === document.querySelector("#sp-body iframe.pgs-frame"), P2), true, "the comments work on the panel's frame");
        const g = await page.evaluate(() => { const b = document.querySelector("#sp-body").getBoundingClientRect(), f = document.querySelector("#sp-body iframe").getBoundingClientRect(); return { w: Math.round(b.width - f.width), h: f.height, over: document.documentElement.scrollWidth - innerWidth }; });
        if (g.w > 2 || g.h < 200 || g.over) throw new Error("frame geometry " + JSON.stringify(g));
        await shot("open");
      });

      await check(`${tag}: a new version is offered, not swapped under the reader; reload shows it`, async () => {
        publish(A, `Split report ${width} ${tag0}`, ["--key", `splitrep${width}`]);
        await page.waitForSelector("#sp-pgnew", { state: "visible", timeout: 30000 });
        same(await page.locator("#sp-pgnew").innerText(), "v2 is out · reload", "pill");
        if (!(await panel()).frame.endsWith(`/p/${P2}/1`)) throw new Error("the frame moved by itself");
        await shot("newer");
        await page.click("#sp-pgnew");
        await until(page, (id) => (document.querySelector("#sp-body iframe.pgs-frame")?.getAttribute("src") || "").endsWith(`/p/${id}/2`), P2);
        same(await page.locator("#sp-pgv").innerText(), "v2", "version pill");
      });

      await check(`${tag}: it is remembered for the agent (reload), and back returns to the list`, async () => {
        await page.reload({ waitUntil: "load" });
        await page.waitForSelector("#sp-body iframe.pgs-frame", { timeout: 15000 });
        if (!(await panel()).frame.endsWith(`/p/${P2}/2`)) throw new Error("after reload: " + (await panel()).frame);
        await page.click("#sp-pgback");
        await page.waitForSelector("#sp-pgl");
        const p = await panel();
        same([p.rows, p.frame], [[P2, P1], ""], "back to the list");
        same(await page.evaluate((id) => document.activeElement?.dataset?.id === id, P2), true, "focus on the Page's row");
        same(await page.locator("#sp-pgl .pgs-pill").first().innerText(), "v2", "the row has the new version");
      });

      await check(`${tag}: Full screen opens the Page on the Pages screen; Back returns to the agent with it in the panel`, async () => {
        await page.click(`#sp-pgl a.pgs-card[data-id="${P1}"]`);
        await page.waitForSelector("#sp-pgfull");
        await page.click("#sp-pgfull");
        await until(page, (id) => location.hash === "#/pages/" + id, P1);
        await page.waitForSelector("#pgs-view iframe.pgs-frame", { state: "visible" });
        same(await page.evaluate(() => pgcFrame() === document.querySelector("#pgs-view iframe.pgs-frame")), true, "comments follow the full-screen viewer");
        await shot("fullscreen");
        await page.goBack();
        await until(page, (h) => decodeURIComponent(location.hash) === h, route(A));
        await page.waitForSelector("#sp-body iframe.pgs-frame", { state: "visible" });
        const p = await panel();
        if (!p.frame.endsWith(`/p/${P1}/1`)) throw new Error("panel after Back: " + p.frame);
        same(await page.evaluate((id) => pgc.id === id && pgcFrame() === document.querySelector("#sp-body iframe.pgs-frame"), P1), true, "the comments came back to the panel");
        await page.click("#sp-pgback");
      });

      await check(`${tag}: Doc, Diff and Status are unchanged; the arrow keys step through all four`, async () => {
        await page.focus("#sp-tab-pages");
        await page.keyboard.press("ArrowRight");
        same((await panel()).tab, "doc", "ArrowRight from Pages wraps to Doc");
        await page.keyboard.press("ArrowLeft");
        same((await panel()).tab, "pages", "ArrowLeft from Doc wraps to Pages");
        await page.waitForSelector("#sp-pgl");
        await page.keyboard.press("ArrowLeft");
        same((await panel()).tab, "status", "ArrowLeft from Pages is Status");
        await page.waitForSelector("#sp-st .st", { state: "visible" });
        same(await page.locator("#sp-body iframe, #sp-pgl").count(), 0, "Pages gone from the body");
        await page.click("#sp-tab-diff");
        same((await panel()).tab, "diff", "Diff");
        same(await page.locator("#sp-body.sp-flush").count(), 0, "Diff has the body's padding back");
        await page.click("#sp-tab-pages");
        await page.waitForSelector("#sp-pgl");
        await page.keyboard.press("Escape");
        same((await panel()).open, false, "Escape closes the panel");
        await page.click("#sp-btn");
        same((await panel()).tab, "pages", "reopens on Pages");
      });

      await check(`${tag}: no page errors`, async () => { same(errs, [], "page errors"); });
    } finally {
      await browser.close();
    }
  }
} finally {
  fs.rmSync(work, { recursive: true, force: true });
}

const bad = results.filter((r) => !r.ok);
console.log(`pagessplit: ${results.length - bad.length}/${results.length} ok`);
process.exit(bad.length ? 1 : 0);
