package configstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/maximhq/bifrost/core/complexity"
	"github.com/maximhq/bifrost/framework/configstore/tables"
)

// ComplexityAnalyzerConfig is the persisted/configurable analyzer runtime config.
type ComplexityAnalyzerConfig = complexity.AnalyzerConfig

// GetComplexityAnalyzerConfig retrieves the full complexity analyzer config from governance_config.
func GetComplexityAnalyzerConfig(ctx context.Context, store ConfigStore) (*ComplexityAnalyzerConfig, error) {
	if store == nil {
		return nil, fmt.Errorf("config store is nil")
	}

	configEntry, err := store.GetConfig(ctx, tables.ConfigComplexityAnalyzerConfigKey)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if configEntry == nil || configEntry.Value == "" {
		return nil, nil
	}

	var cfg ComplexityAnalyzerConfig
	if err := json.Unmarshal([]byte(configEntry.Value), &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal complexity analyzer config: %w", err)
	}
	normalized := cfg.Normalized()
	if err := normalized.Validate(); err != nil {
		return nil, fmt.Errorf("invalid complexity analyzer config: %w", err)
	}
	return &normalized, nil
}

// UpdateComplexityAnalyzerConfig upserts the full analyzer config into the
// governance config store.
func UpdateComplexityAnalyzerConfig(ctx context.Context, store ConfigStore, cfg *ComplexityAnalyzerConfig) error {
	if store == nil {
		return fmt.Errorf("config store is nil")
	}
	if cfg == nil {
		return fmt.Errorf("complexity analyzer config is nil")
	}

	normalized := cfg.Normalized()
	if err := normalized.Validate(); err != nil {
		return fmt.Errorf("invalid complexity analyzer config: %w", err)
	}

	configJSON, err := json.Marshal(normalized)
	if err != nil {
		return fmt.Errorf("failed to marshal complexity analyzer config: %w", err)
	}

	return store.UpdateConfig(ctx, &tables.TableGovernanceConfig{
		Key:   tables.ConfigComplexityAnalyzerConfigKey,
		Value: string(configJSON),
	})
}
