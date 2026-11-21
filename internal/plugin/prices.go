package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/sketchain/cpa-quota/internal/pricing"
	"github.com/sketchain/cpa-quota/internal/store"
)

// Origins of the ratio table in use.
const (
	PriceOriginRemote   = "remote"
	PriceOriginDatabase = "database"
	PriceOriginNone     = "none"
)

// maxPriceBody bounds the ratio table download.
const maxPriceBody = 8 << 20

// priceState is the ratio table in use and how it was obtained.
type priceState struct {
	mu          sync.Mutex
	table       *pricing.Table
	origin      string
	updatedAt   time.Time
	lastAttempt time.Time
	lastErr     string
}

// PriceStatus describes the ratio table in the quota API.
type PriceStatus struct {
	SourceURL string `json:"source_url"`
	// Origin is remote (fetched by this process), database (the last table
	// any fetch stored) or none.
	Origin string `json:"origin"`
	// UpdatedAt is when the table in use was fetched.
	UpdatedAt     *time.Time `json:"updated_at"`
	Models        int        `json:"models"`
	LastAttemptAt *time.Time `json:"last_attempt_at"`
	LastError     string     `json:"last_error,omitempty"`
}

func (p *priceState) loaded() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.table != nil
}

func (p *priceState) current() *pricing.Table {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.table
}

func (p *priceState) status(url string) PriceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := PriceStatus{SourceURL: url, Origin: p.origin, Models: p.table.Len(), LastError: p.lastErr}
	if status.Origin == "" {
		status.Origin = PriceOriginNone
	}
	if !p.updatedAt.IsZero() {
		at := p.updatedAt.UTC()
		status.UpdatedAt = &at
	}
	if !p.lastAttempt.IsZero() {
		at := p.lastAttempt.UTC()
		status.LastAttemptAt = &at
	}
	return status
}

// due reports whether the table should be fetched now: it is missing or
// older than the refresh period, and the last failed attempt is older than
// the retry period.
func (p *priceState) due(now time.Time, refresh, retry time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.table != nil && now.Sub(p.updatedAt) < refresh {
		return false
	}
	return p.lastAttempt.IsZero() || now.Sub(p.lastAttempt) >= retry
}

// maybeRefreshPrices fetches the ratio table when it is due. It runs within
// usage.handle, so the fetch has a short timeout and a failure keeps the
// table in use.
func (a *App) maybeRefreshPrices() {
	config, _, _, prices, _ := a.state()
	if prices == nil {
		return
	}
	refresh := time.Duration(config.PriceRefreshHours * float64(time.Hour))
	retry := time.Duration(config.PriceRetryMinutes * float64(time.Minute))
	if prices.due(a.Now(), refresh, retry) {
		a.refreshPrices(false)
	}
}

// refreshPrices fetches the ratio table now. On success it is stored for
// other starts; on failure the table in use stays, and without one the
// stored table is loaded.
func (a *App) refreshPrices(initial bool) error {
	config, db, _, prices, _ := a.state()
	if prices == nil {
		return fmt.Errorf("the plugin is not configured")
	}
	now := a.Now()
	body, table, err := fetchPrices(a.HTTPClient, config.PriceURL, seconds(config.PriceTimeoutSeconds))
	prices.mu.Lock()
	prices.lastAttempt = now
	if err == nil {
		prices.table, prices.origin, prices.updatedAt, prices.lastErr = table, PriceOriginRemote, now, ""
		prices.mu.Unlock()
		if db != nil {
			db.Submit(store.PriceTableWrite(config.Instance, config.PriceURL, now, table.Len(), body))
		}
		if !initial {
			a.logf("Ratio table refreshed: %d models", table.Len())
		}
		return nil
	}
	prices.lastErr = err.Error()
	missing := prices.table == nil
	prices.mu.Unlock()
	a.warnf("Ratio table refresh failed; keeping the table in use: %v", err)
	if missing && db != nil {
		a.loadStoredPrices(db, config.Instance, prices)
	}
	return err
}

// loadStoredPrices loads the instance's last stored ratio table.
func (a *App) loadStoredPrices(db *store.Store, instance string, prices *priceState) {
	body, fetchedAt, ok, err := db.LoadPriceTable(instance)
	if err != nil || !ok {
		if err != nil {
			a.warnf("No ratio table: the stored one cannot be read: %v", err)
		} else {
			a.warnf("No ratio table: none is stored yet; requests count as unpriced")
		}
		return
	}
	table, err := pricing.Parse(body)
	if err != nil {
		a.warnf("The stored ratio table is invalid: %v", err)
		return
	}
	prices.mu.Lock()
	defer prices.mu.Unlock()
	if prices.table == nil {
		prices.table, prices.origin, prices.updatedAt = table, PriceOriginDatabase, fetchedAt
		a.logf("Using the stored ratio table of %s: %d models", fetchedAt.UTC().Format(time.RFC3339), table.Len())
	}
}

func fetchPrices(client *http.Client, url string, timeout time.Duration) (json.RawMessage, *pricing.Table, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "cpa-quota/"+Version)
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch ratio table: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("fetch ratio table: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPriceBody+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read ratio table: %w", err)
	}
	if len(body) > maxPriceBody {
		return nil, nil, fmt.Errorf("ratio table exceeds %d bytes", maxPriceBody)
	}
	table, err := pricing.Parse(body)
	if err != nil {
		return nil, nil, err
	}
	return body, table, nil
}
