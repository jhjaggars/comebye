#!/usr/bin/env node
// Screenshots (and measures) every empty state: node empties.mjs BASE OUTDIR [--widths 1280,390] [--themes dark,light,nord]
// Each state is forced with mocked API answers, so it does not depend on the fixtures.
import { launch } from "./lib.mjs";
import fs from "node:fs";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const arg = (k, d) => { const i = process.argv.indexOf("--" + k); return i > 0 ? process.argv[i + 1].split(",") : d; };
const widths = arg("widths", ["1280", "390"]).map(Number);
const themes = arg("themes", ["dark", "light"]);
fs.mkdirSync(out, { recursive: true });

const EMPTY_LEDGER = { plans: [], entries: [], marks: [], roles: {} };
const ANSWERED = { todos: [{ id: "t1", kind: "question", question: "Which one?", state: "answered", answer: "ok 1", context: "", agent_name: "w", created_at: new Date().toISOString() }], open: 0, answered: 1 };
// name: [hash, mocks, after(page)]
export const STATES = {
  "todo-none":      { hash: "#/todo", todos: { todos: [], open: 0, answered: 0 }, ledger: true, sel: "#pt-body .empty, #pt-body .empty-state" },
  "todo-answered":  { hash: "#/todo", todos: ANSWERED, ledger: true, sel: "#pt-body .tds-none, #pt-body .td-none" },
  "status-none":    { hash: "#/status", todos: { todos: [], open: 0, answered: 0 }, ledger: true, sel: ".st-sum" },
  "status-noledger": { hash: "#/status", ledger: false, sel: "#pt-body .empty, #pt-body .empty-state" },
  "search-noquery": { hash: "#/transcripts", sel: ".rc-empty, .empty-state", any: true },
  "search-noresult": { hash: "#/transcripts?q=zzzqqqxxnomatch", sel: ".rc-empty, .empty-state", any: true },
  "list-empty":     { hash: "", ledger: true, eval: "state.agents = []; renderList()", sel: "#list .empty, #list .empty-state" },
  "list-host-empty": { hash: "", ledger: true, eval: "state.hostFilter = 'peerbox'; state.agents = state.agents.filter((a) => a.host !== 'peerbox'); renderList()", sel: "#list .empty, #list .empty-state" },
  "review-empty":   { hash: "", ledger: true, eval: "openReview(); await new Promise(r => setTimeout(r, 600)); review.items = []; renderReview()", sel: "#rv-list .ln-empty, #rv-list .empty-state" },
  "sweep-empty":    { hash: "", ledger: true, eval: "openSweep()", sel: "#sweep-list .ln-empty, #sweep-list .empty-state" },
  "briefing-none":  { hash: "#/briefings", ledger: true, sel: ".br-empty, .empty-state" },
  "notify-panel":   { hash: "", ledger: true, click: "#btn-notify", sel: "#notify-status" },
  "launch-dirs":    { hash: "", ledger: true, click: "#btn-launch", sel: ".ln-empty, .empty-state", any: true },
};
const only = arg("only", null);

const browser0 = null;
for (const w of widths) for (const th of themes) {
  const dark = th === "dark";
  const { browser, ctx, page } = await launch({ width: w, dark: th === "light" || th === "dark" ? dark : true });
  await ctx.addInitScript((t) => { if (!["dark", "light"].includes(t)) localStorage.setItem("comebye.theme", t); }, th);
  for (const [name, s] of Object.entries(STATES)) {
    if (only && !only.includes(name)) continue;
    if ((name.startsWith("todo") || name.startsWith("status") || name === "briefing-none") && w < 1000 && name !== "todo-none") continue;
    const p = await ctx.newPage();
    await p.setViewportSize({ width: w, height: w <= 500 ? 844 : 800 });
    await p.route(/\/api\/ledger$/, (r) => s.ledger === false ? r.fulfill({ status: 503, json: { error: "no ledger" } }) : r.fulfill({ json: EMPTY_LEDGER }));
    if (s.todos) await p.route(/\/api\/todos$/, (r) => r.fulfill({ json: s.todos }));
    if (name === "briefing-none") await p.route(/\/api\/brief$/, (r) => r.fulfill({ status: 404, json: { error: "none" } }));
    await p.goto(base + "/" + s.hash);
    await p.waitForTimeout(900);
    if (s.eval) await p.evaluate(`(async () => { ${s.eval} })()`);
    if (s.click) await p.click(s.click).catch(() => {});
    await p.waitForTimeout(700);
    const file = path.join(out, `${name}-${w}-${th}.png`);
    await p.screenshot({ path: file });
    const m = await p.evaluate((sel) => {
      const el = [...document.querySelectorAll(sel)].find((e) => e.offsetParent !== null);
      if (!el) return null;
      const r = el.getBoundingClientRect(), cs = getComputedStyle(el), pr = el.parentElement.getBoundingClientRect();
      return { left: Math.round(r.left - pr.left), padL: parseFloat(cs.paddingLeft), align: cs.textAlign, mid: Math.round((r.left + r.right) / 2 - (pr.left + pr.right) / 2), w: Math.round(r.width), pw: Math.round(pr.width), text: el.textContent.trim().slice(0, 50) };
    }, s.sel);
    console.log(name.padEnd(16), w, th.padEnd(6), JSON.stringify(m));
    await p.close();
  }
  await browser.close();
}
