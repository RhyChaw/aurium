-- Aurium data model. Mirrors Aurium_ERD_v0.2.md §7.
--
-- All ids are ULIDs with a type prefix (p_, t_, c_, s_, a_, m_, i_, e_).
-- Timestamps are RFC 3339 UTC text. JSON columns are validated by the daemon,
-- not by SQLite.

CREATE TABLE projects (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    root       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE repositories (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    path        TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    remote      TEXT
);

CREATE TABLE tasks (
    id             TEXT PRIMARY KEY,
    project_id     TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title          TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN
                     ('created','planning','running','blocked','review','pr_ready','merged','completed','archived')),
    parent_task_id TEXT REFERENCES tasks(id),
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE TABLE containers (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id             TEXT REFERENCES tasks(id),
    repo_id             TEXT NOT NULL REFERENCES repositories(id),
    branch              TEXT NOT NULL,
    slug                TEXT NOT NULL,
    parent_container_id TEXT REFERENCES containers(id),
    -- git parent (a branch name); may be the repository base_branch
    parent_branch       TEXT NOT NULL,
    origin_snapshot_id  TEXT,
    origin_kind         TEXT CHECK (origin_kind IN ('fresh','fork','stack','restore')),
    -- the recorded base: the parent commit this container was last rebased onto (D6)
    base_sha            TEXT NOT NULL,
    pending_base_sha    TEXT,
    head_sha            TEXT,
    driver              TEXT NOT NULL,
    runtime_id          TEXT,
    image               TEXT,
    worktree            TEXT NOT NULL,
    ports_json          TEXT,
    network             TEXT,
    status              TEXT NOT NULL CHECK (status IN
                          ('creating','queued','running','paused','stopped','stale','conflict','drifted','error','archived')),
    last_error          TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    UNIQUE (repo_id, branch)
);

CREATE TABLE snapshots (
    id              TEXT PRIMARY KEY,
    container_id    TEXT NOT NULL REFERENCES containers(id) ON DELETE CASCADE,
    seq             INTEGER NOT NULL,
    label           TEXT,
    trigger         TEXT NOT NULL,
    head_sha        TEXT NOT NULL,
    tree_ref        TEXT NOT NULL,
    base_sha        TEXT NOT NULL,
    image_ref       TEXT NOT NULL,
    manifest_path   TEXT NOT NULL,
    context_version INTEGER NOT NULL,
    bytes           INTEGER,
    created_at      TEXT NOT NULL,
    UNIQUE (container_id, seq)
);

CREATE TABLE agents (
    id               TEXT PRIMARY KEY,
    container_id     TEXT NOT NULL REFERENCES containers(id) ON DELETE CASCADE,
    adapter          TEXT NOT NULL,
    role             TEXT NOT NULL CHECK (role IN ('primary','master','worker')),
    parent_agent_id  TEXT REFERENCES agents(id),
    tmux_session     TEXT NOT NULL,
    model            TEXT,
    status           TEXT NOT NULL CHECK (status IN ('starting','running','idle','blocked','exited','error')),
    started_at       TEXT,
    last_activity_at TEXT
);

CREATE TABLE tokens (
    id           TEXT PRIMARY KEY,
    hash         TEXT NOT NULL UNIQUE,
    container_id TEXT REFERENCES containers(id) ON DELETE CASCADE,
    agent_id     TEXT REFERENCES agents(id),
    scopes       TEXT NOT NULL,
    expires_at   TEXT,
    revoked      INTEGER NOT NULL DEFAULT 0
);

-- ---------------------------------------------------------------- context §8

CREATE TABLE context_items (
    id         TEXT PRIMARY KEY,
    scope      TEXT NOT NULL CHECK (scope IN ('project','container','agent')),
    scope_id   TEXT NOT NULL,
    key        TEXT NOT NULL,
    version    INTEGER NOT NULL DEFAULT 1,
    content    TEXT NOT NULL,
    mime       TEXT NOT NULL DEFAULT 'text/markdown',
    updated_by TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (scope, scope_id, key)
);

CREATE TABLE context_versions (
    item_id    TEXT NOT NULL REFERENCES context_items(id) ON DELETE CASCADE,
    version    INTEGER NOT NULL,
    content    TEXT NOT NULL,
    author     TEXT NOT NULL,
    reason     TEXT,
    created_at TEXT NOT NULL,
    PRIMARY KEY (item_id, version)
);

CREATE TABLE context_grants (
    id           TEXT PRIMARY KEY,
    scope        TEXT NOT NULL,
    scope_id     TEXT NOT NULL,
    key_glob     TEXT NOT NULL,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('project','container','agent','role')),
    subject_id   TEXT NOT NULL,
    perm         TEXT NOT NULL CHECK (perm IN ('read','append','propose','write'))
);

CREATE TABLE context_proposals (
    id           TEXT PRIMARY KEY,
    item_id      TEXT NOT NULL REFERENCES context_items(id) ON DELETE CASCADE,
    base_version INTEGER NOT NULL,
    content      TEXT NOT NULL,
    author       TEXT NOT NULL,
    reason       TEXT,
    status       TEXT NOT NULL CHECK (status IN ('open','accepted','rejected','stale')),
    decided_by   TEXT,
    created_at   TEXT NOT NULL,
    decided_at   TEXT
);

CREATE TABLE context_docs (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    source     TEXT NOT NULL,
    path       TEXT NOT NULL,
    heading    TEXT,
    content    TEXT NOT NULL,
    hash       TEXT NOT NULL,
    indexed_at TEXT NOT NULL
);

CREATE VIRTUAL TABLE context_fts USING fts5(
    key, content,
    content='context_items', content_rowid='rowid',
    tokenize='porter unicode61'
);

CREATE VIRTUAL TABLE docs_fts USING fts5(
    path, heading, content,
    content='context_docs', content_rowid='rowid',
    tokenize='porter unicode61'
);

-- External-content FTS5 tables do not populate themselves. The ERD declares
-- the virtual tables but not these triggers; without them every
-- aurium_context_query would return zero rows.
CREATE TRIGGER context_items_ai AFTER INSERT ON context_items BEGIN
    INSERT INTO context_fts(rowid, key, content) VALUES (new.rowid, new.key, new.content);
END;
CREATE TRIGGER context_items_ad AFTER DELETE ON context_items BEGIN
    INSERT INTO context_fts(context_fts, rowid, key, content) VALUES ('delete', old.rowid, old.key, old.content);
END;
CREATE TRIGGER context_items_au AFTER UPDATE ON context_items BEGIN
    INSERT INTO context_fts(context_fts, rowid, key, content) VALUES ('delete', old.rowid, old.key, old.content);
    INSERT INTO context_fts(rowid, key, content) VALUES (new.rowid, new.key, new.content);
END;

CREATE TRIGGER context_docs_ai AFTER INSERT ON context_docs BEGIN
    INSERT INTO docs_fts(rowid, path, heading, content) VALUES (new.rowid, new.path, new.heading, new.content);
END;
CREATE TRIGGER context_docs_ad AFTER DELETE ON context_docs BEGIN
    INSERT INTO docs_fts(docs_fts, rowid, path, heading, content) VALUES ('delete', old.rowid, old.path, old.heading, old.content);
END;
CREATE TRIGGER context_docs_au AFTER UPDATE ON context_docs BEGIN
    INSERT INTO docs_fts(docs_fts, rowid, path, heading, content) VALUES ('delete', old.rowid, old.path, old.heading, old.content);
    INSERT INTO docs_fts(rowid, path, heading, content) VALUES (new.rowid, new.path, new.heading, new.content);
END;

-- -------------------------------------------------------------------- ipc §9.5

CREATE TABLE messages (
    id                TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    from_agent_id     TEXT,
    from_container_id TEXT,
    to_agent_id       TEXT,
    to_container_id   TEXT,
    to_human          INTEGER NOT NULL DEFAULT 0,
    type              TEXT NOT NULL CHECK (type IN
                        ('REQUEST','RESPONSE','INFORMATION','WARNING','BLOCKED','APPROVAL_REQUIRED','ARTIFACT','DEPENDENCY','CONFLICT')),
    priority          TEXT NOT NULL DEFAULT 'normal' CHECK (priority IN ('normal','high')),
    content           TEXT NOT NULL,
    refs_json         TEXT,
    in_reply_to       TEXT REFERENCES messages(id),
    status            TEXT NOT NULL CHECK (status IN ('queued','delivered','acked')),
    created_at        TEXT NOT NULL,
    delivered_at      TEXT,
    nudged_at         TEXT,
    acked_at          TEXT
);

CREATE TABLE dependencies (
    from_agent_id TEXT NOT NULL,
    to_agent_id   TEXT NOT NULL,
    subject       TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (from_agent_id, to_agent_id, subject)
);

-- ----------------------------------------------------------- integrations §10

CREATE TABLE integrations (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('mcp_stdio','mcp_http','native')),
    name        TEXT NOT NULL,
    config_json TEXT NOT NULL,
    -- a keyring reference, never a credential (§10.6)
    secret_ref  TEXT,
    status      TEXT NOT NULL,
    last_error  TEXT,
    UNIQUE (project_id, name)
);

CREATE TABLE capabilities (
    integration_id    TEXT NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    description       TEXT,
    input_schema_json TEXT,
    risk              TEXT NOT NULL CHECK (risk IN ('low','medium','high')),
    removed           INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (integration_id, name)
);

CREATE TABLE grants (
    id              TEXT PRIMARY KEY,
    integration_id  TEXT NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
    capability_glob TEXT NOT NULL,
    subject_type    TEXT NOT NULL CHECK (subject_type IN ('project','container','agent','role')),
    subject_id      TEXT NOT NULL,
    mode            TEXT NOT NULL CHECK (mode IN ('allow','deny','approve')),
    created_by      TEXT NOT NULL,
    created_at      TEXT NOT NULL
);

CREATE TABLE approvals (
    id             TEXT PRIMARY KEY,
    container_id   TEXT,
    agent_id       TEXT,
    integration_id TEXT,
    capability     TEXT NOT NULL,
    args_json      TEXT NOT NULL,
    reason         TEXT,
    status         TEXT NOT NULL CHECK (status IN ('pending','approved','rejected','expired')),
    decided_by     TEXT,
    created_at     TEXT NOT NULL,
    decided_at     TEXT,
    expires_at     TEXT NOT NULL
);

CREATE TABLE artifacts (
    id           TEXT PRIMARY KEY,
    container_id TEXT REFERENCES containers(id) ON DELETE CASCADE,
    task_id      TEXT,
    kind         TEXT NOT NULL CHECK (kind IN ('pr','file','report','snapshot','url')),
    ref          TEXT NOT NULL,
    meta_json    TEXT,
    created_at   TEXT NOT NULL
);

-- ----------------------------------------------------------------- events §11

CREATE TABLE events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts           TEXT NOT NULL,
    project_id   TEXT,
    container_id TEXT,
    agent_id     TEXT,
    task_id      TEXT,
    type         TEXT NOT NULL,
    actor        TEXT NOT NULL,
    payload_json TEXT NOT NULL
);

CREATE INDEX events_ts        ON events(ts);
CREATE INDEX events_container ON events(container_id, id);
CREATE INDEX events_type      ON events(type, id);

CREATE INDEX containers_project ON containers(project_id);
CREATE INDEX containers_parent  ON containers(parent_container_id);
CREATE INDEX snapshots_container ON snapshots(container_id, seq);
CREATE INDEX agents_container   ON agents(container_id);
CREATE INDEX messages_inbox     ON messages(to_agent_id, status, created_at);
CREATE INDEX messages_container_inbox ON messages(to_container_id, status, created_at);
CREATE INDEX context_items_scope ON context_items(scope, scope_id);
