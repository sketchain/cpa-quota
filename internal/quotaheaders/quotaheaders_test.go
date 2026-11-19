package quotaheaders

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func describe(report *Report) string {
	if report == nil {
		return "nil"
	}
	var parts []string
	for _, w := range report.Windows {
		reset := "-"
		if !w.ResetAt.IsZero() {
			reset = w.ResetAt.Format(time.RFC3339)
		}
		parts = append(parts, fmt.Sprintf("%s/%d=%g@%s", w.LimitKey, w.Seconds, w.UsedPercent, reset))
	}
	return strings.Join(parts, ",")
}

func TestClaudeUnifiedWindows(t *testing.T) {
	report := Parse("claude", http.Header{
		"anthropic-ratelimit-unified-5h-utilization": {"0.23"},
		"anthropic-ratelimit-unified-5h-reset":       {"1800000000"},
		"anthropic-ratelimit-unified-7d-utilization": {"NaN"},
		"x-ratelimit-remaining-tokens":               {"1000"},
	}, at)
	if got := describe(report); got != "unified/18000=23@2027-01-15T08:00:00Z" {
		t.Fatalf("report = %s", got)
	}
	if !report.HasMainWindow() {
		t.Fatal("5-hour window is a main window")
	}
	report = Parse("claude", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"1.0"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.405"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {"-5"},
	}, at)
	if got := describe(report); got != "unified/18000=100@-,unified/604800=40.5@-" {
		t.Fatalf("report = %s", got)
	}
	for _, headers := range []http.Header{
		{"Anthropic-Ratelimit-Unified-5h-Utilization": {"-0.1"}},
		{"Anthropic-Ratelimit-Unified-5h-Utilization": {"Inf"}},
		{"Anthropic-Ratelimit-Unified-5h-Utilization": {strings.Repeat("1", 300)}},
		{},
	} {
		if report := Parse("claude", headers, at); report != nil {
			t.Fatalf("%v gave %s", headers, describe(report))
		}
	}
	if Parse("gemini", http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.5"}}, at) != nil {
		t.Fatal("other providers report nothing")
	}
}

func TestCodexMainWindowsAndCredits(t *testing.T) {
	report := Parse("codex", http.Header{
		"x-codex-plan-type":            {"plus"},
		"x-codex-primary-used-percent": {"20.5"}, "x-codex-primary-window-minutes": {"300"},
		"x-codex-primary-reset-after-seconds": {"1800"},
		"x-codex-secondary-used-percent":      {"40"}, "x-codex-secondary-window-minutes": {"10080"},
		"x-codex-secondary-reset-at": {"1800000000"},
		"x-codex-credits-balance":    {"126.4"}, "x-codex-credits-unlimited": {"false"},
		"x-codex-limit-reached": {"True"},
	}, at)
	if got := describe(report); got != "codex/18000=20.5@2026-10-01T12:30:00Z,codex/604800=40@2027-01-15T08:00:00Z" {
		t.Fatalf("report = %s", got)
	}
	if report.Plan != "plus" || report.CreditBalance == nil || *report.CreditBalance != 126.4 || report.CreditsUnlimited == nil ||
		*report.CreditsUnlimited || !report.LimitReached || !report.HasMainWindow() {
		t.Fatalf("report = %+v", report)
	}
	// Credits alone still make a report; a malformed balance does not count.
	report = Parse("codex", http.Header{"X-Codex-Credits-Unlimited": {"true"}, "X-Codex-Credits-Balance": {"-3"}}, at)
	if report == nil || report.CreditBalance != nil || report.CreditsUnlimited == nil || !*report.CreditsUnlimited || report.HasMainWindow() {
		t.Fatalf("report = %+v", report)
	}
}

func TestCodexWindowsAreAttributedToTheirLimit(t *testing.T) {
	window := func(prefix, minutes string) http.Header {
		return http.Header{prefix + "Used-Percent": {"20"}, prefix + "Window-Minutes": {minutes}, prefix + "Reset-At": {"1800000000"}}
	}
	main := func(active, minutes string) http.Header {
		headers := window("X-Codex-Primary-", minutes)
		headers.Set("X-Codex-Limit-Reached", "true")
		if active != "" {
			headers.Set("X-Codex-Active-Limit", active)
		}
		return headers
	}
	additional := func(name string) http.Header {
		headers := window("X-Codex-Feature-Primary-", "300")
		headers.Set("X-Codex-Feature-Limit-Name", name)
		return headers
	}
	reset := "@2027-01-15T08:00:00Z"
	for _, test := range []struct {
		name    string
		headers http.Header
		want    string
		reached bool
	}{
		{"main limit", main("premium", "300"), "codex/18000=20" + reset, true},
		{"main limit with a codex prefix", main("codex-premium", "300"), "codex/18000=20" + reset, true},
		{"response before limit names", main("", "10080"), "codex/604800=20" + reset, true},
		{"image pool before limit names", main("", "1440"), "nil", false},
		{"image pool", main("images", "300"), "nil", false},
		{"main limit beyond its windows", main("premium", "1440"), "nil", false},
		{"Spark", additional("GPT-5.3-Codex-Spark"), "GPT-5.3-Codex-Spark/18000=20" + reset, false},
		{"code review", window("X-Codex-Code-Review-Primary-", "10080"), "code-review/604800=20" + reset, false},
	} {
		report := Parse("codex", test.headers, at)
		if got := describe(report); got != test.want {
			t.Errorf("%s: windows %s, want %s", test.name, got, test.want)
		}
		if report != nil && report.LimitReached != test.reached {
			t.Errorf("%s: limit reached %v", test.name, report.LimitReached)
		}
	}
	// A Spark response reports the Spark limit in the ungrouped headers too;
	// only the grouped windows count, and they are not main windows.
	report := Parse("codex", http.Header{
		"X-Codex-Active-Limit":         {"codex_spark"},
		"X-Codex-Primary-Used-Percent": {"90"}, "X-Codex-Primary-Window-Minutes": {"300"},
		"X-Codex-Spark-Limit-Name":           {"Spark"},
		"X-Codex-Spark-Primary-Used-Percent": {"20"}, "X-Codex-Spark-Primary-Window-Minutes": {"300"},
	}, at)
	if got := describe(report); got != "Spark/18000=20@-" || report.HasMainWindow() {
		t.Fatalf("additional limit read as main: %s", got)
	}
}
