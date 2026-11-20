// Polish for stopped workers (issue #66, slice 4), against real servers: a hub
// (-provision-host prov, -release-simulate), a peer "prov" that runs a stand-in
// for the worker tool, kv1 (STOPPED in the cloud: its server is not running, so the
// hub sees it down), a plain host p2 that went quiet (offline, the cloud knows
// nothing of it) and a tagged release. Checks, at 390 and 1280 px: the launch
// picker marks kv1 stopped and can pick it (Start boots it and its folders then
// load); the rollout confirm says "stopped, will be started" for kv1 and keeps
// "offline" for p2. Nothing touches a real VM: the stand-in is a shell script.
//
//   node wakepolish.mjs OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
import { launch } from "./lib.mjs";
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, sock = process.env.FAKE_SOCK;
if (!bin || !sock) { console.error("wakepolish.mjs: set COMEBYE_BIN and FAKE_SOCK"); process.exit(2); }
const here = path.dirname(fileURLToPath(import.meta.url));
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "wakepolish-ui-"));
const results = [], log = [], procs = [], byName = {};
const freePort = () => new Promise((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); }); });

const wdir = path.join(work, "worker");
fs.mkdirSync(wdir);
fs.copyFileSync(path.join(here, "fixtures/provision-preflight.json"), path.join(wdir, "preflight.json"));
fs.copyFileSync(path.join(here, "fixtures/provision-catalog.json"), path.join(wdir, "catalog.json"));
for (const n of ["kv1"]) fs.writeFileSync(path.join(wdir, `state-${n}`), "STOPPED");
const stoppedAt = new Date(Date.now() - 3 * 3600e3).toISOString().replace(/\.\d+Z$/, "Z");
fs.writeFileSync(path.join(wdir, "comebye-worker"), `#!/bin/sh
d=${wdir}
echo "$*" >> $d/argv
one() { printf '{"name":"%s","state":"%s","type":"4cpu-16Gi","managed":true,"created_at":"${stoppedAt}","launched_at":"${stoppedAt}","last_stop_at":null,"caps":{}}' "$1" "$(cat $d/state-$1)"; }
case "$4" in
  preflight) cat $d/preflight.json ;;
  catalog) cat $d/catalog.json ;;
  status) printf '{"v":1,"provider":"kubevirt","workers":[%s]}\\n' "$(one kv1)" ;;
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
    env: { ...process.env, COMEBYE_TEST_INSECURE_PEERS: "1" }, stdio: ["ignore", fs.openSync(path.join(out, `wakepolish-${name}.log`), "w"), fs.openSync(path.join(out, `wakepolish-${name}.log`), "a")] });
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

const SHA = "a3f9c1e5d2b7480cf6e1a9b34d70c8e2f15a6b09", V = "v9.8.1";
const hubPort = await freePort(), provPort = await freePort(), p2Port = await freePort(), kvPort = await freePort();
const base = `http://127.0.0.1:${hubPort}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const j = async (m, p, body, hdr = {}) => { const r = await fetch(base + p, { method: m, headers: { "Content-Type": "application/json", ...hdr }, body: body && JSON.stringify(body) }); return { status: r.status, body: await r.json().catch(() => ({})) }; };
let browser;
const booting = new Set();
const boot = setInterval(() => { if (fs.existsSync(path.join(wdir, "started-kv1")) && !booting.has("kv1")) { booting.add("kv1"); serve("kv1", kvPort, ["-release-simulate"]).catch(() => {}); } }, 200);
try {
  await serve("p2", p2Port, ["-ledger=false", "-hub", base, "-release-simulate"]);
  await serve("prov", provPort, ["-ledger=false", "-hub", base, "-release-simulate", "-provision-config", path.join(work, "worker.conf"), "-provision-worker", path.join(wdir, "comebye-worker"), "-provision-create"]);
  await serve("hub", hubPort, ["-hub-mode", "-release-simulate", "-peers", `prov=http://127.0.0.1:${provPort},p2=http://127.0.0.1:${p2Port},kv1=http://127.0.0.1:${kvPort}`, "-provision-host", "prov"]);
  // p2 is a plain host that goes quiet (offline, no cloud says why); kv1 is stopped in the cloud (its server is not running)
  for (let i = 0; i < 100; i++) { const r = await j("GET", "/api/peers"); if (["prov", "p2"].every((n) => (r.body.peers || []).some((p) => p.name === n && p.status === "ok"))) break; await sleep(150); }
  byName.p2.kill();
  const t = await j("POST", "/api/todos", { question: "", kind: "release", no_wait: true, release: { version: V, commit: SHA } });
  const id = t.body.todo.id;
  const d0 = (await j("GET", "/api/release/" + id)).body;
  const ap = await j("POST", `/api/release/${id}/approve`, { version: V, commit: SHA }, { "X-Comebye-Csrf": d0.csrf });
  if (ap.status !== 202) throw new Error("approve " + JSON.stringify(ap));
  for (let i = 0; i < 100; i++) { const x = await j("GET", "/api/todos/" + id); if (x.body.todo.release.state === "tagged") break; await sleep(150); }
  for (let i = 0; i < 100; i++) { const r = await j("GET", "/api/peers"); if (["kv1", "p2"].every((n) => (r.body.peers || []).some((p) => p.name === n && p.status !== "ok"))) break; await sleep(150); }

  for (const [width] of [[390], [1280]]) {
    const tag = width >= 1100 ? "desktop" : "phone";
    const { browser: b, page } = await launch({ width, height: width >= 1100 ? 900 : 844, dark });
    browser = b;
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    const shot = (n) => page.screenshot({ path: `${out}/wakepolish-${tag}-${n}.png` }).catch(() => {});
    const noHScroll = async () => { const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]); if (w[0] > w[1]) throw new Error("horizontal overflow " + w.join("/")); };
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForFunction(() => state.hosts && state.hosts.some((h) => h.name === "kv1" && h.status !== "ok"), null, { timeout: 40000 });

    await check(`${tag}: the launch picker marks the stopped kv1 and it can be picked; an offline p2 stays disabled`, async () => {
      await page.waitForFunction(() => ph.data && ph.data.hosts && ph.data.hosts.some((h) => h.name === "kv1" && h.state === "stopped"), null, { timeout: 40000 }).catch(async () => { await page.evaluate(() => loadProvHosts && loadProvHosts(true)); await sleep(3000); });
      await page.evaluate(() => openLaunch());
      await page.waitForSelector('#ln-hosts [data-host="kv1"]', { timeout: 15000 });
      await page.waitForFunction(() => /stopped/.test(document.querySelector('#ln-hosts [data-host="kv1"]')?.innerText || ""), null, { timeout: 30000 });
      const kv = await page.evaluate(() => { const b = document.querySelector('#ln-hosts [data-host="kv1"]'); return { t: b.innerText, off: b.disabled }; });
      if (kv.off) throw new Error("kv1 is disabled: " + JSON.stringify(kv));
      const p2 = await page.evaluate(() => { const b = document.querySelector('#ln-hosts [data-host="p2"]'); return b && { t: b.innerText, off: b.disabled }; });
      if (!p2 || !p2.off || /stopped/.test(p2.t)) throw new Error("p2: " + JSON.stringify(p2));
      await page.locator('#ln-hosts [data-host="kv1"]').click();
      await page.waitForSelector('#ln-list [data-wk-start="kv1"]', { timeout: 15000 });
      const msg = await page.locator("#ln-list").innerText();
      if (!/kv1 is stopped/.test(msg)) throw new Error("list: " + msg);
      await shot("launch-picker");
      await noHScroll();
      if (width < 420) {
        const box = await page.locator('#ln-list [data-wk-start="kv1"]').boundingBox();
        if (!box || box.width < 300 || box.height < 40) throw new Error("the button is not full width on a phone: " + JSON.stringify(box));
      }
    });

    await check(`${tag}: the rollout confirm lists kv1 as stopped, will be started, and p2 as offline`, async () => {
      await page.evaluate(() => { launch.open && hideLaunch(); });
      await page.goto(base + "/#/peers/~releases", { waitUntil: "load" });
      await page.waitForSelector('[data-rel="rollout"]', { timeout: 40000 });
      await page.locator('[data-rel="rollout"]').click();
      await page.waitForSelector(".rel-confirm .rl-order li", { timeout: 15000 });
      const li = await page.evaluate(() => Object.fromEntries([...document.querySelectorAll(".rel-confirm .rl-order li")].map((x) => [x.dataset.host, x.innerText.replace(/\s+/g, " ")])));
      if (!/stopped, will be started \(about \d+ min of runtime\)/.test(li.kv1 || "")) throw new Error("kv1: " + JSON.stringify(li));
      if (/offline/.test(li.kv1)) throw new Error("kv1 still says offline: " + li.kv1);
      if (!/offline/.test(li.p2 || "") || /will be started/.test(li.p2)) throw new Error("p2: " + JSON.stringify(li));
      await page.locator(".rel-confirm").scrollIntoViewIfNeeded();
      await shot("rollout-confirm");
      await noHScroll();
      if (width < 420) {
        const box = await page.locator('.rel-confirm [data-rel="confirm-rollout"]').boundingBox();
        if (!box || box.width < 300) throw new Error("the confirm button is not full width: " + JSON.stringify(box));
      }
      await page.locator('[data-rel="cancel"]').click();
    });
    await check(`${tag}: no console errors`, async () => {
      const bad = log.filter((l) => /^(pageerror|console\.(error|warning))/.test(l) && !/Failed to load resource/.test(l));
      if (bad.length) throw new Error(bad.slice(0, 3).join(" | "));
    });
    if (width >= 1100) {
      await check("desktop: Start in the picker boots kv1, and its folder list then loads", async () => {
        await page.goto(base + "/", { waitUntil: "load" });
        await page.waitForFunction(() => state.hosts && state.hosts.some((h) => h.name === "kv1"), null, { timeout: 40000 });
        await page.evaluate(() => openLaunch());
        await page.waitForSelector('#ln-hosts [data-host="kv1"]', { timeout: 15000 });
        await page.locator('#ln-hosts [data-host="kv1"]').click();
        await page.locator('#ln-list [data-wk-start="kv1"]').click();
        await page.waitForSelector("#ln-list .wk-prog", { timeout: 20000 });
        await shot("launch-starting");
        // kv1 answered the folder request: its folders, or (a machine with no ~/code) its own "no launch root" answer
        await page.waitForFunction(() => { const l = document.querySelector("#ln-list"); return l && !l.querySelector("[data-wk-launch]") && !/Loading/.test(l.innerText) && (l.querySelector(".ln-dir") || /launch root/.test(l.innerText)); }, null, { timeout: 120000 }).catch(async (e) => { await shot("launch-folders-missing"); throw e; });
        await shot("launch-folders");
      });
    }
    await b.close();
    browser = null;
  }
} finally {
  clearInterval(boot);
  if (browser) await browser.close().catch(() => {});
  for (const p of procs) p.kill();
  fs.writeFileSync(path.join(out, "wakepolish-console.log"), log.join("\n"));
}
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"} ${name}${ok ? "" : ": " + why}`); if (!ok) failed++; }
console.log(`wakepolish: ${results.length - failed}/${results.length} passed`);
process.exit(failed ? 1 : 0);
