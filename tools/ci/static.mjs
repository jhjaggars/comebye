#!/usr/bin/env node
// Static facts about web/'s source, read in well under a second with no browser (the lint job runs it):
//
//   node tools/ci/static.mjs [WEB_DIR]
//
//  1. Every var(--x) with no fallback has a definition: a `--x:` in the CSS (index.html or a script's styles), a
//     setProperty("--x", ...) from a script, or a theme token (themes.js turns each into --name).
//  2. The stacking tokens are in order: backdrop < menu < popup < sheet < screen < scrim < toast (read from :root).
// Exit 1 with the offenders listed.
import fs from "node:fs";
import vm from "node:vm";
import path from "node:path";
import { fileURLToPath } from "node:url";

const web = path.resolve(process.argv[2] || path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "..", "web"));
const files = fs.readdirSync(web).filter((f) => /\.(html|js|css)$/.test(f) && !f.endsWith("_test.go")).sort();
const src = Object.fromEntries(files.map((f) => [f, fs.readFileSync(path.join(web, f), "utf8")]));
const all = Object.values(src).join("\n");

const defined = new Set();
for (const m of all.matchAll(/(--[A-Za-z0-9_-]+)\s*:/g)) defined.add(m[1]);
for (const m of all.matchAll(/setProperty\(\s*["'`](--[A-Za-z0-9_-]+)/g)) defined.add(m[1]);
// A theme's tokens become custom properties named after them (themes.js applies "--" + key).
if (src["themes.js"]) {
  const ctx = { globalThis: null }; ctx.globalThis = ctx; vm.createContext(ctx);
  try {
    vm.runInContext(src["themes.js"], ctx, { filename: "themes.js" });
    for (const t of Object.values(ctx.ComebyeThemes.THEMES || {})) for (const k of Object.keys(t.vars || {})) defined.add("--" + k);
  } catch (e) { console.log("FAIL themes.js does not load: " + e.message); process.exit(1); }
}

const problems = [];
for (const [f, text] of Object.entries(src)) {
  const lines = text.split("\n");
  lines.forEach((line, i) => {
    for (const m of line.matchAll(/var\(\s*(--[A-Za-z0-9_-]+)\s*\)/g)) {
      if (!defined.has(m[1])) problems.push(`${f}:${i + 1}: var(${m[1]}) has no fallback and nothing defines ${m[1]}`);
    }
  });
}

// Stacking order.
const root = (all.match(/:root\s*\{[^}]*--z-raised[^}]*\}/) || [""])[0];
const z = (n) => { const m = root.match(new RegExp("--z-" + n + "\\s*:\\s*(-?\\d+)")); return m ? Number(m[1]) : NaN; };
const order = ["backdrop", "menu", "popup", "sheet", "screen", "scrim", "toast"];
for (let i = 0; i < order.length; i++) if (Number.isNaN(z(order[i]))) problems.push(`:root has no --z-${order[i]}`);
for (let i = 1; i < order.length; i++) {
  if (!(z(order[i]) > z(order[i - 1]))) problems.push(`stacking: --z-${order[i]} (${z(order[i])}) must be above --z-${order[i - 1]} (${z(order[i - 1])}); order is ${order.join(" < ")}`);
}

if (problems.length) { console.log(problems.map((p) => "FAIL " + p).join("\n")); process.exit(1); }
console.log(`ok   ${defined.size} custom properties defined; every var() without a fallback has one; stacking tokens in order`);
