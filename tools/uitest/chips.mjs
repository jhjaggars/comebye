// Chips, cards and empty states (design language slice 5): the shared shape and tones, read back
// from computed styles on the screens that show them.
//
//   node chips.mjs BASE OUT_DIR      (env: SCHEME)
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const CHIPS = ".chip, .wt-chip, .host-chip, .lg-chip, .role-chip, .us-chip, .bd-tag, .td-chip, .rel-chip, .bd-chip, .rc-tag, .rv-chip";
const PILLS = ".hf-chip, .rc-chip, .pgs-chip";
const CARDS = ".card, .bcard, .pz-card, .rl-card, .us-card, .rc-card, .pgs-card, .pgs-hit, .pr-card";
const EMPTY = ".empty, .ln-empty, .br-empty, .rc-empty, .td-none, .pal-empty, .pgs-empty, .pr-empty, .us-empty";

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width < 500 ? 844 : 800, dark });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(e.message));
  const tag = `${width}px ${dark ? "dark" : "light"}`;
  const seen = { chips: 0, cards: 0, empties: 0, pills: 0 };
  const bad = { chips: [], cards: [], empties: [], pills: [] };
  for (const hash of ["", "#/todo", "#/status", "#/peers", "#/usage", "#/pages", "#/search"]) {
    await page.goto(base + "/" + hash, { waitUntil: "load" });
    await page.waitForTimeout(900);
    const r = await page.evaluate(([chips, cards, empty, pills]) => {
      const vis = (e) => e.offsetParent !== null;
      const q = (s) => [...document.querySelectorAll(s)].filter(vis);
      const px = (e, p) => getComputedStyle(e)[p];
      return {
        chips: q(chips).map((e) => ({ c: e.className, r: px(e, "borderTopLeftRadius"), fs: px(e, "fontSize"), nw: px(e, "whiteSpace") })),
        cards: q(cards).map((e) => ({ c: e.className, r: px(e, "borderTopLeftRadius"), bw: px(e, "borderTopWidth"), bg: px(e, "backgroundColor") })),
        empties: q(empty).map((e) => ({ c: e.className, a: px(e, "textAlign") })),
        pills: q(pills).map((e) => ({ c: e.className, r: px(e, "borderTopLeftRadius"), fs: px(e, "fontSize"), w: px(e, "fontWeight"), h: Math.round(e.getBoundingClientRect().height), on: e.getAttribute("aria-pressed") === "true" || e.classList.contains("on"), col: px(e, "color"), bg: px(e, "backgroundColor") })),
      };
    }, [CHIPS, CARDS, EMPTY, PILLS]);
    seen.pills += r.pills.length;
    for (const c of r.pills) if (parseFloat(c.r) < 99 || c.fs !== "13px" || Number(c.w) < 600 || c.h < 36) bad.pills.push(hash + " " + JSON.stringify(c));
    seen.chips += r.chips.length; seen.cards += r.cards.length; seen.empties += r.empties.length;
    for (const c of r.chips) if (c.r !== "6px" || !["11px", "12px"].includes(c.fs) || (c.nw !== "nowrap" && !/\blg-chip\b/.test(c.c))) bad.chips.push(hash + " " + JSON.stringify(c));
    for (const c of r.cards) if (c.bw !== "1px" || !["12px", "14px"].includes(c.r)) bad.cards.push(hash + " " + JSON.stringify(c));
    for (const c of r.empties) if (c.a !== "center") bad.empties.push(hash + " " + JSON.stringify(c));
  }
  await check(`${tag}: chips share one shape (6px radius, 11/12px, no wrap)`, async () => {
    if (!seen.chips) throw new Error("no chip on any screen");
    if (bad.chips.length) throw new Error(bad.chips.slice(0, 3).join("\n"));
  });
  await check(`${tag}: filter pills share one shape (pill radius, 13px/600, at least 36px)`, async () => {
    if (!seen.pills) throw new Error("no filter pill on any screen");
    if (bad.pills.length) throw new Error(bad.pills.slice(0, 3).join("\n"));
  });
  await check(`${tag}: cards share one surface (1px edge, 12px radius)`, async () => {
    if (bad.cards.length) throw new Error(bad.cards.slice(0, 3).join("\n"));
  });
  await check(`${tag}: empty states are centred`, async () => {
    if (bad.empties.length) throw new Error(bad.empties.slice(0, 3).join("\n"));
  });
  await check(`${tag}: no page errors`, async () => { if (errs.length) throw new Error(errs.join("; ")); });
  console.log(`     (saw ${seen.pills} pills, ${seen.chips} chips, ${seen.cards} cards, ${seen.empties} empty states)`);
  await browser.close();
}
const nbad = results.filter((r) => !r).length;
console.log(`${results.length - nbad}/${results.length} chip checks passed`);
process.exit(nbad ? 1 : 0);
