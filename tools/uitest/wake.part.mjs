// Waking a stopped worker (issue #66, slice 1), against real servers: a hub
// (-provision-host prov), a peer "prov" with the real path on that runs a
// stand-in for the worker tool, and two workers kv1 and kv2 that the cloud
// says are STOPPED (the stand-in's status). The workers' own servers are not
// running, so the hub sees them down; the stand-in's `start` takes a few
// seconds, prints its progress, then this script brings the worker's server
// up, as a real boot would. Nothing is mocked in the browser, and nothing
// touches a real VM: the stand-in is a shell script.
//
//   (a part of fleetjourney.mjs) wake OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
//
// A part of fleetjourney.mjs (journey.mjs): the script as it was, as the body of run(process); `process` is the one the
// journey gives it (its own argv and env), and process.exit ends the part.
import { launch } from "./lib.mjs";
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

export async function run(process) {
const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, sock = process.env.FAKE_SOCK;
if (!bin || !sock) { console.error("wake.mjs: set COMEBYE_BIN and FAKE_SOCK"); process.exit(2); }
const here = path.dirname(fileURLToPath(import.meta.url));
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "wake-ui-"));
const results = [], log = [], procs = [];
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
    env: { ...process.env, COMEBYE_TEST_INSECURE_PEERS: "1" }, stdio: ["ignore", fs.openSync(path.join(out, `wake-${name}.log`), "w"), fs.openSync(path.join(out, `wake-${name}.log`), "a")] });
  procs.push(p);
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
try {
  await serve("prov", provPort, ["-ledger=false", "-hub", base, "-provision-config", path.join(work, "worker.conf"), "-provision-worker", path.join(wdir, "comebye-worker"), "-provision-create"]);
  await serve("hub", hubPort, ["-hub-mode", "-peers", `prov=http://127.0.0.1:${provPort},kv1=http://127.0.0.1:${ports.kv1},kv2=http://127.0.0.1:${ports.kv2}`, "-provision-host", "prov"]);
  for (let i = 0; i < 80; i++) {
    const r = await fetch(base + "/api/peers").then((x) => x.json()).catch(() => ({}));
    if ((r.peers || []).some((p) => p.name === "prov" && p.status === "ok")) break;
    await new Promise((r) => setTimeout(r, 100));
  }

  for (const [width, host] of [[1280, "kv1"], [390, "kv2"]]) {
    const tag = width >= 1100 ? "desktop" : "phone";
    const { browser: b, page } = await launch({ width, height: width >= 1100 ? 900 : 844, dark });
    browser = b;
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    const shot = (n) => page.screenshot({ path: `${out}/wake-${tag}-${n}.png` }).catch(() => {});
    const card = `.pr-card[data-name="${host}"]`, line = `.wk-line[data-wk-for="${host}"]`;
    const noHScroll = async () => {
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
      if (w[0] > w[1]) throw new Error("horizontal overflow " + w.join("/"));
    };

    await check(`${tag}: a stopped worker shows Stopped with a Start button of at least 40px`, async () => {
      await page.goto(base + "/#/peers", { waitUntil: "load" });
      await page.waitForSelector(`${line} [data-wk-start="${host}"]`, { timeout: 30000 }).catch(async (e) => { await shot("stopped-missing"); throw e; });
      const t = await page.locator(card).innerText();
      if (!/Stopped/.test(t) || !/idle off/.test(t) || !/kubevirt/.test(t)) throw new Error("card: " + t);
      const box = await page.locator(`${line} [data-wk-start]`).boundingBox();
      if (!box || box.height < 40) throw new Error("button " + JSON.stringify(box));
      await shot("stopped");
      await noHScroll();
    });

    await check(`${tag}: tap Start, see the tool's progress, then the worker is up`, async () => {
      await page.locator(`${line} [data-wk-start]`).click();
      await page.waitForSelector(`${line} .wk-prog`, { timeout: 15000 });
      await page.waitForFunction((sel) => /starting|ssh config|waiting for comebye/.test(document.querySelector(sel)?.innerText || ""), `${line} .wk-prog`, { timeout: 15000 });
      const t = await page.locator(card).innerText();
      if (!/Starting/.test(t)) throw new Error("card while starting: " + t);
      await shot("starting");
      await noHScroll();
      await page.waitForFunction((sel) => /ssh config|waiting for comebye/.test(document.querySelector(sel)?.innerText || ""), `${line} .wk-prog`, { timeout: 15000 });
      await shot("progress");
      await page.waitForFunction((n) => { const c = document.querySelector(`.pr-card[data-name="${n}"]`); return c && /Connected/.test(c.innerText) && !document.querySelector(`.wk-line[data-wk-for="${n}"]`); }, host, { timeout: 90000 });
      await shot("up");
      await noHScroll();
    });
    await check(`${tag}: the tool ran start for ${host} only, once`, async () => {
      const a = fs.readFileSync(path.join(wdir, "argv"), "utf8").split("\n").filter((l) => / start /.test(l));
      const mine = a.filter((l) => l.endsWith(" start " + host));
      if (mine.length !== 1) throw new Error(a.join(" | "));
    });
    await check(`${tag}: no console errors`, async () => {
      const bad = log.filter((l) => /^(pageerror|console\.(error|warning))/.test(l));
      if (bad.length) throw new Error(bad.slice(0, 3).join(" | "));
    });
    await b.close();
    browser = null;
  }
} finally {
  clearInterval(boot);
  if (browser) await browser.close().catch(() => {});
  for (const p of procs) p.kill();
  fs.writeFileSync(path.join(out, "wake-console.log"), log.join("\n"));
}
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"} ${name}${ok ? "" : ": " + why}`); if (!ok) failed++; }
console.log(`wake: ${results.length - failed}/${results.length} passed`);
process.exit(failed ? 1 : 0);
}
