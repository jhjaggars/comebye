// The page starters (`comebye pages new`): each one, made and published by
// the real CLI as an agent, is opened in the app's Pages viewer under the
// hub's real page CSP, at phone and desktop width. It must render without a
// console error or a blocked load, never scroll sideways, start in the
// scheme's Comebye colours, and follow a comebye-theme message (posted here
// the way the app's viewer posts it, from ComebyeThemes) to another theme.
//
//   node starters.mjs BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("starters.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "starters-ui-"));
const TYPES = ["doc", "mockup", "deck", "dashboard"];
const results = [];

// The CLI as a pi worker has it: nothing but PATH, HOME and where the hub is.
const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w2:p1" };
const cli = (args, input) => execFileSync(bin, args, { env, input, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] });

async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false, err: e.message }); console.log("FAIL " + name + "\n     " + e.message.split("\n").join("\n     ")); }
}

const ids = {};
await check("starters: pages new makes each type and publish takes it", async () => {
  if (!cli(["pages", "guide"]).startsWith("---\nname: comebye-pages\n")) throw new Error("pages guide is not the skill");
  for (const t of TYPES) {
    const html = cli(["pages", "new", "--type", t, "--title", `Starter ${t}`]);
    const f = path.join(work, t + ".html");
    fs.writeFileSync(f, html);
    ids[t] = cli(["publish", f, "--key", "uitest-starter-" + t]).trim().match(/pg_[0-9a-f]{10}/)[0];
  }
});

const rgb = (hex) => { const h = hex.replace("#", ""); return `rgb(${[0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16)).join(", ")})`; };
const startBg = rgb(dark ? "#101218" : "#f3f4f7");
const other = dark ? "gruvbox-light" : "gruvbox-dark";

for (const width of [1280, 390]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark, bypassCSP: false });
  const log = [];
  page.on("console", (m) => { if (m.type() === "error" || /Content Security Policy|Refused to/.test(m.text())) log.push(m.text()); });
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  for (const t of TYPES) {
    await check(`starters: ${t} at ${width} (${dark ? "dark" : "light"}) renders clean and follows the theme`, async () => {
      if (!ids[t]) throw new Error("not published");
      log.length = 0;
      await page.goto(base + "/#/pages/" + ids[t], { waitUntil: "load" });
      await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
      let fr = null;
      for (let i = 0; i < 50 && !fr; i++) { fr = page.frames().find((f) => f.url().includes("/p/" + ids[t])); if (!fr) await page.waitForTimeout(100); }
      if (!fr) throw new Error("no frame");
      await fr.waitForLoadState("load");
      await page.waitForTimeout(300);
      const m = await fr.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth, bg: getComputedStyle(document.body).backgroundColor, title: document.title }));
      if (m.title !== `Starter ${t}`) throw new Error("title " + m.title);
      if (m.sw > m.cw) throw new Error(`scrolls sideways: ${m.sw} > ${m.cw}`);
      // The scheme's Comebye colours: the page's own fallback, and also what the viewer sends under System.
      if (m.bg !== startBg) throw new Error(`background ${m.bg}, want ${startBg}`);
      if (t === "dashboard" && (await fr.locator("#chart rect").count()) !== 14) throw new Error("the chart did not draw 14 bars");
      if (t === "mockup") { await fr.click("#toggle"); if (!(await fr.locator("#comments").isHidden())) throw new Error("the mockup's toggle did nothing"); }
      // The theme message, built as the app's viewer builds it.
      const want = await page.evaluate((id) => {
        const th = ComebyeThemes.byId[id], tokens = {};
        for (const k of Object.keys(th.vars)) tokens["--" + k] = String(th.vars[k]);
        document.querySelector("iframe.pgs-frame").contentWindow.postMessage({ type: "comebye-theme", scheme: th.mode, tokens }, "*");
        return th.vars.bg;
      }, other);
      await page.waitForTimeout(200);
      const bg = await fr.evaluate(() => getComputedStyle(document.body).backgroundColor);
      if (bg !== rgb(want)) throw new Error(`after ${other}: background ${bg}, want ${rgb(want)}`);
      await page.screenshot({ path: `${out}/starter-${t}-${width}.png` });
      if (log.length) throw new Error("console:\n" + log.join("\n"));
    });
  }
  await browser.close();
}

fs.rmSync(work, { recursive: true, force: true });
const failed = results.filter((r) => !r.ok);
console.log(`starters: ${results.length - failed.length}/${results.length} passed`);
process.exit(failed.length ? 1 : 0);
