package upstream

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/requestsummary"
)

// RequestIDHeader carries copilotd's resolved correlation id in both directions.
const RequestIDHeader = "X-Request-Id"

// Caller applies copilotd's shared upstream-call policy and executes HTTP calls.
type Caller struct {
	provider         identity.Provider
	client           *http.Client
	logger           *slog.Logger
	outboundTimeout  time.Duration
	maxBufferedBytes int64
}

// New builds a Caller from the dependencies shared by every upstream call.
func New(provider identity.Provider, client *http.Client, outboundTimeout time.Duration, maxBufferedBytes int64, logger *slog.Logger) *Caller {
	return &Caller{
		provider:         provider,
		client:           client,
		logger:           logger,
		outboundTimeout:  outboundTimeout,
		maxBufferedBytes: maxBufferedBytes,
	}
}

// Do executes call and returns the upstream response plus its response-path
// context, or one classified failure. ctx is used verbatim so the caller retains
// authority over cancellation and any post-response deadline.
func (c *Caller) Do(ctx context.Context, call Call) (*http.Response, context.Context, *Failure) {
	request, failure := c.Prepare(ctx, call)
	if failure != nil {
		return nil, ctx, failure
	}

	response, err := c.client.Do(request)
	if err != nil {
		return nil, ctx, c.Classify(ctx, err)
	}
	if response.Body != nil {
		response.Body = &contextBoundResponseBody{
			ReadCloser: response.Body,
			ctx:        request.Context(),
		}
	}
	responseCtx := c.Correlate(ctx, response.Header)
	return response, responseCtx, nil
}

// Buffered executes call, returns its response-path context, and reads the
// complete response under the configured cap. It binds the request to a derived
// context before execution, then starts the post-response timer only after Do
// returns successfully.
func (c *Caller) Buffered(ctx context.Context, call Call) (int, []byte, context.Context, *Failure) {
	inner, cancel := context.WithCancelCause(ctx)
	response, responseCtx, failure := c.Do(inner, call)
	if failure != nil {
		cancel(context.Canceled)
		return 0, nil, responseCtx, failure
	}
	defer func() {
		cancel(context.Canceled)
		_ = response.Body.Close()
	}()

	timer := time.AfterFunc(c.outboundTimeout, func() { cancel(context.DeadlineExceeded) })
	defer timer.Stop()

	body, readFailure := c.ReadBounded(response.Body)
	if readFailure == nil {
		return response.StatusCode, body, responseCtx, nil
	}
	return 0, nil, responseCtx, readFailure
}

// Classify maps an execution error to one Failure, consulting both err and
// ctx.Err(), and logging the cause once.
func (c *Caller) Classify(ctx context.Context, err error) *Failure {
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(ctx.Err(), context.DeadlineExceeded),
		errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		return c.failure(ctx, apierror.GatewayTimeout, "the upstream request timed out", false, err)
	case errors.Is(ctx.Err(), context.Canceled):
		return c.failure(ctx, 0, "", true, err)
	default:
		return c.failure(ctx, apierror.BadGateway, "could not reach the upstream", false, err)
	}
}

// Correlate adds a differing upstream request id to a response-path context,
// publishes that context for after-handler access logging, and returns it.
func (c *Caller) Correlate(ctx context.Context, header http.Header) context.Context {
	requestID, ok := logging.RequestIDFrom(ctx)
	if !ok {
		return ctx
	}
	upstreamRequestID := header.Get(RequestIDHeader)
	if upstreamRequestID == "" || upstreamRequestID == requestID {
		return ctx
	}
	correlated := logging.With(ctx, slog.String(logging.UpstreamRequestIDKey, upstreamRequestID))
	requestsummary.RecordCorrelation(ctx, correlated)
	c.logger.DebugContext(correlated, "upstream response correlation")
	return correlated
}

func (c *Caller) failure(ctx context.Context, kind apierror.Kind, message string, clientGone bool, err error) *Failure {
	c.logger.WarnContext(ctx, "upstream call failed", slog.Any(logging.ErrorKey, err))
	return &Failure{
		Kind:       kind,
		Message:    message,
		ClientGone: clientGone,
		Err:        err,
	}
}
