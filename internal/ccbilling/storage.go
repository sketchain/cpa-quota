package ccbilling

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sketchain/cpa-quota/internal/pricing"
)

// The estimate tables live in the plugin's PostgreSQL schema, which
// internal/store creates and migrates. Every row carries the instance that
// wrote it, so instances sharing the database never read each other's data.
// Event times are stored as Unix nanoseconds, so a rebuilt estimate sees
// exactly the instants the live one did; the tables also carry generated
// timestamptz columns for people reading them.

func nanos(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UTC().UnixNano()
}

func timeAt(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func transact(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("Begin estimate transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("Commit estimate transaction: %w", err)
	}
	return nil
}

func loadStream(ctx context.Context, db *sql.DB, instance, authIndex string) (StreamState, error) {
	var raw []byte
	switch err := db.QueryRowContext(ctx, `SELECT state FROM streams WHERE instance = $1 AND auth_index = $2`, instance, authIndex).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return StreamState{}, nil
	case err != nil:
		return StreamState{}, fmt.Errorf("Read estimate stream: %w", err)
	}
	var state StreamState
	if err := json.Unmarshal(raw, &state); err != nil {
		return StreamState{}, fmt.Errorf("Read estimate stream: %w", err)
	}
	return state, nil
}

func loadSegments(ctx context.Context, db *sql.DB, instance, authIndex string, since time.Time) ([]Segment, error) {
	rows, err := db.QueryContext(ctx, `SELECT block_ns, at_ns, cost_usd, gain, models FROM segments
		WHERE instance = $1 AND auth_index = $2 AND at_ns >= $3 ORDER BY at_ns, id`, instance, authIndex, nanos(since))
	if err != nil {
		return nil, fmt.Errorf("Read segments: %w", err)
	}
	defer rows.Close()
	var segments []Segment
	for rows.Next() {
		var block, at int64
		var models []byte
		var segment Segment
		if err := rows.Scan(&block, &at, &segment.CostUSD, &segment.Gain, &models); err != nil {
			return nil, fmt.Errorf("Read segments: %w", err)
		}
		segment.Block, segment.At = timeAt(block), timeAt(at)
		if len(models) > 0 {
			if err := json.Unmarshal(models, &segment.Models); err != nil {
				return nil, fmt.Errorf("Read segment models: %w", err)
			}
			if len(segment.Models) == 0 {
				segment.Models = nil
			}
		}
		segments = append(segments, segment)
	}
	return segments, rows.Err()
}

// commitAdvance stores an account's new segments with the stream position
// that produced them, so a failure retries the same data.
func commitAdvance(ctx context.Context, db *sql.DB, instance, authIndex string, state StreamState, segments []Segment, buckets []KBucket) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return transact(ctx, db, func(tx *sql.Tx) error {
		if state.restarted {
			// The stored segments measure another window.
			if _, err := tx.ExecContext(ctx, `DELETE FROM segments WHERE instance = $1 AND auth_index = $2`, instance, authIndex); err != nil {
				return fmt.Errorf("Reset segments: %w", err)
			}
		}
		for _, segment := range segments {
			var models any
			if len(segment.Models) > 0 {
				encoded, err := json.Marshal(segment.Models)
				if err != nil {
					return err
				}
				models = string(encoded)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO segments (instance, auth_index, block_ns, at_ns, cost_usd, gain, models)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, instance, authIndex, nanos(segment.Block), nanos(segment.At), segment.CostUSD, segment.Gain, models); err != nil {
				return fmt.Errorf("Save segments: %w", err)
			}
		}
		for _, bucket := range buckets {
			if _, err := tx.ExecContext(ctx, `INSERT INTO k_buckets (instance, auth_index, bucket_ns, gain_5h, gain_7d) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (instance, auth_index, bucket_ns) DO UPDATE
				SET gain_5h = k_buckets.gain_5h + excluded.gain_5h, gain_7d = k_buckets.gain_7d + excluded.gain_7d`,
				instance, authIndex, nanos(bucket.At), bucket.Gain5h, bucket.Gain7d); err != nil {
				return fmt.Errorf("Save k evidence: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO streams (instance, auth_index, state, updated_at) VALUES ($1, $2, $3, now())
			ON CONFLICT (instance, auth_index) DO UPDATE SET state = excluded.state, updated_at = excluded.updated_at`,
			instance, authIndex, string(raw)); err != nil {
			return fmt.Errorf("Save estimate stream: %w", err)
		}
		return nil
	})
}

func pruneSegments(ctx context.Context, db *sql.DB, instance string, before time.Time) error {
	return transact(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM segments WHERE instance = $1 AND at_ns < $2`, instance, nanos(before)); err != nil {
			return fmt.Errorf("Prune segments: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM k_buckets WHERE instance = $1 AND bucket_ns < $2`, instance, nanos(before)); err != nil {
			return fmt.Errorf("Prune k evidence: %w", err)
		}
		return nil
	})
}

func loadKBuckets(ctx context.Context, db *sql.DB, instance, authIndex string, since time.Time) ([]KBucket, error) {
	rows, err := db.QueryContext(ctx, `SELECT bucket_ns, gain_5h, gain_7d FROM k_buckets
		WHERE instance = $1 AND auth_index = $2 AND bucket_ns >= $3 ORDER BY bucket_ns`, instance, authIndex, nanos(since))
	if err != nil {
		return nil, fmt.Errorf("Read k evidence: %w", err)
	}
	defer rows.Close()
	var buckets []KBucket
	for rows.Next() {
		var at int64
		var bucket KBucket
		if err := rows.Scan(&at, &bucket.Gain5h, &bucket.Gain7d); err != nil {
			return nil, fmt.Errorf("Read k evidence: %w", err)
		}
		bucket.At = timeAt(at)
		buckets = append(buckets, bucket)
	}
	return buckets, rows.Err()
}

// resetStreams forgets the instance's segments, k evidence and stream
// positions, so the next refresh rebuilds them from the retained request
// history.
func resetStreams(ctx context.Context, db *sql.DB, instance string) error {
	return transact(ctx, db, func(tx *sql.Tx) error {
		for _, table := range []string{"segments", "streams", "k_buckets", "blocks"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE instance = $1`, instance); err != nil {
				return fmt.Errorf("Reset segments: %w", err)
			}
		}
		return nil
	})
}

// knownAccounts lists the accounts of a provider the instance has requests
// or quota reports of.
func knownAccounts(ctx context.Context, db *sql.DB, instance, provider string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT auth_index FROM requests WHERE instance = $1 AND provider = $2
		UNION SELECT auth_index FROM quota_reports WHERE instance = $1 AND provider = $2`, instance, provider)
	if err != nil {
		return nil, fmt.Errorf("Read accounts: %w", err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var index string
		if err := rows.Scan(&index); err != nil {
			return nil, fmt.Errorf("Read accounts: %w", err)
		}
		result = append(result, index)
	}
	return result, rows.Err()
}

// firstReport is the time of an account's earliest main window report.
func firstReport(ctx context.Context, db *sql.DB, instance string, source QuotaSource, authIndex string) (time.Time, bool, error) {
	var first sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT min(at_ns) FROM quota_report_windows WHERE instance = $1 AND auth_index = $2 AND provider = $3`,
		instance, authIndex, source.Provider).Scan(&first); err != nil {
		return time.Time{}, false, fmt.Errorf("Read quota reports: %w", err)
	}
	if !first.Valid {
		return time.Time{}, false, nil
	}
	return timeAt(first.Int64), true, nil
}

// readObservations returns an account's main window reports made after from
// and no later than to, one per request, oldest first, with the credit
// balance each report named.
func readObservations(ctx context.Context, db *sql.DB, instance string, source QuotaSource, authIndex string, from, to time.Time) ([]Observation, error) {
	rows, err := db.QueryContext(ctx, `SELECT w.report_id, w.at_ns, w.seconds, w.used_percent, w.reset_ns, r.credit_balance
		FROM quota_report_windows w JOIN quota_reports r ON r.id = w.report_id
		WHERE w.instance = $1 AND w.auth_index = $2 AND w.provider = $3 AND w.limit_key = $4 AND w.seconds IN ($5, $6)
		AND w.at_ns > $7 AND w.at_ns <= $8
		ORDER BY w.at_ns, w.report_id`,
		instance, authIndex, source.Provider, source.LimitKey, int64(FiveHours/time.Second), int64(Week/time.Second), nanos(from), nanos(to))
	if err != nil {
		return nil, fmt.Errorf("Read quota reports: %w", err)
	}
	defer rows.Close()
	var observations []Observation
	lastID := int64(-1)
	for rows.Next() {
		var id, at, seconds, resetAt int64
		var used float64
		var credits sql.NullFloat64
		if err := rows.Scan(&id, &at, &seconds, &used, &resetAt, &credits); err != nil {
			return nil, fmt.Errorf("Read quota reports: %w", err)
		}
		if id != lastID || len(observations) == 0 {
			observations = append(observations, Observation{At: timeAt(at)})
			lastID = id
			if credits.Valid && finite(credits.Float64) {
				observations[len(observations)-1].HasCredits, observations[len(observations)-1].Credits = true, credits.Float64
			}
		}
		obs := &observations[len(observations)-1]
		if seconds == int64(FiveHours/time.Second) {
			obs.Has5h, obs.Used5h, obs.Reset5h = true, used, timeAt(resetAt)
		} else {
			obs.Has7d, obs.Used7d, obs.Reset7d = true, used, timeAt(resetAt)
		}
	}
	return observations, rows.Err()
}

// maxRequestDuration bounds how long before its completion a request began.
const maxRequestDuration = 2 * time.Hour

// readCosts returns the costs, in USD, of an account's requests completed
// after from and no later than to. Requests whose cost stays out of the
// segments are marked with the reason; each request is judged by its own
// record and quota report only.
func readCosts(ctx context.Context, db *sql.DB, instance string, rules SegmentRules, authIndex string, from, to time.Time) ([]CostEvent, error) {
	rows, err := db.QueryContext(ctx, `SELECT requested_ns, completed_ns, cost_usd, model, priced, main_window, accounting
		FROM requests
		WHERE instance = $1 AND auth_index = $2 AND requested_ns > $3 AND requested_ns <= $4 AND completed_ns > $5 AND completed_ns <= $4
		ORDER BY requested_ns, id`, instance, authIndex, nanos(from.Add(-maxRequestDuration)), nanos(to), nanos(from))
	if err != nil {
		return nil, fmt.Errorf("Read request costs: %w", err)
	}
	defer rows.Close()
	var costs []CostEvent
	for rows.Next() {
		var at, completed int64
		var cost float64
		var model, accounting string
		var priced, mainWindow bool
		if err := rows.Scan(&at, &completed, &cost, &model, &priced, &mainWindow, &accounting); err != nil {
			return nil, fmt.Errorf("Read request costs: %w", err)
		}
		event := CostEvent{At: timeAt(completed), USD: cost, Model: NormalizeModel(model)}
		switch {
		case rules.MainWindowOnly && !mainWindow:
			// Another pool's request leaves only its own cost out, priced
			// or not.
			event.Excluded = ExcludedOtherLimit
		case !costKnown(priced, accounting):
			event.Excluded, event.Unpriced = ExcludedUnpriced, !priced
		}
		costs = append(costs, event)
	}
	return costs, rows.Err()
}

// costKnown reports whether a request's recorded cost is its real cost:
// its usage is complete and its model priced, or it reported no usage at
// all, which costs nothing whatever the model.
func costKnown(priced bool, accounting string) bool {
	switch accounting {
	case pricing.QualityNone:
		return true
	case pricing.QualityComplete:
		return priced
	}
	return false
}

// NormalizeModel is the form model names are compared in.
func NormalizeModel(model string) string { return strings.ToLower(strings.TrimSpace(model)) }

func segmentStats(ctx context.Context, db *sql.DB, instance string) (int, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM segments WHERE instance = $1`, instance).Scan(&count)
	return count, err
}
