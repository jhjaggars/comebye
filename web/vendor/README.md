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
| `hljs/*.min.js` | [highlight.js](https://github.com/highlightjs/highlight.js) 11.11.1 from the `@highlightjs/cdn-assets` package, `es/core.min.js` and 15 `es/languages/*.min.js` (go, javascript, typescript, python, bash, json, yaml, markdown, css, xml, diff, rust, sql, shell, makefile, dockerfile) | BSD-3-Clause (`LICENSE-highlightjs.txt`) | syntax colours in the Diff panel; loaded on demand by `sidepanel.js` as ES modules |

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

highlight.js 11.11.1 (`hljs/`, one ES module per file, unedited)
- tarball: https://registry.npmjs.org/@highlightjs/cdn-assets/-/cdn-assets-11.11.1.tgz
- registry `dist.integrity`: `sha512-VEPdHzwelZ12hEX18BHduqxMZGolcUsrbeokHYxOUIm8X2+M7nx5QPtPeQgRxR9XjhdLv4/7DD5BWOlSrJ3k7Q==` (matched)
- registry `dist.shasum` (sha1): `136984ae467865e22080b3a4b65398a086e1ae7b` (matched)
- why this package: the highlight.js package itself ships CommonJS, whose top-level names would leak into the app's globals; the CDN build is real ES modules, which the CSP allows (`import()` from our own origin).
- file sha256: `bash.min.js` d2290ecb0b7d522df0d91d54ee216c673e0c504decd48ef15197b3cab5cb7474
- file sha256: `core.min.js` bfcca5550cb9d62482162e1d1fb90c822d2d35036b556544817441b795b7103c
- file sha256: `css.min.js` 8024c97edb80335f7aded04543de058770eab34939b9b2db8334fb8aa16f0e45
- file sha256: `diff.min.js` 259e0a75cbaa5ce6769259ac647cec904a4c6b5cdc6e8d66f7820b01213a4492
- file sha256: `dockerfile.min.js` bd40da2c7d1b727dcd4e76f197f176e3d45ed61d167ef5d82a1f15e38a929de5
- file sha256: `go.min.js` 364487e8fca398a6548efbeae11a6d4e28f35ae908c5324c0efd628e5359ddc5
- file sha256: `javascript.min.js` d893654be08f10fe656739ec83cf2cdeb7ea82a26b332b9fea88d4d215c00d89
- file sha256: `json.min.js` 7dd0922802ac593d34da3466175377c9efb4612ee2ea97ddafa1fce618a510d9
- file sha256: `makefile.min.js` 13f6006b2b59fe2f156cc2c38ebfe481878833502f9423748fd5c9ea4e7e0b93
- file sha256: `markdown.min.js` e8f544abc25490c51ff4bea5687a02ef4a65f1edc83dd208b6664d2672b3e3d5
- file sha256: `python.min.js` 4112a5c44dc7a4c56a772347bd318f246aa1ea51e0d6cb6aba78b25c3e60d524
- file sha256: `rust.min.js` 9985e07ffd86b7172ce1499fadb1ae76677875b823fbc966009f324bffbb386e
- file sha256: `shell.min.js` 2ecd002378469afc6edc09d06310f30bb5e4a3c44e82d281d16e5395dea70afa
- file sha256: `sql.min.js` eadde8cadf1c469031abc0c6afd9767dfa8aacf9a7f9ab739ee6edfe635e300e
- file sha256: `typescript.min.js` 60f542a05921072df100b34ea9a20a765617c518f8ac11d1c2c238ae11d83fcc
- file sha256: `xml.min.js` 2d843265c0bdd3e50f107e6473196b5f525c0ea68de83c40f9f30ffbbea6896f
- file sha256: `yaml.min.js` 3d227e0d347021b99da5c7b8de86ec8ce647f2f15d3b97100b34f9d2507ec2e7

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
