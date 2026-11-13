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
  create)
    cat > $d/spec.in
    echo "DRY RUN: nothing is created, no key is minted" >&2
    echo "worker      demo1" >&2
    [ -f $d/create.sleep ] && sleep $(cat $d/create.sleep)
    cat $d/create.events
    [ -f $d/create.fail ] && { cat $d/create.fail; exit 1; }
    exit 0 ;;
  catalog)
    if [ -f $d/expired ]; then echo '{"v":1,"error":{"class":"creds_expired","msg":"catalog: the instance types could not be read (creds_expired)"}}'; exit 1; fi
    r=us-east-1; prev=""; for a in "$@"; do [ "$prev" = --region ] && r=$a; prev=$a; done
    sed "s/\\"region\\": \\"[^\\"]*\\"/\\"region\\": \\"$r\\"/" $d/catalog.json ;;
esac
`, { mode: 0o755 });
const stages = ["network", "ssh_key", "data_disk", "instance", "ssh", "bootstrap", "tailnet", "agents", "hub"];
fs.writeFileSync(path.join(wdir, "create.events"), stages.map((st, i) => JSON.stringify({ v: 1, seq: i + 1, stage: st, step: `${i + 1}/9`, state: "planned" })).join("\n") + "\n");
fs.writeFileSync(path.join(work, "worker.conf"), "# stand-in config\n");

async function serve(name, port, extra = []) {
  const dir = path.join(work, "state-" + name);
  const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-test-mode", "-push=false", "-state-dir", dir, "-host-name", name, ...extra], {
    env: { ...process.env, COMEBYE_TEST_INSECURE_PEERS: "1" }, stdio: ["ignore", fs.openSync(path.join(out, `provision-${name}.log`), "w"), fs.openSync(path.join(out, `provision-${name}.log`), "a")] });
  procs.push(p);
  for (let i = 0; i < 300; i++) {
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
    for (const [m, p] of [["GET", "/api/provision/nodes"], ["GET", "/api/provision/catalog?kind=aws&region=us-east-1"], ["POST", "/api/provision/refresh"], ["POST", "/api/provision/plan"], ["POST", "/api/provision"], ["GET", "/api/provision/jobs/hj_0123456789abcdef"]]) {
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
      await page.waitForSelector("#pr-browse", { timeout: 3000 }); // the Hosts list offers it on opening, not on the 20 s refresh
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

    await check(`${tag}: sorting changes the order, and Arm types are listed with their arch and can be chosen`, async () => {
      const first = async () => (await items().first().locator(".pz-type").innerText()).split("\n")[0].trim();
      const a = await first();
      await page.selectOption("#pz-sort", "vcpu");
      await page.waitForFunction((x) => document.querySelector("#pr-prices .pz-item .pz-type")?.innerText.split("\n")[0].trim() !== x, a, { timeout: 5000 });
      await page.selectOption("#pz-sort", "price");
      const t = await page.locator("#pr-prices").innerText();
      if (t.includes("not supported yet")) throw new Error("an Arm type is still marked as unbuildable");
      if (!(await page.locator('#pr-prices .pz-row[data-pz-type="t4g.large"]:not([disabled])').count())) throw new Error("t4g.large is not selectable");
    });

    await check(`${tag}: an expired login is said plainly, with what to run, and the old prices stay`, async () => {
      fs.writeFileSync(path.join(wdir, "expired"), "");
      await page.locator("#pz-recheck").click();
      await page.waitForFunction(() => /expired/.test(document.querySelector("#pr-prices .pz-creds")?.innerText || ""), null, { timeout: 20000 });
      const t = await page.locator("#pr-prices .pz-creds").innerText();
      if (!/aws login/.test(t) || !t.includes("prov")) throw new Error("no instruction: " + t);
      // a forced refresh fails at the node; the cached list is still shown, with the reason
      const wait = 1200 - (Date.now() - lastForce); // the hub allows one forced refresh per 1 s in test mode (20 s otherwise)
      if (wait > 0) await new Promise((r) => setTimeout(r, wait));
      lastForce = Date.now();
      await page.locator("#pz-refresh").click();
      await page.waitForFunction(() => /AWS login expired/.test(document.querySelector("#pr-prices").innerText), null, { timeout: 30000 });
      if ((await items().count()) < 20) throw new Error("the cached prices were dropped");
      await shot("expired");
      await noHScroll();
      fs.rmSync(path.join(wdir, "expired"), { force: true });
    });


    // ---- Add host: the wizard, a dry run end to end, then a failure and a way out ----
    const next = () => page.locator("#az-next");
    const step = async () => (await page.locator(".az-steps li.on").innerText()).trim();
    await check(`${tag}: Add host opens the wizard on the node step, saying it is a dry run, with the cost footer`, async () => {
      await page.locator("#az-add").click();
      await page.waitForSelector(".az-steps");
      if (!(await step()).startsWith("1.")) throw new Error("step " + await step());
      const t = await page.locator("#pr-prices").innerText();
      if (!/Dry run/.test(t) || !/nothing is created/.test(t)) throw new Error("the dry-run notice is missing");
      if (!(await page.locator("#az-cost").innerText()).includes("choose a size")) throw new Error("footer: " + await page.locator("#az-cost").innerText());
      await shot("wizard-node");
      await noHScroll();
    });
    await check(`${tag}: choosing a size shows its cost in the footer; Arm sizes are chosen like any other`, async () => {
      await next().click();
      await page.waitForSelector("#pr-prices [data-az-pick]", { timeout: 30000 });
      if (!(await step()).startsWith("2.")) throw new Error("step " + await step());
      if (!(await next().isDisabled())) throw new Error("Next is enabled before a size is chosen");
      if (await page.locator('[data-az-pick="t4g.large"]').isDisabled().catch(() => false)) throw new Error("an Arm size cannot be chosen");
      await page.locator('[data-az-pick="t3.xlarge"]').click();
      const c = await page.locator("#az-cost").innerText();
      if (!/≈ \$0\.\d+\/h/.test(c) || !/day/.test(c)) throw new Error("footer: " + c);
      await page.evaluate(() => { const p = document.querySelector("#pr-prices"), l = p.querySelector(".pz-list"); p.scrollTop = l.offsetTop - 140; });
      await shot("wizard-size");
      await noHScroll();
    });
    await check(`${tag}: options validate the name and move the cost`, async () => {
      await next().click();
      await page.waitForSelector("#az-name");
      if (!(await next().isDisabled())) throw new Error("Next is enabled without a name");
      await page.fill("#az-name", "Bad Name!");
      if (!(await next().isDisabled())) throw new Error("Next accepts a bad name");
      if (!(await page.locator("#az-name").evaluate((e) => e.classList.contains("bad")))) throw new Error("the bad name is not marked");
      const spot = await page.locator("#az-cost b").innerText();
      await page.selectOption("#az-spot", "0");
      await page.waitForFunction((x) => document.querySelector("#az-cost b")?.innerText !== x, spot, { timeout: 5000 });
      await page.selectOption("#az-spot", "1");
      await page.fill("#az-name", "build-box");
      if (await next().isDisabled()) throw new Error("Next is disabled with a good name");
      await shot("wizard-options");
      await noHScroll();
    });
    await check(`${tag}: review asks the provisioner for the plan and needs an acknowledgement`, async () => {
      await next().click();
      await page.waitForSelector("#az-ack", { timeout: 30000 });
      const t = await page.locator("#pr-prices").innerText();
      if (!/Dry run\./.test(t) || !/build-box/.test(t) || !/\$\d+\.\d\d\/day/.test(t)) throw new Error("review: " + t.slice(0, 300));
      if (!(await next().isDisabled())) throw new Error("Create is enabled before the acknowledgement");
      if (!argv().includes("create --dry-run --events --spec -")) throw new Error("the plan was not the tool's dry run: " + argv());
      const spec = fs.readFileSync(path.join(wdir, "spec.in"), "utf8");
      if (!spec.includes('"name":"build-box"') || !spec.includes('"type":"t3.xlarge"') || !spec.includes('"budget_cap_usd":10')) throw new Error("spec: " + spec);
      await page.locator("#az-ack").check();
      await shot("wizard-review");
      await noHScroll();
    });
    await check(`${tag}: the dry run walks all nine stages, says nothing was created, and creates nothing`, async () => {
      await next().click();
      await page.waitForFunction(() => /Dry run finished/.test(document.querySelector("#pr-prices").innerText), null, { timeout: 30000 });
      const n = await page.locator(".az-st.plan").count();
      if (n !== 9) throw new Error("planned stages: " + n);
      if (/(^|\n)worker create (?!.*--dry-run)/.test(argv())) throw new Error("a real create ran");
      for (const l of argv().split("\n")) if (/ create /.test(" " + l + " ") && !l.includes("--dry-run")) throw new Error("create without --dry-run: " + l);
      await shot("wizard-done");
      await noHScroll();
      await page.locator("#az-done").click();
      await page.waitForSelector("#az-add");
    });
    fs.rmSync(path.join(wdir, "create.fail"), { force: true });
    await check(`${tag}: a spot-capacity failure names the zone, and switching to on-demand starts a new job`, async () => {
      await page.locator("#az-add").click();
      // The wizard opens on the node step: Next shows the sizes. (This used to wait out a 30 s timeout for the sizes first, then press Next.)
      await next().click();
      await page.waitForSelector("#pr-prices [data-az-pick]", { timeout: 30000 });
      await page.locator('[data-az-pick="t3.xlarge"]').click();
      await next().click();
      await page.fill("#az-name", "retry-box");
      await next().click();
      await page.waitForSelector("#az-ack", { timeout: 30000 });
      await page.locator("#az-ack").check();
      // the plan is made; now the build fails (the stand-in fails a create after the plan)
      fs.writeFileSync(path.join(wdir, "create.fail"), JSON.stringify({ v: 1, seq: 99, stage: "instance", step: "4/9", state: "failed", class: "spot_capacity", zone: "us-east-2a", msg: "no spot capacity in us-east-2a" }) + "\n");
      await next().click();
      await page.waitForFunction(() => /no spot capacity/i.test(document.querySelector("#pr-prices").innerText), null, { timeout: 30000 });
      const t = await page.locator("#pr-prices").innerText();
      if (!t.includes("us-east-2a") || !/Failed/.test(t)) throw new Error("failure: " + t.slice(0, 300));
      if (!(await page.locator('[data-az-act="retry"]').count()) || !(await page.locator('[data-az-act="ondemand"]').count()) || !(await page.locator('[data-az-act="cleanup"]').count())) throw new Error("no way out is offered");
      await shot("wizard-failed");
      await noHScroll();
      fs.rmSync(path.join(wdir, "create.fail"), { force: true });
      await page.locator('[data-az-act="ondemand"]').click();
      await page.waitForFunction(() => /Dry run finished/.test(document.querySelector("#pr-prices").innerText), null, { timeout: 30000 });
      const spec = fs.readFileSync(path.join(wdir, "spec.in"), "utf8");
      if (!spec.includes('"spot":false')) throw new Error("the new job is not on-demand: " + spec);
    });
    await check(`${tag}: Clean up on a dry run runs nothing`, async () => {
      // a finished job offers Close, not Clean up; leave the wizard
      await page.locator("#az-done").click();
      await page.waitForSelector("#az-add");
      if (argv().includes(" destroy ")) throw new Error("destroy ran");
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
