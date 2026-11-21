package ccbilling

import (
	"math"
	"time"
)

// Sources of an account's totals and k.
const (
	SourceEstimated = "estimated"
	SourceTier      = "tier"
	SourceManual    = "manual"
	SourceAuto      = "auto"
	SourceNone      = "none"
)

// AccountStatus is one account's estimate and current capacity.
type AccountStatus struct {
	AuthIndex string `json:"auth_index"`
	Tier      string `json:"tier"`
	Estimate
	KAuto      *float64  `json:"k_auto,omitempty"`
	KManual    *float64  `json:"k_manual,omitempty"`
	K          *float64  `json:"k,omitempty"`
	KSource    string    `json:"k_source"`
	KEstimate  KEstimate `json:"k_estimate"`
	ColdStart  bool      `json:"cold_start"`
	TotalsFrom string    `json:"totals_from"`
	// NeedsK reports an account whose 5-hour reservation needs a k it does
	// not have: no manual, estimated or tier default k.
	NeedsK bool `json:"needs_k"`
	// FiveHourTotal and WeeklyTotal are the effective totals in USD. A zero
	// FiveHourTotal means it is unknown, which disables 5-hour reservations.
	FiveHourTotal float64 `json:"five_hour_total_usd"`
	WeeklyTotal   float64 `json:"weekly_total_usd"`
	// Utilization in percent; a window whose reset time passed reads as 0.
	Used5h  float64   `json:"used_5h"`
	Used7d  float64   `json:"used_7d"`
	Reset5h time.Time `json:"reset_5h,omitzero"`
	Reset7d time.Time `json:"reset_7d,omitzero"`
	// ObservedAt is when the latest report was made; zero means never.
	ObservedAt time.Time `json:"observed_at,omitzero"`
	// RemainingWeek and RemainingFiveHour are R_week and R_5h after the safety factor.
	RemainingWeek     float64 `json:"remaining_week_usd"`
	RemainingFiveHour float64 `json:"remaining_five_hour_usd"`
}

// ResolveAccount combines an account's estimate, k evidence and latest report.
//
// Segments of the 5-hour window give its total; k turns it into the weekly
// total. Without any k the weekly total is the tier's default and 5-hour
// reservations are off.
func ResolveAccount(authIndex string, settings AccountSettings, estimate Estimate, k KEstimate, latest Observation, params Params, now time.Time) AccountStatus {
	status := AccountStatus{AuthIndex: authIndex, Tier: settings.Tier, Estimate: estimate, KManual: settings.ManualK,
		KEstimate: k, KSource: SourceNone, ObservedAt: latest.At}
	if status.Tier == "" {
		status.Tier = params.fallbackTier()
	}
	status.KAuto = k.K
	switch {
	case settings.ManualK != nil:
		status.K, status.KSource = settings.ManualK, SourceManual
	case status.KAuto != nil:
		status.K, status.KSource = status.KAuto, SourceAuto
	default:
		// Without a manual or estimated k the tier's default, if it has one,
		// keeps the 5-hour reservation working.
		if tierK := params.TierKFor(status.Tier); tierK != nil {
			status.K, status.KSource = tierK, SourceTier
		}
	}
	status.ColdStart = estimate.CleanBlocks < params.ColdStartBlocks || estimate.FiveHourUSD <= 0
	switch {
	case status.ColdStart:
		status.TotalsFrom = SourceTier
		status.WeeklyTotal = params.TierWeekly(status.Tier)
		if status.K != nil {
			status.FiveHourTotal = status.WeeklyTotal / *status.K
		}
	case status.K != nil:
		status.TotalsFrom = SourceEstimated
		status.FiveHourTotal = estimate.FiveHourUSD
		status.WeeklyTotal = *status.K * estimate.FiveHourUSD
	default:
		// The 5-hour total is measured, but the weekly total needs k.
		status.TotalsFrom = SourceTier
		status.FiveHourTotal = estimate.FiveHourUSD
		status.WeeklyTotal = params.TierWeekly(status.Tier)
	}
	status.NeedsK = status.K == nil
	if latest.Has5h && latest.Reset5h.After(now) {
		status.Used5h, status.Reset5h = latest.Used5h, latest.Reset5h
	} else if latest.Has5h && latest.Reset5h.IsZero() {
		status.Used5h = latest.Used5h
	}
	if latest.Has7d && latest.Reset7d.After(now) {
		status.Used7d, status.Reset7d = latest.Used7d, latest.Reset7d
	} else if latest.Has7d && latest.Reset7d.IsZero() {
		status.Used7d = latest.Used7d
	}
	status.RemainingWeek = params.SafetyFactor * status.WeeklyTotal * math.Max(0, 1-status.Used7d/100)
	status.RemainingFiveHour = params.SafetyFactor * status.FiveHourTotal * math.Max(0, 1-status.Used5h/100)
	return status
}
