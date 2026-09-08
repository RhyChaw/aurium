package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RhyChaw/aurium/internal/contextengine"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/ids"
	"github.com/RhyChaw/aurium/internal/ipc"
	"github.com/RhyChaw/aurium/internal/mcp"
	"github.com/RhyChaw/aurium/internal/store"
)

// fakeUpstream stands in for a connected MCP server and records what it was
// asked to do, so tests can assert a call did or did not reach it.
type fakeUpstream struct {
	tools []mcp.Tool
	calls []string
	// secret is the credential a real upstream would hold. Tests assert it
	// never appears anywhere an agent can reach.
	secret string
	fail   error
}

func (f *fakeUpstream) Name() string { return "fake" }
func (f *fakeUpstream) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	return f.tools, nil
}
func (f *fakeUpstream) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.ToolResult, error) {
	f.calls = append(f.calls, name)
	if f.fail != nil {
		return nil, f.fail
	}
	return mcp.TextResult("upstream did " + name), nil
}
func (f *fakeUpstream) Close() error { return nil }

type gwFixture struct {
	g        *Gateway
	store    *store.Store
	upstream *fakeUpstream
	project  store.Project
	cA, cB   store.Container
	aMaster  store.Agent
	aWorker  store.Agent
	integID  string
}

func newGateway(t *testing.T) *gwFixture {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/aurium.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	ctx := context.Background()
	p, _ := s.CreateProject(ctx, "app", "/r")
	repo, _ := s.CreateRepository(ctx, p.ID, "/r", "main", "")

	mk := func(branch, role string) (store.Container, store.Agent) {
		c, _ := s.CreateContainer(ctx, store.Container{
			ProjectID: p.ID, RepoID: repo.ID, Branch: branch, Slug: branch,
			ParentBranch: "main", BaseSHA: "abc", Driver: "local",
			Worktree: "/r/wt/" + branch, Status: store.ContainerRunning,
			OriginKind: store.OriginFresh,
		})
		a, _ := s.CreateAgent(ctx, store.Agent{
			ContainerID: c.ID, Adapter: "shell", Role: role,
			TmuxSession: "agent", Status: store.AgentRunning,
		})
		return c, a
	}
	cA, master := mk("A", store.RoleMaster)
	cB, worker := mk("B", store.RoleWorker)

	bus := events.New(s)
	cx := contextengine.New(s, bus)
	if err := cx.SeedDefaults(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	g := New(s, bus, cx, ipc.New(s, bus, nil))

	// An integration whose secret lives only on the host.
	integID := ids.New(ids.Integration)
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO integrations (id, project_id, kind, name, config_json, secret_ref, status)
		 VALUES (?,?,?,?,?,?,?)`,
		integID, p.ID, "mcp_stdio", "github", `{"cmd":"npx"}`,
		"keyring:aurium/app/github", "connected"); err != nil {
		t.Fatal(err)
	}

	up := &fakeUpstream{
		secret: "ghp_SUPER_SECRET_TOKEN",
		tools: []mcp.Tool{
			{Name: "get_repository", Description: "Read a repo"},
			{Name: "create_pull_request", Description: "Open a PR"},
			{Name: "merge_pull_request", Description: "Merge a PR"},
			{Name: "delete_repository", Description: "Delete a repo"},
		},
	}
	g.RegisterUpstream(integID, up)

	caps, err := g.RegisterCapabilities(ctx, integID, up.tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SeedGrants(ctx, integID, p.ID, caps); err != nil {
		t.Fatal(err)
	}

	return &gwFixture{g: g, store: s, upstream: up, project: p,
		cA: cA, cB: cB, aMaster: master, aWorker: worker, integID: integID}
}

func (f *gwFixture) caller(agent store.Agent, container store.Container) Caller {
	return Caller{
		Subject: Subject{
			AgentID: agent.ID, Role: agent.Role,
			ContainerID: container.ID, ProjectID: f.project.ID,
		},
		Token: store.TokenInfo{ContainerID: container.ID, Scopes: store.DefaultContainerScopes},
	}
}

func toolNames(tools []mcp.Tool) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		out[t.Name] = true
	}
	return out
}

func TestRiskSeedsProduceTheDocumentedDefaults(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()
	master := f.caller(f.aMaster, f.cA)

	cases := map[string]Mode{
		"get_repository":      ModeAllow,   // low
		"create_pull_request": ModeAllow,   // medium
		"merge_pull_request":  ModeApprove, // high
		"delete_repository":   ModeApprove, // high
	}
	for capability, want := range cases {
		got, _, err := f.g.Resolve(ctx, master.Subject, f.integID, capability)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("project default for %s = %q, want %q", capability, got, want)
		}
	}
}

// A delegated worker runs an unreviewed prompt from another agent, so it gets
// less trust than an agent a human started.
func TestWorkersGetLessTrustThanMasters(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()
	worker := f.caller(f.aWorker, f.cB)

	if got, _, _ := f.g.Resolve(ctx, worker.Subject, f.integID, "create_pull_request"); got != ModeApprove {
		t.Errorf("worker medium-risk = %q, want approve", got)
	}
	if got, _, _ := f.g.Resolve(ctx, worker.Subject, f.integID, "delete_repository"); got != ModeDeny {
		t.Errorf("worker high-risk = %q, want deny", got)
	}
}

// §10.1: ungranted tools are not listed at all. A tool an agent can see is a
// tool it will try.
func TestDeniedToolsAreInvisible(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()

	names := toolNames(mustTools(t, f.g, ctx, f.caller(f.aWorker, f.cB)))
	if names["github_delete_repository"] {
		t.Error("a denied tool must not appear in tools/list")
	}
	if !names["github_get_repository"] {
		t.Error("an allowed tool should appear")
	}
}

// §10.1: calling a hidden tool returns -32601 — indistinguishable from a tool
// that does not exist, so an agent cannot enumerate what it may not use.
func TestCallingADeniedToolLooksLikeAMissingTool(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()

	_, rpcErr := f.g.CallTool(ctx, f.caller(f.aWorker, f.cB), "github_delete_repository", nil)
	if rpcErr == nil {
		t.Fatal("a denied call must not succeed")
	}
	if rpcErr.Code != mcp.CodeMethodNotFound {
		t.Fatalf("code = %d, want %d (method not found)", rpcErr.Code, mcp.CodeMethodNotFound)
	}
	// The message must not reveal that the tool exists but is forbidden.
	if strings.Contains(strings.ToLower(rpcErr.Message), "denied") ||
		strings.Contains(strings.ToLower(rpcErr.Message), "permission") {
		t.Errorf("the error leaks that the tool exists: %q", rpcErr.Message)
	}
	if len(f.upstream.calls) != 0 {
		t.Errorf("a denied call reached the upstream: %v", f.upstream.calls)
	}
}

// §62: revoke one container without disconnecting the integration.
func TestContainerScopedDenyOverridesProjectAllow(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()
	master := f.caller(f.aMaster, f.cA)

	if got, _, _ := f.g.Resolve(ctx, master.Subject, f.integID, "get_repository"); got != ModeAllow {
		t.Fatal("precondition: project scope should allow this")
	}

	if err := f.g.RevokeForContainer(ctx, f.integID, f.cA.ID, "human"); err != nil {
		t.Fatal(err)
	}

	if got, _, _ := f.g.Resolve(ctx, master.Subject, f.integID, "get_repository"); got != ModeDeny {
		t.Fatalf("container-scoped deny did not override the project allow, got %q", got)
	}
	// And another container is unaffected: this revokes one container, not the
	// integration.
	other := f.caller(f.aWorker, f.cB)
	if got, _, _ := f.g.Resolve(ctx, other.Subject, f.integID, "get_repository"); got == ModeDeny {
		t.Error("revoking one container must not affect another")
	}
}

func TestAllowedCallReachesTheUpstreamAndIsAudited(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()

	res, rpcErr := f.g.CallTool(ctx, f.caller(f.aMaster, f.cA), "github_get_repository",
		json.RawMessage(`{"owner":"a","repo":"b"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if res.IsError {
		t.Fatalf("call failed: %+v", res)
	}
	if len(f.upstream.calls) != 1 || f.upstream.calls[0] != "get_repository" {
		t.Fatalf("upstream calls = %v", f.upstream.calls)
	}

	evs, _ := f.g.Events.Replay(ctx, 0, events.Filter{Types: []string{events.IntegrationCall}}, 10)
	if len(evs) != 1 {
		t.Fatalf("want one integration.call event, got %d", len(evs))
	}
	// Arguments routinely contain issue bodies and pasted secrets; only a hash
	// belongs in a log.
	payload := evs[0].Payload
	if payload["args_hash"] == "" || payload["args_hash"] == nil {
		t.Error("the audit entry should record an argument hash")
	}
	for _, v := range payload {
		if s, ok := v.(string); ok && strings.Contains(s, "\"owner\"") {
			t.Errorf("raw arguments were written to the audit log: %v", payload)
		}
	}
}

// §10.5: approve-mode holds the call, returns immediately, then executes on
// approval and delivers the result as a message.
func TestApproveModeHoldsThenExecutesOnApproval(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()
	master := f.caller(f.aMaster, f.cA)

	res, rpcErr := f.g.CallTool(ctx, master, "github_merge_pull_request",
		json.RawMessage(`{"pr":42}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	body := res.Content[0].Text
	if !strings.Contains(body, "pending_approval") {
		t.Fatalf("an approve-mode call should return pending, got %s", body)
	}
	// It must return immediately without touching the upstream.
	if len(f.upstream.calls) != 0 {
		t.Fatalf("a held call reached the upstream before approval: %v", f.upstream.calls)
	}

	pending, err := f.g.ListApprovals(ctx, ApprovalPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending approvals, want 1", len(pending))
	}

	if _, err := f.g.Decide(ctx, pending[0].ID, ApprovalApproved, "human"); err != nil {
		t.Fatal(err)
	}
	if len(f.upstream.calls) != 1 || f.upstream.calls[0] != "merge_pull_request" {
		t.Fatalf("approval did not execute the held call: %v", f.upstream.calls)
	}

	// The agent was told to continue, so the result must arrive as a message.
	inbox, _ := f.g.IPC.Inbox(ctx, ipc.Addr{AgentID: f.aMaster.ID}, 10)
	found := false
	for _, m := range inbox {
		if m.Type == ipc.TypeResponse && strings.Contains(m.Content, "approved") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the approval result was not delivered to the agent: %+v", inbox)
	}
}

func TestRejectionDoesNotExecute(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()

	f.g.CallTool(ctx, f.caller(f.aMaster, f.cA), "github_merge_pull_request", json.RawMessage(`{}`))
	pending, _ := f.g.ListApprovals(ctx, ApprovalPending)

	if _, err := f.g.Decide(ctx, pending[0].ID, ApprovalRejected, "human"); err != nil {
		t.Fatal(err)
	}
	if len(f.upstream.calls) != 0 {
		t.Fatalf("a rejected call was executed: %v", f.upstream.calls)
	}

	inbox, _ := f.g.IPC.Inbox(ctx, ipc.Addr{AgentID: f.aMaster.ID}, 10)
	found := false
	for _, m := range inbox {
		if strings.Contains(m.Content, "rejected") {
			found = true
		}
	}
	if !found {
		t.Error("the agent must be told its request was rejected")
	}
}

func TestApprovalCannotBeDecidedTwice(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()
	f.g.CallTool(ctx, f.caller(f.aMaster, f.cA), "github_merge_pull_request", json.RawMessage(`{}`))
	pending, _ := f.g.ListApprovals(ctx, ApprovalPending)

	f.g.Decide(ctx, pending[0].ID, ApprovalApproved, "human")
	if _, err := f.g.Decide(ctx, pending[0].ID, ApprovalRejected, "human"); err == nil {
		t.Fatal("an approval must not be decided twice")
	}
}

// §14's grep test: the upstream's credential must appear nowhere an agent can
// reach — not in the tool list, not in a result, not in the audit log.
func TestUpstreamCredentialNeverReachesTheAgent(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()
	master := f.caller(f.aMaster, f.cA)
	secret := f.upstream.secret

	tools := mustTools(t, f.g, ctx, master)
	blob, _ := json.Marshal(tools)
	if strings.Contains(string(blob), secret) {
		t.Fatal("the upstream credential appears in the tool list")
	}

	res, _ := f.g.CallTool(ctx, master, "github_get_repository", json.RawMessage(`{}`))
	body, _ := json.Marshal(res)
	if strings.Contains(string(body), secret) {
		t.Fatal("the upstream credential appears in a tool result")
	}

	evs, _ := f.g.Events.Replay(ctx, 0, events.Filter{}, 200)
	all, _ := json.Marshal(evs)
	if strings.Contains(string(all), secret) {
		t.Fatal("the upstream credential appears in the audit log")
	}

	// And the database stores a keyring reference, never the value.
	var ref string
	f.store.DB().QueryRow(`SELECT secret_ref FROM integrations WHERE id = ?`, f.integID).Scan(&ref)
	if strings.Contains(ref, secret) {
		t.Fatal("the credential was stored in the database")
	}
	if !strings.HasPrefix(ref, "keyring:") {
		t.Errorf("secret_ref should be a keyring reference, got %q", ref)
	}
}

// Delegation is master-only (§9.2): a worker that could delegate would let one
// prompt fan out without bound.
func TestDelegationToolsAreMasterOnly(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()

	workerTools := toolNames(mustTools(t, f.g, ctx, f.caller(f.aWorker, f.cB)))
	if workerTools["aurium_delegate"] || workerTools["aurium_merge"] {
		t.Error("a worker must not see the delegation tools")
	}

	masterTools := toolNames(mustTools(t, f.g, ctx, f.caller(f.aMaster, f.cA)))
	if !masterTools["aurium_delegate"] || !masterTools["aurium_merge"] {
		t.Error("a master should see the delegation tools")
	}

	// And calling it anyway is refused as an unknown tool.
	_, rpcErr := f.g.CallTool(ctx, f.caller(f.aWorker, f.cB), "aurium_delegate",
		json.RawMessage(`{"title":"x","prompt":"y"}`))
	if rpcErr == nil || rpcErr.Code != mcp.CodeMethodNotFound {
		t.Fatalf("a worker calling aurium_delegate should get method-not-found, got %v", rpcErr)
	}
}

func TestNativeToolsAreAlwaysAvailable(t *testing.T) {
	f := newGateway(t)
	names := toolNames(mustTools(t, f.g, context.Background(), f.caller(f.aWorker, f.cB)))
	for _, want := range []string{
		"aurium_context_query", "aurium_context_get", "aurium_context_append",
		"aurium_context_write", "aurium_ipc_send", "aurium_ipc_inbox",
		"aurium_ipc_ack", "aurium_task_info", "aurium_task_status", "aurium_snapshot",
	} {
		if !names[want] {
			t.Errorf("native tool %q is missing", want)
		}
	}
}

func TestInitializeAdvertisesOneServerAndInstructions(t *testing.T) {
	f := newGateway(t)
	resp := f.g.Handle(context.Background(), f.caller(f.aMaster, f.cA), &mcp.Request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize",
	})
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	init, ok := resp.Result.(mcp.InitializeResult)
	if !ok {
		t.Fatalf("result = %T", resp.Result)
	}
	if init.ServerInfo.Name != "aurium" {
		t.Errorf("server name = %q", init.ServerInfo.Name)
	}
	// The instructions carry the two rules an agent cannot infer.
	if !strings.Contains(init.Instructions, "own branch") {
		t.Error("instructions should state the branch rule")
	}
	if !strings.Contains(init.Instructions, "re-derived") {
		t.Error("instructions should state the environment rule")
	}
}

func TestUnknownMethodIsMethodNotFound(t *testing.T) {
	f := newGateway(t)
	resp := f.g.Handle(context.Background(), f.caller(f.aMaster, f.cA), &mcp.Request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/teleport",
	})
	if resp.Error == nil || resp.Error.Code != mcp.CodeMethodNotFound {
		t.Fatalf("error = %+v", resp.Error)
	}
}

func TestRemovedCapabilitiesKeepTheirGrants(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()

	// The upstream reconnects, temporarily missing a tool.
	if _, err := f.g.RegisterCapabilities(ctx, f.integID, []mcp.Tool{
		{Name: "get_repository", Description: "Read a repo"},
	}, nil); err != nil {
		t.Fatal(err)
	}

	caps, _ := f.g.ListCapabilities(ctx, f.integID)
	var removed, present int
	for _, c := range caps {
		if c.Removed {
			removed++
		} else {
			present++
		}
	}
	if removed == 0 {
		t.Error("tools missing from a reconnect should be marked removed")
	}
	// Grants survive, so a tool coming back does not need re-granting.
	grants, _ := f.g.ListGrants(ctx, f.integID)
	if len(grants) == 0 {
		t.Error("grants must survive a reconnect")
	}
}

func TestRiskOverridesFromConfigAreApplied(t *testing.T) {
	f := newGateway(t)
	ctx := context.Background()

	caps, err := f.g.RegisterCapabilities(ctx, f.integID,
		[]mcp.Tool{{Name: "get_repository"}},
		map[string]string{"get_repository": string(RiskHigh)})
	if err != nil {
		t.Fatal(err)
	}
	if caps[0].Risk != RiskHigh {
		t.Fatalf("risk override not applied: %+v", caps[0])
	}
}

func mustTools(t *testing.T, g *Gateway, ctx context.Context, c Caller) []mcp.Tool {
	t.Helper()
	tools, err := g.ToolsFor(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}
