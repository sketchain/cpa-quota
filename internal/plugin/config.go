package plugin

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sketchain/cpa-quota/internal/ccbilling"
)

// DefaultPriceURL serves the new-api ratio table prices come from.
const DefaultPriceURL = "https://toll.icmp.day/api/ratio_config"

// Config is the plugin's block of plugins.configs in CLIProxyAPI's
// config.yaml. There is no UI: everything is configured here.
type Config struct {
	// Instance names this CLIProxyAPI instance; every row and response carries it.
	Instance string `yaml:"instance"`
	// PostgresDSN is the PostgreSQL connection string all instances share.
	PostgresDSN    string `yaml:"postgres_dsn"`
	PostgresSchema string `yaml:"postgres_schema"`
	// APIToken authorizes the read-only quota API; AdminToken the admin
	// operations. An empty token disables what it guards.
	APIToken   string `yaml:"api_token"`
	AdminToken string `yaml:"admin_token"`

	PriceURL            string  `yaml:"price_url"`
	PriceRefreshHours   float64 `yaml:"price_refresh_hours"`
	PriceRetryMinutes   float64 `yaml:"price_retry_minutes"`
	PriceTimeoutSeconds float64 `yaml:"price_timeout_seconds"`

	// Tier is the Claude subscription tier: pro, max5x or max20x.
	Tier         string   `yaml:"tier"`
	ManualK      *float64 `yaml:"manual_k"`
	SafetyFactor float64  `yaml:"safety_factor"`
	// TierWeeklyUSD and TierK override the tier defaults.
	TierWeeklyUSD map[string]float64 `yaml:"tier_weekly_usd"`
	TierK         map[string]float64 `yaml:"tier_k"`
	// AuthIndexes, when set, are the only accounts tracked; they are
	// tracked from the start, before any usage.
	AuthIndexes []string `yaml:"auth_indexes"`
	// AccountAuthTypes are the credential types whose usage counts.
	AccountAuthTypes []string `yaml:"account_auth_types"`

	DBTimeoutSeconds        float64 `yaml:"db_timeout_seconds"`
	DBRetrySeconds          float64 `yaml:"db_retry_seconds"`
	RefreshIntervalSeconds  float64 `yaml:"refresh_interval_seconds"`
	ProcessBudgetSeconds    float64 `yaml:"process_budget_seconds"`
	RebuildBudgetSeconds    float64 `yaml:"rebuild_budget_seconds"`
	TrendTimezone           string  `yaml:"trend_timezone"`
	ForecastWindow5hMinutes float64 `yaml:"forecast_window_5h_minutes"`
	ForecastWindow7dHours   float64 `yaml:"forecast_window_7d_hours"`

	Estimation   EstimationConfig   `yaml:"estimation"`
	ModelWeights ModelWeightsConfig `yaml:"model_weights"`

	Debug bool `yaml:"debug"`
}

// EstimationConfig holds the estimation parameters.
type EstimationConfig struct {
	SegmentRetentionDays int     `yaml:"segment_retention_days"`
	BlockGapMinutes      float64 `yaml:"block_gap_minutes"`
	WindowPercent        float64 `yaml:"window_percent"`
	ReferenceMode        string  `yaml:"reference_mode"`
	SuspiciousThreshold  float64 `yaml:"suspicious_threshold"`
	ExcludeVoteShare     float64 `yaml:"exclude_vote_share"`
	WeightPower          float64 `yaml:"weight_power"`
	RecentDays           float64 `yaml:"recent_days"`
	RecentShare          float64 `yaml:"recent_share"`
	KMinWeeklyPoints     float64 `yaml:"k_min_weekly_points"`
	ColdStartBlocks      int     `yaml:"cold_start_blocks"`
}

// ModelWeightsConfig configures the model quota multipliers.
type ModelWeightsConfig struct {
	// Enabled makes the estimate convert costs with the multipliers.
	Enabled        bool              `yaml:"enabled"`
	Items          []ModelWeightItem `yaml:"items"`
	MinSegments    int               `yaml:"min_segments"`
	MinCostUSD     float64           `yaml:"min_cost_usd"`
	MinShareSpread float64           `yaml:"min_share_spread"`
}

// ModelWeightItem is one model under test.
type ModelWeightItem struct {
	ID               string   `yaml:"id"`
	Name             string   `yaml:"name"`
	Models           []string `yaml:"models"`
	ManualMultiplier *float64 `yaml:"manual_multiplier"`
}

// DefaultConfig is the configuration of every omitted key.
func DefaultConfig() Config {
	params := ccbilling.DefaultParams()
	weights := ccbilling.DefaultWeightSettings()
	return Config{PostgresSchema: "cpa_quota", PriceURL: DefaultPriceURL, PriceRefreshHours: 24, PriceRetryMinutes: 5, PriceTimeoutSeconds: 5,
		SafetyFactor: params.SafetyFactor, AccountAuthTypes: []string{"oauth"},
		DBTimeoutSeconds: 3, DBRetrySeconds: 60, RefreshIntervalSeconds: 60, ProcessBudgetSeconds: 3, RebuildBudgetSeconds: 20,
		TrendTimezone: "UTC", ForecastWindow5hMinutes: 60, ForecastWindow7dHours: 24,
		Estimation: EstimationConfig{SegmentRetentionDays: params.SegmentRetentionDays, BlockGapMinutes: params.BlockGapMinutes,
			WindowPercent: params.WindowPercent, ReferenceMode: params.ReferenceMode, SuspiciousThreshold: params.SuspiciousThreshold,
			ExcludeVoteShare: params.ExcludeVoteShare, WeightPower: params.WeightPower, RecentDays: params.RecentDays,
			RecentShare: params.RecentShare, KMinWeeklyPoints: params.KMinWeeklyPoints, ColdStartBlocks: params.ColdStartBlocks},
		ModelWeights: ModelWeightsConfig{MinSegments: weights.MinSegments, MinCostUSD: weights.MinCostUSD, MinShareSpread: weights.MinShareSpread}}
}

// ParseConfig reads the plugin's YAML block over the defaults and checks it.
func ParseConfig(raw []byte) (Config, error) {
	config := DefaultConfig()
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return Config{}, fmt.Errorf("read cpa-quota configuration: %w", err)
	}
	config.Instance = strings.TrimSpace(config.Instance)
	config.PostgresDSN = strings.TrimSpace(config.PostgresDSN)
	config.Tier = strings.ToLower(strings.TrimSpace(config.Tier))
	switch {
	case config.Instance == "":
		return Config{}, errors.New("cpa-quota: instance is required")
	case len(config.Instance) > 128:
		return Config{}, errors.New("cpa-quota: instance must not exceed 128 bytes")
	case config.PostgresDSN == "":
		return Config{}, errors.New("cpa-quota: postgres_dsn is required")
	case strings.TrimSpace(config.PostgresSchema) == "":
		return Config{}, errors.New("cpa-quota: postgres_schema must not be empty")
	}
	if parsed, err := url.Parse(config.PriceURL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return Config{}, errors.New("cpa-quota: price_url must be an http or https URL")
	}
	for _, check := range []struct {
		name  string
		value float64
	}{{"price_refresh_hours", config.PriceRefreshHours}, {"price_retry_minutes", config.PriceRetryMinutes},
		{"price_timeout_seconds", config.PriceTimeoutSeconds}, {"db_timeout_seconds", config.DBTimeoutSeconds},
		{"db_retry_seconds", config.DBRetrySeconds}, {"refresh_interval_seconds", config.RefreshIntervalSeconds},
		{"process_budget_seconds", config.ProcessBudgetSeconds}, {"rebuild_budget_seconds", config.RebuildBudgetSeconds},
		{"forecast_window_5h_minutes", config.ForecastWindow5hMinutes}, {"forecast_window_7d_hours", config.ForecastWindow7dHours}} {
		if !(check.value > 0) || check.value > 1e6 {
			return Config{}, fmt.Errorf("cpa-quota: %s must be a positive number", check.name)
		}
	}
	if config.ForecastWindow5hMinutes < 5 || config.ForecastWindow5hMinutes > 300 {
		return Config{}, errors.New("cpa-quota: forecast_window_5h_minutes must be from 5 to 300")
	}
	if config.ForecastWindow7dHours < 1 || config.ForecastWindow7dHours > 168 {
		return Config{}, errors.New("cpa-quota: forecast_window_7d_hours must be from 1 to 168")
	}
	if _, err := time.LoadLocation(config.TrendTimezone); err != nil {
		return Config{}, fmt.Errorf("cpa-quota: trend_timezone: %w", err)
	}
	var types []string
	for _, kind := range config.AccountAuthTypes {
		if kind = strings.ToLower(strings.TrimSpace(kind)); kind != "" && !slices.Contains(types, kind) {
			types = append(types, kind)
		}
	}
	if len(types) == 0 {
		return Config{}, errors.New("cpa-quota: account_auth_types must name at least one credential type")
	}
	config.AccountAuthTypes = types
	var indexes []string
	for _, index := range config.AuthIndexes {
		if index = strings.TrimSpace(index); index != "" && !slices.Contains(indexes, index) {
			indexes = append(indexes, index)
		}
	}
	config.AuthIndexes = indexes
	if _, err := config.Settings(); err != nil {
		return Config{}, fmt.Errorf("cpa-quota: %w", err)
	}
	return config, nil
}

// Account is the settings of every tracked account.
func (c Config) Account() ccbilling.AccountSettings {
	return ccbilling.AccountSettings{Tier: c.Tier, ManualK: c.ManualK}
}

// Settings turns the configuration into estimation settings, with the
// tracked accounts the configuration lists.
func (c Config) Settings() (ccbilling.Settings, error) {
	settings := ccbilling.DefaultSettings()
	e := c.Estimation
	settings.Params.SegmentRetentionDays, settings.Params.BlockGapMinutes, settings.Params.WindowPercent = e.SegmentRetentionDays, e.BlockGapMinutes, e.WindowPercent
	settings.Params.ReferenceMode, settings.Params.SuspiciousThreshold, settings.Params.ExcludeVoteShare = e.ReferenceMode, e.SuspiciousThreshold, e.ExcludeVoteShare
	settings.Params.WeightPower, settings.Params.RecentDays, settings.Params.RecentShare = e.WeightPower, e.RecentDays, e.RecentShare
	settings.Params.KMinWeeklyPoints, settings.Params.ColdStartBlocks, settings.Params.SafetyFactor = e.KMinWeeklyPoints, e.ColdStartBlocks, c.SafetyFactor
	tiers := slices.Clone(settings.Tiers)
	known := map[string]bool{}
	for i, tier := range tiers {
		known[tier.ID] = true
		if value, ok := c.TierWeeklyUSD[tier.ID]; ok {
			tiers[i].WeeklyUSD = value
		}
		if value, ok := c.TierK[tier.ID]; ok {
			k := value
			tiers[i].K = &k
		}
	}
	for _, overrides := range []map[string]float64{c.TierWeeklyUSD, c.TierK} {
		for id := range overrides {
			if !known[id] {
				return ccbilling.Settings{}, fmt.Errorf("%q is not a Claude subscription tier", id)
			}
		}
	}
	if c.Tier != "" && !known[c.Tier] {
		return ccbilling.Settings{}, fmt.Errorf("tier %q is not a Claude subscription tier", c.Tier)
	}
	settings.Tiers = tiers
	weights := ccbilling.WeightSettings{Enabled: c.ModelWeights.Enabled, Items: []ccbilling.WeightItem{}, MinSegments: c.ModelWeights.MinSegments,
		MinCostUSD: c.ModelWeights.MinCostUSD, MinShareSpread: c.ModelWeights.MinShareSpread}
	for _, item := range c.ModelWeights.Items {
		weights.Items = append(weights.Items, ccbilling.WeightItem{ID: item.ID, Name: item.Name, Models: item.Models, Manual: item.ManualMultiplier})
	}
	settings.Weights = weights
	for _, index := range c.AuthIndexes {
		settings.Accounts[index] = c.Account()
	}
	return settings.Normalized()
}

// Location is the time zone the daily trend uses.
func (c Config) Location() *time.Location {
	location, err := time.LoadLocation(c.TrendTimezone)
	if err != nil {
		return time.UTC
	}
	return location
}

func seconds(value float64) time.Duration { return time.Duration(value * float64(time.Second)) }

// configFields describe the configuration for management clients.
func configFields() []configField {
	defaults := DefaultConfig()
	return []configField{
		{Name: "instance", Type: "string", Description: "Required. Name of this CLIProxyAPI instance; written into every row and API response."},
		{Name: "postgres_dsn", Type: "string", Description: "Required. PostgreSQL connection string shared by all instances."},
		{Name: "postgres_schema", Type: "string", Default: defaults.PostgresSchema, Description: "PostgreSQL schema holding the plugin's tables."},
		{Name: "api_token", Type: "string", Description: "Bearer token of the read-only quota API; empty disables it."},
		{Name: "admin_token", Type: "string", Description: "Bearer token of the admin operations; empty disables them."},
		{Name: "price_url", Type: "string", Default: defaults.PriceURL, Description: "new-api ratio table the prices come from."},
		{Name: "price_refresh_hours", Type: "number", Default: defaults.PriceRefreshHours, Description: "Age after which usage handling refreshes the ratio table."},
		{Name: "tier", Type: "enum", EnumValues: []string{"pro", "max5x", "max20x"}, Description: "Claude subscription tier; default pro."},
		{Name: "manual_k", Type: "number", Description: "Weekly/5-hour ratio overriding the estimated one."},
		{Name: "safety_factor", Type: "number", Default: defaults.SafetyFactor, Description: "Discount of the remaining capacity."},
		{Name: "estimation", Type: "object", Description: "Estimation parameters."},
		{Name: "model_weights", Type: "object", Description: "Model quota multipliers; enabled makes the estimate use them."},
	}
}
