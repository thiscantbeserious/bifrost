package complexity

import (
	"fmt"
	"strings"
)

// EditableKeywordConfig is the user-facing subset of keyword lists that can be
// edited through the governance UI and config file. Every other keyword list
// the analyzer uses (weak reasoning, referential phrases, task-shift phrases,
// enum/comprehensiveness/elaboration/limiting markers) is an analyzer internal
// and always resolves from built-in defaults.
//
// ReasoningKeywords maps to the tier-override gate (strongReasoningKeywords),
// because that is what users actually want to control when they say "route
// these phrases to the reasoning model".
type EditableKeywordConfig struct {
	CodeKeywords      []string `json:"code_keywords"`
	ReasoningKeywords []string `json:"reasoning_keywords"`
	TechnicalKeywords []string `json:"technical_keywords"`
	SimpleKeywords    []string `json:"simple_keywords"`
}

// KeywordConfig is the full internal keyword configuration used by the
// compiled matcher. It is assembled from EditableKeywordConfig + defaults at
// analyzer-build time and is not part of the persisted or API surface.
type KeywordConfig struct {
	CodeKeywords              []string
	StrongReasoningKeywords   []string
	WeakReasoningKeywords     []string
	TechnicalKeywords         []string
	SimpleKeywords            []string
	EnumTriggers              []string
	ComprehensivenessMarkers  []string
	ElaborationMarkers        []string
	LimitingQualifiers        []string
	ReferentialPhrases        []string
	ReferentialReferenceWords []string
	ReferentialActionWords    []string
	TaskShiftPhrases          []string
}

// AnalyzerConfig is the full runtime configuration for the complexity analyzer.
// Persisted to the governance config store and exchanged over the management
// API in this shape.
type AnalyzerConfig struct {
	TierBoundaries TierBoundaries        `json:"tier_boundaries"`
	Keywords       EditableKeywordConfig `json:"keywords"`
}

// Validate checks that the analyzer config is internally consistent: tier
// boundaries are strictly ordered inside (0, 1) and every editable keyword
// list is non-empty after normalization. An empty list would wipe a scoring
// dimension, so it is rejected at the API boundary.
func (c *AnalyzerConfig) Validate() error {
	if c == nil {
		return nil
	}
	if err := c.TierBoundaries.Validate(); err != nil {
		return err
	}
	var missing []string
	if len(c.Keywords.CodeKeywords) == 0 {
		missing = append(missing, "code_keywords")
	}
	if len(c.Keywords.ReasoningKeywords) == 0 {
		missing = append(missing, "reasoning_keywords")
	}
	if len(c.Keywords.TechnicalKeywords) == 0 {
		missing = append(missing, "technical_keywords")
	}
	if len(c.Keywords.SimpleKeywords) == 0 {
		missing = append(missing, "simple_keywords")
	}
	if len(missing) > 0 {
		return fmt.Errorf("keyword lists must be non-empty: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Normalized returns a canonical copy suitable for persistence and runtime use.
// Keyword entries are lowercased, trimmed, and deduplicated.
func (c *AnalyzerConfig) Normalized() AnalyzerConfig {
	if c == nil {
		return DefaultAnalyzerConfig()
	}

	return AnalyzerConfig{
		TierBoundaries: c.TierBoundaries,
		Keywords: EditableKeywordConfig{
			CodeKeywords:      normalizeKeywordList(c.Keywords.CodeKeywords),
			ReasoningKeywords: normalizeKeywordList(c.Keywords.ReasoningKeywords),
			TechnicalKeywords: normalizeKeywordList(c.Keywords.TechnicalKeywords),
			SimpleKeywords:    normalizeKeywordList(c.Keywords.SimpleKeywords),
		},
	}
}

// MergeOntoDefaults overlays the editable keyword lists onto the built-in
// defaults and returns the full internal KeywordConfig used by the compiled
// matcher. Empty editable lists fall through to defaults as a defensive guard;
// Validate() should have already rejected them at the API boundary.
func (e EditableKeywordConfig) MergeOntoDefaults() KeywordConfig {
	kw := defaultFullKeywordConfig()
	if len(e.CodeKeywords) > 0 {
		kw.CodeKeywords = append([]string(nil), e.CodeKeywords...)
	}
	if len(e.ReasoningKeywords) > 0 {
		kw.StrongReasoningKeywords = append([]string(nil), e.ReasoningKeywords...)
	}
	if len(e.TechnicalKeywords) > 0 {
		kw.TechnicalKeywords = append([]string(nil), e.TechnicalKeywords...)
	}
	if len(e.SimpleKeywords) > 0 {
		kw.SimpleKeywords = append([]string(nil), e.SimpleKeywords...)
	}
	return kw
}

// Validate checks that tier boundaries satisfy the required ordering.
func (b *TierBoundaries) Validate() error {
	if !(0 < b.SimpleMedium &&
		b.SimpleMedium < b.MediumComplex &&
		b.MediumComplex < b.ComplexReasoning &&
		b.ComplexReasoning < 1) {
		return fmt.Errorf("tier boundaries must satisfy 0 < simple_medium (%.4f) < medium_complex (%.4f) < complex_reasoning (%.4f) < 1",
			b.SimpleMedium, b.MediumComplex, b.ComplexReasoning)
	}
	return nil
}

// DefaultEditableKeywordConfig returns the user-visible default keyword lists.
// ReasoningKeywords seeds from strongReasoningKeywords because that is the
// list users are actually editing when they say "reasoning keywords".
func DefaultEditableKeywordConfig() EditableKeywordConfig {
	return EditableKeywordConfig{
		CodeKeywords:      cloneStringSlice(codeKeywords),
		ReasoningKeywords: cloneStringSlice(strongReasoningKeywords),
		TechnicalKeywords: cloneStringSlice(technicalKeywords),
		SimpleKeywords:    cloneStringSlice(simpleKeywords),
	}
}

// DefaultAnalyzerConfig returns the default thresholds and user-visible
// keyword lists.
func DefaultAnalyzerConfig() AnalyzerConfig {
	return AnalyzerConfig{
		TierBoundaries: DefaultTierBoundaries(),
		Keywords:       DefaultEditableKeywordConfig(),
	}
}

// defaultFullKeywordConfig returns the complete internal keyword set, with all
// 13 lists populated from the package-level defaults. Used by the matcher and
// by MergeOntoDefaults as the base that editable lists overlay onto.
func defaultFullKeywordConfig() KeywordConfig {
	return KeywordConfig{
		CodeKeywords:              cloneStringSlice(codeKeywords),
		StrongReasoningKeywords:   cloneStringSlice(strongReasoningKeywords),
		WeakReasoningKeywords:     cloneStringSlice(weakReasoningKeywords),
		TechnicalKeywords:         cloneStringSlice(technicalKeywords),
		SimpleKeywords:            cloneStringSlice(simpleKeywords),
		EnumTriggers:              cloneStringSlice(enumTriggers),
		ComprehensivenessMarkers:  cloneStringSlice(comprehensivenessMarkers),
		ElaborationMarkers:        cloneStringSlice(elaborationMarkers),
		LimitingQualifiers:        cloneStringSlice(limitingQualifiers),
		ReferentialPhrases:        cloneStringSlice(referentialPhrases),
		ReferentialReferenceWords: cloneStringSlice(referentialReferenceWords),
		ReferentialActionWords:    cloneStringSlice(referentialActionWords),
		TaskShiftPhrases:          cloneStringSlice(taskShiftPhrases),
	}
}

func normalizeKeywordList(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}
