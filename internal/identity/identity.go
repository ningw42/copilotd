// Package identity owns copilotd's outbound GitHub Copilot credential — the
// short-lived Copilot token, its upstream base URL, and the impersonation
// header set the forwarder applies to every upstream call. It exposes a narrow
// seam (the Credential snapshot and the Provider interface) that the forwarder
// and server consume without any Copilot-specific knowledge.
//
// Static provides a fixed test implementation of the same Provider seam as the
// real Manager, which mints Copilot tokens on demand.
package identity

import (
	"context"
	"net/http"
	"sync"
)

// Credential is an immutable snapshot the forwarder applies to one outbound
// request: the upstream base URL, the Copilot bearer token, and an impersonation
// header snapshot. The forwarder treats Headers as opaque and never
// mutates it (it copies onto a fresh outbound header map), so a snapshot taken
// during a concurrent mint is race-free.
type Credential struct {
	BaseURL string      // upstream scheme+host, e.g. "https://api.githubcopilot.com"
	Token   string      // short-lived Copilot bearer token (secret)
	Headers http.Header // impersonation set; opaque to the forwarder
}

// Impersonation provides the current headers used for the GitHub exchange and
// outbound Copilot requests. Implementations may return a live, changing set.
type Impersonation interface {
	Header() http.Header
}

type staticImpersonation struct {
	header http.Header
}

// StaticImpersonation adapts a fixed header set to the Impersonation seam. The
// input is cloned so later caller mutations do not change the fixed set.
func StaticImpersonation(header http.Header) Impersonation {
	if header == nil {
		header = http.Header{}
	}
	return staticImpersonation{header: header.Clone()}
}

func (i staticImpersonation) Header() http.Header { return i.header }

// Provider hands the forwarder a current Credential and separately reports
// local readiness. The real Manager mints on demand inside Current; the Static
// stub returns a fixed value.
type Provider interface {
	// Current returns the credential to use for an outbound request, minting one
	// on demand if the cached token is missing or stale (a no-op for the stub).
	Current(ctx context.Context) (Credential, error)
	// Ready reports whether local prerequisites are present to attempt serving.
	// A remote mint outcome must never change it or latch request admission.
	Ready() bool
}

// Static is a fixed-value Provider used to wire the forward path before the real
// minting Manager exists, and as a test double. Its Credential is constant; its
// local readiness and an optional Current error are settable so tests can
// exercise the readiness gate and request-time credential-failure path. It is
// safe for concurrent use.
type Static struct {
	mu    sync.RWMutex
	cred  Credential
	ready bool
	err   error
}

// NewStatic returns a Static Provider that serves cred with the given readiness.
func NewStatic(cred Credential, ready bool) *Static {
	return &Static{cred: cred, ready: ready}
}

// Current returns the fixed credential, or the configured error if one is set.
func (s *Static) Current(_ context.Context) (Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.err != nil {
		return Credential{}, s.err
	}
	return s.cred, nil
}

// Ready reports the configured readiness.
func (s *Static) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

// SetReady flips the readiness the stub reports.
func (s *Static) SetReady(ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = ready
}

// SetError sets the error Current returns (nil clears it); a seam for the
// request-time credential-failure path the real Manager exercises.
func (s *Static) SetError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}
