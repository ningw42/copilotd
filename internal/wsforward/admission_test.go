package wsforward

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
)

// gatedProvider parks every credential resolution until release closes, so a
// test can hold admitted handlers mid-accept while shutdown waits for them.
type gatedProvider struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func newGatedProvider(capacity int) *gatedProvider {
	return &gatedProvider{
		entered: make(chan struct{}, capacity),
		release: make(chan struct{}),
	}
}

func (p *gatedProvider) Current(ctx context.Context) (identity.Credential, error) {
	p.calls.Add(1)
	p.entered <- struct{}{}
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return identity.Credential{}, errors.New("credential unavailable")
}

func (*gatedProvider) Ready() bool { return true }

func newAdmissionTestProxy(provider identity.Provider, dialClient *http.Client, metrics WsMetrics) *Proxy {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(newTestCaller(provider, logger), dialClient, time.Second, time.Second, 1<<20, nil, logger, logger, 0, metrics)
}

// TestProxyShutdownRejectsLateUpgradesConcurrentlyWithLastAdmittedHandler hammers
// the admission gate with post-drain upgrades while the last admitted handlers
// finish and Shutdown's wait crosses zero. A late handler that registered
// before checking the gate would reuse the WaitGroup during Wait and panic or
// trip the race detector.
func TestProxyShutdownRejectsLateUpgradesConcurrentlyWithLastAdmittedHandler(t *testing.T) {
	const (
		iterations = 40
		admitted   = 4
		late       = 16
	)
	for iteration := 0; iteration < iterations; iteration++ {
		provider := newGatedProvider(admitted + late)
		observed := &recordingWsMetrics{}
		proxy := newAdmissionTestProxy(provider, &http.Client{Transport: http.DefaultTransport}, WsMetrics{Accept: observed})
		handler := proxy.Handler(endpoint.OpenAIResponsesWS())

		var admittedDone sync.WaitGroup
		for i := 0; i < admitted; i++ {
			admittedDone.Add(1)
			go func() {
				defer admittedDone.Done()
				handler.ServeHTTP(httptest.NewRecorder(), validUpgradeRequest())
			}()
		}
		for i := 0; i < admitted; i++ {
			select {
			case <-provider.entered:
			case <-time.After(time.Second):
				t.Fatalf("iteration %d: admitted handler %d did not reach credential resolution", iteration, i)
			}
		}

		proxy.StartDrain()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := make(chan error, 1)
		go func() { shutdownErr <- proxy.Shutdown(shutdownCtx) }()

		lateCodes := make(chan int, late)
		var lateDone sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < late; i++ {
			lateDone.Add(1)
			go func() {
				defer lateDone.Done()
				<-start
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, validUpgradeRequest())
				lateCodes <- recorder.Code
			}()
		}
		close(start)
		close(provider.release)

		select {
		case err := <-shutdownErr:
			if err != nil {
				t.Fatalf("iteration %d: shutdown error = %v, want nil once admitted handlers finish", iteration, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: shutdown did not return after admitted handlers were released", iteration)
		}
		cancel()
		lateDone.Wait()
		admittedDone.Wait()
		close(lateCodes)
		for code := range lateCodes {
			if code != http.StatusServiceUnavailable {
				t.Fatalf("iteration %d: late upgrade status = %d, want 503", iteration, code)
			}
		}
		if got := provider.calls.Load(); got != admitted {
			t.Fatalf("iteration %d: credential resolutions = %d, want %d (late upgrades must do no upstream work)", iteration, got, admitted)
		}
		accepts, _ := observed.snapshot()
		rejected := 0
		for _, outcome := range accepts {
			if outcome == AcceptRejected {
				rejected++
			}
		}
		// The gated provider's credential failure is itself NotReady, so every
		// admitted handler and every late upgrade books exactly one rejection.
		if len(accepts) != admitted+late || rejected != admitted+late {
			t.Fatalf("iteration %d: accept observations = %v, want %d rejections", iteration, accepts, admitted+late)
		}
	}
}

func TestProxyShutdownWaitsForAdmittedHandlerMidCredential(t *testing.T) {
	provider := newGatedProvider(1)
	proxy := newAdmissionTestProxy(provider, &http.Client{Transport: http.DefaultTransport}, WsMetrics{})
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(httptest.NewRecorder(), validUpgradeRequest())
	}()
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not reach credential resolution")
	}

	shutdownErr := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { shutdownErr <- proxy.Shutdown(ctx) }()
	assertShutdownPendingUntilRelease(t, shutdownErr, func() { close(provider.release) })
	<-handlerDone
}

func TestProxyShutdownWaitsForAdmittedHandlerMidDial(t *testing.T) {
	dialEntered := make(chan struct{})
	releaseDial := make(chan struct{})
	dialClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(dialEntered)
		select {
		case <-releaseDial:
		case <-request.Context().Done():
		}
		return nil, errors.New("dial released")
	})}
	proxy := newAdmissionTestProxy(identity.NewStatic(identity.Credential{
		BaseURL: "http://upstream.invalid",
		Token:   "copilot-token",
	}, true), dialClient, WsMetrics{})
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(httptest.NewRecorder(), validUpgradeRequest())
	}()
	select {
	case <-dialEntered:
	case <-time.After(time.Second):
		t.Fatal("handler did not reach the upstream dial")
	}

	shutdownErr := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { shutdownErr <- proxy.Shutdown(ctx) }()
	assertShutdownPendingUntilRelease(t, shutdownErr, func() { close(releaseDial) })
	<-handlerDone
}

// assertShutdownPendingUntilRelease proves the admitted handler is still
// counted: a late upgrade is rejected while Shutdown keeps waiting, and
// Shutdown completes cleanly only after release lets the handler finish.
func assertShutdownPendingUntilRelease(t *testing.T, shutdownErr <-chan error, release func()) {
	t.Helper()
	select {
	case err := <-shutdownErr:
		t.Fatalf("shutdown returned %v while an admitted handler was still running", err)
	default:
	}
	release()
	select {
	case err := <-shutdownErr:
		if err != nil {
			t.Fatalf("shutdown error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return after the admitted handler finished")
	}
}

func TestProxyShutdownIsRepeatableAndNeverReopensAdmission(t *testing.T) {
	provider := newGatedProvider(1)
	proxy := newAdmissionTestProxy(provider, &http.Client{Transport: http.DefaultTransport}, WsMetrics{})
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := proxy.Shutdown(ctx)
		cancel()
		if err != nil {
			t.Fatalf("shutdown %d error = %v, want nil", i, err)
		}
		recorder := httptest.NewRecorder()
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, validUpgradeRequest())
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status after shutdown %d = %d, want 503", i, recorder.Code)
		}
	}
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("credential resolutions = %d, want 0", got)
	}
}
