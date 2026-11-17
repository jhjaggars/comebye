// "Chat about this" in the Page viewer (issue #22, slice 2): the bar button and the top item
// in the ⋯ menu start the read-only helper (POST /api/pages/{id}/chat) and open its conversation.
// The route is answered by the script (its own tests are in internal/server/chat_test.go): the
// check is what the app sends, shows and opens.
//
//   node pagechat.mjs BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("pagechat.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "pagechat-ui-"));
const results = [];
const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w5:p1" };
const cli = (args) => execFileSync(bin, args, { env, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false, err: e.message }); console.log("FAIL " + name + "\n     " + e.message.split("\n").join("\n     ")); }
}

const f = path.join(work, "p.html");
fs.writeFileSync(f, `<!doctype html><html><head><meta charset="utf-8"><title>Failover drill</title><meta name="viewport" content="width=device-width,initial-scale=1"></head><body style="font:16px/1.5 sans-serif;margin:16px"><h1>Failover drill</h1><p>Two seconds of slack.</p></body></html>`);
const stamp = Date.now();
const id = cli(["publish", f, "--key", `uitest-pagechat-${stamp}`]).match(/pg_[0-9a-f]{10}/)[0];
const agents = await (await fetch(base + "/api/agents")).json();
const target = (agents.agents || agents).find((a) => a.pane_id);
if (!target) throw new Error("no agent in the fixtures to open");

for (const width of [1280, 390]) {
  const tag = `${width} (${dark ? "dark" : "light"})`;
  const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark, bypassCSP: false });
  const posts = [];
  let mode = "ok", hold = null;
  await page.route(/\/api\/pages\/[^/]+\/chat$/, async (r) => {
    posts.push({ url: new URL(r.request().url()).pathname, method: r.request().method(), site: r.request().headers()["sec-fetch-site"], body: r.request().postData() });
    if (hold) await hold;
    if (mode === "no_host") return r.fulfill({ status: 409, json: { code: "chat_no_host", message: "no chat can start: nothing starts agents" } });
    if (mode === "broken") return r.fulfill({ status: 502, json: { code: "unknown_method", message: "the host broke" } });
    if (mode === "rate") return r.fulfill({ status: 429, json: { code: "chat_rate", message: "starting chats too fast: wait a minute" } });
    const body = { agent: { pane_id: target.pane_id, name: target.name }, host: target.host && target.host !== "local" ? target.host : undefined, name: target.name, page: id, version: 1 };
    if (mode === "note") body.prompt_note = "the helper started at a dialog (answer it in the app)";
    if (mode === "open") Object.assign(body, { reused: "open" });
    if (mode === "resumed") Object.assign(body, { reused: "resumed", page_note: "Page is now v2: the chat was told to read it again." });
    if (mode === "replaced") Object.assign(body, { replaced: "chat-old" });
    if (mode === "away") return r.fulfill({ status: 409, json: { code: "chat_host_away", message: "desk is not answering" } });
    r.fulfill({ json: body });
  });
  const toastText = async () => (await page.locator("#toast").textContent()).trim();
  const open = async () => {
    posts.length = 0;
    await page.goto(base + "/#/pages/" + id, { waitUntil: "load" });
    await page.waitForSelector("#pgs-chat", { timeout: 8000 });
    await page.waitForTimeout(500);
  };
  const box = async (sel) => (await page.locator(sel).boundingBox());

  await check(`pagechat: the bar button and the menu item show at ${tag}`, async () => {
    await open();
    const b = await box("#pgs-chat");
    if (!b || b.height < 44 || b.width < 44) throw new Error("button box " + JSON.stringify(b));
    const word = await page.locator("#pgs-chat .pgs-chatl").isVisible();
    if (word !== (width > 500)) throw new Error(`the word "Chat" visible=${word}`);
    await page.screenshot({ path: `${out}/pagechat-bar-${width}.png` });
    await page.click("#pgs-menub");
    await page.waitForSelector("#pgs-menu:not([hidden])");
    await page.waitForTimeout(500);
    const items = await page.locator("#pgs-menu [role=menuitem]:visible").allTextContents();
    if (items[0].trim() !== "Chat about this") throw new Error("menu items: " + items.join(" | "));
    const m = await box("#pgs-chatmi");
    if (m.height < (width > 500 ? 32 : 44)) throw new Error("menu item height " + m.height);
    await page.screenshot({ path: `${out}/pagechat-menu-${width}.png` });
  });

  await check(`pagechat: a tap posts to the route once, shows the toast and opens the conversation at ${tag}`, async () => {
    await open();
    let release; hold = new Promise((r) => (release = r));
    await page.click("#pgs-chat");
    await page.waitForTimeout(300);
    if (!/Opening the chat/.test(await toastText())) throw new Error("toast while starting: " + (await toastText()));
    await page.screenshot({ path: `${out}/pagechat-toast-${width}.png` });
    await page.click("#pgs-chat", { force: true }); // a second tap while it starts
    await page.waitForTimeout(200);
    release(); hold = null;
    await page.waitForFunction(() => /^#\/(agent|a)\b|detail|chat/i.test(location.hash) || document.body.classList.contains("in-detail") || !document.getElementById("detail")?.hidden, null, { timeout: 8000 }).catch(() => {});
    await page.waitForTimeout(800);
    if (posts.length !== 1) throw new Error("posts: " + JSON.stringify(posts));
    if (posts[0].url !== `/api/pages/${id}/chat` || posts[0].method !== "POST") throw new Error("post " + JSON.stringify(posts[0]));
    const opened = await page.evaluate(() => !!(document.getElementById("detail") && !document.getElementById("detail").hidden && document.getElementById("detail").offsetParent));
    if (!opened) throw new Error("the conversation did not open; toast: " + (await toastText()));
    await page.screenshot({ path: `${out}/pagechat-opened-${width}.png` });
  });

  await check(`pagechat: the menu item posts too, and a waiting prompt is explained at ${tag}`, async () => {
    mode = "note";
    await open();
    await page.click("#pgs-menub");
    await page.waitForSelector("#pgs-menu:not([hidden])");
    await page.click("#pgs-chatmi");
    await page.waitForTimeout(500);
    if (posts.length !== 1) throw new Error("posts: " + posts.length);
    const t = await toastText();
    if (!/waiting at a prompt/.test(t) || !/answer/i.test(t)) throw new Error("toast: " + t);
    if (!(await page.locator("#pgs-menu").isHidden())) throw new Error("the menu stayed open");
  });

  await check(`pagechat: "Start a new chat…" is in the menu and posts new:true; a plain tap posts none at ${tag}`, async () => {
    mode = "ok";
    await open();
    await page.click("#pgs-menub");
    await page.waitForSelector("#pgs-menu:not([hidden])");
    const items = (await page.locator("#pgs-menu [role=menuitem]:visible").allTextContents()).map((s) => s.trim());
    if (items[0] !== "Chat about this" || items[1] !== "Start a new chat…") throw new Error("menu items: " + items.join(" | "));
    const m = await box("#pgs-chatnew");
    if (m.height < (width > 500 ? 32 : 44)) throw new Error("menu item height " + m.height);
    await page.screenshot({ path: `${out}/pagechat-newitem-${width}.png` });
    await page.click("#pgs-chatnew");
    await page.waitForTimeout(500);
    if (posts.length !== 1 || JSON.parse(posts[0].body).new !== true) throw new Error("posts: " + JSON.stringify(posts));
    if (!(await page.locator("#pgs-menu").isHidden())) throw new Error("the menu stayed open");
    await open();
    await page.click("#pgs-chat");
    await page.waitForTimeout(500);
    if (posts.length !== 1 || JSON.parse(posts[0].body).new) throw new Error("a plain tap sent new: " + JSON.stringify(posts));
  });

  for (const [m, re] of [["open", /^Opened /], ["resumed", /^Resumed .*Page is now v2/], ["replaced", /earlier chat was closed/]]) {
    await check(`pagechat: a reused chat says so (${m}) and opens at ${tag}`, async () => {
      mode = m;
      await open();
      await page.click("#pgs-chat");
      await page.waitForTimeout(500);
      const t = await toastText();
      if (!re.test(t)) throw new Error("toast: " + t);
    });
  }

  for (const [m, re] of [["away", /not answering/], ["no_host", /No host can start a chat/], ["rate", /wait a minute/i], ["broken", /^Could not start a chat: .*host broke/]]) {
    await check(`pagechat: the ${m} refusal is a plain sentence, and the page stays at ${tag}`, async () => {
      mode = m;
      await open();
      await page.click("#pgs-chat");
      await page.waitForTimeout(500);
      const t = await toastText();
      if (!re.test(t)) throw new Error("toast: " + t);
      if (!/#\/pages\//.test(page.url())) throw new Error("left the page: " + page.url());
      await page.click("#pgs-chat"); // not stuck busy
      await page.waitForTimeout(300);
      if (posts.length !== 2) throw new Error("a retry did not post: " + posts.length);
      if (m === "no_host") await page.screenshot({ path: `${out}/pagechat-refused-${width}.png` });
    });
  }
  await browser.close();
}

fs.rmSync(work, { recursive: true, force: true });
const failed = results.filter((r) => !r.ok);
console.log(`pagechat: ${results.length - failed.length}/${results.length} passed`);
process.exit(failed.length ? 1 : 0);
