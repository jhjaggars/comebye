// The browser's half of GPG unlock with a passkey (issue #3). Pure WebCrypto,
// no DOM, nothing stored: the formats are the host's (internal/gpgunlock).
//
//   challenge SHA-256("comebye-approval-v1\0" || request bytes)
//   PRF salt  SHA-256("comebye-prf-v1\0" || host || "\0" || keygrips joined by ",")
//   K         HKDF-SHA256(PRF output, no salt, info "comebye-gpg-k-v1"), 32 bytes
//   sealed    base64url(JSON{v,curve,epk,iv,ct}): K sealed to the request's key:
//             ECDH with an ephemeral key, HKDF(salt = challenge, info
//             "comebye-seal-v1"), AES-256-GCM with the challenge as AAD
//   blob      AES-256-GCM of the passphrase under K; AAD
//             "comebye-gpg-blob-v1\0" || host || "\0" || keygrips joined by ","
//
// K, the PRF output and the passphrase bytes live in typed arrays that the
// caller wipes (wipe) as soon as they have been used.
(function (root) {
  "use strict";

  const enc = new TextEncoder();
  const subtle = () => root.crypto.subtle;

  const b64 = (buf) => {
    const u = new Uint8Array(buf);
    let s = "";
    for (let i = 0; i < u.length; i += 0x8000) s += String.fromCharCode.apply(null, u.subarray(i, i + 0x8000));
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  };
  const unb64 = (s) => Uint8Array.from(atob(String(s).replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));
  const cat = (...parts) => {
    const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
    let o = 0;
    for (const p of parts) { out.set(p, o); o += p.length; }
    return out;
  };
  const sha256 = async (bytes) => new Uint8Array(await subtle().digest("SHA-256", bytes));
  const wipe = (u) => { if (u && u.fill) u.fill(0); };

  // canonical is the host's request JSON: keys sorted, no spaces (Go's
  // json.Marshal of a map). The hub keeps the host's own bytes; this rebuilds
  // them from the parsed object so the challenge can be checked here.
  function canonical(v) {
    if (Array.isArray(v)) return "[" + v.map(canonical).join(",") + "]";
    if (v && typeof v === "object") return "{" + Object.keys(v).sort().map((k) => JSON.stringify(k) + ":" + canonical(v[k])).join(",") + "}";
    return JSON.stringify(v);
  }

  const approvalChallenge = (requestBytes) => sha256(cat(enc.encode("comebye-approval-v1\0"), requestBytes));

  const prfSalt = (host, keygrips) => sha256(enc.encode("comebye-prf-v1\0" + host + "\0" + keygrips.join(",")));

  async function hkdf(ikm, salt, info) {
    const k = await subtle().importKey("raw", ikm, "HKDF", false, ["deriveBits"]);
    return new Uint8Array(await subtle().deriveBits({ name: "HKDF", hash: "SHA-256", salt, info: enc.encode(info) }, k, 256));
  }

  // deriveK is K from the passkey's PRF output (an ArrayBuffer or typed array).
  const deriveK = (prf) => hkdf(prf, new Uint8Array(0), "comebye-gpg-k-v1");

  const curveParams = (curve) => (curve === "P-256" ? { name: "ECDH", namedCurve: "P-256" } : curve === "X25519" ? { name: "X25519" } : null);

  // seal seals K to the request's one-use key (pub: raw bytes). The result is
  // the string the host opens. A curve this browser lacks is an error, not a
  // silent switch.
  async function seal(curve, pub, challenge, k) {
    const alg = curveParams(curve);
    if (!alg) throw new Error("The request names a key type this app does not know.");
    const peer = await subtle().importKey("raw", pub, alg, false, []);
    const eph = await subtle().generateKey(alg, true, ["deriveBits"]);
    const shared = new Uint8Array(await subtle().deriveBits({ ...alg, public: peer }, eph.privateKey, 256));
    const sk = await hkdf(shared, challenge, "comebye-seal-v1");
    wipe(shared);
    const key = await subtle().importKey("raw", sk, "AES-GCM", false, ["encrypt"]);
    wipe(sk);
    const iv = root.crypto.getRandomValues(new Uint8Array(12));
    const ct = await subtle().encrypt({ name: "AES-GCM", iv, additionalData: challenge }, key, k);
    const epk = await subtle().exportKey("raw", eph.publicKey);
    return b64(enc.encode(JSON.stringify({ v: 1, curve, epk: b64(epk), iv: b64(iv), ct: b64(ct) })));
  }

  const blobAAD = (host, keygrips) => enc.encode("comebye-gpg-blob-v1\0" + host + "\0" + keygrips.join(","));

  // encryptBlob is the passphrase at rest. pass is a Uint8Array (wiped by the caller).
  async function encryptBlob(k, host, keygrips, pass) {
    const key = await subtle().importKey("raw", k, "AES-GCM", false, ["encrypt"]);
    const iv = root.crypto.getRandomValues(new Uint8Array(12));
    const ct = await subtle().encrypt({ name: "AES-GCM", iv, additionalData: blobAAD(host, keygrips) }, key, pass);
    return { v: 1, host, keygrips, iv: b64(iv), ct: b64(ct) };
  }

  // fingerprint is the short name of a public key (base64url PKIX DER): the
  // first 10 bytes of its SHA-256, as five groups of four hex digits, the same
  // as the terminal's.
  async function fingerprint(spki) {
    const h = [...(await sha256(unb64(spki))).subarray(0, 10)].map((x) => x.toString(16).padStart(2, "0")).join("").toUpperCase();
    return [0, 4, 8, 12, 16].map((i) => h.slice(i, i + 4)).join("-");
  }

  // checkRequest checks what the browser can of a host's request before it
  // asks for Face ID: the hub's challenge is the hash of this very request,
  // the request is for this host and this origin, and the host key's signature
  // holds (where this browser has Ed25519; the host checks it all again).
  // It returns {challenge, host, keygrips, curve, pub, setup, hostKey,
  // fingerprint, signed} or throws a message for the user.
  async function checkRequest(req, hubChallenge, approvalHost, rpID, nowSec) {
    if (!req || typeof req !== "object" || req.action !== "gpg-unlock" || req.v !== 1) throw new Error("This is not a GPG unlock request.");
    if (typeof req.host !== "string" || !Array.isArray(req.keygrips) || !req.keygrips.length || typeof req.pub !== "string" || typeof req.host_key !== "string" || typeof req.sig !== "string") {
      throw new Error("The unlock request is incomplete.");
    }
    if (approvalHost && req.host.toLowerCase() !== String(approvalHost).toLowerCase()) throw new Error("The request is for another host than the item says.");
    if (req.rp !== rpID) throw new Error("The request is for another address than this one.");
    if (typeof req.exp === "number" && nowSec && req.exp < nowSec) throw new Error("This request expired: ask again.");
    const { sig, ...unsigned } = req;
    const challenge = await approvalChallenge(enc.encode(canonical(req)));
    if (b64(challenge) !== hubChallenge) throw new Error("The request does not match what the hub asked Face ID to approve. Nothing was sent.");
    let signed = false;
    try {
      const key = await subtle().importKey("spki", unb64(req.host_key), { name: "Ed25519" }, false, ["verify"]);
      if (!(await subtle().verify({ name: "Ed25519" }, key, unb64(sig), enc.encode(canonical(unsigned))))) {
        throw new Error("The request is not signed by the host key it names. Nothing was sent.");
      }
      signed = true;
    } catch (e) {
      if (!(e && (e.name === "NotSupportedError" || e.name === "SyntaxError"))) throw e; // no Ed25519 here: the host checks it
    }
    return {
      challenge, host: req.host, keygrips: req.keygrips, curve: req.curve || "P-256", pub: unb64(req.pub), setup: !!req.setup,
      hostKey: req.host_key, fingerprint: await fingerprint(req.host_key), signed,
    };
  }

  const api = { b64, unb64, canonical, approvalChallenge, prfSalt, deriveK, seal, encryptBlob, fingerprint, checkRequest, wipe };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.CBUnlock = api;
})(typeof globalThis !== "undefined" ? globalThis : this);
