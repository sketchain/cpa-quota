package pricing

import "math"

// Detail is the token usage CLIProxyAPI reports in a usage record.
type Detail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

func (d Detail) empty() bool {
	return d.InputTokens == 0 && d.OutputTokens == 0 && d.ReasoningTokens == 0 && d.CachedTokens == 0 &&
		d.CacheReadTokens == 0 && d.CacheCreationTokens == 0 && d.TotalTokens == 0
}

// Accounting qualities of a breakdown. Only complete usage is priced:
// counts that contradict each other, or a total without its parts, are
// recorded but cost nothing rather than being guessed.
const (
	// QualityNone: the request reported no usage.
	QualityNone         = ""
	QualityComplete     = "complete"
	QualityInconsistent = "inconsistent"
)

// Breakdown is usage split into non-overlapping priced buckets. Output
// includes Reasoning.
type Breakdown struct {
	Quality    string
	Uncached   int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
	Reasoning  int64
	Total      int64
}

// Billable reports whether the breakdown can be priced.
func (b Breakdown) Billable() bool { return b.Quality == QualityComplete }

// Split reads a usage record's counts with the semantics CLIProxyAPI's
// Claude parser uses: input, cache reads and cache writes are separate
// counts, and the output count already includes reasoning; when the reported
// total also adds reasoning, reasoning is separate from output.
func Split(detail Detail) Breakdown {
	if detail.empty() {
		return Breakdown{}
	}
	return claudeSplit(detail)
}

func claudeSplit(d Detail) Breakdown {
	input, ok := sum(d.InputTokens, d.CacheReadTokens, d.CacheCreationTokens)
	if !ok || d.OutputTokens < 0 || d.ReasoningTokens < 0 {
		return inconsistent(d.TotalTokens, 0)
	}
	output := d.OutputTokens
	if separate, ok := sum(input, d.OutputTokens, d.ReasoningTokens); ok && d.TotalTokens == separate {
		output = d.OutputTokens + d.ReasoningTokens
	} else if !ok || d.ReasoningTokens > d.OutputTokens {
		return inconsistent(d.TotalTokens, 0)
	}
	expected, ok := sum(input, output)
	if !ok {
		return inconsistent(d.TotalTokens, 0)
	}
	total := d.TotalTokens
	switch {
	case total == 0:
		total = expected
	case total != expected:
		return inconsistent(d.TotalTokens, expected)
	}
	return Breakdown{Quality: QualityComplete, Uncached: d.InputTokens, CacheRead: d.CacheReadTokens, CacheWrite: d.CacheCreationTokens,
		Output: output, Reasoning: d.ReasoningTokens, Total: total}
}

func inconsistent(total, computed int64) Breakdown {
	if total <= 0 {
		total = computed
	}
	return Breakdown{Quality: QualityInconsistent, Total: max(total, 0)}
}

func sum(values ...int64) (int64, bool) {
	var total int64
	for _, value := range values {
		if value < 0 || total > math.MaxInt64-value {
			return 0, false
		}
		total += value
	}
	return total, true
}

// Cost prices a breakdown in USD; usage that is not billable costs nothing.
func Cost(rates Rates, breakdown Breakdown) float64 {
	if !breakdown.Billable() {
		return 0
	}
	return perMillion(breakdown.Uncached, rates.Input) + perMillion(breakdown.CacheRead, rates.CacheRead) +
		perMillion(breakdown.CacheWrite, rates.CacheWrite) + perMillion(breakdown.Output, rates.Output)
}

func perMillion(tokens int64, rate float64) float64 {
	if tokens <= 0 || rate == 0 {
		return 0
	}
	return float64(tokens) / 1_000_000 * rate
}
