// Package usage meters what the fleet spends (§D26).
//
// Two halves: a price table, and a recorder that turns an adapter's reported
// token counts into a usage_events row. Nothing here estimates. A call that
// reports no usage produces no row, and a model with no price produces a row
// marked unpriced — because a cost view that quietly under-reports is worse
// than one that shows a gap and says what is missing.
package usage

import "strings"

// Price is the cost of a million tokens, in US dollars.
//
// Prices are list prices as published on the dates in PricesVerified below.
// They are a convenience, not an invoice: discounts, batch pricing, cache
// reads and long-context tiers all move the real number, and the only
// authority on what was actually charged is the provider's own billing page.
type Price struct {
	Provider  string
	InputPMT  float64
	OutputPMT float64
}

// PricesVerified records when this table was last checked, so a stale figure
// is visible rather than merely wrong. The Usage tab shows it.
const PricesVerified = "2026-05"

// prices is keyed by a normalised model name. Lookup is longest-prefix, so a
// dated snapshot like "claude-opus-5-20260401" matches its family without the
// table needing a row per release.
var prices = map[string]Price{
	// Anthropic
	"claude-opus-5":   {Provider: "anthropic", InputPMT: 15, OutputPMT: 75},
	"claude-sonnet-5": {Provider: "anthropic", InputPMT: 3, OutputPMT: 15},
	"claude-haiku-4":  {Provider: "anthropic", InputPMT: 0.80, OutputPMT: 4},
	"claude-opus-4":   {Provider: "anthropic", InputPMT: 15, OutputPMT: 75},
	"claude-sonnet-4": {Provider: "anthropic", InputPMT: 3, OutputPMT: 15},

	// OpenAI
	"gpt-5":      {Provider: "openai", InputPMT: 1.25, OutputPMT: 10},
	"gpt-5-mini": {Provider: "openai", InputPMT: 0.25, OutputPMT: 2},
	"gpt-4.1":    {Provider: "openai", InputPMT: 2, OutputPMT: 8},
	"gpt-4o":     {Provider: "openai", InputPMT: 2.50, OutputPMT: 10},
	"o3":         {Provider: "openai", InputPMT: 2, OutputPMT: 8},
}

// Lookup returns the price for a model, and whether one is known.
//
// The longest matching prefix wins so that "gpt-5-mini" is not priced as
// "gpt-5" — the shorter key is a prefix of the longer one, and taking the
// first match would overcharge by five times.
func Lookup(model string) (Price, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return Price{}, false
	}
	best := ""
	for key := range prices {
		if strings.HasPrefix(m, key) && len(key) > len(best) {
			best = key
		}
	}
	if best == "" {
		return Price{}, false
	}
	return prices[best], true
}

// Cost prices a call. The second return says whether a price was found at all;
// callers must not treat a false as $0.
func Cost(model string, inputTokens, outputTokens int64) (float64, bool) {
	p, ok := Lookup(model)
	if !ok {
		return 0, false
	}
	return float64(inputTokens)/1e6*p.InputPMT + float64(outputTokens)/1e6*p.OutputPMT, true
}

// ProviderFor maps an adapter name onto the company being billed. It is the
// one place that mapping lives, so adding an adapter is one line here rather
// than a string comparison in four packages.
func ProviderFor(adapter string) string {
	switch strings.ToLower(adapter) {
	case "claude":
		return "anthropic"
	case "codex":
		return "openai"
	default:
		// `shell` and `custom` spend nothing Aurium can see.
		return ""
	}
}

// Models lists the priced models, for the UI to show what is covered.
func Models() []string {
	out := make([]string, 0, len(prices))
	for k := range prices {
		out = append(out, k)
	}
	return out
}
