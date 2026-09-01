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
	"slices"
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
	if observed.clientVersion != "0.152.1" {
		t.Errorf("client_version = %q, want 0.152.1", observed.clientVersion)
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

func TestLatestCodexBinaryReplacesBundledRemoteCatalogModel(t *testing.T) {
	observed := observeLatestCodexBinaryCatalog(t, replacedBundledDefaultModel(t))

	if observed.modelsCalls == 0 {
		t.Fatalf("Codex did not fetch the command-auth model catalog:\n%s", observed.output)
	}
	if observed.responsesCalls == 0 {
		t.Fatalf("Codex did not make a Responses request after catalog merge:\n%s", observed.output)
	}
	if observed.selectedModel != "gpt-5.4" {
		t.Errorf("selected model = %q, want remotely replaced bundled model gpt-5.4", observed.selectedModel)
	}
}

func TestLatestCodexBinaryKeepsBundledSourceAheadOfMatchingAlias(t *testing.T) {
	modelsBody := matchingRemoteAliasAndSource(t)
	observed := observeLatestCodexBinaryCatalog(t, modelsBody)

	if observed.modelsCalls == 0 {
		t.Fatalf("Codex did not fetch the command-auth model catalog:\n%s", observed.output)
	}
	if observed.responsesCalls == 0 {
		t.Fatalf("Codex did not make a Responses request after catalog merge:\n%s", observed.output)
	}
	if observed.selectedModel != "gpt-5.6-sol" {
		t.Errorf("selected model = %q, want untouched bundled source gpt-5.6-sol ahead of metadata-matching alias", observed.selectedModel)
	}

	const alias = "gpt-5.6-sol-audit-alias"
	withoutAlias := observeLatestCodexBinaryCatalogWithModel(t, []byte(`{"models":[]}`), alias)
	if withoutAlias.responsesCalls == 0 || withoutAlias.selectedModel != alias {
		t.Fatalf("negative control changed: Codex no longer forwards arbitrary explicit model %q without a catalog entry:\n%s", alias, withoutAlias.output)
	}
	if slices.Contains(withoutAlias.listedModels, alias) {
		t.Fatalf("empty remote catalog unexpectedly listed alias %q", alias)
	}

	aliasObserved := observeLatestCodexBinaryCatalogWithModel(t, modelsBody, alias)
	if aliasObserved.responsesCalls == 0 {
		t.Fatalf("Codex did not make a Responses request with the accepted alias:\n%s", aliasObserved.output)
	}
	if aliasObserved.selectedModel != alias {
		t.Errorf("explicitly selected model = %q, want accepted remote alias %q", aliasObserved.selectedModel, alias)
	}
	if !slices.Contains(aliasObserved.listedModels, alias) {
		t.Errorf("merged model list %q does not contain accepted alias %q", aliasObserved.listedModels, alias)
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
	listedModels    []string
	output          string
}

func observeLatestCodexBinaryCatalog(t *testing.T, modelsBody []byte) codexCatalogObservation {
	t.Helper()
	return observeLatestCodexBinaryCatalogWithModel(t, modelsBody, "")
}

func observeLatestCodexBinaryCatalogWithModel(t *testing.T, modelsBody []byte, model string) codexCatalogObservation {
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
	args := []string{"exec", "--skip-git-repo-check", "--json"}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "return one short word")
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	_ = cmd.Run() // The fake Responses endpoint deliberately terminates the run with HTTP 400.
	if ctx.Err() != nil {
		t.Fatalf("Codex audit timed out: %v\n%s", ctx.Err(), output.String())
	}

	var listedModels []string
	if model != "" {
		listedModels = listLatestCodexBinaryModels(t, binary, codexHome)
	}
	mu.Lock()
	got.listedModels = listedModels
	got.output = output.String()
	observed := got
	mu.Unlock()
	return observed
}

func listLatestCodexBinaryModels(t *testing.T, binary, codexHome string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "app-server", "--listen", "stdio://")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("open Codex app-server stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("open Codex app-server stdout: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start Codex app-server: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		cancel()
		_ = cmd.Wait()
	}()

	encoder := json.NewEncoder(stdin)
	decoder := json.NewDecoder(stdout)
	if err := encoder.Encode(map[string]any{
		"id": 1, "method": "initialize",
		"params": map[string]any{
			"clientInfo": map[string]string{"name": "catalog-audit", "version": "1"},
		},
	}); err != nil {
		t.Fatalf("initialize Codex app-server: %v", err)
	}
	readCodexAppServerResult(t, decoder, 1, &stderr)
	if err := encoder.Encode(map[string]any{"method": "initialized"}); err != nil {
		t.Fatalf("notify initialized Codex app-server: %v", err)
	}
	if err := encoder.Encode(map[string]any{
		"id": 2, "method": "model/list",
		"params": map[string]any{"includeHidden": true, "limit": 100},
	}); err != nil {
		t.Fatalf("list Codex app-server models: %v", err)
	}
	result := readCodexAppServerResult(t, decoder, 2, &stderr)
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("decode Codex app-server model list: %v: %s", err, result)
	}
	models := make([]string, len(response.Data))
	for i, model := range response.Data {
		models[i] = model.ID
	}
	return models
}

func readCodexAppServerResult(t *testing.T, decoder *json.Decoder, wantID int, stderr *bytes.Buffer) json.RawMessage {
	t.Helper()
	for {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := decoder.Decode(&message); err != nil {
			t.Fatalf("read Codex app-server response %d: %v: %s", wantID, err, stderr.String())
		}
		var id int
		if json.Unmarshal(message.ID, &id) != nil || id != wantID {
			continue
		}
		if len(message.Error) > 0 && !bytes.Equal(message.Error, []byte("null")) {
			t.Fatalf("Codex app-server response %d error: %s", wantID, message.Error)
		}
		return message.Result
	}
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
	const wantSHA256 = "b82018241214a4a7c6b97b198585192d1dbc3aab1ddcdc640f04d8dee8c606f9"
	if got := hex.EncodeToString(hasher.Sum(nil)); got != wantSHA256 {
		t.Fatalf("Codex binary SHA-256 = %s, want pinned %s", got, wantSHA256)
	}

	output, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("Codex version: %v: %s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != "codex-cli 0.152.1" {
		t.Fatalf("Codex version = %q, want codex-cli 0.152.1", got)
	}
}

func replacedBundledDefaultModel(t *testing.T) []byte {
	t.Helper()
	model := vendoredCodexModel(t, "gpt-5.4")
	model["priority"] = json.RawMessage("0")
	model["visibility"] = json.RawMessage(`"list"`)
	return marshalRemoteModels(t, model)
}

func matchingRemoteAliasAndSource(t *testing.T) []byte {
	t.Helper()
	source := vendoredCodexModel(t, "gpt-5.6-sol")
	alias := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		alias[key] = value
	}
	alias["slug"] = json.RawMessage(`"gpt-5.6-sol-audit-alias"`)
	return marshalRemoteModels(t, alias)
}

func singleRemoteDefaultModel(t *testing.T) []byte {
	t.Helper()
	model := vendoredCodexModel(t, "gpt-5.4")
	model["slug"] = json.RawMessage(`"gpt-5.4-audit-alias"`)
	model["priority"] = json.RawMessage("0")
	model["visibility"] = json.RawMessage(`"list"`)
	return marshalRemoteModels(t, model)
}

func vendoredCodexModel(t *testing.T, wantSlug string) map[string]json.RawMessage {
	t.Helper()
	var envelope struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(embeddedCodexModels, &envelope); err != nil {
		t.Fatalf("decode vendored Codex snapshot: %v", err)
	}
	for _, model := range envelope.Models {
		var slug string
		if err := json.Unmarshal(model["slug"], &slug); err != nil {
			t.Fatalf("decode vendored snapshot slug: %v", err)
		}
		if slug == wantSlug {
			return model
		}
	}
	t.Fatalf("vendored Codex snapshot has no %s entry", wantSlug)
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
