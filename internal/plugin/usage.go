package plugin

import (
	"slices"
	"strings"
	"time"

	"github.com/sketchain/cpa-quota/internal/ccbilling"
	"github.com/sketchain/cpa-quota/internal/pricing"
	"github.com/sketchain/cpa-quota/internal/quotaheaders"
	"github.com/sketchain/cpa-quota/internal/safeguard"
	"github.com/sketchain/cpa-quota/internal/store"
)

// HandleUsage records one usage record. CLIProxyAPI delivers records one by
// one from a background queue, off the client's request path; everything
// here is synchronous and every network operation has a short timeout, so a
// slow database or price server delays the queue by at most those timeouts.
//
// It records a request and its quota report for a tracked account, and a
// safeguard event for a Claude policy refusal; then it refreshes the ratio
// table when due and the estimates when their interval has passed.
func (a *App) HandleUsage(record UsageRecord) {
	config, db, manager, prices, running := a.state()
	if !running || db == nil || manager == nil {
		return
	}
	provider := trimLower(record.Provider)
	authIndex := strings.TrimSpace(record.AuthIndex)
	at := record.RequestedAt
	if at.IsZero() {
		at = a.Now()
	}
	var writes []store.Write
	if refusal, ok := safeguard.Detect(record.Failed, record.Failure.Body); ok && provider == ccbilling.ProviderClaude {
		writes = append(writes, store.SafeguardWrite(store.SafeguardEvent{Instance: config.Instance, At: at, AuthIndex: authIndex, Provider: provider,
			Model: pricing.BillingModel(record.Model, record.Alias), Category: refusal.Category, Reason: refusal.Reason,
			RequestID: strings.TrimSpace(record.RequestID), StatusCode: record.Failure.StatusCode, Stream: record.Stream}))
	}
	if a.counts(config, provider, authIndex, record.AuthType) {
		// A refresh running meanwhile cannot add the account; the next one
		// finds it in the database.
		manager.Track(authIndex)
		a.maybeRefreshPrices()
		request, report := buildFacts(config, record, provider, authIndex, at, prices.current())
		writes = append(writes, store.RecordWrite(&request, report))
		if obs, ok := observation(report, quotaheaders.MainLimit(provider), at); ok {
			manager.Observe(authIndex, obs)
		}
	}
	if len(writes) > 0 {
		db.Submit(writes...)
	} else {
		db.Flush()
	}
	manager.MaybeRefresh()
}

// counts reports whether a record belongs to the instance's subscription
// accounts: the configured provider, a configured credential type and an
// auth index, one of auth_indexes when they are configured.
func (a *App) counts(config Config, provider, authIndex, authType string) bool {
	return authIndex != "" && provider == config.Provider() && slices.Contains(config.AccountAuthTypes, trimLower(authType)) &&
		(len(config.AuthIndexes) == 0 || slices.Contains(config.AuthIndexes, authIndex))
}

// buildFacts prices a record and reads its quota headers.
func buildFacts(config Config, record UsageRecord, provider, authIndex string, at time.Time, table *pricing.Table) (store.Request, *store.QuotaReport) {
	detail := record.Detail
	breakdown := pricing.Split(provider, pricing.Detail{InputTokens: detail.InputTokens, OutputTokens: detail.OutputTokens,
		ReasoningTokens: detail.ReasoningTokens, CachedTokens: detail.CachedTokens, CacheReadTokens: detail.CacheReadTokens,
		CacheCreationTokens: detail.CacheCreationTokens, TotalTokens: detail.TotalTokens})
	rates, priced := table.Lookup(record.Model, record.Alias)
	cost := 0.0
	if priced {
		cost = pricing.Cost(rates, breakdown)
		// ServiceTier is what the client asked for, as cpa-super-billing
		// priced it.
		if config.CodexFastModeBilling && provider == ccbilling.ProviderCodex && breakdown.Billable() &&
			strings.EqualFold(strings.TrimSpace(record.ServiceTier), "priority") {
			cost *= pricing.FastModeMultiplier
		}
	}
	headers := quotaheaders.Parse(provider, record.ResponseHeaders, at)
	request := store.Request{Instance: config.Instance, AuthIndex: authIndex, Provider: provider, RequestID: strings.TrimSpace(record.RequestID),
		RequestedAt: at, Latency: record.Latency, Model: pricing.BillingModel(record.Model, record.Alias),
		UpstreamModel: pricing.WithoutOptions(record.Model), ServiceTier: strings.TrimSpace(record.ServiceTier), Stream: record.Stream,
		InputTokens: breakdown.Uncached, CacheReadTokens: breakdown.CacheRead, CacheWriteTokens: breakdown.CacheWrite, OutputTokens: breakdown.Output,
		ReasoningTokens: breakdown.Reasoning, TotalTokens: breakdown.Total, Accounting: breakdown.Quality, CostUSD: cost, Priced: priced,
		MainWindow: headers.HasMainWindow(), Failed: record.Failed, StatusCode: record.Failure.StatusCode}
	if headers == nil {
		return request, nil
	}
	report := &store.QuotaReport{Instance: config.Instance, AuthIndex: authIndex, Provider: provider, RequestID: request.RequestID, At: at,
		Plan: headers.Plan, CreditBalance: headers.CreditBalance, CreditsUnlimited: headers.CreditsUnlimited, LimitReached: headers.LimitReached}
	for _, window := range headers.Windows {
		report.Windows = append(report.Windows, store.ReportWindow{LimitKey: window.LimitKey, Seconds: window.Seconds,
			UsedPercent: window.UsedPercent, ResetAt: window.ResetAt})
	}
	return request, report
}

// observation is what a report says about the main windows, for the live
// state; false when it names none.
func observation(report *store.QuotaReport, limit string, at time.Time) (ccbilling.Observation, bool) {
	if report == nil {
		return ccbilling.Observation{}, false
	}
	obs := ccbilling.Observation{At: at}
	if report.CreditBalance != nil {
		obs.HasCredits, obs.Credits = true, *report.CreditBalance
	}
	for _, window := range report.Windows {
		if window.LimitKey != limit {
			continue
		}
		switch window.Seconds {
		case quotaheaders.FiveHourSeconds:
			obs.Has5h, obs.Used5h, obs.Reset5h = true, window.UsedPercent, window.ResetAt
		case quotaheaders.WeekSeconds:
			obs.Has7d, obs.Used7d, obs.Reset7d = true, window.UsedPercent, window.ResetAt
		}
	}
	obs.LimitReached = report.LimitReached && (obs.Has5h || obs.Has7d)
	return obs, obs.Has5h || obs.Has7d
}
