// A refused prompt says why and what to do: the server's reason, with "Send anyway" (a held input box)
// and "Queue", at 390 and 1280px. The 409 is stubbed in the browser; "Send anyway" must resend with force.
//
//   node refused.mjs BASE OUT_DIR       (env: SCHEME)
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
  await check(`refused prompt at ${width}px: the reason, Send anyway and Queue; Send anyway resends with force`, async () => {
    const bodies = [];
    await page.route("**/prompt", async (route) => {
      const req = route.request();
      if (req.method() !== "POST") return route.continue();
      const b = JSON.parse(req.postData() || "{}");
      bodies.push(b);
      if (b.force) return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ agent: {} }) });
      return route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ code: "input_busy", message: 'held: the input box in that pane has text in it ("half a sentence"); a message sent now would be typed after it' }) });
    });
    await page.goto(base + "/", { waitUntil: "load" });
    await page.locator("#agents .agent").first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.locator("#prompt-text").click();
    await page.keyboard.type("please look at this");
    await page.evaluate(() => document.querySelector("#btn-send").click());
    await page.waitForFunction(() => /Not sent: the input box/.test(document.querySelector("#toast")?.innerText || ""), null, { timeout: 8000 });
    const labels = await page.$$eval("#toast button", (bs) => bs.map((b) => b.textContent));
    if (!labels.includes("Send anyway") || !labels.includes("Queue")) throw new Error("toast buttons: " + JSON.stringify(labels));
    const g = await page.evaluate(() => { const r = document.querySelector("#toast").getBoundingClientRect(); return { inside: r.left >= 0 && r.right <= innerWidth + 1, scroll: document.documentElement.scrollWidth > innerWidth }; });
    if (!g.inside || g.scroll) throw new Error("toast geometry " + JSON.stringify(g));
    await page.waitForTimeout(400);
    await page.screenshot({ path: `${out}/refused-${width}.png` });
    await page.locator("#toast button", { hasText: "Send anyway" }).click();
    await page.waitForTimeout(500);
    if (!bodies.some((b) => b.force === true && b.text === "please look at this")) throw new Error("no forced resend: " + JSON.stringify(bodies));
  });
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`refused: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
