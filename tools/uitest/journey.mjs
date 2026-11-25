// A journey (#139 slice 6): several parts, each once a script of its own, run one after the other in one node process,
// so they share one Chrome (lib.mjs starts it once per process) and the fixture hub run.sh started for the journey,
// in place of a Chrome start and a server set-up each.
//
// A part is a NAME.part.mjs file whose body is `export async function run(process) { ... }`: the script as it was,
// reading its arguments and environment from the `process` it is given (its own argv and env, as run.sh passed them to
// the script) and ending with process.exit(code), which here ends only that part. Its checks, their names and the
// "ok"/"FAIL" lines it prints are the script's, unchanged, so tools/ci/flakes.sh and the CI logs read them as before.
//
//   await journey("navjourney", [{ part: "leftnav", argv: [base, out] }, ...])
//
// Each part prints "== part NAME: N s" when it ends (run.sh's "== script" lines, one level down), and "FAIL NAME: ..."
// when it threw rather than ended. PARTS="a b" in the environment runs only those parts (a quick look at one). The
// journey exits 1 when any part ended with a non-zero code.
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { settleStats } from "./lib.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));

class PartExit extends Error {
  constructor(code) { super("process.exit(" + code + ")"); this.code = code; }
}

// partProcess: what a part sees as `process`: the real one, with its own argv and env, and an exit that ends the part.
function partProcess(name, argv, env) {
  const real = globalThis.process;
  const p = Object.create(real);
  p.argv = [real.argv[0], path.join(here, name + ".mjs"), ...argv];
  p.env = { ...real.env, ...env };
  p.exit = (code = 0) => { throw new PartExit(code); };
  p.exitCode = undefined;
  return p;
}

export async function journey(name, parts) {
  const only = (globalThis.process.env.PARTS || "").split(/\s+/).filter(Boolean);
  let rc = 0, current = "", rejected = false;
  // A script crashed on an unhandled rejection; a part that has one while it runs fails, as the script did. One after
  // its part ended (a stray promise the script's process.exit used to cut short) is noted and ignored.
  globalThis.process.on("unhandledRejection", (e) => {
    const why = String((e && e.stack) || e).split("\n").join("\n     ");
    if (current) { rejected = true; console.log(`FAIL ${current}: an unhandled rejection\n     ${why}`); }
    else console.log(`(after its part ended, an unhandled rejection: ${why.split("\n")[0]})`);
  });
  for (const { part, argv = [], env = {} } of parts) {
    if (only.length && !only.includes(part)) continue;
    const t0 = Date.now(), p = partProcess(part, argv, env);
    let code = 0;
    current = part; rejected = false; globalThis.__uitestPart = part;
    try {
      const m = await import(pathToFileURL(path.join(here, part + ".part.mjs")).href);
      await m.run(p);
      code = p.exitCode || 0;
    } catch (e) {
      if (e instanceof PartExit) code = e.code;
      else { code = 1; console.log(`FAIL ${part}: the part threw\n     ` + String((e && e.stack) || e).split("\n").join("\n     ")); }
    }
    current = "";
    for (const l of settleStats()) console.log(l);
    if (rejected && !code) code = 1;
    console.log(`== part ${part}: ${Math.round((Date.now() - t0) / 1000)} s${code ? " (exit " + code + ")" : ""}`);
    if (code) rc = 1;
  }
  console.log(`${name}: ${rc ? "FAILED" : "ok"}`);
  globalThis.process.exit(rc);
}
