// Settings > Appearance > Surfaces (web/themes.js DEPTHS, <html data-depth>): Layered (the default) puts the app on
// the tonal scale (--s1/--s2/--s3, --edge, quiet shadows, the dark top-light, accent-tint hover and selection); Flat
// is today's look. Computed-style assertions only, no screenshot comparison: at 390, 1280 and 1920, in Comebye Dark and
// Light and in Nord, Gruvbox Light and Catppuccin Mocha, each of Layered and Flat sets the card, bar, composer, menu,
// border and shadow colours the tokens say; the radio in Settings flips it live, saves it, survives a reload and is
// cleared by Reset to defaults; Pages are sent the Flat tokens under Flat.
//
//   node depth.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const results = [], log = [];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const same = (got, want, what) => { if (JSON.stringify(got) !== JSON.stringify(want)) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };
const rgb = (h) => { h = h.replace("#", ""); if (h.length === 3) h = h.replace(/./g, "$&$&"); return "rgb(" + [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16)).join(", ") + ")"; };

// What the page computes for the surfaces under test, and the theme's tokens, in one round trip.
const probe = (page) => page.evaluate(() => {
  // A plain card made here, not one from the fleet: which rows exist (blocked, selected, working) depends on the fixtures.
  document.getElementById("depth-probe")?.remove();
  const pr = document.createElement("div"); pr.id = "depth-probe"; pr.className = "agent"; pr.style.cssText = "position:fixed;left:-999px;top:0;width:200px"; document.body.appendChild(pr);
  const css = (sel, ...props) => { const e = document.querySelector(sel); if (!e) return null; const cs = getComputedStyle(e); return Object.fromEntries(props.map((p) => [p, cs.getPropertyValue(p)])); };
  const t = ComebyeThemes.byId[document.documentElement.getAttribute("data-theme")];
  return {
    vars: t.vars, depth: document.documentElement.getAttribute("data-depth"),
    agent: css("#depth-probe", "background-color", "border-top-color", "box-shadow"),
    header: css("header", "background-color", "box-shadow"),
    prompt: css(".prompt", "background-color", "box-shadow"),
    menu: css("#row-menu", "background-color", "border-top-color", "box-shadow"),
    root: css(":root", "--panel", "--panel2", "--card-edge"),
  };
});
async function open(width, theme, depth, dark) {
  const { browser, ctx, page } = await launch({ width, height: width < 500 ? 844 : 800, dark });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await ctx.addInitScript(([t, d]) => { try { localStorage.setItem("comebye.theme", t); if (d) localStorage.setItem("comebye.depth", d); } catch {} }, [theme, depth]);
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector(".agent");
  await page.addStyleTag({ content: "*, *::before, *::after { transition: none !important; animation: none !important; }" }); // a colour mid-transition is not what is under test
  return { browser, page };
}

const THEMES = [["dark", true], ["light", false], ["nord", true], ["gruvbox-light", false], ["catppuccin-mocha", true]];
for (const width of [390, 1280, 1920]) {
  for (const [theme, dark] of THEMES) {
    await check(`${width} ${theme}: Layered lifts the card, the bar, the composer and the menu onto the scale; Flat is the plain palette`, async () => {
      const L = await open(width, theme, "", dark);
      try {
        const p = await probe(L.page), v = p.vars;
        same(p.depth, null, "Layered leaves data-depth off");
        same(p.agent["background-color"], rgb(v.s1), "card background is --s1");
        same(p.agent["border-top-color"], rgb(v.edge), "card edge is --edge");
        same(p.agent["box-shadow"] !== "none", true, "card has a shadow");
        same(p.header["background-color"], rgb(v.s1), "bar is --s1");
        same(p.header["box-shadow"] !== "none", true, "bar is lifted");
        if (p.prompt) { same(p.prompt["background-color"], rgb(v.s2), "composer is --s2"); same(p.prompt["box-shadow"] !== "none", true, "composer is lifted"); }
        same(p.menu["background-color"], rgb(v.s3), "menu is --s3");
        same(p.menu["border-top-color"], rgb(v.edge), "menu edge is --edge");
        same(p.menu["box-shadow"].includes("inset") || p.menu["box-shadow"] !== "none", true, "menu has a shadow");
        if (width === 1280 && theme === "dark") await L.page.screenshot({ path: `${out}/depth-${width}-${theme}-layered.png` });
      } finally { await L.browser.close(); }
      const F = await open(width, theme, "flat", dark);
      try {
        const p = await probe(F.page), v = p.vars;
        same(p.depth, "flat", "Flat sets data-depth");
        same(p.agent["background-color"], rgb(v.panel), "card is --panel");
        same(p.agent["border-top-color"], rgb(v.border), "card border is --border");
        same(p.agent["box-shadow"], "none", "card has no shadow");
        same(p.header["background-color"], rgb(v.bg), "bar is --bg");
        same(p.header["box-shadow"], "none", "bar is flat");
        if (p.prompt) { same(p.prompt["background-color"], rgb(v.panel2), "composer is --panel2"); same(p.prompt["box-shadow"], "none", "composer is flat"); }
        same(p.menu["background-color"], rgb(v.panel2), "menu is --panel2");
        same(p.root["--card-edge"].trim(), v.border, "edge token is the border");
      } finally { await F.browser.close(); }
    });
  }
}

for (const dark of [true, false]) {
  const tag = dark ? "dark" : "light";
  await check(`${tag}: the Surfaces radio flips it live, saves it, survives a reload; Reset puts Layered back; Pages get the Flat tokens`, async () => {
    const { browser, page } = await open(1280, dark ? "dark" : "light", "", dark);
    try {
      await page.goto(base + "/#/settings", { waitUntil: "load" });
      await page.waitForSelector("#sx-depth-default");
      same(await page.locator("#sx-depth-default").getAttribute("aria-checked"), "true", "Layered is the default");
      const card = () => page.evaluate(() => { const e = document.querySelector("#sx-body .sx-field:has(> .sx-lbl)"); const c = getComputedStyle(e); return [c.backgroundColor, c.boxShadow]; });
      const v = await page.evaluate(() => ComebyeThemes.byId[document.documentElement.getAttribute("data-theme")].vars);
      same((await card())[0], rgb(v.s1), "a Settings field is a card (--s1)");
      await page.locator("#sx-depth-flat").click();
      same(await page.evaluate(() => document.documentElement.getAttribute("data-depth")), "flat", "data-depth at once");
      same(await page.evaluate(() => localStorage.getItem("comebye.depth")), "flat", "saved");
      same((await card())[1], "none", "the field card is gone under Flat");
      same(await page.evaluate(() => { const t = ComebyeThemes.depthTokens(ComebyeThemes.byId[document.documentElement.getAttribute("data-theme")].vars); return [t.s1, t.edge]; }), [v.panel, v.border], "Pages are sent panel and border as s1 and edge");
      await page.reload({ waitUntil: "load" });
      same(await page.evaluate(() => document.documentElement.getAttribute("data-depth")), "flat", "still Flat after a reload");
      await page.waitForSelector("#sx-reset");
      await page.locator("#sx-reset").click();
      same(await page.evaluate(() => [document.documentElement.getAttribute("data-depth"), localStorage.getItem("comebye.depth")]), [null, null], "Reset clears it");
      same(await page.locator("#sx-depth-default").getAttribute("aria-checked"), "true", "Layered shown again");
      await page.screenshot({ path: `${out}/depth-settings-${tag}.png` });
    } finally { await browser.close(); }
  });
}

await check("an unknown stored value shows Layered", async () => {
  const { browser, page } = await open(1280, "dark", "sideways", true);
  try { same(await page.evaluate(() => document.documentElement.getAttribute("data-depth")), null, "data-depth"); } finally { await browser.close(); }
});

if (log.length) console.log(log.join("\n"));
console.log(`${results.filter(Boolean).length}/${results.length} depth checks passed`);
process.exit(results.every(Boolean) && !log.length ? 0 : 1);
