// Pages (comebye publish): the sandbox, and how the screen looks.
//
// Pages are published the way a pi worker does it: by the real `comebye`
// binary, from a bare environment, as an agent. A hostile page then tries to
// read the hub's API, post to it, read the hub's cookie and localStorage and
// navigate the app's window, in the app's iframe and opened directly: every
// attempt must fail while the page's own script runs. The same attack code
// run from the hub's own origin must succeed, so the test cannot pass because
// the attacks are broken.
//
//   node pages.mjs BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR)
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("pages.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
const pagesUrl = process.env.PAGES_URL || ""; // set when the hub runs a pages listener
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "pages-ui-"));
const results = [];
const log = [];

// The CLI as a pi worker has it: nothing but PATH, HOME and where the hub is.
function publish(args, input, pane = "w2:p1") {
  const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: pane };
  return execFileSync(bin, ["publish", ...args], { env, input, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
}
const idOf = (link) => link.match(/pg_[0-9a-f]{10}/)[0];
const file = (name, text) => { const p = path.join(work, name); fs.writeFileSync(p, text); return p; };

// ---- the attack, as source text: run in the page and, as a control, from the hub ----
const PROBE = `async function probe(o) {
  const out = {}, viol = [];
  document.addEventListener("securitypolicyviolation", (e) => viol.push(e.effectiveDirective));
  const rec = async (k, f) => { try { out[k] = "READ: " + String(await f()).slice(0, 50); } catch (e) { out[k] = "blocked: " + ((e && e.name) || "error"); } };
  await rec("fetch /api/agents", async () => { const r = await fetch("/api/agents"); const t = await r.text(); if (!r.ok || !t) throw new Error("no"); return t; });
  await rec("xhr /api/agents", () => new Promise((res, rej) => { const x = new XMLHttpRequest(); x.open("GET", "/api/agents"); x.onload = () => (x.status === 200 && x.responseText ? res(x.responseText) : rej(new Error("status " + x.status))); x.onerror = () => rej(new Error("xhr error")); x.send(); }));
  await rec("fetch absolute hub url", async () => { const r = await fetch(o.hub + "/api/agents"); const t = await r.text(); if (!r.ok || !t) throw new Error("no"); return t; });
  await rec("post /api/todos", async () => { const r = await fetch("/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ question: "PWNED " + o.tag, kind: "decision" }) }); if (!r.ok) throw new Error("status " + r.status); return await r.text(); });
  await rec("document.cookie", () => { const c = document.cookie; if (!c.includes("hubsecret")) throw new DOMException("no secret", "NotFound"); return c; });
  await rec("localStorage", () => { const v = localStorage.getItem("hubsecret"); if (v !== "s3cret") throw new DOMException("no secret", "NotFound"); return v; });
  await rec("fetch another page", async () => { const r = await fetch("/p/" + o.other); const t = await r.text(); if (!r.ok || !t) throw new Error("no"); return t; });
  if (o.frame) {
    await rec("parent.document", () => parent.document.title);
    // A tab may open now (allow-popups; the browser's blocker decides without a click), but the page can read nothing from it.
    await rec("window.open: read the popup", () => { const w = window.open(o.hub + "/api/agents"); if (!w) throw new DOMException("popup blocked", "NotAllowed"); return String(w.document.body.innerText).slice(0, 40); });
    out["top.location"] = await (async () => { try { window.top.location.href = o.hub + "/api/agents"; return "no exception (the browser must still refuse)"; } catch (e) { return "threw " + e.name; } })();
    await rec("nested iframe on the API", () => new Promise((res, rej) => { const f = document.createElement("iframe"); f.src = "/api/agents"; f.onload = () => { try { res("loaded: " + f.contentDocument.body.innerText.slice(0, 40)); } catch (e) { rej(e); } }; document.body.append(f); setTimeout(() => rej(new DOMException("no load", "Timeout")), 800); }));
    await rec("script from the hub", () => new Promise((res, rej) => { const s = document.createElement("script"); s.src = "/api/agents"; s.onload = () => res("loaded"); s.onerror = () => rej(new DOMException("blocked", "ScriptError")); document.body.append(s); }));
    await rec("form post", () => new Promise((res, rej) => { const f = document.createElement("form"); f.method = "post"; f.action = "/api/todos"; const i = document.createElement("input"); i.name = "x"; f.append(i); document.body.append(f); f.submit(); setTimeout(() => rej(new DOMException("did not navigate", "NotAllowed")), 500); }));
    await new Promise((r) => setTimeout(r, 300));
  }
  out.violations = viol.length;
  return out;
}`;

const probePage = (hub, other, tag) => `<!doctype html><html><head><meta charset="utf-8"><title>Probe: a hostile page</title>
<style>body{font:15px system-ui;margin:24px;background:#0d1b2a;color:#e0e6ee}h1{margin:0 0 12px}#alive{padding:10px;background:#1b3a4b;border-radius:6px}pre{white-space:pre-wrap;word-break:break-all}</style></head>
<body><h1>Hostile page</h1><p id="alive">script has not run</p><pre id="result">running…</pre>
<script>
document.getElementById("alive").textContent = "own script ran: 6 x 7 = " + (6 * 7);
${PROBE}
probe({ hub: ${JSON.stringify(hub)}, other: ${JSON.stringify(other)}, tag: ${JSON.stringify(tag)}, frame: window.top !== window })
  .then((r) => { document.getElementById("result").textContent = JSON.stringify(r); window.__done = true; });
</script></body></html>`;

const directProbePage = (hub, other, tag) => probePage(hub, other, tag).replace("frame: window.top !== window", "frame: false");

const mdSource = `# Failover test: 3 of 3 passed

Run on **w1**, \`kubectl\` 1.31. See [the dashboard](https://example.com/dash) and [below](#table).

| step | result | ms |
|------|--------|---:|
| drain node | ok | 412 |
| promote replica | ok | 1 920 |
| restore | ok | 3 305 |

<script>window.__pwned = 1; document.title = "pwned"</script>
<img src="x" onerror="window.__pwned = 2">

- [x] data kept
- [ ] latency under 1s

\`\`\`sh
comebye publish report.md --key failover
\`\`\`
`;

const chartHtml = `<!doctype html><html><head><meta charset="utf-8"><title>Latency by hour</title>
<style>body{font:15px system-ui;margin:0;padding:20px;background:#fff;color:#111}h1{font-size:20px;margin:0 0 4px}p{color:#555;margin:0 0 14px}
.bar{display:flex;align-items:center;gap:8px;margin:5px 0}.bar b{width:48px;font-weight:500}.bar i{display:block;height:16px;background:#2563eb;border-radius:3px}
.wide{min-width:900px;border-collapse:collapse}.wide td,.wide th{border:1px solid #ccc;padding:4px 10px}</style></head><body>
<h1>Latency by hour</h1><p>p95 in milliseconds, last 12 hours (drawn by this page's own script)</p><div id="c"></div>
<script>const d=[120,140,135,180,260,410,380,300,220,190,150,130];
d.forEach((v,i)=>{const r=document.createElement("div");r.className="bar";r.innerHTML="<b>"+String(i).padStart(2,"0")+":00</b><i style='width:"+v*1.2+"px'></i><span>"+v+" ms</span>";document.getElementById("c").append(r)});</script>
<div style="overflow-x:auto;margin-top:16px"><table class="wide"><tr><th>hour</th><th>p50</th><th>p95</th><th>p99</th><th>errors</th><th>notes</th></tr><tr><td>05:00</td><td>40</td><td>410</td><td>900</td><td>3</td><td>deploy at 05:02, cache cold for a few minutes</td></tr></table></div>
</body></html>`;

const pages = {};
const check = async (name, fn) => {
  try { await fn(); results.push([name, true, ""]); } catch (e) {
    results.push([name, false, String(e.message).split("\n")[0]]);
    await shotFail?.(name);
  }
};
let shotFail = null;
// A frame under the page CSP cannot take Playwright's string-evaluating waits: poll with evaluate.
async function until(fr, fn, arg, ms = 8000) {
  const end = Date.now() + ms;
  for (;;) {
    if (await fr.evaluate(fn, arg).catch(() => false)) return;
    if (Date.now() > end) throw new Error("timed out waiting for the page");
    await new Promise((r) => setTimeout(r, 100));
  }
}

// ---- publish: as a pi worker would, with the CLI only ----
await check("publish: the CLI alone prints a link, lists, shows, updates in place", async () => {
  pages.secret = idOf(publish([file("secret.md", "# Quarterly numbers\n\nsecret-marker-4711"), "--handoff", "h-report"]));
  // stdin and a key, the way a script without a file does it
  pages.md = idOf(publish(["-", "--key", "failover", "--title", "Failover test"], mdSource, "w1:p1"));
  pages.chart = idOf(publish([file("latency.html", chartHtml)]));
  const again = publish(["-", "--key", "failover"], mdSource.replace("3 of 3", "3 of 3 (rerun)"), "w1:p1");
  if (idOf(again) !== pages.md) throw new Error("same key made another page: " + again);
  const list = execFileSync(bin, ["pages", "list"], { env: { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w2:p1" }, encoding: "utf8" });
  for (const id of [pages.secret, pages.md, pages.chart]) if (!list.includes(id)) throw new Error("not listed: " + id + "\n" + list);
  if (!/Latency by hour/.test(list) || !/Failover test/.test(list)) throw new Error("titles missing:\n" + list);
  const probeFile = file("probe.html", probePage(base, pages.secret, "frame"));
  pages.probe = idOf(publish([probeFile, "--title", "Probe: a hostile page"]));
  pages.direct = idOf(publish([file("probe2.html", directProbePage(base, pages.secret, "direct")), "--title", "Probe, opened directly"]));
});

// ---- the sandbox, in the app's iframe ----
const desktop = await launch({ width: 1280, height: 800, dark, bypassCSP: false });
const { page } = desktop;
page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
let popups = 0;
desktop.ctx.on("page", () => { popups++; });
const frameOf = (id) => page.frames().find((f) => f.url().includes("/p/" + id));
const todoCount = async () => (await (await fetch(base + "/api/todos?state=all")).json()).todos.filter((t) => /PWNED/.test(t.question)).length;
const shot = (name) => page.screenshot({ path: `${out}/${name}.png` });
shotFail = (name) => page.screenshot({ path: `${out}/pages-fail-${name.replace(/\W+/g, "-").slice(0, 40)}.png` }).catch(() => {});

await check("control: the same attacks from the hub's own origin all succeed", async () => {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.evaluate(() => { document.cookie = "hubsecret=s3cret; path=/"; localStorage.setItem("hubsecret", "s3cret"); });
  const r = await page.evaluate(`(${PROBE})({ hub: ${JSON.stringify(base)}, other: ${JSON.stringify(pages.secret)}, tag: "control", frame: false })`);
  for (const k of ["fetch /api/agents", "xhr /api/agents", "fetch absolute hub url", "post /api/todos", "document.cookie", "localStorage", "fetch another page"]) {
    if (!String(r[k]).startsWith("READ")) throw new Error(`control could not do "${k}": ${r[k]}; the attacks prove nothing`);
  }
  if ((await todoCount()) !== 1) throw new Error("the control's POST did not land");
  // clean up the control's to-do item so only the page can add one from here on
  const t = (await (await fetch(base + "/api/todos?state=all")).json()).todos.find((x) => /PWNED/.test(x.question));
  await fetch(base + "/api/todos/" + t.id + "/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
});

const bad = (r, keys) => keys.filter((k) => !String(r[k]).startsWith("blocked")).map((k) => `${k}: ${r[k]}`);
const NET = ["fetch /api/agents", "xhr /api/agents", "fetch absolute hub url", "post /api/todos", "document.cookie", "localStorage", "fetch another page"];
const FRAME = ["parent.document", "window.open: read the popup", "nested iframe on the API", "script from the hub", "form post"];
let probeResult = null;

await check("sandbox: in the app's iframe nothing of the hub is reachable and the page's own script runs", async () => {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.evaluate(() => { window.__marker = "app window"; document.cookie = "hubsecret=s3cret; path=/"; localStorage.setItem("hubsecret", "s3cret"); });
  const before = await todoCount();
  popups = 0;
  await page.goto(base + `/#/pages/${pages.probe}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  const sandbox = await page.locator("iframe.pgs-frame").getAttribute("sandbox");
  if (sandbox !== "allow-scripts allow-popups allow-popups-to-escape-sandbox") throw new Error(`iframe sandbox=${sandbox}`);
  await page.evaluate(() => { window.__marker = "app window"; });
  let fr;
  for (let i = 0; i < 60 && !fr; i++) { fr = frameOf(pages.probe); if (!fr) await page.waitForTimeout(100); }
  if (!fr) throw new Error("page frame not found");
  await until(fr, () => window.__done === true);
  probeResult = JSON.parse(await fr.locator("#result").innerText());
  if (!/own script ran: 6 x 7 = 42/.test(await fr.locator("#alive").innerText())) throw new Error("the page's own script did not run");
  await page.waitForTimeout(700); // time for a top navigation to land, if one would
  const failures = bad(probeResult, [...NET, ...FRAME]);
  if (failures.length) throw new Error("not blocked: " + failures.join("; "));
  if (!/sandbox/.test(probeResult["top.location"] || "") && !/threw|no exception/.test(probeResult["top.location"])) throw new Error("top.location: " + probeResult["top.location"]);
  if (probeResult.violations < 3) throw new Error("the browser reported only " + probeResult.violations + " CSP violations");
  if (page.url() !== base + `/#/pages/${pages.probe}`) throw new Error("the app's window was navigated: " + page.url());
  if ((await page.evaluate(() => window.__marker)) !== "app window") throw new Error("the app's document was replaced");
  // A scripted window.open may open a tab (headless Chrome has no popup blocker); it must not have navigated or replaced the app's window, checked above.
  for (const pg of desktop.ctx.pages()) if (pg !== page) await pg.close().catch(() => {});
  if ((await todoCount()) !== before) throw new Error("the page managed to add a to-do item");
  if (/s3cret|secret-marker-4711/.test(JSON.stringify(probeResult))) throw new Error("the page saw hub data");
});

await check("sandbox: opened directly in a tab the same header applies", async () => {
  const p2 = await desktop.ctx.newPage();
  await p2.goto(base + "/", { waitUntil: "load" });
  await p2.evaluate(() => { document.cookie = "hubsecret=s3cret; path=/"; localStorage.setItem("hubsecret", "s3cret"); });
  const before = await todoCount();
  const resp = await p2.goto(base + "/p/" + pages.direct, { waitUntil: "load" });
  const csp = resp.headers()["content-security-policy"] || "";
  if (!/^sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox;/.test(csp) || /allow-same-origin/.test(csp)) throw new Error("CSP: " + csp);
  if (Object.keys(resp.headers()).some((h) => h.startsWith("access-control-"))) throw new Error("CORS headers on a page");
  await until(p2, () => window.__done === true);
  const r = JSON.parse(await p2.locator("#result").innerText());
  if (!/own script ran: 6 x 7 = 42/.test(await p2.locator("#alive").innerText())) throw new Error("own script did not run");
  const failures = bad(r, NET);
  if (failures.length) throw new Error("not blocked: " + failures.join("; "));
  if ((await todoCount()) !== before) throw new Error("the page managed to add a to-do item");
  // the response headers of other /p/ answers, errors included
  for (const [u, status] of [["/p/pg_0000000000", 404], ["/p/_/nope.js", 404], ["/p/" + pages.md, 200]]) {
    const r2 = await fetch(base + u);
    if (r2.status !== status || !/^sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox;/.test(r2.headers.get("content-security-policy") || "") || [...r2.headers.keys()].some((h) => h.startsWith("access-control-"))) throw new Error(`${u}: ${r2.status} ${r2.headers.get("content-security-policy")}`);
  }
  await p2.close();
});

await check("markdown: rendered in the sandbox, its script and handler do nothing, links open in a new tab", async () => {
  await page.goto(base + `/#/pages/${pages.md}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  let fr;
  for (let i = 0; i < 60 && !fr; i++) { fr = frameOf(pages.md); if (!fr) await page.waitForTimeout(100); }
  await fr.waitForSelector("#out h1", { timeout: 8000 });
  const h1 = await fr.locator("#out h1").innerText();
  if (!/Failover test: 3 of 3/.test(h1)) throw new Error("h1: " + h1);
  if ((await fr.locator("#out table tr").count()) !== 4) throw new Error("table rows " + (await fr.locator("#out table tr").count()));
  if (await fr.evaluate(() => window.__pwned)) throw new Error("script or handler in the markdown ran");
  if (await fr.locator("#out script, #out img").count()) throw new Error("script or img survived");
  const lk = await fr.locator('#out a[href^="https://example.com"]').evaluate((a) => [a.target, a.rel, a.textContent]);
  if (lk[0] !== "_blank" || !/noopener/.test(lk[1]) || lk[2] !== "the dashboard") throw new Error("external link: " + lk);
  // two versions were published: the picker shows both
  const opts = await page.locator("#pgs-ver option").allTextContents();
  if (opts.length !== 2 || !/latest/.test(opts[0])) throw new Error("versions: " + opts);
  await shot("pages-markdown-1280");
});

// ---- the theme message: the app sends its colours into the frame ----
// The page carries the listener of the page template (the same code): it applies
// a known --token with a colour value and ignores everything else.
const themePage = `<!doctype html><html><head><meta charset="utf-8"><title>Theme probe</title>
<style>body{background:var(--bg,#fff);color:var(--text,#000);font:16px system-ui;padding:24px}</style></head><body><h1>Theme probe</h1><p id="p">waiting</p>
<script>(function () {
  var KNOWN = ["bg","panel","text","muted","blue","border"];
  var ok = {}; KNOWN.forEach(function (k) { ok["--" + k] = 1; });
  var COLOUR = /^(#[0-9a-f]{3,8}|(rgb|hsl)a?\\(\\s*[0-9.%\\s,\\/-]+\\))$/i;
  window.addEventListener("message", function (e) {
    var d = e.data;
    if (!d || typeof d !== "object" || d.type !== "comebye-theme" || !d.tokens || typeof d.tokens !== "object") return;
    var root = document.documentElement;
    Object.keys(d.tokens).forEach(function (k) { var v = d.tokens[k]; if (ok[k] === 1 && typeof v === "string" && v.length <= 64 && COLOUR.test(v)) root.style.setProperty(k, v); });
    if (d.scheme === "light" || d.scheme === "dark") root.style.colorScheme = d.scheme;
  });
})();</script></body></html>`;
await check("theme: the page gets the app's theme after load and again when the theme changes; bad tokens are ignored", async () => {
  pages.theme = idOf(publish([file("theme.html", themePage)]));
  await page.goto(base + "/", { waitUntil: "load" });
  await page.goto(base + `/#/pages/${pages.theme}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  let fr;
  for (let i = 0; i < 60 && !fr; i++) { fr = frameOf(pages.theme); if (!fr) await page.waitForTimeout(100); }
  const appTok = (id, k) => page.evaluate(([id, k]) => ComebyeThemes.byId[id].vars[k], [id, k]);
  const frameVars = () => fr.evaluate(() => { const c = getComputedStyle(document.documentElement); return { bg: c.getPropertyValue("--bg").trim(), blue: c.getPropertyValue("--blue").trim(), scheme: document.documentElement.style.colorScheme, body: getComputedStyle(document.body).backgroundColor }; });
  const rgb = (h) => "rgb(" + [1, 3, 5].map((i) => parseInt(h.slice(i, i + 2), 16)).join(", ") + ")";
  const want = async (id, why) => {
    const bg = await appTok(id, "bg"), blue = await appTok(id, "blue");
    const mode = await page.evaluate((id) => ComebyeThemes.byId[id].mode, id);
    let v;
    for (let i = 0; i < 40; i++) { v = await frameVars(); if (v.bg === bg) break; await page.waitForTimeout(100); }
    if (v.bg !== bg || v.blue !== blue || v.scheme !== mode || v.body !== rgb(bg)) throw new Error(`${why}: page has ${JSON.stringify(v)}, app theme ${id} has bg=${bg} blue=${blue} ${mode}`);
  };
  const first = await page.evaluate(() => ComebyeThemes.resolve()); // System: the OS scheme's Comebye theme
  await want(first, "after load");
  await shot("pages-theme-" + first + "-1280");
  // switching the theme in the app (as the picker does) repaints the page
  await page.evaluate(() => ComebyeThemes.set("gruvbox-light"));
  await want("gruvbox-light", "after switching to Gruvbox Light");
  await shot("pages-theme-gruvbox-light-1280");
  await page.evaluate(() => ComebyeThemes.set("dracula"));
  await want("dracula", "after switching to Dracula");
  // the page ignores what is not a known token with a colour: nothing here may stick
  const dracula = await frameVars();
  await fr.evaluate(() => {
    const send = (data) => window.postMessage(data, "*");
    send({ type: "comebye-theme", tokens: { "--bg": "url(https://evil.example/x)", "--text": "red; background: red", "--evil": "#ff0000", "evil": "#00ff00", "--panel": 5 }, scheme: "sepia" });
    send({ type: "other", tokens: { "--bg": "#123456" } });
    send("comebye-theme");
  });
  await page.waitForTimeout(300);
  const after = await fr.evaluate(() => { const r = document.documentElement; return { bg: r.style.getPropertyValue("--bg"), text: r.style.getPropertyValue("--text"), evil: r.style.getPropertyValue("--evil"), scheme: r.style.colorScheme }; });
  if (after.bg !== dracula.bg || after.text === "red; background: red" || after.evil || after.scheme === "sepia") throw new Error("a bad theme message got through: " + JSON.stringify(after));
  await page.evaluate(() => ComebyeThemes.set("system"));
});

// ---- images inlined at publish, Mermaid in Markdown, the version pill, clipboard-write ----
const PNG = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAFElEQVR4nGP8z8Dwn4EIwESMolGFxCsEABhYAhHYAxhTAAAAAElFTkSuQmCC", "base64");
await check("images: publish inlines a page's local images, and the frame shows them", async () => {
  fs.mkdirSync(path.join(work, "img"), { recursive: true });
  fs.writeFileSync(path.join(work, "img", "shot.png"), PNG);
  const html = `<!doctype html><html><head><meta charset="utf-8"><title>Shots</title><style>.bg{width:20px;height:20px;background:url(img/shot.png)}</style></head><body>
<img id="a" src="img/shot.png" alt="a"><img id="b" srcset="img/shot.png 1x, img/shot.png 2x" alt="b"><div id="c" class="bg"></div><img id="d" src="img/missing.png" alt="d"></body></html>`;
  const f = file("shots.html", html);
  const out2 = execFileSync(bin, ["publish", f], { env: { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w2:p1" }, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] });
  pages.shots = idOf(out2);
  const body = await (await fetch(base + "/api/pages/" + pages.shots + "?body=1")).text().catch(() => "");
  await page.goto(base + `/#/pages/${pages.shots}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  let fr;
  for (let i = 0; i < 60 && !fr; i++) { fr = frameOf(pages.shots); if (!fr) await page.waitForTimeout(100); }
  await until(fr, () => document.getElementById("a").complete);
  const st = await fr.evaluate(() => ({ a: document.getElementById("a").naturalWidth, a_src: document.getElementById("a").src.slice(0, 22), b: document.getElementById("b").currentSrc.slice(0, 22), c: getComputedStyle(document.getElementById("c")).backgroundImage.slice(0, 26), d: document.getElementById("d").src }));
  if (st.a !== 2 || !st.a_src.startsWith("data:image/png") || !st.b.startsWith("data:image/png") || !st.c.startsWith('url("data:image/png')) throw new Error("not inlined: " + JSON.stringify(st) + body.slice(0, 80));
  if (!st.d.endsWith("img/missing.png") && !/missing/.test(st.d)) throw new Error("the missing image was rewritten: " + st.d);
});

await check("images: a reference that leaves the page's directory is refused by the CLI", async () => {
  const f = file("escape.html", `<img src="../../../etc/hostname">`);
  let err = "";
  try { publish([f]); } catch (e) { err = String(e.stderr || e.message); }
  if (!/outside the page's directory/.test(err)) throw new Error("publish said: " + err);
});

const mermaidMd = "# Flow\n\n```mermaid\nflowchart LR\n  A[Start] --> B{Ok?}\n  B -->|yes| C[Done]\n```\n\nafter the diagram\n";
const hostileMd = "# Hostile\n\n```mermaid\nflowchart LR\n  A[\"<img src=x onerror=window.__pwned=1>\"] --> B\n  click A callback \"window.__pwned=2\"\n  click B href \"javascript:window.__pwned=3\"\n```\n\n```mermaid\nthis is not a diagram\n```\n";
await check("mermaid: a mermaid block draws an SVG in the Markdown page; a page without one never loads Mermaid", async () => {
  pages.mmd = idOf(publish(["-", "--kind", "md", "--title", "Flow"], mermaidMd));
  const asked = [];
  const onReq = (rq) => { if (/\/p\/_\/mermaid\.js/.test(rq.url())) asked.push(rq.url()); };
  page.on("request", onReq);
  await page.goto(base + `/#/pages/${pages.md}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  let plain;
  for (let i = 0; i < 60 && !plain; i++) { plain = frameOf(pages.md); if (!plain) await page.waitForTimeout(100); }
  await plain.waitForSelector("#out h1", { timeout: 8000 });
  await page.waitForTimeout(500);
  if (asked.length) throw new Error("Mermaid was loaded by a page without a diagram: " + asked);
  await page.goto(base + `/#/pages/${pages.mmd}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  let fr;
  for (let i = 0; i < 60 && !fr; i++) { fr = frameOf(pages.mmd); if (!fr) await page.waitForTimeout(100); }
  await fr.waitForSelector("#out .mmd svg", { timeout: 15000 });
  page.off("request", onReq);
  if (asked.length !== 1) throw new Error("mermaid.js requests: " + asked.length);
  const st = await fr.evaluate(() => ({ nodes: document.querySelectorAll("#out .mmd svg .node").length, text: document.querySelector("#out .mmd svg").textContent, rest: document.getElementById("out").innerText, w: document.querySelector("#out .mmd svg").getBoundingClientRect().width }));
  if (st.nodes < 3 || !/Start/.test(st.text) || !/after the diagram/.test(st.rest)) throw new Error("diagram: " + JSON.stringify(st));
  await page.waitForTimeout(200);
  await shot("pages-mermaid-1280");
});

await check("mermaid: a hostile diagram (html label, click callbacks, javascript: link) runs nothing, a broken one shows its source", async () => {
  pages.mmdBad = idOf(publish(["-", "--kind", "md", "--title", "Hostile diagram"], hostileMd));
  await page.goto(base + `/#/pages/${pages.mmdBad}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  let fr;
  for (let i = 0; i < 60 && !fr; i++) { fr = frameOf(pages.mmdBad); if (!fr) await page.waitForTimeout(100); }
  await until(fr, () => !!document.querySelector("#out .u") || document.querySelectorAll("#out .mmd").length > 1, null, 15000);
  await page.waitForTimeout(500);
  const st = await fr.evaluate(() => ({ pwned: window.__pwned || 0, img: document.querySelectorAll("#out img").length, js: document.querySelectorAll("#out [href^=javascript], #out [onclick], #out [onerror]").length, note: (document.querySelector("#out .u") || {}).textContent || "", pres: document.querySelectorAll("#out pre").length }));
  if (st.pwned || st.img || st.js) throw new Error("hostile diagram ran or left markup: " + JSON.stringify(st));
  if (!/could not be drawn/.test(st.note) || st.pres < 1) throw new Error("broken diagram: " + JSON.stringify(st));
});

await check("clipboard: the frame is allowed to write the clipboard (and the sandbox is unchanged)", async () => {
  await page.goto(base + `/#/pages/${pages.chart}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  const attrs = await page.locator("iframe.pgs-frame").evaluate((f) => [f.getAttribute("allow"), f.getAttribute("sandbox")]);
  if (attrs[0] !== "clipboard-write" || attrs[1] !== "allow-scripts allow-popups allow-popups-to-escape-sandbox") throw new Error("iframe allow/sandbox: " + attrs);
  let fr;
  for (let i = 0; i < 60 && !fr; i++) { fr = frameOf(pages.chart); if (!fr) await page.waitForTimeout(100); }
  await fr.waitForSelector(".bar", { timeout: 8000 });
  const allowed = await fr.evaluate(() => [document.featurePolicy.allowsFeature("clipboard-write"), document.featurePolicy.allowsFeature("clipboard-read"), document.featurePolicy.allowsFeature("camera")]);
  if (!allowed[0] || allowed[1] || allowed[2]) throw new Error("permissions policy in the frame (write, read, camera): " + allowed);
});

await check("version pill: a newer version shows as a pill that reloads the frame on click; a pinned version stays", async () => {
  const v1 = '<!doctype html><html><head><meta charset="utf-8"><title>Live</title></head><body><h1 id="v">version one</h1><script>window.__mark=1</script></body></html>';
  pages.live = idOf(publish(["-", "--key", "livepage", "--kind", "html"], v1));
  await page.goto(base + `/#/pages/${pages.live}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  let fr;
  for (let i = 0; i < 60 && !fr; i++) { fr = frameOf(pages.live); if (!fr) await page.waitForTimeout(100); }
  await fr.waitForSelector("#v");
  if (await page.locator("#pgs-new").count()) throw new Error("a pill with no newer version");
  publish(["-", "--key", "livepage", "--kind", "html"], v1.replace("version one", "version two"));
  await page.evaluate(() => pagesLoad());
  await page.waitForSelector("#pgs-new", { timeout: 8000 });
  const txt = await page.locator("#pgs-new").innerText();
  if (!/v2 is out/.test(txt)) throw new Error("pill text: " + txt);
  if ((await fr.evaluate(() => document.getElementById("v").textContent)) !== "version one" || !(await fr.evaluate(() => window.__mark === 1))) throw new Error("the page was swapped under the reader");
  // a phone that opened version 2, then version 3 comes out: the pill fits at 390
  const phone = await launch({ width: 390, height: 844, dark, bypassCSP: false });
  const m = phone.page;
  await m.goto(base + `/#/pages/${pages.live}`, { waitUntil: "load" });
  await m.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  await m.waitForTimeout(500);
  publish(["-", "--key", "livepage", "--kind", "html"], v1.replace("version one", "version three"));
  await m.evaluate(() => pagesLoad());
  await page.evaluate(() => pagesLoad());
  await m.waitForSelector("#pgs-new", { timeout: 8000 });
  const over = await m.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector(".pgs-bar").scrollWidth, document.querySelector(".pgs-bar").clientWidth, document.getElementById("pgs-new").getBoundingClientRect().height]);
  if (over[0] > over[1] || over[2] > over[3] || over[4] < 36) throw new Error("pill at 390: " + over);
  await m.screenshot({ path: `${out}/pages-pill-390.png` });
  await phone.browser.close();
  await page.waitForFunction(() => /v3 is out/.test(document.getElementById("pgs-new")?.textContent || ""), null, { timeout: 8000 });
  await page.waitForTimeout(150);
  await shot("pages-pill-1280");
  await page.locator("#pgs-new").click();
  await until(page, () => !document.querySelector("#pgs-new") && /\/p\/pg_[0-9a-f]{10}\/3$/.test(document.querySelector("iframe.pgs-frame").src), null, 5000);
  let f2;
  for (let i = 0; i < 60 && !f2; i++) { f2 = frameOf(pages.live); if (f2 && !(await f2.evaluate(() => document.getElementById("v")?.textContent === "version three").catch(() => false))) f2 = null; if (!f2) await page.waitForTimeout(100); }
  if (!f2) throw new Error("the frame did not load version three");
  // a reader who picked version 1 is not moved by a version 3
  await page.locator("#pgs-menub").click();
  await page.locator("#pgs-ver").selectOption("1");
  await until(page, () => /\/1$/.test(document.querySelector("iframe.pgs-frame").src), null, 5000);
  publish(["-", "--key", "livepage", "--kind", "html"], v1.replace("version one", "version four"));
  await page.evaluate(() => pagesLoad());
  await until(page, () => document.querySelectorAll("#pgs-ver option").length === 4, null, 5000);
  if (await page.locator("#pgs-new").count() || !/\/1$/.test(await page.locator("iframe.pgs-frame").getAttribute("src"))) throw new Error("a pinned version was moved or got a pill");
});

await check("links: a click on a link in a page opens a new tab (not the app) with no opener; the page still reaches nothing of the app (the sandbox check above, with a scripted window.open, still passes)", async () => {
  const target = base + "/p/" + pages.chart;
  const html = `<!doctype html><html><head><meta charset="utf-8"><title>Links</title></head><body>
<a id="l1" href="${target}" target="_blank" rel="noopener">noopener link</a><br><a id="l2" href="${target}" target="_blank">plain target=_blank link</a></body></html>`;
  pages.links = idOf(publish([file("links.html", html)]));
  await page.goto(base + "/", { waitUntil: "load" });
  await page.goto(base + `/#/pages/${pages.links}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  let fr;
  for (let i = 0; i < 60 && !fr; i++) { fr = frameOf(pages.links); if (!fr) await page.waitForTimeout(100); }
  await fr.waitForSelector("#l1");
  const appUrl = page.url();
  for (const id of ["l1", "l2"]) {
    // A click on a just-attached out-of-process frame can land before the browser has hit-test data for it and be dropped
    // (a flake in CI, never a missing allow-popups: that fails every try). Wait for two painted frames, then click again
    // if no tab came; a sandbox that really forbids popups still fails after the last try.
    let pop;
    for (let tries = 0; !pop; tries++) {
      await fr.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
      const opened = desktop.ctx.waitForEvent("page", { timeout: 3000 });
      opened.catch(() => {});
      await fr.locator("#" + id).click();
      try { pop = await opened; } catch (e) { if (tries >= 3) throw e; }
    }
    await pop.waitForLoadState("load");
    if (!pop.url().includes("/p/" + pages.chart)) throw new Error(id + " opened " + pop.url());
    const opener = await pop.evaluate(() => window.opener === null);
    // the plain link's opener is the page's own window at most, never the app: it cannot reach the app either way
    if (id === "l1" && !opener) throw new Error("window.opener is set with rel=noopener");
    if (id === "l2" && !opener) {
      const reach = await pop.evaluate(() => { try { return window.opener.parent === window.opener ? "page" : "app?"; } catch { return "blocked"; } });
      if (reach === "app?") throw new Error("a popup could reach the app through its opener");
    }
    await pop.close();
  }
  if (page.url() !== appUrl) throw new Error("the app navigated: " + page.url());
  await shot("pages-links-1280");
});

// ---- the pages listener: its own origin, and nothing but /p ----
await check(pagesUrl ? "listener: pages come from an origin of their own that has no /api, no app, no way in" : "listener: not configured in this pass (pages share the app's origin)", async () => {
  if (!pagesUrl) {
    const fr = frameOf(pages.md) || null;
    const src = await page.evaluate(() => document.querySelector("iframe.pgs-frame")?.src || "");
    if (src && !src.startsWith(base + "/p/")) throw new Error("iframe src " + src);
    return;
  }
  const origin = new URL(pagesUrl).origin;
  if (origin === new URL(base).origin) throw new Error("the pages origin is the app's");
  // The iframe is on the pages origin; the app's CSP names it.
  await page.goto(base + `/#/pages/${pages.chart}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 8000 });
  const src = await page.locator("iframe.pgs-frame").getAttribute("src");
  if (!src.startsWith(origin + "/p/")) throw new Error("iframe src " + src);
  const appCsp = (await page.request.get(base + "/")).headers()["content-security-policy"] || "";
  if (!appCsp.includes("frame-src 'self' " + origin)) throw new Error("app CSP: " + appCsp);
  // Everything but /p/... is a 404 there, with the page policy.
  for (const [m, p] of [["GET", "/api/agents"], ["GET", "/api/pages"], ["GET", "/api/whoami"], ["GET", "/api/events"], ["GET", "/"], ["GET", "/app.js"], ["GET", "/index.html"], ["POST", "/api/todos"], ["POST", "/api/pages"], ["DELETE", "/api/pages/" + pages.chart]]) {
    const r = await fetch(origin + p, { method: m, headers: m === "GET" ? {} : { "Content-Type": "application/json" }, body: m === "GET" || m === "DELETE" ? undefined : "{}" });
    const body = await r.text();
    if (r.status !== 404 || /agents|todos|<html/i.test(body) || !/^sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox;/.test(r.headers.get("content-security-policy") || "")) throw new Error(`${m} ${p} on the pages origin: ${r.status} ${body.slice(0, 40)}`);
  }
  // A page there reaches the pages origin's /api? It does not exist, and the hub's is unreachable.
  const tabs = await desktop.ctx.newPage();
  const resp = await tabs.goto(origin + "/p/" + pages.direct, { waitUntil: "load" });
  if (resp.status() !== 200) throw new Error("page on the listener: " + resp.status());
  await until(tabs, () => window.__done === true);
  const rr = JSON.parse(await tabs.locator("#result").innerText());
  const bad2 = bad(rr, NET);
  if (bad2.length) throw new Error("not blocked on the listener: " + bad2.join("; "));
  await tabs.close();
  // The app's own origin still answers /api (the control is not a 404 everywhere).
  if ((await fetch(base + "/api/agents")).status !== 200) throw new Error("control: the app's /api is gone");
  await shot("pages-listener-1280");
});

// ---- the screen, at both widths ----
await check("screens: the list and an open page at 1280 and 390", async () => {
  await page.goto(base + "/#/pages", { waitUntil: "load" });
  await page.waitForSelector(".pgs-card", { timeout: 8000 });
  const cards = await page.locator(".pgs-card").count();
  if (cards < 5) throw new Error("cards " + cards);
  const titles = (await page.locator(".pgs-title").allInnerTexts()).join("|");
  if (!/Latency by hour/.test(titles) || !/Failover test/.test(titles)) throw new Error("titles " + titles);
  await page.waitForTimeout(300);
  await shot("pages-list-1280");
  await page.goto(base + `/#/pages/${pages.chart}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached" });
  let fr;
  for (let i = 0; i < 60 && !fr; i++) { fr = frameOf(pages.chart); if (!fr) await page.waitForTimeout(100); }
  await fr.waitForSelector(".bar", { timeout: 8000 });
  await page.waitForTimeout(300);
  // Who made it and the sandbox note are behind the (i) button: closed at first, one tap away.
  if (await page.locator("#pgs-infopop").isVisible()) throw new Error("the info popover opened by itself");
  await page.locator("#pgs-infob").click();
  const prov = await page.locator("#pgs-infopop").innerText();
  if (!/Published by .*w2:p1/.test(prov) || !/sandbox/.test(prov)) throw new Error("provenance: " + prov);
  await page.keyboard.press("Escape");
  if (await page.locator("#pgs-infopop").isVisible()) throw new Error("Escape did not close the info popover");
  await shot("pages-open-1280");
  // Focus: the list collapses to its rail while the reader stays; the button toggles it back, and so does leaving the page.
  const collapsed = () => page.evaluate(() => document.body.classList.contains("list-collapsed"));
  const navShown = () => page.evaluate(() => { const r = document.querySelector("#agents")?.getBoundingClientRect(); return !!r && r.width > 100 && r.height > 0; });
  if (await collapsed()) throw new Error("the list was collapsed before focus");
  await page.locator("#pgs-focus").click();
  if (!(await collapsed()) || (await page.locator("#pgs-focus").getAttribute("aria-pressed")) !== "true") throw new Error("focus did not collapse the list");
  if (await navShown()) throw new Error("the list is still visible in focus mode");
  if (!(await page.locator("iframe.pgs-frame").isVisible())) throw new Error("the page went away in focus mode");
  await shot("pages-focus-1280");
  await page.locator("#pgs-focus").click();
  if ((await collapsed()) || (await page.locator("#pgs-focus").getAttribute("aria-pressed")) !== "false" || !(await navShown())) throw new Error("focus off did not bring the list back");
  await page.locator("#pgs-focus").click();
  await page.locator("#pgs-vback").click();
  await page.waitForSelector(".pgs-card", { timeout: 8000 });
  if ((await collapsed()) || !(await navShown())) throw new Error("leaving the page did not bring the list back");
  await page.goto(base + `/#/pages/${pages.chart}`, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached" });
  // filter (the gallery is behind the viewer's back arrow)
  await page.locator("#pgs-vback").click();
  await page.locator("#pgs-q").fill("failover");
  if ((await page.locator(".pgs-card").count()) !== 1) throw new Error("filter left " + (await page.locator(".pgs-card").count()));
  await page.locator("#pgs-q").fill("");
  // the hash opens the same screens on a phone
  const phone = await launch({ width: 390, height: 844, dark, bypassCSP: false });
  const m = phone.page;
  await m.goto(base + "/#/pages", { waitUntil: "load" });
  await m.waitForSelector(".pgs-card", { timeout: 8000 });
  const over = await m.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector(".pgs-list").scrollWidth, document.querySelector(".pgs-list").clientWidth]);
  if (over[0] > over[1] || over[2] > over[3]) throw new Error("list scrolls sideways " + over);
  await m.waitForTimeout(300);
  await m.screenshot({ path: `${out}/pages-list-390.png` });
  await m.locator(`.pgs-card[data-id="${pages.chart}"]`).click();
  await m.waitForSelector("iframe.pgs-frame", { state: "attached" });
  let mf;
  for (let i = 0; i < 60 && !mf; i++) { mf = m.frames().find((f) => f.url().includes("/p/" + pages.chart)); if (!mf) await m.waitForTimeout(100); }
  await mf.waitForSelector(".bar", { timeout: 8000 });
  await m.waitForTimeout(300);
  const over2 = await m.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector(".pgs-bar").scrollWidth, document.querySelector(".pgs-bar").clientWidth]);
  if (over2[0] > over2[1] || over2[2] > over2[3]) throw new Error("page view scrolls sideways " + over2);
  await m.screenshot({ path: `${out}/pages-open-390.png` });
  // back returns to the list, and back again to the app
  await m.locator("#pgs-vback").click();
  await m.waitForSelector(".pgs-card", { state: "visible" });
  await m.locator("#pgs-back").click();
  await m.waitForSelector("#pages", { state: "hidden" });
  await phone.browser.close();
});

await check("search: page text is searched on the hub, hits show under the titles with marked snippets that cannot become markup", async () => {
  const evil = idOf(publish([file("evil.md", "# Notes\n\nthe <img src=x onerror=window.__pgx=1> needlezz is here and the <b>bold</b> lease"), "--title", "Plain notes"]));
  for (const [w, h, tag] of [[1280, 800, "desktop"], [390, 844, "phone"]]) {
    await page.setViewportSize({ width: w, height: h });
    await page.goto("about:blank");
    await page.goto(base + "/#/pages", { waitUntil: "load" });
    await page.waitForSelector(".pgs-card", { timeout: 8000 });
    await page.locator("#pgs-q").fill("needlezz");
    await page.waitForSelector(".pgs-hit", { timeout: 8000 });
    const t = await page.locator("#pgs-items").innerText();
    if (!/IN PAGE TEXT/i.test(t) || !t.includes("Plain notes") || !t.includes("<img src=x onerror=window.__pgx=1>") || !/1 match/.test(t)) throw new Error("hits: " + t);
    const r = await page.evaluate(() => ({ imgs: document.querySelectorAll("#pgs-items img, #pgs-items b").length, x: window.__pgx, marked: document.querySelector("#pgs-items .pgs-snip mark")?.textContent }));
    if (r.imgs || r.x !== undefined || r.marked !== "needlezz") throw new Error("markup: " + JSON.stringify(r));
    const over = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
    if (over[0] > over[1]) throw new Error("sideways " + over);
    await page.waitForTimeout(200);
    await page.screenshot({ path: `${out}/pages-bodysearch-${tag}.png` });
  }
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.locator(".pgs-hit").first().click();
  await page.waitForSelector("iframe.pgs-frame", { state: "attached" });
  if (!(await page.evaluate(() => location.hash)).startsWith("#/pages/" + evil)) throw new Error("hit did not open the page");
  // Nothing in any body: the usual empty notice; one letter is not searched. (The filter is in the gallery, behind the back arrow.)
  await page.locator("#pgs-vback").click();
  await page.locator("#pgs-q").fill("zzzznotthere");
  await page.waitForTimeout(900);
  if (!/No page matches/.test(await page.locator("#pgs-items").innerText()) || await page.locator(".pgs-hit").count()) throw new Error("empty search");
  await page.locator("#pgs-q").fill("");
  await page.locator("#pgs-q").fill("a");
  await page.waitForTimeout(700);
  if (await page.locator(".pgs-hit").count()) throw new Error("one letter searched the bodies");
  await page.locator("#pgs-q").fill("");
});

await check("open session: a Page made in an indexed session names it per version and opens the transcript at the turn", async () => {
  // The Pages here are published with random ids, so the index fixture cannot hold them: the
  // lookup answers as the real route does (tests in internal/server cover the route itself).
  const SMOKE = { host: "devbox", kind: "claude", id: "11111111-aaaa-4aaa-8aaa-111111111111", title: "Retry the upgrade smoke check" };
  const ver = (await (await fetch(base + "/api/pages/" + pages.chart)).json()).page.version;
  const item = (id, n, seq) => ({ session: SMOKE, type: "page", ref: id, detail: "version " + n, seq, ts: new Date(Date.now() - 120000).toISOString() });
  await page.route(/\/api\/recall\/produced\?ref=/, (r) => {
    const id = new URL(r.request().url()).searchParams.get("ref");
    if (id === pages.chart) return r.fulfill({ json: { produced: [item(id, ver, 22)] } });
    if (id === pages.secret) return r.fulfill({ json: { produced: [item(id, 99, 24)] } }); // another version's
    return r.fulfill({ json: { produced: [] } });
  });
  for (const [w, h, tag] of [[1280, 800, "desktop"], [390, 844, "phone"]]) {
    await page.setViewportSize({ width: w, height: h });
    await page.goto("about:blank");
    await page.goto(base + "/#/pages/" + pages.chart, { waitUntil: "load" });
    await page.waitForSelector("#pgs-infob", { timeout: 8000 });
    await page.locator("#pgs-infob").click();
    await page.waitForSelector("#pgs-opensess", { timeout: 8000 });
    const t = await page.locator("#pgs-made").innerText();
    if (!/Made in Retry the upgrade smoke check/.test(t) || !/turn 22/.test(t) || !/Open session/.test(t)) throw new Error("strip: " + t);
    const over = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector(".pgs-bar").scrollWidth, document.querySelector(".pgs-bar").clientWidth]);
    if (over[0] > over[1] || over[2] > over[3]) throw new Error("sideways " + over);
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${out}/pages-opensession-${tag}.png` });
  }
  await page.setViewportSize({ width: 1280, height: 800 });
  if (!(await page.locator("#pgs-opensess").isVisible())) await page.locator("#pgs-infob").click();
  await page.locator("#pgs-opensess").click();
  // (waitForFunction evals a string, which this page's CSP refuses: poll with evaluate.)
  const poll = async (fn, what) => { for (let i = 0; i < 80; i++) { if (await page.evaluate(fn)) return; await page.waitForTimeout(100); } throw new Error("timed out: " + what); };
  await poll(() => location.hash.startsWith("#/search/devbox/claude/11111111-aaaa-4aaa-8aaa-111111111111") && location.hash.includes("seq=22"), "hash");
  await poll(() => /Retry the upgrade/.test(document.querySelector(".rc-stitle")?.textContent || ""), "transcript");
  // A page with no session in the index shows no strip; one whose other versions only were made there says so.
  await page.goto("about:blank");
  await page.goto(base + "/#/pages/" + pages.md, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached" });
  await page.waitForTimeout(400);
  if (await page.locator("#pgs-made").isVisible()) throw new Error("a strip for a page the index does not know");
  await page.goto("about:blank");
  await page.goto(base + "/#/pages/" + pages.secret, { waitUntil: "load" });
  await poll(() => /No session found for this version/.test(document.querySelector("#pgs-made")?.textContent || ""), "other versions note");
  await page.unroute(/\/api\/recall\/produced\?ref=/);
  // The hub's publish stamp names the session when the index matched nothing; the link shows only
  // if the index has that session (it does for devbox/claude/11111111…, not for the other id).
  const STAMP = { host: "devbox", kind: "claude", id: "11111111-aaaa-4aaa-8aaa-111111111111" };
  let stampWith = STAMP;
  await page.route(/\/api\/pages(\?.*)?$/, async (r) => {
    const resp = await r.fetch();
    const j = await resp.json();
    for (const p of j.pages || []) if (p.id === pages.md) p.versions[p.versions.length - 1].session = stampWith;
    await r.fulfill({ response: resp, json: j });
  });
  await page.route(/\/api\/recall\/sessions\/devbox\/claude\/11111111[^/?]*\?limit=1$/, (r) => r.fulfill({ json: { session: { ...SMOKE }, messages: [] } }));
  await page.route(/\/api\/recall\/sessions\/devbox\/claude\/gone[^/?]*\?limit=1$/, (r) => r.fulfill({ status: 404, json: { code: "no_session", message: "no such session" } }));
  await page.route(/\/api\/recall\/produced\?ref=/, (r) => r.fulfill({ json: { produced: [] } }));
  await page.goto("about:blank");
  await page.goto(base + "/#/pages/" + pages.md, { waitUntil: "load" });
  await poll(() => /Made in Retry the upgrade smoke check · devbox/.test(document.querySelector("#pgs-made")?.textContent || ""), "stamped strip");
  if (await page.locator("#pgs-made").innerText().then((t) => /turn/.test(t))) throw new Error("a turn without the index match");
  if (!/#\/search\/devbox\/claude\/11111111/.test(await page.locator("#pgs-opensess").getAttribute("href"))) throw new Error("stamp link");
  if (!(await page.locator("#pgs-opensess").isVisible())) await page.locator("#pgs-infob").click();
  await page.screenshot({ path: `${out}/pages-stamp-desktop.png` });
  stampWith = { host: "devbox", kind: "claude", id: "gone-0000" };
  await page.goto("about:blank");
  await page.goto(base + "/#/pages/" + pages.md, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached" });
  await page.waitForTimeout(600);
  if (await page.locator("#pgs-made").isVisible()) throw new Error("a strip for a session the index lacks");
  await page.unroute(/\/api\/pages(\?.*)?$/);
  await page.unroute(/\/api\/recall\/produced\?ref=/);
});

await check("gallery: grid and list, chips, sort, and the new/updated dot", async () => {
  await page.goto(base + "/#/pages", { waitUntil: "load" });
  await page.waitForSelector(".pgs-card", { timeout: 8000 });
  // Desktop, no page open: the gallery has the width, the viewer pane is gone, the cards are a grid.
  const g = await page.evaluate(() => { const it = document.querySelector("#pgs-items"), cs = getComputedStyle(it); return [document.querySelector(".pgs-list").clientWidth, getComputedStyle(document.querySelector(".pgs-view")).display, cs.display, cs.gridTemplateColumns.split(" ").length, it.className]; });
  if (g[0] < 900 || g[1] !== "none" || g[2] !== "grid" || g[3] < 3) throw new Error("gallery layout " + g);
  await shot("pages-gallery-1280");
  // Grid or list, remembered on this device.
  await page.locator('#pgs-views [data-view="list"]').click();
  if ((await page.locator("#pgs-items.pgs-rows").count()) !== 1) throw new Error("list view not shown");
  await page.reload({ waitUntil: "load" });
  await page.waitForSelector("#pgs-items.pgs-rows .pgs-card", { timeout: 8000 });
  await page.locator('#pgs-views [data-view="grid"]').click();
  if ((await page.locator("#pgs-items.pgs-grid").count()) !== 1) throw new Error("grid view not shown");
  // Chips: Revised keeps only pages with more than one version; All brings them back.
  const all = await page.locator(".pgs-card").count();
  await page.locator('.pgs-chip[data-chip="rev"]').click();
  const revCards = await page.locator(".pgs-card").count();
  const pills = (await page.locator(".pgs-card .pgs-pill").allInnerTexts()).filter((x) => x === "v1").length;
  if (revCards < 1 || revCards >= all || pills) throw new Error("revised chip: " + revCards + " of " + all + ", v1 pills " + pills);
  await page.locator('.pgs-chip[data-chip="all"]').click();
  if ((await page.locator(".pgs-card").count()) !== all) throw new Error("All chip did not restore the cards");
  // Sort by title.
  await page.locator("#pgs-sort").selectOption("title");
  const titles = await page.locator(".pgs-title").allInnerTexts();
  const sorted = titles.slice().sort((x, y) => x.localeCompare(y));
  if (titles.join("|") !== sorted.join("|")) throw new Error("title sort: " + titles.join("|"));
  await page.locator("#pgs-sort").selectOption("updated");
  // The dot: a page published after the first look is "new" until it is opened; a republish is "updated".
  publish(["-", "--key", "dotprobe", "--title", "Dot probe"], "# Dot probe\n\nfirst\n", "w1:p1");
  await page.reload({ waitUntil: "load" });
  const card = () => page.locator(".pgs-card", { hasText: "Dot probe" });
  await card().waitFor({ timeout: 8000 });
  if ((await card().locator(".pgs-dot").innerText().catch(() => "")) !== "new") throw new Error("no new dot on a page never opened");
  await card().click();
  await page.waitForSelector("iframe.pgs-frame", { state: "attached" });
  // In the pane: the viewer replaces the gallery (no column beside it), the nav and the agent list stay, and the back arrow
  // (shown only now) returns to the gallery.
  const c = await page.evaluate(() => {
    const vis = (s) => { const e = document.querySelector(s); if (!e) return false; const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0 && getComputedStyle(e).visibility !== "hidden"; };
    return { list: vis(".pgs-list"), view: vis("#pgs-view"), nav: vis("#nav"), pagesRow: !!document.querySelector('#nav .nv.on[data-page="pages"]'), back: vis("#pgs-vback"), pane: Math.round(document.querySelector("#pages").getBoundingClientRect().left) };
  });
  if (c.list || !c.view || !c.nav || !c.pagesRow || !c.back || c.pane < 300) throw new Error("open page in the pane " + JSON.stringify(c));
  await page.locator("#pgs-vback").click();
  await page.waitForSelector("#pages:not(.in-page) .pgs-card");
  if (await card().locator(".pgs-dot").count()) throw new Error("dot stays after the page was opened");
  publish(["-", "--key", "dotprobe", "--title", "Dot probe"], "# Dot probe\n\nsecond\n", "w1:p1");
  await page.reload({ waitUntil: "load" });
  await card().waitFor({ timeout: 8000 });
  if ((await card().locator(".pgs-dot").innerText().catch(() => "")) !== "updated") throw new Error("no updated dot after a republish");
  // Sort by recently opened puts the page just opened first among those opened.
  await card().click();
  await page.waitForSelector("iframe.pgs-frame", { state: "attached" });
  await page.locator("#pgs-vback").click();
  await page.locator("#pgs-sort").selectOption("opened");
  if (!/Dot probe/.test((await page.locator(".pgs-title").first().innerText()))) throw new Error("opened sort");
  await page.locator("#pgs-sort").selectOption("updated");
  // Phone: rows by default (a title in full beside a small preview), two columns when the reader picks the grid; no sideways scroll.
  const ph = await launch({ width: 390, height: 844, dark, bypassCSP: false });
  await ph.page.goto(base + "/#/pages", { waitUntil: "load" });
  await ph.page.waitForSelector(".pgs-card", { timeout: 8000 });
  if ((await ph.page.locator("#pgs-items.pgs-rows").count()) !== 1) throw new Error("the phone does not start on rows");
  const wide = await ph.page.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector(".pgs-card").getBoundingClientRect().width]);
  if (wide[0] > wide[1] || wide[2] < 300) throw new Error("phone rows " + wide);
  await ph.page.screenshot({ path: `${out}/pages-rows-390.png` });
  await ph.page.locator('#pgs-views button[data-view="grid"]').click();
  const cols = await ph.page.evaluate(() => [getComputedStyle(document.querySelector("#pgs-items")).gridTemplateColumns.split(" ").length, document.documentElement.scrollWidth, innerWidth]);
  if (cols[0] !== 2 || cols[1] > cols[2]) throw new Error("phone grid " + cols);
  await ph.page.screenshot({ path: `${out}/pages-gallery-390.png` });
  await ph.browser.close();
});

await check("pages: an empty gallery says so, with the command as a copy button, in the pane and on a phone", async () => {
  for (const [w, h, tag] of [[1280, 800, "desktop"], [390, 844, "phone"]]) {
    const e = await launch({ width: w, height: h, dark, bypassCSP: false });
    await e.page.route(/\/api\/pages$/, (r) => r.fulfill({ contentType: "application/json", body: JSON.stringify({ pages: [], pages_url: "", public_url: "" }) }));
    await e.page.goto(base + "/#/pages", { waitUntil: "load" });
    await e.page.waitForSelector(".pgs-zero", { timeout: 8000 });
    const z = await e.page.evaluate(() => { const b = document.querySelector(".pgs-zero .cp"); return { text: document.querySelector(".pgs-zero").innerText, copy: b && b.dataset.copy, over: document.documentElement.scrollWidth > innerWidth }; });
    if (!/No pages yet/.test(z.text) || z.copy !== "comebye publish report.html" || z.over) throw new Error("empty state " + JSON.stringify(z));
    await e.page.screenshot({ path: `${out}/pages-empty-${tag}.png` });
    await e.browser.close();
  }
});

await check("previews: a card shows its page's skeleton, escaped, per version, fetched by version", async () => {
  const doc = (lead) => `<!doctype html><title>Preview probe</title><body><h1>Preview probe</h1><p>${lead}</p><h2>Heading one</h2><h2>Heading two</h2><table></table><script>document.title="X"</script>`;
  publish(["-", "--key", "pvprobe", "--title", "Preview probe"], doc("First paragraph quotes &lt;img src=x onerror=window.__pv=1&gt; as text, and is long enough to lead."), "w1:p1");
  const reqs = [];
  const onReq = (r) => { if (/\/preview\?/.test(r.url())) reqs.push(r.url()); };
  page.on("request", onReq);
  await page.goto("about:blank");
  await page.goto(base + "/#/pages", { waitUntil: "load" });
  const card = () => page.locator(".pgs-card", { hasText: "Preview probe" });
  await card().locator(".pgs-lead").waitFor({ timeout: 8000 });
  const lead = await card().locator(".pgs-lead").innerText();
  if (!lead.includes("<img src=x onerror=window.__pv=1>")) throw new Error("the page's text is not shown as text: " + lead);
  if ((await card().locator(".pgs-thumb img").count()) || (await page.evaluate(() => window.__pv))) throw new Error("page text became markup");
  const heads = await card().locator(".pgs-heads span").allInnerTexts();
  const flags = await card().locator(".pgs-flags span").allInnerTexts();
  if (heads.join("|") !== "Heading one|Heading two" || flags.join("|") !== "table") throw new Error("skeleton " + heads + " / " + flags);
  if (!reqs.length || reqs.some((u) => !/[?]v=\d+$/.test(u))) throw new Error("preview requests are not by version: " + reqs.join(" "));
  await shot("pages-previews-1280");
  // A republish is a new version with its own preview.
  publish(["-", "--key", "pvprobe", "--title", "Preview probe"], doc("The second version leads with different words that are long enough to read."), "w1:p1");
  await page.reload({ waitUntil: "load" });
  await card().locator(".pgs-lead", { hasText: "second version" }).waitFor({ timeout: 8000 });
  if (!reqs.some((u) => /[?]v=2$/.test(u))) throw new Error("no request for v2: " + reqs.join(" "));
  // With a page open the viewer replaces the gallery: no card is on screen to carry a thumbnail.
  await card().click();
  await page.waitForSelector("iframe.pgs-frame", { state: "attached" });
  const shown = await page.evaluate(() => [location.hash, document.getElementById("pages").className, innerWidth, document.querySelector(".pgs-list").getBoundingClientRect().width]);
  if (shown[3] !== 0) throw new Error("the gallery shows beside an open page " + shown);
  page.off("request", onReq);
  const ph = await launch({ width: 390, height: 844, dark, bypassCSP: false });
  await ph.page.goto(base + "/#/pages", { waitUntil: "load" });
  await ph.page.locator(".pgs-card .pgs-lead").first().waitFor({ timeout: 8000 });
  const o = await ph.page.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector(".pgs-list").scrollWidth, document.querySelector(".pgs-list").clientWidth]);
  if (o[0] > o[1] || o[2] > o[3]) throw new Error("previews scroll sideways " + o);
  await ph.page.screenshot({ path: `${out}/pages-previews-390.png` });
  await ph.browser.close();
});

await check("links: a to-do link to a page opens it in the app; delete asks and removes", async () => {
  // a to-do item whose link is the page's URL
  const link = base + "/p/" + pages.secret;
  const added = await (await fetch(base + "/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ question: "Read the report", kind: "action", no_wait: true, links: [{ label: "Report", url: link }] }) })).json();
  await page.goto("about:blank"); // a fresh load: the to-do list is read on load
  await page.goto(base + "/#/todo", { waitUntil: "load" });
  // Select the item; its link is shown in the detail pane.
  await page.locator("#p-todo").getByText("Read the report").first().click();
  const lk = page.locator(`#p-todo a[href="${link}"]`).filter({ visible: true }).first();
  await lk.waitFor({ state: "visible", timeout: 8000 });
  await lk.click();
  await until(page, (id) => location.hash === "#/pages/" + id, pages.secret, 4000);
  await page.waitForSelector("iframe.pgs-frame", { state: "attached" });
  // delete: the app asks first
  let asked = "";
  page.once("dialog", (d) => { asked = d.message(); d.accept(); });
  await page.locator("#pgs-menub").click();
  await page.locator("#pgs-del").click();
  await until(page, (id) => !document.querySelector(`.pgs-card[data-id="${id}"]`), pages.secret, 5000);
  if (!/Delete/.test(asked)) throw new Error("no confirmation: " + asked);
  const r = await fetch(base + "/p/" + pages.secret);
  if (r.status !== 404) throw new Error("deleted page answers " + r.status);
});

fs.writeFileSync(`${out}/pages-browser.log`, log.join("\n") + "\n");
fs.writeFileSync(`${out}/pages-probe.json`, JSON.stringify(probeResult, null, 2) + "\n");
await desktop.browser.close();
fs.rmSync(work, { recursive: true, force: true });
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"}  ${name}${ok ? "" : "  -- " + why}`); if (!ok) failed++; }
process.exit(failed ? 1 : 0);
