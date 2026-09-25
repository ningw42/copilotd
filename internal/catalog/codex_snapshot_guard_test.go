package catalog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// vendoredSnapshotTestFiles are the only catalog test files that may read the
// vendored snapshot, its release record, or a value derived from either. Every
// other test runs on synthetic Codex models, so a routine vendored-floor bump
// edits no test.
var vendoredSnapshotTestFiles = map[string]string{
	"codex_compatibility_test.go":        "identity, startup load, and round-trip fidelity of the exact vendored bytes",
	"codex_binary_compatibility_test.go": "opt-in executable audit of the pinned Codex client against vendored entries",
	"codex_models_cache_test.go":         "the Codex Models cache's embedded floor, compared by identity only",
}

func TestOnlyAllowListedTestsReadTheVendoredSnapshot(t *testing.T) {
	fset, files := parseCatalogPackageFiles(t)
	derived := vendoredSnapshotDerivedNames(files)

	referencing := make(map[string]bool, len(vendoredSnapshotTestFiles))
	var violations []string
	for name, file := range files {
		if !strings.HasSuffix(name, "_test.go") {
			continue
		}
		declared := make(map[*ast.Ident]bool)
		for _, declaration := range packageDeclarations(file) {
			for _, ident := range declaration.names {
				declared[ident] = true
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if !ok || !derived[ident.Name] || declared[ident] {
				return true
			}
			referencing[name] = true
			if _, allowed := vendoredSnapshotTestFiles[name]; !allowed {
				violations = append(violations, fset.Position(ident.Pos()).String()+" references "+ident.Name)
			}
			return true
		})
	}
	sort.Strings(violations)
	for _, violation := range violations {
		t.Errorf("%s; logic tests must run on synthetic Codex models", violation)
	}
	for name, reason := range vendoredSnapshotTestFiles {
		if !referencing[name] {
			t.Errorf("allow-listed %s (%s) no longer reads the vendored snapshot; remove it from the allow-list", name, reason)
		}
	}
}

// parseCatalogPackageFiles parses every Go file in the catalog package, so
// derivation follows production declarations such as NewModelsCache.
func parseCatalogPackageFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("list catalog package: %v", err)
	}
	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		files[entry.Name()] = file
	}
	if len(files) == 0 {
		t.Fatal("found no catalog files to guard")
	}
	return fset, files
}

type packageDeclaration struct {
	names []*ast.Ident
	body  ast.Node
}

// packageDeclarations returns the file's package-level functions and values,
// each with the syntax that may read other package-level names.
func packageDeclarations(file *ast.File) []packageDeclaration {
	var declarations []packageDeclaration
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			declarations = append(declarations, packageDeclaration{names: []*ast.Ident{decl.Name}, body: decl.Body})
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				if value, ok := spec.(*ast.ValueSpec); ok {
					declarations = append(declarations, packageDeclaration{names: value.Names, body: value})
				}
			}
		}
	}
	return declarations
}

// vendoredSnapshotDerivedNames returns the embedded vendored bytes and release
// record plus every package-level declaration, production or test, that reads
// them directly or through another such declaration.
func vendoredSnapshotDerivedNames(files map[string]*ast.File) map[string]bool {
	var declarations []packageDeclaration
	for _, file := range files {
		declarations = append(declarations, packageDeclarations(file)...)
	}
	derived := map[string]bool{"embeddedCodexModels": true, "embeddedCodexReleaseJSON": true}
	for changed := true; changed; {
		changed = false
		for _, declaration := range declarations {
			if declaration.body == nil || !referencesAny(declaration.body, derived) {
				continue
			}
			for _, ident := range declaration.names {
				if ident.Name != "_" && !derived[ident.Name] {
					derived[ident.Name] = true
					changed = true
				}
			}
		}
	}
	return derived
}

func referencesAny(node ast.Node, names map[string]bool) bool {
	found := false
	ast.Inspect(node, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && names[ident.Name] {
			found = true
		}
		return !found
	})
	return found
}
