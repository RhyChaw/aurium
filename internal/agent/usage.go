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

// ErrorReporter is implemented by adapters whose headless output can report a
// failure while still exiting zero. `claude -p` does: an envelope with
// is_error true and a result explaining why.
type ErrorReporter interface {
	// ReportedError returns true when the output says the run failed,
	// whatever the exit code was.
	ReportedError(stdout string) bool
}

// ReplyParser is implemented by adapters whose headless output wraps the
// agent's answer in an envelope. Separate from UsageParser because the two are
// different questions — what did it say, and what did it cost — and an adapter
// may well be able to answer one and not the other.
type ReplyParser interface {
	// ParseReply extracts the agent's answer from a headless run's stdout.
	// False means the output was not an envelope this adapter recognises, and
	// the caller should fall back to showing the raw output rather than
	// showing nothing.
	ParseReply(stdout string) (string, bool)
}

// claudeJSONResult is the shape of `claude -p --output-format json`.
//
// VERIFIED against a real `claude -p --output-format json` run on 2026-09-14:
// the envelope is one line carrying `result`, `is_error`, `subtype`,
// `total_cost_usd`, `usage` (with the three cache/input counts and
// `output_tokens`) and `modelUsage`. It carries no top-level `model`.
//
// Still parsed defensively — every field is optional, an unrecognised payload
// yields no report rather than a wrong one, and nothing here fails a run —
// because the format is the CLI's to change.
type claudeJSONResult struct {
	Model   string `json:"model"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
	Type    string `json:"type"`
	// ModelUsage is keyed by model name. Verified against a real run on
	// 2026-09-14: the envelope carries no top-level `model`, so this is the
	// only place the model appears — and a usage row with an empty model
	// cannot be priced or grouped.
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
	Usage      struct {
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
			Model: r.modelName(), InputTokens: in, OutputTokens: r.Usage.OutputTokens,
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

// modelName returns the model this run used.
//
// Top-level `model` first because a future envelope may add one; otherwise the
// single key of modelUsage. With several — a run that fell back mid-turn — the
// lexically first is chosen so the value is at least stable, and the token
// counts are already a total across all of them.
func (r claudeJSONResult) modelName() string {
	if r.Model != "" {
		return r.Model
	}
	best := ""
	for name := range r.ModelUsage {
		if best == "" || name < best {
			best = name
		}
	}
	return best
}

// ParseReply pulls the answer out of Claude Code's JSON envelope.
//
// Falling back to raw stdout when this fails is deliberate: showing the user a
// wall of JSON is ugly, and showing them nothing is a bug.
func (c *Claude) ParseReply(stdout string) (string, bool) {
	for _, line := range lastLinesFirst(stdout) {
		var r claudeJSONResult
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if strings.TrimSpace(r.Result) == "" {
			continue
		}
		return strings.TrimSpace(r.Result), true
	}
	return "", false
}

// ReportedError honours the envelope's own verdict. A zero exit with
// is_error true is a failure, and painting that tile green would be a lie.
func (c *Claude) ReportedError(stdout string) bool {
	for _, line := range lastLinesFirst(stdout) {
		var r claudeJSONResult
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.Type == "result" || r.Result != "" {
			return r.IsError
		}
	}
	return false
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
