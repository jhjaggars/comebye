// Pages: finding the pages that run JavaScript. The hub flags a version whose HTML holds a <script> element; the Pages
// screen shows a "JS" badge on those cards, an "Interactive" chip that toggles the search term has:script, and the term
// itself works in the box alone or with words (the words are searched in the pages' text by the hub). Light and dark, 390 and 1280.
//
//   (a part of pagesjourney.mjs) pagescript BASE OUT_DIR       (env: COMEBYE_BIN, COMEBYE_STATE_DIR)
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
if (!bin || !stateDir) { console.error("pagescript.mjs: set COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
const work = fs.mkdtempSync(path.join(os.tmpdir(), "pagescript-ui-"));
const results = [];
async function check(name, fn) {
  try { await fn(); results.push(true); console.log("ok   " + name); }
  catch (e) { results.push(false); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}
function publish(args, input) {
  const env = { PATH: "/usr/bin:/bin", HOME: work, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w2:p1" };
  return execFileSync(bin, ["publish", ...args], { env, input, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
}
const idOf = (link) => link.match(/pg_[0-9a-f]{10}/)[0];
const ids = {
  inline: idOf(publish(["-", "--kind", "html", "--title", "Quartzchart live"], "<title>Quartzchart live</title><p>quartzword with a chart</p><script>document.title='x'</script>")),
  src: idOf(publish(["-", "--kind", "html", "--title", "Quartz widget"], "<title>Quartz widget</title><p>other text</p><script src=\"/none.js\"></script>")),
  plain: idOf(publish(["-", "--kind", "html", "--title", "Quartz static"], "<title>Quartz static</title><p>quartzword, and the word script in prose</p><noscript>no js</noscript>")),
  md: idOf(publish(["-", "--kind", "md", "--title", "Quartz notes"], "# Quartz notes\n\nquartzword <script>x</script>\n")),
};

const cards = (page) => page.evaluate(() => [...document.querySelectorAll("#pgs-items .pgs-card")].map((c) => c.dataset.id));
const settle = (page) => page.waitForTimeout(700); // the box's text search runs a moment after typing stops

for (const dark of [false, true]) {
  for (const w of [390, 1280]) {
    const tag = `${dark ? "dark" : "light"}-${w}`;
    await check(`${tag}: flag in the API, badge on flagged cards, the chip toggles has:script, the term works alone and with words`, async () => {
      const { browser, page } = await launch({ width: w, dark });
      page.setDefaultTimeout(8000);
      try {
        const list = (await (await fetch(base + "/api/pages")).json()).pages;
        for (const [k, want] of [["inline", true], ["src", true], ["plain", false], ["md", false]]) {
          const p = list.find((x) => x.id === ids[k]);
          if (!p || !!p.script !== want) throw new Error(`api script flag of ${k}: ${p && p.script}`);
        }
        await page.goto(base + "/#/pages", { waitUntil: "load" });
        await page.waitForSelector(".pgs-card");
        // Badge: on the two flagged cards, not on the others.
        for (const [k, want] of [["inline", 1], ["src", 1], ["plain", 0], ["md", 0]]) {
          const n = await page.locator(`.pgs-card[data-id="${ids[k]}"] .pgs-js`).count();
          if (n !== want) throw new Error(`badge on ${k}: ${n}`);
        }
        const chip = page.locator("#pgs-chips .pgs-chip[data-script]");
        if ((await chip.innerText()) !== "Interactive" || (await chip.getAttribute("aria-pressed")) !== "false") throw new Error("chip not offered, or already pressed");
        const total = (await cards(page)).length;
        await page.fill("#pgs-q", "quartz");
        await settle(page);
        const quartz = (await cards(page)).filter((id) => Object.values(ids).includes(id));
        if (quartz.length !== 4) throw new Error("quartz titles: " + quartz.length);
        await page.screenshot({ path: `${out}/pagescript-${tag}-off.png` });
        // The chip adds the term to the box and narrows the list to flagged pages.
        await chip.click();
        await settle(page);
        if ((await page.inputValue("#pgs-q")) !== "has:script quartz" || (await chip.getAttribute("aria-pressed")) !== "true") throw new Error("chip did not put the term in the box: " + (await page.inputValue("#pgs-q")));
        const on = await cards(page);
        if (on.length !== 2 || !on.includes(ids.inline) || !on.includes(ids.src)) throw new Error("has:script quartz shows " + on.length);
        await page.screenshot({ path: `${out}/pagescript-${tag}-on.png` });
        // Words after the term are searched in the page text: only "inline" has the word in its body, as text.
        await page.fill("#pgs-q", "has:script quartzword");
        await settle(page);
        const hits = await page.evaluate(() => [...document.querySelectorAll("#pgs-items .pgs-hit")].map((c) => c.dataset.id));
        if (hits.join() !== ids.inline) throw new Error("has:script quartzword body hits: " + hits);
        // The term alone, typed by hand: the chip follows it.
        await page.fill("#pgs-q", "has:script");
        await settle(page);
        const alone = await cards(page);
        if (!alone.includes(ids.inline) || !alone.includes(ids.src) || alone.includes(ids.plain) || alone.includes(ids.md) || alone.length >= total + 1) throw new Error("has:script alone: " + alone.length);
        if ((await chip.getAttribute("aria-pressed")) !== "true") throw new Error("chip does not follow the typed term");
        // Toggling off clears the term and brings everything back.
        await chip.click();
        await settle(page);
        if ((await page.inputValue("#pgs-q")) !== "" || (await cards(page)).length !== total) throw new Error("chip off did not restore the list");
        const over = await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth);
        if (over) throw new Error("horizontal overflow");
      } finally { await browser.close(); }
    });
  }
}
fs.rmSync(work, { recursive: true, force: true });
process.exit(results.every(Boolean) ? 0 : 1);
}
