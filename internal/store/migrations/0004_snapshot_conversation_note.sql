-- host placement: a snapshot's rootfs no longer always holds the agent's
-- conversation.
--
-- Every snapshot before this migration was taken under in-container
-- placement, the only kind that existed: the transcript lived in $HOME
-- inside the captured rootfs, so it defaults to true for every existing row.
-- New snapshots taken under agent_placement: host set it to false and fill
-- in note, so a restore says what it is bringing back instead of silently
-- starting a fresh conversation.
ALTER TABLE snapshots ADD COLUMN includes_conversation INTEGER NOT NULL DEFAULT 1;
ALTER TABLE snapshots ADD COLUMN note TEXT;
