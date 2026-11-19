// The text-size setting (Settings > Appearance, web/themes.js comebye.rs -> <html data-rs> -> --rs): four steps,
// Small 93% / Default / Large 115% / Larger 130%, per device, for the conversation only. At 390, 1280 and 3440:
// a reply's text, a tool line, thinking and inline code are 15 / 14 / 13 / 13 px times the factor (±0.5), a fenced
// block 12 times it, the composer's text 16 times it; the header title, the list and the Settings screen do not
// change; at 390 with Larger nothing scrolls sideways and the tap targets stay >= 44 px; the stored value is on
// <html> by DOMContentLoaded, an unknown one shows the default, another tab's change reaches an open chat, the
// split view follows, and the Appearance radios work by keyboard, with a preview that follows the choice.
// Run in light and dark. The transcript is mocked, so it needs no live agent.
//
//   node textsize.mjs BASE OUT_DIR
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
const SIZES = [{ id: "small", f: 0.93 }, { id: "", f: 1 }, { id: "large", f: 1.15 }, { id: "larger", f: 1.3 }];

async function session(width, height, dark) {
  const { browser, page } = await launch({ width, height, dark });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: MSGS, total: MSGS.length, more: false, format: "claude" } }));
  await page.addInitScript(() => {
    document.addEventListener("DOMContentLoaded", () => { window.__rsAtDCL = document.documentElement.getAttribute("data-rs"); });
  });
  return { browser, page };
}
const store = (page, v) => page.evaluate((v) => { if (v === null) localStorage.removeItem("comebye.rs"); else localStorage.setItem("comebye.rs", v); }, v);
async function openChat(page, v) {
  await page.goto(base + "/#/settings", { waitUntil: "load" });
  await store(page, v);
  await page.goto("about:blank");
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#chat .msg.assistant");
  await page.evaluate(() => { const t = document.querySelector("#chat .thinking .tr-line"); if (t && t.getAttribute("aria-expanded") !== "true") t.click(); });
  await page.waitForSelector("#chat .think-body:not([hidden])");
}
// Computed font sizes of the conversation's parts and of the chrome that must not move, plus overflow and hit heights.
const measure = (page, chat) => page.evaluate((chat) => {
  const px = (el) => (el ? parseFloat(getComputedStyle(el).fontSize) : null);
  const root = document.querySelector(chat);
  const q = (s) => root.querySelector(s);
  const h = (el) => (el ? el.getBoundingClientRect().height : null);
  return {
    attr: document.documentElement.getAttribute("data-rs"),
    reply: px(q(".msg.assistant .bubble")), user: px(q(".msg.user .bubble")), line: px(q(".tr-line")),
    think: px(q(".think-body")), code: px(q(".msg.assistant .bubble p code")), pre: px(q(".msg.assistant .bubble pre")),
    composer: chat === "#chat" ? px(document.querySelector("#detail .prompt .composer")) : null,
    title: px(document.querySelector("#detail header .title")),
    hits: [...root.querySelectorAll(".tr-line")].map(h),
    cw: root.clientWidth, sw: root.scrollWidth, docOverflow: document.documentElement.scrollWidth - window.innerWidth,
  };
}, chat);
const chrome = (page) => page.evaluate(() => {
  const px = (el) => (el ? parseFloat(getComputedStyle(el).fontSize) : null);
  return { title: px(document.querySelector("#detail header .title")), h1: px(document.querySelector("header h1")) };
});

for (const dark of [true, false]) {
  const tag = dark ? "dark" : "light";
  for (const [w, hgt] of [[3440, 1200], [1280, 800], [390, 844]]) {
    const { browser, page } = await session(w, hgt, dark);
    let base0;
    for (const s of SIZES) {
      await check(`${w} ${tag}: "${s.id || "default"}" scales the conversation by ${s.f}, the chrome stays`, async () => {
        await openChat(page, s.id);
        const m = await measure(page, "#chat");
        same(m.attr, s.id || null, "data-rs");
        near(m.reply, 15 * s.f, "reply"); near(m.line, 14 * s.f, "tool line"); near(m.think, 13 * s.f, "thinking");
        near(m.code, 15 * 0.87 * s.f, "inline code", 0.6); near(m.pre, 12 * s.f, "fenced block");
        near(m.user, 15 * s.f, "user bubble");
        near(m.composer, 16 * s.f, "composer text");
        const c = await chrome(page);
        if (!base0) base0 = c; else same(JSON.stringify(c), JSON.stringify(base0), "chrome font sizes");
        same(m.title, 16, "header title");
        same(m.sw <= m.cw && m.docOverflow <= 0, true, `no sideways scroll (sw ${m.sw}, cw ${m.cw}, doc ${m.docOverflow})`);
        if (w === 390) for (const hh of m.hits) if (hh < 44 - 0.01) throw new Error("tool line is " + hh + " px tall, want >= 44");
        await page.screenshot({ path: `${out}/textsize-${w}-${tag}-${s.id || "default"}.png` });
      });
    }
    if (w === 3440) {
      await check(`${w} ${tag}: an unknown stored value shows the default`, async () => {
        await openChat(page, "huge");
        const m = await measure(page, "#chat");
        same(m.attr, null, "data-rs"); near(m.reply, 15, "reply");
      });
      await check(`${w} ${tag}: the stored value is on <html> by DOMContentLoaded`, async () => {
        await openChat(page, "larger");
        same(await page.evaluate(() => window.__rsAtDCL), "larger", "data-rs at DOMContentLoaded");
      });
      await check(`${w} ${tag}: a storage event from another tab updates a chat that is already open`, async () => {
        await openChat(page, "");
        near((await measure(page, "#chat")).reply, 15, "before");
        const other = await page.context().newPage();
        await other.goto(base + "/#/settings", { waitUntil: "load" });
        await other.evaluate(() => localStorage.setItem("comebye.rs", "large"));
        await page.waitForFunction(() => document.documentElement.getAttribute("data-rs") === "large");
        near((await measure(page, "#chat")).reply, 15 * 1.15, "after another tab set Large");
        await other.close();
      });
      await check(`${w} ${tag}: the split view (#sv-chat) follows the setting`, async () => {
        await openChat(page, "larger");
        await page.locator("#chat .subrun .sa-line").click();
        await page.locator("#chat .subrun .sa-steps").click();
        await page.waitForFunction(() => !document.getElementById("subview").hidden && document.querySelector("#sv-chat .msg.assistant"));
        near((await measure(page, "#sv-chat")).reply, 15 * 1.3, "#sv-chat reply");
      });
      await check(`${w} ${tag}: Settings > Appearance radios work by keyboard and click, the preview follows, Settings itself does not scale`, async () => {
        await page.goto(base + "/#/settings", { waitUntil: "load" });
        await store(page, null);
        await page.reload({ waitUntil: "load" });
        await page.waitForSelector("#sx-rs-default");
        same(await page.locator("#sx-view [role=radiogroup]").count(), 7, "seven radio groups");
        same(await page.locator('[aria-labelledby="sx-rs-lbl"] .sx-radio[aria-checked="true"] .sx-rn').textContent(), "Default", "default checked");
        const prevPx = () => page.evaluate(() => parseFloat(getComputedStyle(document.querySelector(".sx-prev .msg.assistant .bubble")).fontSize));
        const h2 = () => page.evaluate(() => parseFloat(getComputedStyle(document.querySelector("#sx-view h2")).fontSize));
        const h2a = await h2();
        near(await prevPx(), 15, "preview at default");
        await page.locator("#sx-rs-default").focus();
        await page.keyboard.press("ArrowRight");
        same(await page.locator('[aria-labelledby="sx-rs-lbl"] .sx-radio[aria-checked="true"] .sx-rn').textContent(), "Large", "after ArrowRight");
        same(await page.evaluate(() => localStorage.getItem("comebye.rs")), "large", "stored");
        same(await page.evaluate(() => document.activeElement.id), "sx-rs-large", "focus stays on the choice");
        near(await prevPx(), 15 * 1.15, "preview at Large");
        await page.locator("#sx-rs-larger").click();
        same(await page.evaluate(() => localStorage.getItem("comebye.rs")), "larger", "stored by click");
        near(await prevPx(), 15 * 1.3, "preview at Larger");
        same(await h2(), h2a, "Settings heading unchanged");
        // the preview fits inside the settings column
        same(await page.evaluate(() => { const w = document.querySelector(".sx-prev-wrap").getBoundingClientRect(), v = document.querySelector(".sx-prev").getBoundingClientRect(); return v.right <= w.right + 1 && v.bottom <= w.bottom + 2; }), true, "preview fits its frame");
        await page.screenshot({ path: `${out}/textsize-settings-${w}-${tag}.png` });
      });
    }
    if (w === 390) {
      await check(`${w} ${tag}: Larger does not shrink the composer's buttons (the setting changes text, not tap targets)`, async () => {
        const hs = async (v) => { await openChat(page, v); return page.evaluate(() => [...document.querySelectorAll("#detail .prompt .send, #detail .prompt .attach")].filter((e) => e.offsetHeight).map((e) => e.getBoundingClientRect().height)); };
        const d = await hs(""), l = await hs("larger");
        same(JSON.stringify(l), JSON.stringify(d), "button heights at Larger vs Default");
        if (!d.length) throw new Error("no composer buttons");
      });
    }
    await browser.close();
  }
}
if (log.length) console.log(log.join("\n"));
const bad = results.filter((r) => !r).length;
console.log(`${results.length - bad}/${results.length} checks passed`);
process.exit(bad || log.length ? 1 : 0);
