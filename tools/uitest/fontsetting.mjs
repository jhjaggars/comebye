// The Font setting beside Theme (web/themes.js FONTS, the picker in web/app.js), with the server's
// Content-Security-Policy in force: each choice sets <html data-font>, persists across a reload, is applied
// before first paint; the default loads no font file; the CSP lets the woff2 files load. Screenshots of each
// choice at 390 and 1280 (the run's colour scheme) land in OUT_DIR.
//
//   node fontsetting.mjs BASE OUT_DIR      (env: SCHEME)
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
const log = [];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const same = (got, want, what) => { if (got !== want) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };

const OPTS = [
  { label: "System sans", id: "", shot: "system" },
  { label: "Serif headings", id: "serif-heads", shot: "serif-heads" },
  { label: "Serif reading", id: "serif-reading", shot: "serif-reading" },
  { label: "Geist Sans + Mono", id: "geist", shot: "geist" },
];
const CTX = "## A heading\n\nSome **bold** and *italic* reading text with `inline code` that wraps over a couple of lines so the face can be judged.\n\n- one\n- two\n\n```sh\nls -la /etc\n```\n";
const todo = await fetch(base + "/api/todos", { method: "POST", headers: { "content-type": "application/json" },
  body: JSON.stringify({ question: "Which font reads better? " + Date.now(), kind: "decision", no_wait: true, context: CTX }) }).then((r) => r.json());
const tid = todo.todo.id;

async function session(width) {
  const { browser, ctx, page } = await launch({ width, height: width < 500 ? 844 : 800, dark, bypassCSP: false });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  page.on("console", (m) => { if (/Content Security Policy|violat|Refused/i.test(m.text())) log.push("csp: " + m.text()); });
  await page.addInitScript(() => {
    document.addEventListener("DOMContentLoaded", () => { window.__fontAtDCL = document.documentElement.getAttribute("data-font"); });
  });
  return { browser, ctx, page };
}
const fonts = (page) => page.evaluate(() => performance.getEntriesByType("resource").map((e) => e.name).filter((n) => /\.woff2/.test(n)).map((n) => n.replace(/^.*\//, "")));
const attr = (page) => page.evaluate(() => document.documentElement.getAttribute("data-font"));
async function openTodo(page) {
  await page.goto(base + "/#/todo", { waitUntil: "load" });
  await page.waitForSelector(".td-item", { state: "attached" });
  if (!(await page.locator(`.td-item[data-id="${tid}"].open .td-ctx:visible`).count())) {
    await page.locator(`.td-item[data-id="${tid}"] > .td-row:visible`).first().click();
  }
  await page.locator(`.td-item[data-id="${tid}"] .td-ctx:visible`).first().waitFor();
}
async function pick(page, label) {
  await page.locator("#btn-fleet").click();
  await page.locator("#f-font").click();
  await page.locator("#font-panel").waitFor({ state: "visible" });
  await page.locator(`#font-panel .tp-opt:has(.tp-name:text-is("${label}"))`).click();
  await page.locator("#font-panel").waitFor({ state: "hidden" });
}

for (const width of [390, 1280]) {
  const { browser, page } = await session(width);
  await check(`${width}: default has no data-font and loads no font file`, async () => {
    await openTodo(page);
    same(await attr(page), null, "data-font");
    await page.evaluate(() => document.fonts.ready);
    same(JSON.stringify(await fonts(page)), "[]", "woff2 requests");
    same(await page.evaluate(() => getComputedStyle(document.body).fontFamily), '-apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif', "body font");
  }).catch(() => {});
  for (const o of OPTS) {
    await check(`${width}: picking "${o.label}" sets the variant and survives a reload`, async () => {
      await openTodo(page);
      await pick(page, o.label);
      same(await attr(page), o.id || null, "data-font after picking");
      same(await page.evaluate(() => localStorage.getItem("comebye.font")), o.id, "stored");
      await page.evaluate(() => document.fonts.ready);
      await page.reload({ waitUntil: "load" });
      same(await page.evaluate(() => window.__fontAtDCL), o.id || null, "data-font at DOMContentLoaded (before first paint)");
      same(await attr(page), o.id || null, "data-font after reload");
      await openTodo(page);
      await page.evaluate(() => document.fonts.ready);
      await page.waitForTimeout(300);
      await page.locator("#btn-fleet").click();
      same((await page.locator("#f-font-val").textContent()).trim(), o.label, "menu value");
      await page.keyboard.press("Escape");
      const want = { "": [], "serif-heads": ["GeistSerif-Regular.woff2"], "serif-reading": ["GeistSerif-Regular.woff2"], "geist": ["GeistSans-Variable.woff2"] }[o.id];
      const got = await fonts(page);
      for (const f of want) if (!got.includes(f)) throw new Error(`expected ${f} to load, got ${JSON.stringify(got)}`);
      if (!o.id && got.length) throw new Error("the default loaded " + got);
      const fam = await page.evaluate((tid) => { const it = document.querySelector(`.td-item[data-id="${tid}"]`); return {
        head: getComputedStyle(document.querySelector("header h1")).fontFamily,
        ctx: getComputedStyle(it.querySelector(".td-ctx")).fontFamily,
        q: getComputedStyle(it.querySelector(".td-q")).fontFamily,
        title: getComputedStyle(document.querySelector(".td-title")).fontFamily,
        ui: getComputedStyle(document.body).fontFamily,
      }; }, tid);
      const serif = (f) => /Geist Serif/.test(f);
      same(serif(fam.head), !!o.id && o.id !== "geist", "header is serif");
      same(serif(fam.q), !!o.id && o.id !== "geist", "open question is serif");
      same(serif(fam.ctx), o.id === "serif-reading", "context is serif");
      same(serif(fam.title), false, "card titles under 15px stay sans");
      same(/^"?Geist"?,/.test(fam.ui), o.id === "geist", "UI is Geist");
      await page.screenshot({ path: `${out}/font-${o.shot}-${width}.png` });
    });
  }
  await check(`${width}: the server's CSP allows font-src 'self' and serves the font files`, async () => {
    const r = await page.request.get(base + "/vendor/fonts/GeistSerif-Regular.woff2");
    same(r.status(), 200, "status");
    same(r.headers()["content-type"], "font/woff2", "content type");
    const csp = (await page.request.get(base + "/")).headers()["content-security-policy"] || "";
    if (!/font-src 'self'/.test(csp)) throw new Error("no font-src 'self' in: " + csp);
    if (log.some((l) => l.startsWith("csp:"))) throw new Error(log.join("\n"));
  });
  await page.evaluate(() => localStorage.removeItem("comebye.font"));
  await browser.close();
}

await fetch(base + `/api/todos/${tid}/drop`, { method: "POST", headers: { "content-type": "application/json" }, body: "{}" }).catch(() => {});
const bad = results.filter((r) => !r).length + (log.length ? 1 : 0);
if (log.length) console.log("page errors:\n" + log.join("\n"));
console.log(`${results.filter(Boolean).length}/${results.length} fontsetting checks passed`);
process.exit(bad ? 1 : 0);
