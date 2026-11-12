// Segmented controls, menus and sheets, toasts and banners (design language slice 6): the shared
// look and the stacking order, read back from computed styles.
//
//   node controls.mjs BASE OUT_DIR      (env: SCHEME)
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width < 500 ? 844 : 800, dark });
  page.setDefaultTimeout(8000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(e.message));
  const tag = `${width}px ${dark ? "dark" : "light"}`;
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#btn-fleet");
  await page.waitForTimeout(600);

  await check(`${tag}: the stacking tokens are ordered backdrop < menu < popup < sheet < screen < scrim < toast`, async () => {
    const z = await page.evaluate(() => {
      const cs = getComputedStyle(document.documentElement), g = (n) => Number(cs.getPropertyValue("--z-" + n));
      return ["raised", "pop", "bar", "pill", "view", "backdrop", "menu", "popup", "sheet", "screen", "scrim", "toast", "toast-over"].map(g);
    });
    for (let i = 1; i < z.length; i++) if (!(z[i] > z[i - 1])) throw new Error("not increasing: " + z.join(", "));
  });

  await check(`${tag}: a segmented control is a 10px-radius hairline box and its pressed segment is the accent`, async () => {
    for (const hash of ["#/status", "#/usage", "#/briefings", "#/search"]) {
      await page.goto(base + "/" + hash, { waitUntil: "load" });
      await page.waitForTimeout(700);
      const r = await page.evaluate(() => {
        const blue = (() => { const p = document.createElement("i"); p.style.color = "var(--blue)"; document.body.append(p); const c = getComputedStyle(p).color; p.remove(); return c; })();
        return [...document.querySelectorAll(".seg, .bd-seg, .bc-seg, .pgs-seg, .us-seg")].filter((e) => e.offsetParent).map((e) => {
          const cs = getComputedStyle(e), on = e.querySelector('button[aria-pressed="true"]');
          return { c: e.className, r: cs.borderTopLeftRadius, bw: cs.borderTopWidth, on: on ? getComputedStyle(on).backgroundColor === blue : null };
        });
      });
      for (const s of r) if (s.r !== "10px" || s.bw !== "1px" || s.on === false) throw new Error(hash + " " + JSON.stringify(s));
    }
  });

  await check(`${tag}: the fleet menu opens, sits over the page, and is a bottom sheet with a grab handle on a phone`, async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#btn-fleet");
    await page.locator("#btn-fleet").click();
    await page.waitForSelector("#f-menu:not([hidden])");
    await page.waitForTimeout(500); // the sheet slides in
    const m = await page.evaluate(() => {
      const e = document.getElementById("f-menu"), cs = getComputedStyle(e), g = e.querySelector(".grab");
      const t = getComputedStyle(document.documentElement);
      return { z: cs.zIndex, pos: cs.position, rad: cs.borderTopLeftRadius, grab: g ? getComputedStyle(g).display : "none", sheet: t.getPropertyValue("--z-sheet").trim(), popup: t.getPropertyValue("--z-popup").trim(), bk: getComputedStyle(document.getElementById("sheet-bk")).display, bottom: e.getBoundingClientRect().bottom, vh: innerHeight };
    });
    if (m.pos !== "fixed") throw new Error("position " + m.pos);
    if (width < 600) {
      if (m.z !== m.sheet) throw new Error(`phone sheet z ${m.z}, want ${m.sheet}`);
      if (m.rad !== "16px") throw new Error("sheet radius " + m.rad);
      if (m.grab === "none") throw new Error("no grab handle");
      if (m.bk === "none") throw new Error("no backdrop");
      if (Math.round(m.bottom) !== m.vh) throw new Error("not docked to the bottom");
    } else if (m.z !== m.popup) throw new Error(`popover z ${m.z}, want ${m.popup}`);
    await page.keyboard.press("Escape");
    await page.waitForSelector("#f-menu", { state: "hidden" });
  });

  await check(`${tag}: a banner and the briefing card share the 12px notice surface`, async () => {
    const r = await page.evaluate(() => [...document.querySelectorAll(".rtr-banner, .br-card, .notice")].filter((e) => e.offsetParent).map((e) => ({ c: e.className, r: getComputedStyle(e).borderTopLeftRadius, bw: getComputedStyle(e).borderTopWidth })));
    for (const s of r) if (s.r !== "12px" || s.bw !== "1px") throw new Error(JSON.stringify(s));
  });

  await check(`${tag}: no page errors`, async () => { if (errs.length) throw new Error(errs.join("; ")); });
  await browser.close();
}
const nbad = results.filter((r) => !r).length;
console.log(`${results.length - nbad}/${results.length} control checks passed`);
process.exit(nbad ? 1 : 0);
