// The hub closes a release's to-do item itself once the hub and every host run
// its version: a real comebye built with a version stamped in (-ldflags
// -X main.version=...) and -release-simulate, driven over HTTP.
//
//   node releaseauto.mjs OUT_DIR       (env: COMEBYE_STAMPED_BIN, FAKE_SOCK, AUTO_PORT)
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [out] = process.argv.slice(2);
const bin = process.env.COMEBYE_STAMPED_BIN, sock = process.env.FAKE_SOCK, port = process.env.AUTO_PORT;
if (!bin || !sock || !port) { console.error("releaseauto.mjs: set COMEBYE_STAMPED_BIN, FAKE_SOCK and AUTO_PORT"); process.exit(2); }
const base = `http://127.0.0.1:${port}`;
const dir = fs.mkdtempSync(path.join(os.tmpdir(), "relauto-"));
const log = fs.openSync(path.join(out, "releaseauto.log"), "w");
const p = spawn(bin, ["-sock", sock, "-addr", `127.0.0.1:${port}`, "-insecure-no-auth", "-push=false", "-state-dir", dir, "-host-name", "autohub", "-release-simulate"], { stdio: ["ignore", log, log] });
const results = [];
const check = async (name, fn) => { try { await fn(); results.push([name, true, ""]); } catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); } };
const j = async (m, u, b, h = {}) => { const r = await fetch(base + u, { method: m, headers: { "Content-Type": "application/json", ...h }, body: b && JSON.stringify(b) }); return { code: r.status, body: await r.json().catch(() => ({})) }; };
try {
  for (let i = 0; i < 80; i++) { try { if ((await fetch(base + "/api/agents")).ok) break; } catch {} await new Promise((r) => setTimeout(r, 100)); }
  const SHA = "0123456789abcdef0123456789abcdef01234567";
  const add = async (version) => {
    const t = (await j("POST", "/api/todos", { question: "", kind: "release", no_wait: true, release: { version, commit: SHA } })).body.todo;
    const v = (await j("GET", "/api/release/" + t.id)).body;
    if ((await j("POST", `/api/release/${t.id}/approve`, { version, commit: SHA }, { "X-Comebye-Csrf": v.csrf })).code !== 202) throw new Error("approve " + version);
    for (let i = 0; i < 100; i++) { if ((await j("GET", "/api/todos/" + t.id)).body.todo.release.state === "tagged") return t; await new Promise((r) => setTimeout(r, 100)); }
    throw new Error("not tagged");
  };
  await check("auto-close: a release newer than the hub keeps its item open and the tab shows it behind", async () => {
    const t = await add("v101.0.1");
    const v = (await j("GET", "/api/releases")).body;
    if (!v.current || v.todo.id !== t.id || v.rollout.behind !== 1 || v.rollout.complete || !v.needs_you) throw new Error(JSON.stringify(v.rollout));
    if ((await j("GET", "/api/todos/" + t.id)).body.todo.state !== "open") throw new Error("closed too early");
    if ((await j("POST", `/api/todos/${t.id}/done`, {})).body.code !== "release_pending") throw new Error("manual close was not refused");
  });
  await check("auto-close: a release the hub runs closes its item by itself, shows as all upgraded, and joins the history", async () => {
    const t = await add("v0.0.1"); // the stamped hub runs v100.0.0: not behind
    const v = (await j("GET", "/api/releases")).body;
    const it = (await j("GET", "/api/todos/" + t.id)).body.todo;
    if (it.state !== "done" || it.closed_by !== "comebye") throw new Error("item: " + it.state + " " + it.closed_by);
    if (!v.rollout) throw new Error("no rollout in the view");
  });
} finally { p.kill(); }
for (const [n, ok, why] of results) console.log(`${ok ? "PASS" : "FAIL"}  ${n}${ok ? "" : "  -- " + why}`);
process.exit(results.some((r) => !r[1]) ? 1 : 0);
