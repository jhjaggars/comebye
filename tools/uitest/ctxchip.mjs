// The context usage chip in the agent view header: "ctx 62% · 124k" next to the status word, toned by how full
// the window is (muted; warning from 80%, danger from 95%), hidden when nothing is known, tokens only when the
// model's window is not known, a percentage only on a phone, and moving while the agent works.
//
//   node ctxchip.mjs BASE OUT_DIR       (env: SCHEME)
// BASE is a hub on its own fake herdr (-context): ctx-ok 62% (sonnet 4.6, 200k), ctx-warn 83%, ctx-danger 96%,
// ctx-nowindow (a model with no known window), ctx-pi 50% (window from the Pi config), ctx-none (no usage) and
// ctx-live (30%, the test talks to it: 40% after), ctx-compacted (compacted after its last reply: the figure is stale).
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

const PANES = { ok: "w31:p1", warn: "w32:p1", danger: "w33:p1", nowindow: "w34:p1", pi: "w35:p1", live: "w38:p1", live2: "w39:p1", none: "w36:p1", compacted: "w37:p1" };

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  const open = async (pane) => {
    await page.goto(base + "/", { waitUntil: "load" });
    const sel = `#agents .agent[data-id$="${pane}"]`;
    await page.waitForSelector(sel, { timeout: 15000 });
    await page.locator(sel).first().click();
    await page.waitForSelector("#detail", { state: "visible" });
    await page.waitForFunction(() => document.querySelector("#chat")?.children.length > 0, null, { timeout: 8000 });
  };
  const chip = () => page.evaluate(() => {
    const e = document.querySelector("#d-ctx"), cs = getComputedStyle(e), r = e.getBoundingClientRect();
    const root = getComputedStyle(document.documentElement);
    const st = document.querySelector("#d-status").getBoundingClientRect();
    const hd = document.querySelector("#detail header").getBoundingClientRect();
    return { hidden: e.hidden || cs.display === "none", text: e.innerText.replace(/\s+/g, " ").trim(), color: cs.color, level: e.dataset.level, title: e.title,
      h: r.height, right: r.right, statusLeft: st.left, statusH: st.height, hdRight: hd.right, scroll: document.documentElement.scrollWidth > innerWidth,
      vars: { muted: root.getPropertyValue("--muted"), yellow: root.getPropertyValue("--yellow"), red: root.getPropertyValue("--red") } };
  });
  // the computed colour of a custom property's value, as rgb()
  const rgbOf = (v) => page.evaluate((c) => { const d = document.createElement("i"); d.style.color = c; document.body.appendChild(d); const x = getComputedStyle(d).color; d.remove(); return x; }, v);

  await check(`chip at ${width}px: ctx-ok shows ${width > 500 ? "ctx 62% · 124k" : "the percentage only"}, muted, beside the status, no wrap`, async () => {
    await open(PANES.ok);
    await page.waitForFunction(() => !document.querySelector("#d-ctx").hidden, null, { timeout: 8000 });
    const c = await chip();
    const want = width > 500 ? "ctx 62% · 124k" : "ctx 62%";
    if (c.text !== want) throw new Error(`text ${JSON.stringify(c.text)}, wanted ${JSON.stringify(want)}`);
    if (c.color !== await rgbOf(c.vars.muted)) throw new Error("not the muted tone: " + c.color);
    if (c.h > 26) throw new Error("chip wraps: height " + c.h);
    if (c.right > c.statusLeft + 1) throw new Error("chip overlaps the status word");
    if (c.statusH > 24) throw new Error("the status word wraps: " + c.statusH);
    if (c.scroll) throw new Error("the page scrolls sideways");
    if (!/62%\)/.test(c.title) || !/124,000 of 200,000 tokens/.test(c.title) || !/claude-sonnet-4-6/.test(c.title)) throw new Error("title " + c.title);
    await page.screenshot({ path: `${out}/ctx-ok-${width}.png`, clip: { x: 0, y: 0, width, height: 70 } });
  });

  await check(`chip at ${width}px: warning colour at 83%, danger at 96%, tokens only when the window is unknown, Pi from its config`, async () => {
    for (const [k, text, tone] of [["warn", "ctx 83%", "yellow"], ["danger", "ctx 96%", "red"], ["nowindow", "ctx 40k", "muted"], ["pi", "ctx 50%", "muted"]]) {
      await open(PANES[k]);
      await page.waitForFunction(() => !document.querySelector("#d-ctx").hidden, null, { timeout: 8000 });
      const c = await chip();
      if (!c.text.startsWith(text)) throw new Error(`${k}: text ${JSON.stringify(c.text)}, wanted ${text}`);
      if (c.color !== await rgbOf(c.vars[tone])) throw new Error(`${k}: colour ${c.color}, wanted ${tone} ${c.vars[tone]}`);
      if (k === "nowindow" && !/window is not known/.test(c.title)) throw new Error("title " + c.title);
      if (c.scroll || c.h > 26) throw new Error(`${k}: geometry ${JSON.stringify(c)}`);
      await page.screenshot({ path: `${out}/ctx-${k}-${width}.png`, clip: { x: 0, y: 0, width, height: 70 } });
    }
  });

  await check(`chip at ${width}px: hidden with no usage and after a compaction`, async () => {
    for (const k of ["none", "compacted"]) {
      await open(PANES[k]);
      await page.waitForTimeout(800);
      const c = await chip();
      if (!c.hidden) throw new Error(`${k}: the chip shows ${JSON.stringify(c.text)}`);
    }
  });

  await check(`chip at ${width}px: a tap says the exact numbers; it moves when the agent replies`, async () => {
    await open(width > 500 ? PANES.live2 : PANES.live); // each viewport talks to its own agent: a reply moves it for good
    await page.waitForFunction(() => /^ctx 30%/.test(document.querySelector("#d-ctx").innerText), null, { timeout: 8000 });
    await page.locator("#d-ctx").click();
    await page.waitForFunction(() => /60,000 of 200,000 tokens/.test(document.querySelector("#toast")?.textContent || ""), null, { timeout: 3000 });
    await page.evaluate(async () => { await fetch(`${pathFor(state.selected)}/prompt`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ text: "again?" }) }); });
    await page.waitForFunction(() => /^ctx 40%/.test(document.querySelector("#d-ctx").innerText), null, { timeout: 20000 });
  });
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`ctxchip: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
