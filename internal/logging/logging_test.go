package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/ningw42/copilotd/internal/config"
)

func textCfg(level string) config.ServeConfig {
	return config.ServeConfig{LogLevel: level, LogFormat: "text"}
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewWithWriter(&buf, textCfg("warn"))
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	logger.Info("this should be filtered")
	logger.Warn("this should appear")

	out := buf.String()
	if strings.Contains(out, "this should be filtered") {
		t.Errorf("info line emitted at warn level:\n%s", out)
	}
	if !strings.Contains(out, "this should appear") {
		t.Errorf("warn line missing:\n%s", out)
	}
}

func TestNewLoggerFormatSelection(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := NewWithWriter(&buf, config.ServeConfig{LogLevel: "info", LogFormat: "json"})
		if err != nil {
			t.Fatal(err)
		}
		logger.Info("hello")
		var rec map[string]any
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
		}
		if rec["msg"] != "hello" {
			t.Errorf("msg = %v, want hello", rec["msg"])
		}
	})
	t.Run("text", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := NewWithWriter(&buf, textCfg("info"))
		if err != nil {
			t.Fatal(err)
		}
		logger.Info("hello")
		if !strings.Contains(buf.String(), "msg=hello") {
			t.Errorf("text output missing msg=hello:\n%s", buf.String())
		}
	})
}

func TestNewLoggerAddsServiceAndVersion(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewWithWriter(&buf, textCfg("info"))
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("startup")
	out := buf.String()
	if !strings.Contains(out, "service=copilotd") {
		t.Errorf("missing service attribute:\n%s", out)
	}
	if !strings.Contains(out, "version=") {
		t.Errorf("missing version attribute:\n%s", out)
	}
}

func TestAddSourceOnlyAtDebug(t *testing.T) {
	t.Run("info has no source", func(t *testing.T) {
		var buf bytes.Buffer
		logger, _ := NewWithWriter(&buf, textCfg("info"))
		logger.Info("x")
		if strings.Contains(buf.String(), "source=") {
			t.Errorf("info-level log should not include source:\n%s", buf.String())
		}
	})
	t.Run("debug has source", func(t *testing.T) {
		var buf bytes.Buffer
		logger, _ := NewWithWriter(&buf, textCfg("debug"))
		logger.Debug("x")
		if !strings.Contains(buf.String(), "source=") {
			t.Errorf("debug-level log should include source:\n%s", buf.String())
		}
	})
}

func TestContextHandlerInjectsRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := NewWithWriter(&buf, textCfg("info"))

	ctx := WithRequestID(context.Background(), "req-123")
	logger.InfoContext(ctx, "handled")

	if !strings.Contains(buf.String(), "request_id=req-123") {
		t.Errorf("request_id not injected from context:\n%s", buf.String())
	}
}

func TestContextHandlerNoRequestIDWithoutContextValue(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := NewWithWriter(&buf, textCfg("info"))
	logger.Info("no ctx id") // background context, no id
	if strings.Contains(buf.String(), "request_id=") {
		t.Errorf("request_id should be absent when not in context:\n%s", buf.String())
	}
}

// This is the slog footgun: a naive context handler that returns the inner
// handler (or an unwrapped self) from WithAttrs/WithGroup silently drops
// attributes/groups or stops injecting request_id. Assert all three survive.
func TestContextHandlerPreservesAttrsAndGroups(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := NewWithWriter(&buf, textCfg("info"))

	logger = logger.With("base_attr", "base_val").WithGroup("grp")
	ctx := WithRequestID(context.Background(), "rid-xyz")
	logger.InfoContext(ctx, "msg", "inner_attr", "inner_val")

	out := buf.String()
	if !strings.Contains(out, "base_attr=base_val") {
		t.Errorf("WithAttrs dropped the base attribute:\n%s", out)
	}
	if !strings.Contains(out, "grp.inner_attr=inner_val") {
		t.Errorf("WithGroup dropped the group:\n%s", out)
	}
	if !strings.Contains(out, "request_id=rid-xyz") {
		t.Errorf("request_id injection lost after WithAttrs/WithGroup:\n%s", out)
	}
}

func TestNewRequestIDIsUUIDv4(t *testing.T) {
	for range 20 {
		id := NewRequestID()
		parsed, err := uuid.Parse(id)
		if err != nil {
			t.Fatalf("NewRequestID() = %q, not a valid UUID: %v", id, err)
		}
		if parsed.Version() != 4 {
			t.Errorf("NewRequestID() = %q, version = %d, want 4", id, parsed.Version())
		}
	}
}

func TestResolveRequestID(t *testing.T) {
	longID := strings.Repeat("a", 128)
	tooLong := strings.Repeat("a", 129)

	t.Run("well-formed inbound honored", func(t *testing.T) {
		for _, in := range []string{
			"550e8400-e29b-41d4-a716-446655440000",
			"abc_123.DEF-456",
			longID,
			"x",
		} {
			if got := ResolveRequestID(in); got != in {
				t.Errorf("ResolveRequestID(%q) = %q, want it honored", in, got)
			}
		}
	})

	t.Run("malformed or oversized regenerated", func(t *testing.T) {
		for _, in := range []string{
			"",
			tooLong,
			"has space",
			"has/slash",
			"emoji-\U0001F600",
			"semi;colon",
		} {
			got := ResolveRequestID(in)
			if got == in {
				t.Errorf("ResolveRequestID(%q) should have regenerated, got the input back", in)
			}
			if _, err := uuid.Parse(got); err != nil {
				t.Errorf("ResolveRequestID(%q) = %q, not a fresh UUID: %v", in, got, err)
			}
		}
	})
}

// The logging package must stay free of net/http so it remains reusable and
// independently testable.
func TestLoggingHasNoNetHTTPDependency(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	for _, pkg := range pkgs {
		for file, f := range pkg.Files {
			for _, imp := range f.Imports {
				if strings.Trim(imp.Path.Value, `"`) == "net/http" {
					t.Errorf("%s imports net/http; logging must not depend on it", file)
				}
			}
		}
	}
}
