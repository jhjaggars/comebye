// Signing an agent in from the Peers screen, against real servers: a hub and a peer ("kube") it proxies to, on the
// fake herdr, with the fake claude (internal/signin/testdata/claude: no account, no network, never a real
// `claude auth login`) first on their PATH and a throw-away HOME each.
//
//   node signin.mjs OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
//
// Click Sign in, read the link, type a wrong code (failed, Try again), try again with GOOD-CODE and expect green chips.
// Each width (1280, then the 390 phone) starts on fresh servers: a host's sign-in state is cached for a minute.
import { clickStable, launch, readInPage } from "./lib.mjs";
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, sock = process.env.FAKE_SOCK;
if (!bin || !sock) { console.error("signin.mjs: set COMEBYE_BIN and FAKE_SOCK"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const fakeClaude = path.join(path.dirname(fileURLToPath(import.meta.url)), "../../internal/signin/testdata/claude");
const work = fs.mkdtempSync(path.join(os.tmpdir(), "signin-ui-"));
const results = [];
const log = [];
let procs = [];

const freePort = () => new Promise((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); }); });
const binDir = path.join(work, "bin");
fs.mkdirSync(binDir);
fs.copyFileSync(fakeClaude, path.join(binDir, "claude"));
fs.chmodSync(path.join(binDir, "claude"), 0o755);
fs.mkdirSync(path.join(work, "code/proj/.git"), { recursive: true });

async function serve(tag, name, port, extra = []) {
  const home = path.join(work, `home-${tag}-${name}`);
  fs.mkdirSync(home, { recursive: true });
  const o = fs.openSync(path.join(out, `signin-${tag}-${name}.log`), "w");
  const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-test-mode", "-push=false", "-state-dir", path.join(work, `state-${tag}-${name}`), "-host-name", name,
    "-launch-root", path.join(work, "code"), ...extra], {
    env: { ...process.env, HOME: home, PATH: binDir + ":" + process.env.PATH, COMEBYE_TEST_INSECURE_PEERS: "1", ANTHROPIC_API_KEY: "", ANTHROPIC_AUTH_TOKEN: "" }, stdio: ["ignore", o, o] });
  procs.push(p);
  for (let i = 0; i < 300; i++) {
    try { if ((await fetch(`http://127.0.0.1:${port}/api/agents`)).ok) return; } catch {}
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(name + " did not start");
}

let browser;
try {
  for (const width of [1280, 390]) {
    const tag = width >= 1100 ? "desktop" : "phone";
    const hubPort = await freePort(), kubePort = await freePort();
    const base = `http://127.0.0.1:${hubPort}`;
    await serve(tag, "kube", kubePort);
    await serve(tag, "hub", hubPort, ["-hub-mode"]);
    const add = await fetch(base + "/api/peers", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name: "kube", urls: [`http://127.0.0.1:${kubePort}`] }) });
    if (add.status !== 201) throw new Error("add kube: " + add.status);
    for (let i = 0; i < 80; i++) {
      const r = await fetch(base + "/api/peers").then((x) => x.json());
      if ((r.peers || []).some((p) => p.name === "kube" && p.status === "ok")) break;
      await new Promise((r) => setTimeout(r, 100));
    }

    async function check(name, fn) {
      try { await fn(); results.push([name, true, ""]); }
      catch (e) {
        // What the page knew when it failed: the route, the host's settings load, the sign-in it holds and kept.
        const seen = await page.evaluate(() => ({ hash: location.hash, cfg: prs.cfg.kube && prs.cfg.kube.state, si: sis.by.kube && { state: sis.by.kube.state, id: !!sis.by.kube.id, url: !!sis.by.kube.url, attaching: !!sis.by.kube.attaching },
          kept: sessionStorage.getItem("comebye.signin"), dialog: !!document.querySelector(".si-dialog") })).catch((x) => ({ err: String(x.message).split("\n")[0] }));
        seen.net = siNet.slice(-6); // the last sign-in answers: which server said what (#139)
        results.push([name, false, String(e.message).split("\n")[0] + " (page: " + JSON.stringify(seen) + ")"]);
        await page.screenshot({ path: `${out}/signin-${tag}-fail-${results.length}.png` }).catch(() => {});
      }
    }
    const { browser: b, page } = await launch({ width, height: width >= 1100 ? 800 : 844, dark });
    browser = b;
    // Every answer to a sign-in call, with its code: a failed check says where a 404 came from (the hub or the host).
    const siNet = [];
    page.on("response", async (r) => {
      const u = new URL(r.url());
      if (!/\/signin(\/|$)/.test(u.pathname)) return;
      const body = await r.text().catch(() => "");
      const line = `${r.request().method()} ${u.pathname} ${r.status()} ${(/"code":"([^"]*)"/.exec(body) || [])[1] || ""}`.trim();
      siNet.push(line); log.push("net: " + line);
    });
    page.on("console", (m) => log.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => log.push(`pageerror: ${e.message}`));
    const shot = (n) => page.screenshot({ path: `${out}/signin-${tag}-${n}.png` }).catch(() => {});
    const noHScroll = async () => {
      const w = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth, document.querySelector("#peers .pr-view").scrollWidth, document.querySelector("#peers .pr-view").clientWidth]);
      if (w[0] > w[1] || w[2] > w[3]) throw new Error("horizontal overflow " + w.join("/"));
    };
    const chip = () => page.locator('.pr-agent[data-agent="claude"] .chip');
    const chipText = async () => ((await chip().count()) ? (await chip().innerText()).trim() : "");
    // A colour may still be fading in (the app's transitions): wait until the element has settled on the token's colour.
    const hasTone = (sel, token, what) => page.waitForFunction(([q, t]) => {
      const p = document.createElement("i"); p.style.color = `var(${t})`; document.body.append(p);
      const want = getComputedStyle(p).color; p.remove();
      const e = document.querySelector(q); return !!e && getComputedStyle(e).color === want;
    }, [sel, token], { timeout: 4000 }).catch(() => { throw new Error(`${what} is not ${token}`); });
    const link = async () => { await page.waitForSelector("#si-url"); return (await readInPage(page, "#si-url"))?.text ?? ""; };

    await check(`${tag}: kube's row has an orange dot while its claude is signed out, and the host page says Signed out with a Sign in button`, async () => {
      await page.goto(base + "/#/peers", { waitUntil: "load" });
      await page.waitForSelector('.pr-card[data-name="kube"]', { timeout: 5000 });
      await page.waitForSelector('.pr-card[data-name="kube"] .pr-need', { timeout: 5000 });
      await shot("list-dot");
      await clickStable(page, '.pr-card[data-name="kube"]');
      await page.waitForSelector('.pr-agent[data-agent="claude"]');
      await page.waitForFunction(() => /Signed out/.test(document.querySelector('.pr-agent[data-agent="claude"] .chip')?.textContent || ""), null, { timeout: 5000 });
      if (!(await page.locator('.pr-agent[data-agent="claude"] [data-signin="claude"]').isVisible())) throw new Error("no Sign in button");
      await noHScroll();
      await shot("agents-signed-out");
    });

    await check(`${tag}: Sign in opens a dialog that names the host, shows the link as text with Copy, and a code box with autofill off`, async () => {
      await clickStable(page, '[data-signin="claude"]');
      const url = await link();
      if (!/^https:\/\/claude\.com\/cai\/oauth\/authorize\?/.test(url)) throw new Error("link: " + url);
      const title = await page.locator("#si-title").innerText();
      if (!/claude/.test(title) || !/kube/.test(title)) throw new Error("title: " + title);
      if ((await page.locator("#si-copy").innerText()).trim() !== "Copy") throw new Error("no Copy");
      const at = await page.locator("#si-code").evaluate((e) => [e.autocomplete, e.getAttribute("autocapitalize"), e.spellcheck, e.getAttribute("inputmode")]);
      if (at.join() !== "off,off,false,text") throw new Error("code box attributes " + at);
      if (!(await page.locator('[data-si="cancel"]').isVisible())) throw new Error("no Cancel while waiting");
      if (!/Waiting/.test(await chipText())) throw new Error("row chip: " + await chipText());
      await hasTone("#si-chip", "--orange", "the waiting chip");
      if (width < 600) { // phone: the code box sits under the link, the buttons are full width and 44 px
        const [u, c] = await Promise.all([page.locator("#si-url").boundingBox(), page.locator("#si-code").boundingBox()]);
        if (c.y < u.y + u.height) throw new Error("the code box is not under the link");
        for (const sel of ['[data-si="send"]', '[data-si="cancel"]', "#si-copy"]) {
          const bx = await page.locator(sel).boundingBox();
          if (bx.height < 43.5) throw new Error(sel + " is " + bx.height + " px tall");
          if (bx.width < 300) throw new Error(sel + " is " + bx.width + " px wide");
        }
      }
      await noHScroll();
      await shot("waiting");
    });

    await check(`${tag}: leaving and coming back (and a reload) re-attaches to the running sign-in`, async () => {
      const before = await link();
      await page.evaluate(() => { location.hash = "#/peers"; });
      await page.waitForFunction(() => location.hash === "#/peers");
      await clickStable(page, '.pr-card[data-name="kube"]');
      if ((await link()) !== before) throw new Error("another link after coming back");
      await page.reload({ waitUntil: "load" });
      await page.waitForSelector("#si-url", { timeout: 8000 });
      if ((await link()) !== before) throw new Error("another link after a reload");
    });

    await check(`${tag}: a wrong code shows Failed in red with Try again`, async () => {
      await page.locator("#si-code").fill("WRONG-CODE");
      await clickStable(page, '[data-si="send"]');
      await page.waitForFunction(() => document.querySelector("#si-chip")?.textContent.trim() === "Failed", null, { timeout: 15000 });
      if (!(await page.locator('[data-si="retry"]').isVisible())) throw new Error("no Try again");
      if (!/not accepted/.test(await page.locator(".si-dialog").innerText())) throw new Error("no reason: " + await page.locator(".si-dialog").innerText());
      await hasTone("#si-chip", "--red", "the failed chip");
      await noHScroll();
      await shot("failed");
    });

    await check(`${tag}: Try again, then GOOD-CODE: the chip turns green, the dot goes, and the button is gone`, async () => {
      await clickStable(page, '[data-si="retry"]');
      await link();
      await page.locator("#si-code").fill("GOOD-CODE");
      await clickStable(page, '[data-si="send"]');
      await page.waitForFunction(() => document.querySelector("#si-chip")?.textContent.trim().startsWith("Signed in"), null, { timeout: 15000 });
      await hasTone("#si-chip", "--green", "the dialog chip");
      await page.waitForFunction(() => /Signed in · claude\.ai/.test(document.querySelector('.pr-agent[data-agent="claude"] .chip')?.textContent || ""), null, { timeout: 8000 });
      await hasTone('.pr-agent[data-agent="claude"] .chip', "--green", "the row chip");
      if (await page.locator('.pr-agent[data-agent="claude"] [data-signin]').count()) throw new Error("Sign in is still offered");
      await shot("signed-in");
      await clickStable(page, '[data-si="close"]');
      if (await page.locator(".si-dialog").count()) throw new Error("the dialog stayed after Done");
      const st = await fetch(base + "/api/hosts/kube/settings").then((r) => r.json());
      if (!st.agent_auth || st.agent_auth.claude.state !== "signed_in" || st.agent_auth.claude.method !== "claude.ai") throw new Error("agent_auth: " + JSON.stringify(st.agent_auth));
      if (JSON.stringify(st.agent_auth).includes("@")) throw new Error("an account name in agent_auth");
      await page.goto(base + "/#/peers", { waitUntil: "load" });
      await page.waitForSelector('.pr-card[data-name="kube"]');
      await page.waitForFunction(() => /connected/i.test(document.querySelector('.pr-card[data-name="kube"]')?.textContent || ""));
      await new Promise((r) => setTimeout(r, 400));
      if (await page.locator('.pr-card[data-name="kube"] .pr-need').count()) throw new Error("the orange dot stayed");
      await shot("list-clear");
    });

    await page.close();
    await browser.close();
    browser = null;
    for (const p of procs) p.kill();
    procs = [];
  }
} finally {
  if (browser) await browser.close().catch(() => {});
  for (const p of procs) p.kill();
  fs.rmSync(work, { recursive: true, force: true });
}

fs.writeFileSync(`${out}/signin-console.log`, log.join("\n") + "\n");
let failed = 0;
for (const [name, ok, why] of results) { console.log(`${ok ? "PASS" : "FAIL"} ${name}${ok ? "" : ": " + why}`); if (!ok) failed++; }
const errs = log.filter((l) => /pageerror|console\.error/.test(l) && !/Failed to load resource/.test(l));
if (errs.length) { console.log("FAIL console errors:\n  " + errs.join("\n  ")); failed++; }
process.exit(failed ? 1 : 0);
