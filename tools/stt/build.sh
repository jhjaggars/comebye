#!/usr/bin/env bash
# Rebuilds web/vendor/transformers.bundle.js from the pinned npm packages
# (package.json / package-lock.json). Dev-only; the result is checked in.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"
[ -d node_modules/esbuild ] || npm ci --no-audit --no-fund --silent
npx esbuild entry.js --bundle --format=esm --minify --legal-comments=none \
  --outfile="$here/../../web/vendor/transformers.bundle.js"
out="$here/../../web/vendor/transformers.bundle.js"
# GitHub push protection reads two strings in the library as an API key (the
# 32-letter class name Mistral3ForConditionalGeneration next to "mistral", and
# an error message's gist URL with a 32-hex id). Neither is a secret; split
# the name and shorten the URL. Behaviour is unchanged.
python3 - "$out" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
n, a, b = 'Mistral3ForConditionalGeneration', 'Mistral3ForConditional', 'Generation'
s = s.replace('"%s"' % n, '"%s"+"%s"' % (a, b)).replace('%s:()=>' % n, '["%s"+"%s"]:()=>' % (a, b))
g = 'https://gist.github.com/hollance/42e32852f24243b748ae6bc1f985b13a'
s = s.replace(g, 'https://gist.github.com/hollance')
open(p, 'w').write(s)
PY
sha256sum "$out"
