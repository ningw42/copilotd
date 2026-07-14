package identity

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// Static must satisfy the Provider interface the forwarder and server depend on.
var _ Provider = (*Static)(nil)

func TestStaticCurrentReturnsFixedCredential(t *testing.T) {
	cred := Credential{
		BaseURL: "https://api.example.invalid",
		Token:   "copilot-token",
		Headers: http.Header{"Copilot-Integration-Id": {"vscode-chat"}},
	}
	s := NewStatic(cred, true)

	got, err := s.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if got.BaseURL != cred.BaseURL || got.Token != cred.Token {
		t.Errorf("Current() = %+v, want %+v", got, cred)
	}
	if got.Headers.Get("Copilot-Integration-Id") != "vscode-chat" {
		t.Errorf("Current() headers = %v, want the impersonation set", got.Headers)
	}
}

func TestStaticReadyReflectsState(t *testing.T) {
	s := NewStatic(Credential{}, false)
	if s.Ready() {
		t.Errorf("Ready() = true, want false")
	}
	s.SetReady(true)
	if !s.Ready() {
		t.Errorf("Ready() = false after SetReady(true), want true")
	}
}

func TestStaticCurrentSurfacesError(t *testing.T) {
	s := NewStatic(Credential{Token: "tok"}, true)
	want := errors.New("mint failed")
	s.SetError(want)

	if _, err := s.Current(context.Background()); !errors.Is(err, want) {
		t.Errorf("Current() error = %v, want %v", err, want)
	}
	s.SetError(nil)
	if _, err := s.Current(context.Background()); err != nil {
		t.Errorf("Current() error = %v after clearing, want nil", err)
	}
}
