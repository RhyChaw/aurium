-- Worktree leases.
--
-- The daemon was meant to be the only writer (D17), which is why StackBox's
-- flock was dropped. That is not yet true: the CLI still opens the same
-- database in-process and runs git directly in worktrees, while auriumd's
-- watcher does the same. Two processes rebasing one worktree concurrently
-- corrupts it in ways git cannot recover from.
--
-- A lease is taken by BOTH processes before any git operation that mutates a
-- worktree. It is advisory in the sense that nothing physically prevents a
-- third program from ignoring it, but every Aurium code path goes through it.
--
-- expires_at exists because a holder can die. A lease with no expiry would
-- wedge a container permanently after one crash, which is worse than the race
-- it prevents.
CREATE TABLE leases (
    container_id TEXT PRIMARY KEY REFERENCES containers(id) ON DELETE CASCADE,
    holder       TEXT NOT NULL,
    operation    TEXT NOT NULL,
    acquired_at  TEXT NOT NULL,
    expires_at   TEXT NOT NULL
);

CREATE INDEX leases_expiry ON leases(expires_at);
