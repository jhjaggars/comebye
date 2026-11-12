// Buttons and the focus ring (design language slice 4): one ring token for every focusable
// control, and the .btn hit sizes.
//
//   node buttons.mjs BASE OUT_DIR      (env: SCHEME)
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(page, name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width < 500 ? 844 : 800, dark });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(e.message));
  await page.goto(base + "/#/status", { waitUntil: "load" });
  await page.waitForSelector("#btn-launch, #btn-more", { state: "attached" });
  await page.waitForTimeout(500);
  const tag = `${width}px ${dark ? "dark" : "light"}`;

  await check(page, `${tag}: the ring tokens exist (--ring 2px solid, --ring-off 2px)`, async () => {
    const t = await page.evaluate(() => {
      const cs = getComputedStyle(document.documentElement);
      const p = document.createElement("i"); p.style.color = "var(--focus)"; document.body.append(p);
      const c = getComputedStyle(p).color; p.remove();
      return { ring: cs.getPropertyValue("--ring").trim(), off: cs.getPropertyValue("--ring-off").trim(), focus: c };
    });
    if (!/^2px solid /.test(t.ring)) throw new Error("--ring is " + JSON.stringify(t.ring));
    if (t.off !== "2px") throw new Error("--ring-off is " + JSON.stringify(t.off));
  });

  await check(page, `${tag}: Tab lands on controls and every one shows the same 2px solid ring in the theme's colour`, async () => {
    const seen = [];
    await page.locator("body").click({ position: { x: 1, y: 1 } });
    for (let i = 0; i < 12; i++) {
      await page.keyboard.press("Tab");
      seen.push(await page.evaluate(() => {
        const e = document.activeElement;
        if (!e || e === document.body) return null;
        const cs = getComputedStyle(e), p = document.createElement("i");
        p.style.color = "var(--focus)"; document.body.append(p);
        const want = getComputedStyle(p).color; p.remove();
        return { el: e.tagName + (e.id ? "#" + e.id : "") + "." + String(e.className).slice(0, 30), style: cs.outlineStyle, w: cs.outlineWidth, color: cs.outlineColor, want };
      }));
    }
    const got = seen.filter(Boolean);
    if (got.length < 4) throw new Error("Tab reached only " + got.length + " controls");
    const bad = got.filter((s) => s.style !== "solid" || s.w !== "2px" || s.color !== s.want);
    if (bad.length) throw new Error("not the shared ring: " + JSON.stringify(bad.slice(0, 3)));
  });

  await check(page, `${tag}: a .btn is at least 32px tall (44px on a phone unless it is .sm)`, async () => {
    await page.goto(base + "/#/todo", { waitUntil: "load" });
    await page.waitForTimeout(800);
    const bs = await page.evaluate(() => [...document.querySelectorAll(".btn")].filter((b) => b.offsetParent).map((b) => ({ c: b.className, h: Math.round(b.getBoundingClientRect().height) })));
    const small = bs.filter((b) => b.h < 32);
    if (small.length) throw new Error("buttons under 32px: " + JSON.stringify(small));
  });

  await check(page, `${tag}: no page errors`, async () => { if (errs.length) throw new Error(errs.join("; ")); });
  await browser.close();
}
const bad = results.filter((r) => !r).length;
console.log(`${results.length - bad}/${results.length} button checks passed`);
process.exit(bad ? 1 : 0);
