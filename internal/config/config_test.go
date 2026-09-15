package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const full = `
version: 1
project:
  name: my-app
  base_branch: main
  context:
    - ./docs
    - ~/Obsidian/MyApp
sandbox:
  driver: docker
  image: node:20-alpine
  agent: claude
  resources: { cpus: 2, memory: 2g, pids: 2048 }
  idle_pause_minutes: 15
  ports:
    - { internal: 3000, env: PORT }
  volumes:
    per_sandbox: [ node_modules ]
    shared:
      - { name: npm-cache, path: /home/aurium/.npm }
  env: { NODE_ENV: development }
  env_passthrough: [ ANTHROPIC_API_KEY ]
  services:
    db: { image: postgres:16-alpine, env: { POSTGRES_PASSWORD: postgres }, snapshot: true }
hooks:
  post_create: ["npm ci"]
  post_sync: ["npm test"]
stack:
  auto_sync: false
  idle_seconds_before_sync: 90
  push: { remote: origin, force_with_lease: true }
snapshot:
  auto_on: [ task_transition, pre_sync ]
  keep_last: 10
  include_ignored: false
agents:
  master: { adapter: codex }
  delegation: { mode: fork, max_depth: 1 }
integrations:
  github:
    mcp: "npx -y @modelcontextprotocol/server-github"
    risk_overrides: { merge_pull_request: high }
grants:
  - { integration: github, capability: "merge_*", subject: project, mode: approve }
  - { integration: github, capability: "*", subject: "role:worker", mode: deny }
`

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "aurium.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFullConfigRoundTrips(t *testing.T) {
	c, err := Load(write(t, full))
	if err != nil {
		t.Fatal(err)
	}

	if c.Project.Name != "my-app" || c.Project.BaseBranch != "main" {
		t.Fatalf("project = %+v", c.Project)
	}
	if len(c.Project.Context) != 2 {
		t.Fatalf("context paths = %v", c.Project.Context)
	}
	if c.Sandbox.Driver != "docker" || c.Sandbox.Agent != "claude" {
		t.Fatalf("sandbox = %+v", c.Sandbox)
	}
	if c.Sandbox.Resources.Memory != "2g" || c.Sandbox.Resources.CPUs != 2 {
		t.Fatalf("resources = %+v", c.Sandbox.Resources)
	}
	if len(c.Sandbox.Ports) != 1 || c.Sandbox.Ports[0].Internal != 3000 || c.Sandbox.Ports[0].Env != "PORT" {
		t.Fatalf("ports = %+v", c.Sandbox.Ports)
	}
	if len(c.Sandbox.Volumes.PerSandbox) != 1 || c.Sandbox.Volumes.PerSandbox[0] != "node_modules" {
		t.Fatalf("per-sandbox volumes = %v", c.Sandbox.Volumes.PerSandbox)
	}
	if len(c.Sandbox.Volumes.Shared) != 1 || c.Sandbox.Volumes.Shared[0].Path != "/home/aurium/.npm" {
		t.Fatalf("shared volumes = %+v", c.Sandbox.Volumes.Shared)
	}
	db, ok := c.Sandbox.Services["db"]
	if !ok || !db.Snapshot {
		t.Fatalf("services = %+v", c.Sandbox.Services)
	}
	if len(c.Hooks.PostCreate) != 1 || c.Hooks.PostCreate[0] != "npm ci" {
		t.Fatalf("hooks = %+v", c.Hooks)
	}
	if c.Snapshot.KeepLast != 10 || c.Snapshot.IncludeIgnored {
		t.Fatalf("snapshot = %+v", c.Snapshot)
	}
	if c.Agents.Delegation.MaxDepth != 1 || c.Agents.Delegation.Mode != "fork" {
		t.Fatalf("delegation = %+v", c.Agents.Delegation)
	}
	gh, ok := c.Integrations["github"]
	if !ok || gh.RiskOverrides["merge_pull_request"] != "high" {
		t.Fatalf("integrations = %+v", c.Integrations)
	}
	if len(c.Grants) != 2 || c.Grants[1].Subject != "role:worker" || c.Grants[1].Mode != "deny" {
		t.Fatalf("grants = %+v", c.Grants)
	}
}

// §12.2: "Unknown keys are errors." A silently ignored typo in a security
// setting — a grant, an env_passthrough — is the dangerous kind of bug.
func TestUnknownKeyIsAnErrorNamingTheKey(t *testing.T) {
	_, err := Load(write(t, "version: 1\nproject: {name: a, base_branch: main}\nsandbox:\n  drivr: docker\n"))
	if err == nil {
		t.Fatal("an unknown key must be rejected")
	}
	if !strings.Contains(err.Error(), "drivr") {
		t.Fatalf("the error must name the offending key, got: %v", err)
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	c, err := Load(write(t, "version: 1\nproject: {name: a, base_branch: main}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Sandbox.Driver != "docker" {
		t.Errorf("default driver = %q, want docker (D2)", c.Sandbox.Driver)
	}
	if c.Sandbox.IdlePauseMinutes != 15 {
		t.Errorf("default idle_pause_minutes = %d, want 15", c.Sandbox.IdlePauseMinutes)
	}
	if c.Snapshot.KeepLast != 10 {
		t.Errorf("default keep_last = %d, want 10", c.Snapshot.KeepLast)
	}
	if c.Snapshot.IncludeIgnored {
		t.Error("include_ignored must default to false: build output is derivable (§5.2)")
	}
	if c.Agents.Delegation.MaxDepth != 1 {
		t.Errorf("default delegation depth = %d, want 1 (workers cannot delegate in Phase C)", c.Agents.Delegation.MaxDepth)
	}
	if c.Stack.Push.Remote != "origin" {
		t.Errorf("default push remote = %q", c.Stack.Push.Remote)
	}
}

func TestMissingRequiredFieldsAreRejected(t *testing.T) {
	for name, body := range map[string]string{
		"no project name": "version: 1\nproject: {base_branch: main}\n",
		"no base branch":  "version: 1\nproject: {name: a}\n",
		"bad version":     "version: 99\nproject: {name: a, base_branch: main}\n",
	} {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestUnknownDriverAndAgentAreRejectedEarly(t *testing.T) {
	_, err := Load(write(t, "version: 1\nproject: {name: a, base_branch: main}\nsandbox: {driver: vmware}\n"))
	if err == nil || !strings.Contains(err.Error(), "vmware") {
		t.Fatalf("unknown driver must be rejected by name, got %v", err)
	}
}

// A credential passed through into a container that is not the agent's own is
// exactly what §10.6 warns about; catching it at config load is cheaper than
// at doctor time.
func TestEnvPassthroughFlagsSuspiciousNames(t *testing.T) {
	c, err := Load(write(t, `
version: 1
project: {name: a, base_branch: main}
sandbox:
  agent: claude
  env_passthrough: [ ANTHROPIC_API_KEY, AWS_SECRET_ACCESS_KEY, PORT ]
`))
	if err != nil {
		t.Fatal(err)
	}
	warnings := c.Warnings()
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "AWS_SECRET_ACCESS_KEY") {
		t.Errorf("a third-party credential in env_passthrough should warn; got %v", warnings)
	}
	if strings.Contains(joined, "ANTHROPIC_API_KEY") {
		t.Errorf("the agent's own key is expected and must not warn; got %v", warnings)
	}
	if strings.Contains(joined, "PORT") {
		t.Errorf("an ordinary variable must not warn; got %v", warnings)
	}
}

func TestVolumeNamesMustNotCollide(t *testing.T) {
	_, err := Load(write(t, `
version: 1
project: {name: a, base_branch: main}
sandbox:
  volumes:
    per_sandbox: [ node_modules ]
    shared:
      - { name: node_modules, path: /cache }
`))
	if err == nil {
		t.Fatal("a volume that is both per-sandbox and shared is ambiguous and must be rejected")
	}
}

func TestAgentPlacementDefaultsToInContainer(t *testing.T) {
	var c Config
	c.Normalize()
	if c.Sandbox.AgentPlacement != PlacementInContainer {
		t.Fatalf("an unset placement must default to %q, got %q",
			PlacementInContainer, c.Sandbox.AgentPlacement)
	}
}

func TestAgentPlacementRejectsAnUnknownValue(t *testing.T) {
	c := Config{
		Version: 1,
		Sandbox: Sandbox{AgentPlacement: "somewhere-else"},
		Project: Project{Name: "test", BaseBranch: "main"},
	}
	c.Normalize()
	if err := c.Validate(); err == nil {
		t.Fatal("an unknown placement must be rejected, not silently accepted")
	}
}

func TestAgentPlacementAcceptsHost(t *testing.T) {
	c := Config{
		Version: 1,
		Sandbox: Sandbox{AgentPlacement: PlacementHost},
		Project: Project{Name: "test", BaseBranch: "main"},
	}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatalf("host is a valid placement: %v", err)
	}
}
