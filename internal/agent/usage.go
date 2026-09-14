package agent

import (
	"encoding/json"
	"strings"
)

// UsageReport is what a headless run reported about what it spent.
//
// CostUSD is only meaningful when HasCost is set: an adapter that reports
// tokens but no price leaves it zero, and a zero that means "not reported"
// must never be read as "free" (§D26).
type UsageReport struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	HasCost      bool
}

// UsageParser is implemented by adapters whose headless output carries token
// counts. It is a separate interface rather than a method on Adapter because
// most adapters cannot do it, and a required method every implementation
// answers with zeroes is a method that lies.
type UsageParser interface {
	// ParseUsage extracts a report from a headless run's stdout. The bool is
	// false when the output carried no usage at all, which is different from
	// a run that used no tokens.
	ParseUsage(stdout string) (UsageReport, bool)
}

// claudeJSONResult is the shape of `claude -p --output-format json`.
//
// UNVERIFIED, for the same reason the rest of the claude adapter is: this was
// written from the documented output format and no run has been observed. It
// is parsed defensively — every field is optional, an unrecognised payload
// yields no report rather than a wrong one, and nothing here fails a run.
type claudeJSONResult struct {
	Model string `json:"model"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		// Cache reads and writes are billed at different rates. They are
		// folded into the input count rather than dropped, because dropping
		// them would under-report, and Aurium's price table has no cache
		// tiers to price them properly with.
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
}

// ParseUsage reads the JSON envelope Claude Code prints in headless mode.
func (c *Claude) ParseUsage(stdout string) (UsageReport, bool) {
	// The CLI may print progress lines before the result object, so the last
	// JSON object in the output is the one that matters. Scanning backwards
	// also means a prompt that happened to contain a JSON object cannot be
	// mistaken for the result.
	for _, line := range lastLinesFirst(stdout) {
		var r claudeJSONResult
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		in := r.Usage.InputTokens + r.Usage.CacheCreationInputTokens + r.Usage.CacheReadInputTokens
		if in == 0 && r.Usage.OutputTokens == 0 {
			continue
		}
		report := UsageReport{
			Model: r.Model, InputTokens: in, OutputTokens: r.Usage.OutputTokens,
		}
		// The CLI's own figure beats Aurium's price table when it is there:
		// it knows the account's actual rates and Aurium does not.
		if r.TotalCostUSD != nil {
			report.CostUSD, report.HasCost = *r.TotalCostUSD, true
		}
		return report, true
	}
	return UsageReport{}, false
}

// lastLinesFirst returns the non-empty, brace-delimited lines of s, last
// first.
func lastLinesFirst(s string) []string {
	lines := strings.Split(s, "\n")
	var out []string
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}") {
			out = append(out, t)
		}
	}
	// A pretty-printed object spans lines and none of them match above, so
	// the whole output is offered as a last resort.
	if len(out) == 0 {
		if t := strings.TrimSpace(s); strings.HasPrefix(t, "{") {
			out = append(out, t)
		}
	}
	return out
}
