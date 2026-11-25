// The agent header below 1100px is slim: back, the status as the list's dot with the context used as a ring around
// it, the name over host · path, the side panel and the menu. The model, the context chip, the terminal switch, the
// status word and refresh live in the menu instead. Landscape keeps its one 44px row; desktop (1280) is as before.
//
//   node slimheader.mjs BASE OUT_DIR       (env: SCHEME)
// BASE is the hub on the fake herdr -context (see ctxchip.mjs): ctx-ok 62% (Sonnet 4.6), ctx-warn 83% and
// ctx-danger 96% (Opus 5.5), ctx-nowindow (tokens only), ctx-none (no usage), ctx-live (30%, 40% after a prompt).
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const tag = dark ? "dark" : "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const PANES = { ok: "w31:p1", warn: "w32:p1", danger: "w33:p1", nowindow: "w34:p1", none: "w36:p1", live: "w38:p1" };
const IPHONE = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1";
const SIZES = [
  { name: "portrait", w: 390, h: 844, phone: true },
  { name: "landscape", w: 844, h: 390, phone: true },
  { name: "desktop", w: 1280, h: 800, phone: false },
];
// The header's controls, by id: what a phone keeps, and what moves into the menu.
const KEPT = ["back", "d-dot", "sp-btn", "d-more"];
const MOVED = ["d-modelchip", "d-ctx", "btn-terminal", "d-status", "d-refresh"];

const { browser, ctx: first } = await launch({ width: 390, height: 844, dark });
await first.close().catch(() => {}); // each size gets its own context below (landscape needs touch at 844px)
try {
  for (const s of SIZES) {
    const tg = `${s.name} ${s.w}x${s.h} ${tag}`;
    const ctx = await browser.newContext({
      viewport: { width: s.w, height: s.h }, deviceScaleFactor: s.phone ? 2 : 1, isMobile: s.phone, hasTouch: s.phone, bypassCSP: true,
      colorScheme: dark ? "dark" : "light", ...(s.phone ? { userAgent: IPHONE } : {}),
    });
    const page = await ctx.newPage();
    const errs = [];
    page.on("pageerror", (e) => errs.push(String(e)));
    const open = async (pane) => {
      await page.goto(base + "/#/agent/" + encodeURIComponent(pane), { waitUntil: "load" });
      await page.waitForSelector("#detail", { state: "visible", timeout: 15000 });
      await page.waitForFunction(() => document.querySelector("#chat")?.children.length > 0, null, { timeout: 8000 });
      await page.waitForTimeout(400);
    };
    const shown = (id) => page.evaluate((i) => { const e = document.getElementById(i); return !!e && e.getClientRects().length > 0 && getComputedStyle(e).visibility !== "hidden"; }, id);
    const box = (sel) => page.evaluate((q) => { const r = document.querySelector(q).getBoundingClientRect(); return { l: r.left, r: r.right, t: r.top, b: r.bottom, w: r.width, h: r.height }; }, sel);
    const ring = () => page.evaluate(() => {
      const d = document.getElementById("d-dot"), f = document.getElementById("d-ring-fill"), dot = document.getElementById("d-dot-in");
      const ringShown = d.querySelector(".d-ring").getClientRects().length > 0;
      const rgb = (c) => { const i = document.createElement("i"); i.style.color = c; document.body.appendChild(i); const x = getComputedStyle(i).color; i.remove(); return x; };
      const root = getComputedStyle(document.documentElement);
      return { ringShown, pct: d.dataset.ctx, dash: f.getAttribute("stroke-dasharray"), stroke: getComputedStyle(f).stroke,
        label: d.getAttribute("aria-label"), role: d.getAttribute("role"), dotCls: dot.className, dotBg: getComputedStyle(dot).backgroundColor,
        tones: { muted: rgb(root.getPropertyValue("--muted")), yellow: rgb(root.getPropertyValue("--yellow")), red: rgb(root.getPropertyValue("--red")) } };
    });
    const menuOpen = async () => {
      await page.locator("#d-more").click();
      await page.waitForSelector("#d-menu:not([hidden])", { timeout: 3000 });
      await page.waitForTimeout(350); // the sheet slides in
    };
    const menuClosed = () => page.waitForSelector("#d-menu", { state: "hidden", timeout: 3000 });
    const toast = (re) => page.waitForFunction((src) => new RegExp(src).test(document.querySelector("#toast")?.textContent || ""), re.source, { timeout: 4000 });

    try {
      if (s.phone) {
        await check(`${tg}: the header has only back, the status dot, the name, the side panel and the menu`, async () => {
          await open(PANES.warn);
          const why = [];
          for (const id of KEPT) if (!(await shown(id))) why.push(id + " is not shown");
          for (const id of MOVED) if (await shown(id)) why.push(id + " is still in the header");
          const extra = await page.evaluate((known) => [...document.querySelectorAll("#detail > header > *")]
            .filter((e) => e.getClientRects().length && !e.classList.contains("meta") && !known.includes(e.id)).map((e) => e.id || e.className), KEPT);
          if (extra.length) why.push("other things in the header: " + extra.join(", "));
          for (const id of ["back", "sp-btn", "d-more"]) { const b = await box("#" + id); if (b.w < 44 || b.h < 44) why.push(`${id} is ${b.w}x${b.h}, under 44px`); }
          const hd = await box("#detail > header");
          if (s.name === "landscape" && hd.h > 46) why.push(`landscape header ${hd.h}px, want one 44px row`);
          if (s.name === "portrait" && hd.h > 76) why.push(`portrait header ${hd.h}px: a second line is back`);
          const sub = await page.evaluate(() => [...document.querySelectorAll("#detail > header .meta > *")].filter((e) => e.getClientRects().length).map((e) => e.id));
          if (sub.join() !== "d-title,d-subtitle") why.push("the name's lines: " + sub.join());
          if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) why.push("the page scrolls sideways");
          if (why.length) throw new Error(why.join("\n"));
          await page.screenshot({ path: `${out}/slim-header-${s.name}-${tag}.png`, clip: { x: 0, y: 0, width: s.w, height: Math.min(s.h, 120) } });
        });

        await check(`${tg}: the name gets the freed width`, async () => {
          await open(PANES.warn);
          const meta = await box("#detail > header .meta"), dot = await box("#d-dot"), sp = await box("#sp-btn");
          // Between the dot and the side panel button only the gaps are left: the meta takes the rest.
          if (sp.l - meta.r > 24 || meta.l - dot.r > 16) throw new Error(`meta ${JSON.stringify(meta)} between dot ${dot.r} and panel ${sp.l}`);
          const t = await page.evaluate(() => { const e = document.getElementById("d-title"), u = document.getElementById("d-subtitle"); return { tc: e.scrollWidth > e.clientWidth, uc: u.scrollWidth > u.clientWidth, t: e.textContent, u: u.textContent }; });
          if (t.tc) throw new Error(`the name "${t.t}" is cut`);
          if (s.name === "portrait" && meta.w < 180) throw new Error(`the name has only ${meta.w}px`);
          if (t.uc && s.name === "landscape") throw new Error(`host · path "${t.u}" is cut at ${s.w}px`);
        });

        await check(`${tg}: the ring reflects the context used, toned like the chip; none without a percentage`, async () => {
          for (const [k, pct, tone] of [["ok", "62", "muted"], ["warn", "83", "yellow"], ["danger", "96", "red"]]) {
            await open(PANES[k]);
            await page.waitForFunction(() => document.getElementById("d-dot").dataset.ctx, null, { timeout: 8000 });
            const r = await ring();
            if (!r.ringShown || r.pct !== pct || r.dash !== `${pct} 100`) throw new Error(`${k}: ring ${JSON.stringify(r)}`);
            if (r.stroke !== r.tones[tone]) throw new Error(`${k}: ring ${r.stroke}, wanted ${tone} ${r.tones[tone]}`);
            await page.screenshot({ path: `${out}/slim-ring-${k}-${s.name}-${tag}.png`, clip: { x: 0, y: 0, width: Math.min(s.w, 390), height: 80 } });
          }
          for (const k of ["nowindow", "none"]) {
            await open(PANES[k]);
            await page.waitForTimeout(500);
            const r = await ring();
            if (r.ringShown || r.pct) throw new Error(`${k}: a ring with no percentage ${JSON.stringify(r)}`);
          }
        });

        await check(`${tg}: the ring follows the agent's replies (30% to 40%)`, async () => {
          if (s.name !== "portrait") return; // one viewport talks to the live agent: a reply moves it for good
          await open(PANES.live);
          await page.waitForFunction(() => document.getElementById("d-dot").dataset.ctx === "30", null, { timeout: 8000 });
          await page.evaluate(async () => { await fetch(`${pathFor(state.selected)}/prompt`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ text: "again?" }) }); });
          await page.waitForFunction(() => document.getElementById("d-dot").dataset.ctx === "40" && document.getElementById("d-ring-fill").getAttribute("stroke-dasharray") === "40 100", null, { timeout: 20000 });
        });

        await check(`${tg}: the dot is the list's dot for each status, and its aria-label says the status`, async () => {
          await open(PANES.ok);
          let r = await ring();
          if (r.role !== "img" || r.label !== "Status: idle; context 62% used") throw new Error("label " + JSON.stringify(r.label));
          for (const st of ["working", "blocked", "done", "idle"]) {
            r = await page.evaluate((v) => { const a = agentById(state.selected); a.agent_status = v; renderDetail(); return null; }, st);
            r = await ring();
            const listBg = await page.evaluate((v) => { const i = document.createElement("span"); i.className = "dot " + v; document.getElementById("agents").appendChild(i); const c = getComputedStyle(i).backgroundColor; i.remove(); return c; }, st);
            if (r.dotCls !== "dot " + st) throw new Error(`${st}: dot class ${r.dotCls}`);
            if (!r.label.startsWith("Status: " + st)) throw new Error(`${st}: label ${r.label}`);
            if (st !== "working" && r.dotBg !== listBg) throw new Error(`${st}: dot ${r.dotBg}, the list's ${listBg}`); // working pulses
            if (st === "blocked") await page.screenshot({ path: `${out}/slim-blocked-${s.name}-${tag}.png`, clip: { x: 0, y: 0, width: Math.min(s.w, 390), height: 80 } });
          }
        });

        await check(`${tg}: the blocked reason and the progress note still show under the name`, async () => {
          await open(PANES.ok);
          const g = await page.evaluate(() => {
            const a = agentById(state.selected);
            a.agent_status = "blocked"; a.blocked_on = { label: "Allow Bash: rm -rf build?" };
            renderDetail();
            const why = document.getElementById("d-why");
            const w = { hidden: why.hidden || !why.getClientRects().length, text: why.textContent };
            a.agent_status = "working"; a.blocked_on = null;
            ledger.on = true;
            ledger.byAgent.set(agentId(a), { id: "t1", status: "running", progress: { text: "running the browser checks", at: new Date().toISOString() } });
            renderDetail();
            const n = document.getElementById("d-note");
            return { why: w, note: { hidden: n.hidden || !n.getClientRects().length, text: n.textContent } };
          });
          if (g.why.hidden || !/Allow Bash/.test(g.why.text)) throw new Error("blocked reason " + JSON.stringify(g.why));
          if (g.note.hidden || !/browser checks/.test(g.note.text)) throw new Error("note " + JSON.stringify(g.note));
          await page.screenshot({ path: `${out}/slim-note-${s.name}-${tag}.png`, clip: { x: 0, y: 0, width: Math.min(s.w, 390), height: 120 } });
        });

        await check(`${tg}: the menu has the model, Terminal, Refresh and Context, each 44px and each works`, async () => {
          await open(PANES.warn);
          await menuOpen();
          const items = await page.evaluate(() => [...document.querySelectorAll("#d-menu > button")].filter((b) => b.getClientRects().length)
            .map((b) => ({ id: b.id, text: b.textContent.trim(), h: b.getBoundingClientRect().height })));
          const want = [["d-mx-model", "Opus 5.5"], ["d-mx-term", "Show raw terminal"], ["d-mx-refresh", "Refresh"], ["d-mx-ctx", "Context 83% used"]];
          const top = items.slice(0, 4).map((i) => [i.id, i.text]);
          if (JSON.stringify(top) !== JSON.stringify(want)) throw new Error("menu starts " + JSON.stringify(top));
          for (const i of items) if (i.h < 44) throw new Error(`${i.id} is ${i.h}px tall`);
          // Four more items: on a short screen the menu scrolls, and its last item can still be reached.
          const last = await page.evaluate(() => {
            const b = [...document.querySelectorAll("#d-menu > button")].filter((x) => x.getClientRects().length).pop();
            b.scrollIntoView({ block: "end" });
            const r = b.getBoundingClientRect(), m = document.getElementById("d-menu").getBoundingClientRect();
            return { id: b.id, b: r.bottom, mb: m.bottom, vh: innerHeight };
          });
          if (last.b > last.mb + 1 || last.b > last.vh + 1) throw new Error("the menu's last item is out of reach: " + JSON.stringify(last));
          await page.evaluate(() => { document.getElementById("d-menu").scrollTop = 0; });
          await page.screenshot({ path: `${out}/slim-menu-${s.name}-${tag}.png` });
          // Terminal: the raw view, and the item then offers the chat back.
          await page.locator("#d-mx-term").click();
          await menuClosed();
          await page.waitForFunction(() => state.chat && state.chat.terminal && getComputedStyle(document.getElementById("output")).display !== "none", null, { timeout: 8000 });
          await menuOpen();
          if ((await page.textContent("#d-mx-term")).trim() !== "Show chat") throw new Error("the item does not offer the chat back");
          await page.locator("#d-mx-term").click();
          await menuClosed();
          await page.waitForFunction(() => state.chat && !state.chat.terminal && document.getElementById("chat").style.display !== "none", null, { timeout: 8000 });
          // Refresh: the view asks the hub again.
          await menuOpen();
          const asked = page.waitForRequest((r) => r.url().includes("/api/agents"), { timeout: 5000 });
          await page.locator("#d-mx-refresh").click();
          await menuClosed();
          await asked;
          // Context and the model: the exact numbers and the full model.
          await menuOpen();
          await page.locator("#d-mx-ctx").click();
          await menuClosed();
          await toast(/tokens \(83%\)/);
          await menuOpen();
          await page.locator("#d-mx-model").click();
          await menuClosed();
          await toast(/claude-opus-5-5/);
        });
      } else {
        await check(`${tg}: desktop is unchanged: the chips, terminal, status word and refresh in the header, no dot, the menu as before`, async () => {
          await open(PANES.warn);
          const why = [];
          for (const id of ["sp-btn", "d-more", ...MOVED]) if (!(await shown(id))) why.push(id + " is not shown");
          if (await shown("d-dot")) why.push("the slim header's dot shows on desktop");
          const st = await page.evaluate(() => ({ text: document.getElementById("d-status").textContent, ctx: document.getElementById("d-ctx").innerText.replace(/\s+/g, " ").trim(), m: document.getElementById("d-modelchip").textContent }));
          if (st.text !== "idle" || st.ctx !== "ctx 83% · 830k" || st.m !== "Opus 5.5") why.push("header " + JSON.stringify(st));
          const hd = await box("#detail > header");
          if (Math.round(hd.h) !== 72) why.push(`header ${hd.h}px, was 72`);
          await menuOpen();
          const mx = await page.evaluate(() => [...document.querySelectorAll("#d-menu .d-mx")].filter((b) => b.getClientRects().length).map((b) => b.id));
          if (mx.length) why.push("the phone's menu items show on desktop: " + mx.join(", "));
          await page.keyboard.press("Escape");
          if (why.length) throw new Error(why.join("\n"));
          await page.screenshot({ path: `${out}/slim-header-desktop-${tag}.png`, clip: { x: 0, y: 0, width: s.w, height: 80 } });
        });
      }
      if (errs.length) await check(`${tg}: no page errors`, async () => { throw new Error(errs.join("; ")); });
    } finally {
      await ctx.close().catch(() => {});
    }
  }
} finally {
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`slimheader: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
