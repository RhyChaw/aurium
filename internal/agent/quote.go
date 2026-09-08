package agent

import "strings"

// shellQuote wraps a value in single quotes for `sh -lc`. Model names and
// prompts come from config and from agents, so they are never interpolated
// raw into a command line.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
