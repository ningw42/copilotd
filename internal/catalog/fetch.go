package catalog

import (
	"context"

	"github.com/ningw42/copilotd/internal/endpoint"
)

// FetchErrorKind identifies why a model Catalog fetch could not produce a
// complete response. Values implement error so callers can classify wrapped
// failures with errors.Is without depending on the Forwarder implementation.
type FetchErrorKind uint8

const (
	ErrNoCredential FetchErrorKind = iota + 1
	ErrBuildUpstream
	ErrUpstreamUnreachable
	ErrUpstreamTimeout
	ErrUpstreamRead
)

func (kind FetchErrorKind) Error() string {
	switch kind {
	case ErrNoCredential:
		return "no upstream credential available"
	case ErrBuildUpstream:
		return "could not build the upstream request"
	case ErrUpstreamUnreachable:
		return "could not reach the upstream"
	case ErrUpstreamTimeout:
		return "the upstream request timed out"
	case ErrUpstreamRead:
		return "could not read the upstream response"
	default:
		return "unknown Catalog fetch failure"
	}
}

// Fetcher obtains one current, account-authorized Copilot model Catalog without
// interpreting its response.
type Fetcher interface {
	FetchModels(ctx context.Context, upstream endpoint.Route) (status int, body []byte, err error)
}
