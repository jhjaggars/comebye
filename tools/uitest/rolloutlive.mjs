// The rolling rollout end to end, on a simulated fleet of real servers: a hub (-release-simulate, hub mode) and three peers
// (-ledger=false, -release-simulate: they "upgrade" by writing a record and then report the new version). Nothing is mocked:
// Roll out to all goes through the hub's routes, the loop runs in the hub process, and the hub is KILLED and restarted in the
// middle of the rollout, which must carry on from release/rollout.json. Then a version whose simulated upgrade fails (patch
// 13) stops the rollout at the canary and leaves the rest untouched.
//
//   node rolloutlive.mjs OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
import { launch } from "./lib.mjs";
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";

const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, sock = process.env.FAKE_SOCK;
if (!bin || !sock) { console.error("rolloutlive.mjs: set COMEBYE_BIN and FAKE_SOCK"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "rollout-live-"));
const SHA = "a3f9c1e5d2b7480cf6e1a9b34d70c8e2f15a6b09";
const freePort = () => new Promise((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); }); });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const procs = {};
const results = [];

function start(name, port, extra) {
  const log = fs.openSync(path.join(out, `rollout-${name}.log`), "a");
  const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-push=false", "-state-dir", path.join(work, "state-" + name), "-host-name", name, "-release-simulate", ...extra], {
    env: { ...process.env, COMEBYE_TEST_INSECURE_PEERS: "1" }, stdio: ["ignore", log, log] });
  procs[name] = p;
  return p;
}
async function up(port) {
  for (let i = 0; i < 80; i++) { try { if ((await fetch(`http://127.0.0.1:${port}/api/agents`)).ok) return; } catch {} await sleep(100); }
  throw new Error("server on " + port + " did not start");
}
async function check(name, fn) {
  try { await fn(); results.push([name, true, ""]); } catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); }
}

const hubPort = await freePort(), peerPorts = [await freePort(), await freePort(), await freePort()];
const names = ["p1", "p2", "p3"];
const base = `http://127.0.0.1:${hubPort}`;
const j = async (m, p, body, hdr = {}) => { const r = await fetch(base + p, { method: m, headers: { "Content-Type": "application/json", ...hdr }, body: body && JSON.stringify(body) }); return { status: r.status, body: await r.json().catch(() => ({})) }; };
let browser;
try {
  names.forEach((n, i) => start(n, peerPorts[i], ["-ledger=false", "-hub", base]));
  start("hub", hubPort, ["-hub-mode"]);
  await Promise.all([up(hubPort), ...peerPorts.map(up)]);
  names.forEach(async (n, i) => { const r = await j("POST", "/api/peers", { name: n, urls: [`http://127.0.0.1:${peerPorts[i]}`] }); if (r.status !== 201) throw new Error("add " + n + JSON.stringify(r)); });
  for (let i = 0; i < 100; i++) { const r = await j("GET", "/api/peers"); if (names.every((n) => (r.body.peers || []).some((p) => p.name === n && p.status === "ok"))) break; await sleep(100); }

  const tag = async (version) => {
    const t = await j("POST", "/api/todos", { question: "", kind: "release", no_wait: true, release: { version, commit: SHA } });
    const id = t.body.todo.id;
    const d = (await j("GET", "/api/release/" + id)).body;
    const a = await j("POST", `/api/release/${id}/approve`, { version, commit: SHA }, { "X-Comebye-Csrf": d.csrf });
    if (a.status !== 202) throw new Error("approve " + JSON.stringify(a));
    for (let i = 0; i < 100; i++) { const x = await j("GET", "/api/todos/" + id); if (x.body.todo.release.state === "tagged") return { id, csrf: d.csrf }; await sleep(100); }
    throw new Error("not tagged");
  };
  const view = async () => (await j("GET", "/api/releases")).body;
  const job = async () => (await view()).rollout_job;
  const until = async (what, fn, ms = 90000) => { const t0 = Date.now(); for (;;) { let v; try { v = await fn(); } catch {} if (v) return v; if (Date.now() - t0 > ms) throw new Error("timed out: " + what + " " + JSON.stringify(await job().catch(() => null))); await sleep(150); } };
  const phases = (jb) => jb.plan.map((s) => s.host + "=" + s.phase).join(" ");
  const audit = async (id) => ((await j("GET", "/api/release/" + id)).body.audit || []).filter((e) => e.action.startsWith("rollout.")).map((e) => e.action + (e.host ? ":" + e.host : ""));

  // ---- scenario 1: a healthy rollout, the hub killed and restarted in the middle ----
  const V1 = "v9.7.1";
  const r1 = await tag(V1);
  await check("the plan offers the peers, then the hub last", async () => {
    const v = await view();
    const plan = v.rollout_plan || [];
    if (plan.map((s) => s.host + "/" + s.wave).join() !== "p1/canary,p2/peers,p3/peers,hub/hub") throw new Error(JSON.stringify(plan));
  });
  await check("Roll out to all starts a rollout; the canary goes first and soaks while nothing else is touched", async () => {
    const s = await j("POST", "/api/release/rollout", { todo: r1.id, version: V1, commit: SHA, hosts: names, hub: true }, { "X-Comebye-Csrf": r1.csrf });
    if (s.status !== 202) throw new Error(JSON.stringify(s));
    const dup = await j("POST", "/api/release/rollout", { todo: r1.id, version: V1, commit: SHA, hosts: names, hub: true }, { "X-Comebye-Csrf": r1.csrf });
    if (dup.status !== 409) throw new Error("a second start was accepted: " + dup.status);
    const jb = await until("p1 soaking", async () => { const x = await job(); return x && x.plan[0].phase === "soaking" && x; });
    if (jb.plan[1].phase !== "pending" || jb.plan[2].phase !== "pending" || jb.plan[3].phase !== "pending") throw new Error("others touched: " + phases(jb));
    const up1 = await j("POST", `/api/hosts/p2/release/upgrade`, { todo: r1.id, version: V1 }, { "X-Comebye-Csrf": r1.csrf });
    if (up1.status !== 409 || !JSON.stringify(up1.body).includes("rollout_active")) throw new Error("manual upgrade was not refused: " + JSON.stringify(up1));
  });
  await check("the hub is killed (SIGKILL) mid-rollout and restarted: the rollout carries on from rollout.json and finishes", async () => {
    procs.hub.kill("SIGKILL");
    await sleep(500);
    const file = JSON.parse(fs.readFileSync(path.join(work, "state-hub", "release", "rollout.json"), "utf8"));
    if (file.state !== "running") throw new Error("the file says " + file.state);
    start("hub", hubPort, ["-hub-mode"]);
    await up(hubPort);
    for (let i = 0; i < 100; i++) { const r = await j("GET", "/api/peers"); if (names.every((n) => (r.body.peers || []).some((p) => p.name === n && p.status === "ok"))) break; await sleep(100); }
    const done = await until("done", async () => { const x = await job(); return x && x.state === "done" && x; }, 120000);
    if (phases(done) !== "p1=ok p2=ok p3=ok hub=ok") throw new Error(phases(done));
    const acts = await audit(r1.id);
    const has = (a) => acts.filter((x) => x === a || x.startsWith(a + ":")).length;
    if (has("rollout.resume-after-restart") !== 1 || has("rollout.finish") !== 1 || has("rollout.start") !== 1) throw new Error(acts.join(" "));
    // every host was started exactly once
    const steps = acts.filter((a) => a.startsWith("rollout.step"));
    if (steps.join() !== "rollout.step:p1,rollout.step:p2,rollout.step:p3,rollout.step:hub") throw new Error(steps.join());
    const v = await view();
    if (!v.hosts.every((h) => h.version === V1)) throw new Error(v.hosts.map((h) => h.name + ":" + h.version).join());
  });

  // ---- scenario 2: a build whose simulated upgrade fails stops at the canary ----
  const V2 = "v9.7.13";
  const r2 = await tag(V2);
  await check("a failing canary stops the rollout; the rest are not touched; Retry fails the same way; Cancel gives the host buttons back", async () => {
    const s = await j("POST", "/api/release/rollout", { todo: r2.id, version: V2, commit: SHA, hosts: names, hub: true }, { "X-Comebye-Csrf": r2.csrf });
    if (s.status !== 202) throw new Error(JSON.stringify(s));
    const jb = await until("stopped", async () => { const x = await job(); return x && x.state === "stopped" && x; });
    if (phases(jb) !== "p1=failed p2=pending p3=pending hub=pending" || !/rolled_back: simulated smoke check failed/.test(jb.stop.reason) || !jb.plan[0].output) throw new Error(phases(jb) + " " + JSON.stringify(jb.stop));
    const v = await view();
    if (!v.needs_you || !/stopped at p1/.test(v.needs_text)) throw new Error("not flagged: " + v.needs_text);
    const rs = await j("POST", "/api/release/rollout/resume", { id: jb.id }, { "X-Comebye-Csrf": r2.csrf });
    if (rs.status !== 200) throw new Error("resume " + JSON.stringify(rs));
    const again = await until("stopped again", async () => { const x = await job(); return x && x.state === "stopped" && x.plan[0].attempt === 1 && x; });
    if (phases(again) !== "p1=failed p2=pending p3=pending hub=pending") throw new Error(phases(again));
    const c = await j("POST", "/api/release/rollout/cancel", { id: jb.id }, { "X-Comebye-Csrf": r2.csrf });
    if (c.status !== 200 || (await job()).state !== "cancelled") throw new Error("cancel " + JSON.stringify(c));
    const todo = (await j("GET", "/api/todos/" + r2.id)).body.todo;
    if (!/cancelled/.test(todo.release.rollout_note || "")) throw new Error("no note on the item: " + JSON.stringify(todo.release));
    const up2 = await j("POST", `/api/hosts/p2/release/upgrade`, { todo: r2.id, version: V2 }, { "X-Comebye-Csrf": r2.csrf });
    if (JSON.stringify(up2.body).includes("rollout_active")) throw new Error("still locked after Cancel");
  });

  // ---- the pictures: the finished rollout in the app, at both widths ----
  for (const width of [1280, 390]) {
    await check(`the Fleet card after the rollouts at ${width}: nothing scrolls sideways`, async () => {
      const { browser: b, page } = await launch({ width, height: width >= 1100 ? 800 : 844, dark });
      browser = b;
      await page.goto(base + "/#/peers/~releases", { waitUntil: "load" });
      await page.waitForSelector("#pr-releases [data-fleet] .rl-host", { timeout: 20000 });
      await page.waitForTimeout(500);
      await page.screenshot({ path: `${out}/rolloutlive-${width}.png` });
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
      if (w[0] > w[1]) throw new Error("sideways scroll " + w.join("/"));
      await b.close(); browser = null;
    });
  }
} catch (e) {
  results.push(["setup", false, String(e.stack || e).split("\n").slice(0, 3).join(" | ")]);
} finally {
  if (browser) await browser.close().catch(() => {});
  for (const p of Object.values(procs)) p.kill("SIGKILL");
  fs.rmSync(work, { recursive: true, force: true });
}
let bad = 0;
for (const [n, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"}  ${n}${ok ? "" : "  -- " + why}`); if (!ok) bad++; }
process.exit(bad ? 1 : 0);
