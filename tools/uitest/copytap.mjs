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

// ---- slice 4: copy what a tool ran, a hub check, a terminal snapshot ----
await check("chat: a tool card copies the command and the result", async () => {
  const msgs = [
    { id: "tc1", kind: "tool", tool: "Bash", args: "go test ./internal/upgrade/", result: "ok  \tgithub.com/x/upgrade\t0.412s\nok  \tgithub.com/x/other\t0.2s", ms: 412 },
    { id: "tc2", kind: "tool", tool: "Bash", args: "comebye upgrade --smoke-only", result: "smoke: dial tcp 10.0.0.7:443: connection refused", err: true, ms: 9 },
  ];
  await page.route(/\/transcript\?/, (r) => r.fulfill({ json: { messages: msgs, total: 2, more: false, format: "claude" } }));
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  const card = page.locator('#chat .toolcard[data-id="tc1"]');
  await card.waitFor({ timeout: 8000 });
  await card.locator("summary").click();
  const cmd = card.locator('.cpb[data-copy-target=".toolargs"]');
  const h = await cmd.evaluate((e) => e.getBoundingClientRect().height);
  if (h < 35.5) throw new Error("tap target is " + h + "px high, want 36");
  await sentinel(); await cmd.click(); await page.waitForTimeout(300);
  same(await clip(), "go test ./internal/upgrade/", "command");
  await page.waitForTimeout(1600);
  await sentinel(); await card.locator('.cpb[data-copy-target=".toolresult"]').click(); await page.waitForTimeout(300);
  same(await clip(), "ok  \tgithub.com/x/upgrade\t0.412s\nok  \tgithub.com/x/other\t0.2s", "result, with its line break and tab");
  same(await card.evaluate((d) => d.open), true, "the card stays open");
  await page.unroute(/\/transcript\?/);
});

await check("answer card: Copy screen copies the terminal snapshot", async () => {
  const agents = await fetch(base + "/api/agents").then((r) => r.json());
  const list = Array.isArray(agents) ? agents : agents.agents || [];
  const cands = list.filter((a) => ["needs-input", "trust-prompt", "claude-trust"].includes(a.name));
  if (!cands.length) throw new Error("no blocked agent in the fake fleet: " + JSON.stringify(list.map((a) => a.name || a.id)));
  let found = false;
  for (const blocked of cands) {
    await page.goto("about:blank");
    await page.goto(base + "/#/agent/" + encodeURIComponent(blocked.pane_id || blocked.id.split("/").pop()), { waitUntil: "load" });
    found = await page.waitForSelector("#answer-snap", { state: "attached", timeout: 4000 }).then(() => true, () => false);
    if (found) break;
  }
  if (!found) throw new Error("no answer card with a snapshot for " + cands.map((a) => a.name));
  const b = page.locator('.cpb[data-copy-target="#answer-snap"]');
  if (!(await b.isVisible())) await page.locator(".ans-more summary").click();
  await b.waitFor({ state: "visible" });
  const want = (await page.locator("#answer-snap").evaluate((e) => e.innerText)).replace(/\n+$/, "");
  if (!want) throw new Error("the snapshot is empty");
  await sentinel(); await b.click(); await page.waitForTimeout(300);
  same(await clip(), want, "snapshot");
});

// ---- slice 5: error toasts, the Pages viewer and a copy button inside a page ----
await check("an error toast carries a Copy button that copies the message", async () => {
  await page.goto("about:blank");
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForFunction(() => typeof toast === "function");
  await page.evaluate(() => toast("could not send: gpg failed to sign the data", true));
  const b = page.locator("#toast.error .toast-act");
  await b.waitFor({ state: "visible" });
  const h = await b.evaluate((e) => e.getBoundingClientRect().height);
  if (h < 35.5) throw new Error("tap target is " + h + "px high, want 36");
  await page.waitForTimeout(3200); // longer than a plain toast
  if (!(await b.isVisible())) throw new Error("the error toast was gone after 3.2 s");
  await sentinel(); await b.click(); await page.waitForTimeout(300);
  same(await clip(), "could not send: gpg failed to sign the data", "toast text, without the button label");
  same((await b.innerText()).trim(), "Copied", "label");
  await page.evaluate(() => toast("saved"));
  same(await page.locator("#toast .toast-act").count(), 0, "a plain toast has no button");
});

const PG = `<!doctype html><html><head><meta charset="utf-8"><title>Copy page</title></head><body>
<button type="button" id="b" style="min-height:44px">Copy</button><pre id="p">go test ./...</pre>
<script>
function copy(text) {
  const old = () => { const t = document.createElement("textarea"); t.value = text; t.readOnly = true;
    t.style.cssText = "position:fixed;top:0;opacity:0;font-size:16px"; document.body.append(t);
    t.select(); t.setSelectionRange(0, text.length); let ok = false;
    try { ok = document.execCommand("copy"); } catch {} t.remove(); return ok; };
  return (navigator.clipboard ? navigator.clipboard.writeText(text).then(() => true, old) : Promise.resolve(old()));
}
document.getElementById("b").onclick = () => copy(document.getElementById("p").textContent).then((ok) => { document.title = ok ? "copied" : "failed"; document.getElementById("b").textContent = ok ? "Copied" : "Failed"; });
</script></body></html>`;
const pub = await post("/api/pages", { title: "Copy page " + Date.now(), body: PG, kind: "html" });
const pgid = (pub.page || pub).id;
await check("pages: Copy link uses the shared helper, and a page's own copy button works in the sandbox", async () => {
  if (!pgid) throw new Error("publish: " + JSON.stringify(pub));
  await page.goto("about:blank");
  await page.goto(base + "/#/pages/" + pgid, { waitUntil: "load" });
  const cl = page.locator("#pgs-copy");
  await cl.waitFor({ timeout: 8000 });
  same(await page.locator("iframe.pgs-frame").getAttribute("allow"), "clipboard-write", "frame allow");
  await sentinel(); await cl.click(); await page.waitForTimeout(400);
  const link = await clip();
  if (!link.includes("/p/" + pgid)) throw new Error("link copied: " + JSON.stringify(link));
  const fr = page.frameLocator("iframe.pgs-frame");
  await fr.locator("#b").click();
  await fr.locator("#b").filter({ hasText: "Copied" }).waitFor({ timeout: 4000 });
  same(await clip(), "go test ./...", "the page's own copy");
});
await fetch(base + "/api/pages/" + pgid, { method: "DELETE" }).catch(() => {});

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
await dcheck("the hub-check block copies its lines", async (pg) => {
  await pg.route(/\/api\/ledger$/, async (r) => {
    const j = await (await r.fetch()).json();
    j.entries = [...(j.entries || []), { id: "cpfx", host: "uitest", repo_name: "comebye", branch: "fix/rail-dot", agent_name: "fix-rail-dot", status: "finished",
      created_at: new Date(Date.now() - 3600e3).toISOString(), finished_at: new Date(Date.now() - 600e3).toISOString(), final_report: "Pushed.", live: { present: false } }];
    r.fulfill({ json: j });
  });
  await pg.route(/\/api\/ledger\/cpfx\/hubcheck$/, (r) => r.fulfill({ json: { id: "cpfx", contradictions: 1, items: [
    { kind: "pushed", state: "bad", text: "the report says pushed, but 2 commit(s) are on no remote" },
    { kind: "ci", state: "ok", text: "CI is green for the head 9f3c2a1d7e4b" }] } }));
  await pg.goto("about:blank");
  await pg.goto(base + "/#/status", { waitUntil: "load" });
  await pg.locator('#ps-body details[data-id="cpfx"] summary').click();
  const b = pg.locator('#ps-body details[data-id="cpfx"] .hubchk .cpb');
  await b.waitFor();
  await pg.evaluate(() => navigator.clipboard.writeText("sentinel"));
  await b.click(); await pg.waitForTimeout(300);
  const got = await dclip();
  if (!/1 claim in the report contradicts what the hub found\n/.test(got) || !/the report says pushed, but 2 commit\(s\) are on no remote\n/.test(got) || !/CI is green/.test(got) || /Copy|[✓✗]/.test(got)) throw new Error("hub check text: " + JSON.stringify(got));
  await pg.unroute(/\/api\/ledger$/); await pg.unroute(/\/api\/ledger\/cpfx\/hubcheck$/);
});

await dcheck("a Status report has Copy report that copies its Markdown", async (pg) => {
  await pg.route(/\/api\/ledger$/, async (r) => {
    const j = await (await r.fetch()).json();
    j.entries = [...(j.entries || []), { id: "cpfx", host: "uitest", repo_name: "comebye", branch: "fix/rail-dot", agent_name: "fix-rail-dot", status: "finished",
      created_at: new Date(Date.now() - 3600e3).toISOString(), finished_at: new Date(Date.now() - 600e3).toISOString(), final_report: REPORT, live: { present: false } }];
    r.fulfill({ json: j });
  });
  await pg.goto("about:blank");
  await pg.goto(base + "/#/status", { waitUntil: "load" });
  const sm = pg.locator('#ps-body details[data-id="cpfx"] summary');
  await sm.waitFor();
  if (!(await pg.locator('#ps-body details[data-id="cpfx"]').evaluate((d) => d.open))) await sm.click();
  const b = pg.locator('#ps-body details[data-id="cpfx"] .cpb-rep');
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
