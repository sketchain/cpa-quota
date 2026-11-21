package ccbilling

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Host gives the manager the plugin's database and clock. Every method must
// be safe to call while the manager holds its own lock, and none may call
// back into it.
type Host interface {
	// WithDB runs fn with the database, its context bounded by timeout. It
	// fails at once while the database is unavailable.
	WithDB(timeout time.Duration, fn func(ctx context.Context, db *sql.DB) error) error
	// DBGeneration changes whenever the database becomes available after
	// it was not, so the manager reloads what it cached from it.
	DBGeneration() uint64
	Now() time.Time
	Logf(format string, args ...any)
}

// accountCache is an account's history as the refresh keeps it.
type accountCache struct {
	stream   StreamState
	segments []Segment
	buckets  []KBucket
}

// accountSnapshot is what a refresh computed about an account.
type accountSnapshot struct {
	// estimate is on the basis in effect; raw and weighted are both bases.
	estimate Estimate
	raw      Estimate
	weighted Estimate
	k        KEstimate
	cursor   time.Time
	kCursor  time.Time
	// excluded comes from the account's stream.
	excluded ExcludedCost
	// forecast is the consumption forecast as of the snapshot.
	forecast AccountForecast
	// trend is the account's daily k evidence and segments.
	trend []TrendDay
}

// snapshot is everything the quota API reads. A refresh replaces it as a whole.
type snapshot struct {
	at       time.Time
	settings Settings
	accounts map[string]accountSnapshot
	weights  weightsSnapshot
}

// Manager keeps the estimates of every tracked account of one instance.
//
// The quota API only reads the latest snapshot and the live utilization; it
// never waits for a refresh. Refreshes run synchronously within host calls
// (usage.handle and API requests): a refresh is skipped while another runs,
// and none starts a goroutine.
type Manager struct {
	host Host
	// Instance scopes every row the manager reads and writes.
	Instance string

	// refreshMu serializes refreshes and owns the fields below it.
	refreshMu  sync.Mutex
	settings   Settings
	template   AccountSettings
	allowed    []string
	accounts   map[string]*accountCache
	generation uint64
	attached   bool
	// discovered reports that the database's accounts were read since the
	// caches were last loaded.
	discovered  bool
	lastRefresh time.Time
	lastPrune   time.Time
	// weightCache holds the last weight estimate and the inputs it used;
	// nil when there is none.
	weightCache *weightCache
	persisted   persistedState

	snap atomic.Pointer[snapshot]
	// loaded holds the settings, so reads without a snapshot still know the
	// tracked accounts.
	loaded atomic.Pointer[Settings]

	liveMu sync.RWMutex
	latest map[string]Observation

	// trackMu guards the accounts Track saw while a refresh ran.
	trackMu      sync.Mutex
	pendingTrack map[string]bool

	lastError atomic.Value

	// RefreshInterval throttles usage-triggered refreshes.
	RefreshInterval time.Duration
	// ProcessBudget bounds the history one refresh processes, so a large
	// backlog is spread over several host calls.
	ProcessBudget time.Duration
	// DBTimeout bounds each database call besides the processing budget.
	DBTimeout time.Duration
	// Forecast configures the consumption forecast the refresh computes.
	ForecastWindow5h, ForecastWindow7d time.Duration
	// TrendLocation splits the daily trend into days; nil means UTC.
	TrendLocation *time.Location
}

func NewManager(host Host) *Manager {
	m := &Manager{host: host, accounts: map[string]*accountCache{}, latest: map[string]Observation{}, RefreshInterval: time.Minute,
		ProcessBudget: 3 * time.Second, DBTimeout: 5 * time.Second, ForecastWindow5h: DefaultForecastWindow5h,
		ForecastWindow7d: DefaultForecastWindow7d}
	m.lastError.Store("")
	settings := DefaultSettings()
	m.settings = settings
	m.loaded.Store(&settings)
	return m
}

// Loaded reports whether a refresh has produced a snapshot.
func (m *Manager) Loaded() bool { return m.snap.Load() != nil }

// Settings returns the current settings.
func (m *Manager) Settings() Settings { return *m.loaded.Load() }

// Configure applies new settings. template is the settings of every account
// the manager tracks. A non-empty allowed limits tracking to those auth
// indexes. Accounts already tracked keep being tracked when they still
// qualify.
func (m *Manager) Configure(settings Settings, template AccountSettings, allowed []string) (Settings, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	accounts := map[string]AccountSettings{}
	for index := range settings.Accounts {
		accounts[index] = template
	}
	for index := range m.settings.Accounts {
		accounts[index] = template
	}
	settings.Accounts = map[string]AccountSettings{}
	for index, account := range accounts {
		if len(allowed) == 0 || slices.Contains(allowed, index) {
			settings.Accounts[index] = account
		}
	}
	normalized, err := settings.Normalized()
	if err != nil {
		return Settings{}, err
	}
	m.settings, m.template, m.allowed = normalized, template, slices.Clone(allowed)
	m.loaded.Store(&normalized)
	m.weightCache = nil
	m.snap.Store(nil)
	m.lastRefresh = time.Time{}
	return normalized, nil
}

// Track starts tracking an account and reports
// whether it is tracked. It never waits for a running refresh: an account
// it cannot add now is added by the next refresh, which reads the accounts
// the database has records of.
func (m *Manager) Track(authIndex string) bool {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return false
	}
	if _, ok := m.loaded.Load().Accounts[authIndex]; ok {
		return true
	}
	if !m.refreshMu.TryLock() {
		// A refresh is running; the next one adds the account.
		m.trackMu.Lock()
		if m.pendingTrack == nil {
			m.pendingTrack = map[string]bool{}
		}
		m.pendingTrack[authIndex] = true
		m.trackMu.Unlock()
		return false
	}
	defer m.refreshMu.Unlock()
	return m.trackLocked(authIndex)
}

// trackPendingLocked needs refreshMu. It adds the accounts Track could not.
func (m *Manager) trackPendingLocked() {
	m.trackMu.Lock()
	pending := m.pendingTrack
	m.pendingTrack = nil
	m.trackMu.Unlock()
	for _, index := range sortedKeys(pending) {
		m.trackLocked(index)
	}
}

func (m *Manager) trackLocked(authIndex string) bool {
	if _, ok := m.settings.Accounts[authIndex]; ok {
		return true
	}
	if len(m.allowed) > 0 && !slices.Contains(m.allowed, authIndex) {
		return false
	}
	next := m.settings
	next.Accounts = make(map[string]AccountSettings, len(m.settings.Accounts)+1)
	for index, account := range m.settings.Accounts {
		next.Accounts[index] = account
	}
	next.Accounts[authIndex] = m.template
	normalized, err := next.Normalized()
	if err != nil {
		m.recordError(err)
		return false
	}
	m.settings = normalized
	m.loaded.Store(&normalized)
	// The next refresh estimates the new account at once.
	m.lastRefresh = time.Time{}
	return true
}

// Rebuild discards the instance's segments and k evidence and rebuilds them
// from the retained request history with the current parameters, processing
// for at most budget now; later refreshes continue where it stopped.
func (m *Manager) Rebuild(budget time.Duration) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if err := m.withDB(m.DBTimeout, func(ctx context.Context, db *sql.DB) error { return resetStreams(ctx, db, m.Instance) }); err != nil {
		return err
	}
	m.accounts = map[string]*accountCache{}
	m.weightCache = nil
	m.persisted = persistedState{}
	saved := m.ProcessBudget
	if budget > 0 {
		m.ProcessBudget = budget
	}
	defer func() { m.ProcessBudget = saved }()
	return m.refreshLocked()
}

// Refresh processes new history now, waiting for a running refresh.
func (m *Manager) Refresh() error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	return m.refreshLocked()
}

// MaybeRefresh refreshes when the interval has passed and no other refresh
// is running; it never waits.
func (m *Manager) MaybeRefresh() {
	if !m.refreshMu.TryLock() {
		return
	}
	defer m.refreshMu.Unlock()
	if m.snap.Load() != nil && m.host.Now().Sub(m.lastRefresh) < m.RefreshInterval {
		return
	}
	_ = m.refreshLocked()
}

// Observe records an account's latest report as soon as its usage arrives.
func (m *Manager) Observe(authIndex string, obs Observation) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || (!obs.Has5h && !obs.Has7d) {
		return
	}
	m.liveMu.Lock()
	defer m.liveMu.Unlock()
	m.latest[authIndex] = mergeLatest(m.latest[authIndex], obs)
}

// mergeLatest keeps the newest value of each window. Reports of the same
// period never lower its utilization: a lower one arrived out of order.
func mergeLatest(current, next Observation) Observation {
	merge := func(has *bool, used *float64, reset *time.Time, nextHas bool, nextUsed float64, nextReset time.Time, newer bool) {
		if !nextHas {
			return
		}
		samePeriod := *has && absDuration(nextReset.Sub(*reset)) <= boundaryJitter
		switch {
		case !*has || (newer && !samePeriod && !nextReset.Before(reset.Add(-boundaryJitter))):
			*has, *used, *reset = true, nextUsed, nextReset
		case samePeriod && nextUsed > *used:
			*used = nextUsed
		}
	}
	newer := next.At.After(current.At)
	merge(&current.Has5h, &current.Used5h, &current.Reset5h, next.Has5h, next.Used5h, next.Reset5h, newer)
	merge(&current.Has7d, &current.Used7d, &current.Reset7d, next.Has7d, next.Used7d, next.Reset7d, newer)
	if newer {
		current.At = next.At
	}
	return current
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

// withDB needs refreshMu. When the database came back after being
// unavailable, or is used for the first time, every cached history is
// dropped so the next refresh reloads it.
func (m *Manager) withDB(timeout time.Duration, fn func(ctx context.Context, db *sql.DB) error) error {
	return m.host.WithDB(timeout, func(ctx context.Context, db *sql.DB) error {
		if generation := m.host.DBGeneration(); !m.attached || generation != m.generation {
			m.attached, m.generation = true, generation
			m.accounts = map[string]*accountCache{}
			m.weightCache = nil
			m.persisted = persistedState{}
			m.lastPrune = time.Time{}
			m.discovered = false
		}
		return fn(ctx, db)
	})
}

// refreshLocked needs refreshMu. It processes new history, recomputes the
// estimates, saves them and publishes a new snapshot.
func (m *Manager) refreshLocked() error {
	now := m.host.Now()
	accounts := map[string]accountSnapshot{}
	var weights weightsSnapshot
	latestStored := map[string]Observation{}
	m.trackPendingLocked()
	err := m.withDB(m.ProcessBudget+m.DBTimeout, func(ctx context.Context, db *sql.DB) error {
		if !m.discovered {
			// The accounts the database has records of, once per load:
			// usage handling tracks new ones as they appear.
			known, err := knownAccounts(ctx, db, m.Instance)
			if err != nil {
				return err
			}
			for _, index := range known {
				m.trackLocked(index)
			}
			m.discovered = true
		}
		var err error
		params := m.settings.Params
		cutoff := now.Add(-params.retention())
		if m.lastPrune.IsZero() || now.Sub(m.lastPrune) > time.Hour {
			if err := pruneSegments(ctx, db, m.Instance, cutoff); err != nil {
				return err
			}
			m.lastPrune = now
		}
		deadline := time.Now().Add(m.ProcessBudget)
		for _, authIndex := range m.settings.authIndexes() {
			cache := m.accounts[authIndex]
			if cache == nil {
				stream, err := loadStream(ctx, db, m.Instance, authIndex)
				if err != nil {
					return err
				}
				segments, err := loadSegments(ctx, db, m.Instance, authIndex, cutoff)
				if err != nil {
					return err
				}
				buckets, err := loadKBuckets(ctx, db, m.Instance, authIndex, cutoff)
				if err != nil {
					return err
				}
				cache = &accountCache{stream: stream, segments: segments, buckets: buckets}
				m.accounts[authIndex] = cache
			}
			if err := m.advance(ctx, db, authIndex, cache, now.Add(-ReorderHorizon), deadline); err != nil {
				return err
			}
			first := 0
			for first < len(cache.segments) && cache.segments[first].At.Before(cutoff) {
				first++
			}
			cache.segments = cache.segments[first:]
			first = 0
			for first < len(cache.buckets) && cache.buckets[first].At.Before(cutoff) {
				first++
			}
			cache.buckets = cache.buckets[first:]
		}
		if weights, err = m.refreshWeights(ctx, db, now); err != nil {
			return err
		}
		params = m.settings.AccountParams()
		for _, authIndex := range m.settings.authIndexes() {
			cache := m.accounts[authIndex]
			stream := cache.stream
			account := accountSnapshot{k: EstimateK(cache.buckets, params, now), cursor: stream.Cursor, kCursor: stream.K.Cursor,
				excluded: stream.Excluded}
			account.raw = EstimateSegments(cache.segments, params, now)
			account.weighted = account.raw
			if len(weights.factors) > 0 {
				account.weighted = EstimateSegments(ConvertSegments(cache.segments, weights.factors), params, now)
			}
			account.estimate = account.raw
			segments := cache.segments
			if weights.basis == BasisWeighted {
				account.estimate = account.weighted
				segments = ConvertSegments(cache.segments, weights.factors)
			}
			account.trend = dailyTrend(cache.buckets, segments, SegmentExclusions(segments, params, now), m.location())
			accounts[authIndex] = account
		}
		latest, err := readLatest(ctx, db, m.Instance, m.settings, now.Add(-Week-time.Hour))
		if err != nil {
			return err
		}
		latestStored = latest
		return nil
	})
	if err != nil {
		m.recordError(err)
		return err
	}
	m.liveMu.Lock()
	for index, obs := range latestStored {
		m.latest[index] = mergeLatest(m.latest[index], obs)
	}
	m.liveMu.Unlock()
	snap := &snapshot{at: now, settings: m.settings, accounts: accounts, weights: weights}
	m.forecastLocked(snap, now)
	m.snap.Store(snap)
	m.lastRefresh = now
	failed := false
	if err := m.persistLocked(snap, now); err != nil {
		// The snapshot stands; its tables are written again next time.
		failed = true
		m.recordError(err)
	}
	if !failed {
		m.lastError.Store("")
	}
	return nil
}

func (m *Manager) location() *time.Location {
	if m.TrendLocation == nil {
		return time.UTC
	}
	return m.TrendLocation
}

func (m *Manager) recordError(err error) {
	if err.Error() != m.lastError.Load().(string) {
		m.host.Logf("Quota estimation failed: %v", err)
	}
	m.lastError.Store(err.Error())
}

// LastError is the latest estimation error, "" after a clean refresh.
func (m *Manager) LastError() string { return m.lastError.Load().(string) }

// advance processes an account's history in chunks of at most a day until
// until, or until the deadline passes. Segments and k evidence keep separate
// cursors, so k can be rebuilt without touching the segments.
func (m *Manager) advance(ctx context.Context, db *sql.DB, authIndex string, cache *accountCache, until, deadline time.Time) error {
	params := m.settings.AccountParams()
	if cache.stream.Cursor.IsZero() || cache.stream.K.Cursor.IsZero() {
		first, ok, err := firstReport(ctx, db, m.Instance, authIndex)
		if err != nil || !ok {
			return err
		}
		start := first.Add(-time.Nanosecond)
		if cache.stream.Cursor.IsZero() {
			cache.stream.Cursor = start
		}
		if cache.stream.K.Cursor.IsZero() {
			cache.stream.K.Cursor = start
		}
	}
	for {
		from := cache.stream.Cursor
		if cache.stream.K.Cursor.Before(from) {
			from = cache.stream.K.Cursor
		}
		if !from.Before(until) {
			return nil
		}
		end := from.Add(24 * time.Hour)
		if end.After(until) {
			end = until
		}
		observations, err := readObservations(ctx, db, m.Instance, authIndex, from, end)
		if err != nil {
			return err
		}
		next := cache.stream
		var segments []Segment
		if next.Cursor.Before(end) {
			costs, err := readCosts(ctx, db, m.Instance, authIndex, next.Cursor, end)
			if err != nil {
				return err
			}
			segments = next.Advance(observations, costs, end, params)
		}
		var buckets []KBucket
		if next.K.Cursor.Before(end) {
			buckets = BucketEvidence(next.K.Advance(observations, end))
		}
		if err := commitAdvance(ctx, db, m.Instance, authIndex, next, segments, buckets); err != nil {
			return err
		}
		cache.stream = next
		cache.segments = append(cache.segments, segments...)
		cache.buckets = mergeBuckets(cache.buckets, buckets)
		if time.Now().After(deadline) {
			return nil
		}
	}
}

// live returns a copy of the live reports.
func (m *Manager) live() map[string]Observation {
	m.liveMu.RLock()
	defer m.liveMu.RUnlock()
	latest := make(map[string]Observation, len(m.latest))
	for index, obs := range m.latest {
		latest[index] = obs
	}
	return latest
}

func accountStatus(snap *snapshot, latest map[string]Observation, authIndex string, now time.Time) AccountStatus {
	account := snap.accounts[authIndex]
	return ResolveAccount(authIndex, snap.settings.Accounts[authIndex], account.estimate, account.k, latest[authIndex], snap.settings.AccountParams(), now)
}

// current is the latest snapshot, or an empty one built from the settings
// before the first refresh, so reads always describe every tracked account.
func (m *Manager) current() (*snapshot, bool) {
	settings := *m.loaded.Load()
	if snap := m.snap.Load(); snap != nil {
		missing := false
		for index := range settings.Accounts {
			if _, ok := snap.settings.Accounts[index]; !ok {
				missing = true
			}
		}
		if !missing {
			return snap, true
		}
		// Accounts tracked since the snapshot read as not estimated yet.
		merged := *snap
		merged.settings.Accounts = settings.Accounts
		merged.accounts = make(map[string]accountSnapshot, len(settings.Accounts))
		for index := range settings.Accounts {
			account, ok := snap.accounts[index]
			if !ok {
				account = accountSnapshot{k: KEstimate{Basis: KBasisNone}}
			}
			merged.accounts[index] = account
		}
		return &merged, true
	}
	snap := &snapshot{settings: settings, accounts: map[string]accountSnapshot{}}
	for _, index := range settings.authIndexes() {
		snap.accounts[index] = accountSnapshot{k: KEstimate{Basis: KBasisNone}}
	}
	return snap, false
}

// AccountView adds an account's history to its status.
type AccountView struct {
	AccountStatus
	Cursor       time.Time      `json:"cursor,omitzero"`
	KCursor      time.Time      `json:"k_cursor,omitzero"`
	RecentBlocks []BlockSummary `json:"recent_blocks"`
	// Excluded is the cost left out of the segments.
	Excluded ExcludedCost `json:"excluded"`
	// Basis is the basis of the estimate; Weights the multipliers it uses.
	Basis   string            `json:"basis"`
	Weights []EffectiveWeight `json:"weights"`
	// WeightEstimate is the historical estimate of the multipliers.
	WeightEstimate  WeightEstimate  `json:"weight_estimate"`
	WeightsEnabled  bool            `json:"weights_enabled"`
	Forecast        AccountForecast `json:"forecast"`
	ColdStartBlocks int             `json:"cold_start_blocks"`
	SafetyFactor    float64         `json:"safety_factor"`
}

// View describes every tracked account.
type View struct {
	Accounts []AccountView `json:"accounts"`
	Segments int           `json:"stored_segments"`
	Error    string        `json:"error,omitempty"`
	// At is when the snapshot was computed; zero before the first one.
	At time.Time `json:"at,omitzero"`
	// Loaded is false while no estimate snapshot exists: every account is
	// then in cold start.
	Loaded bool `json:"loaded"`
}

// View describes every tracked account from the latest snapshot and the live
// reports. It never refreshes or touches the database.
func (m *Manager) View() View {
	snap, loaded := m.current()
	now := m.host.Now()
	latest := m.live()
	view := View{Accounts: []AccountView{}, Error: m.LastError(), At: snap.at, Loaded: loaded}
	params := snap.settings.AccountParams()
	weights := snap.weights
	for _, index := range snap.settings.authIndexes() {
		cached := snap.accounts[index]
		account := AccountView{AccountStatus: accountStatus(snap, latest, index, now), RecentBlocks: []BlockSummary{},
			Cursor: cached.cursor, KCursor: cached.kCursor, Excluded: cached.excluded, Forecast: cached.forecast,
			ColdStartBlocks: params.ColdStartBlocks, SafetyFactor: params.SafetyFactor, Basis: BasisRaw, Weights: []EffectiveWeight{},
			WeightEstimate: WeightEstimate{Items: []ItemEstimate{}}}
		if weights.basis != "" {
			account.Basis = weights.basis
		}
		if weights.effective != nil {
			account.Weights = weights.effective
		}
		if weights.estimate.Items != nil {
			account.WeightEstimate = weights.estimate
		}
		account.WeightsEnabled = snap.settings.Weights.Enabled
		blocks := cached.estimate.Blocks
		account.RecentBlocks = slices.Clone(blocks[max(0, len(blocks)-20):])
		slices.Reverse(account.RecentBlocks)
		for _, block := range cached.estimate.Blocks {
			view.Segments += block.Segments
		}
		view.Accounts = append(view.Accounts, account)
	}
	return view
}

// readLatest reads the stored reports a refresh merges into the live state;
// tests replace it.
var readLatest = latestObservations

// latestObservations reads each tracked account's latest main window reports
// made since since: the newest report of each window.
func latestObservations(ctx context.Context, db *sql.DB, instance string, settings Settings, since time.Time) (map[string]Observation, error) {
	result := map[string]Observation{}
	for _, index := range settings.authIndexes() {
		var merged Observation
		for _, length := range []time.Duration{FiveHours, Week} {
			point, ok, err := latestWindowPoint(ctx, db, instance, index, length, since)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if length == FiveHours {
				merged.Has5h, merged.Used5h, merged.Reset5h = true, point.Used, point.Reset
			} else {
				merged.Has7d, merged.Used7d, merged.Reset7d = true, point.Used, point.Reset
			}
			if point.At.After(merged.At) {
				merged.At = point.At
			}
		}
		if merged.Has5h || merged.Has7d {
			result[index] = merged
		}
	}
	return result, nil
}
