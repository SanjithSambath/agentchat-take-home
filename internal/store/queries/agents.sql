-- Queries for the `agents` table. See ALL_DESIGN_IMPLEMENTATION/sql-metadata-plan.md §12.

-- name: CreateAgent :one
INSERT INTO agents (id, created_at) VALUES ($1, now())
RETURNING id, created_at;

-- name: AgentExists :one
SELECT EXISTS(SELECT 1 FROM agents WHERE id = $1);

-- name: ListAllAgentIDs :many
-- Used once at startup to warm the agent-existence sync.Map.
SELECT id FROM agents;
