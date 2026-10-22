// Browser test for the chat's markdown rendering (web/app.js renderMarkdown
// with the vendored marked + DOMPurify), run in headless Chrome against a
// live herdr-phone so the real CSP and script loading apply. No npm
// dependencies: Node 22+ (global WebSocket) and a Chrome/Chromium binary.
//
// Point it at an instance started without an allowlist on its own port,
// e.g. `herdr-phone -addr 127.0.0.1:19741 -insecure-no-auth -push=false`
// (headless Chrome sends no Tailscale identity):
//
//   node web/test/markdown.test.mjs http://127.0.0.1:19741/ \
//     [--chrome google-chrome] [--port 19733] \
//     [--agent '<host>/<pane>' --screenshot out.png]
//
// With --agent, it also opens that agent's chat at 360px wide, checks the
// page does not scroll sideways, and optionally saves a screenshot.
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const args = process.argv.slice(2);
const opt = (name, def) => {
  const i = args.indexOf("--" + name);
  return i >= 0 ? args[i + 1] : def;
};
const base = args.find((a) => /^https?:\/\//.test(a));
if (!base) {
  console.error("usage: markdown.test.mjs <herdr-phone URL> [--chrome bin] [--port n] [--agent id --screenshot file]");
  process.exit(2);
}
const chromeBin = opt("chrome", process.env.CHROME || "google-chrome");
const port = Number(opt("port", "19733"));
const agent = opt("agent");
const shot = opt("screenshot");

// The fixture: every markdown feature agents use, then XSS payloads.
export const FIXTURE = String.raw`# Heading one
## Heading two
### Heading three

Plain paragraph with **bold**, *italic*, ~~struck~~, ` + "`inline <b>code</b>`" + String.raw` and a [docs link](https://example.com/docs "Docs").
Second line of the same paragraph (soft break). Bare URL: https://example.org/path and Vec<String> stays literal.

- level one
  - level two
    - level three
- back to one

3. third
4. fourth
   1. nested ordered

- [x] done task
- [ ] open task

| Left | Center | Right |
|:-----|:------:|------:|
| a | b | c |
| a much longer cell that should make the table scroll sideways on a phone | ` + "`x`" + String.raw` | 12345678901234567890 |

> A quote
> > nested quote

---

` + "```go\nfunc main() {\n\tfmt.Println(\"hi\") // a very long line that has to scroll horizontally inside the code block\n}\n```" + String.raw`

` + "```html\n<script>alert('in code')</script>\n<img src=x onerror=alert(1)>\n```" + String.raw`

![diagram](https://example.com/d.png) and ![inline data](data:image/png;base64,AAAA)

<details>
<summary>More</summary>

Hidden **markdown**.
</details>

Press <kbd>Ctrl</kbd>+<kbd>C</kbd>.<br>After break.

XSS-1 <script>window.__xss=1;alert(1)</script>

XSS-2 <img src=x onerror="window.__xss=2">

XSS-3 [js link](javascript:alert(3)) [mixed case](JaVaScRiPt:alert(3)) [entity](&#106;avascript:alert(3)) [data](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==) [relative](src/main.go) [mail](mailto:a@example.com)

XSS-4 <a href="javascript:alert(4)">raw anchor</a>

<div onclick="alert(5)" class="ans-head jump-btn" style="position:fixed;inset:0" id="answer">XSS-5 div</div>

<details open ontoggle="alert(6)"><summary>XSS-6</summary>x</details>

<iframe src="https://example.com"></iframe>
<form action="/api/x"><input type="text" name="q"><button>go</button></form>
<svg onload="alert(7)"><script>alert(7)</script></svg>
<style>body{display:none}</style>
<math><mtext><table><mglyph><style><img src=x onerror=alert(8)>

[click me](javascript:alert(9) "t")
`;

// inPage runs in the app's page: renders the fixture and checks it.
function inPage(fixture) {
  const out = [];
  const ok = (cond, msg) => out.push({ ok: !!cond, msg });
  ok(typeof renderMarkdown === "function", "renderMarkdown is defined");
  ok(window.marked && window.DOMPurify, "vendored libraries loaded");
  const html = renderMarkdown(fixture, "test:1");
  ok(renderMarkdown(fixture, "test:1") === html, "second render of the same message comes from the cache");
  const host = document.createElement("div");
  host.className = "bubble md";
  host.innerHTML = html;
  const q = (s) => host.querySelector(s);
  const qa = (s) => [...host.querySelectorAll(s)];
  const text = host.textContent;

  // Features.
  ok(q("h1") && q("h2") && q("h3"), "h1-h3 rendered");
  ok(q("strong") && q("em") && q("del"), "bold, italic, strikethrough");
  ok(qa("p > code").some((c) => c.textContent === "inline <b>code</b>"), "inline code is literal");
  ok(q('a[href="https://example.com/docs"][title="Docs"]'), "titled link");
  ok(q('a[href="https://example.org/path"]'), "bare URL autolinked");
  ok(text.includes("Vec<String> stays literal"), "unknown raw tag shown as text");
  ok(q("p br"), "soft line break becomes <br>");
  ok(q("ul > li > ul > li > ul > li"), "three-level nested list");
  ok(q('ol[start="3"] > li > ol > li'), "ordered list keeps start and nests");
  const boxes = qa('input[type="checkbox"]');
  ok(boxes.length === 2 && boxes.every((b) => b.disabled) && boxes.filter((b) => b.checked).length === 1, "task list: two disabled checkboxes, one checked");
  ok(q(".md-table > table > thead > tr > th"), "table wrapped in scroll box");
  ok(q('th[align="center"]') && q('td[align="right"]'), "table alignment");
  ok(q("blockquote > blockquote"), "nested blockquote");
  ok(q("hr"), "horizontal rule");
  ok(q("pre > code.language-go"), "fenced code keeps its language class");
  const htmlCode = q("pre > code.language-html");
  ok(htmlCode && htmlCode.textContent.includes("<script>alert('in code')</script>"), "HTML inside a code block is literal text");
  const img = qa("a").find((a) => a.textContent.includes("diagram"));
  ok(img && img.getAttribute("href") === "https://example.com/d.png", "remote image becomes a link");
  ok(text.includes("🖼 inline data") && !qa("a").some((a) => a.textContent.includes("inline data")), "data: image becomes plain text");
  ok(q("details > summary") && q("details strong"), "raw <details> with markdown inside");
  ok(qa("kbd").length === 2, "raw <kbd> kept");
  ok(q('a[href="mailto:a@example.com"]'), "mailto link kept");

  // Sanitization.
  ok(!q("script, style, iframe, object, embed, form, button, svg, math, img, link, meta, base, textarea, select"), "no dangerous elements");
  const all = qa("*");
  ok(all.every((el) => [...el.attributes].every((a) => !/^on/i.test(a.name))), "no event handler attributes");
  ok(all.every((el) => !el.hasAttribute("style") && !el.hasAttribute("id") && !el.hasAttribute("name")), "no style/id/name attributes");
  ok(all.every((el) => !el.hasAttribute("class") || /^(language-[\w+#.-]+|md-table)( |$)/.test(el.getAttribute("class"))), "only renderer classes survive");
  ok(qa("[href]").every((el) => /^(https?:|mailto:)/i.test(el.getAttribute("href"))), "only http(s)/mailto hrefs");
  ok(qa("a[href]").every((a) => a.target === "_blank" && a.rel === "noopener noreferrer"), "links open in a new tab, noopener noreferrer");
  ok(qa("input").every((i) => i.type === "checkbox" && i.disabled), "only disabled checkboxes");
  ok(all.every((el) => [...el.attributes].every((a) => !/javascript:|data:/i.test(a.value))), "no javascript:/data: in any attribute");
  ok(!qa("details").some((d) => d.open), "no forced-open details");
  ok(text.includes("<script>window.__xss=1;alert(1)</script>"), "raw <script> shown as text, not run");
  ok(text.includes('<a href="javascript:alert(4)">'), "raw javascript: anchor shown as text");
  ok(qa("a:not([href])").some((a) => a.textContent === "js link"), "javascript: markdown link loses its href");
  ok(qa("a:not([href])").some((a) => a.textContent === "relative"), "relative link loses its href");
  ok(text.includes("XSS-5 div"), "harmless content of stripped markup kept");

  // Live insertion must not run anything either.
  window.__xss = 0;
  const live = document.createElement("div");
  live.className = "md";
  live.style.cssText = "position:absolute;left:-9999px;width:300px";
  live.innerHTML = html;
  document.body.appendChild(live);

  // Over-long text falls back to escaped plain text.
  const big = renderMarkdown("<b>x</b>" + "y".repeat(100001));
  ok(big.startsWith('<div class="md-plain">&lt;b&gt;'), "over-long message rendered as escaped plain text");
  return new Promise((resolve) => setTimeout(() => {
    ok(window.__xss === 0, "no payload executed after insertion");
    live.remove();
    resolve(out);
  }, 300));
}

// --- minimal CDP client ---
function cdp(wsUrl) {
  const ws = new WebSocket(wsUrl);
  let id = 0;
  const pending = new Map();
  const listeners = [];
  ws.onmessage = (ev) => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) {
      const { res, rej } = pending.get(m.id);
      pending.delete(m.id);
      m.error ? rej(new Error(m.error.message)) : res(m.result);
    } else if (m.method) listeners.forEach((f) => f(m));
  };
  return new Promise((resolve, reject) => {
    ws.onerror = reject;
    ws.onopen = () => resolve({
      send: (method, params = {}) => new Promise((res, rej) => {
        const i = ++id;
        pending.set(i, { res, rej });
        ws.send(JSON.stringify({ id: i, method, params }));
      }),
      on: (f) => listeners.push(f),
      close: () => ws.close(),
    });
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function waitFor(c, expr, ms = 10000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    const r = await c.send("Runtime.evaluate", { expression: expr, returnByValue: true });
    if (r.result && r.result.value) return true;
    await sleep(100);
  }
  throw new Error("timed out waiting for " + expr);
}

async function main() {
  const profile = mkdtempSync(join(tmpdir(), "mdtest-chrome-"));
  const chrome = spawn(chromeBin, [
    "--headless=new", `--remote-debugging-port=${port}`, `--user-data-dir=${profile}`,
    "--no-first-run", "--no-default-browser-check", "--disable-gpu", "about:blank",
  ], { stdio: "ignore" });
  let failed = 0;
  try {
    let ver;
    for (let i = 0; i < 100 && !ver; i++) {
      try { ver = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json(); } catch { await sleep(100); }
    }
    if (!ver) throw new Error("chrome did not start");
    const target = await (await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, { method: "PUT" })).json();
    const c = await cdp(target.webSocketDebuggerUrl);
    const dialogs = [];
    const errors = [];
    c.on((m) => {
      if (m.method === "Page.javascriptDialogOpening") {
        dialogs.push(m.params.message);
        c.send("Page.handleJavaScriptDialog", { accept: false });
      }
      if (m.method === "Runtime.exceptionThrown") errors.push(m.params.exceptionDetails.text);
    });
    await c.send("Page.enable");
    await c.send("Runtime.enable");
    await c.send("Emulation.setDeviceMetricsOverride", { width: 360, height: 780, deviceScaleFactor: 2, mobile: true });
    await c.send("Page.navigate", { url: base });
    await waitFor(c, "typeof renderMarkdown === 'function' && document.readyState === 'complete'");

    const r = await c.send("Runtime.evaluate", {
      expression: `(${inPage.toString()})(${JSON.stringify(FIXTURE)})`,
      awaitPromise: true, returnByValue: true,
    });
    if (r.exceptionDetails) throw new Error("in-page test threw: " + JSON.stringify(r.exceptionDetails));
    for (const t of r.result.value) {
      console.log((t.ok ? "ok   " : "FAIL ") + t.msg);
      if (!t.ok) failed++;
    }

    if (agent) {
      await c.send("Page.navigate", { url: base.replace(/\/?$/, "/") + "#/agent/" + agent });
      await waitFor(c, "document.querySelectorAll('#chat .msg.assistant .bubble.md').length > 0");
      await sleep(500);
      // Open thinking/compaction so their lazy markdown renders too.
      await c.send("Runtime.evaluate", { expression: "document.querySelectorAll('#chat details.thinking, #chat details.compaction').forEach(d => d.open = true)" });
      await sleep(300);
      const v = (await c.send("Runtime.evaluate", {
        returnByValue: true,
        expression: `(() => {
          const chat = document.getElementById('chat');
          const de = document.documentElement;
          return {
            pageOverflow: de.scrollWidth - de.clientWidth,
            chatOverflow: chat.scrollWidth - chat.clientWidth,
            rich: document.querySelectorAll('#chat .bubble.md table, #chat .bubble.md pre, #chat .bubble.md h2').length,
            thinkMd: !!document.querySelector('#chat .think-body.md[data-md] *'),
            compactMd: !!document.querySelector('#chat .compaction .note.md[data-md] *'),
            userPlain: [...document.querySelectorAll('#chat .msg.user .bubble')].every(b => b.children.length === 0),
          };
        })()`,
      })).result.value;
      const check = (cond, msg) => { console.log((cond ? "ok   " : "FAIL ") + msg); if (!cond) failed++; };
      check(v.pageOverflow <= 0, `no horizontal page overflow at 360px (${v.pageOverflow})`);
      check(v.chatOverflow <= 0, `chat column does not scroll sideways (${v.chatOverflow})`);
      check(v.rich > 0, "chat shows rendered markdown");
      check(v.thinkMd, "thinking block renders markdown when opened");
      check(v.compactMd, "compaction summary renders markdown when opened");
      check(v.userPlain, "user messages stay plain text");
      if (shot) {
        // Scroll the first rich bubble into view and capture the viewport.
        await c.send("Runtime.evaluate", { expression: "document.querySelector('#chat .bubble.md h2, #chat .bubble.md table')?.scrollIntoView({block:'start'})" });
        await sleep(300);
        const png = await c.send("Page.captureScreenshot", { format: "png" });
        writeFileSync(shot, Buffer.from(png.data, "base64"));
        console.log("screenshot: " + shot);
      }
    }
    const check = (cond, msg) => { console.log((cond ? "ok   " : "FAIL ") + msg); if (!cond) failed++; };
    check(dialogs.length === 0, "no alert/confirm dialogs (" + dialogs.join(", ") + ")");
    check(errors.length === 0, "no uncaught page errors (" + errors.join("; ") + ")");
    c.close();
  } finally {
    chrome.kill("SIGTERM");
    await sleep(300);
    rmSync(profile, { recursive: true, force: true });
  }
  console.log(failed ? `${failed} failed` : "all passed");
  process.exit(failed ? 1 : 0);
}

main().catch((e) => { console.error(e); process.exit(1); });
