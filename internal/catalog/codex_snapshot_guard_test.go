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

// vendoredCodexSnapshotTestFiles are the only catalog test files that may read
// the embedded Codex snapshot or a value derived from it. Every other test runs
// on synthetic Codex catalogs, so a routine vendored-floor bump edits no test.
var vendoredCodexSnapshotTestFiles = map[string]string{
	"codex_compatibility_test.go":        "identity, startup load, and round-trip fidelity of the exact vendored bytes",
	"codex_binary_compatibility_test.go": "opt-in executable audit of the pinned Codex client against vendored entries",
	"codex_models_cache_test.go":         "the Codex Models cache's embedded floor, compared by identity only",
}

func TestOnlyVendoredSnapshotTestsReadTheEmbeddedCodexCatalog(t *testing.T) {
	files := parseCatalogTestFiles(t)
	derived := vendoredCodexDerivedNames(files)

	referencing := make(map[string]bool, len(vendoredCodexSnapshotTestFiles))
	var violations []string
	for name, file := range files {
		declared := declaredNames(file.syntax)
		ast.Inspect(file.syntax, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if !ok || !derived[ident.Name] || declared[ident] {
				return true
			}
			referencing[name] = true
			if _, allowed := vendoredCodexSnapshotTestFiles[name]; !allowed {
				violations = append(violations, file.fset.Position(ident.Pos()).String()+" references "+ident.Name)
			}
			return true
		})
	}
	sort.Strings(violations)
	for _, violation := range violations {
		t.Errorf("%s; logic tests must run on synthetic Codex catalogs", violation)
	}
	for name := range vendoredCodexSnapshotTestFiles {
		if !referencing[name] {
			t.Errorf("allow-listed %s no longer reads the vendored Codex snapshot; remove it from the allow-list", name)
		}
	}
}

type parsedTestFile struct {
	fset   *token.FileSet
	syntax *ast.File
}

func parseCatalogTestFiles(t *testing.T) map[string]parsedTestFile {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("list catalog package: %v", err)
	}
	fset := token.NewFileSet()
	files := make(map[string]parsedTestFile)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		syntax, err := parser.ParseFile(fset, entry.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		files[entry.Name()] = parsedTestFile{fset: fset, syntax: syntax}
	}
	if len(files) == 0 {
		t.Fatal("found no catalog test files to guard")
	}
	return files
}

// vendoredCodexDerivedNames returns the embedded Codex bytes' identifier plus
// every package-level test declaration that reads them, directly or through
// another such declaration.
func vendoredCodexDerivedNames(files map[string]parsedTestFile) map[string]bool {
	type declaration struct {
		names []string
		body  ast.Node
	}
	var declarations []declaration
	for _, file := range files {
		for _, decl := range file.syntax.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Body != nil {
					declarations = append(declarations, declaration{names: []string{decl.Name.Name}, body: decl.Body})
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					var names []string
					for _, name := range value.Names {
						if name.Name != "_" {
							names = append(names, name.Name)
						}
					}
					if len(names) > 0 {
						declarations = append(declarations, declaration{names: names, body: value})
					}
				}
			}
		}
	}

	derived := map[string]bool{"embeddedCodexModels": true}
	for changed := true; changed; {
		changed = false
		for _, declaration := range declarations {
			if derived[declaration.names[0]] || !referencesAny(declaration.body, derived) {
				continue
			}
			for _, name := range declaration.names {
				derived[name] = true
			}
			changed = true
		}
	}
	return derived
}

// declaredNames returns the identifiers that name package-level functions and
// values, so the guard reports only references.
func declaredNames(file *ast.File) map[*ast.Ident]bool {
	declared := make(map[*ast.Ident]bool)
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			declared[decl.Name] = true
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				if value, ok := spec.(*ast.ValueSpec); ok {
					for _, name := range value.Names {
						declared[name] = true
					}
				}
			}
		}
	}
	return declared
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
