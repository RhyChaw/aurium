# Setup — from clone to a running dashboard in one command

**Date:** 2026-09-15
**Status:** accepted, not yet implemented
**Extends:** `README.md` ("Run it"), the `run`/`build`/`shim` targets in
`Makefile`, and `aurium doctor` in `internal/cli/misc.go`.

## The problem

The README says the way in is `make run`. On a machine that has everything
that is true. This design exists because of what happened on a machine that
did not: getting Aurium from a clean clone to a dashboard took seven manual
interventions, and the tool built to prevent exactly that — `aurium doctor` —
reported a clean bill of health for the thing that was broken.

Five distinct failures, in the order they were hit:

1. **No Go toolchain at all.** The `Makefile` notes that the toolchain is
   pinned in `go.mod` and that "`go build` will fetch and use it regardless of
   what is on PATH". That is true only once *some* `go` exists to honour the
   pin. With no Go on the machine there is nothing to bootstrap from, and the
   first instruction in the README fails at the first word.
2. **A present-but-non-functional `git`.** macOS ships `/usr/bin/git` as a
   shim that refuses to run until the Xcode licence is accepted. The file
   exists, is executable, and errors on every invocation. This broke `make`,
   `python3` and `clang` identically, and it broke `go build` in a way that
   named neither git nor the licence — only `error obtaining VCS status: exit
   status 69`.
3. **`doctor` passed it anyway.** `binaryExists` is `exec.LookPath` and
   nothing more, so it answers "is there a file at this name" when the
   question is "does this work". `doctor` printed `ok git` while every git
   invocation on the machine was failing. A diagnostic that gives a false
   pass on a live blocker is worse than no diagnostic, because it redirects
   the search away from the cause.
4. **A port collision with no readable explanation.** `:7770` was held by
   another macOS user's daemon. `aurium up` correctly refused to start a
   second one, but suggested `--restart`, which cannot work across a user
   boundary — the process belongs to someone else. The message also reported
   two contradictory uptimes for the same process.
5. **Docker installed but not running, `tmux` absent.** The only two failures
   `doctor` actually caught.

Nothing in that list is exotic. Items 1 and 2 are the default state of a Mac
that has never been used for Go development — which is the state of every new
contributor's machine, and therefore the state this project must handle well
if it wants contributors at all.

## Shape

Two stages, split at the only place the system can be split: **a Go binary
cannot install Go.** Everything before the first successful `go build` must be
shell; everything after it belongs in Go, where it can be tested.

```
setup.sh  ──►  aurium doctor --fix  ──►  first-run wizard in the dashboard
(pre-Go)       (machine readiness)       (app readiness)
```

The split is not cosmetic. Stage 0 cannot be unit-tested in Go and must stay
small enough to read in one sitting. Stage 1 is where the knowledge lives.
Stage 2 is where a human is finally looking at a screen.

## Stage 0 — `setup.sh`

POSIX `sh` at the repo root, also servable as `curl -fsSL …/setup.sh | sh`.
Targets `darwin|linux` × `arm64|amd64`. It does only what must precede a Go
binary:

1. Detect OS and architecture.
2. Probe prerequisites **by running them**, never by locating them. A
   candidate binary is executed (`git --version`) and judged on its exit
   status, with stderr preserved for the report. This is the one rule that
   turns failure 2 from a mystery into a sentence.
3. If no usable Go (`>= 1.25`) is on PATH, download the pinned `go1.27.1`,
   verify it against a **SHA256 committed to this repo**, and extract it to
   `~/.local/go`. No `sudo`, no system directories, no package manager.
4. Build with `CGO_ENABLED=0`. Aurium's only SQLite dependency is
   `modernc.org/sqlite`, which is pure Go, so the build needs no C compiler —
   which is why an unusable `clang` does not have to stop it. Pass
   `-buildvcs=false` when git is not functional.
5. `exec ./bin/aurium doctor --fix`.

Two constraints are load-bearing. The script **must not require `make`**,
because `make` is among the tools the Xcode licence disables; it calls
`go build` directly, and the `Makefile` gains a `setup` target that delegates
to the script rather than the other way round. And it **must never escalate
privileges**, so that piping it from `curl` is a defensible thing to ask a
stranger to do.

## Stage 1 — `aurium doctor --fix`

The checks move out of `newDoctorCmd` into a new `internal/preflight` package
as data: a table of `{Name, Probe, Severity, Remedy}`. `doctor` becomes one
renderer of that table rather than the place it is written down.

`binaryExists` is replaced by `binaryWorks`: `LookPath`, then execute with a
short timeout, treating a non-zero exit as failure and surfacing the first
line of stderr. Today's silent pass becomes the whole answer:

```
FAIL  git: xcrun: error: You have not agreed to the Xcode license
      fix: sudo xcodebuild -license accept   (needs a real terminal)
```

Remedies come in exactly two kinds:

- **Auto** — unprivileged and idempotent: create `~/.aurium`, install the
  cross-compiled `aurium-mcp` shims, `open -a Docker`.
- **Manual** — prints the exact command and exits non-zero. Anything needing
  `sudo`, a GUI installer, or a TTY lives here permanently. `doctor` never
  escalates on the user's behalf.

New checks: the Go toolchain; **port availability, naming the owning user and
pid** when the address is held, and suppressing the `--restart` suggestion
when the holder belongs to someone else; Docker daemon; `tmux` (optional);
free disk space. A `--json` flag makes the whole table consumable by CI.

## Stage 2 — the first-run wizard

The daemon exposes `GET /v1/preflight`, returning the same table Stage 1
renders as text. **That shared table is the point of the whole design:** the
CLI and the wizard cannot drift into two different opinions about what Aurium
needs, because there is only one list and neither surface owns it.

A new `internal/api/web/views/setup.js` follows the existing view convention —
ES modules, no build step, render from the one store. `app.js` routes to it
when a daemon has no projects *and* no provider accounts. The steps:

1. **Environment** — the preflight table, live over the existing SSE stream,
   so a fix applied in a terminal turns the row green without a reload.
2. **Connect a provider** — reuses `renderProviders`.
3. **Connect GitHub** — already built.
4. **First project and container.**

The wizard is skippable at every step and re-enterable afterwards. It is a
screen, not a modal, and never a gate in front of the dashboard.

## Error handling

Every failure names the **blocking step**, not a downstream symptom. The
`Makefile` already records this lesson in a comment — a missing shim once
surfaced as `no such file` against a path nothing had ever written to, naming
the victim instead of the cause. `error obtaining VCS status: exit status 69`
is the same bug in a different coat. A preflight failure reports what was run,
what it returned, and the one command that fixes it.

Network failures fetching Go retry once, then explain what to download by hand
and where to put it. A checksum mismatch is fatal and never retried.

## Testing

Test-driven, and the first test written is the regression test for the bug
this design was born from.

- `internal/preflight`: table-driven unit tests over fake probes. The required
  case is **binary present but exits non-zero**, which today reports `ok`.
  Also: port held by another user, Go present but too old, Go absent.
- `setup.sh`: `shellcheck` and `sh -n` in CI, plus an end-to-end run in a bare
  `ubuntu` container with no Go installed, asserting that `/v1/health` answers
  afterwards. That container is the only honest test of stage 0, because it is
  the only environment that genuinely lacks a toolchain.
- Wizard: `TestEveryDashboardModuleIsEmbedded` checks a hardcoded list, so
  `views/setup.js` must be added to it — the test does not discover new views,
  and a view missing from that list is a blank page with one console error.
  `TestModuleImportsResolveToJavaScript` then covers it for free. A smoke test
  asserts the wizard renders on an empty daemon and does not on a populated
  one.
- CI matrix: `macos-latest` and `ubuntu-latest`.

## Out of scope

Windows and PowerShell (WSL2 is the Linux path). Installing Docker on the
user's behalf. Managing the Rust/Tauri toolchain — `desktop/` stays optional
and setup only detects it and points at `desktop/README.md`. Package-manager
integration. Uninstall.

## Deliberately excluded, and why

Two real defects were found while diagnosing the above. Neither belongs here:

- **Cross-user token exposure.** The dashboard token is served in the HTML of
  a loopback listener, and any other local user account can read it with
  `curl` and then drive the API — project list, agent names, worktree paths.
  Loopback binding is not user isolation. This is a security issue and wants
  its own change, not a paragraph in a setup spec.
- **`/v1/health` under-reports uptime**, apparently across system sleep: it
  claimed 45m for a process `ps` put at 15h11m, and `aurium up` printed both
  numbers in one message.
