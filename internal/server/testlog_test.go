package server

import (
	"bytes"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// lockedLog is a log capture that goroutines may write while a test reads it.
type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLog points the global log at a lockedLog for the test and puts the
// old writer back after it. The global log is shared by every goroutine in the
// binary, including ones another test started and has not stopped yet, so a
// bare bytes.Buffer there is a data race (TestNoBareBufferAsTheGlobalLog).
func captureLog(t *testing.T) *lockedLog {
	t.Helper()
	l := &lockedLog{}
	old := log.Writer()
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(old) })
	return l
}

// The global log must never write into an unlocked buffer: a goroutine from
// another test may log while this one reads it, and the race detector fails
// the run at random (issue #139). This walks every Go file in the repository
// and refuses log.SetOutput (or a *log.Logger's SetOutput) given a
// bytes.Buffer or strings.Builder, declared or built in place. Use
// captureLog, or a writer with its own lock.
func TestNoBareBufferAsTheGlobalLog(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root not found from %s: %v", root, err)
	}
	var bad []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root && (strings.HasPrefix(n, ".") || n == "node_modules" || n == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := gotoken.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, pos := range bareBufferLogs(f) {
			bad = append(bad, rel+":"+strconv.Itoa(fset.Position(pos).Line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Errorf("SetOutput given an unlocked buffer (a data race with any goroutine that logs); use captureLog(t):\n  %s", strings.Join(bad, "\n  "))
	}
}

// bareBufferLogs finds SetOutput calls in f whose argument is a bytes.Buffer
// or strings.Builder: &x or x for a variable declared as (or initialised to)
// one, or one built in place. Names are tracked per file, which is enough for
// test code and errs on the side of reporting.
func bareBufferLogs(f *ast.File) []gotoken.Pos {
	isBufType := func(e ast.Expr) bool {
		if st, ok := e.(*ast.StarExpr); ok {
			e = st.X
		}
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && (pkg.Name == "bytes" && sel.Sel.Name == "Buffer" || pkg.Name == "strings" && sel.Sel.Name == "Builder")
	}
	isBufValue := func(e ast.Expr) bool {
		switch v := e.(type) {
		case *ast.UnaryExpr:
			if cl, ok := v.X.(*ast.CompositeLit); ok {
				return isBufType(cl.Type)
			}
		case *ast.CompositeLit:
			return isBufType(v.Type)
		case *ast.CallExpr:
			if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "new" && len(v.Args) == 1 {
				return isBufType(v.Args[0])
			}
		}
		return false
	}
	bufs := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ValueSpec:
			for i, name := range v.Names {
				if v.Type != nil && isBufType(v.Type) || i < len(v.Values) && isBufValue(v.Values[i]) {
					bufs[name.Name] = true
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range v.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && i < len(v.Rhs) && isBufValue(v.Rhs[i]) {
					bufs[id.Name] = true
				}
			}
		}
		return true
	})
	var out []gotoken.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetOutput" {
			return true
		}
		arg := call.Args[0]
		if isBufValue(arg) {
			out = append(out, call.Pos())
			return true
		}
		if u, ok := arg.(*ast.UnaryExpr); ok && u.Op == gotoken.AND {
			arg = u.X
		}
		if id, ok := arg.(*ast.Ident); ok && bufs[id.Name] {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

// The lint itself: it catches the shapes that caused #139 and lets a locked
// writer through.
func TestBareBufferLogsFindsTheShapes(t *testing.T) {
	src := `package p
import ("bytes"; "log"; "strings")
func a() { var b bytes.Buffer; log.SetOutput(&b) }
func b() { b := new(bytes.Buffer); log.SetOutput(b) }
func c() { log.SetOutput(&bytes.Buffer{}) }
func d() { var s strings.Builder; l := log.New(nil, "", 0); l.SetOutput(&s) }
func ok() { l := &lockedLog{}; log.SetOutput(l) }
`
	f, err := parser.ParseFile(gotoken.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(bareBufferLogs(f)); got != 4 {
		t.Errorf("found %d, want 4", got)
	}
}
