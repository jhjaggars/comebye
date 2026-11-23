package server

import (
	"encoding"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"
)

// `comebye api` (internal/apipy) gives scripts typed, strict records for
// the hub's read routes. They are generated here from the Go types the
// routes answer with, so they cannot drift: this test fails when a struct
// changes and the generated files were not refreshed. Refresh them with
//
//	go test ./internal/server -run TestAPITypes -update-api-types
var updateAPITypes = flag.Bool("update-api-types", false, "rewrite internal/apipy's generated record types")

// apiRoots are the answers of the routes the Python module reads, under the
// names scripts see. Every struct reachable from them becomes a record too.
var apiRoots = []struct {
	name string
	t    reflect.Type
}{
	{"Whoami", reflect.TypeOf(whoamiView{})},
	{"Agents", reflect.TypeOf(agentsView{})},
	{"Ledger", reflect.TypeOf(ledgerView{})},
	{"TodoList", reflect.TypeOf(todoListView{})},
	{"TodoView", reflect.TypeOf(todoView{})},
	{"Queue", reflect.TypeOf(queueView{})},
	{"PageList", reflect.TypeOf(pageListView{})},
	{"PageView", reflect.TypeOf(pageView{})},
	{"Log", reflect.TypeOf(logView{})},
	{"Archived", reflect.TypeOf(archivedView{})},
	{"Removals", reflect.TypeOf(removalsView{})},
	{"Coordinators", reflect.TypeOf(coordinatorsView{})},
}

// apiNames renames the records whose Go names are internal ("entryView").
var apiNames = map[string]string{
	"server.entryView":       "Entry",
	"server.browserAgent":    "Agent",
	"server.browserHost":     "Host",
	"server.archivedRow":     "ArchivedSession",
	"server.coordinatorInfo": "Coordinator",
	"server.removal":         "Removal",
	"server.liveInfo":        "Live",
}

const apiModule = "github.com/jhjaggars/comebye/"

func TestAPITypes(t *testing.T) {
	g := newAPIGen(t)
	for _, r := range apiRoots {
		g.names[r.t] = r.name
	}
	for _, r := range apiRoots {
		g.visit(r.t)
	}
	g.assignNames()
	py, ref := g.python(), g.reference()

	dir := filepath.Join("..", "apipy")
	files := map[string]string{
		filepath.Join(dir, "comebye", "_types.py"): py,
		filepath.Join(dir, "types.txt"):            ref,
	}
	for path, want := range files {
		if *updateAPITypes {
			if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (generate it: go test ./internal/server -run TestAPITypes -update-api-types)", err)
		}
		if string(got) != want {
			t.Errorf("%s is stale: the Go types behind `comebye api` changed. Regenerate it:\n\tgo test ./internal/server -run TestAPITypes -update-api-types\nfirst difference: %s", path, firstDiff(string(got), want))
		}
	}
}

func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("line %d: have %q, want %q", i+1, x, y)
		}
	}
	return "none"
}

type apiField struct {
	json, attr, typ, doc string
	optional             bool // omitempty: may be absent
}

type apiRecord struct {
	t      reflect.Type
	name   string
	doc    string
	fields []apiField
}

type apiGen struct {
	t       *testing.T
	names   map[reflect.Type]string
	order   []reflect.Type
	records map[reflect.Type]*apiRecord
	docs    map[string]map[string]string // "pkg.Type" -> field (or "" for the type) -> doc
	parsed  map[string]bool
}

func newAPIGen(t *testing.T) *apiGen {
	return &apiGen{t: t, names: map[reflect.Type]string{}, records: map[reflect.Type]*apiRecord{}, docs: map[string]map[string]string{}, parsed: map[string]bool{}}
}

var (
	jsonMarshaler = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshaler = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	timeType      = reflect.TypeOf(time.Time{})
)

func goName(t reflect.Type) string {
	return filepath.Base(t.PkgPath()) + "." + t.Name()
}

// typ is t as the Python module spells it: str, int, float, bool, time,
// any, a record's name, [X] (a list), {X} (a map with string keys) and X?
// (may be null).
func (g *apiGen) typ(t reflect.Type) string {
	if t == timeType {
		return "time"
	}
	if t.Kind() != reflect.Pointer && (t.Implements(jsonMarshaler) || reflect.PointerTo(t).Implements(jsonMarshaler)) {
		return "any"
	}
	if t.Kind() != reflect.Pointer && (t.Implements(textMarshaler) || reflect.PointerTo(t).Implements(textMarshaler)) {
		return "str"
	}
	switch t.Kind() {
	case reflect.String:
		return "str"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "float"
	case reflect.Interface:
		return "any"
	case reflect.Pointer:
		inner := g.typ(t.Elem())
		if strings.HasSuffix(inner, "?") || inner == "any" {
			return inner
		}
		return inner + "?"
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return "str" // base64
		}
		return "[" + g.typ(t.Elem()) + "]"
	case reflect.Map:
		return "{" + g.typ(t.Elem()) + "}"
	case reflect.Struct:
		g.visit(t)
		return "@" + goName(t) // a record: named once every name is known
	}
	g.t.Fatalf("comebye api: no Python type for %v", t)
	return ""
}

func (g *apiGen) visit(t reflect.Type) {
	if _, ok := g.records[t]; ok {
		return
	}
	if t.Name() == "" {
		g.t.Fatalf("comebye api: an anonymous struct (%v) has no record name: give it a named type", t)
	}
	r := &apiRecord{t: t, doc: g.doc(t, "")}
	g.records[t] = r
	g.order = append(g.order, t)
	r.fields = g.fields(t, t)
}

type depthField struct {
	f     apiField
	depth int
}

// fields lists t's JSON fields as encoding/json does: embedded structs'
// fields promoted, the shallowest of a name wins.
func (g *apiGen) fields(owner, t reflect.Type) []apiField {
	var all []depthField
	var walk func(t reflect.Type, depth int)
	walk = func(t reflect.Type, depth int) {
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			tag := sf.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			ft := sf.Type
			if sf.Anonymous && name == "" {
				et := ft
				if et.Kind() == reflect.Pointer {
					et = et.Elem()
				}
				if et.Kind() == reflect.Struct {
					walk(et, depth+1)
					continue
				}
			}
			if !sf.IsExported() {
				continue
			}
			if name == "" {
				name = sf.Name
			}
			typ := g.typ(ft)
			if strings.Contains(","+opts+",", ",string,") {
				typ = "str"
			}
			all = append(all, depthField{apiField{json: name, attr: pyAttr(name), typ: typ, doc: g.doc(t, sf.Name),
				optional: strings.Contains(","+opts+",", ",omitempty,") || strings.Contains(","+opts+",", ",omitzero,")}, depth})
		}
	}
	walk(t, 0)
	best := map[string]int{}
	for _, d := range all {
		if b, ok := best[d.f.json]; !ok || d.depth < b {
			best[d.f.json] = d.depth
		}
	}
	var out []apiField
	seen := map[string]bool{}
	for _, d := range all {
		if d.depth != best[d.f.json] || seen[d.f.json] {
			continue
		}
		seen[d.f.json] = true
		out = append(out, d.f)
	}
	return out
}

var pyKeywords = map[string]bool{}

func init() {
	for _, k := range strings.Fields("False None True and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield") {
		pyKeywords[k] = true
	}
}

// pyAttr is a JSON key as a Python attribute: "from" is from_.
func pyAttr(key string) string {
	var b strings.Builder
	for i, r := range key {
		switch {
		case r == '_' || unicode.IsLetter(r) && r < 128 || unicode.IsDigit(r) && i > 0:
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := b.String()
	if pyKeywords[s] {
		s += "_"
	}
	return s
}

// assignNames gives every record a Python name: apiNames's, else the Go
// name capitalised, prefixed with its package's when two would clash.
func (g *apiGen) assignNames() {
	base := map[reflect.Type]string{}
	count := map[string]int{}
	for _, t := range g.order {
		n, ok := g.names[t]
		if !ok {
			n, ok = apiNames[goName(t)]
		}
		if !ok {
			n = capital(t.Name())
		}
		base[t] = n
		count[n]++
	}
	for _, t := range g.order {
		n := base[t]
		if count[n] > 1 {
			if _, fixed := g.names[t]; !fixed {
				n = capital(filepath.Base(t.PkgPath())) + n
			}
		}
		g.records[t].name = n
	}
	byGo := map[string]string{}
	seen := map[string]reflect.Type{}
	for _, t := range g.order {
		r := g.records[t]
		if o, dup := seen[r.name]; dup {
			g.t.Fatalf("comebye api: %v and %v are both %s: add one to apiNames", o, t, r.name)
		}
		seen[r.name] = t
		byGo["@"+goName(t)] = r.name
	}
	for _, t := range g.order {
		r := g.records[t]
		for i, f := range r.fields {
			for k, v := range byGo {
				// A record reference ends at a delimiter: "@pkg.T" then ], }, ? or the end.
				for _, end := range []string{"]", "}", "?", ""} {
					if end == "" {
						if strings.HasSuffix(f.typ, k) {
							f.typ = strings.TrimSuffix(f.typ, k) + v
						}
						continue
					}
					f.typ = strings.ReplaceAll(f.typ, k+end, v+end)
				}
			}
			r.fields[i] = f
		}
	}
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// sortedRecords: the roots first, in apiRoots's order, then the rest by name.
func (g *apiGen) sortedRecords() []*apiRecord {
	var roots, rest []*apiRecord
	isRoot := map[reflect.Type]bool{}
	for _, r := range apiRoots {
		isRoot[r.t] = true
		roots = append(roots, g.records[r.t])
	}
	for _, t := range g.order {
		if !isRoot[t] {
			rest = append(rest, g.records[t])
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].name < rest[j].name })
	return append(roots, rest...)
}

func (g *apiGen) python() string {
	var b strings.Builder
	b.WriteString("# Code generated by internal/server/apitypes_test.go; DO NOT EDIT.\n")
	b.WriteString("# Regenerate: go test ./internal/server -run TestAPITypes -update-api-types\n")
	b.WriteString("# The records of the hub's read routes, from the Go types they answer with.\n\n")
	b.WriteString("from ._record import Record\n")
	recs := g.sortedRecords()
	for _, r := range recs {
		fmt.Fprintf(&b, "\n\nclass %s(Record):\n", r.name)
		doc := r.doc
		if doc == "" {
			doc = r.name + "."
		}
		fmt.Fprintf(&b, "    %s\n", pyString(doc+" (Go: "+goName(r.t)+")"))
		fmt.Fprintf(&b, "    _go = %s\n", pyString(goName(r.t)))
		if len(r.fields) == 0 {
			b.WriteString("    _fields = {}\n")
			continue
		}
		b.WriteString("    _fields = {\n")
		for _, f := range r.fields {
			fmt.Fprintf(&b, "        %s: (%s, %s),\n", pyString(f.attr), pyString(f.json), pyString(f.typ))
		}
		b.WriteString("    }\n")
	}
	b.WriteString("\n\nRECORDS = {\n")
	for _, r := range recs {
		fmt.Fprintf(&b, "    %s: %s,\n", pyString(r.name), r.name)
	}
	b.WriteString("}\n")
	return b.String()
}

func pyString(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b) // a JSON string is a valid Python literal
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// reference is the records as `comebye api types` prints them.
func (g *apiGen) reference() string {
	var b strings.Builder
	b.WriteString("Records (generated from the hub's Go types; T? may be None, [T] is a list, {T} a dict with str keys,\n")
	b.WriteString("time is a timezone-aware UTC datetime, any is plain JSON).\n")
	for _, r := range g.sortedRecords() {
		fmt.Fprintf(&b, "\n%s  (%s)", r.name, goName(r.t))
		if r.doc != "" {
			fmt.Fprintf(&b, "  %s", r.doc)
		}
		b.WriteString("\n")
		w := 0
		for _, f := range r.fields {
			if n := len(f.attr) + 1 + len(f.typ); n > w {
				w = n
			}
		}
		for _, f := range r.fields {
			line := fmt.Sprintf("  %-*s", w, f.attr+" "+f.typ)
			if f.doc != "" {
				line += "  " + f.doc
			}
			b.WriteString(strings.TrimRight(line, " ") + "\n")
		}
	}
	return b.String()
}

// doc is the first sentence of the Go comment on t (field "") or its field.
func (g *apiGen) doc(t reflect.Type, field string) string {
	pkg := t.PkgPath()
	if !strings.HasPrefix(pkg, apiModule) {
		return ""
	}
	if !g.parsed[pkg] {
		g.parsed[pkg] = true
		g.parse(pkg)
	}
	return g.docs[goName(t)][field]
}

func (g *apiGen) parse(pkg string) {
	dir := filepath.Join("..", "..", strings.TrimPrefix(pkg, apiModule))
	fset := gotoken.NewFileSet()
	matches, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			g.t.Fatal(err)
		}
		short := filepath.Base(pkg) + "."
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != gotoken.TYPE {
				continue
			}
			for _, sp := range gd.Specs {
				ts := sp.(*ast.TypeSpec)
				m := map[string]string{}
				cg := ts.Doc
				if cg == nil && len(gd.Specs) == 1 {
					cg = gd.Doc
				}
				m[""] = firstSentence(cg, ts.Name.Name)
				if st, ok := ts.Type.(*ast.StructType); ok {
					for _, fl := range st.Fields.List {
						c := fl.Doc
						if c == nil {
							c = fl.Comment
						}
						for _, n := range fl.Names {
							m[n.Name] = firstSentence(c, n.Name)
						}
					}
				}
				g.docs[short+ts.Name.Name] = m
			}
		}
	}
}

// firstSentence of a comment, without a leading "Name is"/"Name:" echo,
// at most 160 bytes.
func firstSentence(cg *ast.CommentGroup, name string) string {
	if cg == nil {
		return ""
	}
	s := strings.Join(strings.Fields(cg.Text()), " ")
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}
	for _, p := range []string{name + " is ", name + " are ", name + ": ", name + ", ", name + " "} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimPrefix(s, p)
			break
		}
	}
	if len(s) > 160 {
		cut := strings.LastIndex(s[:157], " ")
		if cut < 80 {
			cut = 157
		}
		s = s[:cut] + "..."
	}
	if s != "" {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	return s
}
