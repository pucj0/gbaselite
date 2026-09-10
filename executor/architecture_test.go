package executor

import (
	"fmt"
	"gbaselite/storageengine"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Parse all source variants, not only the host GOOS build. Archived modules and
// non-source artifacts are excluded; migration exceptions are function-specific.
func architectureViolations(path string, src any) []string {
	f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		return []string{err.Error()}
	}
	var issues []string
	imports := map[string]string{}
	runtime := strings.HasPrefix(path, "executor/") || strings.HasPrefix(path, "server/") || strings.HasPrefix(path, "sql/") || strings.HasPrefix(path, "planner/") || strings.HasPrefix(path, "physical/")
	for _, imp := range f.Imports {
		name, _ := strconv.Unquote(imp.Path.Value)
		alias := filepath.Base(name)
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		imports[alias] = name
		if alias == "." && strings.HasPrefix(name, "gbaselite/") {
			issues = append(issues, "dot import bypasses architecture checks")
		}
		if runtime && (strings.Contains(name, "bbolt") || name == "gbaselite/mvcc" || strings.HasPrefix(name, "gbaselite/storageengine/mvccadapter") || strings.Contains(name, "legacy") || name == "gbaselite/replication") {
			issues = append(issues, "concrete backend import: "+name)
		}
		if name == "gbaselite/storageengine/testkit" {
			issues = append(issues, "test backend imported by production")
		}
		if runtime && name == "gbaselite/enginefactory" && path != "executor/open_options.go" {
			issues = append(issues, "backend factory outside composition root")
		}
	}
	for _, decl := range f.Decls {
		fn := ""
		if d, ok := decl.(*ast.FuncDecl); ok {
			fn = d.Name.Name
		}
		loader := path == "executor/legacy_reader.go" && fn == "loadLegacyForMigration"
		migration := path == "executor/legacy_migration.go"
		ast.Inspect(decl, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.SelectorExpr:
				if id, ok := n.X.(*ast.Ident); ok && imports[id.Name] == "gbaselite/storage" && strings.Contains(n.Sel.Name, "Persistence") && !loader {
					issues = append(issues, "legacy persistence outside migration reader")
				}
				if runtime && n.Sel.Name == "StorageMode" && !(path == "executor/open_options.go" && fn == "OpenWithOptions") {
					issues = append(issues, "runtime mode dispatch")
				}
			case *ast.Ident:
				if n.Name == "loadLegacyForMigration" && !loader && !(migration && fn == "MigrateLegacy") {
					issues = append(issues, "migration reader reachable from runtime")
				}
				if strings.HasPrefix(n.Name, "openLegacy") || n.Name == "legacyEngine" || n.Name == "legacyTransaction" {
					issues = append(issues, "legacy runtime symbol")
				}
				if (path == "executor/physical_select.go" || path == "executor/physical_join.go" || path == "executor/physical_binding.go") && (n.Name == "sourceOperator" || n.Name == "rowSource") {
					issues = append(issues, "operator callback roundtrip")
				}
			case *ast.BasicLit:
				if runtime && !loader && !migration && n.Kind == token.STRING {
					v, _ := strconv.Unquote(n.Value)
					if v == "snapshot" || v == "paged" {
						issues = append(issues, "legacy runtime mode literal")
					}
				}
			}
			return true
		})
	}
	return issues
}
func TestProductionArchitectureBoundaries(t *testing.T) {
	root := ".."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." {
				name := d.Name()
				if strings.HasPrefix(name, ".") || name == "data" || name == "logs" || name == "bin" || name == "dist" || name == "release" || name == "node_modules" || name == "vendor" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, issue := range architectureViolations(rel, source) {
			t.Errorf("%s: %s", rel, issue)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestArchitectureChecksRejectRegressionFixtures(t *testing.T) {
	fixtures := []struct{ path, source string }{
		{"executor/bad.go", "package executor; import disk \"gbaselite/storage\"; func f(){_ = disk.NewPagedPersistence(\"x\",0)}"},
		{"executor/bad.go", "package executor; import . \"gbaselite/storage\"; var _ = NewPersistence"},
		{"executor/legacy_reader.go", "package executor; func runtime(){loadLegacyForMigration(\"x\",\"paged\")}"},
		{"executor/bad.go", "package executor; import _ \"gbaselite/storageengine/mvccadapter\""},
		{"physical/bad.go", "package physical; import _ \"gbaselite/storageengine/testkit\""},
		{"server/bad.go", "package server; func f(){switch options.StorageMode{case \"paged\":}}"},
		{"executor/physical_select.go", "package executor; func f(){sourceOperator(rowSource(ctx,op))}"},
		{"executor/physical_binding.go", "package executor; func f(){sourceOperator(callback)}"},
	}
	for _, f := range fixtures {
		if _, err := parser.ParseFile(token.NewFileSet(), f.path, f.source, 0); err != nil {
			t.Fatal("invalid regression fixture", err)
		}
		if got := architectureViolations(f.path, f.source); len(got) == 0 {
			t.Errorf("accepted %s", f.source)
		}
	}
	if got := architectureViolations("executor/legacy_reader.go", "package executor; import s \"gbaselite/storage\"; func loadLegacyForMigration(){_ = s.NewPersistence(\"x\")}"); len(got) > 0 {
		t.Fatal(got)
	}
}
func TestRuntimeRejectsLegacyBeforeOpeningBackend(t *testing.T) {
	for _, mode := range []string{"snapshot", "paged", "SNAPSHOT", " paged "} {
		called := false
		_, err := OpenWithOptions(t.TempDir(), "root", "test", OpenOptions{StorageMode: mode, BackendFactory: func(string, storageengine.Options) (storageengine.Engine, error) {
			called = true
			return nil, fmt.Errorf("factory should not run")
		}})
		if err == nil || called {
			t.Fatal(mode, err, called)
		}
	}
}
