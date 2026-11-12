// The Peers screen and the host defaults in the launch sheet, against real
// servers: a hub, a peer ("kube") it proxies to, and a peer that is down
// ("ghost"), all on the fake herdr. Nothing is mocked: the settings are
// written through the hub to the peer's own settings.json and read back, and
// a launch on the peer follows them.
//
//   node peers.mjs OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
//
// Each check runs at 1280 (two panes) and at 390 (a list, then one host).
import { launch } from "./lib.mjs";
import { releaseView } from "./relfix.mjs";
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";

const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, sock = process.env.FAKE_SOCK;
if (!bin || !sock) { console.error("peers.mjs: set COMEBYE_BIN and FAKE_SOCK"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "peers-ui-"));
const results = [];
const log = [];
const procs = [];

const freePort = () => new Promise((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); }); });
// Agents the peers can start: stand-ins on PATH, so the checks do not depend on what is installed here.
const binDir = path.join(work, "bin");
fs.mkdirSync(binDir);
for (const k of ["pi", "claude", "codex"]) { fs.writeFileSync(path.join(binDir, k), "#!/bin/sh\n"); fs.chmodSync(path.join(binDir, k), 0o755); }
fs.mkdirSync(path.join(work, "code/proj/.git"), { recursive: true });

async function serve(name, port, extra = []) {
  const dir = path.join(work, "state-" + name);
  const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-test-mode", "-push=false", "-state-dir", dir, "-host-name", name,
    "-launch-root", path.join(work, "code"), ...extra], {
    env: { ...process.env, PATH: binDir + ":" + process.env.PATH, COMEBYE_TEST_INSECURE_PEERS: "1" }, stdio: ["ignore", fs.openSync(path.join(out, `peers-${name}.log`), "w"), fs.openSync(path.join(out, `peers-${name}.log`), "a")] });
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
try {
  await serve("kube", kubePort);
  await serve("hub", hubPort, ["-hub-mode"]);
  const j = (m, p, body) => fetch(base + p, { method: m, headers: { "Content-Type": "application/json" }, body: body && JSON.stringify(body) }).then(async (r) => ({ status: r.status, body: await r.json().catch(() => ({})) }));
  for (const [name, url] of [["kube", `http://127.0.0.1:${kubePort}`], ["ghost", "http://127.0.0.1:1"]]) {
    const r = await j("POST", "/api/peers", { name, urls: [url] });
    if (r.status !== 201) throw new Error("add " + name + ": " + JSON.stringify(r));
  }
  for (let i = 0; i < 80; i++) { // wait until the hub has reached kube
    const r = await j("GET", "/api/peers");
    if ((r.body.peers || []).some((p) => p.name === "kube" && p.status === "ok")) break;
    await new Promise((r) => setTimeout(r, 100));
  }

  async function check(name, fn) {
    try { await fn(); results.push([name, true, ""]); }
    catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); }
  }

  for (const width of [1280, 390]) {
    const tag = width >= 1100 ? "desktop" : "phone";
    await j("PUT", "/api/hosts/kube/settings", {}); // each width starts from a host with nothing set
    const { browser: b, page } = await launch({ width, height: width >= 1100 ? 800 : 844, dark });
    browser = b;
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    const shot = (n) => page.screenshot({ path: `${out}/peers-${tag}-${n}.png` }).catch(() => {});
    const noHScroll = async () => {
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector("#peers .pr-view").scrollWidth, document.querySelector("#peers .pr-view").clientWidth]);
      if (w[0] > w[1] || w[2] > w[3]) throw new Error("horizontal overflow " + w.join("/"));
    };
    const card = (n) => page.locator(`.pr-card[data-name="${n}"]`);
    const get = (p) => fetch(base + p).then((r) => r.json());

    await check(`${tag}: the list shows every host with its status and version, within 3 s of opening (not on the 20 s refresh)`, async () => {
      await page.goto(base + "/#/peers", { waitUntil: "load" });
      await page.waitForSelector("#peers", { state: "visible" });
      await card("kube").waitFor({ timeout: 3000 });
      const t = await page.locator("#pr-items").innerText();
      for (const want of ["hub", "(this host)", "kube", "ghost", "Connected", "Down"]) if (!t.includes(want)) throw new Error(`list lacks ${want}: ${t}`);
      await page.waitForFunction(() => document.querySelector("#pr-items").innerText.includes("default: not set"), null, { timeout: 3000 });
      if (width < 1100 && await page.locator("#pr-view").isVisible()) throw new Error("the phone shows a detail before one is picked");
      if (width >= 1100 && !(await page.locator("#pr-view").isVisible())) throw new Error("the desktop has no second pane");
      await shot("list");
    });

    await check(`${tag}: a host's page shows its status, installed agents and form`, async () => {
      await card("kube").click();
      await page.waitForSelector("#pr-form");
      const t = (await page.locator("#pr-view").innerText()).toLowerCase(); // the section headings are drawn in capitals
      for (const want of ["kube", "connected", "installed agents", "pi", "claude", "codex", "default agent", "allowed agents", "model and thinking by agent", "starting agents"]) if (!t.includes(want)) throw new Error(`detail lacks ${want}`);
      if (width < 1100 && await page.locator("#pr-items").isVisible()) throw new Error("the phone still shows the list");
      if ((await page.locator("#pr-save").isEnabled())) throw new Error("Save is enabled with nothing changed");
      await noHScroll();
      await shot("detail");
    });

    await check(`${tag}: editing and saving writes the peer's own settings and survives a reload`, async () => {
      await page.selectOption("#pr-default", "pi");
      for (const k of ["pi", "codex"]) await page.locator(`input[data-allow="${k}"]`).check();
      await page.locator("#pr-m-codex").fill("gpt-5");
      await page.selectOption("#pr-th-pi", "high");
      if (!(await page.locator("#pr-save").isEnabled())) throw new Error("Save stayed disabled after edits");
      if (!(await page.locator("#pr-dirty").innerText()).includes("Unsaved")) throw new Error("no unsaved mark");
      await shot("edited");
      await page.locator("#pr-save").click();
      await page.waitForFunction(() => /^Saved/.test(document.querySelector("#toast")?.textContent || ""), null, { timeout: 5000 });
      // Through the hub and straight from the peer: the same document.
      for (const url of [base + "/api/hosts/kube/settings", `http://127.0.0.1:${kubePort}/api/settings`]) {
        const s = (await fetch(url).then((r) => r.json())).settings;
        if (s.default_kind !== "pi" || s.allowed_kinds.join() !== "pi,codex" || s.kinds.codex.model !== "gpt-5" || s.kinds.pi.thinking !== "high") throw new Error(url + ": " + JSON.stringify(s));
      }
      await page.reload({ waitUntil: "load" });
      await page.waitForSelector("#pr-form");
      if ((await page.locator("#pr-default").inputValue()) !== "pi" || (await page.locator("#pr-m-codex").inputValue()) !== "gpt-5") throw new Error("the form lost the saved values");
      if (await page.locator("#pr-save").isEnabled()) throw new Error("Save enabled right after saving");
      await shot("saved");
    });

    await check(`${tag}: Revert drops unsaved edits`, async () => {
      await page.locator("#pr-m-codex").fill("something-else");
      await page.locator("#pr-revert").click();
      if ((await page.locator("#pr-m-codex").inputValue()) !== "gpt-5") throw new Error("Revert kept the edit");
    });

    await check(`${tag}: a stale revision is reported and the form reloads`, async () => {
      await page.locator("#pr-m-codex").fill("stale-edit");
      await fetch(`http://127.0.0.1:${kubePort}/api/settings`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ default_kind: "pi", allowed_kinds: ["pi", "codex"], kinds: { codex: { model: "gpt-5.1" }, pi: { thinking: "high" } } }) });
      await page.locator("#pr-save").click();
      await page.waitForFunction(() => /Someone else/.test(document.querySelector("#toast")?.textContent || ""), null, { timeout: 5000 });
      await page.waitForFunction(() => document.querySelector("#pr-m-codex")?.value === "gpt-5.1", null, { timeout: 5000 });
      await fetch(base + "/api/hosts/kube/settings", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ default_kind: "pi", allowed_kinds: ["pi", "codex"], kinds: { codex: { model: "gpt-5" }, pi: { thinking: "high" } } }) });
    });

    await check(`${tag}: a host that is down has no form`, async () => {
      await page.goto(base + "/#/peers/ghost", { waitUntil: "load" });
      await page.waitForSelector("#pr-view .pr-bar");
      const t = await page.locator("#pr-view").innerText();
      if (!t.includes("ghost") || !t.includes("not answering")) throw new Error("no explanation: " + t);
      if (await page.locator("#pr-form").count()) throw new Error("a form for a host that is down");
      await shot("down");
    });

    await check(`${tag}: the hub's own settings are editable too, and the back control returns`, async () => {
      await page.goto(base + "/#/peers/hub", { waitUntil: "load" });
      await page.waitForSelector("#pr-form");
      if (!(await page.locator("#pr-view").innerText()).includes("This host")) throw new Error("not marked as this host");
      await page.locator("#pr-back").click();
      if (width < 1100) await page.waitForFunction(() => document.querySelector("#pr-items").offsetParent !== null);
      await page.locator("#pr-back").click().catch(() => {});
    });

    await check(`${tag}: the launch sheet marks the host's default and preselects its model`, async () => {
      await page.goto(base, { waitUntil: "load" });
      await page.waitForSelector("#btn-launch", { state: "visible" });
      await page.locator("#btn-launch").click();
      await page.waitForSelector("#launch", { state: "visible" });
      await page.locator('#ln-hosts [data-host="kube"]').click();
      await page.waitForSelector("#ln-kinds [data-kind]");
      await page.waitForFunction(() => document.querySelector('#ln-kinds [aria-pressed="true"]')?.dataset.kind === "pi", null, { timeout: 5000 });
      const kinds = await page.locator("#ln-kinds").innerText();
      if (!/pi\s*default/.test(kinds) || kinds.includes("claude")) throw new Error("kinds on kube: " + kinds);
      // codex has a host model; pi has a host thinking level
      await page.locator('#ln-kinds [data-kind="codex"]').click();
      await page.waitForFunction(() => document.querySelector("#ln-model-val")?.textContent.includes("Host default (gpt-5)"), null, { timeout: 5000 });
      await shot("launch-codex");
      await page.locator('#ln-kinds [data-kind="pi"]').click();
      await page.waitForFunction(() => document.querySelector("#ln-model-think")?.textContent === "high", null, { timeout: 5000 });
      await page.locator("#ln-close").click();
    });
    await check(`${tag}: the Peers entry of the navigation (and the palette) shows an orange dot while a release needs you, and not otherwise`, async () => {
      let state = "green";
      await page.route(/\/api\/releases(\?.*)?$/, (r) => r.fulfill({ json: releaseView(state) }));
      try {
        await page.goto(base + "/", { waitUntil: "load" });
        const dot = '#nav .nv[data-page="peers"] .bdot-o';
        await page.waitForSelector('#nav .nv[data-page="peers"]', { state: "attached" }); // the phone hides the rail: its way in is the fleet menu
        await page.waitForSelector(dot, { state: "attached", timeout: 12000 });
        if (!/a release needs you: Approve v0\.5\.22/.test(await page.locator(dot).getAttribute("aria-label"))) throw new Error("dot label");
        const c = await page.locator(dot).evaluate((e) => getComputedStyle(e).backgroundColor);
        const o = await page.evaluate(() => { const d = document.createElement("i"); d.style.background = "var(--orange)"; document.body.append(d); const v = getComputedStyle(d).backgroundColor; d.remove(); return v; });
        if (c !== o) throw new Error(`the dot is ${c}, not orange ${o}`);
        await page.evaluate(() => { document.querySelector("#btn-cmds, #kb-open").click(); });
        await page.locator("#pal-q").fill("releases");
        await page.waitForFunction(() => /Releases \(Peers tab\) · needs you/.test(document.querySelector("#palette")?.innerText || ""));
        await page.keyboard.press("Escape");
        await shot("nav-dot");
        // the fleet menu (the phone's way to Peers) carries it too
        await page.locator("#btn-fleet").click();
        await page.waitForSelector("#f-peers .nv-dot:not([hidden])");
        await page.keyboard.press("Escape");
        const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
        if (w[0] > w[1]) throw new Error("horizontal overflow " + w);
        // nothing needs the user: no dot
        state = "done";
        await page.evaluate(() => loadReleases(false));
        await page.waitForFunction(() => !document.querySelector('#nav .nv[data-page="peers"] .bdot-o'));
      } finally { await page.unroute(/\/api\/releases(\?.*)?$/); }
    });
    await page.close();
    await browser.close();
    browser = null;
  }

  await check("through the hub the peer's launch options follow its settings and a disallowed kind is refused", async () => {
    const o = await j("GET", "/api/hosts/kube/launch/options");
    if (o.body.default_kind !== "pi" || o.body.default_source !== "setting" || o.body.default_thinking !== "high" || o.body.kinds.join() !== "pi,codex") throw new Error("options: " + JSON.stringify(o.body));
    // (the fake herdr cannot create workspaces, so a real start is covered by the Go tests)
    const b = await j("POST", "/api/hosts/kube/launch", { dir: path.join(work, "code/proj"), name: "ui-x", kind: "claude" });
    if (b.status !== 400 || b.body.code !== "kind_not_allowed") throw new Error("claude on kube: " + JSON.stringify(b));
  });
} finally {
  if (browser) await browser.close().catch(() => {});
  for (const p of procs) p.kill();
  fs.rmSync(work, { recursive: true, force: true });
}

fs.writeFileSync(`${out}/peers-console.log`, log.join("\n") + "\n");
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"} ${name}${ok ? "" : ": " + why}`); if (!ok) failed++; }
const errs = log.filter((l) => /pageerror|console\.error/.test(l) && !/Failed to load resource/.test(l));
if (errs.length) { console.log("FAIL console errors:\n  " + errs.join("\n  ")); failed++; }
process.exit(failed ? 1 : 0);
