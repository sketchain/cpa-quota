package ccbilling

import (
	"time"
)

// Estimate states of an account in the quota API.
const (
	StateCold      = "cold"
	StateEstimated = "estimated"
)

// k sources of an account in the quota API.
const (
	KSourceManual  = "manual"
	KSourceAuto    = "auto"
	KSourceDefault = "default"
	KSourceNone    = "none"
)

// WindowForecast is when a window reaches 100% at the current rate.
type WindowForecast struct {
	// Source is the rate used: "reading" (utilization reports), "spend"
	// (recorded cost over the estimated total) or "none".
	Source string `json:"source"`
	// PerHourPercent is utilization points per hour.
	PerHourPercent float64 `json:"per_hour_percent"`
	// Outcome is eta, after_reset, idle, reached or unknown.
	Outcome    string     `json:"outcome"`
	ExhaustsAt *time.Time `json:"exhausts_at"`
	// LookbackSeconds is the period the rate was measured over.
	LookbackSeconds int64 `json:"lookback_seconds"`
}

// WindowQuota is one quota window of an account.
type WindowQuota struct {
	UsedPercent float64    `json:"used_percent"`
	ResetAt     *time.Time `json:"reset_at"`
	// TotalUSD is the window's estimated total; zero when unknown.
	TotalUSD float64 `json:"total_usd"`
	// RemainingUSD is SafetyFactor × TotalUSD × (1 − used).
	RemainingUSD float64        `json:"remaining_usd"`
	Forecast     WindowForecast `json:"forecast"`
}

// EstimateSummary is what the account's segments say.
type EstimateSummary struct {
	// Ratio is utilization points per USD on Basis; USDPerPoint its inverse.
	Ratio            float64 `json:"ratio"`
	USDPerPoint      float64 `json:"usd_per_point"`
	CleanBlocks      int     `json:"clean_blocks"`
	TotalBlocks      int     `json:"total_blocks"`
	ColdStartBlocks  int     `json:"cold_start_blocks"`
	Segments         int     `json:"segments"`
	ExcludedSegments int     `json:"excluded_segments"`
	// VoidedSegments counts the segments voided because a request in them
	// had a cost that could not be computed; they are not in Segments.
	VoidedSegments int    `json:"voided_segments"`
	Basis          string `json:"basis"`
	// EstimatedAt is when the snapshot was computed; null before the first.
	EstimatedAt *time.Time `json:"estimated_at"`
}

// ModelWeightSuggestion is one model under test.
type ModelWeightSuggestion struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Models []string `json:"models"`
	// Multiplier is the effective multiplier and Source where it comes
	// from: manual, calibration, estimated or default.
	Multiplier float64 `json:"multiplier"`
	Source     string  `json:"source"`
	// Status is the historical estimate's status; Estimated, Low and High
	// its multiplier and 95% interval when Status is ok.
	Status        string   `json:"status"`
	Estimated     *float64 `json:"estimated_multiplier"`
	Low           *float64 `json:"low"`
	High          *float64 `json:"high"`
	CalibrationID int64    `json:"calibration_id,omitempty"`
}

// ModelWeights is the model quota multipliers.
type ModelWeights struct {
	// Enabled reports whether the estimate converts costs with them.
	Enabled     bool                    `json:"enabled"`
	Basis       string                  `json:"basis"`
	Suggestions []ModelWeightSuggestion `json:"suggestions"`
}

// AccountQuota is one account in the quota API, and the detail column of
// the estimates table.
type AccountQuota struct {
	AuthIndex string   `json:"auth_index"`
	Tier      string   `json:"tier"`
	State     string   `json:"state"`
	K         *float64 `json:"k"`
	KSource   string   `json:"k_source"`
	KAuto     *float64 `json:"k_auto"`
	// TotalsFrom is estimated, or tier while the totals are the tier's.
	TotalsFrom   string          `json:"totals_from"`
	SafetyFactor float64         `json:"safety_factor"`
	ObservedAt   *time.Time      `json:"observed_at"`
	FiveHour     WindowQuota     `json:"five_hour"`
	Week         WindowQuota     `json:"week"`
	Estimate     EstimateSummary `json:"estimate"`
	ModelWeights ModelWeights    `json:"model_weights"`
}

func timePointer(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	at = at.UTC()
	return &at
}

func floatCopy(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

// Quotas describes every tracked account from the latest snapshot and the
// live reports, in auth index order. It never refreshes or touches the
// database; before the first snapshot every account is in cold start.
func (m *Manager) Quotas() []AccountQuota {
	snap, _ := m.current()
	return quotas(snap, m.live(), m.host.Now())
}

func quotas(snap *snapshot, latest map[string]Observation, now time.Time) []AccountQuota {
	result := []AccountQuota{}
	for _, index := range snap.settings.authIndexes() {
		result = append(result, accountQuota(snap, latest, index, now))
	}
	return result
}

func accountQuota(snap *snapshot, latest map[string]Observation, authIndex string, now time.Time) AccountQuota {
	cached := snap.accounts[authIndex]
	status := accountStatus(snap, latest, authIndex, now)
	params := snap.settings.AccountParams()
	quota := AccountQuota{AuthIndex: authIndex, Tier: status.Tier, K: floatCopy(status.K), KAuto: floatCopy(status.KAuto),
		TotalsFrom: status.TotalsFrom, SafetyFactor: params.SafetyFactor, ObservedAt: timePointer(status.ObservedAt), State: StateEstimated}
	if status.ColdStart {
		quota.State = StateCold
	}
	switch status.KSource {
	case SourceManual:
		quota.KSource = KSourceManual
	case SourceAuto:
		quota.KSource = KSourceAuto
	case SourceTier:
		quota.KSource = KSourceDefault
	default:
		quota.KSource = KSourceNone
	}
	forecast := cached.forecast
	if forecast.FiveHour.Source == "" {
		forecast = unknownForecast()
	}
	window := func(used float64, reset time.Time, total, remaining float64, f AccountWindowForecast, lookback int64) WindowQuota {
		result := WindowQuota{UsedPercent: used, ResetAt: timePointer(reset), TotalUSD: total, RemainingUSD: remaining,
			Forecast: WindowForecast{Source: f.Source, Outcome: f.Outcome.Status, ExhaustsAt: timePointer(f.Outcome.At), LookbackSeconds: lookback}}
		switch f.Source {
		case RateReading:
			result.Forecast.PerHourPercent = f.Reading.PerHour
		case RateSpend:
			result.Forecast.PerHourPercent = f.Spend.PerHour
		}
		if result.Forecast.Outcome == "" {
			result.Forecast.Outcome = OutcomeUnknown
		}
		if result.Forecast.Source == "" {
			result.Forecast.Source = RateNone
		}
		return result
	}
	quota.FiveHour = window(status.Used5h, status.Reset5h, status.FiveHourTotal, status.RemainingFiveHour, forecast.FiveHour, forecast.Window5h)
	quota.Week = window(status.Used7d, status.Reset7d, status.WeeklyTotal, status.RemainingWeek, forecast.Week, forecast.Window7d)
	weights := estimateWeights(snap)
	quota.Estimate = EstimateSummary{Ratio: status.Ratio, CleanBlocks: status.CleanBlocks, TotalBlocks: status.TotalBlocks,
		ColdStartBlocks: params.ColdStartBlocks, Segments: status.Estimate.Segments, ExcludedSegments: status.ExcludedSegments,
		VoidedSegments: cached.excluded.Voided(), Basis: weights.Basis, EstimatedAt: timePointer(snap.at)}
	if status.Ratio > 0 {
		quota.Estimate.USDPerPoint = 1 / status.Ratio
	}
	quota.ModelWeights = ModelWeights{Enabled: snap.settings.Weights.Enabled, Basis: weights.Basis,
		Suggestions: []ModelWeightSuggestion{}}
	estimates := map[string]ItemEstimate{}
	for _, item := range snap.weights.estimate.Items {
		estimates[item.ID] = item
	}
	effective := weights.Weights
	if len(effective) == 0 {
		// Before the first snapshot the items are known, their multipliers not.
		for _, item := range snap.settings.Weights.Items {
			effective = append(effective, EffectiveWeight{ID: item.ID, Name: item.Name, Models: item.Models, Multiplier: 1,
				Source: WeightSourceDefault, Status: WeightInsufficient})
			if item.Manual != nil {
				effective[len(effective)-1].Multiplier, effective[len(effective)-1].Source = *item.Manual, WeightSourceManual
			}
		}
	}
	for _, weight := range effective {
		suggestion := ModelWeightSuggestion{ID: weight.ID, Name: weight.Name, Models: weight.Models, Multiplier: weight.Multiplier,
			Source: weight.Source, Status: weight.Status, CalibrationID: weight.CalibrationID}
		if suggestion.Models == nil {
			suggestion.Models = []string{}
		}
		if item, ok := estimates[weight.ID]; ok && item.Status == WeightOK && item.Multiplier != nil {
			low, high := item.Low, item.High
			suggestion.Estimated, suggestion.Low, suggestion.High = floatCopy(item.Multiplier), &low, &high
		}
		quota.ModelWeights.Suggestions = append(quota.ModelWeights.Suggestions, suggestion)
	}
	return quota
}
