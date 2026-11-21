package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// docs/background-processes.md names every background loop the binary runs.
// This test keeps it current. It reads the code with go/parser (no regexps
// over Go source) and finds two kinds of loop:
//
//   - every `go` statement in main.go and announce.go: what main starts;
//   - every function in a non-test file under internal/ that makes a ticker
//     (time.NewTicker or time.Tick): a loop on a clock, wherever it is started.
//
// Each is named as Type.method (or a plain function's name), and the doc must
// carry that name in backticks. A name is a function, not a line number, so
// moving code or adding lines never fails the test; only a new loop, a renamed
// one, or a doc row for one that is gone does.
//
// What it leaves out, on purpose, to avoid false alarms: one-shot goroutines
// (a request's work, an ask, a send), and loops paced by time.After or
// time.Sleep, which in this code are mostly timeouts and retries. The loops
// main starts are caught by the first scan however they are paced.

const bgDoc = "docs/background-processes.md"

// launchVars maps the variables main.go calls methods on to their types, so
// that `go srv.RunQuiet(...)` and the ticker in Server.RunQuiet share a name.
var launchVars = map[string]string{
	"srv":     "Server",
	"fl":      "Fleet",
	"manager": "Manager",
}

// bgLaunch is one `go` statement in main: names lists what it may be called in
// the doc (one entry for `go x.Method()`, every mapped call for a func literal).
type bgLaunch struct {
	pos   string
	names []string
}

func mainLaunches(t *testing.T) []bgLaunch {
	t.Helper()
	fset := token.NewFileSet()
	var out []bgLaunch
	for _, file := range []string{"main.go", "announce.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			g, ok := n.(*ast.GoStmt)
			if !ok {
				return true
			}
			l := bgLaunch{pos: fset.Position(g.Pos()).String()}
			if lit, ok := g.Call.Fun.(*ast.FuncLit); ok {
				ast.Inspect(lit.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok {
						if _, sel := c.Fun.(*ast.SelectorExpr); !sel {
							return true // make, len, a closure: not a name for the goroutine
						}
						if name := mappedCall(c.Fun); name != "" {
							l.names = append(l.names, name)
						}
					}
					return true
				})
				if len(l.names) == 0 {
					t.Errorf("%s: a goroutine whose body calls no method of %v: start it as a named function or method, so the doc can name it", l.pos, keys(launchVars))
					return false
				}
			} else if name := mappedCall(g.Call.Fun); name != "" {
				l.names = []string{name}
			} else {
				t.Errorf("%s: `go %s`: add the variable's type to launchVars in background_doc_test.go", l.pos, exprString(g.Call.Fun))
				return false
			}
			out = append(out, l)
			return false
		})
	}
	return out
}

// mappedCall names a call: Type.method for a method of a variable in
// launchVars, the function's name for a plain function, "" otherwise.
func mappedCall(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			if typ, ok := launchVars[x.Name]; ok {
				return typ + "." + f.Sel.Name
			}
		}
	}
	return ""
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	}
	return "..."
}

// tickerSite is a function that makes a ticker.
type tickerSite struct {
	name, pos string
}

func tickerSites(t *testing.T) []tickerSite {
	t.Helper()
	fset := token.NewFileSet()
	var out []tickerSite
	err := filepath.WalkDir("internal", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var at token.Pos
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok && at == token.NoPos {
					if s := exprString(c.Fun); s == "time.NewTicker" || s == "time.Tick" {
						at = c.Pos()
					}
				}
				return at == token.NoPos
			})
			if at != token.NoPos {
				out = append(out, tickerSite{name: funcName(fn), pos: fset.Position(at).String()})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// funcName is Type.method for a method, the name for a function.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if s, ok := typ.(*ast.StarExpr); ok {
		typ = s.X
	}
	if ix, ok := typ.(*ast.IndexExpr); ok { // a generic receiver
		typ = ix.X
	}
	return exprString(typ) + "." + fn.Name.Name
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	backticked = regexp.MustCompile("`([^`\n]+)`")
	// A row of the processes table: its second cell holds the name and file:line.
	// Its first name is the one checked; more names may follow, comma-separated.
	codeCell = regexp.MustCompile("^`([A-Za-z_][A-Za-z0-9_.]*)`(?:,\\s*`[^`]+`)*\\s*<br>\\s*`([^`:]+\\.go):([0-9]+)`")
)

func readBgDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(bgDoc)
	if err != nil {
		t.Fatalf("%v: the background-process reference is missing", err)
	}
	return string(b)
}

// TestBackgroundDocNamesEveryLoop fails when main starts a goroutine, or code
// under internal/ makes a ticker, that the doc does not name.
func TestBackgroundDocNamesEveryLoop(t *testing.T) {
	doc := readBgDoc(t)
	named := map[string]bool{}
	for _, m := range backticked.FindAllStringSubmatch(doc, -1) {
		named[m[1]] = true
	}
	for _, l := range mainLaunches(t) {
		ok := false
		for _, n := range l.names {
			ok = ok || named[n]
		}
		if !ok {
			t.Errorf("%s: main starts a goroutine (%s) that %s does not name: add a row for it", l.pos, strings.Join(l.names, " / "), bgDoc)
		}
	}
	for _, s := range tickerSites(t) {
		if !named[s.name] {
			t.Errorf("%s: %s runs on a ticker and %s does not name it: add a row (or, for a per-request loop, a line under \"Not background processes\")", s.pos, s.name, bgDoc)
		}
	}
}

// TestBackgroundDocRowsAreLive fails when a row of the processes table names a
// loop the code no longer has, or points at a file that does not declare it.
// The line number is where it was when the doc was written; it is not checked,
// so an edit above it does not fail the test.
func TestBackgroundDocRowsAreLive(t *testing.T) {
	doc := readBgDoc(t)
	live := map[string]bool{}
	for _, l := range mainLaunches(t) {
		for _, n := range l.names {
			live[n] = true
		}
	}
	for _, s := range tickerSites(t) {
		live[s.name] = true
	}
	rows := 0
	for _, line := range strings.Split(doc, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 || !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		m := codeCell.FindStringSubmatch(strings.TrimSpace(cells[2]))
		if m == nil {
			continue
		}
		rows++
		name, file := m[1], m[2]
		if !live[name] {
			t.Errorf("%s names %s, which main no longer starts and no ticker runs in: remove or rename its row", bgDoc, name)
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("%s: the row for %s points at %s: %v", bgDoc, name, file, err)
			continue
		}
		method := name[strings.LastIndex(name, ".")+1:]
		if !regexp.MustCompile(`(?m)^func (\([^)]*\) )?` + regexp.QuoteMeta(method) + `\(`).Match(src) {
			t.Errorf("%s: the row for %s points at %s, which does not declare %s", bgDoc, name, file, method)
		}
	}
	if rows < 10 {
		t.Errorf("%s: read only %d rows of the processes table; has its format changed? (the second cell is `Name`<br>`file.go:line`)", bgDoc, rows)
	}
}
