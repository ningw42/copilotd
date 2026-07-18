package catalog

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/ningw42/copilotd/internal/apierror"
)

// Descriptor binds a provider-shaped catalog to its Surface, upstream Route
// membership predicate, and pure renderer.
type Descriptor struct {
	Surface       apierror.Surface
	RequiredRoute Route
	Render        func([]Model) ([]byte, error)
}

// Handler fetches one current Copilot catalog and renders it for a Surface.
// Credential/transport details stay behind the narrow Fetcher interface.
func Handler(desc Descriptor, fetcher Fetcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, body, err := fetcher.FetchModels(r.Context())
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			writeFetchError(w, desc.Surface, err)
			return
		}
		if status != http.StatusOK {
			apierror.Write(w, desc.Surface, apierror.BadGateway, "upstream models request failed")
			return
		}

		models, err := Decode(body)
		if err != nil {
			apierror.Write(w, desc.Surface, apierror.BadGateway, "upstream models response was invalid")
			return
		}
		representation, err := desc.Render(Filter(models, desc.RequiredRoute))
		if err != nil {
			apierror.Write(w, desc.Surface, apierror.BadGateway, "could not render the models catalog")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(representation)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(representation)
		}
	}
}

func writeFetchError(w http.ResponseWriter, surface apierror.Surface, err error) {
	switch {
	case errors.Is(err, ErrNoCredential):
		apierror.Write(w, surface, apierror.NotReady, "no upstream credential available")
	case errors.Is(err, ErrUpstreamTimeout):
		apierror.Write(w, surface, apierror.GatewayTimeout, "the upstream request timed out")
	default:
		apierror.Write(w, surface, apierror.BadGateway, "could not fetch the upstream models catalog")
	}
}
