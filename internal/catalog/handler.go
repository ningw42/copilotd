package catalog

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/endpoint"
)

// Rendering bundles the request-time and representation concerns that stay
// outside the facts-only endpoint contract.
type Rendering struct {
	Render func([]Model) ([]byte, error)
	Codex  CodexDescriptor
	Logger *slog.Logger
}

// CodexDescriptor contains the opt-in gate and pure-render settings for the
// OpenAI catalog's Codex client shape. A zero value preserves the provider-
// shaped Phase 6a response.
type CodexDescriptor struct {
	Enabled      bool
	RenderConfig CodexRenderConfig
}

// Handler fetches one current Copilot catalog and renders it for a Surface.
// Credential/transport details stay behind the narrow Fetcher interface.
func Handler(ep endpoint.Catalog, rendering Rendering, fetcher Fetcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, body, err := fetcher.FetchModels(r.Context(), ep.Upstream())
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			writeFetchError(w, ep.Surface(), err)
			return
		}
		if status != http.StatusOK {
			apierror.Write(w, ep.Surface(), apierror.BadGateway, "upstream models request failed")
			return
		}

		models, err := Decode(body)
		if err != nil {
			apierror.Write(w, ep.Surface(), apierror.BadGateway, "upstream models response was invalid")
			return
		}
		filtered := Filter(models, ep.RequiredRoute())
		shape := CatalogShapeOpenAI
		var representation []byte
		if servesCodexShape(ep, rendering, r) {
			shape = CatalogShapeCodex
			var outcome CodexRenderOutcome
			representation, outcome, err = RenderCodex(filtered, rendering.Codex.RenderConfig)
			if err == nil && outcome.SkippedReviewer != "" && rendering.Logger != nil {
				rendering.Logger.WarnContext(r.Context(), "Codex catalog reviewer was skipped",
					slog.String("reviewer", outcome.SkippedReviewer))
			}
		} else {
			representation, err = rendering.Render(filtered)
		}
		if err != nil {
			apierror.Write(w, ep.Surface(), apierror.BadGateway, "could not render the models catalog")
			return
		}
		if ep.Surface() == endpoint.OpenAI {
			StoreCatalogShape(r.Context(), shape)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(representation)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(representation)
		}
	}
}

func servesCodexShape(ep endpoint.Catalog, rendering Rendering, r *http.Request) bool {
	return ep.Surface() == endpoint.OpenAI &&
		r.URL.Query().Has("client_version") &&
		rendering.Codex.Enabled &&
		(rendering.Codex.RenderConfig.AutoReviewModel != "" || rendering.Codex.RenderConfig.OverrideLimits)
}

func writeFetchError(w http.ResponseWriter, surface endpoint.Surface, err error) {
	switch {
	case errors.Is(err, ErrNoCredential):
		apierror.Write(w, surface, apierror.NotReady, "no upstream credential available")
	case errors.Is(err, ErrUpstreamTimeout):
		apierror.Write(w, surface, apierror.GatewayTimeout, "the upstream request timed out")
	default:
		apierror.Write(w, surface, apierror.BadGateway, "could not fetch the upstream models catalog")
	}
}
