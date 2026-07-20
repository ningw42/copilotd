package server

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/wsforward"
)

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
