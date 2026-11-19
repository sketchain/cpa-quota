package ccbilling

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// claudeRegressionDigest runs a simulated Claude account with sporadic
// outside usage through the manager and describes every estimate it
// produces. The golden digest below is the account part of the one
// cpa-super-billing's claude_regression_test.go pins, for the same
// simulation, so the estimates stay value for value what they were.
func claudeRegressionDigest(t *testing.T) string {
	t.Helper()
	sim := newSimulation(29, 40, 6)
	sim.run(24, sporadicOutside)
	host := newFakeHost(t)
	host.now = sim.end(24).Add(-30 * time.Minute)
	// The golden digest's host reported no stored utilization; the live
	// reports below are the only ones.
	saved := readLatest
	readLatest = func(context.Context, *sql.DB, string, Settings, time.Time) (map[string]Observation, error) {
		return map[string]Observation{}, nil
	}
	t.Cleanup(func() { readLatest = saved })
	facts := newFacts(t, host)
	for i, obs := range sim.obs {
		facts.request("a1", ProviderClaude, obs.At, sim.costs[i].At.Sub(obs.At), sim.costs[i].USD, "claude-test", true, true)
		facts.report("a1", ProviderClaude, obs.At, "", claudeWindows(obs)...)
	}
	facts.flush()
	manager := newTestManager(host)
	settings := DefaultSettings()
	manualK := 6.5
	configure(t, manager, settings, AccountSettings{Tier: TierMax5x}, "a1")
	last := sim.obs[len(sim.obs)-1]
	last.Reset7d = host.now.Add(3 * 24 * time.Hour)
	var b strings.Builder
	describe := func(label string) {
		if err := manager.Refresh(); err != nil {
			t.Fatal(err)
		}
		var stored int
		if err := host.db.QueryRow(`SELECT count(*) FROM segments WHERE instance = $1`, testInstance).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s segments=%d\n", label, stored)
		for _, account := range manager.View().Accounts {
			k := "nil"
			if account.K != nil {
				k = fmt.Sprintf("%.10g", *account.K)
			}
			fmt.Fprintf(&b, "account %s tier=%s ratio=%.10g t5=%.10g blocks=%d/%d segs=%d/%d k=%s/%s cold=%v from=%s T5=%.10g T7=%.10g u=%.4g/%.4g R=%.10g/%.10g\n",
				account.AuthIndex, account.Tier, account.Ratio, account.Estimate.FiveHourUSD, account.CleanBlocks, account.TotalBlocks,
				account.Estimate.Segments, account.ExcludedSegments, k, account.KSource, account.ColdStart, account.TotalsFrom,
				account.FiveHourTotal, account.WeeklyTotal, account.Used5h, account.Used7d, account.RemainingFiveHour, account.RemainingWeek)
		}
	}
	for _, used := range []float64{20, 60, 97} {
		manager.Observe("a1", Observation{At: host.now, Has5h: true, Used5h: used, Reset5h: host.now.Add(2 * time.Hour),
			Has7d: true, Used7d: 40, Reset7d: last.Reset7d})
		describe(fmt.Sprintf("5h=%g", used))
	}
	configure(t, manager, settings, AccountSettings{Tier: TierMax5x, ManualK: &manualK}, "a1")
	describe("manual k")
	return b.String()
}

func TestClaudeResultsMatchCPASuperBilling(t *testing.T) {
	got := claudeRegressionDigest(t)
	if got != claudeRegressionGolden {
		t.Fatalf("Claude digest changed:\n--- got\n%s\n--- want\n%s", got, claudeRegressionGolden)
	}
}

// claudeRegressionGolden keeps the segment count and account lines of
// cpa-super-billing's golden digest (6178db1); its pool, fixed key and key
// admission lines describe features this plugin does not have.
const claudeRegressionGolden = `5h=20 segments=10959
account a1 tier=max5x ratio=2.496990777 t5=40.0482056 blocks=97/98 segs=10959/683 k=5.995554391/auto cold=false from=estimated T5=40.0482056 T7=240.1111949 u=20/40 R=28.83470803/129.6600453
5h=60 segments=10959
account a1 tier=max5x ratio=2.496990777 t5=40.0482056 blocks=97/98 segs=10959/683 k=5.995554391/auto cold=false from=estimated T5=40.0482056 T7=240.1111949 u=60/40 R=14.41735401/129.6600453
5h=97 segments=10959
account a1 tier=max5x ratio=2.496990777 t5=40.0482056 blocks=97/98 segs=10959/683 k=5.995554391/auto cold=false from=estimated T5=40.0482056 T7=240.1111949 u=97/40 R=1.081301551/129.6600453
manual k segments=10959
account a1 tier=max5x ratio=2.496990777 t5=40.0482056 blocks=97/98 segs=10959/683 k=6.5/manual cold=false from=estimated T5=40.0482056 T7=260.3133364 u=97/40 R=1.081301551/140.5692016
`
