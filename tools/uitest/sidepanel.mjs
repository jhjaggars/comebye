// The Doc and Diff side panels beside an agent's chat, against a real server
// on its own fake herdr whose agent "panel" works in a real git worktree
// (a temp HOME, under a dot-directory like ~/.worktrees). Nothing is mocked:
// the panel reads the files and git through the hub, the agent declares its
// document with the real `comebye show`, and the checks edit the files while
// the panel is open.
//
//   node sidepanel.mjs BASE OUT_DIR      (env: PANEL_HOME, COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME)
//
// Each check runs at 1440 (a split beside the chat) and at 390 (a bottom sheet).
import { launch } from "./lib.mjs";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";

const [base, out] = process.argv.slice(2);
const home = process.env.PANEL_HOME, bin = process.env.COMEBYE_BIN, stateDir = process.env.COMEBYE_STATE_DIR;
if (!home || !bin || !stateDir) { console.error("sidepanel.mjs: set PANEL_HOME, COMEBYE_BIN and COMEBYE_STATE_DIR"); process.exit(2); }
const dark = process.env.SCHEME !== "light";
const wt = path.join(home, ".worktrees", "proj", "feat");
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

// ---- the worktree: a feature branch one commit past main, with every kind of change ----
const git = (...a) => execFileSync("git", ["-C", wt, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", ...a], { stdio: "pipe" });
const w = (rel, text) => { const p = path.join(wt, rel); fs.mkdirSync(path.dirname(p), { recursive: true }); fs.writeFileSync(p, text); };
const SECTIONS = ["1. Goal", "2. Plan", "3. Scope", "4. Design", "5. Risks", "6. Testing", "7. Rollout", "8. Metrics", "9. Staffing", "10. Timeline", "11. Open questions", "12. Appendix"];
const para = (i) => `Paragraph ${i} of the drill document. `.repeat(3) + "The cluster keeps serving writes while one node is down, and heals by itself once the node is back; nobody touches it. ".repeat(4) + "\n\n";
let plan = "# Garage failover drill\n\n*Draft 4 · owner: platform*\n\n";
SECTIONS.forEach((s, i) => {
  plan += `## ${s}\n\n` + Array.from({ length: 12 }, (_, k) => para(i * 100 + k)).join("");
  if (i === 1) plan += "| Step | Who | Expected |\n|---|---|---|\n| Stop `garage-2` | drill script | writes keep working |\n| Write 1000 objects | load script | 0 errors |\n\n```sh\n./drill.sh --node garage-2 --objects 1000 --a-very-long-flag-that-goes-on-and-on-and-on-and-on-and-on-and-on --and-another-one\n```\n\n";
  if (i === 3) plan += "https://example.com/" + "a-very-long-unbroken-link-segment/".repeat(10) + "\n\n";
});
fs.mkdirSync(wt, { recursive: true });
git("init", "-q", "-b", "main");
w("a.go", "package a\n\nfunc A() {}\n");
w("README.md", "# proj\n\nRun a drill by hand.\n");
w("docs/plan.md", "# stub\n");
git("add", "."); git("commit", "-q", "-m", "base");
git("checkout", "-q", "-b", "feat");
w("docs/plan.md", plan);
w("c.go", "package c\n\nfunc C() int { return 3 }\n");
git("add", "."); git("commit", "-q", "-m", "add c and the plan");
w("a.go", "package a\n\nfunc A() { println(\"" + "x".repeat(160) + "\") }\n");           // unstaged, with a long line
w("README.md", "# proj\n\nRun a drill with ./drill.sh NODE.\n"); git("add", "README.md"); // staged
w("drill.sh", "#!/usr/bin/env bash\nset -euo pipefail\nAWS_KEY=AKIAABCDEFGHIJKLMNOP\necho drill\n"); // untracked, with a secret
w(".env", "DB_PASSWORD=supersecretvalue\n");                                                      // untracked, withheld
fs.mkdirSync(path.join(wt, ".github"), { recursive: true }); w(".github/notes.md", "# hidden\n");
const PLAN_SIZE = fs.statSync(path.join(wt, "docs/plan.md")).size;

const cli = (...args) => execFileSync(bin, args, { cwd: wt, env: { PATH: "/usr/bin:/bin", HOME: home, COMEBYE_URL: base, COMEBYE_STATE_DIR: stateDir, HERDR_PANE_ID: "w7:p1" }, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const sel = '#agents .agent[data-id$="w7:p1"]';

await check(`the real CLI declares the document (${(PLAN_SIZE / 1024).toFixed(0)} KB) and refuses ones the panel would not open`, async () => {
  const o = cli("show", "docs/plan.md", "--section", "7. Rollout");
  if (!/now opens .*docs\/plan\.md/.test(o)) throw new Error("show said: " + o);
  for (const bad of ["a.go", ".github/notes.md", "/etc/passwd"]) {
    let failed = false;
    try { cli("show", bad); } catch (e) { failed = true; if (!/(only \.md|hidden|not one|not read)/.test(String(e.stderr))) throw new Error(bad + ": " + e.stderr); }
    if (!failed) throw new Error("show accepted " + bad);
  }
  const w2 = cli("show", "docs/plan.md", "--section", "7. Rollout");
  if (!w2) throw new Error("the pin was lost after refusals");
});

for (const width of [1440, 390]) {
  const desk = width > 500;
  const { browser, page } = await launch({ width, height: desk ? 900 : 844, dark });
  const consoleErrs = [];
  page.on("console", (m) => { if (m.type() === "error") consoleErrs.push(m.text()); });
  page.on("pageerror", (e) => consoleErrs.push(String(e)));
  const tag = `${width}`;
  const noOverflow = async (what) => {
    const g = await page.evaluate(() => { const b = document.querySelector("#sp-body"); return { page: document.documentElement.scrollWidth - innerWidth, body: b ? b.scrollWidth - b.clientWidth : 0 }; });
    if (g.page > 0 || g.body > 1) throw new Error(`${what}: horizontal overflow ${JSON.stringify(g)}`);
  };
  const body = () => page.locator("#sp-body");
  const openAgent = async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector(sel, { timeout: 15000 });
    await page.locator(sel).first().click();
    await page.waitForSelector("#detail", { state: "visible" });
  };
  const inView = (q) => page.evaluate((s) => { const el = document.querySelector(s), b = document.querySelector("#sp-body"); if (!el) return false; const r = el.getBoundingClientRect(), br = b.getBoundingClientRect(); return r.top >= br.top - 2 && r.top < br.bottom - 20; }, q);

  await check(`${tag}: the panel is closed until asked; the button (and D on a keyboard) opens it beside the chat`, async () => {
    await openAgent();
    if (await page.locator("#sp").isVisible()) throw new Error("open by default");
    if (!(await page.locator("#sp-btn").isVisible())) throw new Error("no button");
    if (desk) { await page.keyboard.press("d"); } else { await page.locator("#sp-btn").click(); }
    await page.waitForSelector("#sp", { state: "visible" });
    await page.waitForSelector("#sp-doc h2", { timeout: 10000 });
    const g = await page.evaluate(() => { const r = (s) => document.querySelector(s).getBoundingClientRect(); return { sp: r("#sp"), main: r("#detail main"), vh: innerHeight, vw: innerWidth }; });
    if (desk && !(g.sp.left >= g.main.right - 2 && g.main.width > 300 && g.sp.width > 400)) throw new Error("not a split: " + JSON.stringify(g));
    if (!desk && !(g.sp.bottom >= g.vh - 1 && g.sp.width >= g.vw - 1 && g.sp.height > g.vh * 0.6)) throw new Error("not a bottom sheet: " + JSON.stringify(g));
    await noOverflow("doc");
  });

  await check(`${tag}: the pinned ${(PLAN_SIZE / 1024).toFixed(0)} KB document is rendered Markdown (sanitized), opened at the section the agent cites`, async () => {
    const n = await page.locator("#sp-doc h2").count();
    if (n < 12) throw new Error(n + " h2 headings");
    if (await page.locator("#sp-doc .md-plain").count()) throw new Error("fell back to plain text");
    if (await page.locator("#sp-doc script, #sp-doc img").count()) throw new Error("unsafe elements");
    if (!/7\. Rollout/.test(await page.locator("#sp .cite").innerText())) throw new Error("no cite bar: " + await page.locator("#sp .cite").innerText());
    await page.waitForFunction(() => { const h = document.querySelector("#sp-doc h2.cited"), b = document.querySelector("#sp-body"); if (!h) return false; const r = h.getBoundingClientRect(), br = b.getBoundingClientRect(); return r.top >= br.top - 2 && r.top < br.top + br.height / 2; }, null, { timeout: 5000 });
    if (!/^7\. Rollout/.test(await page.locator("#sp-doc h2.cited").first().innerText())) throw new Error("cited the wrong heading");
    const ids = await page.evaluate(() => [...document.querySelectorAll("#sp-doc h2")].map((h) => h.id));
    if (ids.some((i) => !i.startsWith("sp-h-")) || new Set(ids).size !== ids.length) throw new Error("heading anchors: " + ids.join());
    await noOverflow("doc");
    await page.screenshot({ path: `${out}/sidepanel-doc-${tag}.png` });
  });

  await check(`${tag}: a section jump from the list of sections, and the agent's "Show", land on the heading`, async () => {
    await page.locator("#sp-toc").selectOption({ label: "  2. Plan" });
    await page.waitForFunction(() => { const h = [...document.querySelectorAll("#sp-doc h2")].find((x) => /^2\. Plan/.test(x.textContent)), b = document.querySelector("#sp-body"); if (!h) return false; const r = h.getBoundingClientRect(), br = b.getBoundingClientRect(); return r.top >= br.top - 2 && r.top < br.top + 120; }, null, { timeout: 5000 });
    await page.locator('#sp .cite [data-act="cite"]').click();
    await page.waitForFunction(() => { const h = document.querySelector("#sp-doc h2.cited"), b = document.querySelector("#sp-body"); const r = h.getBoundingClientRect(), br = b.getBoundingClientRect(); return r.top >= br.top - 2 && r.top < br.top + br.height / 2; }, null, { timeout: 5000 });
    // a wide table and code block scroll inside their own box
    await noOverflow("doc after jumps");
  });

  await check(`${tag}: a new "comebye show" moves the open panel to the new section within seconds`, async () => {
    cli("show", "docs/plan.md", "--section", "2. Plan");
    await page.waitForFunction(() => /2\. Plan/.test(document.querySelector("#sp .cite")?.textContent || "") && /^2\. Plan/.test(document.querySelector("#sp-doc h2.cited")?.textContent || ""), null, { timeout: 9000 });
    cli("show", "docs/plan.md", "--section", "7. Rollout");
    await page.waitForFunction(() => /^7\. Rollout/.test(document.querySelector("#sp-doc h2.cited")?.textContent || ""), null, { timeout: 9000 });
  });

  await check(`${tag}: editing the document on disk updates the open panel (live), keeping the reader's place`, async () => {
    await page.evaluate(() => { document.querySelector("#sp-body").scrollTop += 300; });
    const before = await page.evaluate(() => document.querySelector("#sp-body").scrollTop);
    fs.appendFileSync(path.join(wt, "docs/plan.md"), "\n## 13. Added while you read\n\nlive-doc-marker\n");
    await page.waitForFunction(() => /live-doc-marker/.test(document.querySelector("#sp-body")?.textContent || ""), null, { timeout: 9000 });
    const after = await page.evaluate(() => document.querySelector("#sp-body").scrollTop);
    if (Math.abs(after - before) > 40) throw new Error(`the page jumped: ${before} -> ${after}`);
  });

  await check(`${tag}: the user can pick another file or type a path, under the same rules; refused ones say why`, async () => {
    await page.locator("#sp-path").fill("README.md");
    await page.locator("#sp-path-form button").click();
    await page.waitForFunction(() => /Run a drill with/.test(document.querySelector("#sp-body")?.textContent || ""), null, { timeout: 8000 });
    for (const [p, re] of [["../../../../etc/passwd", /not one this panel may read|neither in/], [".github/notes.md", /hidden/], ["a.go", /only \.md/]]) {
      await page.locator("#sp-path").fill(p);
      await page.locator("#sp-path-form button").click();
      await page.waitForFunction((src) => new RegExp(src).test(document.querySelector("#sp-body")?.textContent || ""), re.source, { timeout: 8000 });
    }
    // back to the agent's document
    await page.locator("#sp-file").selectOption({ index: 0 });
    await page.waitForFunction(() => document.querySelectorAll("#sp-doc h2").length > 10, null, { timeout: 8000 });
    await noOverflow("doc after picks");
  });

  await check(`${tag}: the Diff tab lists unstaged, staged, untracked and committed files; secrets are hidden, .env is withheld`, async () => {
    await page.locator("#sp-tab-diff").click();
    await page.waitForSelector("#sp-body details.df", { timeout: 10000 });
    // More than 8 files start collapsed, as an outline; open them to read the placeholders.
    if (/Expand all/.test(await page.locator("#sp-fold").innerText())) await page.locator("#sp-fold").click();
    const txt = await body().innerText();
    for (const p of ["a.go", "README.md", "drill.sh", "c.go", "docs/plan.md"]) if (!txt.includes(p)) throw new Error("missing " + p + " in:\n" + txt.slice(0, 600));
    for (const h of ["Unstaged", "Staged", "Untracked", "Commits since main"]) if (!new RegExp(h, "i").test(txt)) throw new Error("no section " + h);
    if (/AKIAABCDEFGHIJKLMNOP|supersecretvalue/.test(await body().innerHTML())) throw new Error("a secret reached the page");
    if (!/Contents withheld \(environment file\): 1 changed line/.test(txt)) throw new Error("no withheld placeholder:\n" + txt.slice(0, 800));
    if (!/«redacted»/.test(txt)) throw new Error("no redaction mark");
    if (!/2 files|\d+ files/.test(await page.locator("#sp-bar").innerText())) throw new Error("no totals");
    if (!/\d+/.test(await page.locator("#sp-n").innerText())) throw new Error("no badge");
    await noOverflow("diff");
    await page.screenshot({ path: `${out}/sidepanel-diff-${tag}.png` });
  });

  await check(`${tag}: editing a file shows in the open Diff within seconds`, async () => {
    fs.appendFileSync(path.join(wt, "c.go"), "// live-diff-marker\n");
    await page.waitForFunction(() => /live-diff-marker/.test(document.querySelector("#sp-body")?.textContent || ""), null, { timeout: 9000 });
    w("fresh-untracked.txt", "created while the panel was open\n");
    await page.waitForFunction(() => /fresh-untracked\.txt/.test(document.querySelector("#sp-body")?.textContent || ""), null, { timeout: 9000 });
  });

  await check(`${tag}: a file skipped for size says why and a Load button fetches it; the change set over the total cap does the same, hand-written files first`, async () => {
    const line = "0123456789abcdef0123456789abcdef0123\n";
    w("bigfile.txt", line.repeat(2000));                                    // ~74 KB: over the per-file cap
    for (let i = 0; i < 24; i++) w(`vendor/pkg/g${String(i).padStart(2, "0")}.txt`, line.repeat(1000)); // ~37 KB each, ~880 KB: over the total
    w("late-handwritten.go", "package late\n\nvar Late = 1 // hand-written-marker\n");
    try {
      await page.waitForFunction(() => /Not loaded: the change set is large/.test(document.querySelector("#sp-body")?.textContent || ""), null, { timeout: 12000 });
      const big = page.locator('#sp-body details.df[data-key$="|bigfile.txt"]');
      if (!(await big.evaluate((e) => e.open))) await big.locator("summary").click();
      const txt = await body().innerText();
      if (!/Too big for the panel \(\d+ KB, 2000 lines\)/.test(txt)) throw new Error("no per-file reason:\n" + txt.slice(0, 600));
      if (!/files? not loaded: the change set is large \(over 768 KB\)/.test(txt)) throw new Error("no note");
      if (/Too big to show here/.test(txt)) throw new Error("the old vague message is back");
      // the hand-written file kept its text although it is listed after the vendored ones
      const late = page.locator('#sp-body details.df[data-key$="|late-handwritten.go"]');
      if (!(await late.locator(".ln.add").count())) throw new Error("the hand-written file lost the budget to generated ones");
      // vendored files start collapsed
      const v = page.locator('#sp-body details.df[data-key$="|vendor/pkg/g23.txt"]');
      if (await v.evaluate((e) => e.open)) throw new Error("a generated file starts open");
      await v.locator("summary").click();
      const btn = v.locator("button.ld");
      if (!(await btn.isVisible())) throw new Error("no Load diff button");
      await noOverflow("load placeholder");
      await page.screenshot({ path: `${out}/sidepanel-load-before-${tag}.png` });
      if (desk) { await page.setViewportSize({ width: 1280, height: 900 }); await page.screenshot({ path: `${out}/sidepanel-load-before-1280.png` }); await page.setViewportSize({ width, height: 900 }); }
      await btn.click();
      await v.locator(".ln.add").first().waitFor({ timeout: 8000 });
      if ((await v.locator(".ln.add").count()) !== 1000) throw new Error("loaded " + (await v.locator(".ln.add").count()) + " lines");
      await big.locator("button.ld").click();
      await big.locator(".ln.add").first().waitFor({ timeout: 8000 });
      await noOverflow("loaded diff");
      await page.screenshot({ path: `${out}/sidepanel-load-after-${tag}.png` });
      if (desk) { await page.setViewportSize({ width: 1280, height: 900 }); await page.screenshot({ path: `${out}/sidepanel-load-after-1280.png` }); await page.setViewportSize({ width, height: 900 }); }
    } finally {
      fs.rmSync(path.join(wt, "vendor"), { recursive: true, force: true });
      for (const f of ["bigfile.txt", "late-handwritten.go"]) fs.rmSync(path.join(wt, f), { force: true });
    }
    await page.waitForFunction(() => !/vendor\/pkg|bigfile/.test(document.querySelector("#sp-body")?.textContent || ""), null, { timeout: 12000 });
  });

  await check(`${tag}: files collapse one by one and all at once, and the list of files jumps to one`, async () => {
    if (/Expand all/.test(await page.locator("#sp-fold").innerText())) await page.locator("#sp-fold").click();
    await page.locator("#sp-fold").click();
    if (await page.locator("#sp-body details.df[open]").count()) throw new Error("not all collapsed");
    if (!/Expand all/.test(await page.locator("#sp-fold").innerText())) throw new Error("button label");
    // one file by its own header
    await page.locator('#sp-body details.df summary').first().click();
    if ((await page.locator("#sp-body details.df[open]").count()) !== 1) throw new Error("opening one file");
    await page.locator('#sp-body details.df summary').first().click();
    await page.locator("#sp-jump").selectOption({ label: "c.go" });
    await page.waitForFunction(() => { const d = [...document.querySelectorAll("#sp-body details.df")].find((x) => x.dataset.key.endsWith("|c.go")); return d && d.open; }, null, { timeout: 4000 });
    if (!(await inView('#sp-body details.df[open] .p[title="c.go"]'))) throw new Error("c.go is not in view after the jump");
    if (/Collapse all/.test(await page.locator("#sp-fold").innerText())) await page.locator("#sp-fold").click();
    await page.locator("#sp-fold").click(); // expand all
    if (await page.locator("#sp-body details.df:not([open])").count()) throw new Error("not all expanded");
    // a long line scrolls inside its box, not the page
    await noOverflow("diff after folding");
  });

  if (desk) {
    await check(`${tag}: the divider resizes the split and the width is remembered for this agent`, async () => {
      await page.locator("#sp-tab-doc").click();
      const before = (await page.locator("#sp").boundingBox()).width;
      const gr = await page.locator("#sp-grip").boundingBox();
      await page.mouse.move(gr.x + gr.width / 2, gr.y + 200);
      await page.mouse.down();
      await page.mouse.move(gr.x - 260, gr.y + 200, { steps: 8 });
      await page.mouse.up();
      const after = (await page.locator("#sp").boundingBox()).width;
      if (after < before + 150) throw new Error(`width ${before} -> ${after}`);
      await page.reload({ waitUntil: "load" });
      await page.waitForSelector("#sp", { state: "visible", timeout: 10000 });
      const again = (await page.locator("#sp").boundingBox()).width;
      if (Math.abs(again - after) > 4) throw new Error(`after reload ${after} -> ${again}`);
      if (!(await page.locator("#sp-tab-doc").getAttribute("aria-selected") === "true")) throw new Error("tab not remembered");
      await page.screenshot({ path: `${out}/sidepanel-resized-${tag}.png` });
      // keyboard: the divider moves with arrows
      await page.locator("#sp-grip").focus();
      await page.keyboard.press("ArrowRight");
      const narrower = (await page.locator("#sp").boundingBox()).width;
      if (!(narrower < again)) throw new Error("arrow did not resize");
    });
    await check(`${tag}: D closes the panel and the chat gets its width back; the choice is remembered`, async () => {
      await page.locator("#prompt-text").evaluate((e) => e.blur());
      await page.keyboard.press("d");
      await page.waitForSelector("#sp", { state: "hidden" });
      const main = (await page.locator("#detail main").boundingBox()).width;
      const det = (await page.locator("#detail").boundingBox()).width;
      if (main < det - 4) throw new Error(`chat ${main} of ${det}`);
      await page.reload({ waitUntil: "load" });
      await page.waitForSelector(sel); await page.locator(sel).first().click();
      await page.waitForSelector("#detail", { state: "visible" });
      if (await page.locator("#sp").isVisible()) throw new Error("reopened by itself");
      await page.keyboard.press("d");
      await page.waitForSelector("#sp", { state: "visible" });
    });
  } else {
    await check(`${tag}: the sheet has Doc and Diff tabs and closes with ×; the page behind does not scroll sideways`, async () => {
      await page.locator("#sp-tab-diff").click();
      if (await page.locator("#sp-tab-diff").getAttribute("aria-selected") !== "true") throw new Error("tab");
      await page.screenshot({ path: `${out}/sidepanel-sheet-diff-${tag}.png` });
      await page.locator("#sp-tab-doc").click();
      await page.locator("#sp .sp-x").click();
      await page.waitForSelector("#sp", { state: "hidden" });
      if (await page.locator("#sp-btn").getAttribute("aria-pressed") !== "false") throw new Error("button state");
      await noOverflow("chat");
      await page.locator("#sp-btn").click();
      await page.waitForSelector("#sp", { state: "visible" });
    });
  }

  await check(`${tag}: the panel's own controls are reachable by keyboard and labelled`, async () => {
    const names = await page.evaluate(() => [...document.querySelectorAll("#sp button, #sp select, #sp input, #sp [role=separator]")].filter((e) => e.offsetParent !== null).map((e) => (e.getAttribute("aria-label") || e.textContent || "").trim() || e.id));
    if (names.some((n) => !n)) throw new Error("an unlabelled control: " + names.join("|"));
    const small = await page.evaluate(() => [...document.querySelectorAll("#sp .sp-tab, #sp .sp-x, #sp-btn")].filter((e) => e.offsetParent !== null).map((e) => { const r = e.getBoundingClientRect(); return Math.min(r.width, r.height); }));
    if (small.some((s) => s < 40)) throw new Error("a touch target under 40px: " + small.join());
  });

  await check(`${tag}: a click on the Diff tab that lands while a Doc load is in flight is not dropped until the next poll`, async () => {
    await page.locator("#sp-tab-doc").click();
    await page.waitForSelector("#sp-doc", { timeout: 10000 });
    const held = [];
    await page.route(/\/file(\?|$)/, (r) => held.push(r)); // hold the Doc loads: one is then in flight
    fs.appendFileSync(path.join(wt, "c.go"), "// race-marker\n");
    const t0 = Date.now();
    while (!held.length && Date.now() - t0 < 6000) await sleep(50); // the Doc poll
    if (!held.length) throw new Error("no Doc load came in 6 s");
    await page.locator("#sp-tab-diff").click(); // the click lands during that load
    const t1 = Date.now();
    await page.unroute(/\/file(\?|$)/);
    for (const r of held) await r.continue();
    await page.waitForFunction(() => /race-marker/.test(document.querySelector("#sp-body")?.textContent || ""), null, { timeout: 9000 });
    const ms = Date.now() - t1;
    // Dropped, it waited for the next poll (up to 3 s); queued, it runs when the load ends.
    if (ms > 800) throw new Error(`the Diff took ${ms} ms to load after the Doc load ended`);
  });

  await check(`${tag}: no console errors`, async () => {
    const bad = consoleErrs.filter((e) => !/favicon|Failed to load resource: the server responded with a status of (403|404|415|413)/.test(e));
    if (bad.length) throw new Error(bad.join("\n"));
  });
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`sidepanel: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
