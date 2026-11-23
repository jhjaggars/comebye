// Package apipy is the Python module `comebye api run` puts on a script's
// path: comebye/ (hand-written, except _types.py, which
// internal/server/apitypes_test.go generates from the hub's Go types), the
// guide `comebye api --help` prints and the record reference of
// `comebye api types`.
package apipy

import (
	"embed"
	"io/fs"
)

//go:embed comebye/*.py
var module embed.FS

//go:embed help.txt
var Help string

//go:embed types.txt
var Types string

// Module is the comebye package: comebye/__init__.py and the rest.
func Module() fs.FS { return module }
