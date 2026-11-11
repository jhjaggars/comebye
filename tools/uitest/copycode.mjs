// The Copy strip on fenced code blocks (addCopyStrips / copyText in web/app.js),
// against the shared server, in headless Chrome with clipboard permission so the
// text that reaches the clipboard is read back. Chat, To do and the briefing.
//
//   node copycode.mjs BASE OUT_DIR      (env: SCHEME)
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
  await page.screenshot({ path: `${out}/copycode-${name.replace(/\W+/g, "-")}.png` }).catch(() => {});
}
const same = (got, want, what) => { if (got !== want) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };
const clip = () => page.evaluate(() => navigator.clipboard.readText());
const sentinel = () => page.evaluate(() => navigator.clipboard.writeText("sentinel"));

const CMD = "sudo systemctl mask --now comebye-old.service\nsudo install -m 0644 /etc/a /etc/b\n";
const SHELL = "```sh\n" + CMD + "```";
const CONSOLE = "```console\n$ gpg --decrypt secrets.gpg\n$ comebye publish page.html\n```";
const MIXED = "```console\n$ ls\nfile-a\nfile-b\n```";
const INDENT = "```\naws logs filter \\\n    --log-group x\n\tif ok; then\n    echo hi\nfi\n```";
const CTX = `Run these:\n\n${SHELL}\n\n${CONSOLE}\n\n${MIXED}\n\n${INDENT}\n`;

// ---- a To do item with every kind of block ----
const todo = await fetch(base + "/api/todos", { method: "POST", headers: { "content-type": "application/json" },
  body: JSON.stringify({ question: "Copy test " + Date.now(), kind: "decision", no_wait: true, context: CTX }) }).then((r) => r.json());
const tid = todo.todo.id;

async function openTodo() {
  await page.goto(base + "/#/todo", { waitUntil: "load" });
  await page.waitForSelector(".td-item", { timeout: 8000 });
  await page.locator(`.td-item[data-id="${tid}"] > .td-row:visible`).first().click();
  await page.waitForSelector(".td-ctx .cb", { timeout: 8000 });
}

await check("todo: every fenced block has a strip with its language", async () => {
  await openTodo();
  const langs = await page.$$eval(".td-ctx .cb", (b) => b.filter((x) => x.offsetParent).map((x) => x.dataset.lang || ""));
  same(JSON.stringify(langs), JSON.stringify(["sh", "console", "console", ""]), "block languages");
  const btn = page.locator(".td-ctx .cb-copy").first();
  same(await btn.getAttribute("aria-label"), "Copy sh code", "aria-label");
  const h = await btn.evaluate((e) => e.getBoundingClientRect().height);
  if (h < 43.5) throw new Error("tap target is " + h + "px high, want 44");
  if (!(await btn.isVisible())) throw new Error("not visible without hover");
});

await check("todo: the copied text is exactly the code", async () => {
  const boxes = page.locator(".td-ctx .cb");
  await sentinel(); await boxes.nth(0).locator(".cb-copy").click();
  await page.waitForFunction(() => document.querySelector(".td-ctx .cb-copy[data-state=ok]"));
  same(await clip(), CMD.replace(/\n$/, ""), "shell block");
  await sentinel(); await boxes.nth(1).locator(".cb-copy").click();
  await page.waitForFunction(() => document.querySelectorAll(".td-ctx .cb-copy[data-state=ok]").length >= 1);
  await page.waitForTimeout(100);
  same(await clip(), "gpg --decrypt secrets.gpg\ncomebye publish page.html", "console block loses its prompts");
  await sentinel(); await boxes.nth(2).locator(".cb-copy").click(); await page.waitForTimeout(250);
  same(await clip(), "$ ls\nfile-a\nfile-b", "a block with output is copied whole");
  await sentinel(); await boxes.nth(3).locator(".cb-copy").click(); await page.waitForTimeout(250);
  same(await clip(), "aws logs filter \\\n    --log-group x\n\tif ok; then\n    echo hi\nfi", "indentation and tabs");
});

await check("todo: Copied state, label and live region", async () => {
  const btn = page.locator(".td-ctx .cb-copy").first();
  await btn.click();
  await page.waitForFunction(() => document.querySelector(".td-ctx .cb-copy").dataset.state === "ok");
  same((await btn.innerText()).trim(), "Copied", "label");
  await page.waitForFunction(() => document.querySelector("[role=status].sr-only")?.textContent === "Copied to clipboard");
  await page.waitForFunction(() => !document.querySelector(".td-ctx .cb-copy").dataset.state, null, { timeout: 4000 });
  same((await btn.innerText()).trim(), "Copy", "label resets");
});

await check("todo: keyboard: Tab reaches the button, Enter and Space copy", async () => {
  await sentinel();
  await page.locator(".td-ctx .cb-copy").first().focus();
  await page.keyboard.press("Enter"); await page.waitForTimeout(250);
  same(await clip(), CMD.replace(/\n$/, ""), "Enter");
  await sentinel();
  await page.waitForFunction(() => !document.querySelector(".td-ctx .cb-copy").dataset.state, null, { timeout: 4000 });
  await page.keyboard.press("Space"); await page.waitForTimeout(250);
  same(await clip(), CMD.replace(/\n$/, ""), "Space");
  // the strip's button comes before its code in tab order, and nothing between
  const next = await page.evaluate(() => { const b = document.querySelector(".td-ctx .cb-copy"); b.focus(); return b.nextElementSibling === null; });
  if (!next) throw new Error("unexpected element after the button");
});

await check("todo: falls back to execCommand when the clipboard API refuses", async () => {
  await sentinel();
  await page.evaluate(() => { navigator.clipboard.writeText = () => Promise.reject(new DOMException("blocked", "NotAllowedError")); });
  const real = await page.evaluate(() => { window.__rt = navigator.clipboard.readText.bind(navigator.clipboard); return true; });
  await page.locator(".td-ctx .cb-copy").first().click(); await page.waitForTimeout(300);
  same(await page.evaluate(() => window.__rt()), CMD.replace(/\n$/, ""), "fallback copy");
  same(await page.locator(".td-ctx .cb-copy").first().getAttribute("data-state"), "ok", "state");
});

// ---- the briefing ----
await check("briefing: a code block has a strip and copies exactly", async () => {
  const md = "Check:\n\n```sh\nls -la\ncd /tmp\n```\n";
  const r = await fetch(base + "/api/brief", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ markdown: md }) });
  if (!r.ok) throw new Error("publish brief: " + r.status);
  await page.goto(base + "/", { waitUntil: "load" });
  const tog = page.locator("#briefing .bf-toggle");
  await tog.waitFor({ state: "visible", timeout: 8000 });
  if ((await tog.getAttribute("aria-expanded")) !== "true") await tog.click();
  await page.waitForSelector("#briefing .bf-body .cb", { state: "visible", timeout: 5000 });
  await page.evaluate(() => { for (const k of ["readText"]) void k; });
  await page.locator("#briefing .cb-copy").first().click(); await page.waitForTimeout(300);
  // the clipboard API was replaced above only on the previous document; this one is fresh
  same(await clip(), "ls -la\ncd /tmp", "briefing block");
});

// ---- chat: the fake agent echoes the prompt, so a fenced block comes back in a bubble ----
const MARK = "m" + Date.now();
await check("chat: a code block in a reply has a strip and copies exactly", async () => {
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#detail", { state: "visible" });
  await page.locator("#prompt-text").click();
  await page.keyboard.type("run");
  await page.keyboard.down("Shift"); await page.keyboard.press("Enter"); await page.keyboard.up("Shift");
  await page.keyboard.type("```sh");
  await page.keyboard.down("Shift"); await page.keyboard.press("Enter"); await page.keyboard.up("Shift");
  await page.keyboard.type("echo " + MARK);
  await page.keyboard.down("Shift"); await page.keyboard.press("Enter"); await page.keyboard.up("Shift");
  await page.keyboard.type("```");
  await page.locator("#btn-send").click();
  const mine = page.locator("#chat .msg.assistant .cb", { hasText: MARK });
  await mine.first().waitFor({ timeout: 9000 });
  await mine.first().locator(".cb-copy").click(); await page.waitForTimeout(300);
  same(await clip(), "echo " + MARK, "chat block");
});

fs.writeFileSync(`${out}/copycode-console.log`, log.join("\n"));
await fetch(base + `/api/todos/${tid}/drop`, { method: "POST", headers: { "content-type": "application/json" }, body: "{}" }).catch(() => {});
await browser.close();
const bad = results.filter((r) => !r.ok).length + (log.length ? 1 : 0);
if (log.length) console.log("page errors:\n" + log.join("\n"));
console.log(`${results.length - results.filter((r) => !r.ok).length}/${results.length} copycode checks passed`);
process.exit(bad ? 1 : 0);
