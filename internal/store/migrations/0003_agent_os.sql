-- Dashboard v1: projects that span repositories, provider accounts, usage.
--
-- Every statement here is additive. A database written by an older build keeps
-- working; the new columns default to the values that describe what those rows
-- already are (a project with no descriptor is standalone, an agent with no
-- provider account predates accounts existing).

-- The path of this project's aurium.project.yaml (§D22). Empty means the
-- project is a single repository initialised by `aurium init` and has no
-- descriptor — the standalone case, which stays first-class. Storing it as a
-- column rather than probing the filesystem means "is this a multi-repo
-- project" is answerable without touching disk, including for a project whose
-- root is currently unmounted.
ALTER TABLE projects ADD COLUMN descriptor TEXT NOT NULL DEFAULT '';

-- A connected provider account.
--
-- secret_ref is a keyring reference and never a credential, for exactly the
-- reason integrations.secret_ref is (§10.6, D17). source records how the
-- account was connected, because "pasted" and "this host was already logged in"
-- are different trust stories and the difference matters when one stops working.
CREATE TABLE provider_accounts (
    id         TEXT PRIMARY KEY,
    provider   TEXT NOT NULL CHECK (provider IN ('anthropic','openai')),
    label      TEXT NOT NULL,
    auth_kind  TEXT NOT NULL CHECK (auth_kind IN ('api_key','subscription')),
    -- How the adapter receives it: ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN,
    -- OPENAI_API_KEY.
    env_var    TEXT NOT NULL,
    secret_ref TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL CHECK (source IN ('pasted','host_env','cli_login')),
    status     TEXT NOT NULL CHECK (status IN ('connected','error')),
    last_error TEXT,
    created_at TEXT NOT NULL,
    UNIQUE (provider, label)
);

-- Which credential this agent burns (§D24). Nullable: the `shell` adapter has
-- no provider, and an agent started before any account was connected has none
-- either.
ALTER TABLE agents ADD COLUMN provider_account_id TEXT REFERENCES provider_accounts(id);

-- A human-readable name for the rail. Defaults to empty and the UI falls back
-- to the adapter name, so nothing has to be backfilled.
ALTER TABLE agents ADD COLUMN display_name TEXT NOT NULL DEFAULT '';

-- One metered call.
--
-- priced separates "this model has no price in the table" from "this call cost
-- nothing". Folding the first into $0.00 would make the Usage tab quietly
-- under-report spend, which is the one thing a cost view must never do.
CREATE TABLE usage_events (
    id                  TEXT PRIMARY KEY,
    ts                  TEXT NOT NULL,
    project_id          TEXT,
    container_id        TEXT,
    agent_id            TEXT,
    provider            TEXT NOT NULL,
    provider_account_id TEXT,
    model               TEXT NOT NULL DEFAULT '',
    kind                TEXT NOT NULL CHECK (kind IN ('exec','delegation','manual')),
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    cost_usd            REAL NOT NULL DEFAULT 0,
    priced              INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX usage_ts       ON usage_events(ts);
CREATE INDEX usage_project  ON usage_events(project_id, ts);
CREATE INDEX usage_agent    ON usage_events(agent_id, ts);
