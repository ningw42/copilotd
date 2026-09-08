// Package reporthttp adapts daemon-owned Usage reports to the local HTTP
// protocol. It has no inference, credential, or readiness dependencies.
package reporthttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
)

const Path = "/usage/v1/report"
const (
	MaxRawQueryBytes = 16 << 10
	AdmissionSlots   = 2
	WorkTimeout      = 5 * time.Second
	WriteTimeout     = 5 * time.Second
)

type QueryFunc func(context.Context, report.Query) (report.Report, error)

// Handler registers no paths itself. A nil QueryFunc explicitly disables
// reporting, including parsing and storage access.
func Handler(query QueryFunc) http.Handler {
	slots := make(chan struct{}, AdmissionSlots)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Arm network deadlines only when work ends. An earlier read deadline
		// can cancel net/http's background read (and the request context) before
		// the work deadline. writeBody bounds every normal/early response; panic
		// unwinding must give outer generic recovery the same fresh budget.
		defer func() {
			if failure := recover(); failure != nil {
				_ = setResponseDeadline(w)
				panic(failure)
			}
		}()
		if r.Method != "GET" && r.Method != "HEAD" {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, r, 405, "method_not_allowed", "Use GET or HEAD.")
			return
		}
		if query == nil {
			writeError(w, r, 503, "usage_meter_disabled", "Usage meter is disabled on this daemon.")
			return
		}
		q, err := parseQuery(r.URL.RawQuery)
		if err != nil {
			writeError(w, r, 400, "invalid_query", "Invalid report query parameters.")
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", "1")
			writeError(w, r, 429, "report_busy", "All report slots are occupied; try again.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), WorkTimeout)
		defer cancel()
		result, err := query(ctx, q)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			writeError(w, r, 504, "report_timeout", "Report work timed out.")
			return
		}
		if err != nil {
			writeFailure(w, r, err)
			return
		}
		body, err := encodeReport(ctx, result)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			writeError(w, r, 504, "report_timeout", "Report work timed out.")
			return
		}
		if err != nil {
			writeFailure(w, r, err)
			return
		}
		if ctx.Err() != nil {
			return
		}
		writeBody(w, r, 200, body)
	})
}
func parseQuery(raw string) (report.Query, error) {
	if len(raw) > MaxRawQueryBytes {
		return report.Query{}, errors.New("query too long")
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return report.Query{}, err
	}
	for key, value := range values {
		switch key {
		case "period", "since", "until", "timezone", "surface", "model":
		default:
			return report.Query{}, errors.New("unknown key")
		}
		if len(value) != 1 || value[0] == "" {
			return report.Query{}, errors.New("empty or duplicate value")
		}
	}
	if _, ok := values["timezone"]; !ok {
		return report.Query{}, errors.New("timezone required")
	}
	q := report.Query{Period: values.Get("period"), Since: values.Get("since"), Until: values.Get("until"), Timezone: values.Get("timezone"), Surface: values.Get("surface")}
	if model, ok := values["model"]; ok {
		q.Model = &model[0]
	}
	return q, nil
}
func writeFailure(w http.ResponseWriter, r *http.Request, err error) {
	var failure *report.Error
	if !errors.As(err, &failure) {
		writeError(w, r, 503, "usage_unavailable", "Usage data is unavailable on this daemon.")
		return
	}
	status := 503
	switch failure.Code {
	case report.InvalidQuery:
		status = 400
	case report.TooLarge, report.Overflow:
		status = 422
	case report.Timeout:
		status = 504
	}
	writeError(w, r, status, string(failure.Code), failure.Message)
}
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	body, _ := json.Marshal(struct {
		SchemaVersion int `json:"schema_version"`
		Error         struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{SchemaVersion: 1, Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message}})
	writeBody(w, r, status, body)
}
func writeBody(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	if err := setResponseDeadline(w); err != nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method != "HEAD" {
		_, _ = w.Write(body)
	}
}

func setResponseDeadline(w http.ResponseWriter) error {
	// HTTP/1 can drain an unread request body while flushing a response, even
	// after the handler returns (HEAD/small errors). Bound that read with the
	// SAME budget as writing; a write deadline alone cannot interrupt it.
	// Leave both deadlines in place for net/http's final flush/body cleanup;
	// net/http resets them before reusing the connection for another request.
	// Production wrappers support ResponseController via Unwrap; recorders may
	// not. Never set a global timeout that would deadline inference or SSE.
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(WriteTimeout)
	if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}
