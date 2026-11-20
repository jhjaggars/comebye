// One browser smoke for the shared look. The tokens themselves (the focus ring, the chip, card, button, segmented
// control and notice shapes, the theme surface steps) are read from the source by tools/ci/static.mjs; this only shows
// that the app really paints them: on the screens that show them, every control ring is one ring, every chip one shape,
// every card one shape, a pressed segment differs from an unpressed one, and on a wide window the list column is a
// different surface from the conversation. No pixel or colour values.
//
//   node shapes.mjs BASE OUT_DIR      (env: SCHEME)
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const CHIPS = ".chip, .wt-chip, .host-chip, .lg-chip, .role-chip, .us-chip, .bd-tag, .td-chip, .rel-chip, .bd-chip, .rc-tag, .rv-chip";
const CARDS = ".card, .bcard, .pz-card, .rl-card, .us-card, .rc-card, .pgs-card, .pgs-hit, .pr-card";
const SEGS = ".seg, .bd-seg, .bc-seg, .pgs-seg, .us-seg";
const same = (a, what) => { const u = [...new Set(a)]; if (u.length > 1) throw new Error(`${what} differ: ${JSON.stringify(u)}`); };

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width < 500 ? 844 : 800, dark });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(e.message));
  const tag = `${width}px ${dark ? "dark" : "light"}`;
  const seen = { chips: [], cards: [], segs: [] };
  for (const hash of ["", "#/todo", "#/status", "#/peers", "#/usage", "#/briefings", "#/pages"]) {
    await page.goto(base + "/" + hash, { waitUntil: "load" });
    await page.waitForTimeout(700);
    const r = await page.evaluate(([chips, cards, segs]) => {
      const q = (s) => [...document.querySelectorAll(s)].filter((e) => e.offsetParent !== null);
      return {
        chips: q(chips).map((e) => getComputedStyle(e).borderTopLeftRadius),
        cards: q(cards).map((e) => getComputedStyle(e).borderTopLeftRadius),
        segs: q(segs).map((e) => { const on = e.querySelector('button[aria-pressed="true"]'), off = e.querySelector('button[aria-pressed="false"]'); return { r: getComputedStyle(e).borderTopLeftRadius, differ: on && off ? getComputedStyle(on).backgroundColor !== getComputedStyle(off).backgroundColor : null }; }),
      };
    }, [CHIPS, CARDS, SEGS]);
    seen.chips.push(...r.chips); seen.cards.push(...r.cards); seen.segs.push(...r.segs);
  }
  await check(`${tag}: every chip has one shape, every card has one shape`, async () => {
    if (!seen.chips.length || !seen.cards.length) throw new Error(`saw ${seen.chips.length} chips and ${seen.cards.length} cards`);
    same(seen.chips, "chip radii"); same(seen.cards, "card radii");
  });
  await check(`${tag}: segmented controls share a shape and the pressed segment looks different`, async () => {
    if (!seen.segs.length) throw new Error("no segmented control on any screen");
    same(seen.segs.map((s) => s.r), "segmented control radii");
    if (seen.segs.some((s) => s.differ === false)) throw new Error("a pressed segment looks like an unpressed one");
  });
  await check(`${tag}: Tab lands on controls and every one shows the same focus ring`, async () => {
    await page.goto(base + "/#/status", { waitUntil: "load" });
    await page.waitForTimeout(500);
    await page.locator("body").click({ position: { x: 1, y: 1 } });
    const rings = [];
    for (let i = 0; i < 12; i++) {
      await page.keyboard.press("Tab");
      const r = await page.evaluate(() => { const e = document.activeElement; if (!e || e === document.body) return null; const cs = getComputedStyle(e); return [cs.outlineStyle, cs.outlineWidth, cs.outlineColor].join(" "); });
      if (r) rings.push(r);
    }
    if (rings.length < 4) throw new Error("Tab reached only " + rings.length + " controls");
    if (rings.some((r) => /^none/.test(r) || / 0px /.test(r))) throw new Error("a focused control shows no ring: " + JSON.stringify([...new Set(rings)]));
    same(rings, "focus rings");
  });
  if (width >= 1100) await check(`${tag}: the list column is a different surface from the conversation`, async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#agents .agent");
    const r = await page.evaluate(() => ({ list: getComputedStyle(document.body).backgroundColor, detail: getComputedStyle(document.querySelector("#detail")).backgroundColor }));
    if (r.list === r.detail) throw new Error(`list ${r.list}, detail ${r.detail}`);
  });
  await check(`${tag}: no page errors`, async () => { if (errs.length) throw new Error(errs.join("; ")); });
  await browser.close();
}
const bad = results.filter((r) => !r).length;
console.log(`${results.length - bad}/${results.length} shape checks passed`);
process.exit(bad ? 1 : 0);
