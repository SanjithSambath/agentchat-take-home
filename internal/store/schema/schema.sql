-- ============================================================
-- AgentMail Metadata Schema
-- Target: PostgreSQL 17 / Neon Serverless (us-east-1, pooled)
--
-- Source of truth: ALL_DESIGN_IMPLEMENTATION/sql-metadata-plan.md
--
-- Every CREATE is idempotent (IF NOT EXISTS) so startup migration is safe
-- to run repeatedly. See internal/store/postgres.go (Phase 1) for the
-- migration driver.
-- ============================================================

-- uuid-ossp is only used by ad-hoc admin scripts; primary keys come from
-- the Go layer as UUIDv7 to preserve insertion order.
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- ---------- agents ------------------------------------------
-- One row per registered agent. UUIDv7 primary key minted by the server.
CREATE TABLE IF NOT EXISTS agents (
    id         UUID PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------- conversations -----------------------------------
-- One row per conversation. head_seq is the server-side cached S2 tail
-- sequence; updated inline on every successful append so the unread query
-- can read (head_seq - ack_seq) without hitting S2.
CREATE TABLE IF NOT EXISTS conversations (
    id             UUID PRIMARY KEY,
    s2_stream_name TEXT NOT NULL,
    head_seq       BIGINT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_conversations_s2_stream
    ON conversations (s2_stream_name);

-- ---------- members -----------------------------------------
-- Many-to-many join between conversations and agents. Hard-deleted on
-- leave; the authoritative membership history lives in the S2 event log
-- (agent_joined / agent_left records).
CREATE TABLE IF NOT EXISTS members (
    conversation_id UUID NOT NULL REFERENCES conversations (id),
    agent_id        UUID NOT NULL REFERENCES agents (id),
    joined_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_id, agent_id)
);

CREATE INDEX IF NOT EXISTS idx_members_agent_id
    ON members (agent_id);

-- ---------- cursors -----------------------------------------
-- Two cursors per (agent, conversation):
--   delivery_seq — server-advanced. Two-tier: in-memory hot tier updated
--                  on every SSE delivery, batched flush to this table
--                  every CURSOR_FLUSH_INTERVAL_S seconds via unnest()
--                  upsert. Powers tail resume after reconnect.
--   ack_seq      — client-advanced only via POST /conversations/:cid/ack.
--                  Synchronous write-through; no hot tier. Powers
--                  GET /agents/me/unread.
-- Both survive leave so re-invite resumes cleanly.
--
-- No foreign keys — FK checks add overhead on the hot path. Agent and
-- conversation existence is validated by the API layer before any
-- cursor write.
CREATE TABLE IF NOT EXISTS cursors (
    agent_id        UUID   NOT NULL,
    conversation_id UUID   NOT NULL,
    delivery_seq    BIGINT NOT NULL DEFAULT 0,
    ack_seq         BIGINT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, conversation_id),
    CHECK (delivery_seq >= 0),
    CHECK (ack_seq >= 0)
);

-- ---------- in_progress_messages ----------------------------
-- Atomic in-flight claim. The UNIQUE (conversation_id, message_id) row
-- is the idempotency gate — a second streaming write with the same
-- message_id fails the INSERT and the handler replays the prior outcome
-- from messages_dedup (or rejects with 409 already_processed).
--
-- Populated on message_start; deleted on message_end/abort. The recovery
-- sweep at startup drains leftover rows.
CREATE TABLE IF NOT EXISTS in_progress_messages (
    message_id      UUID NOT NULL,
    conversation_id UUID NOT NULL,
    agent_id        UUID NOT NULL,
    s2_stream_name  TEXT NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id),
    UNIQUE (conversation_id, message_id)
);

-- ---------- messages_dedup ----------------------------------
-- Terminal outcome per (conversation, client-supplied message_id):
--   'complete' — message_end appended; start_seq/end_seq let retries
--                return the cached result with already_processed=true.
--   'aborted'  — message_abort was written (client disconnect, leave
--                mid-stream, slow_writer timeout, or recovery sweep).
--                Retries with the same message_id get 409 already_aborted
--                so the client generates a fresh id.
CREATE TABLE IF NOT EXISTS messages_dedup (
    conversation_id UUID NOT NULL,
    message_id      UUID NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('complete', 'aborted')),
    start_seq       BIGINT,
    end_seq         BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_id, message_id)
);
