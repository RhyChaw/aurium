# Aurium dashboard v1 — the agent OS surface

**Date:** 2026-09-14
**Status:** accepted, implemented in the same change
**Extends:** `Aurium_ERD_v0.2.md` §11.3 (dashboard v1), §9.1 (base-URL provider
proxy, deferred), §12.2 (`aurium.yaml`), and closes four rows of
`docs/GAPS.md`.

## The problem

Dashboard v0 answers "what containers exist and what is the git forest doing".
That is a *runtime* view. What the product actually is — the README says it in
the first line — is an operating environment for parallel coding agents, and
an operating environment is judged on a different question: **where are my
agents, which one needs me, and what is all of this costing?**

Four things are missing before that question can be answered at all:

1. **A project is a single repository.** `projects.root` is a repo root and
   `app.Project()` resolves a project by that root. Real work spans an API
   repo, a web repo and an infra repo; today that is three unrelated projects
   with three unrelated forests.
2. **Nothing creates a project from the UI.** `aurium init` in a terminal is
   the only door in.
3. **Agents are a footnote.** They appear as a chip on a container row. The
   unit a human thinks in — "the agent working on OAuth" — has no place of its
   own, no status colour, no transcript, and no way to be spoken to.
4. **There is no account or cost model at all.** Which credential an agent
   burns, whether it is a subscription or an API key, and how many tokens the
   fleet spent this morning are not recorded anywhere.

## Shape of the answer

Three surfaces, one daemon, no new build step.

```
┌ Home ──────────────────────────────────────────────────────────┐
│  Projects (cards)      + New project      Standalone repos     │
└────────────────────────────────────────────────────────────────┘
        │ click a project
        ▼
┌ Workspace ─────────────────────────────────────────────────────┐
│ ┌ rail ────────┐ ┌ heartbeat ───────────┐ ┌ chat ────────────┐ │
│ │ ▭ project A  │ │                      │ │ agent a_01J…     │ │
│ │  ▭▭▭ agents  │ │   live pulse of the  │ │ ─────────────    │ │
│ │ ▭ project B  │ │   whole fleet + the  │ │ transcript of    │ │
│ │  ▭▭  agents  │ │   vitals that matter │ │ IPC + events     │ │
│ │ ▭ project C  │ │                      │ │ ─────────────    │ │
│ │  ▭   agents  │ │                      │ │ > say something  │ │
│ └──────────────┘ └──────────────────────┘ └──────────────────┘ │
└────────────────────────────────────────────────────────────────┘

  Tabs: Workspace · Approvals · Usage · Providers · Events
```

The rail carries **every** project at once, not just the open one — the whole
point is peripheral vision. Each project is a container of thin agent tiles
(5:1, stacked); clicking the project header opens that project; clicking a
tile opens that agent in the chat pane.

## Decisions

Continuing the ERD's numbering.

**D22 — A project is a set of repositories described by a descriptor file.**
`aurium.project.yaml` at the project root lists the repos that belong to it.
The per-repo `aurium.yaml` keeps its present job (sandbox, hooks, grants) and
is untouched. A project may still be a single repo with no descriptor at all —
that is the *standalone* case and it stays a first-class citizen, because
requiring a descriptor to try the product out would be a tax on the first five
minutes.

**D23 — A repository, not a root path, resolves a project.** `app.Project()`
today does `ProjectByRoot(dirContainingAuriumYaml)`. It becomes
`RepositoryByPathAny(root)` → `project_id`. One line of intent, and multi-repo
projects follow from it: a repo anywhere on disk can belong to a project whose
root is somewhere else entirely.

**D24 — Provider accounts are first-class, and a credential is still never in
the database.** A `provider_accounts` row records *which* account an agent
runs on (`anthropic`/`openai`, subscription or API key) and holds a keyring
reference, exactly as `integrations.secret_ref` does. `agents.provider_account_id`
is what makes "which company, which agent" answerable.

**D25 — Subscription login is the provider CLI's own login, captured.** There
is no public OAuth client for a Claude or ChatGPT subscription that Aurium
could legitimately drive. What exists and is documented is `claude
setup-token` (prints a `CLAUDE_CODE_OAUTH_TOKEN`) and `codex login` (writes
`~/.codex/auth.json`). So "connect by subscription" means: Aurium detects an
existing host login if there is one, and otherwise shows the exact command and
takes the token it produces. Inventing an OAuth dance that does not exist would
be worse than an honest two-step.

**D26 — Usage is recorded only where a provider actually reports it.** The
`Adapter.Execute` path returns real token counts (`claude -p --output-format
json` reports them). Interactive REPL turns do not surface usage to anything
Aurium can see. So `usage_events` records what is measured and the Usage tab
says plainly, on the page, what is not counted. A dashboard that silently
under-reports spend is worse than one that shows a gap.

**D27 — Still no build step.** The dashboard grows from three files to a dozen
ES modules under `internal/api/web/`, loaded natively by `<script type=module>`
and embedded by the existing `//go:embed web`. `make build` continues to need
nothing but Go. The ERD's Vite+React line stays a deviation, recorded in the
plan doc, for the same reason it already was.

## Data model

Migration `0003_agent_os.sql`, additive only:

```sql
ALTER TABLE projects   ADD COLUMN descriptor TEXT NOT NULL DEFAULT '';
ALTER TABLE agents     ADD COLUMN provider_account_id TEXT REFERENCES provider_accounts(id);
ALTER TABLE agents     ADD COLUMN display_name TEXT NOT NULL DEFAULT '';

CREATE TABLE provider_accounts (
    id         TEXT PRIMARY KEY,              -- pa_…
    provider   TEXT NOT NULL CHECK (provider IN ('anthropic','openai')),
    label      TEXT NOT NULL,
    auth_kind  TEXT NOT NULL CHECK (auth_kind IN ('api_key','subscription')),
    env_var    TEXT NOT NULL,                 -- how the adapter receives it
    secret_ref TEXT NOT NULL DEFAULT '',      -- keyring reference, never a secret
    source     TEXT NOT NULL,                 -- pasted | host_env | cli_login
    status     TEXT NOT NULL,                 -- connected | error
    last_error TEXT,
    created_at TEXT NOT NULL,
    UNIQUE (provider, label)
);

CREATE TABLE usage_events (
    id                  TEXT PRIMARY KEY,     -- u_…
    ts                  TEXT NOT NULL,
    project_id          TEXT,
    container_id        TEXT,
    agent_id            TEXT,
    provider            TEXT NOT NULL,
    provider_account_id TEXT,
    model               TEXT NOT NULL DEFAULT '',
    kind                TEXT NOT NULL,        -- exec | delegation | manual
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    cost_usd            REAL NOT NULL DEFAULT 0,
    priced              INTEGER NOT NULL DEFAULT 0  -- 0 when the model has no price
);
```

`descriptor` is empty exactly for standalone projects, so "is this a real
multi-repo project" is a column read rather than a filesystem probe.

`priced` matters: a zero cost for an unknown model and a zero cost for a free
call are different facts, and the Usage tab shows the unpriced token count
separately rather than folding it into `$0.00`.

## The descriptor

```yaml
version: 1
project:
  name: my-product
  description: API, web and infra, worked on together.
repos:
  - path: ./api                 # relative to this file, or absolute, or ~-rooted
    base_branch: main
  - path: ./web
    base_branch: main
defaults:                       # seeds each repo's aurium.yaml when Aurium writes one
  driver: local
  agent: claude
  image: node:20-alpine
context:                        # indexed for aurium_context_query, project-wide
  - ./docs
```

Unknown keys are errors, matching `internal/config`'s existing stance and for
the same reason: a typo in a repo path that is silently ignored produces a
project that is quietly missing a third of itself.

## Status colours

The rail exists to be read from across the room, so the mapping is fixed and
small:

| agent state | colour | means |
|---|---|---|
| `starting` | **amber** | spinning up |
| `running` | **accent, pulsing** | working, nothing needed |
| `idle`, `exited` | **green** | done |
| `blocked`, `error` | **red** | it needs you |

A pending approval addressed to an agent forces red whatever the row says,
because the agent is, in fact, waiting on a human — and the stored status may
not have caught up yet.

## Heartbeat

One `<canvas>` scrolling right-to-left at ~30fps, fed by the SSE stream that is
already open. Amplitude is events-per-tick; each event type contributes a
coloured blip on its own lane (container / agent / context / gateway /
approval). Around it: agents by state, pending approvals, tokens in the last
hour, and the daemon's own pulse so a dead stream is visible rather than
merely still.

It is decorative *and* diagnostic: a flat line while four agents claim to be
running is a real signal, and it is the kind of signal a table of rows does not
give you.

## API additions

Every route below is added to `api/openapi.yaml` in the same change —
`internal/api/openapi_test.go` fails otherwise.

```
POST   /v1/projects                    create (descriptor + repos + per-repo init)
GET    /v1/projects/{p}                one project, with repositories
POST   /v1/projects/{p}/repos          attach a repo to an existing project
GET    /v1/projects/{p}/agents         the rail's data: agents + their containers
GET    /v1/agents/{a}                  agent detail
GET    /v1/agents/{a}/messages         the chat transcript (IPC ∪ events)
POST   /v1/agents/{a}/message          speak to an agent
GET    /v1/providers                   connected accounts (never a secret)
POST   /v1/providers                   connect one
DELETE /v1/providers/{account}         disconnect (and delete the keyring entry)
GET    /v1/providers/detect            what this host already has
GET    /v1/usage                       aggregates, grouped
GET    /v1/usage/series                buckets for the sparkline
GET    /v1/heartbeat                   vitals, so the pane is alive before the stream is
```

`GET /v1/providers` returns `secret_ref` and never the secret, exactly as the
integrations surface does. There is deliberately no route that turns a
reference back into a credential.

## Testing

- `internal/project`: descriptor round-trip, relative/absolute/`~` path
  resolution, unknown-key rejection, a repo listed twice.
- `internal/store`: provider CRUD, the `UNIQUE(provider,label)` collision,
  usage aggregation over a known fixture, migration applied to a v0002
  database.
- `internal/usage`: pricing lookup including the unknown-model path, bucketing
  across a window boundary.
- `internal/api`: every new route authenticated; `POST /v1/projects` creating
  two repos; a container token refused on `/v1/providers`; the OpenAPI drift
  test (already exists) covering the additions.
- The dashboard is driven headlessly against the running daemon, as the
  approvals inbox already was: create a project, see its agents, open one,
  send it a message, read the usage tab.

## What this deliberately does not do

- No terminal emulator. §11.3's reasoning stands; the chat pane sends messages
  through IPC and tmux, and `aurium attach` remains the way to take the wheel.
- No provider proxy (`ANTHROPIC_BASE_URL` → daemon). That is ERD §9.1 Phase D
  and it is the *only* way to get exact per-turn usage for interactive agents;
  it is named here as the successor to D26's gap, not built.
- No cloud accounts, teams, or billing. One user, one daemon, one machine.
