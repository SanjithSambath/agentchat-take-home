-- Queries for the `cursors` table. See sql-metadata-plan.md §7 and §12.

-- name: GetDeliveryCursor :one
SELECT delivery_seq FROM cursors
WHERE agent_id = $1 AND conversation_id = $2;

-- name: GetAckCursor :one
SELECT ack_seq FROM cursors
WHERE agent_id = $1 AND conversation_id = $2;

-- name: UpsertDeliveryCursor :exec
-- Single-row delivery_seq UPSERT for disconnect/leave/shutdown flush.
-- Regression-guarded: a stale/out-of-order write is a silent no-op.
-- Does NOT touch ack_seq.
INSERT INTO cursors (agent_id, conversation_id, delivery_seq, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (agent_id, conversation_id)
DO UPDATE SET delivery_seq = EXCLUDED.delivery_seq, updated_at = EXCLUDED.updated_at
WHERE cursors.delivery_seq < EXCLUDED.delivery_seq;

-- name: AckCursor :exec
-- Synchronous write-through UPSERT for POST /conversations/:cid/ack.
-- Does NOT touch delivery_seq. Regression-guarded.
INSERT INTO cursors (agent_id, conversation_id, ack_seq, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (agent_id, conversation_id)
DO UPDATE SET ack_seq = EXCLUDED.ack_seq, updated_at = EXCLUDED.updated_at
WHERE cursors.ack_seq < EXCLUDED.ack_seq;

-- name: ListUnreadForAgent :many
-- Single indexed join powering GET /agents/me/unread.
SELECT c.id                                   AS conversation_id,
       c.head_seq                             AS head_seq,
       COALESCE(k.ack_seq, 0)::bigint         AS ack_seq,
       (c.head_seq - COALESCE(k.ack_seq, 0))::bigint AS event_delta
FROM members m
JOIN conversations c ON c.id = m.conversation_id
LEFT JOIN cursors k
       ON k.agent_id = m.agent_id
      AND k.conversation_id = m.conversation_id
WHERE m.agent_id = $1
  AND c.head_seq > COALESCE(k.ack_seq, 0)
ORDER BY c.head_seq DESC
LIMIT $2;
