package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

func nanos(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UTC().UnixNano()
}

// Request is the compact record of one request on an account.
type Request struct {
	Instance      string
	AuthIndex     string
	Provider      string
	RequestID     string
	RequestedAt   time.Time
	Latency       time.Duration
	Model         string
	UpstreamModel string
	ServiceTier   string
	Stream        bool
	// Token buckets as priced: uncached input, cache reads, cache writes,
	// and billed output, which includes reasoning.
	InputTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	OutputTokens     int64
	ReasoningTokens  int64
	TotalTokens      int64
	Accounting       string
	CostUSD          float64
	Priced           bool
	// MainWindow reports that the request's own quota report carried a
	// window of the provider's main limit.
	MainWindow bool
	Failed     bool
	StatusCode int
}

// CompletedAt is when the request's usage counted against the account.
func (r Request) CompletedAt() time.Time {
	return r.RequestedAt.Add(time.Duration(max(r.Latency.Milliseconds(), 0)) * time.Millisecond)
}

// ReportWindow is one window a quota report named.
type ReportWindow struct {
	LimitKey    string
	Seconds     int64
	UsedPercent float64
	ResetAt     time.Time
}

// QuotaReport is what one response reported about the account's quota. At is
// the request's start, when the provider evaluated it.
type QuotaReport struct {
	Instance         string
	AuthIndex        string
	Provider         string
	RequestID        string
	At               time.Time
	Plan             string
	CreditBalance    *float64
	CreditsUnlimited *bool
	LimitReached     bool
	Windows          []ReportWindow
}

// SafeguardEvent is a request the provider refused for policy reasons.
type SafeguardEvent struct {
	Instance   string
	At         time.Time
	AuthIndex  string
	Provider   string
	Model      string
	Category   string
	Reason     string
	RequestID  string
	StatusCode int
	Stream     bool
}

// RecordWrite stores a request and its quota report, either of which may be
// nil, in one transaction. A request ID seen before is not stored again, so
// a redelivered usage record changes nothing.
func RecordWrite(request *Request, report *QuotaReport) Write {
	return Write{Kind: "usage", Apply: func(ctx context.Context, tx *sql.Tx) error {
		if request != nil {
			r := request
			if _, err := tx.ExecContext(ctx, `INSERT INTO requests (instance, auth_index, provider, request_id, requested_ns, latency_ms, completed_ns,
				model, upstream_model, service_tier, stream, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
				total_tokens, accounting, cost_usd, priced, main_window, failed, status_code)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
				ON CONFLICT (instance, request_id) WHERE request_id <> '' DO NOTHING`,
				r.Instance, r.AuthIndex, r.Provider, r.RequestID, nanos(r.RequestedAt), r.Latency.Milliseconds(), nanos(r.CompletedAt()),
				r.Model, r.UpstreamModel, r.ServiceTier, r.Stream, r.InputTokens, r.CacheReadTokens, r.CacheWriteTokens, r.OutputTokens,
				r.ReasoningTokens, r.TotalTokens, r.Accounting, r.CostUSD, r.Priced, r.MainWindow, r.Failed, r.StatusCode); err != nil {
				return err
			}
		}
		if report != nil {
			q := report
			var id int64
			err := tx.QueryRowContext(ctx, `INSERT INTO quota_reports (instance, auth_index, provider, request_id, at_ns, plan, credit_balance,
				credits_unlimited, limit_reached) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				ON CONFLICT (instance, request_id) WHERE request_id <> '' DO NOTHING RETURNING id`,
				q.Instance, q.AuthIndex, q.Provider, q.RequestID, nanos(q.At), q.Plan, q.CreditBalance, q.CreditsUnlimited, q.LimitReached).Scan(&id)
			switch {
			case err == sql.ErrNoRows:
				return nil
			case err != nil:
				return err
			}
			for _, w := range q.Windows {
				if _, err := tx.ExecContext(ctx, `INSERT INTO quota_report_windows (report_id, instance, auth_index, provider, limit_key, seconds, at_ns,
					used_percent, reset_ns) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT DO NOTHING`,
					id, q.Instance, q.AuthIndex, q.Provider, w.LimitKey, w.Seconds, nanos(q.At), w.UsedPercent, nanos(w.ResetAt)); err != nil {
					return err
				}
			}
		}
		return nil
	}}
}

// SafeguardWrite stores a safeguard event once per request ID.
func SafeguardWrite(event SafeguardEvent) Write {
	return Write{Kind: "safeguard event", Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO safeguard_events (instance, at_ns, auth_index, provider, model, category, reason, request_id,
			status_code, stream) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (instance, request_id) WHERE request_id <> '' DO NOTHING`,
			event.Instance, nanos(event.At), event.AuthIndex, event.Provider, event.Model, event.Category, event.Reason, event.RequestID,
			event.StatusCode, event.Stream)
		return err
	}}
}

// PriceTableWrite keeps the instance's last successfully fetched ratio table.
func PriceTableWrite(instance, sourceURL string, fetchedAt time.Time, models int, body json.RawMessage) Write {
	return Write{Kind: "price table", Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO price_tables (instance, source_url, fetched_at, models, body) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (instance) DO UPDATE SET source_url = excluded.source_url, fetched_at = excluded.fetched_at,
			models = excluded.models, body = excluded.body WHERE price_tables.fetched_at <= excluded.fetched_at`,
			instance, sourceURL, fetchedAt.UTC(), models, string(body))
		return err
	}}
}

// LoadPriceTable reads the instance's stored ratio table; ok is false when
// none is stored.
func (s *Store) LoadPriceTable(instance string) (body []byte, fetchedAt time.Time, ok bool, err error) {
	err = s.WithDB(0, func(ctx context.Context, db *sql.DB) error {
		scanErr := db.QueryRowContext(ctx, `SELECT body, fetched_at FROM price_tables WHERE instance = $1`, instance).Scan(&body, &fetchedAt)
		switch {
		case scanErr == sql.ErrNoRows:
			return nil
		case scanErr != nil:
			return scanErr
		}
		ok = true
		return nil
	})
	return body, fetchedAt, ok, err
}
