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

  await check(page, `${tag}: action buttons on every screen share one look (12px radius, 600 weight, hairline, 32/40/44px)`, async () => {
    // the folded per-surface buttons (design language finish): one base look, three sizes
    const SEL = ".btn, .td-acts button, .ds-fix, .ds-confirm, .ds-fk, .ds-nav button, .br-sched button, .bf-stale-acts button, .empty button.empty-act, .tds-none button, .rel button:not(.rel-refresh), .rl-card button:not(.rel-refresh), #sp .cite button, #sp .hk button, .us-crumbs button";
    const seen = [], bad = [];
    for (const hash of ["#/todo", "#/status", "#/briefings", "#/usage", "#/peers", "#/pages"]) {
      await page.goto(base + "/" + hash, { waitUntil: "load" });
      await page.waitForTimeout(800);
      if (hash === "#/todo") { const row = page.locator(".td-row").first(); if (await row.count()) await row.click().catch(() => {}); await page.waitForTimeout(300); }
      const r = await page.evaluate((sel) => [...document.querySelectorAll(sel)].filter((b) => b.offsetParent).map((b) => {
        const cs = getComputedStyle(b);
        return { c: b.className.toString().slice(0, 30) || b.tagName, r: cs.borderTopLeftRadius, w: cs.fontWeight, bw: cs.borderTopWidth, h: Math.round(b.getBoundingClientRect().height) };
      }), SEL);
      for (const b of r) { seen.push(b); if (b.r !== "12px" || Number(b.w) < 600 || b.bw !== "1px" || ![32, 40, 44].includes(b.h)) bad.push(hash + " " + JSON.stringify(b)); }
    }
    if (!seen.length) throw new Error("no action button on any screen");
    if (bad.length) throw new Error(bad.slice(0, 4).join("\n"));
  });

  await check(page, `${tag}: segmented controls share one look (pressed = accent, 36/40/44px)`, async () => {
    const bad = [];
    let n = 0;
    for (const hash of ["#/usage", "#/briefings", "#/pages"]) {
      await page.goto(base + "/" + hash, { waitUntil: "load" });
      await page.waitForTimeout(800);
      const r = await page.evaluate(() => [...document.querySelectorAll(".seg button, .bd-seg button, .bc-seg button, .pgs-seg button, .us-seg button")].filter((b) => b.offsetParent).map((b) => {
        const cs = getComputedStyle(b);
        return { c: b.parentElement.className, p: b.getAttribute("aria-pressed"), bg: cs.backgroundColor, h: Math.round(b.getBoundingClientRect().height), w: cs.fontWeight };
      }));
      n += r.length;
      for (const b of r) if (![36, 40, 44].includes(b.h) || (b.p === "true" && Number(b.w) < 600)) bad.push(hash + " " + JSON.stringify(b));
      const pressed = r.filter((b) => b.p === "true").map((b) => b.bg), off = r.filter((b) => b.p === "false").map((b) => b.bg);
      if (pressed.length && off.length && pressed.some((p) => off.includes(p))) bad.push(hash + " pressed and unpressed look the same");
    }
    if (!n) throw new Error("no segmented control on any screen");
    if (bad.length) throw new Error(bad.slice(0, 4).join("\n"));
  });

  await check(page, `${tag}: no page errors`, async () => { if (errs.length) throw new Error(errs.join("; ")); });
  await browser.close();
}
const bad = results.filter((r) => !r).length;
console.log(`${results.length - bad}/${results.length} button checks passed`);
process.exit(bad ? 1 : 0);
