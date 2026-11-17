// The reading-width setting (Settings > Appearance, web/themes.js comebye.col -> <html data-col> -> --col):
// four presets, per device. At 3440 the assistant reply is 820 / 1120 / 1480 / pane-20 px wide, the composer and
// the bars above it share its left edge and width, the user bubble ends on its right edge; at 1280 a wide value
// is capped by the pane; at 390 every value gives the same layout. The stored value is on <html> by
// DOMContentLoaded, an unknown one shows the default, a storage event from another tab updates an open chat,
// the split view (#sv-chat) follows, and the radios work by keyboard. Run in light and dark.
// The transcript is mocked, so it needs no live agent.
//
//   node readcol.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";
import { U, A } from "./fixtures/toolruns.mjs";

const [base, out] = process.argv.slice(2);
const results = [];
const log = [];
const same = (got, want, what) => { if (got !== want) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };
const near = (got, want, what, tol = 1) => { if (Math.abs(got - want) > tol) throw new Error(`${what}: got ${got}, want ${want} (±${tol})`); };
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

const para = "The reading column is as wide as the setting says, and no wider than the window allows, so a long paragraph like this one wraps at the column and not at the edge of a very wide monitor. ";
const MSGS = [
  U("u1", "Make the conversation use more of my ultrawide screen."),
  A("a1", (para.repeat(3) + "\n\n").repeat(3)),
  { id: "s1", kind: "tool", tool: "Agent", args: "review the diff", result: "Looks fine.", ms: 9000,
    subagent: { agent_id: "ag1", type: "reviewer", description: "Review the diff", status: "completed", report: "Looks fine.", dur_ms: 9000, tool_uses: 3 } },
  A("a2", para.repeat(2)),
];
const PRESETS = [{ id: "", px: 820 }, { id: "wide", px: 1120 }, { id: "wider", px: 1480 }, { id: "full", px: 0 }];

async function session(width, height, dark) {
  const { browser, ctx, page } = await launch({ width, height, dark });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: MSGS, total: MSGS.length, more: false, format: "claude" } }));
  await page.addInitScript(() => {
    document.addEventListener("DOMContentLoaded", () => { window.__colAtDCL = document.documentElement.getAttribute("data-col"); });
  });
  return { browser, ctx, page };
}
const store = (page, v) => page.evaluate((v) => { if (v === null) localStorage.removeItem("comebye.col"); else localStorage.setItem("comebye.col", v); }, v);
async function openChat(page, v) {
  await page.goto(base + "/#/settings", { waitUntil: "load" }); // the origin, to set the store before the chat loads
  await store(page, v);
  await page.goto("about:blank");
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#chat .msg.assistant");
}
// Everything the check needs about the column in one trip: rects of the first reply, the user bubble, the composer
// and the bars (the hidden ones are shown for the measurement), the scroll container, and sideways overflow.
const measure = (page, chat) => page.evaluate((chat) => {
  const R = (el) => { if (!el) return null; const r = el.getBoundingClientRect(); return { l: r.left, r: r.right, w: r.width }; };
  const root = document.querySelector(chat);
  const bars = {};
  for (const id of ["prompt", "queued", "d-markbar", "d-chatstrip", "keypad"]) {
    const el = id === "prompt" ? document.querySelector("#detail .prompt") : document.getElementById(id);
    if (!el) { bars[id] = null; continue; }
    const wasHidden = el.hidden;
    el.hidden = false;
    if (!el.offsetHeight) el.style.minHeight = "10px";
    bars[id] = R(el);
    el.hidden = wasHidden; el.style.minHeight = "";
  }
  return {
    attr: document.documentElement.getAttribute("data-col"),
    col: getComputedStyle(document.documentElement).getPropertyValue("--col").trim(),
    reply: R(root.querySelector(".msg.assistant")), user: R(root.querySelector(".msg.user")),
    cw: root.clientWidth, sw: root.scrollWidth,
    docOverflow: document.documentElement.scrollWidth - window.innerWidth, bars,
  };
}, chat);
const want = (p, cw) => (p.px ? Math.min(p.px, cw - 20) : cw - 20);

for (const dark of [true, false]) {
  const tag = dark ? "dark" : "light";
  {
    const { browser, page } = await session(3440, 1200, dark);
    for (const p of PRESETS) {
      await check(`3440 ${tag}: "${p.id || "comfortable"}" reply, composer, bars and user bubble line up on the column`, async () => {
        await openChat(page, p.id);
        const m = await measure(page, "#chat");
        same(m.attr, p.id || null, "data-col");
        near(m.reply.w, want(p, m.cw), "reply width");
        for (const [id, b] of Object.entries(m.bars)) {
          if (!b) { if (id !== "prompt") continue; throw new Error("no composer"); }
          near(b.l, m.reply.l, `${id} left edge`); near(b.w, m.reply.w, `${id} width`);
        }
        near(m.user.r, m.reply.r, "user bubble right edge");
        same(m.sw <= m.cw, true, "no sideways scroll in #chat");
        await page.screenshot({ path: `${out}/readcol-3440-${tag}-${p.id || "comfortable"}.png` });
      });
    }
    await check(`3440 ${tag}: an unknown stored value shows the default`, async () => {
      await openChat(page, "huge");
      const m = await measure(page, "#chat");
      same(m.attr, null, "data-col"); near(m.reply.w, 820, "reply width");
    });
    await check(`3440 ${tag}: the stored value is on <html> by DOMContentLoaded`, async () => {
      await openChat(page, "wider");
      same(await page.evaluate(() => window.__colAtDCL), "wider", "data-col at DOMContentLoaded");
    });
    await check(`3440 ${tag}: a storage event from another tab updates a chat that is already open`, async () => {
      await openChat(page, "");
      near((await measure(page, "#chat")).reply.w, 820, "before");
      const other = await page.context().newPage();
      await other.goto(base + "/#/settings", { waitUntil: "load" });
      await other.evaluate(() => localStorage.setItem("comebye.col", "wide"));
      await page.waitForFunction(() => document.documentElement.getAttribute("data-col") === "wide");
      near((await measure(page, "#chat")).reply.w, 1120, "after another tab set Wide");
      await other.evaluate(() => localStorage.setItem("comebye.col", "nonsense"));
      await page.waitForFunction(() => !document.documentElement.hasAttribute("data-col"));
      near((await measure(page, "#chat")).reply.w, 820, "after an unknown value");
      await other.close();
    });
    await check(`3440 ${tag}: the split view (#sv-chat) follows the setting`, async () => {
      for (const p of PRESETS) {
        await openChat(page, p.id);
        await page.locator("#chat .subrun .sa-line").click();
        await page.locator("#chat .subrun .sa-steps").click();
        await page.waitForFunction(() => !document.getElementById("subview").hidden && document.querySelector("#sv-chat .msg.assistant"));
        const m = await measure(page, "#sv-chat");
        near(m.reply.w, want(p, m.cw), `${p.id || "comfortable"}: #sv-chat reply width`);
        near(m.user.r, m.reply.r, "#sv-chat user bubble right edge");
      }
      await page.screenshot({ path: `${out}/readcol-3440-${tag}-split-full.png` });
    });
    await check(`3440 ${tag}: Settings > Appearance radios work by keyboard and click, aria-checked follows, the chat follows`, async () => {
      await page.goto(base + "/#/settings", { waitUntil: "load" });
      await store(page, null);
      await page.reload({ waitUntil: "load" });
      await page.waitForSelector('#sx-view .sx-radio');
      same(await page.locator("#sx-view h2").first().textContent(), "Appearance", "first section");
      if (!/Saved on this device only\./.test(await page.locator("#sx-view").textContent())) throw new Error("no device-only label");
      same(await page.locator('#sx-view [role=radiogroup]').count(), 3, "three radio groups (width, text size, spacing)");
      same(await page.locator('[aria-labelledby="sx-col-lbl"] .sx-radio[aria-checked="true"]').count(), 1, "one checked");
      same(await page.locator('[aria-labelledby="sx-col-lbl"] .sx-radio[aria-checked="true"] .sx-rn').textContent(), "Comfortable", "default checked");
      await page.locator('[aria-labelledby="sx-col-lbl"] .sx-radio[aria-checked="true"]').focus();
      await page.keyboard.press("ArrowRight");
      same(await page.locator('[aria-labelledby="sx-col-lbl"] .sx-radio[aria-checked="true"] .sx-rn').textContent(), "Wide", "after ArrowRight");
      same(await page.evaluate(() => localStorage.getItem("comebye.col")), "wide", "stored");
      same(await page.evaluate(() => document.documentElement.getAttribute("data-col")), "wide", "applied");
      same(await page.evaluate(() => document.activeElement.id), "sx-col-wide", "focus stays on the choice");
      await page.keyboard.press("End");
      same(await page.locator('[aria-labelledby="sx-col-lbl"] .sx-radio[aria-checked="true"] .sx-rn').textContent(), "Full", "after End");
      await page.locator("#sx-col-wider").click();
      same(await page.evaluate(() => localStorage.getItem("comebye.col")), "wider", "stored by click");
      await page.screenshot({ path: `${out}/readcol-settings-${tag}.png` });
      await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"));
      await page.waitForSelector("#chat .msg.assistant");
      near((await measure(page, "#chat")).reply.w, 1480, "chat after choosing Wider");
    });
    await browser.close();
  }
  {
    const { browser, page } = await session(1280, 800, dark);
    for (const p of PRESETS) {
      await check(`1280 ${tag}: "${p.id || "comfortable"}" column is the preset capped by the pane, no sideways scroll`, async () => {
        await openChat(page, p.id);
        const m = await measure(page, "#chat");
        near(m.reply.w, want(p, m.cw), "reply width");
        near(m.bars.prompt.l, m.reply.l, "composer left edge"); near(m.bars.prompt.w, m.reply.w, "composer width");
        same(m.sw <= m.cw && m.docOverflow <= 0, true, "no sideways scroll");
        await page.screenshot({ path: `${out}/readcol-1280-${tag}-${p.id || "comfortable"}.png` });
      });
    }
    await browser.close();
  }
  {
    const { browser, page } = await session(390, 844, dark);
    let first;
    for (const p of PRESETS) {
      await check(`390 ${tag}: "${p.id || "comfortable"}" gives the same layout as the default, no sideways scroll`, async () => {
        await openChat(page, p.id);
        const m = await measure(page, "#chat");
        same(m.sw <= m.cw && m.docOverflow <= 0, true, "no sideways scroll");
        const key = JSON.stringify([m.reply, m.user, m.bars.prompt]);
        if (!first) first = key; else same(key, first, "layout");
        await page.screenshot({ path: `${out}/readcol-390-${tag}-${p.id || "comfortable"}.png` });
      });
    }
    await check(`390 ${tag}: the Appearance section says the setting has no effect on this window`, async () => {
      await page.goto(base + "/#/settings", { waitUntil: "load" });
      await page.waitForSelector("#sx-view .sx-radio");
      if (!/no effect here/.test(await page.locator("#sx-view").textContent())) throw new Error("no caption");
      same(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, "no sideways scroll in Settings");
    });
    await browser.close();
  }
}
if (log.length) console.log(log.join("\n"));
const bad = results.filter((r) => !r).length;
console.log(`${results.length - bad}/${results.length} checks passed`);
process.exit(bad || log.length ? 1 : 0);
