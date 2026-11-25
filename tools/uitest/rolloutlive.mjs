// The rolling rollout end to end, on a simulated fleet of real servers: a hub (-release-simulate, hub mode) and three peers
// (-ledger=false, -release-simulate: they "upgrade" by writing a record and then report the new version). Nothing is mocked:
// Roll out to all goes through the hub's routes, the loop runs in the hub process, and the hub is KILLED and restarted in the
// middle of the rollout, which must carry on from release/rollout.json. Then a version whose simulated upgrade fails (patch
// 13) stops the rollout at the first peer (there is no canary by default) and leaves the rest untouched. A fourth peer, "prov",
// is -hub-only (the in-cluster provisioner): it reports with_hub, is never upgraded in place, and is done after the hub's
// deploy ("upgraded with the hub"). Last, a peer that is down is skipped, and caught up by itself when it is back.
// The hub sends its notifications to a local ntfy stand-in: each rollout sends exactly one note when it first finishes or
// stops (issue #141, slice 5), none after the hub restart, the Retry or the catch-up.
//
//   node rolloutlive.mjs OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
import { launch } from "./lib.mjs";
import { spawn } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
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
// The ntfy stand-in: every notification the hub publishes.
const notes = [];
const ntfy = await new Promise((res) => {
  const srv = http.createServer((req, rsp) => { let b = ""; req.on("data", (c) => (b += c)); req.on("end", () => { try { notes.push(JSON.parse(b)); } catch {} rsp.end("{}"); }); });
  srv.listen(0, "127.0.0.1", () => res(srv));
});
const hubArgs = ["-hub-mode", "-ntfy-url", `http://127.0.0.1:${ntfy.address().port}`, "-ntfy-topic", "rollout-live"];
const rolloutNotes = (v) => notes.filter((n) => (n.title || "").startsWith(v + " roll"));

function start(name, port, extra) {
  const log = fs.openSync(path.join(out, `rollout-${name}.log`), "a");
  const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-test-mode", "-push=false", "-state-dir", path.join(work, "state-" + name), "-host-name", name, "-release-simulate", ...extra], {
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

const hubPort = await freePort(), peerPorts = [await freePort(), await freePort(), await freePort()], provPort = await freePort();
const names = ["p1", "p2", "p3"], fleet = [...names, "prov"];
const base = `http://127.0.0.1:${hubPort}`;
const j = async (m, p, body, hdr = {}) => { const r = await fetch(base + p, { method: m, headers: { "Content-Type": "application/json", ...hdr }, body: body && JSON.stringify(body) }); return { status: r.status, body: await r.json().catch(() => ({})) }; };
let browser;
try {
  names.forEach((n, i) => start(n, peerPorts[i], ["-ledger=false", "-hub", base]));
  start("prov", provPort, ["-ledger=false", "-hub-only", "-hub", base]);
  start("hub", hubPort, hubArgs);
  await Promise.all([up(hubPort), ...peerPorts.map(up), up(provPort)]);
  [...peerPorts, provPort].forEach(async (port, i) => { const n = fleet[i]; const r = await j("POST", "/api/peers", { name: n, urls: [`http://127.0.0.1:${port}`] }); if (r.status !== 201) throw new Error("add " + n + JSON.stringify(r)); });
  for (let i = 0; i < 100; i++) { const r = await j("GET", "/api/peers"); if (fleet.every((n) => (r.body.peers || []).some((p) => p.name === n && p.status === "ok"))) break; await sleep(100); }

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
  await check("the plan offers the peers, then the hub, then prov upgraded with the hub; prov says with_hub and has no button", async () => {
    const v = await view();
    const plan = v.rollout_plan || [];
    // No canary by default: every peer is in the "peers" wave, in fleet order (the order they announced themselves,
    // which is not fixed here); the hub is last, and prov (with_hub) after it.
    const steps = plan.map((s) => s.host + "/" + s.wave + (s.mode ? "/" + s.mode : ""));
    if ([...steps.slice(0, -2).sort(), ...steps.slice(-2)].join() !== "p1/peers,p2/peers,p3/peers,hub/hub/deploy,prov/hub/with_hub") throw new Error(JSON.stringify(plan));
    const prov = v.hosts.find((h) => h.name === "prov");
    if (!prov || !prov.with_hub || prov.enabled) throw new Error("prov: " + JSON.stringify(prov));
    if (v.rollout_plan_error) throw new Error("plan error: " + v.rollout_plan_error);
  });
  for (const width of [1280, 390]) {
    await check(`Roll out to all in the app at ${width}: one confirm lists the peers, the hub, and prov "with the hub"; nothing scrolls sideways`, async () => {
      const { browser: b, page } = await launch({ width, height: width >= 1100 ? 800 : 844, dark });
      browser = b;
      await page.goto(base + "/#/peers/~releases", { waitUntil: "load" });
      await page.waitForSelector('#pr-releases [data-rel="rollout"]', { timeout: 20000 });
      if (await page.$("[data-rollout-plan-error]")) throw new Error("the card says Roll out to all is not available");
      await page.click('#pr-releases [data-rel="rollout"]');
      await page.waitForSelector("#pr-releases .rl-order [data-with-hub]", { timeout: 5000 });
      const txt = await page.$eval("#pr-releases .rel-confirm", (el) => el.textContent);
      if (!/Confirm: roll out v9\.7\.1 to 5 hosts/.test(txt) || !/upgraded with the hub by Bump hub/.test(txt)) throw new Error(txt);
      const order = await page.$$eval("#pr-releases .rl-order li", (els) => els.map((e) => e.dataset.host));
      if (order.slice(-2).join() !== "hub,prov") throw new Error(order.join());
      await page.screenshot({ path: `${out}/rolloutlive-confirm-${width}.png` });
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
      if (w[0] > w[1]) throw new Error("sideways scroll " + w.join("/"));
      await b.close(); browser = null;
    });
  }
  await check("Roll out to all starts a rollout; one peer at a time, with no canary and no soak", async () => {
    const s = await j("POST", "/api/release/rollout", { todo: r1.id, version: V1, commit: SHA, hosts: names, hub: true }, { "X-Comebye-Csrf": r1.csrf });
    if (s.status !== 202) throw new Error(JSON.stringify(s));
    const dup = await j("POST", "/api/release/rollout", { todo: r1.id, version: V1, commit: SHA, hosts: names, hub: true }, { "X-Comebye-Csrf": r1.csrf });
    if (dup.status !== 409) throw new Error("a second start was accepted: " + dup.status);
    // p1 is upgraded alone: while it is not done, nobody else is touched; and no step ever soaks.
    const jb = await until("p1 upgrading", async () => { const x = await job(); return x && x.plan[0].phase !== "pending" && x; });
    if (jb.plan[0].phase !== "ok" && jb.plan.slice(1).some((x) => x.phase !== "pending")) throw new Error("others touched: " + phases(jb));
    if (jb.plan.some((x) => x.phase === "soaking" || x.wave === "canary") || jb.canary) throw new Error("a canary or a soak: " + JSON.stringify(jb.plan));
    const up1 = await j("POST", `/api/hosts/p2/release/upgrade`, { todo: r1.id, version: V1 }, { "X-Comebye-Csrf": r1.csrf });
    if (up1.status !== 409 || !JSON.stringify(up1.body).includes("rollout_active")) throw new Error("manual upgrade was not refused: " + JSON.stringify(up1));
  });
  await check("the hub is killed (SIGKILL) mid-rollout and restarted: the rollout carries on from rollout.json and finishes", async () => {
    procs.hub.kill("SIGKILL");
    await sleep(500);
    const file = JSON.parse(fs.readFileSync(path.join(work, "state-hub", "release", "rollout.json"), "utf8"));
    if (file.state !== "running") throw new Error("the file says " + file.state);
    start("hub", hubPort, hubArgs);
    await up(hubPort);
    for (let i = 0; i < 100; i++) { const r = await j("GET", "/api/peers"); if (names.every((n) => (r.body.peers || []).some((p) => p.name === n && p.status === "ok"))) break; await sleep(100); }
    const done = await until("done", async () => { const x = await job(); return x && x.state === "done" && x; }, 120000);
    if (phases(done) !== "p1=ok p2=ok p3=ok hub=ok prov=ok" || done.plan[4].detail !== "upgraded with the hub") throw new Error(phases(done) + " " + JSON.stringify(done.plan[4]));
    const acts = await audit(r1.id);
    const has = (a) => acts.filter((x) => x === a || x.startsWith(a + ":")).length;
    if (has("rollout.resume-after-restart") !== 1 || has("rollout.finish") !== 1 || has("rollout.start") !== 1) throw new Error(acts.join(" "));
    // every host was started exactly once
    const steps = acts.filter((a) => a.startsWith("rollout.step"));
    if (steps.join() !== "rollout.step:p1,rollout.step:p2,rollout.step:p3,rollout.step:hub") throw new Error(steps.join());
    const v = await view();
    if (!v.hosts.every((h) => h.version === V1)) throw new Error(v.hosts.map((h) => h.name + ":" + h.version).join());
  });
  await check("the finished rollout sent one note, from the restarted hub, and the item says the same line", async () => {
    await until("the note", async () => rolloutNotes(V1).length > 0, 10000);
    await sleep(1500); // a second one would have come by now
    const n = rolloutNotes(V1);
    if (n.length !== 1 || n[0].title !== V1 + " rolled out" || n[0].message !== V1 + " is on every host") throw new Error(JSON.stringify(n));
    const acts = await audit(r1.id);
    if (acts.filter((a) => a.startsWith("rollout.told")).length !== 1) throw new Error(acts.join(" "));
    const todo = (await j("GET", "/api/todos/" + r1.id)).body.todo;
    if (todo.state === "open" && todo.release.rollout_note !== V1 + " is on every host") throw new Error(JSON.stringify(todo.release));
  });

  // ---- scenario 2: a build whose simulated upgrade fails stops at the first peer ----
  const V2 = "v9.7.13";
  const r2 = await tag(V2);
  await check("a failing first peer stops the rollout; the rest are not touched; Retry fails the same way; Cancel gives the host buttons back", async () => {
    const s = await j("POST", "/api/release/rollout", { todo: r2.id, version: V2, commit: SHA, hosts: names, hub: true }, { "X-Comebye-Csrf": r2.csrf });
    if (s.status !== 202) throw new Error(JSON.stringify(s));
    const jb = await until("stopped", async () => { const x = await job(); return x && x.state === "stopped" && x; });
    if (phases(jb) !== "p1=failed p2=pending p3=pending hub=pending prov=pending" || !/rolled_back: simulated smoke check failed/.test(jb.stop.reason) || !jb.plan[0].output) throw new Error(phases(jb) + " " + JSON.stringify(jb.stop));
    const v = await view();
    if (!v.needs_you || !/stopped at p1/.test(v.needs_text)) throw new Error("not flagged: " + v.needs_text);
    const rs = await j("POST", "/api/release/rollout/resume", { id: jb.id }, { "X-Comebye-Csrf": r2.csrf });
    if (rs.status !== 200) throw new Error("resume " + JSON.stringify(rs));
    const again = await until("stopped again", async () => { const x = await job(); return x && x.state === "stopped" && x.plan[0].attempt === 1 && x; });
    if (phases(again) !== "p1=failed p2=pending p3=pending hub=pending prov=pending") throw new Error(phases(again));
    const c = await j("POST", "/api/release/rollout/cancel", { id: jb.id }, { "X-Comebye-Csrf": r2.csrf });
    if (c.status !== 200 || (await job()).state !== "cancelled") throw new Error("cancel " + JSON.stringify(c));
    const todo = (await j("GET", "/api/todos/" + r2.id)).body.todo;
    if (!/cancelled/.test(todo.release.rollout_note || "")) throw new Error("no note on the item: " + JSON.stringify(todo.release));
    const up2 = await j("POST", `/api/hosts/p2/release/upgrade`, { todo: r2.id, version: V2 }, { "X-Comebye-Csrf": r2.csrf });
    if (JSON.stringify(up2.body).includes("rollout_active")) throw new Error("still locked after Cancel");
  });
  await check("the stop sent one note naming the host and the reason; the Retry that stopped again sent none", async () => {
    const n = rolloutNotes(V2);
    if (n.length !== 1 || n[0].title !== V2 + " rollout stopped" || !/^v9\.7\.13 stopped at p1: .*simulated smoke check failed/.test(n[0].message) || !n[0].tags.includes("warning")) throw new Error(JSON.stringify(n));
  });

  // ---- scenario 3: p3 is down; it is skipped, the rest go, and it is caught up by itself when it is back ----
  const V3 = "v9.7.2";
  const r3 = await tag(V3);
  await check("a peer that is down is skipped without holding up the others; the item can be closed skipping it; it is upgraded by itself when it is back", async () => {
    procs.p3.kill("SIGKILL");
    await until("p3 offline", async () => (await view()).hosts.some((h) => h.name === "p3" && h.offline), 30000);
    const s = await j("POST", "/api/release/rollout", { todo: r3.id, version: V3, commit: SHA, hosts: names, hub: true }, { "X-Comebye-Csrf": r3.csrf });
    if (s.status !== 202) throw new Error(JSON.stringify(s));
    const done = await until("done with p3 skipped", async () => { const x = await job(); return x && x.state === "done" && x; }, 120000);
    if (phases(done) !== "p1=ok p2=ok p3=skipped hub=ok prov=ok" || !done.plan[2].catch_up) throw new Error(phases(done) + " " + JSON.stringify(done.plan[2]));
    // The user calls it done while p3 is away: plain done is refused, "done, skip offline hosts" closes it and names p3.
    const d0 = await j("POST", `/api/todos/${r3.id}/done`, {});
    if (d0.status !== 409 || !/--skip-offline/.test(JSON.stringify(d0.body))) throw new Error("plain done: " + JSON.stringify(d0));
    const d1 = await j("POST", `/api/todos/${r3.id}/done`, { skip_offline: true });
    if (d1.status !== 200 || d1.body.todo.state !== "done" || !/p3 offline/.test(d1.body.todo.release.rollout_note || "")) throw new Error("skip offline: " + JSON.stringify(d1));
    start("p3", peerPorts[2], ["-ledger=false", "-hub", base]);
    await up(peerPorts[2]);
    const caught = await until("p3 caught up", async () => { const x = await job(); return x && x.state === "done" && x.plan[2].phase === "ok" && x; }, 90000);
    if (phases(caught) !== "p1=ok p2=ok p3=ok hub=ok prov=ok") throw new Error(phases(caught));
    const acts = await audit(r3.id);
    if (!acts.includes("rollout.catchup:p3")) throw new Error(acts.join(" "));
    await sleep(1500);
    const n = rolloutNotes(V3);
    if (n.length !== 1 || n[0].message !== V3 + " is on every host (1 behind until they're back: p3)") throw new Error("notes: " + JSON.stringify(n));
    const v = await view();
    if (!v.hosts.every((h) => h.version === V3)) throw new Error(v.hosts.map((h) => h.name + ":" + h.version).join());
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
  ntfy.close();
  fs.rmSync(work, { recursive: true, force: true });
}
let bad = 0;
for (const [n, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"}  ${n}${ok ? "" : "  -- " + why}`); if (!ok) bad++; }
process.exit(bad ? 1 : 0);
