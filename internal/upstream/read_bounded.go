package upstream

import (
	"context"
	"errors"
	"io"
	"math"

	"github.com/ningw42/copilotd/internal/apierror"
)

var errResponseBodyTooLarge = errors.New("upstream response body exceeds the maximum allowed size")

type contextBoundResponseBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b *contextBoundResponseBody) upstreamCallContext() context.Context {
	return b.ctx
}

// ReadBounded reads body under max. It is context-free so the owner of the
// request context can classify a read interrupted by its own cancellation.
func ReadBounded(body io.Reader, max int64) ([]byte, *Failure) {
	reader := body
	if max < math.MaxInt64 {
		reader = io.LimitReader(body, max+1)
	}
	contents, err := io.ReadAll(reader)
	if err != nil {
		return nil, &Failure{
			Kind:    apierror.BadGateway,
			Message: "could not read the upstream response",
			Err:     err,
		}
	}
	if int64(len(contents)) > max {
		return nil, &Failure{
			Kind:    apierror.BadGateway,
			Message: errResponseBodyTooLarge.Error(),
			Err:     errResponseBodyTooLarge,
		}
	}
	return contents, nil
}

// ReadBounded reads body under the Caller's configured buffered cap. When body
// came from Do, it also classifies cancellation from that call's bound context.
func (c *Caller) ReadBounded(body io.Reader) ([]byte, *Failure) {
	contents, failure := ReadBounded(body, c.maxBufferedBytes)
	if failure == nil {
		return contents, nil
	}

	ctx := context.Background()
	if bound, ok := body.(interface{ upstreamCallContext() context.Context }); ok {
		ctx = bound.upstreamCallContext()
	}
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, c.failure(ctx, apierror.GatewayTimeout, "the upstream request timed out", false, failure.Err)
	case errors.Is(cause, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return nil, c.failure(ctx, 0, "", true, failure.Err)
	default:
		return nil, c.failure(ctx, failure.Kind, failure.Message, failure.ClientGone, failure.Err)
	}
}
