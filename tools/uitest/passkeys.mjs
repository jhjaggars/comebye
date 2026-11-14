// Passkeys, slice 1 (issue #3): Settings, Passkeys (register, rename, remove) and the approval item in To do,
// at 390 and 1280px, in the scheme the run was started with. Chrome's virtual authenticator (CDP WebAuthn domain,
// with PRF on) stands in for Apple Passwords, so the real navigator.credentials, the real attestation and
// assertions and the hub's real checks all run. A second virtual authenticator without PRF checks the refusal.
//
//   node passkeys.mjs BASE OUT_DIR       (env: COMEBYE_STATE_DIR for the agent token, SCHEME)
//
// WebAuthn needs a registrable host name: 127.0.0.1 does not do, so the page is opened as localhost (the hub
// allows that only under -test-mode).
import { launch } from "./lib.mjs";
import fs from "node:fs";
import path from "node:path";

const [rawBase, out] = process.argv.slice(2);
const base = rawBase.replace("127.0.0.1", "localhost");
const stateDir = process.env.COMEBYE_STATE_DIR;
if (!stateDir) { console.error("passkeys.mjs: set COMEBYE_STATE_DIR"); process.exit(2); }
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const token = fs.readFileSync(path.join(stateDir, "agent-token"), "utf8").trim();
const results = [];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let shotPage = null;
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); if (shotPage) await shotPage.screenshot({ path: `${out}/passkeys-FAIL-${results.length}.png` }).catch(() => {}); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const J = { "Content-Type": "application/json" };
const listKeys = async () => (await fetch(base + "/api/passkeys").then((r) => r.json()));
const fileApproval = async (subject) => {
  const r = await fetch(base + "/api/approvals", { method: "POST", headers: { ...J, "X-Comebye-Agent-Token": token, "X-Comebye-Agent": "w4:p1" }, body: JSON.stringify({ action: "gpg-unlock", subject }) });
  const j = await r.json();
  if (!j.todo) throw new Error("file approval: " + r.status + " " + JSON.stringify(j));
  return j;
};
const todo = async (id) => (await fetch(base + "/api/todos/" + id).then((r) => r.json())).todo;
const addAuth = (cdp, hasPrf) => cdp.send("WebAuthn.addVirtualAuthenticator", { options: {
  protocol: "ctap2", ctap2Version: "ctap2_1", transport: "internal", hasResidentKey: true, hasUserVerification: true, isUserVerified: true,
  automaticPresenceSimulation: true, hasPrf } });

for (const width of [390, 1280]) {
  const { browser, ctx, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  // The phone gets the iPhone user agent from launch(); WebAuthn does not care.
  shotPage = page;
  page.on("pageerror", (e) => console.log("     pageerror: " + e));
  page.on("console", (m) => { if (m.type() === "error") console.log("     console.error: " + m.text()); });
  const cdp = await ctx.newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  const noScroll = async (what) => {
    const g = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: innerWidth }));
    if (g.sw > g.iw) throw new Error(`${what}: page scrolls sideways (${g.sw} > ${g.iw})`);
  };
  // On a phone the list sits under a toggle on the home screen; on a desktop it is the To do page's pane.
  const scope = width > 500 ? "#p-todo" : "#todo";
  const openTodo = async (id) => {
    if (width > 500) {
      // Leave Settings through the nav, as a person would (the To do page replaces the screen in the pane).
      if (await page.locator("#settings:not([hidden])").count()) await page.click('#nav button[data-page="todo"]');
      else await page.goto(base + "/#/todo", { waitUntil: "load" });
    } else {
      await page.goto(base + "/", { waitUntil: "load" });
      await page.waitForSelector("#todo .td-toggle", { state: "visible", timeout: 15000 });
      if ((await page.locator("#todo .td-toggle").getAttribute("aria-expanded")) === "false") await page.locator("#todo .td-toggle").click();
    }
    await page.evaluate(() => loadTodos()); // the list refreshes every few seconds: ask now
    await page.waitForSelector(`${scope} .td-item[data-id="${id}"] .td-row`, { state: "visible", timeout: 15000 });
    await page.locator(`${scope} .td-item[data-id="${id}"] .td-row`).click();
    await page.waitForSelector(`${scope} .td-item.open[data-id="${id}"] .ap-go`, { state: "visible", timeout: 8000 });
  };
  const body = () => page.locator("#sx-view").innerText();
  const open = async () => {
    await page.goto(base + "/#/settings", { waitUntil: "load" });
    await page.waitForSelector("#settings:not([hidden])");
    await page.waitForFunction(() => !/Loading/.test(document.querySelector("#sx-view")?.innerText || "Loading"), null, { timeout: 8000 });
  };

  await check(`${width}px: Settings opens to an empty Passkeys list`, async () => {
    for (const k of (await listKeys()).passkeys) await fetch(base + "/api/passkeys/" + k.id, { method: "DELETE", headers: { "X-Comebye-Csrf": (await listKeys()).csrf } });
    await open();
    const t = await body();
    if (!/Passkeys/.test(t) || !/No passkeys yet/.test(t) || !/AutoFill From/.test(t)) throw new Error("body: " + t);
    await noScroll("empty");
    await page.screenshot({ path: `${out}/passkeys-empty-${width}.png` });
  });

  await check(`${width}px: a passkey without PRF is refused with the iOS Settings message, and nothing is stored`, async () => {
    const a = await addAuth(cdp, false);
    await page.fill("#sx-name", "No PRF phone");
    await page.click("#sx-add");
    await page.waitForSelector(".sx-msg.err", { timeout: 10000 });
    const t = await body();
    if (!/turn on Passwords under Settings, General, AutoFill & Passwords, AutoFill From/.test(t)) throw new Error("message: " + t);
    if ((await listKeys()).passkeys.length) throw new Error("a passkey without PRF was stored");
    await noScroll("no prf");
    await page.screenshot({ path: `${out}/passkeys-noprf-${width}.png` });
    await cdp.send("WebAuthn.removeVirtualAuthenticator", { authenticatorId: a.authenticatorId });
  });

  const auth = await addAuth(cdp, true);
  await check(`${width}px: registering with PRF adds a card with a fingerprint`, async () => {
    await open();
    await page.fill("#sx-name", "Jesse's iPhone");
    await page.click("#sx-add");
    await page.waitForSelector('.sx-card[data-fp]', { timeout: 10000 });
    const t = await body();
    if (!/Jesse's iPhone/.test(t) || !/Fingerprint [0-9A-F]{4}-[0-9A-F]{4}-/.test(t) || !/Added /.test(t)) throw new Error("body: " + t);
    const l = await listKeys();
    if (l.passkeys.length !== 1 || l.rp_id !== "localhost" || l.passkeys[0].alg !== -7) throw new Error("hub list: " + JSON.stringify(l));
    await noScroll("one passkey");
    await page.screenshot({ path: `${out}/passkeys-one-${width}.png` });
  });

  await check(`${width}px: a second passkey needs the first one's Face ID first, then both are listed`, async () => {
    await page.fill("#sx-name", "Second");
    const before = await cdp.send("WebAuthn.getCredentials", { authenticatorId: auth.authenticatorId });
    await page.click("#sx-add");
    await page.waitForFunction(() => document.querySelectorAll(".sx-card[data-fp]").length === 2, null, { timeout: 12000 });
    const l = await listKeys();
    if (l.passkeys.length !== 2) throw new Error("hub list: " + JSON.stringify(l));
    const after = await cdp.send("WebAuthn.getCredentials", { authenticatorId: auth.authenticatorId });
    if (after.credentials.length !== before.credentials.length + 1) throw new Error("credentials " + before.credentials.length + " -> " + after.credentials.length);
    // The first passkey was used for the approval: the hub recorded it.
    if (!l.passkeys.find((k) => k.name === "Jesse's iPhone").last_used_at) throw new Error("first passkey's use not recorded");
    await noScroll("two passkeys");
    await page.screenshot({ path: `${out}/passkeys-two-${width}.png` });
  });

  await check(`${width}px: rename and remove (with its confirmation)`, async () => {
    await page.locator('.sx-card:has-text("Second") [data-sx="rename"]').click();
    await page.fill("#sx-rename", "Travel key");
    await page.keyboard.press("Enter");
    await page.waitForFunction(() => /Travel key/.test(document.querySelector("#sx-view").innerText));
    if ((await listKeys()).passkeys.map((k) => k.name).join() !== "Jesse's iPhone,Travel key") throw new Error("renamed list: " + JSON.stringify((await listKeys()).passkeys.map((k) => k.name)));
    await page.locator('.sx-card:has-text("Travel key") [data-sx="remove"]').click();
    const t = await body();
    if (!/Remove Travel key\?|Remove .*Travel key/.test(t) || !/iOS Passwords app/.test(t)) throw new Error("confirm text: " + t);
    await noScroll("confirm");
    await page.screenshot({ path: `${out}/passkeys-confirm-${width}.png` });
    await page.locator('[data-sx="cancel"]').click();
    if ((await listKeys()).passkeys.length !== 2) throw new Error("Cancel removed it");
    await page.locator('.sx-card:has-text("Travel key") [data-sx="remove"]').click();
    await page.locator('[data-sx="remove-go"]').click();
    await page.waitForFunction(() => document.querySelectorAll(".sx-card[data-fp]").length === 1);
    if ((await listKeys()).passkeys.length !== 1) throw new Error("not removed on the hub");
  });

  await check(`${width}px: an approval item in To do is approved with Face ID and closes`, async () => {
    const { todo: it } = await fileApproval("signing key 62B4 " + width);
    await openTodo(it.id);
    const t = await page.locator(`${scope} .td-item[data-id="${it.id}"]`).innerText();
    if (!/GPG is locked on uitest/.test(t) || !/Approve with Face ID/.test(t) || !/Not now/.test(t)) throw new Error("card text: " + t);
    if (await page.locator(`${scope} .td-item[data-id="${it.id}"] .td-text`).count()) throw new Error("an approval has no answer box");
    await noScroll("approval card");
    await page.screenshot({ path: `${out}/passkeys-approval-${width}.png` });
    await page.locator(`${scope} .td-item[data-id="${it.id}"] .ap-go`).click();
    for (const end = Date.now() + 10000; Date.now() < end; await sleep(150)) { if ((await todo(it.id)).state === "done") break; }
    const done = await todo(it.id);
    if (done.state !== "done" || done.approval.state !== "approved") throw new Error("item: " + JSON.stringify(done));
  });

  await check(`${width}px: Not now denies the request`, async () => {
    const { todo: it, approval } = await fileApproval("signing key again " + width);
    await openTodo(it.id);
    await page.locator(`${scope} .td-item[data-id="${it.id}"] .ap-no`).click();
    for (const end = Date.now() + 8000; Date.now() < end; await sleep(150)) { if ((await todo(it.id)).state === "dropped") break; }
    const d = await todo(it.id);
    if (d.state !== "dropped") throw new Error("item: " + JSON.stringify(d));
    const r = await fetch(base + "/api/approvals/" + approval.id).then((x) => x.json());
    if (r.approval.state !== "denied") throw new Error("approval: " + JSON.stringify(r.approval));
  });

  await check(`${width}px: Back closes Settings`, async () => {
    await open();
    if (width < 1100) {
      await page.click("#sx-back");
      await page.waitForSelector("#settings", { state: "hidden" });
    } else {
      await page.keyboard.press("Escape");
      await page.waitForSelector("#settings", { state: "hidden" });
    }
  });
  await browser.close();
}

// Leave no passkey behind for the scripts that run after this one.
try { const l = await listKeys(); for (const k of l.passkeys) await fetch(base + "/api/passkeys/" + k.id, { method: "DELETE", headers: { "X-Comebye-Csrf": l.csrf } }); } catch {}
const bad = results.filter((r) => !r.ok).length;
console.log(`passkeys: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
