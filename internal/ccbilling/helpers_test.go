package ccbilling

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sketchain/cpa-quota/internal/pgtest"
)

// testInstance is the instance every test manager runs as.
const testInstance = "test"

// fakeHost serves a PostgreSQL schema of its own with the plugin's tables,
// counts every call and can fail WithDB to imitate an outage.
type fakeHost struct {
	db         *sql.DB
	now        time.Time
	calls      atomic.Int64
	dbErr      error
	generation atomic.Uint64
	nextID     atomic.Int64

	mu   sync.Mutex
	logs []string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	db, _ := pgtest.Open(t)
	return &fakeHost{db: db, now: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
}

func (h *fakeHost) WithDB(timeout time.Duration, fn func(context.Context, *sql.DB) error) error {
	h.calls.Add(1)
	if h.dbErr != nil {
		return h.dbErr
	}
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return fn(ctx, h.db)
}

func (h *fakeHost) DBGeneration() uint64 { return h.generation.Load() }
func (h *fakeHost) Now() time.Time       { h.calls.Add(1); return h.now }
func (h *fakeHost) Logf(format string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, fmt.Sprintf(format, args...))
}

var errOffline = errors.New("PostgreSQL is unavailable")

// newTestManager returns a manager of the test instance that refreshes on
// every request and processes any backlog at once.
func newTestManager(host *fakeHost) *Manager {
	manager := NewManager(host)
	manager.Instance = testInstance
	manager.RefreshInterval = 0
	manager.ProcessBudget = time.Hour
	return manager
}

// configure applies settings, tracking the given accounts with the
// template's provider and tier.
func configure(t *testing.T, manager *Manager, settings Settings, template AccountSettings, accounts ...string) {
	t.Helper()
	settings.Accounts = map[string]AccountSettings{}
	for _, index := range accounts {
		settings.Accounts[index] = template
	}
	if _, err := manager.Configure(settings, template, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
}

// batch inserts rows into a table in multi-row statements.
type batch struct {
	t       *testing.T
	db      *sql.DB
	table   string
	columns []string
	rows    [][]any
	// before are flushed first, so rows they reference exist.
	before []*batch
}

func newBatch(t *testing.T, db *sql.DB, table string, columns ...string) *batch {
	return &batch{t: t, db: db, table: table, columns: columns}
}

func (b *batch) add(values ...any) {
	b.rows = append(b.rows, values)
	if len(b.rows) >= 500 {
		b.flush()
	}
}

func (b *batch) flush() {
	b.t.Helper()
	if len(b.rows) == 0 {
		return
	}
	for _, other := range b.before {
		other.flush()
	}
	var placeholders []string
	var args []any
	for _, row := range b.rows {
		var marks []string
		for _, value := range row {
			args = append(args, value)
			marks = append(marks, fmt.Sprintf("$%d", len(args)))
		}
		placeholders = append(placeholders, "("+strings.Join(marks, ", ")+")")
	}
	if _, err := b.db.Exec(`INSERT INTO `+b.table+` (`+strings.Join(b.columns, ", ")+`) VALUES `+strings.Join(placeholders, ", "), args...); err != nil {
		b.t.Fatal(err)
	}
	b.rows = nil
}

// facts writes requests and quota reports of the test instance the way the
// plugin records them.
type facts struct {
	host     *fakeHost
	requests *batch
	reports  *batch
	windows  *batch
}

func newFacts(t *testing.T, host *fakeHost) *facts {
	f := &facts{host: host,
		requests: newBatch(t, host.db, "requests", "instance", "auth_index", "provider", "request_id", "requested_ns", "latency_ms",
			"completed_ns", "model", "upstream_model", "cost_usd", "priced", "main_window", "accounting"),
		reports: newBatch(t, host.db, "quota_reports", "id", "instance", "auth_index", "provider", "request_id", "at_ns", "credit_balance"),
		windows: newBatch(t, host.db, "quota_report_windows", "report_id", "instance", "auth_index", "provider", "limit_key", "seconds",
			"at_ns", "used_percent", "reset_ns")}
	f.windows.before = []*batch{f.reports}
	return f
}

// request records a request; mainWindow says whether its own report named a
// main window.
func (f *facts) request(authIndex, provider string, at time.Time, latency time.Duration, usd float64, model string, priced, mainWindow bool) {
	f.requestUsage(authIndex, provider, at, latency, usd, model, priced, mainWindow, "complete")
}

// requestUsage records a request with the given accounting quality.
func (f *facts) requestUsage(authIndex, provider string, at time.Time, latency time.Duration, usd float64, model string, priced, mainWindow bool,
	accounting string) {
	id := f.host.nextID.Add(1)
	completed := at.Add(time.Duration(max(latency.Milliseconds(), 0)) * time.Millisecond)
	f.requests.add(testInstance, authIndex, provider, fmt.Sprintf("r%d", id), nanos(at), latency.Milliseconds(), nanos(completed), model, model,
		usd, priced, mainWindow, accounting)
}

// report records a quota report with its windows; credits "" means none.
func (f *facts) report(authIndex, provider string, at time.Time, credits string, windows ...testWindow) {
	id := f.host.nextID.Add(1)
	var balance any
	if credits != "" {
		balance = credits
	}
	f.reports.add(id, testInstance, authIndex, provider, fmt.Sprintf("q%d", id), nanos(at), balance)
	for _, window := range windows {
		f.windows.add(id, testInstance, authIndex, provider, window.limit, window.seconds, nanos(at), window.used, nanos(window.reset))
	}
}

func (f *facts) flush() {
	f.requests.flush()
	f.reports.flush()
	f.windows.flush()
}

// claudeWindows are a report's unified 5-hour and 7-day windows.
func claudeWindows(obs Observation) []testWindow {
	return []testWindow{{"unified", 18000, obs.Used5h, obs.Reset5h}, {"unified", 604800, obs.Used7d, obs.Reset7d}}
}

type testWindow struct {
	limit   string
	seconds int64
	used    float64
	reset   time.Time
}
