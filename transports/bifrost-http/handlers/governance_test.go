package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/maximhq/bifrost/core/complexity"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/plugins/governance"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

// mockGovernanceManagerForVK embeds the interface so unimplemented methods panic.
// Only GetGovernanceData is needed for the getVirtualKeys handler path.
type mockGovernanceManagerForVK struct {
	GovernanceManager
}

func (m *mockGovernanceManagerForVK) GetGovernanceData() *governance.GovernanceData {
	return nil
}

func (m *mockGovernanceManagerForVK) ReloadComplexityAnalyzerConfig(ctx context.Context, config *configstore.ComplexityAnalyzerConfig) error {
	return nil
}

// mockConfigStoreForVK embeds the interface so unimplemented methods panic.
// Only GetVirtualKeysPaginated is called in the non-from_memory path.
type mockConfigStoreForVK struct {
	configstore.ConfigStore
}

func (m *mockConfigStoreForVK) GetVirtualKeysPaginated(_ context.Context, _ configstore.VirtualKeyQueryParams) ([]configstoreTables.TableVirtualKey, int64, error) {
	return nil, 0, nil
}

func (m *mockConfigStoreForVK) GetVirtualKeys(_ context.Context) ([]configstoreTables.TableVirtualKey, error) {
	return nil, nil
}

type mockGovernanceManagerForBoundaries struct {
	GovernanceManager
	reloadAnalyzerCalls int
	reloadErr           error
}

func (m *mockGovernanceManagerForBoundaries) ReloadComplexityAnalyzerConfig(ctx context.Context, config *configstore.ComplexityAnalyzerConfig) error {
	m.reloadAnalyzerCalls++
	return m.reloadErr
}

func (m *mockGovernanceManagerForBoundaries) GetGovernanceData() *governance.GovernanceData {
	return nil
}

type mockConfigStoreForBoundaries struct {
	configstore.ConfigStore
	analyzerConfig *configstore.ComplexityAnalyzerConfig
}

func (m *mockConfigStoreForBoundaries) GetConfig(_ context.Context, key string) (*configstoreTables.TableGovernanceConfig, error) {
	if key != configstoreTables.ConfigComplexityAnalyzerConfigKey {
		return nil, configstore.ErrNotFound
	}
	if m.analyzerConfig == nil {
		return nil, configstore.ErrNotFound
	}
	raw, err := json.Marshal(m.analyzerConfig)
	if err != nil {
		return nil, err
	}
	return &configstoreTables.TableGovernanceConfig{Key: key, Value: string(raw)}, nil
}

func (m *mockConfigStoreForBoundaries) UpdateConfig(_ context.Context, cfg *configstoreTables.TableGovernanceConfig, _ ...*gorm.DB) error {
	if cfg.Key != configstoreTables.ConfigComplexityAnalyzerConfigKey {
		return nil
	}
	if cfg.Value == "" {
		m.analyzerConfig = nil
		return nil
	}
	var parsed configstore.ComplexityAnalyzerConfig
	if err := json.Unmarshal([]byte(cfg.Value), &parsed); err != nil {
		return err
	}
	copy := parsed
	m.analyzerConfig = &copy
	return nil
}

// TestGetVirtualKeys_PaginatedEndpoint_ResponseShape verifies the JSON response
// from the paginated virtual keys endpoint contains all expected fields.
func TestGetVirtualKeys_PaginatedEndpoint_ResponseShape(t *testing.T) {
	SetLogger(&mockLogger{})

	h := &GovernanceHandler{
		configStore:       &mockConfigStoreForVK{},
		governanceManager: &mockGovernanceManagerForVK{},
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/governance/virtual-keys?limit=10&offset=0")

	h.getVirtualKeys(ctx)

	if ctx.Response.StatusCode() != 200 {
		t.Fatalf("expected status 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to parse JSON response: %v", err)
	}

	// Assert expected fields exist with correct types
	requiredFields := []struct {
		key      string
		wantType string
	}{
		{"virtual_keys", "array"},
		{"total_count", "number"},
		{"count", "number"},
		{"limit", "number"},
		{"offset", "number"},
	}

	for _, f := range requiredFields {
		val, ok := resp[f.key]
		if !ok {
			t.Errorf("response missing required field %q", f.key)
			continue
		}
		switch f.wantType {
		case "array":
			if _, ok := val.([]interface{}); !ok {
				// nil decodes as nil, which is fine — JSON null for empty array
				if val != nil {
					t.Errorf("field %q: expected array, got %T", f.key, val)
				}
			}
		case "number":
			if _, ok := val.(float64); !ok {
				t.Errorf("field %q: expected number, got %T", f.key, val)
			}
		}
	}

	// Verify no unexpected extra top-level fields
	allowedKeys := map[string]bool{
		"virtual_keys": true,
		"total_count":  true,
		"count":        true,
		"limit":        true,
		"offset":       true,
	}
	for key := range resp {
		if !allowedKeys[key] {
			t.Errorf("unexpected field %q in response", key)
		}
	}
}

// TestGetVirtualKeys_PaginatedEndpoint_QueryParams verifies query parameters are
// parsed and reflected in the response.
func TestGetVirtualKeys_PaginatedEndpoint_QueryParams(t *testing.T) {
	SetLogger(&mockLogger{})

	h := &GovernanceHandler{
		configStore:       &mockConfigStoreForVK{},
		governanceManager: &mockGovernanceManagerForVK{},
	}

	tests := []struct {
		name       string
		uri        string
		wantLimit  float64
		wantOffset float64
	}{
		{
			name:       "explicit limit and offset",
			uri:        "/api/governance/virtual-keys?limit=10&offset=5",
			wantLimit:  10,
			wantOffset: 5,
		},
		{
			name:       "no params uses defaults",
			uri:        "/api/governance/virtual-keys",
			wantLimit:  0,
			wantOffset: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod("GET")
			ctx.Request.SetRequestURI(tt.uri)

			h.getVirtualKeys(ctx)

			if ctx.Response.StatusCode() != 200 {
				t.Fatalf("expected status 200, got %d", ctx.Response.StatusCode())
			}

			var resp map[string]interface{}
			if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
				t.Fatalf("failed to parse JSON: %v", err)
			}

			if got := resp["limit"].(float64); got != tt.wantLimit {
				t.Errorf("limit: got %v, want %v", got, tt.wantLimit)
			}
			if got := resp["offset"].(float64); got != tt.wantOffset {
				t.Errorf("offset: got %v, want %v", got, tt.wantOffset)
			}
		})
	}
}

func TestGetComplexityAnalyzerConfig_DefaultsWhenMissing(t *testing.T) {
	SetLogger(&mockLogger{})

	h := &GovernanceHandler{
		configStore:       &mockConfigStoreForBoundaries{},
		governanceManager: &mockGovernanceManagerForBoundaries{},
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/governance/complexity-analyzer-config")

	h.getComplexityAnalyzerConfig(ctx)

	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())

	var resp configstore.ComplexityAnalyzerConfig
	require.NoError(t, json.Unmarshal(ctx.Response.Body(), &resp))
	require.Equal(t, complexity.DefaultTierBoundaries(), resp.TierBoundaries)
	require.NotEmpty(t, resp.Keywords.CodeKeywords)
}

func TestUpdateComplexityAnalyzerConfig_PersistsAndReloads(t *testing.T) {
	SetLogger(&mockLogger{})

	store := &mockConfigStoreForBoundaries{}
	manager := &mockGovernanceManagerForBoundaries{}
	h := &GovernanceHandler{
		configStore:       store,
		governanceManager: manager,
	}

	payload := `{
		"tier_boundaries":{"simple_medium":0.22,"medium_complex":0.44,"complex_reasoning":0.77},
		"keywords":{
			"code_keywords":["function","endpoint","router"],
			"reasoning_keywords":["step by step"],
			"technical_keywords":["kubernetes","latency"],
			"simple_keywords":["what is"]
		}
	}`

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("PUT")
	ctx.Request.SetRequestURI("/api/governance/complexity-analyzer-config")
	ctx.Request.SetBodyString(payload)

	h.updateComplexityAnalyzerConfig(ctx)

	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), string(ctx.Response.Body()))
	require.NotNil(t, store.analyzerConfig)
	require.Equal(t, 0.22, store.analyzerConfig.TierBoundaries.SimpleMedium)
	require.Equal(t, []string{"function", "endpoint", "router"}, store.analyzerConfig.Keywords.CodeKeywords)
	require.Equal(t, []string{"step by step"}, store.analyzerConfig.Keywords.ReasoningKeywords)
	require.Equal(t, 1, manager.reloadAnalyzerCalls)
}

func TestUpdateComplexityAnalyzerConfig_RejectsUnknownKeywordFields(t *testing.T) {
	SetLogger(&mockLogger{})

	store := &mockConfigStoreForBoundaries{}
	manager := &mockGovernanceManagerForBoundaries{}
	h := &GovernanceHandler{
		configStore:       store,
		governanceManager: manager,
	}

	// strong_reasoning_keywords is a legacy internal field — API now only accepts
	// the 4 editable dimensions. Strict decoder must reject unknown fields.
	payload := `{
		"tier_boundaries":{"simple_medium":0.22,"medium_complex":0.44,"complex_reasoning":0.77},
		"keywords":{
			"code_keywords":["function"],
			"reasoning_keywords":["step by step"],
			"technical_keywords":["kubernetes"],
			"simple_keywords":["what is"],
			"strong_reasoning_keywords":["rogue"]
		}
	}`

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("PUT")
	ctx.Request.SetRequestURI("/api/governance/complexity-analyzer-config")
	ctx.Request.SetBodyString(payload)

	h.updateComplexityAnalyzerConfig(ctx)

	require.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode(), string(ctx.Response.Body()))
	require.Nil(t, store.analyzerConfig)
	require.Equal(t, 0, manager.reloadAnalyzerCalls)
}

func TestUpdateComplexityAnalyzerConfig_RejectsEmptyKeywordList(t *testing.T) {
	SetLogger(&mockLogger{})

	store := &mockConfigStoreForBoundaries{}
	manager := &mockGovernanceManagerForBoundaries{}
	h := &GovernanceHandler{
		configStore:       store,
		governanceManager: manager,
	}

	// Missing simple_keywords — partial payload must be rejected so it can't
	// silently wipe the live keyword list (P1 fix).
	payload := `{
		"tier_boundaries":{"simple_medium":0.22,"medium_complex":0.44,"complex_reasoning":0.77},
		"keywords":{
			"code_keywords":["function"],
			"reasoning_keywords":["step by step"],
			"technical_keywords":["kubernetes"]
		}
	}`

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("PUT")
	ctx.Request.SetRequestURI("/api/governance/complexity-analyzer-config")
	ctx.Request.SetBodyString(payload)

	h.updateComplexityAnalyzerConfig(ctx)

	require.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode(), string(ctx.Response.Body()))
	require.Contains(t, string(ctx.Response.Body()), "simple_keywords")
	require.Nil(t, store.analyzerConfig)
	require.Equal(t, 0, manager.reloadAnalyzerCalls)
}

func TestUpdateComplexityAnalyzerConfig_RollsBackStoreWhenReloadFails(t *testing.T) {
	SetLogger(&mockLogger{})

	previous := configstore.ComplexityAnalyzerConfig{
		TierBoundaries: complexity.TierBoundaries{
			SimpleMedium:     0.15,
			MediumComplex:    0.35,
			ComplexReasoning: 0.65,
		},
		Keywords: complexity.EditableKeywordConfig{
			CodeKeywords:      []string{"function", "api"},
			ReasoningKeywords: []string{"analyze"},
			TechnicalKeywords: []string{"latency"},
			SimpleKeywords:    []string{"what is"},
		},
	}
	store := &mockConfigStoreForBoundaries{analyzerConfig: &previous}
	manager := &mockGovernanceManagerForBoundaries{reloadErr: errors.New("boom")}
	h := &GovernanceHandler{
		configStore:       store,
		governanceManager: manager,
	}

	payload := `{
		"tier_boundaries":{"simple_medium":0.22,"medium_complex":0.44,"complex_reasoning":0.77},
		"keywords":{
			"code_keywords":["function","endpoint","router"],
			"reasoning_keywords":["step by step"],
			"technical_keywords":["kubernetes","latency"],
			"simple_keywords":["what is"]
		}
	}`

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("PUT")
	ctx.Request.SetRequestURI("/api/governance/complexity-analyzer-config")
	ctx.Request.SetBodyString(payload)

	h.updateComplexityAnalyzerConfig(ctx)

	require.Equal(t, fasthttp.StatusInternalServerError, ctx.Response.StatusCode(), string(ctx.Response.Body()))
	require.Contains(t, string(ctx.Response.Body()), "failed to reload complexity analyzer config")
	require.Equal(t, 1, manager.reloadAnalyzerCalls)
	require.NotNil(t, store.analyzerConfig)
	require.Equal(t, previous.TierBoundaries, store.analyzerConfig.TierBoundaries)
	require.Equal(t, previous.Keywords.CodeKeywords, store.analyzerConfig.Keywords.CodeKeywords)
	require.Equal(t, previous.Keywords.ReasoningKeywords, store.analyzerConfig.Keywords.ReasoningKeywords)
	require.Equal(t, previous.Keywords.TechnicalKeywords, store.analyzerConfig.Keywords.TechnicalKeywords)
	require.Equal(t, previous.Keywords.SimpleKeywords, store.analyzerConfig.Keywords.SimpleKeywords)
}

// Ensure mockLogger satisfies schemas.Logger (already defined in middlewares_test.go
// but we reference it here — same package, so no redeclaration needed).
var _ schemas.Logger = (*mockLogger)(nil)

func TestBudgetRemovalRequestDetection(t *testing.T) {
	tests := []struct {
		name string
		req  *UpdateBudgetRequest
		want bool
	}{
		{
			name: "nil request is not removal",
			req:  nil,
			want: false,
		},
		{
			name: "empty object is removal",
			req:  &UpdateBudgetRequest{},
			want: true,
		},
		{
			name: "max limit present is not removal",
			req:  &UpdateBudgetRequest{MaxLimit: bifrostFloat(10)},
			want: false,
		},
		{
			name: "reset duration only is not removal",
			req:  &UpdateBudgetRequest{ResetDuration: bifrostString("1h")},
			want: false,
		},
		{
			name: "calendar aligned only is treated as removal",
			req:  &UpdateBudgetRequest{CalendarAligned: bifrostBool(true)},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBudgetRemovalRequest(tt.req); got != tt.want {
				t.Fatalf("isBudgetRemovalRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRateLimitRemovalRequestDetection(t *testing.T) {
	tests := []struct {
		name string
		req  *UpdateRateLimitRequest
		want bool
	}{
		{
			name: "nil request is not removal",
			req:  nil,
			want: false,
		},
		{
			name: "empty object is removal",
			req:  &UpdateRateLimitRequest{},
			want: true,
		},
		{
			name: "token limit present is not removal",
			req:  &UpdateRateLimitRequest{TokenMaxLimit: bifrostInt64(100)},
			want: false,
		},
		{
			name: "request limit present is not removal",
			req:  &UpdateRateLimitRequest{RequestMaxLimit: bifrostInt64(10)},
			want: false,
		},
		{
			name: "durations only is not removal",
			req: &UpdateRateLimitRequest{
				TokenResetDuration:   bifrostString("1h"),
				RequestResetDuration: bifrostString("1h"),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRateLimitRemovalRequest(tt.req); got != tt.want {
				t.Fatalf("isRateLimitRemovalRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCollectProviderConfigDeleteIDs(t *testing.T) {
	budgetID := "budget-1"
	rateLimitID := "rate-limit-1"

	tests := []struct {
		name             string
		config           configstoreTables.TableVirtualKeyProviderConfig
		initialBudgetIDs []string
		initialRateIDs   []string
		wantBudgetIDs    []string
		wantRateIDs      []string
	}{
		{
			name: "collects both IDs",
			config: configstoreTables.TableVirtualKeyProviderConfig{
				Budgets:     []configstoreTables.TableBudget{{ID: budgetID}},
				RateLimitID: &rateLimitID,
			},
			wantBudgetIDs: []string{budgetID},
			wantRateIDs:   []string{rateLimitID},
		},
		{
			name: "appends to existing slices",
			config: configstoreTables.TableVirtualKeyProviderConfig{
				Budgets:     []configstoreTables.TableBudget{{ID: budgetID}},
				RateLimitID: &rateLimitID,
			},
			initialBudgetIDs: []string{"budget-0"},
			initialRateIDs:   []string{"rate-limit-0"},
			wantBudgetIDs:    []string{"budget-0", budgetID},
			wantRateIDs:      []string{"rate-limit-0", rateLimitID},
		},
		{
			name:   "ignores missing IDs",
			config: configstoreTables.TableVirtualKeyProviderConfig{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBudgetIDs, gotRateIDs := collectProviderConfigDeleteIDs(tt.config, tt.initialBudgetIDs, tt.initialRateIDs)

			if len(gotBudgetIDs) != len(tt.wantBudgetIDs) {
				t.Fatalf("budget IDs length = %d, want %d", len(gotBudgetIDs), len(tt.wantBudgetIDs))
			}
			for i := range gotBudgetIDs {
				if gotBudgetIDs[i] != tt.wantBudgetIDs[i] {
					t.Fatalf("budget IDs[%d] = %q, want %q", i, gotBudgetIDs[i], tt.wantBudgetIDs[i])
				}
			}

			if len(gotRateIDs) != len(tt.wantRateIDs) {
				t.Fatalf("rate limit IDs length = %d, want %d", len(gotRateIDs), len(tt.wantRateIDs))
			}
			for i := range gotRateIDs {
				if gotRateIDs[i] != tt.wantRateIDs[i] {
					t.Fatalf("rate limit IDs[%d] = %q, want %q", i, gotRateIDs[i], tt.wantRateIDs[i])
				}
			}
		})
	}
}

func bifrostFloat(v float64) *float64 {
	return &v
}

func bifrostInt64(v int64) *int64 {
	return &v
}

func bifrostString(v string) *string {
	return &v
}

func bifrostBool(v bool) *bool {
	return &v
}
