# The host driver — the agent on your machine, its commands in a container

**Date:** 2026-09-15
**Status:** accepted, not yet implemented. The one assumption it rests on was
verified by spike before writing (see *Evidence*).
**Extends:** `internal/runtime` (agent placement), `internal/agent`
(`HeadlessCommand`, already present), and `internal/gateway` (one new tool).
Changes no schema and adds no driver.

**Amended 2026-09-15, before implementation.** An earlier draft of this spec
called for "a third driver beside `docker` and `local`". That was wrong, and
writing the plan is what caught it. `Driver` is twenty-odd methods of container
lifecycle — create, snapshot, volumes, networks — and this design changes none
of them: the container stays Docker, because running commands in a reproducible
environment is exactly what it is for. A new driver would have meant
reimplementing all of it to change one thing.

The real seam is one call. A headless turn runs in the container solely because
`converse.go` does `drv.Exec(runCtx, c.RuntimeID, cmd, runOpts)`, and an
interactive one because `manager.go` builds an `agent.Session` around the
driver. **Where the agent process runs is an axis orthogonal to which driver
owns the container**, and naming it that way makes the change small: placement
is a setting, not a subsystem.

## The problem

Aurium caps out at about three agents, and the reason is structural rather than
incidental. Each agent is a `claude` process living *inside* its container, so
the container must be sized for a working agent — the shipped default is
`{ cpus: 2, memory: 2g }`. Docker Desktop's VM on a 16 GB Mac holds 7.7 GB.
Three agents and the machine is full.

Meanwhile a Claude Code session on the host measures **452 MB** of RSS, because
the model runs on Anthropic's servers and the local process is mostly waiting on
a socket. Ten of those is 4.5 GB of ordinary host memory. Ten containers is
20 GB into a 7.7 GB VM, which is not a tuning problem.

The product is "every coding agent you have, on one screen". At three, the
screen is not the constraint.

## The keystone

This design is far smaller than it sounds, because of something already true:
`createArgs` bind-mounts the worktree with **identical paths on both sides**
(D3). The worktree already lives on the host at, say,
`/Users/you/proj/.aurium/wt/agent-a`, and is visible inside the container at
that same absolute path.

So a host process and a container process refer to the same file by the same
string. No path translation, no filesystem proxy, no synchronisation. The only
thing that has to move is the agent process.

`Bind`'s own comment states the intent: Aurium mounts at the identical path
"so that absolute paths in build output, editor state, language servers and
error messages mean the same thing inside and outside the container". That was
written for build output and language servers. It happens to be exactly the
property a host-side agent needs, which is why this design costs so little.

## Shape

Placement has two values. `in-container` is today's behaviour and stays the
default. `host` runs the agent process on the machine, with its commands routed
back into the container it already has.

| | today (`in-container`) | `host` placement |
|---|---|---|
| Worktree | host, bind-mounted | unchanged |
| Agent process | `claude` in container tmux | `claude -p` on the host, ~450 MB |
| `Read`/`Edit`/`Write` | container filesystem | host filesystem, native |
| `Bash` | inside the container, **invisible to Aurium** | `aurium_exec` → `docker exec`, logged and gated |
| Conversation | container `$HOME`, opaque to Aurium | `~/.claude`, readable |
| Idle container | holds 2 GB | stopped; costs disk, not memory |
| Credentials | `ANTHROPIC_API_KEY` copied into the container | the host's own Claude Code login |

The container does not disappear. It stops holding the agent and becomes what it
is actually good at: a reproducible environment to run commands in.

## Three moving parts

**1. Launch.** The driver starts `claude -p <prompt> --output-format json` as a
host process with `cwd` set to the worktree, `--continue` carrying the
conversation between turns. Each turn returns structured JSON — content, tool
calls, token counts, cost — which lands in the IPC history the dashboard already
renders. No dashboard change is needed to see it, and `UsageMeter` gets the
provider's own figures rather than an estimate.

**2. The wall.** Composed as argv, not as configuration:
`--disallowedTools Bash` removes the built-in shell,
`--mcp-config` points at the daemon, and `--allowedTools mcp__aurium__exec`
admits one new gateway tool that runs `docker exec` into that agent's own
container. `Prepare()` already writes an instruction file; it gains one
paragraph saying commands run in the container, not on the host.

The `aurium-mcp` stdio→HTTP shim is not needed on this path. It exists to reach
the daemon from inside a container; a host process calls `/mcp` directly.

**3. The gain in visibility.** Today the window shows IPC message history —
`agentMessages` reads `IPC.History`, not the agent's conversation. The
conversation is opaque to Aurium; `docker commit` merely carries it along so
`--continue` can resume. On the host it arrives as JSON per turn, so what the
window shows stops being a proxy for what the agent did.

## Evidence

The design rests on one assumption — that an agent denied `Bash` will use an MCP
exec tool instead, rather than degrading. That was spiked before this spec was
written, with a throwaway project, a fifty-line stdio MCP server and a real
`claude -p` run.

- `--disallowedTools Bash` works: the agent reports no Bash tool available.
- With no alternative offered, it **degrades gracefully** — it read the file and
  answered correctly rather than failing.
- With the exec tool offered, it used it immediately and without coaching:
  `wc -l < main.go`, logged by the stub, zero permission denials.
- On a three-part task it issued two compound commands
  (`cd … && go version; echo ---; go build …; echo exit=$?`) entirely through
  the tool, with no attempt to reach `Bash`, across ten turns.
- Cost was $0.12–0.18 per small task at four to six turns.

The spike also surfaced something not anticipated. Because the stub ran commands
in a bare shell, the agent found `go: command not found` while reading host files
perfectly well — and **diagnosed that accurately**, searching the usual install
locations before concluding, rather than thrashing. That split is the intended
property (files shared, toolchain not), but an agent that is not told about it
will spend turns investigating a machine it thinks is broken. Hence the
paragraph in the instruction file: it is cheap, and its absence is expensive.

## What this costs

**Snapshots lose conversation resume.** `--continue` works today because the
transcript sits in `$HOME` inside the rootfs that `docker commit` captures. On
the host it is in `~/.claude`, outside it. The `local` driver already reports
`Snapshot: false` rather than faking it, and the host driver must be as honest:
either the snapshot engine learns to capture the host transcript directory, or
the driver's capabilities say conversation resume is not available. It must not
report a capability it half has.

**The wall is cooperative, not kernel-enforced.** `--disallowedTools` is the
agent's own runtime honouring a flag, not a namespace refusing a syscall. For
your own repository on your own laptop that is a reasonable trade, and it is
strictly more visibility than today, where `Bash` inside a container was
invisible to Aurium as well. It must never be described as a sandbox.

**A second placement is a second path to keep correct**, and the two must not
drift on anything a user can observe — token counts, transcript rows, error
text. The mitigation is that both paths produce the same `ExecResult`-shaped
outcome and are tested against the same assertions, so a divergence fails a
test rather than surprising someone.

## Interfaces

- No new driver. `sandbox.agent_placement` in `aurium.yaml`, one of
  `in-container` (default) or `host`.
- `converse.go` chooses between `drv.Exec` and a host process for the turn.
  `manager.go` starts no tmux session under `host` placement, which the
  existing `Caps.Tmux` gate already expresses for a different reason.
- One gateway tool, `aurium_exec`, which runs a command in the agent's container
  and returns combined output with an exit status. It is subject to the existing
  risk classification, so a destructive verb in a command is held for approval
  exactly as an integration call is — which `Bash` in a container never was.
- `driver: host` selectable per project in `aurium.yaml`, beside `docker` and
  `local`.

## Error handling

A `claude` binary missing from the host fails at agent start with that fact,
not at first turn with a confusing empty result — the same rule the preflight
table applies everywhere else. A container that is stopped when `aurium_exec` is
called is started on demand and left running for the idle window. If the host
transcript is missing when `--continue` is requested, the turn starts a fresh
conversation and says so in the transcript, rather than silently losing history
the user believes is there.

## Testing

- The driver against a fake command runner: turn JSON parsed into IPC messages,
  cost extracted, a non-zero exit surfaced, a missing `claude` reported at start.
- `aurium_exec` routed through the existing risk classifier: `rm -rf` held for
  approval, `ls` not.
- An end-to-end test on a real repository with `driver: host`: one agent, one
  turn, asserting the worktree changed and the transcript recorded the tool call.
- A capability test asserting the host driver reports `Snapshot: false` while it
  is false, so the honesty is enforced rather than remembered.

## Out of scope

Interactive `tmux` sessions on the host, and therefore `aurium attach` for host
agents. Headless turns first, and a second drive path only once one is proven —
but note this is not merely sequencing. `Caps.Tmux` already carries the
judgement: drivers without it "can still run headless agents through Exec; they
simply have nothing to attach to, and Aurium must not try to spawn a REPL on the
user's own machine". Reviving host tmux later means overturning that, not just
implementing it.
Kernel-level sandboxing of the host process. Capturing the host transcript into
snapshots. Adapters other than `claude`; `codex` gets the same treatment once
this one works.
