# Aurium

An operating environment for parallel coding agents. The fundamental unit is an
**isolated, snapshot-able, stackable container**: a git worktree, a derived
image, per-container volumes, and one agent.

Built from [`Aurium_ERD_v0.2.md`](Aurium_ERD_v0.2.md).

## Why

Running several coding agents on one repository normally means they trip over
each other: one worktree, one git index, one `node_modules`. Aurium gives each
agent its own container and makes the relationships between them explicit —
a container can be *stacked* on another and kept up to date by rebasing onto a
**recorded base**.

## Status

| Phase | Scope | State |
|---|---|---|
| **A** | Runtime core: worktrees, guard hooks, drivers, recorded-base sync, adapters, CLI | **done** |
| **B** | Daemon, HTTP API + SSE, snapshots, fork/stack, dashboard v0 | **done** |
| **C** | Context engine, IPC, MCP gateway, delegation | **done** |
| **D (part)** | Tauri shell, dashboard v1, provider accounts, usage | **done** |
| D (rest) | Plugins, podman parity, sidecars, provider proxy | out of scope |

## Quick start

```sh
make build

cd ~/code/my-app
./bin/aurium init                       # writes aurium.yaml, installs guard hooks
./bin/aurium container create A         # a container on branch A
./bin/aurium container create B --parent A   # B stacked on A
./bin/aurium tree                       # the forest, with live sync status

# ... A moves ...
./bin/aurium sync B                     # replay B's own commits onto A's tip

./bin/aurium snapshot A --label v1      # capture source, rootfs and volumes
./bin/aurium restore A 1                # put it all back
./bin/aurium stack C --on A             # a child that tracks A
./bin/aurium fork A --branch A-copy     # an independent peer

./bin/aurium dashboard                  # live web UI at 127.0.0.1:7770

# Several repositories worked on together
cat > ~/work/aurium.project.yaml <<'YAML'
version: 1
project: { name: my-product }
repos:
  - path: ./api
  - path: ./web
YAML
cd ~/work/api && ./bin/aurium init     # joins the project above, not a new one

# The accounts agents run on. The credential goes to your OS keyring; the
# database keeps a reference to it and nothing else.
./bin/aurium provider detect            # what this host already has
./bin/aurium provider connect anthropic --label work < key.txt
./bin/aurium provider connect openai --from-cli-login --kind subscription
./bin/aurium usage --window 24h --by agent

# Context agents share, versioned and permissioned
./bin/aurium context set task/objective "Add OAuth with PKCE"
./bin/aurium context query "token lifetime"

# Capabilities, not credentials
export GITHUB_TOKEN=...
./bin/aurium integration connect github \
    --mcp "npx -y @modelcontextprotocol/server-github" --secret-env GITHUB_TOKEN
./bin/aurium integration capabilities github   # who may call what
./bin/aurium integration grant github --deny B # revoke one container
./bin/aurium approve                           # what is waiting on you

./bin/aurium events                     # the audit trail behind all of it
```

No Docker? `aurium init --driver local --agent shell` runs everything on the
host with no isolation — useful for trying the model out and for the test
suite.

## The ideas that carry their weight

**The recorded base.** A container remembers the exact parent commit it was
last built on. Its own commits are therefore precisely those after that
commit, so `git rebase --onto <newParentTip> <recordedBase>` replays exactly
its work and nothing else. `git rebase <parent>` looks equivalent until the
parent amends a commit — then patch-id matching fails, the child's stale copy
gets replayed on top of the amended one, and it conflicts on the very file the
parent just fixed. `internal/stack/recordedbase_test.go` is that case.

**The guard hook.** Containers share the host's `.git` at an identical path.
A `reference-transaction` hook vetoes any update to a branch that is not the
container's own, so a child can never mutate a parent (Invariant 1).

**Environment is derived, never patched.** A container's environment is a pure
function of (parent snapshot, source tree, declared hooks). Sync and restore
re-derive it. Aurium never attempts a filesystem-layer rebase.

**Snapshots exclude process memory.** CRIU is Linux-only and absent from
Docker Desktop and OrbStack, so a snapshot is (git tree, rootfs image, volume
archives, context version). Agent *conversation* state survives anyway,
because it lives in the container's `$HOME`.

**One MCP server per agent.** Agents never reach an integration directly. One
gateway means one place that decides what a given agent may do, one place that
holds credentials, and one audit log. Ungranted tools are not listed at all and
calling one is indistinguishable from calling a tool that does not exist — a
tool an agent can see is a tool it will try.

**A project is a set of repositories, and a repository resolves it.** Work
spans an API repo, a web repo and an infra repo; three unrelated projects with
three unrelated forests is the wrong shape for that. `aurium.project.yaml`
names the members, and a directory finds its project through its `repositories`
row rather than through a project root — which is the one change that lets a
repo live anywhere and still belong. A single repo with no descriptor stays a
first-class project, because requiring one would tax the first five minutes.

**Provider accounts, and an honest meter.** An agent is recorded against the
account that pays for it, so "which company, which agent" is a query rather
than a guess, and the credential stays in the OS keyring. Usage is recorded
only where a provider actually reports it — headless runs, today — and a model
with no price is stored as *unpriced* rather than as free. A cost view that
quietly under-reports is worse than one that shows the gap, so the dashboard
shows it, above the numbers.

**Compare-and-set on context.** Agents share knowledge without silently
overwriting each other: a writer states the version it read, and a stale write
is refused with the current content attached so it can reconcile. Appends
commute and need no version, which is why `append` is a separate, weaker
permission.

## Layout

```
cmd/aurium/          CLI
internal/ids/        ULID identifiers with type prefixes
internal/store/      SQLite schema and typed queries
internal/events/     event bus — audit log, dashboard feed, plugin surface
internal/gitx/       git wrapper, worktrees, guard hooks
internal/stack/      recorded-base rebase engine and the container forest
internal/runtime/    driver interface (docker, local), image builder, manager
internal/runtime/snapshot/  take, restore, fork, stack, retention
internal/agent/      adapter interface: claude, codex, shell
internal/config/     aurium.yaml
internal/api/        HTTP API, SSE, embedded dashboard
internal/daemon/     auriumd: listeners, watcher, reconcile
internal/contextengine/  versioned context, permissions, projection, search
internal/ipc/        agent messaging
internal/mcp/        JSON-RPC 2.0 + MCP, stdio server and client
internal/gateway/    the single MCP server agents see: grants, approvals, audit
internal/secrets/    keyring-backed credentials (never in the database)
internal/cli/        commands
assets/              guard hooks, Dockerfile template, mkuser.sh
```

## Development

```sh
make test          # no Docker or tmux required
make test-docker   # adds the container-runtime integration tests
make vet
```

`go test ./...` must pass on a machine with no Docker daemon and no tmux;
everything needing them sits behind a build tag.

Deviations from the ERD are recorded in
[`docs/superpowers/plans/2026-09-08-aurium-phases-abc.md`](docs/superpowers/plans/2026-09-08-aurium-phases-abc.md).
