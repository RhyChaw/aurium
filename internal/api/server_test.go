package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/contextengine"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/providers"
	"github.com/RhyChaw/aurium/internal/runtime"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/secrets"
	"github.com/RhyChaw/aurium/internal/store"
	"github.com/RhyChaw/aurium/internal/usage"
)

// testKeyring keeps credentials in memory for the duration of a test.
type testKeyring struct{ m map[string]string }

func (k *testKeyring) Set(service, account, secret string) error {
	k.m[service+"/"+account] = secret
	return nil
}

func (k *testKeyring) Get(service, account string) (string, error) {
	v, ok := k.m[service+"/"+account]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}

func (k *testKeyring) Delete(service, account string) error {
	delete(k.m, service+"/"+account)
	return nil
}

const hostToken = "aurh_test_host_token"

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	app    *app.App
	proj   store.Project
	repo   store.Repository
	cA     store.Container
	cToken string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := filepath.Join(t.TempDir(), "app")
	os.MkdirAll(root, 0o755)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "T"},
		{"commit", "-qm", "initial", "--allow-empty"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "aurium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	bus := events.New(st)
	ctx := context.Background()
	p, _ := st.CreateProject(ctx, "app", root)
	repo, _ := st.CreateRepository(ctx, p.ID, root, "main", "")

	sec := secrets.New(&secrets.FileFallback{Path: filepath.Join(t.TempDir(), "secrets")})
	// A fake keyring, so no test reaches the developer's real login keychain.
	sec.Keyring = &testKeyring{m: map[string]string{}}

	a := &app.App{
		Store: st, Events: bus, Home: t.TempDir(),
		Manager: &runtime.Manager{
			Store: st, Events: bus,
			Drivers:  driver.Registry{"local": driver.NewLocal()},
			Adapters: agent.DefaultRegistry(),
			HomeRoot: t.TempDir(),
		},
		Context:   contextengine.New(st, bus),
		IPC:       ipc.New(st, bus, nil),
		Secrets:   sec,
		Providers: providers.New(st, sec, bus),
		Usage:     usage.New(st, bus),
	}
	a.Providers.Home = t.TempDir()

	cA, err := st.CreateContainer(ctx, store.Container{
		ProjectID: p.ID, RepoID: repo.ID, Branch: "A", Slug: "A",
		ParentBranch: "main", BaseSHA: "abc", Driver: "local",
		Worktree: root, Status: store.ContainerRunning, OriginKind: store.OriginFresh,
	})
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := st.CreateToken(ctx, cA.ID, "", store.DefaultContainerScopes, 0)
	if err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, app: a, proj: p, repo: repo, cA: cA, cToken: plain}
	h.srv = httptest.NewServer(New(a, hostToken, nil))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) do(method, path, token, body string) *http.Response {
	h.t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func TestHealthNeedsNoToken(t *testing.T) {
	h := newHarness(t)
	// The CLI polls health to decide whether the daemon it just spawned is up,
	// before it has read a token.
	res := h.do("GET", "/v1/health", "", "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health = %d, want 200", res.StatusCode)
	}
}

func TestEveryOtherRouteRequiresAToken(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/v1/projects",
		"/v1/projects/" + h.proj.ID + "/containers",
		"/v1/containers/" + h.cA.ID,
		"/v1/events",
	} {
		res := h.do("GET", path, "", "")
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", path, res.StatusCode)
		}
	}
}

func TestBadTokenIsRejected(t *testing.T) {
	h := newHarness(t)
	res := h.do("GET", "/v1/projects", "aur_not_a_real_token", "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", res.StatusCode)
	}
}

// A leaked container token must be a bounded problem: it can reach its own
// container and nothing else.
func TestContainerTokenCannotReachAnotherContainer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	other, err := h.app.Store.CreateContainer(ctx, store.Container{
		ProjectID: h.proj.ID, RepoID: h.repo.ID, Branch: "B", Slug: "B",
		ParentBranch: "main", BaseSHA: "abc", Driver: "local",
		Worktree: h.proj.Root, Status: store.ContainerRunning, OriginKind: store.OriginFresh,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Its own container: allowed.
	res := h.do("GET", "/v1/containers/"+h.cA.ID, h.cToken, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("own container = %d, want 200", res.StatusCode)
	}

	// Somebody else's: refused.
	res = h.do("GET", "/v1/containers/"+other.ID, h.cToken, "")
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("another container = %d, want 403", res.StatusCode)
	}
}

// §14: snapshot:self must not permit snapshotting another container.
func TestScopeIsEnforcedPerRoute(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A token with only read scopes cannot create a task.
	narrow, _, err := h.app.Store.CreateToken(ctx, h.cA.ID, "", []string{store.ScopeContextRead}, 0)
	if err != nil {
		t.Fatal(err)
	}
	res := h.do("POST", "/v1/projects/"+h.proj.ID+"/tasks", narrow, `{"title":"nope"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("task creation without task:* = %d, want 403", res.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(res.Body).Decode(&body)
	if !strings.Contains(body["error"], "task:") {
		t.Errorf("the error should name the missing scope, got %q", body["error"])
	}
}

func TestHostTokenHasFullAccess(t *testing.T) {
	h := newHarness(t)
	res := h.do("POST", "/v1/projects/"+h.proj.ID+"/tasks", hostToken, `{"title":"a real task"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("host token task creation = %d, want 201", res.StatusCode)
	}
	var task store.Task
	json.NewDecoder(res.Body).Decode(&task)
	if task.Title != "a real task" || task.Status != store.TaskCreated {
		t.Fatalf("task = %+v", task)
	}
}

// D19: a state change must be in the audit log before the caller is told it
// happened.
func TestStateChangesEmitEventsBeforeResponding(t *testing.T) {
	h := newHarness(t)
	res := h.do("POST", "/v1/projects/"+h.proj.ID+"/tasks", hostToken, `{"title":"audited"}`)
	res.Body.Close()

	got, err := h.app.Events.Replay(context.Background(), 0, events.Filter{
		Types: []string{events.TaskCreated},
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the event must already be persisted when the response arrives, got %d", len(got))
	}
}

func TestTaskTransitionRejectsAnUnknownStatus(t *testing.T) {
	h := newHarness(t)
	res := h.do("POST", "/v1/projects/"+h.proj.ID+"/tasks", hostToken, `{"title":"t"}`)
	var task store.Task
	json.NewDecoder(res.Body).Decode(&task)
	res.Body.Close()

	res = h.do("PATCH", "/v1/tasks/"+task.ID, hostToken, `{"status":"teleported"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown status = %d, want 400", res.StatusCode)
	}
}

func TestTreeReturnsTheForestWithLiveSyncStatus(t *testing.T) {
	h := newHarness(t)
	res := h.do("GET", "/v1/projects/"+h.proj.ID+"/tree", hostToken, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("tree = %d", res.StatusCode)
	}
	var body struct {
		BaseBranch string `json:"base_branch"`
		Roots      []struct {
			SyncStatus string `json:"sync_status"`
		} `json:"roots"`
	}
	json.NewDecoder(res.Body).Decode(&body)
	if body.BaseBranch != "main" {
		t.Errorf("base_branch = %q", body.BaseBranch)
	}
	if len(body.Roots) != 1 || body.Roots[0].SyncStatus == "" {
		t.Fatalf("expected one root with a live sync status, got %+v", body.Roots)
	}
}

func TestMissingResourceIs404(t *testing.T) {
	h := newHarness(t)
	res := h.do("GET", "/v1/containers/c_does_not_exist", hostToken, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", res.StatusCode)
	}
}

// SSE must replay from `since` so a reconnecting client misses nothing.
func TestEventStreamReplaysFromSince(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	first, _ := h.app.Events.EmitReturning(ctx, events.Event{Type: events.TaskCreated, Actor: "human"})
	h.app.Events.Emit(ctx, events.Event{Type: events.ContainerCreated, Actor: "human"})
	h.app.Events.Emit(ctx, events.Event{Type: events.AgentStarted, Actor: "human"})

	req, _ := http.NewRequestWithContext(ctx, "GET",
		h.srv.URL+"/v1/events?since="+itoa(first.ID), nil)
	req.Header.Set("Authorization", "Bearer "+hostToken)

	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	var seen []string
	scanner := bufio.NewScanner(res.Body)
	deadline := time.Now().Add(3 * time.Second)
	for scanner.Scan() && time.Now().Before(deadline) {
		line := scanner.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var e struct {
				Type string `json:"type"`
			}
			if json.Unmarshal([]byte(data), &e) == nil {
				seen = append(seen, e.Type)
			}
		}
		if len(seen) == 2 {
			break
		}
	}
	if len(seen) != 2 {
		t.Fatalf("replay delivered %v, want the two events after the first", seen)
	}
	if seen[0] != events.ContainerCreated || seen[1] != events.AgentStarted {
		t.Fatalf("replay order wrong: %v", seen)
	}
}

// EventSource cannot set headers, so the SSE route alone accepts ?token=.
func TestEventStreamAcceptsAQueryToken(t *testing.T) {
	h := newHarness(t)
	client := &http.Client{Timeout: 2 * time.Second}

	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/events?token="+hostToken, nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("SSE with a query token = %d, want 200", res.StatusCode)
	}

	// And that concession must not extend to any other route.
	res2 := h.do("GET", "/v1/projects?token="+hostToken, "", "")
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query token on a non-SSE route = %d, want 401", res2.StatusCode)
	}
}

func TestDashboardIsServedWithTheTokenAndNoStore(t *testing.T) {
	h := newHarness(t)
	res, err := http.Get(h.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	body := make([]byte, 4096)
	n, _ := res.Body.Read(body)
	page := string(body[:n])

	if !strings.Contains(page, `name="aurium-token"`) {
		t.Error("the dashboard needs the host token injected to call the API")
	}
	if !strings.Contains(page, hostToken) {
		t.Error("token not present in the page")
	}
	// The page carries a credential, so it must never be cached.
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
