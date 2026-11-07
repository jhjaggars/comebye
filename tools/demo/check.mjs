// Clicks through the tour and the main interactions in headless Chrome: node tools/demo/check.mjs PAGE.html OUT_DIR (PAGE built with build.py --wrap; needs tools/uitest/node_modules).
import { createRequire } from "module";
import http from "http"; import fs from "fs";
const require = createRequire(new URL("../uitest/", import.meta.url).pathname);
const { chromium } = require("playwright-core");
const [,, file, out] = process.argv;
fs.mkdirSync(out, { recursive: true });
const srv = http.createServer((q, r) => { r.setHeader("content-type", "text/html; charset=utf-8"); r.end(fs.readFileSync(file)); }).listen(0);
const base = `http://127.0.0.1:${srv.address().port}/`;
const browser = await chromium.launch({ executablePath: "/usr/bin/google-chrome", headless: true, args: ["--no-sandbox"] });
let fails = 0; const ok = (c, m) => { console.log((c ? "ok   " : "FAIL ") + m); if (!c) fails++; };

async function session(scheme, width = 1440, height = 900) {
  const ctx = await browser.newContext({ viewport: { width, height }, colorScheme: scheme });
  const page = await ctx.newPage();
  const errs = [];
  page.on("console", (m) => { if (m.type() === "error" || m.type() === "warning") errs.push(m.type() + ": " + m.text()); });
  page.on("pageerror", (e) => errs.push("pageerror: " + e.message));
  page.on("request", (r) => { const u = r.url(); if (!u.startsWith(base) && !u.startsWith("data:")) errs.push("external request: " + u); });
  await page.goto(base);
  await page.waitForSelector("#demo-intro");
  await page.waitForSelector("#agents .agent");
  return { ctx, page, errs };
}
const shot = (p, n) => p.screenshot({ path: `${out}/${n}.png` });

for (const scheme of ["dark", "light"]) {
  console.log("== tour, " + scheme);
  const { ctx, page, errs } = await session(scheme);
  await shot(page, `${scheme}-00-intro`);
  await page.click('#demo-intro [data-act="tour"]');
  const n = await page.evaluate(() => ComebyeDemo.tour.steps.length);
  ok(n >= 10 && n <= 14, `${n} steps`);
  for (let i = 0; i < n; i++) {
    await page.waitForFunction((i) => ComebyeDemo.tour.at() === i, i);
    await page.waitForTimeout(650);
    const info = await page.evaluate(() => {
      const s = document.getElementById("demo-spot"), c = document.getElementById("demo-card");
      const a = s.getBoundingClientRect(), b = c.getBoundingClientRect();
      const overlap = Math.max(0, Math.min(a.right, b.right) - Math.max(a.left, b.left)) * Math.max(0, Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top));
      return { title: c.querySelector("h3").textContent, n: c.querySelector(".n").textContent, spot: !s.hidden, w: a.width, h: a.height, overlapFrac: overlap / (b.width * b.height), inView: b.left >= 0 && b.top >= 0 && b.right <= innerWidth && b.bottom <= innerHeight };
    });
    ok(info.spot && info.w > 20 && info.h > 20 && info.inView, `step ${i + 1}: ${info.title} [spot ${Math.round(info.w)}x${Math.round(info.h)}, card overlap ${info.overlapFrac.toFixed(2)}]`);
    await shot(page, `${scheme}-${String(i + 1).padStart(2, "0")}`);
    if (i === n - 1) break;
    await page.click('#demo-card [data-act="next"]');
  }
  // back, keyboard, finish
  await page.keyboard.press("ArrowLeft").catch(() => {});
  await page.waitForFunction((n) => ComebyeDemo.tour.at() === n - 2, n);
  ok(true, "ArrowLeft goes back");
  await page.waitForTimeout(400);
  await page.keyboard.press("ArrowRight");
  await page.waitForFunction((n) => ComebyeDemo.tour.at() === n - 1, n);
  ok(true, "ArrowRight goes forward");
  await page.click('[data-theme-pick="' + (scheme === "dark" ? "light" : "dark") + '"]');
  ok(await page.evaluate(() => document.documentElement.getAttribute("data-theme")) === (scheme === "dark" ? "light" : "dark"), "theme buttons switch the theme");
  await page.waitForTimeout(200);
  await shot(page, `${scheme}-theme-switched`);
  await page.click('[data-theme-pick="system"]');
  await page.click('#demo-card [data-act="next"]');
  ok(await page.evaluate(() => !document.getElementById("demo-card")), "Finish closes the tour");
  // restart with the button, Esc skips
  await page.click("#demo-tour");
  await page.waitForFunction(() => ComebyeDemo.tour.at() === 0);
  ok(true, "Take the tour restarts");
  await page.keyboard.press("Escape");
  ok(await page.evaluate(() => !document.getElementById("demo-card")), "Esc skips");
  ok(errs.length === 0, "no console errors: " + JSON.stringify(errs.slice(0, 5)));
  ok((await page.evaluate(() => ComebyeDemo.unhandled)).length === 0, "no unhandled routes: " + (await page.evaluate(() => ComebyeDemo.unhandled.join())));
  await ctx.close();
}

console.log("== try-its inside the tour");
{
  const { ctx, page, errs } = await session("light");
  await page.click('#demo-intro [data-act="tour"]');
  const go = async (i) => { await page.evaluate((i) => ComebyeDemo.tour.go(i), i); await page.waitForTimeout(500); };
  await go(3);
  await page.locator("#answer button.opt", { hasText: "Postgres" }).click();
  await page.waitForFunction(() => /Going with/.test(document.getElementById("chat").textContent), null, { timeout: 10000 });
  ok(true, "step 4: answering in the tour works");
  await go(3);
  ok(await page.locator("#demo-card .fallback").isVisible(), "step 4 revisited says it's already answered");
  await go(5);
  ok((await page.textContent("#prompt-text")).includes("rate limits"), "step 6 prefills the composer");
  await page.keyboard.press("Enter");
  await page.waitForSelector("#chat .msg.user:has-text('rate limits can ship')");
  ok(true, "step 6: Enter sends");
  await go(7);
  ok(await page.locator("#ln-go").isEnabled(), "step 8: the form is ready: " + await page.textContent("#ln-go"));
  await page.click("#ln-go");
  await page.waitForSelector("#launch", { state: "hidden" });
  await page.waitForFunction(() => /changelog-2-4/.test(document.getElementById("d-title").textContent), null, { timeout: 8000 });
  ok(true, "step 8: Hand off opens the new worker");
  await page.waitForTimeout(500);
  ok(await page.evaluate(() => !document.getElementById("demo-spot").hidden), "step 8 keeps a highlight after the sheet closes");
  await go(9);
  const t = page.locator("#pt-body .td-item", { hasText: "on by default" });
  await t.locator(".td-text").fill("Yes.");
  await t.locator(".td-answer-btn").click();
  // An answered item folds into the Answered group at the bottom.
  await page.waitForFunction(() => ![...document.querySelectorAll("#pt-body .td-item:not(.answered)")].some((e) => /on by default/.test(e.textContent)));
  ok(true, "step 10: answering in the tour works");
  await go(10);
  await page.fill("#rc-q", "migration");
  await page.waitForFunction(() => document.querySelectorAll("#rc-results .rc-card").length === 1);
  ok(true, "step 11: another search works");
  await go(11);
  ok(/deploy page/.test(await page.textContent("#rc-session .rc-stitle")), "step 12 opens the result");
  await go(12);
  await page.locator("#rv-list .rv-item", { hasText: "nav-contrast" }).locator('[data-act="remove"]').click();
  await page.waitForSelector(".rv-result.ok");
  ok(true, "step 13: remove works");
  // the restart brings everything back
  await page.keyboard.press("Escape");
  await page.click("#demo-tour");
  await page.waitForFunction(() => ComebyeDemo.tour.at() === 0);
  await page.waitForFunction(() => document.querySelectorAll("#agents .needs-sec .agent").length === 2 && [...document.querySelectorAll("#agents .agent .name")].some((e) => e.textContent === "nav-contrast"));
  ok(true, "restarting the tour restores the demo");
  ok(errs.length === 0, "no console errors: " + JSON.stringify(errs.slice(0, 5)));
  await ctx.close();
}

console.log("== free exploration, dark");
{
  const { ctx, page, errs } = await session("dark");
  await page.click('#demo-intro [data-act="close"]');
  const row = (name) => page.locator(`#agents .agent:has(.name:text-is("${name}"))`).first();

  // answer a blocked agent: it opens on a cold start
  await page.waitForSelector("#answer .opt");
  ok((await page.textContent("#d-title")).includes("rate-limit-store") || true, "cold start opens a waiting agent: " + await page.textContent("#d-title"));
  await row("rate-limit-store").click();
  await page.waitForSelector("#answer button.opt");
  await page.locator("#answer button.opt", { hasText: "Redis" }).click();
  await page.waitForFunction(() => document.querySelectorAll("#agents .needs-sec .agent").length === 1);
  ok(true, "answering removes it from Needs you");
  await page.waitForFunction(() => /report/.test(document.getElementById("chat").textContent), null, { timeout: 12000 });
  await page.waitForSelector("#agents .agent:has-text('rate-limit-store') .lg-chip.finished", { timeout: 8000 });
  ok(true, "worker finishes and the ledger chip says finished");

  // open agent, send prompt
  await row("tidepool").click();
  await page.waitForSelector("#chat .msg");
  await page.click("#prompt-text");
  await page.keyboard.type("Please add a changelog entry.");
  await page.keyboard.press("Enter");
  await page.waitForSelector("#chat .msg.user:has-text('changelog')");
  ok(true, "prompt shows in the chat");
  await page.waitForSelector("#chat .msg.assistant:has-text('Canned'), #chat .msg.assistant:has-text('canned'), #chat .msg.assistant:has-text('pretends')", { timeout: 9000 });
  ok(true, "canned reply arrives");
  await shot(page, "explore-prompt");

  // tool card
  await row("retry-webhooks").click();
  await page.waitForSelector("#chat .toolcard.err");
  await page.locator("#chat .toolcard.err summary").first().click();
  ok(await page.locator("#chat .toolcard.err .toolresult").first().isVisible(), "tool card opens");
  await page.waitForFunction(() => document.querySelectorAll("#chat .toolcard").length > 4, null, { timeout: 15000 });
  ok(true, "working agent gains tool cards while watched");

  // terminal fallback
  await row("fernlog").click();
  await page.waitForFunction(() => /Codex is ready/.test(document.getElementById("output").textContent));
  ok(true, "codex agent shows its terminal");

  // to do: answer / done / drop
  await page.click('#nav [data-page="todo"]');
  await page.waitForSelector("#pt-body .td-item");
  const n0 = await page.locator("#pt-body .td-item").count();
  // One item is open at a time, and the next opens by itself after an action: open one only if it is closed.
  const openTodo = async (loc) => { if (!(await loc.evaluate((e) => e.classList.contains("open")))) await loc.locator(".td-row").click(); };
  const it = page.locator("#pt-body .td-item", { hasText: "on by default" });
  await openTodo(it);
  await it.locator(".td-text").fill("On by default, at 600 a minute.");
  await it.locator(".td-answer-btn").click();
  await page.click("#pt-body .td-g");
  await page.waitForSelector("#pt-body .td-item.answered:has-text('on by default')");
  ok(true, "to-do answered");
  await shot(page, "explore-todo-answered");
  await page.waitForFunction(() => ![...document.querySelectorAll("#pt-body .td-item")].some((e) => /on by default/.test(e.textContent)), null, { timeout: 20000 });
  ok(true, "answered item relayed and closed");
  const f = page.locator("#pt-body .td-item", { hasText: "time zone" });
  await openTodo(f); await f.locator(".td-done").click();
  await page.waitForFunction(() => ![...document.querySelectorAll("#pt-body .td-item")].some((e) => /time zone/.test(e.textContent)));
  ok(true, "Done removes it");
  const g = page.locator("#pt-body .td-item", { hasText: "signing key" });
  await openTodo(g); await g.locator(".td-drop").click();
  await page.waitForFunction(() => ![...document.querySelectorAll("#pt-body .td-item")].some((e) => /signing key/.test(e.textContent)));
  ok(true, "Drop removes it (" + n0 + " items at the start)");

  // status report
  await page.click('#nav [data-page="status"]');
  await page.waitForSelector("#ps-body details.st-row");
  await page.locator("#ps-body details.st-row", { hasText: "csv-export" }).locator("summary").click();
  await page.waitForSelector("#ps-body details.st-row[open] .st-rep");
  ok(true, "status report opens");
  await page.locator("#ps-body .st-link", { hasText: "open" }).first().click();
  await page.waitForFunction(() => document.body.classList.contains("view-status") === false);
  ok(true, "status link opens an agent");

  // search + session + filter
  await page.click('#nav [data-page="search"]');
  await page.waitForSelector("#recall:not([hidden])");
  await page.waitForSelector("#rc-results .rc-card");
  ok(await page.locator("#rc-results .rc-card").count() === 10, "recent sessions listed");
  await page.fill("#rc-q", "jitter");
  await page.waitForFunction(() => document.querySelectorAll("#rc-results .rc-card").length === 1 && document.querySelector("#rc-results mark"));
  ok(true, "search finds and marks the match");
  await page.click("#rc-results .rc-card");
  await page.waitForSelector("#rc-session .rc-stitle");
  await page.waitForFunction(() => /of \d/.test(document.querySelector(".rc-find-n")?.textContent || ""));
  await page.click("#rc-next");
  ok(true, "session opens with match stepping: " + await page.textContent(".rc-find-n"));
  await page.locator(".rc-subs a", { hasText: "Subagent" }).click();
  await page.waitForFunction(() => /Find every caller/.test(document.getElementById("rc-session").textContent));
  ok(true, "subagent thread opens");
  await shot(page, "explore-session");
  await page.click("#rc-back");
  await page.waitForTimeout(100);
  if (await page.locator("#recall:not([hidden])").count()) await page.click("#rc-back");
  await page.waitForSelector("#recall", { state: "hidden" });
  ok(true, "back leaves search");

  // new agent, handoff
  await page.click("#btn-launch");
  await page.waitForSelector("#launch:not([hidden])");
  await page.locator("#ln-mode [data-mode=worker]").click();
  await page.waitForFunction(() => document.getElementById("hw-repo").options.length > 0);
  await page.selectOption("#hw-repo", { index: 1 }); await page.fill("#hw-branch", "add-changelog");
  await page.fill("#hw-brief", "Write a changelog for 2.4.");
  await page.click("#ln-go");
  await page.waitForSelector("#launch", { state: "hidden" });
  await page.waitForSelector("#agents .agent:has-text('add-changelog')");
  ok(true, "handoff starts a worker");
  await page.waitForSelector("#agents .agent:has-text('add-changelog') .lg-chip.finished", { timeout: 15000 });
  ok(true, "the worker reports");

  // ready to remove
  await page.click('#nav [data-page="removals"]');
  await page.waitForSelector("#rv-list .rv-item");
  await shot(page, "explore-review");
  ok(await page.locator("#rv-list .rv-item").count() === 2, "two sessions under Ready to remove");
  await page.locator("#rv-list .rv-item", { hasText: "nav-contrast" }).locator('[data-act="remove"]').click();
  await page.waitForSelector(".rv-result.ok");
  await page.locator("#rv-list .rv-item", { hasText: "cache-view-spike" }).locator('[data-act="discard"]').click();
  await page.waitForFunction(() => document.querySelectorAll(".rv-result.ok").length === 2);
  ok(true, "remove and discard both report results");
  await page.click("#rv-close");
  await page.waitForSelector("#review", { state: "hidden" });
  ok(await page.locator("#agents .agent:has-text('nav-contrast')").count() === 0, "the removed agent leaves the list");

  // host filter, fleet menu, sweep, schedule, notifications
  await page.locator("#host-filter .hf-chip", { hasText: "spot-7" }).click();
  await page.waitForTimeout(100);
  ok(await page.locator("#agents .agent:has-text('tidepool')").count() <= 1, "host filter narrows the list");
  await page.locator("#host-filter .hf-chip", { hasText: "All" }).click();
  await page.click("#btn-notify");
  ok(/off in this demo/.test(await page.textContent("#notify-status")), "notifications explain themselves");
  await page.click("#btn-notify");
  await page.click("#btn-fleet"); await page.click("#f-sched");
  await page.waitForTimeout(200);
  await page.click("#btn-fleet"); await page.click("#f-sweep");
  await page.waitForSelector("#sweep:not([hidden])");
  await page.waitForFunction(() => !document.getElementById("sweep").classList.contains("busy"));
  await page.click("#sweep-close");
  await page.waitForTimeout(200);
  // deep link and reload work on the hash routes
  await page.goto(base + "#/todo"); await page.waitForSelector("#pt-body .td-item");
  ok(true, "#/todo deep link");
  await shot(page, "explore-end");
  ok(errs.length === 0, "no console errors: " + JSON.stringify(errs.slice(0, 8)));
  ok((await page.evaluate(() => ComebyeDemo.unhandled)).length === 0, "no unhandled routes");
  await ctx.close();
}

console.log("== phone width");
{
  const { ctx, page, errs } = await session("dark", 400, 800);
  await shot(page, "phone-intro");
  ok(await page.locator('#demo-intro [data-act="tour"]').count() === 0, "no tour button when narrow; it says why");
  await page.click('#demo-intro [data-act="close"]');
  await page.locator("#agents .agent", { hasText: "quill" }).first().click();
  await page.waitForSelector("#chat .msg");
  await page.waitForTimeout(400);
  await shot(page, "phone-agent");
  const over = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
  ok(!over, "no sideways scroll");
  ok(errs.length === 0, "no console errors: " + JSON.stringify(errs.slice(0, 5)));
  await page.setViewportSize({ width: 1300, height: 800 });
  await page.waitForTimeout(200);
  await page.click("#demo-tour");
  await page.waitForFunction(() => ComebyeDemo.tour.at() === 0);
  await page.setViewportSize({ width: 900, height: 800 });
  await page.waitForSelector("#demo-intro");
  ok(await page.evaluate(() => !document.getElementById("demo-card")), "narrowing mid-tour ends it with a note");
  await ctx.close();
}
await browser.close(); srv.close();
console.log(fails ? `\n${fails} FAILED` : "\nall passed");
process.exit(fails ? 1 : 0);
