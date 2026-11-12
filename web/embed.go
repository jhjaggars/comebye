// Package web embeds the phone app's static assets.
package web

import "embed"

// FS holds the static files served at /.
//
//go:embed index.html app.js keys.js sidepanel.js themes.js stt.js stt-worker.js manifest.webmanifest sw.js icon-192.png icon-512.png favicon.svg vendor/*.js vendor/hljs/*.js vendor/fonts/*.woff2 vendor/LICENSE-*.txt
var FS embed.FS
