// The "GPG is locked" item stays (fix/gpg-request-expiry): the live check. The real `comebye gpg watch` (short --every) keeps
// one item across rounds, renewing its request in place; the answer lands after the first request's 2 minutes; "Not now" snoozes it.
// (Header of the script it was copied from:) GPG unlock with a passkey, slice 3 (issue #3): the Set up and Unlock cards in To do, at 390 and 1280px, in the scheme the
// run was started with. Real parts everywhere but the phone: the real comebye CLI on the host side (`comebye gpg setup`,
// `unlock`, `status`) with real gpg in a throwaway GNUPGHOME, the real hub relay, and Chrome's virtual authenticator with PRF
// on for the passkey (so the real navigator.credentials, the real PRF output, the real WebCrypto seal run in the page).
//
//   node gpgunlock.mjs BASE OUT_DIR   (env: COMEBYE_BIN, COMEBYE_STATE_DIR, GNUPGHOME (a temp one: run.sh sets it), SCHEME)
//
// Checked: the card says the unlock is fixed at 8 hours and has no picker; setup shows the host fingerprint the CLI printed;
// the failure messages; and after use no secret is left in the DOM, in storage, or sent to the hub in the clear.
import { launch } from "./lib.mjs";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [rawBase, out] = process.argv.slice(2);
const base = rawBase.replace("127.0.0.1", "localhost");
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR, gnupg = process.env.GNUPGHOME;
if (!bin || !stateDir) { console.error("gpgunlock.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
// Never the user's own keys: only a throwaway GNUPGHOME under the temp directory.
if (!gnupg || !path.resolve(gnupg).startsWith(os.tmpdir() + path.sep) || path.resolve(gnupg) === path.resolve(os.homedir(), ".gnupg")) {
  console.error("gpgunlock.mjs: GNUPGHOME must be a temp directory (run.sh sets one)"); process.exit(2);
}
const have = (b) => spawnSync("sh", ["-c", `command -v ${b}`]).status === 0;
if (!have("gpg") || !have("gpg-connect-agent") || !have("gpgconf") || !(have("gpg-preset-passphrase") || ["/usr/lib/gnupg/gpg-preset-passphrase", "/usr/libexec/gpg-preset-passphrase", "/usr/lib/gnupg2/gpg-preset-passphrase"].some((p) => fs.existsSync(p)))) {
  console.log("gpgunlock: gpg or its preset tool is not installed: skipped"); process.exit(0);
}
fs.mkdirSync(out, { recursive: true });
fs.mkdirSync(gnupg, { recursive: true, mode: 0o700 });
fs.chmodSync(gnupg, 0o700);
const dark = process.env.SCHEME !== "light";
const results = [];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let shotPage = null;
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); if (shotPage) await shotPage.screenshot({ path: `${out}/gpgunlock-FAIL-${results.length}.png` }).catch(() => {}); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const HOST = "uitest";
const PASS = "correct horse battery staple 7a1f";
const env = { ...process.env, PATH: process.env.PATH, COMEBYE_URL: rawBase, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w2:p1", GNUPGHOME: gnupg };
const cli = (args, input) => {
  const r = spawnSync(bin, args, { env, input, encoding: "utf8" });
  return { code: r.status, out: (r.stdout || "") + (r.stderr || ""), stdout: r.stdout || "" };
};
const J = { "Content-Type": "application/json" };
const listKeys = async () => (await fetch(base + "/api/passkeys").then((r) => r.json()));
const todo = async (id) => (await fetch(base + "/api/todos/" + id).then((r) => r.json())).todo;
const addAuth = (cdp, hasPrf) => cdp.send("WebAuthn.addVirtualAuthenticator", { options: {
  protocol: "ctap2", ctap2Version: "ctap2_1", transport: "internal", hasResidentKey: true, hasUserVerification: true, isUserVerified: true,
  automaticPresenceSimulation: true, hasPrf } });

// A throwaway key (ed25519, signing) with a passphrase, in the temp GNUPGHOME.
execFileSync("gpg", ["--batch", "--pinentry-mode", "loopback", "--passphrase", PASS, "--quick-gen-key", "UI Test <ui@example.invalid>", "ed25519", "sign", "never"], { env, stdio: "pipe" });
const FPR = execFileSync("gpg", ["--batch", "--list-secret-keys", "--with-colons"], { env, encoding: "utf8" }).split("\n").map((l) => l.split(":")).find((f) => f[0] === "fpr")[9];
const status = () => JSON.parse(cli(["gpg", "status", "--json"]).stdout);
const cleanHub = async () => {
  cli(["gpg", "reset", "--yes"]);
  const l = await listKeys();
  for (const k of l.passkeys) await fetch(base + "/api/passkeys/" + k.id, { method: "DELETE", headers: { "X-Comebye-Csrf": l.csrf } });
};


import { spawn } from "node:child_process";
const width = 390;
await cleanHub();
const { browser, ctx, page } = await launch({ width, height: 844, dark });
shotPage = page;
page.on("pageerror", (e) => console.log("     pageerror: " + e));
const cdp = await ctx.newCDPSession(page);
await cdp.send("WebAuthn.enable");
await addAuth(cdp, true);
const itemSel = (id) => `#todo .td-item[data-id="${id}"]`;
const lst = async (q = "") => { const l = await fetch(base + "/api/todos" + q).then((r) => r.json()); return l.items || l.todos || []; };
const approvals = async () => (await lst("?state=all")).filter((t) => t.kind === "approval" && /GPG is locked/.test(t.question));
const openTodo = async (id) => {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#todo .td-toggle", { state: "visible", timeout: 15000 });
  if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
  await page.evaluate(() => loadTodos());
  await page.waitForSelector(`${itemSel(id)} .td-row`, { state: "visible", timeout: 15000 });
  if (!(await page.locator(`${itemSel(id)}.open`).count())) await page.locator(`${itemSel(id)} .td-row`).click();
  await page.waitForSelector(`${itemSel(id)}.open .gu-card`, { state: "visible", timeout: 8000 });
  await page.waitForFunction((sel) => !/Reading the request/.test(document.querySelector(sel)?.innerText || "Reading the request"), itemSel(id), { timeout: 8000 });
};
let watcher;
const stopWatcher = () => { if (watcher) { watcher.kill("SIGTERM"); watcher = null; } };
const wlog = [];
const startWatcher = () => { watcher = spawn(bin, ["gpg", "watch", "--every", "10s"], { env }); watcher.stdout.on("data", (d) => wlog.push(String(d))); watcher.stderr.on("data", (d) => wlog.push("ERR " + d)); };

await check("register a passkey and set GPG unlock up (real CLI, real gpg, throwaway GNUPGHOME)", async () => {
  await page.goto(base + "/#/settings", { waitUntil: "load" });
  await page.waitForSelector("#sx-name", { timeout: 8000 });
  await page.fill("#sx-name", "Test phone");
  await page.click("#sx-add");
  await page.waitForSelector(".sx-card[data-fp]", { timeout: 10000 });
  const r = cli(["gpg", "setup", "--key", FPR, "--host", HOST, "--yes"]);
  if (r.code !== 0) throw new Error("setup: " + r.out);
  let it;
  for (const end = Date.now() + 10000; Date.now() < end && !it; await sleep(200)) it = (await lst()).find((t) => /Set up GPG/.test(t.question));
  await openTodo(it.id);
  const sel = itemSel(it.id);
  await page.locator(`${sel} .ap-confirm`).check();
  await page.locator(`${sel} .ap-pass`).fill(PASS);
  await page.locator(`${sel} .ap-go`).click();
  for (const end = Date.now() + 25000; Date.now() < end; await sleep(200)) { const t = (await fetch(base + "/api/todos/" + it.id).then((r) => r.json())).todo; if (t.state === "done") break; }
  if (status().locked) throw new Error("not unlocked after setup");
  const l = cli(["gpg", "lock"]);
  if (!status().locked) throw new Error("lock: " + l.out);
});

let first, firstApproval, t0;
await check("the watcher (every 10 s) files ONE item, and it is still the same one after 2.5 minutes of rounds", async () => {
  startWatcher();
  for (const end = Date.now() + 40000; Date.now() < end && !first; await sleep(500)) first = (await approvals())[0];
  if (!first) throw new Error("no item filed; watcher said: " + wlog.join(""));
  firstApproval = first.approval.id; t0 = Date.now();
  const seenApprovals = new Set([firstApproval]), seenTodos = new Set([first.id]);
  while (Date.now() - t0 < 150000) {
    await sleep(5000);
    const all = await approvals();
    all.forEach((t) => { seenTodos.add(t.id); if (t.approval) seenApprovals.add(t.approval.id); });
    const open = all.filter((t) => t.state === "open");
    if (open.length !== 1 || open[0].id !== first.id) throw new Error(`at ${Math.round((Date.now() - t0) / 1000)} s: ${open.length} open, ids ${all.map((t) => t.id + ":" + t.state)}`);
  }
  console.log(`     after ${Math.round((Date.now() - t0) / 1000)} s: ${seenTodos.size} to-do item(s), ${seenApprovals.size} requests (approval ids: ${[...seenApprovals].join(" ")})`);
  if (seenTodos.size !== 1) throw new Error("more than one item in the history: " + [...seenTodos]);
  if (seenApprovals.size < 2) throw new Error("the request was never renewed");
  const old = (await fetch(base + "/api/approvals/" + firstApproval).then((r) => r.json())).approval;
  console.log("     the first request is now: " + old.state);
  if (old.state === "pending") throw new Error("the first request should have run out by now");
});

await check("Face ID after the 2 minute mark of the first request works, on the same item", async () => {
  await openTodo(first.id);
  const sel = itemSel(first.id);
  const txt = await page.locator(`${sel} .gu-card`).innerText();
  if (!/Unlock with Face ID/.test(txt)) throw new Error("card: " + txt);
  await page.screenshot({ path: `${out}/gpgrenew-card-${width}.png` });
  await page.locator(`${sel} .ap-go`).click();
  for (const end = Date.now() + 25000; Date.now() < end; await sleep(300)) { const t = (await fetch(base + "/api/todos/" + first.id).then((r) => r.json())).todo; if (t.state === "done") break; }
  const t = (await fetch(base + "/api/todos/" + first.id).then((r) => r.json())).todo;
  if (t.state !== "done" || status().locked) throw new Error("item " + t.state + ", status " + JSON.stringify(status()));
  console.log(`     unlocked ${Math.round((Date.now() - t0) / 1000)} s after the first request was filed`);
});

await check("after the unlock the watcher files nothing more (history still one item)", async () => {
  await sleep(25000);
  const all = await approvals();
  if (all.length !== 1) throw new Error("items: " + all.map((t) => t.id + ":" + t.state));
});

await check('"Not now": lock again, the watcher files one item, Not now drops it, and the watcher stays quiet', async () => {
  cli(["gpg", "lock"]);
  let it;
  for (const end = Date.now() + 40000; Date.now() < end && !it; await sleep(500)) it = (await approvals()).find((t) => t.state === "open");
  if (!it) throw new Error("no new item after a relock; watcher: " + wlog.join(""));
  const r = await fetch(base + "/api/todos/" + it.id + "/drop", { method: "POST", body: "{}", headers: { ...J, "X-Comebye-Csrf": (await listKeys()).csrf } });
  if (!r.ok) throw new Error("drop " + r.status);
  await sleep(45000); // four-plus rounds
  const open = (await approvals()).filter((t) => t.state === "open");
  if (open.length) throw new Error("the watcher re-filed straight after Not now: " + open.map((t) => t.id));
  console.log("     quiet 45 s after Not now (snoozed 10 min)");
});
stopWatcher();
console.log("watcher output: " + JSON.stringify(wlog.join("")));
await browser.close();
const bad = results.filter((r) => !r.ok).length;
console.log(bad ? `${bad} FAILED` : "all passed");
process.exit(bad ? 1 : 0);
