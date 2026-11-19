// Package pgtest gives tests their own PostgreSQL schema.
//
// Tests that need the database read its connection string from
// CPA_QUOTA_TEST_DSN and are skipped when it is unset. CI sets it to a
// postgres service container.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/sketchain/cpa-quota/internal/store"
)

// EnvDSN names the environment variable holding the test database.
const EnvDSN = "CPA_QUOTA_TEST_DSN"

// DSN returns the test database's connection string, skipping the test when
// none is configured.
func DSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("%s is not set", EnvDSN)
	}
	return dsn
}

// Schema returns a fresh schema name, dropped when the test ends.
func Schema(t testing.TB) string {
	t.Helper()
	dsn := DSN(t)
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	schema := "cq_test_" + hex.EncodeToString(random)
	t.Cleanup(func() {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
	})
	return schema
}

// Open returns a database handle on a fresh, migrated schema.
func Open(t testing.TB) (*sql.DB, string) {
	t.Helper()
	dsn := DSN(t)
	schema := Schema(t)
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["search_path"] = `"` + schema + `"`
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, db, schema); err != nil {
		t.Fatal(err)
	}
	return db, schema
}
