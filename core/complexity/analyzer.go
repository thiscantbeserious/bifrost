package complexity

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ComplexityInput is the normalized input for the analyzer.
// The caller is responsible for extracting text from request payloads.
type ComplexityInput struct {
	LastUserText   string   // last user message text
	PriorUserTexts []string // previous user message texts (up to 10)
	SystemText     string   // concatenated system/developer prompt text
}

// ComplexityContributions captures the weighted contribution of each lexical
// dimension to the last-message score before conversation blending/floors.
type ComplexityContributions struct {
	Code          float64
	Reasoning     float64
	Technical     float64
	SimplePenalty float64
	TokenCount    float64
}

// ComplexityResult holds the computed complexity scores and tier classification.
type ComplexityResult struct {
	// Weighted total score (0.0–1.0)
	Score float64
	// Computed tier: "SIMPLE", "MEDIUM", "COMPLEX", or "REASONING"
	Tier string

	// Individual dimension scores used in the weighted sum (0.0–1.0 each)
	CodePresence       float64
	ReasoningMarkers   float64
	TechnicalTerms     float64
	SimpleIndicators   float64
	TokenCount         float64
	ConversationCtx    float64
	SystemPromptSignal float64 // net weighted contribution from system lexical assist
	OutputComplexity   float64

	// Weighted contributions for the last message score.
	Contributions ComplexityContributions

	// Debug info — match counts per dimension (for logging, not exposed to CEL)
	CodeMatchCount      int
	ReasoningMatchCount int
	TechnicalMatchCount int
	SimpleMatchCount    int
	OutputMatchCount    int

	// Debug internals for eval/logging
	LastMessageScore    float64
	ConversationBlend   float64
	ReferentialFollowup bool
	SimpleWeightApplied float64
	OutputFloorMinScore float64
	OutputFloorApplied  bool
	WordCount           int
}

// TierBoundaries defines the score thresholds for tier classification.
type TierBoundaries struct {
	SimpleMedium     float64 `json:"simple_medium"`
	MediumComplex    float64 `json:"medium_complex"`
	ComplexReasoning float64 `json:"complex_reasoning"`
}

// DefaultTierBoundaries returns the default tier boundary thresholds.
func DefaultTierBoundaries() TierBoundaries {
	return TierBoundaries{
		SimpleMedium:     0.15,
		MediumComplex:    0.35,
		ComplexReasoning: 0.60,
	}
}

// ComplexityAnalyzer computes complexity scores from normalized text input.
// It is stateless and safe for concurrent use.
type ComplexityAnalyzer struct {
	tierBoundaries TierBoundaries
	matcher        *compiledKeywordMatcher
}

// NewComplexityAnalyzer creates a new analyzer with the given tier boundaries.
// If boundaries is nil, default boundaries are used.
func NewComplexityAnalyzer(boundaries *TierBoundaries) *ComplexityAnalyzer {
	config := DefaultAnalyzerConfig()
	if boundaries != nil {
		config.TierBoundaries = *boundaries
	}
	return NewComplexityAnalyzerWithConfig(&config)
}

// NewComplexityAnalyzerWithConfig creates a new analyzer with the full runtime config.
// If config is nil, built-in defaults are used.
func NewComplexityAnalyzerWithConfig(config *AnalyzerConfig) *ComplexityAnalyzer {
	resolved := DefaultAnalyzerConfig()
	if config != nil {
		candidate := config.Normalized()
		if err := candidate.Validate(); err == nil {
			resolved = candidate
		}
	}
	fullKeywords := resolved.Keywords.MergeOntoDefaults()
	return &ComplexityAnalyzer{
		tierBoundaries: resolved.TierBoundaries,
		matcher:        newCompiledKeywordMatcher(fullKeywords),
	}
}

// Analyze computes complexity scores from the normalized input.
func (a *ComplexityAnalyzer) Analyze(input ComplexityInput) *ComplexityResult {
	lastScanMask := lastTextBaseScanMask
	if len(input.PriorUserTexts) > 0 {
		lastScanMask = lastTextFullScanMask
	}

	lastSignals := a.matcher.analyzeText(input.LastUserText, lastScanMask)
	systemSignals := a.matcher.analyzeText(input.SystemText, systemTextScanMask)

	// Primary message signals
	userCodeScore := scoreCount(lastSignals.codeCount, 3)
	reasoningScore := scoreCount(lastSignals.reasoningCount, 2)
	userTechnicalScore := scoreCount(lastSignals.technicalCount, 3)
	userSimpleScore := scoreCount(lastSignals.simpleCount, 2)
	outputScore, outputCount := scoreOutputComplexity(lastSignals)
	tokenScore := scoreTokenCount(lastSignals.wordCount)

	// System prompt provides soft lexical context for code/technical/simple signals,
	// but never drives reasoning override, token count, or output complexity.
	systemCodeScore := scoreCount(systemSignals.codeCount, 3)
	systemTechnicalScore := scoreCount(systemSignals.technicalCount, 3)
	systemSimpleScore := scoreCount(systemSignals.simpleCount, 2)

	codeScore := clamp(userCodeScore+(systemCodeScore*systemPromptAssistFactor), 0.0, 1.0)
	technicalScore := clamp(userTechnicalScore+(systemTechnicalScore*systemPromptAssistFactor), 0.0, 1.0)
	simpleScore := clamp(userSimpleScore+(systemSimpleScore*systemPromptAssistFactor), 0.0, 1.0)

	// Conditional simple dampener:
	// Only apply full dampener on short, low-signal asks
	wordCount := lastSignals.wordCount
	effectiveSimpleWeight := simpleWeight
	signalCount := 0
	if userCodeScore >= 0.3 {
		signalCount++
	}
	if userTechnicalScore >= 0.3 {
		signalCount++
	}
	if reasoningScore >= 0.3 {
		signalCount++
	}
	if lastSignals.simpleCount > 0 && (wordCount >= 30 || signalCount >= 2) {
		effectiveSimpleWeight = 0.01
	}

	systemLexicalContribution := ((codeScore - userCodeScore) * codeWeight) +
		((technicalScore - userTechnicalScore) * technicalWeight) -
		((simpleScore - userSimpleScore) * effectiveSimpleWeight)

	codeContribution := codeScore * codeWeight
	reasoningContribution := reasoningScore * reasoningWeight
	technicalContribution := technicalScore * technicalWeight
	simplePenalty := -(simpleScore * effectiveSimpleWeight)
	tokenContribution := tokenScore * tokenCountWeight

	// Weighted sum for last message (output complexity applied separately as a score floor)
	lastMsgScore := codeContribution +
		reasoningContribution +
		technicalContribution +
		simplePenalty +
		tokenContribution
	lastMsgScore = clamp(lastMsgScore, 0.0, 1.0)

	// Conversation context score (prior user turns only)
	// Only blend when there is conversation history; otherwise use 100% last message score
	var blended float64
	var convScore float64
	referentialFollowup := false
	if len(input.PriorUserTexts) > 0 {
		convScore = a.scoreConversationContext(input.PriorUserTexts)
		lastWeight := defaultLastMessageBlendWeight
		contextWeight := defaultConversationBlendWeight
		if isReferentialFollowup(lastSignals, lastMsgScore, convScore, wordCount) {
			lastWeight = referentialLastMessageBlendWeight
			contextWeight = referentialConversationBlendWeight
			referentialFollowup = true
		}

		weightedBlend := (lastMsgScore * lastWeight) + (convScore * contextWeight)
		blended = math.Max(lastMsgScore, weightedBlend)
	} else {
		blended = lastMsgScore
	}

	// Output complexity as a score floor: strong output signals set a minimum score
	// This handles requests like "list every X and explain each" where the output
	// will be huge even though input keywords are sparse
	outputFloorMinScore := 0.0
	outputFloorApplied := false
	if outputScore > 0.5 {
		outputFloorMinScore = outputScore * 0.5
		if blended < outputFloorMinScore {
			blended = outputFloorMinScore
			outputFloorApplied = true
		}
	}

	finalScore := clamp(blended, 0.0, 1.0)

	// Tier classification with reasoning override
	strongCount := lastSignals.strongReasoningCount
	tier := a.classifyTier(finalScore)
	if strongCount >= 2 {
		tier = "REASONING"
	} else if strongCount >= 1 && (userCodeScore > 0.5 || userTechnicalScore > 0.5) {
		tier = "REASONING"
	}

	return &ComplexityResult{
		Score:              finalScore,
		Tier:               tier,
		CodePresence:       codeScore,
		ReasoningMarkers:   reasoningScore,
		TechnicalTerms:     technicalScore,
		SimpleIndicators:   simpleScore,
		TokenCount:         tokenScore,
		ConversationCtx:    convScore,
		SystemPromptSignal: systemLexicalContribution,
		OutputComplexity:   outputScore,
		Contributions: ComplexityContributions{
			Code:          codeContribution,
			Reasoning:     reasoningContribution,
			Technical:     technicalContribution,
			SimplePenalty: simplePenalty,
			TokenCount:    tokenContribution,
		},
		CodeMatchCount:      lastSignals.codeCount,
		ReasoningMatchCount: lastSignals.reasoningCount,
		TechnicalMatchCount: lastSignals.technicalCount,
		SimpleMatchCount:    lastSignals.simpleCount,
		OutputMatchCount:    outputCount,
		LastMessageScore:    lastMsgScore,
		ConversationBlend:   blended,
		ReferentialFollowup: referentialFollowup,
		SimpleWeightApplied: effectiveSimpleWeight,
		OutputFloorMinScore: outputFloorMinScore,
		OutputFloorApplied:  outputFloorApplied,
		WordCount:           wordCount,
	}
}

type compiledKeywordMask uint16

const (
	maskCode compiledKeywordMask = 1 << iota
	maskReasoning
	maskStrongReasoning
	maskTechnical
	maskSimple
	maskEnum
	maskComprehensive
	maskElaboration
	maskLimiter
	maskReferentialPhrase
	maskReferentialReference
	maskReferentialAction
	maskTaskShift
)

const (
	lastTextBaseScanMask = maskCode | maskReasoning | maskStrongReasoning | maskTechnical | maskSimple | maskEnum | maskComprehensive | maskElaboration | maskLimiter
	lastTextFullScanMask = lastTextBaseScanMask | maskReferentialPhrase | maskReferentialReference | maskReferentialAction | maskTaskShift
	systemTextScanMask   = maskCode | maskTechnical | maskSimple
	contextTextScanMask  = maskCode | maskReasoning | maskTechnical
)

type keywordMatchMode uint8

const (
	matchModeWholeWord keywordMatchMode = iota
	matchModeBoundarySubstring
	matchModePlainSubstring
)

type compiledKeyword struct {
	text      string
	mask      compiledKeywordMask
	matchMode keywordMatchMode
}

type compiledKeywordMatcher struct {
	wholeWordKeywords         []compiledKeyword
	boundarySubstringKeywords []compiledKeyword
	plainSubstringKeywords    []compiledKeyword
}

type textSignalCounts struct {
	wordCount               int
	codeCount               int
	reasoningCount          int
	strongReasoningCount    int
	technicalCount          int
	simpleCount             int
	enumCount               int
	comprehensiveCount      int
	elaborationCount        int
	limitingQualifierCount  int
	referentialPhraseCount  int
	referentialReferenceCnt int
	referentialActionCount  int
	taskShiftCount          int
}

func newCompiledKeywordMatcher(keywords KeywordConfig) *compiledKeywordMatcher {
	entries := make(map[string]compiledKeyword)
	addKeywords := func(keywords []string, mask compiledKeywordMask) {
		for _, kw := range keywords {
			text := strings.ToLower(kw)
			entry, ok := entries[text]
			if !ok {
				entry = compiledKeyword{
					text:      text,
					mask:      mask,
					matchMode: keywordMatchModeFor(text),
				}
			} else {
				entry.mask |= mask
			}
			entries[text] = entry
		}
	}

	addKeywords(keywords.CodeKeywords, maskCode)
	addKeywords(keywords.StrongReasoningKeywords, maskReasoning|maskStrongReasoning)
	addKeywords(keywords.WeakReasoningKeywords, maskReasoning)
	addKeywords(keywords.TechnicalKeywords, maskTechnical)
	addKeywords(keywords.SimpleKeywords, maskSimple)
	addKeywords(keywords.EnumTriggers, maskEnum)
	addKeywords(keywords.ComprehensivenessMarkers, maskComprehensive)
	addKeywords(keywords.ElaborationMarkers, maskElaboration)
	addKeywords(keywords.LimitingQualifiers, maskLimiter)
	addKeywords(keywords.ReferentialPhrases, maskReferentialPhrase)
	addKeywords(keywords.ReferentialReferenceWords, maskReferentialReference)
	addKeywords(keywords.ReferentialActionWords, maskReferentialAction)
	addKeywords(keywords.TaskShiftPhrases, maskTaskShift)

	matcher := &compiledKeywordMatcher{}
	for _, entry := range entries {
		switch entry.matchMode {
		case matchModeWholeWord:
			matcher.wholeWordKeywords = append(matcher.wholeWordKeywords, entry)
		case matchModeBoundarySubstring:
			matcher.boundarySubstringKeywords = append(matcher.boundarySubstringKeywords, entry)
		case matchModePlainSubstring:
			matcher.plainSubstringKeywords = append(matcher.plainSubstringKeywords, entry)
		}
	}
	return matcher
}

func keywordMatchModeFor(keyword string) keywordMatchMode {
	if strings.Contains(keyword, " ") {
		return matchModePlainSubstring
	}
	for _, r := range keyword {
		if !isWordChar(r) {
			return matchModeBoundarySubstring
		}
	}
	return matchModeWholeWord
}

func (m *compiledKeywordMatcher) analyzeText(text string, scanMask compiledKeywordMask) textSignalCounts {
	if text == "" {
		return textSignalCounts{}
	}

	lowerText := strings.ToLower(text)
	signals := textSignalCounts{
		wordCount: countWordsNoAlloc(text),
	}

	if len(lowerText) >= wordPresenceSetMinBytes {
		wordPresence := buildWordPresenceSet(lowerText)
		for _, keyword := range m.wholeWordKeywords {
			if keyword.mask&scanMask == 0 {
				continue
			}
			if _, ok := wordPresence[keyword.text]; ok {
				signals.addMask(keyword.mask)
			}
		}
	} else {
		for _, keyword := range m.wholeWordKeywords {
			if keyword.mask&scanMask == 0 {
				continue
			}
			if containsWord(lowerText, keyword.text) {
				signals.addMask(keyword.mask)
			}
		}
	}
	for _, keyword := range m.boundarySubstringKeywords {
		if keyword.mask&scanMask == 0 {
			continue
		}
		if containsWord(lowerText, keyword.text) {
			signals.addMask(keyword.mask)
		}
	}
	for _, keyword := range m.plainSubstringKeywords {
		if keyword.mask&scanMask == 0 {
			continue
		}
		if strings.Contains(lowerText, keyword.text) {
			signals.addMask(keyword.mask)
		}
	}

	return signals
}

func (s *textSignalCounts) addMask(mask compiledKeywordMask) {
	if mask&maskCode != 0 {
		s.codeCount++
	}
	if mask&maskReasoning != 0 {
		s.reasoningCount++
	}
	if mask&maskStrongReasoning != 0 {
		s.strongReasoningCount++
	}
	if mask&maskTechnical != 0 {
		s.technicalCount++
	}
	if mask&maskSimple != 0 {
		s.simpleCount++
	}
	if mask&maskEnum != 0 {
		s.enumCount++
	}
	if mask&maskComprehensive != 0 {
		s.comprehensiveCount++
	}
	if mask&maskElaboration != 0 {
		s.elaborationCount++
	}
	if mask&maskLimiter != 0 {
		s.limitingQualifierCount++
	}
	if mask&maskReferentialPhrase != 0 {
		s.referentialPhraseCount++
	}
	if mask&maskReferentialReference != 0 {
		s.referentialReferenceCnt++
	}
	if mask&maskReferentialAction != 0 {
		s.referentialActionCount++
	}
	if mask&maskTaskShift != 0 {
		s.taskShiftCount++
	}
}

func buildWordPresenceSet(text string) map[string]struct{} {
	words := make(map[string]struct{}, 64)
	start := -1
	for i, r := range text {
		if isWordChar(r) {
			if start == -1 {
				start = i
			}
			continue
		}
		if start != -1 {
			words[text[start:i]] = struct{}{}
			start = -1
		}
	}
	if start != -1 {
		words[text[start:]] = struct{}{}
	}
	return words
}

func countWordsNoAlloc(text string) int {
	count := 0
	inWord := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			inWord = false
			continue
		}
		if !inWord {
			count++
			inWord = true
		}
	}
	return count
}

func scoreCount(count, capAt int) float64 {
	if capAt <= 0 {
		return 0.0
	}
	return math.Min(1.0, float64(count)/float64(capAt))
}

func scoreOutputComplexity(signals textSignalCounts) (float64, int) {
	totalCount := signals.enumCount + signals.comprehensiveCount + signals.elaborationCount
	if totalCount == 0 {
		return 0.0, 0
	}

	enumScore := math.Min(1.0, float64(signals.enumCount))
	compScore := math.Min(1.0, float64(signals.comprehensiveCount))
	elabScore := math.Min(1.0, float64(signals.elaborationCount))

	rawScore := (enumScore * 0.4) + (compScore * 0.3) + (elabScore * 0.3)
	if signals.limitingQualifierCount > 0 {
		rawScore *= 0.3
	}

	return math.Min(1.0, rawScore), totalCount
}

func (a *ComplexityAnalyzer) scoreConversationContext(priorUserTexts []string) float64 {
	if len(priorUserTexts) == 0 {
		return 0.0
	}

	texts := priorUserTexts
	if len(texts) > 10 {
		texts = texts[len(texts)-10:]
	}

	var weightedTotal float64
	var totalWeight float64
	lastIdx := len(texts) - 1
	for idx, text := range texts {
		signals := a.matcher.analyzeText(text, contextTextScanMask)
		code := scoreCount(signals.codeCount, 3)
		tech := scoreCount(signals.technicalCount, 3)
		reasoning := scoreCount(signals.reasoningCount, 2)
		msgScore := (code*codeWeight + tech*technicalWeight + reasoning*reasoningWeight) /
			(codeWeight + technicalWeight + reasoningWeight)
		weight := 1.0
		if lastIdx > 0 {
			weight = 1.0 + (2.0 * float64(idx) / float64(lastIdx))
		}
		weightedTotal += msgScore * weight
		totalWeight += weight
	}

	if totalWeight == 0 {
		return 0.0
	}

	return math.Min(1.0, weightedTotal/totalWeight)
}

func isReferentialFollowup(signals textSignalCounts, lastMsgScore, convScore float64, wordCount int) bool {
	if wordCount == 0 || wordCount > referentialMaxWordCount {
		return false
	}
	if lastMsgScore >= referentialMaxStandaloneScore || convScore < referentialMinContextScore {
		return false
	}
	if signals.taskShiftCount > 0 {
		return false
	}
	if signals.referentialPhraseCount > 0 {
		return true
	}

	hasReference := signals.referentialReferenceCnt > 0
	hasAction := signals.referentialActionCount > 0
	return hasReference && hasAction
}

func (a *ComplexityAnalyzer) classifyTier(score float64) string {
	switch {
	case score < a.tierBoundaries.SimpleMedium:
		return "SIMPLE"
	case score < a.tierBoundaries.MediumComplex:
		return "MEDIUM"
	case score < a.tierBoundaries.ComplexReasoning:
		return "COMPLEX"
	default:
		return "REASONING"
	}
}

// --- Dimension weights ---

const (
	codeWeight                         = 0.30
	reasoningWeight                    = 0.25
	technicalWeight                    = 0.25
	simpleWeight                       = 0.05 // dampener, subtracted
	tokenCountWeight                   = 0.10
	systemPromptAssistFactor           = 0.25
	defaultLastMessageBlendWeight      = 0.60
	defaultConversationBlendWeight     = 0.40
	referentialLastMessageBlendWeight  = 0.35
	referentialConversationBlendWeight = 0.65
	referentialMaxStandaloneScore      = 0.15
	referentialMaxWordCount            = 6
	referentialMinContextScore         = 0.20
	wordPresenceSetMinBytes            = 8 * 1024
	// Output complexity is applied as a score floor, not a weighted dimension
)

// --- Keyword lists ---
// CodePresence: implementation/code syntax/workflow signals
var codeKeywords = []string{
	"function", "class", "api", "database", "algorithm", "code", "implement",
	"debug", "error", "syntax", "compile", "runtime", "library", "framework",
	"variable", "loop", "array", "object", "method", "interface",
	"regex", "deploy", "docker", "sql", "query", "schema", "endpoint",
	"refactor", "bug", "parse", "async", "webhook", "migration",
	"ci/cd", "pipeline", "rest", "graphql", "test", "unit test",
	"python", "javascript", "typescript", "golang", "java", "ruby",
	"github actions", "monorepo", "aws cli", "config rule", "config rules",
	"retry", "fallback", "middleware", "patch", "diff", "pr", "pull request",
	"commit", "commit message", "behavior change",
	"cel", "auto-routing", "rwmutex", "goroutine",
}

// Reasoning markers — split into strong and weak for override logic
var strongReasoningKeywords = []string{
	"step by step", "think through", "tradeoffs", "pros and cons",
	"justify", "critique", "implications", "explain why",
	"root cause analysis", "reconstruct the sequence",
	"reconstruct the most likely sequence", "what should have happened instead",
	"explain your reasoning", "weigh the tradeoffs", "recommend a design",
}

var weakReasoningKeywords = []string{
	"reason", "analyze", "evaluate", "compare", "assess", "consider",
	"why does", "what if", "how would", "what are the", "which approach",
	"think about", "design", "most likely", "reconstruct", "verify",
	"assumption", "hypothesis", "compare and contrast", "weigh the options",
	"recommend one", "given these constraints", "under these constraints",
}

// TechnicalTerms: architecture/distributed/security/infrastructure signals
var technicalKeywords = []string{
	"architecture", "distributed", "encryption", "authentication", "scalability",
	"microservices", "kubernetes", "infrastructure", "protocol", "latency",
	"throughput", "concurrency", "optimization", "load balancer", "caching",
	"sharding", "replication", "consensus", "mutex", "deadlock",
	"race condition", "api gateway", "terraform", "observability",
	"access token", "refresh token", "rbac", "sso", "oidc", "saml",
	"tenant", "multi-tenant", "audit log", "failover", "idempotency",
	"zero downtime", "incident", "outage", "postmortem", "root cause",
	"telemetry", "metrics", "configmap", "connection pool", "payment processing",
	"saas", "feature flag", "operational risk", "vendor lock-in",
	"s3 bucket", "misconfiguration", "remediation", "oltp", "olap",
	"ledger", "metering", "aggregation", "proration", "credits", "dunning",
	"invoice", "invoice generation", "double-entry", "reconciliation",
	"chart of accounts", "hipaa", "quarantine workflow", "retention policy",
	"audit trail", "pre-signed url", "entitlements", "seat limits",
	"usage quotas", "deprovisioning", "permission drift", "role mapping",
	"fraud detection", "manual review", "feedback loop",
	"model serving", "a/b testing", "identity resolution",
	"deterministic replay", "tamper evidence", "hash chain",
	"approval workflow", "vpc", "soc 2", "data residency",
	"disaster recovery", "data race", "struct copy", "hybrid search",
}

// SimpleIndicators: signals for trivial/greeting-type requests
var simpleKeywords = []string{
	"what is", "define", "hello", "hi", "thanks", "how do i spell",
	"translate", "what does", "who is", "when was", "tell me about",
	"good morning", "good night", "how are you", "simple", "brief",
	"short", "quick", "beginner", "basic", "concise",
}

// --- Output complexity keywords ---

var enumTriggers = []string{
	"list every", "list all", "enumerate all", "all possible",
	"every single", "show all", "name all", "give me all",
}

var comprehensivenessMarkers = []string{
	"comprehensive", "exhaustive", "complete list", "full list",
	"in detail", "detailed breakdown", "thorough", "in-depth",
}

var elaborationMarkers = []string{
	"and what it does", "explain each", "describe each", "for each",
	"with examples", "with descriptions", "along with",
}

var limitingQualifiers = []string{
	"briefly", "top 3", "top 5", "top 10", "in one sentence",
	"quickly", "summarize", "just the", "only the", "keep it short",
	"tl;dr", "tldr",
}

var referentialPhrases = []string{
	"do it", "try again", "continue", "go ahead", "proceed",
	"that one", "this one", "same thing", "again", "retry",
	"yes do that", "go with that", "use option 1", "use option 2", "use option 3",
	"now write it",
}

var referentialReferenceWords = []string{
	"it", "this", "that", "same", "previous", "earlier",
}

var referentialActionWords = []string{
	"do", "retry", "continue", "proceed", "use", "fix",
	"rewrite", "shorten", "clean", "adjust", "make", "give", "answer",
}

var taskShiftPhrases = []string{
	"translate", "summarize", "in one sentence", "one sentence",
	"in spanish", "in french", "in german", "more politely", "more polite",
}

// --- Scoring functions ---

// containsWord checks if a word appears in text delimited by non-alphanumeric boundaries.
func containsWord(text, word string) bool {
	idx := 0
	for {
		pos := strings.Index(text[idx:], word)
		if pos == -1 {
			return false
		}
		start := idx + pos
		end := start + len(word)

		startOk := start == 0 || !isWordChar(lastRune(text[:start]))
		endOk := end == len(text) || !isWordChar(firstRune(text[end:]))

		if startOk && endOk {
			return true
		}
		idx = start + 1
		if idx >= len(text) {
			return false
		}
	}
}

func firstRune(text string) rune {
	r, _ := utf8.DecodeRuneInString(text)
	return r
}

func lastRune(text string) rune {
	r, _ := utf8.DecodeLastRuneInString(text)
	return r
}

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// scoreTokenCount scores based on word count of the text.
func scoreTokenCount(words int) float64 {
	switch {
	case words < 15:
		return float64(words) / 15.0 * 0.3
	case words <= 400:
		return 0.3 + float64(words-15)/385.0*0.4
	default:
		extra := math.Min(0.3, float64(words-400)/600.0*0.3)
		return 0.7 + extra
	}
}

func clamp(val, min, max float64) float64 {
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}
