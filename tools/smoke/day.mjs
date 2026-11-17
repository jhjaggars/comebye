// The day-in-the-life smoke driver (see README.md). run.sh starts a throwaway hub, a stand-in herdr and a bare origin,
// then runs this against them:
//
//   node day.mjs BASE OUT_DIR       (env: SMOKE_TMP, COMEBYE_BIN, COMEBYE_URL, PAGES_URL, GNUPGHOME, CHROME)
//
// Real: the hub, the comebye CLI (as each agent, with its pane id and the local agent token), git (real worktrees, a real
// bare origin), gpg, and headless Chrome at phone width with a virtual authenticator. Stand-ins: herdr (tools/smoke/fakeherdr)
// and the agents (this script plays the coordinator and the workers, reading what the hub typed into their panes).
import { launch } from "../uitest/lib.mjs";
import { PASS, requireThrowawayGnupg, gpgInstalled, throwawayKey, addAuth } from "../uitest/gpghelp.mjs";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import path from "node:path";

const [rawBase, out] = process.argv.slice(2);
const tmp = process.env.SMOKE_TMP, bin = process.env.COMEBYE_BIN, gnupg = process.env.GNUPGHOME;
if (!rawBase || !out || !tmp || !bin) { console.error("day.mjs: run it through tools/smoke/run.sh"); process.exit(2); }
requireThrowawayGnupg("day.mjs", gnupg);
fs.mkdirSync(out, { recursive: true });
const base = rawBase.replace("127.0.0.1", "localhost"); // WebAuthn needs a secure context: localhost is one
const repo = `${tmp}/code/proj`, origin = `${tmp}/origin.git`;
const J = { "Content-Type": "application/json" };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// dumpState writes what is needed to see why a check failed (agents, the ledger, to-dos and what each stand-in was typed) to
// OUT/state-<tag>.txt. It runs only after a failure and never throws.
async function dumpState(tag) {
  try {
    const lines = [];
    const agents = await get("/api/agents").then((r) => r.agents).catch((e) => String(e));
    lines.push("agents: " + JSON.stringify(agents, null, 1));
    lines.push("ledger: " + JSON.stringify(await get("/api/ledger").catch((e) => String(e)), null, 1));
    lines.push("todos: " + JSON.stringify(await get("/api/todos?state=all").catch((e) => String(e)), null, 1));
    for (const a of Array.isArray(agents) ? agents : []) {
      const r = await rpc("smoke.prompts", { target: a.name }).catch(() => null);
      if (r) lines.push(`--- prompts typed to ${a.name} (${a.pane_id}):\n` + r.prompts.map((p, i) => `[${i}] ${p.slice(0, 400)}`).join("\n"));
    }
    fs.writeFileSync(`${out}/state-${tag}.txt`, lines.join("\n\n") + "\n");
  } catch { /* a failed dump must never fail the run */ }
}

// ---- checks -----------------------------------------------------------------------------------------------------
const results = [];
let shotPage = null;
async function check(name, fn) {
  const t0 = Date.now();
  try { await fn(); results.push({ name, ok: true }); console.log(`ok   ${name} (${((Date.now() - t0) / 1000).toFixed(1)}s)`); return true; }
  catch (e) {
    results.push({ name, ok: false });
    if (shotPage) await shotPage.screenshot({ path: `${out}/FAIL-${results.length}.png` }).catch(() => {});
    await dumpState(`FAIL-${results.length}`);
    console.log("FAIL " + name + "\n     " + String(e.stack && !/^Error: /.test(String(e)) ? e.stack : e.message).split("\n").join("\n     "));
    return false;
  }
}
// until polls a condition (a wait, not a retry: nothing is run again to get a different answer).
async function until(fn, what, ms = 20000) {
  for (const end = Date.now() + ms; Date.now() < end; await sleep(150)) { const v = await fn(); if (v) return v; }
  throw new Error("timed out waiting for " + what);
}
const must = (cond, msg) => { if (!cond) throw new Error(msg); };

// ---- the stand-in herdr (its smoke.* methods) and the hub ------------------------------------------------------
function rpc(method, params = {}) {
  return new Promise((resolve, reject) => {
    const c = net.createConnection(`${tmp}/h.sock`);
    let buf = "";
    c.on("connect", () => c.write(JSON.stringify({ id: "1", method, params }) + "\n"));
    c.on("data", (d) => { buf += d; if (buf.includes("\n")) { c.end(); const m = JSON.parse(buf); m.error ? reject(new Error(`${method}: ${m.error.code} ${m.error.message}`)) : resolve(m.result); } });
    c.on("error", reject);
  });
}
const get = async (p) => (await fetch(rawBase + p)).json();
const post = async (p, body) => { const r = await fetch(rawBase + p, { method: "POST", headers: J, body: JSON.stringify(body || {}) }); return { status: r.status, body: await r.json().catch(() => ({})) }; };
const agentsNow = async () => (await get("/api/agents")).agents;
const agentNamed = async (name) => (await agentsNow()).find((a) => a.name === name);
const statusOf = async (name) => { const a = await agentNamed(name); return a && (a.agent_status || a.status); };
const todoList = async (state = "all") => (await get("/api/todos?state=" + state)).todos;
const todoById = async (id) => (await get("/api/todos/" + id)).todo;

// cli runs the real comebye CLI as the agent in `pane` (what an agent's shell does), or as the user (pane null, with a login).
function cli(pane, args, input) {
  const env = { PATH: process.env.PATH, HOME: process.env.HOME, GNUPGHOME: gnupg, COMEBYE_URL: rawBase, COMEBYE_STATE_DIR: `${tmp}/state`, COMEBYE_LOGIN: "me@example.invalid" };
  if (pane) env.HERDR_PANE_ID = pane;
  const r = spawnSync(bin, args, { env, input, encoding: "utf8", timeout: 120000 });
  return { code: r.status, out: (r.stdout || "") + (r.stderr || ""), stdout: r.stdout || "" };
}
const ok = (r, what) => { if (r.code !== 0) throw new Error(`${what}: exit ${r.code}: ${r.out.trim()}`); return r.out; };
const git = (dir, ...args) => { const r = spawnSync("git", ["-C", dir, ...args], { encoding: "utf8" }); if (r.status !== 0) throw new Error(`git ${args.join(" ")}: ${r.stderr}`); return r.stdout.trim(); };

// A stand-in agent: its pane is found by name (a resume gives it a new one).
class Agent {
  constructor(name) { this.name = name; }
  async pane() { return (await rpc("agent.get", { target: this.name })).agent.pane_id; }
  async prompts() { return (await rpc("smoke.prompts", { target: this.name })).prompts; }
  async argv() { return (await rpc("pane.process_info", { pane_id: await this.pane() })).process_info.foreground_processes[0].argv; }
  // The turn ends: what a real agent's status does when it stops (herdr reports idle).
  async idle() { await rpc("smoke.status", { target: this.name, status: "idle" }); }
  async working() { await rpc("smoke.status", { target: this.name, status: "working" }); }
  async run(args, input) { return cli(await this.pane(), args, input); }
  async gotPrompt(re, what, ms) { return until(async () => (await this.prompts()).find((p) => re.test(p)), `${this.name} to be typed ${what}`, ms); }
}
const REPORT = (summary, changes) => `## Summary\n${summary}\n\n## Changes\n${changes}\n\n## Verification\nVerified live: the stand-in worker ran the steps by hand.\nUnit tests only: none\nNot verified: none\n\n## Follow-ups\nNone.\n\n## Decisions for the user\nNone.\n`;

const coord = new Agent("coord");
const worker1 = new Agent("one"), worker2 = new Agent("two");
const WT = (b) => `${tmp}/worktrees/proj/${b}`;

// ---- the browser: a phone, the real Content-Security-Policy ---------------------------------------------------
const { browser, ctx, page } = await launch({ width: 390, height: 844, dark: process.env.SCHEME !== "light", bypassCSP: false });
shotPage = page;
const pageErrors = [], consoleErrors = [];
page.on("pageerror", (e) => pageErrors.push(String(e)));
const failedLoads = [];
page.on("response", (r) => { if (r.status() >= 400) failedLoads.push(`${r.status()} ${r.request().method()} ${r.url().replace(/^https?:\/\/[^/]+/, "")}`); });
page.on("console", (m) => { if (m.type() === "error") consoleErrors.push(m.text()); });
const shot = (name) => page.screenshot({ path: `${out}/${name}.png` });
const noScroll = async (what) => { const g = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: innerWidth })); must(g.sw <= g.iw, `${what}: the page scrolls sideways (${g.sw} > ${g.iw})`); };
const phoneTodo = async () => {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForSelector("#todo .td-toggle", { state: "visible", timeout: 15000 });
  if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
  await page.evaluate(() => loadTodos());
};
const openItem = async (id) => {
  await phoneTodo();
  const sel = `#todo .td-item[data-id="${id}"]`;
  await page.waitForSelector(`${sel} .td-row`, { state: "visible", timeout: 15000 });
  if (!(await page.locator(`${sel}.open`).count())) await page.locator(`${sel} .td-row`).click();
  return sel;
};

// =====================================================================================================================
// Setup: a coordinator is running, and the user made it the coordinator (the app's "Make coordinator").
await check("setup: a coordinator agent runs and the hub knows it as the coordinator", async () => {
  await rpc("smoke.add_agent", { name: "coord", kind: "claude", cwd: repo });
  await until(() => agentNamed("coord"), "the hub to see the coordinator");
  const id = (await agentNamed("coord")).id;
  const r = await post("/api/roles", { agent_id: id, role: "coordinator" });
  must(r.status === 200 && r.body.roles?.[id]?.role === "coordinator", "role: " + JSON.stringify(r));
  await coord.idle();
});

// ---- 1. The user starts a worker; it reports; the coordinator is told ------------------------------------------------
let h1;
await check("1. the user's ask becomes a handoff: the worker starts on its own branch in a real git worktree and is told its task", async () => {
  const r = cli(await coord.pane(), ["handoff", "--host", "smoke", "--repo", repo, "--branch", "feat/hello", "--name", "one", "--kind", "claude", "--brief", "-"],
    "Intent: add hello.txt saying hi. Report when it is done.\n");
  const o = ok(r, "handoff");
  must(/Handed off feat\/hello/.test(o) && !/warning:/.test(o), "handoff said: " + o);
  h1 = o.match(/entry:\s+(h[0-9a-f]+)/)[1];
  must(git(WT("feat-hello"), "rev-parse", "--abbrev-ref", "HEAD") === "feat/hello", "the worktree is not on feat/hello");
  must(git(repo, "worktree", "list").includes("feat-hello"), "git does not list the worktree");
  const p = await worker1.gotPrompt(/handed this task by coord/, "its brief");
  must(/add hello\.txt/.test(p), "the brief lost the user's ask: " + p.slice(0, 300));
  await until(async () => (await statusOf("one")) === "working", "the worker to start its turn on the brief");
});

await check("1. the worker commits and reports; the hub marks it finished and types the finished notice into the coordinator's pane", async () => {
  fs.writeFileSync(`${WT("feat-hello")}/hello.txt`, "hi\n");
  git(WT("feat-hello"), "add", "hello.txt"); git(WT("feat-hello"), "commit", "-qm", "add hello.txt");
  const o = ok(await worker1.run(["report", "-"], REPORT("Added hello.txt.", "- hello.txt")), "report");
  must(!/warning:/.test(o), "the report was not in the handoff's format: " + o);
  await worker1.idle();
  await until(async () => (await get("/api/ledger")).entries?.find((e) => e.id === h1 && e.status === "finished") || (await get("/api/ledger")).ledger?.find?.((e) => e.id === h1 && e.status === "finished"), "the ledger entry to be finished").catch(async (e) => { throw new Error(e.message + ": " + JSON.stringify(await get("/api/ledger")).slice(0, 400)); });
  const n = await coord.gotPrompt(/\(hub notice\).*feat\/hello.*finished/s, "the finished notice");
  must(n.includes(h1), "the notice does not name the handoff " + h1);
  await coord.idle(); // the coordinator's turn on the notice ends
  const rd = ok(cli(await coord.pane(), ["read", h1, "--report"]), "read --report");
  must(/Added hello\.txt/.test(rd), "the coordinator cannot read the report: " + rd.slice(0, 200));
});

// ---- 2. A worker files a decision; the user answers it on the phone; the answer reaches the worker; the item closes ----
let t2;
await check("2. a second worker, mid-task, files a decision to-do (with buttons) and it shows on the phone", async () => {
  ok(cli(await coord.pane(), ["handoff", "--host", "smoke", "--repo", repo, "--branch", "feat/two", "--name", "two", "--kind", "claude", "--brief", "-"], "Intent: add a second file. Ask the user which greeting to use.\n"), "handoff two");
  await worker2.gotPrompt(/handed this task by coord/, "its brief");
  await until(() => agentNamed("two"), "the hub to list the second worker"); // a real worker takes longer than this to ask
  const o = ok(await worker2.run(["todo", "add", "Which greeting should the second file use?", "--kind", "decision", "--waiting", "two",
    "--option", "hi|keep it short", "--option", "hello|say the whole word", "--recommend", "1", "--context", "-"], "The first file says hi. The user picks for the second."), "todo add");
  t2 = o.match(/Added (t[0-9a-f]{10})/)[1];
  const sel = await openItem(t2);
  await page.waitForSelector(`${sel}.open .td-opt[data-n="1"]`, { state: "visible", timeout: 8000 });
  await noScroll("to do card");
  await shot("2-todo-card-390");
});
await check("2. the user taps a choice in the app; the hub types the answer, labelled as the user's, into the worker; the item closes", async () => {
  await page.evaluate(() => { CHOICE_UNDO_MS = 300; });
  await page.locator(`#todo .td-item[data-id="${t2}"] .td-opt[data-n="1"]`).click();
  const it = await until(async () => { const t = await todoById(t2); return t.state === "done" && t; }, "the item to close");
  must(it.answer.text === "ok 1" && it.answer.via === "app" && it.closed_by === "hub", "item: " + JSON.stringify(it.answer) + " closed_by " + it.closed_by);
  const p = await worker2.gotPrompt(/\(hub relay\)/, "the relay");
  must(/Their words: "ok 1"/.test(p) && /user's own answer/.test(p), "the relay is not labelled as the user's own words: " + p);
  must(!(await get("/api/todos?state=open")).todos.some((t) => t.id === t2), "the item is still open");
  await shot("2-todo-closed-390");
  await worker2.idle();
});

// ---- 3. Landing -------------------------------------------------------------------------------------------------------
let t3;
await check("3. the coordinator asks the user to OK the landing; the user answers in the app; the coordinator is told", async () => {
  const o = ok(cli(await coord.pane(), ["todo", "add", "Land feat/hello on main?", "--kind", "decision", "--waiting", "coord",
    "--option", "Land it|push the work to main", "--option", "Not yet|leave it as it is", "--recommend", "1", "--context", "-"],
    "one added hello.txt (saying hi). Landing pushes it to main."), "todo add");
  t3 = o.match(/Added (t[0-9a-f]{10})/)[1];
  const sel = await openItem(t3);
  await page.waitForSelector(`${sel}.open .td-opt[data-n="1"]`, { state: "visible", timeout: 8000 });
  await shot("3-land-ok-390");
  await page.locator(`${sel} .td-opt[data-n="1"]`).click();
  await until(async () => (await todoById(t3)).state === "answered", "the answer to be recorded");
  const n = await coord.gotPrompt(/the user answered to-do item\(s\) /, "the answer notice");
  must(n.includes(t3), "the notice does not name " + t3);
});
await check("3. the coordinator relays the OK with `comebye land`; the worker is told, labelled as an agent's, to push to main", async () => {
  ok(cli(await coord.pane(), ["todo", "done", t3]), "todo done");
  ok(cli(await coord.pane(), ["land", "one", "--note", 'the user tapped "Land it" (ok 1) in the app']), "land");
  await coord.idle();
  const p = await worker1.gotPrompt(/the user has OKed landing/, "the land prompt");
  must(/From agent coord .*not the user and not the hub/.test(p), "the land prompt does not say who it is from: " + p.slice(0, 200));
  must(/git push origin HEAD:main/.test(p), "the land prompt does not say how to push");
  must((await agentNamed("one")).id, "agent missing");
});
await check("3. the worker lands to the bare origin; the hub sees it and the board shows it landed", async () => {
  await worker1.working();
  const wt = WT("feat-hello");
  git(wt, "fetch", "-q", "origin"); git(wt, "rebase", "-q", "origin/main");
  must(git(wt, "log", "--merges", "origin/main..HEAD") === "", "merge commits");
  git(wt, "push", "-q", "origin", "HEAD:main");
  const tip = git(wt, "rev-parse", "HEAD");
  must(git(origin, "rev-parse", "main") === tip, "the bare origin's main is not the worker's tip");
  must(git(origin, "show", "main:hello.txt") === "hi", "origin has no hello.txt");
  ok(await worker1.run(["report", "-"], REPORT("Landed hello.txt on main.", `- pushed ${tip.slice(0, 7)} to origin main`)), "landing report");
  await worker1.idle();
  const b = await until(async () => { const r = cli(await coord.pane(), ["board"]); return /landed, recent \(1\)\n- one \[landed\]/.test(r.stdout) && r.stdout; }, "the board to show one landed");
  must(!/ready to land, or landing \(\d+\)\n(- .*\n)*- one /.test(b), "board still lists it as ready to land:\n" + b);
  await page.goto(base + "/", { waitUntil: "load" });
  const row = await until(async () => { const t = await page.locator("#agents").innerText().catch(() => ""); return /landed/i.test(t) && t; }, "the app's agent list to say landed").catch(() => null);
  await shot("3-landed-390");
  must(row, "the app's agent list never said landed");
});

// ---- 4. A Page, and "Chat about this" ---------------------------------------------------------------------------------
let pageId;
await check("4. the worker publishes a Page; the user opens it in the viewer on the phone and it renders in the frame", async () => {
  const f = `${tmp}/report-page.html`;
  fs.writeFileSync(f, `<!doctype html><html><head><meta charset="utf-8"><title>Hello report</title><meta name="viewport" content="width=device-width,initial-scale=1"></head><body style="font:16px/1.5 sans-serif;margin:16px"><h1>Hello report</h1><p id="p">hello.txt says hi, and it is on main.</p></body></html>`);
  const o = ok(await worker1.run(["publish", f, "--key", "smoke-day-report"]), "publish");
  pageId = o.match(/pg_[0-9a-f]{10}/)[0];
  await page.goto(base + "/#/pages/" + pageId, { waitUntil: "load" });
  await page.waitForSelector("iframe.pgs-frame", { state: "attached", timeout: 10000 });
  const fr = await until(() => page.frames().find((x) => x.url().includes("/p/" + pageId)), "the page frame");
  await fr.waitForSelector("#p", { timeout: 10000 });
  must(/hello\.txt says hi/.test(await fr.locator("#p").innerText()), "the page text is not in the frame");
  await noScroll("page viewer");
  await shot("4-page-390");
});
await check("4. the user taps \"Chat about this\"; the hub starts a read-only claude helper and opens its conversation", async () => {
  await page.waitForSelector("#pgs-chat", { timeout: 8000 });
  const before = new Set((await agentsNow()).map((a) => a.id));
  const resp = page.waitForResponse((r) => /\/api\/pages\/[^/]+\/chat$/.test(r.url()), { timeout: 20000 });
  await page.click("#pgs-chat");
  const r = await resp;
  must(r.status() === 200, "POST chat answered " + r.status() + ": " + (await r.text()).slice(0, 300));
  const body = await r.json();
  const name = body.name || body.agent?.name;
  must(name, "no helper in the answer: " + JSON.stringify(body));
  const helper = new Agent(name);
  const a = await until(async () => (await agentsNow()).find((x) => x.name === name && !before.has(x.id)), "the helper to show in the agent list");
  must(a.agent === "claude" || a.kind === "claude", "the helper is not a claude: " + JSON.stringify(a));
  await helper.gotPrompt(new RegExp(pageId), "the page it is to read", 15000);
  const argv = (await helper.argv()).join(" ");
  must(/--permission-mode plan/.test(argv) && /--disallowedTools ExitPlanMode/.test(argv), "the helper is not read-only: " + argv);
  const hash = "#/agent/" + encodeURIComponent(await helper.pane());
  await until(async () => (await page.evaluate(() => location.hash)) === hash, `the app to open the helper's conversation (${hash})`, 8000);
  await shot("4-chat-390");
});

// ---- 5. Park and resume a worker; restart an idle one ---------------------------------------------------------------------
await check("5. the user parks the second worker (idle, answered): its pane closes, its worktree, branch and session stay", async () => {
  await until(async () => (await statusOf("two")) === "idle", "two to be idle");
  const pane = await worker2.pane();
  const o = ok(cli(await coord.pane(), ["park", "two"]), "park");
  const pk = o.match(/pk_[0-9a-f]+/)[0];
  await until(async () => !(await agentNamed("two")), "the parked agent to leave the agent list");
  await rpc("agent.get", { target: pane }).then(() => { throw new Error("its pane is still open"); }, () => {});
  must(fs.existsSync(WT("feat-two")) && git(WT("feat-two"), "rev-parse", "--abbrev-ref", "HEAD") === "feat/two", "the worktree or branch is gone");
  const l = ok(cli(await coord.pane(), ["parked"]), "parked");
  must(l.includes(pk) && /feat\/two/.test(l), "parked lists: " + l);
  check.pk = pk;
});
await check("5. the user resumes it: a new pane on the same session, in the same worktree, and it is told what happened", async () => {
  const o = ok(cli(await coord.pane(), ["resume", check.pk, "--host", "smoke"]), "resume");
  must(/Resumed two as smoke\/w\d+:p1/.test(o), "resume said: " + o);
  const a = await until(() => agentNamed("two"), "the agent to be back");
  must(a.cwd === WT("feat-two") || (a.worktree?.checkout || "").endsWith("feat-two") || (a.cwd || "").endsWith("feat-two"), "not in the same worktree: " + a.cwd);
  const argv = await worker2.argv();
  must(argv.includes("--resume") && /^5a0c0de0-/.test(argv[argv.indexOf("--resume") + 1]), "not on its session: " + argv.join(" "));
  await worker2.gotPrompt(/you were parked .* and resumed just now/s, "the catch-up note");
  await worker2.idle();
});
await check("5. the user restarts the idle worker: same pane, same name, same session and flags, and it did not lose its worktree", async () => {
  await until(async () => (await statusOf("two")) === "idle", "two to be idle");
  const pane = await worker2.pane(), before = await worker2.argv();
  const o = ok(cli(await coord.pane(), ["restart", "two"]), "restart");
  const a = await until(() => agentNamed("two"), "the agent to be back");
  must(a.pane_id === pane, `it moved from ${pane} to ${a.pane_id}: ${o}`);
  const after = await worker2.argv();
  must(JSON.stringify(after) === JSON.stringify(before), `flags changed:\n  ${before.join(" ")}\n  ${after.join(" ")}`);
  must(fs.existsSync(WT("feat-two")), "the worktree is gone");
});

// ---- 6. GPG unlock with Face ID ---------------------------------------------------------------------------------------------
if (!gpgInstalled()) {
  // A machine without gpg skips step 6 and says so; CI sets SMOKE_REQUIRE_GPG, so a runner image that loses gpg is a failure, not a quiet skip.
  await check("6. gpg and its preset tool are installed (SMOKE_REQUIRE_GPG)", async () => {
    must(!process.env.SMOKE_REQUIRE_GPG, "gpg, gpg-connect-agent, gpgconf or gpg-preset-passphrase is missing");
    console.log("     skipped step 6: gpg is not installed on this machine");
  });
} else {
  const env = { ...process.env, GNUPGHOME: gnupg };
  const FPR = throwawayKey(env);
  const coordPane = await coord.pane();
  const cdp = await ctx.newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  await addAuth(cdp, true);
  let item;
  await check("6. a passkey is registered (virtual authenticator with PRF) and `comebye gpg setup` files the set-up item", async () => {
    await page.goto(base + "/#/settings", { waitUntil: "load" });
    await page.waitForSelector("#sx-name", { timeout: 8000 });
    await page.fill("#sx-name", "Smoke phone");
    await page.click("#sx-add");
    await page.waitForSelector(".sx-card[data-fp]", { timeout: 10000 });
    const r = cli(coordPane, ["gpg", "setup", "--key", FPR, "--host", "smoke", "--yes"]);
    must(r.code === 0 && /Filed "Set up GPG unlock on smoke"/.test(r.out), "setup: " + r.out);
    item = await until(async () => (await todoList("open")).find((t) => t.kind === "approval" && /^Set up GPG unlock on smoke/.test(t.question)), "the set-up item");
  });
  await check("6. Face ID and the passphrase set it up and unlock for 8 hours; the key signs with no prompt; the item closes", async () => {
    const sel = await openItem(item.id);
    await page.waitForSelector(`${sel}.open .gu-card`, { state: "visible", timeout: 8000 });
    await until(() => page.evaluate((s) => !/Reading the request/.test(document.querySelector(s)?.innerText || "Reading the request"), sel), "the card to read the request");
    await noScroll("gpg card");
    await shot("6-gpg-card-390");
    await page.locator(`${sel} .ap-confirm`).check();
    await page.locator(`${sel} .ap-pass`).fill(PASS);
    await page.locator(`${sel} .ap-go`).click();
    await until(async () => (await todoById(item.id)).state === "done", "the item to close", 30000);
    const st = JSON.parse(cli(coordPane, ["gpg", "status", "--json"]).stdout);
    must(!st.locked && st.has_blob, "status: " + JSON.stringify(st));
    const left = Date.parse(st.unlocked_until) - Date.now();
    must(left > 7.9 * 3600e3 && left < 8.01 * 3600e3, "the unlock is not 8 hours: " + left / 3600e3);
    const sg = spawnSync("gpg", ["--batch", "--pinentry-mode", "error", "-u", FPR, "--clearsign"], { env, input: "x", encoding: "utf8" });
    must(sg.status === 0 && /BEGIN PGP SIGNED MESSAGE/.test(sg.stdout), "sign: " + sg.stderr);
  });
}

// ---- 7. What the coordinator sees ------------------------------------------------------------------------------------------
await check("7. `comebye board` shows what happened: one landed, two still on it, nothing needing the user", async () => {
  const b = ok(cli(await coord.pane(), ["board"]), "board");
  must(/landed, recent \(1\)\n- one \[landed\]/.test(b), "board has no landed one:\n" + b);
  must(/needs you \(0\)/.test(b), "board says something needs the user:\n" + b);
  must(/- two\b/.test(b), "board does not list two:\n" + b);
});
await check("7. `comebye log` lists what the hub did itself: the relay of the user's answer and the landing", async () => {
  const l = ok(cli(await coord.pane(), ["log", "--since", "1h"]), "log");
  must(/relay\s+relayed .* → two \("ok 1"\)/.test(l), "no relay in the log:\n" + l);
  must(/landed\s+handoff h[0-9a-f]+ \(branch feat\/hello .* landed on origin\/main/.test(l), "no landing in the log:\n" + l);
});
await check("7. `comebye coordinator digest` carries the handoffs, the board and what the hub handled", async () => {
  const d = ok(cli(await coord.pane(), ["coordinator", "digest"]), "digest");
  must(/feat\/hello reported \(status now landed\)/.test(d), "digest has no landed handoff:\n" + d);
  must(/handed off `h[0-9a-f]+` proj feat\/two/.test(d), "digest does not know the second handoff:\n" + d);
  must(/## The hub handled since your last turn/.test(d) && /relay/.test(d), "digest has no hub-handled section:\n" + d);
});

await check("the browser run was clean: no page errors", async () => { must(pageErrors.length === 0, pageErrors.join("; ")); });
await check("the browser run was clean: no failed loads and no console errors from the app", async () => {
  // Two things are expected and are not the app's fault: this Chrome does not know the CSP directive 'webrtc' the app sends on
  // purpose (the starters check tolerates it the same way), and the transcript index has no session for the helper that Chat
  // about this just started (404 on /api/recall/sessions/...). A load that failed is told apart by its URL (failedLoads).
  const loads = failedLoads.filter((l) => !/^404 GET \/api\/recall\/sessions\//.test(l));
  const bad = consoleErrors.filter((t) => !/Unrecognized Content-Security-Policy directive 'webrtc'/.test(t) && !/^Failed to load resource/.test(t));
  must(loads.length === 0 && bad.length === 0, `failed loads: ${loads.join(", ") || "none"}; console errors: ${bad.slice(0, 5).join("; ") || "none"}`);
});

await browser.close();
const bad = results.filter((r) => !r.ok).length;
console.log(`day: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
