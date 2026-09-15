package api

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A committed API document that nothing checks is a document that goes stale
// the first time somebody adds a route. This test is the only thing that makes
// api/openapi.yaml worth reading: it fails when the code and the spec diverge
// in either direction.
func TestOpenAPIMatchesRegisteredRoutes(t *testing.T) {
	spec, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("api/openapi.yaml is missing: %v", err)
	}

	documented := documentedRoutes(t, string(spec))
	registered := registeredRoutesFromSource(t)

	if len(registered) == 0 {
		t.Fatal("found no registered routes; the extractor needs updating")
	}

	for _, r := range registered {
		if !documented[r] {
			t.Errorf("route %q is registered but not described in api/openapi.yaml", r)
		}
	}
	for r := range documented {
		if !contains(registered, r) {
			t.Errorf("route %q is described in api/openapi.yaml but not registered", r)
		}
	}
}

var routeRe = regexp.MustCompile(`"(GET|POST|PATCH|PUT|DELETE) (/[^"]*)"`)

// registeredRoutesFromSource reads server.go rather than the mux, because
// net/http's ServeMux exposes no way to enumerate its patterns.
func registeredRoutesFromSource(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range routeRe.FindAllStringSubmatch(string(body), -1) {
		out = append(out, strings.ToLower(m[1])+" "+m[2])
	}
	sort.Strings(out)
	return out
}

// documentedRoutes parses the subset of YAML this file uses: a `paths:` block
// of `  /path:` keys, each with `    method:` children. A full YAML parser
// would be a dependency for one test.
func documentedRoutes(t *testing.T, spec string) map[string]bool {
	t.Helper()
	methods := map[string]bool{
		"get": true, "post": true, "patch": true, "put": true, "delete": true,
	}

	out := map[string]bool{}
	inPaths := false
	currentPath := ""

	for _, line := range strings.Split(spec, "\n") {
		if strings.HasPrefix(line, "paths:") {
			inPaths = true
			continue
		}
		if !inPaths {
			continue
		}
		// A new top-level key ends the paths block.
		if len(line) > 0 && line[0] != ' ' && line[0] != '#' {
			break
		}

		if strings.HasPrefix(line, "  /") && strings.HasSuffix(strings.TrimSpace(line), ":") {
			currentPath = strings.TrimSuffix(strings.TrimSpace(line), ":")
			continue
		}
		if currentPath == "" {
			continue
		}
		if strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") {
			key := strings.TrimSuffix(strings.TrimSpace(line), ":")
			if methods[key] {
				out[key+" "+currentPath] = true
			}
		}
	}
	return out
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
