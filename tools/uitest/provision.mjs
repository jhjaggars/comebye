// Adding a host, slice 1: the provisioner role and the price browser, against
// real servers. A hub (-provision-host prov), and a peer "prov" that is a
// provisioner (-provision-config) running a stand-in for the worker tool: a
// script that answers `preflight --json` and `catalog --json` from the
// recorded files in fixtures/ (the catalog is a real read-only AWS fetch of
// us-east-1, nothing account-specific). Nothing is mocked in the browser: the
// app talks to the hub, the hub to the peer, the peer to the script.
//
//   node provision.mjs OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
//
// Each check runs at 1280 and at 390.
import { launch } from "./lib.mjs";
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, sock = process.env.FAKE_SOCK;
if (!bin || !sock) { console.error("provision.mjs: set COMEBYE_BIN and FAKE_SOCK"); process.exit(2); }
const here = path.dirname(fileURLToPath(import.meta.url));
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "prov-ui-"));
const results = [], log = [], procs = [];

const freePort = () => new Promise((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); }); });

// The stand-in for comebye-worker. $4 is the command (after -config X -no-prompt). Files next to it set the mode.
const wdir = path.join(work, "worker");
fs.mkdirSync(wdir);
fs.copyFileSync(path.join(here, "fixtures/provision-preflight.json"), path.join(wdir, "preflight.json"));
fs.copyFileSync(path.join(here, "fixtures/provision-catalog.json"), path.join(wdir, "catalog.json"));
fs.writeFileSync(path.join(wdir, "comebye-worker"), `#!/bin/sh
d=${wdir}
echo "$*" >> $d/argv
case "$4" in
  preflight)
    if [ -f $d/expired ]; then sed 's/"aws": {[^}]*}/"aws": {"state": "expired", "note": "profile comebye-provision: the login expired; run aws login"}/' $d/preflight.json; else cat $d/preflight.json; fi ;;
  catalog)
    if [ -f $d/expired ]; then echo '{"v":1,"error":{"class":"creds_expired","msg":"catalog: the instance types could not be read (creds_expired)"}}'; exit 1; fi
    r=us-east-1; prev=""; for a in "$@"; do [ "$prev" = --region ] && r=$a; prev=$a; done
    sed "s/\\"region\\": \\"[^\\"]*\\"/\\"region\\": \\"$r\\"/" $d/catalog.json ;;
esac
`, { mode: 0o755 });
fs.writeFileSync(path.join(work, "worker.conf"), "# stand-in config\n");

async function serve(name, port, extra = []) {
  const dir = path.join(work, "state-" + name);
  const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-push=false", "-state-dir", dir, "-host-name", name, ...extra], {
    env: { ...process.env, COMEBYE_TEST_INSECURE_PEERS: "1" }, stdio: ["ignore", fs.openSync(path.join(out, `provision-${name}.log`), "w"), fs.openSync(path.join(out, `provision-${name}.log`), "a")] });
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
try {
  await serve("prov", provPort, ["-ledger=false", "-hub", base, "-provision-config", path.join(work, "worker.conf"), "-provision-worker", path.join(wdir, "comebye-worker")]);
  await serve("plain", plainPort);
  // -provision-host must name a peer the hub already knows (as -release-host does): the peers are static here.
  await serve("hub", hubPort, ["-hub-mode", "-peers", `prov=http://127.0.0.1:${provPort},plain=http://127.0.0.1:${plainPort}`, "-provision-host", "prov"]);
  const j = (m, p, body, h = {}) => fetch(base + p, { method: m, headers: { "Content-Type": "application/json", ...h }, body: body && JSON.stringify(body) }).then(async (r) => ({ status: r.status, body: await r.json().catch(() => ({})) }));
  for (let i = 0; i < 80; i++) {
    const r = await j("GET", "/api/peers");
    if (["prov", "plain"].every((n) => (r.body.peers || []).some((p) => p.name === n && p.status === "ok"))) break;
    await new Promise((r) => setTimeout(r, 100));
  }
  let lastForce = 0;
  const argv = () => fs.existsSync(path.join(wdir, "argv")) ? fs.readFileSync(path.join(wdir, "argv"), "utf8") : "";

  // The server side, once.
  await check("the hub lists the provisioner with its preflight, and never a value", async () => {
    const n = (await j("GET", "/api/provision/nodes")).body;
    if (!n.enabled || n.default !== "prov" || n.nodes.length !== 1 || n.nodes.some((x) => x.name === "plain") || n.nodes[0].preflight.aws.state !== "ok" || n.nodes[0].preflight.region !== "us-east-2") throw new Error(JSON.stringify(n));
    if (/tskey|AKIA|641875867446|@/.test(JSON.stringify(n))) throw new Error("the answer holds something credential-shaped");
  });
  await check("an agent token is refused on every route, and refresh needs the page's token", async () => {
    for (const [m, p] of [["GET", "/api/provision/nodes"], ["GET", "/api/provision/catalog?kind=aws&region=us-east-1"], ["POST", "/api/provision/refresh"]]) {
      const r = await j(m, p, m === "POST" ? { what: "nodes" } : undefined, { "X-Comebye-Agent-Token": "nope" });
      if (r.status === 200) throw new Error(m + " " + p + " answered 200 to a wrong token");
    }
    const r = await j("POST", "/api/provision/refresh", { what: "nodes" });
    if (r.status !== 403 || r.body.code !== "csrf") throw new Error("refresh without a token: " + JSON.stringify(r));
    // the peer itself answers only its hub
    const direct = await fetch(`http://127.0.0.1:${provPort}/api/provisioner/preflight`);
    if (direct.status !== 403) throw new Error("the peer answered a browser: " + direct.status);
  });

  for (const width of [1280, 390]) {
    const tag = width >= 1100 ? "desktop" : "phone";
    fs.rmSync(path.join(wdir, "expired"), { force: true });
    await j("POST", "/api/provision/refresh", { what: "nodes" }, { "X-Comebye-Csrf": (await j("GET", "/api/provision/nodes")).body.csrf });
    const { browser: b, page } = await launch({ width, height: width >= 1100 ? 900 : 844, dark });
    browser = b;
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    const shot = (n) => page.screenshot({ path: `${out}/provision-${tag}-${n}.png` }).catch(() => {});
    const noHScroll = async () => {
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector("#pr-prices").scrollWidth, document.querySelector("#pr-prices").clientWidth]);
      if (w[0] > w[1] || w[2] > w[3]) throw new Error("horizontal overflow " + w.join("/"));
    };
    const items = () => page.locator("#pr-prices .pz-item");

    await check(`${tag}: Hosts offers Browse prices, and it opens the Prices tab`, async () => {
      await page.goto(base + "/#/peers", { waitUntil: "load" });
      await page.waitForSelector("#pr-browse");
      if (!(await page.locator("#pr-browse").innerText()).includes("prov")) throw new Error("the entry does not name the default node");
      await shot("hosts");
      await page.locator("#pr-browse").click();
      await page.waitForSelector("#pz-via");
      if ((await page.locator("#pr-tab-prices").getAttribute("aria-selected")) !== "true") throw new Error("the Prices tab is not selected");
      const h = await page.evaluate(() => location.hash);
      if (h !== "#/peers/~prices") throw new Error("hash " + h);
    });

    await check(`${tag}: the preflight shows each credential as present or missing, with the node and region`, async () => {
      await page.waitForSelector("#pr-prices .pz-creds");
      const t = await page.locator("#pr-prices .pz-creds").innerText();
      for (const want of ["AWS", "present", "profile comebye-provision", "Google Cloud", "missing", "Tailscale OAuth client", "OpenShell gateway", "ssh key"]) if (!t.includes(want)) throw new Error(`preflight lacks ${want}: ${t}`);
      if ((await page.locator("#pz-via").innerText()).indexOf("prov (default)") < 0) throw new Error("the node is not marked as the default");
      if ((await page.locator("#pz-region").inputValue()) !== "us-east-2") throw new Error("the region is not the node's own: " + await page.locator("#pz-region").inputValue());
      const opts = await page.locator("#pz-region option").allInnerTexts();
      if (opts.length < 3) throw new Error("the region is not selectable: " + opts);
      await shot("preflight");
    });

    await check(`${tag}: the price list loads for the node's region and hides the small sizes`, async () => {
      await page.waitForSelector("#pr-prices .pz-item", { timeout: 20000 });
      const n = await items().count();
      const t = await page.locator("#pr-prices").innerText();
      if (/t3\.nano|t3\.micro|t3\.small/.test(t)) throw new Error("a small size is shown by default");
      if (!/\(\d+ hidden\)/.test(t)) throw new Error("the page does not say how many sizes are hidden");
      if (n < 20) throw new Error("only " + n + " rows");
      if (!argv().includes("catalog --kind aws --region us-east-2 --json")) throw new Error("the node was not asked for its own region: " + argv());
      await noHScroll();
      await page.locator("#pr-prices .pz-list").scrollIntoViewIfNeeded();
      await page.evaluate(() => { const p = document.querySelector("#pr-prices"), l = p.querySelector(".pz-list"); p.scrollTop = l.offsetTop - 140; });
      await shot("prices");
    });

    await check(`${tag}: Show small sizes adds them, and the region can be changed`, async () => {
      const before = await items().count();
      await page.locator("#pz-small").check();
      await page.waitForFunction((b) => document.querySelectorAll("#pr-prices .pz-item").length > b, before, { timeout: 10000 });
      if (!(await page.locator("#pr-prices").innerText()).includes("t3.nano")) throw new Error("t3.nano is not listed with small sizes shown");
      await page.locator("#pz-small").uncheck();
      await page.waitForFunction((b) => document.querySelectorAll("#pr-prices .pz-item").length === b, before, { timeout: 10000 });
      await page.selectOption("#pz-region", "us-east-1");
      await page.waitForFunction(() => /instance types · us-east-1/i.test(document.querySelector("#pr-prices").innerText), null, { timeout: 20000 });
      if (!argv().includes("--region us-east-1")) throw new Error("the node was not asked for us-east-1");
    });

    await check(`${tag}: a row opens to the spot price in each zone, and capacity can be checked`, async () => {
      await page.locator('#pr-prices .pz-row[data-pz-type="t3.xlarge"]').click();
      await page.waitForSelector("#pr-prices .pz-detail");
      const zones = await page.locator("#pr-prices .pz-detail .pz-zone").count();
      if (zones < 3) throw new Error("zones: " + zones);
      await page.locator("#pr-prices [data-pz-score]").click();
      await page.waitForFunction(() => /\d+\/10/.test(document.querySelector("#pr-prices .pz-detail").innerText), null, { timeout: 20000 });
      if (!argv().includes("--score t3.xlarge")) throw new Error("the score was not asked for: " + argv());
      await shot("detail");
      await noHScroll();
    });

    await check(`${tag}: sorting changes the order, and Arm types are listed but marked`, async () => {
      const first = async () => (await items().first().locator(".pz-type").innerText()).split("\n")[0].trim();
      const a = await first();
      await page.selectOption("#pz-sort", "vcpu");
      await page.waitForFunction((x) => document.querySelector("#pr-prices .pz-item .pz-type")?.innerText.split("\n")[0].trim() !== x, a, { timeout: 5000 });
      await page.selectOption("#pz-sort", "price");
      const t = await page.locator("#pr-prices").innerText();
      if (!t.includes("needs work")) throw new Error("no Arm type is marked");
    });

    await check(`${tag}: an expired login is said plainly, with what to run, and the old prices stay`, async () => {
      fs.writeFileSync(path.join(wdir, "expired"), "");
      await page.locator("#pz-recheck").click();
      await page.waitForFunction(() => /expired/.test(document.querySelector("#pr-prices .pz-creds")?.innerText || ""), null, { timeout: 20000 });
      const t = await page.locator("#pr-prices .pz-creds").innerText();
      if (!/aws login/.test(t) || !t.includes("prov")) throw new Error("no instruction: " + t);
      // a forced refresh fails at the node; the cached list is still shown, with the reason
      const wait = 21000 - (Date.now() - lastForce); // the hub allows one forced refresh per 20 s
      if (wait > 0) await new Promise((r) => setTimeout(r, wait));
      lastForce = Date.now();
      await page.locator("#pz-refresh").click();
      await page.waitForFunction(() => /AWS login expired/.test(document.querySelector("#pr-prices").innerText), null, { timeout: 30000 });
      if ((await items().count()) < 20) throw new Error("the cached prices were dropped");
      await shot("expired");
      await noHScroll();
      fs.rmSync(path.join(wdir, "expired"), { force: true });
    });

    await check(`${tag}: no console errors, no CSP violations`, async () => {
      const bad = log.filter((l) => /^(pageerror|console\.(error|warning))/.test(l));
      if (bad.length) throw new Error(bad.slice(0, 3).join(" | "));
    });
    await b.close();
    browser = null;
  }
} finally {
  if (browser) await browser.close().catch(() => {});
  for (const p of procs) p.kill();
  fs.writeFileSync(path.join(out, "provision-console.log"), log.join("\n"));
}
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"} ${name}${ok ? "" : ": " + why}`); if (!ok) failed++; }
console.log(`provision: ${results.length - failed}/${results.length} passed`);
process.exit(failed ? 1 : 0);
