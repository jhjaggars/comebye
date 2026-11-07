#!/usr/bin/env node
// Regenerates the Comebye feature screenshots, the walkthrough video and the
// gallery page from the demo page, so they can be refreshed after UI changes.
// All content comes from the demo's fake backend. See README.md.
//
//   node tools/demo/shots.mjs OUT_DIR [--only=shots|video|gallery]
//
// Needs python3, google-chrome (CHROME=/path to override), ffmpeg, magick, and
// tools/uitest/node_modules (npm ci there). Writes OUT_DIR/{dark,light,video}/,
// OUT_DIR/12-light-and-dark.png and OUT_DIR/gallery.html.
import { createRequire } from "module";
import { execFileSync } from "child_process";
import { fileURLToPath } from "url";
import fs from "fs"; import http from "http"; import path from "path"; import os from "os";

const [, , outArg, flag] = process.argv;
if (!outArg) { console.error("usage: shots.mjs OUT_DIR [--only=shots|video|gallery]"); process.exit(2); }
const here = path.dirname(fileURLToPath(import.meta.url));
const out = path.resolve(outArg);
const only = flag ? flag.replace("--only=", "") : "";
const { chromium } = createRequire(path.join(here, "../uitest/"))("playwright-core");
const CHROME = process.env.CHROME || "/usr/bin/google-chrome";
const TOUR_URL = process.env.TOUR_URL || "https://claude.ai/artifact/UTWy3VcH9Vyriu5FcdutEB";
const REPO_URL = "https://github.com/jhjaggars/comebye";

// Every shot: file name, title and one-line caption, in gallery order.
const DESKTOP = [
  ["01-needs-you", "Agents across projects, and who needs you", "Every agent on every host in one list. The two stuck on a question sit at the top, and the open one asks which store to use."],
  ["02-conversation", "An agent's conversation", "A worker's session as a chat, with rendered Markdown and a tool card for each step. This one is open on a failing test."],
  ["03-prompting", "Prompting an agent", "Your message goes into the agent's own terminal pane, and you can watch it start work and line up the next message."],
  ["04-coordinator", "A coordinator running a feature", "quill splits one request into a plan of three workers. The plan's progress sits in the sidebar, fed by the hub's ledger."],
  ["05-handoff", "Handing work to a worker", "Pick a host, a repository and a branch. The worker gets its own worktree, and merging stays with you."],
  ["06-status", "Status", "Generated from the ledger and the to-do list: what's blocked, waiting on you, running and recently finished, by project."],
  ["07-todo", "To do", "Decisions only you can make, with the context and links an agent left. Your answer goes back through the coordinator."],
  ["08-search", "Searching past sessions", "Search every archived session across hosts, filtered by host, agent, project and where the words appear."],
  ["09-session", "Reading a past session", "The whole transcript opens at the match, with the handoff brief, tool calls and subagents."],
  ["10-ready-to-remove", "Ready to remove", "Nothing goes until you confirm. Each session is checked first, and unmerged work is listed before you can discard it."],
  ["11-hosts", "Several hosts, one list", "Host chips filter the list to one machine. An agent waiting on you stays visible whichever host you pick."],
];
const PHONE = [
  ["p1-list", "On your phone: the list", "The same list at 390px wide, with what needs you first."],
  ["p2-answer", "On your phone: answering", "A question from an agent becomes buttons you can tap."],
  ["p3-chat", "On your phone: the chat", "Follow a worker's session and prompt it from anywhere on your tailnet."],
];

const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const CLEAN = `#demo-foot,#demo-intro,#toast{display:none!important} #agents{padding-bottom:20px!important}`;
const row = (name) => `#agents .agent:has(.name:text-is("${name}"))`;

function buildPage() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "comebye-shots-"));
  const file = path.join(dir, "demo.html");
  execFileSync("python3", [path.join(here, "build.py"), file, "--wrap"], { stdio: "ignore" });
  return file;
}

async function open(browser, base, scheme, { phone = false, dsf = 2, video = false } = {}) {
  const ctx = await browser.newContext({
    viewport: phone ? { width: 390, height: 844 } : { width: 1440, height: 900 },
    deviceScaleFactor: dsf, colorScheme: scheme, isMobile: phone, hasTouch: phone,
    ...(phone ? { userAgent: "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Mobile Safari/537.36" } : {}),
  });
  const page = await ctx.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });
  await page.goto(base);
  await page.waitForSelector("#agents .agent");
  await page.evaluate(() => document.querySelector('#demo-intro [data-act="close"]').click());
  await page.addStyleTag({ content: CLEAN });
  if (!phone) await page.waitForSelector('#nav [data-page="todo"]');
  await wait(500); // the ledger and to-dos arrive just after the agents
  page.demoErrors = errors;
  return { ctx, page };
}

async function settle(page, { blur = true } = {}) {
  if (blur) await page.evaluate(() => document.activeElement && document.activeElement.blur && document.activeElement.blur());
  await page.mouse.move(1, 1);
  await page.evaluate(() => { const a = document.getElementById("agents"); if (a) a.scrollTop = 0; });
  await wait(450);
}

// Each scene puts the real app in one state by using it.
const SCENES = {
  async "01-needs-you"(p) { await p.click(row("rate-limit-store")); await p.waitForSelector("#answer button.opt"); await settle(p); },
  async "02-conversation"(p) {
    await p.click(row("retry-webhooks")); await p.waitForSelector("#chat .toolcard.err");
    await p.click("#chat .toolcard.err summary");
    await p.evaluate(() => { const c = document.getElementById("chat"); c.scrollTop = c.scrollHeight; });
    await settle(p);
  },
  async "03-prompting"(p) {
    await p.click(row("tidepool")); await p.waitForSelector("#chat .msg");
    await p.click("#prompt-text");
    await p.keyboard.type("Add the new v2 rate limit headers to the API reference, with an example 429.");
    await p.keyboard.press("Enter");
    // Shot right away: the agent has started, and the demo's canned reply is seconds off.
    await p.waitForSelector("#chat .msg.user:has-text('rate limit headers')");
    await p.waitForFunction(() => /working/.test(document.getElementById("d-status").textContent));
    await p.evaluate(() => { setPromptValue("Then link it from the 2.4 changelog"); updateSendButton(); document.getElementById("agents").scrollTop = 0; });
    await p.mouse.move(1, 1);
    await wait(120);
    if (/canned/i.test(await p.textContent("#chat"))) throw new Error("03-prompting: the canned reply got into the shot");
  },
  async "04-coordinator"(p) {
    await p.click(row("quill")); await p.waitForSelector("#chat .toolcard");
    await p.evaluate(() => { document.getElementById("chat").scrollTop = 0; });
    await settle(p);
  },
  async "05-handoff"(p) {
    await p.click("#btn-launch"); await p.waitForSelector("#launch:not([hidden])");
    await p.click("#ln-mode [data-mode=worker]");
    await p.waitForFunction(() => document.getElementById("hw-repo").options.length > 1);
    await p.selectOption("#hw-repo", "/home/dev/code/tidepool");
    await p.fill("#hw-branch", "rate-limit-dashboard");
    await p.fill("#hw-brief", "Add a per-key rate limit chart to the admin dashboard, using the limiter's counters. Finish with a report.");
    await settle(p);
  },
  async "06-status"(p) { await p.click('#nav [data-page="status"]'); await p.waitForSelector("#ps-body details.st-row");
    await p.locator("#ps-body details.st-row", { hasText: "csv-export" }).locator("summary").click();
    await p.waitForSelector("#ps-body details.st-row[open] .st-rep"); await settle(p); },
  async "07-todo"(p) { await p.click('#nav [data-page="todo"]');
    const it = p.locator("#pt-body .td-item", { hasText: "on by default" }); await it.locator(".td-row").click();
    await it.locator(".td-text").fill("On by default, at 600 a minute. Tell the partner with bulk imports first."); await settle(p); },
  async "08-search"(p) { await p.click('#nav [data-page="search"]'); await p.waitForSelector("#rc-results .rc-card");
    await p.fill("#rc-q", "backoff"); await p.waitForSelector("#rc-results mark"); await settle(p); },
  async "09-session"(p) { await p.click('#nav [data-page="search"]'); await p.waitForSelector("#rc-results .rc-card");
    await p.fill("#rc-q", "backoff"); await p.waitForSelector("#rc-results mark");
    await p.click("#rc-results .rc-card"); await p.waitForSelector("#rc-session .rc-stitle"); await settle(p); },
  async "10-ready-to-remove"(p) { await p.click('#nav [data-page="removals"]'); await p.waitForSelector("#rv-list .rv-item");
    await settle(p); },
  async "11-hosts"(p) { await p.click('#nav [data-page="status"]');
    await p.locator("#host-filter .hf-chip", { hasText: "buildbox" }).click(); await settle(p); },
  async "p1-list"(p) { await settle(p); },
  async "p2-answer"(p) { await p.click(row("rate-limit-store")); await p.waitForSelector("#answer button.opt"); await settle(p); },
  async "p3-chat"(p) { await p.click(row("retry-webhooks")); await p.waitForSelector("#chat .toolcard");
    await p.evaluate(() => { const c = document.getElementById("chat"); c.scrollTop = c.scrollHeight; }); await settle(p); },
};

async function makeShots(browser, base) {
  for (const scheme of ["dark", "light"]) {
    fs.mkdirSync(path.join(out, scheme), { recursive: true });
    for (const [name] of [...DESKTOP, ...PHONE]) {
      const phone = name.startsWith("p");
      const { ctx, page } = await open(browser, base, scheme, { phone });
      await SCENES[name](page);
      await page.screenshot({ path: path.join(out, scheme, name + ".png") });
      if (page.demoErrors.length) throw new Error(`${scheme}/${name}: ${page.demoErrors.join("; ")}`);
      await ctx.close();
      console.log(`shot ${scheme}/${name}`);
    }
  }
  // Light and dark: the same screen, side by side.
  execFileSync("magick", ["(", path.join(out, "dark/01-needs-you.png"), "-resize", "50%", ")", "(", "-size", "24x900", "xc:#7f8694", ")", "(", path.join(out, "light/01-needs-you.png"), "-resize", "50%", ")", "+append", "+repage", "-depth", "8", path.join(out, "12-light-and-dark.png")]);
}

/* ---------- the walkthrough ---------- */
const CURSOR = `
  const c = document.createElement("div"); c.id = "wt-cursor";
  c.style.cssText = "position:fixed;left:-40px;top:-40px;width:18px;height:18px;border-radius:50%;background:rgba(96,165,250,.55);border:2px solid #fff;box-shadow:0 1px 6px rgba(0,0,0,.6);z-index:99999;pointer-events:none;transform:translate(-50%,-50%)";
  document.body.append(c);
  addEventListener("mousemove", (e) => { c.style.left = e.clientX + "px"; c.style.top = e.clientY + "px"; }, true);
  const b = document.createElement("div"); b.id = "wt-caption";
  b.style.cssText = "position:fixed;left:50%;bottom:92px;transform:translateX(-50%);padding:9px 18px;border-radius:10px;background:rgba(10,12,18,.9);color:#fff;font:600 19px system-ui,sans-serif;z-index:99998;pointer-events:none;white-space:nowrap";
  document.body.append(b);
  window.__caption = (t) => { b.textContent = t; b.style.display = t ? "" : "none"; };
  window.__caption("");`;

async function makeVideo(browser, base) {
  const dir = path.join(out, "video"), frames = fs.mkdtempSync(path.join(os.tmpdir(), "comebye-frames-"));
  fs.mkdirSync(dir, { recursive: true });
  const { ctx, page } = await open(browser, base, "dark", { dsf: 1 });
  await page.evaluate(CURSOR);
  const cdp = await ctx.newCDPSession(page);
  const list = [];
  cdp.on("Page.screencastFrame", async (f) => {
    const file = path.join(frames, `f${String(list.length).padStart(5, "0")}.jpg`);
    fs.writeFileSync(file, Buffer.from(f.data, "base64"));
    list.push({ file, t: f.metadata.timestamp });
    await cdp.send("Page.screencastFrameAck", { sessionId: f.sessionId }).catch(() => {});
  });
  const caption = (t) => page.evaluate((t) => window.__caption(t), t);
  let mx = 700, my = 450;
  const glide = async (sel) => {
    const l = typeof sel === "string" ? page.locator(sel).first() : sel;
    await l.scrollIntoViewIfNeeded();
    const b = await l.boundingBox();
    const x = b.x + b.width / 2, y = b.y + b.height / 2, n = 16;
    for (let i = 1; i <= n; i++) { const k = i / n, e = k * k * (3 - 2 * k); await page.mouse.move(mx + (x - mx) * e, my + (y - my) * e); await wait(16); }
    mx = x; my = y;
    await wait(120);
  };
  const click = async (sel) => { await glide(sel); await page.mouse.down(); await page.mouse.up(); };

  await cdp.send("Page.startScreencast", { format: "jpeg", quality: 92, everyNthFrame: 1 });
  await page.mouse.move(mx, my);
  await caption("One list: every agent on every host");
  await wait(2000);
  await caption("Two agents need you. Open one and answer");
  await click(row("rate-limit-store"));
  await page.waitForSelector("#answer button.opt");
  await wait(800);
  await click(page.locator("#answer button.opt", { hasText: "Redis" }));
  await page.waitForFunction(() => document.querySelectorAll("#agents .needs-sec .agent").length === 1);
  await caption("The worker carries on and reports back");
  await wait(1700);
  await caption("Read any session as it happens");
  await click(row("retry-webhooks"));
  await page.waitForSelector("#chat .toolcard.err");
  await wait(300);
  await click("#chat .toolcard.err summary");
  await wait(1100);
  await caption("A coordinator splits the work into workers");
  await click(row("quill"));
  await page.waitForSelector("#chat .toolcard");
  await page.evaluate(() => { document.getElementById("chat").scrollTop = 0; });
  await wait(1500);
  await caption("Status is generated, so it's always current");
  await click('#nav [data-page="status"]');
  await wait(1500);
  await caption("Decisions wait for you in To do");
  await click('#nav [data-page="todo"]');
  const it = page.locator("#pt-body .td-item", { hasText: "on by default" });
  await click(it.locator(".td-row"));
  await wait(200);
  await click(it.locator(".td-text"));
  await page.keyboard.type("On by default, at 600 a minute.", { delay: 14 });
  await click(it.locator(".td-answer-btn"));
  await wait(700);
  await caption("Search every past session, on any host");
  await click('#nav [data-page="search"]');
  await page.waitForSelector("#rc-results .rc-card");
  await click("#rc-q");
  await page.keyboard.type("backoff", { delay: 30 });
  await page.waitForSelector("#rc-results mark");
  await wait(300);
  await click("#rc-results .rc-card");
  await page.waitForSelector("#rc-session .rc-stitle");
  await wait(1400);
  await click("#rc-back");
  await wait(120);
  if (await page.locator("#recall:not([hidden])").count()) await click("#rc-back");
  await caption("Clean up when you confirm, and never before");
  await click('#nav [data-page="removals"]');
  await page.waitForSelector("#rv-list .rv-item");
  await wait(2200);
  await cdp.send("Page.stopScreencast");
  if (page.demoErrors.length) throw new Error("walkthrough: " + page.demoErrors.join("; "));
  await ctx.close();

  // Frames arrive only when the screen changes: each one lasts until the next.
  let concat = "ffconcat version 1.0\n";
  for (let i = 0; i < list.length; i++) {
    const d = i + 1 < list.length ? Math.max(0.01, list[i + 1].t - list[i].t) : 0.5;
    concat += `file '${list[i].file}'\nduration ${d.toFixed(3)}\n`;
  }
  const cf = path.join(frames, "list.txt");
  fs.writeFileSync(cf, concat);
  const secs = list.at(-1).t - list[0].t;
  const ff = (...a) => execFileSync("ffmpeg", ["-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", cf, ...a]);
  ff("-vf", "fps=30,format=yuv420p", "-c:v", "libx264", "-crf", "20", "-preset", "slow", "-movflags", "+faststart", path.join(dir, "walkthrough.mp4"));
  ff("-vf", "fps=30,format=yuv420p", "-c:v", "libvpx-vp9", "-crf", "32", "-b:v", "0", "-row-mt", "1", path.join(dir, "walkthrough.webm"));
  ff("-vf", "fps=10,scale=960:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=128:stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle", path.join(dir, "walkthrough.gif"));
  execFileSync("ffmpeg", ["-y", "-loglevel", "error", "-ss", "5", "-i", path.join(dir, "walkthrough.mp4"), "-frames:v", "1", "-q:v", "3", path.join(dir, "poster.jpg")]);
  fs.rmSync(frames, { recursive: true, force: true });
  console.log(`walkthrough: ${secs.toFixed(1)} s, ${list.length} frames`);
}

/* ---------- the gallery page ---------- */
function makeGallery() {
  const esc = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;");
  const fig = (name, title, cap, cls = "") => `<figure class="${cls}"><button type="button" class="shot" data-name="${name}" aria-label="Enlarge: ${esc(title)}"><img src="dark/${name}.png" data-name="${name}" alt="${esc(title)}: ${esc(cap)}" loading="lazy" width="${cls ? 780 : 2880}" height="${cls ? 1688 : 1800}"></button><figcaption><b>${esc(title)}</b> ${esc(cap)}</figcaption></figure>`;
  const page = `<title>Comebye Screenshots</title>
<meta name="claude:viewport-fit-sides" content="cover">
<style>
/* A plain contact sheet on the app's own palette: the pictures are the design. */
:root { --bg:#f3f4f7; --panel:#ffffff; --panel2:#eceef3; --border:#d3d7e0; --text:#1a1d26; --muted:#596072; --accent:#1c58b8; --on-accent:#ffffff;
  --sans: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; }
@media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) { --bg:#101218; --panel:#181a20; --panel2:#1f222b; --border:#2a2e3a; --text:#e6e8ee; --muted:#8b91a0; --accent:#60a5fa; --on-accent:#0b0d11; color-scheme: dark } }
:root[data-theme="dark"] { --bg:#101218; --panel:#181a20; --panel2:#1f222b; --border:#2a2e3a; --text:#e6e8ee; --muted:#8b91a0; --accent:#60a5fa; --on-accent:#0b0d11; color-scheme: dark }
body { background: var(--bg); color: var(--text); font: 15px/1.5 var(--sans); }
main { max-width: 1240px; margin: 0 auto; padding-block: 32px 56px; padding-left: max(16px, env(safe-area-inset-left, 0px)); padding-right: max(16px, env(safe-area-inset-right, 0px)); display: grid; gap: 36px; }
h1 { font-size: 30px; line-height: 1.15; margin: 0 0 8px; font-weight: 700; text-wrap: balance; }
h2 { font-size: 18px; margin: 0 0 12px; font-weight: 650; }
p { margin: 0; max-width: 68ch; color: var(--muted); }
a { color: var(--accent); }
.bar { display: flex; flex-wrap: wrap; gap: 10px 20px; align-items: center; margin-top: 14px; }
.seg { display: inline-flex; border: 1px solid var(--border); border-radius: 8px; overflow: hidden; }
.seg button { font: inherit; font-size: 14px; font-weight: 600; min-height: 36px; padding: 0 14px; background: var(--panel); color: var(--text); border: 0; cursor: pointer; }
.seg button[aria-pressed="true"] { background: var(--accent); color: var(--on-accent); }
button:focus-visible, a:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
video, .split img { display: block; width: 100%; height: auto; max-width: 100%; border: 1px solid var(--border); border-radius: 10px; background: #101218; }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 28px 24px; }
.phones { display: grid; grid-template-columns: repeat(3, minmax(0, 300px)); gap: 24px; }
figure { margin: 0; min-width: 0; }
.shot { display: block; width: 100%; padding: 0; border: 1px solid var(--border); border-radius: 10px; overflow: hidden; background: var(--panel2); cursor: zoom-in; }
.shot img { display: block; width: 100%; height: auto; }
figcaption { margin-top: 8px; font-size: 14px; color: var(--muted); max-width: 62ch; }
figcaption b { display: block; color: var(--text); font-size: 15px; }
#view { position: fixed; inset: 0; z-index: 10; display: flex; align-items: center; justify-content: center; padding: 16px max(16px, env(safe-area-inset-right, 0px)) 16px max(16px, env(safe-area-inset-left, 0px)); background: rgba(6, 8, 14, .88); cursor: zoom-out; }
#view img { max-width: 100%; max-height: 100%; border-radius: 8px; }
@media (max-width: 760px) { .grid { grid-template-columns: minmax(0, 1fr); } .phones { grid-template-columns: repeat(2, minmax(0, 1fr)); } h1 { font-size: 25px; } }
</style>
<main>
  <header>
    <h1>Comebye in screenshots</h1>
    <p>One coordinator, quill, is rolling out rate limits for an API called tidepool with three workers, while other agents get on with other work across three hosts. Comebye is the web app over all of them. Everything shown is made up, and comes from the same fake backend as the <a href="${TOUR_URL}" target="_blank" rel="noopener">clickable tour</a>.</p>
    <div class="bar">
      <div class="seg" role="group" aria-label="Theme of the screenshots">
        <button type="button" id="pick-dark" data-pick="dark" aria-pressed="true">Dark</button><button type="button" id="pick-light" data-pick="light" aria-pressed="false">Light</button>
      </div>
      <a href="${TOUR_URL}" target="_blank" rel="noopener">Take the clickable tour</a>
      <a href="${REPO_URL}" target="_blank" rel="noopener">Source on GitHub</a>
    </div>
  </header>
  <section aria-labelledby="h-walk">
    <h2 id="h-walk">The main flow</h2>
    <video controls muted playsinline preload="metadata" poster="video/poster.jpg" width="1440" height="900">
      <source src="video/walkthrough.mp4" type="video/mp4"><source src="video/walkthrough.webm" type="video/webm">
    </video>
  </section>
  <section aria-labelledby="h-desk">
    <h2 id="h-desk">On the desktop</h2>
    <div class="grid">${DESKTOP.map(([n, t, c]) => fig(n, t, c)).join("\n")}
    <figure class="split"><img src="12-light-and-dark.png" alt="The same screen in the dark theme and the light theme, side by side" loading="lazy" width="2904" height="900"><figcaption><b>Light and dark</b> Follows your system, or set it from the menu.</figcaption></figure></div>
  </section>
  <section aria-labelledby="h-phone">
    <h2 id="h-phone">On a phone</h2>
    <div class="phones">${PHONE.map(([n, t, c]) => fig(n, t, c, "ph")).join("\n")}</div>
  </section>
</main>
<div id="view" hidden><img id="view-img" alt=""></div>
<script>
  let theme = "dark";
  const imgs = () => document.querySelectorAll("img[data-name]");
  for (const b of document.querySelectorAll("[data-pick]")) b.addEventListener("click", () => {
    theme = b.dataset.pick;
    for (const x of document.querySelectorAll("[data-pick]")) x.setAttribute("aria-pressed", String(x === b));
    for (const i of imgs()) i.src = theme + "/" + i.dataset.name + ".png";
  });
  const view = document.getElementById("view"), big = document.getElementById("view-img");
  const close = () => { view.hidden = true; big.removeAttribute("src"); };
  for (const s of document.querySelectorAll(".shot")) s.addEventListener("click", () => {
    big.src = theme + "/" + s.dataset.name + ".png"; big.alt = s.querySelector("img").alt; view.hidden = false;
  });
  view.addEventListener("click", close);
  addEventListener("keydown", (e) => { if (e.key === "Escape") close(); });
</script>
`;
  fs.writeFileSync(path.join(out, "gallery.html"), page);
  console.log("gallery: " + path.join(out, "gallery.html"));
}

fs.mkdirSync(out, { recursive: true });
if (only !== "gallery") {
  const file = buildPage();
  const srv = http.createServer((q, r) => { r.setHeader("content-type", "text/html; charset=utf-8"); r.end(fs.readFileSync(file)); }).listen(0);
  const base = `http://127.0.0.1:${srv.address().port}/`;
  const browser = await chromium.launch({ executablePath: CHROME, headless: true, args: ["--no-sandbox"] });
  try {
    if (only !== "video") await makeShots(browser, base);
    if (only !== "shots") await makeVideo(browser, base);
  } finally { await browser.close(); srv.close(); fs.rmSync(path.dirname(file), { recursive: true, force: true }); }
}
if (only !== "shots" && only !== "video") makeGallery();
