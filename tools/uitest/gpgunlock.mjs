// GPG unlock with a passkey, slice 3 (issue #3): the Set up and Unlock cards in To do, at 390 and 1280px, in the scheme the
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
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";

const [rawBase, out] = process.argv.slice(2);
const base = rawBase.replace("127.0.0.1", "localhost");
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR, gnupg = process.env.GNUPGHOME;
if (!bin || !stateDir) { console.error("gpgunlock.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
requireThrowawayGnupg("gpgunlock.mjs", gnupg);
if (!gpgInstalled()) { console.log("gpgunlock: gpg or its preset tool is not installed: skipped"); process.exit(0); }
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

// A throwaway key (ed25519, signing) with a passphrase, in the temp GNUPGHOME.
const FPR = throwawayKey(env);
const status = () => JSON.parse(cli(["gpg", "status", "--json"]).stdout);
const cleanHub = async () => {
  cli(["gpg", "reset", "--yes"]);
  const l = await listKeys();
  for (const k of l.passkeys) await fetch(base + "/api/passkeys/" + k.id, { method: "DELETE", headers: { "X-Comebye-Csrf": l.csrf } });
};

for (const width of [390, 1280]) {
  await cleanHub();
  const { browser, ctx, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  shotPage = page;
  page.on("pageerror", (e) => console.log("     pageerror: " + e));
  if (process.env.GU_DEBUG) await page.addInitScript(() => { window.__guDebug = true; });
  if (process.env.GU_DEBUG) page.on("console", (m) => console.log("     console." + m.type() + ": " + m.text()));
  // Everything the page sends to the hub, to prove no secret goes in the clear.
  const sent = [];
  page.on("request", (r) => { if (r.method() !== "GET") sent.push((r.postData() || "")); });
  const cdp = await ctx.newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  const auth = await addAuth(cdp, true);
  const scope = width > 500 ? "#p-todo" : "#todo";
  const noScroll = async (what) => {
    const g = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: innerWidth }));
    if (g.sw > g.iw) throw new Error(`${what}: page scrolls sideways (${g.sw} > ${g.iw})`);
  };
  const itemSel = (id) => `${scope} .td-item[data-id="${id}"]`;
  const openTodo = async (id) => {
    if (width > 500) {
      if (await page.locator("#settings:not([hidden])").count()) await page.click('#nav button[data-page="todo"]');
      else await page.goto(base + "/#/todo", { waitUntil: "load" });
    } else {
      await page.goto(base + "/", { waitUntil: "load" });
      await page.waitForSelector("#todo .td-toggle", { state: "visible", timeout: 15000 });
      if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
    }
    await page.evaluate(() => loadTodos());
    await page.waitForSelector(`${itemSel(id)} .td-row`, { state: "visible", timeout: 15000 });
    if (!(await page.locator(`${itemSel(id)}.open`).count())) await page.locator(`${itemSel(id)} .td-row`).click();
    await page.waitForSelector(`${itemSel(id)}.open .gu-card`, { state: "visible", timeout: 8000 });
    await page.waitForFunction((sel) => !/Reading the request/.test(document.querySelector(sel)?.innerText || "Reading the request"), itemSel(id), { timeout: 8000 });
  };
  const card = (id) => page.locator(`${itemSel(id)} .gu-card`).innerText();
  const itemOf = async (re) => {
    for (const end = Date.now() + 10000; Date.now() < end; await sleep(200)) {
      const l = await fetch(base + "/api/todos").then((r) => r.json());
      const it = (l.items || l.todos || []).find((t) => t.kind === "approval" && t.state === "open" && re.test(t.question));
      if (it) return it;
    }
    throw new Error("no open item matching " + re);
  };

  await check(`${width}px: register a passkey (PRF) so the host has one to trust`, async () => {
    await page.goto(base + "/#/settings", { waitUntil: "load" });
    await page.waitForSelector("#settings:not([hidden])");
    await page.waitForSelector("#sx-name", { timeout: 8000 });
    await page.fill("#sx-name", "Test phone");
    await page.click("#sx-add");
    await page.waitForSelector(".sx-card[data-fp]", { timeout: 10000 });
  });

  let setupItem;
  await check(`${width}px: comebye gpg setup files the "Set up GPG unlock" item with the host key fingerprint`, async () => {
    const r = cli(["gpg", "setup", "--key", FPR, "--host", HOST, "--yes"]);
    if (r.code !== 0 || !/Filed "Set up GPG unlock on uitest"/.test(r.out)) throw new Error("setup: " + r.out);
    cli.hostFp = (r.out.match(/Host key fingerprint: ([0-9A-F-]{24})/) || [])[1];
    if (!cli.hostFp) throw new Error("no fingerprint printed: " + r.out);
    setupItem = await itemOf(/^Set up GPG unlock on uitest/);
  });

  await check(`${width}px: the setup card: fixed 8 hours, the fingerprint, a passphrase box, no picker`, async () => {
    await openTodo(setupItem.id);
    const t = await card(setupItem.id);
    if (!/The unlock lasts 8 hours/.test(t) || !t.includes(cli.hostFp) || !/Passphrase of the GPG key/.test(t) || !/Set up with Face ID/.test(t)) throw new Error("card: " + t);
    const q = async (sel) => page.locator(`${itemSel(setupItem.id)} ${sel}`).count();
    if (await q(":is(select, input[type=radio], input[type=range], input[type=number])")) throw new Error("the card has a lifetime picker");
    if ((await page.locator(`${itemSel(setupItem.id)} .ap-pass`).getAttribute("type")) !== "password") throw new Error("the passphrase box is not a password field");
    if (await q(".td-text")) throw new Error("the passphrase box must not be a draft box");
    await noScroll("setup card");
    await page.screenshot({ path: `${out}/gpgunlock-setup-${width}.png` });
  });

  await check(`${width}px: failures say why: fingerprint not confirmed, no passphrase, Face ID cancelled`, async () => {
    const sel = itemSel(setupItem.id);
    await page.locator(`${sel} .ap-go`).click();
    await page.waitForFunction((s) => /Compare the fingerprint/.test(document.querySelector(s)?.innerText || ""), sel, { timeout: 4000 });
    await page.locator(`${sel} .ap-confirm`).check();
    await page.locator(`${sel} .ap-go`).click();
    await page.waitForFunction((s) => /Type the passphrase/.test(document.querySelector(s)?.innerText || ""), sel, { timeout: 4000 });
    if (!(await page.locator(`${sel} .ap-confirm`).isChecked())) throw new Error("the confirmation was lost on a redraw");
    // Face ID cancelled (the person taps Cancel on the sheet): the browser rejects the call with NotAllowedError. What was
    // typed stays for another try, and the request is not spent.
    await page.evaluate(() => {
      navigator.credentials.get = () => { delete navigator.credentials.get; return Promise.reject(new DOMException("cancelled", "NotAllowedError")); }; // once
    });
    await page.locator(`${sel} .ap-pass`).fill(PASS);
    await page.locator(`${sel} .ap-go`).click();
    await page.waitForFunction((s) => /Face ID was cancelled or timed out/.test(document.querySelector(s)?.innerText || ""), sel, { timeout: 15000 });
    if ((await page.locator(`${sel} .ap-pass`).inputValue()) !== PASS) throw new Error("a cancelled Face ID lost the typed passphrase");
    await noScroll("failure");
    await page.screenshot({ path: `${out}/gpgunlock-cancelled-${width}.png` });
    const a = await fetch(base + "/api/approvals/" + setupItem.approval.id).then((r) => r.json());
    if (a.approval.state !== "pending") throw new Error("a cancelled Face ID settled the request: " + a.approval.state);
  });

  await check(`${width}px: a wrong passphrase is refused by the host, stores nothing, and the item says so`, async () => {
    const sel = itemSel(setupItem.id);
    await page.locator(`${sel} .ap-pass`).fill("not the passphrase");
    await page.locator(`${sel} .ap-go`).click();
    await page.waitForFunction((s) => /Not unlocked\. That passphrase is not the key's/.test(document.querySelector(s)?.innerText || ""), sel, { timeout: 20000 });
    const st = status();
    if (st.has_blob || !st.locked) throw new Error("status after a wrong passphrase: " + JSON.stringify(st));
    if ((await todo(setupItem.id)).state !== "open") throw new Error("the item closed");
    if (await page.locator(`${sel} .ap-pass`).count()) throw new Error("a spent request still offers a passphrase box");
    await noScroll("host refused");
    await page.screenshot({ path: `${out}/gpgunlock-refused-${width}.png` });
  });

  await check(`${width}px: a new request reuses the item; the right passphrase sets up and unlocks, item closes`, async () => {
    const r = cli(["gpg", "unlock"]);
    if (r.code !== 0) throw new Error("unlock: " + r.out);
    for (const end = Date.now() + 8000; Date.now() < end; await sleep(200)) { const it = await todo(setupItem.id); if (it.approval && it.approval.state === "pending") break; }
    await openTodo(setupItem.id);
    const sel = itemSel(setupItem.id);
    const t = await card(setupItem.id);
    if (/Not unlocked/.test(t) || !/Set up with Face ID/.test(t)) throw new Error("card after a new request: " + t);
    await page.locator(`${sel} .ap-confirm`).check();
    await page.locator(`${sel} .ap-pass`).fill(PASS);
    await page.locator(`${sel} .ap-go`).click();
    for (const end = Date.now() + 25000; Date.now() < end; await sleep(200)) { if ((await todo(setupItem.id)).state === "done") break; }
    if ((await todo(setupItem.id)).state !== "done") throw new Error("item: " + JSON.stringify(await todo(setupItem.id)));
    const st = status();
    if (st.locked || !st.has_blob || !st.unlocked_until) throw new Error("status: " + JSON.stringify(st));
    const left = Date.parse(st.unlocked_until) - Date.now();
    if (left < 7.9 * 3600e3 || left > 8.01 * 3600e3) throw new Error("the unlock is not 8 hours: " + left / 3600e3);
    // The key really signs with no prompt.
    const sg = spawnSync("gpg", ["--batch", "--pinentry-mode", "error", "-u", FPR, "--clearsign"], { env, input: "x", encoding: "utf8" });
    if (sg.status !== 0 || !/BEGIN PGP SIGNED MESSAGE/.test(sg.stdout)) throw new Error("sign: " + sg.stderr);
  });

  await check(`${width}px: no secret is left in the DOM or in storage, and none went to the hub in the clear`, async () => {
    await page.waitForFunction(() => !document.querySelector(".ap-pass"), null, { timeout: 5000 });
    const leaks = await page.evaluate(async (pass) => {
      const found = [];
      const b64 = (s) => btoa(s), b64u = (s) => btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
      const needles = [pass, b64(pass), b64u(pass)];
      const has = (s) => needles.some((n) => String(s).includes(n));
      if (has(document.documentElement.outerHTML)) found.push("DOM html");
      for (const el of document.querySelectorAll("input, textarea")) if (has(el.value)) found.push("input value");
      if ([...document.querySelectorAll("input[type=password]")].some((e) => e.value)) found.push("a password box holds text");
      for (const st of [localStorage, sessionStorage]) for (let i = 0; i < st.length; i++) { const k = st.key(i); if (has(k) || has(st.getItem(k))) found.push("web storage " + k); if (/prf|passphrase|unlock-k|\bK\b/i.test(k)) found.push("suspect key " + k); }
      // The app keeps one database, the saved lists (comebye-cache, #136 slice 4): every record in it is read for the
      // passphrase. Any other database is a leak.
      if (indexedDB.databases) for (const d of await indexedDB.databases()) {
        if (d.name !== "comebye-cache") { found.push("indexedDB " + d.name); continue; }
        const recs = await new Promise((res) => {
          const rq = indexedDB.open(d.name);
          rq.onerror = () => res(null);
          rq.onsuccess = () => {
            const db = rq.result;
            if (!db.objectStoreNames.contains("src")) { db.close(); res([]); return; }
            const g = db.transaction("src").objectStore("src").getAll();
            g.onsuccess = () => { db.close(); res(g.result); };
            g.onerror = () => { db.close(); res(null); };
          };
        });
        if (!recs) found.push("indexedDB comebye-cache (unreadable)");
        else for (const r of recs) if (has(JSON.stringify(r))) found.push("indexedDB comebye-cache " + r.name);
      }
      for (const k of await caches.keys()) found.push("cache " + k);
      if (has(document.cookie)) found.push("cookie");
      return found;
    }, PASS);
    if (leaks.length) throw new Error("left behind: " + leaks.join(", "));
    const bad = sent.filter((b) => [PASS, Buffer.from(PASS).toString("base64"), Buffer.from(PASS).toString("base64url")].some((n) => b.includes(n)));
    if (bad.length) throw new Error("the passphrase was sent to the hub in the clear");
    if (!sent.some((b) => b.includes('"blob"') && b.includes('"sealed"'))) throw new Error("the sealed answer was not seen in the traffic: " + sent.length);
    // The hub's own files hold none of it either.
    for (const f of ["approvals.json", "todos.json"]) {
      const p = path.join(stateDir, f);
      if (fs.existsSync(p) && fs.readFileSync(p, "utf8").includes(PASS)) throw new Error(f + " holds the passphrase");
    }
    const blob = fs.readFileSync(path.join(stateDir, "gpg-blob.json"), "utf8");
    if (blob.includes(PASS)) throw new Error("the stored blob is not encrypted");
  });

  let unlockItem;
  await check(`${width}px: locked again: the unlock card has the fixed 8 hours, no passphrase box, no picker`, async () => {
    if (cli(["gpg", "lock"]).code !== 0) throw new Error("lock");
    const r = cli(["gpg", "unlock"]);
    if (r.code !== 0) throw new Error("unlock: " + r.out);
    unlockItem = await itemOf(/^GPG is locked on uitest/);
    await openTodo(unlockItem.id);
    const t = await card(unlockItem.id);
    if (!/Unlocks the GPG key on uitest/.test(t) || !/The unlock lasts 8 hours/.test(t) || !/Unlock with Face ID/.test(t) || !/Not now/.test(t)) throw new Error("card: " + t);
    const q = async (sel) => page.locator(`${itemSel(unlockItem.id)} ${sel}`).count();
    if (await q(":is(.ap-pass, .ap-confirm, select, input, textarea)")) throw new Error("the unlock card has inputs");
    await noScroll("unlock card");
    await page.screenshot({ path: `${out}/gpgunlock-unlock-${width}.png` });
  });

  await check(`${width}px: Unlock with Face ID unlocks the key and closes the item`, async () => {
    await page.locator(`${itemSel(unlockItem.id)} .ap-go`).click();
    for (const end = Date.now() + 25000; Date.now() < end; await sleep(200)) { if ((await todo(unlockItem.id)).state === "done") break; }
    if ((await todo(unlockItem.id)).state !== "done") throw new Error("item: " + JSON.stringify(await todo(unlockItem.id)));
    if (status().locked) throw new Error("still locked");
    await page.waitForFunction(() => /Unlocked until \d/.test(document.getElementById("toast")?.innerText || ""), null, { timeout: 6000 })
      .catch(async () => { throw new Error("toast: " + (await page.locator("#toast").innerText().catch(() => ""))); });
  });

  // The host timer, live (before "Not now": that one snoozes the watcher on the hub for a while): `comebye gpg watch --once` against the real hub, whose fake herdr has working agents.
  await check(`${width}px: gpg watch files one item for a locked host, none twice, and withdraws it when pinentry unlocks`, async () => {
    cli(["gpg", "lock"]);
    fs.rmSync(path.join(stateDir, "gpg-filed.json"), { force: true }); // the earlier asks in this script would hold the watcher back (its cooldown)
    const open = async () => (await (await fetch(`${base}/api/todos`)).json());
    const mine = async () => ((await open()).items || (await open()).todos || []).filter((i) => /^GPG is locked on uitest/.test(i.question) && i.state === "open");
    // Earlier scripts leave the fixture's agents idle: start a turn, which keeps one working for a couple of seconds.
    const ag = (await fetch(base + "/api/agents").then((r) => r.json())).agents[0];
    const pr = await fetch(`${base}/api/agents/${encodeURIComponent(ag.pane_id)}/prompt`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ text: "work" }) });
    if (![200, 202].includes(pr.status)) throw new Error("prompt: " + pr.status);
    let w1 = { code: 0, out: "" };
    for (const end = Date.now() + 6000; Date.now() < end && !/filed/.test(w1.out); await sleep(250)) w1 = cli(["gpg", "watch", "--once"]);
    if (w1.code !== 0 || !/filed/.test(w1.out)) throw new Error("first round: " + w1.code + " " + w1.out);
    const w2 = cli(["gpg", "watch", "--once"]);
    const u = cli(["gpg", "unlock"]);
    if (w2.code !== 0 || /filed/.test(w2.out) || !/Already asked/.test(u.out)) throw new Error("second round: " + w2.out + " / unlock: " + u.out);
    if ((await mine()).length !== 1) throw new Error("items: " + JSON.stringify(await mine()));
    // Unlock without the app: sign once with the passphrase typed to the agent, as pinentry would.
    const sg = spawnSync("gpg", ["--batch", "--pinentry-mode", "loopback", "--passphrase", PASS, "-u", FPR, "--clearsign"], { env, input: "x", encoding: "utf8" });
    if (sg.status !== 0) throw new Error("sign: " + sg.stderr);
    if (status().locked) throw new Error("the agent did not cache the passphrase");
    const w3 = cli(["gpg", "watch", "--once"]);
    if (w3.code !== 0 || !/withdrawn/.test(w3.out)) throw new Error("third round: " + w3.out);
    if ((await mine()).length !== 0) throw new Error("the item is still open");
  });

  await check(`${width}px: Not now denies an unlock request`, async () => {
    cli(["gpg", "lock"]);
    const r = cli(["gpg", "unlock", "--json"]);
    if (r.code !== 0) throw new Error("unlock: " + r.out);
    const it = await itemOf(/^GPG is locked on uitest/);
    await openTodo(it.id);
    await page.locator(`${itemSel(it.id)} .ap-no`).click();
    for (const end = Date.now() + 8000; Date.now() < end; await sleep(150)) { if ((await todo(it.id)).state === "dropped") break; }
    if ((await todo(it.id)).state !== "dropped") throw new Error("item not dropped");
    if (!status().locked) throw new Error("unlocked by Not now");
  });

  await browser.close();
}

await cleanHub();
spawnSync("gpgconf", ["--kill", "gpg-agent"], { env });
const bad = results.filter((r) => !r.ok).length;
console.log(`gpgunlock: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
