// Slight gradients (web/index.html "Gradients", web/themes.js gradient()): soft chrome and accent threads, under
// Surfaces: Layered. Computed-style checks, no screenshot comparison. By default, in Comebye Dark and Light and in
// Nord, at 390 and 1280: the bars end in a fade instead of a rule, the scrolling lists fade at their edge, the
// current tab has a fading accent line, a blocked row glows from its edge, the release card that needs you has a warm
// wash, the offline strip a soft wash, and the working and blocked dots are lit. Under each of prefers-contrast: more,
// prefers-reduced-transparency: reduce and forced-colors: active (emulated through CDP, and checked with matchMedia so
// the check cannot pass on a feature the browser ignored), and under Surfaces: Flat, every one of them is gone and the
// rules are back.
//
//   node gradients.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";
import { releaseView } from "./relfix.mjs";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
let failed = 0;
async function check(name, fn) {
  try { await fn(); console.log("ok   " + name); }
  catch (e) { failed++; console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

// Everything under test, read in one round trip. A pseudo-element is "on" when its content is not none.
// The list rows come from whichever agents the hub has, so a blocked row and the two lit dots are made here, inside
// #agents, with the app's own classes (as depth.mjs makes its card).
const probe = (page) => page.evaluate(() => {
  if (!document.getElementById("grad-probe")) {
    const d = document.createElement("div"); d.id = "grad-probe";
    d.innerHTML = '<div class="agent blocked"><span class="dot blocked"></span><div class="name">probe</div></div><div class="agent working"><span class="dot working"></span><div class="name">probe</div></div>';
    document.getElementById("agents").prepend(d);
  }
  const cs = (sel, pseudo) => { const e = document.querySelector(sel); return e ? getComputedStyle(e, pseudo || null) : null; };
  const on = (sel, pseudo) => { const s = cs(sel, pseudo); return !!s && s.content !== "none" && s.content !== "normal" && s.display !== "none"; };
  const img = (sel, pseudo) => { const s = cs(sel, pseudo); return s ? s.backgroundImage : null; };
  const strip = document.getElementById("hub-strip");
  if (strip) { strip.hidden = false; strip.dataset.kind = strip.dataset.kind || "offline"; } // its look, not its timing (offline.mjs has that)
  return {
    header: { fade: on("header", "::after"), fadeImg: img("header", "::after"), rule: cs("header").borderBottomColor },
    list: on("#agents", "::after"),
    blocked: img(".agent.blocked"), blockedGrad: cs(".agent.blocked")?.getPropertyValue("--grad").trim() || "",
    working: img(".dot.working"), blockedDot: img(".dot.blocked"),
    strip: strip ? img("#hub-strip") : null, stripFade: strip ? on("#hub-strip", "::after") : null,
  };
});
const probeTabs = (page) => page.evaluate(() => {
  const t = document.querySelector('.pr-tabs button[aria-selected="true"]'), a = getComputedStyle(t, "::after");
  const card = document.querySelector(".rl-card.rl-attn"), bar = getComputedStyle(document.querySelector(".pr-tabs"));
  return { line: a.content !== "none" ? a.backgroundImage : "none", border: getComputedStyle(t).borderBottomColor,
    tabsFade: getComputedStyle(document.querySelector(".pr-tabs"), "::after").content !== "none", tabsRule: bar.borderBottomColor,
    card: card ? getComputedStyle(card).backgroundImage : null };
});
const probeChat = (page) => page.evaluate(() => { const a = getComputedStyle(document.getElementById("chat"), "::after"); return { fade: a.content !== "none", img: a.backgroundImage }; });

const CLEAR = "rgba(0, 0, 0, 0)";
const has = (s, what) => typeof s === "string" && s.includes(what);

async function open({ width, theme, dark, depth, feature }) {
  const { browser, ctx, page } = await launch({ width, height: width < 500 ? 844 : 800, dark });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => errors.push("pageerror: " + e.message));
  await ctx.addInitScript(([t, d]) => { try { localStorage.setItem("comebye.theme", t); if (d) localStorage.setItem("comebye.depth", d); } catch {} }, [theme, depth || ""]);
  await page.route(/\/api\/releases(\?.*)?$/, (r) => r.fulfill({ json: releaseView("green") }));
  if (feature) {
    const cdp = await ctx.newCDPSession(page);
    await cdp.send("Emulation.setEmulatedMedia", { features: [{ name: feature[0], value: feature[1] }] });
  }
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent");
  if (feature) {
    const m = await page.evaluate((q) => matchMedia(q).matches, `(${feature[0]}: ${feature[1]})`);
    if (!m) throw new Error(`the browser did not take (${feature[0]}: ${feature[1]})`);
  }
  return { browser, page };
}
async function peers(page) {
  await page.goto(base + "/#/peers", { waitUntil: "load" });
  await page.click("#pr-tab-rel");
  await page.waitForSelector(".rl-card[data-card=release]");
}
// The first agent's conversation, whichever agent that is.
async function chat(page) {
  await page.goto(base + "/", { waitUntil: "load" });
  const id = await page.locator("#agents .agent[data-id]").first().getAttribute("data-id");
  await page.goto(base + "/#/agent/" + encodeURIComponent(id), { waitUntil: "load" });
  await page.waitForSelector("#detail #chat", { state: "attached" });
}

const THEMES = [["dark", true], ["light", false], ["nord", true]];
for (const width of [390, 1280]) {
  for (const [theme, dark] of THEMES) {
    await check(`${width} ${theme}: by default the bars fade, the lists fade, the tab line, the blocked glow, the release wash, the strip wash and the lit dots are drawn`, async () => {
      const { browser, page } = await open({ width, theme, dark });
      try {
        const p = await probe(page);
        if (width < 1100) {
          if (!p.header.fade || !has(p.header.fadeImg, "linear-gradient")) throw new Error("the header has no fade: " + JSON.stringify(p.header));
          if (p.header.rule !== CLEAR) throw new Error("the header still ends in a rule: " + p.header.rule);
        }
        if (!p.list) throw new Error("the agents list has no bottom fade");
        if (!has(p.blocked, "linear-gradient(90deg") || !p.blockedGrad) throw new Error("a blocked row has no edge glow: " + p.blocked);
        if (!has(p.working, "radial-gradient")) throw new Error("the working dot is not lit: " + p.working);
        if (!has(p.blockedDot, "radial-gradient")) throw new Error("the blocked dot is not lit: " + p.blockedDot);
        if (!has(p.strip, "linear-gradient(90deg") || !p.stripFade) throw new Error("the offline strip has no wash or fade: " + p.strip + " " + p.stripFade);
        await peers(page);
        const t = await probeTabs(page);
        if (!has(t.line, "linear-gradient(90deg")) throw new Error("the current tab has no fading line: " + t.line);
        if (t.border !== CLEAR) throw new Error("the current tab keeps its hard underline: " + t.border);
        if (!t.tabsFade || t.tabsRule !== CLEAR) throw new Error("the tab bar does not fade: " + JSON.stringify(t));
        if (!has(t.card, "linear-gradient")) throw new Error("the release that needs you has no wash: " + t.card);
        if (width === 390 && theme !== "nord") await page.screenshot({ path: `${out}/gradients-${width}-${theme}-releases.png` });
        await chat(page);
        const c = await probeChat(page);
        if (!c.fade || !has(c.img, "linear-gradient")) throw new Error("the conversation has no fade above the composer: " + JSON.stringify(c));
      } finally { await browser.close(); }
    });
  }
}

// The off switches. Each one alone must turn every gradient off and bring the rules back.
const OFF = [["prefers-contrast", "more"], ["prefers-reduced-transparency", "reduce"], ["forced-colors", "active"]];
for (const feature of OFF) {
  for (const [theme, dark] of [["dark", true], ["light", false]]) {
    await check(`390 ${theme}, (${feature.join(": ")}): no gradient anywhere, the rules are back`, async () => {
      const { browser, page } = await open({ width: 390, theme, dark, feature });
      try {
        const p = await probe(page);
        if (p.header.fade) throw new Error("the header still fades");
        if (feature[0] !== "forced-colors" && p.header.rule === CLEAR) throw new Error("the header's rule did not come back");
        if (p.list) throw new Error("the agents list still fades");
        if (has(p.blocked, "gradient")) throw new Error("a blocked row still glows: " + p.blocked);
        if (has(p.working, "gradient") || has(p.blockedDot, "gradient")) throw new Error("a dot is still lit: " + p.working + " " + p.blockedDot);
        if (has(p.strip, "gradient") || p.stripFade) throw new Error("the offline strip still has its wash or fade");
        await peers(page);
        const t = await probeTabs(page);
        if (t.line !== "none") throw new Error("the current tab still has the fading line");
        if (feature[0] !== "forced-colors" && t.border === CLEAR) throw new Error("the current tab lost its underline");
        if (t.tabsFade) throw new Error("the tab bar still fades");
        if (has(t.card, "gradient")) throw new Error("the release card still has its wash: " + t.card);
        await chat(page);
        if ((await probeChat(page)).fade) throw new Error("the conversation still fades above the composer");
      } finally { await browser.close(); }
    });
  }
}

await check("390 dark, Surfaces: Flat: no gradient (they belong to Layered)", async () => {
  const { browser, page } = await open({ width: 390, theme: "dark", dark: true, depth: "flat" });
  try {
    const p = await probe(page);
    if (p.header.fade || p.list || has(p.blocked, "gradient") || has(p.working, "gradient") || has(p.strip, "gradient")) throw new Error("a gradient under Flat: " + JSON.stringify(p));
    if (p.header.rule === CLEAR) throw new Error("the header lost its rule under Flat");
  } finally { await browser.close(); }
});

if (errors.length) { failed++; console.log("FAIL page errors\n     " + errors.join("\n     ")); }
console.log(failed ? `gradients: ${failed} failed` : "gradients: all ok");
process.exit(failed ? 1 : 0);
