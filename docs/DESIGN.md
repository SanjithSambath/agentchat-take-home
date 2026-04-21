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

*Agent topology.* The `Agent (LLM)` box above spans two implementations: an **in-process resident** (§10) that calls the store/S2 layers directly, and an **external Claude Code persona** (§10b) that speaks and listens entirely via this HTTP/SSE surface. Both produce indistinguishable events on the S2 stream; peers cannot tell them apart. The read-only **observer lane** (§13) serves UI clients without granting them agent identity.

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
- `GET /conversations/{cid}` returns metadata + member list for a single conversation. Membership-gated (403 `not_member` for non-members — prevents membership snooping). Primary consumer: `run_agent.py` daemons verifying `--target join:<cid>` before opening an SSE tail.

**Concurrency:** `SELECT ... FOR UPDATE` serializes concurrent leave operations per conversation. Five race conditions analyzed and resolved.

→ Locking & race conditions: [`sql-metadata-plan.md`](sql-metadata-plan.md) §9
→ Connection registry for leave termination: [`sql-metadata-plan.md`](sql-metadata-plan.md) §10
→ Wire format & error codes: [`http-api-layer-plan.md`](http-api-layer-plan.md) §3

### 3. Event Model

Six event types on the S2 stream: `message_start`, `message_append`, `message_end`, `message_abort`, `agent_joined`, `agent_left`. S2 record headers carry the event type (dispatch without JSON parsing), body carries the JSON payload. Timestamps are S2-assigned (arrival mode).

**Two fundamental Go types:**
- `Event` — produced by write paths (type + body). Serialized to S2 records in the store wrapper.
- `SequencedEvent` — consumed by read paths (seq num + timestamp + type + raw JSON). NOT pre-parsed on the SSE hot path — raw pass-through. Parsed on demand by history handler and Claude agent.

**No event-level dedup.** Readers demultiplex by `message_id` on the stream rather than tracking `(message_id, seq_num)` tuples. HTTP-level idempotency (`messages_dedup`, §8) closes the outer loop; S2 SDK retries carry the same `message_id` and are absorbed transparently by demux. Event-level tracking would be belt-on-belt.

→ Complete type system, constructors, serializers: [`event-model-plan.md`](event-model-plan.md)

### 4. Write Path

Two write patterns, both writing to the same S2 stream. Both require the client to supply `message_id` (UUIDv7) as the idempotency key — for the complete POST it is a required JSON field; for the streaming POST it is declared on the mandatory first NDJSON line. Retries with the same `message_id` either replay the cached result (`already_processed: true`) or receive `409 already_aborted` so the client generates a fresh id.

**Complete message send** (`POST /conversations/{cid}/messages`): Body carries `{"message_id", "content"}`. Server runs the dedup gate (see `messages_dedup` + UNIQUE on `in_progress_messages` in §8), then writes `[message_start, message_append, message_end]` as a single S2 batch append — atomic, all-or-nothing. Single round-trip.

**Streaming message send** (`POST /conversations/{cid}/messages/stream`): Agent opens a single HTTP POST with an NDJSON streaming body. Server reads the first line `{"message_id":"..."}` to run the dedup gate, then reads `{"content":"..."}` lines via `bufio.NewScanner(r.Body)` with an explicit 1 MiB + 1 KiB line-buffer cap (see [`http-api-layer-plan.md`](http-api-layer-plan.md) §6.1), appending each to S2 in real-time via pipelined AppendSession. Every `Submit` is wrapped with a 2 s `context.WithTimeout` so a stalled S2 path cannot pin the handler goroutine indefinitely — on timeout, the handler enters the shared `abort` path (Unary-append `message_abort`, record `messages_dedup` aborted, delete `in_progress_messages`, return `503 slow_writer`). Message lifecycle is implicit in the HTTP request lifecycle — start = dedup gate passed and `message_start` appended, chunks = NDJSON lines, end = body closes, abort = connection drops, leave, or Submit timeout.

**Why NDJSON streaming POST** (not WebSocket, not per-token HTTP POST): HTTP-native (every proxy understands it), agent-friendly (12 lines of sync Python), decoupled from read path (independent failure domains), stateless for horizontal scaling, standard middleware applies unchanged. WebSocket and per-token POST were evaluated and rejected with full rationale.

**Unary vs AppendSession — one optimization per write shape.** Complete sends use a single **Unary** batch append (three events atomic, all-or-nothing). Streaming sends use pipelined **AppendSession** (non-blocking submits; submit token N while N-1's ack is still in-flight — mandatory at LLM token rates where Unary's 40 ms ack caps at ~25 sequential appends/sec). Using AppendSession for complete sends was rejected as premature generalization: session lifecycle + inflight queue added complexity with zero latency benefit at one round-trip.

**Streaming timeout ladder.** A long-lived NDJSON POST has four bounded deadlines, each targeting a distinct failure mode: (1) **5 s handshake** — the first line `{"message_id"}` must arrive within 5 s or the handler exits before touching the dedup gate; (2) **2 s per-`Submit`** — every AppendSession submit carries its own `context.WithTimeout` so a stalled S2 append cannot pin the handler goroutine (timeout → `503 slow_writer`, abort path); (3) **5 min idle** — inter-line gap cap between content chunks; (4) **1 hr absolute** — hard ceiling regardless of activity. The AppendSession in-flight queue is capped at **256** submits (~5 s of 50 tok/s) — a saturated queue plus slow S2 surfaces as `slow_writer` instead of silent memory growth.

→ Transport decision analysis: [`spec.md`](spec.md) Phase 2, Challenge 3
→ S2 append strategies (Unary vs AppendSession): [`s2-architecture-plan.md`](s2-architecture-plan.md) §5
→ Streaming handler timeout architecture: [`http-api-layer-plan.md`](http-api-layer-plan.md) §4

### 5. Read Path

**Consumption model (active / passive / wake-up).** Agents consume a conversation in one of three modes, determined entirely by the transport they open — the server holds no per-agent mode state. Active = open SSE tail. Passive = member with no tail (messages accumulate on S2, agent is not interrupted). Wake-Up = client-initiated pull via `GET /agents/me/unread` to learn which conversations have unread material, followed by `GET /conversations/{cid}/messages?from=<ack_seq>` to catch up on assembled messages and `POST /conversations/{cid}/ack` to advance the ack cursor. See [`spec.md`](spec.md) §1.4.

**SSE (real-time)** (`GET /conversations/{cid}/stream`): S2 read session → SSE event translation. Catch-up from `delivery_seq`, then seamless transition to real-time tailing. SSE `id` = S2 sequence number; `Last-Event-ID` on reconnect is the authoritative client-confirmed resume point and is the **only** signal that advances the durable `delivery_seq` cursor during a live stream (see §6). One active SSE connection per (agent, conversation) — a new connection cancels the prior tail via the connection registry and waits briefly for orderly shutdown (last-writer-wins, bounded wait). The tailer goroutine hands events to the writer over a buffered channel (cap **64**) with a **500 ms push deadline** — if the socket writer can't accept within that window, the handler assumes TCP backpressure is starving the connection and tears it down rather than accumulate memory.

**History (canonical passive catch-up)** (`GET /conversations/{cid}/messages`): Reconstructs complete messages from the raw event stream. Groups events by `message_id`, assembles content, returns structured messages with `status` (complete / in_progress / aborted). Two cursor modes: `before` (DESC pagination) and `from` (ASC, ack-aligned catch-up) — mutually exclusive. Assembled messages only — no raw-events alternative.

**Observer read lane** (`GET /observer/conversations/{cid}/stream` + `/messages`): same S2 read primitives, no auth, no membership gate, no cursor persistence — see §13.

**Concurrent message interleaving:** When two agents stream simultaneously, their records interleave on the S2 stream. Readers demultiplex by `message_id`. The interleaving IS the total order — it represents the temporal reality of concurrent composition.

→ SSE enrichment & timestamp injection: [`event-model-plan.md`](event-model-plan.md) §7
→ ReadSession management: [`s2-architecture-plan.md`](s2-architecture-plan.md) §6
→ History endpoint contract: [`http-api-layer-plan.md`](http-api-layer-plan.md) §3

### 6. Read Cursor Management

Every `(agent, conversation)` pair owns one row in the `cursors` table. That row holds **two cursors** side-by-side, each answering a different question.

| Cursor | Question it answers | Who advances it | How |
|---|---|---|---|
| `delivery_seq` | "From what sequence should the server resume streaming this agent on reconnect?" | **Server, on client-confirmed signals only** | SSE: advanced on `Last-Event-ID` reconnect (SSE-spec confirmation) or explicit `POST /ack`. In-process resident: advanced inline per event (direct S2 consumption, no proxy between). |
| `ack_seq` | "What's the highest S2 sequence number the agent has *confirmed reading*?" | **Client** | Only advances when the agent calls `POST /conversations/{cid}/ack {"seq": N}`. |

**Why two cursors, not one?** "The server has a confirmed resume point for this agent" and "the LLM actually processed this" are different facts. `delivery_seq` is the server-owned resume hint — advanced on SSE-spec signals (`Last-Event-ID`) and `POST /ack`, used as the `ReadSession` start on reconnect. `ack_seq` is the agent-owned work cursor — advanced only by the explicit `POST /ack`, used to compute `unread = head_seq - ack_seq`. Collapsing them into one would either (a) make every resume require an explicit ack from the client (chatty), or (b) conflate "server believes this was delivered" with "agent finished processing this," which are not the same claim. Two cursors cost one extra `int8` column and buy both properties cleanly.

**`delivery_seq` — the resume-point cursor (two-tier, confirmation-gated for external clients).** The storage shape is the same two-tier design on both write paths; what differs is the advancement rule.

- **Tier 1 (hot, RAM):** an in-memory `map[(agent_id, conv_id)] → seq`, flushed every 5 s plus immediate flushes on SSE disconnect, `POST /leave`, and graceful shutdown (SIGINT/SIGTERM).
- **Tier 2 (durable, Postgres):** a background goroutine flushes the dirty subset as a single `unnest()`-batched `UPSERT` — one query covers thousands of dirty cursors atomically.
- **Advancement rule — external SSE clients (phantom-advance fix).** The cursor is **never** advanced inside the per-event `rc.Flush()` loop. `Flush()` only guarantees bytes reached the kernel send buffer — through a buffering proxy (ngrok, nginx with default `proxy_buffering`) the client may see nothing, disconnect, and reconnect to a cursor already past unread events. External clients advance the cursor on exactly two confirmed signals: (1) `Last-Event-ID: N` on reconnect → store `N+1` (the SSE-spec client confirmation), (2) `POST /conversations/{cid}/ack {"seq": N}` → store `GREATEST(delivery_seq, N)`.
- **Advancement rule — in-process resident agent.** Consumes S2 via direct `ReadSession`; no HTTP transport, no buffering proxy, no ambiguity. Advances `delivery_seq` inline per event in the tailer loop.
- **Consequence — tail delivery is at-least-once, bounded by what the client confirms.** Re-delivery is scoped to the window between the last client confirmation (`Last-Event-ID` or ack) and the disconnect point, not to flush cadence. Clients dedupe on the monotonic S2 sequence number carried in the SSE `id:` field; `Last-Event-ID` on reconnect resumes from exactly the right place.

**`ack_seq` — the correctness-path cursor (single-tier, synchronous).**
- The client calls `POST /conversations/{cid}/ack {"seq": N}` after finishing its work through sequence `N`.
- Written **synchronously to Postgres before the API returns** — no hot tier, no batching.
- **Regression-guarded at the SQL layer:** UPSERT with `WHERE cursors.ack_seq < EXCLUDED.ack_seq`. Out-of-order retries, duplicate acks, or a stale client can never move the cursor backward.
- **Why synchronous?** `GET /agents/me/unread` computes `conversations.head_seq - ack_seq` per membership. Any staleness here is an observable wrong unread count — a correctness bug, not a latency bug.

**SQL-level monotonic guards are applied consistently across every cursor-like counter.** `delivery_seq` and `ack_seq` upserts both use `WHERE cursors.<col> < EXCLUDED.<col>`; `conversations.head_seq` advances via `UPDATE … WHERE head_seq < $2`. Out-of-order writes, concurrent retries, and batched flushes can never rewind a counter — the database is the monotonicity authority, not the caller. Callers can retry freely without ordering concerns.

**How the two cursors interact.**
- **Reconnect after disconnect:** agent reopens `GET /conversations/{cid}/stream` with `Last-Event-ID: N` → server advances `delivery_seq` to `N+1` inline and opens the S2 `ReadSession` from there. Catch-up replay transitions seamlessly into real-time tailing. Any events between the last confirmed id and the disconnect point are re-delivered; clients dedupe by the monotonic S2 sequence number carried in the SSE `id:` field.
- **Leave + re-invite:** the `members` row is hard-deleted on leave, but the `cursors` row is **preserved**. A re-invited agent resumes tailing at `delivery_seq` and sees an accurate unread count from `ack_seq` — zero manual recovery.
- **Unread count (passive / wake-up mode):** purely `conversations.head_seq - ack_seq` — one indexed point lookup per conversation the agent is a member of. The agent doesn't need an active SSE connection to know whether new material exists, which is what enables the passive and wake-up consumption modes in §5.

→ Cursor architecture & flush mechanics: [`sql-metadata-plan.md`](sql-metadata-plan.md) §7

### 7. S2 Stream Storage

S2 is the entire message storage and delivery layer. One stream per conversation. Express storage class (40ms append ack — non-negotiable for real-time streaming). Single basin `agentmail` in `us-east-1`. Streams auto-create on first append.

**Key operations:**
- **Unary append:** System events, complete messages. Single batch, atomic.
- **AppendSession (pipelined):** Streaming token writes. Non-blocking submits — submit token N while N-1's ack is in-flight. Mandatory at LLM token rates (30-100/sec) where Unary's 40ms ack caps at 25 sequential appends/sec.
- **ReadSession (tailing):** One per SSE connection. Catch-up → real-time, indefinite tailing.
- **Read range:** History endpoint, recovery sweep. Bounded batch, no tailing.

**Three dedup layers, each tuned for its own failure mode.** (1) **HTTP idempotency** — `messages_dedup` keyed on `(conversation_id, message_id)` caches the terminal outcome per client send; a `complete` row replays cached `start_seq`/`end_seq` on retry, an `aborted` row returns `409 already_aborted` so the client generates a fresh id. (2) **In-flight tracking** — `in_progress_messages` (UNIQUE on the same key) is the crash-recovery anchor: on startup, the recovery sweep per row writes `message_abort(server_crash)` to S2, inserts an `aborted` dedup row (`ON CONFLICT DO NOTHING`), and deletes the claim — each Postgres step wrapped in short exponential-backoff retry (100 ms → 300 ms → 900 ms, 3 attempts) so a transient blip doesn't orphan rows for the next restart. No S2 stream scanning. (3) **Reader demux** — readers group events by `message_id` on the stream, which transparently absorbs S2 SDK-level retry duplicates. Each layer owns a disjoint failure mode; stacking them is what makes the write path idempotent end-to-end.

→ Full architecture: [`s2-architecture-plan.md`](s2-architecture-plan.md)

### 8. PostgreSQL Metadata (Neon)

Postgres is the **metadata plane**; S2 is the **content plane**. The split is intentional: a materialized view of identity, membership, and read position in a queryable relational store beats deriving those from stream replay (which would make every membership check O(stream)). pgx v5 (native, no database/sql) + sqlc code generation. UUIDv7 primary keys everywhere so insertion order is preserved without a sequence. pgxpool with 15 connections; indexed point lookups clear ~50K queries/sec.

**Six tables, one purpose each:**

| Table | Row shape | Why it exists — why Postgres (not S2) |
|---|---|---|
| `agents` | `(id UUIDv7, created_at)` | Identity plane. Agent IDs are bearer credentials; validating them on every request requires an O(1) point lookup, not stream replay. Permanent (no deletion). |
| `conversations` | `(id, s2_stream_name, head_seq, created_at)` | Conversation identity + the **cached S2 tail sequence**. `head_seq` is advanced inline on every successful append (regression-guarded) so `GET /agents/me/unread = head_seq - ack_seq` costs one indexed point lookup — no S2 round-trip on the read hot path. |
| `members` | `(conversation_id, agent_id, joined_at)` | Membership gate. Hard-deleted on leave; authoritative membership *history* still lives in the S2 event log (`agent_joined` / `agent_left`). **No foreign keys** — FK checks add overhead on the hot path; the API layer validates existence before writes. |
| `cursors` | `(agent_id, conv_id, delivery_seq, ack_seq, updated_at)` | Read positions (§6). Two cursors per pair. `CHECK (seq ≥ 0)`. Preserved across leave so re-invite resumes without manual recovery. |
| `in_progress_messages` | `(message_id PK, conv_id, agent_id, s2_stream, started_at)` + UNIQUE `(conv_id, message_id)` | In-flight idempotency gate. INSERT-on-start + DELETE-on-end defines a claim window. A second streaming POST with the same `message_id` fails the INSERT and replays the prior outcome. Also the crash-recovery anchor (see §7). |
| `messages_dedup` | `(conv_id, message_id, status, start_seq, end_seq, created_at)` | Terminal-outcome cache per client send. `complete` rows carry `start_seq`/`end_seq` so retries return `already_processed=true` without replaying S2; `aborted` rows return `409 already_aborted` forcing a fresh id. |

**Caching layers — each matched to its access pattern:**

| Cache | What it stores | Why this shape |
|---|---|---|
| **Agent existence** — `sync.Map` | `uuid → struct{}` | Populated at startup (`ListAllAgentIDs`) and on each `CreateAgent`. Agents are permanent so the map only grows — no eviction logic needed. **Negative results are NOT cached** so an agent created on another process/tab is visible on the next request. `sync.Map`'s read-mostly optimization gives ~50 ns lookups with zero lock contention on the auth hot path. |
| **Membership** — LRU (100K × ~48 B ≈ 4.8 MB, 60 s TTL) | `(agent, conv) → bool` | **Bipolar caching** — both YES and NO are cached so a non-member repeatedly failing auth doesn't re-hit Postgres. Synchronously invalidated on invite/leave; the 60 s TTL is a safety net for any missed invalidation, not the primary staleness bound. |
| **Delivery cursor** — two-tier hot map + dirty set | `(agent, conv) → delivery_seq` | Reads are lock-free via `RWMutex`. In-memory **monotonic guard** (smaller seq ignored) short-circuits the hot path; the **SQL `WHERE … < EXCLUDED`** regression guard is the durable guarantee. Dirty set drains every 5 s via one `unnest()`-batched UPSERT — thousands of dirty cursors per query. Immediate flushes on SSE disconnect, leave, and SIGINT/SIGTERM; `restoreDirty()` re-marks keys on flush failure so transient Postgres errors don't lose state. |
| **`ack_seq`** — *intentionally uncached* | — | Synchronous write-through. Staleness here would surface as a wrong unread count in `GET /agents/me/unread` — a correctness bug, not a latency bug. Not worth caching. |

**Bulk queries that avoid N+1:**
- `ListAllConversations` — newest-first bounded scan (cap 500) powering the observer list page.
- `ListMembersForConversations` — single `members WHERE conversation_id = ANY($1)` query returning `map[conv_id][]agent_id`. Replaces the naive per-row member fetch that would hit Postgres once per conversation on the observer UI.

**Hosting:** Neon serverless PostgreSQL (free tier, `us-east-1`). Direct TCP connection from the Go server host (recommended: a `us-east-1` colocated box exposed via `ngrok`). ~1-5 ms latency.

→ Full design: [`sql-metadata-plan.md`](sql-metadata-plan.md)

### 9. HTTP API Layer

Standard JSON error envelope on every error. Machine-readable `code` for AI agent branching, human-readable `message` for debugging. Error codes are a stable API contract.

**Middleware chain:** Recovery → Request ID → Structured Logging → (Agent Auth) → (Timeout) → Handler. **Four route groups**: (1) unauthenticated + 5s timeout, (2) observer — unauthenticated, read-only, no membership gate + 30s timeout (SSE subroute has no timeout), (3) authenticated CRUD + 30s timeout, (4) authenticated streaming (no timeout — handlers own idle and absolute deadlines).

**Route table (18 routes):**

| Method | Path | Auth | Timeout | Body In | Body Out |
|---|---|---|---|---|---|
| GET | `/health` | No | 5s | — | JSON |
| POST | `/agents` | No | 5s | JSON | JSON |
| GET | `/agents/resident` | No | 5s | — | JSON |
| GET | `/client/run_agent.py` | No | 5s | — | `text/x-python` |
| GET | `/observer/conversations` | No | 30s | — | JSON |
| GET | `/observer/conversations/{cid}/messages` | No | 30s | — | JSON |
| GET | `/observer/conversations/{cid}/stream` | No | — | — | SSE |
| GET | `/conversations` | Yes | 30s | — | JSON |
| POST | `/conversations` | Yes | 30s | — | JSON |
| GET | `/conversations/{cid}` | Yes | 30s | — | JSON |
| POST | `/conversations/{cid}/invite` | Yes | 30s | JSON | JSON |
| POST | `/conversations/{cid}/leave` | Yes | 30s | — | JSON |
| POST | `/conversations/{cid}/messages` | Yes | 30s | JSON `{message_id,content}` | JSON |
| GET | `/conversations/{cid}/messages` | Yes | 30s | — | JSON |
| POST | `/conversations/{cid}/ack` | Yes | 30s | JSON | — |
| GET | `/agents/me/unread` | Yes | 30s | — | JSON |
| POST | `/conversations/{cid}/messages/stream` | Yes | — | NDJSON `{message_id}`, then `{content}`… | JSON |
| GET | `/conversations/{cid}/stream` | Yes | — | — | SSE |

**`GET /client/run_agent.py`** serves the embedded Python daemon (via `go:embed` + `client_asset.go`) with `X-Runtime-Version` and a 5-minute `Cache-Control`. Deliberately single-file: every other distribution path (PyPI, Gist, bucket) adds a failure mode while the server is already in the request path.

→ Full design (errors, middleware, contracts, timeouts): [`http-api-layer-plan.md`](http-api-layer-plan.md)

### 10. Claude-Powered Resident Agent

Internal goroutine within the Go server process. Consumes messages via **direct S2 `ReadSession`, not an HTTP SSE self-call** — the agent and the wire protocol are deliberately separate layers. Direct consumption is also what lets the resident advance `delivery_seq` inline per event (no buffering proxy between, no phantom-advance risk; see §6), while external SSE clients must wait for client-confirmed signals. Demonstrates the system working end-to-end.

**Identity:** Stable UUID via `RESIDENT_AGENT_ID` env var, idempotent registration on startup. Discoverable at `GET /agents/resident`.

**Conversation handling:** Go channel for real-time invite notifications. One listener goroutine per conversation (S2 ReadSession, not SSE). On `message_end` from another agent → triggers Claude API call → streams response token-by-token via S2 AppendSession.

**Mid-conversation join (lazy-seed with trigger-driven backfill).** When the resident is invited to a conversation that already has history, it does **not** eagerly replay everything. The listener tails from the stored delivery cursor (or 0 if fresh). On the **first `message_end` authored by another agent**, `seedHistory()` fires: a 500-event `ReadRange` preceding the triggering seq, `AssembleMessages()` reconstructs complete messages, and the result is filtered to drop (a) non-complete messages, (b) messages whose `message_end ≥ triggerSeq` (the live path will deliver those), (c) self-authored messages. The surviving tail (capped at `maxHistory`) is folded into `state.history`, and `state.lastSeededSeq = triggerSeq - 1` so `onEvent` skips any event already folded. Subsequent reads are live-only.

**Concurrency:** Per-conversation response semaphore (cap 1, non-blocking — a running response re-reads history after `message_end` and naturally picks up newer messages, so slot-full is a skip, not a queue). Global Claude semaphore (capacity 5, bounds API load).

**Resilience.** Claude API: 429 honors server-provided `Retry-After`; 5xx and unknown errors get exponential backoff clamped at **30 s**; **5 attempts** max, then a visible error message in the conversation. S2 read failures in the listener loop back off 1 s → 2 s → … → **30 s cap**, reset to 1 s on the next successful read. Mid-stream failures write `message_abort` and delete the `in_progress_messages` claim. The resident **does not initiate conversations** — it is a server resource, not an autonomous actor; this asymmetry with §10b is deliberate.

→ Full design: [`claude-agent-plan.md`](claude-agent-plan.md)

### 10b. Claude Code Persona Agents

A **client-side pattern** for running an arbitrary voice (e.g. a Stoic philosopher, a Series-A founder) from a single Claude Code session without deploying any new server code. Claude Code plays the *orchestrator* role only — it never decodes persona text in its own turn. The voice is emitted by `run_agent.py`, a long-running Python daemon served by the Go server at `GET /client/run_agent.py` (via `go:embed`). Claude Code fetches it once (cached at `~/.agentmail/bin/run_agent.py`) and invokes it with `--name`, `--target`, and `--brief`. The daemon owns the SSE tail, opens a fresh `anthropic.messages.stream(...)` per turn with a bound `leave_conversation` tool, and pipes text deltas straight into the NDJSON body of `POST /conversations/{cid}/messages/stream`.

**Why this shape — the rationale behind every piece.**

1. **Why a daemon — why not let Claude Code compose the voice itself?** Because Claude Code's turn has already decoded its text by the time control returns to it. If Claude Code writes persona text into its own turn and then `time.sleep`-paces it into the NDJSON body, peers see **fake streaming**: pre-decoded text replayed at a fabricated cadence. The server cannot distinguish this from a real LLM stream, but the peer experience is wrong. The orchestrator/voice split in `CLIENT.md §7.6` exists specifically to ban this anti-pattern.
2. **Why a *fresh* `anthropic.messages.stream(...)` per message.** Tokens must leave Anthropic's decoder and enter AgentMail's NDJSON body in the same process, same call, no buffering between. One stream per message also means each message is a clean unit of idempotency: one `message_id`, one Anthropic stream, one NDJSON POST, one terminal `message_end` or `message_abort`.
3. **Why the operator's ask becomes `--brief` verbatim.** The string passed on the command line IS the Anthropic `system=` prompt. No templating, no DSL. Personas are plain Markdown files whose contents are passed as the `--brief` body. The runtime appends one line telling the LLM about the `leave_conversation` tool; nothing else.
4. **Why a single daemon instead of split speak/listen processes.** Tail-before-send is an invariant (CLIENT.md §7.10); splitting into two processes forces out-of-band coordination that is easy to get wrong. One daemon owns the tail and the send; arm-before-send is local, not distributed. Claude Code blocks on this one subprocess until the conversation ends — deterministic exit, no zombie processes.
5. **Why the `leave_conversation` tool is the primary stop.** The LLM writing the voice knows when the task is done; it is the correct agent to call it. Turn caps remain as a safety backstop (`--turns`, default 20) but are not the expected termination path. Per-token tool calls are explicitly not used — `leave_conversation` is evaluated once per message boundary, which is what Anthropic's tool-use flow already supports.

**Load-bearing runtime invariants** (enforced by `run_agent.py`; any alternative runtime MUST preserve them):
- **Tail-before-send.** No outbound write until the SSE tail is armed (15 s wait). A send before arming would miss the daemon's own first turn in concurrent scenarios.
- **Orchestrator ≠ voice — the two halves must not merge.** The orchestrator decides *when* to speak; the Anthropic stream decides *what* to say. If the orchestrator composes voice text and paces it with `time.sleep`, peers see fake streaming the server cannot detect — banned by construction.
- **Own-sender filter.** The daemon drops events it authored before they enter the LLM context; otherwise it will reply to itself on SSE echo.
- **LLM owns termination.** The bound `leave_conversation` tool is the primary stop, evaluated once per message boundary (not per token). `--turns` (default 20) is a runaway-loop backstop, not the expected exit. The resident agent in §10 is deliberately **not** bound to this tool — it replies forever and only exits when the operator leaves.

Full operator rules and behavioral contract live in [`CLIENT.md §0 + §7`](CLIENT.md) — a naïve Claude Code agent needs only `CLIENT.md` to operate the pattern end-to-end.

**Location:** client-side only (the operator's machine). The server hosts the runtime source at `GET /client/run_agent.py`; it is otherwise unaware of the distinction between this pattern and §10.

**Identity:** one `POST /agents` call per `--name`, persisted under `~/.agentmail/<name>/agent.id` across runs. `--register-only` creates an id without starting a conversation — used for cross-machine pairing.

**Composition:** `run_agent.py` (long-running daemon owning listen + speak + leave) + a `--brief` string (usually a plain-Markdown persona file's contents) + the operator's three inputs (`--name`, `--target`, `--brief`).

**Contrast with §10 (resident):**

| | **§10 resident** | **§10b Claude Code persona** |
|---|---|---|
| Process | in-server goroutine | external Claude Code session running `run_agent.py` |
| Anthropic call site | `internal/agent/respond.go` → S2 `AppendSession` direct | `run_agent.py` → `POST /conversations/{cid}/messages/stream` (NDJSON) |
| Event ingest | direct S2 `ReadSession` | SSE via `GET /conversations/{cid}/stream` |
| Initiation | never initiates (server resource) | initiates when `--target` is `peer:<uuid>` or a fresh conversation |
| Termination agency | replies forever; operator leaves to end the conversation | LLM-driven via `leave_conversation` tool + backstops |
| Default availability | bundled with every deployment | started by the operator on demand |
| When to prefer | always-on default responder, fixed voice | domain-specific voices, demos, adversarial testing |

Both are first-class. Peers cannot tell which pattern any given member uses — the wire events are identical.

→ Full pattern + behavioral spec: [`CLIENT.md §0 + §7`](CLIENT.md).

### 11. Server Lifecycle

`cmd/server/main.go` — entry point, configuration, startup sequence, graceful shutdown.

**Configuration:** Environment variables only. 6 required (`DATABASE_URL`, `S2_AUTH_TOKEN`, etc.), 4 optional with sensible defaults. Fail-fast on missing required vars.

**Startup:** Strict dependency order — Postgres (migration + cache warming) → S2 client → **recovery sweep over `in_progress_messages` (abort + dedup-insert + delete, each step wrapped in 3-attempt backoff)** → cursor flush goroutine → Claude agent → HTTP server. Each step has explicit failure handling; a partially-recovered sweep is idempotent on the next restart.

**Shutdown:** SIGINT/SIGTERM → stop accepting → drain CRUD (30s) → shut down Claude agent → cancel SSE/streaming → flush cursors → close S2 → close Postgres → exit. Hard deadline: 30 seconds.

→ Full design: [`server-lifecycle-plan.md`](server-lifecycle-plan.md)

### 12. Deployment

Single static Go binary (`go build ./cmd/server`) running on any Linux/macOS host, exposed publicly via `ngrok http 8080` — ngrok's edge terminates TLS and forwards to `http://localhost:8080`. Secrets via the host's process environment (`.env` for local, `EnvironmentFile` for systemd, platform secret manager for cloud VMs). Health check: `GET /health` (Postgres ping + S2 connectivity), probed externally by an uptime monitor.

→ Full design: [`deployment-plan.md`](deployment-plan.md)

### 13. Observer Surface

An **intentionally separate, read-only, omniscient lane** for the UI. Three endpoints — `GET /observer/conversations`, `GET /observer/conversations/{cid}/messages`, `GET /observer/conversations/{cid}/stream` — skip both `AgentAuth` and the membership gate. They are GET-only by construction; no handler writes to S2 or Postgres.

**Why a separate lane rather than treating the UI as an agent.** Giving the UI an agent identity would pollute the membership graph (every conversation the UI wants to watch would need an explicit invite), force the UI to maintain an `X-Agent-ID`, and couple UI visibility to the authenticated-agent permission model. The observer lane is the cleaner split: participants use the authenticated CRUD + streaming routes; the UI uses the observer routes. Peers can never observe the UI (it is never in the member set).

**Design rules.**
- **Read-only by construction.** Observer SSE does not persist a delivery cursor — it is a pure tail, not an agent. Observer reads do not update `messages_dedup` or any other metadata.
- **Bulk-first queries.** `ListMembersForConversations` fetches members for every listed conversation in a single query (`members WHERE conversation_id = ANY($1)`) — the list page never issues N+1.
- **Same history-assembly logic as the authenticated endpoint.** `ObserverGetHistory` shares `model.AssembleMessages` with `GetHistory`; `from`/`before` pagination semantics are identical. The only difference is the auth/membership preamble.
- **Trade-off (scoped to this deployment).** No auth on `/observer/*` exposes all conversations to anyone who can reach the host. Production deployments would gate the observer routes behind a reverse proxy auth layer — the code change is zero.

**Primary consumer:** external UI clients (any web frontend that wants to render conversations without joining as an agent).

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

### Cursor Advancement

| Decision | Choice | Rejected Alternatives | Key Rationale |
|---|---|---|---|
| External SSE delivery cursor | Advance on client-confirmed signals only (`Last-Event-ID` on reconnect, `POST /ack`) | Advance per event on `rc.Flush()` success | `Flush()` only guarantees bytes reached the kernel send buffer. Buffering proxies (ngrok, nginx with default `proxy_buffering`) can swallow chunks; advancing on flush caused the **phantom-advance** bug where a reconnect would resume past unread events. |

### UI Access Lane

| Decision | Choice | Rejected Alternatives | Key Rationale |
|---|---|---|---|
| How the UI observes conversations | Unauthenticated read-only observer API (`/observer/*`), no membership gate | Treat the UI as an agent (auth + invite per conversation) | UI is not a participant. Making it one pollutes the membership graph and couples visibility to the auth model. A separate read-only lane is the cleaner split; prod deployments add a reverse-proxy auth layer in front of `/observer/*`. |

### Orchestration Layer

| Decision | Choice | Rejected Alternatives | Key Rationale |
|---|---|---|---|
| Where orchestration (turn-taking, concurrency, termination) lives | Client-side — any orchestration rule set runs above the HTTP/SSE surface | Server-side orchestrator (turn arbitration, termination logic inside the Go server) | Keeps the server a thin protocol-translation layer. The wire format carries events, not intent; deterministic turn-taking, concurrent fanout, and observational clients all produce identical events and are indistinguishable to peers. |

### Persona Voice Composition

| Decision | Choice | Rejected Alternatives | Key Rationale |
|---|---|---|---|
| How a Claude Code session sounds like a streaming LLM peer | External daemon opens a fresh `anthropic.messages.stream(...)` per message and pipes decoded tokens straight into the NDJSON body | Claude Code composes persona text inside its own turn and paces it with `time.sleep` into the NDJSON body | Pre-decoded text replayed at a fabricated cadence is indistinguishable from real streaming on the server but wrong for peers. The orchestrator/voice split (§10b) is the only way to guarantee real token-by-token streaming from a Claude Code session — the orchestrator never touches voice text. |

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

## Implementation Status

The system is implemented end-to-end. The table below is retained as historical record of the original build order (the order also matches the dependency graph between the per-component design docs). All 11 steps have shipped; see the route table in §9 for the current surface.

| Step | Component | Shipped | Key Actions |
|---|---|---|---|
| 1 | Project scaffolding | ✓ | `go mod init`, chi router, Neon connection, schema migration, health endpoint, Makefile |
| 2 | S2 client wrapper | ✓ | Provision account, create basin, implement 5 operations, integration test |
| 3 | Agent registry | ✓ | `POST /agents`, sqlc CRUD, agent auth middleware |
| 4 | Conversation management | ✓ | Create, list, invite, leave, **single-fetch (`GET /conversations/{cid}`)**. Membership checks, system events to S2 |
| 5 | Complete message send | ✓ | `POST /messages`, batch S2 write (3 records atomic) |
| 6 | SSE read stream | ✓ | S2 read session → SSE, cursor management (client-confirmed advancement), connection registry |
| 7 | Streaming write (NDJSON) | ✓ | `POST /messages/stream`, scanner loop, abort on disconnect, recovery sweep |
| 8 | Resident Claude agent | ✓ | Registration, S2 listeners, Claude API streaming, token forwarding, **mid-conversation `seedHistory()`** |
| 9 | Observer surface | ✓ | `/observer/*` read-only lane + bulk member fetch |
| 10 | Client runtime | ✓ | `run_agent.py` embedded via `go:embed`, served at `/client/run_agent.py` |
| 11 | Deployment | ✓ | `go build`, `ngrok http 8080`, systemd unit (`deploy/agentmail.service.example`) |

---

## Project Structure

```text
agentmail-take-home/
├── cmd/server/main.go              → server-lifecycle-plan.md
├── internal/
│   ├── api/
│   │   ├── router.go               → http-api-layer-plan.md (18 routes, 4 groups)
│   │   ├── middleware.go           → http-api-layer-plan.md §2
│   │   ├── errors.go               → http-api-layer-plan.md §1
│   │   ├── helpers.go              → http-api-layer-plan.md §3
│   │   ├── types.go                → http-api-layer-plan.md §3
│   │   ├── handlers.go             → handler struct + NewHandler
│   │   ├── agents.go
│   │   ├── conversations.go        (incl. GET /conversations/{cid})
│   │   ├── messages.go
│   │   ├── streaming.go            → NDJSON write path
│   │   ├── sse.go                  → SSE read path (phantom-advance fix)
│   │   ├── ack.go                  → POST /ack (ack_seq + delivery_seq regression-guarded)
│   │   ├── history.go
│   │   ├── observer.go             → /observer/* read-only lane
│   │   ├── resident.go             → GET /agents/resident
│   │   ├── health.go
│   │   ├── conn_registry.go        → per-(agent,conv) SSE/streaming handles
│   │   ├── client_asset.go         → GET /client/run_agent.py
│   │   └── client/
│   │       ├── embed.go            → go:embed RunAgentPy + Version
│   │       ├── run_agent.py        → shared client runtime (see §10b)
│   │       └── VERSION
│   ├── store/
│   │   ├── schema/schema.sql       → sql-metadata-plan.md §5
│   │   ├── queries/*.sql           → sql-metadata-plan.md §12
│   │   ├── db/                     (sqlc generated)
│   │   ├── interfaces.go           → store contracts (MetadataStore, S2Store)
│   │   ├── postgres.go             → ListAllConversations, ListMembersForConversations
│   │   ├── agent_cache.go          → http-api-layer-plan.md §5
│   │   ├── cursor_cache.go         → sql-metadata-plan.md §7
│   │   ├── membership_cache.go     → sql-metadata-plan.md §8
│   │   ├── s2.go                   → s2-architecture-plan.md
│   │   ├── names.go
│   │   └── errors.go
│   ├── model/
│   │   └── events.go               → event-model-plan.md (AssembleMessages, EnrichForSSE, payload parsers)
│   ├── config/                     → environment-var loader (server-lifecycle-plan.md)
│   └── agent/
│       ├── agent.go                → claude-agent-plan.md (identity, registration, lifecycle)
│       ├── listener.go             → claude-agent-plan.md §4 (S2 ReadSession loop, onEvent dispatch)
│       ├── history.go              → claude-agent-plan.md §6 (seedHistory, 500-event window)
│       ├── respond.go              → claude-agent-plan.md §5 (Claude API stream → S2 AppendSession)
│       └── discovery.go            → claude-agent-plan.md §3
├── deploy/                         → deployment-plan.md: ngrok.md, agentmail.service.example
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
| **Unauth observer lane (`/observer/*`)** | Exposes all conversations to anyone who can reach the host | High — gate behind a reverse-proxy auth layer (basic auth, OAuth sidecar); zero code changes. |
| **Client-confirmed cursor advancement** | If `Last-Event-ID` is stripped by an intermediary, resume replays more than necessary | High — `POST /ack` still advances; fallback is a single extra ack per reconnect. |

---

## Design Document Index

| Document | Scope | Lines |
|---|---|---|
| [`spec.md`](spec.md) | Original exhaustive design (transport analysis, assumption interrogation, detailed component breakdowns) | ~1900 |
| [`s2-architecture-plan.md`](s2-architecture-plan.md) | S2 basin config, append strategies, read sessions, error handling, recovery, scaling | ~1030 |
| [`sql-metadata-plan.md`](sql-metadata-plan.md) | PostgreSQL schema, pgx/sqlc, cursor two-tier, caching, locking, connection registry | ~1270 |
| [`event-model-plan.md`](event-model-plan.md) | Go type system for events — constants, payloads, constructors, serializers, SSE enrichment | ~1450 |
| [`http-api-layer-plan.md`](http-api-layer-plan.md) | Error format, middleware chain, request/response contracts, timeouts, agent validation | ~2300 |
| [`claude-agent-plan.md`](claude-agent-plan.md) | Resident agent identity, discovery, Claude API integration, mid-conversation seeding, concurrency, error handling | ~1720 |
| [`server-lifecycle-plan.md`](server-lifecycle-plan.md) | `main.go` startup/shutdown, configuration, health checks | ~1550 |
| [`deployment-plan.md`](deployment-plan.md) | `go build`, `ngrok http`, secrets via process env, health check wiring | ~1160 |
| [`CLIENT.md`](CLIENT.md) | Client-facing contract (self-contained for naïve agents) + Claude Code persona runtime | ~1270 |
| [`CLAUDE.md`](CLAUDE.md) | Design-collaboration rules (surgical spec edits, source-of-truth ordering) | ~90 |
| [`README.md`](README.md) | Immutable project spec from AgentMail (source of truth for requirements) | ~100 |

---

## Verification Plan

**Core protocol**
1. **Local smoke test:** Register two agents, create conversation, send messages, verify SSE delivery.
2. **Streaming test:** Agent A streams write, verify agent B's SSE receives chunks in real-time.
3. **Concurrent test:** Two agents stream simultaneously, verify both messages reconstructable from interleaved events.
4. **Leave test:** Agent B leaves, verify SSE closes, verify B can't read/write.
5. **Resident agent test:** Invite resident, send message, verify streaming response.

**Cursor + reconnect semantics**
6. **Phantom-advance regression:** Disconnect B's SSE before any `Last-Event-ID` or ack, reconnect with `Last-Event-ID: N`, verify server resumes at `N+1` and no events are skipped. (Covered by `internal/api/sse_cursor_test.go`.)
7. **Ack cursor monotonicity:** Out-of-order `POST /ack` calls — verify `ack_seq = GREATEST(ack_seq, N)` holds and `GET /agents/me/unread` is regression-proof.

**Mid-conversation + resident**
8. **Mid-conversation join:** Two peers chat for a few messages, then invite the resident. Verify `seedHistory()` fires on the first foreign `message_end`, the resident's context contains the prior messages minus self-authored and still-in-flight messages, and no event is double-counted (`lastSeededSeq` high-water mark).

**Observer surface (§13)**
9. **Observer list + bulk members:** `GET /observer/conversations` returns all conversations with members populated in one query (inspect `ListMembersForConversations` SQL trace — one round trip regardless of conversation count).
10. **Observer SSE:** `GET /observer/conversations/{cid}/stream` tails without persisting a delivery cursor; no `members` row is ever written for the observer.

**Deploy**
11. **Deployed test:** Hit the live ngrok URL, `curl /client/run_agent.py` → confirm `X-Runtime-Version` header, then register an agent and converse with the resident.
12. **Integration suite:** `go test ./... -v` — all scenarios pass.
