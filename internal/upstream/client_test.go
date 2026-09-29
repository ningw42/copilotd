package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestCallerDoReturnsFirstRedirectWithoutFollowingIt(t *testing.T) {
	target, targetCalls := redirectTarget(t)
	copilot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/redirect-target")
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, "copilot redirect body")
	}))
	t.Cleanup(copilot.Close)
	caller := New(readyExecutionProvider(copilot.URL), time.Second, time.Second, 1<<20, slog.Default())

	response, _, failure := caller.Do(context.Background(), executionCall())

	if failure != nil {
		t.Fatalf("Do() failure = %#v, want the first upstream response", failure)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read redirect body: %v", err)
	}
	if response.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want Copilot's 302", response.StatusCode)
	}
	if got, want := response.Header.Get("Location"), target.URL+"/redirect-target"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if got := string(body); got != "copilot redirect body" {
		t.Errorf("body = %q, want Copilot's redirect body", got)
	}
	if got := targetCalls.Load(); got != 0 {
		t.Errorf("redirect target requests = %d, want 0", got)
	}
}

func TestHandshakeClientReturnsFirstRedirectWithoutFollowingIt(t *testing.T) {
	target, targetCalls := redirectTarget(t)
	copilot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/redirect-target")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(copilot.Close)
	caller := New(readyExecutionProvider(copilot.URL), time.Second, time.Second, 1<<20, slog.Default())

	response, err := caller.HandshakeClient().Get(copilot.URL + "/responses")

	if err != nil {
		t.Fatalf("handshake GET: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want Copilot's 302", response.StatusCode)
	}
	if got := targetCalls.Load(); got != 0 {
		t.Errorf("redirect target requests = %d, want 0", got)
	}
}

func TestReplacementTransportsKeepRedirectRefusal(t *testing.T) {
	redirecting := func(calls *atomic.Int32) http.RoundTripper {
		return executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			response := executionResponse(request, http.StatusFound, http.NoBody)
			response.Header.Set("Location", "https://upstream.invalid/redirect-target")
			return response, nil
		})
	}

	t.Run("call transport", func(t *testing.T) {
		var calls atomic.Int32
		caller := New(readyExecutionProvider("https://upstream.invalid"), time.Second, time.Second, 1<<20, slog.Default(), WithTransport(redirecting(&calls)))

		response, _, failure := caller.Do(context.Background(), executionCall())

		if failure != nil {
			t.Fatalf("Do() failure = %#v, want the first upstream response", failure)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusFound || calls.Load() != 1 {
			t.Errorf("Do() = status %d after %d round trips, want 302 after 1", response.StatusCode, calls.Load())
		}
	})

	t.Run("handshake transport", func(t *testing.T) {
		var calls atomic.Int32
		caller := New(readyExecutionProvider("https://upstream.invalid"), time.Second, time.Second, 1<<20, slog.Default(), WithHandshakeTransport(redirecting(&calls)))

		response, err := caller.HandshakeClient().Get("https://upstream.invalid/responses")

		if err != nil {
			t.Fatalf("handshake GET: %v", err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusFound || calls.Load() != 1 {
			t.Errorf("handshake GET = status %d after %d round trips, want 302 after 1", response.StatusCode, calls.Load())
		}
	})
}

func TestNewBuildsEachClientOnItsOwnTransport(t *testing.T) {
	const responseHeaderTimeout = 7 * time.Second
	caller := New(readyExecutionProvider("https://upstream.invalid"), responseHeaderTimeout, time.Second, 1<<20, slog.Default())

	call := clientTransport(t, "call", caller.client)
	handshake := clientTransport(t, "handshake", caller.HandshakeClient())

	if call == handshake {
		t.Fatal("call and handshake clients share one transport, want separate transports")
	}
	assertTransportSettings(t, "call", call, map[string]any{
		"DisableCompression":    true,
		"ForceAttemptHTTP2":     true,
		"MaxIdleConns":          100,
		"MaxIdleConnsPerHost":   100,
		"IdleConnTimeout":       90 * time.Second,
		"ResponseHeaderTimeout": responseHeaderTimeout,
	})
	assertTransportSettings(t, "handshake", handshake, nil)
}

func TestCallerDoLeavesCompressionNegotiationAndDecodingToCaller(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte(`{"opaque":true}`)); err != nil {
		t.Fatalf("compress fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("finish compressed fixture: %v", err)
	}
	wantBody := compressed.Bytes()
	acceptEncoding := make(chan []string, 1)
	copilot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptEncoding <- r.Header.Values("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wantBody)
	}))
	t.Cleanup(copilot.Close)
	caller := New(readyExecutionProvider(copilot.URL), time.Second, time.Second, 1<<20, slog.Default())
	call := executionCall()
	call.AcceptIdentityEncoding = false

	response, _, failure := caller.Do(context.Background(), call)

	if failure != nil {
		t.Fatalf("Do() failure = %#v, want response", failure)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got := <-acceptEncoding; len(got) != 0 {
		t.Errorf("transport-supplied Accept-Encoding = %q, want absent", got)
	}
	if response.Uncompressed {
		t.Error("response marked transparently decompressed, want encoded response")
	}
	if got := response.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
	if !bytes.Equal(body, wantBody) {
		t.Errorf("body = %x, want encoded bytes %x", body, wantBody)
	}
}

// clientTransport checks the client-level settings both clients share and
// returns the client's transport.
func clientTransport(t *testing.T, name string, client *http.Client) *http.Transport {
	t.Helper()
	if client.Timeout != 0 {
		t.Errorf("%s client Timeout = %v, want none", name, client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("%s client transport = %T, want *http.Transport", name, client.Transport)
	}
	if reflect.ValueOf(transport.Proxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Errorf("%s transport Proxy is not http.ProxyFromEnvironment", name)
	}
	return transport
}

// assertTransportSettings checks the named fields of transport and that every
// other exported field apart from Proxy keeps its zero value.
func assertTransportSettings(t *testing.T, name string, transport *http.Transport, want map[string]any) {
	t.Helper()
	value := reflect.ValueOf(transport).Elem()
	for field := range want {
		if _, ok := value.Type().FieldByName(field); !ok {
			t.Fatalf("http.Transport has no field %s", field)
		}
	}
	for i := range value.NumField() {
		field := value.Type().Field(i)
		if !field.IsExported() || field.Name == "Proxy" {
			continue
		}
		got := value.Field(i)
		if expected, ok := want[field.Name]; ok {
			if !reflect.DeepEqual(got.Interface(), expected) {
				t.Errorf("%s transport %s = %v, want %v", name, field.Name, got.Interface(), expected)
			}
			continue
		}
		if !got.IsZero() {
			t.Errorf("%s transport %s = %v, want zero value", name, field.Name, got.Interface())
		}
	}
}

// redirectTarget serves the Location of a redirect test and counts the
// requests that reach it.
func redirectTarget(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(target.Close)
	return target, &calls
}
