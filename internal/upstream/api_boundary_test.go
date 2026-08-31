package upstream_test

import (
	"go/build"
	"testing"
)

func TestProductionImportsStayWithinLeafAllowlist(t *testing.T) {
	pkg, err := build.Default.ImportDir(".", build.IgnoreVendor)
	if err != nil {
		t.Fatalf("load internal/upstream: %v", err)
	}

	allowed := map[string]bool{
		"github.com/ningw42/copilotd/internal/apierror":       true,
		"github.com/ningw42/copilotd/internal/endpoint":       true,
		"github.com/ningw42/copilotd/internal/identity":       true,
		"github.com/ningw42/copilotd/internal/logging":        true,
		"github.com/ningw42/copilotd/internal/requestsummary": true,
	}
	for _, importPath := range pkg.Imports {
		if allowed[importPath] {
			continue
		}
		dependency, err := build.Default.Import(importPath, ".", build.FindOnly)
		if err == nil && dependency.Goroot {
			continue
		}
		t.Errorf("production import %q is outside the internal/upstream direct-import allowlist", importPath)
	}
}

func TestForwardProductionImportsExcludeCatalog(t *testing.T) {
	pkg, err := build.Default.ImportDir("../forward", build.IgnoreVendor)
	if err != nil {
		t.Fatalf("load internal/forward: %v", err)
	}

	const forbidden = "github.com/ningw42/copilotd/internal/catalog"
	for _, importPath := range pkg.Imports {
		if importPath == forbidden {
			t.Fatalf("internal/forward imports forbidden dependency %q", forbidden)
		}
	}
}
