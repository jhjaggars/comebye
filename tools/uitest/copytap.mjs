// Tap-to-copy values, whole-message copy and the other copy buttons (the "cp"
// class and data-copy-* in web/app.js), against the shared server, in headless
// Chrome with clipboard permission so the text that reaches the clipboard is
// read back. Phone-size by default; a desktop pass runs at the end.
//
//   node copytap.mjs BASE OUT_DIR      (env: SCHEME)
import { launch } from "./lib.mjs";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const { browser, ctx, page } = await launch({ dark });
await ctx.grantPermissions(["clipboard-read", "clipboard-write"], { origin: new URL(base).origin });
page.setDefaultTimeout(8000);
const log = [];
page.on("pageerror", (e) => log.push("pageerror: " + e.message));
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
  await page.screenshot({ path: `${out}/copytap-${name.replace(/\W+/g, "-")}.png` }).catch(() => {});
}
const same = (got, want, what) => { if (got !== want) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };
const clip = () => page.evaluate(() => navigator.clipboard.readText());
const sentinel = () => page.evaluate(() => navigator.clipboard.writeText("sentinel"));
const post = (path, body) => fetch(base + path, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) }).then((r) => r.json());

// ---- slice 2: inline values ----
const CTX = "Check commit `c9fa5cc` on branch `research/copy-buttons`, the file `/tmp/shots/shot-390.png`, " +
  "item `tdac2953fe3`, the word `foo`, the flag `--force`, `and/or` is not a path? and `error: gpg failed to sign`.\n\n" +
  "A link [`/docs/a`](https://example.com/a) and a block:\n\n```\n/tmp/inside-a-block\n```\n";
const todo = await post("/api/todos", { question: "Copy tap " + Date.now(), kind: "decision", no_wait: true, context: CTX });
const tid = todo.todo.id;

async function openTodo() {
  await page.goto("about:blank");
  await page.goto(base + "/#/todo", { waitUntil: "load" });
  await page.waitForSelector(".td-item", { timeout: 8000 });
  await page.locator(`.td-item[data-id="${tid}"] > .td-row:visible`).first().click();
  await page.waitForSelector(".td-ctx code", { timeout: 8000 });
}
const ctxCp = () => page.locator(".td-ctx code.cp");

await check("inline: only SHAs, paths, branches, ids and errors are tap-to-copy", async () => {
  await openTodo();
  const got = await page.$$eval(".td-ctx code", (c) => c.filter((x) => x.offsetParent).map((x) => [x.textContent, x.classList.contains("cp")]));
  const cp = got.filter((g) => g[1]).map((g) => g[0]);
  same(JSON.stringify(cp), JSON.stringify(["c9fa5cc", "research/copy-buttons", "/tmp/shots/shot-390.png", "tdac2953fe3", "and/or", "error: gpg failed to sign"]), "tap-to-copy values");
  const plain = got.filter((g) => !g[1]).map((g) => g[0]);
  for (const w of ["foo", "--force", "/docs/a"]) if (!plain.includes(w)) throw new Error(w + " should stay plain: " + JSON.stringify(plain));
  const roles = await ctxCp().evaluateAll((c) => c.every((x) => x.getAttribute("role") === "button" && x.tabIndex === 0 && x.getAttribute("aria-label").startsWith("Copy ")));
  same(roles, true, "role, tabindex and aria-label");
});

await check("inline: a tap copies the value and shows a check", async () => {
  await sentinel();
  const sha = ctxCp().first();
  await sha.evaluate((e) => e.scrollIntoView({ block: "center" }));
  await sha.click();
  await page.waitForFunction(() => document.querySelector(".td-ctx code.cp").dataset.state === "ok");
  same(await clip(), "c9fa5cc", "sha");
  await page.waitForFunction(() => document.querySelector("[role=status].sr-only")?.textContent === "Copied c9fa5cc");
  await page.waitForFunction(() => !document.querySelector(".td-ctx code.cp").dataset.state, null, { timeout: 4000 });
  await sentinel(); await ctxCp().nth(2).click(); await page.waitForTimeout(250);
  same(await clip(), "/tmp/shots/shot-390.png", "path");
});

await check("inline: the tap area is about 32px high and a tap just above the text lands", async () => {
  const sha = ctxCp().first();
  await sha.evaluate((e) => e.scrollIntoView({ block: "center" }));
  const box = await sha.boundingBox();
  const area = await sha.evaluate((e) => { const r = getComputedStyle(e, "::before"); return [r.top, r.bottom, r.position]; });
  same(area[2], "absolute", "hit area pseudo-element");
  // the pseudo-element reaches 10px above and below the code box
  const h = box.height + 14; // inline code: 7px above and below
  if (h < 30) throw new Error("tap area is " + h + "px high, want 30 or more");
  await sentinel();
  await page.mouse.click(box.x + box.width / 2, box.y - 7);
  await page.waitForTimeout(300);
  same(await clip(), "c9fa5cc", "tap 7px above the text");
});

await check("inline: Enter and Space copy from the keyboard", async () => {
  await ctxCp().nth(1).evaluate((e) => e.scrollIntoView({ block: "center" }));
  await ctxCp().nth(1).focus();
  await sentinel(); await page.keyboard.press("Enter"); await page.waitForTimeout(250);
  same(await clip(), "research/copy-buttons", "Enter");
  await page.waitForTimeout(1700);
  await ctxCp().nth(1).focus();
  await sentinel(); await page.keyboard.press("Space"); await page.waitForTimeout(250);
  same(await clip(), "research/copy-buttons", "Space");
});

await check("inline: the To do item id is tap-to-copy", async () => {
  const id = page.locator(".td-item .td-meta .cp:visible").first();
  await id.waitFor();
  same(await id.evaluate((e) => e.getAttribute("aria-label")), "Copy item id", "label");
  await sentinel(); await id.click(); await page.waitForTimeout(250);
  same(await clip(), tid, "item id");
});

// ---- slice 3: copy a whole message, report or item ----
await check("todo: Copy question and context copies the Markdown source", async () => {
  await openTodo();
  const b = page.locator(".td-item .td-cprow .cpb:visible").first();
  await b.evaluate((e) => e.scrollIntoView({ block: "center" }));
  const h = await b.evaluate((e) => e.getBoundingClientRect().height);
  if (h < 35.5) throw new Error("tap target is " + h + "px high, want 36");
  same(await b.getAttribute("aria-label"), "Copy the question and its context as Markdown", "aria-label");
  await sentinel(); await b.click();
  await page.waitForFunction(() => document.querySelector(".td-cprow .cpb")?.dataset.state === "ok");
  same(await clip(), todo.todo.question + "\n\n" + CTX.trimEnd(), "question and context");
  same((await b.innerText()).trim(), "Copied", "label");
  await page.waitForFunction(() => !document.querySelector(".td-cprow .cpb").dataset.state, null, { timeout: 4000 });
  same((await b.innerText()).trim(), "Copy question and context", "label resets");
});

const MARK = "q" + Date.now();
const SENT = "run it\n```sh\necho " + MARK + "\n```";
await check("chat: Copy under a message copies its Markdown source (yours and the reply)", async () => {
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#detail", { state: "visible" });
  await page.locator("#prompt-text").click();
  for (const [i, line] of SENT.split("\n").entries()) {
    if (i) { await page.keyboard.down("Shift"); await page.keyboard.press("Enter"); await page.keyboard.up("Shift"); }
    await page.keyboard.type(line);
  }
  await page.locator("#btn-send").click();
  const reply = page.locator("#chat .msg.assistant", { has: page.locator(".cb", { hasText: MARK }) });
  await reply.first().waitFor({ timeout: 9000 });
  const mine = page.locator("#chat .msg.user", { hasText: MARK });
  await mine.last().waitFor();
  const ub = mine.last().locator(".cpb");
  await ub.evaluate((e) => e.scrollIntoView({ block: "center" }));
  same(await ub.getAttribute("aria-label"), "Copy message", "aria-label");
  const h = await ub.evaluate((e) => e.getBoundingClientRect().height);
  if (h < 35.5) throw new Error("tap target is " + h + "px high, want 36");
  await sentinel(); await ub.click(); await page.waitForTimeout(300);
  same(await clip(), SENT, "your message");
  const ab = reply.last().locator(".cpb");
  await sentinel(); await ab.click(); await page.waitForTimeout(300);
  const got = await clip();
  if (!got.includes("```sh") || !got.includes("echo " + MARK)) throw new Error("reply source lost its fence: " + JSON.stringify(got));
  const sz = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
  same(sz, false, "page scrolls sideways");
});

// ---- the same on a desktop-size window ----
const desk = await launch({ width: 1280, height: 800, dark });
await desk.ctx.grantPermissions(["clipboard-read", "clipboard-write"], { origin: new URL(base).origin });
desk.page.on("pageerror", (e) => log.push("pageerror(desktop): " + e.message));
async function dcheck(name, fn) {
  try { await fn(desk.page); results.push({ name: "desktop: " + name, ok: true }); console.log("ok   desktop: " + name); }
  catch (e) { results.push({ name: "desktop: " + name, ok: false }); console.log("FAIL desktop: " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
  await desk.page.screenshot({ path: `${out}/copytap-desktop-${name.replace(/\W+/g, "-")}.png` }).catch(() => {});
}
const dclip = () => desk.page.evaluate(() => navigator.clipboard.readText());
await dcheck("the To do item id and an inline path copy", async (pg) => {
  await pg.goto(base + "/#/todo", { waitUntil: "load" });
  await pg.waitForSelector(`#p-todo .td-item[data-id="${tid}"] .td-row`, { state: "visible", timeout: 8000 });
  await pg.locator(`#p-todo .td-item[data-id="${tid}"] .td-row`).first().click();
  const id = pg.locator(".td-panel .td-sv .cp:visible").first();
  await id.waitFor();
  await pg.evaluate(() => navigator.clipboard.writeText("sentinel"));
  await id.click(); await pg.waitForTimeout(250);
  same(await dclip(), tid, "item id");
  await pg.evaluate(() => navigator.clipboard.writeText("sentinel"));
  await pg.locator("#p-todo .td-ctx code.cp").nth(2).click(); await pg.waitForTimeout(250);
  same(await dclip(), "/tmp/shots/shot-390.png", "path");
});

const REPORT = "## Summary\nFixed the **rail dot**.\n\n```sh\ngo test ./...\n```\n\nCommit `0badc0de1`.";
await dcheck("a Status report has Copy report that copies its Markdown", async (pg) => {
  await pg.route(/\/api\/ledger$/, async (r) => {
    const j = await (await r.fetch()).json();
    j.entries = [...(j.entries || []), { id: "cpfx", host: "uitest", repo_name: "comebye", branch: "fix/rail-dot", agent_name: "fix-rail-dot", status: "finished",
      created_at: new Date(Date.now() - 3600e3).toISOString(), finished_at: new Date(Date.now() - 600e3).toISOString(), final_report: REPORT, live: { present: false } }];
    r.fulfill({ json: j });
  });
  await pg.goto(base + "/#/status", { waitUntil: "load" });
  await pg.locator('#ps-body details[data-id="cpfx"] summary').click();
  const b = pg.locator('#ps-body details[data-id="cpfx"] .cpb');
  await b.waitFor();
  const open = await pg.locator('#ps-body details[data-id="cpfx"]').evaluate((d) => d.open);
  same(open, true, "pressing Copy report must not close the report");
  await pg.evaluate(() => navigator.clipboard.writeText("sentinel"));
  await b.click(); await pg.waitForTimeout(300);
  same(await dclip(), REPORT, "report source");
  same(await pg.locator('#ps-body details[data-id="cpfx"]').evaluate((d) => d.open), true, "report stays open after Copy");
  await pg.unroute(/\/api\/ledger$/);
});

fs.writeFileSync(`${out}/copytap-console.log`, log.join("\n"));
await post(`/api/todos/${tid}/drop`, {}).catch(() => {});
await browser.close();
await desk.browser.close();
const bad = results.filter((r) => !r.ok).length + (log.length ? 1 : 0);
if (log.length) console.log("page errors:\n" + log.join("\n"));
console.log(`${results.length - results.filter((r) => !r.ok).length}/${results.length} copytap checks passed`);
process.exit(bad ? 1 : 0);
