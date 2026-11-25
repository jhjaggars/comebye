// Opening the app with no network (issue #136, slice 5): sw.js keeps the app shell, so a cold open offline loads the
// app and the lists saved on the device (slice 4) show under the Offline strip. Phone (390) and desktop (1280):
// - the hub stamps its build version into sw.js, and the cache holds only the shell (no /api/*, no Pages);
// - load online, go offline (context.setOffline), reload: the app loads (no browser offline page), the saved rows show,
//   the strip says Offline; /api/* fails as a network error and never comes from the worker;
// - online again: live, no strip, rows not marked old;
// - a new release (through a small proxy here that rewrites sw.js's version and marks index.html): the next load shows
//   it at once (network first), the new worker replaces the old cache, and offline the new shell is the one served.
//
//   node shell.mjs BASE OUT_DIR       (env: SCHEME)
// The hub is never changed; the "new release" exists only in the proxy.
import http from "node:http";
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  if (process.env.ONLY && !new RegExp(process.env.ONLY).test(name)) return;
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const SHELL = ["/", "/themes.js", "/vendor/marked.umd.js", "/vendor/purify.min.js", "/app.js", "/unlockcrypto.js", "/keys.js",
  "/sidepanel.js", "/stt.js", "/manifest.webmanifest", "/favicon.svg", "/icon-192.png", "/icon-512.png"];
const stripText = (page) => page.evaluate(() => { const e = document.querySelector("#hub-strip"); return e && !e.hidden ? e.innerText.replace(/\s+/g, " ").trim() : ""; });
const hubVersion = async () => (await (await fetch(base + "/api/version")).json()).version;

// shellCaches: the worker's caches, by name, with the paths each holds.
const shellCaches = (page) => page.evaluate(async () => {
  const o = {};
  for (const n of await caches.keys()) o[n] = (await (await caches.open(n)).keys()).map((r) => new URL(r.url).pathname).sort();
  return o;
});
// ready: the worker controls the page and its cache for `version` holds the whole shell.
async function ready(page, version, ms = 15000) {
  const end = Date.now() + ms;
  for (;;) {
    const ctl = await page.evaluate(() => !!navigator.serviceWorker.controller);
    const c = await shellCaches(page);
    const have = c["comebye-shell-" + version] || [];
    if (ctl && SHELL.every((p) => have.includes(p)) && Object.keys(c).length === 1) return c;
    if (Date.now() > end) throw new Error(`worker not ready for ${version}: controlled ${ctl}, caches ${JSON.stringify(c)}`);
    await sleep(250);
  }
}
const isApp = (page) => page.evaluate(() => !!document.querySelector("#agents") && typeof agentsSrc !== "undefined");

// proxy: forwards everything to the hub; after release(), sw.js has another VERSION and the page carries a marker,
// as a new build of the hub would serve.
function proxy() {
  const up = new URL(base);
  let next = false;
  const srv = http.createServer((req, res) => {
    const path = req.url.split("?")[0];
    const rewrite = next && (path === "/sw.js" || path === "/" || path === "/index.html");
    const headers = { ...req.headers };
    if (rewrite) { delete headers["if-none-match"]; delete headers["if-modified-since"]; delete headers["accept-encoding"]; }
    const p = http.request({ host: up.hostname, port: up.port, method: req.method, path: req.url, headers }, (r) => {
      if (!rewrite || r.statusCode !== 200) { res.writeHead(r.statusCode, r.headers); r.pipe(res); return; }
      const parts = [];
      r.on("data", (b) => parts.push(b));
      r.on("end", () => {
        let body = Buffer.concat(parts).toString();
        body = path === "/sw.js" ? body.replace(/const VERSION = "([^"]*)";/, 'const VERSION = "$1n2";') : body.replace("<head>", '<head><meta name="uitest-build" content="2">');
        const h = { ...r.headers, etag: '"uitest-n2-' + path.length + '"' };
        delete h["content-length"];
        res.writeHead(200, h);
        res.end(body);
      });
    });
    p.on("error", () => { res.writeHead(502); res.end(); });
    req.pipe(p);
  });
  return new Promise((r) => srv.listen(0, "127.0.0.1", () => r({ url: `http://127.0.0.1:${srv.address().port}`, release: () => { next = true; }, close: () => srv.close() })));
}

async function phase(width) {
  const desk = width > 500, tag = desk ? "desktop" : "phone";
  const open = async () => {
    const { browser, ctx, page } = await launch({ width, height: desk ? 800 : 844, dark });
    const errs = [], fromSW = [];
    page.on("pageerror", (e) => errs.push(e.message));
    page.on("response", (r) => { if (/\/api\//.test(r.url()) && r.fromServiceWorker()) fromSW.push(r.url()); });
    return { browser, ctx, page, errs, fromSW };
  };
  // load: open the app at `at` and wait until the hub's list is in and saved on the device.
  const load = async (page, at) => {
    await page.goto(at + "/");
    await page.waitForFunction(() => typeof agentsSrc !== "undefined" && agentsSrc.at > 0 && !agentsSrc.saved, null, { timeout: 10000 });
  };
  const version = await hubVersion();

  await check(`${tag}: the hub stamps its version into sw.js`, async () => {
    const t = await (await fetch(base + "/sw.js")).text();
    if (!t.includes(`const VERSION = "${version}";`)) throw new Error("sw.js has no VERSION " + version + ": " + (t.match(/const VERSION.*/) || ["none"])[0]);
  });

  await check(`${tag}: a cold open offline loads the app, shows the saved list and says Offline; back online it goes live`, async () => {
    const { browser, ctx, page, errs, fromSW } = await open();
    try {
      await load(page, base);
      const c = await ready(page, version);
      const kept = Object.values(c).flat().filter((p) => !SHELL.includes(p));
      if (kept.length) throw new Error("the cache holds more than the shell: " + kept);
      // Let slice 4's store catch up: it writes a fresh list within 10 s of its answer.
      const dump = () => page.evaluate(() => new Promise((res) => {
        const rq = indexedDB.open("comebye-cache");
        rq.onerror = () => res([]);
        rq.onsuccess = () => { const db = rq.result; if (!db.objectStoreNames.contains("src")) { db.close(); res([]); return; } const g = db.transaction("src").objectStore("src").getAll(); g.onsuccess = () => { db.close(); res(g.result.map((r) => r.name)); }; g.onerror = () => { db.close(); res([]); }; };
      }));
      // Every list answered so far, not just the agents: the strip follows the screen in front (a desktop can open on
      // To do), and a screen with nothing saved says so in its body instead.
      await page.evaluate(() => { cacheFlush(true); });
      for (let end = Date.now() + 20000; ; await sleep(250)) {
        const pending = await page.evaluate(() => cache.dirty.size + (cache.flushing ? 1 : 0));
        if ((await dump()).includes("agents") && !pending) break;
        if (Date.now() > end) throw new Error(`not all saved: store ${await dump()}, ${pending} waiting`);
      }
      await ctx.setOffline(true);
      await page.reload();
      if (page.url().startsWith("chrome-error:") || !(await isApp(page))) throw new Error("no app offline: " + page.url() + " " + (await page.evaluate(() => document.body.innerText.slice(0, 200))));
      await page.waitForFunction(() => document.querySelectorAll("#agents .agent").length > 0, null, { timeout: 5000 }).catch(async () => { throw new Error("no saved rows offline: " + JSON.stringify(await page.evaluate(() => [document.querySelector("#agents")?.innerText.slice(0, 300), document.getElementById("hub-strip")?.innerText]))); });
      await page.waitForFunction(() => { const e = document.getElementById("hub-strip"); return e && !e.hidden && e.dataset.kind === "offline"; }, null, { timeout: 5000 })
        .catch(async () => { throw new Error("strip: " + JSON.stringify(await stripText(page))); });
      const s = await stripText(page);
      if (!/^Offline · /.test(s)) throw new Error("strip: " + JSON.stringify(s));
      if (!(await page.evaluate(() => document.querySelector("#agents").classList.contains("stale")))) throw new Error("the saved rows are not marked old");
      await sleep(1000); // let the saved To do card draw
      await page.screenshot({ path: `${out}/shell-offline-${tag}.png` });
      // /api/* is a network error offline: the worker never answers it from a cache.
      const api = await page.evaluate(() => fetch("/api/version").then((r) => "answered " + r.status, () => "failed"));
      if (api !== "failed") throw new Error("/api/version offline was " + api);
      // Back online: live.
      await ctx.setOffline(false);
      await page.waitForFunction(() => document.getElementById("hub-strip")?.hidden && !document.querySelector("#agents").classList.contains("stale"), null, { timeout: 20000 })
        .catch(async () => { throw new Error("not live again: strip " + JSON.stringify(await stripText(page))); });
      await page.reload();
      await page.waitForFunction(() => typeof agentsSrc !== "undefined" && agentsSrc.at > 0 && !agentsSrc.saved, null, { timeout: 10000 });
      if (fromSW.length) throw new Error("/api/ answered by the worker: " + fromSW.slice(0, 3));
      if (errs.length) throw new Error("page errors: " + errs.join("; "));
    } finally { await browser.close(); }
  });

  await check(`${tag}: a new release replaces the cached shell on the next load, with no hard refresh`, async () => {
    const px = await proxy();
    const { browser, ctx, page, errs } = await open();
    try {
      await load(page, px.url);
      await ready(page, version);
      if (await page.evaluate(() => !!document.querySelector('meta[name="uitest-build"]'))) throw new Error("marked before the release");
      px.release();
      await page.reload();
      // Network first: the new page is the one shown at once.
      if (!(await page.evaluate(() => !!document.querySelector('meta[name="uitest-build"]')))) throw new Error("the reload showed the old page");
      // The new worker took over and the old cache is gone.
      await ready(page, version + "n2");
      const cached = await page.evaluate(async () => (await (await caches.match("/")).text()).includes("uitest-build"));
      if (!cached) throw new Error("the cached page is the old one");
      await ctx.setOffline(true);
      await page.reload();
      if (!(await isApp(page))) throw new Error("no app offline after the release");
      if (!(await page.evaluate(() => !!document.querySelector('meta[name="uitest-build"]')))) throw new Error("offline, the old shell was served");
      await ctx.setOffline(false);
      if (errs.length) throw new Error("page errors: " + errs.join("; "));
    } finally { await browser.close(); px.close(); }
  });
}

for (const w of (process.env.WIDTHS || "390 1280").split(/\s+/).map(Number)) await phase(w);
const bad = results.filter((r) => !r.ok).length;
console.log(`shell: ${results.length - bad} passed, ${bad} failed`);
process.exit(bad ? 1 : 0);
