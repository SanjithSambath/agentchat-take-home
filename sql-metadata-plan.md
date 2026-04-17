# AgentMail: Metadata Storage Layer — Complete Design Plan

## Executive Summary

This document specifies the complete design for AgentMail's relational metadata layer: the storage system that manages agent identities, conversation records, membership state, and read cursors. This is everything that is NOT message content — message content lives in S2 streams (designed separately).

**Core decisions:**
- **Database:** PostgreSQL (Neon serverless, free tier)
- **Driver:** pgx v5 native interface + sqlc code generation
- **Primary keys:** UUIDv7 (time-ordered, RFC 9562)
- **Hosting:** Neon (`us-east-1`) accessed from Go server on Fly.io (`iad`)
- **Cursor strategy:** Two-tier (in-memory hot + batched Postgres warm)
- **Membership checks:** In-process LRU cache with TTL + synchronous invalidation
- **Leave locking:** `SELECT ... FOR UPDATE` (row-level, per-conversation)
- **Leave semantics:** Hard delete from members table, preserve cursor
- **No Redis.** No additional dependencies beyond Postgres.

**Scale target:** Designed for millions of agents. Scaling path to billions documented but not implemented.

---

## 1. Why PostgreSQL, Not SQLite

The original spec.md proposed SQLite. Here is why PostgreSQL is the correct choice for this system, and why SQLite fails at our target scale.

### SQLite's breaking points

| Constraint | Impact at scale |
|---|---|
| **Single-writer lock** | ALL writes serialize — cursor flushes, invites, leaves, agent registration queue behind each other. At 10K cursor writes/sec, this is a hard wall. |
| **Database-level locks, not row-level** | Two agents leaving DIFFERENT conversations block each other. PostgreSQL's `FOR UPDATE` locks only the rows in the relevant conversation. |
| **No concurrent write connections** | WAL mode allows concurrent readers, but only one writer at a time. Every write-path goroutine contends for the same lock. |
| **No `LISTEN/NOTIFY`** | No path to multi-instance cache invalidation without adding another dependency. |
| **No native UUID type** | UUIDs stored as 36-byte TEXT, not 16-byte binary. 2.25x storage overhead on every UUID column and index. |
| **No `unnest()` batch operations** | Cursor flush requires N individual INSERT statements or brittle multi-value INSERT strings. |
| **Embedded = single process** | If you need a second server instance (horizontal scaling), you need a separate database entirely. |

### What PostgreSQL provides

- **Row-level locking** (`FOR UPDATE`) — concurrent leave operations on different conversations don't block each other
- **Connection pooling** with true concurrent writers — pgxpool manages 15 connections handling 30K+ queries/sec
- **`unnest()` batch upserts** — flush 10K cursor updates in a single 5-20ms statement
- **Native UUID type** — 16-byte binary storage, 55% smaller than TEXT representation
- **`LISTEN/NOTIFY`** — multi-instance cache invalidation (scaling path)
- **Mature ecosystem** — pgx, sqlc, monitoring, backups, replicas, partitioning — all battle-tested at scale

### What we lose

- Zero-config embedded deployment. PostgreSQL is an external dependency.
- Single-binary simplicity. The Go binary now requires a database connection string.

**These costs are already paid** by the decision to deploy on Fly.io + Neon. The operational complexity is Neon's problem, not ours.

---

## 2. Hosting: Neon Serverless PostgreSQL

### Why Neon, not Fly.io Postgres

Fly.io's own documentation is titled **"This Is Not Managed Postgres."** Their unmanaged Postgres is a VM running a Postgres Docker image — you handle backups, failover, monitoring, and recovery. Their truly managed offering starts at $38/month with no free tier.

| Dimension | Neon | Fly.io Postgres (unmanaged) | Fly.io Managed Postgres |
|---|---|---|---|
| **Management** | Fully managed | Self-managed | Fully managed |
| **Cost** | Free tier (0.5 GB, 100 CU-hours/mo) | ~$0 (VM cost only) | $38/mo minimum |
| **Backups** | Automatic, point-in-time recovery | Manual | Automatic |
| **Failover** | Automatic | Manual | Automatic |
| **Monitoring** | Built-in dashboard | DIY | Built-in |
| **PostgreSQL version** | 16, 17 | Whatever you install | 16 |
| **Connection pooling** | Built-in PgBouncer | DIY | Built-in |

### Architecture

```text
┌─────────────────────┐         TCP (direct)        ┌──────────────────────┐
│  Go Server          │ ──────────────────────────→  │  Neon PostgreSQL     │
│  (Fly.io, iad)      │         ~1-5ms latency       │  (AWS us-east-1)     │
│                     │ ←──────────────────────────   │  PostgreSQL 17       │
│  pgxpool (15 conns) │                              │                      │
└─────────────────────┘                              └──────────────────────┘
```

### Connection Configuration

Two connection strings from Neon:

1. **Direct connection** (`postgresql://...neon.tech/agentmail`): Full PostgreSQL protocol. Supports prepared statements, advisory locks, session-level features. Use this for our Go server.

2. **Pooled connection** (`postgresql://...neon.tech/agentmail?pgbouncer=true`): Routes through PgBouncer in transaction mode. No prepared statements across transactions. Use this if we ever need more connections than the compute endpoint allows.

**Our choice: Direct connection.** We manage our own pool via pgxpool. pgx's built-in statement caching (`QueryExecModeCacheStatement`) gives us the performance benefit of prepared statements without PgBouncer's limitations.

### Neon-Specific Considerations

- **Scale-to-zero:** Neon suspends compute after 5 minutes of inactivity. Cold start is <500ms. For the live service evaluation, disable scale-to-zero on the primary branch to eliminate cold starts: `ALTER SYSTEM SET neon.suspend_timeout = '0'` or configure via Neon dashboard.
- **Storage:** Free tier is 0.5 GB. Our metadata is tiny — at 1M agents with 5 conversations each, the total data footprint is:
  - agents table: 1M × 24 bytes (UUID + timestamp) = ~24 MB
  - members table: 5M × 40 bytes = ~200 MB
  - cursors table: 5M × 40 bytes = ~200 MB
  - conversations table: 1M × 60 bytes = ~60 MB
  - Total with indexes: ~700 MB. This exceeds the free tier. At our actual take-home scale (hundreds of agents), we're well under 0.5 GB. Document the scaling path: upgrade to Neon Launch ($19/mo, 10 GB) when real data grows.
- **Compute hours:** 100 CU-hours/month at 0.25 CU = 400 hours of runtime = ~16 days continuous. For a take-home evaluation period, this is sufficient. If evaluation runs longer, upgrade or enable scale-to-zero during off-hours.

---

## 3. Driver Stack: pgx v5 + sqlc

### pgx v5 Native Interface

**Why native, not `database/sql` wrapper:**
- ~50% faster than `database/sql` due to binary wire protocol and statement caching
- Native PostgreSQL type support (UUID, JSONB, arrays, composite types)
- Batch operations (`pgx.Batch`, `pgx.CopyFrom`) not available through `database/sql`
- pgxpool is purpose-built for pgx, not a generic pool

**Key packages:**
- `github.com/jackc/pgx/v5` — driver
- `github.com/jackc/pgx/v5/pgxpool` — connection pool
- `github.com/jackc/pgx/v5/pgtype` — type system for UUID, arrays, etc.

### sqlc Code Generation

**What it does:** You write SQL queries in `.sql` files. sqlc parses them against your schema at build time and generates type-safe Go structs + functions that call pgx.

**Why it's worth the build step:**
- **Compile-time SQL validation.** A typo in a column name fails `sqlc generate`, not at runtime.
- **No manual row scanning.** sqlc generates `ScanRow` functions that map columns to struct fields.
- **Zero runtime overhead.** Generated code is the same pgx calls you'd write by hand.
- **Schema refactoring safety.** Change a column type → `sqlc generate` fails → you see every broken query immediately.

**Where sqlc doesn't apply:** Dynamic queries and batch operations. Specifically:
- `unnest()` batch upserts for cursor flush — raw pgx with `pool.Query()`
- Dynamic query building (if ever needed) — raw pgx

**Configuration (`sqlc.yaml`):**
```yaml
version: "2"
sql:
  - engine: "postgresql"
    queries: "internal/store/queries/"
    schema: "internal/store/schema/"
    gen:
      go:
        package: "db"
        out: "internal/store/db"
        sql_package: "pgx/v5"
        emit_json_tags: true
        emit_empty_slices: true
```

**Build integration:** Add `go generate` directive or Makefile target:
```makefile
generate:
	sqlc generate
```

### Dependencies (Go modules)

```
github.com/jackc/pgx/v5          # PostgreSQL driver
github.com/google/uuid            # UUIDv7 generation
```

sqlc is a build-time CLI tool, not a Go dependency. Install via `go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest` or use the official Docker image in CI.

---

## 4. Primary Key Strategy: UUIDv7

### Why UUIDv7, not UUIDv4

UUIDv4 is random. Every INSERT scatters across the B-tree index, causing random page splits and cache thrashing. UUIDv7 is time-ordered (embeds a millisecond timestamp), so inserts append to the rightmost leaf page — the same pattern as auto-incrementing integers but with distributed generation (no coordination needed).

**Measured impact at scale (from PostgreSQL benchmarks, 2024):**

| Metric | UUIDv4 | UUIDv7 | Improvement |
|---|---|---|---|
| Bulk insert throughput | Baseline | +49% | Sequential page writes vs random |
| Index size (1M rows) | Baseline | -25% | Fewer page splits → less fragmentation |
| Buffer cache hit ratio | Lower | Higher | Hot rightmost pages stay cached |

### Implementation

**Go side:** Generate with `uuid.NewV7()` from `github.com/google/uuid` (v1.6.0+, RFC 9562 compliant).

**PostgreSQL side:** Store as `UUID` type (16 bytes binary). NOT `TEXT` (36 bytes). The `UUID` type supports native comparison, indexing, and sorting without text parsing.

**Schema pattern:**
```sql
CREATE TABLE agents (
    id UUID PRIMARY KEY,  -- UUIDv7 generated in Go, passed as parameter
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

No `DEFAULT gen_random_uuid()` — we always generate in Go to ensure UUIDv7. If a row somehow gets inserted without an explicit ID (bug), it should fail loudly, not silently fall back to UUIDv4.

### UUIDv7 and information leakage

UUIDv7 embeds a millisecond-precision timestamp. This means agent IDs reveal when the agent was created. The spec says "opaque identifier" — does this violate opacity?

**No.** "Opaque" means the client shouldn't depend on internal structure for functionality — it's an identifier, not a timestamp field. The embedded timestamp is an implementation detail that happens to be useful for debugging (sorting by creation order). The spec doesn't require cryptographic opacity, and there's no authentication to protect, so timestamp leakage has no security implication.

---

## 5. Schema Design

### Complete Schema

```sql
-- ============================================================
-- AgentMail Metadata Schema
-- PostgreSQL 17 / Neon Serverless
-- ============================================================

-- Agents: each row is a registered agent identity
CREATE TABLE agents (
    id          UUID PRIMARY KEY,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Conversations: each row maps to one S2 stream
CREATE TABLE conversations (
    id              UUID PRIMARY KEY,
    s2_stream_name  TEXT NOT NULL,
    head_seq        BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One S2 stream per conversation, one conversation per stream
CREATE UNIQUE INDEX idx_conversations_s2_stream ON conversations(s2_stream_name);

-- Members: many-to-many join between conversations and agents
-- Hard-deleted on leave. S2 stream has the full membership event history.
CREATE TABLE members (
    conversation_id  UUID NOT NULL REFERENCES conversations(id),
    agent_id         UUID NOT NULL REFERENCES agents(id),
    joined_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_id, agent_id)
);

-- For "list conversations for agent" queries
CREATE INDEX idx_members_agent_id ON members(agent_id);

-- Cursors: server-managed read positions for at-least-once delivery.
-- Two cursors per (agent, conversation):
--   delivery_seq  — advanced by the server as events are shipped over a tail (SSE).
--                   Batched flush every 5s from an in-memory hot tier. Powers tail resume.
--   ack_seq       — advanced only by explicit client POST /conversations/:cid/ack.
--                   Synchronous write-through (no hot tier). Powers GET /agents/me/unread.
-- No foreign keys — validated at API layer, and cursors may outlive membership
-- (both preserved on leave for potential re-invite resume).
CREATE TABLE cursors (
    agent_id         UUID NOT NULL,
    conversation_id  UUID NOT NULL,
    delivery_seq     BIGINT NOT NULL DEFAULT 0,
    ack_seq          BIGINT NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, conversation_id),
    CHECK (delivery_seq >= 0),
    CHECK (ack_seq >= 0)
);
```

### Table-by-Table Rationale

#### `agents`

- **No indexes beyond PK.** The only query is `SELECT 1 FROM agents WHERE id = $1` (existence check). PK index handles it.
- **No metadata columns.** Spec says "no metadata." Don't add `name`, `status`, `email`, or anything else.
- **No soft-delete.** Spec doesn't mention agent deletion. Agents are permanent.

#### `conversations`

- **`s2_stream_name TEXT NOT NULL`:** Decouples conversation ID from S2 stream name. Format: `conv-{conversation_id}`. If we need to remap streams (migration, disaster recovery), we change this column without changing conversation IDs.
- **`UNIQUE` index on `s2_stream_name`:** Prevents bugs where two conversations accidentally point to the same S2 stream. A constraint violation here means a code bug, not a user error.
- **`head_seq BIGINT NOT NULL DEFAULT 0`:** Cached latest S2 sequence number for the conversation. Updated inline on every successful S2 append from the value returned by `AppendResult.LastSeqNum()`. This is the server-side source of truth for "what is the current tail of this conversation" — queried by `GET /agents/me/unread` and by the `ack_seq ≤ head_seq` validation in `POST /conversations/:cid/ack`. The alternative (querying S2 for every unread computation) would do an S2 round-trip per conversation per unread request; caching in Postgres collapses the unread query to one indexed SQL join. Drift window: a few milliseconds between S2 ack and the inline UPDATE — acceptable.

#### `members`

- **PK order `(conversation_id, agent_id)`:** Optimized for the hottest queries:
  - `WHERE conversation_id = $1 AND agent_id = $2` — exact point lookup (membership check, every API call)
  - `WHERE conversation_id = $1` — range scan (list all members of a conversation)
  - Both use the PK index directly. No secondary index needed for these patterns.
- **Separate index `(agent_id)`:** For `WHERE agent_id = $1` (list conversations for an agent). This query happens less frequently (only on `GET /conversations`), but it must be indexed.
- **Foreign keys with no CASCADE:** Agents and conversations are permanent. If we ever add deletion, we'll add explicit cascade logic — not a silent `ON DELETE CASCADE` that could wipe data unexpectedly.
- **Hard delete on leave.** No `left_at` column. Reasons:
  - Simpler membership check: `WHERE conversation_id = $1 AND agent_id = $2` — no `AND left_at IS NULL` condition on every query
  - S2 stream has full membership history (`agent_joined`, `agent_left` events) if audit is ever needed
  - Re-invite is just a fresh `INSERT` — clean, simple
  - No partial indexes needed, no NULL handling complexity

#### `cursors`

- **No foreign keys.** Cursor writes are on the performance-critical path. FK checks on every UPSERT add overhead (Postgres must verify agent and conversation exist). We validate at the API layer before any cursor operation.
- **Two independent cursors in one row.** `delivery_seq` and `ack_seq` are independent semantics colocated in a single row so that an unread query needs one indexed lookup, not a join between two tables. They never share a write path:
  - `delivery_seq` — written by the tail delivery loop via the batched-flush hot tier (`unnest()` upsert every 5s). Carries an `WHERE delivery_seq < EXCLUDED.delivery_seq` regression guard. Updated at event-delivery rate (tens to hundreds per second per tail).
  - `ack_seq` — written synchronously by the `POST /conversations/:cid/ack` handler. No hot tier. Carries an `WHERE ack_seq < EXCLUDED.ack_seq` regression guard. Updated only when the client explicitly acks (low volume, correctness-critical).
- **No invariant between `delivery_seq` and `ack_seq`.** `ack_seq` can exceed `delivery_seq` (passive reader that caught up via `GET /conversations/:cid/messages` and acked without ever opening a tail) or trail it (active tailer that received events but has not yet processed/acked). Both cursors are individually bounded by `conversations.head_seq`.
- **Both cursors survive leave.** When an agent leaves, neither `delivery_seq` nor `ack_seq` is deleted. On re-invite, the agent resumes tails from `delivery_seq` and gets an accurate unread count from `ack_seq`. This is better UX than resetting to 0 (which would force re-reading the entire conversation).
- **`BIGINT` for both seq columns.** S2 sequence numbers are 64-bit. `INTEGER` (32-bit, max ~2.1B) would overflow for a conversation with 30 tokens/sec sustained for ~2.3 years. `BIGINT` handles 64-bit values safely.
- **`CHECK (delivery_seq >= 0)` and `CHECK (ack_seq >= 0)`.** Defense-in-depth against negative sequence bugs. S2 sequences start at 0.
- **PK order `(agent_id, conversation_id)`:** Optimized for disconnect cleanup — "flush all cursors for agent X" is a range scan on physically adjacent rows. Also the ideal layout for the `GET /agents/me/unread` join, which starts from `members` (agent_id-indexed) and probes this table by `(agent_id, conversation_id)`.

### Why No Additional Tables

The schema has exactly four tables. No more are needed:

- **No `messages` table.** Messages live in S2 streams. The metadata layer doesn't store message content.
- **No `sessions` or `connections` table.** Active connections are tracked in-memory (`ConnRegistry`). Persisting them to Postgres would add write overhead for ephemeral state.
- **No `events` or `audit` table.** S2 streams ARE the audit log. Every membership change is recorded as an `agent_joined` or `agent_left` event on the conversation's stream.

---

## 6. Connection Pooling

### pgxpool Configuration

```go
poolConfig, _ := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))

poolConfig.MaxConns = 15                              // Optimal for 4-core SSD: (cores * 2) + headroom
poolConfig.MinConns = 5                               // Pre-warm 5 connections on startup
poolConfig.MaxConnLifetime = 30 * time.Minute         // Recycle connections to prevent stale state
poolConfig.MaxConnLifetimeJitter = 5 * time.Minute    // Stagger recycling to prevent thundering herd
poolConfig.MaxConnIdleTime = 5 * time.Minute          // Release idle connections
poolConfig.HealthCheckPeriod = 30 * time.Second       // Detect dead connections

pool, _ := pgxpool.NewWithConfig(ctx, poolConfig)
```

### Why 15 Connections Handles Thousands of Goroutines

Membership checks and cursor reads are indexed point lookups: sub-millisecond execution time. A goroutine holds a connection for ~100-500 microseconds. With 15 connections:

```
Throughput = 15 connections × (1,000,000 µs / 300 µs per query) = 50,000 queries/sec
```

At 50K queries/sec with 15 connections, pgxpool queues goroutines waiting for a connection. The queue wait is negligible because connections free up in microseconds.

### Critical Rule: Never Hold a Connection During SSE

SSE handlers run for minutes to hours. If an SSE handler holds a database connection for its lifetime, the 15-connection pool starves after 15 concurrent SSE connections.

**Pattern:**
```
SSE handler starts:
  1. Query Postgres for cursor (acquire connection → query → release)
  2. Open S2 read session (no database connection held)
  3. Stream events to client (no database connection held)
  4. Every 5 seconds: cursor flush goroutine handles batch update (its own connection, briefly)
  5. On disconnect: flush final cursor (acquire connection → query → release)
```

The SSE handler touches the database exactly twice (connect + disconnect), and both are sub-millisecond. The pool stays healthy.

---

## 7. Cursor Management: Two-Tier Architecture

### Architecture Diagram

Two cursors per `(agent, conversation)` — `delivery_seq` (two-tier, batched) and `ack_seq` (single-tier, synchronous). Only `delivery_seq` is cached in memory; `ack_seq` writes go straight to Postgres.

```text
                    In-Memory (Hot Tier)                     PostgreSQL (Warm Tier)
                    ────────────────────                     ──────────────────────────
SSE event delivered
        │
        ▼
┌─────────────────────┐                              ┌──────────────────────────────┐
│   CursorCache       │    every 5 seconds           │   cursors table              │
│   sync.RWMutex      │ ───────────────────────────→ │   (agent_id, conv_id,        │
│   map[key]entry     │    batch UPSERT delivery_seq │    delivery_seq, ack_seq,    │
│   (delivery_seq)    │    via unnest() arrays       │    updated_at)               │
│   dirty set         │                              │                              │
└─────────────────────┘                              │                              │
        │                                            │                              │
        │  on disconnect / leave                     │                              │
        │  immediate single delivery_seq flush       │                              │
        └───────────────────────────────────────────▶│                              │
                                                     │                              │
POST /conversations/:cid/ack                         │                              │
        │  synchronous write-through,                │                              │
        │  bypasses hot tier                         │                              │
        └───────────────────────────────────────────▶│   (ack_seq UPSERT)           │
                                                     └──────────────────────────────┘
```

### Hot Tier: In-Memory Map

```go
type cursorKey struct {
    AgentID        uuid.UUID
    ConversationID uuid.UUID
}

type cursorEntry struct {
    DeliverySeq uint64    // written on every tail event delivery; batched-flushed
    UpdatedAt   time.Time
}

type CursorCache struct {
    mu      sync.RWMutex
    cursors map[cursorKey]cursorEntry
    dirty   map[cursorKey]struct{}
}
```

**Scope of the hot tier:** only `delivery_seq` is cached. `ack_seq` is not tracked in memory — every `POST /conversations/:cid/ack` writes directly to Postgres (see §7 "Ack Path" below). Mixing them would force the ack handler to coordinate with the flush goroutine; keeping them separate keeps each path simple and correct.

**Why `sync.RWMutex` + regular map, not `sync.Map`:**

`sync.Map` is optimized for two patterns: (1) write-once-read-many, or (2) disjoint key sets per goroutine. Cursor updates are write-heavy from many goroutines on overlapping keys — neither pattern applies. A `sync.RWMutex` gives predictable performance.

**At extreme scale (>100K concurrent SSE connections):** Shard the map into 64 buckets keyed by `hash(agent_id) % 64`. Each bucket has its own `sync.RWMutex`. This reduces lock contention to 1/64th. Not implemented now — document as scaling optimization.

### Warm Tier: Batched PostgreSQL Flush (`delivery_seq` only)

**Flush SQL (unnest batch upsert):**
```sql
INSERT INTO cursors (agent_id, conversation_id, delivery_seq, updated_at)
SELECT * FROM unnest($1::uuid[], $2::uuid[], $3::bigint[], $4::timestamptz[])
ON CONFLICT (agent_id, conversation_id)
DO UPDATE SET delivery_seq = EXCLUDED.delivery_seq, updated_at = EXCLUDED.updated_at
WHERE cursors.delivery_seq < EXCLUDED.delivery_seq;
```

**The `WHERE cursors.delivery_seq < EXCLUDED.delivery_seq` clause is critical.** It prevents a stale flush from regressing the cursor. Scenario: server A flushes cursor at seq 100. Server B (after failover) flushes stale cursor at seq 80. Without the WHERE clause, cursor regresses to 80 and the agent re-receives 20 events unnecessarily. With it, the stale flush is a no-op.

**The upsert does NOT touch `ack_seq`.** `DO UPDATE SET` lists only `delivery_seq` and `updated_at`. If the row does not yet exist, it is inserted with `ack_seq = 0` (schema default). If it exists, `ack_seq` is left alone — the ack handler owns that column.

**Performance:** A single `unnest()` upsert of 10,000 rows completes in ~5-20ms. Even at 100K active cursors, a single flush takes <100ms — well within the 5-second interval.

### Ack Path: Synchronous Write-Through

`POST /conversations/:cid/ack` with `{"seq": N}` performs an immediate single-row upsert — no cache, no batching:

```sql
INSERT INTO cursors (agent_id, conversation_id, ack_seq, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (agent_id, conversation_id)
DO UPDATE SET ack_seq = EXCLUDED.ack_seq, updated_at = EXCLUDED.updated_at
WHERE cursors.ack_seq < EXCLUDED.ack_seq;
```

**Why no hot tier for `ack_seq`:**
- **Volume.** Ack volume is orders of magnitude lower than event-delivery volume. A batched-flush design is unjustified at this scale.
- **Correctness.** `GET /agents/me/unread` consults `ack_seq` directly from Postgres. A 5-second flush delay would make the unread view lie — an agent that just acked seq 127 could get back a count including events ≤ 127 on the very next call.
- **Simplicity.** Separating the two write paths removes all coordination between the ack handler and the flush goroutine. No lock-order concerns, no dirty-bit contention.

**Regression guard.** Same `WHERE cursors.ack_seq < EXCLUDED.ack_seq` pattern — replayed or out-of-order acks are silent no-ops, never un-acks.

**Validation.** `seq > head_seq` is rejected with 400 before the UPSERT runs (checked in the handler against `conversations.head_seq`). `seq < 0` is rejected at JSON deserialization time by schema validation.

### Flush Triggers

| Trigger | What happens | Why |
|---|---|---|
| **Periodic (every 5 sec)** | Background goroutine flushes ALL dirty `delivery_seq` values | Bounds data loss on crash to 5 seconds |
| **Clean disconnect** | SSE handler flushes THIS agent's `delivery_seq` immediately | Zero data loss on graceful close |
| **Server shutdown** | Graceful shutdown flushes ALL dirty `delivery_seq` values before exit | Zero data loss on planned restart |
| **Agent leave** | Flush this agent's `delivery_seq` for this conversation | Preserve accurate position for potential re-invite |
| **`POST .../ack` received** | Synchronous single-row `ack_seq` UPSERT, bypasses hot tier | Client has durability on return; next `/unread` reflects the ack |

### Crash Recovery

Server crashes → in-memory `delivery_seq` values lost (`ack_seq` is always durable, never cached). Recovery:
1. Agent reconnects via SSE
2. Server reads `delivery_seq` from Postgres: last flushed value (at most 5 seconds stale)
3. S2 read session starts from that position
4. Agent re-receives up to ~150 events (5 sec × ~30 events/sec)
5. Events carry sequence numbers → client deduplicates trivially

This is textbook **at-least-once delivery** — the industry standard for streaming systems.

### Cursor Behavior on Leave and Re-Invite (both cursors preserved)

**On leave:**
1. Flush `delivery_seq` to Postgres (preserve last known position). `ack_seq` is already durable — nothing to flush.
2. Remove from in-memory cache.
3. Do NOT delete the `cursors` row from Postgres.

**On re-invite + SSE connect:**
1. Check in-memory cache (miss — agent wasn't connected).
2. Fall back to Postgres — row exists from before leave with both `delivery_seq` and `ack_seq` intact.
3. Resume tailing from `delivery_seq`.
4. Agent catches up on all messages sent while they were gone.

**On re-invite + `GET /agents/me/unread`:**
1. The join reads `ack_seq` directly from Postgres (same value as before leave).
2. Any message that landed on the conversation while the agent was gone shows up as unread (`head_seq - ack_seq > 0`).
3. The agent can then catch up via `GET /conversations/:cid/messages?from=<ack_seq>` and ack.

**Why this is better than deleting cursors on leave:**

If we deleted cursors, a re-invited agent starts from sequence 0 — re-reading the ENTIRE conversation history, AND losing its ack state so the unread count is inflated to "everything." For a long conversation with 100K events, that's a massive unnecessary replay and a broken unread view. Preserving both cursors means they only replay/count what they missed while gone.

The spec says "when an agent is added, it has access to the full conversation history." This is satisfied by the S2 stream containing all events — the agent CAN read from 0 by passing `?from=0` on the SSE endpoint. But the default (resume from cursors) is better UX.

### Interface

```go
type CursorStore interface {
    // --- Delivery cursor (tail resume; two-tier, batched) ---

    // GetDeliveryCursor reads delivery_seq: memory first, Postgres fallback.
    // Returns 0 if no row exists (start tail from beginning).
    GetDeliveryCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error)

    // UpdateDeliveryCursor writes to memory only. No I/O, cannot fail, does not block.
    // Batched flush to Postgres by the background goroutine.
    UpdateDeliveryCursor(agentID, convID uuid.UUID, seqNum uint64)

    // FlushOne flushes a single agent's delivery_seq to Postgres (on disconnect/leave).
    FlushOne(ctx context.Context, agentID, convID uuid.UUID) error

    // FlushAll flushes all dirty delivery_seq values to Postgres (periodic/shutdown).
    FlushAll(ctx context.Context) error

    // Start begins the background delivery_seq flush goroutine. Blocks until ctx is canceled.
    Start(ctx context.Context)

    // --- Ack cursor (client-controlled; single-tier, synchronous) ---

    // Ack performs a synchronous write-through UPSERT of ack_seq.
    // Regression-guarded: seq < current ack_seq is a silent no-op.
    // Returns ErrAckBeyondHead if seq > head_seq of the conversation.
    Ack(ctx context.Context, agentID, convID uuid.UUID, seq uint64) error

    // GetAckCursor reads ack_seq directly from Postgres. Returns 0 if no row exists.
    GetAckCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error)

    // ListUnreadForAgent returns every conversation the agent is a member of where
    // head_seq > ack_seq, ordered by head_seq DESC. Default limit 100, max 500.
    ListUnreadForAgent(ctx context.Context, agentID uuid.UUID, limit int32) ([]UnreadEntry, error)
}

type UnreadEntry struct {
    ConversationID uuid.UUID
    HeadSeq        uint64
    AckSeq         uint64
    EventDelta     uint64 // head_seq - ack_seq
}
```

---

## 8. Membership Caching

### The Problem

Every API call validates: "Is agent X a member of conversation Y?" This is a `SELECT 1 FROM members WHERE conversation_id = $1 AND agent_id = $2` — an indexed point lookup, sub-millisecond. But at millions of agents with thousands of concurrent API calls, this becomes the #1 query by volume.

### The Solution: In-Process LRU Cache

```go
type MembershipCache struct {
    cache *lru.Cache[membershipKey, membershipEntry]
    ttl   time.Duration
    pool  *pgxpool.Pool
}

type membershipKey struct {
    AgentID        uuid.UUID
    ConversationID uuid.UUID
}

type membershipEntry struct {
    IsMember  bool
    CachedAt  time.Time
}
```

### Cache Configuration

| Parameter | Value | Rationale |
|---|---|---|
| **Max entries** | 100,000 | At ~48 bytes per entry: ~4.8 MB. Negligible memory cost. |
| **TTL** | 60 seconds | Safety net for any missed invalidation. Short enough that stale data is brief. Long enough that the cache is useful. |
| **Eviction** | LRU | Least-recently-used entries evicted when cache is full. Hot entries (active conversations) stay cached. |

### Cache Operations

**Read path (every API call):**
```
1. Check cache for (agent_id, conversation_id)
2. If hit AND not expired (age < TTL): return cached result
3. If miss OR expired:
   a. Query Postgres: SELECT 1 FROM members WHERE conversation_id = $1 AND agent_id = $2
   b. Store result in cache (true or false)
   c. Return result
```

**Write path (invite):**
```
1. INSERT INTO members ON CONFLICT DO NOTHING
2. Set cache: (agent_id, conversation_id) → true
```

**Write path (leave):**
```
1. DELETE FROM members WHERE conversation_id = $1 AND agent_id = $2
2. Delete from cache: (agent_id, conversation_id)
   (Don't set to false — the agent might be re-invited immediately. Let the next check query Postgres fresh.)
```

### Why In-Process, Not Redis

| Dimension | In-process LRU | Redis |
|---|---|---|
| **Latency** | ~50 nanoseconds | ~500 microseconds (network hop) |
| **Failure mode** | Process crash = cache lost (fine, Postgres is source of truth) | Redis down = every request falls through to Postgres (thundering herd) |
| **Dependency** | None | External service to provision, monitor, pay for |
| **Multi-instance** | Each instance has own cache | Shared cache across instances |

For single-instance deployment, in-process is 10,000x faster with zero operational cost.

### Multi-Instance Scaling Path (Documented, Not Implemented)

When we go to 2+ server instances, each has its own local cache. Problem: Instance A processes a leave, Instance B's cache still says "is member." Solution:

```sql
-- In the leave transaction:
BEGIN;
DELETE FROM members WHERE conversation_id = $1 AND agent_id = $2;
NOTIFY membership_changed, $1::text;  -- broadcast conversation ID
COMMIT;
```

Each instance listens on a dedicated connection:
```go
conn.Exec(ctx, "LISTEN membership_changed")
// In a goroutine:
for {
    notification, _ := conn.WaitForNotification(ctx)
    cache.InvalidateConversation(notification.Payload)  // evict all entries for this conversation
}
```

**LISTEN/NOTIFY reliability:** Notifications are transactional (only sent on commit), broadcast to all listeners, and queued up to 8 GB. The only loss scenario is a listener not connected during the notification — mitigated by the 60-second TTL (stale cache self-expires within a minute).

### Interface

```go
type MembershipService interface {
    // IsMember checks if agent is a member (cache-first, Postgres fallback)
    IsMember(ctx context.Context, agentID, convID uuid.UUID) (bool, error)

    // AddMember adds agent to conversation (Postgres + cache populate)
    AddMember(ctx context.Context, convID, agentID uuid.UUID) error

    // RemoveMember removes agent from conversation (Postgres + cache invalidate)
    // Returns error if agent is last member.
    RemoveMember(ctx context.Context, convID, agentID uuid.UUID) error

    // ListMembers returns all members of a conversation (always from Postgres, not cached)
    ListMembers(ctx context.Context, convID uuid.UUID) ([]uuid.UUID, error)

    // ListConversations returns all conversations for an agent (always from Postgres, not cached)
    ListConversations(ctx context.Context, agentID uuid.UUID) ([]Conversation, error)
}
```

---

## 9. Concurrency & Locking

### Race Condition 1: Last Member Cannot Leave

**Scenario:** Two agents in a conversation both call leave simultaneously. Both check member count, both see 2, both proceed. Conversation has 0 members.

**Solution:** `SELECT ... FOR UPDATE` serializes concurrent leave operations per conversation.

```sql
BEGIN;

-- Lock all member rows for this conversation.
-- Second concurrent leave blocks here until first commits.
SELECT agent_id FROM members WHERE conversation_id = $1 FOR UPDATE;

-- Application checks:
-- 1. Is the leaving agent in the result set? (if not: 404)
-- 2. Is the result set size == 1? (if yes: reject, return 409)

-- If checks pass:
DELETE FROM members WHERE conversation_id = $1 AND agent_id = $2;

COMMIT;
```

**Why this doesn't deadlock:** Both transactions lock the same rows in the same order (B-tree order of `(conversation_id, agent_id)`). The second transaction blocks (not deadlocks) until the first commits. PostgreSQL's lock manager handles this natively.

**Why `SELECT agent_id ...` instead of `SELECT COUNT(*) ...`:** Selecting actual rows lets us verify the leaving agent is a member in the same locked query. Avoids a separate membership check.

### Race Condition 2: Invite Idempotency

**Scenario:** Two requests simultaneously invite agent B to conversation C.

**Solution:** `INSERT ... ON CONFLICT DO NOTHING`

```sql
INSERT INTO members (conversation_id, agent_id, joined_at)
VALUES ($1, $2, now())
ON CONFLICT (conversation_id, agent_id) DO NOTHING
RETURNING conversation_id;
```

If the insert succeeds (RETURNING returns a row), agent was newly added — write `agent_joined` event to S2. If the insert is a no-op (RETURNING returns no row), agent was already a member — skip the S2 event.

### Race Condition 3: Invite vs. Leave on the Same Agent

**Scenario:** Agent A invites agent B. Simultaneously, agent B leaves.

**Analysis:** These operate on the same row `(conversation_id, agent_B)`. PostgreSQL serializes them. Two outcomes:

1. **Leave commits first:** Row deleted. Then invite inserts a fresh row. Agent B is a member again. `agent_left` then `agent_joined` events on S2 stream. Correct.
2. **Invite commits first:** `ON CONFLICT DO NOTHING` (agent B is already a member). Then leave deletes the row. Agent B is gone. Only `agent_left` event on S2 stream. Correct.

Both outcomes are consistent and the S2 event stream reflects reality.

### Race Condition 4: Last Member Leave vs. Invite

**Scenario:** Conversation has 1 member (agent A). Agent A leaves. Simultaneously, agent A invites agent B.

**Analysis:** The leave path uses `FOR UPDATE` to lock all member rows. The invite path does `INSERT`. These don't conflict directly (INSERT adds a new row, FOR UPDATE locks existing rows). But:

1. **Leave acquires lock first:** Sees count = 1. Rejects leave. Releases lock. Invite proceeds. Agent B is added. Now count = 2. Agent A can leave later. Correct.
2. **Invite commits first:** Agent B is added. Count = 2. Leave acquires lock, sees count = 2, proceeds. Agent A leaves. Count = 1. Conversation survives with agent B. Correct.

Both outcomes are safe.

### Race Condition 5: Concurrent Agent Registration

**No race condition.** Each registration generates a UUIDv7 independently. UUIDs don't collide. Inserts are independent rows. No locking needed.

---

## 10. Connection Registry (Active Stream Termination)

### The Problem

The spec requires: "Active streaming connections are terminated on leave." When an agent leaves a conversation, their SSE read stream and any in-progress streaming write must be killed synchronously — before the leave response is returned.

### Design

```go
type ConnRegistry struct {
    mu    sync.Mutex
    conns map[connKey]*connEntry
}

type connKey struct {
    AgentID        uuid.UUID
    ConversationID uuid.UUID
}

type connEntry struct {
    Cancel context.CancelFunc   // cancels the SSE/stream goroutine
    Done   chan struct{}         // closed when the goroutine exits
}
```

### Leave Handler Flow

```
1. Begin Postgres transaction
2. SELECT ... FOR UPDATE (lock members)
3. Check count > 1
4. DELETE member row
5. Commit transaction
6. Invalidate membership cache
7. Look up (agent_id, conversation_id) in ConnRegistry
8. If found:
   a. Call entry.Cancel()           -- signals the SSE goroutine to stop
   b. <-entry.Done                  -- blocks until goroutine confirms exit
   c. Remove entry from registry
9. Write agent_left event to S2 stream
10. Return 200
```

**Why step 8b (waiting for goroutine exit) matters:** If we return 200 before the SSE goroutine exits, there's a window where the agent receives events after being told they left. The `Done` channel ensures the stream is dead before we respond.

**Timeout on wait:** Add a 5-second timeout on `<-entry.Done` to prevent a stuck goroutine from blocking the leave response indefinitely. If timeout fires, log a warning and proceed.

### SSE Handler Registration

```
1. Create context with cancel: ctx, cancel := context.WithCancel(r.Context())
2. Create done channel: done := make(chan struct{})
3. Register: registry.Register(agentID, convID, cancel, done)
4. defer:
   a. close(done)                   -- signals that goroutine has exited
   b. registry.Deregister(agentID, convID)
   c. cursor.FlushOne(agentID, convID)  -- flush final cursor position
5. Run SSE loop until ctx.Done()
```

### One SSE Connection Per (Agent, Conversation)

The spec implies a single reader per agent per conversation. If an agent opens a second SSE connection to the same conversation:

1. Look up existing entry in ConnRegistry
2. If found: cancel the old connection (call entry.Cancel(), wait on entry.Done)
3. Register the new connection
4. Old SSE goroutine sees context cancellation, flushes cursor, exits
5. New SSE goroutine starts from the flushed cursor position

This prevents resource leaks from abandoned connections.

---

## 11. Store Interface: Complete Go Design

### Top-Level Interface

```go
// Store is the top-level metadata store. It composes all sub-stores.
type Store interface {
    Agents() AgentStore
    Conversations() ConversationStore
    Members() MembershipService
    Cursors() CursorStore
    ConnRegistry() *ConnRegistry
    Close() error
}
```

### Sub-Interfaces

```go
type AgentStore interface {
    Create(ctx context.Context) (uuid.UUID, error)
    Exists(ctx context.Context, id uuid.UUID) (bool, error)
}

type ConversationStore interface {
    Create(ctx context.Context, id uuid.UUID, s2StreamName string) error
    Get(ctx context.Context, id uuid.UUID) (*Conversation, error)
    Exists(ctx context.Context, id uuid.UUID) (bool, error)
}

type MembershipService interface {
    IsMember(ctx context.Context, agentID, convID uuid.UUID) (bool, error)
    AddMember(ctx context.Context, convID, agentID uuid.UUID) error
    RemoveMember(ctx context.Context, convID, agentID uuid.UUID) error
    ListMembers(ctx context.Context, convID uuid.UUID) ([]uuid.UUID, error)
    ListConversations(ctx context.Context, agentID uuid.UUID) ([]ConversationWithMembers, error)
}

type CursorStore interface {
    // Delivery cursor (two-tier, batched). See §7 for full semantics.
    GetDeliveryCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error)
    UpdateDeliveryCursor(agentID, convID uuid.UUID, seqNum uint64)
    FlushOne(ctx context.Context, agentID, convID uuid.UUID) error
    FlushAll(ctx context.Context) error
    Start(ctx context.Context)

    // Ack cursor (single-tier, synchronous). See §7 for full semantics.
    Ack(ctx context.Context, agentID, convID uuid.UUID, seq uint64) error
    GetAckCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error)
    ListUnreadForAgent(ctx context.Context, agentID uuid.UUID, limit int32) ([]UnreadEntry, error)
}

type UnreadEntry struct {
    ConversationID uuid.UUID
    HeadSeq        uint64
    AckSeq         uint64
    EventDelta     uint64
}
```

### Domain Types

```go
type Conversation struct {
    ID            uuid.UUID
    S2StreamName  string
    CreatedAt     time.Time
}

type ConversationWithMembers struct {
    ID        uuid.UUID
    Members   []uuid.UUID
    CreatedAt time.Time
}
```

---

## 12. SQL Queries (sqlc Source)

### agents.sql

```sql
-- name: CreateAgent :one
INSERT INTO agents (id, created_at) VALUES ($1, now()) RETURNING id, created_at;

-- name: AgentExists :one
SELECT EXISTS(SELECT 1 FROM agents WHERE id = $1);
```

### conversations.sql

```sql
-- name: CreateConversation :exec
INSERT INTO conversations (id, s2_stream_name, head_seq, created_at)
VALUES ($1, $2, 0, now());

-- name: GetConversation :one
SELECT id, s2_stream_name, head_seq, created_at FROM conversations WHERE id = $1;

-- name: ConversationExists :one
SELECT EXISTS(SELECT 1 FROM conversations WHERE id = $1);

-- name: GetConversationHeadSeq :one
SELECT head_seq FROM conversations WHERE id = $1;

-- name: UpdateConversationHeadSeq :exec
-- Called inline with every successful S2 append using AppendResult.LastSeqNum().
-- Regression-guarded: an out-of-order write cannot rewind head_seq.
UPDATE conversations
SET head_seq = $2
WHERE id = $1 AND head_seq < $2;
```

### members.sql

```sql
-- name: AddMember :one
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
SELECT agent_id FROM members WHERE conversation_id = $1 FOR UPDATE;

-- name: ListConversationsForAgent :many
SELECT m.conversation_id, c.created_at
FROM members m
JOIN conversations c ON c.id = m.conversation_id
WHERE m.agent_id = $1
ORDER BY c.created_at DESC;

-- name: ListMembersForConversation :many
SELECT agent_id FROM members WHERE conversation_id = $1;
```

### cursors.sql

```sql
-- name: GetDeliveryCursor :one
SELECT delivery_seq FROM cursors WHERE agent_id = $1 AND conversation_id = $2;

-- name: GetAckCursor :one
SELECT ack_seq FROM cursors WHERE agent_id = $1 AND conversation_id = $2;

-- name: UpsertDeliveryCursor :exec
-- Single-row delivery_seq UPSERT for disconnect/leave/shutdown flush of one agent.
-- Does NOT touch ack_seq.
INSERT INTO cursors (agent_id, conversation_id, delivery_seq, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (agent_id, conversation_id)
DO UPDATE SET delivery_seq = EXCLUDED.delivery_seq, updated_at = EXCLUDED.updated_at
WHERE cursors.delivery_seq < EXCLUDED.delivery_seq;

-- name: AckCursor :exec
-- Synchronous write-through UPSERT for POST /conversations/:cid/ack.
-- Does NOT touch delivery_seq. Regression-guarded: replayed/out-of-order acks
-- are silent no-ops. Handler validates seq <= conversations.head_seq before calling.
INSERT INTO cursors (agent_id, conversation_id, ack_seq, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (agent_id, conversation_id)
DO UPDATE SET ack_seq = EXCLUDED.ack_seq, updated_at = EXCLUDED.updated_at
WHERE cursors.ack_seq < EXCLUDED.ack_seq;

-- name: ListUnreadForAgent :many
-- Powers GET /agents/me/unread. Single indexed join: members (agent-indexed)
-- joined against conversations (PK) and LEFT JOIN cursors. Filters to only
-- conversations with unread events (head_seq > ack_seq).
SELECT c.id                                         AS conversation_id,
       c.head_seq                                   AS head_seq,
       COALESCE(k.ack_seq, 0)                       AS ack_seq,
       c.head_seq - COALESCE(k.ack_seq, 0)          AS event_delta
FROM members m
JOIN conversations c ON c.id = m.conversation_id
LEFT JOIN cursors k
       ON k.agent_id = m.agent_id
      AND k.conversation_id = m.conversation_id
WHERE m.agent_id = $1
  AND c.head_seq > COALESCE(k.ack_seq, 0)
ORDER BY c.head_seq DESC
LIMIT $2;
```

**Batch delivery_seq flush uses raw pgx (not sqlc)** because it requires `unnest()` with array parameters. The single-row `UpsertDeliveryCursor` above is for disconnect/leave/shutdown paths; the periodic flush goroutine uses a hand-written `unnest()` upsert (see §7).

---

## 13. Migration Strategy

### Initial Migration (v001)

A single migration file creates the complete schema. Run on server startup before accepting traffic.

**Migration tool options:**
- **golang-migrate** (`github.com/golang-migrate/migrate`): File-based migrations, supports pgx. Industry standard.
- **goose** (`github.com/pressly/goose`): Similar, slightly simpler API.
- **Embedded SQL:** For the take-home, the simplest approach is embedding the schema SQL in the Go binary and running it on startup with `CREATE TABLE IF NOT EXISTS`. No migration tool needed for a single-version schema.

**Recommendation for take-home:** Embedded SQL with `IF NOT EXISTS`. Document golang-migrate as the production migration strategy.

### Schema Evolution Path

When the schema needs to change (adding columns, indexes, etc.):

1. Add a numbered migration file (e.g., `002_add_foo.sql`)
2. golang-migrate tracks which migrations have been applied in a `schema_migrations` table
3. On deploy, run `migrate up` — only unapplied migrations execute
4. Backward-incompatible changes require a multi-step deploy (add new column → deploy code that uses both → drop old column)

---

## 14. Scaling Roadmap (Millions → Billions)

This section documents the path from the current design (millions of agents, single instance) to billions of agents.

### Phase 1: Current Design (Millions)

- Single Fly.io instance + Neon Postgres
- In-process membership cache (LRU, 100K entries, 60s TTL)
- In-memory cursor hot tier + batched Postgres flush
- pgxpool with 15 connections

**Handles:** ~1M agents, ~5M conversations, ~50K concurrent SSE connections (limited by Fly.io instance memory/CPU)

### Phase 2: Read Replicas (Tens of Millions)

- Add Neon read replicas
- Route read queries (membership checks, cursor reads, conversation listings) to replicas
- Keep writes on primary (agent registration, invite/leave, cursor flush)
- Membership cache reduces read replica load by 90%+

**Handles:** ~10M agents. Read replicas handle the increased read volume.

### Phase 3: Partitioning (Hundreds of Millions)

- Hash-partition `members` table by `conversation_id` (64-128 partitions)
- Hash-partition `cursors` table by `agent_id` (64-128 partitions)
- Each partition is independently vacuumed, reindexed, and backed up
- Queries with the partition key prune to a single partition

**Tradeoff:** `ListConversationsForAgent` now scans all partitions of `members` (agent_id is not the partition key). Mitigation: add a denormalized `agent_conversations` table indexed by agent_id, maintained via trigger. Or accept the cross-partition scan (it's an infrequent query).

**Handles:** ~500M agents.

### Phase 4: Dedicated Instances (Billions)

- Replace Neon with dedicated PostgreSQL (AWS RDS or self-managed)
- Shard by tenant/region if multi-tenant
- Redis between hot tier and Postgres for cursor durability across server restarts
- LISTEN/NOTIFY for cross-instance cache invalidation
- PgBouncer for connection pooling across multiple server instances

**Handles:** Billions of agents. This is a different system at this point — document the architecture, don't build it.

---

## 15. Edge Cases & Failure Modes

### Server Crash During Leave Transaction

**Scenario:** Server crashes after `DELETE FROM members` but before committing the transaction.

**Result:** PostgreSQL rolls back the uncommitted transaction. Member row is restored. The leave didn't happen. Agent is still a member. Correct — no partial state.

### Server Crash During Invite

**Scenario:** Server crashes after `INSERT INTO members` commits but before writing `agent_joined` event to S2.

**Result:** Agent is a member in Postgres but no `agent_joined` event exists on the S2 stream. The membership is correct (agent can read/write), but the event stream is missing the system event.

**Mitigation:** On server startup, reconcile: for each member in Postgres, check if the S2 stream has a corresponding `agent_joined` event after the most recent `agent_left` for that agent. If not, write a backfill `agent_joined` event. This is a recovery sweep, not a hot path.

**Simpler mitigation for take-home:** Accept the inconsistency. The missing system event doesn't affect functionality — it's cosmetic (history doesn't show "Agent B joined"). Document the reconciliation as a production enhancement.

### Neon Cold Start During Evaluation

**Scenario:** Evaluator hits the API after the Neon compute has been idle for 5+ minutes. First request takes ~500ms instead of ~5ms.

**Mitigation:** Disable scale-to-zero for the evaluation period. Or: add a health check that queries Postgres — if the health check runs on a schedule (e.g., Fly.io's built-in health checks every 30 seconds), the Neon compute never goes idle.

### pgxpool Exhaustion

**Scenario:** All 15 connections are in use. A new goroutine tries to acquire a connection.

**Result:** pgxpool queues the goroutine. It blocks until a connection is released. Since our queries are sub-millisecond, the wait is typically <1ms. If the queue grows (all connections stuck on slow queries), pgxpool eventually times out with a context error.

**Mitigation:** Set `context.WithTimeout` on all database operations:
```go
ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
defer cancel()
```
If Postgres is truly unresponsive, requests fail fast with a 5-second timeout instead of hanging indefinitely.

### Membership Cache Poisoning

**Scenario:** Cache says agent is a member. Agent was actually removed by a direct Postgres manipulation (not through our API).

**Result:** Agent can read/write for up to 60 seconds (TTL) after removal.

**Mitigation:** This only happens if someone modifies Postgres directly, bypassing our API. Don't do that. The 60-second TTL is the safety net — eventually the cache self-corrects.

---

## 16. Files to Create

```
internal/
├── store/
│   ├── schema/
│   │   └── schema.sql            # Complete DDL
│   ├── queries/
│   │   ├── agents.sql            # sqlc queries for agents
│   │   ├── conversations.sql     # sqlc queries for conversations
│   │   ├── members.sql           # sqlc queries for members
│   │   └── cursors.sql           # sqlc queries for cursors
│   ├── db/                       # sqlc generated code (git-committed)
│   │   ├── db.go
│   │   ├── models.go
│   │   ├── agents.sql.go
│   │   ├── conversations.sql.go
│   │   ├── members.sql.go
│   │   └── cursors.sql.go
│   ├── postgres.go               # Store implementation (composes sub-stores)
│   ├── cursor_cache.go           # In-memory cursor hot tier
│   ├── membership_cache.go       # In-memory membership LRU cache
│   └── conn_registry.go          # Active connection tracking
├── model/
│   └── types.go                  # Domain types (Conversation, ConversationWithMembers)
sqlc.yaml                         # sqlc configuration
```
