// Pages: Back returns to where you came from. Arriving at a page from another screen (To do, an agent's chat), Pages' own
// back button and the browser's history.back() both return to that screen; from the gallery they return to the gallery; a
// cold deep link has nothing in the app behind it, so the button falls back to the gallery. Switching versions adds no entry
// Back has to step through, and deleting a page you came to by link returns to where you came from.
//
// A link is clicked the way a report's or an item's link is: an <a href="#/pages/..."> in the app, handled by the app.
//
//   (a part of pagesjourney.mjs) pageback BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME, WIDTHS)
//
// A part of pagesjourney.mjs (journey.mjs): the script as it was, as the body of run(process); `process` is the one the
// journey gives it (its own argv and env), and process.exit ends the part.
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

export async function run(process) {
const [base, out] = process.argv.slice(2);
const bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!bin || !stateDir) { console.error("pageback.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const work = fs.mkdtempSync(path.join(os.tmpdir(), "pageback-ui-"));
const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w5:p1" };
let seq = 0;
const publish = (title, key) => {
  const f = path.join(work, `p${++seq}.html`);
  fs.writeFileSync(f, `<!doctype html><title>${title}</title><h1>${title}</h1>`);
  return execFileSync(bin, ["publish", f, "--title", title, ...(key ? ["--key", key] : [])], { env, encoding: "utf8" }).match(/pg_[0-9a-f]{10}/)[0];
};
const J = { "Content-Type": "application/json" };
const live = (await (await fetch(base + "/api/agents")).json()).agents;
if (!live.length) { console.error("pageback.mjs: the server has no agents"); process.exit(2); }
const AG = live[0];

async function until(page, fn, arg, opts = {}) {
  const end = Date.now() + (opts.timeout || 8000);
  for (;;) {
    if (await page.evaluate(fn, arg)) return;
    if (Date.now() > end) throw new Error("timed out waiting for " + String(fn).slice(0, 120));
    await page.waitForTimeout(50);
  }
}
const results = [];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
const hashIs = (page, h) => until(page, (h) => location.hash === h, h);
const hashPre = (page, h) => until(page, (h) => location.hash.startsWith(h), h);
// Follow a link to the page the way a report's link does.
const follow = (page, id, ver) => page.evaluate(([id, ver]) => {
  const a = document.createElement("a");
  a.href = "#/pages/" + id + (ver ? "/" + ver : ""); a.textContent = "link"; a.id = "pb-link";
  a.style.cssText = "position:fixed;top:0;left:0;z-index:99999;padding:8px;background:#fff";
  document.body.append(a);
}, [id, ver]).then(() => page.click("#pb-link")).then(() => page.evaluate(() => document.getElementById("pb-link")?.remove()));
const pageShown = (page, id) => until(page, (id) => !document.getElementById("pages").hidden && location.hash.startsWith("#/pages/" + id), id);

for (const width of (process.env.WIDTHS || "390,1280").split(",").map(Number)) {
  const phone = width < 500, tag = `${width}px ${dark ? "dark" : "light"}`;
  const A = publish(`Back A ${width}`), B = publish(`Back B ${width}`), D = publish(`Back D ${width}`);
  const open = async () => {
    const { browser, page } = await launch({ width, height: phone ? 844 : 800, dark, bypassCSP: false });
    page.setDefaultTimeout(8000);
    return { browser, page };
  };
  const ready = async (page, hash) => { await page.goto(base + "/" + (hash || ""), { waitUntil: "load" }); await until(page, () => typeof state !== "undefined" && state.agents.length > 0); };
  const agentHash = "#/agent/" + encodeURIComponent(AG.pane_id);

  await check(`${tag}: from an agent's chat, the back button returns to that agent`, async () => {
    const { browser, page } = await open();
    try {
      await ready(page);
      await page.evaluate((h) => { location.hash = h; }, agentHash);
      await hashIs(page, agentHash); await until(page, (id) => state.selected === id, AG.id);
      await follow(page, A);
      await pageShown(page, A);
      await page.locator("#pgs-back, #pgs-vback").locator("visible=true").first().click();
      await hashIs(page, agentHash);
      await until(page, (id) => state.selected === id && document.getElementById("pages").hidden, AG.id);
    } finally { await browser.close(); }
  });

  await check(`${tag}: from an agent's chat, history.back() returns to that agent and the page leaves`, async () => {
    const { browser, page } = await open();
    try {
      await ready(page);
      await page.evaluate((h) => { location.hash = h; }, agentHash);
      await until(page, (id) => state.selected === id, AG.id);
      await follow(page, A);
      await pageShown(page, A);
      await page.evaluate(() => history.back());
      await hashIs(page, agentHash);
      await until(page, (id) => state.selected === id && document.getElementById("pages").hidden, AG.id);
    } finally { await browser.close(); }
  });

  if (!phone) {
    await check(`${tag}: from the To do screen, back button and history.back() both return to it`, async () => {
      const { browser, page } = await open();
      try {
        await ready(page, "#/todo");
        await until(page, () => state.view === "todo");
        for (const how of ["button", "history"]) {
          await follow(page, A);
          await pageShown(page, A);
          if (how === "button") await page.locator("#pgs-back, #pgs-vback").locator("visible=true").first().click(); else await page.evaluate(() => history.back());
          await hashIs(page, "#/todo");
          await until(page, () => state.view === "todo" && document.getElementById("pages").hidden);
        }
      } finally { await browser.close(); }
    });
  }

  await check(`${tag}: from the gallery, back returns to the gallery without growing the history`, async () => {
    const { browser, page } = await open();
    try {
      await ready(page, "#/pages");
      await page.waitForSelector(`.pgs-card[data-id="${A}"]`);
      const len0 = await page.evaluate(() => history.length);
      await page.click(`.pgs-card[data-id="${A}"]`);
      await pageShown(page, A);
      await page.locator("#pgs-back, #pgs-vback").locator("visible=true").first().click();
      await hashIs(page, "#/pages");
      if ((await page.evaluate(() => history.length)) > len0 + 1) throw new Error("history grew by more than the page's own entry");
      await page.click(`.pgs-card[data-id="${B}"]`);
      await pageShown(page, B);
      await page.evaluate(() => history.back());
      await hashIs(page, "#/pages");
    } finally { await browser.close(); }
  });

  await check(`${tag}: a cold deep link: the back button falls back to the gallery`, async () => {
    const { browser, page } = await open();
    try {
      await page.goto(base + "/#/pages/" + A, { waitUntil: "load" });
      await pageShown(page, A);
      await page.locator("#pgs-back, #pgs-vback").locator("visible=true").first().click();
      await hashIs(page, "#/pages");
      await until(page, () => !document.getElementById("pages").hidden && !/^#\/pages\/./.test(location.hash));
    } finally { await browser.close(); }
  });

  await check(`${tag}: a version switch adds no entry: one Back leaves the page for where it came from`, async () => {
    const { browser, page } = await open();
    try {
      // a second version of the page
      const f = path.join(work, "v2.html"); fs.writeFileSync(f, "<!doctype html><title>v2</title><h1>v2</h1>");
      execFileSync(bin, ["publish", f, "--key", "backver" + width, "--title", "Ver " + width], { env, encoding: "utf8" });
      const V = execFileSync(bin, ["publish", f, "--key", "backver" + width, "--title", "Ver " + width], { env, encoding: "utf8" }).match(/pg_[0-9a-f]{10}/)[0];
      await ready(page);
      await page.evaluate((h) => { location.hash = h; }, agentHash);
      await until(page, (id) => state.selected === id, AG.id);
      await follow(page, V);
      await pageShown(page, V);
      await until(page, () => { const s = document.getElementById("pgs-ver"); return s && s.options.length > 1; });
      const len = await page.evaluate(() => history.length);
      await page.evaluate(() => { const s = document.getElementById("pgs-ver"); s.value = s.options[s.options.length - 1].value; s.dispatchEvent(new Event("change", { bubbles: true })); });
      await until(page, (id) => /^#\/pages\/[^/]+\/\d+$/.test(location.hash), V);
      if ((await page.evaluate(() => history.length)) !== len) throw new Error("the switch added a history entry");
      await page.locator("#pgs-back, #pgs-vback").locator("visible=true").first().click();
      await hashIs(page, agentHash);
    } finally { await browser.close(); }
  });

  await check(`${tag}: deleting a page reached by a link returns to where it came from`, async () => {
    const { browser, page } = await open();
    try {
      await ready(page);
      await page.evaluate((h) => { location.hash = h; }, agentHash);
      await until(page, (id) => state.selected === id, AG.id);
      await follow(page, D);
      await pageShown(page, D);
      page.once("dialog", (d) => d.accept());
      await page.click("#pgs-menub");
      await page.click("#pgs-del");
      await hashIs(page, agentHash);
      await until(page, (id) => state.selected === id && document.getElementById("pages").hidden, AG.id);
    } finally { await browser.close(); }
  });
}
fs.rmSync(work, { recursive: true, force: true });
process.exit(results.every(Boolean) ? 0 : 1);
}
