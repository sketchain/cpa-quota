package ccbilling

import (
	"context"
	"database/sql"
	"time"
)

// Exclusion modes of a trend's segments.
const (
	// ExclusionEstimate marks the segments the estimate's filter excluded.
	ExclusionEstimate = "estimate"
	// ExclusionRaw means no estimate exists, so no segment is marked.
	ExclusionRaw = "raw"
)

// TrendHour sums an account's 1% segments that ended within one hour, like
// the hourly k buckets. CostUSD and Gain cover the kept segments only.
type TrendHour struct {
	At       time.Time `json:"at"`
	Segments int       `json:"segments"`
	Excluded int       `json:"excluded"`
	CostUSD  float64   `json:"cost_usd"`
	Gain     float64   `json:"gain"`
}

// Trend is an account's retained k evidence and segments by hour, so the page
// can split them into days in the viewer's time zone.
type Trend struct {
	AuthIndex string `json:"auth_index"`
	// Loaded is false while no estimate snapshot exists.
	Loaded bool      `json:"loaded"`
	From   time.Time `json:"from"`
	At     time.Time `json:"at"`
	// RetentionDays and KMinWeeklyPoints are the parameters the data was
	// retained and judged with.
	RetentionDays    int     `json:"retention_days"`
	KMinWeeklyPoints float64 `json:"k_min_weekly_points"`
	// K is the account's effective k and KSource where it comes from;
	// NeedsK reports that no k is set or estimated.
	K         *float64 `json:"k,omitempty"`
	KSource   string   `json:"k_source"`
	NeedsK    bool     `json:"needs_k"`
	Exclusion string   `json:"exclusion"`
	// Basis is the estimate's basis; on the weighted basis segment costs
	// are in baseline USD.
	Basis    string      `json:"basis"`
	Buckets  []KBucket   `json:"buckets"`
	Segments []TrendHour `json:"segment_hours"`
}

// Trend reads an account's retained hourly k buckets and segments. Segments
// are summed per hour, marking those the estimate excluded. It reads
// the latest snapshot and the database only: it never refreshes, migrates or
// writes, and does not wait for a running refresh. The second result is false
// when the account is not configured.
func (m *Manager) Trend(authIndex string) (Trend, bool, error) {
	snap := m.snap.Load()
	var settings Settings
	if snap != nil {
		settings = snap.settings
	} else {
		settings = *m.loaded.Load()
	}
	if _, ok := settings.Accounts[authIndex]; !ok {
		return Trend{}, false, nil
	}
	params := settings.AccountParams()
	now := m.host.Now()
	// The snapshot's cutoff selects exactly the segments its estimate used.
	asOf := now
	if snap != nil {
		asOf = snap.at
	}
	trend := Trend{AuthIndex: authIndex, Loaded: snap != nil, From: asOf.Add(-params.retention()), At: now,
		RetentionDays: params.SegmentRetentionDays, KMinWeeklyPoints: params.KMinWeeklyPoints, Exclusion: ExclusionRaw, Basis: BasisRaw,
		Buckets: []KBucket{}, Segments: []TrendHour{}}
	var segments []Segment
	// The host's database, not withDB: that would drop the refresh's caches
	// and needs the refresh lock.
	err := m.host.WithDB(m.DBTimeout, func(ctx context.Context, db *sql.DB) error {
		var err error
		if segments, err = loadSegments(ctx, db, m.Instance, authIndex, trend.From); err != nil {
			return err
		}
		buckets, err := loadKBuckets(ctx, db, m.Instance, authIndex, trend.From)
		if buckets != nil {
			trend.Buckets = buckets
		}
		return err
	})
	if err != nil {
		return Trend{}, true, err
	}
	var excluded []bool
	var status AccountStatus
	if snap != nil {
		if weights := snap.weights; weights.basis == BasisWeighted {
			// The estimate filtered the converted costs.
			segments = ConvertSegments(segments, weights.factors)
			trend.Basis = BasisWeighted
		}
		excluded = SegmentExclusions(segments, params, snap.at)
		trend.Exclusion = ExclusionEstimate
		status = accountStatus(snap, m.live(), authIndex, now)
	} else {
		status = ResolveAccount(authIndex, settings.Accounts[authIndex], Estimate{}, KEstimate{Basis: KBasisNone}, Observation{}, params, now)
	}
	trend.K, trend.KSource, trend.NeedsK = status.K, status.KSource, status.NeedsK
	for i, segment := range segments {
		hour := segment.At.UTC().Truncate(kBucketWidth)
		if n := len(trend.Segments); n == 0 || !trend.Segments[n-1].At.Equal(hour) {
			trend.Segments = append(trend.Segments, TrendHour{At: hour})
		}
		item := &trend.Segments[len(trend.Segments)-1]
		if excluded != nil && excluded[i] {
			item.Excluded++
			continue
		}
		item.Segments++
		item.CostUSD += segment.CostUSD
		item.Gain += segment.Gain
	}
	return trend, true, nil
}

// TrendDay sums an account's k evidence and segments of one day. K is the
// day's 5-hour gain over its 7-day gain, nil when the day has no 7-day gain;
// CostUSD and Gain cover the segments the estimate kept.
type TrendDay struct {
	Day              string   `json:"day"`
	Gain5h           float64  `json:"gain_5h"`
	Gain7d           float64  `json:"gain_7d"`
	K                *float64 `json:"k,omitempty"`
	Segments         int      `json:"segments"`
	ExcludedSegments int      `json:"excluded_segments"`
	CostUSD          float64  `json:"cost_usd"`
	Gain             float64  `json:"gain"`
}

// dailyTrend splits an account's hourly k buckets and segments into days of
// location, oldest first. excluded marks the segments the estimate excluded.
func dailyTrend(buckets []KBucket, segments []Segment, excluded []bool, location *time.Location) []TrendDay {
	days := map[string]*TrendDay{}
	day := func(at time.Time) *TrendDay {
		key := at.In(location).Format(time.DateOnly)
		entry := days[key]
		if entry == nil {
			entry = &TrendDay{Day: key}
			days[key] = entry
		}
		return entry
	}
	for _, bucket := range buckets {
		entry := day(bucket.At)
		entry.Gain5h += bucket.Gain5h
		entry.Gain7d += bucket.Gain7d
	}
	for i, segment := range segments {
		entry := day(segment.At)
		if i < len(excluded) && excluded[i] {
			entry.ExcludedSegments++
			continue
		}
		entry.Segments++
		entry.CostUSD += segment.CostUSD
		entry.Gain += segment.Gain
	}
	result := make([]TrendDay, 0, len(days))
	for _, key := range sortedKeys(days) {
		entry := *days[key]
		if entry.Gain7d > gainEpsilon && entry.Gain5h > gainEpsilon {
			k := entry.Gain5h / entry.Gain7d
			entry.K = &k
		}
		result = append(result, entry)
	}
	return result
}

// TrendDays is an account's daily trend as of the latest snapshot; false when
// the account is not tracked or no snapshot exists.
func (m *Manager) TrendDays(authIndex string) ([]TrendDay, bool) {
	snap := m.snap.Load()
	if snap == nil {
		return nil, false
	}
	account, ok := snap.accounts[authIndex]
	return account.trend, ok
}
