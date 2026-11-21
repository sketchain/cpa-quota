package quotaheaders

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

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
	report := Parse(http.Header{
		"anthropic-ratelimit-unified-5h-utilization": {"0.23"},
		"anthropic-ratelimit-unified-5h-reset":       {"1800000000"},
		"anthropic-ratelimit-unified-7d-utilization": {"NaN"},
		"x-ratelimit-remaining-tokens":               {"1000"},
	})
	if got := describe(report); got != "unified/18000=23@2027-01-15T08:00:00Z" {
		t.Fatalf("report = %s", got)
	}
	report = Parse(http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"1.0"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.405"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {"-5"},
	})
	if got := describe(report); got != "unified/18000=100@-,unified/604800=40.5@-" {
		t.Fatalf("report = %s", got)
	}
	for _, headers := range []http.Header{
		{"Anthropic-Ratelimit-Unified-5h-Utilization": {"-0.1"}},
		{"Anthropic-Ratelimit-Unified-5h-Utilization": {"Inf"}},
		{"Anthropic-Ratelimit-Unified-5h-Utilization": {strings.Repeat("1", 300)}},
		{},
	} {
		if report := Parse(headers); report != nil {
			t.Fatalf("%v gave %s", headers, describe(report))
		}
	}
}
