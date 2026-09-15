package api

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

// The dashboard is a tree of ES modules, and go:embed silently skips nothing
// here but would skip a directory whose name began with "_" or ".". A missing
// module is a blank page with one console error, which is exactly the failure
// this test exists to make loud.
func TestEveryDashboardModuleIsEmbedded(t *testing.T) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"index.html", "style.css", "aurium-v2.css", "app.js",
		"logo.svg", "logo-square.svg",
		"lib/api.js", "lib/dom.js", "lib/state.js", "lib/stream.js", "lib/theme.js", "lib/dialog.js",
		"views/home.js", "views/workspace.js", "views/rail.js", "views/heartbeat.js",
		"views/chat.js", "views/repos.js", "views/approvals.js", "views/usage.js",
		"views/providers.js", "views/events.js", "views/setup.js",
	}
	for _, name := range want {
		if _, err := fs.Stat(sub, name); err != nil {
			t.Errorf("%s is not embedded: %v", name, err)
		}
	}
}

// Every module app.js imports must actually be reachable over HTTP. The
// single-page fallback serves index.html for unknown paths, so a mistyped
// import would answer 200 with HTML — the browser would refuse it as a module
// and the page would be blank with no server-side sign of trouble.
func TestModuleImportsResolveToJavaScript(t *testing.T) {
	h := newHarness(t)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	var modules []string
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".js") {
			modules = append(modules, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) < 10 {
		t.Fatalf("found only %d modules; the dashboard has more than that", len(modules))
	}

	for _, m := range modules {
		res := h.do("GET", "/"+m, "", "")
		body := make([]byte, 64)
		n, _ := res.Body.Read(body)
		res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Errorf("GET /%s = %d", m, res.StatusCode)
			continue
		}
		// The fallback would answer HTML here, which a browser refuses to
		// execute as a module.
		if strings.Contains(string(body[:n]), "<!doctype") {
			t.Errorf("GET /%s served the index fallback, not the module", m)
		}
		if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
			t.Errorf("GET /%s content-type = %q; a module needs a JavaScript type", m, ct)
		}
	}
}

// The page carries the host token, so it must never be cached, and the token
// must be in the body rather than in the URL.
func TestIndexCarriesTheTokenAndIsNotCached(t *testing.T) {
	h := newHarness(t)

	res := h.do("GET", "/", "", "")
	defer res.Body.Close()
	body, err := readBody(res)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(body, hostToken) {
		t.Fatal("the dashboard must be served with the host token injected")
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("a page carrying a credential must not be cached: %q",
			res.Header.Get("Cache-Control"))
	}
	// The module entry point has to be a module, or none of the imports load.
	if !strings.Contains(body, `type="module"`) {
		t.Fatal("app.js must be loaded as a module")
	}
}
