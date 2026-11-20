package ccbilling

import (
	"math"
	"strings"
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
	noUsage bool   // the provider reported no usage
	credits string
	windows []testWindow
}

// insertRequests stores requests of an account the way the plugin records
// them: a request, and a quota report with its windows when the response
// reported any.
func insertRequests(t *testing.T, host *fakeHost, authIndex, provider string, requests []testRequest) {
	t.Helper()
	limit := DefaultProviders()[provider].LimitKey
	f := newFacts(t, host)
	for _, request := range requests {
		main := false
		for _, window := range request.windows {
			if window.limit == limit && (window.seconds == 18000 || window.seconds == 604800) {
				main = true
			}
		}
		usage := request.usage
		if usage == "" {
			usage = "complete"
		}
		if request.noUsage {
			usage = ""
		}
		f.requestUsage(authIndex, provider, request.at, request.latency, request.usd, request.model, request.source != "none", main, usage)
		if len(request.windows) > 0 || request.credits != "" {
			f.report(authIndex, provider, request.at, request.credits, request.windows...)
		}
	}
	f.flush()
}

func codexMain(used5, used7 float64, reset5, reset7 time.Time) []testWindow {
	return []testWindow{{"codex", 18000, used5, reset5}, {"codex", 604800, used7, reset7}}
}

func codexSettings() Settings {
	settings := DefaultSettings()
	settings.Accounts["c1"] = AccountSettings{Provider: ProviderCodex}
	return settings
}

var codexAccount = AccountSettings{Provider: ProviderCodex}

func TestProviderSettingsValidation(t *testing.T) {
	settings := codexSettings()
	settings.Accounts["c1"] = AccountSettings{Tier: TierPro, Provider: ProviderCodex}
	if _, err := settings.Normalized(); err == nil || !strings.Contains(err.Error(), `"pro" is not a tier of ChatGPT accounts`) {
		t.Fatalf("claude tier on a codex account: %v", err)
	}
	settings.Accounts["c1"] = AccountSettings{Tier: TierTeam, Provider: ProviderCodex}
	normalized, err := settings.Normalized()
	if err != nil || normalized.Accounts["c1"].Provider != ProviderCodex || normalized.AccountProvider("c1") != ProviderCodex {
		t.Fatalf("account provider = %+v, %v", normalized.Accounts["c1"], err)
	}
	settings.Accounts["c1"] = AccountSettings{Provider: "gemini"}
	if _, err := settings.Normalized(); err == nil || !strings.Contains(err.Error(), `Unknown provider "gemini"`) {
		t.Fatalf("unknown provider: %v", err)
	}
	settings = codexSettings()
	profile := settings.Providers[ProviderCodex]
	profile.Tiers = []Tier{{ID: TierTeam, WeeklyUSD: 80, K: floatPointer(0.5)}}
	settings.Providers = cloneProviders(settings.Providers)
	settings.Providers[ProviderCodex] = profile
	if _, err := settings.Normalized(); err == nil || !strings.Contains(err.Error(), "default k of tier Team") {
		t.Fatalf("k out of range: %v", err)
	}
	profile.Tiers = []Tier{{ID: "plus", WeeklyUSD: 80}}
	settings.Providers[ProviderCodex] = profile
	if _, err := settings.Normalized(); err == nil || !strings.Contains(err.Error(), `Unknown tier "plus"`) {
		t.Fatalf("unknown tier: %v", err)
	}
	profile.Tiers = []Tier{{ID: TierTeam, WeeklyUSD: 80, K: floatPointer(5)}}
	cold := 3
	profile.Params = ParamOverrides{ColdStartBlocks: &cold}
	profile.Rules = SegmentRules{}
	profile.LimitKey = "other"
	settings.Providers[ProviderCodex] = profile
	normalized, err = settings.Normalized()
	if err != nil {
		t.Fatal(err)
	}
	codex := normalized.Providers[ProviderCodex]
	if codex.Tiers[0].WeeklyUSD != 80 || *codex.Tiers[0].K != 5 || codex.LimitKey != "codex" || !codex.Rules.MainWindowOnly {
		t.Fatalf("codex profile = %+v", codex)
	}
	if params := normalized.ParamsFor(ProviderCodex); params.ColdStartBlocks != 3 || params.TierWeekly("") != 80 {
		t.Fatalf("codex params = %+v", params)
	}
	if params := normalized.ParamsFor(ProviderClaude); params.ColdStartBlocks != 20 {
		t.Fatalf("claude params took codex's override: %+v", params)
	}
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
		costs = append(costs, CostEvent{At: at.Add(30 * time.Second), USD: float64(i + 1), Model: "gpt-test"})
	}
	var state StreamState
	state.Cursor = start.Add(-time.Second)
	params := codexSettings().ParamsFor(ProviderCodex)
	segments := state.Advance(observations, costs, start.Add(time.Hour), params)
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

func codexManager(t *testing.T, settings Settings) (*Manager, *fakeHost) {
	t.Helper()
	host := newFakeHost(t)
	manager := newTestManager(host)
	configure(t, manager, settings, codexAccount, "c1")
	return manager, host
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

// steadyRequests are requests of an account whose 5-hour window rises by
// one point for every $0.10 (5-hour total $10) and whose 7-day window by one
// point for every $0.60 (weekly total $60), in blocks of busy minutes.
func steadyRequests(start time.Time, blocks int, with5h bool, extra func(i int, request *testRequest)) []testRequest {
	var requests []testRequest
	spent := 0.0
	reset7 := start.Add(6 * 24 * time.Hour)
	for block := 0; block < blocks; block++ {
		blockStart := start.Add(time.Duration(block) * 2 * time.Hour)
		reset5 := blockStart.Add(5 * time.Hour)
		spent5 := 0.0
		for i := 0; i < 30; i++ {
			at := blockStart.Add(time.Duration(i) * time.Minute)
			request := testRequest{at: at, latency: 10 * time.Second, usd: 0.05, model: "gpt-test"}
			windows := []testWindow{{"codex", 604800, math.Floor(spent / 0.6), reset7}}
			if with5h {
				windows = append(windows, testWindow{"codex", 18000, math.Floor(spent5 / 0.1), reset5})
			}
			request.windows = windows
			if extra != nil {
				extra(block*30+i, &request)
			}
			requests = append(requests, request)
			if request.usd > 0 && len(request.windows) > 0 && request.windows[0].limit == "codex" {
				spent += request.usd
				spent5 += request.usd
			}
		}
		// The next block starts a new 5-hour window.
		if with5h {
			spent5 = 0
		}
	}
	return requests
}

func TestCodexAccountWithFiveHourWindow(t *testing.T) {
	settings := codexSettings()
	cold, points := 3, 100.0
	profile := settings.Providers[ProviderCodex]
	profile.Params.ColdStartBlocks = &cold
	// Too little 7-day gain to estimate k.
	profile.Params.KMinWeeklyPoints = &points
	settings.Providers[ProviderCodex] = profile
	manager, host := codexManager(t, settings)
	start := host.now.Add(-2 * 24 * time.Hour)
	insertRequests(t, host, "c1", ProviderCodex, steadyRequests(start, 8, true, nil))
	account := refreshedAccount(t, manager, "c1")
	if account.Provider != ProviderCodex || account.Window == WindowWeek || account.Tier != TierTeam {
		t.Fatalf("account = %+v", account.AccountStatus)
	}
	if account.ColdStart || math.Abs(account.Estimate.FiveHourUSD-10) > 1 {
		t.Fatalf("5-hour estimate = %+v", account.Estimate)
	}
	// No manual, estimated or tier k: the weekly total is the tier's, and
	// the 5-hour total is measured but not turned into a weekly one.
	if account.K != nil || !account.NeedsK || account.WeeklyTotal != 60 || account.TotalsFrom != SourceTier || account.FiveHourTotal <= 0 {
		t.Fatalf("account without k = %+v", account.AccountStatus)
	}
	if quota := manager.Quotas()[0]; quota.KSource != KSourceNone || quota.State != StateEstimated || quota.K != nil {
		t.Fatalf("quota without k = %+v", quota)
	}
	manual := 6.0
	configure(t, manager, settings, AccountSettings{Provider: ProviderCodex, ManualK: &manual}, "c1")
	account = refreshedAccount(t, manager, "c1")
	if account.NeedsK || account.KSource != SourceManual || math.Abs(account.WeeklyTotal-6*account.FiveHourTotal) > 1e-9 || account.FiveHourTotal <= 0 {
		t.Fatalf("account with manual k = %+v", account.AccountStatus)
	}
}

func TestCodexAccountWithOnlyTheWeeklyWindow(t *testing.T) {
	settings := codexSettings()
	cold := 3
	profile := settings.Providers[ProviderCodex]
	profile.Params.ColdStartBlocks = &cold
	settings.Providers[ProviderCodex] = profile
	manager, host := codexManager(t, settings)
	start := host.now.Add(-4 * 24 * time.Hour)
	// Larger requests so the weekly window rises in each block.
	insertRequests(t, host, "c1", ProviderCodex, steadyRequests(start, 12, false, func(_ int, request *testRequest) { request.usd = 0.3 }))
	account := refreshedAccount(t, manager, "c1")
	if account.Window != WindowWeek || account.NeedsK || account.FiveHourTotal != 0 {
		t.Fatalf("weekly-only account = %+v", account.AccountStatus)
	}
	if account.ColdStart || account.TotalsFrom != SourceEstimated || math.Abs(account.WeeklyTotal-60) > 6 {
		t.Fatalf("weekly total = %+v", account.AccountStatus)
	}
	segments := account.Estimate.Segments
	// A 5-hour window appearing later restarts the segments on it.
	later := host.now.Add(-5 * time.Minute)
	insertRequests(t, host, "c1", ProviderCodex, []testRequest{
		{at: later, usd: 0.05, model: "gpt-test", windows: codexMain(1, 80, later.Add(4*time.Hour), later.Add(48*time.Hour))},
	})
	host.now = host.now.Add(20 * time.Minute)
	account = refreshedAccount(t, manager, "c1")
	var stored int
	if err := host.db.QueryRow(`SELECT count(*) FROM segments WHERE auth_index = 'c1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if account.Window == WindowWeek || stored != 0 || segments == 0 {
		t.Fatalf("after a 5-hour window: window %q, %d stored segments (had %d)", account.Window, stored, segments)
	}
}

// Requests metered against another limit or without a report leave only
// their cost out; a request whose cost is unknown voids its segment.
func TestCodexSegmentsExcludeOtherLimitsAndVoidUnpriced(t *testing.T) {
	settings := codexSettings()
	start := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	reset := start.Add(5 * time.Hour)
	var others, unknown []testRequest
	for i, request := range steadyRequests(start, 6, true, nil) {
		at := request.at.Add(20 * time.Second)
		switch i % 10 {
		case 1:
			// A Spark request reports only its own pool.
			others = append(others, testRequest{at: at, usd: 1, model: "gpt-spark", windows: []testWindow{{"bengalfox", 18000, 50, reset}}})
		case 3:
			others = append(others, testRequest{at: at, usd: 2, model: "gpt-review", windows: []testWindow{{"code-review", 604800, 10, reset}}})
		case 5:
			unknown = append(unknown, testRequest{at: at, usd: 0, model: "gpt-unpriced", source: "none",
				windows: codexMain(request.windows[1].used, request.windows[0].used, request.windows[1].reset, request.windows[0].reset)})
		case 7:
			// No report at all, such as a failed request.
			others = append(others, testRequest{at: at, usd: 3, model: "gpt-test"})
		case 9:
			// Another pool's request of an unpriced model is still only
			// another pool's request.
			others = append(others, testRequest{at: at, usd: 0, model: "gpt-spark-next", source: "none",
				windows: []testWindow{{"bengalfox", 18000, 50, reset}}})
		}
	}
	estimate := func(extra []testRequest) AccountView {
		t.Helper()
		manager, host := codexManager(t, settings)
		insertRequests(t, host, "c1", ProviderCodex, append(steadyRequests(start, 6, true, nil), extra...))
		return refreshedAccount(t, manager, "c1")
	}
	reference := estimate(nil)

	account := estimate(others)
	if account.Estimate.Ratio != reference.Estimate.Ratio || account.Estimate.Segments != reference.Estimate.Segments ||
		account.Excluded.Voided() != 0 {
		t.Fatalf("other limits changed the estimate: %+v vs %+v, excluded %+v", account.Estimate, reference.Estimate, account.Excluded)
	}
	excluded := account.Excluded
	if math.Abs(excluded.USD[ExcludedOtherLimit]-float64(18*1+18*2+18*3)) > 1e-9 || excluded.Requests[ExcludedOtherLimit] != 18*4 ||
		len(excluded.UnpricedModels) != 0 {
		t.Fatalf("excluded = %+v", excluded)
	}

	// Three unpriced requests per block, each completing in its own
	// segment: those 18 segments are voided, the rest are unchanged.
	account = estimate(append(others, unknown...))
	excluded = account.Excluded
	if excluded.VoidedSegments[ExcludedUnpriced] != 18 || account.Estimate.Segments != reference.Estimate.Segments-18 ||
		excluded.Requests[ExcludedUnpriced] != 18 || excluded.UnpricedModels["gpt-unpriced"] != 18 || len(excluded.UnpricedModels) != 1 ||
		excluded.Requests[ExcludedOtherLimit] != 18*4 {
		t.Fatalf("segments %d (reference %d), excluded = %+v", account.Estimate.Segments, reference.Estimate.Segments, excluded)
	}
	if account.Estimate.TotalBlocks != reference.Estimate.TotalBlocks || account.Estimate.Ratio <= 0 {
		t.Fatalf("voiding changed the blocks: %+v vs %+v", account.Estimate, reference.Estimate)
	}
}

// A used-up window or credits being spent pause the segments; they resume
// once usage moves the utilization again.
func TestCodexSegmentsPauseWhileExhausted(t *testing.T) {
	settings := codexSettings()
	manager, host := codexManager(t, settings)
	start := host.now.Add(-2 * 24 * time.Hour)
	reset7 := start.Add(6 * 24 * time.Hour)
	var requests []testRequest
	add := func(at time.Time, used5, used7 float64, credits string, usd float64) {
		requests = append(requests, testRequest{at: at, latency: time.Second, usd: usd, model: "gpt-test", credits: credits,
			windows: codexMain(used5, used7, at.Truncate(5*time.Hour).Add(5*time.Hour), reset7)})
	}
	// Normal rises, then the 5-hour window is used up while requests go on.
	for i := 0; i < 10; i++ {
		add(start.Add(time.Duration(i)*time.Minute), float64(90+i), 40, "", 0.1)
	}
	for i := 0; i < 5; i++ {
		add(start.Add(time.Duration(10+i)*time.Minute), 100, 40, "", 5)
	}
	insertRequests(t, host, "c1", ProviderCodex, requests)
	host.now = start.Add(30 * time.Minute)
	account := refreshedAccount(t, manager, "c1")
	if account.Estimate.Segments != 8 {
		t.Fatalf("segments before the limit = %d", account.Estimate.Segments)
	}
	manager.Observe("c1", Observation{At: host.now, Has5h: true, Used5h: 100, Reset5h: host.now.Add(time.Hour), LimitReached: true})
	account = refreshedAccount(t, manager, "c1")
	if !account.Paused || account.PauseReason != PauseLimitReached {
		t.Fatalf("account at the limit = %+v", account.AccountStatus)
	}
	// Credits are spent while the windows stay below the limit.
	requests = nil
	next := start.Add(6 * time.Hour)
	for i, credits := range []string{"10", "9.5", "9", "8.5"} {
		add(next.Add(time.Duration(i)*time.Minute), float64(i+1), 41, credits, 5)
	}
	// Then usage moves the utilization again.
	for i := 0; i < 6; i++ {
		add(next.Add(time.Duration(10+i)*time.Minute), float64(10+i), 41, "8.5", 0.1)
	}
	insertRequests(t, host, "c1", ProviderCodex, requests)
	host.now = next.Add(5 * time.Minute)
	manager.Observe("c1", Observation{At: host.now.Add(-time.Hour), Has5h: true, Used5h: 4, Reset5h: next.Add(5 * time.Hour)})
	host.now = next.Add(30 * time.Minute)
	account = refreshedAccount(t, manager, "c1")
	// The credit requests cost $20 but moved the utilization 3 points: no
	// segment includes them. After them, 5 rises make 4 segments.
	if account.Estimate.Segments != 8+4 {
		t.Fatalf("segments after resuming = %d", account.Estimate.Segments)
	}
	for _, segment := range manager.accounts["c1"].segments {
		if segment.CostUSD > 1 {
			t.Fatalf("a paused cost reached segment %+v", segment)
		}
	}
	// Replaying the credit period alone pauses with the credits reason.
	var state StreamState
	state.Cursor = next.Add(-time.Second)
	var observations []Observation
	for i, credits := range []float64{10, 9.5} {
		observations = append(observations, Observation{At: next.Add(time.Duration(i) * time.Minute), Has5h: true, Used5h: float64(i + 1),
			Reset5h: next.Add(time.Hour), HasCredits: true, Credits: credits})
	}
	state.Advance(observations, nil, next.Add(time.Hour), settings.ParamsFor(ProviderCodex))
	if !state.Paused || state.PauseReason != PauseCredits {
		t.Fatalf("credit stream = %+v", state)
	}
	// Claude has no such rule.
	var claude StreamState
	claude.Cursor = next.Add(-time.Second)
	claude.Advance([]Observation{{At: next, Has5h: true, Used5h: 100, Reset5h: next.Add(time.Hour)}}, nil, next.Add(time.Hour), DefaultParams())
	if claude.Paused {
		t.Fatal("a Claude stream paused")
	}
}
