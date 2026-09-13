package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMandatoryReportCLITestInventoryNamesExistingTests(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "usage", "reportcli", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no reportcli test files found")
	}

	available := make(map[string]struct{})
	for _, path := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil {
				available[function.Name.Name] = struct{}{}
			}
		}
	}

	const prefix = "internal/usage/reportcli:"
	for _, required := range mandatoryTests(runtime.GOOS) {
		name, found := strings.CutPrefix(required, prefix)
		if !found || strings.Contains(name, "/") {
			continue
		}
		if _, found := available[name]; !found {
			t.Errorf("mandatory test %s does not name an existing reportcli test", required)
		}
	}
}
