// A Page must paint in the app's mode from its first frame, not in the device's and then flip.
//
// The device's colour scheme is emulated one way and the app's theme the other (device dark + app light, then
// the reverse). A heavy Page (the starter template plus a few MB of inline images, the network slowed so its
// load event comes late) records its own background and prefers-color-scheme on every animation frame from the
// head; after it loaded, every sample must be in the app's mode: no frame in the device's. A Markdown Page
// with a Mermaid diagram is checked the same way, and the diagram's theme must follow the app. A theme change
// while the Page is open must reach it, in the Page viewer (#pgs-view) and in the split (#rs).
//
//   node pagetheme.mjs BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("pagetheme.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
const work = fs.mkdtempSync(path.join(os.tmpdir(), "pagetheme-ui-"));
const results = [];
const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w5:p1" };
const cli = (args) => execFileSync(bin, args, { env, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false, err: e.message }); console.log("FAIL " + name + "\n     " + e.message.split("\n").join("\n     ")); }
}

// The recorder runs from the head, so its first sample is the frame's first paint.
const RECORDER = `<script>
window.__s = [];
(function tick() {
  var b = document.body, c = b ? getComputedStyle(b).backgroundColor : "";
  var cv = getComputedStyle(document.documentElement).colorScheme;
  window.__s.push({ t: Math.round(performance.now()), body: !!b, bg: c, mq: matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light", ready: document.readyState, vis: getComputedStyle(document.documentElement).visibility });
  if (window.__s.length < 2000) requestAnimationFrame(tick);
})();
</script>`;

const PNG = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAFElEQVR4nGP8z8Dwn4EIwESMolGFxCsEABhYAhHYAxhTAAAAAElFTkSuQmCC", "base64");
function heavyHTML() {
  let h = cli(["pages", "new", "--title", "Heavy theme probe"]);
  // ~1.6 MB of distinct inline images after the text: the load event waits for them (about 4 s at the 350 KB/s the
  // check emulates, well after the first paint and the mid-load screenshot 1.2 s in), the first paint does not.
  const blob = Buffer.concat([PNG, Buffer.alloc(300 * 1024, 7)]).toString("base64");
  let imgs = "";
  for (let i = 0; i < 4; i++) imgs += `<img alt="" width="8" height="8" src="data:image/png;base64,${blob}${i}">\n`;
  h = h.replace("<head>", "<head>\n" + RECORDER);
  return h.replace("</main>", imgs + "</main>");
}
const mdSource = "# Markdown theme probe\n\nSome text in the shell.\n\n```mermaid\ngraph TD\n  A[Start] --> B[End]\n```\n";

const lum = (rgb) => { const m = String(rgb).match(/[\d.]+/g); if (!m) return null; const [r, g, b] = m.map(Number); return 0.2126 * r + 0.7152 * g + 0.0722 * b; };
const modeOfBg = (rgb) => { const l = lum(rgb); return l == null ? null : l < 128 ? "dark" : "light"; };

const stamp = Date.now();
const heavy = path.join(work, "heavy.html"); fs.writeFileSync(heavy, heavyHTML());
const md = path.join(work, "probe.md"); fs.writeFileSync(md, mdSource);
const heavyId = cli(["publish", heavy, "--key", `uitest-pagetheme-heavy-${stamp}`]).match(/pg_[0-9a-f]{10}/)[0];
const mdId = cli(["publish", md, "--key", `uitest-pagetheme-md-${stamp}`]).match(/pg_[0-9a-f]{10}/)[0];

const THEMES = { light: "gruvbox-light", dark: "dracula" };
const frameOf = (page, id) => page.frames().find((f) => f.url().includes("/p/" + id));

async function open(page, id, appMode, hash) {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.evaluate((t) => ComebyeThemes.set(t), THEMES[appMode]);
  await page.goto(base + hash, { waitUntil: "load" });
}

for (const width of [390, 1280]) {
  for (const device of ["dark", "light"]) {
    const app = device === "dark" ? "light" : "dark";
    const tag = `${width}, device ${device}, app ${app}`;
    await check(`pagetheme: a heavy Page paints in the app's mode from its first frame, never the device's (${tag})`, async () => {
      const { browser, ctx, page } = await launch({ width, height: width > 500 ? 800 : 844, dark: device === "dark", bypassCSP: false });
      try {
        await page.goto(base + "/", { waitUntil: "load" });
        await page.evaluate((t) => ComebyeThemes.set(t), THEMES[app]);
        // Only the Page is slowed (the app is already loaded, and the hash change below does not reload it).
        const cdp = await ctx.newCDPSession(page);
        await cdp.send("Network.enable");
        await cdp.send("Network.emulateNetworkConditions", { offline: false, latency: 150, downloadThroughput: 350 * 1024, uploadThroughput: 350 * 1024 });
        await page.goto(base + "/#/pages/" + heavyId, { waitUntil: "commit" });
        let fr;
        for (let i = 0; i < 200 && !fr; i++) { fr = frameOf(page, heavyId); if (!fr) await page.waitForTimeout(50); }
        if (!fr) throw new Error("the Page's frame never appeared");
        for (let i = 0; i < 100; i++) { try { if (await fr.evaluate(() => !!document.body)) break; } catch (e) {} await page.waitForTimeout(50); }
        await page.waitForTimeout(1200);
        await page.screenshot({ path: `${out}/pagetheme-heavy-${width}-device-${device}-midload.png` }); // the Page still loading
        // wait for the frame's load
        for (let i = 0; i < 400; i++) { try { if (await fr.evaluate(() => document.readyState) === "complete") break; } catch (e) {} await page.waitForTimeout(100); }
        await page.waitForTimeout(600);
        const s = await fr.evaluate(() => window.__s);
        const withBody = s.filter((x) => x.body);
        if (!withBody.length) throw new Error("no sample with a body");
        const loading = withBody.filter((x) => x.ready !== "complete");
        if (!loading.length) throw new Error("the Page was already loaded at its first sample: the test saw no first paint");
        // A frame counts when it could be seen: hidden (visibility) until the theme arrives is the point.
        const bad = withBody.filter((x) => x.vis !== "hidden" && modeOfBg(x.bg) !== app);
        await page.screenshot({ path: `${out}/pagetheme-heavy-${width}-device-${device}.png` });
        if (bad.length) throw new Error(`${bad.length} of ${withBody.length} frames (${loading.length} before load) painted outside the app's ${app} mode; first: ${JSON.stringify(bad[0])}; ${JSON.stringify(withBody.slice(0, 3))}`);
        // Chrome does not make the frame's prefers-color-scheme follow the iframe element (tried: it stays the device's), so the Page's own media query is not asserted here.
        if (!withBody.some((x) => x.vis === "hidden") && !withBody.some((x) => modeOfBg(x.bg) === app)) throw new Error("no sample at all in the app's mode");
        const el = await page.evaluate(() => { const f = document.querySelector("iframe.pgs-frame"); return f && getComputedStyle(f).colorScheme; });
        if (el !== app) throw new Error(`the iframe element's color-scheme is ${el}, want ${app}`);
      } finally { await browser.close(); }
    });
  }
}

for (const device of ["dark", "light"]) {
  const app = device === "dark" ? "light" : "dark";
  await check(`pagetheme: a Markdown Page and its Mermaid diagram follow the app (device ${device}, app ${app}), and a theme change while open reaches them`, async () => {
    const { browser, page } = await launch({ width: 390, height: 844, dark: device === "dark", bypassCSP: false });
    try {
      await open(page, mdId, app, "/#/pages/" + mdId);
      // The frame is looked up on every read, and a read that meets a navigation (the viewer loads the Page into its
      // frame again, and a theme change may too) counts as "not yet": a frame kept from before it would be destroyed.
      const state = async () => {
        const fr = frameOf(page, mdId);
        if (!fr) return { frame: false };
        return fr.evaluate(() => {
          const n = document.querySelector(".mmd svg .node rect, .mmd svg .node polygon, .mmd svg rect");
          return { bg: document.body ? getComputedStyle(document.body).backgroundColor : "", node: n ? getComputedStyle(n).fill : "", svg: !!document.querySelector(".mmd svg") };
        }).catch((e) => ({ frame: true, err: String(e.message).split("\n")[0] }));
      };
      // 15 s: the first read also waits for the frame to appear (it had 10 s of its own before).
      const until = async (mode) => { let v; for (let i = 0; i < 150; i++) { v = await state(); if (v.svg && modeOfBg(v.bg) === mode && modeOfBg(v.node) === mode) return v; await page.waitForTimeout(100); } throw new Error(`want ${mode}, the Markdown Page has ${JSON.stringify(v)}`); };
      await until(app);
      await page.screenshot({ path: `${out}/pagetheme-md-device-${device}.png` });
      await page.evaluate((t) => ComebyeThemes.set(t), THEMES[device]);
      await until(device); // the app switches to the device's mode: Page and diagram follow
    } finally { await browser.close(); }
  });
}

await check("pagetheme: a theme change while a Page is open reaches the split (#rs) and the viewer, the frame element's color-scheme too", async () => {
  const { browser, page } = await launch({ width: 1280, height: 800, dark: true, bypassCSP: false });
  try {
    await open(page, heavyId, "light", "/#/pages/" + heavyId);
    await page.waitForSelector("#pgs-view iframe.pgs-frame", { timeout: 8000 });
    const fr = () => frameOf(page, heavyId);
    const want = async (mode) => {
      let v;
      for (let i = 0; i < 100; i++) {
        v = await page.evaluate(() => { const f = document.querySelector("#pgs-view iframe.pgs-frame"); return f && getComputedStyle(f).colorScheme; });
        const b = fr() && await fr().evaluate(() => getComputedStyle(document.body).backgroundColor).catch(() => "");
        if (v === mode && modeOfBg(b) === mode) return;
        await page.waitForTimeout(100);
      }
      throw new Error(`want ${mode}, element ${v}`);
    };
    await want("light");
    await page.evaluate((t) => ComebyeThemes.set(t), THEMES.dark);
    await want("dark");
    await page.evaluate((t) => ComebyeThemes.set(t), THEMES.light);
    await want("light");
  } finally { await browser.close(); }
});

const failed = results.filter((r) => !r.ok);
fs.writeFileSync(`${out}/pagetheme.json`, JSON.stringify(results, null, 1));
console.log(`pagetheme: ${results.length - failed.length}/${results.length} ok`);
process.exit(failed.length ? 1 : 0);
