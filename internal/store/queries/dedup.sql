-- Queries for the `messages_dedup` table. See sql-metadata-plan.md §12.

-- name: GetMessageDedup :one
-- Write-path idempotency lookup. Called at the very start of both message-write
-- handlers. On hit, the handler replays the cached terminal outcome instead
-- of writing to S2 again.
SELECT status, start_seq, end_seq
FROM messages_dedup
WHERE conversation_id = $1 AND message_id = $2;

-- name: InsertMessageDedupComplete :exec
-- Called after a successful S2 append of message_start + appends + message_end.
-- ON CONFLICT DO NOTHING: first writer wins; a concurrent sweeper becomes a no-op.
INSERT INTO messages_dedup (conversation_id, message_id, status, start_seq, end_seq)
VALUES ($1, $2, 'complete', $3, $4)
ON CONFLICT (conversation_id, message_id) DO NOTHING;

-- InsertMessageDedupAborted is intentionally omitted here. start_seq is
-- nullable, and sqlc cannot express nullable positional bigint parameters
-- cleanly — the aborted insert is implemented via raw pgx in postgres.go.
