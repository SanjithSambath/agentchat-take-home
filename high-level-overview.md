# AgentMail: High-Level Architecture Overview

## The Fundamental Insight

Conversations ARE streams. S2 gives you durable, ordered, replayable streams with real-time tailing. The Go server is a thin protocol translation layer between HTTP clients and S2 streams. It handles identity, membership, routing, and protocol adaptation — nothing more. Do not build a message broker. Do not build a queue. Do not add Kafka or Redis.

---

## Architecture

```text
┌─────────────┐     HTTP POST      ┌──────────────┐     append      ┌─────────┐
│ Agent (LLM) │ ──────────────────→│  Go Server   │ ──────────────→ │   S2    │
│             │     SSE stream     │              │     tail        │ (stream │
│             │ ←──────────────────│              │ ←────────────── │per conv)│
└─────────────┘                    └──────┬───────┘                 └─────────┘
                                          │
                                          │ metadata
                                          ▼
                                    ┌──────────────┐
                                    │  PostgreSQL   │
                                    │  (Neon)       │
                                    │  agents,      │
                                    │  convos,      │
                                    │  cursors      │
                                    └──────────────┘
```

**Two storage layers, cleanly separated:**

| Layer | Stores | Technology | Role |
|---|---|---|---|
| **S2** | All message content | One stream per conversation | Ordering, durability, replay, real-time tailing |
| **PostgreSQL (Neon)** | All metadata | Agents, conversations, members, cursors | Identity, membership, read positions |

**Transport:**

| Direction | Transport | When |
|---|---|---|
| **Writes (complete)** | `HTTP POST` | Single request-response for full messages |
| **Writes (streaming)** | `NDJSON streaming POST` | Token-by-token over a single persistent HTTP connection |
| **Reads (real-time)** | `SSE` | Tail a conversation, replay from cursor |
| **Reads (history)** | `HTTP GET` | Reconstructed complete messages, pagination |

**Language:** Go. First-class S2 SDK. Goroutines handle thousands of concurrent SSE connections. Single binary deployment.

---

## Component Map

Every component has a dedicated design document with exhaustive implementation detail. This section provides the architectural summary and cross-reference for each.

### 1. Agent Registry

Agents are opaque identities — no metadata, no auth. `POST /agents` generates a UUIDv7, inserts into PostgreSQL, returns the ID. The agent ID is the sole credential. Agents are permanent (no deletion). Agent existence is validated on every subsequent API call via middleware with an in-process `sync.Map` cache for zero-contention reads.

→ Agent validation caching: [`http-api-layer-plan.md`](http-api-layer-plan.md) §5
→ Schema & queries: [`sql-metadata-plan.md`](sql-metadata-plan.md) §5, §12

### 2. Conversation Management

Create, list, invite, leave — all metadata operations in PostgreSQL. S2 streams auto-create on first append via `CreateStreamOnAppend: true`. System events (`agent_joined`, `agent_left`) are written to the S2 stream so membership changes appear in conversation history.

**Key behaviors:**
- Creator is auto-member. Same agent set can have multiple conversations.
- Invite is idempotent for existing members (`ON CONFLICT DO NOTHING`), error for nonexistent agents.
- Leave rejects if last member (409). Hard-deletes membership row, preserves both cursors (`delivery_seq` + `ack_seq`) for re-invite resume.
- Leave terminates active SSE and streaming write connections for the departing agent, writes `message_abort` for any in-progress message before writing `agent_left`.

**Concurrency:** `SELECT ... FOR UPDATE` serializes concurrent leave operations per conversation. Five race conditions analyzed and resolved.

→ Locking & race conditions: [`sql-metadata-plan.md`](sql-metadata-plan.md) §9
→ Connection registry for leave termination: [`sql-metadata-plan.md`](sql-metadata-plan.md) §10
→ Wire format & error codes: [`http-api-layer-plan.md`](http-api-layer-plan.md) §3

### 3. Event Model

Six event types on the S2 stream: `message_start`, `message_append`, `message_end`, `message_abort`, `agent_joined`, `agent_left`. S2 record headers carry the event type (dispatch without JSON parsing), body carries the JSON payload. Timestamps are S2-assigned (arrival mode).

**Two fundamental Go types:**
- `Event` — produced by write paths (type + body). Serialized to S2 records in the store wrapper.
- `SequencedEvent` — consumed by read paths (seq num + timestamp + type + raw JSON). NOT pre-parsed on the SSE hot path — raw pass-through. Parsed on demand by history handler and Claude agent.

→ Complete type system, constructors, serializers: [`event-model-plan.md`](event-model-plan.md)

### 4. Write Path

Two write patterns, both writing to the same S2 stream:

**Complete message send** (`POST /conversations/{cid}/messages`): Server writes `[message_start, message_append, message_end]` as a single S2 batch append — atomic, all-or-nothing. Single round-trip.

**Streaming message send** (`POST /conversations/{cid}/messages/stream`): Agent opens a single HTTP POST with an NDJSON streaming body. Server reads `{"content":"..."}` lines via `bufio.NewScanner(r.Body)`, appending each to S2 in real-time via pipelined AppendSession. Message lifecycle is implicit in the HTTP request lifecycle — start = request received, chunks = NDJSON lines, end = body closes, abort = connection drops.

**Why NDJSON streaming POST** (not WebSocket, not per-token HTTP POST): HTTP-native (every proxy understands it), agent-friendly (12 lines of sync Python), decoupled from read path (independent failure domains), stateless for horizontal scaling, standard middleware applies unchanged. WebSocket and per-token POST were evaluated and rejected with full rationale.

→ Transport decision analysis: [`spec.md`](spec.md) Phase 2, Challenge 3
→ S2 append strategies (Unary vs AppendSession): [`s2-architecture-plan.md`](s2-architecture-plan.md) §5
→ Streaming handler timeout architecture: [`http-api-layer-plan.md`](http-api-layer-plan.md) §4

### 5. Read Path

**Consumption model (active / passive / wake-up).** Agents consume a conversation in one of three modes, determined entirely by the transport they open — the server holds no per-agent mode state. Active = open SSE tail. Passive = member with no tail (messages accumulate on S2, agent is not interrupted). Wake-Up = client-initiated pull via `GET /agents/me/unread` to learn which conversations have unread material, followed by `GET /conversations/{cid}/messages?from=<ack_seq>` to catch up on assembled messages and `POST /conversations/{cid}/ack` to advance the ack cursor. See [`spec.md`](spec.md) §1.4.

**SSE (real-time)** (`GET /conversations/{cid}/stream`): S2 read session → SSE event translation. Catch-up from `delivery_seq`, then seamless transition to real-time tailing. SSE `id` = S2 sequence number (auto-resume via `Last-Event-ID` on reconnect). One active SSE connection per (agent, conversation) — new connection replaces old.

**History (canonical passive catch-up)** (`GET /conversations/{cid}/messages`): Reconstructs complete messages from the raw event stream. Groups events by `message_id`, assembles content, returns structured messages with `status` (complete / in_progress / aborted). Two cursor modes: `before` (DESC pagination) and `from` (ASC, ack-aligned catch-up) — mutually exclusive. Assembled messages only — no raw-events alternative.

**Concurrent message interleaving:** When two agents stream simultaneously, their records interleave on the S2 stream. Readers demultiplex by `message_id`. The interleaving IS the total order — it represents the temporal reality of concurrent composition.

→ SSE enrichment & timestamp injection: [`event-model-plan.md`](event-model-plan.md) §7
→ ReadSession management: [`s2-architecture-plan.md`](s2-architecture-plan.md) §6
→ History endpoint contract: [`http-api-layer-plan.md`](http-api-layer-plan.md) §3

### 6. Read Cursor Management

**Two cursors per (agent, conversation):** `delivery_seq` (server-advanced, two-tier — in-memory hot tier updated on every SSE event delivery, batched flush to PostgreSQL every 5 seconds via `unnest()` batch upsert, immediate flush on disconnect/leave/shutdown) and `ack_seq` (client-advanced only by `POST /conversations/{cid}/ack`, single-tier synchronous write-through — no hot tier, no batching). Both cursors live in one `cursors` row.

**Delivery semantics:** Tail delivery is at-least-once — on crash, agent may re-receive up to ~5 seconds of events, deduplicated client-side by sequence number. `ack_seq` is exactly-what-you-acked (synchronous, regression-guarded). Both cursors survive leave — on re-invite, agent resumes tailing from `delivery_seq` and gets an accurate unread count from `ack_seq`.

→ Cursor architecture & flush mechanics: [`sql-metadata-plan.md`](sql-metadata-plan.md) §7

### 7. S2 Stream Storage

S2 is the entire message storage and delivery layer. One stream per conversation. Express storage class (40ms append ack — non-negotiable for real-time streaming). Single basin `agentmail` in `us-east-1`. Streams auto-create on first append.

**Key operations:**
- **Unary append:** System events, complete messages. Single batch, atomic.
- **AppendSession (pipelined):** Streaming token writes. Non-blocking submits — submit token N while N-1's ack is in-flight. Mandatory at LLM token rates (30-100/sec) where Unary's 40ms ack caps at 25 sequential appends/sec.
- **ReadSession (tailing):** One per SSE connection. Catch-up → real-time, indefinite tailing.
- **Read range:** History endpoint, recovery sweep. Bounded batch, no tailing.

**Crash recovery:** `in_progress_messages` PostgreSQL table tracks active streaming writes. On server restart, recovery sweep writes `message_abort` for each row. No S2 stream scanning.

→ Full architecture: [`s2-architecture-plan.md`](s2-architecture-plan.md)

### 8. PostgreSQL Metadata (Neon)

Five tables: `agents`, `conversations` (with cached `head_seq` updated inline on every S2 append, powers the unread query), `members`, `cursors` (two columns per row — `delivery_seq` + `ack_seq`), `in_progress_messages`. pgx v5 native interface + sqlc code generation. UUIDv7 primary keys. pgxpool with 15 connections (~50K queries/sec throughput on indexed point lookups).

**Caching layers:**
- Agent existence: `sync.Map` (write-once, read-many, ~50ns reads, no eviction needed)
- Membership: LRU cache (100K entries, 60s TTL, synchronous invalidation on invite/leave)
- Delivery cursor: In-memory hot tier with batched Postgres flush (every 5s). `ack_seq` is NOT cached — synchronous write-through for correctness.

**Hosting:** Neon serverless PostgreSQL (free tier, `us-east-1`). Direct TCP connection from Go server on Fly.io (`iad`). ~1-5ms latency.

→ Full design: [`sql-metadata-plan.md`](sql-metadata-plan.md)

### 9. HTTP API Layer

Standard JSON error envelope on every error. Machine-readable `code` for AI agent branching, human-readable `message` for debugging. Error codes are a stable API contract.

**Middleware chain:** Recovery → Request ID → Structured Logging → (Agent Auth) → (Timeout) → Handler. Three route groups: unauthenticated, authenticated + 30s timeout, authenticated + streaming (no timeout, handler-managed idle detection).

**Route table:**

| Method | Path | Auth | Timeout | Body In | Body Out |
|---|---|---|---|---|---|
| POST | `/agents` | No | 30s | — | JSON |
| GET | `/agents/resident` | No | 30s | — | JSON |
| GET | `/health` | No | 5s | — | JSON |
| POST | `/conversations` | Yes | 30s | — | JSON |
| GET | `/conversations` | Yes | 30s | — | JSON |
| POST | `/conversations/{cid}/invite` | Yes | 30s | JSON | JSON |
| POST | `/conversations/{cid}/leave` | Yes | 30s | — | JSON |
| POST | `/conversations/{cid}/messages` | Yes | 30s | JSON | JSON |
| GET | `/conversations/{cid}/messages` | Yes | 30s | — | JSON |
| POST | `/conversations/{cid}/ack` | Yes | 30s | JSON | — |
| GET | `/agents/me/unread` | Yes | 30s | — | JSON |
| POST | `/conversations/{cid}/messages/stream` | Yes | — | NDJSON | JSON |
| GET | `/conversations/{cid}/stream` | Yes | — | — | SSE |

→ Full design (errors, middleware, contracts, timeouts): [`http-api-layer-plan.md`](http-api-layer-plan.md)

### 10. Claude-Powered Resident Agent

Internal goroutine within the Go server process. Calls store/S2 layers directly — not via HTTP self-calls. Demonstrates the system working end-to-end.

**Identity:** Stable UUID via `RESIDENT_AGENT_ID` env var, idempotent registration on startup. Discoverable at `GET /agents/resident`.

**Conversation handling:** Go channel for real-time invite notifications. One listener goroutine per conversation (S2 ReadSession, not SSE). On `message_end` from another agent → triggers Claude API call → streams response token-by-token via S2 AppendSession.

**Concurrency:** Per-conversation channel semaphore (sequential responses within a conversation). Global Claude semaphore (capacity 5, bounds API load). History: lazy-seeded sliding window (last 50 messages).

**Error handling:** Claude API failures produce a visible error message in the conversation. 429 → exponential backoff with `Retry-After`. Mid-stream failures write `message_abort`. In-progress tracking via Postgres for crash recovery.

→ Full design: [`claude-agent-plan.md`](claude-agent-plan.md)

### 11. Server Lifecycle

`cmd/server/main.go` — entry point, configuration, startup sequence, graceful shutdown.

**Configuration:** Environment variables only. 6 required (`DATABASE_URL`, `S2_AUTH_TOKEN`, etc.), 4 optional with sensible defaults. Fail-fast on missing required vars.

**Startup:** Strict dependency order — Postgres (migration + cache warming) → S2 (client + recovery sweep) → cursor flush goroutine → Claude agent → HTTP server. Each step has explicit failure handling.

**Shutdown:** SIGINT/SIGTERM → stop accepting → drain CRUD (30s) → shut down Claude agent → cancel SSE/streaming → flush cursors → close S2 → close Postgres → exit. Hard deadline: 30 seconds.

→ Full design: [`server-lifecycle-plan.md`](server-lifecycle-plan.md)

### 12. Deployment

Fly.io single instance (`shared-cpu-1x`, 512 MB RAM, `iad` region). Multi-stage Docker build (Go builder → Alpine runtime, ~15 MB image). Secrets via `fly secrets set`. Health check: `GET /health` (Postgres ping + S2 connectivity).

→ Full design: [`deployment-plan.md`](deployment-plan.md)

---

## Design Decisions Summary

### Transport Choices

| Decision | Choice | Rejected Alternatives | Key Rationale |
|---|---|---|---|
| Streaming writes | NDJSON streaming POST | WebSocket, per-token HTTP POST | HTTP-native, agent-friendly, stateless, decoupled from reads |
| Streaming reads | SSE | Polling, WebSocket | Maps perfectly to S2 tailing, auto-reconnect via `Last-Event-ID` |
| History reads | HTTP GET with pagination | — | Polling-friendly fallback, reconstructed complete messages |

→ Full transport analysis: [`spec.md`](spec.md) Phase 2, Challenges 3-4

### Stream Topology

One S2 stream per conversation. Per-agent inbox model was evaluated and rejected: write amplification (1 message → N writes), no shared ordering, conversation reconstruction requires merge-sort.

### Storage Split

S2 for messages, PostgreSQL for metadata. Pure event-sourcing (derive everything from S2 streams) was evaluated and rejected: makes every membership check require stream replay or in-memory materialized view. PostgreSQL as a materialized view of conversation metadata is simpler and correct.

### No Service Layer

API handlers ARE the orchestration. The store handles data access, S2 handles stream operations, handlers compose them. An intermediate service layer would add indirection for zero benefit at this system's complexity.

---

## Assumptions Examined

| # | Assumption | Type | Position |
|---|---|---|---|
| 1 | S2 is the right storage primitive | Recommended | Agree — purpose-built for this problem |
| 2 | Conversations need total ordering | Implicit | Agree within conversation scope. Cross-conversation ordering is irrelevant. |
| 3 | Server mediates all access | Explicit | Agree — direct S2 access bypasses membership checks |
| 4 | No authentication needed | Explicit | Agree for scope — agent IDs as bearer tokens |
| 5 | Plaintext only | Explicit | Accept — don't build what isn't asked for |
| 6 | AI coding agents are primary clients | Explicit | **Most important.** HTTP-native transports, curl-friendly APIs, self-describing JSON. |
| 7 | Real-time streaming matters | Explicit | Agree — tokens as they arrive is the whole point |
| 8 | Unbounded participants | Implicit | Agree for API. >100 members would cause practical issues. |
| 9 | Messages are small | Implicit | Agree — LLM outputs are text, few KB at most |
| 10 | Single server instance | Implicit | Accept for take-home. Design doesn't preclude horizontal scaling. |

---

## Implementation Roadmap

| Step | Component | Est. Time | Key Actions |
|---|---|---|---|
| 1 | Project scaffolding | 30 min | `go mod init`, chi router, Neon connection, schema migration, health endpoint, Makefile |
| 2 | S2 client wrapper | 1 hr | Provision account, create basin, implement 5 operations, integration test |
| 3 | Agent registry | 30 min | `POST /agents`, sqlc CRUD, agent auth middleware |
| 4 | Conversation management | 1.5 hr | Create, list, invite, leave. Membership checks, system events to S2 |
| 5 | Complete message send | 1 hr | `POST /messages`, batch S2 write (3 records atomic) |
| 6 | SSE read stream | 2 hr | S2 read session → SSE, cursor management, connection registry |
| 7 | Streaming write (NDJSON) | 1.5 hr | `POST /messages/stream`, scanner loop, abort on disconnect, recovery sweep |
| 8 | Claude agent | 2 hr | Registration, SSE listeners, Claude API streaming, token forwarding |
| 9 | Testing | 2 hr | Integration suite, concurrent writers, disconnect/reconnect |
| 10 | Documentation | 1.5 hr | `DESIGN.md`, `FUTURE.md`, `CLIENT.md` |
| 11 | Deployment | 1 hr | Dockerfile, fly.toml, deploy, verify |

**Total estimated: ~14.5 hours**

---

## Project Structure

```text
agentmail-take-home/
├── cmd/server/main.go              → server-lifecycle-plan.md
├── internal/
│   ├── api/
│   │   ├── router.go               → http-api-layer-plan.md
│   │   ├── middleware.go            → http-api-layer-plan.md §2
│   │   ├── errors.go               → http-api-layer-plan.md §1
│   │   ├── helpers.go               → http-api-layer-plan.md §3
│   │   ├── types.go                 → http-api-layer-plan.md §3
│   │   ├── agents.go
│   │   ├── conversations.go
│   │   ├── messages.go
│   │   ├── sse.go
│   │   ├── history.go
│   │   └── health.go
│   ├── store/
│   │   ├── schema/schema.sql        → sql-metadata-plan.md §5
│   │   ├── queries/*.sql            → sql-metadata-plan.md §12
│   │   ├── db/                      (sqlc generated)
│   │   ├── postgres.go              → sql-metadata-plan.md
│   │   ├── agent_cache.go           → http-api-layer-plan.md §5
│   │   ├── cursor_cache.go          → sql-metadata-plan.md §7
│   │   ├── membership_cache.go      → sql-metadata-plan.md §8
│   │   ├── conn_registry.go         → sql-metadata-plan.md §10
│   │   ├── s2.go                    → s2-architecture-plan.md
│   │   └── s2_test.go
│   ├── model/
│   │   ├── events.go                → event-model-plan.md
│   │   └── types.go
│   └── agent/
│       ├── agent.go                 → claude-agent-plan.md
│       ├── listener.go              → claude-agent-plan.md §4
│       ├── respond.go               → claude-agent-plan.md §5
│       ├── history.go               → claude-agent-plan.md §6
│       └── discovery.go             → claude-agent-plan.md §3
├── tests/
│   ├── integration_test.go
│   ├── streaming_test.go
│   └── concurrent_test.go
├── docs/
│   ├── DESIGN.md
│   ├── FUTURE.md
│   └── CLIENT.md
├── go.mod
├── go.sum
├── Dockerfile                       → deployment-plan.md §1
├── fly.toml                         → deployment-plan.md §2
├── Makefile
├── sqlc.yaml
└── README.md                        (immutable project spec)
```

---

## Key Dependencies

| Package | Purpose |
|---|---|
| `github.com/go-chi/chi/v5` | HTTP router |
| `github.com/s2-streamstore/s2-sdk-go` | S2 stream client |
| `github.com/anthropics/anthropic-sdk-go` | Claude API |
| `github.com/google/uuid` (v1.6.0+) | UUIDv7 generation (RFC 9562) |
| `github.com/jackc/pgx/v5` | PostgreSQL driver (native + pgxpool) |
| `sqlc` (build-time CLI) | Type-safe SQL code generation |
| `github.com/rs/zerolog` | Structured logging |

---

## Blast Radius & Reversibility

| Decision | Worst Case if Wrong | Reversibility |
|---|---|---|
| **Go** | Slower to prototype than Python | Low — full rewrite. But Go is the right call for this domain. |
| **S2** | Outage or API breaking change | Medium — swap via `internal/store/s2.go` wrapper. |
| **PostgreSQL (Neon)** | Neon outage or cold-start latency | Medium — standard PostgreSQL, portable to any host. |
| **SSE for reads** | Client doesn't support SSE | High — history polling endpoint exists as fallback. |
| **NDJSON POST for writes** | Proxy buffers the body | High — complete message POST works universally as fallback. |
| **One stream per conversation** | Hot conversation with many writers | Low — S2 handles 100 MiBps per stream. LLM rates won't come close. |
| **Event-based record model** | Complexity in demultiplexing | Medium — could simplify to complete-message-only, but lose streaming. |

---

## Design Document Index

| Document | Scope | Lines |
|---|---|---|
| [`spec.md`](spec.md) | Original exhaustive design (transport analysis, assumption interrogation, detailed component breakdowns) | ~1600 |
| [`s2-architecture-plan.md`](s2-architecture-plan.md) | S2 basin config, append strategies, read sessions, error handling, recovery, scaling | ~950 |
| [`sql-metadata-plan.md`](sql-metadata-plan.md) | PostgreSQL schema, pgx/sqlc, cursor two-tier, caching, locking, connection registry | ~1020 |
| [`event-model-plan.md`](event-model-plan.md) | Go type system for events — constants, payloads, constructors, serializers, SSE enrichment | ~1440 |
| [`http-api-layer-plan.md`](http-api-layer-plan.md) | Error format, middleware chain, request/response contracts, timeouts, agent validation | ~1770 |
| [`claude-agent-plan.md`](claude-agent-plan.md) | Resident agent identity, discovery, Claude API integration, concurrency, error handling | ~1670 |
| [`server-lifecycle-plan.md`](server-lifecycle-plan.md) | `main.go` startup/shutdown, configuration, health checks | ~1550 |
| [`deployment-plan.md`](deployment-plan.md) | Dockerfile, fly.toml, secrets, Fly.io machine config | ~1240 |
| [`gaps.md`](gaps.md) | Design audit — what's exhaustively designed vs. sketched, gap analysis | ~270 |

---

## Verification Plan

1. **Local smoke test:** Register two agents, create conversation, send messages, verify SSE delivery.
2. **Streaming test:** Agent A streams write, verify agent B's SSE receives chunks in real-time.
3. **Concurrent test:** Two agents stream simultaneously, verify both messages reconstructable from interleaved events.
4. **Reconnect test:** Disconnect B's SSE, send messages from A, reconnect B, verify cursor-based catch-up.
5. **Leave test:** Agent B leaves, verify SSE closes, verify B can't read/write.
6. **Claude agent test:** Invite Claude agent, send message, verify streaming response.
7. **Integration suite:** `go test ./tests/... -v` — all scenarios pass.
8. **Deployed test:** Hit live Fly.io URL, register agent, converse with Claude agent.
