// The browser's GPG-unlock crypto (web/unlockcrypto.js) driven from Node for the Go interop test
// (internal/gpgunlock/interop_test.go): stdin is {request, challenge, rp, host, prf, pass?}, stdout the
// answer's browser-made parts. The host (Go) must open what this seals and decrypt what this encrypts.
import { createRequire } from "node:module";
import fs from "node:fs";
const require = createRequire(import.meta.url);
const U = require("../unlockcrypto.js");
const inp = JSON.parse(fs.readFileSync(0, "utf8"));
const req = JSON.parse(inp.request);
const c = await U.checkRequest(req, inp.challenge, inp.host, inp.rp, Math.floor(Date.now() / 1000));
const salt = await U.prfSalt(c.host, c.keygrips);
const k = await U.deriveK(U.unb64(inp.prf));
const sealed = await U.seal(c.curve, c.pub, c.challenge, k);
const out = { sealed, salt: U.b64(salt), fingerprint: c.fingerprint, signed: c.signed, setup: c.setup, challenge: U.b64(c.challenge) };
if (inp.pass !== undefined) out.blob = await U.encryptBlob(k, c.host, c.keygrips, new TextEncoder().encode(inp.pass));
// A request changed after the challenge was made is refused here, not signed for.
let refused = "";
try { await U.checkRequest({ ...req, nonce: "AAAA" }, inp.challenge, inp.host, inp.rp, 0); } catch (e) { refused = e.message; }
out.refusedTampered = refused;
console.log(JSON.stringify(out));
