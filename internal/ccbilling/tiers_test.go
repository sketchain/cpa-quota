package ccbilling

import (
	"slices"
	"strings"
	"testing"
)

// editWeights changes the weights in a copy of settings, so settings read
// from a manager are not changed in place.
func editWeights(settings *Settings, edit func(*WeightSettings)) {
	settings.Weights.Items = slices.Clone(settings.Weights.Items)
	edit(&settings.Weights)
}

func TestTierSettingsValidation(t *testing.T) {
	settings := DefaultSettings()
	settings.Accounts["a1"] = AccountSettings{Tier: "team"}
	if _, err := settings.Normalized(); err == nil || !strings.Contains(err.Error(), `Tier "team" is not a Claude tier`) {
		t.Fatalf("unknown account tier: %v", err)
	}
	settings = DefaultSettings()
	settings.Tiers = []Tier{{ID: TierMax5x, WeeklyUSD: 900, K: floatPointer(0.5)}}
	if _, err := settings.Normalized(); err == nil || !strings.Contains(err.Error(), "default k of tier Max 5x") {
		t.Fatalf("k out of range: %v", err)
	}
	settings.Tiers = []Tier{{ID: "team", WeeklyUSD: 80}}
	if _, err := settings.Normalized(); err == nil || !strings.Contains(err.Error(), `Unknown tier "team"`) {
		t.Fatalf("unknown tier: %v", err)
	}
	settings.Tiers = []Tier{{ID: TierMax5x, WeeklyUSD: 900, K: floatPointer(5)}}
	normalized, err := settings.Normalized()
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.Tiers) != 3 || normalized.Tiers[0].ID != TierPro || normalized.Tiers[1].WeeklyUSD != 900 || *normalized.Tiers[1].K != 5 {
		t.Fatalf("tiers = %+v", normalized.Tiers)
	}
	params := normalized.AccountParams()
	if params.TierWeekly(TierMax5x) != 900 || params.TierWeekly("") != 250 || *params.TierKFor(TierMax5x) != 5 || params.fallbackTier() != TierPro {
		t.Fatalf("params = %+v", params)
	}
}
