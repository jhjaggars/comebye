// The phone's agent chat with floating chrome (#142 slice 1, design D; index.html "Floating chrome"): below 1100px
// the header is two glass capsules over the conversation, which scrolls under them. At 390x844 and 844x390 (an
// iPhone's insets emulated), light and dark:
//   - back, the name, the side panel and ⋮ are each at least 44x44, none overlaps another, and each is what a tap at
//     its middle hits;
//   - each works: back goes to the list, the name and ⋮ open the agent menu, the side panel button opens the panel;
//   - back and the name each take their own press layer;
//   - the conversation runs to the top of the screen, a message scrolled up shows between and under the capsules, and
//     at rest the first message starts under them (and their chips);
//   - the blocked reason, the progress note and the offline strip show as chips under the left capsule (beside it in
//     landscape, in the one row), the note waits while the composer has the keyboard, and the busy bar runs along the
//     left capsule's foot;
// and at 1280 the header is the desktop's as before: sticky, no glass, the wrappers laid out as if absent.
//
//   node floatchrome.mjs BASE OUT_DIR
import { launch } from "./lib.mjs";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
let failed = 0;
async function check(name, fn) {
  try { await fn(); console.log("ok   " + name); }
  catch (e) { failed++; console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const IPHONE = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1";
const CONTROLS = ["back", "d-name", "sp-btn", "d-more"];

// An agent whose chat shows (the fixtures have several; some show the terminal).
let chatId = null;
async function openChat(page) {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#agents .agent[data-id]");
  const ids = await page.locator("#agents .agent[data-id]").evaluateAll((es) => es.slice(0, 8).map((e) => e.dataset.id));
  if (chatId) ids.unshift(chatId);
  for (const id of ids) {
    await page.goto("about:blank");
    await page.goto(base + "/#/agent/" + id.split("/").map(encodeURIComponent).join("/"), { waitUntil: "load" });
    try { await page.waitForSelector("#detail #chat .msg", { state: "visible", timeout: 2000 }); } catch { continue; }
    await page.waitForTimeout(500);
    if (await page.locator("#detail #chat").isVisible()) { chatId = id; return; }
  }
  throw new Error("no agent among " + ids.join(", ") + " shows a chat");
}
const rect = (page, q) => page.evaluate((q) => { const e = document.querySelector(q); if (!e || !e.getClientRects().length) return null; const r = e.getBoundingClientRect(); return { l: r.left, t: r.top, r: r.right, b: r.bottom, w: r.width, h: r.height }; }, q);
const overlap = (a, b) => Math.min(a.r, b.r) - Math.max(a.l, b.l) > 0.5 && Math.min(a.b, b.b) - Math.max(a.t, b.t) > 0.5;

for (const [width, height] of [[390, 844], [844, 390]]) {
  const land = width > height;
  for (const theme of ["light", "dark"]) {
    const tag = `${width}x${height} ${theme}`;
    const { browser: owner } = await launch({ dark: theme === "dark" });
    const ctx = await owner.newContext({ viewport: { width, height }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, colorScheme: theme, userAgent: IPHONE, bypassCSP: true });
    const page = await ctx.newPage();
    const errs = [];
    page.on("pageerror", (e) => errs.push(String(e)));
    page.setDefaultTimeout(8000);
    await page.addInitScript((t) => { try { localStorage.setItem("comebye.theme", t); } catch {} }, theme);
    const cdp = await ctx.newCDPSession(page);
    await cdp.send("Emulation.setSafeAreaInsetsOverride", { insets: land ? { top: 0, bottom: 21, left: 47, right: 47 } : { top: 47, bottom: 34, left: 0, right: 0 } });
    try {
      await check(`${tag}: back, the name, the side panel and ⋮ are each 44x44 or more, apart, and what a tap at their middle hits`, async () => {
        await openChat(page);
        await page.waitForSelector("#sp-btn:not([hidden])");
        const why = [], boxes = {};
        for (const id of CONTROLS) {
          const b = await rect(page, "#" + id);
          if (!b) { why.push(id + " is not shown"); continue; }
          boxes[id] = b;
          if (b.w < 44 || b.h < 44) why.push(`${id} is ${b.w}x${b.h}`);
          const hit = await page.evaluate(([id, x, y]) => { const e = document.elementFromPoint(x, y); return !!e && !!e.closest("#" + id); }, [id, (b.l + b.r) / 2, (b.t + b.b) / 2]);
          if (!hit) why.push(`a tap at the middle of ${id} lands elsewhere`);
        }
        const ids = Object.keys(boxes);
        for (let i = 0; i < ids.length; i++) for (let j = i + 1; j < ids.length; j++) if (overlap(boxes[ids[i]], boxes[ids[j]])) why.push(`${ids[i]} and ${ids[j]} overlap`);
        const role = await page.evaluate(() => [document.getElementById("d-name").getAttribute("role"), document.getElementById("d-name").tabIndex]);
        if (role[0] !== "button" || role[1] !== 0) why.push("the name is not a button: " + JSON.stringify(role));
        const st = land ? 0 : 47;
        if (boxes.back && boxes.back.t < Math.max(12, st)) why.push(`the capsules start at ${boxes.back.t}, inside the status bar (${st})`);
        if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) why.push("the page scrolls sideways");
        if (why.length) throw new Error(why.join("\n"));
        await page.screenshot({ path: `${out}/floatchrome-${width}x${height}-${theme}.png` });
      });

      await check(`${tag}: back and the name each take their own press layer`, async () => {
        for (const [id, other] of [["back", "d-name"], ["d-name", "back"]]) {
          const b = await rect(page, "#" + id);
          await page.mouse.move((b.l + b.r) / 2, (b.t + b.b) / 2);
          await page.mouse.down();
          const s = await page.evaluate(([a, o]) => [document.getElementById(a).classList.contains("is-pressed"), document.getElementById(o).classList.contains("is-pressed")], [id, other]);
          await page.mouse.move(2, height - 2); // off it before letting go: no click
          await page.mouse.up();
          if (!s[0] || s[1]) throw new Error(`pressing ${id}: ${id} pressed ${s[0]}, ${other} pressed ${s[1]}`);
          await page.waitForTimeout(350);
        }
      });

      await check(`${tag}: the name and ⋮ open the agent menu, the side panel button opens the panel, back goes to the list`, async () => {
        for (const id of ["d-name", "d-more"]) {
          await page.locator("#" + id).click();
          await page.waitForSelector("#d-menu:not([hidden])", { timeout: 3000 });
          await page.keyboard.press("Escape");
          await page.waitForSelector("#d-menu", { state: "hidden", timeout: 3000 });
          await page.waitForTimeout(250);
        }
        await page.locator("#d-name").focus();
        await page.keyboard.press("Enter");
        await page.waitForSelector("#d-menu:not([hidden])", { timeout: 3000 });
        await page.keyboard.press("Escape");
        await page.waitForSelector("#d-menu", { state: "hidden", timeout: 3000 });
        await page.locator("#sp-btn").click();
        await page.waitForFunction(() => document.getElementById("detail").classList.contains("sp-open"), null, { timeout: 3000 });
        if (land) { // the sheet takes the height under the capsules, so they stay in reach
          const sp = await rect(page, "#sp"), cap = await rect(page, "#detail > header .d-cap-r");
          if (sp.t < cap.b - 0.5) throw new Error(`the panel's sheet (top ${sp.t}) covers the capsules (bottom ${cap.b})`);
          await page.locator("#sp-btn").click();
        } else await page.locator("#sp .sp-x").click();
        await page.waitForFunction(() => !document.getElementById("detail").classList.contains("sp-open"), null, { timeout: 3000 });
        // Back: opened from the list, as a tap on a row does, it goes back to the list.
        await page.goto(base + "/", { waitUntil: "load" });
        await page.waitForSelector("#agents .agent[data-id]");
        await page.evaluate((id) => openDetail(id), chatId);
        await page.waitForSelector("#detail #chat", { state: "visible" });
        await page.waitForTimeout(400);
        await page.locator("#back").click();
        await page.waitForFunction(() => getComputedStyle(document.getElementById("detail")).display === "none" && getComputedStyle(document.getElementById("list")).display !== "none", null, { timeout: 4000 });
      });

      await check(`${tag}: the conversation runs under the capsules, and at rest starts below them`, async () => {
        await openChat(page);
        await page.evaluate(() => {
          const c = document.getElementById("chat");
          c.replaceChildren();
          for (let i = 0; i < 40; i++) { const m = document.createElement("div"); m.className = "msg assistant"; m.dataset.i = i; m.innerHTML = '<div class="bubble">probe message ' + i + " with enough words to fill a whole line across the column</div>"; c.append(m); }
          c.scrollTop = 0;
        });
        await page.waitForTimeout(300);
        const g = await page.evaluate(() => {
          const c = document.getElementById("chat").getBoundingClientRect(), m = document.querySelector("#chat .msg").getBoundingClientRect();
          const caps = [...document.querySelectorAll("#detail > header .d-cap")].map((e) => e.getBoundingClientRect().bottom);
          return { top: c.top, first: m.top, caps: Math.max(...caps) };
        });
        if (g.top > 0.5) throw new Error("the conversation starts at " + g.top + ", not at the top of the screen");
        if (g.first < g.caps) throw new Error(`at rest the first message (${g.first}) is under the capsules (${g.caps})`);
        await page.evaluate(() => { document.getElementById("chat").scrollTop = 300; });
        await page.waitForTimeout(250);
        // Between the two capsules, at their middle: a message, seen through (it scrolled under them).
        const under = await page.evaluate(() => {
          const l = document.querySelector("#detail > header .d-cap-l").getBoundingClientRect(), r = document.querySelector("#detail > header .d-cap-r").getBoundingClientRect();
          const y = (l.top + l.bottom) / 2, x = (l.right + r.left) / 2;
          const msgs = [...document.querySelectorAll("#chat .msg")].filter((m) => { const b = m.getBoundingClientRect(); return b.top < y && b.bottom > y; });
          const s = document.createElement("style"); s.textContent = "#detail > header::before { pointer-events: auto !important; }"; // what lies over it there: only the band
          document.head.append(s); const e = document.elementFromPoint(x, y); s.remove();
          return { msgs: msgs.length, over: e ? (e.closest("#detail > header") ? "header" : e.closest("#chat") ? "chat" : e.tagName) : null };
        });
        if (!under.msgs || under.over !== "header") throw new Error("no message under the capsule row, or something but the header over it: " + JSON.stringify(under));
        await page.screenshot({ path: `${out}/floatchrome-scrolled-${width}x${height}-${theme}.png` });
      });

      await check(`${tag}: blocked, note and offline show as chips by the left capsule; the note waits while typing; the busy bar shows`, async () => {
        // Offline first: no poll can then answer and redraw the agent as the hub has it (not blocked).
        await ctx.setOffline(true);
        await page.waitForFunction(() => { const h = document.getElementById("hub-strip"); return h && !h.hidden && h.dataset.kind === "offline"; }, null, { timeout: 8000 });
        // Blocked with a note, as the hub would send it. Set again before each read: an answer already on its way when the
        // link was cut can still redraw the agent as the hub has it (not blocked).
        const block = () => page.evaluate(() => {
          const a = agentById(state.selected);
          a.agent_status = "blocked"; a.blocked_on = { label: "Allow Bash: rm -rf build?" };
          ledger.on = true;
          ledger.byAgent.set(agentId(a), { id: "t1", status: "running", progress: { text: "running the browser checks", at: new Date().toISOString() } });
          renderDetail();
        });
        await block();
        await page.waitForTimeout(300);
        await block();
        const why = [];
        const cap = await rect(page, "#detail > header .d-cap-l"), capR = await rect(page, "#detail > header .d-cap-r");
        const g = await page.evaluate(() => ({ strip: document.getElementById("hub-strip").parentElement.id, border: getComputedStyle(document.querySelector("#detail > header .d-cap-l"), "::before").borderTopColor,
          orange: (() => { const i = document.createElement("i"); i.style.color = "var(--orange)"; document.body.append(i); const c = getComputedStyle(i).color; i.remove(); return c; })() }));
        if (g.strip !== "d-chips") why.push("the offline strip is not one of the chips: in #" + g.strip);
        if (g.border !== g.orange) why.push(`a blocked agent's capsule is not edged orange: ${g.border}`);
        for (const id of ["d-why", "d-note", "hub-strip"]) {
          const b = await rect(page, "#" + id);
          if (!b) { why.push(id + " is not shown"); continue; }
          if (land ? b.l < cap.r || b.r > capR.l || b.t < cap.t - 0.5 : b.t < cap.b || b.l < cap.l - 0.5) why.push(`${id} is not ${land ? "beside" : "under"} the left capsule: ${JSON.stringify(b)} vs ${JSON.stringify(cap)}`);
          if (b.r > width + 0.5) why.push(`${id} runs off the screen: ${b.r}`);
          for (const c of CONTROLS) { const cb = await rect(page, "#" + c); if (cb && overlap(b, cb)) why.push(`${id} overlaps ${c}`); }
        }
        // At rest the first message starts under the chips too.
        await page.evaluate(() => { document.getElementById("chat").scrollTop = 0; });
        await page.waitForTimeout(250);
        const f = await page.evaluate(() => ({ first: document.querySelector("#chat .msg").getBoundingClientRect().top, chips: Math.max(...[...document.querySelectorAll("#d-chips > *")].filter((e) => e.getClientRects().length).map((e) => e.getBoundingClientRect().bottom)) }));
        if (f.first < f.chips) why.push(`at rest the first message (${f.first}) is under the chips (${f.chips})`);
        await page.screenshot({ path: `${out}/floatchrome-chips-${width}x${height}-${theme}.png` });
        await page.locator("#prompt-text").focus();
        await page.waitForTimeout(200);
        await block();
        if (await rect(page, "#d-note")) why.push("the note still shows while typing");
        if (!(await rect(page, "#d-why"))) why.push("the blocked reason went while typing");
        await page.screenshot({ path: `${out}/floatchrome-typing-${width}x${height}-${theme}.png` });
        await page.evaluate(() => document.activeElement.blur());
        await ctx.setOffline(false);
        // Busy: the sweep along the left capsule's foot.
        const busy = await page.evaluate(() => {
          const h = document.querySelector("#detail > header"); h.classList.add("busy");
          const a = getComputedStyle(document.querySelector("#detail > header .d-cap-l"), "::after");
          const r = { content: a.content, h: a.height, img: a.backgroundImage, bottom: a.bottom };
          return r;
        });
        if (busy.content === "none" || !busy.img.includes("linear-gradient") || parseFloat(busy.h) < 2) why.push("no busy bar on the left capsule: " + JSON.stringify(busy));
        if (why.length) throw new Error(why.join("\n"));
      });
      if (errs.length) throw new Error("page errors: " + errs.join(" | "));
    } catch (e) { failed++; console.log("FAIL " + tag + "\n     " + e.message); }
    finally { await owner.close(); }
  }
}

await check("1280: the desktop's header is as before (sticky, one bar, no capsules, the name plain text)", async () => {
  const { browser, ctx: c0 } = await launch({ width: 1280, height: 800, dark: true });
  await c0.close();
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, bypassCSP: true });
  const page = await ctx.newPage();
  try {
    await openChat(page);
    const g = await page.evaluate(() => {
      const h = document.querySelector("#detail > header"), cs = getComputedStyle(h);
      return { pos: cs.position, band: getComputedStyle(h, "::before").content, role: document.getElementById("d-name").getAttribute("role"),
        wraps: [...document.querySelectorAll("#detail > header :is(.d-cap, .d-name, .d-nm, .d-chips)")].map((e) => getComputedStyle(e).display),
        glass: getComputedStyle(document.querySelector("#detail > header .d-cap-l"), "::before").content, top: getComputedStyle(document.getElementById("chat")).paddingTop,
        title: document.getElementById("d-title").getBoundingClientRect().left > document.getElementById("detail").getBoundingClientRect().left };
    });
    if (g.pos !== "sticky" || g.band !== "none" || g.role || g.glass !== "none" || g.wraps.some((d) => d !== "contents") || !g.title) throw new Error(JSON.stringify(g));
    await page.screenshot({ path: `${out}/floatchrome-1280.png` });
  } finally { await browser.close(); }
});

console.log(failed ? `floatchrome: ${failed} failed` : "floatchrome: all ok");
process.exit(failed ? 1 : 0);
