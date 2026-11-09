// A busy agent that herdr calls "blocked" (a dialog phrase in recent command output matched one of
// its rules) while its screen shows no dialog and a running spinner. comebye shows it as working,
// with a note, on the row and on the opened agent; it is not in Needs you and has no answer card.
//
//   node falseblocked.mjs BASE OUT_DIR       (env: SCHEME)
// BASE is a hub on its own fake herdr (-false-blocked), so the shared fleet of drive.mjs is untouched.
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  await check(`false blocked at ${width}px: herdr says blocked, the screen shows a running command, so it is working, with why, and not in Needs you`, async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    const sel = '#agents .agent[data-id$="w8:p1"]';
    await page.waitForFunction((q) => { const r = document.querySelector(q); return r && r.classList.contains("working") && /^working \(running: Running/.test(r.querySelector(".why.quiet")?.textContent || ""); }, sel, { timeout: 15000 });
    if (await page.locator(".needs-sec").count()) throw new Error("a Needs you group is shown");
    if (await page.locator(sel + ".blocked").count()) throw new Error("the row is styled blocked");
    if (/\bneed/.test(await page.locator("#count").innerText())) throw new Error("the count says an agent needs input: " + await page.locator("#count").innerText());
    const g = await page.evaluate((q) => { const r = document.querySelector(q), w = r.querySelector(".why"), rb = r.getBoundingClientRect(), wb = w.getBoundingClientRect(); return { inside: wb.left >= rb.left - 1 && wb.right <= rb.right + 1, scroll: document.documentElement.scrollWidth > innerWidth }; }, sel);
    if (!g.inside || g.scroll) throw new Error("row geometry " + JSON.stringify(g));
    await page.locator(sel).first().screenshot({ path: `${out}/false-blocked-row-${width}.png` });
    await page.locator(sel).first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.waitForFunction(() => { const w = document.querySelector("#d-why"); return w && !w.hidden && /^working \(running: /.test(w.textContent) && document.querySelector("#d-status").textContent === "working"; }, null, { timeout: 8000 });
    if (await page.locator("#answer").isVisible()) throw new Error("the answer card shows for an agent with no dialog");
    await page.screenshot({ path: `${out}/false-blocked-open-${width}.png` });
  });
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`false-blocked: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
