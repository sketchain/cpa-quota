// Package store owns the plugin's PostgreSQL connection: it migrates the
// schema, writes the facts usage.handle collects, and keeps writes that fail
// in an ordered in-memory queue until the database accepts them again.
//
// Nothing here starts a goroutine or timer. Reconnection and queued writes
// happen synchronously within the host calls that use the store; database/sql
// and the pgx driver keep their own internal goroutines.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// MaxPending is the default bound of the writes kept in memory while the
// database refuses them.
const MaxPending = 10000

// Config configures a Store.
type Config struct {
	DSN    string
	Schema string
	// Timeout bounds connecting and each database operation.
	Timeout time.Duration
	// RetryInterval is the least time between attempts while the database
	// is unavailable.
	RetryInterval time.Duration
	// MaxPending bounds the queued writes; zero means MaxPending.
	MaxPending int
	Now        func() time.Time
	Logf       func(format string, args ...any)
}

// Write is one queued unit of work, applied in its own transaction.
type Write struct {
	// Kind names the write in logs.
	Kind  string
	Apply func(ctx context.Context, tx *sql.Tx) error
}

// Status describes the database connection.
type Status struct {
	// Available is false while the database could not be reached or
	// refused the last operation; LastError says why.
	Available bool      `json:"available"`
	Connected bool      `json:"connected"`
	LastError string    `json:"last_error,omitempty"`
	FailedAt  time.Time `json:"failed_at,omitzero"`
	// Pending writes wait in memory; Dropped counts those discarded since
	// the plugin started, because the queue was full or the database
	// rejected them permanently.
	Pending       int    `json:"pending_writes"`
	Dropped       int64  `json:"dropped_writes"`
	SchemaVersion int    `json:"schema_version"`
	Generation    uint64 `json:"-"`
}

// Store is safe for concurrent use.
type Store struct {
	cfg Config

	mu sync.Mutex
	db *sql.DB
	// available is false after a failure until an attempt succeeds again;
	// attempts are at most one per RetryInterval meanwhile.
	available   bool
	lastAttempt time.Time
	lastErr     string
	failedAt    time.Time
	// generation increases whenever the database becomes available after
	// being unavailable, so readers reload what they cached from it.
	generation uint64
	pending    []Write
	dropped    int64

	// writeMu serializes writers, so queued writes keep their order.
	writeMu sync.Mutex
}

// New returns a store that has not connected yet.
func New(cfg Config) *Store {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = time.Minute
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = MaxPending
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if strings.TrimSpace(cfg.Schema) == "" {
		cfg.Schema = "cpa_quota"
	}
	return &Store{cfg: cfg}
}

// Open connects and migrates now, regardless of the retry interval.
func (s *Store) Open() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connectLocked()
}

// Close releases the connection pool.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db, s.available = nil, false
	return err
}

func (s *Store) connectLocked() error {
	s.lastAttempt = s.cfg.Now()
	if s.db == nil {
		db, err := openDB(s.cfg)
		if err != nil {
			return s.failLocked(err)
		}
		s.db = db
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*s.cfg.Timeout)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		return s.failLocked(fmt.Errorf("connect to PostgreSQL: %w", err))
	}
	if err := Migrate(ctx, s.db, s.cfg.Schema); err != nil {
		return s.failLocked(err)
	}
	if !s.available {
		s.generation++
		if s.lastErr != "" {
			s.cfg.Logf("PostgreSQL is available again")
		}
	}
	s.available, s.lastErr, s.failedAt = true, "", time.Time{}
	return nil
}

func openDB(cfg Config) (*sql.DB, error) {
	config, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres_dsn: %w", redactDSNError(err))
	}
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["search_path"] = quoteIdent(cfg.Schema)
	config.RuntimeParams["application_name"] = "cpa-quota"
	// The server cancels a statement that outlives the timeout even when the
	// client side is stuck.
	config.RuntimeParams["statement_timeout"] = fmt.Sprint(int64((2 * cfg.Timeout) / time.Millisecond))
	if config.ConnectTimeout <= 0 || config.ConnectTimeout > cfg.Timeout {
		config.ConnectTimeout = cfg.Timeout
	}
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(time.Hour)
	return db, nil
}

// redactDSNError keeps a DSN parse error from echoing the password.
func redactDSNError(err error) error {
	var parseErr *pgconn.ParseConfigError
	if errors.As(err, &parseErr) {
		return errors.New("invalid connection string")
	}
	return err
}

func (s *Store) failLocked(err error) error {
	if s.available || s.lastErr != err.Error() {
		s.cfg.Logf("PostgreSQL unavailable: %v", err)
	}
	s.available, s.lastErr, s.failedAt = false, err.Error(), s.cfg.Now()
	return err
}

// ready reports whether the database may be used now, reconnecting when it
// was unavailable and the retry interval has passed.
func (s *Store) ready() (*sql.DB, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.available {
		if s.cfg.Now().Sub(s.lastAttempt) < s.cfg.RetryInterval {
			return nil, false
		}
		if s.connectLocked() != nil {
			return nil, false
		}
	}
	return s.db, true
}

// fail marks the database unavailable after an operation failed.
func (s *Store) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAttempt = s.cfg.Now()
	_ = s.failLocked(err)
}

// Status reports the connection state.
func (s *Store) Status() Status {
	s.mu.Lock()
	status := Status{Available: s.available, Connected: s.db != nil && s.available, LastError: s.lastErr, FailedAt: s.failedAt,
		Pending: len(s.pending), Dropped: s.dropped, Generation: s.generation}
	s.mu.Unlock()
	if status.Available {
		status.SchemaVersion = SchemaVersion
	}
	return status
}

// Pending reports how many writes wait in memory.
func (s *Store) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// Submit applies the queued writes in order, then the new ones. Whatever the
// database does not accept stays queued, oldest first; beyond MaxPending
// the oldest are dropped and logged.
func (s *Store) Submit(writes ...Write) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	s.pending = append(s.pending, writes...)
	if over := len(s.pending) - s.cfg.MaxPending; over > 0 {
		s.pending = append([]Write(nil), s.pending[over:]...)
		s.dropped += int64(over)
		s.cfg.Logf("Pending database writes exceeded %d; dropped the %d oldest", s.cfg.MaxPending, over)
	}
	s.mu.Unlock()
	s.flushLocked()
}

// Flush applies the queued writes now, as far as the database accepts
// them, and reports whether none remain.
func (s *Store) Flush() bool {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.flushLocked()
}

func (s *Store) flushLocked() bool {
	for {
		s.mu.Lock()
		if len(s.pending) == 0 {
			s.mu.Unlock()
			return true
		}
		next := s.pending[0]
		s.mu.Unlock()
		db, ok := s.ready()
		if !ok {
			return false
		}
		err := s.apply(db, next)
		switch {
		case err == nil:
		case permanent(err):
			// Retrying cannot help a write the database rejects as invalid.
			s.cfg.Logf("Dropped a %s write the database rejected: %v", next.Kind, err)
			s.mu.Lock()
			s.dropped++
			s.mu.Unlock()
		default:
			s.fail(fmt.Errorf("write %s: %w", next.Kind, err))
			return false
		}
		s.mu.Lock()
		s.pending = s.pending[1:]
		s.mu.Unlock()
	}
}

func (s *Store) apply(db *sql.DB, write Write) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := write.Apply(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// permanent reports a database error that a retry would repeat: invalid
// data, a constraint violation or a statement the schema does not accept.
func permanent(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code[:2] {
	case "22", "23", "42":
		return true
	}
	return false
}

// ErrUnavailable reports that the database cannot be used right now.
var ErrUnavailable = errors.New("PostgreSQL is unavailable")

// WithDB runs fn with the database when it is available, with a context
// bounded by timeout (the store's when zero). A failure of fn that is not a
// rejected statement marks the database unavailable.
func (s *Store) WithDB(timeout time.Duration, fn func(ctx context.Context, db *sql.DB) error) error {
	db, ok := s.ready()
	if !ok {
		status := s.Status()
		if status.LastError != "" {
			return fmt.Errorf("%w: %s", ErrUnavailable, status.LastError)
		}
		return ErrUnavailable
	}
	if timeout <= 0 {
		timeout = s.cfg.Timeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := fn(ctx, db)
	if err != nil && !permanent(err) && !errors.Is(err, ErrUnavailable) && isConnectionError(err) {
		s.fail(err)
	}
	return err
}

// Generation increases each time the database becomes available after it
// was not; readers caching database state reload when it changes.
func (s *Store) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// isConnectionError reports failures of the connection rather than of the
// statement, which make the database unavailable until it recovers.
func isConnectionError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Class 08: connection exception; 57: operator intervention
		// (shutdown, cancel); 53: insufficient resources.
		switch pgErr.Code[:2] {
		case "08", "57", "53":
			return true
		}
		return false
	}
	return true
}
