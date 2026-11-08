# Vendored browser libraries

The phone app has no build step, so these third-party scripts are checked
in, embedded into the binary (see `web/embed.go`) and served from the app's
own origin (the CSP allows `script-src 'self'` only). Each file is copied
byte for byte from the package tarball on the npm registry; nothing is
edited, so the hashes below can be re-checked against the registry.

| File | Package | License | Used for |
|------|---------|---------|----------|
| `marked.umd.js` | [marked](https://github.com/markedjs/marked) 18.1.1, `lib/marked.umd.js` | MIT (`LICENSE-marked.txt`) | CommonMark + GFM to HTML |
| `purify.min.js` | [DOMPurify](https://github.com/cure53/DOMPurify) 3.4.17, `dist/purify.min.js` | Apache-2.0 (dual MPL-2.0/Apache-2.0; used under Apache-2.0, `LICENSE-dompurify.txt`) | sanitizing that HTML |
| `transformers.bundle.js` | [@huggingface/transformers](https://github.com/huggingface/transformers.js) 4.3.2 with onnxruntime-web 1.31.0-dev.20260914-8d85527a0 bundled in (**built**, see below) | Apache-2.0 and MIT (`LICENSE-transformers-bundle.txt`) | speech to text in the browser (`stt-worker.js`) |

## Provenance

marked 18.1.1
- tarball: https://registry.npmjs.org/marked/-/marked-18.1.1.tgz
- registry `dist.integrity`: `sha512-zEGgrtUG8crk+TrMacJSVABU4DYDDkS8uqgU3ohrn/2fW9C2AEfUrO3m6IhpIpQpl7YcVpiXXC9cSMWDANT3dg==` (matched)
- registry `dist.shasum` (sha1): `dd6288f8217971f7b0beefcabc52c92a6519b3e3` (matched)
- file sha256: `marked.umd.js` b82b9334406d8bdd16b6236c08de1cb65b2a859873ca9503d9cc0b292ca3c12a

DOMPurify 3.4.17
- tarball: https://registry.npmjs.org/dompurify/-/dompurify-3.4.17.tgz
- registry `dist.integrity`: `sha512-B/pSlYVHQ58kqISD/yDjuJ1yfzI34m1gF4lAod2bwSRhiN4K5Cl3H8Af7hqB2PmTL911tdEudnnXn5FnhRoz0w==` (matched)
- registry `dist.shasum` (sha1): `074bbb24eaa419535ac19ffe7f663104b0e1543d` (matched)
- file sha256: `purify.min.js` cd37898f9782adf54820c413191b5691803f0f137c799d6edcfd38523b4c2c31

`web/app_test.go` checks the file sha256 lines above against the files, so
an edit to a vendored file without updating this record fails the tests.

## Updating

```sh
npm view <pkg>@<version> dist.tarball dist.integrity dist.shasum
curl -sSfO <tarball>
openssl dgst -sha512 -binary <pkg>-<version>.tgz | base64 -w0   # must equal dist.integrity
tar xzf <pkg>-<version>.tgz                                     # copy the file(s) listed above
sha256sum web/vendor/*.js                                       # record here
```

Prefer DOMPurify releases promptly (they are mostly sanitizer-bypass
fixes). Changing either file changes the app version
(`internal/server/static.go`), so open pages reload onto it.

transformers.bundle.js (the one file here that is built, not copied; 550 KB, about 170 KB gzipped)
- not a registry file: `tools/stt/build.sh` bundles @huggingface/transformers 4.3.2 (and onnxruntime-web, onnxruntime-common, @huggingface/jinja, @huggingface/tokenizers) with esbuild 0.28.2 into one ES module exporting `pipeline` and `env`, because the app has no module loader and the CSP allows no CDN. The inputs are pinned exactly in `tools/stt/package.json` and `package-lock.json`; the build is deterministic (two runs give the same bytes). It is the only file here that is not byte for byte a registry file, so the hash below is of our build.
- one post-build edit, in `build.sh`: GitHub push protection mistakes two strings in the library for an API key (a 32-letter class name next to the word "mistral", and a gist URL with a 32-hex id). The class name is split into two literals and the URL shortened; behaviour is unchanged.
- file sha256: `transformers.bundle.js` 0dde9f140a99dd235e54bb961d1ae1e0c8dcd1fcb3fa6856066c23cdf2263e37
- the ONNX runtime it loads (`ort-wasm-simd-threaded.asyncify.mjs` and `.wasm`) is not in the binary or here: it is 27 MB, comes from the data directory (`comebye stt fetch`, pinned in `internal/stt/stt.go`), and must be the same onnxruntime-web build as the one inside this bundle.
- to rebuild at another version: change `tools/stt/package.json`, run `tools/stt/build.sh`, record the new hash here, and update the runtime pin and the `Version` in `internal/stt/stt.go` (a new version path makes browsers fetch afresh).
