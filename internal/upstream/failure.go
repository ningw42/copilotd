// Package upstream owns the shared policy for authenticated calls to GitHub
// Copilot while leaving each caller's response tail transport-specific.
package upstream

import (
	"net/http"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/endpoint"
)

// Failure is one classified upstream call failure, already mapped to the
// copilotd-originated signal that answers it.
type Failure struct {
	Kind       apierror.Kind // signal to render; not consulted when ClientGone is set
	Message    string        // human-readable text, rendered in the Surface's dialect
	ClientGone bool          // caller disconnected; nothing may be written
	Err        error         // underlying cause; logged once at classification, never rendered
}

// RespondTo renders f on w in surface's dialect and reports whether it wrote.
// A ClientGone failure writes nothing and reports false, so callers can skip
// metrics and logging that only a rendered failure warrants.
func (f *Failure) RespondTo(w http.ResponseWriter, surface endpoint.Surface) bool {
	if f.ClientGone {
		return false
	}
	apierror.Write(w, surface, f.Kind, f.Message)
	return true
}
