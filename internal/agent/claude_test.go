package agent

import (
	"strings"
	"testing"
)

func TestHeadlessCommandDeniesTheShellUnderHostPlacement(t *testing.T) {
	c := &Claude{}
	argv := c.HeadlessCommand("do the thing", ExecOpts{
		HostSandboxed: true, MCPConfigPath: "/tmp/mcp.json",
	})
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"--disallowedTools Bash",
		"--allowedTools mcp__aurium__exec",
		"--mcp-config /tmp/mcp.json",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv is missing %q: %v", want, argv)
		}
	}
}

// In-container placement is the default and must be untouched: the container
// is the wall there, and denying the shell inside it would cripple the agent
// for no gain.
func TestHeadlessCommandLeavesTheShellAloneInAContainer(t *testing.T) {
	c := &Claude{}
	joined := strings.Join(c.HeadlessCommand("do the thing", ExecOpts{}), " ")
	if strings.Contains(joined, "disallowedTools") {
		t.Errorf("in-container placement must not deny Bash: %s", joined)
	}
}
