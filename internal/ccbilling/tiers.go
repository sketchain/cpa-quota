package ccbilling

import (
	"fmt"
	"slices"
)

// ProviderClaude is the provider name CLIProxyAPI gives Claude OAuth
// credentials; only its accounts are tracked.
const ProviderClaude = "claude"

// mainLimitKey is the quota report limit whose 5-hour and 7-day windows are
// the subscription's main windows: Claude's unified limit.
const mainLimitKey = "unified"

// Tier is one subscription tier: its default weekly total, used while an
// account is in cold start, and its default k. A nil K means no default: an
// account then needs a manual or estimated k for 5-hour reservations.
type Tier struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	WeeklyUSD float64  `json:"weekly_usd"`
	K         *float64 `json:"k"`
}

func floatPointer(value float64) *float64 { return &value }

// DefaultTiers returns the built-in tiers in display order; the first is the
// default of accounts without a tier.
func DefaultTiers() []Tier {
	return []Tier{
		{ID: TierPro, Name: "Pro", WeeklyUSD: 250, K: floatPointer(8)},
		{ID: TierMax5x, Name: "Max 5x", WeeklyUSD: 1250, K: floatPointer(8)},
		{ID: TierMax20x, Name: "Max 20x", WeeklyUSD: 2500, K: floatPointer(8)},
	}
}

func hasTier(tiers []Tier, id string) bool {
	return slices.ContainsFunc(tiers, func(tier Tier) bool { return tier.ID == id })
}

// normalizedTiers validates the operator's tiers. Only their amounts and k
// are the operator's; the tiers themselves are built in.
func normalizedTiers(input []Tier) ([]Tier, error) {
	tiers := DefaultTiers()
	for _, tier := range input {
		index := slices.IndexFunc(tiers, func(known Tier) bool { return known.ID == tier.ID })
		if index < 0 {
			return nil, fmt.Errorf("Unknown tier %q", tier.ID)
		}
		if !finite(tier.WeeklyUSD) || tier.WeeklyUSD <= 0 || tier.WeeklyUSD > 1e9 {
			return nil, fmt.Errorf("The weekly total of tier %s must be a positive amount", tiers[index].Name)
		}
		if tier.K != nil && (!finite(*tier.K) || *tier.K < 1 || *tier.K > 100) {
			return nil, fmt.Errorf("The default k of tier %s must be from 1 to 100", tiers[index].Name)
		}
		tiers[index].WeeklyUSD = tier.WeeklyUSD
		tiers[index].K = nil
		if tier.K != nil {
			tiers[index].K = floatPointer(*tier.K)
		}
	}
	return tiers, nil
}

// tierTables are the tiers' amounts and k as the maps Params carries.
func tierTables(tiers []Tier) (map[string]float64, map[string]float64) {
	weekly, k := map[string]float64{}, map[string]float64{}
	for _, tier := range tiers {
		weekly[tier.ID] = tier.WeeklyUSD
		if tier.K != nil {
			k[tier.ID] = *tier.K
		}
	}
	return weekly, k
}

// AccountParams is the parameters of the tracked accounts: the global ones
// with the tiers.
func (s Settings) AccountParams() Params {
	params := s.Params
	tiers := s.Tiers
	if tiers == nil {
		tiers = DefaultTiers()
	}
	params.TierWeeklyUSD, params.TierK = tierTables(tiers)
	if len(tiers) > 0 {
		params.defaultTier = tiers[0].ID
	}
	return params
}
