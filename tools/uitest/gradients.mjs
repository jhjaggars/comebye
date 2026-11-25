// Slight gradients (web/index.html "Gradients", web/themes.js gradient()): soft chrome and accent threads, under
// Surfaces: Layered. Computed-style checks, and a pixel check of the top fade. By default, in Comebye Dark and Light
// and in Nord, at 390 and 1280: the conversation's header fades into the messages instead of ending in a rule, every
// other bar keeps its rule and has no fade, the scrolling lists fade at their bottom edge, the
// current tab has a fading accent line, a blocked row glows from its edge, the release card that needs you has a warm
// wash, the offline strip a soft wash, and the working and blocked dots are lit. Under each of prefers-contrast: more,
// prefers-reduced-transparency: reduce and forced-colors: active (emulated through CDP, and checked with matchMedia so
// the check cannot pass on a feature the browser ignored), and under Surfaces: Flat, every one of them is gone and the
// rules are back. The top fade (pixels, at 390x844 and 844x390, light and dark): on the list, To do, Pages, Status,
// Peers and Settings the band under the top bars is the same with every pseudo-element hidden (nothing is drawn over
// the screen's real colours); in a conversation a message at rest is the same too, and one scrolled under the header
// is faded.
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
const probeChat = (page) => page.evaluate(() => {
  const c = document.getElementById("chat"), a = getComputedStyle(c, "::after"), b = getComputedStyle(c, "::before");
  return { fade: a.content !== "none", img: a.backgroundImage, top: b.content !== "none" && b.backgroundImage.includes("linear-gradient"),
    rule: getComputedStyle(document.querySelector("#detail > header")).borderBottomColor };
});

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
// An agent's conversation: the first row whose pane shows the chat (one without a transcript shows the raw terminal).
// A row's data-id is "host/pane"; the route takes each part encoded on its own (app.js agentHash).
let chatId = null; // the row found the first time, tried first after that
async function chat(page) {
  await page.goto(base + "/", { waitUntil: "load" });
  const ids = await page.locator("#agents .agent[data-id]").evaluateAll((es) => es.slice(0, 8).map((e) => e.dataset.id));
  if (chatId) ids.unshift(chatId);
  for (const id of ids) {
    await page.goto(base + "/#/agent/" + id.split("/").map(encodeURIComponent).join("/"), { waitUntil: "load" });
    // The pane can show the chat first and switch to the terminal once it learns there is no transcript: wait it out.
    try { await page.waitForSelector("#detail #chat .msg", { state: "visible", timeout: 2000 }); } catch { continue; }
    await page.waitForTimeout(600);
    if (await page.locator("#detail #chat").isVisible()) { chatId = id; return; }
  }
  throw new Error("no agent among " + ids.join(", ") + " shows a chat");
}

const THEMES = [["dark", true], ["light", false], ["nord", true]];
for (const width of [390, 1280]) {
  for (const [theme, dark] of THEMES) {
    await check(`${width} ${theme}: by default only the conversation's header fades, the lists fade, the tab line, the blocked glow, the release wash, the strip wash and the lit dots are drawn`, async () => {
      const { browser, page } = await open({ width, theme, dark });
      try {
        const p = await probe(page);
        // Only the conversation's header fades (below); the list's header keeps its rule and draws nothing under itself.
        if (p.header.fade) throw new Error("the list's header draws a fade over the list: " + JSON.stringify(p.header));
        if (width < 1100 && p.header.rule === CLEAR) throw new Error("the list's header lost its rule");
        if (!p.list) throw new Error("the agents list has no bottom fade");
        if (!has(p.blocked, "linear-gradient(90deg") || !p.blockedGrad) throw new Error("a blocked row has no edge glow: " + p.blocked);
        if (!has(p.working, "radial-gradient")) throw new Error("the working dot is not lit: " + p.working);
        if (!has(p.blockedDot, "radial-gradient")) throw new Error("the blocked dot is not lit: " + p.blockedDot);
        if (!has(p.strip, "linear-gradient(90deg")) throw new Error("the offline strip has no wash: " + p.strip);
        if (p.stripFade) throw new Error("the offline strip draws a fade over what is under it");
        await peers(page);
        const t = await probeTabs(page);
        if (!has(t.line, "linear-gradient(90deg")) throw new Error("the current tab has no fading line: " + t.line);
        if (t.border !== CLEAR) throw new Error("the current tab keeps its hard underline: " + t.border);
        if (t.tabsFade || t.tabsRule === CLEAR) throw new Error("the tab bar fades over the list instead of ending in its rule: " + JSON.stringify(t));
        if (!has(t.card, "linear-gradient")) throw new Error("the release that needs you has no wash: " + t.card);
        if (width === 390 && theme !== "nord") await page.screenshot({ path: `${out}/gradients-${width}-${theme}-releases.png` });
        await chat(page);
        const c = await probeChat(page);
        if (!c.fade || !has(c.img, "linear-gradient")) throw new Error("the conversation has no fade above the composer: " + JSON.stringify(c));
        if (!c.top || c.rule !== CLEAR) throw new Error("the conversation's header does not fade into the messages: " + JSON.stringify(c));
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
        const c = await probeChat(page);
        if (c.fade || c.top) throw new Error("the conversation still fades: " + JSON.stringify(c));
        if (feature[0] !== "forced-colors" && c.rule === CLEAR) throw new Error("the conversation's header lost its rule");
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

// The top fade, by pixels. NOPSEUDO hides every ::before/::after, so a band that is the same with and without it has
// nothing drawn over it. The band is the 24 px under the lowest top bar of the screen (header, tab bar, Pages bar).
const NOPSEUDO = "*::before, *::after { visibility: hidden !important; }";
const band = async (page, y, h = 24) => {
  const vp = page.viewportSize();
  return page.screenshot({ clip: { x: 0, y, width: vp.width, height: h }, animations: "disabled" });
};
// How many pixels differ by more than 2 levels in a channel: anti-aliasing can shift an edge by a level when a layer
// changes, a fade over a different colour differs by far more (10+ levels in its top rows). Decoded in the page.
const differing = (page, a, b) => page.evaluate(async ([a, b]) => {
  const px = async (s) => {
    const bmp = await createImageBitmap(new Blob([Uint8Array.from(atob(s), (ch) => ch.charCodeAt(0))], { type: "image/png" }));
    const c = new OffscreenCanvas(bmp.width, bmp.height), x = c.getContext("2d"); x.drawImage(bmp, 0, 0);
    return x.getImageData(0, 0, bmp.width, bmp.height).data;
  };
  const [p, q] = [await px(a), await px(b)];
  let n = 0;
  for (let i = 0; i < p.length; i += 4) if (Math.max(Math.abs(p[i] - q[i]), Math.abs(p[i + 1] - q[i + 1]), Math.abs(p[i + 2] - q[i + 2])) > 2) n++;
  return n;
}, [a.toString("base64"), b.toString("base64")]);
const sameWithout = async (page, y, h) => {
  const a = await band(page, y, h);
  const tag = await page.addStyleTag({ content: NOPSEUDO });
  const b = await band(page, y, h);
  await tag.evaluate((e) => e.remove());
  return (await differing(page, a, b)) === 0;
};
const barBottom = (page) => page.evaluate(() => {
  const scr = [...document.querySelectorAll(".screen:not([hidden]), #st-screen:not([inert]), #list")].find((e) => e.checkVisibility()) || document.body;
  let y = 0;
  for (const e of scr.querySelectorAll("header, .pr-tabs, .pgs-bar, .rs-bar, .hub-strip")) {
    if (!e.checkVisibility()) continue;
    const r = e.getBoundingClientRect(); if (r.top < 200 && r.bottom > y) y = r.bottom;
  }
  return Math.ceil(y);
});
const SCREENS = [["list", "/"], ["To do", "/#/todo"], ["Pages", "/#/pages"], ["Status", "/#/status"], ["Peers", "/#/peers"], ["Settings", "/#/settings"]];
for (const [width, height] of [[390, 844], [844, 390]]) {
  for (const [theme, dark] of [["light", false], ["dark", true]]) {
    await check(`${width}x${height} ${theme}: the top fade is only over messages scrolled under the conversation's header`, async () => {
      // A phone either way round: touch and a coarse pointer, so 844x390 takes the landscape layout (44 px bars).
      const { browser: owner } = await launch({ dark });
      const browser = await owner.newContext({ viewport: { width, height }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, colorScheme: dark ? "dark" : "light",
        userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1" });
      const page = await browser.newPage();
      page.setDefaultTimeout(8000);
      page.on("pageerror", (e) => errors.push("pageerror: " + e.message));
      try {
        await page.addInitScript((t) => { try { localStorage.setItem("comebye.theme", t); } catch {} }, theme);
        await page.goto(base + "/", { waitUntil: "load" });
        if (!(await page.evaluate(() => matchMedia("(pointer: coarse)").matches))) throw new Error("the browser did not take a coarse pointer");
        for (const [name, route] of SCREENS) {
          await page.goto(base + route, { waitUntil: "load" });
          if (route === "/") await page.waitForSelector("#agents .agent");
          await page.waitForTimeout(500);
          const y = await barBottom(page);
          if (!(await sameWithout(page, y))) throw new Error(`${name}: something is drawn over the 24 px under its top bar (y ${y})`);
        }
        await chat(page);
        await page.waitForTimeout(300);
        // Enough messages to scroll, made with the app's own classes; then the band at rest from the first message down,
        // and the band under the header once a message has scrolled under it.
        await page.evaluate(() => {
          const c = document.getElementById("chat");
          c.replaceChildren();
          for (let i = 0; i < 30; i++) { const m = document.createElement("div"); m.className = "msg assistant"; m.innerHTML = '<div class="bubble">probe message ' + i + " with enough words to fill a line</div>"; c.append(m); }
          c.scrollTop = 0;
        });
        await page.waitForTimeout(200);
        const g = await page.evaluate(() => {
          const c = document.getElementById("chat").getBoundingClientRect(), m = document.querySelector("#chat .msg").getBoundingClientRect();
          return { top: Math.ceil(c.top), first: Math.ceil(m.top), header: Math.ceil(document.querySelector("#detail > header").getBoundingClientRect().bottom) };
        });
        if (g.first < g.top) throw new Error("the first message starts above the scroller: " + JSON.stringify(g));
        if (!(await sameWithout(page, g.first, 24))) throw new Error("the top fade covers the first message at rest: " + JSON.stringify(g));
        await page.evaluate(() => { document.getElementById("chat").scrollTop = 200; });
        await page.waitForTimeout(200);
        if (await sameWithout(page, g.top, 8)) throw new Error("no fade over a message scrolled under the conversation's header: " + JSON.stringify(g));
        await page.screenshot({ path: `${out}/topfade-${width}x${height}-${theme}-chat.png` });
      } finally { await browser.close(); owner.close(); }
    });
  }
}

if (errors.length) { failed++; console.log("FAIL page errors\n     " + errors.join("\n     ")); }
console.log(failed ? `gradients: ${failed} failed` : "gradients: all ok");
process.exit(failed ? 1 : 0);
