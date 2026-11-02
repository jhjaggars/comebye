#!/usr/bin/env node
// Screenshot and inspect one page of the running app (see dev.sh), so you
// can SEE a UI change: open the PNG with your Read tool (it shows images);
// the printed geometry and DOM are for agents that cannot view images.
//
//   node tools/uitest/shot.mjs ROUTE [options]
//
// ROUTE is a full URL, or a path/hash against the dev server (the URL that
// `dev.sh url` prints): "/", "#/agent/w3:p1", "agent/w3:p1" (shorthand for
// the hash). Open the launch sheet with --click '#btn-launch'.
//
//   --width N            viewport width: 390 (phone, default), 768, 1280...
//   --height N           viewport height (default 844; desktop widths 800)
//   --dark | --light     colour scheme (default: the browser's, light)
//   --click SEL          click an element (repeatable, in order)
//   --wait SEL           wait for an element to be visible (repeatable)
//   --scroll SEL:PX      scroll an element by PX (SEL "window" for the page)
//   --full               full-page screenshot (default: the viewport)
//   --out FILE           PNG path (default: ./shot-<width>-<theme>.png)
//   --inspect SEL        print visibility + bounding box (repeatable)
//   --dom SEL            print the element's outerHTML, trimmed (repeatable)
//   --a11y SEL           print the element's accessibility tree (default body)
//   --base URL           dev server URL (default: $COMEBYE_URL, else the one
//                        dev.sh recorded for this checkout)
//   --settle MS          wait this long after the steps (default 400)
//
// Prints: the PNG path, console errors/warnings, failed requests, and for
// each --inspect selector `visible x y w h` (or `missing`). Exit 1 if the
// page raised an error.
import { launch } from "./lib.mjs";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const argv = process.argv.slice(2);
const opt = { click: [], wait: [], scroll: [], inspect: [], dom: [], a11y: [] };
let route = null;
const MULTI = new Set(["click", "wait", "scroll", "inspect", "dom", "a11y"]);
for (let i = 0; i < argv.length; i++) {
  const a = argv[i];
  if (a === "--dark") opt.dark = true;
  else if (a === "--light") opt.dark = false;
  else if (a === "--full") opt.full = true;
  else if (a.startsWith("--")) {
    const k = a.slice(2);
    const v = argv[++i];
    if (v === undefined) die(`${a} needs a value`);
    if (MULTI.has(k)) opt[k].push(v); else opt[k] = v;
  } else if (route === null) route = a;
  else die(`unexpected argument ${a}`);
}
function die(m) { console.error("shot: " + m); process.exit(2); }
if (route === null) die("usage: shot.mjs ROUTE [--width N] [--dark|--light] [--click SEL] [--wait SEL] [--scroll SEL:PX] [--out FILE] [--inspect SEL] [--dom SEL] (see the header of this file)");

const here = path.dirname(fileURLToPath(import.meta.url));
function recordedBase() {
  try {
    const dir = fs.readFileSync(path.join(here, ".dev-run"), "utf8").trim();
    return fs.readFileSync(path.join(dir, "url"), "utf8").trim();
  } catch { return null; }
}
const base = (opt.base || process.env.COMEBYE_URL || recordedBase() || "").replace(/\/$/, "");
let url;
if (/^https?:\/\//.test(route)) url = route;
else {
  if (!base) die("no dev server: run tools/uitest/dev.sh start, or pass --base URL");
  if (route.startsWith("agent/")) url = base + "/#/" + route;
  else url = base + (route.startsWith("/") || route.startsWith("#") ? (route.startsWith("#") ? "/" + route : route) : "/" + route);
}

const width = Number(opt.width || 390);
const height = Number(opt.height || (width <= 500 ? 844 : 800));
const theme = opt.dark === undefined ? "default" : opt.dark ? "dark" : "light";
const out = opt.out || `shot-${width}-${theme}.png`;

const { browser, page } = await launch({ width, height, dark: opt.dark });
const problems = [];
page.on("console", (m) => { if (m.type() === "error" || m.type() === "warning") problems.push(`console.${m.type()}: ${m.text()}`); });
page.on("pageerror", (e) => problems.push(`pageerror: ${e.message}`));
page.on("requestfailed", (r) => r.failure()?.errorText !== "net::ERR_ABORTED" && problems.push(`requestfailed: ${r.url()} ${r.failure()?.errorText}`));
page.on("response", (r) => { if (r.status() >= 400) problems.push(`http ${r.status()}: ${r.request().method()} ${r.url()}`); });

let code = 0;
try {
  await page.goto(url, { waitUntil: "load" });
  // Wait for the app to draw something before the steps start.
  await page.waitForSelector("#list, #detail", { state: "visible", timeout: 8000 }).catch(() => {});
  // Run the steps in command-line order.
  const order = argv.flatMap((a, i) => (["--click", "--wait", "--scroll"].includes(a) ? [[a.slice(2), argv[i + 1]]] : []));
  for (const [kind, arg] of order) {
    if (kind === "click") await page.locator(arg).first().click({ timeout: 5000 });
    else if (kind === "wait") await page.waitForSelector(arg, { state: "visible", timeout: 8000 });
    else {
      const k = arg.lastIndexOf(":");
      const sel = arg.slice(0, k), px = Number(arg.slice(k + 1));
      await page.evaluate(([sel, px]) => { (sel === "window" ? window : document.querySelector(sel)).scrollBy(0, px); }, [sel, px]);
    }
    await page.waitForTimeout(150);
  }
  await page.waitForTimeout(Number(opt.settle || 400));
  await page.screenshot({ path: out, fullPage: !!opt.full });
  console.log(`screenshot: ${path.resolve(out)} (${width}x${height}, ${theme}) of ${page.url()}`);

  for (const sel of opt.inspect) {
    const r = await page.evaluate((sel) => {
      const els = [...document.querySelectorAll(sel)];
      return els.slice(0, 5).map((e) => {
        const b = e.getBoundingClientRect(), cs = getComputedStyle(e);
        const shown = cs.display !== "none" && cs.visibility !== "hidden" && Number(cs.opacity) > 0 && b.width > 0 && b.height > 0;
        const inView = b.right > 0 && b.bottom > 0 && b.left < innerWidth && b.top < innerHeight;
        return { n: els.length, shown, inView, x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height), text: (e.innerText || e.getAttribute("aria-label") || "").trim().slice(0, 40) };
      });
    }, sel);
    if (!r.length) console.log(`inspect ${sel}: missing`);
    else r.forEach((e, i) => console.log(`inspect ${sel}${r[0].n > 1 ? `[${i}/${e.n}]` : ""}: ${e.shown ? "visible" : "hidden"}${e.shown && !e.inView ? " (off-screen)" : ""} x=${e.x} y=${e.y} w=${e.w} h=${e.h}${e.text ? ` "${e.text.replace(/\s+/g, " ")}"` : ""}`));
  }
  for (const sel of opt.dom) {
    const h = await page.locator(sel).first().evaluate((e) => e.outerHTML).catch(() => null);
    console.log(`dom ${sel}: ${h === null ? "missing" : h.length > 4000 ? h.slice(0, 4000) + "…" : h}`);
  }
  for (const sel of opt.a11y) {
    const t = await page.locator(sel || "body").first().ariaSnapshot().catch((e) => "(" + e.message.split("\n")[0] + ")");
    console.log(`a11y ${sel}:\n${t}`);
  }
} catch (e) {
  console.error("shot: " + String(e.message).split("\n")[0]);
  code = 1;
  await page.screenshot({ path: out }).then(() => console.log(`screenshot (after the failure): ${path.resolve(out)}`)).catch(() => {});
}
if (problems.length) { console.log("problems:"); [...new Set(problems)].forEach((p) => console.log("  " + p)); }
else console.log("problems: none");
if (problems.some((p) => p.startsWith("pageerror") || p.startsWith("console.error"))) code = code || 1;
await browser.close();
process.exit(code);
