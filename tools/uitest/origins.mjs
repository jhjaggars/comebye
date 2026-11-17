// Who said it: in an agent's conversation the user's own messages are bubbles; text the hub typed (a notice, a
// relayed answer) is a quiet grey line with a "hub" chip; text another agent sent has an "agent NAME" chip. The
// server marks each user message's origin from its own record; a message that only starts like a hub notice,
// with no record, arrives as origin "user" and stays a plain bubble. Run at 390 and 1280px (SCHEME picks the theme).
// The transcript is mocked, like toolruns.mjs.
//
//   node origins.mjs BASE OUT_DIR      (env: SCHEME)
import { launch } from "./lib.mjs";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [], log = [];
const t0 = Date.now() - 600000;
const at = (i) => new Date(t0 + i * 60000).toISOString();
const messages = [
  { id: "m1", kind: "user", time: at(0), text: "Please look at the failing test in the cache package.", origin: "user" },
  { id: "m2", kind: "assistant", time: at(1), text: "On it." },
  { id: "m3", kind: "user", time: at(2), origin: "hub", text: "Comebye (hub notice): the worker on branch fix/cache in comebye on laharl (handoff h123) finished. Read its report with comebye read h123 --report. [notice a1b2c3]" },
  { id: "m4", kind: "user", time: at(3), origin: "relay", text: "Comebye (hub relay): the user answered your to-do t12 (\"Which key?\") in the app.\nTheir words: ok 2\nThis is the user's own answer, typed by the hub. Carry on." },
  { id: "m5", kind: "user", time: at(4), origin: "agent", origin_name: "coordinator-2 (laharl/w8J:p1)", text: "From agent coordinator-2 (laharl/w8J:p1), not the user and not the hub: please rebase onto main before you push." },
  { id: "m6", kind: "user", time: at(5), origin: "user", text: "Comebye (hub notice): this only starts like a hub notice; nobody typed it but me." },
  { id: "m7", kind: "assistant", time: at(6), text: "Done." },
];
const { browser, page } = await launch({ dark, width: 1280, height: 800 });
page.setDefaultTimeout(8000);
page.on("pageerror", (e) => log.push("pageerror: " + e.message));
await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages, total: messages.length, more: false, format: "claude" } }));

async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

for (const width of [390, 1280]) {
  await page.setViewportSize({ width, height: width > 500 ? 800 : 844 });
  await check(`origin labels at ${width}px: user bubbles, hub lines with a chip, an agent chip, a spoofed prefix stays a bubble`, async () => {
    await page.goto("about:blank");
    await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
    await page.waitForSelector("#chat .msg.sys");
    const g = await page.evaluate(() => {
      const chat = document.getElementById("chat");
      const lines = [...chat.querySelectorAll(".msg.sys")].map((e) => ({ cls: e.className, chip: e.querySelector(".ochip").textContent, txt: e.querySelector(".otxt").textContent.slice(0, 30) }));
      const bubbles = [...chat.querySelectorAll(".msg.user .bubble")].map((b) => b.textContent.slice(0, 30));
      const chip = chat.querySelector(".msg.sys .ochip"), cs = getComputedStyle(chip), line = chat.querySelector(".msg.sys");
      const r = line.getBoundingClientRect(), cr = chat.getBoundingClientRect();
      return { lines, bubbles, color: cs.color, bg: cs.backgroundColor, scroll: document.documentElement.scrollWidth > innerWidth, inside: r.left >= cr.left - 1 && r.right <= cr.right + 1 };
    });
    const chips = g.lines.map((l) => l.chip).join("|");
    if (chips !== "hub|hub|agent coordinator-2") throw new Error("chips " + chips);
    if (g.bubbles.length !== 2 || !/^Please look/.test(g.bubbles[0]) || !/^Comebye \(hub notice\): this onl/.test(g.bubbles[1])) throw new Error("bubbles " + JSON.stringify(g.bubbles));
    if (g.scroll || !g.inside) throw new Error("geometry " + JSON.stringify(g));
    if (g.color === g.bg) throw new Error("chip colours " + g.color);
    await page.screenshot({ path: `${out}/origins-${width}.png` });
  });
  await check(`origin labels at ${width}px: a hub line opens to its full text`, async () => {
    const line = page.locator("#chat .msg.sys").nth(1);
    await line.locator("summary").click();
    const body = await line.locator(".obody").innerText();
    if (!/Their words: ok 2/.test(body)) throw new Error("body " + body);
    await page.screenshot({ path: `${out}/origins-open-${width}.png` });
  });
}
await browser.close();
fs.writeFileSync(`${out}/origins-console.log`, log.join("\n"));
const failed = results.filter((r) => !r.ok).length + log.length;
for (const l of log) console.log("     " + l);
console.log(`${results.length - results.filter((r) => !r.ok).length}/${results.length} origins checks passed`);
process.exit(failed ? 1 : 0);
