# The loop supervisor — a fleet that iterates until a gate passes

**Date:** 2026-09-15
**Status:** accepted, not yet implemented
**Extends:** `internal/runtime/delegate.go` (§9.4 delegation), `internal/gateway`
(approvals, `aurium_merge`), `internal/events` (`agent.exited`), and the
dashboard rail. Depends on nothing that does not already exist except the
supervisor itself.

## The problem

Aurium can already fan out. `aurium_delegate` in fork mode gives a worker its
own container, worktree and branch and returns immediately, so a master agent
that calls it three times has three agents working in parallel. What Aurium
cannot do is *finish*.

Four things are missing, and they are the difference between a fan-out and a
loop:

1. **Nothing re-wakes the master.** `agent.exited` is emitted on every agent
   that stops, and no subscriber consumes it. A master that delegates three
   workers has no reason to run again, so the fan-out is a one-shot: work
   happens, and then a human has to notice and decide what next.
2. **Nothing defines done.** There is no criterion anywhere in the system for
   whether delegated work achieved anything. The only judge available today is
   a person reading branches.
3. **Nothing bounds the spend.** `Delegation` meters headless runs through
   `UsageMeter`, so the numbers exist, but nothing consults them to stop. An
   agent loop with no cap is an open line to a metered API.
4. **Nothing rations capacity.** `internal/scheduler` does not exist; the
   README lists it as out of scope. Measured on a 16 GB Mac with the shipped
   `resources: { cpus: 2, memory: 2g }`, Docker Desktop's VM holds **about
   three** agents. There is no admission control, so agent four does not queue
   — it fails wherever it happens to fail.

The fourth is the one that turns this from a feature into a constraint. A
"fleet" on this hardware is two or three agents at a time. The design has to
make that a queue rather than a crash.

## Shape

A **Loop** is a persisted goal plus a gate command, driven by a supervisor
inside the daemon until the gate passes or a budget is exhausted.

```
aurium loop start --goal "make the rail render in under 8ms" \
                  --until "go test ./internal/api/..." \
                  --workers 2 --max-rounds 5 --max-spend 15
```

**Control flow is Go; judgement is an agent.** The supervisor cannot decompose
a goal — it is a state machine, and splitting "make the rail render in under
8ms" into subtasks is exactly what a language model is for. So a loop owns one
long-lived **master agent** that does the thinking while the supervisor does the
driving. The master is the agent that survives every round, and it is the one
the dashboard marks as senior.

One round:

```
supervisor asks the master agent for N subtasks
  → spawns N workers (Delegate, fork mode: own container, own branch)
  → waits for every worker's agent.exited
  → merges every worker branch that has commits on it
  → runs the gate on the merged result
      pass  → done, notify
      fail  → hands the gate's output and the workers' reports back to the
              master, which produces round R+1's subtasks
```

The division is deliberate. Everything that must be reliable — waiting,
counting, capping, queueing, stopping — is Go a test can drive. Everything that
must be intelligent — what to try, what a failure means, what to try next — is
the master agent. Neither can do the other's job, and putting the budget in the
half that wants to keep spending is the mistake this avoids.

## Why the daemon owns the loop

The alternative — the master agent loops by itself using `aurium_delegate` and
its IPC inbox — needs almost no new code, and is wrong for three reasons. The
master must stay alive burning tokens to orchestrate. The loop dies when it
dies. And the budget becomes the honour system, enforced by the one component
that wants to keep going.

In the daemon the loop survives any agent dying, costs nothing to orchestrate,
and is ordinary Go that can be unit-tested against a fake clock and a fake
runtime. It is also the only place a queue can live, and the capacity ceiling
makes a queue mandatory.

`MaxDepth` stays at its default of 1: workers still cannot delegate. The
supervisor is the only fan-out point, so the tree is one level deep by
construction and no prompt can fork-bomb the machine.

## The gate

The gate is a command the **user** supplies, and it runs **inside a container**
on a scratch worktree holding the merged result — never on the host. A gate
running on the host would be arbitrary code execution assembled from agent
output, which is the exact boundary the approval system exists to hold.

Success is the command's exit status. Nothing else. An agent cannot report
success; it can only produce a tree that makes the gate pass. This is the same
instinct as `TestOpenAPIMatchesRegisteredRoutes`: a claim that nothing checks is
a claim that goes stale, and here the claim is "the work is done".

## Budget and termination

Every loop carries three caps, all mandatory, all defaulted, first to trip wins:

| cap | default | why |
|---|---|---|
| `max_rounds` | 5 | a loop that has not converged in five rounds is not converging |
| `max_spend` | $10 | read from `UsageMeter`, the only cap denominated in the thing that actually hurts |
| `max_wall` | 2h | catches a wedged worker that never exits and so never trips the other two |

Terminal states are `passed`, `exhausted`, `failed` and `cancelled`. Every one
notifies. **Branches are left in place on exhaustion**: three rounds of work is
the most valuable thing an exhausted loop produced, and deleting it to report a
tidy failure would throw away the only reason to read the result.

## Capacity, honestly

The supervisor queues workers against `max_concurrent`, defaulting to 2. Asking
for `--workers 10` on a 16 GB Mac queues eight of them; the loop still works and
is simply slower. This is the admission control the system has never had, and it
arrives scoped to loops rather than as a general scheduler — a smaller claim,
and one that can actually be finished.

Raising the real ceiling is a different change: `idle_pause_minutes` is
advertised in every generated `aurium.yaml` and implemented nowhere
(issue #9). Wired with `Stop` rather than `Pause`, so memory is reclaimed rather
than merely frozen, it is what would make a large fleet with a small working set
possible. This design does not depend on it and does not deliver it.

## The supervisor is visibly senior

`views/rail.js` already renders a role chip for any agent that is not `primary`,
so `master` and `worker` are distinguishable today — as five points of grey
lowercase text, which is not a hierarchy anyone reads at a glance.

The supervising agent gets a real promotion in the rail: a heavier border and a
filled accent ground rather than the flat tile, an explicit **SUPERVISOR** label
rather than the lowercase chip, and its workers nested beneath it. The
supervisor's own tile carries the round number, the gate's last result and the
budget consumed, because those are properties of the loop rather than of any
worker, and there is nowhere else they belong.

## Interfaces

- `POST /v1/projects/{project}/loops` — start; `GET /v1/loops/{loop}` — state;
  `POST /v1/loops/{loop}/stop` — cancel. All three documented in
  `api/openapi.yaml`, or `TestOpenAPIMatchesRegisteredRoutes` fails, which is
  the point of it.
- `aurium loop start | status | stop`.
- Two tables: `loops` (goal, gate, caps, state) and `loop_rounds` (round number,
  worker agent ids, gate output, spend).

## Error handling

A worker that never exits is caught by `max_wall`, not by waiting forever. Merging takes every worker branch
carrying commits, in the order the workers were spawned. A conflict fails that
round rather than the loop: the conflicting paths become part of the next
round's subtasks, which is the situation a master agent is for. A worker that
produced no commits is not an error — it is a subtask that turned out to need
nothing, and it merges trivially by being skipped. A gate command that cannot run at all — bad
command, missing binary — fails the loop immediately with the command's own
stderr, rather than being read as a failing test and burning five rounds trying
to fix code that was never broken.

## Testing

- `internal/loop` against a fake runtime and a fake clock: a gate that passes on
  round one, a gate that never passes (each cap trips in turn), a worker that
  never exits, a merge conflict, a gate command that does not exist.
- The capacity queue: `--workers 10` with `max_concurrent 2` starts exactly two
  at a time and still completes.
- An end-to-end test on a real repository with a trivially failing test and a
  one-line fix, asserting the loop reaches `passed`.

## Out of scope

Adaptive fan-out. Best-of-N racing. Cross-project loops. Workers delegating.
Resuming a loop across a daemon restart — a restart stops the loop cleanly and
says so, and resumption is a second change once the state machine has proven
itself.
