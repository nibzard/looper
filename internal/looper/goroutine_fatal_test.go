package looper

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A goroutine that outlives its test and then calls t.Fatal panics the whole
// suite binary: testing prints "Fail in goroutine after TestX has completed"
// and every later result is masked. T111 records the shape from
// TestSubmitRaceReportYieldsToAcknowledgement, whose flip goroutine wrote
// controller metadata through writeControllerMetadata; the write moved to the
// error-returning writeControllerMetadataFile and the test joined the
// goroutine. This guard keeps that class fixed: no go statement in the
// repository's test sources may reach a FailNow-family call, directly or
// through a package-local helper it calls with the test's t. Closures held in
// variables and methods on harness types stay out of a static read's reach;
// the guard still covers both shapes the recorded defect took.

// failNowMethods are the testing.T methods that end the goroutine they run
// on. testing documents them as legal only on the goroutine running the test.
var failNowMethods = map[string]bool{
	"Fatal": true, "Fatalf": true, "FailNow": true,
	"Skip": true, "Skipf": true, "SkipNow": true,
}

// failNowGuard reads the repository's test sources and reports the go
// statements whose goroutine can reach a FailNow-family call.
type failNowGuard struct {
	t    *testing.T
	fset *token.FileSet
	root string
	// helpers are the functions whose first parameter is a *testing.T,
	// indexed by the directory allowed to call them, so a bare name another
	// package also defines is never mistaken for a local helper.
	helpers map[string]map[string]*ast.FuncDecl
}

// at renders a position relative to the repository root, the form findings
// are cited in.
func (g *failNowGuard) at(pos token.Pos) string {
	p := g.fset.Position(pos)
	if rel, err := filepath.Rel(g.root, p.Filename); err == nil && !strings.HasPrefix(rel, "..") {
		p.Filename = rel
	}
	return p.String()
}

func TestGoroutinesDoNotCallFailNow(t *testing.T) {
	g, files := newFailNowGuard(t)
	for path, file := range files {
		dir := filepath.Dir(path)
		ast.Inspect(file, func(n ast.Node) bool {
			stmt, ok := n.(*ast.GoStmt)
			if !ok {
				return true
			}
			names := g.enclosingTestingNames(file, stmt.Pos())
			g.checkCall(stmt.Pos(), stmt.Call, names, g.helpers[dir], map[*ast.FuncDecl]bool{})
			return true
		})
	}
}

func newFailNowGuard(t *testing.T) (*failNowGuard, map[string]*ast.File) {
	t.Helper()
	root := repositoryRoot(t)
	sources := testSourceFiles(t, root)
	g := &failNowGuard{
		t:       t,
		fset:    token.NewFileSet(),
		root:    root,
		helpers: map[string]map[string]*ast.FuncDecl{},
	}
	files := map[string]*ast.File{}
	for _, path := range sources {
		file, err := parser.ParseFile(g.fset, path, nil, 0)
		if err != nil {
			t.Fatalf("%s does not parse: %v", path, err)
		}
		files[path] = file
		dir := filepath.Dir(path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || testingParam(fn) == "" {
				continue
			}
			if g.helpers[dir] == nil {
				g.helpers[dir] = map[string]*ast.FuncDecl{}
			}
			g.helpers[dir][fn.Name.Name] = fn
		}
	}
	return g, files
}

// checkCall follows one call a goroutine makes. origin is the go statement's
// position; names holds the *testing.T variables in scope where it appears;
// helpers are the package-local helpers it may call; visited breaks helper
// cycles.
func (g *failNowGuard) checkCall(origin token.Pos, call *ast.CallExpr,
	names map[string]bool, helpers map[string]*ast.FuncDecl, visited map[*ast.FuncDecl]bool) {
	if literal, ok := call.Fun.(*ast.FuncLit); ok {
		g.checkBody(origin, nil, literal.Body, names, helpers, visited)
		return
	}
	var name string
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		name = fun.Name
	case *ast.SelectorExpr:
		name = fun.Sel.Name
	default:
		return
	}
	fn, ok := helpers[name]
	if !ok || visited[fn] {
		return
	}
	passesT := false
	for _, arg := range call.Args {
		if id, ok := arg.(*ast.Ident); ok && names[id.Name] {
			passesT = true
		}
	}
	if !passesT {
		return
	}
	visited[fn] = true
	g.checkBody(origin, fn, fn.Body, names, helpers, visited)
}

// checkBody scans one goroutine or helper body. owner is the helper the body
// belongs to, whose own t parameter is the only t in scope there.
func (g *failNowGuard) checkBody(origin token.Pos, owner *ast.FuncDecl, body *ast.BlockStmt,
	names map[string]bool, helpers map[string]*ast.FuncDecl, visited map[*ast.FuncDecl]bool) {
	scope := names
	if owner != nil {
		scope = map[string]bool{testingParam(owner): true}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if recv, ok := sel.X.(*ast.Ident); ok && failNowMethods[sel.Sel.Name] && scope[recv.Name] {
				where := g.at(origin)
				if owner != nil && testingParam(owner) == recv.Name {
					where += " through " + owner.Name.Name + " at " + g.at(call.Pos())
				} else {
					where += " at " + g.at(call.Pos())
				}
				g.t.Errorf("%s: this goroutine can call %s.%s, which ends a goroutine the test is "+
					"not running; return the error to the test goroutine and report it there",
					where, recv.Name, sel.Sel.Name)
			}
			return true
		}
		g.checkCall(origin, call, scope, helpers, visited)
		return true
	})
}

// enclosingTestingNames returns the names of the *testing.T parameters of the
// declaration the position sits in, the t a goroutine there can capture.
func (g *failNowGuard) enclosingTestingNames(file *ast.File, pos token.Pos) map[string]bool {
	names := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || pos < fn.Pos() || pos > fn.End() {
			continue
		}
		if param := testingParam(fn); param != "" {
			names[param] = true
		}
	}
	return names
}

// testingParam reports the name of fn's first parameter when that parameter
// is a *testing.T, else the empty string.
func testingParam(fn *ast.FuncDecl) string {
	if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
		return ""
	}
	field := fn.Type.Params.List[0]
	if len(field.Names) == 0 || !isTestingT(field.Type) {
		return ""
	}
	return field.Names[0].Name
}

func isTestingT(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "T" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing"
}

// repositoryRoot walks up from this file to the directory holding go.mod.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test file location is unavailable")
	}
	root := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatalf("no go.mod above %s", thisFile)
		}
		root = parent
	}
}

// testSourceFiles lists every Go test file in the repository, skipping the
// generated trees, anchored at the root so fixtures that chdir elsewhere
// still find the tree.
func testSourceFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			if strings.HasSuffix(entry.Name(), "_test.go") {
				files = append(files, path)
			}
			return nil
		}
		switch name := entry.Name(); {
		case strings.HasPrefix(name, "."), name == "build", name == "node_modules",
			strings.HasPrefix(name, "looper_"), name == "to-do.json.context", name == "__pycache__":
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no test sources found; the guard cannot run")
	}
	return files
}
