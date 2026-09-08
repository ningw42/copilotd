package report

import (
	"context"
	"errors"
)

// Code classifies report-domain failures independently of inference errors.
type Code string

const (
	InvalidQuery Code = "invalid_query"
	TooLarge     Code = "report_too_large"
	Overflow     Code = "aggregation_overflow"
	Unavailable  Code = "usage_unavailable"
	Timeout      Code = "report_timeout"
)

// Error contains only public, path-free guidance. Driver errors never escape
// this module. Cancellation remains detectable through errors.Is.
type Error struct {
	Code    Code
	Message string
	cause   error
}

func (e *Error) Error() string     { return e.Message }
func (e *Error) Unwrap() error     { return e.cause }
func invalid(message string) error { return &Error{Code: InvalidQuery, Message: message} }
func tooLarge() error {
	return &Error{Code: TooLarge, Message: "Report exceeds a fixed limit; narrow the date range or model/Surface selection."}
}

func publicError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		code := Unavailable
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = Timeout
		}
		return &Error{Code: code, Message: "Report work was canceled or timed out.", cause: ctx.Err()}
	}
	var failure *Error
	if errors.As(err, &failure) {
		return failure
	}
	return &Error{Code: Unavailable, Message: "Usage data is unavailable on this daemon."}
}

const (
	MaxDates   = 3660
	MaxBuckets = 4096
)
