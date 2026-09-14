package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An agent reaches GitHub through the MCP gateway under a grant (§10). Letting
// it call these would route around every one of those rules — and the clone
// route in particular writes to the filesystem as the user.
func TestGitHubRoutesAreHostOnly(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/github"},
		{"GET", "/v1/github/repos"},
		{"POST", "/v1/projects/" + h.proj.ID + "/github/clone"},
		{"POST", "/v1/projects/" + h.proj.ID + "/github/tools"},
	} {
		res := h.do(c.method, c.path, h.cToken, `{"clone_url":"https://example.com/x.git"}`)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with a container token = %d, want 403", c.method, c.path, res.StatusCode)
		}
	}
}

// Status must answer whether or not GitHub is reachable: the Providers tab
// renders "not connected" from it, and a 500 there would blank the page.
func TestGitHubStatusAlwaysAnswers(t *testing.T) {
	h := newHarness(t)
	res := h.do("GET", "/v1/github", hostToken, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 whatever the connection state", res.StatusCode)
	}
	body, _ := readBody(res)
	// Whatever it says, it must never say it in a way that carries a token.
	for _, leak := range []string{"ghp_", "gho_", "github_pat_"} {
		if strings.Contains(body, leak) {
			t.Fatalf("status leaked something that looks like a token: %s", body)
		}
	}
}

// `dest` is a path a request names. Without confinement this route would clone
// anything anywhere as the user, which is far more authority than "add a
// repository to this project" asks for.
func TestCloneDestinationIsConfinedToTheProject(t *testing.T) {
	root := t.TempDir()

	for _, dest := range []string{
		"../escape",
		"../../etc/aurium",
		filepath.Join(t.TempDir(), "elsewhere"),
	} {
		if _, err := resolveDest(root, dest); err == nil {
			t.Errorf("resolveDest(%q) must be refused", dest)
		}
	}

	inside, err := resolveDest(root, "api")
	if err != nil {
		t.Fatal(err)
	}
	if inside != filepath.Join(root, "api") {
		t.Fatalf("resolveDest = %q", inside)
	}
	// An absolute path that happens to be inside is fine; it is the escape
	// that matters, not the spelling.
	if _, err := resolveDest(root, filepath.Join(root, "web")); err != nil {
		t.Fatalf("an absolute path inside the root must be allowed: %v", err)
	}
}

func TestCloneRequiresAURL(t *testing.T) {
	h := newHarness(t)
	res := h.do("POST", "/v1/projects/"+h.proj.ID+"/github/clone", hostToken, `{}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", res.StatusCode)
	}
}

// The PR lookup is decoration on a rail that must render regardless. No
// connection, no remote, a rate limit — all of them mean "no badge".
func TestRailRendersWithoutGitHub(t *testing.T) {
	h := newHarness(t)

	// The fixture repository has no remote, which is the commonest case.
	res := h.do("GET", "/v1/projects/"+h.proj.ID+"/agents", hostToken, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the rail must render without GitHub: %d", res.StatusCode)
	}
}

// A stale askpass script is a file with a credential in it.
func TestAskpassScriptsDoNotAccumulate(t *testing.T) {
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "aurium-askpass-*"))
	if len(before) > 0 {
		t.Fatalf("askpass scripts were left behind by an earlier run: %v", before)
	}
}
