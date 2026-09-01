package catalog

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/requestsummary"
	"github.com/ningw42/copilotd/internal/upstream"
)

// Rendering bundles the request-time and representation concerns that stay
// outside the facts-only endpoint contract.
type Rendering struct {
	Render func([]Model) ([]byte, error)
	Codex  CodexDescriptor
}

// RenderDescriptors contains the complete renderer-specific contracts projected
// by the composition root. Its zero value preserves both provider-shaped catalogs.
type RenderDescriptors struct {
	Anthropic AnthropicRenderConfig
	Codex     CodexDescriptor
}

// CodexDescriptor contains the opt-in gate and pure-render settings for the
// OpenAI catalog's Codex client shape. A zero value preserves the provider-
// shaped Phase 6a response.
type CodexDescriptor struct {
	Enabled      bool
	Models       *cache.Value[[]byte]
	RenderConfig CodexRenderConfig
}

// Source performs one upstream call for the current Copilot model Catalog and
// returns its bounded bytes with the response-path context.
type Source interface {
	Buffered(ctx context.Context, call upstream.Call) (int, []byte, context.Context, *upstream.Failure)
}

var _ Source = (*upstream.Caller)(nil)

// Handler obtains one current Copilot Catalog and renders it for a Surface.
// Credential/transport details stay behind the narrow Source interface.
func Handler(logger *slog.Logger, ep endpoint.Catalog, rendering Rendering, source Source) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, body, responseCtx, failure := source.Buffered(r.Context(), upstream.Call{
			Route:                  ep.Upstream(),
			Method:                 http.MethodGet,
			AcceptIdentityEncoding: true,
		})
		if failure != nil {
			failure.RespondTo(w, ep.Surface())
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
		shape := requestsummary.CatalogShapeOpenAI
		var representation []byte
		if servesCodexShape(ep, rendering, r) {
			shape = requestsummary.CatalogShapeCodex
			var outcome CodexRenderOutcome
			currentBytes := embeddedCodexModels
			if rendering.Codex.Models != nil {
				currentBytes, _ = rendering.Codex.Models.Current()
			}
			var codexModels CodexModels
			codexModels, err = parseCodexModels(currentBytes)
			if err == nil {
				representation, outcome, err = RenderCodex(codexModels, filtered, rendering.Codex.RenderConfig)
			}
			if err == nil {
				for _, unapplied := range outcome.UnappliedAliases {
					logger.WarnContext(responseCtx, "Codex catalog alias mapping was not applied",
						slog.String(logging.ModelKey, unapplied.Alias),
						slog.String(logging.MetadataSourceKey, unapplied.Source),
						slog.String(logging.SkipReasonKey, string(unapplied.Reason)))
				}
				for _, skipped := range outcome.SkippedReviewers {
					logger.WarnContext(responseCtx, "Codex catalog reviewer was skipped",
						slog.String(logging.ModelKey, skipped.Model),
						slog.String(logging.ReviewerKey, skipped.Reviewer))
				}
			}
		} else {
			representation, err = rendering.Render(filtered)
		}
		if err != nil {
			apierror.Write(w, ep.Surface(), apierror.BadGateway, "could not render the models catalog")
			return
		}
		if ep.Surface() == endpoint.OpenAI {
			requestsummary.RecordCatalogShape(r.Context(), shape)
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
		(len(rendering.Codex.RenderConfig.ModelAliases) > 0 ||
			rendering.Codex.RenderConfig.AutoReviewModel != "" ||
			len(rendering.Codex.RenderConfig.AutoReviewModelOverrides) > 0 ||
			rendering.Codex.RenderConfig.OverrideLimits)
}
