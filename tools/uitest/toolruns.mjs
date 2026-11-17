// Tool calls in a chat are one quiet line per run ("Ran 3 commands, edited 2 files ›"): the line, the compact list, a
// row's input and output, the failure peek, grouping across polls, and the 44px tap targets. The transcript is
// mocked (a long claude one and a pi one), so it needs no live agent.
//
//   node toolruns.mjs BASE OUT_DIR      (env: SCHEME; SHOTS=1 also writes labelled screenshots at 390 and 1280)
import { launch } from "./lib.mjs";
import fs from "node:fs";
import { T, U, A, claude, pi } from "./fixtures/toolruns.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
const log = [];
const same = (got, want, what) => { if (got !== want) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };

const { browser, ctx, page } = await launch({ dark, width: 390, height: 844 });
page.setDefaultTimeout(8000);
page.on("pageerror", (e) => log.push("pageerror: " + e.message));
let current = claude;
await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: current, total: current.length, more: false, format: current === pi ? "pi" : "claude" } }));

async function open(msgs) {
  current = msgs;
  await page.goto("about:blank");
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#chat .toolrun", { timeout: 8000 });
}
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
  await page.screenshot({ path: `${out}/toolruns-${name.replace(/\W+/g, "-")}.png` }).catch(() => {});
}
const lines = () => page.$$eval("#chat .toolrun .tr-txt", (e) => e.map((x) => x.textContent));

const setWorking = (w) => page.evaluate((w) => { const a = agentById(state.selected); a.agent_status = w ? "working" : "idle"; renderActivity(); }, w);
const RUN1 = "Read 3 files, ran a search, ran 3 commands, edited 2 files, searched the web";
const run = (n) => page.locator("#chat .toolrun").nth(n);

await check("claude: consecutive calls are one line each, in first-seen order; no card boxes", async () => {
  await open(claude);
  same(JSON.stringify(await lines()), JSON.stringify([RUN1, "Ran 2 commands", "Read a file, ran a command"]), "lines");
  same(await page.locator("#chat .toolcard:not(.subagent)").count(), 0, "plain tool cards");
  same(await page.locator("#chat .toolcard.subagent").count(), 1, "the subagent card stays a card");
  same(await page.locator("#chat .tr-list:visible").count(), 1, "only the live run (a call is running) starts open");
  same(await run(2).locator(".tr-list").isVisible(), true, "the live run is the last");
  same(await run(0).locator(".tr-line").getAttribute("aria-expanded"), "false", "aria-expanded");
});

await check("failures: a red count for earlier ones, the first output line only when the last call failed", async () => {
  await open(claude);
  same(await run(0).locator(".tr-bad").textContent(), "1 failed", "run 1 count");
  same(await run(0).locator(".tr-peek").isVisible(), false, "run 1 has no peek (its last call passed)");
  same(await run(1).locator(".tr-bad").textContent(), "2 failed", "run 2 count");
  same(await run(1).locator(".tr-peek").textContent(), "restart_test.go:88: exit status 2: no such file or directory", "run 2 peek");
  same(await run(2).locator(".tr-bad").isVisible(), false, "run 3 has no count");
  const [peek, bad, normal] = await Promise.all([
    run(1).locator(".tr-peek").evaluate((e) => getComputedStyle(e).color),
    run(1).locator(".tr-bad").evaluate((e) => getComputedStyle(e).color),
    run(1).locator(".tr-txt").evaluate((e) => getComputedStyle(e).color),
  ]);
  same(peek, bad, "peek is in the error colour");
  if (bad === normal) throw new Error("the error colour equals the line colour");
});

await check("tap: the line opens a compact list, a row opens that call's input and output", async () => {
  await open(claude);
  await run(0).locator(".tr-line").click();
  same(await run(0).locator(".tr-item .tr-row").count(), 10, "rows");
  same(await run(0).locator(".tr-list").isVisible(), true, "list open");
  same(await run(0).locator(".tr-line").getAttribute("aria-expanded"), "true", "aria-expanded");
  const row = run(0).locator('.tr-item[data-id="c3"]');
  same(await row.locator(".tr-detail").isVisible(), false, "detail starts closed");
  await row.locator(".tr-row").click();
  same(await row.locator(".tr-detail").isVisible(), true, "detail open");
  same(await row.locator(".toolargs").textContent(), "Bash go test ./internal/upgrade/ -run Flaky -count=20", "input");
  if (!(await row.locator(".toolresult").textContent()).includes("--- FAIL: TestFlaky")) throw new Error("output missing");
  same(await row.locator(".toolresult.err").count(), 1, "a failed call's output is marked");
  same(await row.locator(".tr-st").textContent(), "failed · 40ms", "status");
  await run(0).locator(".tr-line").click();
  same(await run(0).locator(".tr-list").isVisible(), false, "list closes again");
});

await check("tap targets: line and rows are at least 44px on a phone", async () => {
  await open(claude);
  await run(0).locator(".tr-line").click();
  const hs = await run(0).locator(".tr-line, .tr-item .tr-row").evaluateAll((e) => e.map((x) => x.getBoundingClientRect().height));
  if (hs.length < 11 || hs.some((h) => h < 43.5)) throw new Error("heights " + JSON.stringify(hs));
  same(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth || document.getElementById("chat").scrollWidth > document.getElementById("chat").clientWidth), false, "sideways scroll");
});

await check("poll: a running call finishes and a new call joins the run, patched in place", async () => {
  await open(claude);
  const last = run(2);
  same(await last.locator(".tr-dot.run").count(), 1, "the running dot");
  same(await last.locator(".tr-list").isVisible(), true, "the live run is open without a tap");
  await setWorking(true); // the agent keeps working after the call ends: the run stays open
  await last.locator('.tr-item[data-id="e2"] .tr-row').click();
  await last.evaluate((e) => { e._mark = "same node"; });
  const next = claude.map((m) => (m.id === "e2" ? { ...m, result: "ok", ms: 1200 } : m)).concat(T("e3", "Edit", "web/app.js", "The file has been updated."));
  current = next;
  await page.evaluate(() => loadChat(false));
  await page.waitForFunction(() => document.querySelectorAll("#chat .tr-item").length === 15);
  same(await page.locator("#chat .toolrun").count(), 3, "still three runs");
  same(await last.evaluate((e) => e._mark), "same node", "the run was not re-rendered");
  same(await last.locator(".tr-txt").textContent(), "Read a file, ran a command, edited a file", "line recomputed");
  same(await last.locator(".tr-dot.run").count(), 0, "no running dot");
  same(await last.locator('.tr-item[data-id="e2"] .tr-st').textContent(), "1.2s", "row status");
  same(await last.locator(".tr-list").isVisible() && await last.locator('.tr-item[data-id="e2"] .tr-detail').isVisible(), true, "open list and row stay open");
  same(await last.locator('.tr-item[data-id="e2"] .toolresult').textContent(), "ok", "output filled in");
  // A failure arriving in the last call shows the peek, and a later passing call hides it again.
  current = next.map((m) => (m.id === "e3" ? { ...m, err: true, result: "patch rejected\nline 2" } : m));
  await page.evaluate(() => loadChat(false));
  await page.waitForFunction(() => document.querySelectorAll("#chat .toolrun")[2].querySelector(".tr-peek:not([hidden])"));
  same(await last.locator(".tr-peek").textContent(), "patch rejected", "peek on a new failure");
  same(await last.locator(".tr-bad").textContent(), "1 failed", "count");
});

await check("pi: lower-case tool names group the same way", async () => {
  await open(pi);
  same(JSON.stringify(await lines()), JSON.stringify(["Ran a command, read a file, ran 3 searches, edited a file, wrote a file"]), "pi line");
});

await check("the line sits in line with the reply text, between a user bubble and the reply", async () => {
  await open(claude);
  const g = await page.evaluate(() => {
    const r = (e) => e.getBoundingClientRect();
    const asst = [...document.querySelectorAll("#chat .msg.assistant .bubble")][0];
    const line = document.querySelector("#chat .tr-line");
    const user = document.querySelector("#chat .msg.user .bubble");
    return { aLeft: r(asst).left, lLeft: r(line).left, uRight: r(user).right, lRight: r(line).right, chatRight: r(document.getElementById("chat")).right };
  });
  if (Math.abs(g.aLeft - g.lLeft) > 8) throw new Error("the line is not aligned with the reply text: " + JSON.stringify(g));
  if (g.lRight > g.chatRight) throw new Error("overflows: " + JSON.stringify(g));
});


// ---- slice 4: the live tail ----
const poll = () => page.evaluate(() => loadChat(false));
const open2 = (n) => run(n).locator(".tr-list").isVisible();
const live = [U("u1", "Fix it."), A("a0", "On it."), T("x1", "Read", "a.go", "package a"), T("x2", "Bash", "go test ./...", "ok")];

await check("live: while the agent works, the latest run is open; with no reply and the agent idle it is not", async () => {
  current = live;
  await open(live);
  await setWorking(true);
  same(await open2(0), true, "open while working and latest");
  await setWorking(false);
  same(await open2(0), false, "folds when the agent is not working");
  await setWorking(true);
  same(await open2(0), true, "opens again");
  same(await run(0).locator(".tr-line").getAttribute("aria-expanded"), "true", "aria-expanded");
});

await check("live: a call running opens its run even when the agent looks idle; the fold on a reply", async () => {
  await open(live);
  await setWorking(false);
  current = live.concat(T("x3", "Bash", "sleep 30", "", { ms: 0, result: "" }));
  await poll();
  await page.waitForFunction(() => document.querySelectorAll("#chat .tr-item").length === 3);
  same(await open2(0), true, "open while a call runs");
  await setWorking(true);
  current = live.concat(T("x3", "Bash", "sleep 30", "done", { ms: 30000 }), A("a1", "All green."));
  await poll();
  await page.waitForSelector("#chat .msg.assistant >> text=All green.");
  same(await open2(0), false, "folded once the reply follows");
  same(await run(0).locator(".tr-line").getAttribute("aria-expanded"), "false", "aria-expanded");
  same(await run(0).locator(".tr-txt").textContent(), "Read a file, ran 2 commands", "summary line");
});

await check("live: a new run after a reply opens and the old one stays folded; a later reply folds it", async () => {
  await open(live.concat(A("a1", "Done step one.")));
  await setWorking(true);
  same(await open2(0), false, "run followed by a reply is folded even though the agent works");
  current = live.concat(A("a1", "Done step one."), T("y1", "Edit", "b.go", "ok"));
  await poll();
  await page.waitForFunction(() => document.querySelectorAll("#chat .toolrun").length === 2);
  same(await open2(0), false, "old run folded");
  same(await open2(1), true, "new run live");
  current = current.concat(T("y2", "Bash", "go vet", "ok"));
  await poll();
  await page.waitForFunction(() => document.querySelectorAll("#chat .toolrun")[1].querySelectorAll(".tr-item").length === 2);
  same(await open2(1), true, "still live after a poll adds a call");
});

await check("live: the user's open or close is kept per run across polls and beats the live rule", async () => {
  await open(live.concat(A("a1", "Done step one."), T("y1", "Edit", "b.go", "ok")));
  await setWorking(true);
  // closing the live run stays closed across polls, even as calls join it
  await run(1).locator(".tr-line").click();
  same(await open2(1), false, "the user closed the live run");
  current = current.concat(T("y2", "Bash", "go vet", "ok"));
  await poll();
  await page.waitForFunction(() => document.querySelectorAll("#chat .toolrun")[1].querySelectorAll(".tr-item").length === 2);
  same(await open2(1), false, "stays closed across a poll");
  await setWorking(true);
  same(await open2(1), false, "and across a status update");
  // opening a folded run stays open when a reply arrives after the live run
  await run(0).locator(".tr-line").click();
  same(await open2(0), true, "the user opened the old run");
  current = current.concat(A("a2", "Second reply."));
  await poll();
  await page.waitForSelector("#chat .msg.assistant >> text=Second reply.");
  same(await open2(0), true, "kept open");
  same(await open2(1), false, "still closed");
});

await check("live: folding keeps the reading position (reader scrolled up) and follows the bottom (reader at the end)", async () => {
  const filler = [];
  for (let i = 0; i < 14; i++) filler.push(U("fu" + i, "Question " + i), A("fa" + i, "Answer number " + i + " with a few words to take some room in the chat."));
  const base = filler.concat(T("z1", "Read", "a.go", "x"), T("z2", "Bash", "go build", "x"), T("z3", "Bash", "sleep 9", "", { ms: 0, result: "" }));
  await open(base);
  await setWorking(true);
  const at = () => page.evaluate(() => { const c = document.getElementById("chat"); return { top: c.scrollTop, gap: c.scrollHeight - c.scrollTop - c.clientHeight }; });
  // 1. scrolled up: a message in view keeps its on-screen place when the run above/below folds
  await page.evaluate(() => { const c = document.getElementById("chat"); c.scrollTop = 0; });
  await page.evaluate(() => document.getElementById("chat").dispatchEvent(new Event("scroll")));
  const probe = () => page.evaluate(() => { const e = [...document.querySelectorAll("#chat .msg.assistant .bubble")][3]; return Math.round(e.getBoundingClientRect().top); });
  const before = await probe();
  current = base.slice(0, -1).concat(T("z3", "Bash", "sleep 9", "done", { ms: 9000 }), A("fz", "Finished."));
  await poll();
  await page.waitForSelector("#chat .msg.assistant >> text=Finished.");
  same(await open2(0), false, "folded");
  same(await probe(), before, "the message in view did not move");
  same((await at()).top, 0, "scrollTop unchanged");
  same(await page.locator(".jump-anchor:not(.hidden)").count(), 1, "the jump button is shown");
  // 1b. the reader is on the run's own line (its list open below it): the line stays put when it folds
  const tall = filler.concat(Array.from({ length: 24 }, (_, i) => T("t" + i, "Read", "f" + i + ".go", "x")), T("z3", "Bash", "sleep 9", "", { ms: 0, result: "" }));
  await open(tall);
  await setWorking(true);
  await page.evaluate(() => { const c = document.getElementById("chat"); const l = document.querySelector("#chat .tr-line"); c.scrollTop += l.getBoundingClientRect().top - c.getBoundingClientRect().top - 40; });
  const lineTop = () => page.evaluate(() => Math.round(document.querySelector("#chat .tr-line").getBoundingClientRect().top));
  await page.waitForTimeout(600);
  const lt = await lineTop();
  if ((await at()).gap < 100) throw new Error("setup: the reader is still at the end");
  current = tall.slice(0, -1).concat(T("z3", "Bash", "sleep 9", "done", { ms: 9000 }), A("fz", "Finished.\n\n" + "A long closing paragraph that takes a few lines of the chat. ".repeat(60)));
  await poll();
  await page.waitForSelector("#chat .msg.assistant >> text=Finished.");
  same(await lineTop(), lt, "the run line did not move");
  // 2. at the end: the reader stays at the end after the fold
  await open(base);
  await setWorking(true);
  await page.evaluate(() => { const c = document.getElementById("chat"); c.scrollTop = c.scrollHeight; });
  current = base.slice(0, -1).concat(T("z3", "Bash", "sleep 9", "done", { ms: 9000 }), A("fz", "Finished."));
  await poll();
  await page.waitForSelector("#chat .msg.assistant >> text=Finished.");
  if ((await at()).gap > 4) throw new Error("not at the bottom after the fold: " + JSON.stringify(await at()));
});

await check("show more: a run over 25 calls lists its newest 6 and a Show N more reveals the rest", async () => {
  const many = [U("mu", "Go."), ...Array.from({ length: 30 }, (_, i) => T("m" + i, "Read", "f" + i + ".go", "x"))];
  await open(many);
  await run(0).locator(".tr-line").click();
  same(await run(0).locator(".tr-item:visible").count(), 6, "visible rows");
  same(await run(0).locator(".tr-more").textContent(), "Show 24 more", "button");
  const ids = await run(0).locator(".tr-item:visible").evaluateAll((e) => e.map((x) => x.dataset.id));
  same(ids[0] + ".." + ids[5], "m24..m29", "the newest are shown");
  const h = await run(0).locator(".tr-more").evaluate((e) => e.getBoundingClientRect().height);
  if (h < 43.5) throw new Error("Show more is " + h + "px high");
  await run(0).locator(".tr-more").click();
  same(await run(0).locator(".tr-item:visible").count(), 30, "all rows after the tap");
  same(await run(0).locator(".tr-more").isVisible(), false, "button gone");
  // a poll adding a call keeps all of them shown
  current = many.concat(T("m30", "Read", "g.go", "x"));
  await poll();
  await page.waitForFunction(() => document.querySelectorAll("#chat .tr-item").length === 31);
  same(await run(0).locator(".tr-item:visible").count(), 31, "kept shown across a poll");
  // 25 calls or fewer show everything
  await open([U("mu", "Go."), ...Array.from({ length: 25 }, (_, i) => T("n" + i, "Read", "f" + i + ".go", "x"))]);
  await run(0).locator(".tr-line").click();
  same(await run(0).locator(".tr-item:visible").count(), 25, "25 rows");
  same(await run(0).locator(".tr-more").isVisible(), false, "no button at 25");
});

// ---- desktop: 1280 ----
await browser.close();
{
  const d = await launch({ dark, width: 1280, height: 800 });
  d.page.setDefaultTimeout(8000);
  d.page.on("pageerror", (e) => log.push("pageerror: " + e.message));
  await d.page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: claude, total: claude.length, more: false, format: "claude" } }));
  const shots = process.env.SHOTS ? process.env.SHOTS_DIR || out : null;
  const dcheck = async (name, fn) => {
    try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
    catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
  };
  await dcheck("desktop: the line is a 32px target and the list and detail open", async () => {
    await d.page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
    await d.page.waitForSelector("#chat .toolrun");
    const r = d.page.locator("#chat .toolrun").first();
    const h = await r.locator(".tr-line").evaluate((e) => e.getBoundingClientRect().height);
    if (h < 31.5 || h > 40) throw new Error("line height " + h);
    if (shots) await d.page.screenshot({ path: `${shots}/toolruns-1280-collapsed.png` });
    await r.locator(".tr-line").click();
    await r.locator('.tr-item[data-id="c3"] .tr-row').click();
    same(await r.locator('.tr-item[data-id="c3"] .tr-detail').isVisible(), true, "detail");
    same(await d.page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, "sideways scroll");
    if (shots) { await r.locator('.tr-item[data-id="c3"]').scrollIntoViewIfNeeded(); await d.page.screenshot({ path: `${shots}/toolruns-1280-open.png` }); }
  });
  await d.browser.close();
}
fs.writeFileSync(`${out}/toolruns-console.log`, log.join("\n"));
const failed = results.filter((r) => !r.ok).length + log.length;
for (const l of log) console.log("     " + l);
console.log(`${results.length - results.filter((r) => !r.ok).length}/${results.length} toolruns checks passed`);
process.exit(failed ? 1 : 0);
