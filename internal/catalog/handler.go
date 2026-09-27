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
	"github.com/ningw42/copilotd/internal/upstream"
)

// RenderDescriptors contains the complete renderer-specific contracts projected
// by the composition root. Its zero value preserves both provider-shaped catalogs.
type RenderDescriptors struct {
	Anthropic AnthropicRenderConfig
	Codex     CodexDescriptor
}

// CodexDescriptor contains the models source and pure-render settings for the
// OpenAI catalog's Codex client shape. A present Models source, the Codex
// models.json cached value, enables the Codex catalog; a nil one disables it.
// There is no implicit vendored-snapshot source.
type CodexDescriptor struct {
	Models       *cache.Value[[]byte]
	RenderConfig CodexRenderConfig
}

// Source performs one upstream call for the current Copilot model Catalog and
// returns its bounded bytes with the response-path context.
type Source interface {
	Buffered(ctx context.Context, call upstream.Call) (int, []byte, context.Context, *upstream.Failure)
}

var _ Source = (*upstream.Caller)(nil)

// Handler obtains one current Copilot Catalog and renders it in the
// representation the Catalog Endpoint's Surface selects, configured by
// descriptors. Credential/transport details stay behind the narrow Source
// interface. recordShape, when non-nil, receives the request context and the
// OpenAI Catalog Shape after each successful render; it is a narrow hook so
// catalog never imports requestsummary.
func Handler(logger *slog.Logger, ep endpoint.Catalog, descriptors RenderDescriptors, source Source, recordShape func(context.Context, Shape)) http.HandlerFunc {
	if ep.Surface() != endpoint.OpenAI {
		// Shape names OpenAI Catalog representations; the Anthropic Catalog has
		// one representation and records none.
		recordShape = nil
	}
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
		representation, shape, err := descriptors.render(ep, r, filtered, responseCtx, logger)
		if err != nil {
			apierror.Write(w, ep.Surface(), apierror.BadGateway, "could not render the models catalog")
			return
		}
		if recordShape != nil {
			recordShape(r.Context(), shape)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(representation)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(representation)
		}
	}
}

// render renders models in the representation ep's Surface selects and returns
// the Shape actually served. It owns the OpenAI Catalog's shape decision:
// whether the Codex client shape or the OpenAI provider shape wins. The
// Anthropic Catalog has a single representation and reports no Shape.
func (d RenderDescriptors) render(ep endpoint.Catalog, r *http.Request, models []Model, responseCtx context.Context, logger *slog.Logger) ([]byte, Shape, error) {
	if ep.Surface() != endpoint.OpenAI {
		representation, err := RenderAnthropicWithConfig(models, d.Anthropic)
		return representation, "", err
	}
	if !d.Codex.servesCodexShape(ep, r) {
		representation, err := RenderOpenAI(models)
		return representation, ShapeOpenAI, err
	}

	currentBytes, _ := d.Codex.Models.Current()
	codexModels, err := parseCodexModels(currentBytes)
	if err != nil {
		return nil, "", err
	}
	representation, outcome, err := RenderCodex(codexModels, models, d.Codex.RenderConfig)
	if err != nil {
		return nil, "", err
	}
	for _, unapplied := range outcome.UnappliedAliases {
		logger.WarnContext(responseCtx, "Codex catalog alias mapping was not applied",
			slog.String(logging.ModelKey, unapplied.Alias),
			slog.String(logging.MetadataSourceKey, unapplied.MetadataSource),
			slog.String(logging.SkipReasonKey, string(unapplied.Reason)))
	}
	for _, skipped := range outcome.SkippedReviewers {
		logger.WarnContext(responseCtx, "Codex catalog reviewer was skipped",
			slog.String(logging.ModelKey, skipped.Model),
			slog.String(logging.ReviewerKey, skipped.Reviewer))
	}
	return representation, ShapeCodex, nil
}
