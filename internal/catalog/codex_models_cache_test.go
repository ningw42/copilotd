package catalog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
)

const testCodexReleaseCommit = "1234567890abcdef1234567890abcdef12345678"

func TestModelsCacheServesLatestReleaseBytesWithoutCredentials(t *testing.T) {
	t.Parallel()

	const tag = "rust-v0.145.0"
	fetched := validCodexModelsBytes(t, "gpt-5.4", "fresh")
	var latestCalls atomic.Int32
	var modelsCalls atomic.Int32
	github := newCodexModelsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want credential-free request", got)
		}
		switch r.URL.Path {
		case "/repos/openai/codex/releases/latest":
			latestCalls.Add(1)
			_, _ = io.WriteString(w, `{"tag_name":"`+tag+`"}`)
		case "/repos/openai/codex/contents/codex-rs/models-manager/models.json":
			modelsCalls.Add(1)
			if got := r.URL.Query().Get("ref"); got != testCodexReleaseCommit {
				t.Errorf("models ref = %q, want peeled commit %q", got, testCodexReleaseCommit)
			}
			if got := r.Header.Get("Accept"); got != "application/vnd.github.raw+json" {
				t.Errorf("Accept = %q, want GitHub raw media type", got)
			}
			_, _ = w.Write(fetched)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(github.Close)

	registry := cache.NewRegistry()
	models := NewModelsCache(ModelsCacheConfig{
		RefreshInterval: time.Hour,
	}, ModelsEdge{
		BaseURL: github.URL,
		Client:  github.Client(),
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())

	got, status := models.Current()
	if string(got) != string(fetched) {
		t.Fatalf("Current bytes = %s, want fetched release %s", got, fetched)
	}
	if status.Source != "fetched" || status.Version != tag || status.LastSuccess == nil {
		t.Fatalf("status = %#v, want fetched %s with last success", status, tag)
	}
	if latestCalls.Load() != 2 || modelsCalls.Load() != 1 {
		t.Fatalf("requests: latest=%d models=%d, want 2/1", latestCalls.Load(), modelsCalls.Load())
	}
}

func TestModelsCacheFetchesLatestReleaseAtResolvedCommit(t *testing.T) {
	const tag = "rust-v0.151.0"
	fetched := validCodexModelsBytes(t, "gpt-5.4", "commit pinned")
	var commitCalls atomic.Int32
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case latestCodexReleasePath:
			_, _ = io.WriteString(w, `{"tag_name":"`+tag+`"}`)
		case "/repos/openai/codex/commits/" + tag:
			commitCalls.Add(1)
			if got := r.Header.Get("Accept"); got != "application/vnd.github.sha" {
				t.Errorf("commit Accept = %q, want GitHub SHA media type", got)
			}
			_, _ = io.WriteString(w, testCodexReleaseCommit)
		case codexModelsContentPath:
			if got := r.URL.Query().Get("ref"); got != testCodexReleaseCommit {
				t.Errorf("models ref = %q, want peeled commit %q", got, testCodexReleaseCommit)
			}
			_, _ = w.Write(fetched)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(github.Close)

	registry := cache.NewRegistry()
	models := NewModelsCache(ModelsCacheConfig{RefreshInterval: time.Hour}, ModelsEdge{
		BaseURL: github.URL,
		Client:  github.Client(),
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	registry.Prime(context.Background())

	got, status := models.Current()
	if !bytes.Equal(got, fetched) || status.Source != "fetched" || status.Version != tag {
		t.Fatalf("Current = %d bytes, %#v; want commit-pinned %s bytes", len(got), status, tag)
	}
	if commitCalls.Load() != 1 {
		t.Fatalf("commit resolution calls = %d, want 1", commitCalls.Load())
	}
}

func TestModelsCacheAcceptsNullInstructionsVariables(t *testing.T) {
	t.Parallel()

	const tag = "rust-v0.150.0"
	fetched := codexModelsBytesWithInstructionsVariables(t, nil)
	github := newCodexModelsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case latestCodexReleasePath:
			_, _ = io.WriteString(w, `{"tag_name":"`+tag+`"}`)
		case codexModelsContentPath:
			_, _ = w.Write(fetched)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(github.Close)

	registry := cache.NewRegistry()
	models := NewModelsCache(ModelsCacheConfig{RefreshInterval: time.Hour}, ModelsEdge{
		BaseURL: github.URL,
		Client:  github.Client(),
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())

	got, status := models.Current()
	if !bytes.Equal(got, fetched) {
		t.Fatalf("Current returned %d bytes, want exact %d-byte fetched release", len(got), len(fetched))
	}
	if status.Source != "fetched" || status.Version != tag || status.LastSuccess == nil {
		t.Fatalf("status = %#v, want fetched %s with last success", status, tag)
	}
}

func TestModelsCacheAcceptsCurrentInstructionSources(t *testing.T) {
	tests := []struct {
		name    string
		fetched []byte
	}{
		{name: "canonical only", fetched: codexModelsBytesWithoutField(t, "base_instructions")},
		{name: "legacy only", fetched: codexModelsBytesWithoutField(t, "model_messages")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			github := newCodexModelsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case latestCodexReleasePath:
					_, _ = io.WriteString(w, `{"tag_name":"rust-v0.151.0"}`)
				case codexModelsContentPath:
					_, _ = w.Write(tc.fetched)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(github.Close)

			registry := cache.NewRegistry()
			models := NewModelsCache(ModelsCacheConfig{RefreshInterval: time.Hour}, ModelsEdge{
				BaseURL: github.URL,
				Client:  github.Client(),
			}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
			registry.Prime(context.Background())

			got, status := models.Current()
			if !bytes.Equal(got, tc.fetched) {
				t.Fatalf("Current returned %d bytes, want exact %d-byte fetched release", len(got), len(tc.fetched))
			}
			if status.Source != "fetched" || status.Version != "rust-v0.151.0" || status.LastSuccess == nil {
				t.Fatalf("status = %#v, want fetched current instruction source", status)
			}
		})
	}
}

func TestModelsEdgeRejectsUnstableLatestRelease(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		// GitHub's releases/latest endpoint excludes these states. Retain the
		// checks as defense in depth for malformed or alternate API responses.
		{name: "defense in depth: draft flag", body: `{"tag_name":"rust-v0.151.0","draft":true}`},
		{name: "defense in depth: prerelease flag", body: `{"tag_name":"rust-v0.151.0","prerelease":true}`},
		{name: "prerelease suffix", body: `{"tag_name":"rust-v0.152.0-alpha.1"}`},
		{name: "wrong release lineage", body: `{"tag_name":"v0.151.0"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			github := newCodexModelsServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(github.Close)

			edge := ModelsEdge{BaseURL: github.URL, Client: github.Client()}
			if tag, err := edge.latestReleaseTag(context.Background()); err == nil {
				t.Fatalf("latestReleaseTag accepted unstable tag %q", tag)
			}
		})
	}
}

func TestModelsCacheUnchangedReleaseSkipsModelsDownload(t *testing.T) {
	t.Parallel()

	var latestCalls atomic.Int32
	var commitCalls atomic.Int32
	github := newCodexModelsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != latestCodexReleasePath {
			t.Errorf("path = %q, want latest release peek only", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		latestCalls.Add(1)
		_, _ = io.WriteString(w, `{"tag_name":"`+embeddedCodexModelsVersion+`"}`)
	}), &commitCalls)
	t.Cleanup(github.Close)

	registry := cache.NewRegistry()
	models := NewModelsCache(ModelsCacheConfig{RefreshInterval: time.Hour}, ModelsEdge{
		BaseURL: github.URL,
		Client:  github.Client(),
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())

	got, status := models.Current()
	if string(got) != string(embeddedCodexModels) {
		t.Fatal("Current did not retain the embedded floor")
	}
	if status.Source != "fallback" || status.Version != embeddedCodexModelsVersion || status.LastSuccess != nil {
		t.Fatalf("status = %#v, want cold embedded floor", status)
	}
	if latestCalls.Load() != 1 {
		t.Fatalf("latest release requests = %d, want 1", latestCalls.Load())
	}
	if commitCalls.Load() != 0 {
		t.Fatalf("commit resolution requests = %d, want 0 for unchanged release", commitCalls.Load())
	}
}

func TestModelsCacheNewReleaseWithFloorContentRekeysTheEmbeddedFloor(t *testing.T) {
	t.Parallel()

	const tag = "rust-v0.145.0"
	github := newCodexModelsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case latestCodexReleasePath:
			_, _ = io.WriteString(w, `{"tag_name":"`+tag+`"}`)
		case codexModelsContentPath:
			_, _ = w.Write(embeddedCodexModels)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(github.Close)

	registry := cache.NewRegistry()
	models := NewModelsCache(ModelsCacheConfig{RefreshInterval: time.Hour}, ModelsEdge{
		BaseURL: github.URL,
		Client:  github.Client(),
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())

	got, status := models.Current()
	if string(got) != string(embeddedCodexModels) {
		t.Fatal("Current did not serve the embedded allocation for floor-identical content")
	}
	if status.Source != "fallback" || status.Version != tag || status.LastSuccess == nil {
		t.Fatalf("status = %#v, want confirmed fallback at %s", status, tag)
	}
}

func TestModelsCacheRekeysSameFetchedContentThenReleasesItForFloor(t *testing.T) {
	t.Parallel()

	ahead := validCodexModelsBytes(t, "gpt-5.4", "ahead")
	tags := []string{"rust-v0.145.0", "rust-v0.146.0", "rust-v0.147.0"}
	bodies := [][]byte{ahead, append([]byte(nil), ahead...), embeddedCodexModels}
	var latestCalls atomic.Int32
	var modelsCalls atomic.Int32
	github := newCodexModelsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case latestCodexReleasePath:
			call := int(latestCalls.Add(1) - 1)
			phase := call / 2
			if phase >= len(tags) {
				t.Errorf("unexpected latest release call %d", call+1)
				phase = len(tags) - 1
			}
			_, _ = io.WriteString(w, `{"tag_name":"`+tags[phase]+`"}`)
		case codexModelsContentPath:
			call := int(modelsCalls.Add(1) - 1)
			if call >= len(bodies) {
				t.Errorf("unexpected models call %d", call+1)
				call = len(bodies) - 1
			}
			_, _ = w.Write(bodies[call])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(github.Close)

	registry := cache.NewRegistry()
	models := NewModelsCache(ModelsCacheConfig{RefreshInterval: time.Hour}, ModelsEdge{
		BaseURL: github.URL,
		Client:  github.Client(),
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())
	first, firstStatus := models.Current()
	if firstStatus.Source != "fetched" || firstStatus.Version != tags[0] {
		t.Fatalf("first status = %#v, want fetched %s", firstStatus, tags[0])
	}

	registry.Prime(context.Background())
	rekeyed, rekeyedStatus := models.Current()
	if rekeyedStatus.Source != "fetched" || rekeyedStatus.Version != tags[1] {
		t.Fatalf("rekeyed status = %#v, want fetched %s", rekeyedStatus, tags[1])
	}
	if &rekeyed[0] != &first[0] {
		t.Fatal("same-content release replaced the served fetched allocation")
	}

	registry.Prime(context.Background())
	floor, floorStatus := models.Current()
	if floorStatus.Source != "fallback" || floorStatus.Version != tags[2] || floorStatus.LastSuccess == nil {
		t.Fatalf("floor status = %#v, want confirmed fallback %s", floorStatus, tags[2])
	}
	if &floor[0] != &embeddedCodexModels[0] {
		t.Fatal("floor-identical release did not return to the embedded allocation")
	}
}

func TestModelsCacheRejectsMalformedReleaseAndKeepsFloor(t *testing.T) {
	t.Parallel()

	const tag = "rust-v0.145.0"
	github := newCodexModelsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case latestCodexReleasePath:
			_, _ = io.WriteString(w, `{"tag_name":"`+tag+`"}`)
		case codexModelsContentPath:
			_, _ = io.WriteString(w, `{"models":[{"base_instructions":"missing slug"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(github.Close)

	registry := cache.NewRegistry()
	models := NewModelsCache(ModelsCacheConfig{RefreshInterval: time.Hour}, ModelsEdge{
		BaseURL: github.URL,
		Client:  github.Client(),
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())

	got, status := models.Current()
	if string(got) != string(embeddedCodexModels) {
		t.Fatal("Current did not retain the embedded floor after rejection")
	}
	if status.Source != "fallback" || status.Version != embeddedCodexModelsVersion || status.LastSuccess != nil {
		t.Fatalf("status = %#v, want untouched cold floor", status)
	}
}

func TestModelsCacheMalformedReleaseAfterWarmSuccessHoldsLastGood(t *testing.T) {
	t.Parallel()

	good := validCodexModelsBytes(t, "gpt-5.4", "last good")
	tags := []string{"rust-v0.145.0", "rust-v0.146.0"}
	bodies := [][]byte{good, codexModelsBytesWithInstructionsVariables(t, []string{})}
	var latestCalls atomic.Int32
	var modelsCalls atomic.Int32
	github := newCodexModelsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case latestCodexReleasePath:
			phase := int(latestCalls.Add(1)-1) / 2
			if phase >= len(tags) {
				phase = len(tags) - 1
			}
			_, _ = io.WriteString(w, `{"tag_name":"`+tags[phase]+`"}`)
		case codexModelsContentPath:
			call := int(modelsCalls.Add(1) - 1)
			if call >= len(bodies) {
				call = len(bodies) - 1
			}
			_, _ = w.Write(bodies[call])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(github.Close)

	registry := cache.NewRegistry()
	models := NewModelsCache(ModelsCacheConfig{RefreshInterval: time.Hour}, ModelsEdge{
		BaseURL: github.URL,
		Client:  github.Client(),
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())
	warm, warmStatus := models.Current()
	registry.Prime(context.Background())
	held, heldStatus := models.Current()

	if heldStatus.Source != "fetched" || heldStatus.Version != tags[0] {
		t.Fatalf("held status = %#v, want last-good fetched %s", heldStatus, tags[0])
	}
	if heldStatus.LastSuccess == nil || warmStatus.LastSuccess == nil || !heldStatus.LastSuccess.Equal(*warmStatus.LastSuccess) {
		t.Fatalf("held last success = %v, want original %v", heldStatus.LastSuccess, warmStatus.LastSuccess)
	}
	if &held[0] != &warm[0] {
		t.Fatal("malformed release replaced the last-good fetched allocation")
	}
}

func TestModelsCacheDisabledRefreshNeverCallsGitHub(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("GitHub request made while Codex catalog refresh is disabled")
		return nil, nil
	})}
	registry := cache.NewRegistry()
	models := NewModelsCache(ModelsCacheConfig{}, ModelsEdge{
		BaseURL: "https://github.invalid",
		Client:  client,
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())

	got, status := models.Current()
	if string(got) != string(embeddedCodexModels) || status.Source != "fallback" || status.LastSuccess != nil {
		t.Fatalf("Current = %d bytes, %#v; want untouched embedded floor", len(got), status)
	}
}

func TestModelsCacheBoundsEachCredentialFreeEdgeCall(t *testing.T) {
	t.Parallel()

	fetched := validCodexModelsBytes(t, "gpt-5.4", "bounded")
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want credential-free request", got)
		}
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Error("edge request has no deadline")
		} else if remaining := time.Until(deadline); remaining < 4*time.Second || remaining > modelsRequestTimeout {
			t.Errorf("edge request deadline remaining = %v, want approximately 5s", remaining)
		}
		body := []byte(`{"tag_name":"rust-v0.145.0"}`)
		if strings.HasPrefix(req.URL.Path, codexReleaseCommitPath) {
			body = []byte(testCodexReleaseCommit)
		} else if req.URL.Path == codexModelsContentPath {
			body = fetched
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    req,
		}, nil
	})}
	registry := cache.NewRegistry()
	models := NewModelsCache(ModelsCacheConfig{RefreshInterval: time.Hour}, ModelsEdge{
		BaseURL: "https://github.invalid",
		Client:  client,
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())

	_, status := models.Current()
	if calls.Load() != 4 {
		t.Fatalf("edge calls = %d, want bounded peek, fetch peek, commit resolution, and models read", calls.Load())
	}
	if status.Source != "fetched" || status.Version != "rust-v0.145.0" || status.LastSuccess == nil {
		t.Fatalf("status = %#v, want fetched release after three bounded calls", status)
	}
}

func TestModelsEdgeRejectsResponseOverCap(t *testing.T) {
	const responseLimit = 8 << 20
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, responseLimit+1))
	}))
	t.Cleanup(github.Close)

	edge := ModelsEdge{BaseURL: github.URL, Client: github.Client()}
	if _, err := edge.getBytes(context.Background(), "/oversized", ""); err == nil {
		t.Fatal("getBytes accepted a response above the credential-free edge cap")
	}
}

func TestModelsCacheHungRequestTimesOutAndHoldsLastGood(t *testing.T) {
	t.Parallel()

	const tag = "rust-v0.145.0"
	fetched := validCodexModelsBytes(t, "gpt-5.4", "last good")
	var calls atomic.Int32
	timedOut := make(chan error, 1)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if call > 4 {
			<-req.Context().Done()
			timedOut <- req.Context().Err()
			return nil, req.Context().Err()
		}
		body := []byte(`{"tag_name":"` + tag + `"}`)
		if strings.HasPrefix(req.URL.Path, codexReleaseCommitPath) {
			body = []byte(testCodexReleaseCommit)
		} else if req.URL.Path == codexModelsContentPath {
			body = fetched
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    req,
		}, nil
	})}
	registry := cache.NewRegistry()
	models := NewModelsCache(ModelsCacheConfig{RefreshInterval: 10 * time.Millisecond}, ModelsEdge{
		BaseURL: "https://github.invalid",
		Client:  client,
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())
	warm, warmStatus := models.Current()
	if warmStatus.Source != "fetched" || warmStatus.LastSuccess == nil {
		t.Fatalf("warm status = %#v, want fetched last-good value", warmStatus)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	started := time.Now()
	go func() {
		models.Run(ctx)
		close(runDone)
	}()

	select {
	case err := <-timedOut:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("hung request error = %v, want context deadline exceeded", err)
		}
	case <-time.After(modelsRequestTimeout + time.Second):
		t.Fatal("hung request did not stop within its five-second edge bound")
	}
	elapsed := time.Since(started)
	if elapsed < 4*time.Second || elapsed > modelsRequestTimeout+time.Second {
		t.Errorf("hung request stopped after %v, want approximately five seconds", elapsed)
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("models refresh loop did not stop after cancellation")
	}

	held, heldStatus := models.Current()
	if heldStatus.Source != "fetched" || heldStatus.Version != warmStatus.Version {
		t.Fatalf("held status = %#v, want last-good status %#v", heldStatus, warmStatus)
	}
	if heldStatus.LastSuccess == nil || !heldStatus.LastSuccess.Equal(*warmStatus.LastSuccess) {
		t.Fatalf("held last success = %v, want original %v", heldStatus.LastSuccess, warmStatus.LastSuccess)
	}
	if &held[0] != &warm[0] {
		t.Fatal("hung request replaced the last-good fetched allocation")
	}
}

func newCodexModelsServer(next http.Handler, commitCalls ...*atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, codexReleaseCommitPath) {
			if len(commitCalls) > 0 {
				commitCalls[0].Add(1)
			}
			_, _ = io.WriteString(w, testCodexReleaseCommit)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }
