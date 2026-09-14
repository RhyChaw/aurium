package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/project"
	"github.com/RhyChaw/aurium/internal/store"
)

// readAll drains a response body into a string for assertions about what did
// and did not come back.
func readBody(res *http.Response) (string, error) {
	b, err := io.ReadAll(res.Body)
	return string(b), err
}

func decode[T any](t *testing.T, res *http.Response) T {
	t.Helper()
	defer res.Body.Close()
	var out T
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return out
}

// gitRepoAt makes a real repository, because attaching one runs git.
func gitRepoAt(t *testing.T, dir, branch string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", branch},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "T"},
		{"commit", "-qm", "initial", "--allow-empty"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestCreateProjectSpanningRepos(t *testing.T) {
	h := newHarness(t)
	root := t.TempDir()
	gitRepoAt(t, filepath.Join(root, "api"), "main")
	gitRepoAt(t, filepath.Join(root, "web"), "main")

	body := `{"name":"Product","root":"` + root + `","driver":"local","agent":"shell",
	          "repos":[{"path":"./api"},{"path":"./web"}]}`
	res := h.do("POST", "/v1/projects", hostToken, body)
	if res.StatusCode != http.StatusCreated {
		defer res.Body.Close()
		msg, _ := readBody(res)
		t.Fatalf("create = %d: %s", res.StatusCode, msg)
	}
	out := decode[struct {
		Project      store.Project      `json:"project"`
		Repositories []store.Repository `json:"repositories"`
	}](t, res)
	if len(out.Repositories) != 2 {
		t.Fatalf("want 2 repositories, got %d", len(out.Repositories))
	}
	if out.Project.Descriptor == "" {
		t.Fatal("a multi-repo project must record where its descriptor lives")
	}

	// The descriptor is on disk and says the same thing as the database.
	if _, err := project.Load(filepath.Join(root, project.Filename)); err != nil {
		t.Fatalf("descriptor: %v", err)
	}

	res = h.do("GET", "/v1/projects/"+out.Project.ID, hostToken, "")
	got := decode[struct {
		Repositories []store.Repository `json:"repositories"`
		Standalone   bool               `json:"standalone"`
	}](t, res)
	if len(got.Repositories) != 2 || got.Standalone {
		t.Fatalf("GET project = %+v", got)
	}
}

// An agent able to register repositories could attach anything on the host.
func TestAgentsCannotCreateProjectsOrAttachRepos(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/projects"},
		{"POST", "/v1/projects/" + h.proj.ID + "/repos"},
	} {
		res := h.do(c.method, c.path, h.cToken, `{"name":"x","path":"/tmp"}`)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with a container token = %d, want 403", c.method, c.path, res.StatusCode)
		}
	}
}

func TestAttachingARepoTwiceIsANoOp(t *testing.T) {
	h := newHarness(t)
	repo := gitRepoAt(t, filepath.Join(t.TempDir(), "extra"), "main")
	body := `{"path":"` + repo + `","driver":"local","agent":"shell"}`

	first := decode[store.Repository](t, h.do("POST",
		"/v1/projects/"+h.proj.ID+"/repos", hostToken, body))
	second := decode[store.Repository](t, h.do("POST",
		"/v1/projects/"+h.proj.ID+"/repos", hostToken, body))

	if first.ID != second.ID {
		t.Fatalf("re-attaching created a second row: %s then %s", first.ID, second.ID)
	}
}

func TestProjectAgentsCarryTheirColourBand(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for _, status := range []string{
		store.AgentStarting, store.AgentRunning, store.AgentIdle, store.AgentBlocked,
	} {
		a, err := h.app.Store.CreateAgent(ctx, store.Agent{
			ContainerID: h.cA.ID, Adapter: "shell", Role: store.RolePrimary, Status: status,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.app.Store.UpdateAgentStatus(ctx, a.ID, status); err != nil {
			t.Fatal(err)
		}
	}

	out := decode[struct {
		Agents []AgentTile `json:"agents"`
	}](t, h.do("GET", "/v1/projects/"+h.proj.ID+"/agents", hostToken, ""))

	seen := map[string]int{}
	for _, tile := range out.Agents {
		seen[tile.State]++
		if tile.Container.Branch != "A" {
			t.Fatalf("a tile must know its container: %+v", tile.Container)
		}
	}
	want := map[string]int{
		StateStarting: 1, StateWorking: 1, StateDone: 1, StateAttention: 1,
	}
	for state, n := range want {
		if seen[state] != n {
			t.Errorf("state %q appeared %d times, want %d (%v)", state, seen[state], n, seen)
		}
	}
}

// Reading a transcript must not consume the agent's inbox: at-least-once
// delivery means "delivered" is a claim about the recipient, and a human
// reading over its shoulder is not the recipient.
func TestReadingATranscriptDoesNotDeliverMessages(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	a, err := h.app.Store.CreateAgent(ctx, store.Agent{
		ContainerID: h.cA.ID, Adapter: "shell", Role: store.RolePrimary,
	})
	if err != nil {
		t.Fatal(err)
	}

	res := h.do("POST", "/v1/agents/"+a.ID+"/message", hostToken, `{"content":"please rebase"}`)
	if res.StatusCode != http.StatusCreated {
		defer res.Body.Close()
		msg, _ := readBody(res)
		t.Fatalf("send = %d: %s", res.StatusCode, msg)
	}
	res.Body.Close()

	out := decode[struct {
		Messages []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
			From    struct {
				AgentID string `json:"agent,omitempty"`
			} `json:"from"`
		} `json:"messages"`
	}](t, h.do("GET", "/v1/agents/"+a.ID+"/messages", hostToken, ""))

	if len(out.Messages) != 1 || out.Messages[0].Content != "please rebase" {
		t.Fatalf("transcript = %+v", out.Messages)
	}
	if out.Messages[0].Status != "queued" {
		t.Fatalf("reading must not deliver: status = %q", out.Messages[0].Status)
	}
	// A human's message has no sending agent; that absence is how a reader
	// tells it from another agent's.
	if out.Messages[0].From.AgentID != "" {
		t.Fatalf("a dashboard message must have no sending agent, got %q", out.Messages[0].From.AgentID)
	}
}

// authenticate() only checks the {container} path value, and these routes carry
// an {agent}. Without an explicit check an agent token could read any other
// agent's transcript.
func TestAgentTokenCannotReadAnotherContainersAgent(t *testing.T) {
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
	foreign, err := h.app.Store.CreateAgent(ctx, store.Agent{
		ContainerID: other.ID, Adapter: "shell", Role: store.RolePrimary,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/v1/agents/" + foreign.ID,
		"/v1/agents/" + foreign.ID + "/messages",
	} {
		res := h.do("GET", path, h.cToken, "")
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s with a foreign container token = %d, want 403", path, res.StatusCode)
		}
	}
	res := h.do("POST", "/v1/agents/"+foreign.ID+"/message", h.cToken, `{"content":"hi"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("sending to a foreign agent = %d, want 403", res.StatusCode)
	}
}

func TestProviderRoutesAreHostOnly(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/providers"},
		{"POST", "/v1/providers"},
		{"GET", "/v1/providers/detect"},
		{"DELETE", "/v1/providers/pa_x"},
		{"GET", "/v1/usage"},
		{"GET", "/v1/usage/series"},
	} {
		res := h.do(c.method, c.path, h.cToken, `{}`)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with a container token = %d, want 403", c.method, c.path, res.StatusCode)
		}
	}
}

// The API must never be able to hand back a credential it was given.
func TestConnectingAnAccountNeverEchoesTheCredential(t *testing.T) {
	h := newHarness(t)

	res := h.do("POST", "/v1/providers", hostToken,
		`{"provider":"anthropic","auth_kind":"api_key","label":"work","secret":"sk-ant-SECRETVALUE"}`)
	if res.StatusCode != http.StatusCreated {
		defer res.Body.Close()
		msg, _ := readBody(res)
		t.Fatalf("connect = %d: %s", res.StatusCode, msg)
	}
	acct := decode[store.ProviderAccount](t, res)
	if acct.SecretRef == "" {
		t.Fatal("the account must name where its credential lives")
	}

	listed := h.do("GET", "/v1/providers", hostToken, "")
	defer listed.Body.Close()
	body, _ := readBody(listed)
	if strings.Contains(body, "SECRETVALUE") {
		t.Fatal("the credential came back through the API")
	}

	// Disconnecting removes both the row and the credential.
	res = h.do("DELETE", "/v1/providers/"+acct.ID, hostToken, "")
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("disconnect = %d, want 204", res.StatusCode)
	}
}

func TestDuplicateAccountLabelIsAConflict(t *testing.T) {
	h := newHarness(t)
	body := `{"provider":"anthropic","auth_kind":"api_key","label":"work","secret":"sk-ant-x"}`
	h.do("POST", "/v1/providers", hostToken, body).Body.Close()

	res := h.do("POST", "/v1/providers", hostToken, body)
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("a repeated label = %d, want 409", res.StatusCode)
	}
}

// Detect reports whether a credential exists, never what it is.
func TestDetectNamesVariablesOnly(t *testing.T) {
	h := newHarness(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-SECRETVALUE")

	res := h.do("GET", "/v1/providers/detect", hostToken, "")
	defer res.Body.Close()
	body, _ := readBody(res)
	if !strings.Contains(body, "ANTHROPIC_API_KEY") {
		t.Fatal("detect must name the variable that is set")
	}
	if strings.Contains(body, "SECRETVALUE") {
		t.Fatal("detect leaked a credential")
	}
}

func TestUsageReportSeparatesUnpricedTokens(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.app.Store.RecordUsage(ctx, store.UsageEvent{
		ProjectID: h.proj.ID, Provider: store.ProviderAnthropic, Model: "claude-opus-5",
		Kind: store.UsageExec, InputTokens: 1000, OutputTokens: 100, CostUSD: 0.5, Priced: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.Store.RecordUsage(ctx, store.UsageEvent{
		ProjectID: h.proj.ID, Provider: store.ProviderOpenAI, Model: "mystery",
		Kind: store.UsageExec, InputTokens: 400, OutputTokens: 40,
	}); err != nil {
		t.Fatal(err)
	}

	out := decode[struct {
		Totals store.UsageGroup              `json:"totals"`
		Groups map[string][]store.UsageGroup `json:"groups"`
	}](t, h.do("GET", "/v1/usage?window=24h", hostToken, ""))

	if out.Totals.CostUSD != 0.5 {
		t.Fatalf("cost = %v, want only the priced call", out.Totals.CostUSD)
	}
	if out.Totals.UnpricedTokens != 440 {
		t.Fatalf("unpriced = %d, want 440", out.Totals.UnpricedTokens)
	}
	if len(out.Groups["provider"]) != 2 {
		t.Fatalf("grouped by provider = %+v", out.Groups["provider"])
	}
	// A project group with no name is a group nobody can read.
	if out.Groups["project"][0].Label != "app" {
		t.Fatalf("project group label = %q", out.Groups["project"][0].Label)
	}
}

func TestUsageWindowIsValidated(t *testing.T) {
	h := newHarness(t)
	for _, w := range []string{"forever", "-1h", "100000h"} {
		res := h.do("GET", "/v1/usage?window="+w, hostToken, "")
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("window=%s = %d, want 400", w, res.StatusCode)
		}
	}
}

func TestHeartbeatCountsWhatTheRailPaints(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for _, status := range []string{store.AgentRunning, store.AgentBlocked} {
		a, _ := h.app.Store.CreateAgent(ctx, store.Agent{
			ContainerID: h.cA.ID, Adapter: "shell", Role: store.RolePrimary,
		})
		if err := h.app.Store.UpdateAgentStatus(ctx, a.ID, status); err != nil {
			t.Fatal(err)
		}
	}

	hb := decode[Heartbeat](t, h.do("GET", "/v1/heartbeat", hostToken, ""))
	if hb.Agents[StateWorking] != 1 || hb.Agents[StateAttention] != 1 {
		t.Fatalf("agents = %v", hb.Agents)
	}
	if hb.Projects != 1 {
		t.Fatalf("projects = %d", hb.Projects)
	}
	if hb.TS == "" {
		t.Fatal("the heartbeat must be timestamped, or a stale one looks live")
	}
}

// The heartbeat counts every project's agents and every pending approval. An
// agent has no use for a map of the whole fleet, and handing it one is a free
// reconnaissance report.
func TestHeartbeatIsHostOnly(t *testing.T) {
	h := newHarness(t)
	res := h.do("GET", "/v1/heartbeat", h.cToken, "")
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("heartbeat with a container token = %d, want 403", res.StatusCode)
	}
}

// The chat pane needs two facts the agent's status cannot give it: whether a
// turn is in flight right now, and whether this agent can answer at all.
//
// `running` means the agent is alive, which a shell agent is forever — showing
// a thinking indicator for all of it makes the indicator mean nothing, and
// showing a composer that files messages nowhere is a box that lies.
func TestAgentReportsWhetherItCanChat(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	shell, err := h.app.Store.CreateAgent(ctx, store.Agent{
		ContainerID: h.cA.ID, Adapter: "shell", Role: store.RolePrimary,
	})
	if err != nil {
		t.Fatal(err)
	}
	claude, err := h.app.Store.CreateAgent(ctx, store.Agent{
		ContainerID: h.cA.ID, Adapter: "claude", Role: store.RoleWorker,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Both alive, so status cannot be what distinguishes them.
	for _, id := range []string{shell.ID, claude.ID} {
		if err := h.app.Store.UpdateAgentStatus(ctx, id, store.AgentRunning); err != nil {
			t.Fatal(err)
		}
	}

	type detail struct {
		CanChat  bool `json:"can_chat"`
		Thinking bool `json:"thinking"`
	}
	got := decode[detail](t, h.do("GET", "/v1/agents/"+shell.ID, hostToken, ""))
	if got.CanChat {
		t.Fatal("the shell adapter has no model; claiming it can chat offers a composer that does nothing")
	}
	if got.Thinking {
		t.Fatal("a running shell agent is not mid-turn")
	}

	got = decode[detail](t, h.do("GET", "/v1/agents/"+claude.ID, hostToken, ""))
	if !got.CanChat {
		t.Fatal("the claude adapter runs headless, so it can answer in the pane")
	}
	if got.Thinking {
		t.Fatal("no turn was started, so nothing is thinking")
	}

	// The rail carries the same two facts, so a tile and its pane cannot
	// disagree about whether the agent is busy.
	tiles := decode[struct {
		Agents []AgentTile `json:"agents"`
	}](t, h.do("GET", "/v1/projects/"+h.proj.ID+"/agents", hostToken, ""))

	byID := map[string]AgentTile{}
	for _, tile := range tiles.Agents {
		byID[tile.Agent.ID] = tile
	}
	if byID[shell.ID].CanChat || !byID[claude.ID].CanChat {
		t.Fatalf("the rail disagrees with the pane: shell=%v claude=%v",
			byID[shell.ID].CanChat, byID[claude.ID].CanChat)
	}
}

// Spawning is the other half of a loop the dashboard could only half do:
// make a project, then be unable to put anything in it.
func TestSpawnAgentCreatesTheContainerItNeeds(t *testing.T) {
	h := newHarness(t)

	res := h.do("POST", "/v1/projects/"+h.proj.ID+"/agents", hostToken,
		`{"adapter":"shell","name":"Scout"}`)
	if res.StatusCode != http.StatusCreated {
		defer res.Body.Close()
		msg, _ := readBody(res)
		t.Fatalf("spawn = %d: %s", res.StatusCode, msg)
	}
	out := decode[struct {
		Agent     store.Agent     `json:"agent"`
		Container store.Container `json:"container"`
	}](t, res)

	if out.Agent.DisplayName != "Scout" {
		t.Fatalf("the name the user gave must stick: %q", out.Agent.DisplayName)
	}
	if out.Container.Branch == "" || out.Container.Worktree == "" {
		t.Fatalf("an agent needs a branch and a worktree of its own: %+v", out.Container)
	}
	if out.Container.Branch == h.repo.BaseBranch {
		t.Fatal("an agent must not be put on the base branch, which is the one thing it could break")
	}

	// Clicking twice must not fail on a name the user never chose.
	second := h.do("POST", "/v1/projects/"+h.proj.ID+"/agents", hostToken, `{"adapter":"shell"}`)
	defer second.Body.Close()
	if second.StatusCode != http.StatusCreated {
		msg, _ := readBody(second)
		t.Fatalf("a second spawn = %d: %s", second.StatusCode, msg)
	}
}
