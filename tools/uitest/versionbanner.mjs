// The "new version" banner must not be raised by a response the browser replayed from its HTTP cache.
// /api/pages/{id}/preview?v=N is served "immutable" for a year, so a replay carries the X-App-Version
// of the build that first fetched it: read as a live version it made a reloaded page think a deploy
// was pending, and the banner came back after every tap on it.
//
//   node versionbanner.mjs BASE OUT_DIR       (env: SCHEME)
import { launch } from "./lib.mjs";

const [base] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

const { browser, page } = await launch({ width: 1280, height: 800, dark });
await page.goto(base + "/", { waitUntil: "load" });
await page.waitForSelector("#agents .agent", { timeout: 15000 });
await page.waitForFunction(() => !!ver.loaded, null, { timeout: 5000 });

await check("a replayed immutable response with an old X-App-Version does not raise the banner", async () => {
  await page.route(/\/api\/replayed\/immutable/, (r) => r.fulfill({ status: 200, json: { ok: true },
    headers: { "X-App-Version": "0ldbu1ld0000", "Cache-Control": "private, max-age=31536000, immutable" } }));
  await page.evaluate(() => api("/api/replayed/immutable"));
  const s = await page.evaluate(() => ({ pending: ver.pending, hidden: $("update-banner").hidden }));
  if (s.pending || !s.hidden) throw new Error("banner raised by a cached response: " + JSON.stringify(s));
});

await check("a live response with a different X-App-Version still raises the banner", async () => {
  await page.route(/\/api\/live\/other/, (r) => r.fulfill({ status: 200, json: { ok: true }, headers: { "X-App-Version": "n3wbu1ld0000" } }));
  await page.evaluate(() => { state.selected = "x"; });
  await page.evaluate(() => api("/api/live/other"));
  const s = await page.evaluate(() => ({ pending: ver.pending, hidden: $("update-banner").hidden }));
  await page.evaluate(() => { state.selected = null; });
  if (s.pending !== "n3wbu1ld0000" || s.hidden) throw new Error("no banner for a live new version: " + JSON.stringify(s));
});
await browser.close();
const bad = results.filter((r) => !r.ok).length;
console.log(`version-banner: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
