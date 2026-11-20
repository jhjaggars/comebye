// Settings > Appearance, one table-driven script (it replaces fontsetting, readcol, textsize, spacing, wrap,
// openbydefault and appearancereset). Each row of SETTINGS is one device-local choice (web/themes.js: comebye.<key>
// -> <html data-<key>>) and the same behaviour is checked for all of them, in the run's colour scheme:
//
//   - a choice moves the computed style of a mocked conversation in the right DIRECTION (wider, larger, roomier,
//     wrapped, open, serif), never to an exact pixel value;
//   - choosing it in Settings by click stores it, and it is still applied after a reload;
//   - the arrow keys move within the radio group; the Settings preview follows;
//   - an unknown stored value shows the default; the value is on <html> by DOMContentLoaded; another tab's change
//     reaches an open chat by the storage event; the split view (#sv-chat) follows;
//   - nothing scrolls sideways on a phone with the largest choice;
//   - Reset to defaults restores every layout choice at once (no reload, in another tab too), and leaves the theme
//     and the font alone.
// Then the few things that belong to one setting: Copy gives the exact text with wrap on, the font migration table,
// code and key bar stay as they are with Serif, the fleet menu's Font entry, the Theme row's panel.
// No count of radio groups, no pixel or line-height values: those broke whenever Settings gained a row.
//
//   node appearance.mjs BASE OUT_DIR      (env: SCHEME)
import { launch } from "./lib.mjs";
import { U, A, T } from "./fixtures/toolruns.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [], log = [];
const same = (got, want, what) => { if (JSON.stringify(got) !== JSON.stringify(want)) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };
const truthy = (v, what) => { if (!v) throw new Error(what); };
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

const SERIF = /^"Iowan Old Style", Charter/;
const MONO = /^ui-monospace/;
const para = "The reading column is as wide as the setting says, and no wider than the window allows, so a long paragraph like this one wraps at the column and not at the edge of a very wide monitor. ";
const LONG = "const token = '" + "abcdefghij".repeat(40) + "';"; // one 400+ character line with no space to break at
const BLOCK = "package main\n\n" + LONG + "\n\tfmt.Println(token)";
const MSGS = [
  U("u1", "Show me the long line, in a wide column."),
  { ...A("a1", "Done. Run `go test ./...` to see it.\n\n```go\n" + BLOCK + "\n```\n\n" + (para.repeat(3) + "\n\n").repeat(2)), think: "Thinking it over.\n\nThe tokens scale." },
  T("c1", "Read", "web/app.js", "// app"),
  T("c2", "Bash", "go test ./...", "ok"),
  { ...A("a2", "A second reply.\n\n" + para.repeat(2)), think: "Another thought." },
  { id: "s1", kind: "tool", tool: "Agent", args: "review the diff", result: "Looks fine.", ms: 9000,
    subagent: { agent_id: "ag1", type: "reviewer", description: "Review the diff", status: "completed", dur_ms: 9000, tool_uses: 3,
      report: "Looks fine.\n\n```\n" + LONG + "\n```\n\n" + para.repeat(2) } },
  A("a3", "All good."),
];

// One row per setting. order: stored values from the least to the most of whatever the setting changes; "" is the
// default. metric(m) is what that change must move, read off the computed style: each number must not go down along
// order (and must go up, when strict), and every non-default choice must differ from the default in at least one.
// radios: what to click, in Settings, to get each value from the defaults. kbd: ArrowRight from the default radio.
const SETTINGS = [
  { key: "col", order: ["", "wide", "wider", "full"], strict: false, metric: (m) => [m.colW],
    radios: { wide: ["sx-col-wide"], wider: ["sx-col-wider"], full: ["sx-col-full"] }, dflt: ["sx-col-default"],
    kbd: { from: "sx-col-default", to: "wide", focus: "sx-col-wide" }, prev: false, split: true },
  { key: "rs", order: ["small", "", "large", "larger"], strict: true, metric: (m) => [m.fs],
    radios: { small: ["sx-rs-small"], large: ["sx-rs-large"], larger: ["sx-rs-larger"] }, dflt: ["sx-rs-default"],
    kbd: { from: "sx-rs-default", to: "large", focus: "sx-rs-large" }, prev: true, split: true },
  { key: "sp", order: ["compact", "", "roomy"], strict: true, metric: (m) => [m.lh, m.gap],
    radios: { compact: ["sx-sp-compact"], roomy: ["sx-sp-roomy"] }, dflt: ["sx-sp-default"],
    kbd: { from: "sx-sp-default", to: "roomy", focus: "sx-sp-roomy" }, prev: true, split: true },
  { key: "wrap", order: ["", "on"], strict: true, metric: (m) => [m.ws === "pre-wrap" ? 1 : 0],
    radios: { on: ["sx-wrap-on"] }, dflt: ["sx-wrap-default"],
    kbd: { from: "sx-wrap-default", to: "on", focus: "sx-wrap-on" }, prev: true, split: true },
  { key: "open", order: ["", "think", "both"], strict: false, metric: (m) => [m.thinkOpen, m.thinkOpen + m.toolOpen],
    radios: { think: ["sx-openthink-on"], both: ["sx-openthink-on", "sx-opentools-on"] }, dflt: ["sx-openthink-default", "sx-opentools-default"],
    kbd: { from: "sx-openthink-default", to: "think", focus: "sx-openthink-on" }, prev: true, split: false },
  { key: "font", order: ["", "serif"], strict: true, metric: (m) => [SERIF.test(m.fam) ? 1 : 0],
    radios: { serif: ["sx-font-serif"] }, dflt: ["sx-font-default"],
    kbd: { from: "sx-font-default", to: "serif", focus: "sx-font-serif" }, prev: false, split: false },
];
const top = (s) => s.order[s.order.length - 1];
const RESET_KEYS = ["col", "rs", "sp", "wrap", "open", "depth"]; // what Reset removes; the theme and the font stay

async function session(width, height) {
  const { browser, ctx, page } = await launch({ width, height, dark });
  await ctx.grantPermissions(["clipboard-read", "clipboard-write"], { origin: new URL(base).origin });
  page.setDefaultTimeout(8000);
  page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: MSGS, total: MSGS.length, more: false, format: "claude" } }));
  await page.addInitScript(() => {
    document.addEventListener("DOMContentLoaded", () => {
      window.__atDCL = Object.fromEntries(["col", "rs", "sp", "wrap", "open", "font"].map((k) => [k, document.documentElement.getAttribute("data-" + k)]));
    });
  });
  return { browser, ctx, page };
}
// Set the stored values (a map key -> value, null removes it) on the app's origin, then leave it.
async function store(page, vals) {
  await page.goto(base + "/#/settings", { waitUntil: "load" });
  await page.evaluate((vals) => { for (const [k, v] of Object.entries(vals)) { if (v === null) localStorage.removeItem("comebye." + k); else localStorage.setItem("comebye." + k, v); } }, vals);
  await page.goto("about:blank");
}
const clearAll = () => Object.fromEntries([...RESET_KEYS, "font"].map((k) => [k, null]));
async function openChat(page, vals = {}) {
  await store(page, { ...clearAll(), ...vals });
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#chat .msg.assistant .cb pre");
  await page.waitForSelector("#chat .thinking .tr-line");
  await page.waitForSelector("#chat .toolrun .tr-line");
}
async function openSettings(page, vals = {}) {
  await store(page, { ...clearAll(), ...vals });
  await page.goto(base + "/#/settings", { waitUntil: "load" });
  await page.waitForSelector("#sx-reset");
}
// What a setting can change, read off the computed style of everything under root (#chat, #sv-chat, .sx-prev).
const measure = (page, root) => page.evaluate((root) => {
  const r = document.querySelector(root), cs = (el) => getComputedStyle(el);
  const reply = r.querySelector(".msg.assistant"), b = r.querySelector(".msg.assistant .bubble"), pre = r.querySelector("pre");
  const fs = parseFloat(cs(b).fontSize);
  const open = (sel) => [...r.querySelectorAll(sel)].filter((l) => l.getAttribute("aria-expanded") === "true").length;
  return {
    colW: reply.getBoundingClientRect().width, fs, lh: parseFloat(cs(b).lineHeight) / fs, gap: parseFloat(cs(r).rowGap) || 0,
    ws: pre ? cs(pre).whiteSpace : null, preScrolls: pre ? pre.scrollWidth > pre.clientWidth : null,
    thinkOpen: open(".thinking .tr-line"), toolOpen: open(".toolrun .tr-line"), fam: cs(b.querySelector("p") || b).fontFamily,
    cw: r.clientWidth, sw: r.scrollWidth, doc: document.documentElement.scrollWidth - window.innerWidth,
  };
}, root);
const attr = (page, k) => page.evaluate((k) => document.documentElement.getAttribute("data-" + k), k);
const stored = (page, k) => page.evaluate((k) => localStorage.getItem("comebye." + k), k);
async function click(page, ids) { for (const id of ids) await page.locator("#" + id).click(); }
async function openSplit(page, waitFor) {
  await page.locator("#chat .subrun .sa-line").click();
  await page.locator("#chat .subrun .sa-steps").click();
  await page.waitForFunction((w) => !document.getElementById("subview").hidden && document.querySelector(w), waitFor);
}
// m[i] must not go down along the list (go up when strict); the first and last must differ.
function ordered(spec, ms, what) {
  const nums = ms.map((m) => spec.metric(m));
  for (let i = 0; i < nums[0].length; i++) {
    for (let j = 1; j < nums.length; j++) {
      const a = nums[j - 1][i], b = nums[j][i];
      if (spec.strict ? !(b > a) : b < a) throw new Error(`${what}: metric ${i} is ${JSON.stringify(nums.map((n) => n[i]))} along ${JSON.stringify(spec.order)}`);
    }
  }
  const d = nums[spec.order.indexOf("")];
  for (let j = 0; j < nums.length; j++) {
    if (spec.order[j] === "") continue;
    if (JSON.stringify(nums[j]) === JSON.stringify(d)) throw new Error(`${what}: ${JSON.stringify(spec.order[j])} looks like the default (${JSON.stringify(d)})`);
  }
}

// ---- 3440: the wide window shows every setting; one pass over the table ----
{
  const { browser, ctx, page } = await session(3440, 1200);
  const dflt = {}; // the default metrics, for Reset
  for (const s of SETTINGS) {
    const vals = (v) => ({ [s.key]: v === "" ? null : v });
    await check(`${s.key}: each choice moves the conversation the right way (${s.order.map((v) => v || "default").join(" < ")})`, async () => {
      const ms = [];
      for (const v of s.order) { await openChat(page, vals(v)); same(await attr(page, s.key), v || null, "data-" + s.key); ms.push(await measure(page, "#chat")); }
      ordered(s, ms, s.key);
      dflt[s.key] = ms[s.order.indexOf("")];
    });
    await check(`${s.key}: chosen in Settings by click it is stored and still applied after a reload`, async () => {
      for (const v of s.order.filter((x) => x !== "")) {
        await openSettings(page);
        await click(page, s.radios[v]);
        same(await stored(page, s.key), v, "stored by click");
        same(await attr(page, s.key), v, "applied at once");
        const last = s.radios[v][s.radios[v].length - 1];
        same(await page.locator("#" + last).getAttribute("aria-checked"), "true", "radio checked");
        await page.reload({ waitUntil: "load" });
        await page.waitForSelector("#sx-reset");
        same(await attr(page, s.key), v, "data-" + s.key + " after reload");
        same(await page.locator("#" + last).getAttribute("aria-checked"), "true", "radio checked after reload");
      }
      await click(page, s.dflt);
      same(await attr(page, s.key), null, "default restores the attribute");
      same(await page.evaluate((k) => localStorage.getItem("comebye." + k) || null, s.key), null, "default stored as default");
    });
    await check(`${s.key}: arrow keys move and select within the radio group, focus stays on the choice`, async () => {
      await openSettings(page);
      await page.locator("#" + s.kbd.from).focus();
      await page.keyboard.press("ArrowRight");
      same(await stored(page, s.key), s.kbd.to, "stored");
      same(await page.evaluate(() => document.activeElement.id), s.kbd.focus, "focus");
    });
    if (s.prev) {
      await check(`${s.key}: the Settings preview follows the choice`, async () => {
        await openSettings(page);
        const before = s.metric(await measure(page, ".sx-prev"));
        await click(page, s.radios[top(s)]);
        const after = s.metric(await measure(page, ".sx-prev"));
        const dir = after.map((v, i) => v - before[i]);
        truthy(dir.some((d) => d > 0) && dir.every((d) => d >= 0), `preview metric ${JSON.stringify(before)} -> ${JSON.stringify(after)} with ${top(s)}`);
        const fits = await page.evaluate(() => { const w = document.querySelector(".sx-prev-wrap").getBoundingClientRect(), v = document.querySelector(".sx-prev").getBoundingClientRect(); return v.right <= w.right + 1; });
        truthy(fits, "the preview overflows its frame");
      });
    }
    await check(`${s.key}: an unknown stored value shows the default`, async () => {
      await openChat(page, { [s.key]: "nonsense" });
      same(await attr(page, s.key), null, "data-" + s.key);
      same(s.metric(await measure(page, "#chat")), s.metric(dflt[s.key]), "metric");
    });
    await check(`${s.key}: the stored value is on <html> by DOMContentLoaded`, async () => {
      await openChat(page, vals(top(s)));
      same(await page.evaluate((k) => window.__atDCL[k], s.key), top(s), "data-" + s.key + " at DOMContentLoaded");
    });
    await check(`${s.key}: another tab's change reaches a chat that is already open`, async () => {
      await openChat(page);
      const before = s.metric(await measure(page, "#chat"));
      const other = await ctx.newPage();
      await other.goto(base + "/#/settings", { waitUntil: "load" });
      await other.evaluate(([k, v]) => localStorage.setItem("comebye." + k, v), [s.key, top(s)]);
      await page.waitForFunction(([k, v]) => document.documentElement.getAttribute("data-" + k) === v, [s.key, top(s)]);
      const after = s.metric(await measure(page, "#chat"));
      truthy(after.some((v, i) => v !== before[i]), `nothing changed in the open chat: ${JSON.stringify(before)}`);
      await other.evaluate((k) => localStorage.setItem("comebye." + k, "nonsense"), s.key);
      await page.waitForFunction((k) => !document.documentElement.hasAttribute("data-" + k), s.key);
      same(s.metric(await measure(page, "#chat")), before, "back to the default on an unknown value");
      await other.close();
    });
    if (s.split) {
      await check(`${s.key}: the split view (#sv-chat) follows the setting`, async () => {
        const wait = s.key === "wrap" ? "#sv-chat .md pre" : "#sv-chat .msg.assistant";
        await openChat(page);
        await openSplit(page, wait);
        const before = s.metric(await measure(page, "#sv-chat"));
        await openChat(page, vals(top(s)));
        await openSplit(page, wait);
        const after = s.metric(await measure(page, "#sv-chat"));
        truthy(after.some((v, i) => v > before[i]) && after.every((v, i) => v >= before[i]), `#sv-chat ${JSON.stringify(before)} -> ${JSON.stringify(after)} with ${top(s)}`);
      });
    }
  }

  await check("wrap: Copy gives the exact original code either way", async () => {
    for (const v of ["on", null]) {
      await openChat(page, { wrap: v });
      await page.evaluate(() => navigator.clipboard.writeText("sentinel"));
      await page.locator("#chat .msg.assistant .cb .cb-copy").first().click();
      await page.waitForFunction(() => document.querySelector("#chat .cb-copy[data-state=ok]"));
      same(await page.evaluate(() => navigator.clipboard.readText()), BLOCK, "copied text, wrap " + (v || "off"));
    }
  });
  await check("wrap: on, the long line does not scroll the code block or the page", async () => {
    await openChat(page, { wrap: "on" });
    const m = await measure(page, "#chat");
    same(m.preScrolls, false, "code block scrolls with wrap on");
    truthy(m.doc <= 0, "page scrolls sideways");
  });
  await check("open: a line closed by hand stays closed through polls, one opened by hand stays open; Up/Down reach the rows", async () => {
    await openChat(page, { open: "both" });
    const think = page.locator("#chat .thinking .tr-line").first(), run = page.locator("#chat .toolrun .tr-line").first();
    await think.click(); await run.click();
    same(await think.getAttribute("aria-expanded"), "false", "thought closed by hand");
    await page.evaluate(() => loadChat(false)); await page.evaluate(() => loadChat(false));
    same(await think.getAttribute("aria-expanded"), "false", "thought still closed after polls");
    same(await run.getAttribute("aria-expanded"), "false", "run still closed after polls");
    await openChat(page);
    await page.locator("#chat .thinking .tr-line").first().click();
    await page.evaluate(() => loadChat(false));
    same(await page.locator("#chat .thinking .tr-line").first().getAttribute("aria-expanded"), "true", "hand-opened thought stays open");
    await openChat(page, { open: "both" });
    await page.evaluate(() => document.querySelector("#chat .toolrun .tr-line").focus());
    await page.keyboard.press("ArrowDown");
    truthy(/tr-row/.test(await page.evaluate(() => document.activeElement.className)), "ArrowDown from an open run did not reach a row");
    await page.keyboard.press("ArrowUp");
    same(await page.evaluate(() => document.activeElement.classList.contains("tr-line")), true, "ArrowUp returns to the line");
  });
  await check("font: Serif is for reading text only: code stays mono, the key bar and header stay as they are", async () => {
    const fam = (sel) => page.evaluate((sel) => { const el = document.querySelector(sel); return el ? getComputedStyle(el).fontFamily : null; }, sel);
    await openChat(page);
    const keybar = await fam(".keybar"), header = await fam("header h1");
    await openChat(page, { font: "serif" });
    truthy(SERIF.test(await fam("#chat .msg.assistant .md p")), "reply is not serif: " + await fam("#chat .msg.assistant .md p"));
    truthy(SERIF.test(await fam("#detail .prompt .composer")), "composer is not serif");
    truthy(MONO.test(await fam("#chat .msg.assistant .md pre code")), "code block is not mono");
    truthy(MONO.test(await fam("#chat .msg.assistant .md p code")), "inline code is not mono");
    same(await fam(".keybar"), keybar, "key bar font");
    same((await page.evaluate(() => performance.getEntriesByType("resource").map((e) => e.name).filter((n) => /\.(woff2?|ttf|otf)/.test(n)))).length, 0, "font files fetched");
  });
  await check("font: every value an older build stored migrates (serif-heads, serif-reading -> serif; geist, unknown -> sans)", async () => {
    for (const [old, want] of [["serif-heads", "serif"], ["serif-reading", "serif"], ["geist", null], ["comic-sans", null], ["SERIF", null], ["serif", "serif"]]) {
      await openSettings(page, { font: old });
      same(await attr(page, "font"), want, `data-font for stored ${JSON.stringify(old)}`);
      same(await page.locator("#sx-font-" + (want || "default")).getAttribute("aria-checked"), "true", "radio for " + old);
      same(await stored(page, "font"), want, "stored value rewritten clean for " + old);
    }
  });
  await check("font: the fleet menu's Font entry toggles Sans and Serif and shows the value", async () => {
    await store(page, clearAll());
    await page.goto(base + "/", { waitUntil: "load" });
    await page.locator("#btn-fleet").click();
    same((await page.locator("#f-font-val").textContent()).trim(), "Sans", "menu value");
    await page.locator("#f-font").click();
    same(await attr(page, "font"), "serif", "toggled to Serif");
    await page.locator("#btn-fleet").click();
    same((await page.locator("#f-font-val").textContent()).trim(), "Serif", "menu value");
    await page.locator("#f-font").click();
    same(await attr(page, "font"), null, "toggled back");
  });

  // Reset: change every layout choice (and the font and the theme), then Reset.
  await check("Reset to defaults: removes every layout choice at once, no reload, leaves theme and font, restores the default conversation, in another tab too", async () => {
    await openSettings(page, { font: "serif", theme: "dracula" });
    await click(page, ["sx-col-wider", "sx-rs-larger", "sx-sp-roomy", "sx-wrap-on", "sx-depth-flat", "sx-openthink-on", "sx-opentools-on"]);
    same(await attr(page, "col"), "wider", "col set"); same(await attr(page, "open"), "both", "open set");
    const p2 = await ctx.newPage();
    await p2.goto(base + "/#/", { waitUntil: "load" });
    same(await p2.evaluate(() => document.documentElement.getAttribute("data-col")), "wider", "tab 2 has the choice");
    await page.evaluate(() => { window.__mark = 1; });
    await page.locator("#sx-reset").click();
    for (const k of RESET_KEYS) { same(await attr(page, k), null, "data-" + k + " removed at once"); same(await stored(page, k), null, "comebye." + k + " removed"); }
    same(await attr(page, "font"), "serif", "font stays");
    same(await stored(page, "theme"), "dracula", "theme stays");
    same(await page.evaluate(() => window.__mark), 1, "no reload");
    for (const id of ["col", "rs", "sp", "wrap", "depth", "openthink", "opentools"]) same(await page.locator(`#sx-${id}-default`).getAttribute("aria-checked"), "true", id + " shows the default");
    same(await page.evaluate(() => document.activeElement.id), "sx-col-default", "focus lands on the first group");
    await p2.waitForFunction(() => !document.documentElement.hasAttribute("data-col") && !document.documentElement.hasAttribute("data-open") && !document.documentElement.hasAttribute("data-rs"));
    same(await p2.evaluate(() => ["data-sp", "data-wrap"].map((a) => document.documentElement.getAttribute(a))), [null, null], "tab 2 sp, wrap");
    await p2.close();
    // The chat is the default one again: every setting's metric is what it was with nothing stored (the font stays Serif).
    await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
    await page.waitForSelector("#chat .thinking .tr-line");
    const m = await measure(page, "#chat");
    for (const s of SETTINGS.filter((x) => x.key !== "font")) same(s.metric(m), s.metric(dflt[s.key]), s.key + " metric after Reset");
    await page.reload({ waitUntil: "load" });
    for (const k of RESET_KEYS) same(await attr(page, k), null, "data-" + k + " after reload");
    await page.screenshot({ path: `${out}/appearance-reset-${dark ? "dark" : "light"}.png` });
  });
  await check("Theme row opens the theme panel (Esc returns to the row, a pick applies and shows); Reset keeps it; Tab reaches Reset and Enter resets", async () => {
    await openSettings(page, { font: "serif" });
    await page.locator("#sx-row-theme").click();
    await page.locator("#theme-panel").waitFor({ state: "visible" });
    truthy((await page.locator("#tp-list [data-theme-id]").count()) > 1, "no theme list");
    await page.keyboard.press("Escape");
    await page.locator("#theme-panel").waitFor({ state: "hidden" });
    same(await page.evaluate(() => document.activeElement.id), "sx-row-theme", "Esc returns to the row");
    await page.locator("#sx-row-theme").click();
    await page.locator('#tp-list [data-theme-id="dracula"]').click();
    await page.locator("#theme-panel").waitFor({ state: "hidden" });
    same(await attr(page, "theme"), "dracula", "theme applied");
    truthy((await page.locator("#sx-theme-val").textContent()).includes("Dracula"), "row does not show Dracula");
    await click(page, ["sx-sp-roomy"]);
    await page.locator("#sx-opentools-on").focus();
    const seen = [];
    for (let i = 0; i < 6 && !seen.includes("sx-reset"); i++) { await page.keyboard.press("Tab"); seen.push(await page.evaluate(() => document.activeElement.id)); }
    truthy(seen.includes("sx-row-theme") && seen.includes("sx-reset"), "Tab does not reach the Theme row and Reset: " + seen);
    await page.keyboard.press("Enter");
    same(await attr(page, "sp"), null, "Enter on Reset resets");
    same(await attr(page, "theme"), "dracula", "theme survives Reset");
    same(await attr(page, "font"), "serif", "font survives Reset");
  });
  await browser.close();
}

// ---- 390 (a phone): the reading width has no effect, and the largest choice of each setting never scrolls sideways ----
{
  const { browser, page } = await session(390, 844);
  await check("390: every reading width gives the default layout; the largest choice of each setting does not scroll sideways", async () => {
    await openChat(page);
    const d = await measure(page, "#chat");
    for (const v of ["wide", "wider", "full"]) {
      await openChat(page, { col: v });
      same((await measure(page, "#chat")).colW, d.colW, "reading width " + v + " on a phone");
    }
    for (const s of SETTINGS) {
      await openChat(page, { [s.key]: top(s) });
      const m = await measure(page, "#chat");
      truthy(m.sw <= m.cw && m.doc <= 0, `${s.key}=${top(s)} scrolls sideways (sw ${m.sw}, cw ${m.cw}, doc ${m.doc})`);
    }
    await openChat(page, Object.fromEntries(SETTINGS.map((s) => [s.key, top(s)])));
    const all = await measure(page, "#chat");
    truthy(all.doc <= 0, "every largest choice together scrolls sideways");
  });
  await check("390: Settings says the reading width has no effect on this window and does not scroll sideways", async () => {
    await openSettings(page);
    truthy(/no effect here/.test(await page.locator("#sx-view").textContent()), "no caption");
    truthy(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), "Settings scrolls sideways");
  });
  await check("390: Larger text does not shrink the composer's buttons", async () => {
    const hs = async (v) => { await openChat(page, v ? { rs: v } : {}); return page.evaluate(() => [...document.querySelectorAll("#detail .prompt .send, #detail .prompt .attach")].filter((e) => e.offsetHeight).map((e) => e.getBoundingClientRect().height)); };
    const d = await hs(""), l = await hs("larger");
    truthy(d.length > 0, "no composer buttons");
    same(l, d, "button heights at Larger vs Default");
  });
  await check("390: Reset restores the default and keeps the font", async () => {
    await openSettings(page, { font: "serif" });
    await click(page, ["sx-rs-larger", "sx-sp-roomy", "sx-wrap-on"]);
    await page.locator("#sx-reset").click();
    for (const k of RESET_KEYS) same(await attr(page, k), null, "data-" + k);
    same(await attr(page, "font"), "serif", "font stays");
    truthy((await page.locator("#sx-reset").evaluate((e) => e.getBoundingClientRect().height)) >= 36, "Reset is not a tap target");
  });
  await browser.close();
}

// ---- 1280: a wide choice is capped by the pane ----
{
  const { browser, page } = await session(1280, 800);
  await check("1280: the widest reading width is capped by the pane and the composer follows the column", async () => {
    await openChat(page, { col: "full" });
    const m = await measure(page, "#chat");
    truthy(m.colW <= m.cw && m.sw <= m.cw && m.doc <= 0, `column ${m.colW} in a pane ${m.cw}, doc overflow ${m.doc}`);
    const left = await page.evaluate(() => [document.querySelector("#chat .msg.assistant"), document.querySelector("#detail .prompt")].map((e) => Math.round(e.getBoundingClientRect().left)));
    truthy(Math.abs(left[0] - left[1]) <= 1, "composer and reply do not share a left edge: " + left);
  });
  await browser.close();
}

if (log.length) { console.log("console:\n  " + log.join("\n  ")); results.push(false); }
console.log(`${results.filter(Boolean).length}/${results.length} appearance checks passed`);
process.exit(results.every(Boolean) ? 0 : 1);
