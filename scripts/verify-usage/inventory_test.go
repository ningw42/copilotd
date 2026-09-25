package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"unicode"
	"unicode/utf8"
)

// TestMandatoryTestInventoryNamesExistingTests checks every GOOS inventory on
// any host, so a renamed mandatory test fails here before native CI.
func TestMandatoryTestInventoryNamesExistingTests(t *testing.T) {
	root := os.DirFS(filepath.Join("..", ".."))
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if err := checkTestInventory(root, goos, mandatoryTests(goos)); err != nil {
			t.Error(err)
		}
	}
}

func TestInventoryCheckerMatchesTopLevelTestDeclarations(t *testing.T) {
	source := func(body string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("package sample\n\nimport \"testing\"\n\n" + body)}
	}
	tree := fstest.MapFS{
		"internal/sample/sample_test.go": source(`func TestPresent(t *testing.T) {}
func Test(*testing.T) {}
func Test_underscore(t *testing.T) {}
func requirePresent(t *testing.T) {}
type suite struct{}
func (suite) TestMethod(t *testing.T) {}
func BenchmarkPresent(b *testing.B) {}
func TestMain(m *testing.M) {}
func Testlowercase(t *testing.T) {}
func TestNoParameter() {}
func TestBenchmarkParameter(b *testing.B) {}
func TestValueParameter(t testing.T) {}
func TestExtraParameter(t *testing.T, name string) {}
func TestTwoNamedParameters(t, u *testing.T) {}
func TestResult(t *testing.T) error { return nil }
func TestTypeParameter[T any](t *testing.T) {}
`),
		"internal/sample/external_test.go": &fstest.MapFile{Data: []byte(`package sample_test

import check "testing"

func TestAliasedImport(t *check.T) {}
`)},
		"internal/sample/dot_test.go": &fstest.MapFile{Data: []byte(`package sample

import . "testing"

func TestDotImport(t *T) {}
func TestDotImportBenchmark(b *B) {}
`)},
		"internal/sample/nested/nested_test.go": source("func TestNested(t *testing.T) {}\n"),
		"internal/production/production.go":     &fstest.MapFile{Data: []byte("package production\n")},
		"internal/broken/broken_test.go":        &fstest.MapFile{Data: []byte("package broken\n\nfunc TestBroken(t *testing.T) {\n")},
		"internal/broken/valid_test.go":         source("func TestValid(t *testing.T) {}\n"),
	}
	for _, tc := range []struct {
		name   string
		entry  string
		reject string // failure reason; empty when the entry is valid
	}{
		{name: "missing parent through subtest", entry: "internal/sample:TestMissing/case", reject: "not declared as a top-level test function"},
		{name: "missing parent through nested subtest", entry: "internal/sample:TestMissing/case/nested", reject: "not declared as a top-level test function"},
		{name: "present parent", entry: "internal/sample:TestPresent"},
		{name: "present parent through subtest", entry: "internal/sample:TestPresent/undiscovered/nested"},
		{name: "bare Test", entry: "internal/sample:Test"},
		{name: "Test followed by non-lowercase", entry: "internal/sample:Test_underscore"},
		{name: "aliased testing import", entry: "internal/sample:TestAliasedImport"},
		{name: "helper", entry: "internal/sample:requirePresent", reject: "not declared as a top-level test function"},
		{name: "method", entry: "internal/sample:TestMethod", reject: "not declared as a top-level test function"},
		{name: "benchmark", entry: "internal/sample:BenchmarkPresent", reject: "not declared as a top-level test function"},
		{name: "TestMain", entry: "internal/sample:TestMain", reject: "not declared as a top-level test function"},
		{name: "Test followed by lowercase", entry: "internal/sample:Testlowercase", reject: "not declared as a top-level test function"},
		{name: "no parameter", entry: "internal/sample:TestNoParameter", reject: "not declared as a top-level test function"},
		{name: "benchmark parameter", entry: "internal/sample:TestBenchmarkParameter", reject: "not declared as a top-level test function"},
		{name: "value parameter", entry: "internal/sample:TestValueParameter", reject: "not declared as a top-level test function"},
		{name: "extra parameter", entry: "internal/sample:TestExtraParameter", reject: "not declared as a top-level test function"},
		{name: "two named parameters", entry: "internal/sample:TestTwoNamedParameters", reject: "not declared as a top-level test function"},
		{name: "result", entry: "internal/sample:TestResult", reject: "not declared as a top-level test function"},
		{name: "type parameter", entry: "internal/sample:TestTypeParameter", reject: "not declared as a top-level test function"},
		{name: "dot-imported testing", entry: "internal/sample:TestDotImport"},
		{name: "dot-imported benchmark parameter", entry: "internal/sample:TestDotImportBenchmark", reject: "not declared as a top-level test function"},
		{name: "nested package", entry: "internal/sample/nested:TestNested"},
		{name: "test declared only in nested package", entry: "internal/sample:TestNested", reject: "not declared as a top-level test function"},
		{name: "no separator", entry: "internal/sample.TestPresent", reject: "malformed entry"},
		{name: "empty package", entry: ":TestPresent", reject: "malformed entry"},
		{name: "empty test", entry: "internal/sample:", reject: "malformed entry"},
		{name: "empty parent", entry: "internal/sample:/case", reject: "malformed entry"},
		{name: "dot-prefixed package", entry: "./internal/sample:TestPresent", reject: "not an exact module-relative package path"},
		{name: "trailing slash package", entry: "internal/sample/:TestPresent", reject: "not an exact module-relative package path"},
		{name: "doubled slash package", entry: "internal//sample:TestPresent", reject: "not an exact module-relative package path"},
		{name: "dot-dot package", entry: "internal/other/../sample:TestPresent", reject: "not an exact module-relative package path"},
		{name: "absolute package", entry: "/internal/sample:TestPresent", reject: "not an exact module-relative package path"},
		{name: "backslash package", entry: `internal\sample:TestPresent`, reject: "not an exact module-relative package path"},
		{name: "module-qualified package", entry: "github.com/ningw42/copilotd/internal/sample:TestPresent", reject: "does not exist"},
		{name: "differently cased package", entry: "internal/Sample:TestPresent", reject: "does not exist"},
		{name: "missing package directory", entry: "internal/missing:TestPresent", reject: "does not exist"},
		{name: "package without test source", entry: "internal/production:TestPresent", reject: "no _test.go files"},
		{name: "unparseable test source", entry: "internal/broken:TestValid", reject: "parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkTestInventory(tree, "plan9", []string{tc.entry})
			if tc.reject == "" {
				if err != nil {
					t.Fatalf("rejected %s: %v", tc.entry, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %s", tc.entry)
			}
			for _, want := range []string{tc.entry, "plan9", tc.reject} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("failure %q does not contain %q", err, want)
				}
			}
		})
	}
}

// checkTestInventory reports every required "package:Test[/subtest]" entry
// whose parent test function is not declared in the package's test sources
// under fsys, a view of the module root. Build constraints are ignored and
// subtest suffixes are left to native accounting.
func checkTestInventory(fsys fs.FS, goos string, required []string) error {
	packages := make(map[string]testPackage)
	var failures []error
	for _, entry := range required {
		if err := checkInventoryEntry(fsys, packages, entry); err != nil {
			failures = append(failures, fmt.Errorf("GOOS=%s mandatory test %s: %w", goos, entry, err))
		}
	}
	return errors.Join(failures...)
}

type testPackage struct {
	declared map[string]bool
	err      error
}

func checkInventoryEntry(fsys fs.FS, packages map[string]testPackage, entry string) error {
	pkg, name, found := strings.Cut(entry, ":")
	parent, _, _ := strings.Cut(name, "/")
	if !found || pkg == "" || parent == "" {
		return errors.New("malformed entry, want package:Test[/subtest]")
	}
	// Native accounting keys tests by the exact module-relative import path,
	// so any other spelling of it could never match a passing test.
	if !fs.ValidPath(pkg) || pkg == "." || strings.Contains(pkg, `\`) {
		return fmt.Errorf("package %q is not an exact module-relative package path", pkg)
	}
	tests, cached := packages[pkg]
	if !cached {
		tests.declared, tests.err = declaredTests(fsys, pkg)
		packages[pkg] = tests
	}
	if tests.err != nil {
		return tests.err
	}
	if !tests.declared[parent] {
		return fmt.Errorf("%s is not declared as a top-level test function", parent)
	}
	return nil
}

// declaredTests parses every _test.go file directly in dir and returns the
// names of the top-level test functions they declare.
func declaredTests(fsys fs.FS, dir string) (map[string]bool, error) {
	if err := exactDirectory(fsys, dir); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	declared := make(map[string]bool)
	sources := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		sources++
		name := path.Join(dir, entry.Name())
		src, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse test source: %w", err)
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && testFunction(function) {
				declared[function.Name.Name] = true
			}
		}
	}
	if sources == 0 {
		return nil, fmt.Errorf("package directory %s has no _test.go files", dir)
	}
	return declared, nil
}

// exactDirectory requires every element of dir to exist under exactly that
// spelling, so case-insensitive hosts reject the same entries as Linux.
func exactDirectory(fsys fs.FS, dir string) error {
	parent := "."
	for _, element := range strings.Split(dir, "/") {
		entries, err := fs.ReadDir(fsys, parent)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(entries, func(entry fs.DirEntry) bool { return entry.IsDir() && entry.Name() == element }) {
			return fmt.Errorf("package directory %s does not exist", dir)
		}
		parent = path.Join(parent, element)
	}
	return nil
}

// testFunction reports whether function has the shape the go command runs as
// a test: a Test-prefixed name not followed by a lowercase letter, no receiver,
// results or type parameters, and one *T or *pkg.T parameter. Like the go
// command this is syntactic; compiling the test binary rejects any other T.
// TestMain is never a mandatory test.
func testFunction(function *ast.FuncDecl) bool {
	suffix, found := strings.CutPrefix(function.Name.Name, "Test")
	if !found || function.Name.Name == "TestMain" || function.Recv != nil || function.Type.TypeParams.NumFields() != 0 || function.Type.Results.NumFields() != 0 {
		return false
	}
	if first, _ := utf8.DecodeRuneInString(suffix); suffix != "" && unicode.IsLower(first) {
		return false
	}
	if function.Type.Params.NumFields() != 1 {
		return false
	}
	pointer, ok := function.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch named := pointer.X.(type) {
	case *ast.Ident:
		return named.Name == "T"
	case *ast.SelectorExpr:
		return named.Sel.Name == "T"
	}
	return false
}
