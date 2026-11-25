// Agent chat on a phone in landscape: a centred column of ~72% of the screen with the composer on it, a one-row
// header, and the notch's safe-area inset replacing our side padding instead of being added to it. It used to take
// the list's 640px cap (601-1099px wide fell between the phone rules and the desktop grid), a 72px header and the
// inset on top of our padding. Phone portrait and desktop keep their layout; a reading width picked in Appearance
// still applies in landscape.
//
//   node landscape.mjs BASE OUT_DIR       (env: SCHEME)
//
// Each size is a mobile, touch context; Chrome's Emulation.setSafeAreaInsetsOverride gives env(safe-area-inset-*)
// the values the phone reports in that orientation. It measures #chat's content box (its rect less its padding),
// the composer and the header against the viewport less the insets.
import { launch } from "./lib.mjs";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const scheme = dark ? "dark" : "light";
const results = [];
const ok = (name) => { results.push({ name, ok: true }); console.log("ok   " + name); };
const fail = (name, why) => { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + why); };

const SIZES = [
  // name, viewport, insets (left/right/top/bottom), mobile
  { name: "iphone-landscape", w: 844, h: 390, ins: { left: 47, right: 47, top: 0, bottom: 21 }, mobile: true },
  { name: "promax-landscape", w: 932, h: 430, ins: { left: 59, right: 59, top: 0, bottom: 21 }, mobile: true },
  { name: "android-landscape", w: 915, h: 412, ins: { left: 0, right: 0, top: 0, bottom: 0 }, mobile: true },
  { name: "iphone-landscape-full", w: 844, h: 390, ins: { left: 47, right: 47, top: 0, bottom: 21 }, mobile: true, col: "full" },
  { name: "iphone-portrait", w: 390, h: 844, ins: { left: 0, right: 0, top: 47, bottom: 34 }, mobile: true },
  { name: "desktop", w: 1280, h: 800, ins: { left: 0, right: 0, top: 0, bottom: 0 }, mobile: false },
];

const { agents } = await (await fetch(base + "/api/agents")).json();
const alpha = agents.find((a) => a.name === "alpha");
if (!alpha) { console.error("landscape.mjs: fixture agent alpha missing"); process.exit(2); }

const { browser } = await launch({ dark });
try {
  for (const s of SIZES) {
    const tag = `${s.name} ${s.w}x${s.h} (${scheme})`;
    const ctx = await browser.newContext({
      viewport: { width: s.w, height: s.h }, deviceScaleFactor: s.mobile ? 2 : 1, isMobile: s.mobile, hasTouch: s.mobile,
      colorScheme: scheme,
      ...(s.mobile ? { userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1" } : {}),
    });
    const page = await ctx.newPage();
    const errs = [];
    page.on("pageerror", (e) => errs.push(String(e)));
    try {
      const cdp = await ctx.newCDPSession(page);
      await cdp.send("Emulation.setSafeAreaInsetsOverride", { insets: s.ins });
      if (s.col) await ctx.addInitScript((c) => localStorage.setItem("comebye.col", c), s.col);
      await page.goto(base + "/#/agent/" + encodeURIComponent(alpha.pane_id), { waitUntil: "load" });
      await page.waitForSelector("#detail #chat .msg", { state: "visible", timeout: 15000 });
      await page.waitForTimeout(300);
      await page.screenshot({ path: `${out}/landscape-${s.name}-${scheme}.png` });
      const g = await page.evaluate(() => {
        const box = (el) => { const r = el.getBoundingClientRect(); return { l: r.left, r: r.right, w: r.width }; };
        const content = (el) => {
          const r = el.getBoundingClientRect(), cs = getComputedStyle(el);
          const l = r.left + parseFloat(cs.borderLeftWidth) + parseFloat(cs.paddingLeft);
          const rr = r.right - parseFloat(cs.borderRightWidth) - parseFloat(cs.paddingRight) - (el.offsetWidth - el.clientWidth - parseFloat(cs.borderLeftWidth) - parseFloat(cs.borderRightWidth));
          return { l, r: rr, w: rr - l };
        };
        const hdr = document.querySelector("#detail header");
        const hcs = getComputedStyle(hdr);
        return {
          vw: innerWidth, sw: document.documentElement.scrollWidth,
          chat: content(document.querySelector("#detail #chat")),
          prompt: box(document.querySelector("#detail main > .prompt")),
          header: { l: hdr.getBoundingClientRect().left + parseFloat(hcs.paddingLeft), r: hdr.getBoundingClientRect().right - parseFloat(hcs.paddingRight), h: hdr.getBoundingClientRect().height, b: hdr.getBoundingClientRect().bottom },
          chatTop: document.querySelector("#detail #chat").getBoundingClientRect().top,
        };
      });
      const L = s.ins.left, R = s.ins.right, avail = s.w - L - R;
      const fmt = (b) => `${Math.round(b.l)}..${Math.round(b.r)} (w ${Math.round(b.r - b.l)})`;
      const geo = `chat ${fmt(g.chat)} from y ${Math.round(g.chatTop)}, composer ${fmt(g.prompt)}, header ${fmt(g.header)} h ${Math.round(g.header.h)}, viewport ${g.vw}, safe ${L}..${s.w - R}`;
      console.log(`     ${tag}: ${geo}`);
      const why = [];
      if (g.sw > g.vw + 1) why.push(`sideways scroll: scrollWidth ${g.sw} > ${g.vw}`);
      if (s.name.includes("landscape")) {
        // Within the safe area, with our padding replaced by the notch inset (not added to it).
        if (g.chat.l < L - 0.5 || g.chat.r > s.w - R + 0.5) why.push(`chat text under the notch: ${fmt(g.chat)} vs safe ${L}..${s.w - R}`);
        if (g.prompt.l < L - 0.5 || g.prompt.r > s.w - R + 0.5) why.push(`composer under the notch: ${fmt(g.prompt)}`);
        if (g.header.l < L - 0.5 || g.header.r > s.w - R + 0.5) why.push(`header under the notch: ${fmt(g.header)}`);
        if (g.header.l > Math.max(16, L) + 0.5 || g.header.r < s.w - Math.max(16, R) - 0.5) why.push(`header padding stacked on the inset: ${fmt(g.header)}`);
        // The column: ~72% of the screen, centred (Full: the safe area less the chat's 10px a side); the composer on it.
        const want = s.col === "full" ? avail - 2 * 10 : Math.min(0.72 * s.w, avail - 2 * 10); // Full: only the case with the notch on both sides
        if (Math.abs(g.chat.w - want) > 3) why.push(`chat column ${Math.round(g.chat.w)} px, want ~${Math.round(want)} of ${avail} usable`);
        const ml = g.chat.l - L, mr = s.w - R - g.chat.r;
        if (Math.abs(ml - mr) > 2) why.push(`chat column off centre: ${Math.round(ml)} px left, ${Math.round(mr)} px right`);
        if (Math.abs(g.prompt.l - g.chat.l) > 1 || Math.abs(g.prompt.r - g.chat.r) > 1) why.push(`composer not on the column: ${fmt(g.prompt)} vs ${fmt(g.chat)}`);
        // Height is scarce: a one-row header and the conversation right under it.
        if (g.header.h > 46) why.push(`header ${Math.round(g.header.h)} px tall, want one 44px row`);
        if (g.chatTop - g.header.b > 1) why.push(`${Math.round(g.chatTop - g.header.b)} px between the header and the conversation`);
      } else if (s.name === "iphone-portrait") {
        // Unchanged: the transcript runs edge to edge less 6px, the composer less 12px a side.
        if (Math.round(g.chat.l) !== 6 || Math.round(g.chat.r) !== s.w - 6) why.push(`portrait chat moved: ${fmt(g.chat)} (want 6..${s.w - 6})`);
        if (Math.round(g.prompt.l) !== 12 || Math.round(g.prompt.r) !== s.w - 12) why.push(`portrait composer moved: ${fmt(g.prompt)} (want 12..${s.w - 12})`);
      } else {
        // Desktop: the default 820px reading column, centred in the pane beside the list, the composer lined up with it.
        if (Math.abs(g.chat.w - 820) > 1) why.push(`desktop column ${Math.round(g.chat.w)}, want 820`);
        if (Math.abs(g.prompt.l - g.chat.l) > 1 || Math.abs(g.prompt.r - g.chat.r) > 1) why.push(`desktop composer not lined up with the column: ${fmt(g.prompt)} vs ${fmt(g.chat)}`);
      }
      if (errs.length) why.push("page errors: " + errs.join("; "));
      if (why.length) fail(`agent chat at ${tag}`, why.join("\n     ")); else ok(`agent chat at ${tag}`);
    } catch (e) {
      fail(`agent chat at ${tag}`, String(e.message).split("\n")[0]);
    } finally {
      await ctx.close().catch(() => {});
    }
  }
} finally {
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`\n${results.length - bad}/${results.length} landscape checks passed`);
process.exit(bad ? 1 : 0);
