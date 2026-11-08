#!/usr/bin/env node
// WCAG contrast check for every theme in web/themes.js, plus a completeness
// check against the tokens web/index.html uses. No browser, no dependencies:
//
//   node tools/uitest/contrast.mjs [--verbose]
//
// Fails (exit 1) when, in any theme:
//   - text (body, muted, quote/snip, code, chip text on its chip, accent
//     text on the surfaces, text on filled buttons/badges/marks) is under 4.5:1;
//   - a non-text indicator (focus ring, the status-dot accents) is under 3:1
//     against the page and panel backgrounds;
//   - a var(--token) used by index.html is missing from the theme.
// Tokens themes.js nudged to pass (an official palette colour that failed
// AA) are listed under "adjusted" in the output.
import fs from "node:fs";
import vm from "node:vm";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const web = path.join(here, "..", "..", "web");
const ctx = { globalThis: null };
ctx.globalThis = ctx;
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(path.join(web, "themes.js"), "utf8"), ctx, { filename: "themes.js" });
const { THEMES, contrast } = ctx.ComebyeThemes;
const verbose = process.argv.includes("--verbose");

const AA = 4.5, UI = 3;
// [foreground token, background token, minimum, what it is]
const TEXT = [
  ["text", "bg", AA, "body text"], ["text", "panel", AA, "body text on cards"], ["text", "panel2", AA, "body text on raised"],
  ["text", "code-bg", AA, "text on code"], ["code-text", "code-bg", AA, "code text"], ["text", "btn-bg", AA, "button text"],
  ["text", "th-bg", AA, "table header"], ["text", "bubble", AA, "chat bubble"], ["text", "rc-user", AA, "recall bubble"],
  ["text", "rc-out", AA, "recall output"], ["text", "mark-bg", AA, "highlight"], ["text", "blocked-bg", AA, "blocked row"],
  ["muted", "bg", AA, "muted text"], ["muted", "panel", AA, "muted on cards"], ["muted", "panel2", AA, "muted on raised"],
  ["muted", "code-bg", AA, "muted on code"], 
  ["quote", "bg", AA, "quote"], ["quote", "panel", AA, "quote on cards"], ["snip", "panel", AA, "snippet"], ["snip", "rc-out", AA, "snippet on output"],
  ["blocked-name", "blocked-bg", AA, "blocked agent name"], ["rcmark-text", "rcmark-bg", AA, "search highlight"],
  ["blue", "chip-blue", AA, "chip: info/open"], ["purple", "chip-purple", AA, "chip: sub-agent"], ["red", "chip-red", AA, "chip: failed/error"],
  ["green", "chip-green", AA, "chip: ok/answered"], ["purple", "chip-finished", AA, "chip: finished"], ["green", "chip-cand", AA, "chip: candidate"],
  ["orange", "chip-rtr", AA, "chip: ready to remove"], ["teal", "chip-landing", AA, "chip: landing"], ["yellow", "chip-yellow", AA, "chip: draft/warn"],
  ["on-accent", "blue", AA, "send button"], ["on-red", "red", AA, "stop button"], ["on-orange", "orange", AA, "badge"],
  ...["blue", "orange", "green", "purple", "red", "yellow", "teal"].flatMap((c) => [[c, "bg", AA, c + " text"], [c, "panel", AA, c + " text on cards"], [c, "panel2", AA, c + " text on raised"]]),
  // Non-text: the focus ring and the status dots (working=blue, idle=muted, blocked=orange, done=green, error=red).
  ["focus", "bg", UI, "focus ring"], ["focus", "panel", UI, "focus ring on cards"],
  ...["blue", "orange", "green", "red"].flatMap((c) => [[c, "bg", UI, c + " status dot"], [c, "panel", UI, c + " status dot on cards"]]),
  ["muted", "bg", UI, "idle status dot"],
];

// Tokens index.html uses; every theme must define all of them.
const html = fs.readFileSync(path.join(web, "index.html"), "utf8");
const used = new Set([...html.matchAll(/var\(--([a-z0-9-]+)/g)].map((m) => m[1]));
for (const own of ["safe-bottom", "list-w"]) used.delete(own); // layout, not colour

let fails = 0, checks = 0;
for (const t of THEMES) {
  const bad = [];
  for (const [fg, bg, min, what] of TEXT) {
    checks++;
    const f = t.vars[fg], b = t.vars[bg];
    if (!f || !b || !/^#/.test(f) || !/^#/.test(b)) { bad.push(`${what}: token ${!f || !/^#/.test(f) ? fg : bg} missing or not a hex`); continue; }
    const r = contrast(f, b);
    if (r < min) bad.push(`${what}: ${fg} ${f} on ${bg} ${b} = ${r.toFixed(2)} < ${min}`);
  }
  for (const k of used) if (!(k in t.vars)) bad.push(`token --${k} is used by index.html but not defined`);
  const adj = Object.entries(t.adjusted || {});
  console.log(`${bad.length ? "FAIL" : "PASS"}  ${t.name} (${t.id})${adj.length ? `  adjusted: ${adj.map(([k, [a, b]]) => `${k} ${a}->${b}`).join(", ")}` : ""}`);
  bad.forEach((m) => console.log("      " + m));
  if (verbose) console.log(`      body ${contrast(t.vars.text, t.vars.bg).toFixed(1)}  muted ${contrast(t.vars.muted, t.vars.panel).toFixed(1)}  min chip ${Math.min(...TEXT.filter((x) => x[3].startsWith("chip")).map(([f, b]) => contrast(t.vars[f], t.vars[b]))).toFixed(1)}`);
  if (bad.length) fails++;
}
console.log(`${THEMES.length} themes, ${checks} contrast checks, ${used.size} tokens used by index.html: ${fails ? fails + " theme(s) failing" : "all pass"}`);
process.exit(fails ? 1 : 0);
