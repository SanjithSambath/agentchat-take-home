-- Queries for the `in_progress_messages` table. See sql-metadata-plan.md §12.

-- name: ClaimInProgressMessage :execrows
-- Atomic in-flight claim. Returns affected row count: 1 means this handler now
-- owns (conversation_id, message_id); 0 means a concurrent writer or a
-- crashed prior attempt already claimed it.
INSERT INTO in_progress_messages (message_id, conversation_id, agent_id, s2_stream_name, started_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (conversation_id, message_id) DO NOTHING;

-- name: DeleteInProgressMessage :exec
DELETE FROM in_progress_messages WHERE message_id = $1;

-- name: ListInProgressMessages :many
-- Drained by the recovery sweep on server startup.
SELECT message_id, conversation_id, agent_id, s2_stream_name, started_at
FROM in_progress_messages;
