package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// SchemaVersion is the table format Migrate brings the database to.
const SchemaVersion = 1

// migrationLock is the advisory lock key that serializes migrations of every
// instance sharing the database ("cpa-quota" in ASCII).
const migrationLock int64 = 0x6370612d71756f74

// generatedAt renders a Unix-nanosecond column as a readable timestamp. Zero
// means unknown and reads as NULL.
func generatedAt(column string) string {
	return fmt.Sprintf(`timestamptz GENERATED ALWAYS AS (CASE WHEN %[1]s = 0 THEN NULL ELSE to_timestamp(%[1]s::double precision / 1e9) END) STORED`, column)
}

// migrations[i] moves the schema from version i to i+1. They run inside one
// transaction holding the migration lock, in the plugin's own schema.
var migrations = []string{
	// Version 1.
	`CREATE TABLE requests (
		id                 bigserial PRIMARY KEY,
		instance           text             NOT NULL,
		auth_index         text             NOT NULL,
		request_id         text             NOT NULL DEFAULT '',
		requested_ns       bigint           NOT NULL,
		latency_ms         bigint           NOT NULL DEFAULT 0,
		completed_ns       bigint           NOT NULL,
		requested_at       ` + generatedAt("requested_ns") + `,
		model              text             NOT NULL DEFAULT '',
		upstream_model     text             NOT NULL DEFAULT '',
		service_tier       text             NOT NULL DEFAULT '',
		stream             boolean          NOT NULL DEFAULT false,
		input_tokens       bigint           NOT NULL DEFAULT 0,
		cache_read_tokens  bigint           NOT NULL DEFAULT 0,
		cache_write_tokens bigint           NOT NULL DEFAULT 0,
		output_tokens      bigint           NOT NULL DEFAULT 0,
		reasoning_tokens   bigint           NOT NULL DEFAULT 0,
		total_tokens       bigint           NOT NULL DEFAULT 0,
		accounting         text             NOT NULL DEFAULT '',
		cost_usd           double precision NOT NULL DEFAULT 0,
		priced             boolean          NOT NULL DEFAULT false,
		failed             boolean          NOT NULL DEFAULT false,
		status_code        integer          NOT NULL DEFAULT 0
	);
	CREATE UNIQUE INDEX requests_request_id ON requests (instance, request_id) WHERE request_id <> '';
	CREATE INDEX requests_account_time ON requests (instance, auth_index, requested_ns);

	CREATE TABLE quota_reports (
		id         bigserial PRIMARY KEY,
		instance   text   NOT NULL,
		auth_index text   NOT NULL,
		request_id text   NOT NULL DEFAULT '',
		at_ns      bigint NOT NULL,
		at         ` + generatedAt("at_ns") + `
	);
	CREATE UNIQUE INDEX quota_reports_request_id ON quota_reports (instance, request_id) WHERE request_id <> '';
	CREATE INDEX quota_reports_account_time ON quota_reports (instance, auth_index, at_ns);

	CREATE TABLE quota_report_windows (
		report_id    bigint           NOT NULL REFERENCES quota_reports (id) ON DELETE CASCADE,
		instance     text             NOT NULL,
		auth_index   text             NOT NULL,
		limit_key    text             NOT NULL,
		seconds      bigint           NOT NULL,
		at_ns        bigint           NOT NULL,
		at           ` + generatedAt("at_ns") + `,
		used_percent double precision NOT NULL,
		reset_ns     bigint           NOT NULL DEFAULT 0,
		reset_at     ` + generatedAt("reset_ns") + `,
		PRIMARY KEY (report_id, limit_key, seconds)
	);
	CREATE INDEX quota_report_windows_lookup ON quota_report_windows (instance, auth_index, limit_key, seconds, at_ns);

	CREATE TABLE streams (
		instance   text        NOT NULL,
		auth_index text        NOT NULL,
		state      jsonb       NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (instance, auth_index)
	);

	CREATE TABLE segments (
		id         bigserial PRIMARY KEY,
		instance   text             NOT NULL,
		auth_index text             NOT NULL,
		block_ns   bigint           NOT NULL,
		block_at   ` + generatedAt("block_ns") + `,
		at_ns      bigint           NOT NULL,
		at         ` + generatedAt("at_ns") + `,
		cost_usd   double precision NOT NULL,
		gain       double precision NOT NULL,
		models     jsonb
	);
	CREATE INDEX segments_account_time ON segments (instance, auth_index, at_ns);

	CREATE TABLE k_buckets (
		instance   text             NOT NULL,
		auth_index text             NOT NULL,
		bucket_ns  bigint           NOT NULL,
		bucket_at  ` + generatedAt("bucket_ns") + `,
		gain_5h    double precision NOT NULL,
		gain_7d    double precision NOT NULL,
		PRIMARY KEY (instance, auth_index, bucket_ns)
	);

	CREATE TABLE blocks (
		instance       text             NOT NULL,
		auth_index     text             NOT NULL,
		start_at       timestamptz      NOT NULL,
		end_at         timestamptz      NOT NULL,
		segments       integer          NOT NULL,
		excluded       integer          NOT NULL,
		gain           double precision NOT NULL,
		cost_usd       double precision NOT NULL,
		clean_gain     double precision NOT NULL,
		clean_cost_usd double precision NOT NULL,
		reference      double precision NOT NULL,
		reference_mode text             NOT NULL,
		clean          boolean          NOT NULL,
		PRIMARY KEY (instance, auth_index, start_at)
	);

	CREATE TABLE estimates (
		instance                text             NOT NULL,
		auth_index              text             NOT NULL,
		tier                    text             NOT NULL,
		state                   text             NOT NULL,
		basis                   text             NOT NULL,
		k                       double precision,
		k_source                text             NOT NULL,
		totals_from             text             NOT NULL,
		safety_factor           double precision NOT NULL,
		used_5h                 double precision NOT NULL,
		reset_5h                timestamptz,
		used_7d                 double precision NOT NULL,
		reset_7d                timestamptz,
		five_hour_total_usd     double precision NOT NULL,
		weekly_total_usd        double precision NOT NULL,
		remaining_five_hour_usd double precision NOT NULL,
		remaining_week_usd      double precision NOT NULL,
		clean_blocks            integer          NOT NULL,
		total_blocks            integer          NOT NULL,
		segments                integer          NOT NULL,
		excluded_segments       integer          NOT NULL,
		ratio                   double precision NOT NULL,
		observed_at             timestamptz,
		estimated_at            timestamptz      NOT NULL,
		detail                  jsonb            NOT NULL,
		PRIMARY KEY (instance, auth_index)
	);

	CREATE TABLE k_trend_daily (
		instance          text             NOT NULL,
		auth_index        text             NOT NULL,
		day               date             NOT NULL,
		gain_5h           double precision NOT NULL,
		gain_7d           double precision NOT NULL,
		k                 double precision,
		segments          integer          NOT NULL,
		excluded_segments integer          NOT NULL,
		cost_usd          double precision NOT NULL,
		gain              double precision NOT NULL,
		updated_at        timestamptz      NOT NULL DEFAULT now(),
		PRIMARY KEY (instance, auth_index, day)
	);

	CREATE TABLE model_weights (
		instance   text        PRIMARY KEY,
		enabled    boolean     NOT NULL,
		basis      text        NOT NULL,
		effective  jsonb       NOT NULL,
		estimate   jsonb       NOT NULL,
		models     jsonb       NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now()
	);

	CREATE TABLE calibrations (
		id         bigserial PRIMARY KEY,
		instance   text   NOT NULL,
		auth_index text   NOT NULL,
		item       jsonb  NOT NULL,
		started_ns bigint NOT NULL,
		started_at ` + generatedAt("started_ns") + `,
		ended_ns   bigint NOT NULL DEFAULT 0,
		ended_at   ` + generatedAt("ended_ns") + `,
		result     jsonb
	);
	CREATE INDEX calibrations_instance ON calibrations (instance, started_ns);

	CREATE TABLE price_tables (
		instance   text        PRIMARY KEY,
		source_url text        NOT NULL,
		fetched_at timestamptz NOT NULL,
		models     integer     NOT NULL,
		body       jsonb       NOT NULL
	);

	CREATE TABLE safeguard_events (
		id          bigserial PRIMARY KEY,
		instance    text    NOT NULL,
		at_ns       bigint  NOT NULL,
		at          ` + generatedAt("at_ns") + `,
		auth_index  text    NOT NULL DEFAULT '',
		model       text    NOT NULL DEFAULT '',
		category    text    NOT NULL DEFAULT '',
		reason      text    NOT NULL DEFAULT '',
		request_id  text    NOT NULL DEFAULT '',
		status_code integer NOT NULL DEFAULT 0,
		stream      boolean NOT NULL DEFAULT false
	);
	CREATE UNIQUE INDEX safeguard_events_request_id ON safeguard_events (instance, request_id) WHERE request_id <> '';
	CREATE INDEX safeguard_events_time ON safeguard_events (instance, at_ns);`,
}

// ErrNewerSchema reports a database migrated by a newer plugin version.
var ErrNewerSchema = errors.New("the database uses a newer cpa-quota schema; upgrade the plugin")

// Migrate creates the plugin's schema and brings its tables to
// SchemaVersion. Instances starting together serialize on an advisory
// transaction lock, so each migration runs exactly once.
func Migrate(ctx context.Context, db *sql.DB, schema string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	quoted := quoteIdent(schema)
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+quoted); err != nil {
		return fmt.Errorf("create schema %s: %w", schema, err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+quoted); err != nil {
		return fmt.Errorf("select schema %s: %w", schema, err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key text PRIMARY KEY, value text NOT NULL)`); err != nil {
		return fmt.Errorf("create meta table: %w", err)
	}
	version := 0
	var raw string
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read schema version: %w", err)
	default:
		if version, err = strconv.Atoi(raw); err != nil {
			return fmt.Errorf("invalid schema version %q", raw)
		}
	}
	if version > SchemaVersion {
		return ErrNewerSchema
	}
	if version == SchemaVersion {
		return tx.Commit()
	}
	for ; version < SchemaVersion; version++ {
		if _, err := tx.ExecContext(ctx, migrations[version]); err != nil {
			return fmt.Errorf("migrate to schema version %d: %w", version+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('schema_version', $1)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, strconv.Itoa(SchemaVersion)); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	return tx.Commit()
}

func quoteIdent(name string) string {
	out := []byte{'"'}
	for i := 0; i < len(name); i++ {
		if name[i] == '"' {
			out = append(out, '"')
		}
		out = append(out, name[i])
	}
	return string(append(out, '"'))
}
