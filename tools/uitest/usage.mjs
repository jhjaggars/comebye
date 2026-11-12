// The Usage screen against a real hub and a real peer, with fixture session
// files for both (usagefix.mjs: counts only, no text) and a fake herdr whose
// coordinator is "laharl". The hub counts its own files, gathers the peer's,
// and labels sessions from the ledger. Run under the servers' real
// Content-Security-Policy (the charts are drawn by hand, nothing is loaded).
//
//   node usage.mjs OUT_DIR   (env: COMEBYE_BIN, FAKE_HERDR, SCHEME=dark|light)
//
// Each check runs at 1280 (desktop) and 390 (phone).
import { launch } from "./lib.mjs";
import { spawn, spawnSync } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, fakeHerdr = process.env.FAKE_HERDR;
if (!bin || !fakeHerdr) { console.error("usage.mjs: set COMEBYE_BIN and FAKE_HERDR"); process.exit(2); }
const here = path.dirname(fileURLToPath(import.meta.url));
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "usage-ui-"));
const results = [], log = [], procs = [];
const freePort = () => new Promise((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); }); });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

spawnSync("node", [path.join(here, "usagefix.mjs"), path.join(work, "usage")], { stdio: "ignore" });
fs.mkdirSync(path.join(work, "state-hub"), { recursive: true });
fs.copyFileSync(path.join(work, "usage/ledger.json"), path.join(work, "state-hub/ledger.json"));

function start(name, args, env) {
  const f = fs.openSync(path.join(out, `usage-${name}.log`), "w");
  const p = spawn(args[0], args.slice(1), { env: { ...process.env, COMEBYE_TEST_INSECURE_PEERS: "1", COMEBYE_USAGE_INTERVAL: "2s", ...env }, stdio: ["ignore", f, f] });
  procs.push(p);
  return p;
}
const up = async (url) => { for (let i = 0; i < 80; i++) { try { if ((await fetch(url)).ok) return; } catch {} await sleep(100); } throw new Error(url + " did not start"); };

const hubPort = await freePort(), kubePort = await freePort();
const base = `http://127.0.0.1:${hubPort}`;
let browser;
try {
  start("herdr", [fakeHerdr, "-sock", path.join(work, "h.sock"), "-dir", path.join(work, "tx"), "-fixtures"]);
  for (let i = 0; i < 50 && !fs.existsSync(path.join(work, "h.sock")); i++) await sleep(100);
  start("kube", [bin, "-sock", path.join(work, "h.sock"), "-addr", `127.0.0.1:${kubePort}`, "-insecure-no-auth", "-test-mode", "-push=false", "-state-dir", path.join(work, "state-kube"), "-host-name", "kube", "-ledger=false"],
    { COMEBYE_ARCHIVE_CLAUDE_DIR: path.join(work, "usage/peer/claude"), COMEBYE_ARCHIVE_PI_DIR: path.join(work, "usage/peer/pi") });
  await up(`http://127.0.0.1:${kubePort}/api/agents`);
  start("hub", [bin, "-sock", path.join(work, "h.sock"), "-addr", `127.0.0.1:${hubPort}`, "-insecure-no-auth", "-test-mode", "-push=false", "-state-dir", path.join(work, "state-hub"), "-host-name", "devbox", "-peers", `kube=http://127.0.0.1:${kubePort}`],
    { COMEBYE_ARCHIVE_CLAUDE_DIR: path.join(work, "usage/hub/claude"), COMEBYE_ARCHIVE_PI_DIR: path.join(work, "usage/hub/pi") });
  await up(base + "/api/agents");
  const j = (m, p, body) => fetch(base + p, { method: m, headers: { "Content-Type": "application/json" }, body: body && JSON.stringify(body) }).then(async (r) => ({ status: r.status, body: await r.json().catch(() => ({})) }));
  const { body: ag } = await j("GET", "/api/agents");
  const lah = (ag.agents || []).find((a) => a.name === "laharl");
  if (!lah) throw new Error("fixture agent laharl missing");
  await j("POST", "/api/roles", { agent_id: lah.id, role: "coordinator" });
  // wait until the hub has gathered the peer and labelled the coordinator
  for (let i = 0; i < 100; i++) {
    const r = await j("GET", "/api/usage?by=role&since=14d");
    const gs = Object.fromEntries((r.body.groups || []).map((g) => [g.key, g.fresh]));
    const kube = (r.body.hosts || []).find((h) => h.name === "kube");
    if (gs.coordinator && gs.worker && kube && kube.gathered) break;
    await sleep(200);
  }

  async function check(name, fn) {
    try { await fn(); results.push([name, true, ""]); }
    catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); }
  }
  const api = (p) => fetch(base + p).then((r) => r.json());

  for (const width of [1280, 390]) {
    const tag = width >= 1100 ? "desktop" : "phone";
    // The first check runs under the server's real Content-Security-Policy (script-src 'self'): the charts are drawn
    // by hand and nothing is loaded. The driver's own waits need eval, so the rest run with the policy bypassed.
    let { browser: b, page } = await launch({ width, height: width >= 1100 ? 900 : 844, dark, bypassCSP: false });
    browser = b;
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    const shot = (n, full) => page.screenshot({ path: `${out}/usage-${tag}-${n}.png` }).catch(() => {});
    const tall = async (n) => { const h = await page.evaluate(() => document.querySelector("#us-view").scrollHeight + 120); await page.setViewportSize({ width, height: Math.min(h, 4000) }); await shot(n); await page.setViewportSize({ width, height: width >= 1100 ? 900 : 844 }); };
    const noHScroll = async () => {
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector("#us-body").scrollWidth, document.querySelector("#us-body").clientWidth]);
      if (w[0] > w[1] || w[2] > w[3]) throw new Error("horizontal overflow " + w.join("/"));
    };
    const legend = () => page.locator("#us-legend").innerText();
    const open = async (hash) => { await page.goto(base + "/" + hash, { waitUntil: "load" }); await page.waitForSelector("#us-chart svg"); };

    await check(`${tag}: the screen opens with tiles, a stacked chart and the numbers match the API`, async () => {
      await open("#/usage");
      const a = await api(`/api/usage?by=kind&since=14d&tz=${-new Date().getTimezoneOffset()}`); // the browser buckets days in its own zone
      const tiles = await page.locator(".us-tile").allInnerTexts();
      if (tiles.length !== 4) throw new Error("tiles: " + tiles.length);
      const fmt = (n) => (n >= 1e6 ? (n / 1e6).toFixed(n >= 1e8 ? 0 : 1) + "M" : (n / 1e3).toFixed(n >= 1e5 ? 0 : 1) + "k");
      if (!tiles[0].includes(fmt(a.totals.fresh))) throw new Error(`fresh tile ${JSON.stringify(tiles[0])} vs ${fmt(a.totals.fresh)}`);
      if (!tiles[1].includes(fmt(a.totals.cr))) throw new Error("cache reads tile: " + tiles[1]);
      if (!/no price/.test(tiles[3])) throw new Error("the cost tile does not say pi has no price: " + tiles[3]);
      const bars = await page.locator("#us-chart g[data-i]").count();
      if (bars !== a.buckets.length) throw new Error(`bars ${bars} vs buckets ${a.buckets.length}`);
      const lg = await legend();
      if (!lg.includes("claude") || !lg.includes("pi")) throw new Error("legend: " + lg);
      await noHScroll();
      await tall("overview");
    });

    await check(`${tag}: no Content-Security-Policy violation drew the screen`, async () => {
      if (log.some((l) => /Content Security Policy|Refused to/.test(l))) throw new Error(log.find((l) => /Content Security Policy|Refused to/.test(l)));
      await page.close(); await browser.close();
      ({ browser: b, page } = await launch({ width, height: width >= 1100 ? 900 : 844, dark }));
      browser = b;
      page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
      page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
      await open("#/usage");
    });

    await check(`${tag}: the category switcher regroups the chart (model, host, token type)`, async () => {
      await page.getByRole("button", { name: "Model", exact: true }).click();
      await page.waitForFunction(() => document.querySelector("#us-legend").innerText.includes("claude-opus-5-5"));
      let lg = await legend();
      if (!lg.includes("homelab/glm-5-3")) throw new Error("model legend: " + lg);
      await page.getByRole("button", { name: "Host", exact: true }).click();
      await page.waitForFunction(() => document.querySelector("#us-legend").innerText.includes("kube"));
      lg = await legend();
      if (!lg.includes("devbox")) throw new Error("host legend: " + lg);
      await page.getByRole("button", { name: "Token type", exact: true }).click();
      await page.waitForFunction(() => document.querySelector("#us-legend").innerText.includes("cache write"));
      lg = await legend();
      if (!lg.includes("input") || !lg.includes("output")) throw new Error("token legend: " + lg);
      if (!(await page.url()).includes("d=token")) throw new Error("the URL does not follow the switcher: " + page.url());
      await page.getByRole("button", { name: "Role", exact: true }).click();
      await page.waitForFunction(() => document.querySelector("#us-legend").innerText.includes("coordinator"));
      await page.getByRole("button", { name: "Purpose", exact: true }).click();
      await page.waitForFunction(() => /build/.test(document.querySelector("#us-legend").innerText));
      lg = await legend();
      if (!lg.includes("review")) throw new Error("the handoff --purpose review is missing: " + lg);
      await page.getByRole("button", { name: "Agent", exact: true }).click();
    });

    await check(`${tag}: a bar can be picked for its numbers (tap or keyboard)`, async () => {
      await page.locator("#us-chart g[data-i]").nth(5).click();
      await page.waitForFunction(() => /:\s+\d/.test(document.querySelector("#us-read").textContent));
      const r1 = await page.locator("#us-read").innerText();
      await page.locator("#us-chart g[data-i]").nth(6).focus();
      await page.keyboard.press("Enter");
      await page.waitForFunction((x) => document.querySelector("#us-read").textContent !== x, r1);
    });

    await check(`${tag}: the measure switch shows all tokens and dollars`, async () => {
      await page.getByRole("button", { name: "All tokens", exact: true }).click();
      await page.waitForFunction(() => /\d(\.\d)?[MB]/.test(document.querySelector("#us-legend").innerText));
      await page.getByRole("button", { name: "Est. dollars", exact: true }).click();
      await page.waitForFunction(() => /\$/.test(document.querySelector("#us-legend").innerText));
      if (!(await page.url()).includes("m=cost")) throw new Error("URL: " + page.url());
      await page.getByRole("button", { name: "Fresh tokens", exact: true }).click();
    });

    await check(`${tag}: drill down project, handoff, session and back`, async () => {
      await page.locator('button.us-row[data-us-down="comebye"]').click();
      await page.waitForFunction(() => document.querySelector(".us-crumbs").innerText.includes("comebye"));
      const t = await page.locator(".us-rows").first().innerText();
      for (const want of ["usage-collector", "usage-screen", "fix-detail-menu", "research-cache"]) if (!t.includes(want)) throw new Error(`handoffs lack ${want}: ${t.slice(0, 200)}`);
      await page.locator('button.us-row', { hasText: "usage-collector" }).click();
      await page.waitForFunction(() => document.querySelector(".us-crumbs").innerText.includes("usage-collector"));
      const s = await page.locator(".us-rows").first().innerText();
      if (!/claude/.test(s) || !s.includes("00000000")) throw new Error("sessions: " + s.slice(0, 200));
      if (!(await page.url()).includes("p=comebye") || !(await page.url()).includes("h=")) throw new Error("URL: " + page.url());
      await noHScroll();
      await tall("drill");
      await page.locator('[data-us-crumb="1"]').click();
      await page.waitForFunction(() => !document.querySelector(".us-crumbs").innerText.includes("usage-collector"));
      await page.locator('[data-us-crumb="0"]').click();
      await page.waitForFunction(() => document.querySelector('button.us-row[data-us-down="comebye"]'));
    });

    await check(`${tag}: the coordinator is called out with its context per request`, async () => {
      const t = await page.locator(".us-coord").innerText();
      if (!/context sent per request/.test(t) || !/replies/.test(t)) throw new Error("coordinator card: " + t.slice(0, 200));
      const ro = await api("/api/usage?by=role&since=14d");
      const c = ro.groups.find((g) => g.key === "coordinator");
      if (!c || !t.includes(c.fresh >= 1e6 ? (c.fresh / 1e6).toFixed(1) + "M" : (c.fresh / 1e3).toFixed(0) + "k")) throw new Error("the card does not show the coordinator's fresh tokens");
      if (!(await page.locator(".us-coord svg polyline").count())) throw new Error("no context line");
    });

    await check(`${tag}: cache efficiency lists each model with hit rate`, async () => {
      const t = await page.locator('[aria-label="Cache efficiency"]').innerText();
      if (!t.includes("claude-opus-5-5") || !/9\d%/.test(t) || !/reads per write/.test(t)) throw new Error(t.slice(0, 200));
    });

    await check(`${tag}: a range switch reloads (24 hours is by the hour)`, async () => {
      await page.getByRole("button", { name: "24h", exact: true }).click();
      await page.waitForFunction(() => document.querySelectorAll("#us-chart g[data-i]").length >= 24);
      await page.getByRole("button", { name: "14 days", exact: true }).click();
      await page.waitForFunction(() => document.querySelectorAll("#us-chart g[data-i]").length < 24);
    });

    await check(`${tag}: a deep link restores the state`, async () => {
      await open("#/usage?d=model&p=comebye&r=7");
      await page.waitForFunction(() => document.querySelector("#us-legend").innerText.includes("claude-opus-5-5"));
      if ((await page.locator('[data-us-r="7"]').getAttribute("aria-pressed")) !== "true") throw new Error("range not restored");
      if (!(await page.locator(".us-crumbs").innerText()).includes("comebye")) throw new Error("drill not restored");
      await open("#/usage");
    });

    await check(`${tag}: a price for a Pi model turns tokens into dollars`, async () => {
      const before = await api("/api/usage?by=kind&since=14d");
      if (!(before.totals.unpriced > 0)) throw new Error("fixture has no unpriced tokens");
      await page.locator("#us-pricedet summary").click();
      await page.waitForSelector('#us-pricebody tr[data-model="claude-opus-5-5"]');
      await page.locator("#us-newmodel").fill("homelab/glm-5-3");
      await page.locator('#us-newrow input[data-f="in"]').fill("1");
      await page.locator('#us-newrow input[data-f="out"]').fill("4");
      await shot("prices");
      await page.locator("#us-saveprices").click();
      await page.waitForFunction(() => /Saved/.test(document.querySelector("#us-pricemsg")?.textContent || ""), null, { timeout: 5000 });
      const after = await api("/api/usage?by=kind&since=14d");
      if (!(after.totals.usd > before.totals.usd) || !(after.totals.unpriced < before.totals.unpriced)) throw new Error(`usd ${before.totals.usd} -> ${after.totals.usd}`);
      const p = await api("/api/usage/prices");
      if (!p.custom["homelab/glm-5-3"] || p.custom["homelab/glm-5-3"].out !== 4) throw new Error("custom: " + JSON.stringify(p.custom));
      await page.locator("#us-resetprices").click();
      await page.waitForFunction(() => /defaults/.test(document.querySelector("#us-pricemsg")?.textContent || ""), null, { timeout: 5000 });
    });

    await check(`${tag}: an agent and the Status page carry the handoff's usage chip, and it opens the drilled screen`, async () => {
      const th = (await api("/api/usage?by=kind&since=14d&limit=100")).top_handoffs;
      const row = th.find((h) => h.name === "usage-collector");
      if (!row) throw new Error("no usage-collector handoff in the API");
      const { agents } = await api("/api/agents");
      const live = agents.find((x) => x.name !== "laharl");
      // The fixture ledger only holds removed workers: make one of them the live agent's running handoff, in the browser only.
      await page.route(/\/api\/ledger$/, async (r) => {
        const resp = await r.fetch(); const j = await resp.json();
        const e = j.entries.find((x) => x.id === row.handoff);
        if (e) { e.agent_id = live.id; e.status = "running"; e.finished_at = null; e.created_at = new Date(Date.now() - 3600000).toISOString(); }
        await r.fulfill({ response: resp, json: j });
      });
      const want = (n) => (n >= 1e6 ? (n / 1e6).toFixed(n >= 1e8 ? 0 : 1) + "M" : (n / 1e3).toFixed(n >= 1e5 ? 0 : 1) + "k");
      await page.goto(base + "/", { waitUntil: "load" });
      await page.waitForFunction((id) => document.querySelector(`#agents .agent[data-id="${id}"] .us-chip`), live.id, { timeout: 15000 });
      const rowChip = await page.locator(`#agents .agent[data-id="${live.id}"] .us-chip`).innerText();
      if (!rowChip.startsWith(want(row.fresh) + " tokens")) throw new Error(`row chip "${rowChip}" vs ${want(row.fresh)}`);
      if (!/cache \d+%/.test(rowChip)) throw new Error("no cache hit in " + rowChip);
      if (await page.locator(`#agents .agent[data-id="${live.id}"] a.us-chip`).count()) throw new Error("a link inside a row that is itself a button");
      await shot("chip-row");
      await page.locator(`#agents .agent[data-id="${live.id}"]`).click();
      await page.waitForSelector("#d-usage a.us-chip", { state: "visible" });
      const box = await page.locator("#d-usage a.us-chip").boundingBox();
      if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("page scrolls sideways");
      await shot("chip-detail");
      await page.locator("#d-usage a.us-chip").click();
      await page.waitForSelector("#us-chart svg");
      const crumbs = await page.locator(".us-crumbs").innerText();
      if (!crumbs.includes("comebye") || !crumbs.includes("usage-collector")) throw new Error("crumbs " + crumbs);
      const val = (await page.locator(".us-rows .us-row .val").first().innerText()).split(/\s/)[0];
      if (val !== want(row.fresh)) throw new Error(`the drill row says ${val}, the chip ${want(row.fresh)}`);
      await noHScroll();
      // The same chip links from the Status page.
      if (width >= 1100) {
        await page.goto(base + "/#/status", { waitUntil: "load" });
        await page.waitForSelector("#ps-body a.us-chip", { timeout: 15000 });
        await shot("chip-status");
      }
      await page.unroute(/\/api\/ledger$/);
    });

    await check(`${tag}: Back leaves the screen and Escape does too`, async () => {
      await open("#/usage");
      await page.locator("#us-back").click();
      await page.waitForSelector("#usage", { state: "hidden" });
      await open("#/usage");
      await page.keyboard.press("Escape");
      await page.waitForSelector("#usage", { state: "hidden" });
    });
    await page.close();
    await browser.close();
    browser = null;
  }

  await check("the transcript text never reaches the hub's store", async () => {
    const f = fs.readFileSync(path.join(work, "state-hub/usage.json"), "utf8");
    if (/"text"|"content"|"message"/.test(f)) throw new Error("usage.json has message fields");
  });
  await check("a bad agent token is refused", async () => {
    const c = await fetch(base + "/api/usage", { headers: { "X-Comebye-Agent-Token": "nope" } });
    if (c.status !== 401 && c.status !== 403) throw new Error("a bad agent token got " + c.status);
  });
} finally {
  if (browser) await browser.close().catch(() => {});
  for (const p of procs) p.kill();
  fs.rmSync(work, { recursive: true, force: true });
}

fs.writeFileSync(`${out}/usage-console.log`, log.join("\n") + "\n");
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"} ${name}${ok ? "" : ": " + why}`); if (!ok) failed++; }
const errs = log.filter((l) => /pageerror|console\.error/.test(l) && !/Failed to load resource/.test(l));
if (errs.length) { console.log("FAIL console errors:\n  " + errs.join("\n  ")); failed++; }
process.exit(failed ? 1 : 0);
