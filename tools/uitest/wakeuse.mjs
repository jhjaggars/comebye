// Using a session on a stopped worker (issue #66, slice 3), against real servers: a hub
// (-provision-host prov), a peer "prov" with the real path on that runs a
// stand-in for the worker tool, and two workers kv1 and kv2 that the cloud
// says are STOPPED (the stand-in's status). The workers' own servers are not
// running, so the hub sees them down; the stand-in's `start` takes a few
// seconds, prints its progress, then this script brings the worker's server
// up, as a real boot would. Nothing is mocked in the browser, and nothing
// touches a real VM: the stand-in is a shell script.
//
// Opening the session shows a Start banner and starts nothing; sending a message starts the host,
// shows "starting kv1…", and then delivers the message once the host has joined.
//
//   node wakeuse.mjs OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
import { launch } from "./lib.mjs";
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, sock = process.env.FAKE_SOCK;
if (!bin || !sock) { console.error("wakeuse.mjs: set COMEBYE_BIN and FAKE_SOCK"); process.exit(2); }
const here = path.dirname(fileURLToPath(import.meta.url));
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "wakeuse-ui-"));
const results = [], log = [], procs = [], byName = {};
const freePort = () => new Promise((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); }); });

const wdir = path.join(work, "worker");
fs.mkdirSync(wdir);
fs.copyFileSync(path.join(here, "fixtures/provision-preflight.json"), path.join(wdir, "preflight.json"));
fs.copyFileSync(path.join(here, "fixtures/provision-catalog.json"), path.join(wdir, "catalog.json"));
for (const n of ["kv1", "kv2"]) fs.writeFileSync(path.join(wdir, `state-${n}`), "STOPPED");
const stoppedAt = new Date(Date.now() - 3 * 3600e3).toISOString().replace(/\.\d+Z$/, "Z");
fs.writeFileSync(path.join(wdir, "comebye-worker"), `#!/bin/sh
d=${wdir}
echo "$*" >> $d/argv
one() { printf '{"name":"%s","state":"%s","type":"4cpu-16Gi","managed":true,"created_at":"${stoppedAt}","launched_at":"${stoppedAt}","last_stop_at":null,"caps":{}}' "$1" "$(cat $d/state-$1)"; }
case "$4" in
  preflight) cat $d/preflight.json ;;
  catalog) cat $d/catalog.json ;;
  status) printf '{"v":1,"provider":"kubevirt","workers":[%s,%s]}\\n' "$(one kv1)" "$(one kv2)" ;;
  start)
    echo "starting $5" >&2; sleep 2
    echo "rewriting the ssh config for $5" >&2; sleep 2
    echo "waiting for comebye on $5 to be active" >&2; sleep 2
    echo RUNNING > $d/state-$5; touch $d/started-$5
    echo "$5 is up, comebye v-test" ;;
esac
`, { mode: 0o755 });
fs.writeFileSync(path.join(work, "worker.conf"), "# stand-in config\n");

async function serve(name, port, extra = []) {
  const dir = path.join(work, "state-" + name);
  const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-test-mode", "-push=false", "-state-dir", dir, "-host-name", name, ...extra], {
    env: { ...process.env, COMEBYE_TEST_INSECURE_PEERS: "1" }, stdio: ["ignore", fs.openSync(path.join(out, `wakeuse-${name}.log`), "w"), fs.openSync(path.join(out, `wakeuse-${name}.log`), "a")] });
  procs.push(p);
  byName[name] = p;
  for (let i = 0; i < 300; i++) {
    try { if ((await fetch(`http://127.0.0.1:${port}/api/agents`)).ok) return; } catch {}
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(name + " did not start");
}
async function check(name, fn) {
  try { await fn(); results.push([name, true, ""]); }
  catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); }
}

const hubPort = await freePort(), provPort = await freePort();
const ports = { kv1: await freePort(), kv2: await freePort() };
const base = `http://127.0.0.1:${hubPort}`;
let browser;
// a real boot: when the stand-in says the worker is up, its server comes up
const booting = new Set();
const boot = setInterval(() => {
  for (const n of ["kv1", "kv2"]) if (fs.existsSync(path.join(wdir, `started-${n}`)) && !booting.has(n)) { booting.add(n); serve(n, ports[n]).catch(() => {}); }
}, 200);
const argvStarts = (h) => (fs.existsSync(path.join(wdir, "argv")) ? fs.readFileSync(path.join(wdir, "argv"), "utf8").split("\n") : []).filter((l) => l.endsWith(" start " + h)).length;
try {
  // the workers are up first, so that the hub has seen their sessions; each powers off while its session is open
  await serve("kv1", ports.kv1);
  await serve("kv2", ports.kv2);
  await serve("prov", provPort, ["-ledger=false", "-hub", base, "-provision-config", path.join(work, "worker.conf"), "-provision-worker", path.join(wdir, "comebye-worker"), "-provision-create"]);
  await serve("hub", hubPort, ["-hub-mode", "-peers", `prov=http://127.0.0.1:${provPort},kv1=http://127.0.0.1:${ports.kv1},kv2=http://127.0.0.1:${ports.kv2}`, "-provision-host", "prov"]);
  const seen = async (n) => ((await fetch(base + "/api/agents").then((x) => x.json()).catch(() => ({}))).agents || []).some((a) => a.host === n);
  for (let i = 0; i < 150 && !((await seen("kv1")) && (await seen("kv2"))); i++) await new Promise((r) => setTimeout(r, 200));

  for (const [width, host] of [[1280, "kv1"], [390, "kv2"]]) {
    const tag = width >= 1100 ? "desktop" : "phone";
    const { browser: b, page } = await launch({ width, height: width >= 1100 ? 900 : 844, dark });
    browser = b;
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    const shot = (n) => page.screenshot({ path: `${out}/wakeuse-${tag}-${n}.png` }).catch(() => {});
    const noHScroll = async () => {
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
      if (w[0] > w[1]) throw new Error("horizontal overflow " + w.join("/"));
    };

    await check(`${tag}: opening a session on the stopped ${host} shows a Start banner and starts nothing`, async () => {
      await page.goto(base + "/", { waitUntil: "load" });
      await page.waitForFunction((h) => state.agents.some((a) => a.host === h), host, { timeout: 30000 });
      await page.evaluate((h) => { location.hash = agentHash(state.agents.find((a) => a.host === h)); }, host);
      await page.waitForFunction((h) => state.selected && state.selected.startsWith(h + "/"), host, { timeout: 15000 });
      // the worker powers off with the session open: it is no longer listed, the host is down
      byName[host].kill();
      await page.waitForFunction((h) => { const x = state.hosts.find((y) => y.name === h); return x && x.status === "down"; }, host, { timeout: 40000 });
      await page.waitForSelector(`#wk-banner:not([hidden]) [data-wk-start="${host}"]`, { timeout: 40000 }).catch(async (e) => { await shot("banner-missing"); const d = await page.evaluate(() => ({ sel: state.selected, ph: !!ph.data, loading: ph.loading, st: state.hosts.map((h) => h.name + ":" + h.status), phh: ph.data && ph.data.hosts.map((h) => h.name + ":" + h.state), nodes: pz.nodes && pz.nodes.enabled, ledger: ledger.on, hash: location.hash })); throw new Error(e.message.split("\n")[0] + " " + JSON.stringify(d)); });
      const t = await page.locator("#wk-banner").innerText();
      if (!t.includes(`${host} is stopped. Start it?`)) throw new Error("banner: " + t);
      const box = await page.locator(`#wk-banner [data-wk-start]`).boundingBox();
      if (!box || box.height < 40) throw new Error("button " + JSON.stringify(box));
      await new Promise((r) => setTimeout(r, 1500));
      if (argvStarts(host) !== 0) throw new Error("opening started the host");
      await shot("banner");
      await noHScroll();
    });

    await check(`${tag}: sending a message starts ${host} once, shows starting, then delivers it`, async () => {
      const text = `hello ${host} after the nap`;
      await page.locator("#prompt-text").click();
      await page.keyboard.type(text);
      await page.locator("#btn-send").click();
      await page.waitForSelector(`#wk-banner .wk-prog`, { timeout: 20000 }).catch(async (e) => { await shot("starting-missing"); throw e; });
      await page.waitForFunction((h) => new RegExp(`starting ${h}`).test(document.querySelector("#chat")?.innerText || "") || /starting/.test(document.querySelector("#wk-banner")?.innerText || ""), host, { timeout: 15000 });
      const t = await page.locator("#wk-banner").innerText();
      if (!/Starting/.test(t)) throw new Error("banner while starting: " + t);
      await shot("starting");
      await noHScroll();
      // the host joins, the prompt is delivered: the banner goes and the message shows as sent
      await page.waitForFunction(() => document.querySelector("#wk-banner")?.hidden, null, { timeout: 90000 });
      await page.waitForFunction((x) => (document.querySelector("#chat")?.innerText || "").includes(x), text, { timeout: 30000 }).catch(async (e) => { await shot("delivered-missing"); throw e; });
      await shot("delivered");
      await noHScroll();
      if (argvStarts(host) !== 1) throw new Error("starts: " + argvStarts(host));
      if (await page.evaluate(() => promptValue().trim())) throw new Error("the text is still in the box");
    });
    await check(`${tag}: no console errors`, async () => {
      const bad = log.filter((l) => /^(pageerror|console\.(error|warning))/.test(l) && !/Failed to load resource/.test(l));
      if (bad.length) throw new Error(bad.slice(0, 3).join(" | "));
    });
    await b.close();
    browser = null;
  }
} finally {
  clearInterval(boot);
  if (browser) await browser.close().catch(() => {});
  for (const p of procs) p.kill();
  fs.writeFileSync(path.join(out, "wakeuse-console.log"), log.join("\n"));
}
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"} ${name}${ok ? "" : ": " + why}`); if (!ok) failed++; }
console.log(`wakeuse: ${results.length - failed}/${results.length} passed`);
process.exit(failed ? 1 : 0);
