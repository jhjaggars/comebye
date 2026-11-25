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
//   - text stops reading on a surface while it is pressed (the press layer's 10% --text);
//   - text, or a status dot, stops reading on a gradient stop (--g-attn, --g-blk, --g-off) or muted on a blocked row;
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
  ["muted", "blocked-bg", AA, "muted line on a blocked row"],
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
  // The list column's surface (--nav-bg, from 1100px): text, muted, quote and the accents must read on it too.
  ["text", "nav-bg", AA, "text on nav"], ["muted", "nav-bg", AA, "muted on nav"], ["quote", "nav-bg", AA, "quote on nav"],
  ...["blue", "orange", "green", "purple", "red", "yellow", "teal"].map((c) => [c, "nav-bg", AA, c + " text on nav"]),
  ["focus", "bg", UI, "focus ring"], ["focus", "panel", UI, "focus ring on cards"],
  // The list column: the keyboard focus ring on the nav, and the selected agent row (2px --focus outline over --panel2 on the nav).
  ["focus", "nav-bg", UI, "focus ring on nav"], ["focus", "panel2", UI, "selected row outline on its --panel2 fill"],
  ["scroll-thumb", "bg", UI, "scrollbar thumb"], ["scroll-thumb", "panel", UI, "scrollbar thumb on cards"],
  ...["blue", "orange", "green", "red"].flatMap((c) => [[c, "bg", UI, c + " status dot"], [c, "panel", UI, c + " status dot on cards"]]),
  ["muted", "bg", UI, "idle status dot"],
  // Surfaces > Layered: the tonal scale (--panel and --panel2 are redefined as s1 and s2), the hover and selected tints.
  ...["s1", "s2", "s3", "hov", "sel"].flatMap((b) => [["text", b, AA, "text on " + b], ["muted", b, AA, "muted on " + b], ["quote", b, AA, "quote on " + b], ["snip", b, AA, "snippet on " + b],
    ...["blue", "orange", "green", "purple", "red", "yellow", "teal"].map((c) => [c, b, AA, c + " text on " + b])]),
  ["focus", "s1", UI, "focus ring on s1"], ["focus", "sel", UI, "selected row outline on its --sel fill"], ["focus", "s3", UI, "focus ring on menus"],
  ["scroll-thumb", "s1", UI, "scrollbar thumb on s1"],
  ...["blue", "orange", "green", "red"].map((c) => [c, "s1", UI, c + " status dot on s1"]),
  // Gradients (web/index.html "Gradients"; themes.js gradient()): every stop with something on it. A stop runs to a
  // transparent copy of itself over its base, which is checked above, so the stop is the end that matters.
  //   g-attn: the wash at the top of a card that needs you, under its title, text, muted lines, chips and buttons.
  ...["text", "muted", "quote", "blue", "orange", "green", "purple", "red", "yellow", "teal"].map((c) => [c, "g-attn", AA, c + " text on the attention wash"]),
  //   g-blk: a blocked row's edge glow, 32px under the status dot only (the text starts after it).
  ["orange", "g-blk", UI, "blocked dot on the blocked row's glow"],
  //   g-off: the offline strip's wash, under its muted text, bold text, Retry and its blue (updating) or green (back) dot.
  ["text", "g-off", AA, "text on the offline strip"], ["muted", "g-off", AA, "muted on the offline strip"],
  ["blue", "g-off", UI, "updating dot on the offline strip"], ["green", "g-off", UI, "back dot on the offline strip"],
];
// The scale must rise: the page to s1 and s1 to s2 by at least 1.05, in every theme (s2 to s3 may collapse where muted text leaves no room).
const STEPS = [["bg", "s1", 1.05], ["s1", "s2", 1.05]];
const DEPTH = ["s1", "s2", "s3", "edge", "hi", "hov", "sel", "sh-sm", "g-attn", "g-blk", "g-off"];

// Tokens index.html uses; every theme must define all of them.
const html = fs.readFileSync(path.join(web, "index.html"), "utf8");
const used = new Set([...html.matchAll(/var\(--([a-z0-9-]+)/g)].map((m) => m[1]));
// Tokens index.html declares itself (safe-bottom, list-w, td-list-w...) are layout, not colour.
for (const m of html.matchAll(/(?:^|[;{\s])--([a-z0-9-]+)\s*:/g)) used.delete(m[1]);

// The press layer (index.html "Press layer"): while held (100-300 ms on a tap), a surface gets 10% of --text over it.
// It is a moment, not a resting state, and any layer that shows moves the surface toward the text, so a theme whose text
// only just meets AA (Solarized, Tokyo Night Day) dips under it while held: what sits on a pressed surface must stay 3:1.
const PRESS = 0.1;
const pressed = (base, text) => "#" + [1, 3, 5].map((i) => {
  const b = parseInt(base.slice(i, i + 2), 16), f = parseInt(text.slice(i, i + 2), 16);
  return Math.round(b + (f - b) * PRESS).toString(16).padStart(2, "0");
}).join("");
const PRESSED = [
  ...["bg", "panel", "panel2", "s1", "s2", "s3"].flatMap((b) => [["text", b, UI, "text on pressed " + b], ["muted", b, UI, "muted on pressed " + b]]),
  ["text", "btn-bg", UI, "text on a pressed button"],
  ["text", "blocked-bg", UI, "text on a pressed blocked row"], ["blocked-name", "blocked-bg", UI, "blocked name on a pressed blocked row"],
  ["muted", "blocked-bg", UI, "muted on a pressed blocked row"],
  // The press layer draws over a surface's gradient: the stops with text on them, held.
  ...["g-attn", "g-off"].flatMap((b) => [["text", b, UI, "text on pressed " + b], ["muted", b, UI, "muted on pressed " + b]]),
];

let fails = 0, checks = 0;
for (const t of THEMES) {
  const bad = [];
  for (const [fg, bg, min, what] of PRESSED) {
    checks++;
    const f = t.vars[fg], b = t.vars[bg];
    if (!/^#[0-9a-f]{6}$/i.test(f || "") || !/^#[0-9a-f]{6}$/i.test(b || "") || !/^#[0-9a-f]{6}$/i.test(t.vars.text || "")) { bad.push(`${what}: ${fg}, ${bg} or text is not a 6-digit hex`); continue; }
    const pb = pressed(b, t.vars.text), r = contrast(f, pb);
    if (r < min) bad.push(`${what}: ${fg} ${f} on ${bg} ${b} pressed (${pb}) = ${r.toFixed(2)} < ${min}`);
  }
  for (const [fg, bg, min, what] of TEXT) {
    checks++;
    const f = t.vars[fg], b = t.vars[bg];
    if (!f || !b || !/^#/.test(f) || !/^#/.test(b)) { bad.push(`${what}: token ${!f || !/^#/.test(f) ? fg : bg} missing or not a hex`); continue; }
    const r = contrast(f, b);
    if (r < min) bad.push(`${what}: ${fg} ${f} on ${bg} ${b} = ${r.toFixed(2)} < ${min}`);
  }
  for (const k of DEPTH) if (!(k in t.vars)) bad.push(`depth token --${k} is not defined`);
  for (const [a, b, min] of STEPS) { checks++; const r = contrast(t.vars[a], t.vars[b]); if (r < min) bad.push(`surface step ${a} -> ${b} = ${r.toFixed(2)} < ${min}`); }
  for (const k of used) if (!(k in t.vars)) bad.push(`token --${k} is used by index.html but not defined`);
  const adj = Object.entries(t.adjusted || {});
  console.log(`${bad.length ? "FAIL" : "PASS"}  ${t.name} (${t.id})${adj.length ? `  adjusted: ${adj.map(([k, [a, b]]) => `${k} ${a}->${b}`).join(", ")}` : ""}`);
  bad.forEach((m) => console.log("      " + m));
  if (verbose) console.log(`      body ${contrast(t.vars.text, t.vars.bg).toFixed(1)}  muted ${contrast(t.vars.muted, t.vars.panel).toFixed(1)}  min chip ${Math.min(...TEXT.filter((x) => x[3].startsWith("chip")).map(([f, b]) => contrast(t.vars[f], t.vars[b]))).toFixed(1)}`);
  if (bad.length) fails++;
}
console.log(`${THEMES.length} themes, ${checks} contrast checks, ${used.size} tokens used by index.html: ${fails ? fails + " theme(s) failing" : "all pass"}`);
process.exit(fails ? 1 : 0);
