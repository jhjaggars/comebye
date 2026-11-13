// Revision marks: an agent puts data-revision="vN" on the blocks it changed in
// version N. The viewer's Page menu offers to outline them (only when the page
// has marks and is past version 1), and the outline can be turned off again.
//
//   node pagerevisions.mjs BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("pagerevisions.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "pagerevisions-ui-"));
const results = [];
const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w5:p1" };
const cli = (args) => execFileSync(bin, args, { env, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false, err: e.message }); console.log("FAIL " + name + "\n     " + e.message.split("\n").join("\n     ")); }
}

const html = (a, b, mark) => `<!doctype html><html><head><meta charset="utf-8"><title>Revision plan</title><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font:16px/1.5 sans-serif;margin:16px"><h1>Plan</h1>
<p id="a">${a}</p><p id="b"${mark ? ' data-revision="v2"' : ""}>${b}</p></body></html>`;
const f = path.join(work, "p.html");
const stamp = Date.now();
const put = (key, a, b, mark) => { fs.writeFileSync(f, html(a, b, mark)); return cli(["publish", f, "--key", key]).match(/pg_[0-9a-f]{10}/)[0]; };

for (const width of [1280, 390]) {
  const tag = `${width} (${dark ? "dark" : "light"})`;
  const key = `uitest-revisions-${width}-${stamp}`, plain = `uitest-revplain-${width}-${stamp}`;
  put(key, "Intro text.", "Cost is four gigabytes.", false);
  const id = put(key, "Intro text.", "Cost is five gigabytes, with versions.", true);
  put(plain, "Plain one.", "Plain two.", false);
  const pid = put(plain, "Plain one.", "Plain two, edited.", false);
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark, bypassCSP: false });
  const frameOf = async (pgId) => {
    await page.goto(base + "/#/pages/" + pgId, { waitUntil: "load" });
    await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
    let fr = null;
    for (let i = 0; i < 50 && !fr; i++) { fr = page.frames().find((x) => x.url().includes("/p/" + pgId)); if (!fr) await page.waitForTimeout(100); }
    await fr.waitForLoadState("load");
    await page.waitForTimeout(400);
    return fr;
  };
  const openMenu = async () => { await page.click("#pgs-menub"); await page.waitForSelector("#pgs-menu:not([hidden])"); };

  await check(`revisions: a version with marked blocks offers to outline them at ${tag}`, async () => {
    const fr = await frameOf(id);
    await openMenu();
    await page.waitForTimeout(600);
    await page.screenshot({ path: `${out}/revisions-menu-${width}.png` });
    const t = await page.locator("#pgs-chg").textContent();
    if (await page.locator("#pgs-chg").isHidden() || !/changed in v2/.test(t)) throw new Error("menu item: " + t);
    await page.click("#pgs-chg");
    await page.waitForTimeout(300);
    const m = await fr.evaluate(() => ({ b: getComputedStyle(document.getElementById("b")).outlineStyle, a: getComputedStyle(document.getElementById("a")).outlineStyle, attr: document.getElementById("b").hasAttribute("data-comebye-changed") }));
    if (m.b !== "dashed" || !m.attr || m.a !== "none") throw new Error("outline: " + JSON.stringify(m));
    await page.screenshot({ path: `${out}/revisions-on-${width}.png` });
    await openMenu();
    if (!/Hide/.test(await page.locator("#pgs-chg").textContent())) throw new Error("the item does not offer to hide");
    await page.click("#pgs-chg");
    await page.waitForTimeout(300);
    if (await fr.evaluate(() => document.getElementById("b").hasAttribute("data-comebye-changed"))) throw new Error("the outline stayed");
  });

  await check(`revisions: a page with no marks does not offer it at ${tag}`, async () => {
    await frameOf(pid);
    await openMenu();
    if (!(await page.locator("#pgs-chg").isHidden())) throw new Error("the item is shown");
  });

  await check(`revisions: version 1 does not offer it at ${tag}`, async () => {
    await page.goto(base + "/#/pages/" + id + "/1", { waitUntil: "load" });
    await page.waitForTimeout(1200);
    await openMenu();
    if (!(await page.locator("#pgs-chg").isHidden())) throw new Error("the item is shown on v1");
  });
  await browser.close();
}

fs.rmSync(work, { recursive: true, force: true });
const failed = results.filter((r) => !r.ok);
console.log(`pagerevisions: ${results.length - failed.length}/${results.length} passed`);
process.exit(failed.length ? 1 : 0);
