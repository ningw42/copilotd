package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/endpoint"
)

// Call describes one authenticated upstream call, independent of transport.
type Call struct {
	Route      endpoint.Route
	Method     string
	Query      string
	ForceQuery bool

	// ClientHeader is the inbound header set to forward under the strip policy.
	// A nil value forwards no client header.
	ClientHeader http.Header
	// Body is the outbound body. A nil or http.NoBody value is wrapped so the
	// Transport treats an otherwise bodyless request as single-attempt.
	Body io.Reader
	// ContentLength is assigned only when non-zero, preserving the length and
	// GetBody that http.NewRequestWithContext derives for a sized body.
	ContentLength int64
	// AcceptIdentityEncoding forces an undecoded upstream response body.
	AcceptIdentityEncoding bool
}

// Prepare builds the authenticated outbound request without executing it.
func (c *Caller) Prepare(ctx context.Context, call Call) (*http.Request, *Failure) {
	credential, err := c.provider.Current(ctx)
	if err != nil {
		return nil, c.failure(ctx, apierror.NotReady, "no upstream credential available", false, err)
	}

	body := call.Body
	if body == nil || body == http.NoBody {
		body = &singleAttemptBody{ReadCloser: http.NoBody}
	}
	url := strings.TrimRight(credential.BaseURL, "/") + string(call.Route)
	request, err := http.NewRequestWithContext(ctx, call.Method, url, body)
	if err != nil {
		return nil, c.failure(ctx, apierror.BadGateway, "could not build the upstream request", false, err)
	}
	request.URL.RawQuery = call.Query
	request.URL.ForceQuery = call.ForceQuery
	if call.ContentLength != 0 {
		request.ContentLength = call.ContentLength
	}
	request.Header = authenticatedOutboundHeaders(ctx, call, credential.Token, credential.Headers)
	return request, nil
}
