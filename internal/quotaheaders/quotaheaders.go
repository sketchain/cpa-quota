// Package quotaheaders reads what Claude's response headers say about the
// account's quota.
//
// Claude reports its unified limit in anthropic-ratelimit-unified-5h-* and
// anthropic-ratelimit-unified-7d-* headers: a utilization fraction and a
// reset time in Unix seconds.
package quotaheaders

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// LimitClaudeUnified is the limit key of Claude's unified limit, which covers
// every model.
const LimitClaudeUnified = "unified"

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
	Windows []Window
}

// Parse reads the quota headers of a Claude response. It returns nil when
// they report nothing. A header given twice counts by its first value.
func Parse(input http.Header) *Report {
	headers := http.Header{}
	for key, values := range input {
		if len(values) > 0 && len(values[0]) <= maxHeaderValue {
			headers.Set(strings.TrimSpace(key), strings.TrimSpace(values[0]))
		}
	}
	report := &Report{}
	parseClaude(report, headers)
	if len(report.Windows) == 0 {
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
		report.Windows = append(report.Windows, Window{LimitKey: LimitClaudeUnified, Seconds: window.seconds, UsedPercent: used,
			ResetAt: unixSeconds(headers.Get(prefix + "Reset"))})
	}
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
