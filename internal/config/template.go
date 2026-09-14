package config

import "fmt"

// Template renders the aurium.yaml written for a newly registered repository.
//
// It lives here rather than in the command that writes it because two callers
// now write one — `aurium init` and the dashboard's project creation — and a
// second copy of this string would drift from the schema it is supposed to
// demonstrate.
//
// The comments are load-bearing. This is the file a user edits next, and the
// two settings most likely to be got wrong (what belongs in env_passthrough,
// and what per_sandbox means) are the two that are explained.
func Template(name, baseBranch, driver, image, adapter string) string {
	return fmt.Sprintf(`version: 1

project:
  name: %s
  base_branch: %s
  # Directories indexed for aurium_context_query.
  context:
    - ./docs

sandbox:
  driver: %s
  image: %s
  agent: %s
  resources: { cpus: 2, memory: 2g, pids: 2048 }
  idle_pause_minutes: 15
  # Ports are published on 127.0.0.1 only.
  ports: []
  volumes:
    # Cloned on fork/stack; these hold derived, per-container state.
    per_sandbox: []
    # Mounted into every container and never cloned.
    shared: []
  env: {}
  # Only the agent's own credential belongs here, and a connected provider
  # account (Providers tab) overrides it. Integration secrets stay on the host
  # and are reached through the MCP gateway.
  env_passthrough: [ ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN ]

hooks:
  post_create: []
  post_sync: []
  pre_destroy: []

stack:
  auto_sync: false
  autostash: false
  push: { remote: origin, force_with_lease: true }

snapshot:
  auto_on: [ task_transition, pre_sync, pre_restore ]
  keep_last: 10
  # Gitignored build output is derivable; keeping it would bloat every snapshot.
  include_ignored: false
`, name, baseBranch, driver, image, adapter)
}
