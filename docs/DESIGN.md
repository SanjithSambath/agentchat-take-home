# Design

This document captures the design decisions behind the AgentMail implementation shipped in this repo and the reasoning for each. It is scoped to choices that are load-bearing — not every small convention.

## 1. Core model: events, not messages

A conversation is a durable append-only log of **events**, not a table of messages. Six event kinds: `message_start`, `message_append`, `message_end`, `message_abort`, `agent_joined`, `agent_left`.

**Why.** Streaming writes and streaming reads are both first-class. If the storage primitive is "message row," streaming tokens force either (a) buffering until EOF, which defeats the purpose, or (b) mutating rows in place, which destroys history and ordering guarantees. Events give us append-only semantics with monotonic sequence numbers from the underlying log, and messages are reconstructed on read via `model.AssembleMessages` — demultiplexing by `message_id`, grouping start/append…append/end (or start/…/abort).

**Trade-off.** Reads are slightly more expensive (the server demultiplexes) and history shows partially completed messages with status `in_progress` when reading mid-stream. The payoff — a uniform event model for live SSE and historical reads, plus a natural fit for S2's append semantics — is worth it.

## 2. Storage split: S2 for the log, Postgres for metadata

Conversation events live in [S2](https://s2.dev) streams (one stream per conversation, `conversations/<uuid>`). Everything else — agents, conversations, membership, cursors, dedup — lives in Neon Postgres.

**Why S2 for events.** S2 is a hosted log service with server-assigned monotonic sequence numbers, Express storage class for low-latency writes (~40 ms acks), a native streaming append session, and a server-driven tailing read session. Building the same primitives on top of Postgres would be possible but lossy: we'd lose pipelined append, tailing reads without polling, and retention management. S2 is the right primitive for the write- and read-heavy streaming path.

**Why Postgres for metadata.** Relational indexes on membership (`agent_id` → `conversation_id`), per-pair cursors (`agent_id × conversation_id`), and idempotency gates (`in_progress_messages`, `messages_dedup`) are natural SQL problems. Postgres also gives us `FOR UPDATE` locking — load-bearing for the last-member leave race in `RemoveMember`.

**Why two systems instead of one.** The alternative is to fold metadata into S2 (e.g., a control stream), but S2 doesn't support secondary indexes or foreign keys. The alternative to Postgres is DynamoDB, which would serve the simple cases (agent exists, cursor get/put) but struggle with the "list conversations for agent" and "list unread" queries that want a join.

## 3. Two transport choices: NDJSON for writes, SSE for reads

**Writes: NDJSON over a single POST** (`POST /conversations/{cid}/messages/stream`). The client sends a header line (`{"message_id": "..."}`), then one JSON chunk per line, then closes. On EOF the server emits `message_end` and returns 200. On client disconnect the server emits `message_abort(disconnect)` and closes the S2 append session.

**Why not WebSocket.** A WebSocket buys bidirectionality we don't need (the server's response on a streaming write is terminal, not an ongoing dialog) and costs us: framing, heartbeats, upgrade middleware, and a second transport to proxy across Cloudflare's edge. A single POST with NDJSON is boringly HTTP — every HTTP proxy, load balancer, and debug tool already understands it.

**Reads: SSE** (`GET /conversations/{cid}/stream`) with `id:` = S2 seq, `Last-Event-ID` for auto-resume, 30 s heartbeat comments, 24 h absolute cap.

**Why SSE.** One-way server-push is exactly SSE's contract. Browsers and HTTP clients treat it as a normal streaming response. Auto-resume via `Last-Event-ID` means crashed clients reconnect without bespoke replay protocol — the header is standard, the seq is just the S2 seq as a decimal string.

## 4. Idempotency: claim gate + terminal dedup

Every write — complete or streaming — is gated by two Postgres tables:

- `in_progress_messages (PK message_id, UNIQUE (conversation_id, message_id))` — claimed at the start of every write. A duplicate `message_id` returns 409 immediately.
- `messages_dedup (PK (conversation_id, message_id))` — terminal outcome (complete | aborted). On retry the server returns the prior result without a second S2 write.

**Why.** Retries are a fact of life (timeouts, load balancer resets, client-initiated retries). The two tables together give us exactly-once *semantics* on top of at-least-once transport: the first attempt wins in S2, the second attempt gets the cached response from dedup, and neither produces a duplicate event in the log.

**Trade-off.** Two Postgres round-trips per write on the happy path (claim + terminal). At billion-agent scale, these collapse to `sync.Map` + LRU caches for agent existence and membership; the dedup row is the only necessary write.

## 5. Two-tier delivery cursor

`UpdateDeliveryCursor(agent, conv, seq)` is an in-memory write with no I/O. A background goroutine flushes the hot tier to Postgres every 5 s via one `unnest()` batched upsert, with a SQL-level regression guard (`WHERE delivery_seq < EXCLUDED.delivery_seq`) so a lagging flush can never rewind a fresher value.

**Why.** Cursor updates are the hottest write path: every SSE event a subscriber receives triggers an update. Synchronous writes would bottleneck on Postgres and multiply our write QPS by N-subscribers. A 5 s buffer costs at most 5 s of re-delivery on crash (acceptable — events are idempotent at read time) and batches everything into one upsert.

**Why not the same pattern for acks.** The ack cursor is called by clients explicitly (and rarely). There's no hot path, so we write it through synchronously with the same regression guard, no in-memory tier.

## 6. Single-connection invariant per (agent, conversation)

`ConnRegistry` tracks the active SSE stream and in-flight streaming writes for each `(agent, conversation)` pair. A new SSE preempts the existing one (last-writer-wins). A leave cancels both.

**Why.** Two concurrent SSE streams from the same agent to the same conversation would double-deliver. Two concurrent streaming writes with the same `message_id` would get gated by `in_progress_messages` anyway, but two writes with *different* `message_id`s are legitimate — so the write registry tracks multiple by `message_id` while the SSE registry enforces exclusivity.

**Trade-off.** The registry is in-memory per instance. With a single process behind one Cloudflare Tunnel this is correct; a fleet of instances needs a coordination layer (Redis pub/sub, or sticky routing by agent id at the Cloudflare Load Balancer / Worker layer) — see `FUTURE.md` §horizontal scaling.

## 7. Resident Claude agent as a first-class HTTP client (almost)

The resident agent is wired in-process: when `RESIDENT_AGENT_ID` and `ANTHROPIC_API_KEY` are set, `cmd/server/main.go` constructs an `agent.Agent` that owns one listener goroutine per conversation it's a member of. Listeners read S2 directly (not via SSE-to-self), detect completed messages from *other* agents, and respond via Anthropic streaming with the response written back as events through the same S2 path.

**Why not run the agent as an external process hitting the HTTP API.** Simpler wiring (no second binary, no second deployment), direct access to S2/Postgres for cursor management, shared graceful shutdown. An external client would be *cleaner* (the resident would be just another AgentMail user), but the take-home scope doesn't need that abstraction and it buys complexity.

**Loop safety.** Listeners filter `sender == self.id` and single-flight per conversation via a per-conversation semaphore. Two resident agents in the same conversation would still loop — that's flagged for FUTURE.md as a production hardening item.

## 8. Go stack and SDK choices

- `chi/v5` for routing — minimal, middleware-oriented, plays well with `http.Handler`.
- `pgx/v5` + `pgxpool` for Postgres — the fastest Postgres driver for Go with first-class `pgtype` support. Uses `QueryExecModeCacheDescribe` to stay compatible with Neon's PgBouncer pooler in transaction mode (no session-level prepared statements).
- `sqlc` for generated queries — typed, parameterized, compile-time schema checks. Two hand-written raw-pgx queries (`InsertMessageDedupAborted`, `RemoveMember`) cover cases sqlc can't express cleanly (nullable positional param, multi-statement transaction with lock+check+delete).
- `s2-sdk-go` — official. Configured with `AppendRetryPolicyAll` + `CompressionZstd` for the write path.
- `anthropic-sdk-go` — official. Streaming messages API.
- `google/uuid` — UUIDv7 for `message_id` (time-ordered, useful for tie-breaks and debugging).
- `rs/zerolog` — structured logging with zero allocations on the hot path. Request ID is attached at middleware level and flows through every handler log line.

## 9. Deployment: single Go binary behind a Cloudflare Tunnel

The server compiles to one static Go binary (`go build ./cmd/server`) and runs on any host — laptop, VM, bare metal — reading configuration from the process environment. Public ingress is a `cloudflared tunnel` pointing at `http://localhost:8080`; Cloudflare's edge terminates TLS and forwards HTTP/2 to the local port. No inbound ports, no certificates to manage, no container runtime required.

**Why cloudflared + one binary.** The managed state (S2, Postgres) is what matters; the server itself is stateless apart from in-memory caches and the connection registry. A single Go binary + tunnel collapses the deploy surface to "run the binary" — no platform-specific config, no Dockerfile, no region pinning. The tunnel is long-lived, HTTP/2-native, and handles SSE and NDJSON streaming without reverse-proxy surprises.

**One instance.** In-memory state (caches, `ConnRegistry`, cursor hot tier) is per-process and not shared — correctness does not depend on it being global. The only correctness risk with horizontal scaling is the single-connection invariant (see §6), which is why we ship one instance; see FUTURE.md §1 for the sticky-routing + pub/sub story.

**Graceful shutdown** drains in this exact order on SIGINT/SIGTERM: HTTP server graceful → resident agent shutdown → final cursor flush → Postgres pool close. A shared `shutdownCtx` enforces the budget end-to-end, so `cloudflared`'s connection drain and a local Ctrl-C both converge on the same deterministic exit path.

**Health check** at `GET /health` pings Postgres and S2 in parallel, each with a 3 s budget, returning 200 on both-ok and 503 otherwise with a per-dependency breakdown. A stream-not-found on the nil-UUID S2 probe is treated as success — it still proves auth and routing. Cloudflare's tunnel doesn't probe this for us; external uptime checks (UptimeRobot, Pingdom, or a Cloudflare Worker on a cron) are the right wiring for alerting.

**Crash recovery.** On startup, before accepting HTTP traffic, the server sweeps `in_progress_messages` and finalizes each row: append `message_abort(server_crash)` to S2, dedup-abort in Postgres, delete the claim. Transient dedup/delete failures retry with short backoff; anything left over reruns on the next restart.

## 10. What we explicitly chose not to do

- **Authentication beyond `X-Agent-ID`.** The take-home doesn't require real auth; the header is a stand-in for an eventual bearer token / mTLS / signed JWT.
- **Rate limiting.** Not in scope; Cloudflare's edge rate limiting (free tier: 10k requests/month per rule) and the Go server's own connection handling give us a basic concurrency cap.
- **Per-agent conversation list pagination.** Current `ListConversationsForAgent` returns everything the agent is a member of. Fine for the take-home; needs cursor pagination at scale.
- **Multi-region.** One region, one machine. See `FUTURE.md` for the multi-region plan.
- **Message-level encryption.** S2 encrypts at rest; we don't layer end-to-end.

These are deliberate cuts, not oversights.
