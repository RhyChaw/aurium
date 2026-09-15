# Stubs and gaps

Every place Aurium returns a canned value, skips an ERD section, or claims
more than it has been shown to do. Maintained as one table on purpose: gaps
scattered through code comments are gaps nobody reads.

Last audited: 2026-09-14, after the dashboard v1, agent-chat and GitHub changes.

## Never executed against a container runtime

**Nothing in this repository has ever run against a real container.** Docker
Desktop on this machine could not start (it crashed during the review), so
every phase gate below is **NOT RUN**, not "passing".

| Gate | Status |
|---|---|
| A: 5 containers × 50 concurrent commits, `git fsck` clean, 20 runs | **not run** — no Docker |
| B: snapshot → mutate rootfs + volume + tracked + untracked → restore reverts all four | **not run** for rootfs and volumes; git-tree half covered by unit tests with the `local` driver |
| B: stack → parent commit → stale → sync → rebased, `base_sha` advanced | run on the **local driver only** |
| C: ungranted tool → `-32601`; approve holds then executes; container-scope deny; credential grep | run against a **stub MCP server** on the local driver, not a container |

The `local` driver runs processes on the host with no isolation. It exercises
the git, context, IPC and gateway layers, and exercises nothing about images,
volumes, rootfs snapshots, bind mounts, uid mapping or tmux.

## Build-tagged tests

**There are none.** `make test-docker` runs `go test -tags docker ./...`,
which matches no files and is identical to `make test`. The claim that
Docker-dependent tests exist behind a tag was false.

## Stubs and unfinished mechanisms

| Area | What is missing | Consequence |
|---|---|---|
| `aurium-mcp` in images | **Fixed 2026-09-14.** `make build` depends on `shim`, which cross-compiles both architectures and installs them to `~/.aurium/bin/aurium-mcp-linux-<arch>`; the builder searches there, beside the running binary, and in a checkout's `bin/`. It is still not embedded in the CLI as the ERD intends — a checkout that has never run `make build` still has no shim, and the error now says so and names the command. | Closed for anyone who builds with make. |
| Docker path end to end | Still **never run**. The shim gap is fixed, but no image has been built and no container started on this machine — Docker Desktop is not running here either. | The `local` driver is the proven path. |
| Adapter verification | The `claude` and `codex` `Prepare()` paths are written from memory. No agent has ever been observed loading them. No nightly smoke job exists (§15 requires one). | Every container may be silently disconnected from Aurium. |
| `notifications/tools/list_changed` | The handshake advertises `listChanged: true`; the gateway never emits the notification, and the stdio shim has no server→client channel to carry one. | An agent that listed tools before a grant change keeps a stale list for its whole session. Revoking a container mid-task (§62) does not take effect until the agent restarts. |
| Upstream lifecycle | `RegisterUpstream` is called once at daemon start. No health check, no reconnect, no re-list. | If an upstream exits mid-session, every later call returns its death error until the daemon restarts. |
| Approvals state machine | States are `pending → approved\|rejected\|expired`. There is no `executing`, and the executed result is **not** written in the same transaction as the RESPONSE message. | A daemon killed between "approved" and the upstream call leaves an approval marked approved that never ran, and no message. See "Crash windows". |
| Two writers | Bridged by `leases` (this change). The CLI still writes SQLite in-process and runs git directly. | Correct under the lease, but D17 ("the daemon is the only writer") is still false. |
| `sync --env` | `SyncEnv` exists but has no test and has never run; it needs a container. | Unproven. |
| Sidecars (`services:`) | Parsed from `aurium.yaml`, never started. | `services.db` is accepted and ignored. |
| `snapshot.include_ignored` | Honoured for the git tree. Volume archiving is untested. | Partial. |
| Admission control (§13 wk10) | Not implemented. `containers.status` has `queued`; nothing sets it. | No memory budget. |
| `aurium pr` (§12.1) | Not implemented. | Phase C gate's "PR created through the gateway" was never demonstrated end to end. |
| `aurium push`, `reparent`, `logs`, `agent message/exec` | Not implemented. | Listed in §12.1, absent. `agent message` is reachable through the dashboard and `POST /v1/agents/{a}/message`, just not the CLI. |
| Local driver across a restart | **Fixed 2026-09-14.** Its registry is in memory, so every container made before a daemon restart reported "runtime object not found" — with the row in the database and the worktree on disk. Reconcile now adopts them, and runs once at startup rather than on the first 30s tick. | Closed. |
| `internal/mcp` flake | `TestClientReportsUpstreamExitWithItsOutput` fails intermittently — the spawned `/bin/sh` exits before its output is read, so the error carries "(no output)". Passes on retry; unrelated to any change made on 2026-09-14. | **A flaky test nobody has fixed.** |
| Driver default | `config.Template` hard-coded `driver: docker`. On a machine with Docker stopped that made a project whose every agent failed, several steps in, with a message about a missing file. Project creation now picks a driver that `Available()` says works, and `Manager.Create` refuses up front with a message naming the fix. | Closed. |
| Stopping a daemon on Windows | `aurium daemon stop` signals the pid with SIGTERM, which Go does not support on Windows. | The command reports the failure rather than pretending; native Windows is a non-goal through Phase D anyway. |
| Podman driver | `NewDocker("podman")` exists; the three documented flag differences are not handled. | Untested. |
| `api/openapi.yaml` | Committed, with `internal/api/openapi_test.go` failing in either direction. | **closed** |
| Dashboard v1 (§11.3) | Approvals, Agents and per-project surfaces shipped (2026-09-14). **Context browsing and the integrations grants matrix are still absent** — the gateway's grants can only be read and changed through `aurium integration`. | Two of the four v1 tabs remain CLI-only. |
| GitHub PR badges | Cached for 60s per (repo, branch). A badge can be a minute stale, and a rate-limited or slow GitHub shows no badge rather than no rail. | Decoration, by design. |
| GitHub agent tools | `POST /v1/projects/{p}/github/tools` spawns `@modelcontextprotocol/server-github` behind the gateway. **The upstream server itself has never been observed running** — the same gap as every other integration, since the Docker path has never worked end to end. The route reports whatever the handshake says. | Unproven end to end. |
| Interactive-session usage | Only headless runs are metered, and only for the `claude` adapter, whose JSON envelope is parsed. A REPL turn surfaces nothing the daemon can read, so it is not counted. The Usage tab says so on the page. | **Spend shown is a lower bound.** ERD §9.1's base-URL provider proxy is the only thing that would close this, and it is Phase D. |
| Provider subscription login | Connecting a seat means running the provider CLI's own login (`claude setup-token`, `codex login`) and handing Aurium the token, or pointing it at the login already on the host. There is no OAuth client Aurium drives. | Two steps, not one. Recorded as D25 rather than hidden. |
| `claude` usage parsing | `internal/agent/usage.go` parses the documented `--output-format json` envelope. **Written from the docs; no run has been observed**, exactly like the rest of the claude adapter. | An adapter change upstream silently stops metering. Unit-tested against a synthetic envelope only. |
| Price table | `internal/usage/pricing.go` holds list prices verified 2026-05. Discounts, batch pricing, cache tiers and long-context tiers are not modelled. | Costs are indicative. The table's date is shown in the UI. |
| Linters | `errcheck` and `staticcheck` are wired into `make lint` but **cannot load this module** under Go 1.27 — errcheck silently reports nothing and exits 0. | A green `make lint` currently means nothing. |

## Crash windows

| Window | What the human sees today |
|---|---|
| Daemon killed between `approved` and the upstream call | The approval reads `approved` with a `decided_at`. The call never ran. No RESPONSE message was sent, so the agent waits forever. Nothing retries on restart. **This is the state machine gap above.** |
| Daemon killed mid-snapshot | Partial archives on disk; the `snapshots` row may be absent. No cleanup pass exists (the plan called for one). |
| CLI killed mid-sync | The lease is released by `defer`, but a `SIGKILL` skips it; the container is blocked until the 5-minute TTL expires. |

## ERD sections not implemented

§9.1 base-URL provider proxy (Phase D — and the only route to exact usage for
interactive agents) · §11.3 dashboard v1's Context and Integrations tabs ·
§12.1 `push`, `pr`, `reparent`, `logs`, `agent` subcommands · §13 admission
control · §6.5 `--env` sync unproven · sidecars · podman parity ·
`aurium graph`.

## Added after the ERD

These are not gaps; they are decisions taken beyond the RFC, recorded here so
the two documents do not silently disagree. Their reasoning is in
`documents/superpowers/specs/2026-09-14-agent-os-dashboard-design.md`.

| | |
|---|---|
| **D22** | A project is a set of repositories described by `aurium.project.yaml`. The ERD listed multi-repo as a non-goal "the runtime does not support"; that is still true of a single *container*, which remains one repo. What changed is that a project may now span several. |
| **D23** | A directory resolves to its project through its `repositories` row, not through `projects.root`. |
| **D24** | `provider_accounts` records which account an agent runs on; the credential stays in the keyring. |
| **D25** | Subscription login is the provider CLI's own login, captured. |
| **D26** | Usage is recorded only where a provider reports it, and the gap is shown rather than hidden. |
| **D27** | The dashboard is ES modules loaded natively — still no Node in `go build`. |
| **D28** | GitHub's token is borrowed from `gh auth token`, not stored. A second copy of a credential the machine already holds is a second thing to revoke and a second thing to go stale. It is re-read on every daemon start because `gh` rotates it. |
| **D29** | A chat turn runs the adapter's headless command rather than typing into a REPL. On the `local` driver there is no session to type into, so a message sat in an inbox nothing would read; a headless turn works on every driver and produces the token counts the meter needs. It does not replace `aurium attach`. |

There is no CLI for GitHub yet: `aurium provider` and `aurium usage` cover
provider accounts and spend, but listing GitHub repositories, cloning one into
a project and enabling agent tools are dashboard-only. `aurium integration
connect github` does the last of those the long way.

`aurium provider` and `aurium usage` cover both from the terminal, so neither
surface is the one that really works (D21). Creating a *project* across several
repositories is still dashboard-only: `aurium init` joins a repo to a project
whose descriptor already exists, but nothing writes a new descriptor from the
CLI.
