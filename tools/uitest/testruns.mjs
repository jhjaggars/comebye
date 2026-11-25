// Test runs on a test VM (#138, slices 1-4), against real servers: a hub and a peer ("kv-a") that plays the VM, on
// the fake herdr. The runs are the records tools/ci/remote.sh writes into the VM's comebye state dir
// (test-runs/RUN.json); nothing is mocked between them and the app: the peer reads them, reports them to the hub, and
// the hub sends them to the browser.
//
//   node testruns.mjs OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
//
// At 1280 and 390: the VM's row in Peers › Hosts shows each busy slot ("browser · slot 2/4 · 3 min 12 s") with the
// starting agent's name linked, the time ticks, the agent's row says "testing on kv-a (slot 2/4)", an expired record
// never shows, and an ended run goes from both. Finished runs (test-runs/done/RUN.json, with RUN.log beside it):
// the VM's row has a "Recent runs" list (open at 1280, closed at 390 until tapped) with each run's result in its
// colour, duration, age, agent link and temperatures; "Log" opens the run's output, read from the VM through the hub;
// the agent's row shows its last result once its runs have ended. Then the VM goes down: the hub keeps its history,
// and its log says the VM is not answering.
import { launch } from "./lib.mjs";
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";

const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, sock = process.env.FAKE_SOCK;
if (!bin || !sock) { console.error("testruns.mjs: set COMEBYE_BIN and FAKE_SOCK"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "testruns-ui-"));
const results = [], log = [], procs = [];

const freePort = () => new Promise((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); }); });
async function serve(name, port, extra = []) {
  const dir = path.join(work, "state-" + name);
  const lf = path.join(out, `testruns-${name}.log`);
  const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-test-mode", "-push=false", "-state-dir", dir, "-host-name", name, ...extra], {
    env: { ...process.env, COMEBYE_TEST_INSECURE_PEERS: "1" }, stdio: ["ignore", fs.openSync(lf, "w"), fs.openSync(lf, "a")] });
  procs.push(p);
  for (let i = 0; i < 300; i++) {
    try { if ((await fetch(`http://127.0.0.1:${port}/api/agents`)).ok) return dir; } catch {}
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(name + " did not start");
}

const hubPort = await freePort(), vmPort = await freePort();
const base = `http://127.0.0.1:${hubPort}`;
let browser;
try {
  const vmState = await serve("kv-a", vmPort);
  await serve("hub", hubPort, ["-hub-mode"]);
  const j = (m, p, body) => fetch(base + p, { method: m, headers: { "Content-Type": "application/json" }, body: body && JSON.stringify(body) }).then(async (r) => ({ status: r.status, body: await r.json().catch(() => ({})) }));
  const r = await j("POST", "/api/peers", { name: "kv-a", urls: [`http://127.0.0.1:${vmPort}`] });
  if (r.status !== 201) throw new Error("add kv-a: " + JSON.stringify(r));
  let agent = null;
  for (let i = 0; i < 100 && !agent; i++) { // the hub has reached kv-a and has its agents
    agent = ((await j("GET", "/api/agents")).body.agents || []).find((a) => a.host === "kv-a" && a.name) || null;
    if (!agent) await new Promise((r) => setTimeout(r, 100));
  }
  if (!agent) throw new Error("the hub never listed kv-a's agents");

  // The records, as remote.sh writes them on the VM.
  const runs = path.join(vmState, "test-runs");
  fs.mkdirSync(runs, { recursive: true });
  const iso = (ms) => new Date(ms).toISOString().replace(/\.\d+Z$/, "Z");
  const record = (id, o) => fs.writeFileSync(path.join(runs, id + ".json"), JSON.stringify({ v: 1, id, suite: "browser", branch: "feat/test-runs-live", head: "0bdfe68aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", agent: "", from: "desk", slot: 1, slots: 4, started: iso(Date.now() - 60e3), expires: iso(Date.now() + 20 * 60e3), ...o }) + "\n");
  const startRuns = () => {
    record("desk-20261124-101500-1", { slot: 2, agent: `kv-a/${agent.pane_id}`, started: iso(Date.now() - 3 * 60e3 - 12e3) });
    record("desk-20261124-101700-2", { slot: 4, suite: "go", branch: "main", from: "laptop" }); // nobody's agent
    record("desk-20261124-090000-3", { slot: 3, started: iso(Date.now() - 3600e3), expires: iso(Date.now() - 60e3) }); // died: expired
  };
  startRuns();

  // Finished runs, as remote.sh leaves them in test-runs/done: the agent's last one failed, with its output beside it.
  const doneDir = path.join(runs, "done");
  fs.mkdirSync(doneDir, { recursive: true });
  const logText = Array.from({ length: 400 }, (_, i) => `ok   step ${i} of the suite`).join("\n") + "\n--- FAIL: TestSomething (0.01s)\nFAIL\n";
  const doneRec = (id, o) => fs.writeFileSync(path.join(doneDir, id + ".json"), JSON.stringify({ v: 1, id, suite: "browser", branch: "feat/test-runs-history",
    head: "1234567aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", agent: "", from: "desk", slot: 1, slots: 4, started: iso(Date.now() - 10 * 60e3),
    ended: iso(Date.now() - 2 * 60e3), secs: 312, rc: 0, ...o }) + "\n");
  doneRec("desk-20261124-095000-7", { agent: `kv-a/${agent.pane_id}`, rc: 1, secs: 252, cpu_start: 61, nvme_start: 60, cpu_end: 88, nvme_end: 71, ended: iso(Date.now() - 2 * 60e3) });
  fs.writeFileSync(path.join(doneDir, "desk-20261124-095000-7.log"), logText);
  doneRec("desk-20261124-090000-6", { suite: "go", rc: 0, secs: 75, from: "laptop", branch: "main", ended: iso(Date.now() - 40 * 60e3) });
  doneRec("desk-20261124-080000-5", { rc: 143, secs: 30, ended: iso(Date.now() - 3 * 3600e3) });

  async function check(name, fn) {
    try { await fn(); results.push([name, true, ""]); }
    catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); }
  }
  await check("the hub carries the VM's unexpired runs on its host status, in slot order", async () => {
    const res = await fetch(base + "/api/events", { signal: AbortSignal.timeout(10000) });
    const rd = res.body.getReader(); let buf = "";
    while (!/"test_runs"/.test(buf)) { const { value, done } = await rd.read(); if (done) break; buf += new TextDecoder().decode(value); }
    rd.cancel().catch(() => {});
    const ev = JSON.parse(buf.split("\n").find((l) => l.startsWith("data:") && l.includes("test_runs")).slice(5));
    const h = ev.hosts.find((x) => x.name === "kv-a");
    const got = (h.test_runs || []).map((x) => `${x.slot}/${x.slots} ${x.suite} ${x.agent || ""}`).join("; ");
    if (got !== `2/4 browser kv-a/${agent.pane_id}; 4/4 go `) throw new Error(got);
  });

  for (const width of [1280, 390]) {
    const tag = width >= 1100 ? "desktop" : "phone";
    startRuns();
    const { browser: b, page } = await launch({ width, height: width >= 1100 ? 800 : 844, dark });
    browser = b;
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    const shot = (n) => page.screenshot({ path: `${out}/testruns-${tag}-${n}.png` }).catch(() => {});
    const runsOf = () => page.locator('.pr-card[data-name="kv-a"] + .tr-runs');

    await check(`${tag}: the VM's row in Peers › Hosts shows its busy slots, with the starting agent linked`, async () => {
      await page.goto(base + "/#/peers", { waitUntil: "load" });
      await runsOf().waitFor({ timeout: 5000 });
      const items = await runsOf().locator(".tr-run").allInnerTexts();
      if (items.length !== 2) throw new Error("runs: " + JSON.stringify(items));
      if (!/^browser · slot 2\/4 · 3 min 1\d s\b/.test(items[0]) || !items[0].includes(agent.name) || !items[0].includes("feat/test-runs-live")) throw new Error("first: " + items[0]);
      if (!/^go · slot 4\/4 · 1 min \d+ s/.test(items[1]) || !items[1].includes("from laptop")) throw new Error("second: " + items[1]);
      const href = await runsOf().locator("a.tr-agent").getAttribute("href");
      if (href !== `#/agent/kv-a/${encodeURIComponent(agent.pane_id)}`) throw new Error("link: " + href);
      if ((await runsOf().locator("a.tr-agent").count()) !== 1) throw new Error("the run with no agent has a link");
      if (await page.locator('.pr-card[data-name="hub"] + .tr-runs').count()) throw new Error("the hub shows runs it has none of");
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
      if (w[0] > w[1]) throw new Error("horizontal overflow " + w);
      await shot("host-row");
    });

    const histOf = () => page.locator('details.tr-hist[data-tr-hist="kv-a"]');
    await check(`${tag}: the VM's row lists its recent runs, ${width >= 1100 ? "open" : "closed until tapped"}, newest first`, async () => {
      await histOf().waitFor({ timeout: 5000 });
      const open = await histOf().evaluate((d) => d.open);
      if (open !== (width >= 1100)) throw new Error("open: " + open);
      const sumText = (await histOf().locator("summary").innerText()).replace(/\s+/g, " ").trim();
      if (sumText !== "Recent runs · 3 · 1 failed") throw new Error("summary: " + sumText);
      if (!open) {
        const sum = histOf().locator("summary");
        if ((await sum.boundingBox()).height < 44) throw new Error("a small target");
        await sum.tap();
        await page.waitForFunction(() => document.querySelector('details.tr-hist[data-tr-hist="kv-a"]').open, null, { timeout: 2000 });
      }
      const items = await histOf().locator(".tr-done").allInnerTexts();
      const flat = items.map((t) => t.replace(/\s+/g, " ").trim());
      if (flat.length !== 3) throw new Error(JSON.stringify(flat));
      if (!/^fail browser · 4 min 12 s · 2m ago\b/i.test(flat[0]) || !flat[0].includes(agent.name) || !flat[0].includes("CPU 61→88 °C · NVMe 60→71 °C") || !flat[0].includes("Log")) throw new Error("first: " + flat[0]);
      if (!/^pass go · 1 min 15 s · 40m ago\b/i.test(flat[1]) || !flat[1].includes("from laptop") || flat[1].includes("Log")) throw new Error("second: " + flat[1]);
      if (!/^stopped browser · 30 s · 3h ago\b/i.test(flat[2])) throw new Error("third: " + flat[2]);
      const href = await histOf().locator(".tr-done").first().locator("a.tr-agent").getAttribute("href");
      if (href !== `#/agent/kv-a/${encodeURIComponent(agent.pane_id)}`) throw new Error("link: " + href);
      // A re-render (the hosts change) keeps the list as the user left it.
      record("desk-20261124-101800-9", { slot: 1, suite: "flakes" });
      await page.waitForFunction(() => document.querySelectorAll('.pr-card[data-name="kv-a"] + .tr-runs .tr-run').length === 3, null, { timeout: 5000 });
      if (!(await histOf().evaluate((d) => d.open))) throw new Error("closed by a re-render");
      fs.rmSync(path.join(runs, "desk-20261124-101800-9.json"));
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
      if (w[0] > w[1]) throw new Error("horizontal overflow " + w);
      await histOf().scrollIntoViewIfNeeded();
      await shot("recent-runs");
    });

    await check(`${tag}: a failed run is red, a passed one green, a stopped one orange`, async () => {
      const got = await page.evaluate(() => {
        const probe = (v) => { const e = document.createElement("span"); e.style.color = `var(${v})`; document.body.append(e); const c = getComputedStyle(e).color; e.remove(); return c; };
        const rows = [...document.querySelectorAll('details.tr-hist[data-tr-hist="kv-a"] .tr-done')];
        const st = (el) => [getComputedStyle(el.querySelector(".tr-res")).color, getComputedStyle(el).borderLeftColor];
        return { red: probe("--red"), green: probe("--green"), orange: probe("--orange"), rows: rows.map(st), nfail: getComputedStyle(document.querySelector(".tr-nfail")).color };
      });
      const want = [got.red, got.green, got.orange];
      got.rows.forEach(([c, b], i) => { if (c !== want[i] || b !== want[i]) throw new Error(`row ${i}: ${c} ${b}, want ${want[i]}`); });
      if (got.nfail !== got.red) throw new Error("the failed count is not red: " + got.nfail);
      if (got.red === got.green) throw new Error("red and green are the same colour");
    });

    await check(`${tag}: "Log" opens the run's output from the VM, at its end, and closes again`, async () => {
      const btn = histOf().locator(".tr-done").first().locator(".tr-log");
      if (width < 1100 && (await btn.boundingBox()).height < 44) throw new Error("a small target");
      if (width < 1100) await btn.tap(); else await btn.click();
      await page.locator("#tl").waitFor({ state: "visible", timeout: 3000 });
      await page.locator("#tl-pre").waitFor({ state: "visible", timeout: 5000 });
      if ((await page.locator("#tl-title").innerText()) !== "browser on kv-a") throw new Error("title: " + await page.locator("#tl-title").innerText());
      if (!(await page.locator("#tl-meta").innerText()).startsWith("failed (exit 1) · 4 min 12 s · 2m ago · feat/test-runs-history · 1234567aaaaa")) throw new Error("meta: " + await page.locator("#tl-meta").innerText());
      if ((await page.locator("#tl-pre").textContent()) !== logText) throw new Error("the log is not the VM's");
      const end = await page.evaluate(() => { const b = document.getElementById("tl-body"); return b.scrollHeight - b.clientHeight - b.scrollTop; });
      if (end > 4) throw new Error("not at the end: " + end);
      const failLine = await page.evaluate(() => { const r = document.createRange(); const pre = document.getElementById("tl-pre"); const t = pre.firstChild; const i = t.data.indexOf("--- FAIL");
        r.setStart(t, i); r.setEnd(t, i + 8); const b = r.getBoundingClientRect(), v = document.getElementById("tl-body").getBoundingClientRect(); return b.top >= v.top && b.bottom <= v.bottom; });
      if (!failLine) throw new Error("the failure is not on screen");
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.getElementById("tl-pre").scrollWidth, document.getElementById("tl-pre").clientWidth]);
      if (w[0] > w[1] || w[2] > w[3]) throw new Error("horizontal overflow " + w);
      await shot("log");
      if (width < 1100) await page.locator("#tl-close").tap(); else await page.keyboard.press("Escape");
      await page.locator("#tl").waitFor({ state: "hidden", timeout: 3000 });
      if (!/#\/peers/.test(page.url())) throw new Error("left the hosts: " + page.url());
    });

    await check(`${tag}: the elapsed time ticks`, async () => {
      const el = runsOf().locator(".tr-for").first();
      const t0 = await el.innerText();
      await page.waitForFunction((t) => document.querySelector(".tr-runs .tr-for").textContent !== t, t0, { timeout: 2500 });
      const t1 = await el.innerText();
      const secs = (t) => { const m = /(\d+) min (\d+) s/.exec(t); return m ? +m[1] * 60 + +m[2] : NaN; };
      if (!(secs(t1) > secs(t0))) throw new Error(`${t0} -> ${t1}`);
    });

    await check(`${tag}: the agent's name opens the agent, through the press layer`, async () => {
      const a = runsOf().locator("a.tr-agent");
      const box = await a.boundingBox();
      if (width < 1100 && box.height < 24) throw new Error("a small target on the phone: " + box.height);
      if (width < 1100) await a.tap(); else await a.click();
      await page.waitForFunction((p) => decodeURIComponent(location.hash).endsWith(p), `/agent/kv-a/${agent.pane_id}`, { timeout: 3000 });
    });

    await check(`${tag}: the starting agent's row says where it is testing`, async () => {
      await page.goto(base + "/#/", { waitUntil: "load" });
      if (width < 1100) await page.waitForSelector("#agents", { state: "visible" });
      const row = page.locator(`#agents .agent[data-id="${agent.id}"]`);
      await row.locator(".tr-on").waitFor({ timeout: 5000 });
      const t = await row.locator(".tr-on").innerText();
      if (t !== "testing on kv-a (slot 2/4)") throw new Error(t);
      const others = await page.locator("#agents .tr-on").count();
      if (others !== 1) throw new Error(`${others} rows say they are testing`);
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
      if (w[0] > w[1]) throw new Error("horizontal overflow " + w);
      await row.scrollIntoViewIfNeeded();
      await shot("agent-row");
    });

    await check(`${tag}: a run that ends goes from the agent's row and the host's`, async () => {
      fs.rmSync(path.join(runs, "desk-20261124-101500-1.json"));
      await page.locator(`#agents .agent[data-id="${agent.id}"] .tr-on`).waitFor({ state: "detached", timeout: 5000 });
      await page.goto(base + "/#/peers", { waitUntil: "load" });
      await page.locator('.pr-card[data-name="kv-a"]').waitFor();
      await page.waitForFunction(() => document.querySelectorAll(".tr-runs .tr-run").length === 1, null, { timeout: 5000 });
      fs.rmSync(path.join(runs, "desk-20261124-101700-2.json"));
      await runsOf().waitFor({ state: "detached", timeout: 5000 });
      await shot("ended");
    });

    await check(`${tag}: once its runs have ended, the agent's row shows its last result, in red for a failure`, async () => {
      await page.goto(base + "/#/", { waitUntil: "load" });
      if (width < 1100) await page.waitForSelector("#agents", { state: "visible" });
      const row = page.locator(`#agents .agent[data-id="${agent.id}"]`);
      await row.locator(".tr-last").waitFor({ timeout: 5000 });
      const t = await row.locator(".tr-last").innerText();
      if (t !== "browser failed on kv-a · 2m ago") throw new Error(t);
      const [c, red] = await row.locator(".tr-last").evaluate((el) => { const e = document.createElement("span"); e.style.color = "var(--red)"; document.body.append(e); const r = getComputedStyle(e).color; e.remove(); return [getComputedStyle(el).color, r]; });
      if (c !== red) throw new Error(`colour ${c}, want ${red}`);
      if ((await page.locator("#agents .tr-last").count()) !== 1) throw new Error("other rows show a last result");
      await page.evaluate((id) => document.querySelector(`#agents .agent[data-id="${CSS.escape(id)}"]`)?.scrollIntoView({ block: "center" }), agent.id); // the list re-renders: no held element
      await shot("agent-last");
    });
    await page.close();
    await browser.close();
    browser = null;
  }

  // The VM goes to sleep: the hub keeps its last report, so its history still shows; its log needs the VM.
  procs[0].kill();
  await check("the VM asleep: the hub still lists its recent runs, and its log says the VM is not answering", async () => {
    const { browser: b, page } = await launch({ width: 390, height: 844, dark });
    browser = b;
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    await page.goto(base + "/#/peers", { waitUntil: "load" });
    const hist = page.locator('details.tr-hist[data-tr-hist="kv-a"]');
    await hist.waitFor({ timeout: 5000 });
    await page.waitForFunction(() => /down|unreachable|asleep|not answering/i.test(document.querySelector('.pr-card[data-name="kv-a"]').innerText), null, { timeout: 10000 });
    await hist.locator("summary").tap();
    if ((await hist.locator(".tr-done").count()) !== 3) throw new Error("history gone while down");
    await hist.locator(".tr-log").first().tap();
    await page.waitForFunction(() => /not answering/.test(document.getElementById("tl-status").textContent), null, { timeout: 30000 });
    if (await page.locator("#tl-pre").isVisible()) throw new Error("a log shown for a VM that is down");
    await page.screenshot({ path: `${out}/testruns-phone-asleep-log.png` }).catch(() => {});
    await browser.close();
    browser = null;
  });
} finally {
  if (browser) await browser.close().catch(() => {});
  for (const p of procs) p.kill();
  fs.rmSync(work, { recursive: true, force: true });
}

fs.writeFileSync(`${out}/testruns-console.log`, log.join("\n") + "\n");
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"} ${name}${ok ? "" : ": " + why}`); if (!ok) failed++; }
const errs = log.filter((l) => /pageerror|console\.error/.test(l) && !/Failed to load resource/.test(l));
if (errs.length) { console.log("FAIL console errors:\n  " + errs.join("\n  ")); failed++; }
process.exit(failed ? 1 : 0);
