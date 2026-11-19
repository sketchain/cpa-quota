package ccbilling

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"
)

// fillHost stores a simulation's reports and costs as the plugin records them.
func fillHost(t *testing.T, host *fakeHost, sim *simulation, authIndex string) {
	t.Helper()
	f := newFacts(t, host)
	for _, obs := range sim.obs {
		f.report(authIndex, ProviderClaude, obs.At, "", claudeWindows(obs)...)
	}
	for _, cost := range sim.costs {
		f.request(authIndex, ProviderClaude, cost.At, 0, cost.USD, cost.Model, true, true)
	}
	f.flush()
}

func weightedHost(t *testing.T) (*Manager, *fakeHost) {
	return weightedHostWith(t, func(session int) []float64 { return []float64{float64(session%7) / 10} })
}

func weightedHostWith(t *testing.T, mix func(int) []float64) (*Manager, *fakeHost) {
	t.Helper()
	host := newFakeHost(t)
	sim := newSimulation(11, 40, 8)
	sim.runMixed(30, []string{"claude-sonnet", "claude-opus"}, []mixedModel{{name: "claude-fable", weight: 4}}, mix, sporadicOutside)
	fillHost(t, host, sim, "a1")
	return newTestManager(host), host
}

func storedSegments(t *testing.T, host *fakeHost, authIndex string) []Segment {
	t.Helper()
	segments, err := loadSegments(context.Background(), host.db, testInstance, authIndex, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return segments
}

func TestWeightsSwitchConvertsOnlyWhenOn(t *testing.T) {
	manager, host := weightedHost(t)
	settings := DefaultSettings()
	editWeights(&settings, ProviderClaude, func(w *WeightSettings) {
		w.Items = []WeightItem{{ID: "fable", Name: "Fable", Models: []string{"claude-fable"}}}
	})
	configure(t, manager, settings, AccountSettings{}, "a1")
	segments := storedSegments(t, host, "a1")
	if len(segments) < 1000 {
		t.Fatalf("segments = %d", len(segments))
	}
	for _, segment := range segments {
		if !segment.Split() {
			t.Fatalf("segment without a split: %+v", segment)
		}
	}
	params := settings.Params
	snap := manager.snap.Load()
	account := snap.accounts["a1"]
	// Off: exactly the estimate without weights, value for value.
	if snap.weights[ProviderClaude].basis != BasisRaw || !reflect.DeepEqual(account.estimate, EstimateSegments(segments, params, snap.at)) ||
		!reflect.DeepEqual(account.estimate, account.raw) {
		t.Fatal("the switched-off estimate differs from the raw estimate")
	}
	effective := snap.weights[ProviderClaude].effective[0]
	if effective.Source != WeightSourceEstimated || math.Abs(effective.Multiplier-4) > 0.2 {
		t.Fatalf("effective = %+v, estimate = %+v", effective, snap.weights[ProviderClaude].estimate)
	}
	off, _ := manager.Estimates()
	if off.For(ProviderClaude).Basis != BasisRaw || off.Accounts["a1"].Ratio != account.raw.Ratio {
		t.Fatalf("estimates = %+v", off)
	}
	// The raw basis excludes Fable-heavy segments as if they were outside usage.
	if account.raw.ExcludedSegments <= account.weighted.ExcludedSegments {
		t.Fatalf("excluded raw %d, weighted %d", account.raw.ExcludedSegments, account.weighted.ExcludedSegments)
	}

	editWeights(&settings, ProviderClaude, func(w *WeightSettings) { w.Enabled = true })
	configure(t, manager, settings, AccountSettings{}, "a1")
	snap = manager.snap.Load()
	account = snap.accounts["a1"]
	factors := map[string]float64{"claude-fable": snap.weights[ProviderClaude].effective[0].Multiplier}
	if snap.weights[ProviderClaude].basis != BasisWeighted || !reflect.DeepEqual(snap.weights[ProviderClaude].factors, factors) ||
		!reflect.DeepEqual(account.estimate, EstimateSegments(ConvertSegments(segments, factors), params, snap.at)) {
		t.Fatal("the switched-on estimate is not the estimate of the converted costs")
	}
	// The weighted ratio is points per baseline USD: 100 / $40.
	if math.Abs(account.estimate.FiveHourUSD-40)/40 > 0.05 {
		t.Fatalf("weighted 5-hour total = %.2f", account.estimate.FiveHourUSD)
	}
	on, ok := manager.Estimates()
	if !ok || on.For(ProviderClaude).Basis != BasisWeighted || on.Accounts["a1"].Ratio != account.weighted.Ratio || len(on.For(ProviderClaude).Weights) != 1 {
		t.Fatalf("estimates = %+v", on)
	}
	weights := weightsViews(snap, manager.live(), host.now)[ProviderClaude]
	if len(weights.Comparison) != 1 || weights.Comparison[0].Raw.Ratio != account.raw.Ratio ||
		weights.Comparison[0].Weighted.Ratio != account.weighted.Ratio || len(weights.Models) != 3 {
		t.Fatalf("weights view = %+v", weights)
	}
	// The quota API shows the suggestion with its historical estimate.
	quota := manager.Quotas()[0]
	if !quota.ModelWeights.Enabled || quota.ModelWeights.Basis != BasisWeighted || len(quota.ModelWeights.Suggestions) != 1 ||
		quota.ModelWeights.Suggestions[0].Estimated == nil || quota.Estimate.Basis != BasisWeighted {
		t.Fatalf("model weights = %+v", quota.ModelWeights)
	}

	// A manual multiplier takes precedence.
	manual := 2.0
	editWeights(&settings, ProviderClaude, func(w *WeightSettings) { w.Items[0].Manual = &manual })
	configure(t, manager, settings, AccountSettings{}, "a1")
	if weight := manager.snap.Load().weights[ProviderClaude].effective[0]; weight.Source != WeightSourceManual || weight.Multiplier != 2 {
		t.Fatalf("effective = %+v", weight)
	}
	// Trend marks the exclusions of the converted costs.
	trend, _, err := manager.Trend("a1")
	if err != nil || trend.Basis != BasisWeighted {
		t.Fatalf("trend = %+v, %v", trend.Basis, err)
	}
}

func TestCalibrationLifecycle(t *testing.T) {
	// Three sessions a day: days 10 to 14 send only Fable, the days before
	// only baseline models, the days after a mix.
	manager, host := weightedHostWith(t, func(session int) []float64 {
		switch day := session / 3; {
		case day < 10:
			return []float64{0}
		case day < 15:
			return []float64{1}
		}
		return []float64{0.3}
	})
	settings := DefaultSettings()
	editWeights(&settings, ProviderClaude, func(w *WeightSettings) { w.Items = []WeightItem{{ID: "fable", Models: []string{"claude-fable"}}} })
	configure(t, manager, settings, AccountSettings{}, "a1")
	if _, err := manager.StartCalibration("missing", "a1"); !IsInvalid(err) {
		t.Fatalf("err = %v", err)
	}
	if _, err := manager.StartCalibration("fable", "a2"); !IsInvalid(err) {
		t.Fatalf("err = %v", err)
	}
	// Calibrate the Fable-only days, which the history already covers.
	host.now = time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	calibration, err := manager.StartCalibration("fable", "a1")
	if err != nil || calibration.Result.Status != CalibrationRunning {
		t.Fatalf("calibration = %+v, %v", calibration, err)
	}
	if _, err := manager.StartCalibration("fable", "a1"); !IsInvalid(err) {
		t.Fatalf("a second calibration of the account: %v", err)
	}
	host.now = time.Date(2026, 9, 15, 23, 0, 0, 0, time.UTC)
	if calibrations := manager.snap.Load().weights[ProviderClaude].calibrations; len(calibrations) != 1 || calibrations[0].Final ||
		calibrations[0].Result.Status != CalibrationRunning {
		t.Fatalf("calibrations = %+v", calibrations)
	}
	if err := manager.StopCalibration(calibration.ID); err != nil {
		t.Fatal(err)
	}
	// The history was processed past the period, so the result is final.
	calibrations := manager.snap.Load().weights[ProviderClaude].calibrations
	if len(calibrations) != 1 || !calibrations[0].Final {
		t.Fatalf("calibrations = %+v", calibrations)
	}
	result := calibrations[0].Result
	// The period has no baseline segments, so it compares with the days before.
	if result.Status != CalibrationOK || result.BaseFrom != CalibrationBaseRecent || math.Abs(*result.Multiplier-4) > math.Max(3*result.StdErr, 0.1) {
		t.Fatalf("result = %+v", result)
	}
	if weight := manager.snap.Load().weights[ProviderClaude].effective[0]; weight.Source != WeightSourceCalibration || weight.CalibrationID != calibration.ID {
		t.Fatalf("effective = %+v", weight)
	}
	var stored string
	if err := host.db.QueryRow(`SELECT result FROM calibrations WHERE instance = $1 AND id = $2`, testInstance, calibration.ID).Scan(&stored); err != nil || stored == "" {
		t.Fatalf("stored result = %q, %v", stored, err)
	}
	if err := manager.StopCalibration(calibration.ID); !IsCalibrationNotFound(err) {
		t.Fatalf("stopping an ended calibration: %v", err)
	}
	if err := manager.DeleteCalibration(calibration.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteCalibration(calibration.ID); !IsCalibrationNotFound(err) {
		t.Fatalf("deleting twice: %v", err)
	}
	if calibrations := manager.snap.Load().weights[ProviderClaude].calibrations; len(calibrations) != 0 {
		t.Fatalf("calibrations = %+v", calibrations)
	}
}

func TestPruneRemovesSegmentSplits(t *testing.T) {
	host := newFakeHost(t)
	ctx := context.Background()
	old, recent := host.now.Add(-48*time.Hour), host.now.Add(-time.Hour)
	if err := commitAdvance(ctx, host.db, testInstance, "a1", StreamState{}, []Segment{
		{At: old, Block: old, CostUSD: 1, Gain: 1, Models: map[string]float64{"x": 1}},
		{At: recent, Block: recent, CostUSD: 2, Gain: 1, Models: map[string]float64{"x": 1.5, "y": 0.5}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := pruneSegments(ctx, host.db, testInstance, host.now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	segments := storedSegments(t, host, "a1")
	if len(segments) != 1 || !reflect.DeepEqual(segments[0].Models, map[string]float64{"x": 1.5, "y": 0.5}) {
		t.Fatalf("segments = %+v", segments)
	}
}
