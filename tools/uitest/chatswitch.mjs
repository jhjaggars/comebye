// An agent whose transcript does not exist yet shows the raw terminal; once the session appears the view
// switches to the chat by itself. A raw terminal chosen by hand stays until "Show chat".
//
//   node chatswitch.mjs BASE OUT_DIR       (env: SCHEME)
// The transcript route of the terminal-only agent (bravo, w2:p1) is mocked: 404, then 200.
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
async function check(name, fn) {
  try { await fn(); console.log("ok   " + name); } catch (e) { failed = true; console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
let failed = false;
const MSGS = { messages: [{ id: "m1", kind: "user", text: "hello there" }, { id: "m2", kind: "assistant", text: "the session is up" }], total: 2, more: false, format: "claude" };

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  let transcriptUp = false, hits = 0;
  await page.route(/\/api\/agents\/w2(:|%3A)p1\/transcript/, (r) => {
    hits++;
    if (!transcriptUp) return r.fulfill({ status: 404, json: { code: "no_session", message: "no session" } });
    return r.fulfill({ json: MSGS });
  });
  const open = async () => {
    await page.goto(base + "/#/agent/" + encodeURIComponent("w2:p1"), { waitUntil: "load" });
    await page.waitForSelector("#detail", { state: "visible" });
  };
  const vis = (sel) => page.evaluate((s) => { const e = document.querySelector(s); return !!e && getComputedStyle(e).display !== "none" && !e.hidden; }, sel);

  await check(`no transcript: raw terminal, then the chat appears by itself at ${width}px`, async () => {
    await open();
    await page.waitForFunction(() => document.querySelector("#output")?.innerText.includes("bravo ready"), null, { timeout: 8000 });
    if (await vis("#chat")) throw new Error("chat visible before a session exists");
    if (await vis("#btn-terminal")) throw new Error("a switch button shows with no transcript");
    await page.screenshot({ path: `${out}/chatswitch-terminal-${width}.png` });
    transcriptUp = true; // no reselect from here
    await page.waitForFunction(() => document.querySelector("#chat")?.innerText.includes("the session is up"), null, { timeout: 10000 });
    if (await vis("#output")) throw new Error("raw output still visible");
    if (!(await vis("#chat"))) throw new Error("chat not visible");
    const label = await page.locator("#btn-terminal").getAttribute("aria-label");
    if (!/raw terminal/i.test(label)) throw new Error("button label " + label);
    await page.screenshot({ path: `${out}/chatswitch-chat-${width}.png` });
    const g = await page.evaluate(() => { const b = document.querySelector("#btn-terminal").getBoundingClientRect(), c = document.querySelector("#d-ctx"), h = document.querySelector("#detail header"), hr = h.getBoundingClientRect(); return { w: b.width, h: b.height, inHeader: h.contains(document.querySelector("#btn-terminal")), right: b.right <= hr.right + 1, left: b.left >= 0, ctxHidden: c.hidden, title: document.querySelector("#btn-terminal").title, hdrH: hr.height, scroll: document.documentElement.scrollWidth <= innerWidth }; });
    if (!g.inHeader || g.w < 44 || g.h < 44 || !g.right || !g.left || !g.scroll) throw new Error("toggle geometry " + JSON.stringify(g));
    if (g.hdrH > 80) throw new Error("header wrapped: " + g.hdrH);
  });

  await check(`a raw terminal chosen by hand sticks until Show chat at ${width}px`, async () => {
    await page.locator("#btn-terminal").click(); // chat -> raw
    await page.waitForFunction(() => document.querySelector("#output")?.innerText.includes("bravo ready"), null, { timeout: 5000 });
    if (await vis("#chat")) throw new Error("chat still visible after Show raw terminal");
    const label = await page.locator("#btn-terminal").getAttribute("aria-label");
    if (!/show chat/i.test(label)) throw new Error("button label " + label);
    const before = hits;
    await page.waitForTimeout(5500); // past a retry interval and a poll
    if (await vis("#chat")) throw new Error("auto-switched back to the chat");
    await page.screenshot({ path: `${out}/chatswitch-manual-${width}.png` });
    await page.locator("#btn-terminal").click(); // raw -> chat
    await page.waitForFunction(() => getComputedStyle(document.querySelector("#chat")).display !== "none" && document.querySelector("#chat").innerText.includes("the session is up"), null, { timeout: 5000 });
    if (await vis("#output")) throw new Error("raw output visible after Show chat");
  });
  await browser.close();
}
process.exit(failed ? 1 : 0);
