// Text fields and iOS zoom: iOS Safari zooms the page in when a text field under 16px takes focus, and leaves it zoomed.
// On a touch screen (pointer: coarse) every text field (input, textarea, select, the composer's contenteditable) has a
// computed font size of at least 16px, at both ends of Settings > Appearance > Text size (Small and Larger). A desktop with
// a mouse is unchanged: at 1280 every field's font size and box are the same with the rule taken out (the
// @media (pointer: coarse) block in web/index.html that sets "--fs-11: 16px").
//
// Per screen and sheet with a field, at 390 x 844 with touch: every field in the document is read, shown or not (a
// field in a closed <details> or a hidden sheet still has a computed size), and the screen's own fields (EXPECT) must be
// there, so a screen that did not load fails instead of passing empty. Pinch zoom stays on: the viewport meta has no
// maximum-scale and no user-scalable=no.
//
//   node fieldzoom.mjs BASE OUT_DIR      (env: SCHEME; COMEBYE_BIN + COMEBYE_STATE_DIR to publish a page for the Pages
//                                         screen and its comment strip; FIELDZOOM_SHOTS=1 also writes before/after PNGs
//                                         of every screen whose fields changed, the "before" with the rule taken out)
import { launch, clickStable, settled } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const tag = dark ? "dark" : "light";
const SHOTS = !!process.env.FIELDZOOM_SHOTS;
let failed = 0;
async function check(name, fn) {
  try { await fn(); console.log("ok   " + name); }
  catch (e) { failed++; console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const IPHONE = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1";
const SIZES = ["small", "larger"]; // the smallest and the largest text size (web/themes.js RSS)

// In the page: every text field, with its computed font size, whether it is shown, and its box.
const FIELDS = (scrollTo = -1) => {
  const NOT_TEXT = ["checkbox", "radio", "range", "color", "file", "button", "submit", "reset", "image", "hidden"];
  const label = (el) => {
    const id = el.id ? "#" + el.id : "";
    const cls = [...el.classList].slice(0, 2).map((c) => "." + c).join("");
    const extra = el.dataset.f ? `[data-f=${el.dataset.f}]` : "";
    return `${el.localName}${id}${cls}${extra}`;
  };
  const els = [...document.querySelectorAll("input, textarea, select, [contenteditable]")]
    .filter((el) => (el.localName === "input" ? !NOT_TEXT.includes(el.type) : el.localName === "textarea" || el.localName === "select" || el.getAttribute("contenteditable") !== "false"));
  if (scrollTo >= 0 && els[scrollTo]) els[scrollTo].scrollIntoView({ block: "center" });
  return els.map((el) => {
    const r = el.getBoundingClientRect(), cs = getComputedStyle(el);
    return { label: label(el), fs: parseFloat(cs.fontSize), shown: !!el.getClientRects().length && cs.visibility !== "hidden", box: [r.left, r.top, r.width, r.height].map((v) => Math.round(v * 100) / 100).join(",") };
  });
};

// Take the field rule out of the stylesheet (on = false) or put it back (on = true).
const TOGGLE = (on) => {
  for (const sh of document.styleSheets) {
    let rules; try { rules = sh.cssRules; } catch { continue; }
    if (on) { if (window.__fzRule && window.__fzRule.sh === sh) { sh.insertRule(window.__fzRule.text, window.__fzRule.i); window.__fzRule = null; return true; } continue; }
    for (let i = rules.length - 1; i >= 0; i--) {
      const r = rules[i];
      if (r instanceof CSSMediaRule && /pointer:\s*coarse/.test(r.conditionText) && r.cssText.includes("--fs-11: 16px")) { window.__fzRule = { sh, i, text: r.cssText }; sh.deleteRule(i); return true; }
    }
  }
  return false;
};
const frames = (page) => page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));

// ---- pages: one with two versions (the version picker) and a paragraph to comment on ----
let pageId = "";
if (process.env.COMEBYE_BIN && process.env.COMEBYE_STATE_DIR) {
  const live = (await (await fetch(base + "/api/agents")).json()).agents.filter((a) => a.pane_id);
  const work = fs.mkdtempSync(path.join(os.tmpdir(), "fieldzoom-ui-"));
  const f = path.join(work, "p.html");
  const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: process.env.COMEBYE_STATE_DIR, HERDR_PANE_ID: live[0].pane_id };
  const key = "fieldzoom-" + Date.now();
  for (const v of [1, 2]) {
    fs.writeFileSync(f, `<!doctype html><html><head><meta charset="utf-8"><title>Field zoom ${v}</title><meta name="viewport" content="width=device-width,initial-scale=1"></head><body style="font:16px/1.5 sans-serif;margin:16px"><h1>Field zoom</h1><p id="a">We will cut over on Friday without a dry run (version ${v}).</p></body></html>`);
    const o = execFileSync(process.env.COMEBYE_BIN, ["publish", f, "--key", key, "--title", "Field zoom"], { env, encoding: "utf8" });
    pageId = (o.match(/pg_[0-9a-f]{10}/) || [""])[0];
  }
  fs.rmSync(work, { recursive: true, force: true });
}
// A to-do, so the To do screen has an item to open (the hub may have none).
await fetch(base + "/api/todos", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ question: "Field zoom test " + Date.now(), kind: "decision", no_wait: true, context: "seeded by fieldzoom.mjs" }) });

// The provisioner's Prices tab needs a hub with peers and a provisioner node: canned answers for those, in the browser.
// The event stream is held quiet, or its agents events would put the one real host back.
async function fakeProvisioner(page) {
  await page.route(/\/api\/events/, (r) => r.fulfill({ status: 200, contentType: "text/event-stream", body: "retry: 60000\n\n" }));
  await page.route(/\/api\/agents(\?|$)/, async (r) => {
    const res = await r.fetch(); const j = await res.json();
    if (j.hosts && j.hosts.length < 2) j.hosts.push({ name: "prov", status: "ok", version: "dev", agents: 0 });
    await r.fulfill({ json: j });
  });
  await page.route(/\/api\/provision\/nodes/, (r) => r.fulfill({ json: { enabled: true, csrf: "x", nodes: [{ name: "prov", default: true, state: "ok", preflight: { region: "us-east-1", regions: ["us-east-1", "us-west-2"] } }] } }));
  await page.route(/\/api\/provision\/catalog/, (r) => r.fulfill({ json: { items: [], region: "us-east-1" } }));
  await page.route(/\/api\/provision\/hosts/, (r) => r.fulfill({ json: { hosts: [] } }));
}

// ---- screens ----
// Each: [name, hash, ready selector, EXPECT (fields that must be there), setup?]. setup runs once the screen is ready.
const agentOpen = async (page) => { await clickStable(page, ".agent.idle"); await page.waitForSelector("#prompt-text", { state: "visible" }); };
const spTab = (t) => async (page) => {
  await agentOpen(page);
  await page.locator("#sp-btn").click();
  await page.waitForSelector(".sp-tab", { state: "visible" });
  await page.locator("#sp-tab-" + t).click();
  await page.waitForSelector(t === "doc" ? "#sp-path" : "#sp-bar", { state: "attached" });
};
const SCREENS = [
  ["list", "#/list", ".agent", ["#lf-q"]],
  ["agent composer", "#/list", ".agent", ["#prompt-text"], agentOpen],
  ["side panel doc", "#/list", ".agent", ["#sp-file", "#sp-toc", "#sp-path"], spTab("doc")],
  ["side panel diff", "#/list", ".agent", [], spTab("diff")],
  ["palette", "#/list", ".agent", ["#pal-q"], async (page) => {
    if (await page.locator("#btn-cmds").isVisible()) await page.locator("#btn-cmds").click(); else await page.keyboard.press("Control+k");
    await page.waitForSelector("#pal-q", { state: "visible" });
  }],
  ["new agent", "#/list", ".agent", [], async (page) => { await page.locator("#btn-launch").click(); await page.waitForSelector("#launch", { state: "visible" }); }],
  ["to-do item", "#/list", "#todo .td-toggle", [], async (page) => {
    if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await clickStable(page, "#todo .td-toggle");
    await clickStable(page, "#todo .td-row");
    await page.waitForSelector("#todo .td-item.open", { state: "visible" });
  }],
  ["settings", "#/settings", "#sx-name", ["#sx-name"]],
  ["settings theme", "#/settings", "#sx-name", ["#theme-panel select"], async (page) => {
    await page.locator("#sx-row-theme").click();
    await page.waitForSelector("#theme-panel:not([hidden])", { state: "visible" });
  }],
  ["search", "#/search", "#rc-q", ["#rc-q", ".rc-chip"]],
  ["archived", "#/archived", "#ar-q", ["#ar-q"]],
  ["usage prices", "#/usage", "#us-pricedet summary", ["#us-pricebody input", "#us-newmodel"], async (page) => {
    await page.locator("#us-pricedet summary").click();
    await page.waitForSelector("#us-pricebody input", { state: "visible" });
  }],
  ["pages", "#/pages", "#pgs-q", ["#pgs-q", "#pgs-sort"]],
  ...(pageId ? [["pages comment", "#/pages/" + pageId, "iframe.pgs-frame", ["#pgs-ver", "#pgs-note"], async (page) => {
    let fr = null;
    for (let i = 0; i < 80 && !fr; i++) { fr = page.frames().find((x) => x.url().includes("/p/" + pageId)); if (!fr) await page.waitForTimeout(100); }
    if (!fr) throw new Error("the page's frame never loaded");
    await fr.waitForLoadState("load");
    await fr.evaluate(() => { const r = document.createRange(); r.selectNodeContents(document.getElementById("a")); const s = getSelection(); s.removeAllRanges(); s.addRange(r); });
    await page.waitForSelector("#pgs-sel:not([hidden])", { timeout: 5000 });
    await page.click("#pgs-addc");
    await page.waitForSelector("#pgs-note", { state: "visible" });
  }]] : []),
  ["provisioner prices", "#/peers/~prices", "#pz-via", ["#pz-via", "#pz-region"], null, fakeProvisioner],
];

const { browser, ctx: first } = await launch({ dark });
await first.close().catch(() => {});
try {
  // The viewport meta lets the page zoom: no maximum-scale, no user-scalable=no.
  await check(`${tag}: the viewport meta leaves pinch zoom on`, async () => {
    const html = await (await fetch(base + "/")).text();
    const m = html.match(/<meta[^>]+name="viewport"[^>]*>/i);
    if (!m) throw new Error("no viewport meta");
    if (/maximum-scale|user-scalable/i.test(m[0])) throw new Error("the viewport meta turns zoom off: " + m[0]);
  });

  // ---- touch at 390: every field at least 16px, at both ends of the text size ----
  const seen = new Map(); // label -> font sizes seen, for the summary
  for (const rs of SIZES) {
    for (const [screen, hash, ready, expect, setup, before] of SCREENS) {
      await check(`touch 390x844 ${tag} text size ${rs}: ${screen}: every text field is at least 16px`, async () => {
        const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, bypassCSP: true, colorScheme: dark ? "dark" : "light", userAgent: IPHONE });
        try {
          await ctx.addInitScript((rs) => { try { localStorage.setItem("comebye.rs", rs); } catch {} }, rs);
          const page = await ctx.newPage();
          page.setDefaultTimeout(8000);
          if (before) await before(page);
          await page.goto(base + "/" + hash, { waitUntil: "load" });
          await page.waitForSelector(ready, { state: "attached" });
          if (setup) await setup(page);
          await settled(page);
          const env = await page.evaluate(() => ({ coarse: matchMedia("(pointer: coarse)").matches, rs: document.documentElement.getAttribute("data-rs") }));
          if (!env.coarse) throw new Error("this context is not (pointer: coarse)");
          if (env.rs !== rs) throw new Error(`the text size is ${env.rs}, not ${rs}`);
          const fields = await page.evaluate(FIELDS);
          for (const sel of expect) if (!(await page.locator(sel).count())) throw new Error(`no ${sel}: the screen did not load`);
          if (!fields.length) throw new Error("no text field on this screen");
          for (const f of fields) { if (!seen.has(f.label)) seen.set(f.label, new Set()); seen.get(f.label).add(f.fs); }
          const small = fields.filter((f) => f.fs < 16);
          if (SHOTS && rs === "small") {
            if (!(await page.evaluate(TOGGLE, false))) throw new Error("the field rule is not in the stylesheet");
            await frames(page);
            const was = await page.evaluate(FIELDS);
            const changed = fields.filter((f, i) => was[i] && was[i].shown && (was[i].fs !== f.fs || was[i].box !== f.box));
            if (changed.length) {
              const n = screen.replace(/\W+/g, "-");
              // The first changed field that is not the list's filter, in view for the pictures (the rule is out: "before").
              const pick = changed.find((f) => !f.label.includes("lf-q")) || changed[0];
              await page.evaluate(FIELDS, fields.indexOf(pick)); await frames(page);
              await page.screenshot({ path: `${out}/fieldzoom-${n}-before-${tag}.png` });
              await page.evaluate(TOGGLE, true); await frames(page);
              await page.screenshot({ path: `${out}/fieldzoom-${n}-after-${tag}.png` });
              console.log(`     shots ${n}: ${[...new Set(changed.map((f) => `${f.label} ${was[fields.indexOf(f)].fs}→${f.fs}px`))].join(", ")}`);
            } else await page.evaluate(TOGGLE, true);
          }
          if (small.length) {
            await page.screenshot({ path: `${out}/fieldzoom-${screen.replace(/\W+/g, "-")}-${rs}-${tag}.png` }).catch(() => {});
            throw new Error(small.map((f) => `${f.label}${f.shown ? "" : " (not shown)"}: ${f.fs}px`).join("\n"));
          }
        } finally { await ctx.close().catch(() => {}); }
      });
    }
  }
  console.log("     fields read: " + [...seen].map(([l, s]) => `${l} ${[...s].join("/")}px`).join(", "));

  // ---- a mouse at 1280: nothing changed ----
  const DESK = SCREENS.filter(([n]) => ["agent composer", "side panel doc", "palette", "settings", "search", "usage prices", "pages", "pages comment", "archived"].includes(n));
  for (const rs of SIZES) {
    for (const [screen, hash, ready, expect, setup] of DESK) {
      await check(`desktop 1280x800 ${tag} text size ${rs}: ${screen}: with a mouse the field rule changes nothing`, async () => {
        const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, deviceScaleFactor: 1, bypassCSP: true, colorScheme: dark ? "dark" : "light" });
        try {
          await ctx.addInitScript((rs) => { try { localStorage.setItem("comebye.rs", rs); } catch {} }, rs);
          const page = await ctx.newPage();
          page.setDefaultTimeout(8000);
          await page.goto(base + "/" + hash, { waitUntil: "load" });
          await page.waitForSelector(ready, { state: "attached" });
          if (setup) await setup(page);
          await settled(page);
          if (await page.evaluate(() => matchMedia("(pointer: coarse)").matches)) throw new Error("a mouse context matched (pointer: coarse)");
          for (const sel of expect) if (!(await page.locator(sel).count())) throw new Error(`no ${sel}: the screen did not load`);
          // With, without, with: a page still filling in changes between the two "with" reads, and is read again.
          let a, b;
          for (let tries = 0; ; tries++) {
            a = await page.evaluate(FIELDS);
            if (!(await page.evaluate(TOGGLE, false))) throw new Error("the field rule is not in the stylesheet");
            await frames(page);
            b = await page.evaluate(FIELDS);
            await page.evaluate(TOGGLE, true);
            await frames(page);
            const c = await page.evaluate(FIELDS);
            if (JSON.stringify(a) === JSON.stringify(c)) break;
            if (tries === 4) throw new Error("the page never held still for two reads");
            await page.waitForTimeout(300);
          }
          const diff = a.filter((x, i) => JSON.stringify(x) !== JSON.stringify(b[i]));
          if (a.length !== b.length || diff.length) throw new Error("fields differ:\n" + diff.slice(0, 8).map((x) => `${x.label} ${x.fs}px ${x.box}, without: ${JSON.stringify(b[a.indexOf(x)])}`).join("\n"));
          if (!a.length) throw new Error("no field read: the screen did not load");
        } finally { await ctx.close().catch(() => {}); }
      });
    }
  }
} finally {
  await browser.close();
}
console.log(failed ? `fieldzoom: ${failed} failed` : "fieldzoom: all passed");
process.exit(failed ? 1 : 0);
