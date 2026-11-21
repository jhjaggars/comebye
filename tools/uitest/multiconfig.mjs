// A provisioner with two worker configs (issue #66 follow-up): AWS, whose login
// has expired (its stand-in's `status` fails with the real error text), and
// KubeVirt, whose stand-in says kv1 is STOPPED. Started as
//   -provision-config aws.conf:aws-worker,kv.conf:kv-worker
// the hub must still show kv1 stopped with a Start button (at 1280 and 390 px),
// say that aws could not be read (for aws only), and Start must run the
// KubeVirt stand-in with its own -config and never the AWS one. Nothing is
// mocked in the browser and nothing touches a real VM: the stand-ins are shell
// scripts.
//
//   node multiconfig.mjs OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
import { launch } from "./lib.mjs";
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, sock = process.env.FAKE_SOCK;
if (!bin || !sock) { console.error("multiconfig.mjs: set COMEBYE_BIN and FAKE_SOCK"); process.exit(2); }
const here = path.dirname(fileURLToPath(import.meta.url));
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "multiconfig-ui-"));
const results = [], log = [], procs = [];
const freePort = () => new Promise((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); }); });

const wdir = path.join(work, "worker");
fs.mkdirSync(wdir);
fs.copyFileSync(path.join(here, "fixtures/provision-preflight.json"), path.join(wdir, "preflight.json"));
fs.copyFileSync(path.join(here, "fixtures/provision-catalog.json"), path.join(wdir, "catalog.json"));
fs.writeFileSync(path.join(wdir, "state-kv1"), "STOPPED");
const stoppedAt = new Date(Date.now() - 3 * 3600e3).toISOString().replace(/\.\d+Z$/, "Z");
// the AWS stand-in: every status fails as an expired login does
fs.writeFileSync(path.join(wdir, "aws-worker"), `#!/bin/sh
d=${wdir}
echo "aws $*" >> $d/argv
case "$4" in
  preflight) cat $d/preflight.json ;;
  catalog) cat $d/catalog.json ;;
  status) echo "aws: [ERROR]: Your session has expired. Please reauthenticate using 'aws login'." >&2; exit 255 ;;
  *) echo "the aws stand-in was asked to $4" >&2; exit 1 ;;
esac
`, { mode: 0o755 });
fs.writeFileSync(path.join(wdir, "kv-worker"), `#!/bin/sh
d=${wdir}
echo "kv $*" >> $d/argv
case "$4" in
  status) printf '{"v":1,"provider":"kubevirt","workers":[{"name":"kv1","state":"%s","type":"4 vCPU, 32Gi","managed":true,"created_at":"${stoppedAt}","launched_at":null,"last_stop_at":null,"caps":{}}]}\\n' "$(cat $d/state-kv1)" ;;
  start)
    echo "starting $5" >&2; sleep 2
    echo "waiting for comebye on $5 to be active" >&2; sleep 2
    echo RUNNING > $d/state-$5; touch $d/started-$5
    echo "$5 is up, comebye v-test" ;;
esac
`, { mode: 0o755 });
fs.writeFileSync(path.join(work, "aws.conf"), "provider=aws\n");
fs.writeFileSync(path.join(work, "kv.conf"), "provider=kubevirt\n");

async function serve(name, port, extra = []) {
  const dir = path.join(work, "state-" + name);
  const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-test-mode", "-push=false", "-state-dir", dir, "-host-name", name, ...extra], {
    env: { ...process.env, COMEBYE_TEST_INSECURE_PEERS: "1" }, stdio: ["ignore", fs.openSync(path.join(out, `multiconfig-${name}.log`), "w"), fs.openSync(path.join(out, `multiconfig-${name}.log`), "a")] });
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
const ports = { kv1: await freePort() };
const base = `http://127.0.0.1:${hubPort}`;
let browser;
// a real boot: when the stand-in says the worker is up, its server comes up
const booting = new Set();
const boot = setInterval(() => {
  for (const n of ["kv1"]) if (fs.existsSync(path.join(wdir, `started-${n}`)) && !booting.has(n)) { booting.add(n); serve(n, ports[n]).catch(() => {}); }
}, 200);
try {
  await serve("prov", provPort, ["-ledger=false", "-hub", base, "-provision-config", `${path.join(work, "aws.conf")}:${path.join(wdir, "aws-worker")},${path.join(work, "kv.conf")}:${path.join(wdir, "kv-worker")}`, "-provision-create"]);
  await serve("hub", hubPort, ["-hub-mode", "-peers", `prov=http://127.0.0.1:${provPort},kv1=http://127.0.0.1:${ports.kv1}`, "-provision-host", "prov"]);
  for (let i = 0; i < 80; i++) {
    const r = await fetch(base + "/api/peers").then((x) => x.json()).catch(() => ({}));
    if ((r.peers || []).some((p) => p.name === "prov" && p.status === "ok")) break;
    await new Promise((r) => setTimeout(r, 100));
  }

  for (const [width, host] of [[390, "kv1"], [1280, "kv1"]]) {
    const tag = width >= 1100 ? "desktop" : "phone";
    const { browser: b, page } = await launch({ width, height: width >= 1100 ? 900 : 844, dark });
    browser = b;
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    const shot = (n) => page.screenshot({ path: `${out}/multiconfig-${tag}-${n}.png` }).catch(() => {});
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

    await check(`${tag}: the AWS failure is said for aws only, and kv1 is not hidden by it`, async () => {
      await page.goto(base + "/#/peers", { waitUntil: "load" });
      await page.waitForSelector(".pr-cloud", { timeout: 30000 }).catch(async (e) => { await shot("no-aws-note"); throw e; });
      const t = await page.locator(".pr-cloud").innerText();
      if (!/aws: .*session has expired/i.test(t)) throw new Error("card: " + t);
      if (/kubevirt/.test(t)) throw new Error("kubevirt blamed: " + t);
      if (!(await page.locator(`.wk-line[data-wk-for="kv1"] [data-wk-start]`).count())) throw new Error("no Start button beside the aws error");
      await shot("aws-error");
      await noHScroll();
    });
    if (width < 1100) { await check("phone: no Start was tapped (kv1 stays stopped for the desktop pass)", async () => {
      if (fs.existsSync(path.join(wdir, "started-kv1"))) throw new Error("it started");
    }); }
    if (width >= 1100) {
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
    await check(`${tag}: Start ran the KubeVirt stand-in with its own -config, once, and never the AWS one`, async () => {
      const a = fs.readFileSync(path.join(wdir, "argv"), "utf8").split("\n").filter((l) => / start /.test(l));
      if (a.length !== 1 || !a[0].startsWith("kv ") || !a[0].includes("-config " + path.join(work, "kv.conf") + " ") || !a[0].endsWith(" start " + host)) throw new Error(a.join(" | "));
    });
    }
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
  fs.writeFileSync(path.join(out, "multiconfig-console.log"), log.join("\n"));
}
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"} ${name}${ok ? "" : ": " + why}`); if (!ok) failed++; }
console.log(`multiconfig: ${results.length - failed}/${results.length} passed`);
process.exit(failed ? 1 : 0);
