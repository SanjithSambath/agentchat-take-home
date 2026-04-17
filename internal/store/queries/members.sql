-- Queries for the `members` table. See sql-metadata-plan.md §12.

-- name: AddMember :one
-- Idempotent insert. RETURNING conversation_id is non-empty when the row was
-- newly added; empty (pgx.ErrNoRows) when the agent was already a member.
INSERT INTO members (conversation_id, agent_id, joined_at)
VALUES ($1, $2, now())
ON CONFLICT (conversation_id, agent_id) DO NOTHING
RETURNING conversation_id;

-- name: RemoveMember :exec
DELETE FROM members WHERE conversation_id = $1 AND agent_id = $2;

-- name: IsMember :one
SELECT EXISTS(
    SELECT 1 FROM members WHERE conversation_id = $1 AND agent_id = $2
);

-- name: ListMembers :many
SELECT agent_id FROM members WHERE conversation_id = $1 ORDER BY joined_at;

-- name: LockMembersForUpdate :many
-- SELECT ... FOR UPDATE. Caller must run inside a transaction to keep the
-- lock — postgres.go's RemoveMember does exactly that via pool.BeginTx.
SELECT agent_id FROM members WHERE conversation_id = $1 FOR UPDATE;

-- name: ListConversationsForAgent :many
SELECT c.id, c.s2_stream_name, c.head_seq, c.created_at
FROM members m
JOIN conversations c ON c.id = m.conversation_id
WHERE m.agent_id = $1
ORDER BY c.created_at DESC;
