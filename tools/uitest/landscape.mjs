// Agent chat on a phone in landscape: a centred column of ~72% of the screen with the composer on it, a one-row
// header, and the notch's safe-area inset replacing our side padding instead of being added to it. It used to take
// the list's 640px cap (601-1099px wide fell between the phone rules and the desktop grid), a 72px header and the
// inset on top of our padding. Phone portrait and desktop keep their layout; a reading width picked in Appearance
// still applies in landscape.
//
// Every other screen gets the same treatment (web/index.html, "phone landscape: every screen"): screens of text and rows
// sit on the chat's 72vw column, grids, tables and charts keep the whole safe area, every bar is one 44px row, the
// inset replaces our padding, and menus, sheets and dialogs fit the 390px height, scrolling inside. Portrait and
// desktop must measure exactly what they did before (BASE below, recorded from the code before the change).
//
//   node landscape.mjs BASE OUT_DIR       (env: SCHEME; LANDSCAPE_RECORD=1 prints the portrait/desktop boxes for BASE)
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

  // ---- Every other screen ----
  // A to-do to open and a page to show: added here, dropped at the end.
  const J = { "content-type": "application/json" };
  const td = (await (await fetch(base + "/api/todos", { method: "POST", headers: J, body: JSON.stringify({ no_wait: true, kind: "decision", question: "Landscape: which key should the service use?", context: "Two ways.\n\n1. Mint a key\n2. Reuse the old key\n\nMy recommendation: ok 2" }) })).json()).todo;
  const pub = await (await fetch(base + "/api/pages", { method: "POST", headers: J, body: JSON.stringify({ title: "Landscape page", kind: "md", body: "# Landscape page\n\nA paragraph.\n" }) })).json();
  const pgid = (pub.page || pub).id;
  const chat = "#/agent/" + encodeURIComponent(alpha.pane_id);
  // kind: "col" sits on the chat's 72vw column, "wide" fills the safe area less our padding, "edge" is exactly the
  // safe area. box is measured as its content box (its rect less its padding), or its border box with edge.
  const SCREENS = [
    { n: "agents list", hash: "#/", wait: "#agents .agent", box: "#list > main", bar: "#list > header", kind: "col" },
    { n: "To do, an open item", hash: "#/todo/" + td.id, wait: "#todo .td-item.open", box: "#list > main", bar: "#list > header", kind: "col", phone: true },
    { n: "Status", hash: "#/status", wait: "#st-screen.open .ss-body", box: ".ss-body", bar: "#st-screen > header", kind: "col", phone: true },
    { n: "Pages", hash: "#/pages", wait: "#pages .pgs-card", box: ".pgs-list", bar: "#pages > header", kind: "wide" },
    { n: "a page", hash: "#/pages/" + pgid, wait: "#pgs-view iframe.pgs-frame", box: "#pgs-view .pgs-frame", bar: "#pgs-view .pgs-bar", kind: "edge" },
    { n: "Peers: Hosts", hash: "#/peers", wait: "#peers .pr-card", box: ".pr-list", bar: "#peers > header", tabs: ".pr-tabs", kind: (w) => (w < 860 ? "col" : "split") }, // from 860px: the list beside a host
    { n: "Peers: Prices", hash: "#/peers/~prices", wait: "#pr-prices:not([hidden])", box: "#pr-prices", bar: "#peers > header", tabs: ".pr-tabs", kind: "wide" },
    { n: "Peers: Releases", hash: "#/peers/~releases", wait: "#pr-releases:not([hidden])", box: "#pr-releases", bar: "#peers > header", tabs: ".pr-tabs", kind: (w) => (w < 900 ? "col" : "wide") }, // from 900px: two columns
    { n: "Archived", hash: "#/archived", wait: "#ar-body", box: "#ar-body", bar: "#archived > header", kind: "col" },
    { n: "Usage", hash: "#/usage", wait: "#us-view .us-card", box: "#us-body", bar: "#usage > header", kind: "wide" },
    { n: "Settings and Appearance", hash: "#/settings", wait: "#sx-view .sx-field", box: "#sx-body", bar: "#settings > header", kind: "col" },
    { n: "New agent", hash: "#/", wait: "#agents .agent", click: ["#btn-launch"], box: "#launch .ln-body", foot: "#launch .ln-foot", bar: "#launch > header", sheet: "#launch", scroller: "#launch .ln-body", full: true, kind: "col" },
    { n: "offline strip", hash: "#/", wait: "#agents .agent", offline: true, box: "#list > main", bar: "#list > header", strip: "#hub-strip", kind: "col" },
    // Menus, sheets and the palette: inside the viewport and the safe area, scrolling inside when they are taller.
    { n: "command palette", hash: "#/", wait: "#agents .agent", click: ["#btn-cmds"], sheet: "#palette .pal", scroller: "#pal-list" },
    { n: "fleet menu", hash: "#/", wait: "#agents .agent", click: ["#btn-fleet"], sheet: "#f-menu", scroller: "#f-menu" },
    { n: "notifications", hash: "#/", wait: "#agents .agent", click: ["#btn-notify"], sheet: "#notify-panel", scroller: "#notify-panel" },
    { n: "row menu", hash: "#/", wait: "#agents .agent .row-more", click: ["#agents .agent .row-more"], sheet: "#row-menu", scroller: "#row-menu" },
    { n: "agent menu", hash: chat, wait: "#detail #chat .msg", click: ["#d-more"], sheet: "#d-menu", scroller: "#d-menu" },
    ...["doc", "diff", "status", "pages"].map((t) => ({ n: `side panel: ${t}`, hash: chat, wait: "#detail #chat .msg", click: ["#sp-btn", `#sp-tab-${t}`], sheet: "#sp", scroller: "#sp-body", first: "#sp .sp-tab", panel: true })),
  ];
  // Portrait and desktop, from the code before this change: [content left, content right, bar height] per screen.
  const BASE = {
    "iphone-portrait": {
      "agents list": [12, 378, 98],
      "To do, an open item": [12, 378, 98],
      "Status": [12, 378, 100],
      "Pages": [12, 378, 98],
      "a page": [0, 390, 95],
      "Peers: Hosts": [12, 378, 98],
      "Peers: Prices": [12, 378, 98],
      "Peers: Releases": [12, 378, 98],
      "Archived": [12, 378, 98],
      "Usage": [12, 378, 98],
      "Settings and Appearance": [12, 378, 98],
      "New agent": [16, 374, 100],
      "offline strip": [12, 378, 98],
    },
    "desktop": {
      "agents list": [12, -12, 51],
      "Pages": [385, 1256, 56],
      "a page": [361, 1280, 48],
      "Peers: Hosts": [373, 738, 56],
      "Peers: Prices": [373, 1268, 56],
      "Peers: Releases": [373, 1268, 56],
      "Archived": [373, 1268, 56],
      "Usage": [373, 1268, 56],
      "Settings and Appearance": [373, 1268, 56],
      "New agent": [336, 944, 53],
      "offline strip": [12, -12, 51],
    },
  };
  const record = {};
  for (const s of SIZES.filter((x) => !x.col)) {
    const land = s.name.includes("landscape");
    const L = s.ins.left, R = s.ins.right, side = Math.max(L, R);
    for (const sc of SCREENS) {
      if (!land && !sc.box) continue;
      const tag = `${sc.n} at ${s.name} ${s.w}x${s.h} (${scheme})`;
      const ctx = await browser.newContext({
        viewport: { width: s.w, height: s.h }, deviceScaleFactor: s.mobile ? 2 : 1, isMobile: s.mobile, hasTouch: s.mobile, colorScheme: scheme,
        ...(s.mobile ? { userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1" } : {}),
      });
      const page = await ctx.newPage();
      const errs = [];
      page.on("pageerror", (e) => errs.push(String(e)));
      try {
        const cdp = await ctx.newCDPSession(page);
        await cdp.send("Emulation.setSafeAreaInsetsOverride", { insets: s.ins });
        await page.goto(base + "/" + sc.hash, { waitUntil: "load" });
        const vis = await page.waitForSelector(sc.wait, { state: "visible", timeout: 10000 }).then(() => true, () => false);
        if (!vis) {
          // Status is a phone screen: the desktop shows it as a page in the main pane instead.
          if (!land && sc.phone && !s.mobile) { await ctx.close(); continue; }
          throw new Error(`${sc.wait} never showed`);
        }
        for (const c of sc.click || []) { await page.locator(c).first().click({ timeout: 4000 }); await page.waitForTimeout(350); }
        if (sc.offline) {
          await ctx.setOffline(true);
          await page.waitForFunction(() => { const h = document.getElementById("hub-strip"); return h && !h.hidden && h.dataset.kind === "offline"; }, null, { timeout: 6000 });
        }
        await page.waitForTimeout(300);
        if (land) await page.screenshot({ path: `${out}/landscape-${sc.n.replace(/[^a-z0-9]+/gi, "-").toLowerCase()}-${s.name}-${scheme}.png` });
        const kind = typeof sc.kind === "function" ? sc.kind(s.w) : sc.kind;
        const g = await page.evaluate((sc) => {
          const q = (sel) => document.querySelector(sel);
          const content = (el) => {
            const r = el.getBoundingClientRect(), cs = getComputedStyle(el);
            return { l: r.left + parseFloat(cs.borderLeftWidth) + parseFloat(cs.paddingLeft), r: r.right - parseFloat(cs.borderRightWidth) - parseFloat(cs.paddingRight) - (el.offsetWidth - el.clientWidth - parseFloat(cs.borderLeftWidth) - parseFloat(cs.borderRightWidth)) };
          };
          const rect = (el) => { const r = el.getBoundingClientRect(); return { l: r.left, r: r.right, t: r.top, b: r.bottom, h: r.height }; };
          const o = { vw: innerWidth, vh: innerHeight, sw: document.documentElement.scrollWidth };
          if (sc.box) { const el = q(sc.box); o.box = sc.kind === "edge" ? rect(el) : content(el); o.boxSw = el.scrollWidth > el.clientWidth + 1 ? `${el.scrollWidth} > ${el.clientWidth}` : ""; }
          if (sc.foot) o.foot = content(q(sc.foot));
          if (sc.bar) o.bar = rect(q(sc.bar));
          if (sc.tabs) o.tabs = rect(q(sc.tabs));
          if (sc.strip) { const st = q(sc.strip); o.strip = { ...content(st), h: st.getBoundingClientRect().height }; }
          if (sc.sheet) {
            const el = q(sc.sheet), sx = q(sc.scroller), cs = getComputedStyle(sx);
            o.sheet = rect(el);
            o.sheetVisible = !!el && el.getBoundingClientRect().height > 0 && getComputedStyle(el).display !== "none";
            o.overflows = sx.scrollHeight > sx.clientHeight + 1;
            o.scrolls = /auto|scroll/.test(cs.overflowY);
            o.scrollerBottom = sx.getBoundingClientRect().bottom;
          }
          if (sc.first) o.first = rect(q(sc.first));
          if (sc.panel) { const b = q("#sp-body"); o.panelBody = content(b); }
          return o;
        }, { ...sc, kind });
        const why = [];
        const r1 = (n) => Math.round(n);
        const fmt = (b) => `${r1(b.l)}..${r1(b.r)} (w ${r1(b.r - b.l)})`;
        if (g.sw > g.vw + 1) why.push(`sideways scroll: scrollWidth ${g.sw} > ${g.vw}`);
        if (g.boxSw) why.push(`${sc.box} scrolls sideways: ${g.boxSw}`);
        if (!land) {
          const got = [r1(g.box.l), r1(g.box.r), g.bar ? r1(g.bar.h) : 0];
          (record[s.name] ||= {})[sc.n] = got;
          const want = BASE[s.name][sc.n];
          if (!want) why.push(`no baseline for ${sc.n} at ${s.name}: got ${JSON.stringify(got)}`);
          else if (got.join() !== want.join()) why.push(`${s.name} moved: ${sc.box} ${got[0]}..${got[1]}, bar ${got[2]} px; was ${want[0]}..${want[1]}, bar ${want[2]} px`);
        } else {
          if (g.box) {
            console.log(`     ${tag}: ${sc.box} ${fmt(g.box)}, safe ${L}..${s.w - R}${g.bar ? `, bar h ${r1(g.bar.h)}` : ""}${g.tabs ? `, tabs h ${r1(g.tabs.h)}` : ""}`);
            if (g.box.l < L - 0.5 || g.box.r > s.w - R + 0.5) why.push(`${sc.box} under the notch: ${fmt(g.box)} vs safe ${L}..${s.w - R}`);
            if (kind === "col") {
              // The chat's column: 72% of the screen, centred in the safe area.
              const want = Math.min(0.72 * s.w, s.w - 2 * Math.max(12, side));
              if (Math.abs(g.box.r - g.box.l - want) > 2) why.push(`column ${r1(g.box.r - g.box.l)} px, want ~${r1(want)} (72% of ${s.w})`);
              const ml = g.box.l - L, mr = s.w - R - g.box.r;
              if (Math.abs(ml - mr) > 2) why.push(`column off centre: ${r1(ml)} px left, ${r1(mr)} px right`);
              if (g.foot && (Math.abs(g.foot.l - g.box.l) > 1 || Math.abs(g.foot.r - g.box.r) > 1)) why.push(`footer not on the column: ${fmt(g.foot)} vs ${fmt(g.box)}`);
            } else if (kind === "wide") {
              // The safe area less our 12px, not the inset plus 12px.
              const wl = Math.max(12, L), wr = s.w - Math.max(12, R);
              if (Math.abs(g.box.l - wl) > 1 || Math.abs(g.box.r - wr) > 1) why.push(`${sc.box} ${fmt(g.box)}, want ${wl}..${wr}: padding stacked on the inset, or a cap`);
            } else if (kind === "split") {
              // The list pane beside the host: our padding or the inset on the notch side, not both.
              if (Math.abs(g.box.l - Math.max(12, L)) > 1) why.push(`${sc.box} starts at ${r1(g.box.l)}, want ${Math.max(12, L)}: padding stacked on the inset`);
            } else if (kind === "edge") {
              if (Math.abs(g.box.l - L) > 1 || Math.abs(g.box.r - (s.w - R)) > 1) why.push(`${sc.box} ${fmt(g.box)}, want the safe area ${L}..${s.w - R}`);
            }
          }
          // One 44px bar (and the Peers tabs right under it, no taller than a tap target).
          if (g.bar && g.bar.h > 46) why.push(`bar ${r1(g.bar.h)} px tall, want one 44px row`);
          if (g.tabs && (g.tabs.h > 46 || g.tabs.t - g.bar.b > 1)) why.push(`tabs ${r1(g.tabs.h)} px tall, ${r1(g.tabs.t - g.bar.b)} px under the bar`);
          if (g.strip) {
            if (g.strip.l < L - 0.5 || g.strip.l > Math.max(16, L) + 0.5) why.push(`offline strip text at ${r1(g.strip.l)}, want ${Math.max(16, L)}: padding stacked on the inset`);
            if (g.strip.h > 46) why.push(`offline strip ${r1(g.strip.h)} px tall`);
          }
          if (sc.sheet) {
            if (!g.sheetVisible) why.push(`${sc.sheet} did not open`);
            else {
              const sh = g.sheet;
              if (sh.t < -0.5 || sh.b > g.vh + 0.5 || sh.l < -0.5 || sh.r > g.vw + 0.5) why.push(`${sc.sheet} outside the viewport: ${r1(sh.l)},${r1(sh.t)}..${r1(sh.r)},${r1(sh.b)} of ${g.vw}x${g.vh}`);
              if (g.overflows && !g.scrolls) why.push(`${sc.scroller} is taller than its box and does not scroll`);
              if (g.scrollerBottom > g.vh + 0.5) why.push(`${sc.scroller} runs below the screen (bottom ${r1(g.scrollerBottom)})`);
              if (!sc.panel && !sc.full && (sh.l < L - 0.5 || sh.r > s.w - R + 0.5)) why.push(`${sc.sheet} under the notch: ${fmt(sh)}`);
              if (sc.panel) {
                if (sh.t > 47) why.push(`side panel starts at ${r1(sh.t)}: it should take the height under the 44px header`);
                if (g.first.l < L - 0.5) why.push(`first side-panel tab under the notch at ${r1(g.first.l)}`);
                if (g.panelBody.l < L - 0.5 || g.panelBody.r > s.w - R + 0.5) why.push(`side panel body under the notch: ${fmt(g.panelBody)}`);
              }
            }
          }
        }
        if (errs.length) why.push("page errors: " + errs.join("; "));
        if (why.length) fail(tag, why.join("\n     ")); else ok(tag);
      } catch (e) {
        fail(tag, String(e.message).split("\n")[0]);
      } finally {
        await ctx.close().catch(() => {});
      }
    }
  }
  if (process.env.LANDSCAPE_RECORD) console.log("BASE " + JSON.stringify(record, null, 1));
  await fetch(base + `/api/todos/${td.id}/drop`, { method: "POST", headers: J, body: "{}" }).catch(() => {});
  if (pgid) await fetch(base + "/api/pages/" + pgid, { method: "DELETE" }).catch(() => {});
} finally {
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`\n${results.length - bad}/${results.length} landscape checks passed`);
process.exit(bad ? 1 : 0);
