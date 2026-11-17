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
import { PASS, requireThrowawayGnupg, gpgInstalled, throwawayKey, addAuth } from "./gpghelp.mjs";
import { spawn, spawnSync } from "node:child_process";
import fs from "node:fs";

const [rawBase, out] = process.argv.slice(2);
const base = rawBase.replace("127.0.0.1", "localhost");
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR, gnupg = process.env.GNUPGHOME;
if (!bin || !stateDir) { console.error("gpgunlock.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
requireThrowawayGnupg("gpgrenew.mjs", gnupg);
if (!gpgInstalled()) { console.log("gpgrenew: gpg or its preset tool is not installed: skipped"); process.exit(0); }
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const results = [];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let shotPage = null;
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); if (shotPage) await shotPage.screenshot({ path: `${out}/gpgunlock-FAIL-${results.length}.png` }).catch(() => {}); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const HOST = "uitest";
const env = { ...process.env, PATH: process.env.PATH, COMEBYE_URL: rawBase, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w2:p1", GNUPGHOME: gnupg };
const cli = (args, input) => {
  const r = spawnSync(bin, args, { env, input, encoding: "utf8" });
  return { code: r.status, out: (r.stdout || "") + (r.stderr || ""), stdout: r.stdout || "" };
};
const J = { "Content-Type": "application/json" };
const listKeys = async () => (await fetch(base + "/api/passkeys").then((r) => r.json()));
const todo = async (id) => (await fetch(base + "/api/todos/" + id).then((r) => r.json())).todo;

// A throwaway key (ed25519, signing) with a passphrase, in the temp GNUPGHOME (checked above). A full run.sh shares that
// GNUPGHOME with gpgunlock.mjs, which made the same key with the same passphrase: throwawayKey reuses it when it is there.
const FPR = throwawayKey(env);
const status = () => JSON.parse(cli(["gpg", "status", "--json"]).stdout);
const cleanHub = async () => {
  cli(["gpg", "reset", "--yes"]);
  const l = await listKeys();
  for (const k of l.passkeys) await fetch(base + "/api/passkeys/" + k.id, { method: "DELETE", headers: { "X-Comebye-Csrf": l.csrf } });
};


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
const startedAt = Date.now() - 1000; // a full run.sh hub holds the earlier scripts' items too: only look at ours
const approvals = async () => (await lst("?state=all")).filter((t) => Date.parse(t.created_at) >= startedAt).filter((t) => t.kind === "approval" && /GPG is locked/.test(t.question));
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
const startWatcher = () => { watcher = spawn(bin, ["gpg", "watch", "--every", "5s"], { env }); watcher.stdout.on("data", (d) => wlog.push(String(d))); watcher.stderr.on("data", (d) => wlog.push("ERR " + d)); };

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

// Earlier scripts in a full run leave the fixture's agents idle, and the watcher files only for a host with an active agent:
// keep one working for the whole check, by starting a turn on it when it is idle (each turn keeps it working for a couple of seconds).
const ag = (await fetch(base + "/api/agents").then((r) => r.json())).agents[0];
const busy = setInterval(async () => { // a new turn only when it is idle: a prompt on a working agent does not extend it
  try {
    const a = (await fetch(base + "/api/agents").then((r) => r.json())).agents.find((x) => x.pane_id === ag.pane_id);
    if (a && (a.agent_status || a.status) === "idle") { const r = await fetch(`${base}/api/agents/${encodeURIComponent(ag.pane_id)}/prompt`, { method: "POST", headers: J, body: JSON.stringify({ text: "work" }) }); busy.last = r.status; }
  } catch {}
}, 500);
let first, firstApproval, t0;
await check("the watcher (every 5 s) files ONE item, and it is still the same one after 2.5 minutes of rounds", async () => {
  startWatcher();
  for (const end = Date.now() + 40000; Date.now() < end && !first; await sleep(500)) first = (await approvals())[0];
  if (!first) throw new Error("no item filed; watcher said: " + wlog.join("") + " agents: " + JSON.stringify((await fetch(base + "/api/agents").then((r) => r.json())).agents.map((a) => [a.host, a.pane_id, a.agent_status || a.status])) + " status: " + JSON.stringify(status()) + " last prompt status: " + busy.last + " prompt agent: " + ag.pane_id + " " + ag.host);
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
clearInterval(busy);
console.log("watcher output: " + JSON.stringify(wlog.join("")));
await browser.close();
const bad = results.filter((r) => !r.ok).length;
console.log(bad ? `${bad} FAILED` : "all passed");
process.exit(bad ? 1 : 0);
