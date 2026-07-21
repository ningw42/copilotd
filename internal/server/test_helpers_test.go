package server

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/impersonation"
	"github.com/ningw42/copilotd/internal/wsforward"
)

type staticImpersonationObserver struct {
	observed impersonation.Observed
}

func (s staticImpersonationObserver) Observe() impersonation.Observed { return s.observed }

func newTestImpersonationObserver() ImpersonationObserver {
	return staticImpersonationObserver{observed: impersonation.Observed{
		EffectiveHeaders: http.Header{
			"Copilot-Integration-Id": {"vscode-chat"},
			"Editor-Plugin-Version":  {"copilot-chat/0.26.7"},
			"Editor-Version":         {"vscode/1.104.1"},
			"User-Agent":             {"GitHubCopilotChat/0.26.7"},
			"X-Github-Api-Version":   {"2025-04-01"},
		},
		Discovery: impersonation.ObservedDiscovery{
			VSCode:      impersonation.ObservedFact{Source: "fallback"},
			CopilotChat: impersonation.ObservedFact{Source: "fallback"},
		},
	}}
}

func newTestWSProxy(provider identity.Provider) *wsforward.Proxy {
	return wsforward.New(
		provider,
		http.DefaultClient,
		time.Second,
		time.Second,
		1<<20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		wsforward.WsMetrics{},
	)
}
