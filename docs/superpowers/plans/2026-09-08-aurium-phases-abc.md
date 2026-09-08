# Aurium Phases A–C Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the Aurium vertical slice described by `Aurium_ERD_v0.2.md` §2 phases A, B and C — an operating environment whose unit is an isolated, snapshot-able, stackable container, with a daemon, context engine, IPC, and an MCP gateway layered on top.

**Architecture:** A single Go monorepo. `auriumd` is the only writer to a SQLite database and the only holder of secrets; the CLI, the embedded web dashboard, and the in-container `aurium-mcp` shim are all HTTP clients of its API (D17, D21). Source of truth for code is git — worktrees bind-mounted into containers at identical host paths, guarded by hooks so a child can only move its own branch. Environment is *derived*, never delta-patched (D13): a snapshot is (git tree, rootfs image, volume archives, context version, agent config) taken while paused (D14).

**Tech Stack:** Go 1.27 · SQLite via `modernc.org/sqlite` (pure Go, WAL) · cobra CLI · `docker` CLI driver + a `local` driver · JSON-RPC 2.0 / MCP over stdio and Streamable HTTP · `zalando/go-keyring` · vanilla-JS dashboard embedded with `go:embed`.

## Global Constraints

- Module path `github.com/RhyChaw/aurium`. Go toolchain is at `~/sdk/go/bin/go` (not on PATH by default).
- **`go build ./...` and `go test ./...` MUST pass on a machine with no Docker daemon and no tmux.** Every test needing a container runtime is gated behind `//go:build docker`; every test needing tmux is gated behind `//go:build tmux`.
- Pure-Go SQLite only (`modernc.org/sqlite`) — never `mattn/go-sqlite3`. CGO must stay off so binaries cross-compile static (D20).
- Every SQLite connection sets `PRAGMA journal_mode=WAL`, `PRAGMA foreign_keys=ON`, `PRAGMA busy_timeout=5000` (D20).
- All ids are ULIDs with a type prefix: `p_` project, `t_` task, `c_` container, `s_` snapshot, `a_` agent, `m_` message, `i_` integration, `e_` event, `ap_` approval, `g_` grant, `ci_` context item, `cp_` context proposal, `tok_` token.
- Timestamps are RFC 3339 UTC text, produced by one helper (`ids.Now()`); never `time.Now().String()`.
- **Every state transition emits an event, persisted before the API call returns** (D19).
- Permissions are default-deny, resolved agent → role → container → project, most specific wins, modes `allow | deny | approve` (D18).
- Secrets never enter the database and never enter a container. `integrations.secret_ref` and `tokens.hash` are references/hashes only (D17, §10.6).
- One interactive agent per container (D15). Concurrency happens across containers.
- Exit codes: 0 ok · 1 usage · 2 git · 3 driver · 4 conflict · 5 locked · 6 permission · 7 approval pending (§12.1).
- No `panic` in library code; return errors. `internal/*` packages never import `cmd/*`.

### Deviations from the ERD, recorded deliberately

| ERD says | This plan does | Why |
|---|---|---|
| `sqlc` generates typed queries | Hand-written typed queries in `internal/store` | Removes a build-time codegen dependency; the contract (typed methods over SQL) is identical |
| `oapi-codegen` generates handlers from `api/openapi.yaml` | Hand-written handlers; `api/openapi.yaml` is committed and kept in sync by a test | Same reason; a test asserts every registered route appears in the spec |
| `modelcontextprotocol/go-sdk` | Hand-rolled JSON-RPC 2.0 + MCP in `internal/mcp` | MCP stdio/HTTP is ~300 lines; avoids a fast-moving dependency in the security-critical gateway |
| Dashboard is Vite + React + TypeScript | Vanilla JS + HTML, embedded via `go:embed` | No Node build step in `go build`; dashboard v0 is 5 read-mostly screens |
| Bubble Tea for `aurium tree --watch` | ANSI redraw loop | One less dependency for one command |

---

## File Structure

```
go.mod  go.sum  Makefile  README.md  .gitignore
api/openapi.yaml                     API contract (§11.2), kept in sync by a test
schema/aurium.schema.json            JSON Schema for aurium.yaml (§12.2)

internal/ids/          ids.go           ULID + type prefixes + Now()
internal/config/       config.go        aurium.yaml load/validate (unknown keys are errors)
internal/store/        store.go         open/migrate/tx helpers
                       migrations/*.sql embedded schema (§7)
                       projects.go tasks.go containers.go snapshots.go agents.go
                       tokens.go contextitems.go grants.go proposals.go
                       messages.go integrations.go approvals.go artifacts.go events.go
internal/events/       bus.go           in-process pub/sub + persistence + SSE fan-out
internal/gitx/         gitx.go          exec wrapper, typed errors
                       worktree.go      worktree add/remove, exclude, env (§5.4)
internal/stack/        forest.go        parent/child forest, topological order
                       sync.go          recorded-base rebase engine (§6.5)
internal/runtime/
  driver/              driver.go        Driver interface + DriverCaps (§5.1)
                       docker.go        docker CLI driver
                       local.go         host-process driver (no container)
  image/               image.go         derived image build + cache (§5.2 L1)
  snapshot/            snapshot.go      take/restore (§6.2–6.3)
                       forkstack.go     fork/stack (§6.4)
                       retention.go     gc (§6.7)
internal/agent/        adapter.go       Adapter interface (§9.1)
                       claude.go codex.go shell.go custom.go
                       tmux.go          session helpers
internal/context/      engine.go        items, CAS writes, append (§8.3–8.4)
                       grants.go        context permission resolution
                       proposals.go
                       projection.go    CONTEXT.md generator (§8.5)
                       fts.go           FTS5 query + doc indexer (§8.6)
internal/ipc/          ipc.go           send/inbox/ack, nudges, dependencies (§9.5)
internal/mcp/          jsonrpc.go       JSON-RPC 2.0 framing
                       stdio.go         stdio server/client
                       types.go         MCP initialize/tools/list/tools/call
internal/gateway/      gateway.go       tool registry, per-subject tool list
                       native.go        aurium_* tools (§10.2)
                       upstream.go      upstream MCP spawn/connect + proxy (§10.1)
                       grants.go        capability permission resolution (§10.4)
                       approvals.go     approve-mode hold/execute (§10.5)
                       risk.go          risk classifier (§10.3)
internal/secrets/      secrets.go       keyring + age-file fallback (§10.6)
internal/api/          server.go        mux, middleware, auth
                       handlers_*.go    one file per resource group
                       sse.go           /v1/events
                       mcp.go           POST /mcp
                       dist/            embedded dashboard
internal/daemon/       daemon.go        wiring, lifecycle, socket + TCP listeners
                       watcher.go       ref poll, reconcile, idle pause (§6.6)
                       admission.go     memory budget (§13 Phase B wk10)
cmd/aurium/            main.go + cmd_*.go   CLI (§12.1)
cmd/auriumd/           main.go
cmd/aurium-mcp/        main.go          stdio→HTTP shim
assets/                Dockerfile.tmpl hooks/reference-transaction hooks/pre-push mkuser.sh
web/                   index.html app.js style.css (copied to internal/api/dist)
docs/                  architecture.md quickstart.md deviations.md
```

---

## Phase A — Runtime core (Tasks 1–11)

### Task 1: Module skeleton, ids, Makefile

**Files:**
- Create: `go.mod`, `Makefile`, `.gitignore`, `internal/ids/ids.go`
- Test: `internal/ids/ids_test.go`

**Interfaces:**
- Produces: `ids.New(prefix string) string`, `ids.Prefix(id string) string`, `ids.Valid(id, prefix string) bool`, `ids.Now() string`, and constants `ids.Project="p"`, `ids.Task="t"`, `ids.Container="c"`, `ids.Snapshot="s"`, `ids.Agent="a"`, `ids.Message="m"`, `ids.Integration="i"`, `ids.Approval="ap"`, `ids.Grant="g"`, `ids.ContextItem="ci"`, `ids.Proposal="cp"`, `ids.Token="tok"`.

- [ ] **Step 1: Write the failing test**

```go
package ids

import (
	"strings"
	"testing"
	"time"
)

func TestNewHasPrefixAndIsSortable(t *testing.T) {
	a := New(Container)
	if !strings.HasPrefix(a, "c_") {
		t.Fatalf("want c_ prefix, got %q", a)
	}
	if len(a) != len("c_")+26 {
		t.Fatalf("want 26 ULID chars, got %d in %q", len(a)-2, a)
	}
	time.Sleep(2 * time.Millisecond)
	b := New(Container)
	if !(a < b) {
		t.Fatalf("ULIDs must sort lexicographically by time: %q !< %q", a, b)
	}
}

func TestValidRejectsWrongPrefix(t *testing.T) {
	c := New(Container)
	if Valid(c, Snapshot) {
		t.Fatal("container id must not validate as a snapshot id")
	}
	if !Valid(c, Container) {
		t.Fatal("container id must validate as a container id")
	}
	if Valid("c_short", Container) {
		t.Fatal("malformed id must not validate")
	}
}

func TestNowIsRFC3339UTC(t *testing.T) {
	n := Now()
	parsed, err := time.Parse(time.RFC3339Nano, n)
	if err != nil {
		t.Fatalf("Now() not RFC3339: %v", err)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("Now() must be UTC, got %v", parsed.Location())
	}
	if !strings.HasSuffix(n, "Z") {
		t.Fatalf("Now() must end in Z, got %q", n)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `~/sdk/go/bin/go test ./internal/ids/ -run TestNew -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Implement `internal/ids/ids.go`**

48-bit big-endian millisecond timestamp + 80 bits `crypto/rand`, encoded Crockford base32 (`0123456789ABCDEFGHJKMNPQRSTVWXYZ`) to exactly 26 chars, joined to the prefix with `_`. `Valid` checks the prefix, the length, and that every body character is in the alphabet. `Now` returns `time.Now().UTC().Format(time.RFC3339Nano)`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `~/sdk/go/bin/go test ./internal/ids/ -v` — Expected: PASS (3 tests).

- [ ] **Step 5: Write the Makefile and .gitignore, then commit**

Makefile targets: `build`, `test`, `test-docker` (adds `-tags docker`), `lint` (`go vet ./...`), `fmt`, `clean`. All invoke `$(GO)` defaulting to `$(HOME)/sdk/go/bin/go`.

```bash
git add go.mod Makefile .gitignore internal/ids
git commit -m "feat(ids): ULID identifiers with type prefixes"
```

---

### Task 2: SQLite store — schema and migrations

**Files:**
- Create: `internal/store/store.go`, `internal/store/migrations/0001_init.sql`, `internal/store/migrations/embed.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: `ids` from Task 1.
- Produces: `store.Open(path string) (*Store, error)`, `(*Store).Close() error`, `(*Store).Tx(ctx, func(*sql.Tx) error) error` (wraps `BEGIN IMMEDIATE`), `(*Store).DB() *sql.DB`, `store.OpenMemory() (*Store, error)` for tests.

The migration file is §7 of the ERD verbatim, plus the FTS5 triggers the ERD implies but does not spell out (external-content FTS tables need `ai`/`ad`/`au` triggers or they never populate), plus a `schema_migrations` table.

- [ ] **Step 1: Write the failing test**

```go
package store

import (
	"context"
	"testing"
)

func TestOpenAppliesSchemaAndPragmas(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var fk int
	if err := s.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatal("foreign_keys must be ON")
	}

	for _, table := range []string{
		"projects", "repositories", "tasks", "containers", "snapshots", "agents", "tokens",
		"context_items", "context_versions", "context_grants", "context_proposals", "context_docs",
		"messages", "dependencies", "integrations", "capabilities", "grants", "approvals",
		"artifacts", "events",
	} {
		var n int
		err := s.DB().QueryRow(
			"SELECT count(*) FROM sqlite_master WHERE name = ?", table).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("table %q missing from schema", table)
		}
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	_, err := s.DB().Exec(
		`INSERT INTO tasks (id, project_id, title, status, created_at, updated_at)
		 VALUES ('t_x','p_nonexistent','t','created','now','now')`)
	if err == nil {
		t.Fatal("insert with dangling project_id must fail")
	}
}

func TestTxRollsBackOnError(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	ctx := context.Background()
	_ = s.Tx(ctx, func(tx *sql.Tx) error {
		tx.Exec(`INSERT INTO projects (id,name,root,created_at) VALUES ('p_1','n','/r','now')`)
		return errors.New("boom")
	})
	var n int
	s.DB().QueryRow("SELECT count(*) FROM projects").Scan(&n)
	if n != 0 {
		t.Fatal("failed Tx must roll back")
	}
}
```

- [ ] **Step 2: Run it — Expected: FAIL, `undefined: OpenMemory`.**
- [ ] **Step 3: Write `0001_init.sql` (ERD §7 verbatim + FTS triggers + `schema_migrations`) and `store.go`.**

`Open` builds the DSN `file:<path>?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)`, sets `db.SetMaxOpenConns(1)` for the writer path, then applies every embedded migration not present in `schema_migrations` inside one transaction.

- [ ] **Step 4: Run `~/sdk/go/bin/go test ./internal/store/ -v`** — Expected: PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(store): SQLite schema and migrations"`

---

### Task 3: Store queries — projects, repositories, tasks, containers

**Files:** Create `internal/store/projects.go`, `tasks.go`, `containers.go`; Test `internal/store/queries_test.go`

**Interfaces:**
- Produces: types `store.Project`, `store.Repository`, `store.Task`, `store.Container` mirroring the §7 columns, and methods `CreateProject`, `GetProject`, `ProjectByRoot`, `ListProjects`, `CreateRepository`, `GetRepository`, `CreateTask`, `GetTask`, `ListTasks`, `TransitionTask(ctx, id, status string)`, `CreateContainer`, `GetContainer`, `GetContainerBySlug`, `ListContainers(ctx, projectID string)`, `UpdateContainerStatus`, `UpdateContainerBaseSHA`, `UpdateContainerHeadSHA`, `SetContainerRuntime(ctx, id, runtimeID, image, network string, ports map[int]int)`, `ArchiveContainer`, `DeleteContainer`.

Tests assert: the `UNIQUE (repo_id, branch)` constraint rejects a second container on the same branch; an invalid `status` is rejected by the CHECK constraint; `TransitionTask` bumps `updated_at`; `ports_json` round-trips a `map[int]int`.

- [ ] Steps 1–5 as Task 1's shape (failing test → run → implement → pass → commit `feat(store): project, task and container queries`).

---

### Task 4: git exec wrapper

**Files:** Create `internal/gitx/gitx.go`; Test `internal/gitx/gitx_test.go`

**Interfaces:**
- Produces:
```go
type Git struct{ Dir string; Env []string; Verbose bool }
func New(dir string) *Git
func (g *Git) Run(ctx context.Context, args ...string) (string, error)   // trimmed stdout
func (g *Git) RunOK(ctx context.Context, args ...string) error
func (g *Git) RevParse(ctx context.Context, rev string) (string, error)
func (g *Git) CurrentBranch(ctx context.Context) (string, error)
func (g *Git) IsClean(ctx context.Context) (bool, error)
func (g *Git) MergeBase(ctx context.Context, a, b string) (string, error)
func (g *Git) AheadBehind(ctx context.Context, a, b string) (ahead, behind int, err error)
func (g *Git) RefExists(ctx context.Context, ref string) bool
func (g *Git) UpdateRef(ctx context.Context, ref, sha string) error

type Error struct{ Args []string; Stderr string; Code int }
func (e *Error) Error() string
func IsConflict(err error) bool   // stderr matches CONFLICT| could not apply |needs merge
```

Tests build a real repo in `t.TempDir()` (`git init -b main`, a commit, a branch) and assert `AheadBehind`, `IsClean` before/after a stray file, and that `IsConflict` is true for a deliberately conflicting `git rebase`.

- [ ] Steps 1–5. Commit `feat(gitx): git exec wrapper with typed errors`.

---

### Task 5: Worktrees, identical paths, guard hooks

**Files:** Create `internal/gitx/worktree.go`, `assets/hooks/reference-transaction`, `assets/hooks/pre-push`, `assets/embed.go`; Test `internal/gitx/worktree_test.go`

**Interfaces:**
- Produces: `gitx.AddWorktree(ctx, repoRoot, slug, branch, startRef string) (path string, err error)` creating `<repoRoot>/.aurium/wt/<slug>`, `gitx.RemoveWorktree(ctx, repoRoot, slug string, force bool) error`, `gitx.EnsureExcluded(repoRoot string) error` appending `.aurium/` to `.git/info/exclude`, `gitx.InstallHooks(repoRoot string) error` writing `assets/hooks/*` into `<repoRoot>/.aurium/hooks` mode 0755, and `gitx.ContainerEnv(repoRoot, branch string, authorName, authorEmail string) []string` returning the `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n` set for `safe.directory=*`, `gc.auto=0`, `commit.gpgsign=false`, `core.hooksPath=<repoRoot>/.aurium/hooks`, plus `GIT_AUTHOR_*`, `GIT_COMMITTER_*`, `AURIUM_GUARD=1`, `AURIUM_BRANCH=<branch>` (§5.4).

`reference-transaction` is the Invariant-1 enforcement point (§5.5): when `AURIUM_GUARD=1`, in the `prepared` phase, it reads `<old> <new> <ref>` lines on stdin and exits 1 if any `refs/heads/*` line names a branch other than `$AURIUM_BRANCH`.

- [ ] **Step 1: Write the failing test** — the important one:

```go
func TestGuardHookBlocksWritingAnotherBranch(t *testing.T) {
	repo := initRepo(t)                       // helper: git init -b main + one commit
	if err := InstallHooks(repo); err != nil { t.Fatal(err) }
	wt, err := AddWorktree(ctx, repo, "feature", "feature", "main")
	if err != nil { t.Fatal(err) }

	g := New(wt)
	g.Env = append(os.Environ(), ContainerEnv(repo, "feature", "A", "a@e")...)

	// Writing our own branch is allowed.
	os.WriteFile(filepath.Join(wt, "f.txt"), []byte("x"), 0o644)
	if err := g.RunOK(ctx, "add", "-A"); err != nil { t.Fatal(err) }
	if err := g.RunOK(ctx, "commit", "-m", "own branch"); err != nil {
		t.Fatalf("commit on own branch must succeed: %v", err)
	}

	// Writing somebody else's branch is refused by the guard hook.
	err = g.RunOK(ctx, "branch", "-f", "main", "HEAD")
	if err == nil {
		t.Fatal("guard hook must refuse an update to refs/heads/main from the feature worktree")
	}
}
```

- [ ] **Steps 2–5** — run (FAIL: undefined), implement, run (PASS), commit `feat(gitx): worktrees, identical-path env and guard hooks`.

---

### Task 6: Driver interface and the `local` driver

**Files:** Create `internal/runtime/driver/driver.go`, `internal/runtime/driver/local.go`; Test `internal/runtime/driver/local_test.go`

**Interfaces:**
- Produces:
```go
type Filesystem string; const (FSShared Filesystem = "shared"; FSRemote Filesystem = "remote")
type Caps struct{ Filesystem Filesystem; Pause, Checkpoint, Sidecars, Snapshot bool }

type Spec struct {
	Name, Image, Network, Workdir string
	Env       []string
	Binds     []Bind          // host path == container path (D3)
	Volumes   []VolumeMount
	Ports     []PortSpec
	Labels    map[string]string
	Resources Resources
}
type Driver interface {
	Name() string
	Capabilities() Caps
	Create(ctx context.Context, s Spec) (runtimeID string, err error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Pause(ctx context.Context, id string) error
	Unpause(ctx context.Context, id string) error
	Destroy(ctx context.Context, id string, keepVolumes bool) error
	Exec(ctx context.Context, id string, cmd []string, o ExecOpts) (ExecResult, error)
	Ports(ctx context.Context, id string) (map[int]int, error)
	Snapshot(ctx context.Context, id, ref string) (RootfsRef, error)
	Restore(ctx context.Context, s Spec, from RootfsRef) (string, error)
	CloneVolumes(ctx context.Context, from, to []VolumeMount) error
	Inspect(ctx context.Context, id string) (State, error)
	List(ctx context.Context, labels map[string]string) ([]Handle, error)
}
```
The `local` driver runs commands directly on the host in the worktree (no isolation); `Capabilities()` reports `Pause:false, Snapshot:false`. It exists so the whole daemon, CLI, context engine, IPC and gateway are testable with no Docker.

- [ ] Steps 1–5. Tests: `Create`/`Exec` echoes, `Ports` empty, `Capabilities().Snapshot == false`, `Snapshot` returns `ErrUnsupported`. Commit `feat(runtime): Driver interface and local driver`.

---

### Task 7: Docker driver

**Files:** Create `internal/runtime/driver/docker.go`; Test `internal/runtime/driver/docker_test.go` (`//go:build docker`) and `internal/runtime/driver/docker_args_test.go` (no build tag — pure argv construction).

**Interfaces:** Produces `driver.NewDocker(bin string) *Docker` implementing `Driver`; `Capabilities()` = `{FSShared, Pause:true, Checkpoint:false, Sidecars:true, Snapshot:true}`.

The argv test is the one that runs everywhere and is where the §5.3 detail lives:

```go
func TestCreateArgsCarryLabelsBindsAndLocalhostPorts(t *testing.T) {
	args := createArgs(Spec{
		Name: "aurium-app-feature", Image: "aurium-local/node-20-alpine:3fa1",
		Network: "aurium-app-feature",
		Binds: []Bind{{Host: "/Users/a/app/.git", Container: "/Users/a/app/.git", RW: true}},
		Ports: []PortSpec{{Internal: 3000}},
		Labels: map[string]string{"aurium.container": "c_1"},
		Env: []string{"AURIUM_TOKEN=t"},
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--label aurium.container=c_1",
		"-v /Users/a/app/.git:/Users/a/app/.git:rw",   // identical host path (D3)
		"-p 127.0.0.1:0:3000",                          // never 0.0.0.0 (D11)
		"--network aurium-app-feature",
		"--init",
		"sleep infinity",                                // D5
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker create args missing %q\ngot: %s", want, joined)
		}
	}
	if strings.Contains(joined, "0.0.0.0") {
		t.Error("ports must never bind 0.0.0.0 (D11)")
	}
}
```

- [ ] Steps 1–5. Commit `feat(runtime): docker CLI driver`.

---

### Task 8: Derived image build and cache

**Files:** Create `internal/runtime/image/image.go`, `assets/Dockerfile.tmpl`, `assets/mkuser.sh`; Test `internal/runtime/image/image_test.go`

**Interfaces:** Produces `image.Builder` with `Tag(base, agentLayer string, uid, gid int) string` (deterministic `aurium-local/<sanitised-base>:<sha256[:12] of base+layer+uid+gid>`) and `Ensure(ctx, spec) (tag string, err error)` which skips the build when `docker image inspect` succeeds. The Dockerfile template installs `git tmux bash zstd`, runs `mkuser.sh` to bake the **host** UID/GID as user `aurium` with `HOME=/home/aurium` (§5.4), copies the `aurium-mcp` binary to `/usr/local/bin`, and appends the adapter's `ImageLayer()` fragment.

Testable without Docker: `Tag` is deterministic and changes when any input changes; `renderDockerfile` output contains the agent layer and the uid.

- [ ] Steps 1–5. Commit `feat(runtime): derived image build and cache`.

---

### Task 9: Recorded-base sync engine

**Files:** Create `internal/stack/forest.go`, `internal/stack/sync.go`; Test `internal/stack/sync_test.go`

**Interfaces:** Produces
```go
type Node struct{ ContainerID, Branch, ParentBranch, BaseSHA, HeadSHA string; Children []*Node }
func BuildForest(cs []store.Container, baseBranch string) []*Node
func TopoOrder(roots []*Node) []*Node        // parents before children

type Eligibility string
const (Eligible Eligibility="eligible"; UpToDate="up_to_date"; Dirty="dirty"; Stale="stale"; Drifted="drifted"; Conflict="conflict")
func CheckEligibility(ctx, g *gitx.Git, c Target) (Eligibility, string, error)
func Sync(ctx, g *gitx.Git, c Target, o Options) (Result, error)
```
`Sync` is D7/D8: run on the **host**, in the child's worktree, `git rebase --onto <parentTip> <recordedBase> <branch>`, and on success set `base_sha = parentTip`. Preconditions (D8): worktree clean (unless `--autostash`), no rebase in progress, parent ref exists, recorded base is an ancestor of the branch (else `drifted`).

This is the highest-value integration test in Phase A — it runs against real git, no Docker:

```go
func TestSyncRebasesChildOntoMovedParentAndUpdatesBase(t *testing.T) {
	repo := initRepo(t)                        // main @ c0
	parentWT := addWorktree(t, repo, "A", "main")
	childBase := headSHA(t, repo, "A")         // recorded base
	childWT := addWorktree(t, repo, "B", "A")

	commit(t, childWT, "b1.txt", "child work")
	commit(t, parentWT, "a1.txt", "parent moves")
	parentTip := headSHA(t, repo, "A")

	res, err := Sync(ctx, gitx.New(childWT), Target{
		Branch: "B", ParentBranch: "A", BaseSHA: childBase,
	}, Options{})
	if err != nil { t.Fatal(err) }
	if res.NewBaseSHA != parentTip {
		t.Fatalf("base_sha must advance to the parent tip: got %s want %s", res.NewBaseSHA, parentTip)
	}
	// The child kept its own work and gained the parent's.
	assertFileExists(t, childWT, "b1.txt")
	assertFileExists(t, childWT, "a1.txt")
	// The parent was not touched (Invariant 1).
	if headSHA(t, repo, "A") != parentTip {
		t.Fatal("sync must not move the parent branch")
	}
}

func TestSyncReportsConflictWithoutLosingWork(t *testing.T) {
	// parent and child both edit the same line -> Eligibility Conflict,
	// rebase left in progress for the agent to resolve, base_sha unchanged.
}

func TestSyncRefusesDirtyWorktree(t *testing.T) { /* Dirty, no rebase attempted */ }
```

- [ ] Steps 1–5. Commit `feat(stack): recorded-base rebase engine and forest`.

---

### Task 10: Agent adapters

**Files:** Create `internal/agent/adapter.go`, `tmux.go`, `shell.go`, `claude.go`, `codex.go`, `custom.go`; Test `internal/agent/adapter_test.go`, `internal/agent/tmux_test.go` (`//go:build tmux`)

**Interfaces:** Produces the §9.1 `Adapter` interface exactly, `agent.Registry` with `Get(name string) (Adapter, bool)` and `Register(Adapter)`, and `agent.Projection` (the struct `Prepare` receives: `ContextPath`, `MCPCommand`, `Token`, `AuriumURL`, `Home`).

`Prepare` writes into the container `$HOME` per §8.5 / Appendix B: claude → `~/.claude/CLAUDE.md` containing `@<wt>/.aurium/CONTEXT.md` plus a `~/.claude.json` `mcpServers.aurium` entry; codex → `~/.codex/AGENTS.md` plus `~/.codex/config.toml` `[mcp_servers.aurium]`; custom → `AURIUM_CONTEXT_FILE`. Each adapter file carries a `// verified against <docs URL> on <date>` comment (§15 risk mitigation).

Tests assert the exact file contents each adapter writes (using the `local` driver against a temp `$HOME`), and that `Capabilities()` is right per the §9.1 table.

- [ ] Steps 1–5. Commit `feat(agent): adapter interface with claude, codex and shell adapters`.

---

### Task 11: `aurium.yaml` config + `aurium init/create/tree/sync/destroy/doctor` (Phase A gate)

**Files:** Create `internal/config/config.go`, `schema/aurium.schema.json`, `cmd/aurium/main.go`, `cmd/aurium/cmd_init.go`, `cmd_container.go`, `cmd_tree.go`, `cmd_sync.go`, `cmd_exec.go`, `cmd_attach.go`, `cmd_doctor.go`, `internal/runtime/manager.go`; Test `internal/config/config_test.go`, `cmd/aurium/cli_test.go`

**Interfaces:** Produces `config.Load(path string) (*Config, error)` with the full §12.2 shape and **unknown keys as errors** (`yaml.Decoder.KnownFields(true)`), and `runtime.Manager` — the object that owns "create a container": resolve config → ensure image → add worktree → install hooks → driver.Create → record ports → adapter.Prepare → post_create hooks → adapter.Start → emit events (§5.3).

- [ ] Config tests: a valid file round-trips; `sandbox: {drivr: docker}` (typo) is an error naming the key; defaults fill in (`idle_pause_minutes: 15`, `snapshot.keep_last: 10`, `include_ignored: false`).
- [ ] CLI test: `aurium init` in a temp repo writes `aurium.yaml`, `.aurium/hooks/*`, the `.git/info/exclude` entry, and a `projects` row.
- [ ] **Phase A gate test** (`internal/stack/cascade_test.go`, real git, no Docker): build a 3-deep chain main→A→B→C, commit in A, assert B and C both report `stale`, cascade-sync in topological order, assert every branch keeps its own commits, `base_sha` advanced at each level, and `git fsck` is clean.
- [ ] Commit, then **open the Phase A PR** — this is a major benchmark.

---

## Phase B — Daemon, snapshots, stacking, dashboard v0 (Tasks 12–20)

### Task 12: Event bus + events store
**Produces:** `events.Bus` with `Emit(ctx, Event) error` (persists **then** fans out, D19), `Subscribe(filter) (<-chan Event, func())`, `Replay(ctx, sinceID int64, filter) ([]Event, error)`. Test: emit persists before subscribers observe; `Replay` is ordered by `id`; a slow subscriber is dropped, never blocking `Emit`.

### Task 13: Token auth + scopes
**Produces:** `store.CreateToken(ctx, containerID, agentID string, scopes []string) (plaintext string, err error)` (stores only a SHA-256 hash), `store.AuthenticateToken(ctx, plaintext) (*TokenInfo, error)`, `TokenInfo.Has(scope string) bool` with glob support (`context:*`). Tests: a revoked token fails; a destroyed container's token fails; `snapshot:self` does not satisfy `snapshot:other`.

### Task 14: Daemon skeleton, HTTP API, SSE
**Produces:** `daemon.New(cfg) (*Daemon, error)`, listeners on `~/.aurium/auriumd.sock` and `127.0.0.1:7770`, bearer middleware, `GET /v1/health`, `GET /v1/events` (SSE with `since`/`types` filters), and the projects/tasks/containers routes of §11.2. Also `cmd/aurium` auto-start: if the socket is absent, spawn `auriumd` detached and poll `/v1/health` for 5s. Tests use `httptest` — no real sockets.

### Task 15: Snapshot take + restore
**Produces:** `snapshot.Take(ctx, deps, containerID, label, trigger string) (*store.Snapshot, error)` implementing §6.2 step-for-step — `driver.Pause` → temp-index `git add -A` + `write-tree` + `commit-tree` + `update-ref refs/aurium/snap/<c>/<seq>` → `driver.Snapshot` → per-volume `tar | zstd` → manifest.json → `driver.Unpause` → emit — and `snapshot.Restore(ctx, deps, containerID string, seq int, backup bool) error` implementing §6.3. The git half is fully testable with no Docker (the driver is `local`, which reports `Snapshot:false`, so the manifest records no rootfs) and that is the subtle half:

```go
func TestTakeCapturesTrackedUntrackedAndHonoursGitignore(t *testing.T) {
	// worktree with: committed.txt (tracked), scratch.txt (untracked),
	// node_modules/x (gitignored)
	snap := mustTake(t, ...)
	tree := lsTree(t, repo, snap.TreeRef)
	assertContains(t, tree, "committed.txt", "scratch.txt")
	assertNotContains(t, tree, "node_modules/x")   // include_ignored:false (§5.2)
}

func TestRestoreRevertsTrackedAndUntrackedEdits(t *testing.T) {
	// take -> edit committed.txt, delete scratch.txt, add junk.txt -> restore
	// -> committed.txt back, scratch.txt back, junk.txt gone
}
```

### Task 15b: Snapshot manifest
**Produces:** `snapshot.Manifest` marshalling to §6.1's JSON exactly (`schema:1`, `git`, `rootfs`, `volumes`, `context_version`, `agents`, `env_hash`, `ports`). Test: a golden-file round-trip against the ERD's example.

### Task 16: Fork and stack
**Produces:** `snapshot.Fork(ctx, deps, containerID string, at int, branch string) (*store.Container, error)` and `snapshot.Stack(ctx, deps, parentID, newBranch string, at int) (*store.Container, error)` implementing the §6.4 table — the difference that matters is the git column: **fork** sets `parent = c.parent` and `base_sha = c.base_sha@seq`; **stack** sets `parent = c.branch` and `base_sha = head_sha@seq`. Tests assert exactly that, plus `origin_kind`, plus that forking does not move the source branch.

### Task 17: `sync --env` re-derivation
**Produces:** `snapshot.SyncEnv(ctx, deps, childID string) error` — §6.5: snapshot child (`pre_sync`) → destroy container keeping the worktree → recreate from the parent's latest snapshot image → clone the parent's latest volume archives → run `hooks.post_create` → restart the agent with `Resume:true`. Docker-tagged integration test: child `npm i -g cowsay` ad hoc → `sync --env` → cowsay gone, `post_create` deps present, source rebased.

### Task 18: Retention and gc
**Produces:** `snapshot.GC(ctx, deps, projectID string, policy Policy) (Report, error)` — keep last N + all labelled + anything referenced by a container's `origin_snapshot_id`; delete git refs, images and archives; run `git gc --auto`. Test: 15 snapshots, 3 labelled, 1 referenced, `keep_last:10` → exactly the right set survives.

### Task 19: Watcher, reconcile, idle pause, admission control
**Produces:** `daemon.Watcher` goroutine group — ref poll 2s (marks children `stale`, emits `container.parent_changed{commits:N}`), container reconcile 30s (driver labels ↔ DB rows), idle pause 60s (`idle_pause_minutes`), auto-snapshot on task transitions — and `daemon.Admission` with a memory budget that puts containers in `queued` rather than over-committing. Tests drive a **fake clock** and a fake driver; no real timers.

### Task 20: Dashboard v0 (Phase B gate)
**Produces:** `web/{index.html,app.js,style.css}` embedded at `internal/api/dist` — Containers (list + tree + status + ports + copyable `aurium attach` command), Events (live SSE), Tasks, Snapshots (list/restore/fork/stack). Test: `GET /` serves HTML; every route the mux registers appears in `api/openapi.yaml`.

- [ ] **Phase B gate:** the first half of the §73 demo (create A, B, C; stack B on A; commit in A; B unchanged; sync; restore) runs from the CLI and is visible live in the dashboard. Commit, then **open the Phase B PR**.

---

## Phase C — Context, IPC, gateway, delegation (Tasks 21–30)

### Task 21: Context items, versions, CAS
**Produces:** `context.Engine` with `Get`, `List`, `Append`, `Write(ctx, ref, baseVersion int, content, reason string) (int, error)` and `ErrStale{CurrentVersion int; Diff string}`. `Write` is §8.4 exactly: `BEGIN IMMEDIATE` → `SELECT version` → match ⇒ `UPDATE`+`INSERT` version row+emit ⇒ new version; mismatch ⇒ `ROLLBACK` ⇒ 409. **The concurrency test is mandatory:** two goroutines writing the same item with the same `base_version` — exactly one succeeds, one gets `ErrStale`, and `context_versions` gains exactly one row.

### Task 22: Context grants
**Produces:** `context.ResolvePerm(ctx, subject Subject, ref ItemRef) (Perm, error)` — resolution order agent → role → container → project, most specific wins, default deny — and `context.SeedDefaults(ctx, projectID string)` writing the §8.3 table (primary/master, worker, human). Tests cover each row of that table plus a stacked child getting `read` on its parent chain's container context via the `parent_container_id` walk (§8.1).

### Task 23: Proposals
**Produces:** `context.Propose`, `context.Decide(ctx, proposalID, decision string)`, and staleness: a proposal goes `stale` when its item moves past `base_version`. Test asserts the state machine `open → accepted|rejected|stale`.

### Task 24: Projection writer
**Produces:** `context.Projection.Render(ctx, containerID string) ([]byte, error)` producing the §8.5 CONTEXT.md (Objective, Constraints, Stack, Decisions last 10, Discoveries last 10, Inbox count, Environment rule, Tools) and `Projection.Write` writing it to `<wt>/.aurium/CONTEXT.md` on `Prepare()` and on every relevant `context.updated`. Test: a `context.updated` event regenerates the file within 1s (§14) and the rendered Markdown contains the parent/base/status Stack line.

### Task 25: FTS retrieval and doc indexing
**Produces:** `context.Query(ctx, subject, q string, scopes []string, k int) ([]Hit, error)` — FTS5 BM25 over `context_fts` restricted to the caller's readable scopes, unioned with `docs_fts` — and `context.Indexer` walking `project.context` paths, splitting Markdown on headings, re-indexing on hash change every 60s. Test: an item readable only by container A does not appear in B's results (this is a security property, not a nicety).

### Task 26: MCP protocol + `aurium-mcp` shim
**Produces:** `internal/mcp` — JSON-RPC 2.0 framing, `initialize`, `tools/list`, `tools/call`, a stdio `Serve(r io.Reader, w io.Writer, h Handler)` and an HTTP client — and `cmd/aurium-mcp` reading `AURIUM_URL` + `/run/aurium/token` (or `AURIUM_TOKEN`) and forwarding to `POST /mcp`. Tests run the shim in-process against an `httptest` daemon and assert a full `initialize`→`tools/list`→`tools/call` round trip, and that a malformed frame returns `-32700` rather than crashing.

### Task 27: Native `aurium_*` tools
**Produces:** `gateway.NativeTools` — the whole §10.2 table: `aurium_context_query|get|list|append|write|propose`, `aurium_ipc_send|inbox|ack`, `aurium_task_status|info`, `aurium_snapshot`, `aurium_delegate`, `aurium_merge`, `aurium_request_approval` — each with a JSON Schema and each checking the caller's token scope and permission. Test: `aurium_context_write` without `write` returns a permission error, not a silent no-op; `aurium_snapshot` with scope `snapshot:self` cannot snapshot another container.

### Task 28: IPC
**Produces:** `ipc.Send`, `ipc.Inbox` (marks `delivered`), `ipc.Ack`, the tmux nudge, the 5-minute high-priority re-nudge, and the §9.5 per-type side effects (`BLOCKED` → agent+task blocked + human notified; `ARTIFACT` → `artifacts` row; `DEPENDENCY` → `dependencies` row + a follow-on `INFORMATION` message when the referenced context key changes; `CONFLICT` → raised by the stack engine). Tests use a fake clock for the re-nudge and assert each side effect.

### Task 29: MCP gateway — upstream, registry, grants, approvals
**Produces:** `gateway.Gateway` — upstream spawn/connect with credentials from the keyring, `tools/list` → `capabilities` rows with `risk` from the §10.3 classifier, per-(container, agent) tool lists where ungranted tools are **not listed at all** and a call to a hidden tool returns `-32601`, the §10.4 resolution with the documented seeds, and the §10.5 approve flow (insert pending + expire at 30m, return `{"status":"pending_approval"}` immediately, execute on approval, deliver the result as a `RESPONSE` message). Plus `internal/secrets` (keyring + age-file fallback).

Mandatory tests (§14): hidden tool not listed and `-32601` on call; `approve` holds then executes and delivers; a container-scope `deny` overrides a project-scope `allow`; **and the grep test — the upstream's credential appears nowhere in the container's env or filesystem.**

### Task 30: Delegation, task lifecycle, PR artifact (Phase C gate)
**Produces:** `aurium_delegate(title, adapter, mode, prompt)` for `master` agents — **fork** mode (snapshot master → fork container → new worker agent + `DEPENDENCY` message back; worker's `review` sends the master an `ARTIFACT` with branch + diff stats; `aurium_merge(child)` runs the same eligibility as sync and merges on the host only when the master's worktree is clean) and **serial** mode (headless `adapter.Execute` inside the master's container while its session is paused). Depth capped at 1. Plus the task state machine and `aurium pr` creating a PR via the gateway's `github_*` capability and recording an `artifacts` row.

- [ ] **Phase C gate:** the §73 demo end to end — Codex master delegates to Claude workers in forked containers, workers report `review`, master merges, a GitHub PR is created through the gateway, one `merge` attempt is held for approval and approved from the dashboard, and the whole trail is in `aurium events`. Commit, then **open the Phase C PR**.

---

## Self-Review

**Spec coverage.** §5.1 Task 6–7 · §5.2 Task 8 · §5.3 Task 7/11 · §5.4 Task 5 · §5.5 Task 5 (git), 16 (volumes/rootfs), 22 (context) · §6.1 Task 15b · §6.2–6.3 Task 15 · §6.4 Task 16 · §6.5 Task 9 (source) + 17 (env) · §6.6 Task 19 · §6.7 Task 18 · §7 Tasks 2–3 · §8.1–8.2 Task 21 · §8.3 Task 22 · §8.4 Task 21 · §8.5 Task 24 · §8.6 Task 25 · §9.1 Task 10 · §9.2 Task 22 · §9.3 Task 28 · §9.4 Task 30 · §9.5 Task 28 · §10.1 Task 29 · §10.2 Task 27 · §10.3–10.5 Task 29 · §10.6 Task 29 · §11.1 Task 12 · §11.2 Task 14 · §11.3 Task 20 · §12.1 Task 11 (+ commands added per phase) · §12.2 Task 11 · §14 test rows are attached to the task that owns each behaviour.

**Gap found and closed:** the ERD's §7 declares external-content FTS5 tables but no triggers, so `context_fts` would never populate. Task 2 adds the `ai`/`ad`/`au` triggers explicitly.

**Gap found and closed:** §6.1's manifest has no task of its own in a naive reading of §13; split out as Task 15b so the golden-file test exists.

**Type consistency:** `base_sha` is the recorded base everywhere (`store.Container.BaseSHA`, `stack.Target.BaseSHA`, `snapshot.Manifest.Git.BaseSHA`); `seq` is always `int`; `Perm` (context) and `Mode` (capability grants) are deliberately distinct types — context has `read|append|propose|write`, capabilities have `allow|deny|approve`, and they must never be conflated.
