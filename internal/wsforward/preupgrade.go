package wsforward

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ningw42/copilotd/internal/logging"
)

func (p *Proxy) logUpstreamRequestID(ctx context.Context, header http.Header) {
	requestID, ok := logging.RequestIDFrom(ctx)
	if !ok {
		return
	}
	upstreamRequestID := header.Get(requestIDHeader)
	if upstreamRequestID == "" || upstreamRequestID == requestID {
		return
	}
	p.logger.InfoContext(ctx, "upstream response correlation",
		slog.String("upstream_request_id", upstreamRequestID))
}

func isWebSocketUpgrade(r *http.Request) bool {
	if r.Method != http.MethodGet || !r.ProtoAtLeast(1, 1) ||
		!headerContainsToken(r.Header, "Connection", "upgrade") ||
		!headerContainsToken(r.Header, "Upgrade", "websocket") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" {
		return false
	}
	keys := r.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keys[0]))
	return err == nil && len(decoded) == 16
}

func headerContainsToken(header http.Header, name, want string) bool {
	for _, value := range header.Values(name) {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}
