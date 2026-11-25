// Add host and Destroy… for a KubeVirt worker, against real servers: a hub and
// a peer "kube" that is the in-cluster provisioner (a kubevirt worker config,
// -provision-create on) running a stand-in for the worker tool. It answers a
// preflight with a kubevirt check, reports the VM as created, lists it with
// `status --json` and destroys on request. Nothing is mocked in the browser.
//
//   (a part of fleetjourney.mjs) provkubevirt OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
//
// A part of fleetjourney.mjs (journey.mjs): the script as it was, as the body of run(process); `process` is the one the
// journey gives it (its own argv and env), and process.exit ends the part.
import { launch } from "./lib.mjs";
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";

export async function run(process) {
const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, sock = process.env.FAKE_SOCK;
if (!bin || !sock) { console.error("provkubevirt.mjs: set COMEBYE_BIN and FAKE_SOCK"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "provkube-ui-"));
const results = [], log = [], procs = [];
const freePort = () => new Promise((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); }); });

const wdir = path.join(work, "worker");
fs.mkdirSync(wdir);
const pf = {
  v: 1, checked_at: new Date().toISOString().replace(/\.\d+Z$/, "Z"),
  aws: { state: "missing", note: "aws is not installed" }, gcp: { state: "missing", note: "gcloud is not installed" },
  tailnet: { state: "ok", note: "OAuth client accepted for tag:worker" }, openshell: { state: "missing", note: "no openshell gateway in the config" },
  ssh: { state: "ok", note: "ssh key present" },
  kubevirt: { state: "ok", note: "in-cluster credentials; cluster answers; KubeVirt and CDI Deployed; namespace comebye-workers; 2 node(s) may take a VM" },
};
fs.writeFileSync(path.join(wdir, "preflight.json"), JSON.stringify(pf));
fs.writeFileSync(path.join(wdir, "comebye-worker"), `#!/bin/sh
d=${wdir}
echo "$*" >> $d/argv
case "$4" in
  preflight) cat $d/preflight.json ;;
  create) cat > $d/spec.in; cat $d/create.events ;;
  status) cat $d/status.json ;;
  destroy)
    echo "saving the sessions to the archive" >&2
    echo '{"v":1,"seq":1,"stage":"destroy","step":"1/1","state":"ok","msg":"destroyed"}'
    echo '{"v":1,"provider":"kubevirt","workers":[]}' > $d/status.json ;;
esac
`, { mode: 0o755 });
const stages = ["network", "ssh_key", "data_disk", "instance", "ssh", "bootstrap", "tailnet", "agents", "hub"];
fs.writeFileSync(path.join(wdir, "create.events"), stages.map((st, i) => JSON.stringify({ v: 1, seq: i + 1, stage: st, step: `${i + 1}/9`, state: "ok" })).join("\n") + "\n");
const launched = new Date(Date.now() - 20 * 60e3).toISOString().replace(/\.\d+Z$/, "Z");
const setStatus = (names) => fs.writeFileSync(path.join(wdir, "status.json"), JSON.stringify({ v: 1, provider: "kubevirt", workers: names.map((name) => ({ name, state: "RUNNING", type: "", managed: true, launched_at: launched, last_stop_at: null, caps: {} })) }));
setStatus([]);
fs.writeFileSync(path.join(work, "worker.conf"), "provider=kubevirt\n# stand-in config\n");

async function serve(name, port, extra = []) {
  const dir = path.join(work, "state-" + name);
  const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-test-mode", "-push=false", "-state-dir", dir, "-host-name", name, ...extra], {
    env: { ...process.env, COMEBYE_TEST_INSECURE_PEERS: "1" }, stdio: ["ignore", fs.openSync(path.join(out, `provkubevirt-${name}.log`), "w"), fs.openSync(path.join(out, `provkubevirt-${name}.log`), "a")] });
  procs.push(p);
  for (let i = 0; i < 300; i++) {
    try { if ((await fetch(`http://127.0.0.1:${port}/api/agents`)).ok) return; } catch {}
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(name + " did not start");
}

const hubPort = await freePort(), kubePort = await freePort();
const base = `http://127.0.0.1:${hubPort}`;
let browser;
async function check(name, fn) {
  try { await fn(); results.push([name, true, ""]); }
  catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); }
}
const argv = () => fs.existsSync(path.join(wdir, "argv")) ? fs.readFileSync(path.join(wdir, "argv"), "utf8") : "";
try {
  await serve("kube", kubePort, ["-ledger=false", "-hub", base, "-provision-config", path.join(work, "worker.conf"), "-provision-worker", path.join(wdir, "comebye-worker"), "-provision-create", "-provision-max-hosts", "3"]);
  // No -provision-host: the hub has no default provisioner and the user picks the node.
  await serve("hub", hubPort, ["-hub-mode", "-peers", `kube=http://127.0.0.1:${kubePort}`]);
  for (let i = 0; i < 80; i++) {
    const r = await fetch(base + "/api/peers").then((x) => x.json()).catch(() => ({}));
    if ((r.peers || []).some((p) => p.name === "kube" && p.status === "ok")) break;
    await new Promise((r) => setTimeout(r, 100));
  }

  for (const width of [1280, 390]) {
    const tag = width >= 1100 ? "desktop" : "phone";
    const name = width >= 1100 ? "kvdesk" : "kvphone";
    const { browser: b, page } = await launch({ width, height: width >= 1100 ? 900 : 844, dark });
    browser = b;
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    const shot = (n) => page.screenshot({ path: `${out}/provkubevirt-${tag}-${n}.png` }).catch(() => {});
    const noHScroll = async () => {
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector("#pr-prices").scrollWidth, document.querySelector("#pr-prices").clientWidth]);
      if (w[0] > w[1] || w[2] > w[3]) throw new Error("horizontal overflow " + w.join("/"));
    };

    await check(`${tag}: Add host offers a KubeVirt VM when a KubeVirt provisioner answers, and AWS is not offered`, async () => {
      await page.goto(base + "/#/peers/~prices", { waitUntil: "load" });
      await page.waitForSelector("#az-add", { timeout: 30000 });
      await page.locator("#az-add").click();
      await page.waitForSelector('input[name="az-kind"]');
      const kinds = await page.locator(".az-kinds").innerText();
      if (!/KubeVirt VM/.test(kinds)) throw new Error("kinds: " + kinds);
      if (!(await page.locator('input[name="az-kind"][value="aws"]').isDisabled())) throw new Error("AWS is offered with no login");
      await page.locator('input[name="az-kind"][value="kubevirt"]').check();
      if (!/KubeVirt cluster/.test(await page.locator(".pz-creds").innerText())) throw new Error("the cluster check is not shown");
      await shot("kind");
      await noHScroll();
    });

    await check(`${tag}: the KubeVirt step shows what it is made from, no size picker, no price`, async () => {
      await page.locator("#az-next").click();
      await page.waitForFunction(() => /KubeVirt/.test(document.querySelector(".az-steps li.on")?.innerText || ""), null, { timeout: 5000 });
      const card = await page.locator("#pr-prices").innerText();
      for (const want of ["in-cluster credentials", "comebye-workers", "no price", "data disk is kept"]) if (!card.includes(want)) throw new Error(`the card lacks ${want}: ${card.slice(0, 400)}`);
      if (/Pricing|vCPU|OpenShell/.test(await page.locator(".az-wrap").innerText())) throw new Error("a size picker or sandbox text is shown");
      if (!/no per-hour price/.test(await page.locator("#az-cost").innerText())) throw new Error("footer: " + await page.locator("#az-cost").innerText());
      await shot("card");
      await noHScroll();
      await page.locator("#az-next").click();
      await page.waitForSelector("#az-name");
      if (await page.locator("#az-budget, #az-boot, #az-data, #az-spot").count()) throw new Error("VM options are shown");
      await page.fill("#az-name", name);
      if (await page.locator("#az-next").isDisabled()) throw new Error("Review stays disabled for a good name");
      await shot("options");
    });

    await check(`${tag}: review, acknowledge and create run the tool with three fields and no cost`, async () => {
      await page.locator("#az-next").click();
      await page.waitForSelector("#az-ack", { timeout: 30000 });
      const rv = await page.locator("#pr-prices").innerText();
      if (!/no per-hour price/.test(rv) || !/no budget cap/.test(rv) || !/comebye-workers|cluster/.test(rv)) throw new Error("review: " + rv.slice(0, 400));
      if (/Dry run/.test(rv)) throw new Error("the create path is on but the review says dry run");
      await shot("review");
      await noHScroll();
      await page.locator("#az-ack").check();
      setStatus([name]);
      await page.locator("#az-next").click();
      await page.waitForFunction((n) => new RegExp(n + " is built").test(document.querySelector("#pr-prices").innerText), name, { timeout: 30000 });
      const spec = fs.readFileSync(path.join(wdir, "spec.in"), "utf8");
      if (spec !== `{"destroy_after_hours":72,"idle_minutes":120,"kind":"kubevirt","name":"${name}"}`) throw new Error("the tool read " + spec);
      if (!/create --events --spec -/.test(argv())) throw new Error("no real create: " + argv());
      await shot("built");
      await page.locator("#az-done").click();
      await page.waitForSelector(`#ph-card [data-ph="${name}"]`, { timeout: 20000 });
      const row = await page.locator(`#ph-card [data-ph="${name}"]`).innerText();
      if (!/KubeVirt VM/.test(row) || !/not priced/.test(row)) throw new Error("row: " + row);
      await shot("listed");
      await noHScroll();
    });

    await check(`${tag}: Destroy… asks for the name, keeps the data disk by default, and destroys through the same job`, async () => {
      await page.locator(`[data-ph-destroy="${name}"]`).click();
      await page.waitForSelector("#ph-confirm");
      const dest = await page.locator(".ph-dest").innerText();
      if (!/removes the VM/.test(dest) || !/data disk/.test(dest)) throw new Error(dest);
      if (await page.locator("#ph-del").isChecked()) throw new Error("the data disk box is ticked by default");
      if (!(await page.locator("#ph-go").isDisabled())) throw new Error("enabled before typing");
      await page.fill("#ph-confirm", name);
      await shot("destroy-confirm");
      await noHScroll();
      await page.locator("#ph-go").click();
      // "nothing is destroyed" is also in the panel's own text, so wait for the tool's call and the host's own progress.
      let a;
      for (let i = 0; i < 300 && !(a = argv().trim().split("\n").filter((l) => l.includes(" destroy ")).pop())?.includes(` destroy ${name} `); i++) await new Promise((r) => setTimeout(r, 100));
      if (!new RegExp(`destroy ${name} --yes --events$`).test(a || "")) throw new Error("argv: " + a);
      await page.waitForFunction((n) => [...document.querySelectorAll("#pr-prices .az-dry")].some((e) => e.innerText.includes(n + " is destroyed.")), name, { timeout: 30000 });
      await shot("destroyed");
      await page.locator("#ph-close").click();
      await page.waitForFunction(() => /data disk was kept/.test(document.querySelector("#ph-card")?.innerText || ""), null, { timeout: 20000 });
      if (/0 GB|\$0\.000 a month/.test(await page.locator("#ph-card").innerText())) throw new Error("a zero-size, zero-cost data disk is shown");
      await shot("destroyed-card");
      await noHScroll();
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
  fs.writeFileSync(path.join(out, "provkubevirt-console.log"), log.join("\n"));
}
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"} ${name}${ok ? "" : ": " + why}`); if (!ok) failed++; }
console.log(`provkubevirt: ${results.length - failed}/${results.length} passed`);
process.exit(failed ? 1 : 0);
}
