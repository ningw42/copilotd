package modelmatch_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/modelmatch"
)

func TestResolvePrefersExactReportedModel(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
		modelmatch.Identity{Provider: "openai", Model: "gpt-5"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
		Method:   modelmatch.MethodExact,
	}
	if got := matcher.Resolve("gpt-5.6-sol"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveObservedSolFastReportedNameToBasePricingModel(t *testing.T) {
	t.Parallel()

	// Observed relationship: OpenAI usage captures requested gpt-5.6-sol-fast
	// and received the Reported model gpt-5.6-sol.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
		modelmatch.Identity{Provider: "openai", Model: "gpt-5"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
		Method:   modelmatch.MethodSuffix,
	}
	if got := matcher.Resolve("gpt-5.6-sol-fast"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveExactFastIdentityWinsOverSuffixFallback(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol-fast"},
		modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol-fast"},
		Method:   modelmatch.MethodExact,
	}
	if got := matcher.Resolve("gpt-5.6-sol-fast"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveSuffixPrefersLowercaseLiteralStemOverUppercaseAlternative(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
		modelmatch.Identity{Provider: "openai", Model: "GPT-5.6-SOL"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
		Method:   modelmatch.MethodSuffix,
	}
	if got := matcher.Resolve("gpt-5.6-sol-fast"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveUppercaseSuffixStemCollisionIsAmbiguous(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
		modelmatch.Identity{Provider: "openai", Model: "GPT-5.6-SOL"},
	)
	want := modelmatch.Resolution{Status: modelmatch.StatusAmbiguous}
	if got := matcher.Resolve("Gpt-5.6-sol-fast"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveDoesNotDiscardMiniVariant(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t, modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"})
	want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
	if got := matcher.Resolve("gpt-5.6-sol-mini"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveRejectsArbitrarySuffix(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t, modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"})
	want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
	if got := matcher.Resolve("gpt-5.6-sol-unknown"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolvePreservesOpenAINumericVersion(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t, modelmatch.Identity{Provider: "openai", Model: "gpt-5"})
	want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
	if got := matcher.Resolve("gpt-5.6"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolvePreservesClaudeNumericGeneration(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t, modelmatch.Identity{Provider: "anthropic", Model: "claude-sonnet-4"})
	want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
	if got := matcher.Resolve("claude-sonnet-4.6"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveObservedClaudeDottedNameToUniqueDatedIdentity(t *testing.T) {
	t.Parallel()

	// Observed identity relationship: Anthropic probes accepted the dotted
	// Haiku request spelling and returned this dated, hyphenated identity. The
	// design assigns those spellings these matcher roles; no usage capture of a
	// dotted Reported model is claimed.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
		Method:   modelmatch.MethodDated,
	}
	if got := matcher.Resolve("claude-haiku-4.5"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveExactDatedIdentityWinsOverUndatedFallback(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
		modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
		Method:   modelmatch.MethodExact,
	}
	if got := matcher.Resolve("claude-haiku-4-5-20251001"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveDatedReportedModelToUndatedBase(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5"},
		Method:   modelmatch.MethodSuffix,
	}
	if got := matcher.Resolve("claude-haiku-4-5-20251001"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveUndatedModelWithMultipleDatesIsAmbiguous(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
		modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5-20251101"},
	)
	want := modelmatch.Resolution{Status: modelmatch.StatusAmbiguous}
	if got := matcher.Resolve("claude-haiku-4-5"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveFinalDatedStageRequiresUniqueIdentity(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixtures: final dated matching requires one identity
	// across every eligible normalized stem, scope, and Reported fast removal.
	ambiguous := modelmatch.Resolution{Status: modelmatch.StatusAmbiguous}
	for _, fixture := range []struct {
		name       string
		reported   string
		candidates []modelmatch.Identity
		want       modelmatch.Resolution
	}{
		{
			name:     "Claude normalized stems with different dates",
			reported: "claude-haiku-4-5",
			candidates: []modelmatch.Identity{
				{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
				{Provider: "anthropic", Model: "claude-haiku-4.5-20251101"},
			},
			want: ambiguous,
		},
		{
			name:     "Claude normalized stems with the same date",
			reported: "claude-haiku-4-5",
			candidates: []modelmatch.Identity{
				{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
				{Provider: "anthropic", Model: "claude-haiku-4.5-20251001"},
			},
			want: ambiguous,
		},
		{
			name:     "ASCII normalized stems with different dates",
			reported: "base",
			candidates: []modelmatch.Identity{
				{Provider: "openai", Model: "base-20260301"},
				{Provider: "openai", Model: "BASE-20260401"},
			},
			want: ambiguous,
		},
		{
			name:     "ASCII normalized stems with the same date",
			reported: "base",
			candidates: []modelmatch.Identity{
				{Provider: "openai", Model: "base-20260301"},
				{Provider: "openai", Model: "BASE-20260301"},
			},
			want: ambiguous,
		},
		{
			name:     "recognized provider scope isolates one identity",
			reported: "openai/base",
			candidates: []modelmatch.Identity{
				{Provider: "openai", Model: "base-20260301"},
				{Provider: "google", Model: "BASE-20260401"},
			},
			want: modelmatch.Resolution{
				Status:   modelmatch.StatusMatched,
				Identity: modelmatch.Identity{Provider: "openai", Model: "base-20260301"},
				Method:   modelmatch.MethodDated,
			},
		},
		{
			name:     "unqualified query intersects original providers",
			reported: "base",
			candidates: []modelmatch.Identity{
				{Provider: "openai", Model: "base-20260301"},
				{Provider: "google", Model: "BASE-20260401"},
			},
			want: ambiguous,
		},
		{
			name:     "original and fast-removed stems are one eligible set",
			reported: "base-fast",
			candidates: []modelmatch.Identity{
				{Provider: "openai", Model: "base-fast-20260301"},
				{Provider: "openai", Model: "base-20260401"},
			},
			want: ambiguous,
		},
		{
			name:     "normalized collision remains eligible after fast removal",
			reported: "base-FAST",
			candidates: []modelmatch.Identity{
				{Provider: "openai", Model: "base-20260301"},
				{Provider: "openai", Model: "BASE-20260401"},
			},
			want: ambiguous,
		},
		{
			name:     "duplicate identical pair remains one identity",
			reported: "BASE",
			candidates: []modelmatch.Identity{
				{Provider: "openai", Model: "base-20260301"},
				{Provider: "openai", Model: "base-20260301"},
			},
			want: modelmatch.Resolution{
				Status:   modelmatch.StatusMatched,
				Identity: modelmatch.Identity{Provider: "openai", Model: "base-20260301"},
				Method:   modelmatch.MethodDated,
			},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			for _, order := range []string{"forward", "reverse"} {
				t.Run(order, func(t *testing.T) {
					candidates := append([]modelmatch.Identity(nil), fixture.candidates...)
					if order == "reverse" {
						for left, right := 0, len(candidates)-1; left < right; left, right = left+1, right-1 {
							candidates[left], candidates[right] = candidates[right], candidates[left]
						}
					}
					matcher := newMatcher(t, candidates...)
					if got := matcher.Resolve(fixture.reported); !reflect.DeepEqual(got, fixture.want) {
						t.Fatalf("Resolve() = %#v, want %#v", got, fixture.want)
					}
				})
			}
		})
	}
}

func TestResolveDatedModelDoesNotJumpSidewaysToAnotherDate(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5-20251101"},
	)
	want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
	if got := matcher.Resolve("claude-haiku-4-5-20251001"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveRejectsInvalidCalendarDateSuffix(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t, modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"})
	want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
	if got := matcher.Resolve("gpt-5.6-sol-2026-02-30"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveASCIICaseNormalizedModelPreservesCandidateSpelling(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t, modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"})
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
		Method:   modelmatch.MethodNormalized,
	}
	if got := matcher.Resolve("GPT-5.6-SOL"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveDoesNotTrimReportedModel(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t, modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"})
	want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
	if got := matcher.Resolve(" gpt-5.6-sol"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveRecognizedProviderQualificationConstrainsExactMatch(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
		modelmatch.Identity{Provider: "xai", Model: "gpt-5.6-sol"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
		Method:   modelmatch.MethodExact,
	}
	if got := matcher.Resolve("openai/gpt-5.6-sol"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveUnqualifiedExactCrossProviderCollisionIsAmbiguous(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "shared-model"},
		modelmatch.Identity{Provider: "google", Model: "shared-model"},
	)
	want := modelmatch.Resolution{Status: modelmatch.StatusAmbiguous}
	if got := matcher.Resolve("shared-model"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveExcludesResellerCandidates(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture from the design acceptance table.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "anthropic", Model: "shared-model"},
		modelmatch.Identity{Provider: "reseller", Model: "shared-model"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "anthropic", Model: "shared-model"},
		Method:   modelmatch.MethodExact,
	}
	if got := matcher.Resolve("shared-model"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveExactIdentityRemainsPresentWhenCallerHasNoPriceForIt(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture. The first identity represents an unpriced
	// original-provider entry that the caller must still supply to the matcher.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "gpt-unpriced"},
		modelmatch.Identity{Provider: "openai", Model: "GPT-UNPRICED"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "openai", Model: "gpt-unpriced"},
		Method:   modelmatch.MethodExact,
	}
	if got := matcher.Resolve("gpt-unpriced"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveRemovesFastAndDateInEitherTerminalOrder(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixtures for both permitted suffix orders.
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
		Method:   modelmatch.MethodSuffix,
	}
	for _, reported := range []string{
		"gpt-5.6-sol-fast-20260301",
		"gpt-5.6-sol-2026-03-01-FAST",
	} {
		t.Run(reported, func(t *testing.T) {
			t.Parallel()
			matcher := newMatcher(t, want.Identity)
			if got := matcher.Resolve(reported); !reflect.DeepEqual(got, want) {
				t.Fatalf("Resolve() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestResolveUndatedFastNameToUniqueDatedBase(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture combining the permitted Reported-name fast
	// fallback with the final observed Claude-style dated-candidate stage.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
		Method:   modelmatch.MethodDated,
	}
	if got := matcher.Resolve("claude-haiku-4.5-fast"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveDatedModelDoesNotJumpSidewaysAfterFastRemoval(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture for the explicit no-sideways-date rule.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5-20251101"},
	)
	want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
	if got := matcher.Resolve("claude-haiku-4-5-20251001-fast"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveNeverStripsFastFromCandidate(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture for the one-way Reported-name rule.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol-fast"},
	)
	want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
	if got := matcher.Resolve("gpt-5.6-sol"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveSuffixUsesFewestRemovals(t *testing.T) {
	t.Parallel()

	// Synthetic ranking fixture: retaining -fast costs one date removal,
	// while reaching the base costs both a date and a fast removal.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol-fast"},
		modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol-fast"},
		Method:   modelmatch.MethodSuffix,
	}
	if got := matcher.Resolve("gpt-5.6-sol-fast-20260301"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveRemovesAtMostOneSuffixOfEachKind(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixtures; the extra recognized-looking suffix remains
	// significant after the one permitted removal.
	matcher := newMatcher(t, modelmatch.Identity{Provider: "openai", Model: "base"})
	want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
	for _, reported := range []string{
		"base-fast-FAST",
		"base-20260301-2026-03-02",
	} {
		t.Run(reported, func(t *testing.T) {
			t.Parallel()
			if got := matcher.Resolve(reported); !reflect.DeepEqual(got, want) {
				t.Fatalf("Resolve() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestResolveRejectsIncompleteDateSuffix(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture for a date-like but incomplete suffix.
	matcher := newMatcher(t, modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"})
	want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
	if got := matcher.Resolve("gpt-5.6-sol-202603"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveDoesNotStripArbitraryProviderLikePrefix(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture: only recognized original-provider qualifiers
	// constrain a namespace; arbitrary prefixes remain part of the model bytes.
	matcher := newMatcher(t, modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"})
	want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
	if got := matcher.Resolve("gateway/gpt-5.6-sol"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveIsIndependentOfCandidateInputOrder(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture combines a retained-literal suffix winner with a
	// normalized collision, then reverses the complete candidate input.
	lower := modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"}
	upper := modelmatch.Identity{Provider: "openai", Model: "GPT-5.6-SOL"}
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: lower,
		Method:   modelmatch.MethodSuffix,
	}
	for name, candidates := range map[string][]modelmatch.Identity{
		"lower first": {lower, upper},
		"upper first": {upper, lower},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			matcher := newMatcher(t, candidates...)
			if got := matcher.Resolve("gpt-5.6-sol-fast"); !reflect.DeepEqual(got, want) {
				t.Fatalf("Resolve() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestResolveDeduplicatesIdenticalCandidatePairs(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture: repeated source pairs are one identity, not an
	// ambiguity.
	identity := modelmatch.Identity{Provider: "google", Model: "gemini-model"}
	matcher := newMatcher(t, identity, identity, identity)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: identity,
		Method:   modelmatch.MethodExact,
	}
	if got := matcher.Resolve("gemini-model"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveSupportsConcurrentImmutableReads(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture exercises one shared, fully constructed matcher.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
		modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
		Method:   modelmatch.MethodDated,
	}

	const callers = 64
	var wait sync.WaitGroup
	failures := make(chan modelmatch.Resolution, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if got := matcher.Resolve("anthropic/claude-haiku-4.5-fast"); !reflect.DeepEqual(got, want) {
				failures <- got
			}
		}()
	}
	wait.Wait()
	close(failures)
	for got := range failures {
		t.Fatalf("concurrent Resolve() = %#v, want %#v", got, want)
	}
}

func TestNewChargesOnlyActuallyRetainedUndatedKeys(t *testing.T) {
	t.Parallel()

	// Synthetic resource-policy fixture. openai/model retains four keys:
	// unscoped/scoped exact and unscoped/scoped normalized, totaling 36 bytes.
	candidate := modelmatch.Identity{Provider: "openai", Model: "model"}
	matcher, err := modelmatch.New(context.Background(), []modelmatch.Identity{candidate}, modelmatch.RetentionLimit{MaxIdentityBytes: 36})
	if err != nil {
		t.Fatalf("New() at literal retained-key boundary: %v", err)
	}
	if got := matcher.RetainedIdentityBytes(); got != 36 {
		t.Fatalf("RetainedIdentityBytes() = %d, want 36", got)
	}
	want := modelmatch.Resolution{Status: modelmatch.StatusMatched, Identity: candidate, Method: modelmatch.MethodExact}
	if got := matcher.Resolve("model"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}

	limited, err := modelmatch.New(context.Background(), []modelmatch.Identity{candidate}, modelmatch.RetentionLimit{MaxIdentityBytes: 35})
	if limited != nil || !errors.Is(err, modelmatch.ErrRetentionLimit) {
		t.Fatalf("New() below literal retained-key boundary = %#v, %v; want nil retention-limit error", limited, err)
	}
}

func TestNewAccountsForDuplicateCollisionAndDatedKeysActuallyRetained(t *testing.T) {
	t.Parallel()

	// Synthetic literal key-retention fixtures. Expectations count only keys
	// actually inserted into the three public matcher's indexes.
	for _, fixture := range []struct {
		name       string
		candidates []modelmatch.Identity
		wantBytes  int
	}{
		{
			name: "duplicate pair",
			candidates: []modelmatch.Identity{
				{Provider: "openai", Model: "model"},
				{Provider: "openai", Model: "model"},
				{Provider: "openai", Model: "model"},
			},
			wantBytes: 36,
		},
		{
			name: "normalized collision",
			candidates: []modelmatch.Identity{
				{Provider: "openai", Model: "base"},
				{Provider: "openai", Model: "BASE"},
			},
			wantBytes: 48,
		},
		{
			name:       "dated keys use shorter stem",
			candidates: []modelmatch.Identity{{Provider: "openai", Model: "base-20260301"}},
			wantBytes:  84,
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			matcher, err := modelmatch.New(context.Background(), fixture.candidates, modelmatch.RetentionLimit{MaxIdentityBytes: fixture.wantBytes})
			if err != nil {
				t.Fatalf("New() at %d-byte boundary: %v", fixture.wantBytes, err)
			}
			if got := matcher.RetainedIdentityBytes(); got != fixture.wantBytes {
				t.Fatalf("RetainedIdentityBytes() = %d, want %d", got, fixture.wantBytes)
			}
			limited, err := modelmatch.New(context.Background(), fixture.candidates, modelmatch.RetentionLimit{MaxIdentityBytes: fixture.wantBytes - 1})
			if limited != nil || !errors.Is(err, modelmatch.ErrRetentionLimit) {
				t.Fatalf("New() below %d-byte boundary = %#v, %v", fixture.wantBytes, limited, err)
			}
		})
	}
}

func TestNewHonorsCanceledIndexConstruction(t *testing.T) {
	t.Parallel()

	// Synthetic constructor fixture.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	matcher, err := modelmatch.New(ctx, []modelmatch.Identity{{Provider: "openai", Model: "model"}}, unlimitedRetention())
	if matcher != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("New() = %#v, %v; want nil, context canceled", matcher, err)
	}
}

func TestResolveNormalizationDoesNotRewriteOtherIdentitySyntax(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixtures for normalization's closed allowlist.
	for name, fixture := range map[string]struct {
		reported  string
		candidate modelmatch.Identity
	}{
		"OpenAI numeric punctuation": {reported: "gpt-5.6", candidate: modelmatch.Identity{Provider: "openai", Model: "gpt-5-6"}},
		"word order":                 {reported: "claude-4-opus", candidate: modelmatch.Identity{Provider: "anthropic", Model: "claude-opus-4"}},
		"Unicode case":               {reported: "é-model", candidate: modelmatch.Identity{Provider: "google", Model: "É-model"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			matcher := newMatcher(t, fixture.candidate)
			want := modelmatch.Resolution{Status: modelmatch.StatusUnknown}
			if got := matcher.Resolve(fixture.reported); !reflect.DeepEqual(got, want) {
				t.Fatalf("Resolve() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestResolveNormalizedAmbiguityStopsBeforeWeakerSuffixMatch(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture: the full Reported name collides at the
	// normalized stage, while removing -fast would otherwise expose one base.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "base-fast"},
		modelmatch.Identity{Provider: "openai", Model: "BASE-FAST"},
		modelmatch.Identity{Provider: "openai", Model: "Base"},
	)
	want := modelmatch.Resolution{Status: modelmatch.StatusAmbiguous}
	if got := matcher.Resolve("Base-fast"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveSuffixAmbiguityStopsBeforeWeakerDatedMatch(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture: removing -fast collides after normalization,
	// while a dated candidate has the full pre-removal stem.
	matcher := newMatcher(t,
		modelmatch.Identity{Provider: "openai", Model: "base"},
		modelmatch.Identity{Provider: "openai", Model: "BASE"},
		modelmatch.Identity{Provider: "openai", Model: "Base-fast-20260301"},
	)
	want := modelmatch.Resolution{Status: modelmatch.StatusAmbiguous}
	if got := matcher.Resolve("Base-fast"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveFastRemovalPreservesRetainedLiteralCasing(t *testing.T) {
	t.Parallel()

	// Synthetic policy fixture: case-insensitive suffix recognition must not
	// case-fold the retained spelling before its literal rank.
	mixed := modelmatch.Identity{Provider: "openai", Model: "Gpt-5.6-sol"}
	matcher := newMatcher(t,
		mixed,
		modelmatch.Identity{Provider: "openai", Model: "gpt-5.6-sol"},
	)
	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: mixed,
		Method:   modelmatch.MethodSuffix,
	}
	if got := matcher.Resolve("Gpt-5.6-sol-FAST"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestNewDetachesCandidateSlice(t *testing.T) {
	t.Parallel()

	// Synthetic immutability fixture.
	candidates := []modelmatch.Identity{{Provider: "xai", Model: "grok-model"}}
	matcher, err := modelmatch.New(context.Background(), candidates, unlimitedRetention())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	candidates[0] = modelmatch.Identity{Provider: "openai", Model: "replacement"}

	want := modelmatch.Resolution{
		Status:   modelmatch.StatusMatched,
		Identity: modelmatch.Identity{Provider: "xai", Model: "grok-model"},
		Method:   modelmatch.MethodExact,
	}
	if got := matcher.Resolve("grok-model"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() after candidate mutation = %#v, want %#v", got, want)
	}
}

func newMatcher(t *testing.T, candidates ...modelmatch.Identity) *modelmatch.Matcher {
	t.Helper()
	matcher, err := modelmatch.New(context.Background(), candidates, unlimitedRetention())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return matcher
}

func unlimitedRetention() modelmatch.RetentionLimit {
	return modelmatch.RetentionLimit{MaxIdentityBytes: int(^uint(0) >> 1)}
}
