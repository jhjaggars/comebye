// The Page stays put while it is reviewed: selecting text, opening the comment box, typing, saving a comment
// (the tray appears), cancelling and removing it must not move or resize the Page's frame, nor reflow or scroll
// the text inside it. The comment strip and tray float over the frame instead of taking room from it.
// Reference boxes: the frame's own bounding box in the app, and a paragraph's box and the scroll offset inside it.
//
//   (a part of pagereplyjourney.mjs) pagefixed BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME)
//
// A part of pagereplyjourney.mjs (journey.mjs): the script as it was, as the body of run(process); `process` is the one the
// journey gives it (its own argv and env), and process.exit ends the part.
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

export async function run(process) {
const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("pagefixed.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "pagefixed-ui-"));
const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w4:p1" };
const cli = (args) => execFileSync(bin, args, { env, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false, err: e.message }); console.log("FAIL " + name + "\n     " + e.message.split("\n").join("\n     ")); }
}

const filler = Array.from({ length: 40 }, (_, i) => `<p>Filler paragraph ${i}: the archive moves in stages and each stage has an owner and a rollback.</p>`).join("");
const f = path.join(work, "long.html");
fs.writeFileSync(f, `<!doctype html><html><head><meta charset="utf-8"><title>Fixed review</title><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font:16px/1.5 sans-serif;margin:16px;background:#fff;color:#111"><h1>Plan: move the archive</h1>
<p id="a">We will cut over on Friday without a dry run. Reads fall back to the old store for seven days, and then the old store is removed for good and nobody can go back to it.</p>
<p id="b">The cost is about four gigabytes a month at today's volume.</p>${filler}</body></html>`);

for (const width of (process.env.WIDTHS || "390,1280,3440").split(",").map(Number)) {
  const key = "uitest-fixed-" + width + "-" + Date.now();
  const id = cli(["publish", f, "--key", key]).match(/pg_[0-9a-f]{10}/)[0];
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark, bypassCSP: false });
  const tag = `${width} (${dark ? "dark" : "light"})`;
  await page.goto(base + "/#/pages/" + id, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  let fr = null;
  for (let i = 0; i < 50 && !fr; i++) { fr = page.frames().find((x) => x.url().includes("/p/" + id)); if (!fr) await page.waitForTimeout(100); }
  if (!fr) throw new Error("no frame");
  await fr.waitForLoadState("load");
  await page.waitForTimeout(300);

  const measure = async () => {
    const box = await page.locator("iframe.pgs-frame").boundingBox();
    const inner = await fr.evaluate(() => {
      const r = document.getElementById("b").getBoundingClientRect();
      return { x: r.x, y: r.y, w: r.width, h: r.height, sy: scrollY, iw: innerWidth, ih: innerHeight };
    });
    return { fx: box.x, fy: box.y, fw: box.width, fh: box.height, ...inner };
  };
  const same = (a, b, step) => {
    const bad = Object.keys(a).filter((k) => Math.abs(a[k] - b[k]) > 1);
    if (bad.length) throw new Error(`${step}: the Page moved: ` + bad.map((k) => `${k} ${a[k]} -> ${b[k]}`).join(", "));
  };
  const select = (sel = "#a") => fr.evaluate((q) => { const r = document.createRange(); r.selectNodeContents(document.querySelector(q)); const s = getSelection(); s.removeAllRanges(); s.addRange(r); }, sel);
  const shot = (n) => page.screenshot({ path: `${out}/fixed-${n}-${width}.png` });

  await check(`fixed: select, open, type, save, cancel and remove leave the Page where it was at ${tag}`, async () => {
    const m0 = await measure();
    await shot("0-before");
    await select();
    await page.waitForSelector("#pgs-sel:not([hidden])", { timeout: 4000 });
    await page.waitForTimeout(150);
    same(m0, await measure(), "select"); await shot("1-selected");
    await page.click("#pgs-addc");
    await page.waitForSelector("#pgs-note");
    same(m0, await measure(), "open the comment box"); await shot("2-open");
    await page.fill("#pgs-note", "Do a dry run first, and keep the old store for a month.");
    same(m0, await measure(), "type");
    await page.click("#pgs-noteadd");
    await page.waitForSelector("#pgs-tray:not([hidden])");
    await page.waitForTimeout(150);
    same(m0, await measure(), "save"); await shot("3-saved");
    // A second pass on other text (a commented quote is not offered again): select, open, cancel.
    await select("p:nth-of-type(6)");
    await page.waitForSelector("#pgs-sel:not([hidden])");
    await page.click("#pgs-addc");
    await page.waitForSelector("#pgs-note");
    same(m0, await measure(), "open again");
    await page.click("#pgs-notecancel");
    await page.waitForSelector("#pgs-sel", { state: "hidden" });
    same(m0, await measure(), "cancel");
    // Remove the saved comment: the tray goes away.
    await page.click("#pgs-tray .pgs-cmtx");
    await page.waitForSelector("#pgs-tray", { state: "hidden" });
    await page.waitForTimeout(150);
    same(m0, await measure(), "remove the comment");
  });

  await check(`fixed: the strip and the tray stay on screen and the app does not scroll sideways at ${tag}`, async () => {
    await select("p:nth-of-type(9)");
    await page.waitForSelector("#pgs-sel:not([hidden])");
    await page.click("#pgs-addc");
    await page.fill("#pgs-note", "Check.");
    const s = await page.locator("#pgs-sel").boundingBox();
    if (s.x < -1 || s.x + s.width > width + 1 || s.y < 0 || s.y + s.height > (width > 500 ? 800 : 844) + 1) throw new Error("the strip is off screen: " + JSON.stringify(s));
    await page.click("#pgs-noteadd");
    await page.waitForSelector("#pgs-tray:not([hidden])");
    const t = await page.locator("#pgs-tray").boundingBox();
    if (t.x < -1 || t.x + t.width > width + 1) throw new Error("the tray is off screen: " + JSON.stringify(t));
    const sw = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth }));
    if (sw.sw > sw.cw) throw new Error("the app scrolls sideways");
  });
  await browser.close();
}

const failed = results.filter((r) => !r.ok);
console.log(`pagefixed: ${results.length - failed.length}/${results.length} passed`);
process.exit(failed.length ? 1 : 0);
}
