// Comments on a page: the user selects text in a page, comments on it, and
// sends the batch. The page is untrusted, so the checks are as much about what
// the viewer refuses: a message from a nested frame, a message that names an
// agent or has a key too many, an oversized quote, a burst, all of which must
// leave the viewer as it was. A good selection shows the strip, the note goes
// to the tray, and Send files one To do item for the user.
//
//   node pagecomments.mjs BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("pagecomments.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "pagecomments-ui-"));
const results = [];
const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w2:p1" };
const cli = (args) => execFileSync(bin, args, { env, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();

async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false, err: e.message }); console.log("FAIL " + name + "\n     " + e.message.split("\n").join("\n     ")); }
}

const f = path.join(work, "plan.html");
fs.writeFileSync(f, `<!doctype html><html><head><meta charset="utf-8"><title>Comment plan</title><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font:16px/1.5 sans-serif;margin:16px"><h1>Plan: move the archive</h1>
<p id="a">We will cut over on Friday without a dry run. Reads fall back to the old store for seven days.</p>
<p id="b">The cost is about four gigabytes a month at today's volume.</p></body></html>`);
const id = cli(["publish", f, "--key", "uitest-comments"]).match(/pg_[0-9a-f]{10}/)[0];

const GOOD = { type: "comebye-selection", v: 1, quote: "cut over on Friday", before: "We will ", after: " without a dry run" };

for (const width of [1280, 390]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark, bypassCSP: false });
  const tag = `${width} (${dark ? "dark" : "light"})`;
  const log = [];
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await page.goto(base + "/#/pages/" + id, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  let fr = null;
  for (let i = 0; i < 50 && !fr; i++) { fr = page.frames().find((x) => x.url().includes("/p/" + id)); if (!fr) await page.waitForTimeout(100); }
  if (!fr) throw new Error("no frame");
  await fr.waitForLoadState("load");
  const stripHidden = () => page.locator("#pgs-sel").evaluate((el) => el.hidden);
  const post = (msg) => fr.evaluate((m) => parent.postMessage(m, "*"), msg);

  await check(`comments: the hub added the reporter to the served page at ${tag}`, async () => {
    const n = await fr.evaluate(() => document.scripts.length);
    if (n < 1) throw new Error("no script in the page");
    const src = await page.evaluate(async (u) => (await fetch(u)).text(), `/api/pages/${id}?body=1`);
    if (src.includes("comebye-selection")) throw new Error("the reporter is stored in the page source");
  });

  await check(`comments: forged and malformed messages leave the viewer as it was at ${tag}`, async () => {
    const bad = [
      { ...GOOD, agent: "uitest/w2:p1" },                       // a key too many (a recipient)
      { ...GOOD, v: 2 },
      { ...GOOD, type: "comebye-theme" },
      { type: "comebye-selection", v: 1, quote: "x", before: "" }, // a key missing
      { ...GOOD, quote: 7 },
      { ...GOOD, quote: "x".repeat(2000) },                     // too long
      "comebye-selection",
      null,
      [GOOD],
    ];
    for (const m of bad) await post(m);
    // From a frame inside the page: not the frame the app opened.
    await fr.evaluate((m) => new Promise((res) => {
      const i = document.createElement("iframe");
      i.srcdoc = "<script>parent.parent.postMessage(" + JSON.stringify(m) + ',"*")<\/script>';
      i.onload = () => res(); document.body.append(i);
    }), GOOD);
    await page.waitForTimeout(400);
    if (!(await stripHidden())) throw new Error("a bad message showed the comment strip");
    await page.waitForTimeout(1100); // the viewer takes at most 10 messages a second: let the burst pass
  });

  await check(`comments: select, comment, tray at ${tag}`, async () => {
    await fr.evaluate(() => { const r = document.createRange(); r.selectNodeContents(document.getElementById("a")); const s = getSelection(); s.removeAllRanges(); s.addRange(r); });
    await page.waitForSelector("#pgs-sel:not([hidden])", { timeout: 4000 });
    const q = await page.locator("#pgs-sel .pgs-selq").textContent();
    if (!q.includes("We will cut over on Friday")) throw new Error("strip shows " + q);
    await page.click("#pgs-addc");
    await page.fill("#pgs-note", "Do a dry run first.");
    await page.click("#pgs-noteadd");
    await page.waitForSelector("#pgs-tray:not([hidden])");
    await fr.evaluate(() => { const r = document.createRange(); r.selectNodeContents(document.getElementById("b")); const s = getSelection(); s.removeAllRanges(); s.addRange(r); });
    await page.waitForSelector("#pgs-sel:not([hidden])");
    await page.click("#pgs-addc");
    await page.fill("#pgs-note", "Is this with seven versions kept?");
    await page.click("#pgs-noteadd");
    const n = await page.locator("#pgs-tray .pgs-cmt").count();
    if (n !== 2) throw new Error("tray has " + n + " comments");
    const sw = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth }));
    if (sw.sw > sw.cw) throw new Error("the app scrolls sideways");
    const box = await page.locator("#pgs-tray").boundingBox();
    if (box.x + box.width > width + 1) throw new Error("the tray runs off the screen");
    await page.screenshot({ path: `${out}/comments-tray-${width}.png` });
  });

  await check(`comments: a page's script cannot make the viewer name a recipient at ${tag}`, async () => {
    const html = await page.locator("#pgs-tray").innerHTML();
    if (html.includes("<script") || html.includes("onerror")) throw new Error("markup from the page in the tray");
  });

  await check(`comments: Send files one To do item at ${tag}`, async () => {
    await page.click("#pgs-sendc");
    await page.waitForSelector("#pgs-tray[hidden]", { state: "attached", timeout: 6000 });
    const todos = await page.evaluate(async () => (await (await fetch("/api/todos?state=all")).json()).todos);
    const mine = todos.filter((t) => t.question.startsWith("Comments on Comment plan (v1)"));
    if (mine.length < 1) throw new Error("no to-do item: " + JSON.stringify(todos.map((t) => t.question)));
    const t = mine[mine.length - 1];
    if (!t.context.includes("> We will cut over on Friday") || !t.context.includes("Do a dry run first.") || t.kind !== "followup") throw new Error("item: " + JSON.stringify(t));
  });

  if (log.length) throw new Error("console: " + log.join("\n"));
  await browser.close();
}

fs.rmSync(work, { recursive: true, force: true });
const failed = results.filter((r) => !r.ok);
console.log(`pagecomments: ${results.length - failed.length}/${results.length} passed`);
process.exit(failed.length ? 1 : 0);
