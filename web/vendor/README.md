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
