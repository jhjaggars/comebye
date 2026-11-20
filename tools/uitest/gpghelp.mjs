// What the GPG unlock scripts (gpgunlock.mjs and tools/smoke/day.mjs) share: the throwaway GNUPGHOME
// guard, the "is gpg here" probe, the throwaway key, and Chrome's virtual authenticator with PRF.
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

export const PASS = "correct horse battery staple 7a1f";

// Never the user's own keys: only a throwaway GNUPGHOME under the temp directory. Exits 2 otherwise.
export function requireThrowawayGnupg(who, gnupg = process.env.GNUPGHOME) {
  if (!gnupg || !path.resolve(gnupg).startsWith(os.tmpdir() + path.sep) || path.resolve(gnupg) === path.resolve(os.homedir(), ".gnupg")) {
    console.error(`${who}: GNUPGHOME must be a temp directory (run.sh sets one)`); process.exit(2);
  }
  fs.mkdirSync(gnupg, { recursive: true, mode: 0o700 });
  fs.chmodSync(gnupg, 0o700);
  return gnupg;
}

// Whether gpg and the tools `comebye gpg` needs are installed.
export function gpgInstalled() {
  const have = (b) => spawnSync("sh", ["-c", `command -v ${b}`]).status === 0;
  return have("gpg") && have("gpg-connect-agent") && have("gpgconf") &&
    (have("gpg-preset-passphrase") || ["/usr/lib/gnupg/gpg-preset-passphrase", "/usr/libexec/gpg-preset-passphrase", "/usr/lib/gnupg2/gpg-preset-passphrase"].some((p) => fs.existsSync(p)));
}

// A throwaway key (ed25519, signing) with a passphrase in env.GNUPGHOME; reuses the one already there. Returns its fingerprint.
export function throwawayKey(env) {
  const list = () => execFileSync("gpg", ["--batch", "--list-secret-keys", "--with-colons"], { env, encoding: "utf8" }).split("\n").map((l) => l.split(":"));
  if (!list().some((f) => f[0] === "fpr")) {
    execFileSync("gpg", ["--batch", "--pinentry-mode", "loopback", "--passphrase", PASS, "--quick-gen-key", "UI Test <ui@example.invalid>", "ed25519", "sign", "never"], { env, stdio: "pipe" });
  }
  return list().find((f) => f[0] === "fpr")[9];
}

// A virtual authenticator on the page's CDP session (the passkey, Face ID stand-in), with the PRF extension when hasPrf.
export const addAuth = (cdp, hasPrf) => cdp.send("WebAuthn.addVirtualAuthenticator", { options: {
  protocol: "ctap2", ctap2Version: "ctap2_1", transport: "internal", hasResidentKey: true, hasUserVerification: true, isUserVerified: true,
  automaticPresenceSimulation: true, hasPrf } });
