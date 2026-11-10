// A Claude auto-mode permission prompt: the command, the classifier's reason and a live
// "will automatically block this request in 1:17" countdown, then Yes/No. The app shows it as a
// permission card with the command, the warning as a note, and working Yes/No buttons.
//
//   node autoblock.mjs BASE OUT_DIR       (env: SCHEME)
// BASE is a hub on its own fake herdr (-autoblock).
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
  await check(`auto-block prompt at ${width}px: command, warning note and Yes/No buttons`, async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    const sel = '#agents .agent[data-id$="w11:p1"]';
    await page.waitForSelector(sel, { timeout: 15000 });
    await page.screenshot({ path: `${out}/autoblock-list-${width}.png` });
    await page.locator(sel).first().click();
    await page.waitForSelector("#answer .ans-opts button.opt", { state: "visible", timeout: 15000 });
    const c = await page.evaluate(() => ({
      detail: document.querySelector("#answer .ans-detail")?.textContent || "",
      note: document.querySelector("#answer .ans-note")?.textContent || "",
      q: document.querySelector("#answer .ans-q")?.textContent || "",
      opts: [...document.querySelectorAll("#answer button.opt")].map((b) => b.textContent.replace(/\s+/g, " ").trim()),
      scroll: document.documentElement.scrollWidth > innerWidth,
    }));
    if (!c.detail.includes("git add internal/server/hubcheck.go") || !c.detail.includes("git log --oneline origin/main..HEAD")) throw new Error("command missing: " + JSON.stringify(c.detail));
    if (c.detail.includes("automatically block")) throw new Error("warning inside the command: " + JSON.stringify(c.detail));
    if (!/Dangerous rm operation/.test(c.note) || !/automatically block this request in 1:17/.test(c.note)) throw new Error("note: " + JSON.stringify(c.note));
    if (c.q !== "Do you want to proceed?") throw new Error("question: " + c.q);
    if (c.opts.length < 2 || !/^Yes/.test(c.opts[0]) || !/^No/.test(c.opts[1])) throw new Error("options: " + JSON.stringify(c.opts));
    if (c.scroll) throw new Error("page scrolls sideways");
    await page.screenshot({ path: `${out}/autoblock-open-${width}.png` });
  });
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`autoblock: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
