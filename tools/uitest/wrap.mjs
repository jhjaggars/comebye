// The wrap setting (Settings > Appearance, web/themes.js comebye.wrap -> <html data-wrap>): Off (default) scrolls a
// long code line sideways, On wraps it (pre-wrap, overflow-wrap: anywhere) in fenced blocks and tool output, per
// device. At 390, 1280 and 3440: a 400-character line has no horizontal scroll with wrap on and still scrolls with it
// off; Copy gives the exact original text either way; the stored value is on <html> by DOMContentLoaded, an unknown
// one shows the default, a storage event updates an open chat, the split view follows, and the Appearance radios
// work by keyboard and click with a preview that follows. Light and dark.
//
//   node wrap.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";
import { U, A, T } from "./fixtures/toolruns.mjs";

const [base, out] = process.argv.slice(2);
const results = [];
const log = [];
const same = (got, want, what) => { if (got !== want) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

const LONG = "const token = '" + "abcdefghij".repeat(40) + "';"; // one 400+ character line, no spaces to break at
const BLOCK = "package main\n\n" + LONG + "\n\tfmt.Println(token)";
const MSGS = [
  U("u1", "Show the long line."),
  { ...A("a1", "Here:\n\n```go\n" + BLOCK + "\n```\n"), think: "Thinking." },
  T("c1", "Bash", "echo", LONG),
  A("a2", "Done."),
  { id: "s1", kind: "tool", tool: "Agent", args: "review the diff", result: "Looks fine.", ms: 9000,
    subagent: { agent_id: "ag1", type: "reviewer", description: "Review the diff", status: "completed", report: "Looks fine.\n\n```\n" + LONG + "\n```", dur_ms: 9000, tool_uses: 3 } },
];

async function session(width, height, dark) {
  const { browser, ctx, page } = await launch({ width, height, dark });
  await ctx.grantPermissions(["clipboard-read", "clipboard-write"], { origin: new URL(base).origin });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: MSGS, total: MSGS.length, more: false, format: "claude" } }));
  await page.addInitScript(() => {
    document.addEventListener("DOMContentLoaded", () => { window.__wrapAtDCL = document.documentElement.getAttribute("data-wrap"); });
  });
  return { browser, page };
}
const store = (page, v) => page.evaluate((v) => { if (v === null) localStorage.removeItem("comebye.wrap"); else localStorage.setItem("comebye.wrap", v); }, v);
async function openChat(page, v) {
  await page.goto(base + "/#/settings", { waitUntil: "load" });
  await store(page, v);
  await page.goto("about:blank");
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#chat .msg.assistant .cb pre");
  await page.evaluate(() => { const t = document.querySelector("#chat .toolrun .tr-line"); if (t && t.getAttribute("aria-expanded") !== "true") t.click(); });
}
const box = (page, sel) => page.evaluate((sel) => {
  const e = document.querySelector(sel);
  if (!e) return null;
  return { cw: e.clientWidth, sw: e.scrollWidth, ws: getComputedStyle(e).whiteSpace, doc: document.documentElement.scrollWidth - window.innerWidth, attr: document.documentElement.getAttribute("data-wrap") };
}, sel);
const clip = (page) => page.evaluate(() => navigator.clipboard.readText());

for (const dark of [true, false]) {
  const tag = dark ? "dark" : "light";
  for (const [w, hgt] of [[3440, 1200], [1280, 800], [390, 844]]) {
    const { browser, page } = await session(w, hgt, dark);
    await check(`${w} ${tag}: wrap on, the 400-character line has no horizontal scroll`, async () => {
      await openChat(page, "on");
      const m = await box(page, "#chat .md pre");
      same(m.attr, "on", "data-wrap");
      same(m.ws, "pre-wrap", "white-space");
      if (!(m.sw <= m.cw)) throw new Error(`code block scrolls: scrollWidth ${m.sw} > clientWidth ${m.cw}`);
      same(m.doc <= 0, true, "page scrolls sideways");
      const t = await box(page, "#chat .tr-pre.toolresult");
      if (t && !(t.sw <= t.cw)) throw new Error(`tool output scrolls: ${t.sw} > ${t.cw}`);
      await page.screenshot({ path: `${out}/wrap-${w}-${tag}-on.png` });
    });
    await check(`${w} ${tag}: wrap off (default), the long line still scrolls`, async () => {
      await openChat(page, null);
      const m = await box(page, "#chat .md pre");
      same(m.attr, null, "data-wrap");
      same(m.ws, "pre", "white-space");
      // the line is wider than the block at 390 and 1280; at 3440 the column may be wide enough only if > 400 chars of code font
      if (w < 3440 && !(m.sw > m.cw)) throw new Error(`code block does not scroll: ${m.sw} <= ${m.cw}`);
      await page.screenshot({ path: `${out}/wrap-${w}-${tag}-off.png` });
    });
    for (const v of ["on", ""]) {
      await check(`${w} ${tag}: Copy gives the exact original text with wrap ${v ? "on" : "off"}`, async () => {
        await openChat(page, v);
        await page.evaluate(() => navigator.clipboard.writeText("sentinel"));
        await page.locator("#chat .msg.assistant .cb .cb-copy").first().click();
        await page.waitForFunction(() => document.querySelector("#chat .cb-copy[data-state=ok]"));
        same(await clip(page), BLOCK, "copied text");
      });
    }
    if (w === 3440) {
      await check(`${w} ${tag}: an unknown stored value shows the default`, async () => {
        await openChat(page, "yes");
        const m = await box(page, "#chat .md pre");
        same(m.attr, null, "data-wrap"); same(m.ws, "pre", "white-space");
      });
      await check(`${w} ${tag}: the stored value is on <html> by DOMContentLoaded`, async () => {
        await openChat(page, "on");
        same(await page.evaluate(() => window.__wrapAtDCL), "on", "data-wrap at DOMContentLoaded");
      });
      await check(`${w} ${tag}: a storage event from another tab updates a chat that is already open`, async () => {
        await openChat(page, "");
        same((await box(page, "#chat .md pre")).ws, "pre", "before");
        const other = await page.context().newPage();
        await other.goto(base + "/#/settings", { waitUntil: "load" });
        await other.evaluate(() => localStorage.setItem("comebye.wrap", "on"));
        await page.waitForFunction(() => document.documentElement.getAttribute("data-wrap") === "on");
        same((await box(page, "#chat .md pre")).ws, "pre-wrap", "after another tab set On");
        await other.close();
      });
      await check(`${w} ${tag}: the split view (#sv-chat) follows the setting`, async () => {
        await openChat(page, "on");
        await page.locator("#chat .subrun .sa-line").click();
        await page.locator("#chat .subrun .sa-steps").click();
        await page.waitForFunction(() => !document.getElementById("subview").hidden && document.querySelector("#sv-chat .md pre"));
        same((await box(page, "#sv-chat .md pre")).ws, "pre-wrap", "#sv-chat white-space");
      });
      await check(`${w} ${tag}: Settings > Appearance radios work by keyboard and click, the preview follows`, async () => {
        await page.goto(base + "/#/settings", { waitUntil: "load" });
        await store(page, null);
        await page.reload({ waitUntil: "load" });
        await page.waitForSelector("#sx-wrap-default");
        same(await page.locator("#sx-view [role=radiogroup]").count(), 6, "six radio groups");
        const sel = '[aria-labelledby="sx-wrap-lbl"] .sx-radio[aria-checked="true"] .sx-rn';
        same(await page.locator(sel).textContent(), "Off", "default checked");
        same(await page.locator("#sx-view .sx-grp").first().textContent(), "Code and conversation", "group heading");
        const prev = () => page.evaluate(() => getComputedStyle(document.querySelector(".sx-prev pre")).whiteSpace);
        same(await prev(), "pre", "preview at Off");
        await page.locator("#sx-wrap-default").focus();
        await page.keyboard.press("ArrowRight");
        same(await page.locator(sel).textContent(), "On", "after ArrowRight");
        same(await page.evaluate(() => localStorage.getItem("comebye.wrap")), "on", "stored");
        same(await page.evaluate(() => document.activeElement.id), "sx-wrap-on", "focus stays on the choice");
        same(await prev(), "pre-wrap", "preview at On");
        same(await page.evaluate(() => { const p = document.querySelector(".sx-prev pre"); return p.scrollWidth <= p.clientWidth; }), true, "preview code does not scroll at On");
        await page.screenshot({ path: `${out}/wrap-settings-${w}-${tag}.png` });
        await page.locator("#sx-wrap-default").click();
        same(await page.evaluate(() => localStorage.getItem("comebye.wrap")), "", "stored by click");
        same(await prev(), "pre", "preview back at Off");
      });
    }
    await browser.close();
  }
}
if (log.length) console.log(log.join("\n"));
const bad = results.filter((r) => !r).length;
console.log(`${results.length - bad}/${results.length} checks passed`);
process.exit(bad || log.length ? 1 : 0);
