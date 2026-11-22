#!/usr/bin/env node
// Static facts about web/'s source, read in well under a second with no browser (the lint job runs it):
//
//   node tools/ci/static.mjs [WEB_DIR]
//
//  1. Every var(--x) with no fallback has a definition: a `--x:` in the CSS (index.html or a script's styles), a
//     setProperty("--x", ...) from a script, or a theme token (themes.js turns each into --name).
//  2. The stacking tokens are in order: backdrop < menu < popup < sheet < screen < scrim < toast (read from :root).
//  3. The one focus ring: --ring / --ring-off in :root, :focus-visible uses them.
//  4. The shared shapes (chip, card, button, segmented control, notice): one rule names every class of the family with the
//     shape token, and no other rule of a family class sets another radius. (This was chips, controls and buttons.mjs.)
//  5. Surface steps in themes.js (all 15 themes): --nav-bg is one calm step from --bg, text, muted and the accents read at
//     4.5:1 on the nav, the focus colour 3:1 on the nav and --panel2. (This was navcontrast.mjs.)
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

// The one focus ring.
const css = Object.entries(src).filter(([f]) => f.endsWith(".html") || f.endsWith(".css")).map(([, t]) => t).join("\n").replace(/\/\*[\s\S]*?\*\//g, "");
if (!/--ring\s*:\s*2px solid var\(--focus\)/.test(css)) problems.push("ring: :root must define --ring as 2px solid var(--focus)");
if (!/--ring-off\s*:\s*2px/.test(css)) problems.push("ring: :root must define --ring-off: 2px");
if (!/:focus-visible\s*\{[^}]*outline:\s*var\(--ring\)[^}]*outline-offset:\s*var\(--ring-off\)/.test(css)) problems.push("ring: :focus-visible must be { outline: var(--ring); outline-offset: var(--ring-off) }");

// Shared shapes: every rule in the stylesheet, as { sels, body }.
const rules = [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => ({ sels: m[1].split(",").map((x) => x.trim().replace(/\s+/g, " ")), body: m[2] }));
const FAMILIES = {
  chip: { val: "var(--r-6)", classes: [".chip", ".wt-chip", ".host-chip", ".lg-chip", ".role-chip", ".us-chip", ".bd-tag", ".td-chip", ".rel-chip", ".bd-chip", ".rc-tag", ".rv-chip"] },
  card: { val: "var(--r-12)", classes: [".card", ".pz-card", ".rl-card", ".us-card", ".rc-card", ".pgs-card", ".pgs-hit", ".pr-card"] },
  seg: { val: "var(--r-10)", classes: [".seg", ".bd-seg", ".pgs-seg", ".us-seg"] },
  notice: { val: "var(--r-12)", classes: [".notice", "#toast", ".rtr-banner"] },
  button: { val: "var(--r-12)", classes: [".btn", ".td-acts button", ".ds-fix", ".ds-confirm", ".ds-fk", ".ds-nav button", ".tds-none button", "#sp .cite button", "#sp .hk button", ".us-crumbs button"] },
};
const radius = (body) => { const m = body.match(/(?:^|[;\s])border-radius\s*:\s*([^;]+)/); return m ? m[1].trim() : null; };
for (const [name, fam] of Object.entries(FAMILIES)) {
  const group = rules.find((r) => fam.classes.every((c) => r.sels.includes(c)) && radius(r.body) === fam.val);
  if (!group) problems.push(`shape: no one rule lists every ${name} class (${fam.classes.join(", ")}) with border-radius: ${fam.val}`);
  for (const r of rules) {
    if (r === group) continue;
    const v = radius(r.body);
    if (v && v !== fam.val && r.sels.some((x) => fam.classes.includes(x))) problems.push(`shape: ${name}: "${r.sels.join(", ")}" sets border-radius: ${v}, not the shared ${fam.val}`);
  }
}

// Surface steps of every theme (themes.js is the one source of the tokens).
if (src["themes.js"]) {
  const ctx = { globalThis: null }; ctx.globalThis = ctx; vm.createContext(ctx); vm.runInContext(src["themes.js"], ctx);
  const { THEMES, contrast } = ctx.ComebyeThemes;
  const STEP = { dark: [1.09, 1.16], light: [1.09, 1.12] };
  for (const t of THEMES) {
    const v = t.vars, step = contrast(v.bg, v["nav-bg"]), [lo, hi] = STEP[t.mode];
    if (step < lo || step > hi) problems.push(`theme ${t.id}: --nav-bg is ${step.toFixed(3)}:1 from --bg, outside ${lo}-${hi}`);
    for (const k of ["text", "muted", "quote", "blue", "orange", "green", "purple", "red", "yellow", "teal"]) if (contrast(v[k], v["nav-bg"]) < 4.5) problems.push(`theme ${t.id}: ${k} on the nav is ${contrast(v[k], v["nav-bg"]).toFixed(2)}:1, under 4.5`);
    for (const k of ["nav-bg", "panel2"]) if (contrast(v.focus, v[k]) < 3) problems.push(`theme ${t.id}: the focus colour on ${k} is ${contrast(v.focus, v[k]).toFixed(2)}:1, under 3`);
  }
}

if (problems.length) { console.log(problems.map((p) => "FAIL " + p).join("\n")); process.exit(1); }
console.log(`ok   ${defined.size} custom properties defined; every var() without a fallback has one; stacking tokens in order; focus ring, shared shapes and theme surface steps hold`);
