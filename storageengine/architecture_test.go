package storageengine_test

import (
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"

	"gbaselite/storageengine"
)

// architecture_test.go is T018's boundary check for the neutral locking surface.
//
// The neutral layer's whole value is that SQL can compose locking DTOs without depending on a
// backend: the moment storageengine itself imports bbolt, the MVCC store, the lock manager,
// replication or the adapter, every alternative backend and every SQL-only build would have to
// link a concrete implementation too. This test states that boundary twice, because an import
// and a field type are two different ways for the dependency to appear:
//
//   - the source check refuses a forbidden import in any non-test file of this package;
//   - the structural check refuses a forbidden package in the type of any exported neutral
//     locking DTO, including through pointers, slices, maps and function signatures.
//
// Test files are excluded from the source check on purpose: the tests in this directory are
// exactly where the neutral vocabulary is compared against the concrete implementations.

// t018ForbiddenNeutralImports are the packages no non-test file of the neutral storage
// contract may import. The lock manager and the MVCC store are the concrete B02
// implementations, bbolt is the physical engine, replication and the adapter are composition,
// and enginefactory is the application root.
var t018ForbiddenNeutralImports = []string{
	"go.etcd.io/bbolt",
	"gbaselite/mvcc",
	"gbaselite/lockmanager",
	"gbaselite/replication",
	"gbaselite/storageengine/mvccadapter",
	"gbaselite/enginefactory",
}

// TestB02T018NeutralPackageNeverImportsABackend walks every non-test source file of this
// package directory and reports any forbidden import.
//
// The walk is deliberately local -- os.ReadDir(".") plus go/parser -- so it inspects exactly
// the directory it claims to: a check that resolved some other path could pass while the files
// next to it imported a backend.
func TestB02T018NeutralPackageNeverImportsABackend(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the storageengine package directory: %v", err)
	}
	forbidden := make(map[string]bool, len(t018ForbiddenNeutralImports))
	for _, path := range t018ForbiddenNeutralImports {
		forbidden[path] = true
	}

	fset := token.NewFileSet()
	checked := 0
	declarations := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		checked++
		declarations += len(file.Imports)
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			if forbidden[path] {
				t.Errorf("storageengine/%s imports %q; the neutral contract must not depend on a concrete backend", entry.Name(), path)
			}
		}
	}
	// A silent zero would turn this test into a no-op after a layout change, so the coverage
	// itself is asserted rather than assumed: files were found, and the parser actually read
	// import declarations out of them instead of returning empty files.
	if checked == 0 {
		t.Fatal("no non-test source file was found in the storageengine package")
	}
	if declarations == 0 {
		t.Fatal("the import scan read no import declaration at all, so it proves nothing")
	}
}

// TestB02T018NeutralLockingDTOsCarryNoBackendType checks the exported neutral locking DTOs
// structurally.
//
// An import is not the only way a backend type reaches a caller: a field whose type lives in
// the lock manager, the MVCC store or bbolt would expose that package through the neutral
// struct even with a clean import list, because the caller would have to name the type to
// build or read the field. The check is recursive, so it also covers a field reached through a
// slice, a pointer, a map or a function signature -- the shapes a DTO actually uses.
func TestB02T018NeutralLockingDTOsCarryNoBackendType(t *testing.T) {
	// Every exported neutral locking type, not only the structs: the string-typed enums are
	// part of the vocabulary a caller composes, so they are checked the same way.
	types := []any{
		storageengine.LockGrant{},
		storageengine.LockComponent{},
		storageengine.LockRequest{},
		storageengine.CurrentObservation{},
		storageengine.LockScopeKind(""),
		storageengine.LockStats{},
		storageengine.WaitPage{},
		storageengine.LockMode(""),
		storageengine.LockComponentKind(""),
	}
	for _, value := range types {
		typ := reflect.TypeOf(value)
		t018RequireNoBackendField(t.Errorf, typ, typ.String(), make(map[reflect.Type]bool))
	}

	// A green walk is only meaningful if the recursion actually ran, and the two container
	// shapes are proved separately: LockComponent is reachable from LockGrant only through the
	// Components slice, and WaitRow from WaitPage only through the Rows slice. Finding either
	// nested type in the visited set shows the walk descended into a slice element instead of
	// stopping at the top-level struct.
	nested := make(map[reflect.Type]bool)
	t018RequireNoBackendField(t.Errorf, reflect.TypeOf(storageengine.LockGrant{}), "LockGrant", nested)
	if !nested[reflect.TypeOf(storageengine.LockComponent{})] {
		t.Fatal("the structural walk did not descend into the LockGrant.Components slice")
	}
	nested = make(map[reflect.Type]bool)
	t018RequireNoBackendField(t.Errorf, reflect.TypeOf(storageengine.WaitPage{}), "WaitPage", nested)
	if !nested[reflect.TypeOf(storageengine.WaitRow{})] {
		t.Fatal("the structural walk did not descend into the WaitPage.Rows slice")
	}
	// The forbidden-package list itself is exercised without naming a forbidden type: a test
	// file of this package may not import one (the sibling dependency test scans test files
	// too), so the list is checked by membership rather than by a real violation.
	if !t018ForbiddenNeutralPackage("gbaselite/lockmanager") {
		t.Fatal("the forbidden list does not contain gbaselite/lockmanager")
	}
	if t018ForbiddenNeutralPackage("gbaselite/storageengine") {
		t.Fatal("the forbidden list contains the neutral package itself")
	}
}

// t018ForbiddenNeutralPackage reports whether one package path is on the forbidden list.
func t018ForbiddenNeutralPackage(path string) bool {
	for _, forbidden := range t018ForbiddenNeutralImports {
		if path == forbidden {
			return true
		}
	}
	return false
}

// t018RequireNoBackendField walks one type and reports when it, or any type reachable from it,
// belongs to a forbidden package. visited stops the walk at a type it has already seen, which
// is what makes the recursion safe for the self-referential shapes a standard library struct
// can contain.
func t018RequireNoBackendField(report func(string, ...any), typ reflect.Type, where string, visited map[reflect.Type]bool) {
	if typ == nil || visited[typ] {
		return
	}
	visited[typ] = true
	if path := typ.PkgPath(); t018ForbiddenNeutralPackage(path) {
		report("%s has type %s from %q, which the neutral locking contract must not expose", where, typ, path)
	}
	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		t018RequireNoBackendField(report, typ.Elem(), where+" (element)", visited)
	case reflect.Map:
		t018RequireNoBackendField(report, typ.Key(), where+" (key)", visited)
		t018RequireNoBackendField(report, typ.Elem(), where+" (value)", visited)
	case reflect.Chan:
		t018RequireNoBackendField(report, typ.Elem(), where+" (element)", visited)
	case reflect.Func:
		for i := 0; i < typ.NumIn(); i++ {
			t018RequireNoBackendField(report, typ.In(i), where+" (parameter)", visited)
		}
		for i := 0; i < typ.NumOut(); i++ {
			t018RequireNoBackendField(report, typ.Out(i), where+" (result)", visited)
		}
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			t018RequireNoBackendField(report, field.Type, where+"."+field.Name, visited)
		}
	}
}
