package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/sketchain/cpa-quota/internal/pgtest"
	"github.com/sketchain/cpa-quota/internal/store"
)

func openSchema(t *testing.T, dsn, schema string) *sql.DB {
	t.Helper()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = `"` + schema + `"`
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestConcurrentMigrationsRunOnce(t *testing.T) {
	dsn := pgtest.DSN(t)
	schema := pgtest.Schema(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := openSchema(t, dsn, schema)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			errs[i] = store.Migrate(ctx, db, schema)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("instance %d: %v", i, err)
		}
	}
	db := openSchema(t, dsn, schema)
	var version string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil || version != fmt.Sprint(store.SchemaVersion) {
		t.Fatalf("version = %q, %v", version, err)
	}
	// A database from a newer plugin is refused rather than changed.
	if _, err := db.Exec(`UPDATE meta SET value = '99' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background(), db, schema); !errors.Is(err, store.ErrNewerSchema) {
		t.Fatalf("migrating a newer schema: %v", err)
	}
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newStore(t *testing.T, dsn, schema string, maxPending int) (*store.Store, *clock, *[]string) {
	t.Helper()
	c := &clock{now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	var logs []string
	var mu sync.Mutex
	s := store.New(store.Config{DSN: dsn, Schema: schema, Timeout: 2 * time.Second, MaxPending: maxPending, Now: c.Now,
		Logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(format, args...))
		}})
	t.Cleanup(func() { s.Close() })
	return s, c, &logs
}

func request(instance string, n int) *store.Request {
	return &store.Request{Instance: instance, AuthIndex: "a1", Provider: "claude", RequestID: fmt.Sprintf("req-%d", n),
		RequestedAt: time.Date(2026, 11, 1, 0, 0, n, 0, time.UTC), Latency: 1500 * time.Millisecond, Model: "claude-opus-4-8",
		CostUSD: float64(n), Priced: true, Accounting: "complete"}
}

func storedRequests(t *testing.T, db *sql.DB, instance string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT request_id FROM requests WHERE instance = $1 ORDER BY id`, instance)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestFailedWritesQueueInOrderAndFlushWhenTheDatabaseReturns(t *testing.T) {
	proxy, dsn := pgtest.NewProxy(t)
	schema := pgtest.Schema(t)
	s, c, logs := newStore(t, dsn, schema, 5)
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	direct := openSchema(t, pgtest.DSN(t), schema)
	s.Submit(store.RecordWrite(request("i1", 1), nil))
	proxy.Down()
	for n := 2; n <= 8; n++ {
		s.Submit(store.RecordWrite(request("i1", n), nil))
	}
	status := s.Status()
	// The first write after the outage fails and marks the database
	// unavailable; the queue keeps the newest five, dropping 2 and 3.
	if status.Available || status.Pending != 5 || status.Dropped != 2 {
		t.Fatalf("status during the outage = %+v", status)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "dropped the 1 oldest") {
		t.Fatalf("drops were not logged: %v", *logs)
	}
	proxy.Up()
	// Within the retry interval the store does not try again.
	if s.Flush() {
		t.Fatal("flushed before the retry interval passed")
	}
	c.Add(time.Minute)
	s.Submit(store.RecordWrite(request("i1", 9), nil))
	// The new write joins the full queue first, which drops 4 as well.
	if status := s.Status(); !status.Available || status.Pending != 0 || status.Dropped != 3 {
		t.Fatalf("status after recovery = %+v", status)
	}
	got := strings.Join(storedRequests(t, direct, "i1"), ",")
	if want := "req-1,req-5,req-6,req-7,req-8,req-9"; got != want {
		t.Fatalf("stored %s, want %s", got, want)
	}
	// Redelivering a request stores nothing new.
	s.Submit(store.RecordWrite(request("i1", 9), nil))
	if n := len(storedRequests(t, direct, "i1")); n != 6 {
		t.Fatalf("redelivery stored a duplicate: %d rows", n)
	}
}

func TestUnavailableAtStartQueuesUntilConnected(t *testing.T) {
	proxy, dsn := pgtest.NewProxy(t)
	schema := pgtest.Schema(t)
	proxy.Down()
	s, c, _ := newStore(t, dsn, schema, 0)
	if err := s.Open(); err == nil {
		t.Fatal("opened without a database")
	}
	generation := s.Generation()
	s.Submit(store.RecordWrite(request("i1", 1), nil), store.SafeguardWrite(store.SafeguardEvent{Instance: "i1", At: time.Now(), RequestID: "r"}))
	if err := s.WithDB(0, func(context.Context, *sql.DB) error { return nil }); !errors.Is(err, store.ErrUnavailable) {
		t.Fatalf("WithDB while unavailable: %v", err)
	}
	if status := s.Status(); status.Available || status.Pending != 2 || status.LastError == "" {
		t.Fatalf("status = %+v", status)
	}
	proxy.Up()
	c.Add(time.Minute)
	if !s.Flush() || s.Generation() == generation {
		t.Fatalf("not reconnected: %+v", s.Status())
	}
	direct := openSchema(t, pgtest.DSN(t), schema)
	var events int
	if err := direct.QueryRow(`SELECT count(*) FROM safeguard_events`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("safeguard events = %d, %v", events, err)
	}
}

func TestRejectedWritesAreDroppedNotRetried(t *testing.T) {
	dsn := pgtest.DSN(t)
	schema := pgtest.Schema(t)
	s, _, logs := newStore(t, dsn, schema, 0)
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	bad := store.Write{Kind: "bad", Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO requests (instance) VALUES ('i1')`)
		return err
	}}
	s.Submit(bad, store.RecordWrite(request("i1", 1), nil))
	if status := s.Status(); !status.Available || status.Pending != 0 || status.Dropped != 1 {
		t.Fatalf("status = %+v, logs %v", status, *logs)
	}
}

func TestInstancesAreIsolated(t *testing.T) {
	db, schema := pgtest.Open(t)
	s, _, _ := newStore(t, pgtest.DSN(t), schema, 0)
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	balance := 12.5
	for _, instance := range []string{"i1", "i2"} {
		report := &store.QuotaReport{Instance: instance, AuthIndex: "a1", Provider: "codex", RequestID: "req-1", At: time.Now(),
			CreditBalance: &balance, Windows: []store.ReportWindow{{LimitKey: "codex", Seconds: 18000, UsedPercent: 3}}}
		s.Submit(store.RecordWrite(request(instance, 1), report))
	}
	for _, instance := range []string{"i1", "i2"} {
		var requests, windows int
		if err := db.QueryRow(`SELECT (SELECT count(*) FROM requests WHERE instance = $1),
			(SELECT count(*) FROM quota_report_windows WHERE instance = $1)`, instance).Scan(&requests, &windows); err != nil {
			t.Fatal(err)
		}
		if requests != 1 || windows != 1 {
			t.Fatalf("%s: %d requests, %d windows", instance, requests, windows)
		}
	}
	if _, _, ok, err := s.LoadPriceTable("i1"); ok || err != nil {
		t.Fatalf("price table before any: %v %v", ok, err)
	}
	fetched := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s.Submit(store.PriceTableWrite("i1", "https://example.invalid", fetched, 2, []byte(`{"success":true}`)))
	body, at, ok, err := s.LoadPriceTable("i1")
	if err != nil || !ok || !at.Equal(fetched) || !strings.Contains(string(body), "success") {
		t.Fatalf("price table: %s %v %v %v", body, at, ok, err)
	}
	if _, _, ok, _ := s.LoadPriceTable("i2"); ok {
		t.Fatal("price table leaked across instances")
	}
}
