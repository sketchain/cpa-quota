package ccbilling

import (
	"math"
	"reflect"
	"slices"
	"testing"
	"time"
)

// simulatedHost stores a Claude account's simulated history as instance's.
func simulatedHost(t *testing.T, days int) (*fakeHost, *simulation) {
	t.Helper()
	sim := newSimulation(7, 40, 6)
	sim.run(days, sporadicOutside)
	host := newFakeHost(t)
	host.now = sim.end(days).Add(-30 * time.Minute)
	fillHost(t, host, sim, "a1")
	return host, sim
}

func TestStreamsWithoutKCursorRebuildKEvidenceAndKeepSegments(t *testing.T) {
	host := newFakeHost(t)
	start := host.now.Add(-48 * time.Hour)
	reset5, reset7 := start.Add(5*time.Hour), start.Add(6*24*time.Hour)
	f := newFacts(t, host)
	for i := 0; i < 240; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		f.report("a1", at, testWindow{"unified", 18000, float64(i / 2), reset5}, testWindow{"unified", 604800, float64(i / 12), reset7})
	}
	f.flush()
	// A stream saved before k evidence existed: segments, but no k cursor.
	cursor := host.now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := host.db.Exec(`INSERT INTO streams (instance, auth_index, state) VALUES ($1, 'a1', $2)`, testInstance, `{"cursor":"`+cursor+`"}`); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []time.Duration{time.Minute, 2 * time.Minute} {
		if _, err := host.db.Exec(`INSERT INTO segments (instance, auth_index, block_ns, at_ns, cost_usd, gain) VALUES ($1, 'a1', $2, $3, 0.5, 1)`,
			testInstance, nanos(start), nanos(start.Add(offset))); err != nil {
			t.Fatal(err)
		}
	}
	manager := newTestManager(host)
	configure(t, manager, DefaultSettings(), AccountSettings{}, "a1")
	var segments, buckets int
	if err := host.db.QueryRow(`SELECT (SELECT count(*) FROM segments), (SELECT count(*) FROM k_buckets)`).Scan(&segments, &buckets); err != nil {
		t.Fatal(err)
	}
	if segments != 2 || buckets == 0 {
		t.Fatalf("segments = %d, k buckets = %d", segments, buckets)
	}
	k := manager.snap.Load().accounts["a1"].k
	if k.K == nil || math.Abs(*k.K-6) > 0.6 {
		t.Fatalf("rebuilt k = %+v", k)
	}
}

func TestInstancesSharingTheDatabaseStayApart(t *testing.T) {
	host, _ := simulatedHost(t, 6)
	first := newTestManager(host)
	configure(t, first, DefaultSettings(), AccountSettings{}, "a1")
	other := newTestManager(host)
	other.Instance = "other"
	// The other instance has an account of the same auth index with one
	// report of its own.
	f := newFacts(t, host)
	f.report("a1", host.now.Add(-time.Hour), testWindow{"unified", 18000, 77, host.now.Add(time.Hour)})
	f.flush()
	if _, err := host.db.Exec(`UPDATE quota_reports SET instance = 'other' WHERE id = $1`, host.nextID.Load()); err != nil {
		t.Fatal(err)
	}
	if _, err := host.db.Exec(`UPDATE quota_report_windows SET instance = 'other' WHERE report_id = $1`, host.nextID.Load()); err != nil {
		t.Fatal(err)
	}
	configure(t, other, DefaultSettings(), AccountSettings{}, "a1")
	mine, theirs := first.Quotas()[0], other.Quotas()[0]
	if mine.Estimate.Segments == 0 || theirs.Estimate.Segments != 0 || theirs.FiveHour.UsedPercent != 77 || mine.FiveHour.UsedPercent == 77 {
		t.Fatalf("mine = %+v, theirs = %+v", mine.Estimate, theirs)
	}
	var rows int
	if err := host.db.QueryRow(`SELECT count(*) FROM segments WHERE instance = 'other'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("other instance segments = %d, %v", rows, err)
	}
	if err := host.db.QueryRow(`SELECT count(*) FROM estimates`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("estimate rows = %d, %v", rows, err)
	}
}

func TestUnavailableDatabaseColdStartsThenLoadsTheHistory(t *testing.T) {
	host, _ := simulatedHost(t, 24)
	host.dbErr = errOffline
	manager := newTestManager(host)
	if _, err := manager.Configure(DefaultSettings(), AccountSettings{Tier: TierMax5x}, nil); err != nil {
		t.Fatal(err)
	}
	if !manager.Track("a1") {
		t.Fatal("a1 not tracked")
	}
	manager.Observe("a1", Observation{At: host.now, Has5h: true, Used5h: 30, Reset5h: host.now.Add(time.Hour),
		Has7d: true, Used7d: 50, Reset7d: host.now.Add(48 * time.Hour)})
	if err := manager.Refresh(); err == nil || manager.LastError() == "" {
		t.Fatal("refreshed without a database")
	}
	quota := manager.Quotas()[0]
	// Cold start: the tier's weekly total and default k, the live utilization.
	if quota.State != StateCold || quota.Week.TotalUSD != 1250 || quota.KSource != KSourceDefault || *quota.K != 8 ||
		math.Abs(quota.Week.RemainingUSD-0.9*1250*0.5) > 1e-9 || math.Abs(quota.FiveHour.RemainingUSD-0.9*1250/8*0.7) > 1e-9 ||
		quota.Estimate.EstimatedAt != nil || quota.TotalsFrom != SourceTier {
		t.Fatalf("cold quota = %+v", quota)
	}
	// The database comes back: the next refresh loads the history.
	host.dbErr = nil
	host.generation.Add(1)
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	quota = manager.Quotas()[0]
	if quota.State != StateEstimated || quota.KSource != KSourceAuto || quota.TotalsFrom != SourceEstimated || quota.Estimate.CleanBlocks < 20 ||
		quota.FiveHour.UsedPercent != 30 || quota.Estimate.EstimatedAt == nil {
		t.Fatalf("estimated quota = %+v", quota)
	}
	if math.Abs(quota.FiveHour.TotalUSD-40)/40 > 0.15 || math.Abs(quota.Week.RemainingUSD-0.9*quota.Week.TotalUSD*0.5) > 1e-9 {
		t.Fatalf("totals = %+v / %+v", quota.FiveHour, quota.Week)
	}
	var state string
	var remaining float64
	if err := host.db.QueryRow(`SELECT state, remaining_week_usd FROM estimates WHERE instance = $1 AND auth_index = 'a1'`, testInstance).Scan(&state, &remaining); err != nil ||
		state != StateEstimated || math.Abs(remaining-quota.Week.RemainingUSD) > 1e-9 {
		t.Fatalf("stored estimate = %s %v, %v", state, remaining, err)
	}
	var blocks int
	if err := host.db.QueryRow(`SELECT count(*) FROM blocks WHERE instance = $1`, testInstance).Scan(&blocks); err != nil || blocks != quota.Estimate.TotalBlocks {
		t.Fatalf("stored blocks = %d of %d, %v", blocks, quota.Estimate.TotalBlocks, err)
	}
	// Losing the database again keeps the snapshot; reads go on.
	host.dbErr = errOffline
	if err := manager.Refresh(); err == nil {
		t.Fatal("refreshed without a database")
	}
	if again := manager.Quotas()[0]; again.State != StateEstimated || again.Estimate.CleanBlocks != quota.Estimate.CleanBlocks {
		t.Fatalf("quota after losing the database = %+v", again)
	}
}

func TestRebuildRecreatesTheSameEstimate(t *testing.T) {
	host, _ := simulatedHost(t, 6)
	manager := newTestManager(host)
	configure(t, manager, DefaultSettings(), AccountSettings{}, "a1")
	before := manager.snap.Load().accounts["a1"].estimate
	if before.Segments == 0 {
		t.Fatal("no segments")
	}
	if err := manager.Rebuild(time.Minute); err != nil {
		t.Fatal(err)
	}
	after := manager.snap.Load().accounts["a1"].estimate
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("rebuilt estimate differs:\n%+v\n%+v", before, after)
	}
}

func TestTrackingFollowsTheDatabaseAndTheAllowedAccounts(t *testing.T) {
	host := newFakeHost(t)
	f := newFacts(t, host)
	for _, index := range []string{"a1", "a2"} {
		f.request(index, host.now.Add(-time.Hour), time.Second, 1, "m", true)
	}
	f.flush()
	manager := newTestManager(host)
	if _, err := manager.Configure(DefaultSettings(), AccountSettings{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	// The database's accounts are tracked.
	if got := manager.Settings().authIndexes(); !slices.Equal(got, []string{"a1", "a2"}) {
		t.Fatalf("tracked = %v", got)
	}
	if _, err := manager.Configure(DefaultSettings(), AccountSettings{}, []string{"a2", "a3"}); err != nil {
		t.Fatal(err)
	}
	if manager.Track("a9") || !manager.Track("a3") {
		t.Fatal("tracking ignores the allowed accounts")
	}
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	if got := manager.Settings().authIndexes(); !slices.Equal(got, []string{"a2", "a3"}) {
		t.Fatalf("tracked = %v", got)
	}
}

func TestAccountsSeenDuringARefreshAreTrackedByTheNext(t *testing.T) {
	host := newFakeHost(t)
	manager := newTestManager(host)
	configure(t, manager, DefaultSettings(), AccountSettings{})
	// Track never waits for a running refresh.
	manager.refreshMu.Lock()
	if manager.Track("a7") {
		t.Fatal("tracked while a refresh held the lock")
	}
	manager.refreshMu.Unlock()
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	if quotas := manager.Quotas(); len(quotas) != 1 || quotas[0].AuthIndex != "a7" {
		t.Fatalf("quotas = %+v", quotas)
	}
}
