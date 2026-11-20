package ccbilling

import (
	"fmt"
	"testing"
	"time"
)

// A request whose cost is unknown voids the segment it completes in; one
// of another pool only leaves its cost out. Cost before a block's first
// rise belongs to no segment, so an unknown one there voids nothing.
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
	// In the segment 14% → 15%, another pool's request.
	costs = append(costs, CostEvent{At: start.Add(4*time.Minute + 40*time.Second), USD: 5, Model: "gpt-spark", Excluded: ExcludedOtherLimit})
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
		excluded.Requests[ExcludedOtherLimit] != 1 || excluded.USD[ExcludedOtherLimit] != 5 || excluded.UnpricedModels["claude-new"] != 2 ||
		len(excluded.UnpricedModels) != 1 || state.Void != "" {
		t.Fatalf("excluded = %+v, void %q", excluded, state.Void)
	}
}

// claudeSteady is steadyRequests on a Claude account's unified windows.
func claudeSteady(start time.Time, blocks int) []testRequest {
	requests := steadyRequests(start, blocks, true, nil)
	for i := range requests {
		windows := make([]testWindow, len(requests[i].windows))
		for j, window := range requests[i].windows {
			window.limit = "unified"
			windows[j] = window
		}
		requests[i].windows = windows
		requests[i].model = "claude-test"
	}
	return requests
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
		insertRequests(t, host, "a1", ProviderClaude, append(claudeSteady(start, 6), extra...))
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
