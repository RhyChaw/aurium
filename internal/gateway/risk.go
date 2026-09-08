package gateway

import "strings"

// Risk levels (§10.3).
type Risk string

const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"
)

// highVerbs are operations that destroy or publish something irreversibly.
var highVerbs = []string{
	"delete", "remove", "drop", "destroy", "merge", "force", "purge",
	"revoke", "disable", "terminate", "close", "archive", "transfer", "pay",
}

// mediumVerbs change state recoverably.
var mediumVerbs = []string{
	"create", "update", "write", "push", "comment", "add", "set", "edit",
	"post", "send", "assign", "label", "upload", "move", "rename", "patch",
}

// lowVerbs only read.
var lowVerbs = []string{
	"get", "list", "search", "read", "fetch", "find", "query", "show",
	"describe", "view", "count", "check", "diff", "log",
}

// ClassifyRisk guesses a capability's risk from its name (§10.3).
//
// It is a heuristic, and it is deliberately biased: an unrecognised capability
// is medium, never low. Guessing low on something destructive hands an agent an
// irreversible action with no approval; guessing high on something harmless
// costs one click. The `risk_overrides` block in aurium.yaml exists because a
// heuristic will be wrong sometimes and the user must be able to correct it.
func ClassifyRisk(name string) Risk {
	// Deliberately NOT lowercased first: splitIdentifier needs the case
	// boundaries to split getPullRequest, and lowercases each word itself.
	words := splitIdentifier(name)

	// High wins outright: "delete_and_list" is destructive whatever else it does.
	for _, v := range highVerbs {
		if hasWord(words, v) {
			return RiskHigh
		}
	}
	for _, v := range mediumVerbs {
		if hasWord(words, v) {
			return RiskMedium
		}
	}
	for _, v := range lowVerbs {
		if hasWord(words, v) {
			return RiskLow
		}
	}
	return RiskMedium
}

func hasWord(words []string, want string) bool {
	for _, w := range words {
		if w == want {
			return true
		}
	}
	return false
}

// containsWord matches a verb as a whole word within snake_case, kebab-case,
// camelCase or a dotted name, so "get_repository" matches "get" but
// "budget_report" does not match "get".
func containsWord(name, word string) bool {
	return hasWord(splitIdentifier(name), word)
}

// splitIdentifier breaks an identifier into lowercase words.
func splitIdentifier(s string) []string {
	var parts []string
	var cur strings.Builder

	flush := func() {
		if cur.Len() > 0 {
			parts = append(parts, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	for i, r := range s {
		switch {
		case r == '_' || r == '-' || r == '.' || r == '/' || r == ' ':
			flush()
		case r >= 'A' && r <= 'Z':
			// A capital starts a new word in camelCase, but not mid-acronym.
			if i > 0 && cur.Len() > 0 {
				prev := rune(s[i-1])
				if !(prev >= 'A' && prev <= 'Z') {
					flush()
				}
			}
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return parts
}
