package ccbilling

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// persistedState remembers what the derived tables last received, so a
// refresh only rewrites what changed. It is dropped whenever the caches are,
// which rewrites everything once.
type persistedState struct {
	blocks  map[string]string
	trend   map[string]TrendDay
	weights string
}

// persistLocked needs refreshMu. It saves a snapshot's results to the
// estimates, blocks, k_trend_daily and model_weights tables in one
// transaction, for downstream readers.
func (m *Manager) persistLocked(snap *snapshot, now time.Time) error {
	latest := m.live()
	next := persistedState{blocks: map[string]string{}, trend: map[string]TrendDay{}}
	return m.withDB(m.DBTimeout, func(ctx context.Context, db *sql.DB) error {
		previous := m.persisted
		err := transact(ctx, db, func(tx *sql.Tx) error {
			for _, index := range snap.settings.authIndexes() {
				quota := accountQuota(snap, latest, index, now)
				if err := saveEstimate(ctx, tx, m.Instance, quota, snap.at); err != nil {
					return err
				}
				account := snap.accounts[index]
				encoded, err := json.Marshal(account.estimate.Blocks)
				if err != nil {
					return err
				}
				next.blocks[index] = string(encoded)
				if previous.blocks[index] != string(encoded) {
					if err := saveBlocks(ctx, tx, m.Instance, index, account.estimate.Blocks); err != nil {
						return err
					}
				}
				for _, day := range account.trend {
					key := index + "\x00" + day.Day
					next.trend[key] = day
					if old, ok := previous.trend[key]; ok && sameTrendDay(old, day) {
						continue
					}
					if err := saveTrendDay(ctx, tx, m.Instance, index, day); err != nil {
						return err
					}
				}
			}
			weights := snap.weights
			row := struct {
				Enabled   bool              `json:"enabled"`
				Basis     string            `json:"basis"`
				Effective []EffectiveWeight `json:"effective"`
				Estimate  WeightEstimate    `json:"estimate"`
				Models    []ModelUsage      `json:"models"`
			}{snap.settings.Weights.Enabled, weights.basis, weights.effective, weights.estimate, weights.models}
			if row.Effective == nil {
				row.Effective = []EffectiveWeight{}
			}
			if row.Estimate.Items == nil {
				row.Estimate.Items = []ItemEstimate{}
			}
			if row.Models == nil {
				row.Models = []ModelUsage{}
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				return err
			}
			next.weights = string(encoded)
			if previous.weights == next.weights {
				return nil
			}
			effective, _ := json.Marshal(row.Effective)
			estimate, _ := json.Marshal(row.Estimate)
			models, _ := json.Marshal(row.Models)
			if _, err := tx.ExecContext(ctx, `INSERT INTO model_weights (instance, enabled, basis, effective, estimate, models, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, now()) ON CONFLICT (instance) DO UPDATE SET enabled = excluded.enabled,
				basis = excluded.basis, effective = excluded.effective, estimate = excluded.estimate, models = excluded.models,
				updated_at = excluded.updated_at`, m.Instance, row.Enabled, row.Basis, string(effective), string(estimate), string(models)); err != nil {
				return fmt.Errorf("Save model weights: %w", err)
			}
			return nil
		})
		if err == nil {
			m.persisted = next
		}
		return err
	})
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func saveEstimate(ctx context.Context, tx *sql.Tx, instance string, quota AccountQuota, at time.Time) error {
	detail, err := json.Marshal(quota)
	if err != nil {
		return err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO estimates (instance, auth_index, tier, state,
		basis, k, k_source, totals_from, safety_factor, used_5h, reset_5h, used_7d, reset_7d, five_hour_total_usd, weekly_total_usd,
		remaining_five_hour_usd, remaining_week_usd, clean_blocks, total_blocks, segments, excluded_segments, ratio, observed_at, estimated_at, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25)
		ON CONFLICT (instance, auth_index) DO UPDATE SET tier = excluded.tier,
		state = excluded.state, basis = excluded.basis,
		k = excluded.k, k_source = excluded.k_source, totals_from = excluded.totals_from, safety_factor = excluded.safety_factor,
		used_5h = excluded.used_5h, reset_5h = excluded.reset_5h, used_7d = excluded.used_7d, reset_7d = excluded.reset_7d,
		five_hour_total_usd = excluded.five_hour_total_usd, weekly_total_usd = excluded.weekly_total_usd,
		remaining_five_hour_usd = excluded.remaining_five_hour_usd, remaining_week_usd = excluded.remaining_week_usd,
		clean_blocks = excluded.clean_blocks, total_blocks = excluded.total_blocks, segments = excluded.segments,
		excluded_segments = excluded.excluded_segments, ratio = excluded.ratio, observed_at = excluded.observed_at,
		estimated_at = excluded.estimated_at, detail = excluded.detail`,
		instance, quota.AuthIndex, quota.Tier, quota.State, quota.Estimate.Basis, quota.K, quota.KSource, quota.TotalsFrom, quota.SafetyFactor, quota.FiveHour.UsedPercent,
		nullableTime(quota.FiveHour.ResetAt), quota.Week.UsedPercent, nullableTime(quota.Week.ResetAt), quota.FiveHour.TotalUSD, quota.Week.TotalUSD,
		quota.FiveHour.RemainingUSD, quota.Week.RemainingUSD, quota.Estimate.CleanBlocks, quota.Estimate.TotalBlocks, quota.Estimate.Segments,
		quota.Estimate.ExcludedSegments, quota.Estimate.Ratio, nullableTime(quota.ObservedAt), at.UTC(), string(detail))
	if err != nil {
		return fmt.Errorf("Save estimate: %w", err)
	}
	return nil
}

func saveBlocks(ctx context.Context, tx *sql.Tx, instance, authIndex string, blocks []BlockSummary) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM blocks WHERE instance = $1 AND auth_index = $2`, instance, authIndex); err != nil {
		return fmt.Errorf("Save blocks: %w", err)
	}
	for _, block := range blocks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO blocks (instance, auth_index, start_at, end_at, segments, excluded, gain, cost_usd,
			clean_gain, clean_cost_usd, reference, reference_mode, clean) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (instance, auth_index, start_at) DO NOTHING`,
			instance, authIndex, block.Start.UTC(), block.End.UTC(), block.Segments, block.Excluded, block.Gain, block.Cost, block.CleanGain,
			block.CleanCost, block.Reference, block.ReferenceMode, block.Clean()); err != nil {
			return fmt.Errorf("Save blocks: %w", err)
		}
	}
	return nil
}

func sameTrendDay(a, b TrendDay) bool {
	sameK := (a.K == nil) == (b.K == nil) && (a.K == nil || *a.K == *b.K)
	return sameK && a.Gain5h == b.Gain5h && a.Gain7d == b.Gain7d && a.Segments == b.Segments &&
		a.ExcludedSegments == b.ExcludedSegments && a.CostUSD == b.CostUSD && a.Gain == b.Gain
}

func saveTrendDay(ctx context.Context, tx *sql.Tx, instance, authIndex string, day TrendDay) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO k_trend_daily (instance, auth_index, day, gain_5h, gain_7d, k, segments, excluded_segments,
		cost_usd, gain, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
		ON CONFLICT (instance, auth_index, day) DO UPDATE SET gain_5h = excluded.gain_5h, gain_7d = excluded.gain_7d, k = excluded.k,
		segments = excluded.segments, excluded_segments = excluded.excluded_segments, cost_usd = excluded.cost_usd, gain = excluded.gain,
		updated_at = excluded.updated_at`,
		instance, authIndex, day.Day, day.Gain5h, day.Gain7d, day.K, day.Segments, day.ExcludedSegments, day.CostUSD, day.Gain)
	if err != nil {
		return fmt.Errorf("Save daily trend: %w", err)
	}
	return nil
}
