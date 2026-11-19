// The spacing setting (Settings > Appearance, web/themes.js comebye.sp -> <html data-sp> -> --lh and --gap): Compact
// (1.4, 6 px) / Comfortable (1.55, 10 px, the default) / Roomy (1.75, 16 px), per device, for the conversation only.
// At 390, 1280 and 3440: a reply's computed line-height is its font size times --lh, thinking's is --lh - .05, the
// gap between two consecutive messages is --gap, tool-run and thinking lines keep their 32/44 px hit height, nothing
// scrolls sideways, the stored value is on <html> by DOMContentLoaded, an unknown one shows the default, the split
// view follows, and the Appearance radios work by keyboard and click with a preview that follows. Light and dark.
//
//   node spacing.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";
import { U, A, T } from "./fixtures/toolruns.mjs";

const [base, out] = process.argv.slice(2);
const results = [];
const log = [];
const same = (got, want, what) => { if (got !== want) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };
const near = (got, want, what, tol = 0.5) => { if (Math.abs(got - want) > tol) throw new Error(`${what}: got ${got}, want ${want} (±${tol})`); };
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

const MSGS = [
  U("u1", "Make the text in the conversation bigger."),
  { ...A("a1", "Done. Run `go test ./...` to see it.\n\n```go\nfunc Scale(n int) int {\n\treturn n * 2\n}\n```\n\n" + "A long paragraph that has to wrap at every size without a sideways scroll, whatever the text size is. ".repeat(4)), think: "Thinking it over.\n\nThe tokens scale." },
  T("c1", "Read", "web/app.js", "// app"),
  T("c2", "Bash", "go test ./...", "ok"),
  { id: "s1", kind: "tool", tool: "Agent", args: "review the diff", result: "Looks fine.", ms: 9000,
    subagent: { agent_id: "ag1", type: "reviewer", description: "Review the diff", status: "completed", report: "Looks fine.", dur_ms: 9000, tool_uses: 3 } },
  A("a2", "All good."),
];
const SPS = [{ id: "compact", lh: 1.4, gap: 6 }, { id: "", lh: 1.55, gap: 10 }, { id: "roomy", lh: 1.75, gap: 16 }];

async function session(width, height, dark) {
  const { browser, page } = await launch({ width, height, dark });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: MSGS, total: MSGS.length, more: false, format: "claude" } }));
  await page.addInitScript(() => {
    document.addEventListener("DOMContentLoaded", () => { window.__spAtDCL = document.documentElement.getAttribute("data-sp"); });
  });
  return { browser, page };
}
const store = (page, v) => page.evaluate((v) => { if (v === null) localStorage.removeItem("comebye.sp"); else localStorage.setItem("comebye.sp", v); }, v);
async function openChat(page, v) {
  await page.goto(base + "/#/settings", { waitUntil: "load" });
  await store(page, v);
  await page.goto("about:blank");
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#chat .msg.assistant");
  await page.evaluate(() => { const t = document.querySelector("#chat .thinking .tr-line"); if (t && t.getAttribute("aria-expanded") !== "true") t.click(); });
  await page.waitForSelector("#chat .think-body:not([hidden])");
}
const measure = (page, chat) => page.evaluate((chat) => {
  const root = document.querySelector(chat);
  const cs = (el) => (el ? getComputedStyle(el) : null);
  const lh = (el) => parseFloat(cs(el).lineHeight);
  const reply = root.querySelector(".msg.assistant .bubble"), think = root.querySelector(".think-body");
  const kids = [...root.children].filter((e) => e.offsetHeight);
  const gaps = [];
  for (let i = 1; i < kids.length; i++) gaps.push(kids[i].getBoundingClientRect().top - kids[i - 1].getBoundingClientRect().bottom);
  const user = root.querySelector(".msg.user");
  return {
    attr: document.documentElement.getAttribute("data-sp"),
    replyLh: lh(reply), replyFs: parseFloat(cs(reply).fontSize), thinkLh: lh(think), thinkFs: parseFloat(cs(think).fontSize),
    cssGap: parseFloat(cs(root).rowGap), gaps, pair: user.nextElementSibling.getBoundingClientRect().top - user.getBoundingClientRect().bottom,
    hits: [...root.querySelectorAll(".tr-line")].map((e) => e.getBoundingClientRect().height),
    cw: root.clientWidth, sw: root.scrollWidth, docOverflow: document.documentElement.scrollWidth - window.innerWidth,
  };
}, chat);
const minHit = (w) => (w <= 600 ? 44 : 32);

for (const dark of [true, false]) {
  const tag = dark ? "dark" : "light";
  for (const [w, hgt] of [[3440, 1200], [1280, 800], [390, 844]]) {
    const { browser, page } = await session(w, hgt, dark);
    for (const s of SPS) {
      await check(`${w} ${tag}: "${s.id || "comfortable"}" is line-height ${s.lh} and a ${s.gap} px gap, tap heights kept`, async () => {
        await openChat(page, s.id);
        const m = await measure(page, "#chat");
        same(m.attr, s.id || null, "data-sp");
        near(m.replyLh, m.replyFs * s.lh, "reply line-height", 0.3);
        near(m.thinkLh, m.thinkFs * (s.lh - 0.05), "thinking line-height", 0.3);
        same(m.cssGap, s.gap, "#chat gap");
        near(m.pair, s.gap, "gap between the user message and the reply", 0.6);
        for (const g of m.gaps.slice(0, 3)) near(g, s.gap, "gap between consecutive items", 0.6);
        for (const hh of m.hits) if (hh < minHit(w) - 0.01) throw new Error(`tool/thinking line is ${hh} px tall, want >= ${minHit(w)}`);
        same(m.sw <= m.cw && m.docOverflow <= 0, true, `no sideways scroll (sw ${m.sw}, cw ${m.cw}, doc ${m.docOverflow})`);
        await page.screenshot({ path: `${out}/spacing-${w}-${tag}-${s.id || "default"}.png` });
      });
    }
    if (w === 3440) {
      await check(`${w} ${tag}: an unknown stored value shows the default`, async () => {
        await openChat(page, "tight");
        const m = await measure(page, "#chat");
        same(m.attr, null, "data-sp"); same(m.cssGap, 10, "gap");
      });
      await check(`${w} ${tag}: the stored value is on <html> by DOMContentLoaded`, async () => {
        await openChat(page, "roomy");
        same(await page.evaluate(() => window.__spAtDCL), "roomy", "data-sp at DOMContentLoaded");
      });
      await check(`${w} ${tag}: a storage event from another tab updates a chat that is already open`, async () => {
        await openChat(page, "");
        same((await measure(page, "#chat")).cssGap, 10, "before");
        const other = await page.context().newPage();
        await other.goto(base + "/#/settings", { waitUntil: "load" });
        await other.evaluate(() => localStorage.setItem("comebye.sp", "roomy"));
        await page.waitForFunction(() => document.documentElement.getAttribute("data-sp") === "roomy");
        same((await measure(page, "#chat")).cssGap, 16, "after another tab set Roomy");
        await other.close();
      });
      await check(`${w} ${tag}: the split view (#sv-chat) follows the setting`, async () => {
        await openChat(page, "roomy");
        await page.locator("#chat .subrun .sa-line").click();
        await page.locator("#chat .subrun .sa-steps").click();
        await page.waitForFunction(() => !document.getElementById("subview").hidden && document.querySelector("#sv-chat .msg.assistant"));
        const m = await page.evaluate(() => { const r = document.getElementById("sv-chat"), b = r.querySelector(".msg.assistant .bubble"); return { gap: parseFloat(getComputedStyle(r).rowGap), lh: parseFloat(getComputedStyle(b).lineHeight) / parseFloat(getComputedStyle(b).fontSize) }; });
        same(m.gap, 16, "#sv-chat gap"); near(m.lh, 1.75, "#sv-chat line-height ratio", 0.02);
      });
      await check(`${w} ${tag}: Settings > Appearance radios work by keyboard and click, the preview follows`, async () => {
        await page.goto(base + "/#/settings", { waitUntil: "load" });
        await store(page, null);
        await page.reload({ waitUntil: "load" });
        await page.waitForSelector("#sx-sp-default");
        same(await page.locator("#sx-view [role=radiogroup]").count(), 7, "seven radio groups");
        const sel = '[aria-labelledby="sx-sp-lbl"] .sx-radio[aria-checked="true"] .sx-rn';
        same(await page.locator(sel).textContent(), "Comfortable", "default checked");
        const prev = () => page.evaluate(() => { const p = document.querySelector(".sx-prev"), b = p.querySelector(".msg.assistant .bubble"), k = [...p.children]; return { gap: parseFloat(getComputedStyle(p).rowGap), lh: parseFloat(getComputedStyle(b).lineHeight) / parseFloat(getComputedStyle(b).fontSize), n: k.length }; });
        let p = await prev(); same(p.gap, 10, "preview gap"); near(p.lh, 1.55, "preview lh", 0.02);
        await page.locator("#sx-sp-default").focus();
        await page.keyboard.press("ArrowRight");
        same(await page.locator(sel).textContent(), "Roomy", "after ArrowRight");
        same(await page.evaluate(() => localStorage.getItem("comebye.sp")), "roomy", "stored");
        same(await page.evaluate(() => document.activeElement.id), "sx-sp-roomy", "focus stays on the choice");
        p = await prev(); same(p.gap, 16, "preview gap at Roomy"); near(p.lh, 1.75, "preview lh at Roomy", 0.02);
        await page.locator("#sx-sp-compact").click();
        same(await page.evaluate(() => localStorage.getItem("comebye.sp")), "compact", "stored by click");
        p = await prev(); same(p.gap, 6, "preview gap at Compact"); near(p.lh, 1.4, "preview lh at Compact", 0.02);
        same(await page.evaluate(() => { const w = document.querySelector(".sx-prev-wrap").getBoundingClientRect(), v = document.querySelector(".sx-prev").getBoundingClientRect(); return v.right <= w.right + 1 && v.bottom <= w.bottom + 2; }), true, "preview fits its frame");
        await page.screenshot({ path: `${out}/spacing-settings-${w}-${tag}.png` });
      });
    }
    await browser.close();
  }
}
if (log.length) console.log(log.join("\n"));
const bad = results.filter((r) => !r).length;
console.log(`${results.length - bad}/${results.length} checks passed`);
process.exit(bad || log.length ? 1 : 0);
