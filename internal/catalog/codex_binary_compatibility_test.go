package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests are opt-in black-box checks for release audits.
// CODEX_CATALOG_AUDIT_BINARY must name the exact pinned Codex executable;
// ordinary repository tests remain network- and tool-independent.
func TestLatestCodexBinaryAcceptsUnknownRemoteCatalogModel(t *testing.T) {
	observed := observeLatestCodexBinaryCatalog(t, singleRemoteDefaultModel(t))

	if observed.modelsCalls == 0 {
		t.Fatalf("Codex did not fetch the command-auth model catalog:\n%s", observed.output)
	}
	if observed.modelsMethod != http.MethodGet {
		t.Errorf("models method = %q, want GET", observed.modelsMethod)
	}
	if observed.clientVersion != "0.151.0" {
		t.Errorf("client_version = %q, want 0.151.0", observed.clientVersion)
	}
	if observed.authorization != "Bearer catalog-audit-token" {
		t.Errorf("Authorization = %q, want command-auth bearer", observed.authorization)
	}
	if observed.originator == "" || observed.userAgent == "" {
		t.Errorf("default request headers: originator=%q User-Agent=%q", observed.originator, observed.userAgent)
	}
	if observed.responsesCalls == 0 {
		t.Fatalf("Codex did not make a Responses request after catalog merge:\n%s", observed.output)
	}
	if observed.responsesMethod != http.MethodPost {
		t.Errorf("Responses method = %q, want POST", observed.responsesMethod)
	}
	if observed.selectedModel != "gpt-5.4-audit-alias" {
		t.Errorf("selected model = %q, want unknown remote priority/visibility winner gpt-5.4-audit-alias", observed.selectedModel)
	}
}

func TestLatestCodexBinaryKeepsBundledSourceAheadOfMatchingAlias(t *testing.T) {
	observed := observeLatestCodexBinaryCatalog(t, matchingRemoteAliasAndSource(t))

	if observed.modelsCalls == 0 {
		t.Fatalf("Codex did not fetch the command-auth model catalog:\n%s", observed.output)
	}
	if observed.responsesCalls == 0 {
		t.Fatalf("Codex did not make a Responses request after catalog merge:\n%s", observed.output)
	}
	if observed.selectedModel != "gpt-5.4" {
		t.Errorf("selected model = %q, want bundled source gpt-5.4 ahead of metadata-matching alias", observed.selectedModel)
	}
}

type codexCatalogObservation struct {
	modelsCalls     int
	responsesCalls  int
	modelsMethod    string
	responsesMethod string
	clientVersion   string
	authorization   string
	originator      string
	userAgent       string
	selectedModel   string
	output          string
}

func observeLatestCodexBinaryCatalog(t *testing.T, modelsBody []byte) codexCatalogObservation {
	t.Helper()
	binary := os.Getenv("CODEX_CATALOG_AUDIT_BINARY")
	if binary == "" {
		t.Skip("set CODEX_CATALOG_AUDIT_BINARY to the pinned Codex executable")
	}
	requireLatestCodexBinary(t, binary)
	printfPath, err := exec.LookPath("printf")
	if err != nil {
		t.Skipf("command-auth contract needs printf on PATH: %v", err)
	}

	var mu sync.Mutex
	var got codexCatalogObservation
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			mu.Lock()
			got.modelsCalls++
			got.modelsMethod = r.Method
			got.clientVersion = r.URL.Query().Get("client_version")
			got.authorization = r.Header.Get("Authorization")
			got.originator = r.Header.Get("originator")
			got.userAgent = r.Header.Get("User-Agent")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"catalog-audit"`)
			_, _ = w.Write(modelsBody)
		case "/responses":
			body, _ := io.ReadAll(r.Body)
			var request struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(body, &request)
			mu.Lock()
			got.responsesCalls++
			got.responsesMethod = r.Method
			got.selectedModel = request.Model
			mu.Unlock()
			http.Error(w, `{"error":{"message":"catalog audit stop","type":"invalid_request_error"}}`, http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	codexHome := t.TempDir()
	config := fmt.Sprintf(`model_provider = "catalog-audit"
approval_policy = "never"

[model_providers.catalog-audit]
name = "catalog-audit"
base_url = %q
wire_api = "responses"

[model_providers.catalog-audit.auth]
command = %q
args = ["catalog-audit-token"]
`, server.URL, printfPath)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write isolated Codex config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "exec", "--skip-git-repo-check", "--json", "return one short word")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	_ = cmd.Run() // The fake Responses endpoint deliberately terminates the run with HTTP 400.
	if ctx.Err() != nil {
		t.Fatalf("Codex audit timed out: %v\n%s", ctx.Err(), output.String())
	}

	mu.Lock()
	got.output = output.String()
	observed := got
	mu.Unlock()
	return observed
}

func requireLatestCodexBinary(t *testing.T, binary string) {
	t.Helper()
	file, err := os.Open(binary)
	if err != nil {
		t.Fatalf("open Codex binary: %v", err)
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		t.Fatalf("hash Codex binary: %v", err)
	}
	const wantSHA256 = "9739cbc928b9c573be83256acd46668f5dd4f119d2d09e05246895ca2aaf0c9a"
	if got := hex.EncodeToString(hasher.Sum(nil)); got != wantSHA256 {
		t.Fatalf("Codex binary SHA-256 = %s, want pinned %s", got, wantSHA256)
	}

	output, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("Codex version: %v: %s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != "codex-cli 0.151.0" {
		t.Fatalf("Codex version = %q, want codex-cli 0.151.0", got)
	}
}

func matchingRemoteAliasAndSource(t *testing.T) []byte {
	t.Helper()
	model := latestCodexModel(t, "gpt-5.4")
	model["priority"] = json.RawMessage("0")
	model["visibility"] = json.RawMessage(`"list"`)
	alias := make(map[string]json.RawMessage, len(model))
	for key, value := range model {
		alias[key] = value
	}
	alias["slug"] = json.RawMessage(`"gpt-5.4-audit-alias"`)
	return marshalRemoteModels(t, model, alias)
}

func singleRemoteDefaultModel(t *testing.T) []byte {
	t.Helper()
	model := latestCodexModel(t, "gpt-5.4")
	model["slug"] = json.RawMessage(`"gpt-5.4-audit-alias"`)
	model["priority"] = json.RawMessage("0")
	model["visibility"] = json.RawMessage(`"list"`)
	return marshalRemoteModels(t, model)
}

func latestCodexModel(t *testing.T, wantSlug string) map[string]json.RawMessage {
	t.Helper()
	latestBytes, err := os.ReadFile("testdata/codex-latest/models.json")
	if err != nil {
		t.Fatalf("read latest Codex fixture: %v", err)
	}
	var envelope struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(latestBytes, &envelope); err != nil {
		t.Fatalf("decode latest Codex fixture: %v", err)
	}
	for _, model := range envelope.Models {
		var slug string
		if err := json.Unmarshal(model["slug"], &slug); err != nil {
			t.Fatalf("decode fixture slug: %v", err)
		}
		if slug == wantSlug {
			return model
		}
	}
	t.Fatalf("latest Codex fixture has no %s entry", wantSlug)
	return nil
}

func marshalRemoteModels(t *testing.T, models ...map[string]json.RawMessage) []byte {
	t.Helper()
	body, err := json.Marshal(struct {
		Models []map[string]json.RawMessage `json:"models"`
	}{Models: models})
	if err != nil {
		t.Fatalf("encode remote model fixture: %v", err)
	}
	return body
}
