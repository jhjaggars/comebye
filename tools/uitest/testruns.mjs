// Test runs on a test VM (#138, slices 1-2), against real servers: a hub and a peer ("kv-a") that plays the VM, on
// the fake herdr. The runs are the records tools/ci/remote.sh writes into the VM's comebye state dir
// (test-runs/RUN.json); nothing is mocked between them and the app: the peer reads them, reports them to the hub, and
// the hub sends them to the browser.
//
//   node testruns.mjs OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
//
// At 1280 and 390: the VM's row in Peers › Hosts shows each busy slot ("browser · slot 2/4 · 3 min 12 s") with the
// starting agent's name linked, the time ticks, the agent's row says "testing on kv-a (slot 2/4)", an expired record
// never shows, and an ended run goes from both.
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
    await page.close();
    await browser.close();
    browser = null;
  }
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
