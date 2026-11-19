package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the trend's time zone must load on hosts without zoneinfo

	"github.com/sketchain/cpa-quota/internal/ccbilling"
	"github.com/sketchain/cpa-quota/internal/store"
)

// App is the plugin. The host calls HandleMethod for every RPC; nothing
// runs between calls: no goroutine or timer is started here.
type App struct {
	// mu guards the fields below.
	mu      sync.Mutex
	config  Config
	running bool
	store   *store.Store
	manager *ccbilling.Manager
	prices  *priceState
	// resourceBase is the prefix of the plugin's resource routes.
	resourceBase string

	// Clock is the time source; HTTPClient fetches the ratio table.
	Clock      func() time.Time
	HTTPClient *http.Client
	// hostLog sends a log line to CLIProxyAPI.
	hostLog func(level, message string)
}

// NewApp returns an unregistered plugin.
func NewApp() *App {
	return &App{Clock: time.Now, HTTPClient: &http.Client{}, hostLog: func(string, string) {}}
}

// SetHostLogger sets the function that forwards log lines to the host.
func (a *App) SetHostLogger(log func(level, message string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hostLog = log
}

func (a *App) logf(format string, args ...any) {
	a.mu.Lock()
	log := a.hostLog
	a.mu.Unlock()
	log("info", "cpa-quota: "+fmt.Sprintf(format, args...))
}

func (a *App) warnf(format string, args ...any) {
	a.mu.Lock()
	log := a.hostLog
	a.mu.Unlock()
	log("warn", "cpa-quota: "+fmt.Sprintf(format, args...))
}

// HandleMethod answers one RPC with its envelope.
func (a *App) HandleMethod(method string, request []byte) []byte {
	result, err := a.handle(method, request)
	if err != nil {
		raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: "plugin_error", Message: err.Error()}})
		return raw
	}
	raw, err := json.Marshal(envelope{OK: true, Result: result})
	if err != nil {
		raw, _ = json.Marshal(envelope{OK: false, Error: &envelopeError{Code: "plugin_error", Message: err.Error()}})
	}
	return raw
}

func (a *App) handle(method string, request []byte) (json.RawMessage, error) {
	switch method {
	case MethodPluginRegister, MethodPluginReconfigure:
		var lifecycle LifecycleRequest
		if err := json.Unmarshal(request, &lifecycle); err != nil {
			return nil, fmt.Errorf("decode %s: %w", method, err)
		}
		if err := a.Configure(lifecycle.ConfigYAML); err != nil {
			return nil, err
		}
		return json.Marshal(registration{SchemaVersion: schemaVersion, Metadata: metadata{Name: PluginName, Version: Version, Author: Author,
			GitHubRepository: Repository, ConfigFields: configFields()}, Capabilities: capabilities{UsagePlugin: true, ManagementAPI: true}})
	case MethodPluginQuiesce:
		return json.RawMessage(`{}`), nil
	case MethodPluginShutdown:
		a.Shutdown()
		return json.RawMessage(`{}`), nil
	case MethodUsageHandle:
		var record UsageRecord
		if err := json.Unmarshal(request, &record); err != nil {
			return nil, fmt.Errorf("decode usage record: %w", err)
		}
		a.HandleUsage(record)
		return json.RawMessage(`{}`), nil
	case MethodManagementRegister:
		var registration ManagementRegistrationRequest
		if err := json.Unmarshal(request, &registration); err != nil {
			return nil, fmt.Errorf("decode %s: %w", method, err)
		}
		return json.Marshal(a.RegisterManagement(registration))
	case MethodManagementHandle:
		var req ManagementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode %s: %w", method, err)
		}
		return json.Marshal(a.HandleManagement(req))
	}
	return nil, fmt.Errorf("unknown method %s", method)
}

// Configure applies a configuration. The first call connects to the
// database and loads the prices; registration succeeds even when neither is
// reachable, and both are retried while handling usage.
func (a *App) Configure(raw []byte) error {
	config, err := ParseConfig(raw)
	if err != nil {
		return err
	}
	settings, err := config.Settings()
	if err != nil {
		return err
	}
	a.mu.Lock()
	previous, running := a.config, a.running
	reopen := !running || previous.PostgresDSN != config.PostgresDSN || previous.PostgresSchema != config.PostgresSchema ||
		previous.DBTimeoutSeconds != config.DBTimeoutSeconds || previous.DBRetrySeconds != config.DBRetrySeconds
	var old *store.Store
	if reopen {
		old = a.store
		a.store = store.New(store.Config{DSN: config.PostgresDSN, Schema: config.PostgresSchema, Timeout: seconds(config.DBTimeoutSeconds),
			RetryInterval: seconds(config.DBRetrySeconds), Now: a.Now, Logf: a.warnf})
	}
	if a.manager == nil || previous.Instance != config.Instance || reopen {
		a.manager = ccbilling.NewManager(a)
	}
	manager := a.manager
	manager.Instance = config.Instance
	manager.RefreshInterval = seconds(config.RefreshIntervalSeconds)
	manager.ProcessBudget = seconds(config.ProcessBudgetSeconds)
	manager.DBTimeout = seconds(config.DBTimeoutSeconds)
	manager.ForecastWindow5h = time.Duration(config.ForecastWindow5hMinutes * float64(time.Minute))
	manager.ForecastWindow7d = time.Duration(config.ForecastWindow7dHours * float64(time.Hour))
	manager.TrendLocation = config.Location()
	if a.prices == nil || previous.PriceURL != config.PriceURL || previous.Instance != config.Instance {
		a.prices = &priceState{}
	}
	a.config, a.running = config, true
	db := a.store
	a.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	if _, err := manager.Configure(settings, config.Account(), config.AuthIndexes); err != nil {
		return err
	}
	if reopen {
		if err := db.Open(); err != nil {
			a.warnf("PostgreSQL is not available; estimates stay in cold start until it is: %v", err)
		}
	}
	if !a.prices.loaded() {
		a.refreshPrices(true)
	}
	manager.MaybeRefresh()
	return nil
}

// Shutdown closes the database connection.
func (a *App) Shutdown() {
	a.mu.Lock()
	db := a.store
	a.running = false
	a.mu.Unlock()
	if db != nil {
		_ = db.Close()
	}
}

// state returns the current configuration and components.
func (a *App) state() (Config, *store.Store, *ccbilling.Manager, *priceState, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.config, a.store, a.manager, a.prices, a.running
}

// WithDB, DBGeneration, Now and Logf make the App the estimate manager's host.
func (a *App) WithDB(timeout time.Duration, fn func(context.Context, *sql.DB) error) error {
	_, db, _, _, running := a.state()
	if !running || db == nil {
		return store.ErrUnavailable
	}
	// Writes the database has not accepted yet would be missed by an
	// estimate advancing past them, so the estimate waits for them.
	if !db.Flush() {
		return fmt.Errorf("%w: %d writes are pending", store.ErrUnavailable, db.Pending())
	}
	return db.WithDB(timeout, fn)
}

func (a *App) DBGeneration() uint64 {
	_, db, _, _, _ := a.state()
	if db == nil {
		return 0
	}
	return db.Generation()
}

func (a *App) Now() time.Time { return a.Clock() }

func (a *App) Logf(format string, args ...any) { a.warnf(format, args...) }

// errUnavailable reports whether err means the database is unavailable.
func errUnavailable(err error) bool { return errors.Is(err, store.ErrUnavailable) }

func trimLower(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
