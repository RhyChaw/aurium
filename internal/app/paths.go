package app

import (
	"os"
	"path/filepath"
)

// CanonicalPath resolves a path to its symlink-free absolute form.
//
// Project roots are stored in the database and looked up by exact string
// match, so the two sides must agree on spelling. They will not by default:
// on macOS /var is a symlink to /private/var, so `git rev-parse
// --show-toplevel` reports /private/var/... while filepath.Abs of the working
// directory reports /var/..., and the lookup silently misses. The same happens
// wherever a home or project directory sits behind a symlink.
//
// If the path cannot be resolved (it does not exist yet), the absolute form is
// returned so callers still get something usable.
func CanonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if _, statErr := os.Stat(abs); statErr != nil {
			return abs
		}
		return abs
	}
	return resolved
}
