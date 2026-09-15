// Package config loads and validates aurium.yaml (§12.2).
//
// Unknown keys are errors, not warnings. A typo in a grant, an
// env_passthrough entry or a risk override would otherwise be silently
// ignored, and those are exactly the settings where silence is dangerous.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Filename is the config file Aurium looks for at a project root.
const Filename = "aurium.yaml"

// SchemaVersion is the only version this build understands.
const SchemaVersion = 1

// Agent placement decides where the agent's own process runs. It is not the
// driver: the container is unchanged either way, and still runs the agent's
// commands. Only the process that talks to the model moves.
const (
	// PlacementInContainer runs the agent inside its container, as Aurium
	// always has. The container must then be sized for a working agent.
	PlacementInContainer = "in-container"
	// PlacementHost runs the agent on this machine, at roughly a fifth of the
	// memory, with its shell denied and its commands routed back into the
	// container through aurium_exec.
	PlacementHost = "host"
)

// Config is the whole of aurium.yaml.
type Config struct {
	Version      int                    `yaml:"version"`
	Project      Project                `yaml:"project"`
	Sandbox      Sandbox                `yaml:"sandbox"`
	Hooks        Hooks                  `yaml:"hooks"`
	Stack        Stack                  `yaml:"stack"`
	Snapshot     SnapshotPolicy         `yaml:"snapshot"`
	Agents       Agents                 `yaml:"agents"`
	Integrations map[string]Integration `yaml:"integrations"`
	Grants       []Grant                `yaml:"grants"`

	// Path is where this config was loaded from; not part of the file.
	Path string `yaml:"-"`
}

type Project struct {
	Name       string `yaml:"name"`
	BaseBranch string `yaml:"base_branch"`
	// Context lists directories indexed for aurium_context_query (§8.6).
	Context []string `yaml:"context"`
}

type Sandbox struct {
	Driver           string             `yaml:"driver"`
	Image            string             `yaml:"image"`
	Agent            string             `yaml:"agent"`
	AgentPlacement   string             `yaml:"agent_placement"`
	Resources        Resources          `yaml:"resources"`
	IdlePauseMinutes int                `yaml:"idle_pause_minutes"`
	Ports            []Port             `yaml:"ports"`
	Volumes          Volumes            `yaml:"volumes"`
	Env              map[string]string  `yaml:"env"`
	EnvPassthrough   []string           `yaml:"env_passthrough"`
	Services         map[string]Service `yaml:"services"`
}

type Resources struct {
	CPUs   float64 `yaml:"cpus"`
	Memory string  `yaml:"memory"`
	PIDs   int     `yaml:"pids"`
}

type Port struct {
	Internal int    `yaml:"internal"`
	Env      string `yaml:"env"`
}

type Volumes struct {
	// PerSandbox volumes are cloned on fork/stack: they hold derived state
	// (node_modules, target/) that belongs to one container.
	PerSandbox []string `yaml:"per_sandbox"`
	// Shared volumes are caches mounted into every container of the project
	// and never cloned.
	Shared []SharedVolume `yaml:"shared"`
}

type SharedVolume struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

type Service struct {
	Image string            `yaml:"image"`
	Env   map[string]string `yaml:"env"`
	Ports []Port            `yaml:"ports"`
	// Snapshot includes this sidecar's volume in container snapshots, which
	// requires pausing it too (§6.2).
	Snapshot bool `yaml:"snapshot"`
}

type Hooks struct {
	PostCreate []string `yaml:"post_create"`
	PostSync   []string `yaml:"post_sync"`
	PreDestroy []string `yaml:"pre_destroy"`
}

type Stack struct {
	AutoSync              bool `yaml:"auto_sync"`
	Autostash             bool `yaml:"autostash"`
	IdleSecondsBeforeSync int  `yaml:"idle_seconds_before_sync"`
	Push                  Push `yaml:"push"`
}

type Push struct {
	Remote         string `yaml:"remote"`
	ForceWithLease bool   `yaml:"force_with_lease"`
}

type SnapshotPolicy struct {
	AutoOn   []string `yaml:"auto_on"`
	KeepLast int      `yaml:"keep_last"`
	// IncludeIgnored defaults to false: gitignored build output is derivable
	// and would bloat every snapshot (§5.2).
	IncludeIgnored bool `yaml:"include_ignored"`
}

type Agents struct {
	Master     AgentPreset `yaml:"master"`
	Worker     AgentPreset `yaml:"worker"`
	Delegation Delegation  `yaml:"delegation"`
}

type AgentPreset struct {
	Adapter string `yaml:"adapter"`
	Model   string `yaml:"model"`
}

type Delegation struct {
	Mode     string `yaml:"mode"`
	MaxDepth int    `yaml:"max_depth"`
}

type Integration struct {
	MCP string `yaml:"mcp"`
	URL string `yaml:"url"`
	// RiskOverrides corrects the name-based classifier (§10.3) per capability.
	RiskOverrides map[string]string `yaml:"risk_overrides"`
}

type Grant struct {
	Integration string `yaml:"integration"`
	Capability  string `yaml:"capability"`
	Subject     string `yaml:"subject"`
	Mode        string `yaml:"mode"`
}

// KnownDrivers and KnownAdapters are validated at load so a typo fails at
// `aurium init` rather than at container creation.
var (
	KnownDrivers  = []string{"docker", "podman", "local"}
	KnownAdapters = []string{"claude", "codex", "shell", "custom"}
	KnownModes    = []string{"allow", "deny", "approve"}
)

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c, err := Parse(body)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	c.Path = path
	return c, nil
}

// Parse decodes and validates config bytes.
func Parse(body []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	// §12.2: unknown keys are errors.
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}

	c.Normalize()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) Normalize() {
	if c.Sandbox.Driver == "" {
		c.Sandbox.Driver = "docker" // D2
	}
	if c.Sandbox.Agent == "" {
		c.Sandbox.Agent = "claude"
	}
	if c.Sandbox.AgentPlacement == "" {
		c.Sandbox.AgentPlacement = PlacementInContainer
	}
	if c.Sandbox.IdlePauseMinutes == 0 {
		c.Sandbox.IdlePauseMinutes = 15
	}
	if c.Snapshot.KeepLast == 0 {
		c.Snapshot.KeepLast = 10
	}
	if len(c.Snapshot.AutoOn) == 0 {
		c.Snapshot.AutoOn = []string{"task_transition", "pre_sync", "pre_restore"}
	}
	if c.Agents.Delegation.Mode == "" {
		c.Agents.Delegation.Mode = "fork"
	}
	if c.Agents.Delegation.MaxDepth == 0 {
		// Workers cannot delegate in Phase C (§9.4).
		c.Agents.Delegation.MaxDepth = 1
	}
	if c.Stack.Push.Remote == "" {
		c.Stack.Push.Remote = "origin"
	}
	if c.Stack.IdleSecondsBeforeSync == 0 {
		c.Stack.IdleSecondsBeforeSync = 90
	}
}

func (c *Config) Validate() error {
	if c.Version != SchemaVersion {
		return fmt.Errorf("version %d is not supported (this build understands version %d)",
			c.Version, SchemaVersion)
	}
	if c.Project.Name == "" {
		return fmt.Errorf("project.name is required")
	}
	if c.Project.BaseBranch == "" {
		return fmt.Errorf("project.base_branch is required")
	}
	if !slices.Contains(KnownDrivers, c.Sandbox.Driver) {
		return fmt.Errorf("sandbox.driver %q is unknown (want one of %v)", c.Sandbox.Driver, KnownDrivers)
	}
	if !slices.Contains(KnownAdapters, c.Sandbox.Agent) {
		return fmt.Errorf("sandbox.agent %q is unknown (want one of %v)", c.Sandbox.Agent, KnownAdapters)
	}
	switch c.Sandbox.AgentPlacement {
	case PlacementInContainer, PlacementHost:
	default:
		return fmt.Errorf("sandbox.agent_placement is %q; it must be %q or %q",
			c.Sandbox.AgentPlacement, PlacementInContainer, PlacementHost)
	}

	// A volume declared both per-sandbox and shared is ambiguous: fork would
	// not know whether to clone it or mount it.
	shared := map[string]bool{}
	for _, v := range c.Sandbox.Volumes.Shared {
		if v.Name == "" || v.Path == "" {
			return fmt.Errorf("sandbox.volumes.shared entries need both name and path")
		}
		shared[v.Name] = true
	}
	for _, name := range c.Sandbox.Volumes.PerSandbox {
		if shared[name] {
			return fmt.Errorf("volume %q is declared both per_sandbox and shared; it must be one or the other", name)
		}
	}

	for i, g := range c.Grants {
		if g.Integration == "" || g.Capability == "" || g.Subject == "" {
			return fmt.Errorf("grants[%d] needs integration, capability and subject", i)
		}
		if !slices.Contains(KnownModes, g.Mode) {
			return fmt.Errorf("grants[%d].mode %q is unknown (want one of %v)", i, g.Mode, KnownModes)
		}
	}

	for _, p := range c.Sandbox.Ports {
		if p.Internal <= 0 || p.Internal > 65535 {
			return fmt.Errorf("sandbox.ports: %d is not a valid port", p.Internal)
		}
	}
	return nil
}

// agentKeyPrefixes are the credential names an agent adapter legitimately
// needs inside a container. Everything else that looks like a secret is worth
// a warning (§10.6).
var agentKeyPrefixes = []string{
	"ANTHROPIC_", "CLAUDE_", "OPENAI_", "CODEX_",
}

var secretHints = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL"}

// Warnings returns non-fatal problems worth telling the user about. `aurium
// doctor` prints them; `aurium init` prints them once.
func (c *Config) Warnings() []string {
	var out []string

	for _, name := range c.Sandbox.EnvPassthrough {
		upper := strings.ToUpper(name)

		isAgentKey := false
		for _, p := range agentKeyPrefixes {
			if strings.HasPrefix(upper, p) {
				isAgentKey = true
				break
			}
		}
		if isAgentKey {
			continue
		}
		for _, hint := range secretHints {
			if strings.Contains(upper, hint) {
				out = append(out, fmt.Sprintf(
					"sandbox.env_passthrough: %s looks like a third-party credential. "+
						"Aurium keeps integration secrets on the host and reaches them through the "+
						"MCP gateway (§10.6); passing one into a container gives every agent there "+
						"direct, unaudited use of it.", name))
				break
			}
		}
	}

	if c.Snapshot.IncludeIgnored {
		out = append(out, "snapshot.include_ignored is on: gitignored build output will be "+
			"captured in every snapshot, which grows storage quickly (§5.2).")
	}
	return out
}

// Find locates aurium.yaml by walking up from dir, so commands work from any
// subdirectory of a project, including inside a container worktree.
func Find(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(abs, Filename)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("config: no %s found in %s or any parent directory", Filename, dir)
		}
		abs = parent
	}
}
