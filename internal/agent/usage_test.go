package agent

import "testing"

func TestClaudeParsesUsageFromTheResultObject(t *testing.T) {
	c := &Claude{}
	out := `starting up
{"type":"result","model":"claude-opus-5","result":"done","usage":{"input_tokens":1200,"output_tokens":340,"cache_read_input_tokens":800},"total_cost_usd":0.42}`

	got, ok := c.ParseUsage(out)
	if !ok {
		t.Fatal("the result object carries usage and must be found")
	}
	// Cache reads are billed, so dropping them would under-report.
	if got.InputTokens != 2000 || got.OutputTokens != 340 {
		t.Fatalf("tokens = %d/%d", got.InputTokens, got.OutputTokens)
	}
	if got.Model != "claude-opus-5" {
		t.Fatalf("model = %q", got.Model)
	}
	// The CLI knows the account's real rates; Aurium's table does not.
	if !got.HasCost || got.CostUSD != 0.42 {
		t.Fatalf("cost = %v (has=%v)", got.CostUSD, got.HasCost)
	}
}

func TestClaudeParsesPrettyPrintedOutput(t *testing.T) {
	c := &Claude{}
	out := "{\n  \"model\": \"claude-sonnet-5\",\n  \"usage\": {\n    \"input_tokens\": 10,\n    \"output_tokens\": 5\n  }\n}"
	got, ok := c.ParseUsage(out)
	if !ok || got.InputTokens != 10 || got.OutputTokens != 5 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	// No cost reported is not a cost of zero.
	if got.HasCost {
		t.Fatal("absent total_cost_usd must not read as free")
	}
}

// An unrecognised payload must produce no report rather than a wrong one: a
// fabricated zero would silently under-report the fleet's spend.
func TestClaudeReportsNothingForUnparseableOutput(t *testing.T) {
	c := &Claude{}
	for _, out := range []string{"", "not json at all", `{"type":"result","result":"hi"}`} {
		if _, ok := c.ParseUsage(out); ok {
			t.Errorf("%q must not yield a usage report", out)
		}
	}
}

// The interface is optional on purpose; asserting it here keeps the contract
// from drifting silently.
func TestClaudeImplementsUsageParser(t *testing.T) {
	var _ UsageParser = (*Claude)(nil)
	if _, ok := any(&Codex{}).(UsageParser); ok {
		t.Fatal("codex exec does not report usage; claiming it does would invent numbers")
	}
}
