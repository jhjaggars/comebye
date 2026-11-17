// A chat helper in the app (issue #22, slice 4): the Agents list marks it with a `chat` chip, its conversation
// starts with a purple strip "Chat about <Page title> · Open page" that links back to the Page, and a review
// to-do about a Page has a "Chat about this" button that starts the same chat the viewer's button does. The hub
// marks the agent from its roles (the test hub takes the role through POST /api/roles); the chat route itself is
// answered by the script (its tests are in internal/server/chat_test.go).
//
//   node chatstrip.mjs BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("chatstrip.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "chatstrip-ui-"));
const results = [];
const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w5:p1" };
const J = { "Content-Type": "application/json" };
const publish = (key, title) => {
  const f = path.join(work, key + ".html");
  fs.writeFileSync(f, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>${title}</title></head><body style="font:16px/1.5 sans-serif;margin:16px"><h1>${title}</h1><p>Two seconds of slack.</p></body></html>`);
  return execFileSync(bin, ["publish", f, "--key", key + "-" + Date.now(), "--title", title], { env, encoding: "utf8" }).match(/pg_[0-9a-f]{10}/)[0];
};
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const TITLE = "Garage failover drill: what to check before the cutover";
const pageId = publish("uitest-chatstrip", TITLE);
const { agents } = await (await fetch(base + "/api/agents")).json();
const chatAgent = agents.find((a) => a.name === "alpha"), plain = agents.find((a) => a.name === "bravo");
if (!chatAgent || !plain) throw new Error("fixture agents missing");
const setRole = (role) => fetch(base + "/api/roles", { method: "POST", headers: J, body: JSON.stringify({ agent_id: chatAgent.id, role, page: pageId }) }).then((r) => r.json());
await setRole("chat");
const marked = (await (await fetch(base + "/api/agents")).json()).agents.find((a) => a.id === chatAgent.id);
if (!marked.chat || marked.chat.page !== pageId || marked.chat.title !== TITLE) throw new Error("the agents API did not carry the chat marker: " + JSON.stringify(marked.chat));

try {
  for (const width of [1280, 390]) {
    const tag = `${width} (${dark ? "dark" : "light"})`;
    const { browser, page } = await launch({ width, height: width > 500 ? 800 : 844, dark, bypassCSP: false });
    const errs = [];
    page.on("pageerror", (e) => errs.push(String(e)));
    const posts = [];
    await page.route(/\/api\/pages\/[^/]+\/chat$/, async (r) => {
      posts.push({ url: new URL(r.request().url()).pathname, method: r.request().method(), body: r.request().postData() });
      r.fulfill({ json: { agent: { pane_id: chatAgent.pane_id, name: chatAgent.name }, name: chatAgent.name, page: pageId, version: 1 } });
    });
    await page.route(/\/api\/pages\/[^/]+\/chat-options$/, (r) => r.fulfill({ json: { ok: true, kind: "claude" } })); // the test hub's default kind has no read-only mode
    try {
      await page.goto(base + "/", { waitUntil: "load" });
      const row = (id) => page.locator(`#agents .agent[data-id="${id}"], #p-agents .agent[data-id="${id}"]`).first();
      await page.waitForSelector(`.agent[data-id="${chatAgent.id}"]`, { state: "attached", timeout: 15000 });

      await check(`chat chip at ${tag}: the Agents list marks the helper and no other agent`, async () => {
        const chips = await page.locator(`.agent[data-id="${chatAgent.id}"] .chat-chip`).allTextContents();
        if (chips.length !== 1 || chips[0].trim() !== "chat") throw new Error("chips on the helper: " + JSON.stringify(chips));
        if (await page.locator(`.agent[data-id="${plain.id}"] .chat-chip`).count()) throw new Error("a worker has the chat chip");
        const all = await page.locator(".chat-chip").count();
        if (all !== 1) throw new Error("chat chips in the list: " + all);
        const g = await page.evaluate(() => ({ scroll: document.documentElement.scrollWidth > innerWidth }));
        if (g.scroll) throw new Error("the list scrolls sideways");
        await row(chatAgent.id).scrollIntoViewIfNeeded();
        await page.screenshot({ path: `${out}/chatstrip-list-${width}-${dark ? "dark" : "light"}.png` });
      });

      await check(`chat strip at ${tag}: the conversation starts with "Chat about <title> · Open page" and the link goes to the Page`, async () => {
        await row(chatAgent.id).click();
        await page.waitForSelector("#d-chatstrip:not([hidden])", { timeout: 8000 });
        const g = await page.evaluate(() => {
          const s = document.getElementById("d-chatstrip"), r = s.getBoundingClientRect(), c = document.getElementById("chat").getBoundingClientRect();
          const a = s.querySelector(".cs-open"), ab = a.getBoundingClientRect(), bg = getComputedStyle(s).backgroundColor;
          return { text: s.innerText.replace(/\s+/g, " ").trim(), href: a.getAttribute("href"), top: r.top, chatTop: c.top, w: r.width, vw: innerWidth, linkH: ab.height, linkRight: ab.right, right: r.right, bg,
            scroll: document.documentElement.scrollWidth > innerWidth };
        });
        if (!/^Chat about .+ · Open page$/.test(g.text)) throw new Error("strip text: " + g.text);
        if (!g.text.includes("Garage failover drill")) throw new Error("the Page title is not in the strip: " + g.text);
        if (g.href !== "#/pages/" + pageId) throw new Error("link: " + g.href);
        if (!(g.top < g.chatTop)) throw new Error(`the strip is not above the conversation: ${g.top} vs ${g.chatTop}`);
        if (g.linkH < 32 || g.linkRight > g.right + 0.5 || g.scroll) throw new Error("link box or overflow: " + JSON.stringify(g));
        if (g.bg === "rgba(0, 0, 0, 0)") throw new Error("the strip has no background");
        await page.screenshot({ path: `${out}/chatstrip-strip-${width}-${dark ? "dark" : "light"}.png` });
        await page.locator("#d-chatstrip .cs-open").click();
        await page.waitForFunction((id) => location.hash === "#/pages/" + id, pageId, { timeout: 8000 });
      });

      await check(`chat strip at ${tag}: a worker's conversation has no strip`, async () => {
        await page.goto(base + "/", { waitUntil: "load" });
        await page.waitForSelector(`.agent[data-id="${plain.id}"]`, { state: "attached", timeout: 15000 });
        await row(plain.id).click();
        await page.waitForSelector("#detail", { state: "visible" });
        await sleep(400);
        if (!(await page.locator("#d-chatstrip").isHidden())) throw new Error("the strip shows on a worker");
      });

      await check(`review button at ${tag}: "Chat about this" is the third button, starts the Page's chat once and the answer is unchanged`, async () => {
        const mk = await (await fetch(base + "/api/todos", { method: "POST", headers: J, body: JSON.stringify({ question: `Review: failover drill ${width}`, kind: "review", no_wait: true, context: "Open the Page.", page: { id: pageId } }) })).json();
        const id = mk.todo.id;
        try {
          const LIST = width > 500 ? "#p-todo" : "#todo";
          if (width > 500) { await page.goto(base + "/#/todo", { waitUntil: "load" }); await page.waitForSelector("#p-todo", { state: "visible" }); }
          else {
            await page.goto(base + "/", { waitUntil: "load" });
            await page.waitForSelector("#todo .td-toggle", { state: "visible", timeout: 15000 });
            if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
          }
          await page.evaluate(async () => { todos.loadedAt = 0; await loadTodos(); renderTodos(true); }); // a hash change alone does not reload the list
          await page.waitForSelector(`${LIST} .td-item[data-id="${id}"] .td-row`, { state: "visible" });
          await page.locator(`${LIST} .td-item[data-id="${id}"] .td-row`).click();
          await page.waitForSelector(`#rs .td-item.open[data-id="${id}"] .rv-bar`, { state: "attached" });
          if (width < 500) await page.locator('#rs [data-rs-tab="decide"]').click();
          const btn = page.locator(`#rs .td-item[data-id="${id}"] .rv-chat`);
          await btn.waitFor({ state: "visible" });
          const g = await page.evaluate((sel) => [...document.querySelectorAll(sel + " .rv-btns button")].map((b) => ({ t: b.innerText.trim(), h: b.getBoundingClientRect().height, w: b.getBoundingClientRect().width })), `#rs .td-item[data-id="${id}"]`);
          if (g.length !== 3 || !/Looks good/.test(g[0].t) || !/Needs changes/.test(g[1].t) || g[2].t !== "💬 Chat about this") throw new Error("buttons: " + JSON.stringify(g));
          if (g.some((b) => b.h < 43.5)) throw new Error("a button is under 44px: " + JSON.stringify(g));
          await page.screenshot({ path: `${out}/chatstrip-review-${width}-${dark ? "dark" : "light"}.png` });
          posts.length = 0;
          await btn.click();
          await page.waitForTimeout(600);
          if (posts.length !== 1 || posts[0].url !== `/api/pages/${pageId}/chat` || posts[0].method !== "POST") throw new Error("posts: " + JSON.stringify(posts));
          const t = (await fetch(base + "/api/todos/" + id).then((r) => r.json())).todo;
          if (t.state !== "open") throw new Error("starting a chat changed the item: " + t.state);
          // The verdict still works.
          await page.goto(base + (width > 500 ? "/#/todo" : "/"), { waitUntil: "load" });
          await page.evaluate(async () => { todos.loadedAt = 0; await loadTodos(); renderTodos(true); });
          if (width < 500) { if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click(); }
          await page.locator(`${LIST} .td-item[data-id="${id}"] .td-row`).click();
          if (width < 500) await page.locator('#rs [data-rs-tab="decide"]').click();
          await page.locator(`#rs .td-item[data-id="${id}"] .rv-good`).click();
          for (const end = Date.now() + 8000; Date.now() < end; await sleep(150)) { if ((await fetch(base + "/api/todos/" + id).then((r) => r.json())).todo.state === "done") break; }
          const after = (await fetch(base + "/api/todos/" + id).then((r) => r.json())).todo;
          if (after.state !== "done" || !after.review || !after.review.verdict) throw new Error("Looks good did not close the review: " + JSON.stringify(after.review));
        } finally {
          await fetch(base + `/api/todos/${id}/drop`, { method: "POST", headers: J, body: "{}" }).catch(() => {});
        }
      });
      await check(`review button at ${tag}: for a kind with no read-only mode it is greyed with the reason, and a tap starts nothing`, async () => {
        const { browser: b2, page: p2 } = await launch({ width, height: width > 500 ? 800 : 844, dark, bypassCSP: false }); // a fresh load: the gate is kept per load
        p2.on("request", (r) => { if (/\/chat$/.test(r.url()) && r.method() === "POST") posts.push({ url: r.url() }); });
        const offPage = publish("uitest-chatstrip-off", "A page that cannot be chatted about " + width); // the gate is asked once per Page and kept
        const REASON = "pi has no read-only mode, so it cannot chat about a page.";
        const off = (r) => r.fulfill({ json: { ok: false, reason: REASON } });
        await p2.route(/\/api\/pages\/[^/]+\/chat-options$/, off);
        const mk = await (await fetch(base + "/api/todos", { method: "POST", headers: J, body: JSON.stringify({ question: `Review: pi page ${width}`, kind: "review", no_wait: true, context: "Open the Page.", page: { id: offPage } }) })).json();
        const id = mk.todo.id;
        try {
          const LIST = width > 500 ? "#p-todo" : "#todo";
          await p2.goto(base + (width > 500 ? "/#/todo" : "/"), { waitUntil: "load" });
          if (width < 500) { await p2.waitForSelector("#todo .td-toggle", { state: "visible", timeout: 15000 }); if ((await p2.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await p2.locator("#todo .td-toggle").click(); }
          await p2.waitForSelector(`${LIST} .td-item[data-id="${id}"] .td-row`, { state: "visible" });
          await p2.locator(`${LIST} .td-item[data-id="${id}"] .td-row`).click();
          if (width < 500) await p2.locator('#rs [data-rs-tab="decide"]').click();
          const btn = p2.locator(`#rs .td-item[data-id="${id}"] .rv-chat`);
          await btn.waitFor({ state: "visible" });
          await p2.locator(`#rs .td-item[data-id="${id}"] .rv-chat[aria-disabled="true"]`).waitFor({ timeout: 5000 });
          if ((await btn.getAttribute("title")) !== REASON) throw new Error("title: " + (await btn.getAttribute("title")));
          await p2.screenshot({ path: `${out}/chatstrip-review-off-${width}-${dark ? "dark" : "light"}.png` });
          posts.length = 0;
          await btn.click({ force: true });
          await p2.waitForTimeout(500);
          if (posts.length) throw new Error("a tap posted: " + JSON.stringify(posts));
          if (!(await p2.locator("#toast").textContent()).includes("no read-only mode")) throw new Error("toast: " + (await p2.locator("#toast").textContent()));
        } finally {
          await b2.close().catch(() => {});
          await fetch(base + `/api/todos/${id}/drop`, { method: "POST", headers: J, body: "{}" }).catch(() => {});
        }
      });
      if (errs.length) throw new Error("page errors: " + errs.join("; "));
    } catch (e) {
      results.push({ name: `chat strip at ${tag}: ${String(e.message).split("\n")[0]}`, ok: false }); console.log("FAIL " + tag + " " + e.message);
    } finally {
      await browser.close();
    }
  }
} finally {
  await setRole("").catch(() => {});
}
const bad = results.filter((r) => !r.ok).length;
console.log(`\n${results.length - bad}/${results.length} chat strip checks passed`);
process.exit(bad ? 1 : 0);
