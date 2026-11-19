// Package quotaheaders reads what a subscription provider's response headers
// say about the account's quota.
//
// Claude reports its unified limit in anthropic-ratelimit-unified-5h-* and
// anthropic-ratelimit-unified-7d-* headers: a utilization fraction and a
// reset time in Unix seconds. ChatGPT, through CLIProxyAPI's Codex channel,
// reports X-Codex-Primary-* and X-Codex-Secondary-* windows: a used percent,
// the window length in minutes and a reset time, for the limit the response
// names in X-Codex-Active-Limit; additional limits carry their own
// X-Codex-<group>-* windows, and code review its X-Codex-Code-Review-* ones.
package quotaheaders

import (
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Limit keys of the windows a report names.
const (
	// LimitClaudeUnified is Claude's unified limit, which covers every model.
	LimitClaudeUnified = "unified"
	// LimitCodexMain is ChatGPT's main (premium) Codex limit.
	LimitCodexMain = "codex"
	// LimitCodexCodeReview is ChatGPT's code review limit.
	LimitCodexCodeReview = "code-review"
)

// Window lengths in seconds of the main windows.
const (
	FiveHourSeconds = 5 * 60 * 60
	WeekSeconds     = 7 * 24 * 60 * 60
)

// maxHeaderValue bounds the header values considered; longer ones are ignored.
const maxHeaderValue = 256

// maxUnixSeconds is the last second of year 9999.
const maxUnixSeconds = 253402300799

// Window is one quota window a response reported.
type Window struct {
	LimitKey    string
	Seconds     int64
	UsedPercent float64
	// ResetAt is zero when the response named no reset.
	ResetAt time.Time
}

// Report is what one response's headers said about the account's quota.
type Report struct {
	Provider string
	Windows  []Window
	// Plan is the subscription plan the response named.
	Plan string
	// CreditBalance and CreditsUnlimited are nil when not reported.
	CreditBalance    *float64
	CreditsUnlimited *bool
	// LimitReached is the provider saying the main limit is used up.
	LimitReached bool
}

// MainWindow returns the report's window of a limit with the given length.
func (r *Report) MainWindow(limitKey string, seconds int64) (Window, bool) {
	if r == nil {
		return Window{}, false
	}
	for _, window := range r.Windows {
		if window.LimitKey == limitKey && window.Seconds == seconds {
			return window, true
		}
	}
	return Window{}, false
}

// MainLimit is the limit key of a provider's main windows; "" when the
// provider has none.
func MainLimit(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return LimitClaudeUnified
	case "codex":
		return LimitCodexMain
	}
	return ""
}

// HasMainWindow reports whether the report names a 5-hour or 7-day window of
// the provider's main limit.
func (r *Report) HasMainWindow() bool {
	if r == nil {
		return false
	}
	limit := MainLimit(r.Provider)
	for _, window := range r.Windows {
		if window.LimitKey == limit && (window.Seconds == FiveHourSeconds || window.Seconds == WeekSeconds) {
			return true
		}
	}
	return false
}

var (
	groupName     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,120}$`)
	balanceFormat = regexp.MustCompile(`^\d+(?:\.\d+)?$`)
)

// Parse reads the quota headers of a response to a request made at at. It
// returns nil when they report nothing. A header given twice counts by its
// first value; a window reported twice keeps the later one.
func Parse(provider string, input http.Header, at time.Time) *Report {
	headers := http.Header{}
	for key, values := range input {
		if len(values) > 0 && len(values[0]) <= maxHeaderValue {
			headers.Set(strings.TrimSpace(key), strings.TrimSpace(values[0]))
		}
	}
	report := &Report{Provider: strings.ToLower(strings.TrimSpace(provider))}
	switch report.Provider {
	case "claude":
		parseClaude(report, headers)
	case "codex":
		parseCodex(report, headers, at)
	default:
		return nil
	}
	if len(report.Windows) == 0 && report.Plan == "" && report.CreditBalance == nil && report.CreditsUnlimited == nil {
		return nil
	}
	return report
}

func parseClaude(report *Report, headers http.Header) {
	for _, window := range []struct {
		name    string
		seconds int64
	}{{"5h", FiveHourSeconds}, {"7d", WeekSeconds}} {
		prefix := "Anthropic-Ratelimit-Unified-" + window.name + "-"
		fraction, ok := number(headers.Get(prefix + "Utilization"))
		if !ok || fraction < 0 {
			continue
		}
		used := fraction * 100
		if math.IsInf(used, 0) {
			continue
		}
		report.add(Window{LimitKey: LimitClaudeUnified, Seconds: window.seconds, UsedPercent: used,
			ResetAt: unixSeconds(headers.Get(prefix + "Reset"))})
	}
}

func parseCodex(report *Report, headers http.Header, at time.Time) {
	// The ungrouped windows belong to the limit the response names. Only the
	// main limit's ("premium", or unnamed on older responses) count as main
	// windows, and only its 5-hour and weekly ones: another pool, such as
	// image generation, reports its own windows there.
	if active := headers.Get("X-Codex-Active-Limit"); active == "" || limitAlias(active) == "premium" {
		before := len(report.Windows)
		codexWindows(report, headers, "X-Codex-", LimitCodexMain, true, at)
		report.LimitReached = strings.EqualFold(headers.Get("X-Codex-Limit-Reached"), "true") && len(report.Windows) > before
	}
	var groups []string
	for key, values := range headers {
		group, found := strings.CutPrefix(key, "X-Codex-")
		if !found {
			continue
		}
		if group, found = strings.CutSuffix(group, "-Limit-Name"); found && groupName.MatchString(group) && values[0] != "" {
			groups = append(groups, group)
		}
	}
	sort.Strings(groups)
	for _, group := range groups {
		prefix := "X-Codex-" + group + "-"
		codexWindows(report, headers, prefix, headers.Get(prefix+"Limit-Name"), false, at)
	}
	if plan := headers.Get("X-Codex-Plan-Type"); len(plan) <= 40 && groupName.MatchString(plan) {
		report.Plan = plan
	}
	codexWindows(report, headers, "X-Codex-Code-Review-", LimitCodexCodeReview, false, at)
	if balance := headers.Get("X-Codex-Credits-Balance"); balanceFormat.MatchString(balance) {
		if value, ok := number(balance); ok {
			report.CreditBalance = &value
		}
	}
	if value := headers.Get("X-Codex-Credits-Unlimited"); value == "true" || value == "false" {
		unlimited := value == "true"
		report.CreditsUnlimited = &unlimited
	}
}

// codexWindows adds the primary and secondary windows under prefix. The
// main limit only has 5-hour and weekly windows.
func codexWindows(report *Report, headers http.Header, prefix, limitKey string, main bool, at time.Time) {
	for _, role := range []string{"Primary-", "Secondary-"} {
		key := prefix + role
		used, ok := number(headers.Get(key + "Used-Percent"))
		if !ok || used < 0 {
			continue
		}
		minutes, err := strconv.ParseInt(headers.Get(key+"Window-Minutes"), 10, 64)
		if err != nil || minutes <= 0 || minutes > 366*24*60 {
			continue
		}
		seconds := minutes * 60
		if main && seconds != FiveHourSeconds && seconds != WeekSeconds {
			continue
		}
		reset := unixSeconds(headers.Get(key + "Reset-At"))
		if reset.IsZero() {
			if after, err := strconv.ParseInt(headers.Get(key+"Reset-After-Seconds"), 10, 64); err == nil && after >= 0 && after <= 366*24*60*60 {
				reset = at.Add(time.Duration(after) * time.Second).UTC()
			}
		}
		report.add(Window{LimitKey: limitKey, Seconds: seconds, UsedPercent: used, ResetAt: reset})
	}
}

func (r *Report) add(window Window) {
	for i := range r.Windows {
		if r.Windows[i].LimitKey == window.LimitKey && r.Windows[i].Seconds == window.Seconds {
			r.Windows[i] = window
			return
		}
	}
	r.Windows = append(r.Windows, window)
}

// limitAlias compares limit names ignoring case, separators and a codex prefix.
func limitAlias(name string) string {
	name = strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(name))
	return strings.TrimPrefix(name, "codex")
}

func number(value string) (float64, bool) {
	parsed, err := strconv.ParseFloat(value, 64)
	return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
}

func unixSeconds(value string) time.Time {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds <= 0 || seconds > maxUnixSeconds {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}
