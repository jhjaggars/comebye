// Keyboard and screen-reader pass over the conversation lines (tool runs, subagent lines, thinking lines).
// Everything is driven by keys only (Tab to arrive, then arrows, Home/End, Enter, Space, Esc), plus a light
// axe-style audit of the same markup (names, aria-expanded/controls, unique ids, focusable scroll boxes, live
// regions). The transcript is mocked, like toolruns.mjs.
//
//   node toolkeys.mjs BASE OUT_DIR      (env: SCHEME)
import { launch } from "./lib.mjs";
import fs from "node:fs";
import { claude } from "./fixtures/toolruns.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [], log = [];
const same = (got, want, what) => { if (got !== want) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };

const { browser, page } = await launch({ dark, width: 1280, height: 800 });
page.setDefaultTimeout(8000);
page.on("pageerror", (e) => log.push("pageerror: " + e.message));
await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: claude, total: claude.length, more: false, format: "claude" } }));

async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
  await page.screenshot({ path: `${out}/toolkeys-${name.replace(/\W+/g, "-").slice(0, 60)}.png` }).catch(() => {});
}
async function open() {
  await page.goto("about:blank");
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#chat .toolrun");
  await page.evaluate(() => { document.activeElement && document.activeElement.blur(); });
}
// What has focus, in words: the line's text for a line, "row:<id>" for a call's row.
const where = () => page.evaluate(() => {
  const e = document.activeElement;
  if (!e || e === document.body) return "body";
  if (e.classList.contains("tr-row")) return "row:" + (e.closest(".tr-item")?.dataset.id || "more");
  if (e.classList.contains("sa-line")) return "agent";
  if (e.closest(".thinking") && e.classList.contains("tr-line")) return "thought";
  if (e.classList.contains("tr-line")) return "run:" + e.querySelector(".tr-txt").textContent.slice(0, 12);
  return e.tagName.toLowerCase() + "." + e.className;
});
const press = async (...keys) => { for (const k of keys) await page.keyboard.press(k); };
// Tab until the focus is on a line (the first one at or after where it is).
async function tabToLine(pred = () => true) {
  for (let i = 0; i < 120; i++) {
    await page.keyboard.press("Tab");
    if (await page.evaluate(() => document.activeElement.classList.contains("tr-line"))) return;
  }
  throw new Error("Tab never reached a line");
}
const expanded = (sel) => page.locator(sel).first().getAttribute("aria-expanded");

await check("Tab reaches the first line, with a visible focus ring; Enter opens and Space closes it", async () => {
  await open();
  await tabToLine();
  const first = await where();
  if (!first.startsWith("thought") && !first.startsWith("run:")) throw new Error("landed on " + first);
  // The first conversation line is the reply's thought or the first run; walk to the first run by arrows.
  while (!(await where()).startsWith("run:")) await press("ArrowDown");
  const ring = await page.evaluate(() => { const c = getComputedStyle(document.activeElement); return { s: c.outlineStyle, w: parseFloat(c.outlineWidth) }; });
  if (ring.s === "none" || ring.w < 2) throw new Error("no focus ring: " + JSON.stringify(ring));
  const line = page.locator("#chat .toolrun .tr-line").first();
  same(await line.getAttribute("aria-expanded"), "false", "starts shut");
  await press("Enter");
  same(await line.getAttribute("aria-expanded"), "true", "Enter opens");
  same(await page.locator("#chat .toolrun").first().locator(".tr-list").isVisible(), true, "list visible");
  await press("Space");
  same(await line.getAttribute("aria-expanded"), "false", "Space closes");
  same(await where().then((w) => w.startsWith("run:")), true, "focus stays on the line");
});

await check("Up/Down walk lines and rows; Home and End jump to the first and last; the ends do not wrap", async () => {
  await open();
  await tabToLine();
  await press("Home");
  const stops = await page.evaluate(() => toolStops(document.getElementById("chat")).length);
  const first = await where();
  await press("End");
  const last = await where();
  if (first === last) throw new Error("Home and End landed on the same stop: " + first);
  await press("ArrowDown");
  same(await where(), last, "Down at the end stays");
  await press("Home");
  same(await where(), first, "Home");
  await press("ArrowUp");
  same(await where(), first, "Up at the start stays");
  // Open the first run: Down then enters its rows, in order; Up comes back.
  while (!(await where()).startsWith("run:")) await press("ArrowDown");
  const runLine = await where();
  await press("Enter");
  await press("ArrowDown");
  same(await where(), "row:c1", "first row");
  await press("ArrowDown");
  same(await where(), "row:c2", "second row");
  await press("End");
  same(await where(), last, "End goes to the very last stop, not the last row");
  await press("Home");
  while ((await where()) !== "row:c1") await press("ArrowDown");
  await press("ArrowUp");
  same(await where(), runLine, "Up from the first row is the run's line");
  // Rows are counted: a shut run contributes only its line.
  await press("Enter");
  const shut = await page.evaluate(() => toolStops(document.getElementById("chat")).length);
  if (shut >= stops + 10 || shut > stops) throw new Error(`shut run still lists rows: ${shut} stops vs ${stops}`);
});

await check("a row opens with Enter, Esc closes it back onto the row, a second Esc closes the run onto its line", async () => {
  await open();
  await tabToLine();
  while (!(await where()).startsWith("run:")) await press("ArrowDown");
  const runLine = await where();
  await press("Enter");
  await press("ArrowDown", "ArrowDown", "ArrowDown"); // c3, the failed call
  same(await where(), "row:c3", "row");
  const row = page.locator('.tr-item[data-id="c3"]');
  await press("Enter");
  same(await row.locator(".tr-row").getAttribute("aria-expanded"), "true", "row open");
  same(await row.locator(".tr-detail").isVisible(), true, "detail visible");
  // The detail's boxes and Copy buttons are Tab stops, and Esc from inside returns to the row.
  await press("Tab");
  same((await where()).startsWith("button.cpb"), true, "Tab goes into the detail (Copy)");
  await press("Escape");
  same(await where(), "row:c3", "Esc from the detail returns to its row");
  same(await row.locator(".tr-detail").isVisible(), false, "detail closed");
  await press("Escape");
  same(await where(), runLine, "Esc on the row closes the run and returns to the line");
  same(await expanded("#chat .toolrun .tr-line"), "false", "run shut");
  // A shut line leaves Esc alone (it is the app's).
  await page.evaluate(() => { window.__esc = null; document.addEventListener("keydown", (e) => { if (e.key === "Escape") window.__esc = e.defaultPrevented; }); });
  await press("Escape");
  same(await page.evaluate(() => window.__esc), false, "Esc on a shut line is not swallowed");
});

await check("the subagent line and a thought: arrows reach them, Enter opens, Esc closes and returns", async () => {
  await open();
  await tabToLine();
  await press("Home");
  while ((await where()) !== "thought") await press("ArrowDown");
  const thought = page.locator("#chat .thinking .tr-line").first();
  await press("Enter");
  same(await thought.getAttribute("aria-expanded"), "true", "thought opens");
  same(await page.locator("#chat .thinking .think-body").first().isVisible(), true, "thought text visible");
  await press("Escape");
  same(await thought.getAttribute("aria-expanded"), "false", "Esc closes the thought");
  same(await where(), "thought", "focus stays on the thought's line");
  while ((await where()) !== "agent") await press("ArrowDown");
  await press("Space");
  const sa = page.locator("#chat .subrun .sa-line");
  same(await sa.getAttribute("aria-expanded"), "true", "agent opens");
  await press("Tab");
  same(await page.evaluate(() => document.activeElement.className), "sa-steps", "Tab reaches View steps");
  await press("Escape");
  same(await sa.getAttribute("aria-expanded"), "false", "Esc inside closes the agent");
  same(await where(), "agent", "focus returns to the agent line");
});

await check("the app's own keys still work next to the lines: ? opens the cheat sheet and Esc closes it, Ctrl+K opens the palette", async () => {
  await open();
  await tabToLine();
  await press("?");
  await page.waitForSelector("#cheats:not([hidden])", { timeout: 3000 });
  await press("Escape");
  await page.waitForSelector("#cheats", { state: "hidden", timeout: 3000 });
  await press("Control+k");
  await page.waitForSelector("#palette:not([hidden])", { timeout: 3000 });
  await press("Escape");
  await page.waitForSelector("#palette", { state: "hidden", timeout: 3000 });
  // and the arrows still work after the layers closed (focus is wherever the app put it: get back on a line)
  await open();
  await tabToLine();
  await press("Home");
  const a = await where();
  await press("ArrowDown");
  if ((await where()) === a) throw new Error("ArrowDown did nothing");
  // j with a line focused is keys.js's (the next agent), not ours: the line handler must not eat it.
  await page.evaluate(() => { window.__j = null; document.addEventListener("keydown", (e) => { if (e.key === "j") window.__j = e.defaultPrevented; }); });
  await press("j");
  if (await page.evaluate(() => window.__j) === null) throw new Error("j never reached the document");
});

await check("screen readers: no live region in the chat; one status line says a failure or a finished subagent, coalesced", async () => {
  await open();
  same(await page.locator("#chat [aria-live], #chat [role=status], #chat [role=alert]").count(), 0, "live regions inside the chat");
  const live = page.locator("#tool-live");
  same(await live.getAttribute("aria-live"), "polite", "polite");
  same(await live.textContent(), "", "quiet at load");
  // A poll that changes nothing says nothing; a running call turning into a failure says so; many at once are one.
  await page.evaluate(() => {
    const run = [...document.querySelectorAll("#chat .toolrun")].pop();
    for (const m of run._calls) patchToolCard({ ...m });
    patchToolCard({ ...run._calls[run._calls.length - 1], err: true, result: "boom: no such file", ms: 80 });
    patchToolCard({ ...run._calls[0], err: true, result: "second", ms: 80 });
  });
  same(await live.textContent(), "", "nothing is said at once (it waits a moment, so a burst is one message)");
  await page.waitForFunction(() => document.getElementById("tool-live").textContent !== "", null, { timeout: 4000 });
  const said = await live.textContent();
  if (!/A tool call failed/.test(said) || said.length > 400) throw new Error("said " + said);
  // A subagent that was running and finished.
  await page.evaluate(() => {
    const sa = document.querySelector("#chat .subrun")._m;
    patchToolCard({ ...sa, result: "", ms: 0, subagent: { ...sa.subagent, status: "running" } });
    patchToolCard({ ...sa });
  });
  await page.waitForFunction(() => /Subagent .*finished/.test(document.getElementById("tool-live").textContent), null, { timeout: 4000 });
  // The line's own label reads the summary and state.
  const label = await page.locator("#chat .toolrun .tr-line").nth(1).getAttribute("aria-label");
  same(label, "Ran 2 commands, 2 failed", "label of a failed run");
  same(await page.locator("#chat .toolrun .tr-line").nth(1).getAttribute("aria-describedby") !== null, true, "the red first line is its description");
});

await check("axe-style audit: names, aria-expanded + aria-controls that resolve, unique ids, focusable scroll boxes, no tiny tap targets", async () => {
  await open();
  await page.locator("#chat .toolrun .tr-line").first().click();
  await page.locator('.tr-item[data-id="c3"] .tr-row').click();
  await page.locator("#chat .subrun .sa-line").click();
  await page.locator("#chat .thinking .tr-line").first().click();
  const bad = await page.evaluate(() => {
    const problems = [];
    const chat = document.getElementById("chat");
    const name = (e) => (e.getAttribute("aria-label") || e.textContent || "").replace(/\s+/g, " ").trim();
    const ids = new Map();
    for (const e of document.querySelectorAll("[id]")) ids.set(e.id, (ids.get(e.id) || 0) + 1);
    for (const [id, n] of ids) if (n > 1) problems.push("duplicate id " + id);
    for (const b of [...chat.querySelectorAll(".tr-line, .tr-row, .sa-steps, .cpb")].filter((x) => x.offsetParent)) {
      if (b.tagName !== "BUTTON") problems.push("not a button: " + b.className);
      if (!name(b)) problems.push("no name: " + b.className);
      if (/tr-line|tr-row/.test(b.className) && !b.classList.contains("tr-more")) {
        if (!b.hasAttribute("aria-expanded")) problems.push("no aria-expanded: " + b.className);
        const c = b.getAttribute("aria-controls");
        if (!c || !document.getElementById(c)) problems.push("aria-controls does not resolve: " + b.className + " " + c);
        else if (b.getAttribute("aria-expanded") === "true" && document.getElementById(c).hidden) problems.push("expanded but its target is hidden: " + c);
        else if (b.getAttribute("aria-expanded") === "false" && !document.getElementById(c).hidden) problems.push("shut but its target is shown: " + c);
      }
    }
    for (const p of chat.querySelectorAll(".tr-pre")) if (p.offsetParent && p.tabIndex < 0) problems.push("scroll box not focusable");
    for (const e of chat.querySelectorAll("[aria-hidden=true]")) if (e.querySelector("button, a, input, [tabindex]")) problems.push("focusable inside aria-hidden");
    for (const e of chat.querySelectorAll("[tabindex]")) if (+e.getAttribute("tabindex") > 0) problems.push("positive tabindex");
    for (const e of chat.querySelectorAll("[aria-controls]")) if (e.tagName !== "BUTTON") problems.push("aria-controls on a " + e.tagName);
    return problems;
  });
  same(JSON.stringify(bad), "[]", "problems");
});

await browser.close();
fs.writeFileSync(`${out}/toolkeys-console.log`, log.join("\n"));
const failed = results.filter((r) => !r.ok).length + log.length;
for (const l of log) console.log("     " + l);
console.log(`${results.length - results.filter((r) => !r.ok).length}/${results.length} toolkeys checks passed`);
process.exit(failed ? 1 : 0);
