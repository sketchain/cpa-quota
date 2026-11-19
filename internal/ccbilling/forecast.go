package ccbilling

import (
	"context"
	"database/sql"
	"math"
	"slices"
	"time"
)

// Forecast outcomes: when a quantity running down at a steady rate reaches 0.
const (
	// OutcomeETA: it reaches 0 at At, before the window resets.
	OutcomeETA = "eta"
	// OutcomeAfterReset: at this rate it would reach 0 at At, after the
	// window resets, so it does not run out in this window.
	OutcomeAfterReset = "after_reset"
	// OutcomeIdle: nothing is being used, so it never reaches 0.
	OutcomeIdle = "idle"
	// OutcomeReached: it is already 0.
	OutcomeReached = "reached"
	// OutcomeUnknown: no rate is known.
	OutcomeUnknown = "unknown"
)

// Rate sources of a forecast.
const (
	RateReading = "reading"
	RateSpend   = "spend"
	RateNone    = "none"
)

// Default and allowed look-back windows of the consumption rate.
const (
	DefaultForecastWindow5h = time.Hour
	DefaultForecastWindow7d = 24 * time.Hour
	MinForecastWindow5h     = 5 * time.Minute
	MaxForecastWindow5h     = FiveHours
	MinForecastWindow7d     = time.Hour
	MaxForecastWindow7d     = Week
)

// Outcome is when a forecast quantity reaches 0.
type Outcome struct {
	Status string    `json:"status"`
	At     time.Time `json:"at,omitzero"`
}

// Project linearly forecasts when remaining, used up at perHour, reaches 0.
// A zero reset means the window's end is unknown.
func Project(now time.Time, remaining, perHour float64, reset time.Time) Outcome {
	switch {
	case !finite(remaining) || !finite(perHour):
		return Outcome{Status: OutcomeUnknown}
	case remaining <= 0:
		return Outcome{Status: OutcomeReached}
	case perHour <= 0:
		return Outcome{Status: OutcomeIdle}
	}
	hours := remaining / perHour
	if hours > 100*365*24 {
		return Outcome{Status: OutcomeIdle}
	}
	at := now.Add(time.Duration(hours * float64(time.Hour)))
	if !reset.IsZero() && at.After(reset) {
		return Outcome{Status: OutcomeAfterReset, At: at}
	}
	return Outcome{Status: OutcomeETA, At: at}
}

// Velocity is a consumption rate measured over [From, To]: utilization
// points per hour.
type Velocity struct {
	Available bool      `json:"available"`
	PerHour   float64   `json:"per_hour"`
	From      time.Time `json:"from,omitzero"`
	To        time.Time `json:"to,omitzero"`
	// StartUsed and EndUsed are the readings the rate was measured between.
	StartUsed float64 `json:"start_used"`
	EndUsed   float64 `json:"end_used"`
	// RecordedUSD is the account's cost at the price table; BasisUSD the
	// same cost on the estimate's basis. USDPerPoint converts it to points.
	RecordedUSD float64 `json:"recorded_usd"`
	BasisUSD    float64 `json:"basis_usd"`
	USDPerPoint float64 `json:"usd_per_point,omitempty"`
	// Outcome is when the window reaches 100%.
	Outcome Outcome `json:"outcome"`
}

// ReadingVelocity measures an account window's utilization rate over the
// window before now from its reports. points are the reports after
// now−window, before the last report at or before it (when found), and
// current the latest utilization and reset. Only reports of the current
// period count: a period that began within the window starts from 0%.
func ReadingVelocity(points []WindowPoint, before WindowPoint, hasBefore bool, current WindowPoint, length, window time.Duration, now time.Time) Velocity {
	if current.Reset.IsZero() || !current.Reset.After(now) {
		return Velocity{}
	}
	same := func(point WindowPoint) bool { return absDuration(point.Reset.Sub(current.Reset)) <= boundaryJitter }
	from, periodStart := now.Add(-window), current.Reset.Add(-length)
	var baseAt time.Time
	var baseUsed float64
	switch {
	case !periodStart.Before(from):
		baseAt = periodStart
	case hasBefore && same(before):
		baseAt, baseUsed = from, before.Used
	default:
		for _, point := range points {
			if same(point) {
				baseAt, baseUsed = point.At, point.Used
				break
			}
		}
	}
	if baseAt.IsZero() || now.Sub(baseAt) < time.Minute {
		return Velocity{}
	}
	return Velocity{Available: true, PerHour: math.Max(0, current.Used-baseUsed) / now.Sub(baseAt).Hours(), From: baseAt, To: now,
		StartUsed: baseUsed, EndUsed: current.Used}
}

// AccountWindowForecast forecasts one window of an account.
type AccountWindowForecast struct {
	Used    float64   `json:"used"`
	ResetAt time.Time `json:"reset_at,omitzero"`
	Reading Velocity  `json:"reading"`
	Spend   Velocity  `json:"spend"`
	// Source is the rate Outcome uses: readings when known, else spend.
	Source  string  `json:"source"`
	Outcome Outcome `json:"outcome"`
}

// AccountForecast forecasts both windows of an account. Window5h and
// Window7d are the look-back windows the rates were measured over.
type AccountForecast struct {
	At       time.Time             `json:"at,omitzero"`
	Window5h int64                 `json:"window_5h_seconds"`
	Window7d int64                 `json:"window_7d_seconds"`
	FiveHour AccountWindowForecast `json:"five_hour"`
	Week     AccountWindowForecast `json:"week"`
}

func unknownForecast() AccountForecast {
	unknown := AccountWindowForecast{Source: RateNone, Outcome: Outcome{Status: OutcomeUnknown},
		Reading: Velocity{Outcome: Outcome{Status: OutcomeUnknown}}, Spend: Velocity{Outcome: Outcome{Status: OutcomeUnknown}}}
	return AccountForecast{FiveHour: unknown, Week: unknown}
}

// accountHistory is what an account's forecast reads from the database.
type accountHistory struct {
	points  map[time.Duration]accountPoints
	spend5h []SpendRow
	spend7d []SpendRow
}

type accountPoints struct {
	points    []WindowPoint
	before    WindowPoint
	hasBefore bool
}

// forecastLocked needs refreshMu. It projects each account of the snapshot
// from its estimate, the live utilization and the recent history. An account
// whose history cannot be read keeps an unknown forecast.
func (m *Manager) forecastLocked(snap *snapshot, now time.Time) {
	w5, w7 := m.ForecastWindow5h, m.ForecastWindow7d
	if w5 <= 0 {
		w5 = DefaultForecastWindow5h
	}
	if w7 <= 0 {
		w7 = DefaultForecastWindow7d
	}
	latest := m.live()
	for _, index := range snap.settings.authIndexes() {
		account := snap.accounts[index]
		account.forecast = unknownForecast()
		account.forecast.At, account.forecast.Window5h, account.forecast.Window7d = now, int64(w5/time.Second), int64(w7/time.Second)
		source := snap.settings.accountSource(index)
		history := accountHistory{points: map[time.Duration]accountPoints{}}
		err := m.withDB(m.DBTimeout, func(ctx context.Context, db *sql.DB) error {
			var err error
			if history.spend5h, err = readAccountSpend(ctx, db, m.Instance, index, now.Add(-w5), now.Add(time.Nanosecond)); err != nil {
				return err
			}
			if history.spend7d, err = readAccountSpend(ctx, db, m.Instance, index, now.Add(-w7), now.Add(time.Nanosecond)); err != nil {
				return err
			}
			for _, window := range []struct{ length, span time.Duration }{{FiveHours, w5}, {Week, w7}} {
				var entry accountPoints
				from := now.Add(-window.span)
				if entry.before, entry.hasBefore, err = lastWindowPoint(ctx, db, m.Instance, source, index, window.length, from); err != nil {
					return err
				}
				if entry.points, err = readWindowPoints(ctx, db, m.Instance, source, index, window.length, from, now); err != nil {
					return err
				}
				history.points[window.length] = entry
			}
			return nil
		})
		if err != nil {
			m.recordError(err)
			snap.accounts[index] = account
			continue
		}
		status := accountStatus(snap, latest, index, now)
		conversion := NewBasisConversion(providerWeights(snap, status.Provider), nil)
		sum := func(rows []SpendRow) [2]float64 {
			var total [2]float64
			for _, row := range rows {
				total[0] += row.USD
				total[1] += conversion.Convert(row.Model, row.USD)
			}
			return total
		}
		for _, window := range []struct {
			target *AccountWindowForecast
			length time.Duration
			span   time.Duration
			used   float64
			reset  time.Time
			total  float64
			spend  [2]float64
		}{
			{&account.forecast.FiveHour, FiveHours, w5, status.Used5h, status.Reset5h, status.FiveHourTotal, sum(history.spend5h)},
			{&account.forecast.Week, Week, w7, status.Used7d, status.Reset7d, status.WeeklyTotal, sum(history.spend7d)},
		} {
			target := window.target
			target.Used, target.ResetAt = window.used, window.reset
			entry := history.points[window.length]
			target.Reading = ReadingVelocity(entry.points, entry.before, entry.hasBefore,
				WindowPoint{Used: window.used, Reset: window.reset}, window.length, window.span, now)
			reset := window.reset
			if reset.IsZero() || !reset.After(now) {
				// The next window opens with the next request.
				reset = now.Add(window.length)
			}
			if target.Reading.Available {
				target.Reading.Outcome = Project(now, 100-window.used, target.Reading.PerHour, reset)
			} else {
				target.Reading.Outcome = Outcome{Status: OutcomeUnknown}
			}
			target.Spend = Velocity{From: now.Add(-window.span), To: now, RecordedUSD: window.spend[0], BasisUSD: window.spend[1],
				Outcome: Outcome{Status: OutcomeUnknown}}
			if window.total > 0 {
				target.Spend.Available, target.Spend.USDPerPoint = true, window.total/100
				target.Spend.PerHour = window.spend[1] / target.Spend.USDPerPoint / window.span.Hours()
				target.Spend.Outcome = Project(now, 100-window.used, target.Spend.PerHour, reset)
			}
			target.Source, target.Outcome = RateNone, Outcome{Status: OutcomeUnknown}
			switch {
			case target.Reading.Available:
				target.Source, target.Outcome = RateReading, target.Reading.Outcome
			case target.Spend.Available:
				target.Source, target.Outcome = RateSpend, target.Spend.Outcome
			}
		}
		snap.accounts[index] = account
	}
}

// providerWeights is the basis and multipliers of one provider's estimates in
// a snapshot.
func providerWeights(snap *snapshot, provider string) ProviderWeights {
	weights := snap.weights[provider]
	entry := ProviderWeights{Basis: weights.basis, Weights: slices.Clone(weights.effective)}
	if entry.Basis == "" {
		entry.Basis = BasisRaw
	}
	if entry.Weights == nil {
		entry.Weights = []EffectiveWeight{}
	}
	return entry
}
