// The model chip beside the context chip in the agent view header: "Sonnet 4.6" for a Claude id, the last path
// segment (minus a build stamp) otherwise, truncated on a phone, full provider/model in the tooltip, shown even
// with no context figure, moving when the transcript reports another model, falling back to the launch model.
//
//   node modelchip.mjs BASE OUT_DIR       (env: SCHEME=light|dark)
// BASE is the hub on the fake herdr -context (see ctxchip.mjs); adds ctx-pi-long (w40:p1) with a long enmaas model.
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const tag = dark ? "dark" : "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const PANES = { claude: "w32:p1", long: "w40:p1", none: "w36:p1", live: "w38:p1", live2: "w39:p1" };

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  const open = async (pane) => {
    await page.goto(base + "/", { waitUntil: "load" });
    const sel = `#agents .agent[data-id$="${pane}"]`;
    await page.waitForSelector(sel, { timeout: 15000 });
    await page.locator(sel).first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.waitForFunction(() => document.querySelector("#chat")?.children.length > 0, null, { timeout: 8000 });
    await page.waitForTimeout(500);
  };
  const geo = () => page.evaluate(() => {
    const phone = innerWidth <= 480, q = (s) => document.querySelector(s), m = q(phone ? "#d-submodel" : "#d-modelchip"), c = q("#d-ctx"), r = m.getBoundingClientRect(), shown = getComputedStyle(m).display !== "none";
    const hd = q("#detail header").getBoundingClientRect(), more = q("#d-more").getBoundingClientRect(), st = q("#d-status").getBoundingClientRect();
    return { mHidden: m.hidden || !shown, mText: m.textContent, mTitle: m.title, mH: r.height, mW: r.width, mRight: r.right, mTrunc: m.scrollWidth > m.clientWidth,
      cHidden: c.hidden, cText: c.innerText.replace(/\s+/g, " ").trim(), moreRight: more.right, hdRight: hd.right, statusH: st.height, stLeft: st.left,
      metaW: q('#detail header .meta').getBoundingClientRect().width, hdW: hd.width, scroll: document.documentElement.scrollWidth > innerWidth };
  });
  const sane = (g, what) => {
    if (g.mH > 26) throw new Error(what + ": chip wraps, height " + g.mH);
    if (g.scroll) throw new Error(what + ": the page scrolls sideways");
    if (g.moreRight > g.hdRight + 1) throw new Error(what + ": the menu button is pushed off-screen");
    if (g.statusH > 24) throw new Error(what + ": the status word wraps");
    if (g.mRight > g.stLeft + 1 && g.hdW > 500) throw new Error(what + ": model chip overlaps the status");
  };
  const shot = (name) => page.screenshot({ path: `${out}/model-${name}-${width}-${tag}.png`, clip: { x: 0, y: 0, width, height: width < 500 ? 96 : 70 } });

  await check(`${width}px ${tag}: a Claude session shows "Opus 5.5" beside ctx, full model in tooltip`, async () => {
    await open(PANES.claude);
    const g = await geo();
    if (g.mHidden || g.mText !== "Opus 5.5") throw new Error("model chip " + JSON.stringify(g));
    if (!/claude-opus-5-5/.test(g.mTitle)) throw new Error("tooltip/aria " + g.mTitle);
    if (g.cHidden || !/^ctx 83%/.test(g.cText)) throw new Error("ctx chip " + g.cText);
        sane(g, "claude"); await shot("claude");
  });

  await check(`${width}px ${tag}: a pi session with a long enmaas model shows the short name, truncated, full path in tooltip`, async () => {
    await open(PANES.long);
    const g = await geo();
    if (g.mText !== "DeepSeek-V4.1-Flash") throw new Error("text " + g.mText);
    if (!/enmaas\/rits\/deepseek-ai\/DeepSeek-V4\.1-Flash-1017/.test(g.mTitle)) throw new Error("tooltip " + g.mTitle);
    sane(g, "pi-long"); await shot("pilong");
    await page.locator(width < 500 ? "#d-submodel" : "#d-modelchip").click();
    await page.waitForFunction(() => /DeepSeek-V4\.1-Flash-1017/.test(document.querySelector("#toast")?.textContent || ""), null, { timeout: 3000 });
  });

  await check(`${width}px ${tag}: no context yet: no model chip, no ctx chip; the launch model fills in when known`, async () => {
    await open(PANES.none);
    let g = await geo();
    if (!g.mHidden || !g.cHidden) throw new Error("expected both hidden: " + JSON.stringify(g));
    await shot("none");
    await page.evaluate(() => { const a = agentById(state.selected); ledger.byAgent.set(agentId(a), { id: "x", model: "claude-opus-5-5" }); renderCtxChip(); });
    g = await geo();
    if (g.mHidden || g.mText !== "Opus 5.5" || !g.cHidden) throw new Error("launch fallback " + JSON.stringify(g));
    sane(g, "none+launch"); await shot("none-launch");
  });

  await check(`${width}px ${tag}: the chip follows a model change in the transcript`, async () => {
    await open(width > 500 ? PANES.live2 : PANES.live);
    if ((await geo()).mText !== "Sonnet 4.6") throw new Error("start " + (await geo()).mText);
    await page.evaluate(() => { state.chat.ctx = { ...state.chat.ctx, model: "claude-haiku-5-5" }; renderCtxChip(); });
    if ((await geo()).mText !== "Haiku 5.5") throw new Error("after /model " + (await geo()).mText);
  });
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`modelchip: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
