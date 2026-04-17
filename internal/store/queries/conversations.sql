-- Queries for the `conversations` table. See sql-metadata-plan.md §12.

-- name: CreateConversation :exec
INSERT INTO conversations (id, s2_stream_name, head_seq, created_at)
VALUES ($1, $2, 0, now());

-- name: GetConversation :one
SELECT id, s2_stream_name, head_seq, created_at
FROM conversations
WHERE id = $1;

-- name: ConversationExists :one
SELECT EXISTS(SELECT 1 FROM conversations WHERE id = $1);

-- name: GetConversationHeadSeq :one
SELECT head_seq FROM conversations WHERE id = $1;

-- name: UpdateConversationHeadSeq :exec
-- Regression-guarded. An out-of-order write cannot rewind head_seq.
UPDATE conversations
SET head_seq = $2
WHERE id = $1 AND head_seq < $2;
