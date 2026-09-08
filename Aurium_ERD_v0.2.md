# Aurium — Engineering Requirements & Technical Architecture RFC v0.2

**Status:** Draft for engineering review
**Date:** 2026-09-07
**Supersedes:** StackBox ERD v0.1 (2026-09-03). Mechanisms from that document that survive are restated here in compressed form so this RFC stands alone; decisions keep their original numbers (D1–D12) and new ones continue from D13.
**Answers:** the eight items the Aurium spec §76 names as "the next document": filesystem layering model, container runtime choice, snapshot format, context database/schema, IPC protocol, agent adapter API, MCP permission model, first repo/package structure.

---

## 0. Executive summary

Aurium re-frames StackBox from "a CLI that runs parallel agents in worktrees" to "an operating environment whose fundamental unit is an isolated, snapshot-able, stackable container, with context, IPC, and integrations layered on top." The re-framing is right. The StackBox runtime survives unchanged as Aurium's bottom layer: it is what makes "container" mean something concrete (git worktree + derived image + bind mounts at identical paths + tmux + guard hooks + recorded-base rebase).

What Aurium adds, and the concrete mechanism this RFC assigns to each:

| Aurium primitive | Mechanism |
|---|---|
| Snapshot | git tree of the worktree (tracked + untracked) under `refs/aurium/snap/…` + `docker commit` of the rootfs + tar of per-container volumes + context version. **Not** process memory. |
| Fork / Stack / Sync | Fork = new sibling lineage from a snapshot. Stack = child whose git parent is the container branch and whose rootfs/volumes start from the parent snapshot. Sync = the StackBox recorded-base rebase for source; environment is *re-derived*, never delta-patched. |
| Context (project / container / agent, with permissions and transactions) | SQLite (`~/.aurium/aurium.db`) with versioned items and compare-and-set commits, projected into each container as a generated file the agent's own conventions already read, and queried dynamically through an MCP server Aurium ships. |
| Agent IPC | Messages table + `aurium_ipc_*` MCP tools + a tmux nudge for interactive agents. At-least-once, explicit ack. |
| Master / worker | Delegation = fork a worker container from the master's snapshot, run the worker there, hand the branch back. One interactive agent per container by default. |
| Integrations / MCP / capabilities / secrets | An **MCP gateway** in the daemon. Agents see exactly one MCP server (`aurium`); the gateway proxies to upstream MCP servers, filters tools by per-(container, agent) grants, holds `approve`-mode calls for a human, and keeps every credential on the host. |
| Event bus / observability | `events` table + in-process pub/sub + SSE endpoint. Everything above emits events. |
| Desktop | A daemon (`auriumd`) with an HTTP/JSON + SSE API; the CLI is the first client; a web dashboard embedded in the daemon is the second. A native shell (Tauri) wraps the dashboard later. The API is also the cloud boundary. |

The §72 "MVP" list is roughly six months for two engineers. It decomposes into four phases; Phase A (the StackBox core) alone already solves the original pain point, and the §73 killer demo needs Phases A–C (~20 weeks). §2 gives the breakdown.

---

## 1. Evaluation of the Aurium spec

### 1.1 Findings

| # | Spec section / claim | Verdict | What this RFC does |
|---|---|---|---|
| 1 | §0, §15 — snapshots include "process metadata … agent state"; §51/§71 Inv. 9 — environment reconstructable from a snapshot | **Process state cannot be captured portably.** CRIU (`docker checkpoint`) is Linux-only, experimental, and unavailable on Docker Desktop/OrbStack. | Define snapshot precisely (§6): filesystem + git + volumes + context version + agent config. Invariant 9 becomes "reconstructible", not "resumable mid-instruction". Agent *conversation* state does survive because it lives in the container's `$HOME`, which `docker commit` captures. |
| 2 | §18 — copy-on-write stacking of container state | **Half true.** Docker image layers are CoW, so rootfs stacking from a parent snapshot is free. Volumes (deps, databases) are not CoW on macOS; git already gives CoW for source. | Rootfs via snapshot image layers; source via git; volumes cloned by tar on fork/stack (seconds to a minute; documented). |
| 3 | §19 — "Sync computes the state delta and applies it to B. Fundamentally different from blindly merging a Git branch." | **For source, the best delta engine that exists is git.** Filesystem-layer deltas do not rebase (a layer is a diff against a specific parent image). | Source sync = `git rebase --onto <parent-tip> <recorded-base>` (StackBox engine, unchanged). Environment sync = re-derive from the parent's new snapshot + the child's declared hooks (§6.5). Ad-hoc environment mutations the child made are lost unless declared; the child's context tells agents this. |
| 4 | §23–25, §58 — master + four workers "all operate within Container C-184 and share its context" | **Reintroduces the collision the product exists to remove**: several agents editing one worktree and one git index concurrently. | One interactive agent per container. Delegation forks a worker container from the master's snapshot (§9.4). Serial in-place workers are allowed as an explicit mode. An agent's *own* subagents (Claude Code's built-in ones) are the agent's business. |
| 5 | §22 — adapter interface has `getContext()` | Context is the runtime's job, not the adapter's; putting it in the adapter re-couples context to providers, which §65 forbids. | Adapter gets `Prepare()` (write projection + MCP config into the container `$HOME`) and nothing else about context (§9.1). |
| 6 | §29–35, §66 — capabilities, not credentials; per-container/per-agent enable/disable; approval on dangerous actions | **Right, and there is a clean mechanism**: an MCP proxy. Both Claude Code and Codex speak MCP; one gateway server per container with server-side filtering gives capability-level permissions and host-only secrets without any agent-specific code. | §10. |
| 7 | §9, §28 — context permissions READ/WRITE/APPEND/PROPOSE/COMMIT and versioned transactions | Right; small to build. COMMIT is not a permission, it is the outcome of WRITE (direct) or PROPOSE (reviewed). | Four permissions, compare-and-set on version (§8). |
| 8 | §72 — MVP includes a desktop application, snapshots, stacking, two adapters, master/worker, IPC, MCP management, event stream, project graph | **~6 months for two engineers, not an MVP.** A native desktop app first would starve the runtime. | Phase the work (§2). Daemon + API + embedded web dashboard is the "desktop" until Phase D. |
| 9 | §46–48 — scheduler, resource declarations, cost | Fine; not MVP. | Admission control (memory budget) in Phase B; priorities and cost in Phase D. |
| 10 | §36–38 — Obsidian, retrieval layer, knowledge graph | The graph already exists as SQLite relations (task→container→snapshot→PR); retrieval is FTS5 over context items and indexed docs. No embeddings needed to start. | §8.6. |
| 11 | §54–55 — cloud runtime "same protocol" | Right if the protocol is the daemon API and the remote side owns its own clone. Bind-mounted worktrees do not exist remotely. | API is the boundary (§11). Remote driver remains post-1.0; the `Driver` interface carries a `Filesystem: shared|remote` flag from day one. |
| 12 | §14 — "infinite containers" | Fine. Storage is the real limit: snapshots × volumes. | Retention policy and `aurium snapshot gc` (§6.7). |
| 13 | §53 — abstract the virtualization mechanism | Right. Docker/Podman CLI first (D2). Apple's `container`/Containerization on macOS 26 and microVM runtimes are candidate drivers later; verify maturity before committing. | §5.1. |
| 14 | §13 — lifecycle includes PAUSED→SNAPSHOT | Snapshot must quiesce; `docker pause` does that for rootfs and volumes. | §6.2 |
| 15 | §71 Inv. 1 — child cannot mutate parent | Enforced for git by the guard hook (own branch only) and for rootfs/volumes by the fact that they are copies. | §5.5 |

### 1.2 Concept mapping (StackBox → Aurium)

| StackBox | Aurium | change |
|---|---|---|
| `sbx` | `aurium` (alias `aur`) | rename |
| sandbox | container (`c_…`) | now has task, parent snapshot, agents, context |
| stack (branch forest) | stack | same engine; parent is a container, not just a branch |
| `stackbox.yaml` | `aurium.yaml` | schema extended (§12.2) |
| `.stackbox/state.json` + flock | `~/.aurium/aurium.db` (SQLite, WAL) owned by `auriumd` | one writer, no flock |
| `.stackbox/` in repo | `.aurium/` in repo (worktrees, hooks, projections) | rename |
| `sbx watch` | `auriumd` (always-on per user, auto-started by the CLI) | required by gateway/events |
| agent adapter (image layer + launch) | adapter (+ `Prepare`, `Execute`, `Status`) | §9.1 |
| — | snapshot, fork, restore | new |
| — | context engine, IPC, gateway, events, API, dashboard | new |

---

## 2. Scope and phasing

Two engineers (E1 runtime/git, E2 daemon/agents/gateway). Weeks are calendar weeks with slack.

| phase | weeks | delivers | proves |
|---|---|---|---|
| **A — Runtime core** (StackBox ERD M0–M4, renamed) | 1–7 | `aurium init/create/attach/exec/tree/sync/push/destroy/doctor`; docker + local drivers; guard hooks; recorded-base cascade; `claude` + `shell` adapters | N agents on one repo without state leakage (the original pain point) |
| **B — Daemon, snapshots, stacking, dashboard v0** | 8–13 | `auriumd` + API + SQLite + events; snapshot/fork/stack/restore; watcher/auto-sync/idle pause; web dashboard (containers, tree, events, attach); admission control | Stack B on A, update A, B unchanged, sync A→B, restore after an agent wrecks its environment |
| **C — Context, IPC, gateway, delegation** | 14–21 | context engine + projection + `aurium` MCP server; IPC; MCP gateway with grants and approvals; secrets in OS keyring; `codex` adapter; master/worker delegation via fork; task lifecycle; PR artifact | The §73 demo end to end |
| **D — Platform** | 22–26+ | priorities/cost, Obsidian + doc indexing, sidecars per container, Tauri shell, plugin API (event subscriptions + integration providers), podman | Open-source release |

Non-goals through D: cloud runtime, native Windows, multi-repo containers (data model supports it; runtime does not), embeddings-based retrieval, marketplace.

---

## 3. Design decisions

Retained from StackBox ERD v0.1 without change: **D1** Go, shell out to `git`; **D2** Docker/Podman CLI first; **D3** identical-path bind mounts; **D4** worktrees inside the repo (`.aurium/wt/<slug>`); **D5** `sleep infinity` + `--init`, agent in tmux; **D6** persisted `base_sha`; **D7** rebase on the host, in the child's worktree, topological order; **D8** hard sync preconditions; **D9** guard hook via env-set `core.hooksPath`; **D10** no git credentials in containers; **D11** ports published to `127.0.0.1:0`.
Amended: **D12** state moves from `state.json` to SQLite owned by the daemon (from Phase B; Phase A uses the same schema in-process).

**D13 — Source of truth is git; environment is derived.** A container's environment is a pure function of (parent snapshot image, source tree, `aurium.yaml` hooks). Sync and restore re-derive it. Aurium never attempts filesystem-layer rebases.

**D14 — Snapshot = (git tree, rootfs image, volume archives, context version, agent config), taken while paused.** Excludes process memory. Restorable on any machine with the same driver.

**D15 — One interactive agent per container.** Concurrency happens across containers (fork), not inside one worktree.

**D16 — Agents integrate with Aurium through two channels only: a generated instruction file and one MCP server.** No provider-specific protocol. A new agent adapter is: image layer, launch command, where it reads instructions, how it loads MCP config.

**D17 — The daemon is the only writer to the database and the only holder of secrets.** CLI, dashboard, containers, and cloud all go through the API. Containers hold a scoped bearer token, never a credential.

**D18 — Permissions are default-deny at the capability level, resolved project → container → agent, most specific wins, with modes `allow | deny | approve`.**

**D19 — Every state transition emits an event before the API call returns.** Events are the audit log, the dashboard feed, and the plugin surface.

**D20 — SQLite via `modernc.org/sqlite` (pure Go).** Keeps static, cross-compiled binaries. WAL mode, `busy_timeout=5000`.

**D21 — The daemon's HTTP API is the product boundary.** Local dashboard, CLI, Tauri shell, plugins, and a future cloud runtime are all clients of the same API.

---

## 4. Architecture

### 4.1 Processes

```
 HOST
 ┌──────────────────────────────────────────────────────────────────────────┐
 │  aurium (CLI)   web dashboard (browser)   [Tauri shell, Phase D]         │
 │        │                │                                                │
 │        ▼                ▼   HTTP/JSON + SSE, bearer token                │
 │  ┌──────────────────────────────────────────────────────────────────┐    │
 │  │ auriumd  (one per user; auto-started by the CLI)                 │    │
 │  │  api ─ store(SQLite) ─ events ─ scheduler ─ watcher              │    │
 │  │  runtime: drivers(docker|podman|local) ─ image ─ snapshot        │    │
 │  │  stack engine ─ context engine ─ ipc ─ gateway(MCP) ─ secrets    │    │
 │  │  upstream MCP servers spawned/connected here (creds from keyring)│    │
 │  └───────────────┬─────────────────────────────┬────────────────────┘    │
 │                  │ docker CLI / engine          │ http://host.docker.internal:7770
 │   ┌──────────────▼──────────┐    ┌─────────────▼────────────┐            │
 │   │ container c_184         │    │ container c_185          │            │
 │   │ tmux:agent (claude)     │    │ tmux:agent (codex)       │            │
 │   │ aurium-mcp (stdio shim) │    │ aurium-mcp (stdio shim)  │            │
 │   │ worktree  /…/.aurium/wt/│    │ worktree                 │            │
 │   │ $HOME: agent cfg, MCP   │    │ $HOME                    │            │
 │   └─────────────────────────┘    └──────────────────────────┘            │
 │   shared, guarded: <repo>/.git      per-container: volumes, network      │
 └──────────────────────────────────────────────────────────────────────────┘
```

- `aurium` — CLI. If `~/.aurium/auriumd.sock` is absent it starts `auriumd` detached, waits for `/v1/health`, then proceeds.
- `auriumd` — daemon. Listens on Unix socket `~/.aurium/auriumd.sock` (host clients) and TCP `127.0.0.1:7770`. On Linux Docker Engine it also binds the aurium bridge gateway address so containers can reach it via `host.docker.internal`; every request needs a bearer token. Single writer to SQLite.
- `aurium-mcp` — 3 MB static Go binary copied into every derived image. Runs as a stdio MCP server for the agent and forwards JSON-RPC to `auriumd` over HTTP with the container's token. Exists so that every MCP client works regardless of HTTP-transport support.

### 4.2 Repository / package structure (monorepo `github.com/<org>/aurium`)

```
cmd/aurium/            CLI (cobra)
cmd/auriumd/           daemon
cmd/aurium-mcp/        in-container stdio→HTTP MCP shim (built for linux/amd64, linux/arm64; embedded in cmd/aurium)
internal/api/          HTTP handlers, SSE, auth, OpenAPI (api/openapi.yaml is the source; handlers generated with oapi-codegen)
internal/store/        SQLite schema, migrations (embedded SQL), typed queries (sqlc)
internal/events/       bus + persistence
internal/runtime/
  driver/              interface + docker/ podman/ local/
  image/               derived-image build + cache
  snapshot/            take/restore/fork/stack
  ports/ sidecars/
internal/gitx/         exec wrapper (from StackBox)
internal/stack/        forest + recorded-base cascade (from StackBox)
internal/context/      items, versions, grants, proposals, projection, fts
internal/ipc/          messages, delivery, nudges
internal/gateway/      MCP server (aurium tools) + upstream proxy + grants + approvals
internal/agent/        adapter interface + claude/ codex/ shell/ custom/
internal/secrets/      keyring adapter (zalando/go-keyring), fallback age-encrypted file
internal/scheduler/    admission control, priorities (Phase D)
internal/config/       aurium.yaml + JSON Schema
web/                   dashboard (Vite + React + TypeScript), built into internal/api/dist via go:embed
assets/                Dockerfile template, hooks, mkuser.sh
docs/  schema/  testdata/
```

Third-party Go deps: cobra, `modelcontextprotocol/go-sdk`, `modernc.org/sqlite`, sqlc (build-time), oapi-codegen (build-time), `zalando/go-keyring`, Bubble Tea only for `aurium tree --watch`.

---

## 5. Runtime layer (the StackBox core, restated)

### 5.1 Container runtime choice

Docker Engine / Docker Desktop / OrbStack via the `docker` CLI; Podman via `podman` with three flag differences. The `Driver` interface (StackBox §4.6) gains:

```go
Snapshot(ctx, c, dest SnapshotDest) (RootfsRef, error)   // docker commit → image
Restore(ctx, c, from RootfsRef) error                      // recreate container from image
CloneVolumes(ctx, from, to *Container) error
Capabilities() DriverCaps  // Filesystem: shared|remote, Pause bool, Checkpoint bool (CRIU), Sidecars bool
```

Future drivers evaluated against the same interface: Apple `container` (macOS 26 Containerization — verify OCI/volume/pause support before committing), Firecracker/Cloud Hypervisor microVMs (cloud runtime), a `remote` driver (`Filesystem: remote`, clone-based).

### 5.2 Filesystem layering model

```
 read-only, shared              ┌──────────────────────────────────────────┐
 across containers              │ L0  base image        node:20-alpine     │  pulled
                                │ L1  aurium layer      git tmux bash      │  built once per (base digest, uid, agent)
                                │                       aurium-mcp, agent  │
                                ├──────────────────────────────────────────┤
 read-only, per lineage         │ L2  parent snapshot   docker commit of   │  present for stack/fork/restore;
                                │                       parent rootfs      │  absent for a fresh container
                                ├──────────────────────────────────────────┤
 writable, per container        │ L3  container rootfs  overlay; $HOME     │  agent config, transcripts, ad-hoc installs
                                └──────────────────────────────────────────┘
 mounts (per container unless noted)
   <repo>/.git                       shared, rw, guarded by hooks          identical host path
   <repo>/.aurium/wt/<slug>          worktree, rw                          identical host path
   <repo>/.aurium/hooks              ro                                    identical host path
   <wt>/node_modules … (declared)    named volume, cloned on fork/stack
   /home/aurium/.npm … (declared)    shared cache volume (repo-wide)
   /run/aurium/token                 ro file: bearer token (or env AURIUM_TOKEN)
 network
   per-container bridge network aurium-<repo>-<slug> (sidecars join by alias); ports → 127.0.0.1:0
```

Source is never in an image layer. Build outputs written into the worktree (`dist/`, `.next/`) live on the bind mount and are captured by the git-tree part of a snapshot only if not gitignored; gitignored outputs are derivable and are excluded by default (`snapshot.include_ignored: false`).

### 5.3 Container creation (docker driver)

Unchanged from StackBox §4.6.1 except: labels are `aurium.project`, `aurium.container`, `aurium.task`; `-e AURIUM_TOKEN=<scoped>` `-e AURIUM_URL=http://host.docker.internal:7770`; `--network aurium-<repo>-<slug>`; image is `aurium-local/<base>:<hash>` or, for stack/fork/restore, `aurium-snap/<container>:<seq>`.

Post-create sequence: `docker port` → record ports → `adapter.Prepare()` writes MCP config + instruction projection into `$HOME` → `hooks.post_create` via `docker exec sh -lc` → `adapter.Start()` (tmux) → events `container.created`, `agent.started`.

### 5.4 Worktrees, identical paths, git config, ports, sidecars, idle pause

As StackBox ERD §4.5, §4.9, §4.13, §4.14, with `.stackbox` → `.aurium`. Summary of the parts an implementer must not skip:

- `git worktree add [--relative-paths] -b <branch> <repo>/.aurium/wt/<slug> <parent-ref>`; `.aurium/` appended to `.git/info/exclude`.
- Container env: `GIT_CONFIG_COUNT` with `safe.directory=*`, `gc.auto=0`, `commit.gpgsign=false`, `core.hooksPath=<repo>/.aurium/hooks`; `GIT_AUTHOR_*`/`GIT_COMMITTER_*` from host config; `SBX_GUARD`→`AURIUM_GUARD=1`, `AURIUM_BRANCH`.
- Host UID/GID baked into L1 via `mkuser.sh` (user `aurium`, `HOME=/home/aurium`).
- Guard hooks (`reference-transaction`, `pre-push`) as StackBox Appendix A with the env names renamed.

### 5.5 Invariant 1 enforcement ("a child cannot mutate a parent")

| parent state | protection |
|---|---|
| git branch | guard hook: child may update only `refs/heads/<own>` |
| rootfs | L2 is an immutable image; the child writes to its own L3 |
| volumes | cloned, not shared |
| context | permissions (§8.3): child containers get READ on parent container context by default |
| processes / network | separate container and network |

---

## 6. Snapshots, fork, stack, sync, restore

### 6.1 Snapshot format

Identifier `s_<ulid>`, per-container sequence number `seq`. Storage:

```
~/.aurium/snapshots/<project>/<container>/<seq>/
  manifest.json
  volumes/<volume-name>.tar.zst
git ref        refs/aurium/snap/<container>/<seq>          → commit whose tree is the worktree state
docker image   aurium-snap/<container>:<seq>               → L0+L1+L2+L3 committed
```

```jsonc
// manifest.json
{
  "schema": 1, "id": "s_01J…", "container": "c_01J…", "seq": 17, "label": "before-oauth-refactor",
  "created_at": "2026-09-07T18:02:11Z", "trigger": "manual|pre_sync|pre_restore|task_transition|auto",
  "git": { "branch": "implement-oauth", "head_sha": "a1b2…", "tree_ref": "refs/aurium/snap/c_01J…/17", "base_sha": "9f8e…", "parent": "main" },
  "rootfs": { "image": "aurium-snap/c_01J…:17", "digest": "sha256:…", "base_image": "aurium-local/node-20-alpine:3fa1…" },
  "volumes": [ { "name": "aurium-<repo>-<slug>-node_modules", "mount": "<wt>/node_modules", "archive": "volumes/node_modules.tar.zst", "bytes": 412331020 } ],
  "context_version": 43,
  "agents": [ { "adapter": "claude", "session": "agent", "resume_hint": "claude --continue" } ],
  "env_hash": "sha256 of aurium.yaml sandbox section",
  "ports": { "3000": 53012 }
}
```

### 6.2 Taking a snapshot (`aurium snapshot <container> [--label]`)

```
1. driver.Pause(c)                                     # quiesce: no writes to rootfs, volumes, index
2. git tree:  cp <admin>/index $TMP_INDEX
              GIT_INDEX_FILE=$TMP_INDEX git -C <wt> add -A          # tracked + untracked, honours .gitignore
              tree=$(GIT_INDEX_FILE=$TMP_INDEX git write-tree)
              commit=$(git commit-tree $tree -p $(git rev-parse HEAD) -m "aurium snapshot <seq>")
              git update-ref refs/aurium/snap/<c>/<seq> $commit
3. rootfs:    docker commit --pause=false <container> aurium-snap/<c>:<seq>     # already paused
4. volumes:   for each per-container volume:
              docker run --rm -v <vol>:/v:ro -v <snapdir>/volumes:/s alpine sh -c 'cd /v && tar cf - . | zstd -T0 > /s/<name>.tar.zst'
5. manifest + context_version (current max version of items in scope container:<c>)
6. driver.Unpause(c); event container.snapshot.created
```

Costs: step 2 is sub-second; step 3 seconds (layer diff only); step 4 is proportional to volume size. Sidecar databases are snapshotted the same way if declared (`services.db.snapshot: true`), which requires the sidecar to be paused too.

### 6.3 Restore (`aurium restore <container> <seq>`)

```
1. snapshot current state first (trigger=pre_restore) unless --no-backup
2. driver.Destroy(c, keepVolumes=false)                # container + per-container volumes
3. git:  git update-ref refs/heads/<branch> <head_sha>
         git -C <wt> read-tree -u --reset <tree_of snap commit>   # worktree = snapshot state
         git -C <wt> reset -q                                     # index = HEAD; snapshot edits show as unstaged
4. volumes: docker volume create <vol>; docker run --rm -v <vol>:/v -v <snapdir>/volumes:/s:ro alpine sh -c 'zstd -dc /s/<name>.tar.zst | tar xf - -C /v'
5. driver.Create(c, image=aurium-snap/<c>:<seq>)       # L2 = the snapshot, fresh L3
6. adapter.Prepare(); adapter.Start(resume=true)      # e.g. `claude --continue` picks up the transcript in $HOME
7. base_sha := manifest.git.base_sha; event container.restored
```

### 6.4 Fork and stack

| | **fork** (`aurium container fork <c> [--at <seq>]`) | **stack** (`aurium container stack <new> --on <c> [--at <seq>]`) |
|---|---|---|
| meaning | independent copy; new lineage | child; will track the parent's future |
| git | new branch from `head_sha@seq`; `parent = c.parent`; `base_sha = c.base_sha@seq` (contains the parent's commits as its own) | new branch from `head_sha@seq`; `parent = c.branch`; `base_sha = head_sha@seq` |
| rootfs | new container from `aurium-snap/<c>:<seq>` | same |
| volumes | cloned from the snapshot archives | same |
| context | container context copied (new scope id), `forked_from` recorded | new container context with `parent_container = c`; READ on parent's context |
| sync later | not applicable | `aurium container sync` |

If `--at` is omitted, a snapshot is taken first (trigger=auto). `clone` is an alias for `fork` at latest.

### 6.5 Sync (`aurium container sync <child> [--env] [--force] [--autostash] [--dry-run]`)

Source: the StackBox engine verbatim (eligibility → `git -C <wt> rebase --onto <parentTip> <base_sha> <branch>` → `base_sha := parentTip` on success; `conflict`/`stale`/`drifted` otherwise; conflicts handed to the agent).

Environment (`--env`, opt-in, D13): take snapshot of the child (pre_sync) → destroy container → recreate from the parent's *latest* snapshot image → clone the parent's latest volumes → run `hooks.post_create` → restart agent with resume. Anything the child installed ad hoc into L3 is gone; the child's projection file states this rule so agents declare environment changes in `aurium.yaml` hooks (which are in source, and therefore sync).

Parent-changed notification (spec §61): the watcher marks the child `stale` and emits `container.parent_changed {commits: N}`; the dashboard shows Review / Sync / Ignore. With `stack.auto_sync: true` eligible children sync automatically.

### 6.6 Watcher

As StackBox §4.11, now a goroutine group inside `auriumd`: ref polling every 2 s, container reconcile every 30 s (labels ↔ DB), idle pause every 60 s, auto-snapshot on task transitions.

### 6.7 Retention

Defaults: keep the last 10 snapshots per container plus all labelled ones plus any referenced by a fork/stack. `aurium snapshot gc` removes the rest (git refs, images, archives) and runs `git gc --auto` on the host. `aurium doctor` reports snapshot storage per project.

---

## 7. Data model (SQLite, `~/.aurium/aurium.db`)

All ids are ULIDs with a type prefix (`p_`, `t_`, `c_`, `s_`, `a_`, `m_`, `i_`, `e_`). Timestamps are RFC 3339 UTC text. JSON columns are validated by the daemon, not by SQLite.

```sql
PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;

CREATE TABLE projects      (id TEXT PRIMARY KEY, name TEXT NOT NULL, root TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL);
CREATE TABLE repositories  (id TEXT PRIMARY KEY, project_id TEXT REFERENCES projects(id), path TEXT NOT NULL, base_branch TEXT NOT NULL, remote TEXT);
CREATE TABLE tasks         (id TEXT PRIMARY KEY, project_id TEXT REFERENCES projects(id), title TEXT NOT NULL, status TEXT NOT NULL
                              CHECK (status IN ('created','planning','running','blocked','review','pr_ready','merged','completed','archived')),
                            parent_task_id TEXT REFERENCES tasks(id), created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE containers    (id TEXT PRIMARY KEY, project_id TEXT REFERENCES projects(id), task_id TEXT REFERENCES tasks(id),
                            repo_id TEXT REFERENCES repositories(id), branch TEXT NOT NULL, slug TEXT NOT NULL,
                            parent_container_id TEXT REFERENCES containers(id), parent_branch TEXT NOT NULL,   -- git parent (branch); may be base_branch
                            origin_snapshot_id TEXT REFERENCES snapshots(id), origin_kind TEXT CHECK (origin_kind IN ('fresh','fork','stack','restore')),
                            base_sha TEXT NOT NULL, pending_base_sha TEXT, head_sha TEXT,
                            driver TEXT NOT NULL, runtime_id TEXT, image TEXT, worktree TEXT NOT NULL, ports_json TEXT, network TEXT,
                            status TEXT NOT NULL CHECK (status IN ('creating','running','paused','stopped','stale','conflict','drifted','error','archived')),
                            last_error TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
                            UNIQUE (repo_id, branch));
CREATE TABLE snapshots     (id TEXT PRIMARY KEY, container_id TEXT REFERENCES containers(id), seq INTEGER NOT NULL, label TEXT, trigger TEXT NOT NULL,
                            head_sha TEXT NOT NULL, tree_ref TEXT NOT NULL, base_sha TEXT NOT NULL, image_ref TEXT NOT NULL, manifest_path TEXT NOT NULL,
                            context_version INTEGER NOT NULL, bytes INTEGER, created_at TEXT NOT NULL, UNIQUE (container_id, seq));
CREATE TABLE agents        (id TEXT PRIMARY KEY, container_id TEXT REFERENCES containers(id), adapter TEXT NOT NULL, role TEXT NOT NULL CHECK (role IN ('primary','master','worker')),
                            parent_agent_id TEXT REFERENCES agents(id), tmux_session TEXT NOT NULL, model TEXT,
                            status TEXT NOT NULL CHECK (status IN ('starting','running','idle','blocked','exited','error')), started_at TEXT, last_activity_at TEXT);
CREATE TABLE tokens        (id TEXT PRIMARY KEY, hash TEXT NOT NULL UNIQUE, container_id TEXT REFERENCES containers(id), agent_id TEXT REFERENCES agents(id),
                            scopes TEXT NOT NULL, expires_at TEXT, revoked INTEGER NOT NULL DEFAULT 0);

-- context (§8)
CREATE TABLE context_items    (id TEXT PRIMARY KEY, scope TEXT NOT NULL CHECK (scope IN ('project','container','agent')), scope_id TEXT NOT NULL,
                               key TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, content TEXT NOT NULL, mime TEXT NOT NULL DEFAULT 'text/markdown',
                               updated_by TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE (scope, scope_id, key));
CREATE TABLE context_versions (item_id TEXT REFERENCES context_items(id), version INTEGER NOT NULL, content TEXT NOT NULL, author TEXT NOT NULL,
                               reason TEXT, created_at TEXT NOT NULL, PRIMARY KEY (item_id, version));
CREATE TABLE context_grants   (id TEXT PRIMARY KEY, scope TEXT NOT NULL, scope_id TEXT NOT NULL, key_glob TEXT NOT NULL,
                               subject_type TEXT NOT NULL CHECK (subject_type IN ('project','container','agent','role')), subject_id TEXT NOT NULL,
                               perm TEXT NOT NULL CHECK (perm IN ('read','append','propose','write')));
CREATE TABLE context_proposals(id TEXT PRIMARY KEY, item_id TEXT REFERENCES context_items(id), base_version INTEGER NOT NULL, content TEXT NOT NULL,
                               author TEXT NOT NULL, reason TEXT, status TEXT NOT NULL CHECK (status IN ('open','accepted','rejected','stale')),
                               decided_by TEXT, created_at TEXT NOT NULL, decided_at TEXT);
CREATE TABLE context_docs     (id TEXT PRIMARY KEY, project_id TEXT REFERENCES projects(id), source TEXT NOT NULL, path TEXT NOT NULL, heading TEXT,
                               content TEXT NOT NULL, hash TEXT NOT NULL, indexed_at TEXT NOT NULL);
CREATE VIRTUAL TABLE context_fts USING fts5(key, content, content='context_items', content_rowid='rowid', tokenize='porter unicode61');
CREATE VIRTUAL TABLE docs_fts    USING fts5(path, heading, content, content='context_docs',  content_rowid='rowid', tokenize='porter unicode61');

-- ipc (§9.5)
CREATE TABLE messages (id TEXT PRIMARY KEY, project_id TEXT REFERENCES projects(id), from_agent_id TEXT, from_container_id TEXT,
                       to_agent_id TEXT, to_container_id TEXT, to_human INTEGER NOT NULL DEFAULT 0,
                       type TEXT NOT NULL CHECK (type IN ('REQUEST','RESPONSE','INFORMATION','WARNING','BLOCKED','APPROVAL_REQUIRED','ARTIFACT','DEPENDENCY','CONFLICT')),
                       priority TEXT NOT NULL DEFAULT 'normal', content TEXT NOT NULL, refs_json TEXT, in_reply_to TEXT REFERENCES messages(id),
                       status TEXT NOT NULL CHECK (status IN ('queued','delivered','acked')), created_at TEXT NOT NULL, delivered_at TEXT, acked_at TEXT);
CREATE TABLE dependencies (from_agent_id TEXT, to_agent_id TEXT, subject TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (from_agent_id, to_agent_id, subject));

-- integrations (§10)
CREATE TABLE integrations (id TEXT PRIMARY KEY, project_id TEXT REFERENCES projects(id), kind TEXT NOT NULL CHECK (kind IN ('mcp_stdio','mcp_http','native')),
                           name TEXT NOT NULL, config_json TEXT NOT NULL, secret_ref TEXT, status TEXT NOT NULL, last_error TEXT, UNIQUE (project_id, name));
CREATE TABLE capabilities (integration_id TEXT REFERENCES integrations(id), name TEXT NOT NULL, description TEXT, input_schema_json TEXT,
                           risk TEXT NOT NULL CHECK (risk IN ('low','medium','high')), PRIMARY KEY (integration_id, name));
CREATE TABLE grants       (id TEXT PRIMARY KEY, integration_id TEXT REFERENCES integrations(id), capability_glob TEXT NOT NULL,
                           subject_type TEXT NOT NULL CHECK (subject_type IN ('project','container','agent','role')), subject_id TEXT NOT NULL,
                           mode TEXT NOT NULL CHECK (mode IN ('allow','deny','approve')), created_by TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE approvals    (id TEXT PRIMARY KEY, container_id TEXT, agent_id TEXT, integration_id TEXT, capability TEXT NOT NULL, args_json TEXT NOT NULL,
                           reason TEXT, status TEXT NOT NULL CHECK (status IN ('pending','approved','rejected','expired')),
                           decided_by TEXT, created_at TEXT NOT NULL, decided_at TEXT, expires_at TEXT NOT NULL);
CREATE TABLE artifacts    (id TEXT PRIMARY KEY, container_id TEXT REFERENCES containers(id), task_id TEXT, kind TEXT NOT NULL CHECK (kind IN ('pr','file','report','snapshot','url')),
                           ref TEXT NOT NULL, meta_json TEXT, created_at TEXT NOT NULL);

-- events (§11)
CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, project_id TEXT, container_id TEXT, agent_id TEXT, task_id TEXT,
                     type TEXT NOT NULL, actor TEXT NOT NULL, payload_json TEXT NOT NULL);
CREATE INDEX events_ts ON events(ts); CREATE INDEX events_container ON events(container_id, id);
```

The project knowledge graph (spec §38) is these foreign keys plus `refs_json` on messages and `meta_json` on artifacts. `aurium graph <task>` walks them; no separate graph store.

Secrets are never in this database: `integrations.secret_ref` and `tokens.hash` are references/hashes. Values live in the OS keyring (`aurium/<project>/<integration>`) or, where no keyring exists, in `~/.aurium/secrets.age` unlocked once per daemon lifetime.

---

## 8. Context engine

### 8.1 Scopes and inheritance

`project:<p>` → `container:<c>` → `agent:<a>`. An agent's effective context is the union, with the more specific scope shadowing on key collision. Container context of a stacked child also includes READ on its parent chain's container context (`parent_container_id` walk), so B can see A's decisions without touching A's files.

### 8.2 Items and versions

Item = (scope, scope_id, key, version, content). Keys are path-like: `task/objective`, `task/constraints`, `decisions/2026-09-07-jwt`, `discoveries/auth-flow`, `plan`, `deps/api-contract`. Every mutation writes a `context_versions` row and emits `context.updated {scope, key, version, author}`.

### 8.3 Permissions

| perm | operation | concurrency rule |
|---|---|---|
| `read` | get / query | — |
| `append` | add a block to the end | serialized by the daemon; no version check needed |
| `propose` | create a proposal against `base_version` | proposal becomes `stale` if the item moves past `base_version`; human or master accepts/rejects |
| `write` | replace content | compare-and-set: request carries `base_version`; daemon rejects with `409 stale` if `item.version != base_version` |

"COMMIT" in the spec is the outcome of `write` (direct) or an accepted proposal. Defaults seeded by `aurium init`:

| subject | project scope | own container scope | parent container scope |
|---|---|---|---|
| role:primary / master | `read` on `**`; `propose` on `architecture/**`, `conventions/**`; `append` on `decisions/**` | `write` on `**` | `read` |
| role:worker | `read` | `append` on `discoveries/**`, `decisions/**`; `write` on `plan/<self>` | `read` |
| human | `write` everywhere | | |

### 8.4 Transactions (spec §28)

```
agent reads  key K            → content, version 42
agent works
agent calls  context_write(K, base_version=42, content)      # needs write
   daemon: BEGIN IMMEDIATE; SELECT version; if 42 → UPDATE to 43, INSERT version row, COMMIT, emit → 200 {version: 43}
                                             else → ROLLBACK → 409 {current_version: 44, diff}
agent re-reads, reconciles, retries
```

With `propose` the same flow produces a `context_proposals` row and an `APPROVAL_REQUIRED`-style message to the reviewer (master agent or human).

### 8.5 Projection into the container (D16)

On `Prepare()` and on every relevant `context.updated`, the daemon regenerates `<wt>/.aurium/CONTEXT.md` (host writes it; the path is inside the bind mount; `.aurium/` is git-excluded):

```markdown
# Aurium container c_01J… — task t_01J… "Implement OAuth"
## Objective            (context: container task/objective v3)
## Constraints          (task/constraints v1)
## Stack                parent: implement-authentication (c_…); base main@9f8e…; status: up-to-date
## Decisions (last 10)  …
## Discoveries (last 10)…
## Inbox                2 unread messages — call aurium_ipc_inbox
## Environment rule     Environment is re-derived on `--env` sync/restore. Declare installs in aurium.yaml hooks.
## Tools                aurium_* (context, ipc, task, snapshot); github_* (create_branch, create_pull_request; merge requires approval)
```

Adapter-specific hook-up, each written into the container's own `$HOME` so it is per-container and captured by snapshots:

| adapter | instruction file | MCP config | notes |
|---|---|---|---|
| claude | `~/.claude/CLAUDE.md` contains `@<wt>/.aurium/CONTEXT.md` (Claude Code import syntax) | `~/.claude.json` user-scope entry `aurium` → `aurium-mcp` (stdio), or `--mcp-config` on launch | verify current file locations against docs before release |
| codex | `~/.codex/AGENTS.md` (global instructions) with the same import or inlined content | `~/.codex/config.toml` `[mcp_servers.aurium] command = "aurium-mcp"` | verify |
| custom | `AURIUM_CONTEXT_FILE` env; MCP config path from adapter config | | |

Long-running interactive agents also get a one-line tmux nudge on important updates ("[aurium] context updated: decisions/… v43 — re-read CONTEXT.md").

### 8.6 Retrieval

`context_query(q, scopes?)` runs FTS5 BM25 over `context_fts` (items in the caller's readable scopes) and `docs_fts` (project doc sources: `project.context: [./docs, /path/to/obsidian-vault]`, indexed by a file walker that splits Markdown on headings, re-indexed on hash change every 60 s). Returns top-k snippets with keys/paths and versions. Embeddings are a Phase D option behind the same call.

---

## 9. Agent layer

### 9.1 Adapter API (replaces spec §22)

```go
type Adapter interface {
    Name() string
    ImageLayer() string                                    // Dockerfile RUN fragment (install the CLI)
    AuthEnv() []string                                     // passthrough names; at least one must be set on host
    Prepare(ctx, c *Container, a *Agent, p Projection) error   // write instruction + MCP config into $HOME
    Start(ctx, c, a, opts StartOpts) error                 // tmux new-session … 'cmd; exec bash -l'; opts.Resume
    Stop / Pause / Resume(ctx, c, a) error
    SendMessage(ctx, c, a, text string) error              // interactive: tmux send-keys -l + Enter
    Execute(ctx, c, a, prompt string, o ExecOpts) (ExecResult, error)  // headless one-shot; usage if available
    Status(ctx, c, a) (AgentStatus, error)                 // running|idle(since)|exited(code)|blocked(marker)
    Capabilities() AdapterCaps                             // MCP bool, Headless bool, Resume bool, Usage bool
}
```

| adapter | image layer | launch | headless | resume | auth env |
|---|---|---|---|---|---|
| `claude` | `npm i -g @anthropic-ai/claude-code` | `claude` | `claude -p "<prompt>" --output-format json` | `claude --continue` | `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` (`claude setup-token`; inject into the container only) |
| `codex` | `npm i -g @openai/codex` | `codex` | `codex exec "<prompt>"` | per docs | `OPENAI_API_KEY` |
| `shell` | — | `bash -l` | — | — | — |
| `custom` | user Dockerfile fragment | `agent_command` | `agent_exec_command` | — | `env_passthrough` |

Flags and file paths for `claude`/`codex` are recorded in adapter code with a link to the docs page they were verified against; CI has a nightly "adapter smoke" job that starts each adapter in a container and asserts the MCP server is listed by the agent.

Provider keys optionally never enter the container: adapters that honour a base-URL override (`ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`) can be pointed at an `auriumd` proxy that injects the key from the keyring. Phase D.

### 9.2 Roles

`primary` (default single agent), `master` (may delegate), `worker` (created by delegation, has a `parent_agent_id`). Roles affect default context grants (§8.3) and gateway grants (§10.4).

### 9.3 Blocked / approval-required (spec §45)

An agent signals via `aurium_task_status(status="blocked", reason, options?)` or by a gateway `approve`-mode call. Both create a message with `to_human=1`, an event, and (for options) an approval record. Humans answer with `aurium approve <id> [--choose B]` or in the dashboard; the answer is delivered as a `RESPONSE` message and, for interactive agents, typed into the session.

### 9.4 Delegation (master / worker, D15)

`aurium_delegate(title, adapter, mode="fork"|"serial", prompt)` available to `master` agents:

- **fork** (default): snapshot the master's container → `container fork` → new worker agent in the new container with `prompt` and a `DEPENDENCY` message back to the master. The worker's result is its branch. When the worker calls `aurium_task_status("review")`, the master receives an `ARTIFACT` message with the branch and diff stats; the master merges it into its own branch (`aurium_merge(child)` → host runs `git -C <master-wt> merge --no-ff <worker-branch>` or a rebase, only when the master's worktree is clean) or requests changes via IPC.
- **serial**: run the worker headless (`adapter.Execute`) *inside the master's container* while the master's session is paused; result returned in the tool response. For small, bounded subtasks.

Workers cannot delegate (depth 1) in Phase C; configurable later.

### 9.5 IPC protocol

Transport: HTTP to `auriumd` (via `aurium-mcp`). Persistence: `messages`. Delivery: pull (`aurium_ipc_inbox`) plus a push nudge (tmux line "[aurium] 1 new REQUEST from agent.backend (m_…)"). Semantics: at-least-once; a message is `delivered` when returned by an inbox call and `acked` when the agent calls `aurium_ipc_ack(id)`. Undelivered high-priority messages are re-nudged every 5 minutes.

```jsonc
{
  "id": "m_01J…", "ts": "2026-09-07T19:43:22Z",
  "from": { "agent": "a_01J…", "container": "c_01J…", "role": "worker" },
  "to":   { "agent": "a_01K…" },                 // or {"container": "c_…"} (its primary agent) or {"human": true}
  "type": "REQUEST", "priority": "normal|high",
  "content": "Please add integration coverage for AuthService.login()",
  "refs": { "task": "t_…", "artifact": "…", "context_key": "deps/api-contract", "files": ["src/auth.ts"] },
  "in_reply_to": null
}
```

Message types and their side effects: `REQUEST`/`RESPONSE`/`INFORMATION`/`WARNING` — none. `BLOCKED` — agent status `blocked`, task status `blocked`, human notified. `APPROVAL_REQUIRED` — always routed to human. `ARTIFACT` — creates an `artifacts` row. `DEPENDENCY` — writes `dependencies`; when the producer's referenced context key changes, the daemon sends the consumer an `INFORMATION` message ("api-contract changed v7→v8"). `CONFLICT` — raised by the stack engine on rebase conflicts.

---

## 10. Integration layer: MCP gateway

### 10.1 Topology

```
 upstream MCP servers (spawned/connected by auriumd, credentials from keyring)
   github-mcp (stdio)   sentry-mcp (http)   linear-mcp (stdio)   native: git, gh, fs-search
                 \            |               /
                  ▼           ▼              ▼
              auriumd gateway: tool registry → capability rows (risk classified)
                              per-request: token → (container, agent) → grants → allow | deny | approve
                              audit: every call → events (integration.call {cap, args-hash, result, ms})
                                        ▲
                  aurium-mcp (stdio) ────┘  one server named "aurium" in each container
                        ▲
                  agent (Claude Code / Codex / custom)
```

Agents see one MCP server whose tool list is computed per (container, agent): native `aurium_*` tools plus proxied tools named `<integration>_<capability>` (e.g. `github_create_pull_request`). Tools the subject is not granted are not listed at all; a call to a hidden tool returns `-32601`.

### 10.2 Native tools (`aurium_*`)

| tool | needs | effect |
|---|---|---|
| `aurium_context_query(q, scopes?)` | read | §8.6 |
| `aurium_context_get(key)` / `aurium_context_list(prefix)` | read | item + version |
| `aurium_context_append(key, text)` | append | |
| `aurium_context_write(key, base_version, content, reason)` | write | CAS |
| `aurium_context_propose(key, base_version, content, reason)` | propose | proposal |
| `aurium_ipc_send(to, type, content, priority?, refs?)` / `aurium_ipc_inbox(limit?)` / `aurium_ipc_ack(ids)` | — | §9.5 |
| `aurium_task_status(status, note?, options?)` | — | task transition; `blocked` notifies human |
| `aurium_task_info()` | — | task, stack position, ports, parent status |
| `aurium_snapshot(label?)` | — | §6.2 on own container |
| `aurium_delegate(...)`, `aurium_merge(child)` | role master | §9.4 |
| `aurium_request_approval(action, reason)` | — | explicit approval outside the gateway (e.g. before a destructive shell command) |

### 10.3 Capability registry

On `aurium integration connect github --mcp "npx -y @modelcontextprotocol/server-github"` (or an HTTP URL) the daemon: stores config (secret in keyring), starts the upstream, calls `tools/list`, writes `capabilities` rows with `risk` from a name classifier (`get|list|search|read` → low; `create|update|comment|push` → medium; `delete|merge|force|remove|drop` → high; overridable in `aurium.yaml`), emits `integration.connected`. Re-lists on reconnect; removed tools are marked and existing grants kept.

### 10.4 Permission resolution (D18)

```
subjects, most specific first: agent:<a> → role:<r> → container:<c> → project:<p>
for each subject, find grants whose capability_glob matches; first match wins; else DENY
default seed on connect: low → allow (project); medium → allow (project); high → approve (project)
worker role seed:  medium → approve, high → deny
```

`aurium integration grant github --container c_185 --deny` writes a container-scope `deny **`, which is exactly spec §62 ("revoke C-185 without disconnecting GitHub").

### 10.5 Approval flow (`approve` mode)

Gateway receives call → inserts `approvals(pending, expires_at = now+30m)` → emits `approval.requested` → returns to the agent immediately with `{"status":"pending_approval","id":"…"}` and a tmux nudge to the human's dashboard/CLI → human `aurium approve <id>` or `reject` → the gateway *executes the original call now* and delivers the result as a `RESPONSE` message to the agent (agents cannot block on long-poll reliably, so the result is asynchronous by design). Expired → rejected, message to agent. Every decision is an event with `decided_by`.

### 10.6 Secrets (spec §35)

Stored via `zalando/go-keyring` under service `aurium`, account `<project>/<integration>`; `~/.aurium/secrets.age` fallback (age-encrypted, passphrase prompted at daemon start). The daemon reads a secret only when spawning/connecting an upstream. Containers receive: a scoped `AURIUM_TOKEN`, the app's declared dev env, and the agent's own provider key (until the base-URL proxy in §9.1 exists). `aurium doctor` warns if any `env_passthrough` name looks like a third-party credential other than the agent's.

---

## 11. Events, API, dashboard

### 11.1 Event types

`project.created` · `task.created|transitioned|completed` · `container.created|started|paused|resumed|stopped|destroyed|snapshot.created|restored|forked|stacked|synced|stale|conflict|parent_changed|drifted` · `agent.started|idle|active|blocked|exited|message.sent|message.delivered|message.acked` · `context.updated|proposed|proposal.decided` · `integration.connected|revoked|call` · `approval.requested|decided|expired` · `pr.created|updated|merged` · `daemon.started|reconciled`.

Every event: `{id, ts, type, actor: "human|daemon|agent:<id>", project_id, container_id?, agent_id?, task_id?, payload}`. Persisted before the API responds; fanned out to SSE subscribers; filterable by type/container/since.

### 11.2 API (HTTP/JSON; `api/openapi.yaml` is authoritative)

```
GET  /v1/health                                   GET  /v1/events?since=<id>&types=…   (SSE)
GET/POST /v1/projects                             GET/POST /v1/projects/{p}/tasks   PATCH /v1/tasks/{t}
GET/POST /v1/projects/{p}/containers              GET/DELETE /v1/containers/{c}
POST /v1/containers/{c}/{snapshot|restore|fork|stack|sync|pause|resume|exec|attach-info}
GET/POST /v1/containers/{c}/agents               POST /v1/agents/{a}/{start|stop|message|execute}
GET  /v1/projects/{p}/tree                        (forest with statuses, ahead/behind, ports)
GET/PUT/POST /v1/context/{scope}/{id}/items[/{key}]     POST …/proposals   PATCH …/proposals/{id}
GET/POST /v1/messages   POST /v1/messages/{m}/ack
GET/POST /v1/projects/{p}/integrations   POST …/{i}/{grant|revoke|reconnect}   GET …/{i}/capabilities
GET /v1/approvals   POST /v1/approvals/{id}/{approve|reject}
POST /mcp                                          (Streamable HTTP MCP endpoint used by aurium-mcp; bearer = container token)
```

Auth: host clients read `~/.aurium/token` (0600, created by the daemon). Containers use scoped tokens (`scopes`: `context:*`, `ipc:*`, `task:*`, `snapshot:self`, `gateway:*`). Tokens are hashed at rest and revoked on destroy.

### 11.3 Dashboard v0 (Phase B) → v1 (Phase C/D)

Served at `http://127.0.0.1:7770/`. v0: Containers (list + tree + status + ports + attach command), Events (live), Tasks (list + transitions), Snapshots (list/restore/fork/stack). v1 adds Agents (status, inbox, role tree), Context (browse/edit/proposals), Integrations (grants matrix per container/agent as spec §32), Approvals inbox. Attach is not in the browser: the dashboard shows the `aurium attach <c>` command and a one-click "copy"; a Tauri shell (Phase D) can open a terminal pane. This mirrors spec §43's tabs without building a terminal emulator.

---

## 12. CLI and configuration

### 12.1 CLI

Global: `--project <path|id>` (default: the project whose root contains cwd, resolving through a container worktree), `--json`, `-v` (echo git/docker commands). Exit codes as StackBox (0 ok · 1 usage · 2 git · 3 driver · 4 conflict · 5 locked · 6 permission · 7 approval pending).

| command | notes |
|---|---|
| `aurium init [--driver] [--image] [--agent]` | project row, `aurium.yaml`, `.aurium/`, hooks, exclude entry, default grants; starts daemon |
| `aurium doctor` | StackBox checks + daemon reachable + keyring available + snapshot storage + adapter smoke |
| `aurium task create "<title>" [--agent claude] [--role master] [--no-container] [--parent-task]` | task + container + primary agent; prints attach command |
| `aurium task list|show|transition <t> <status>` | |
| `aurium container create <branch> [--task t] [--parent <branch|container>] [--agent] [--no-agent]` | fresh container (Phase A `create`) |
| `aurium container fork <c> [--at seq] [--branch]` / `stack <branch> --on <c> [--at seq]` / `clone <c>` | §6.4 |
| `aurium snapshot <c> [--label]` / `aurium snapshot list <c>` / `aurium restore <c> <seq>` / `aurium snapshot gc` | §6.2–6.3, 6.7 |
| `aurium container sync [<c>] [--env] [--force] [--autostash] [--dry-run]` / `reparent <c> --onto <b>` | §6.5 |
| `aurium attach <c> [--shell]` / `exec <c> -- <cmd>` / `logs <c>` / `pause|resume <c>` | |
| `aurium tree [--watch] [--json]` | forest across the project: task, container, agent, status, ahead/behind, ports, unread messages, pending approvals |
| `aurium push <c> [--cascade]` / `aurium pr <c>` | host-side push; PR with base = parent branch via `gh`; PR becomes an artifact |
| `aurium destroy <c> [--cascade] [--keep-volumes] [--delete-branch] [--archive]` | `--archive` takes a final snapshot and marks archived instead of deleting rows |
| `aurium agent start|stop|status <c> [--adapter] [--role]` / `agent message <a> "<text>"` / `agent exec <a> "<prompt>"` | |
| `aurium context get|set|append|query|list|proposals|accept|reject` | scope flags `--project|--container c|--agent a`; `set` takes `--base-version` |
| `aurium inbox` / `aurium approve <id> [--choose X]` / `aurium reject <id>` | human side of IPC/approvals |
| `aurium integration list|connect|disconnect|capabilities|grant|revoke` | §10 |
| `aurium events [--follow] [--type] [--container]` | tail the bus |
| `aurium status` | daemon, containers running/paused, memory budget, pending approvals, stale children |
| `aurium daemon start|stop|status` | usually implicit |

### 12.2 `aurium.yaml` (extends StackBox §4.4)

```yaml
version: 1
project:
  name: my-app
  base_branch: main
  context:                       # indexed for aurium_context_query (§8.6)
    - ./docs
    - ./architecture
    - ~/Obsidian/MyApp
sandbox:                          # identical to StackBox `sandbox:` (driver, image, agent, resources, ports, volumes, env, env_passthrough, services)
  driver: docker
  image: node:20-alpine
  agent: claude
  resources: { cpus: 2, memory: 2g, pids: 2048 }
  idle_pause_minutes: 15
  ports: [ { internal: 3000, env: PORT } ]
  volumes:
    per_sandbox: [ node_modules ]
    shared: [ { name: npm-cache, path: /home/aurium/.npm } ]
  env: { NODE_ENV: development, DATABASE_URL: "postgresql://postgres:postgres@db:5432/app" }
  env_passthrough: [ ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN ]
  services:
    db: { image: postgres:16-alpine, env: { POSTGRES_PASSWORD: postgres }, snapshot: true }
hooks: { post_create: ["npm ci"], post_sync: ["npm test"], pre_destroy: [] }
stack: { auto_sync: false, autostash: false, idle_seconds_before_sync: 90, push: { remote: origin, force_with_lease: true } }
snapshot:
  auto_on: [ task_transition, pre_sync, pre_restore ]
  keep_last: 10
  include_ignored: false
agents:
  master: { adapter: codex, model: "…" }         # optional named agent presets
  worker: { adapter: claude, model: "…" }
  delegation: { mode: fork, max_depth: 1 }
integrations:                                   # declarative; secrets are NOT here (keyring)
  github: { mcp: "npx -y @modelcontextprotocol/server-github", risk_overrides: { merge_pull_request: high } }
grants:                                         # optional seed overrides, same shape as the table
  - { integration: github, capability: "merge_*", subject: project, mode: approve }
  - { integration: github, capability: "*",       subject: role:worker, mode: deny }
```

Unknown keys are errors. JSON Schema published at `schema/aurium.schema.json`.

---

## 13. Build plan

### Phase A — Runtime core (weeks 1–7)

StackBox ERD milestones M0–M4 with the rename, plus: SQLite store from day one (in-process, `BEGIN IMMEDIATE`), event table written by every command, `aurium-mcp` shim scaffolded (health only), `Driver.Capabilities()`. **Gate:** five concurrent agents × 50 commits, `git fsck` clean, 20 runs; cascade scenarios green; quickstart tested by a non-author.

### Phase B — Daemon, snapshots, stacking, dashboard v0 (weeks 8–13)

| week | E1 (runtime/git) | E2 (daemon/web) |
|---|---|---|
| 8 | snapshot take/restore (§6.2–6.3), git tree via temp index, manifest | `auriumd`: API skeleton, auth tokens, SSE, CLI auto-start, DB ownership migration |
| 9 | fork/stack (§6.4), volume clone, `origin_*` bookkeeping | watcher moved into daemon; reconcile; idle pause |
| 10 | `sync --env` re-derive; retention/gc | admission control (memory budget → `queued`) |
| 11 | integration tests: stack B on A → commit in A → B `stale` → sync → B rebased, env intact; restore after `rm -rf node_modules; echo junk > src/x` | dashboard v0: containers/tree/events/tasks/snapshots |
| 12–13 | Linux + macOS soak; podman parity for snapshot | polish, `aurium status`, crash-recovery test (kill -9 daemon during snapshot → reconcile) |

**Gate:** the first half of the §73 demo (create A, B, C; stack B on A; update A; B unchanged; sync; B receives; restore) runs from the CLI and is visible live in the dashboard.

### Phase C — Context, IPC, gateway, delegation (weeks 14–21)

| week | E1 | E2 |
|---|---|---|
| 14 | context items/versions/grants/CAS; projection writer | `aurium-mcp` shim end-to-end (stdio ↔ `/mcp`); native `aurium_context_*` tools |
| 15 | FTS5 indexing of items + doc sources | `claude` adapter `Prepare()` (instruction import + MCP config); nightly adapter smoke |
| 16 | IPC tables, delivery, nudges, ack | `codex` adapter; `aurium_ipc_*`, `aurium_task_*` tools |
| 17 | task lifecycle transitions; PR artifact via `gh`; `pr.merged` poll → reparent offer | gateway: upstream spawn/connect, registry, risk classifier |
| 18 | delegation fork mode + `aurium_merge` (clean-worktree merge on host) | grants resolution, hidden tools, approvals flow, `aurium approve`; keyring |
| 19 | serial delegation; dependency notifications | dashboard v1: agents, context, integrations matrix, approvals inbox |
| 20–21 | full §73 demo rehearsal; conflict handoff via IPC `CONFLICT` | security review of gateway (token scopes, arg logging redaction), docs |

**Gate:** §73 demo end to end: Codex master delegates to Claude workers (forked containers), workers report `review`, master merges, GitHub PR created through the gateway, one `merge` attempt held for approval and approved from the dashboard; audit trail in `aurium events`.

### Phase D — Platform (weeks 22–26+)

Priorities/cost (usage from headless results; interactive usage from adapter transcripts where exposed), Obsidian vault indexing polish, sidecar snapshots, Tauri shell, plugin API (event subscriptions over SSE + external integration providers registering via the API), podman driver parity, release engineering (goreleaser, Homebrew, signed binaries, Apache-2.0 core), two weeks of dogfooding before tagging.

---

## 14. Test plan (additions to StackBox §7)

| area | test |
|---|---|
| snapshot | take → mutate rootfs, volume, tracked file, untracked file → restore → assert all four reverted; process not required to survive |
| fork/stack | fork keeps parent untouched (hashes of parent's rootfs diff and volume equal before/after); stack child `base_sha == parent head@seq` |
| env sync | child installs a package ad hoc → `sync --env` → package gone, `post_create` deps present, source rebased |
| context | CAS: two writers with the same base version → exactly one 200, one 409; proposal goes `stale` when item moves |
| projection | `context.updated` → CONTEXT.md regenerated within 1 s; agent (shell adapter) sees new content |
| ipc | send → inbox returns → ack → status transitions; nudge text appears in `capture-pane`; high-priority re-nudge after 5 min (fake clock) |
| gateway | hidden tool not listed and `-32601` on call; `approve` mode holds, executes after approval, result delivered as RESPONSE; `deny` at container scope overrides project `allow`; credential never appears in container env or filesystem (grep test) |
| delegation | fork mode: worker branch merged by master only when master clean; serial mode: master paused during execution |
| daemon | kill -9 during snapshot → on restart, partial snapshot marked failed and cleaned; containers reconciled from labels |
| security | token of destroyed container rejected; scopes enforced (`snapshot:self` cannot snapshot another container) |
| soak | 10 containers, auto_sync on, 2 masters each delegating 3 workers, 1 h; no orphan containers/networks/volumes; `git fsck` clean |

---

## 15. Risks and open questions

| risk | mitigation |
|---|---|
| Agent CLIs change instruction-file locations, MCP config format, or headless flags | Adapter code carries doc links + verified date; nightly adapter smoke fails loudly; adapters are ~200 lines each |
| Snapshot storage growth (volumes × containers × seq) | zstd, retention defaults, `snapshot gc`, `doctor` report; dedupe archives by content hash (Phase D) |
| `docker commit` of large L3 layers (agents installing GBs) | warn when L3 > 2 GB; recommend hooks; `include_ignored` stays off |
| Approval latency stalls agents | async result delivery (§10.5); agents are told in CONTEXT.md to continue other work |
| Gateway becomes a bottleneck for chatty MCP tools | upstream connections pooled per integration; proxy is streaming; measure in Phase C week 20 |
| Two humans, one project, one daemon | daemon is per-user; shared-project workflows are the cloud runtime's job, not local |
| Obsidian vaults with thousands of notes | incremental hash-based indexing; FTS5 handles 100k rows easily |
| Master merges a worker branch that diverged from the master's newer commits | `aurium_merge` runs the same eligibility as sync and rebases the worker onto the master first |

Open questions for product:

1. Should `task create` default to a `master` agent (spec §58) or a `primary` agent? Recommendation: `primary`; `--role master` opts into delegation. Most tasks do not need a hierarchy.
2. Is the per-container `$HOME` (agent transcripts, settings) part of what users expect a snapshot to restore? Recommendation: yes, and say so — it is what makes "show me what the agent saw" possible.
3. Where does the Tauri shell fit against the embedded web dashboard? Recommendation: dashboard is the product; Tauri is packaging.

---

## Appendix A — §73 killer demo as CLI

```sh
aurium init --agent claude
aurium integration connect github --mcp "npx -y @modelcontextprotocol/server-github"   # token → keyring
aurium task create "Implement authentication" --role master --agent codex      # → c_A
aurium task create "Write comprehensive tests"                                 # → c_C (independent; parent main)
aurium attach c_A                       # Codex master runs; calls aurium_delegate ×4 → c_A1..c_A4 (forked workers)
aurium tree                             # main ── c_A (master, 4 workers) ── ; main ── c_C
aurium snapshot c_A --label auth-v1
aurium container stack build-oauth --on c_A --at 1 --task "Build OAuth on top of authentication"   # → c_B
aurium exec c_A -- sh -c 'echo "// change" >> src/auth.ts && git commit -qam "auth tweak"'
aurium tree                             # c_B: stale (parent moved 1)   — B unchanged
aurium container sync c_B               # B rebased; base_sha updated
aurium integration grant github --container c_B --deny
aurium integration capabilities github  # shows enabled/disabled per container/agent
aurium events --follow                  # the audit trail behind everything above
```

## Appendix B — Files written into a container's `$HOME` by `Prepare()` (claude adapter)

```
/home/aurium/.claude/CLAUDE.md          @/Users/alice/code/app/.aurium/wt/implement-oauth/.aurium/CONTEXT.md
/home/aurium/.claude.json               { "mcpServers": { "aurium": { "command": "aurium-mcp", "args": [] } } }   # verify schema/location
/run/aurium/token                       <scoped bearer token>   (also AURIUM_TOKEN env)
```

`aurium-mcp` reads `AURIUM_URL` and the token, speaks stdio MCP to the agent, and forwards to `POST $AURIUM_URL/mcp`.
