# Stubs and gaps

Every place Aurium returns a canned value, skips an ERD section, or claims
more than it has been shown to do. Maintained as one table on purpose: gaps
scattered through code comments are gaps nobody reads.

Last audited: 2026-09-08, after the Phase A–C review.

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
| `aurium-mcp` in images | Not embedded in the CLI; `image.Builder.MCPBinary` points at `~/.aurium/bin/aurium-mcp`, which nothing populates. `make shim` cross-compiles it but nothing installs it there. | **The docker driver cannot build an image at all.** `image.Ensure` fails reading the binary. This is almost certainly why the Docker path has never worked end to end. |
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
| `aurium push`, `reparent`, `logs`, `agent message/exec` | Not implemented. | Listed in §12.1, absent. |
| Podman driver | `NewDocker("podman")` exists; the three documented flag differences are not handled. | Untested. |
| `api/openapi.yaml` | Does not exist. The plan said a test would keep routes in sync with it. | No API contract document. |
| Dashboard v1 (§11.3) | Agents, Context, Integrations matrix and Approvals inbox tabs are absent. | v0 only: containers, tasks, events. |
| Linters | `errcheck` and `staticcheck` are wired into `make lint` but **cannot load this module** under Go 1.27 — errcheck silently reports nothing and exits 0. | A green `make lint` currently means nothing. |

## Crash windows

| Window | What the human sees today |
|---|---|
| Daemon killed between `approved` and the upstream call | The approval reads `approved` with a `decided_at`. The call never ran. No RESPONSE message was sent, so the agent waits forever. Nothing retries on restart. **This is the state machine gap above.** |
| Daemon killed mid-snapshot | Partial archives on disk; the `snapshots` row may be absent. No cleanup pass exists (the plan called for one). |
| CLI killed mid-sync | The lease is released by `defer`, but a `SIGKILL` skips it; the container is blocked until the 5-minute TTL expires. |

## ERD sections not implemented

§9.1 base-URL provider proxy (Phase D) · §11.3 dashboard v1 · §12.1 `push`,
`pr`, `reparent`, `logs`, `agent` subcommands · §13 admission control ·
§6.5 `--env` sync unproven · sidecars · podman parity · `aurium graph`.
