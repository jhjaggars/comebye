// Adding a host, slice 3: the hosts a provisioner built, what they cost, and
// Destroy…, against real servers. A hub (-provision-host prov) and a peer
// "prov" with the real create path on (-provision-create) running a stand-in
// for the worker tool: it reports an instance as created, answers `status
// --json` from a file, and destroys on request (or fails, when told to).
// Nothing is mocked in the browser.
//
//   node provhosts.mjs OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
import { launch } from "./lib.mjs";
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, sock = process.env.FAKE_SOCK;
if (!bin || !sock) { console.error("provhosts.mjs: set COMEBYE_BIN and FAKE_SOCK"); process.exit(2); }
const here = path.dirname(fileURLToPath(import.meta.url));
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "provhosts-ui-"));
const results = [], log = [], procs = [];
const freePort = () => new Promise((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); }); });

const wdir = path.join(work, "worker");
fs.mkdirSync(wdir);
fs.copyFileSync(path.join(here, "fixtures/provision-preflight.json"), path.join(wdir, "preflight.json"));
fs.copyFileSync(path.join(here, "fixtures/provision-catalog.json"), path.join(wdir, "catalog.json"));
fs.writeFileSync(path.join(wdir, "comebye-worker"), `#!/bin/sh
d=${wdir}
echo "$*" >> $d/argv
case "$4" in
  preflight) cat $d/preflight.json ;;
  catalog)
    r=us-east-1; prev=""; for a in "$@"; do [ "$prev" = --region ] && r=$a; prev=$a; done
    sed "s/\\"region\\": \\"[^\\"]*\\"/\\"region\\": \\"$r\\"/" $d/catalog.json ;;
  create) cat > $d/spec.in; cat $d/create.events ;;
  status) cat $d/status.json ;;
  destroy)
    [ -f $d/destroy.sleep ] && sleep $(cat $d/destroy.sleep)
    if [ -f $d/destroy.fail ]; then echo "archive: the bucket did not answer; nothing was changed" >&2; exit 1; fi
    echo "saving the sessions to the archive" >&2
    echo "demo1 destroyed." >&2
    echo '{"v":1,"seq":1,"stage":"destroy","step":"1/1","state":"ok","msg":"destroyed"}'
    rm -f $d/status.json; echo '{"v":1,"provider":"aws","workers":[]}' > $d/status.json ;;
esac
`, { mode: 0o755 });
const stages = ["network", "ssh_key", "data_disk", "instance", "ssh", "bootstrap", "tailnet", "agents", "hub"];
fs.writeFileSync(path.join(wdir, "create.events"), stages.map((st, i) => JSON.stringify({ v: 1, seq: i + 1, stage: st, step: `${i + 1}/9`, state: "ok" })).join("\n") + "\n");
const launched = new Date(Date.now() - 3 * 3600e3).toISOString().replace(/\.\d+Z$/, "Z");
const status = (extra = {}) => JSON.stringify({ v: 1, provider: "aws", workers: [{ name: "plain", state: "RUNNING", type: "t3.large", mode: "spot", managed: true, launched_at: launched, last_stop_at: null, caps: { budget_cap_usd: 10, destroy_after_hours: 72, launch_price_usd_hour: 0.03 }, ...extra }] });
fs.writeFileSync(path.join(wdir, "status.json"), status());
fs.writeFileSync(path.join(work, "worker.conf"), "# stand-in config\n");

async function serve(name, port, extra = []) {
  const dir = path.join(work, "state-" + name);
  const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-push=false", "-state-dir", dir, "-host-name", name, ...extra], {
    env: { ...process.env, COMEBYE_TEST_INSECURE_PEERS: "1" }, stdio: ["ignore", fs.openSync(path.join(out, `provhosts-${name}.log`), "w"), fs.openSync(path.join(out, `provhosts-${name}.log`), "a")] });
  procs.push(p);
  for (let i = 0; i < 60; i++) {
    try { if ((await fetch(`http://127.0.0.1:${port}/api/agents`)).ok) return; } catch {}
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(name + " did not start");
}

const hubPort = await freePort(), provPort = await freePort(), plainPort = await freePort();
const base = `http://127.0.0.1:${hubPort}`;
let browser;
async function check(name, fn) {
  try { await fn(); results.push([name, true, ""]); }
  catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); }
}
const argv = () => fs.existsSync(path.join(wdir, "argv")) ? fs.readFileSync(path.join(wdir, "argv"), "utf8") : "";
try {
  await serve("prov", provPort, ["-ledger=false", "-hub", base, "-provision-config", path.join(work, "worker.conf"), "-provision-worker", path.join(wdir, "comebye-worker"), "-provision-create"]);
  await serve("plain", plainPort);
  await serve("hub", hubPort, ["-hub-mode", "-peers", `prov=http://127.0.0.1:${provPort},plain=http://127.0.0.1:${plainPort}`, "-provision-host", "prov"]);
  const j = (m, p, body, h = {}) => fetch(base + p, { method: m, headers: { "Content-Type": "application/json", ...h }, body: body && JSON.stringify(body) }).then(async (r) => ({ status: r.status, body: await r.json().catch(() => ({})) }));
  for (let i = 0; i < 80; i++) {
    const r = await j("GET", "/api/peers");
    if (["prov", "plain"].every((n) => (r.body.peers || []).some((p) => p.name === n && p.status === "ok"))) break;
    await new Promise((r) => setTimeout(r, 100));
  }
  const csrf = async () => (await j("GET", "/api/provision/nodes")).body.csrf;
  // The host is named for the plain peer, so its card in Hosts shows the spend line too.
  const spec = { kind: "aws", name: "plain", type: "t3.large", spot: true, boot_disk_gb: 20, data_disk_gb: 50, idle_minutes: 120, max_run: "24h", budget_cap_usd: 10, destroy_after_hours: 72 };
  await check("a real create reaches the books, and the hub lists the host with its spend", async () => {
    const r = await j("POST", "/api/provision", { spec, region: "us-east-2", key: "uitest-key-0001", ack: true }, { "X-Comebye-Csrf": await csrf() });
    if (r.status !== 202 || r.body.dry_run) throw new Error(JSON.stringify(r));
    for (let i = 0; i < 100; i++) { const s = await j("GET", "/api/provision/jobs/" + r.body.id); if (s.body.state === "ok") break; await new Promise((x) => setTimeout(x, 100)); }
    const h = (await j("GET", "/api/provision/hosts?refresh=1")).body;
    const x = (h.hosts || [])[0];
    if (!x || x.name !== "plain" || x.state !== "running" || !(x.spend.usd > 0.05) || !(h.total.usd > 0.05)) throw new Error(JSON.stringify(h));
    if (/tskey|AKIA|@/.test(JSON.stringify(h))) throw new Error("credential-shaped text in the answer");
  });
  await check("an agent token is refused on the hosts routes; destroy needs the page's token", async () => {
    for (const [m, p] of [["GET", "/api/provision/hosts"], ["POST", "/api/provision/hosts/plain/destroy"]]) {
      const r = await j(m, p, m === "POST" ? { confirm: "plain" } : undefined, { "X-Comebye-Agent-Token": "nope" });
      if (r.status === 200 || r.status === 202) throw new Error(m + " " + p + " " + r.status);
    }
    const r = await j("POST", "/api/provision/hosts/plain/destroy", { confirm: "plain" });
    if (r.status !== 403 || r.body.code !== "csrf") throw new Error("destroy without a token: " + JSON.stringify(r));
    const direct = await fetch(`http://127.0.0.1:${provPort}/api/provisioner/hosts`);
    if (direct.status !== 403) throw new Error("the peer answered a browser: " + direct.status);
  });

  for (const width of [1280, 390]) {
    const tag = width >= 1100 ? "desktop" : "phone";
    const { browser: b, page } = await launch({ width, height: width >= 1100 ? 900 : 844, dark });
    browser = b;
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    const shot = (n) => page.screenshot({ path: `${out}/provhosts-${tag}-${n}.png` }).catch(() => {});
    const noHScroll = async () => {
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector("#pr-prices").scrollWidth, document.querySelector("#pr-prices").clientWidth]);
      if (w[0] > w[1] || w[2] > w[3]) throw new Error("horizontal overflow " + w.join("/"));
    };
    const text = () => page.locator("#pr-prices").innerText();

    await check(`${tag}: Prices shows the host with its state, spend so far, and the total`, async () => {
      await page.goto(base + "/#/peers/~prices", { waitUntil: "load" });
      await page.waitForSelector("#ph-card .ph-row", { timeout: 20000 });
      const t = await page.locator("#ph-card").innerText();
      for (const want of ["plain", "running", "so far", "spent so far across 1 host", "t3.large", "spot", "cap $10.00", "% used"]) if (!t.includes(want)) throw new Error(`lacks ${want}: ${t}`);
      if (!/≈ \$0\.\d+ so far/.test(t)) throw new Error("no figure: " + t);
      await shot("hosts-card");
      await noHScroll();
    });

    await check(`${tag}: the host's card in Hosts carries its spend`, async () => {
      await page.goto(base + "/#/peers", { waitUntil: "load" });
      await page.waitForSelector('.pr-card[data-name="plain"] .pr-spend', { timeout: 20000 });
      const t = await page.locator('.pr-card[data-name="plain"] .pr-spend').innerText();
      if (!/built from the app · ≈ \$[\d.]+ so far/.test(t)) throw new Error(t);
      await shot("peer-card");
      await page.goto(base + "/#/peers/~prices", { waitUntil: "load" });
      await page.waitForSelector("#ph-card .ph-row");
    });

    await check(`${tag}: Destroy… asks for the name, and the button stays off until it matches`, async () => {
      await page.locator('[data-ph-destroy="plain"]').click();
      await page.waitForSelector("#ph-confirm");
      const go = page.locator("#ph-go");
      if (!(await go.isDisabled())) throw new Error("enabled before typing");
      await page.fill("#ph-confirm", "plai");
      if (!(await go.isDisabled())) throw new Error("enabled for a partial name");
      await page.fill("#ph-confirm", "PLAIN");
      if (!(await go.isDisabled())) throw new Error("enabled for another case");
      const t = await page.locator(".ph-dest").innerText();
      if (!/saves plain's sessions/.test(t) || !/nothing is destroyed/.test(t) || !/data disk/.test(t)) throw new Error(t);
      await shot("destroy-confirm");
      await noHScroll();
      if (argv().includes(" destroy ")) throw new Error("the tool ran before the confirmation");
    });

    await check(`${tag}: a destroy that fails leaves the host listed, with why`, async () => {
      fs.writeFileSync(path.join(wdir, "destroy.fail"), "");
      await page.fill("#ph-confirm", "plain");
      await page.locator("#ph-go").click();
      await page.waitForFunction(() => /Not destroyed/.test(document.querySelector("#pr-prices").innerText), null, { timeout: 30000 });
      const t = await text();
      if (!/bucket did not answer/.test(t)) throw new Error("no reason: " + t.slice(0, 400));
      await shot("destroy-failed");
      await noHScroll();
      await page.locator("#ph-close").click();
      await page.waitForSelector('[data-ph-destroy="plain"]');
      if (!/running/.test(await page.locator("#ph-card .ph-row").innerText())) throw new Error("the host is no longer shown as running");
      fs.rmSync(path.join(wdir, "destroy.fail"), { force: true });
    });

    await check(`${tag}: Destroy with the name typed saves sessions first, removes the host and freezes the bill`, async () => {
      await page.locator('[data-ph-destroy="plain"]').click();
      await page.fill("#ph-confirm", "plain");
      await page.locator("#ph-go").click();
      await page.waitForFunction(() => /is destroyed/.test(document.querySelector("#pr-prices").innerText), null, { timeout: 30000 });
      const a = argv().trim().split("\n").filter((l) => l.includes(" destroy ")).pop();
      if (!/destroy plain --yes --events$/.test(a)) throw new Error("argv: " + a);
      await shot("destroy-done");
      await page.locator("#ph-close").click();
      await page.waitForFunction(() => /final bill ≈ \$[\d.]+/.test(document.querySelector("#ph-card")?.innerText || ""), null, { timeout: 20000 });
      const t = await page.locator("#ph-card").innerText();
      if (!/destroyed/.test(t) || !/data disk was kept/.test(t) || /Destroy…/.test(t)) throw new Error(t);
      const fin = t.match(/final bill ≈ (\$[\d.]+)/)[1];
      await new Promise((r) => setTimeout(r, 1500));
      await page.locator("#ph-refresh").click();
      await page.waitForFunction(() => !/Refreshing/.test(document.querySelector("#ph-card").innerText), null, { timeout: 20000 });
      if (!(await page.locator("#ph-card").innerText()).includes(fin)) throw new Error("the final bill moved from " + fin);
      await shot("destroyed-card");
      await noHScroll();
    });

    await check(`${tag}: no console errors, no CSP violations`, async () => {
      const bad = log.filter((l) => /^(pageerror|console\.(error|warning))/.test(l));
      if (bad.length) throw new Error(bad.slice(0, 3).join(" | "));
    });
    await b.close();
    browser = null;
    break; // one pass: the destroyed host is gone for the next width; the phone width is checked below
  }

  // Phone width: a new host to look at, and to destroy.
  {
    const r = await j("POST", "/api/provision", { spec: { ...spec, name: "phone1" }, region: "us-east-2", key: "uitest-key-0002", ack: true }, { "X-Comebye-Csrf": await csrf() });
    for (let i = 0; i < 100; i++) { const s = await j("GET", "/api/provision/jobs/" + r.body.id); if (s.body.state === "ok") break; await new Promise((x) => setTimeout(x, 100)); }
    fs.writeFileSync(path.join(wdir, "status.json"), status({ name: "phone1" }));
    await new Promise((x) => setTimeout(x, 2500)); // the hub holds back a repeated refresh for 2 s
    await j("GET", "/api/provision/hosts?refresh=1");
    const { browser: b, page } = await launch({ width: 390, height: 844, dark });
    browser = b;
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    const shot = (n) => page.screenshot({ path: `${out}/provhosts-phone-${n}.png` }).catch(() => {});
    const noHScroll = async () => {
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector("#pr-prices").scrollWidth, document.querySelector("#pr-prices").clientWidth]);
      if (w[0] > w[1] || w[2] > w[3]) throw new Error("horizontal overflow " + w.join("/"));
    };
    await check("phone: the list, one live and one destroyed host, fits at 390 and Destroy… is a 44px target", async () => {
      await page.goto(base + "/#/peers/~prices", { waitUntil: "load" });
      await page.waitForSelector("#ph-card .ph-row", { timeout: 20000 });
      if ((await page.locator("#ph-card .ph-row").count()) !== 2) throw new Error("rows: " + await page.locator("#ph-card .ph-row").count());
      const box = await page.locator('[data-ph-destroy="phone1"]').boundingBox();
      if (!box || box.height < 44) throw new Error("target " + JSON.stringify(box));
      await shot("list");
      await noHScroll();
      await page.locator('[data-ph-destroy="phone1"]').click();
      await page.fill("#ph-confirm", "phone1");
      await shot("confirm");
      await noHScroll();
    });
    await check("phone: no console errors", async () => {
      const bad = log.filter((l) => /^(pageerror|console\.(error|warning))/.test(l));
      if (bad.length) throw new Error(bad.slice(0, 3).join(" | "));
    });
    await b.close();
    browser = null;
  }
} finally {
  if (browser) await browser.close().catch(() => {});
  for (const p of procs) p.kill();
  fs.writeFileSync(path.join(out, "provhosts-console.log"), log.join("\n"));
}
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"} ${name}${ok ? "" : ": " + why}`); if (!ok) failed++; }
console.log(`provhosts: ${results.length - failed}/${results.length} passed`);
process.exit(failed ? 1 : 0);
