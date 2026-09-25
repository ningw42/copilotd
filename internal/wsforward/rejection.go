package wsforward

import (
	"io"
	"net/http"
	"strconv"

	"github.com/ningw42/copilotd/internal/upstream"
)

// relayHandshakeRejection answers the client with Copilot's final non-101
// handshake response instead of a copilotd-originated failure.
//
// coder/websocket retains at most the first 1024 body bytes and discards its
// read error, so the retained bytes are relayed only when a declared length
// proves them complete. net/http never yields more than a declared length and
// ends anything shorter in the discarded error, so equality is that proof.
// Every other body is omitted (the divergence ledger's Omission), including
// complete bodies of unknown length and ones the dial transport decompressed.
func relayHandshakeRejection(w http.ResponseWriter, response *http.Response) {
	retained, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	var body []byte
	if response.ContentLength >= 0 && int64(len(retained)) == response.ContentLength {
		body = retained
	}
	upstream.CopyResponseHeaders(w.Header(), response.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}
