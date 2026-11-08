// Voice input in headless Chrome, with Chrome's fake audio device playing a
// clip as the microphone.
//
//   node stt.mjs BASE_EMPTY OUT [BASE_INSTALLED]
//
// BASE_EMPTY is a comebye whose state dir has no speech files (the "not
// installed" state, the secure-context error). BASE_INSTALLED is one whose
// state dir has them (`comebye stt fetch`); without it the checks that need
// the model are skipped, and say so. Env: STT_CLIP (a 16 kHz mono wav the
// "microphone" plays; default fixtures/speech.wav), STT_WORDS (words that
// must appear in the transcript), STT_GPU=1 (use the GPU: Vulkan flags).
import { chromium } from "playwright-core";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { CHROME } from "./lib.mjs";

const [baseEmpty, out, baseInst] = process.argv.slice(2);
const here = path.dirname(fileURLToPath(import.meta.url));
const clip = process.env.STT_CLIP || path.join(here, "fixtures/speech.wav");
const words = (process.env.STT_WORDS || "brown fox lazy dog").toLowerCase().split(/\s+/);
const gpu = process.env.STT_GPU === "1" ? ["--enable-unsafe-webgpu", "--enable-features=Vulkan", "--use-angle=vulkan", "--use-gl=angle"] : [];
fs.mkdirSync(out, { recursive: true });

const results = [];
async function check(name, fn, page) {
  const t0 = Date.now();
  try { await fn(); results.push([name, true, ""]); }
  catch (e) { results.push([name, false, String(e.message).split("\n")[0]]); }
  if (page) await page.screenshot({ path: `${out}/stt-${name.replace(/\W+/g, "-")}.png` }).catch(() => {});
  console.log(`${results.at(-1)[1] ? "ok  " : "FAIL"} ${name} (${((Date.now() - t0) / 1000).toFixed(1)}s)${results.at(-1)[2] ? " — " + results.at(-1)[2] : ""}`);
}
const skip = (name, why) => { results.push([name, true, "skipped: " + why]); console.log(`skip ${name} — ${why}`); };

// A browser whose "microphone" plays the clip. grant=false leaves the
// permission prompt unanswered, which headless Chrome treats as a refusal.
async function browser({ grant = true, width = 1280, height = 900, dark = true } = {}) {
  const b = await chromium.launch({
    executablePath: CHROME, headless: true,
    args: ["--no-sandbox", "--use-fake-device-for-media-stream", `--use-file-for-fake-audio-capture=${clip}`,
      ...(grant ? ["--use-fake-ui-for-media-stream"] : ["--deny-permission-prompts"]), ...gpu],
  });
  const ctx = await b.newContext({ viewport: { width, height }, colorScheme: dark ? "dark" : "light" });
  const page = await ctx.newPage();
  const reqs = [];
  const logs = [];
  page.on("request", (r) => reqs.push({ url: r.url(), method: r.method() }));
  page.on("console", (m) => logs.push(`${m.type()}: ${m.text()}`));
  page.on("pageerror", (e) => logs.push(`pageerror: ${e.message}`));
  return { b, ctx, page, reqs, logs };
}
// page.waitForFunction evaluates a string, which the app's CSP (rightly)
// blocks, so poll with page.evaluate (a function call, not an eval).
async function until(page, fn, arg, { timeout = 10000 } = {}) {
  const t0 = Date.now();
  for (;;) {
    if (await page.evaluate(fn, arg)) return;
    if (Date.now() - t0 > timeout) throw new Error("timed out waiting for " + fn.toString().slice(0, 90));
    await new Promise((r) => setTimeout(r, 150));
  }
}
const kindIs = (...k) => (ks) => ks.includes(document.querySelector(".stt-status:not([hidden])")?.dataset.kind);
const status = (page, target = "prompt") => page.locator(`.stt-status[data-stt-status="${target}"]:not([hidden])`);
const mic = (page, target = "prompt") => page.locator(`.stt-mic[data-stt="${target}"]:visible`).first();
const text = async (loc) => (await loc.innerText()).replace(/\s+/g, " ").trim();
const promptText = (page) => page.evaluate(() => document.getElementById("prompt-text").innerText.replace(/ /g, " ").trim());

async function openAlpha(page, base) {
  await page.goto(base + "/#/agent/" + encodeURIComponent("w1:p1"), { waitUntil: "load" });
  await page.waitForSelector("#detail", { state: "visible" });
  await page.waitForSelector("#prompt-text", { state: "visible" });
}

// ---- not installed (no model files on the host) ----
{
  const { b, page, reqs } = await browser();
  await check("not installed: the first tap says so and loads nothing", async () => {
    await openAlpha(page, baseEmpty);
    await mic(page).waitFor();
    if (reqs.some((r) => /\/stt\/|transformers\.bundle|stt-worker/.test(r.url))) throw new Error("voice files were requested before any tap");
    await mic(page).click();
    await until(page, (ks) => ks.includes(document.querySelector(".stt-status:not([hidden])")?.dataset.kind), ["missing"], { timeout: 8000 });
    const t = await text(status(page));
    if (!/isn't installed on this host/.test(t) || !/comebye stt fetch/.test(t)) throw new Error("message: " + t);
    if (reqs.some((r) => /\/stt\/|transformers\.bundle|stt-worker/.test(r.url))) throw new Error("it loaded the engine although nothing is installed");
    if ((await mic(page).getAttribute("data-state")) !== "error") throw new Error("button state " + (await mic(page).getAttribute("data-state")));
    await status(page).locator('[data-act="dismiss"]').click();
    await status(page).waitFor({ state: "detached", timeout: 2000 }).catch(() => {});
    if (await status(page).count()) throw new Error("status did not go away");
  }, page);
  await b.close();
}

// ---- no secure context ----
{
  const { b, ctx, page } = await browser();
  await ctx.addInitScript(() => Object.defineProperty(window, "isSecureContext", { value: false }));
  await check("no secure context: a clear error, no request for the engine", async () => {
    await openAlpha(page, baseEmpty);
    await mic(page).click();
    await status(page).waitFor({ timeout: 5000 });
    const t = await text(status(page));
    if (!/secure page/.test(t) || !/https/.test(t)) throw new Error("message: " + t);
  }, page);
  await b.close();
}

if (!baseInst) {
  for (const n of ["mic denied", "enable step, download progress, listening, transcript inserted and not sent", "later visit skips the download", "To do answer box", "shortcut and Esc"]) skip(n, "no installed host given (BASE_INSTALLED)");
} else {
  // ---- mic denied ----
  {
    const { b, page } = await browser({ grant: false });
    await check("mic denied: its own message, and nothing is downloaded", async () => {
      await openAlpha(page, baseInst);
      await mic(page).click();
      await status(page).waitFor({ timeout: 8000 });
      await until(page, () => /blocked/.test(document.querySelector(".stt-status:not([hidden])")?.innerText || ""), null, { timeout: 8000 });
      const t = await text(status(page));
      if (!/Microphone blocked/.test(t)) throw new Error("message: " + t);
      if ((await mic(page).getAttribute("data-state")) !== "error") throw new Error("state");
    }, page);
    await b.close();
  }

  // ---- the main flow, then a later visit, then the To do box ----
  const { b, ctx, page, reqs, logs } = await browser();
  const sent = [];
  await ctx.route(/\/api\/(hosts\/[^/]+\/)?agents\/[^/]+\/prompt$/, (r) => { sent.push(r.request().postData()); r.continue(); });
  let backend = "";
  const timing = {};

  await check("enable step, download progress, listening, transcript inserted and not sent", async () => {
    await openAlpha(page, baseInst);
    await page.locator("#prompt-text").click();
    await page.keyboard.type("Please note: ");
    await mic(page).click();
    await status(page).locator('[data-act="enable"]').waitFor({ timeout: 30000 });
    const offer = await text(status(page));
    if (!/Enable voice input \(downloads \d+ MB\)/.test(offer)) throw new Error("offer: " + offer);
    backend = /CPU/.test(offer) ? "wasm" : "gpu";
    await page.screenshot({ path: `${out}/stt-offer.png` });
    // Nothing of the model or runtime has been requested yet.
    if (reqs.some((r) => /\/stt\/[^?]*\.(onnx|wasm)/.test(r.url))) throw new Error("downloaded before Enable");
    const tEnable = Date.now();
    await status(page).locator('[data-act="enable"]').click();
    await until(page, (ks) => ks.includes(document.querySelector(".stt-status:not([hidden])")?.dataset.kind), ["downloading"], { timeout: 10000 });
    if ((await mic(page).getAttribute("data-state")) !== "down") throw new Error("button not in the downloading state");
    await until(page, () => /(GPU|CPU \(slower\))/.test(document.querySelector(".stt-status:not([hidden])")?.innerText || ""), null, { timeout: 10000 });
    const dl = await text(status(page));
    if (!/Downloading the voice model/.test(dl) || !/(GPU|CPU \(slower\))/.test(dl)) throw new Error("downloading text: " + dl);
    await page.screenshot({ path: `${out}/stt-downloading.png` });
    await until(page, () => /Voice input is ready/.test(document.querySelector(".stt-status:not([hidden])")?.innerText || ""), null, { timeout: 300000 });
    timing.download_and_load_ms = Date.now() - tEnable;
    // Record: the clip is 12 s long.
    await mic(page).click();
    await until(page, (ks) => ks.includes(document.querySelector(".stt-status:not([hidden])")?.dataset.kind), ["listening"], { timeout: 10000 });
    if ((await mic(page).getAttribute("data-state")) !== "listening" || (await mic(page).getAttribute("aria-pressed")) !== "true") throw new Error("button not listening");
    await page.waitForTimeout(1500);
    const lt = await text(status(page));
    if (!/Listening… 0:0[1-9] \/ 0:30/.test(lt)) throw new Error("timer: " + lt);
    await page.screenshot({ path: `${out}/stt-listening.png` });
    await page.waitForTimeout(11500);
    const tStop = Date.now();
    await mic(page).click();
    await until(page, (ks) => ks.includes(document.querySelector(".stt-status:not([hidden])")?.dataset.kind), ["transcribing", "note"], { timeout: 10000 });
    await until(page, (ks) => ks.includes(document.querySelector(".stt-status:not([hidden])")?.dataset.kind), ["note"], { timeout: 120000 });
    timing.transcribe_ms = Date.now() - tStop;
    const got = await promptText(page);
    const low = got.toLowerCase();
    if (!got.startsWith("Please note:")) throw new Error("typed text lost / not at the cursor: " + got);
    for (const w of words) if (!low.includes(w)) throw new Error(`"${w}" not in transcript: ${got}`);
    if (!/nothing is sent/.test(await text(status(page)))) throw new Error("no 'not sent' note");
    await page.waitForTimeout(500);
    if (sent.length) throw new Error("a prompt was sent: " + sent.join(" | "));
    if ((await page.locator("#btn-send").getAttribute("disabled")) !== null) throw new Error("Send should be enabled, waiting for the user");
    await page.screenshot({ path: `${out}/stt-inserted.png` });
    console.log(`     backend ${backend}; transcript: ${got}`);
    console.log(`     timing: ${JSON.stringify(timing)}`);
  }, page);

  await check("everything stayed on the app's origin", async () => {
    const origin = new URL(baseInst).origin;
    const off = reqs.filter((r) => !r.url.startsWith(origin) && !r.url.startsWith("data:") && !r.url.startsWith("blob:"));
    if (off.length) throw new Error("outside requests: " + off.map((r) => r.url).slice(0, 3).join(", "));
    const wasmOk = reqs.some((r) => /\.wasm$/.test(r.url));
    if (!wasmOk) throw new Error("the runtime was never requested");
    const bad = logs.filter((l) => /Content Security Policy|violates/i.test(l) && !/Evaluating a string|inline script/.test(l));
    if (bad.length) throw new Error("CSP: " + bad[0]);
  });

  await check("later visit skips the download (Cache Storage)", async () => {
    const names = await page.evaluate(async () => (await caches.keys()));
    if (!names.some((n) => n.startsWith("comebye-stt-"))) throw new Error("no cache: " + names);
    reqs.length = 0;
    await openAlpha(page, baseInst); // same page, navigated: the worker restarts
    await page.reload({ waitUntil: "load" });
    await page.waitForSelector("#prompt-text", { state: "visible" });
    await mic(page).click();
    // No offer this time: it listens at once.
    await until(page, (ks) => ks.includes(document.querySelector(".stt-status:not([hidden])")?.dataset.kind), ["listening"], { timeout: 15000 });
    await page.waitForTimeout(3000);
    const tStop2 = Date.now();
    await mic(page).click(); // stop, which waits for the model
    await until(page, (ks) => ks.includes(document.querySelector(".stt-status:not([hidden])")?.dataset.kind), ["note"], { timeout: 120000 });
    console.log(`     later visit: stop to text (model start from the cache + transcribing 3 s) ${Date.now() - tStop2} ms`);
    const heavy = reqs.filter((r) => /\.(onnx|wasm)$/.test(r.url));
    if (heavy.length) throw new Error("fetched again: " + heavy.map((r) => r.url.split("/").pop()).join(", "));
  }, page);

  await check("To do answer box", async () => {
    const id = await page.evaluate(async () => (await fetch("/api/todos", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ question: "stt check: which cache?", kind: "decision", no_wait: true }) }).then((x) => x.json())).todo.id);
    await page.goto(baseInst + "/#/todo", { waitUntil: "load" });
    await until(page, () => state.view === "todo", null);
    await page.evaluate(() => { todos.loadedAt = 0; return loadTodos(); });
    const row = page.locator(`#p-todo .td-item[data-id="${id}"] .td-row`);
    await row.click();
    const box = page.locator(`#p-todo .td-item[data-id="${id}"] .td-text`);
    await box.click();
    await box.fill("My answer:");
    const m = mic(page, "todo:" + id);
    await m.click();
    await until(page, (ks) => ks.includes(document.querySelector(".stt-status:not([hidden])")?.dataset.kind), ["listening"], { timeout: 15000 });
    await page.screenshot({ path: `${out}/stt-todo-listening.png` });
    // A repaint of the list while recording must keep the state.
    await page.evaluate(() => renderTodos(true));
    if ((await mic(page, "todo:" + id).getAttribute("data-state")) !== "listening") throw new Error("lost the listening state on a repaint");
    await page.waitForTimeout(12500);
    await mic(page, "todo:" + id).click();
    await until(page, (ks) => ks.includes(document.querySelector(".stt-status:not([hidden])")?.dataset.kind), ["note"], { timeout: 120000 });
    const v = await page.locator(`#p-todo .td-item[data-id="${id}"] .td-text`).inputValue();
    if (!v.startsWith("My answer:") || !words.every((w) => v.toLowerCase().includes(w))) throw new Error("answer box: " + v);
    const t = await page.evaluate((i) => fetch("/api/todos").then((x) => x.json()).then((d) => (d.items || d.todos || []).find((x) => x.id === i)?.state), id);
    if (t && t !== "open") throw new Error("the item was answered: " + t);
    await page.screenshot({ path: `${out}/stt-todo-inserted.png` });
    await page.evaluate((i) => fetch("/api/todos/" + i + "/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }), id);
  }, page);

  await check("shortcut and Esc", async () => {
    await openAlpha(page, baseInst);
    await page.locator("#prompt-text").click();
    await page.keyboard.press("Control+Shift+Space");
    await until(page, (ks) => ks.includes(document.querySelector(".stt-status:not([hidden])")?.dataset.kind), ["listening"], { timeout: 15000 });
    await page.keyboard.press("Escape");
    await until(page, () => /Cancelled/.test(document.querySelector(".stt-status:not([hidden])")?.innerText || ""), null, { timeout: 5000 });
    if (await mic(page).getAttribute("data-state") !== "idle") throw new Error("not idle after Esc");
  }, page);
  await b.close();
}

const bad = results.filter((r) => !r[1]);
console.log(`\n${results.length - bad.length}/${results.length} voice checks passed`);
fs.writeFileSync(`${out}/stt-results.json`, JSON.stringify(results, null, 1));
process.exit(bad.length ? 1 : 0);
