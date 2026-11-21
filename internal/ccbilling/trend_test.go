package ccbilling

import (
	"math"
	"testing"
	"time"
)

// trendManager stores segments with an outside-usage stretch and hourly k
// buckets for a1 before the settings load them, then publishes a snapshot.
func trendManager(t *testing.T) (*Manager, *fakeHost) {
	t.Helper()
	host := newFakeHost(t)
	block := host.now.Add(-30 * time.Hour)
	for i := range 24 {
		cost := 0.1
		if i >= 10 && i < 14 {
			cost = 0.01 // points without plugin cost: outside usage
		}
		if _, err := host.db.Exec(`INSERT INTO segments (instance, auth_index, block_ns, at_ns, cost_usd, gain) VALUES ($1, 'a1', $2, $3, $4, 1)`,
			testInstance, nanos(block), nanos(block.Add(time.Duration(i+1)*time.Minute)), cost); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 6 {
		if _, err := host.db.Exec(`INSERT INTO k_buckets (instance, auth_index, bucket_ns, gain_5h, gain_7d) VALUES ($1, 'a1', $2, $3, $4)`,
			testInstance, nanos(host.now.Add(-time.Duration(48-i)*time.Hour).Truncate(time.Hour)), 8.0, 1.0); err != nil {
			t.Fatal(err)
		}
	}
	manager := newTestManager(host)
	configure(t, manager, DefaultSettings(), AccountSettings{}, "a1", "a2")
	if manager.snap.Load() == nil {
		t.Fatal("no snapshot")
	}
	return manager, host
}

func TestTrendMarksTheEstimatesExclusionsWithoutRefreshingOrWriting(t *testing.T) {
	manager, host := trendManager(t)
	snap := manager.snap.Load()
	estimate := snap.accounts["a1"].estimate
	if estimate.ExcludedSegments == 0 {
		t.Fatalf("the fixture excludes nothing: %+v", estimate)
	}
	// New history a refresh would process.
	host.now = host.now.Add(2 * time.Hour)
	if _, err := host.db.Exec(`SELECT 1`); err != nil {
		t.Fatal(err)
	}
	latest := host.calls.Load()
	trend, found, err := manager.Trend("a1")
	if err != nil || !found {
		t.Fatalf("Trend = %v, %v", found, err)
	}
	if manager.snap.Load() != snap {
		t.Fatal("Trend published a new snapshot")
	}
	// Now, WithDB and nothing else: no refresh reads observations.
	if calls := host.calls.Load() - latest; calls != 2 {
		t.Fatalf("Trend made %d host calls, want Now and WithDB", calls)
	}
	// All 24 segments ended within one hour.
	if !trend.Loaded || trend.Exclusion != ExclusionEstimate || len(trend.Segments) != 1 || len(trend.Buckets) != 6 {
		t.Fatalf("trend = %+v", trend)
	}
	hour := trend.Segments[0]
	excluded, cleanGain, cleanCost := hour.Excluded, hour.Gain, hour.CostUSD
	if !hour.At.Equal(snap.at.Add(-30*time.Hour).Truncate(time.Hour)) || hour.Segments+hour.Excluded != 24 {
		t.Fatalf("segment hour = %+v", hour)
	}
	var blockGain, blockCost float64
	for _, block := range estimate.Blocks {
		blockGain += block.CleanGain
		blockCost += block.CleanCost
	}
	if excluded != estimate.ExcludedSegments || math.Abs(cleanGain-blockGain) > 1e-9 || math.Abs(cleanCost-blockCost) > 1e-9 {
		t.Fatalf("trend excludes %d (%.2f / %.2f), estimate %d (%.2f / %.2f)", excluded, cleanGain, cleanCost,
			estimate.ExcludedSegments, blockGain, blockCost)
	}
	// No manual k and no valid estimate: the tier default.
	if trend.K == nil || *trend.K != 8 || trend.KSource != SourceTier || trend.KMinWeeklyPoints != 10 || trend.RetentionDays != 365 {
		t.Fatalf("trend k = %v %s, params %v %d", trend.K, trend.KSource, trend.KMinWeeklyPoints, trend.RetentionDays)
	}
	if !trend.From.Equal(snap.at.Add(-365 * 24 * time.Hour)) {
		t.Fatalf("trend from = %v", trend.From)
	}
}

func TestTrendAccountsAndUnloadedState(t *testing.T) {
	manager, _ := trendManager(t)
	if _, found, err := manager.Trend("unknown"); found || err != nil {
		t.Fatalf("unconfigured account = %v, %v", found, err)
	}
	// Accounts without history are tracked too.
	if trend, found, err := manager.Trend("a2"); !found || err != nil || len(trend.Segments) != 0 || trend.Buckets == nil {
		t.Fatalf("a2 = %+v, %v, %v", trend, found, err)
	}
	// An account not tracked yet reads nothing.
	host := newFakeHost(t)
	fresh := newTestManager(host)
	if _, found, err := fresh.Trend("a1"); found || err != nil {
		t.Fatalf("Trend of an untracked account = %v, %v", found, err)
	}
	if host.calls.Load() != 0 {
		t.Fatalf("Trend before loading made %d host calls", host.calls.Load())
	}
	// Settings loaded without a snapshot: raw segments and the settings' k.
	settings := *manager.loaded.Load()
	manager.snap.Store(nil)
	manual := 6.5
	settings.Accounts["a1"] = AccountSettings{ManualK: &manual}
	manager.loaded.Store(&settings)
	trend, found, err := manager.Trend("a1")
	if err != nil || !found || trend.Loaded || trend.Exclusion != ExclusionRaw || trend.K == nil || *trend.K != manual || trend.KSource != SourceManual {
		t.Fatalf("unloaded trend = %+v, %v, %v", trend, found, err)
	}
	if len(trend.Segments) != 1 || trend.Segments[0].Excluded != 0 || trend.Segments[0].Segments != 24 ||
		math.Abs(trend.Segments[0].CostUSD-(20*0.1+4*0.01)) > 1e-9 {
		t.Fatalf("raw segment hours = %+v", trend.Segments)
	}
}

func TestSegmentExclusionsMatchEstimate(t *testing.T) {
	block := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var segments []Segment
	for i := range 40 {
		cost := 0.1
		if i%20 >= 8 && i%20 < 12 {
			cost = 0.01
		}
		if i == 20 {
			block = block.Add(time.Hour)
		}
		segments = append(segments, Segment{Block: block, At: block.Add(time.Duration(i+1) * time.Minute), CostUSD: cost, Gain: 1})
	}
	for _, mode := range []string{ReferenceGlobal, ReferenceBlock} {
		params := DefaultParams()
		params.ReferenceMode = mode
		now := block.Add(2 * time.Hour)
		estimate := EstimateSegments(segments, params, now)
		excluded := SegmentExclusions(segments, params, now)
		count := 0
		for _, flag := range excluded {
			if flag {
				count++
			}
		}
		if count != estimate.ExcludedSegments || count == 0 {
			t.Fatalf("%s: %d exclusions, estimate %d", mode, count, estimate.ExcludedSegments)
		}
	}
}

func TestDailyTrendSplitsDaysInTheLocation(t *testing.T) {
	manager, host := trendManager(t)
	days, ok := manager.TrendDays("a1")
	if !ok || len(days) != 2 {
		t.Fatalf("days = %+v, %v", days, ok)
	}
	// Six buckets of 8 / 1 points two days ago, and the 24 segments a day later.
	if days[0].Gain5h != 48 || days[0].Gain7d != 6 || days[0].K == nil || *days[0].K != 8 || days[0].Segments != 0 {
		t.Fatalf("first day = %+v", days[0])
	}
	if days[1].Segments+days[1].ExcludedSegments != 24 || days[1].ExcludedSegments == 0 || days[1].K != nil {
		t.Fatalf("second day = %+v", days[1])
	}
	var stored int
	if err := host.db.QueryRow(`SELECT count(*) FROM k_trend_daily WHERE instance = $1 AND auth_index = 'a1'`, testInstance).Scan(&stored); err != nil || stored != 2 {
		t.Fatalf("stored days = %d, %v", stored, err)
	}
	// Days of another time zone start at its midnight.
	location := time.FixedZone("UTC+14", 14*3600)
	shifted := dailyTrend(manager.accounts["a1"].buckets, nil, nil, location)
	if len(shifted) == 0 || shifted[0].Day == days[0].Day && shifted[0].Gain5h == days[0].Gain5h {
		t.Fatalf("shifted days = %+v", shifted)
	}
}
