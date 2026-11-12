#!/usr/bin/env node
// Design-token ratchet for web/index.html (docs/design-tokens.md).
//
//   node tools/uitest/tokens.mjs            check: exit 1 if a count went UP
//   node tools/uitest/tokens.mjs --update   write tokens-baseline.json (refuses to raise a count)
//   node tools/uitest/tokens.mjs --verbose  list every offender
//
// In padding/margin/gap, border-radius and font-size (and the font: shorthand's size) a px value
// should be a token: var(--sp-8), var(--r-10), var(--fs-14). Two counts per scale:
//   literal  - a positive px value written out where a token exists for it (use the token)
//   offscale - a px value that is on no scale (pick a scale step; add one only with a reason)
// 0, 1px and negative margins (which cannot be a var()) are exempt from "literal".
// The counts may only go down: a new offender fails, a fixed one asks you to lower the baseline.
// Colours are not checked here: contrast.mjs owns them.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const html = fs.readFileSync(path.join(here, "../../web/index.html"), "utf8");
const baselineFile = path.join(here, "tokens-baseline.json");
const css = (html.match(/<style[^>]*>([\s\S]*?)<\/style>/) || [])[1];
if (!css) { console.error("tokens: no <style> block in web/index.html"); process.exit(2); }
const nc = css.replace(/\/\*[\s\S]*?\*\//g, (m) => " ".repeat(m.length));

// The scales are the :root tokens themselves: one source of truth.
const root = (nc.match(/:root\s*\{([\s\S]*?)\n\s*\}/) || [])[1] || "";
const scale = (prefix) => new Set([...root.matchAll(new RegExp(`--${prefix}-(\\d+)\\s*:\\s*(\\d+)px`, "g"))].map((m) => Number(m[2])));
const SCALES = {
  space: { set: scale("sp"), re: /^(padding|margin|gap|row-gap|column-gap)(-(top|bottom|left|right|inline|block)(-(start|end))?)?$/, tok: "--sp-N" },
  radius: { set: scale("r"), re: /^border(-(top|bottom)-(left|right))?-radius$/, tok: "--r-N", pill: [99, 999] },
  font: { set: scale("fs"), re: /^font-size$/, tok: "--fs-N" },
  hit: { set: scale("hit"), re: /^min-height$/, tok: "--hit-N" },
};
for (const [k, s] of Object.entries(SCALES)) if (s.set.size < 5) { console.error(`tokens: the ${k} scale has only ${s.set.size} steps in :root; is the token block gone?`); process.exit(2); }

const TOK = /(env|var|color-mix)\((?:[^()]|\([^()]*\))*\)|(?<![\w.#-])(-?\d*\.?\d+)px\b/g;
const found = { space: { literal: [], offscale: [] }, radius: { literal: [], offscale: [] }, font: { literal: [], offscale: [] }, hit: { literal: [], offscale: [] } };
let sel = "";
for (const m of nc.matchAll(/[^{};]+\{|\}|[^{};]+(?:;|(?=\}))/g)) {
  const t = m[0];
  if (t === "}") continue;
  if (t.endsWith("{")) { sel = t.slice(0, -1).replace(/\s+/g, " ").trim().slice(0, 60); continue; }
  const d = t.match(/^\s*([\w-]+)\s*:\s*([\s\S]*?)\s*;?\s*$/);
  if (!d) continue;
  const [, prop, val] = d;
  let kind = Object.keys(SCALES).find((k) => SCALES[k].re.test(prop));
  let firstOnly = false;
  if (!kind && prop === "font" && /\dpx/.test(val)) { kind = "font"; firstOnly = true; }
  if (!kind) continue;
  const sc = SCALES[kind];
  let seen = false;
  for (const x of val.matchAll(TOK)) {
    if (x[1] || x[2] === undefined) continue;
    if (firstOnly) { if (seen) continue; seen = true; }
    const v = Number(x[2]), a = Math.abs(v);
    if (a === 0 || (a === 1 && kind !== "font")) continue;
    const onScale = sc.set.has(a) || (sc.pill && sc.pill.includes(a));
    const where = `${sel} { ${prop}: ${val.trim().slice(0, 50)} }`;
    if (!onScale) found[kind].offscale.push(where);
    else if (v > 0) found[kind].literal.push(where);
  }
}
// every token the CSS uses must exist
const defined = new Set([...root.matchAll(/(--(?:(?:sp|r|fs|hit|elev)-\d+|r-pill))\s*:/g)].map((m) => m[1]));
const missing = [...new Set([...nc.matchAll(/var\((--(?:(?:sp|r|fs|hit|elev)-\d+|r-pill))\b/g)].map((m) => m[1]))].filter((n) => !defined.has(n));

const counts = {};
for (const [k, v] of Object.entries(found)) counts[k] = { literal: v.literal.length, offscale: v.offscale.length };
const argv = process.argv.slice(2);
let base = {};
try { base = JSON.parse(fs.readFileSync(baselineFile, "utf8")); } catch {}
let fail = false;
if (missing.length) { console.log("FAIL  tokens used but not defined in :root: " + missing.join(", ")); fail = true; }
const lowered = [];
for (const k of Object.keys(counts)) for (const w of ["literal", "offscale"]) {
  const now = counts[k][w], was = base[k]?.[w];
  if (was === undefined) { console.log(`NOTE  ${k}.${w}: ${now} (no baseline; run --update)`); continue; }
  const pad = `${k}.${w}`.padEnd(18);
  if (now > was) { fail = true; console.log(`FAIL  ${pad} ${was} -> ${now}  (${now - was} new)`); found[k][w].slice(0).forEach((x) => argv.includes("--verbose") && console.log("        " + x)); }
  else if (now < was) { lowered.push(`${pad} ${was} -> ${now}`); }
  else console.log(`ok    ${pad} ${now}`);
}
if (argv.includes("--update")) {
  const raised = Object.keys(counts).flatMap((k) => ["literal", "offscale"].filter((w) => base[k]?.[w] !== undefined && counts[k][w] > base[k][w]).map((w) => `${k}.${w}`));
  if (raised.length) { console.error("tokens: --update refuses to raise " + raised.join(", ") + ": use a token or an existing scale step instead"); process.exit(1); }
  fs.writeFileSync(baselineFile, JSON.stringify(counts, null, 2) + "\n");
  console.log("wrote " + path.relative(process.cwd(), baselineFile)); process.exit(fail && missing.length ? 1 : 0);
}
if (lowered.length && !fail) { console.log("LOWER the baseline (good!): " + lowered.join("; ")); console.log("      run: node tools/uitest/tokens.mjs --update"); fail = true; }
if (argv.includes("--verbose")) for (const k of Object.keys(found)) for (const w of ["literal", "offscale"]) found[k][w].forEach((x) => console.log(`  ${k}.${w}: ${x}`));
process.exit(fail ? 1 : 0);
