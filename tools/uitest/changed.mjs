// One "changed" event for the app (issue #143, slice 1), on real servers: a hub (-hub-mode) and one peer, p1
// (-ledger=false), on the run's fake herdr. Nothing is mocked. The hub sends changed{kind} on /api/events when a
// store is written, and the open app reads that store again at once; the screens no longer poll, and the app's
// one safety read is a minute apart even in test mode. Each check writes on the server, then times how long the
// open app takes to show it:
//   - a to-do added on the hub shows in the To do list within about 1 s;
//   - a page published with the CLI shows in the Pages gallery;
//   - a release proposed shows on the Releases tab, which reads nothing while nothing changes;
//   - a parked agent kept on the PEER shows "kept" in Recently closed (the peer's changed{parked}, relayed by the hub);
//   - after the event stream drops (it is refused while a page is published and a to-do added, so those events are
//     lost) and comes back, the open Pages gallery and the to-dos catch up by themselves, long before any safety read.
//
//   node changed.mjs OUT_DIR       (env: COMEBYE_BIN, FAKE_SOCK; SCHEME=dark|light)
import { launch } from "./lib.mjs";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";

const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, sock = process.env.FAKE_SOCK;
if (!bin || !sock) { console.error("changed.mjs: set COMEBYE_BIN and FAKE_SOCK"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "changed-"));
const freePort = () => new Promise((res) => { const s = net.createServer().listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); }); });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// What "at once" means here: the event, a fetch and a redraw on a loaded CI runner. The polls this replaces were
// 15 s (to-dos), 20 s (pages), 15–30 s (releases) and 30 s (parked).
const AT_ONCE = 1500;
const procs = [];
const results = [];
async function check(name, fn) {
  try { const ms = await fn(); results.push([name, true, ms == null ? "" : `${ms} ms`]); }
  catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); }
}

function start(name, port, extra) {
  const log = fs.openSync(path.join(out, `changed-${name}.log`), "a");
  const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-test-mode", "-push=false", "-state-dir", path.join(work, "state-" + name), "-host-name", name, "-release-simulate", ...extra], {
    env: { ...process.env, COMEBYE_TEST_INSECURE_PEERS: "1", COMEBYE_SCRATCH_DIR: path.join(work, "scratch") }, stdio: ["ignore", log, log] });
  procs.push(p);
}
async function up(port) {
  for (let i = 0; i < 100; i++) { try { if ((await fetch(`http://127.0.0.1:${port}/api/agents`)).ok) return; } catch {} await sleep(100); }
  throw new Error("server on " + port + " did not start");
}

const hubPort = await freePort(), peerPort = await freePort();
const base = `http://127.0.0.1:${hubPort}`;
const j = async (m, p, body, hdr = {}) => { const r = await fetch(base + p, { method: m, headers: { "Content-Type": "application/json", ...hdr }, body: body && JSON.stringify(body) }); return { status: r.status, body: await r.json().catch(() => ({})) }; };
// The peer starts with one parked agent on its books.
const peerState = path.join(work, "state-p1");
fs.mkdirSync(peerState, { recursive: true });
const PK = "pk_0123456789";
fs.writeFileSync(path.join(peerState, "parked.json"), JSON.stringify({ records: [{ id: PK, name: "kept-worker", kind: "pi", host: "p1", session_kind: "path", session: "/tmp/none.jsonl",
  cwd: "/tmp", worktree: "/tmp/kept-worker", branch: "feat/kept", why: "manual", parked_at: new Date(Date.now() - 3600e3).toISOString() }] }));
fs.mkdirSync(path.join(work, "scratch"), { recursive: true });

let browser;
try {
  start("p1", peerPort, ["-ledger=false", "-hub", base]);
  start("hub", hubPort, ["-hub-mode"]);
  await Promise.all([up(hubPort), up(peerPort)]);
  const add = await j("POST", "/api/peers", { name: "p1", urls: [`http://127.0.0.1:${peerPort}`] });
  if (add.status !== 201) throw new Error("add p1: " + JSON.stringify(add));
  for (let i = 0; i < 100; i++) { const r = await j("GET", "/api/peers"); if ((r.body.peers || []).some((p) => p.name === "p1" && p.status === "ok")) break; await sleep(100); }

  let page;
  ({ browser, page } = await launch({ width: 1280, height: 800, dark }));
  page.setDefaultTimeout(10000);
  const errs = [];
  page.on("pageerror", (e) => errs.push(String(e)));
  const reads = []; // every GET the app makes, by path
  page.on("request", (r) => { if (r.method() === "GET") reads.push({ at: Date.now(), path: new URL(r.url()).pathname }); });
  const readsOf = (p, since) => reads.filter((x) => x.path === p && x.at >= since).length;
  await page.goto(base + "/#/todo", { waitUntil: "load" });
  await page.waitForSelector("#p-todo", { state: "visible" });
  await page.waitForFunction(() => state.es && state.es.readyState === 1 && todos.on === true);
  await sleep(1500); // the first reads are in

  // shows: how long after write() the selector appears.
  const shows = async (what, write, selector) => {
    const t0 = Date.now();
    await write();
    const sel = typeof selector === "function" ? selector() : selector;
    await page.waitForSelector(sel, { state: "attached", timeout: 15000 }).catch(() => { throw new Error(`${what}: not shown within 15 s`); });
    const ms = Date.now() - t0;
    if (ms > AT_ONCE) throw new Error(`${what}: shown after ${ms} ms, want within ${AT_ONCE} ms (a poll, not the event)`);
    return ms;
  };

  let todoId = "";
  await check("a to-do added on the hub shows in the open app's To do at once", () => shows("to-do", async () => {
    const r = await j("POST", "/api/todos", { no_wait: true, kind: "decision", question: "Shown at once, with no poll?" });
    if (!r.body.todo) throw new Error("add: " + JSON.stringify(r));
    todoId = r.body.todo.id;
  }, () => `#p-todo .td-item[data-id="${todoId}"]`));

  await page.evaluate(() => { location.hash = "#/pages"; });
  await page.waitForSelector("#pages:not([hidden])");
  await sleep(800);
  let pageId = "";
  await check("a page published with the CLI shows in the open Pages gallery at once", async () => {
    const f = path.join(work, "changed.md");
    fs.writeFileSync(f, "# Changed\n\nPublished while the gallery is open.\n");
    const t0 = Date.now();
    const link = execFileSync(bin, ["publish", f, "--title", "Changed at once"], { env: { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: path.join(work, "state-hub"), HERDR_PANE_ID: "w2:p1" }, encoding: "utf8" });
    pageId = link.match(/pg_[0-9a-f]{10}/)[0];
    await page.waitForSelector(`#pages .pgs-card[data-id="${pageId}"]`, { state: "attached", timeout: 15000 }).catch(() => { throw new Error("not shown within 15 s"); });
    const ms = Date.now() - t0;
    if (ms > AT_ONCE + 1000) throw new Error(`shown after ${ms} ms (the CLI included), want within ${AT_ONCE + 1000} ms`);
    return ms;
  });

  await page.evaluate(() => { location.hash = "#/peers/~releases"; });
  await page.waitForSelector("#pr-releases", { state: "visible" });
  await page.waitForFunction(() => rls.data || rls.err, null, { timeout: 15000 });
  await sleep(800);
  await check("the open Releases tab does not poll: no read of /api/releases in 6 s with nothing changed", async () => {
    const t0 = Date.now();
    await sleep(6000);
    const n = readsOf("/api/releases", t0);
    if (n) throw new Error(`${n} reads of /api/releases in 6 s (the old tab polled every 2-4 s in test mode)`);
  });
  await check("a release proposed on the hub shows on the open Releases tab at once", async () => {
    const t0 = Date.now();
    const r = await j("POST", "/api/todos", { question: "", kind: "release", no_wait: true, release: { version: "v0.9.1", commit: "a3f9c1e5d2b7480cf6e1a9b34d70c8e2f15a6b09" } });
    if (!r.body.todo) throw new Error("propose: " + JSON.stringify(r));
    await page.waitForFunction(() => /v0\.9\.1/.test(document.querySelector("#pr-releases").textContent), null, { timeout: 15000 }).catch(() => { throw new Error("not shown within 15 s"); });
    const ms = Date.now() - t0;
    if (ms > AT_ONCE) throw new Error(`shown after ${ms} ms, want within ${AT_ONCE} ms`);
    return ms;
  });

  await page.evaluate(() => { location.hash = ""; });
  await page.waitForSelector(`.parked-row[data-pk="${PK}"]`, { state: "attached", timeout: 15000 });
  await sleep(500);
  await check("a parked agent kept on the peer shows \"kept\" at once (the peer's changed{parked}, relayed by the hub)", () => shows("kept", async () => {
    const r = await j("POST", "/api/hosts/p1/park/pin", { id: PK, pinned: true });
    if (r.status !== 200) throw new Error("pin: " + JSON.stringify(r));
  }, `.parked-row[data-pk="${PK}"] .role-chip`));

  await page.evaluate(() => { location.hash = "#/pages"; });
  await page.waitForSelector("#pages:not([hidden])");
  await check("after the event stream drops and comes back, the open Pages gallery and the to-dos catch up by itself", async () => {
    // Refuse the stream, drop the open one, and write while the app cannot hear about it.
    await page.route(/\/api\/events$/, (r) => r.abort());
    await page.evaluate(() => state.es.onerror());
    await page.waitForFunction(() => !state.es || state.es.readyState !== 1);
    const r = await j("POST", "/api/todos", { no_wait: true, kind: "decision", question: "Added while the stream was down?" });
    const id = r.body.todo.id;
    const f = path.join(work, "down.md");
    fs.writeFileSync(f, "# Down\n\nPublished while the stream was down.\n");
    const pid = execFileSync(bin, ["publish", f, "--title", "Published while down"], { env: { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: path.join(work, "state-hub"), HERDR_PANE_ID: "w2:p1" }, encoding: "utf8" }).match(/pg_[0-9a-f]{10}/)[0];
    await sleep(1500);
    if (await page.locator(`#pages .pgs-card[data-id="${pid}"]`).count()) throw new Error("shown while the stream was down: something else read it");
    const t0 = Date.now();
    await page.unroute(/\/api\/events$/);
    await page.waitForSelector(`#pages .pgs-card[data-id="${pid}"]`, { state: "attached", timeout: 20000 }).catch(() => { throw new Error("the page was not shown within 20 s of the stream coming back"); });
    await page.waitForFunction((x) => todos.items.some((t) => t.id === x), id, { timeout: 5000 }).catch(() => { throw new Error("the to-do was not read after the stream came back"); });
    const ms = Date.now() - t0; // the reconnect's backoff (1–4 s) and the catch-up; the safety read is 60 s away
    if (ms > 6000) throw new Error(`caught up ${ms} ms after the stream came back, want within 6 s (the reconnect, not a later read)`);
    return ms;
  });

  await check("no page errors", async () => { if (errs.length) throw new Error(errs.join("; ")); });
  await page.screenshot({ path: path.join(out, "changed.png") });
} catch (e) {
  results.push(["setup", false, String(e.message).split("\n")[0]]);
} finally {
  if (browser) await browser.close().catch(() => {});
  for (const p of procs) p.kill();
  await sleep(300);
  fs.rmSync(work, { recursive: true, force: true });
}
let fail = 0;
for (const [name, ok, note] of results) { console.log(`${ok ? "ok  " : "FAIL"} ${name}${note ? " (" + note + ")" : ""}`); if (!ok) fail++; }
process.exit(fail ? 1 : 0);
