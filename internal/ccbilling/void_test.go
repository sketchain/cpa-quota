package ccbilling

import (
	"fmt"
	"math"
	"testing"
	"time"
)

type testRequest struct {
	at      time.Time
	latency time.Duration
	usd     float64
	model   string
	source  string // price source; "none" means unpriced
	usage   string // accounting quality; "" means complete
	noUsage bool   // the request reported no usage
	windows []testWindow
}

// insertRequests stores requests of an account the way the plugin records
// them: a request, and a quota report with its windows when the response
// reported any.
func insertRequests(t *testing.T, host *fakeHost, authIndex string, requests []testRequest) {
	t.Helper()
	f := newFacts(t, host)
	for _, request := range requests {
		usage := request.usage
		if usage == "" {
			usage = "complete"
		}
		if request.noUsage {
			usage = ""
		}
		f.requestUsage(authIndex, request.at, request.latency, request.usd, request.model, request.source != "none", usage)
		if len(request.windows) > 0 {
			f.report(authIndex, request.at, request.windows...)
		}
	}
	f.flush()
}

func refreshedAccount(t *testing.T, manager *Manager, authIndex string) AccountView {
	t.Helper()
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	for _, account := range manager.View().Accounts {
		if account.AuthIndex == authIndex {
			return account
		}
	}
	t.Fatalf("account %s missing from %+v", authIndex, manager.View().Accounts)
	return AccountView{}
}

// claudeSteady are requests of a Claude account whose 5-hour window rises
// by one point for every $0.10 (5-hour total $10) and whose 7-day window by
// one point for every $0.60 (weekly total $60), in blocks of busy minutes.
func claudeSteady(start time.Time, blocks int) []testRequest {
	var requests []testRequest
	spent := 0.0
	reset7 := start.Add(6 * 24 * time.Hour)
	for block := 0; block < blocks; block++ {
		blockStart := start.Add(time.Duration(block) * 2 * time.Hour)
		reset5 := blockStart.Add(5 * time.Hour)
		spent5 := 0.0
		for i := 0; i < 30; i++ {
			at := blockStart.Add(time.Duration(i) * time.Minute)
			requests = append(requests, testRequest{at: at, latency: 10 * time.Second, usd: 0.05, model: "claude-test",
				windows: []testWindow{{"unified", 604800, math.Floor(spent / 0.6), reset7}, {"unified", 18000, math.Floor(spent5 / 0.1), reset5}}})
			spent += 0.05
			spent5 += 0.05
		}
	}
	return requests
}

// Reports with decimals cut segments where a whole point is crossed, and a
// rise across several points makes one segment of that gain.
func TestSegmentsCutAtWholePoints(t *testing.T) {
	start := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	reset5, reset7 := start.Add(4*time.Hour), start.Add(5*24*time.Hour)
	used := []float64{10.2, 10.6, 11.3, 11.9, 12.4, 15.2, 15.9, 16.0}
	var observations []Observation
	var costs []CostEvent
	for i, value := range used {
		at := start.Add(time.Duration(i) * time.Minute)
		observations = append(observations, Observation{At: at, Has5h: true, Used5h: value, Reset5h: reset5, Has7d: true, Used7d: 3, Reset7d: reset7})
		costs = append(costs, CostEvent{At: at.Add(30 * time.Second), USD: float64(i + 1), Model: "claude-test"})
	}
	var state StreamState
	state.Cursor = start.Add(-time.Second)
	segments := state.Advance(observations, costs, start.Add(time.Hour), DefaultSettings().AccountParams())
	// Armed at 11.3; the costs completed after it until 12.4 crosses 12
	// (3+4); until 15.2 crosses 13-15 (5); until 16.0 crosses 16 (6+7).
	want := []struct{ gain, cost float64 }{{12.4 - 11.3, 3 + 4}, {15.2 - 12.4, 5}, {16 - 15.2, 6 + 7}}
	if len(segments) != len(want) {
		t.Fatalf("segments = %+v", segments)
	}
	for i, segment := range segments {
		if math.Abs(segment.Gain-want[i].gain) > 1e-6 || math.Abs(segment.CostUSD-want[i].cost) > 1e-9 {
			t.Fatalf("segment %d = %+v, want %+v", i, segment, want[i])
		}
	}
	// Whole-point reports behave as before: each rise is a segment.
	var whole StreamState
	whole.Cursor = start.Add(-time.Second)
	var wholeObs []Observation
	for i, value := range []float64{10, 11, 14, 15} {
		wholeObs = append(wholeObs, Observation{At: start.Add(time.Duration(i) * time.Minute), Has5h: true, Used5h: value, Reset5h: reset5})
	}
	if segments := whole.Advance(wholeObs, costs[:3], start.Add(time.Hour), DefaultParams()); len(segments) != 2 || segments[0].Gain != 3 || segments[1].Gain != 1 {
		t.Fatalf("whole segments = %+v", segments)
	}
	// A fraction times 100 that lands just below a whole point counts as it.
	if !crossesPoint(28.000000000000004, 0.29*100) || crossesPoint(0.29*100, 29.4) {
		t.Fatal("rounding of whole points")
	}
}

// A request whose cost is unknown voids the segment it completes in. Cost
// before a block's first rise belongs to no segment, so an unknown one there
// voids nothing.
func TestAdvanceVoidsSegmentsWithUnknownCost(t *testing.T) {
	start := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	reset5, reset7 := start.Add(4*time.Hour), start.Add(5*24*time.Hour)
	var observations []Observation
	var costs []CostEvent
	for i := range 8 {
		at := start.Add(time.Duration(i) * time.Minute)
		observations = append(observations, Observation{At: at, Has5h: true, Used5h: float64(10 + i), Reset5h: reset5,
			Has7d: true, Used7d: 3, Reset7d: reset7})
		costs = append(costs, CostEvent{At: at.Add(30 * time.Second), USD: 1, Model: "claude-test"})
	}
	// Before the first rise (armed at 11%).
	costs = append(costs, CostEvent{At: start.Add(10 * time.Second), Model: "claude-new", Excluded: ExcludedUnpriced, Unpriced: true})
	// In the segment 12% → 13%, twice: it is voided once.
	costs = append(costs, CostEvent{At: start.Add(2*time.Minute + 40*time.Second), Model: "claude-new", Excluded: ExcludedUnpriced, Unpriced: true},
		CostEvent{At: start.Add(2*time.Minute + 50*time.Second), Model: "claude-test", Excluded: ExcludedUnpriced})
	var state StreamState
	state.Cursor = start.Add(-time.Second)
	segments := state.Advance(observations, costs, start.Add(time.Hour), DefaultParams())
	// Rises at 11..17 make six segments after arming at 11; 12→13 is voided.
	if len(segments) != 5 {
		t.Fatalf("segments = %+v", segments)
	}
	for _, segment := range segments {
		if segment.CostUSD != 1 || segment.Gain != 1 || !segment.Block.Equal(start) {
			t.Fatalf("segment = %+v", segment)
		}
		if segment.At.Equal(start.Add(3 * time.Minute)) {
			t.Fatalf("voided segment kept: %+v", segment)
		}
	}
	excluded := state.Excluded
	if excluded.VoidedSegments[ExcludedUnpriced] != 1 || excluded.Voided() != 1 || excluded.Requests[ExcludedUnpriced] != 3 ||
		excluded.UnpricedModels["claude-new"] != 2 ||
		len(excluded.UnpricedModels) != 1 || state.Void != "" {
		t.Fatalf("excluded = %+v, void %q", excluded, state.Void)
	}
}

// On a Claude account, requests of an unpriced model or with incomplete
// usage void their segments, which leave the totals, and the unpriced
// models are listed; a request that reported no usage costs nothing.
func TestClaudeSegmentsVoidUnknownCost(t *testing.T) {
	start := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	settings := DefaultSettings()
	estimate := func(extra []testRequest) (AccountView, AccountQuota, map[string]int) {
		t.Helper()
		host := newFakeHost(t)
		manager := newTestManager(host)
		configure(t, manager, settings, AccountSettings{Tier: TierMax5x}, "a1")
		insertRequests(t, host, "a1", append(claudeSteady(start, 6), extra...))
		account := refreshedAccount(t, manager, "a1")
		rows, err := host.db.Query(`SELECT at_ns, cost_usd, gain FROM segments WHERE instance = $1 AND auth_index = 'a1'`, testInstance)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		stored := map[string]int{}
		for rows.Next() {
			var at int64
			var cost, gain float64
			if err := rows.Scan(&at, &cost, &gain); err != nil {
				t.Fatal(err)
			}
			stored[fmt.Sprintf("%d %.9f %.6f", at, cost, gain)]++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return account, manager.Quotas()[0], stored
	}
	reference, _, referenceStored := estimate(nil)
	if reference.Estimate.Segments == 0 || reference.Excluded.Voided() != 0 {
		t.Fatalf("reference = %+v, excluded %+v", reference.Estimate, reference.Excluded)
	}

	var extra []testRequest
	for i, request := range claudeSteady(start, 6) {
		at := request.at.Add(20 * time.Second)
		switch i % 10 {
		case 1:
			// A failed request of an unpriced model without usage.
			extra = append(extra, testRequest{at: at, model: "claude-new", source: "none", noUsage: true})
		case 5:
			extra = append(extra, testRequest{at: at, model: "claude-new", source: "none"})
		case 7:
			extra = append(extra, testRequest{at: at, model: "claude-test", usage: "inconsistent"})
		}
	}
	account, quota, stored := estimate(extra)
	// Per block: unknown costs at minutes 5, 7, 15, 17, 25 and 27, each in a
	// segment of its own.
	const voided = 6 * 6
	excluded := account.Excluded
	if excluded.VoidedSegments[ExcludedUnpriced] != voided || account.Estimate.Segments != reference.Estimate.Segments-voided ||
		len(stored) != len(referenceStored)-voided || excluded.Requests[ExcludedUnpriced] != voided ||
		excluded.UnpricedModels["claude-new"] != voided/2 || len(excluded.UnpricedModels) != 1 {
		t.Fatalf("segments %d/%d stored (reference %d/%d), excluded = %+v", account.Estimate.Segments, len(stored),
			reference.Estimate.Segments, len(referenceStored), excluded)
	}
	if quota.Estimate.VoidedSegments != voided || quota.Estimate.Segments != account.Estimate.Segments {
		t.Fatalf("quota estimate = %+v", quota.Estimate)
	}
	// The kept segments are the reference's, unchanged, in the same blocks.
	for segment := range stored {
		if referenceStored[segment] != 1 {
			t.Fatalf("segment %s is not one of the reference's", segment)
		}
	}
	if account.Estimate.TotalBlocks != reference.Estimate.TotalBlocks || account.Estimate.Ratio <= 0 {
		t.Fatalf("estimate = %+v, reference %+v", account.Estimate, reference.Estimate)
	}
}
