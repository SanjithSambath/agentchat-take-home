Here is the fully formatted Markdown for the specification document. The content remains exactly the same, but the hard line-breaks have been removed, the headers have been properly structured, code blocks have been added for APIs and schemas, and the ASCII tables/diagrams have been preserved appropriately.

***

# AgentMail: Exhaustive Spec Analysis & Implementation Plan

## Context

**What:** A stream-based agent-to-agent messaging service with durable history, real-time token streaming, and group conversations.

**Why this matters:** The spec is testing three things simultaneously: 
1. **Systems design taste** — can you model conversations as streams without overengineering.
2. **Infrastructure judgment** — do you reach for the right primitives instead of gluing together a Rube Goldberg machine.
3. **Agent empathy** — is the resulting API something an AI coding agent can use without a human holding its hand.

**The fundamental insight:** Conversations ARE streams. S2 gives you durable, ordered, replayable streams with real-time tailing. The server is a thin protocol translation layer between HTTP clients and S2 streams. Do not build a message broker. Do not build a queue. Do not add Kafka or Redis. The entire message storage and delivery layer is S2. The server handles identity, membership, routing, and protocol adaptation — nothing more.

---

## Architecture Overview

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
* **S2:** All message content. One stream per conversation. Records are events (`message_start`, `message_append`, `message_end`, system events). S2 handles ordering, durability, replay, real-time tailing.
* **PostgreSQL (Neon):** All metadata. Agent registry, conversation membership, read cursors. Managed serverless PostgreSQL via Neon, accessed over direct TCP from Go server on Fly.io.

**Transport:**
* **HTTP POST** for all writes — two tiers: complete messages (single request-response) and streaming messages (NDJSON streaming body over a single persistent HTTP connection)
* **SSE** for streaming reads (tail a conversation in real-time, replay from cursor)
* **History GET** for catch-up reads (reconstructed complete messages, polling-friendly)

**Language:** Go. First-class S2 SDK (v1 HTTP API). Goroutines handle thousands of concurrent SSE connections and streaming write requests. Single binary deployment. The problem domain (concurrent streaming, connection management, protocol translation) is Go's sweet spot.

---

## Phase 1: Exhaustive Component Breakdown

### 1.1 Agent Registry

**What the spec says:** Register provisions a new agent, returns an opaque ID. No metadata. No authentication. Each call creates a new identity. The ID scopes all access.

**Hidden complexities:**
* The agent ID is the sole credential. Leaking it means impersonation. The spec says no auth, so we accept this — but the ID should be a UUIDv7 (time-ordered, RFC 9562), not a sequential integer, to prevent enumeration.
* "No metadata" means no name, no description, nothing. This is intentional — the spec wants you to avoid building user management.
* The spec doesn't mention agent deletion. Agents are permanent. An agent that's a member of conversations cannot be garbage-collected without orphaning messages.

**Implementation:**
* `POST /agents` → generates UUIDv7 via `uuid.NewV7()`, inserts into agents table, returns `{ "agent_id": "uuid" }`
* PostgreSQL table: `agents(id UUID PRIMARY KEY, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`
* No request body needed. No idempotency concerns (each call creates a new agent).
* Validate agent existence on every subsequent API call via middleware.

**Edge cases:**
* Concurrent registration: no conflicts (UUIDs are unique)
* Invalid agent ID in subsequent requests: return 404 with clear error message
* Empty database on fresh deploy: first agent registration works with no special handling

**Files:** `internal/api/agents.go`, `internal/store/postgres.go`

---

### 1.2 Conversation Management

**What the spec says:** Create, list, invite, leave. Server-assigned IDs. Creator is auto-member. Multiple conversations can have identical participant sets. Any member can invite. No roles. Invite is idempotent for existing members, error for nonexistent agents. Leave by last member is rejected. Prior messages remain after leave. Active streams terminated on leave. Re-invite is allowed.

**Hidden complexities:**

* **Create:**
    * Insert into PostgreSQL first (conversation row + creator membership in one transaction), then append `agent_joined` event to S2.
    * The S2 stream auto-creates on this first append via `CreateStreamOnAppend: true` (basin-level config). No explicit `CreateStream` call needed.
    * If S2 is temporarily unreachable after the Postgres insert, the conversation exists and is functional — the stream auto-creates on the next successful write (message send, invite, etc.). Self-healing.
    * Stream name format: `conversations/{conversation_id}` (hierarchical, prefix-listable via S2's `ListStreams` with prefix).
* **List:**
    * Returns conversations where the agent is currently a member.
    * Each entry needs conversation ID and current member list.
    * This is a join query: `conversations JOIN members WHERE agent_id = ?`, then for each conversation, fetch all members.
    * For efficiency: single query with `GROUP_CONCAT` or similar.
    * Consider: should this include conversations the agent has left? The spec says "all conversations the agent is a member of" — current members only.
* **Invite:**
    * Idempotent for existing members (return 200, not error).
    * Error for nonexistent agents (return 404).
    * Error for nonexistent conversations (return 404).
    * Only members can invite. Check membership first.
    * Write an `agent_joined` system event to the S2 stream so it appears in conversation history.
    * The invitee gets access to full history — this is automatic since they'll read from the S2 stream which contains everything.
* **Leave:**
    * Reject if the agent is the last member (return 400 or 409).
    * Write an `agent_left` system event to the S2 stream.
    * **Critical:** terminate active SSE connections and in-progress streaming writes for this agent on this conversation.
        * The ConnRegistry tracks **both** SSE read connections and streaming write connections per `(agent_id, conversation_id)`.
        * On leave, cancel both connection types and wait for their goroutines to exit (5s timeout each).
    * **Critical:** abort any in-progress streaming message from this agent.
        * The streaming write handler's context cancellation triggers its abort path — it appends `message_abort` to S2 before exiting.
        * **Belt-and-suspenders:** If the write handler fails to append `message_abort` (S2 temporarily unreachable), the leave handler appends it as a fallback.
        * The `message_abort` event MUST precede the `agent_left` event on the S2 stream. This ordering is guaranteed because the leave handler waits for the write handler to exit before writing `agent_left`.
    * After leave, reject all read/write requests from this agent for this conversation.
    * The agent's read cursor is preserved in PostgreSQL for potential re-invite resume.

**Edge cases:**
* Race condition: agent A invites agent B while agent B leaves simultaneously. Resolve by checking membership under a transaction/lock.
* Agent invites themselves: already a member, idempotent no-op.
* Leave + immediate re-invite: should work. The agent gets a fresh start but full history is visible.
* Create conversation with no one to talk to: valid. An agent can create a conversation with itself. The spec explicitly says "A single agent can create a conversation with itself."

**Files:** `internal/api/conversations.go`, `internal/store/postgres.go`

**PostgreSQL tables:**
```sql
CREATE TABLE conversations(id UUID PRIMARY KEY, s2_stream_name TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE members(conversation_id UUID NOT NULL REFERENCES conversations(id), agent_id UUID NOT NULL REFERENCES agents(id), joined_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY(conversation_id, agent_id));
```

---

### 1.3 Message Streaming — Write Path

This is the hardest part of the spec. The spec requires streaming writes (token by token) AND the ability to send complete messages. Two distinct write patterns, both writing to the same S2 stream.

**The unit of communication:** An S2 record is the atomic unit on the stream. A "message" is a logical unit composed of one or more records. This distinction is the core design decision.

**Record types (events on the S2 stream):**

* **Event Type:** `message_start`
    * **Purpose:** Opens a new message
    * **Headers:** `type=message_start`
    * **Body (JSON):** `{"message_id":"uuid","sender_id":"uuid"}`
* **Event Type:** `message_append`
    * **Purpose:** Content chunk
    * **Headers:** `type=message_append`
    * **Body (JSON):** `{"message_id":"uuid","content":"tokens"}`
* **Event Type:** `message_end`
    * **Purpose:** Closes a message
    * **Headers:** `type=message_end`
    * **Body (JSON):** `{"message_id":"uuid"}`
* **Event Type:** `message_abort`
    * **Purpose:** Marks abandoned message
    * **Headers:** `type=message_abort`
    * **Body (JSON):** `{"message_id":"uuid","reason":"disconnect"}`
* **Event Type:** `agent_joined`
    * **Purpose:** System event: invite
    * **Headers:** `type=agent_joined`
    * **Body (JSON):** `{"agent_id":"uuid"}`
* **Event Type:** `agent_left`
    * **Purpose:** System event: leave
    * **Headers:** `type=agent_left`
    * **Body (JSON):** `{"agent_id":"uuid"}`

**Why this event model:**
* **Streaming:** Each `message_append` is immediately durable and visible to readers. A reader tailing the stream sees tokens in real-time.
* **Message boundaries:** `message_start`/`message_end` let readers reconstruct complete messages from the event stream.
* **Crash safety:** If an agent crashes mid-message (sends `message_start` + some `message_append` but no `message_end`), the server can detect this and emit `message_abort`. Readers know the message is incomplete.
* **Concurrent writes:** Two agents streaming messages simultaneously produce interleaved records on the stream. The `message_id` field on every record lets readers demultiplex — group events by `message_id` to reconstruct each agent's message independently.
* **Atomic complete messages:** For a non-streaming send, the server writes `[message_start, message_append, message_end]` as a single S2 batch append — atomic, all-or-nothing.

**API for complete message send:**
```http
POST /conversations/:cid/messages
Header: X-Agent-ID: <agent_id>
Body: { "content": "Hello, how are you?" }

Response: 201 Created
{ "message_id": "uuid", "seq": 42 }
```
*(Server writes three records to S2 in one batch: message_start, message_append (full content), message_end. Atomic.)*

**API for streaming message send (NDJSON streaming POST):**

The agent opens a **single HTTP POST request** with a streaming NDJSON body. The server reads the body line-by-line as chunks arrive, appending each to S2 in real-time. The entire message lifecycle — start, all chunks, end — happens within one persistent HTTP connection. No per-token round-trips.

```http
POST /conversations/:cid/messages/stream
Header: X-Agent-ID: <agent_id>
Header: Content-Type: application/x-ndjson

# Request body (streamed line by line as tokens are generated):
{"content":"Hello, "}
{"content":"how are "}
{"content":"you?"}

# Response (sent after body completes):
200 OK
{ "message_id": "uuid", "seq_start": 42, "seq_end": 45 }
```

**Server-side mechanics (Go) — AppendSession for pipelined writes:**
```go
func (h *Handler) StreamMessage(w http.ResponseWriter, r *http.Request) {
    msgID := uuid.NewV7()

    // Track in-progress message (Postgres-first, for crash recovery)
    h.store.InProgress().Insert(ctx, msgID, convID, agentID, streamName)

    // Open pipelined append session (non-blocking submits, async acks)
    session, err := h.s2.OpenAppendSession(ctx, convID)
    defer session.Close()

    // Background goroutine drains acks to detect S2 failures
    errCh := make(chan error, 1)
    go func() { /* drain futures, send first error to errCh */ }()

    session.Submit(ctx, messageStartEvent(msgID, agentID))

    scanner := bufio.NewScanner(r.Body)
    for scanner.Scan() {
        if ctx.Err() != nil { break }  // context canceled (e.g., agent left)
        var chunk struct{ Content string `json:"content"` }
        json.Unmarshal(scanner.Bytes(), &chunk)
        session.Submit(ctx, messageAppendEvent(msgID, chunk.Content))
    }

    if ctx.Err() != nil || scanner.Err() != nil {
        // Abort via Unary append (session may be errored)
        h.s2.AppendEvents(ctx, convID, []Event{messageAbortEvent(msgID, "disconnect")})
        h.store.InProgress().Delete(ctx, msgID)
        return
    }

    session.Submit(ctx, messageEndEvent(msgID))
    // Wait for all acks, then respond
    h.store.InProgress().Delete(ctx, msgID)
    json.NewEncoder(w).Encode(result)
}
```

**Why AppendSession, not Unary per token:** Unary at 40ms ack (Express) = max 25 sequential appends/sec. LLMs emit 30-100 tokens/sec. AppendSession pipelines the appends — submit token N while token N-1's ack is still in-flight. No bottleneck at any token rate S2 can handle.

`bufio.NewScanner(r.Body)` blocks at `scanner.Scan()` until a complete NDJSON line arrives from the client, then returns it. When the client closes the request body, `Scan()` returns `false` (EOF). If the connection drops mid-stream, `r.Context().Done()` fires and/or `r.Body.Read()` returns an error. This works identically on HTTP/1.1 chunked encoding and HTTP/2 DATA frames.

**Client-side scaffolding — the two-line pattern:**

The streaming write is designed for the "scaffolding wraps the agent" model. The agent doesn't know about AgentMail. Scaffolding captures its output and pipes it through a single POST.

*Python (12 lines, sync, stdlib-compatible):*
```python
import requests, json

def stream_message(base_url, agent_id, conv_id, token_iterator):
    def chunks():
        for token in token_iterator:
            yield json.dumps({"content": token}) + "\n"

    resp = requests.post(
        f"{base_url}/conversations/{conv_id}/messages/stream",
        headers={"X-Agent-ID": agent_id, "Content-Type": "application/x-ndjson"},
        data=chunks()
    )
    return resp.json()
```

*Shell (pipe agent output directly):*
```bash
agent_command | while IFS= read -r token; do
  printf '{"content":"%s"}\n' "$token"
done | curl -s -X POST \
  -H "X-Agent-ID: $AGENT_ID" \
  -H "Content-Type: application/x-ndjson" \
  --data-binary @- \
  "$BASE_URL/conversations/$CONV_ID/messages/stream"
```

**Why NDJSON streaming POST (not per-token HTTP POST, not WebSocket):**

The previous design used a separate HTTP POST request for each token (`POST /append` per chunk). At 30–100 tokens/sec from an LLM, that's 30–100 full HTTP request-response cycles per second — each with headers, TCP overhead, and a round-trip. This pattern is not scalable, not robust, and not how agents work. The AgentMail founder explicitly rejected this approach: *"Certainly don't use tool calls [per-token API calls]. Set up scaffolding around the agent to stream its output."*

The NDJSON streaming POST solves this with a single persistent connection:

| Dimension | Per-token HTTP POST (rejected) | NDJSON Streaming POST (chosen) |
| :--- | :--- | :--- |
| **Connections per message** | N+2 requests (start + N chunks + end) | 1 single connection |
| **Per-token overhead** | Full HTTP headers + round-trip (~5–50ms) | Zero — tokens flow as newline-delimited bytes on an open body |
| **Failure mode** | Any single POST can fail, orphaning the message | Connection drops → server reads EOF → emits `message_abort` |
| **Scaffolding complexity** | Agent must make API calls from inside generation loop | Scaffolding pipes stdout to a single curl command |
| **Server complexity** | Track in-progress messages across requests, timeout goroutine | Sequential `for scanner.Scan()` loop, standard HTTP handler |

WebSocket was also considered and rejected. See Challenge 3 (Phase 2) for the full comparative analysis.

**Why the request body uses bare content objects (not typed envelopes):**

The NDJSON lines are `{"content":"token"}`, not `{"type":"append","content":"token"}`. The message lifecycle is implicit in the HTTP request lifecycle:
* **Message start** = server receives the POST request and generates the `message_id`
* **Content chunks** = each NDJSON line in the body
* **Message end** = client closes the request body (EOF)
* **Message abort** = connection drops before body completes

This keeps the client protocol dead simple — the agent's scaffolding only needs to emit `{"content":"..."}` lines. The server handles all lifecycle events internally.

**Acknowledged tradeoffs:**

* **No mid-stream server feedback.** If the agent is removed from the conversation while streaming, they won't know until the response (or via their SSE read connection). Mitigation: extremely rare edge case.
* **Proxy buffering risk.** Some reverse proxies (nginx default config, Kubernetes ingress) buffer chunked POST request bodies. Mitigation: we control our deployment infrastructure (Fly.io supports streaming) and document the `proxy_request_buffering off` directive for self-hosted deployments. AI agents typically don't run behind buffering corporate proxies.
* **Read-once body means no automatic retries.** If the connection drops at 90% completion, the client can't retry with the same stream. Mitigation: for LLM output, you regenerate anyway. The `message_id` serves as an idempotency key for the start event.

**Hidden complexities:**

* **When is a write "successful"?**
    * S2 acknowledges after regional durability (data is in S3). Our server returns success to the client only after S2 acknowledges. This means: a successful write is durable. If the server crashes after S2 ack but before sending the HTTP response, the write is durable but the client thinks it failed. The client retries, creating a duplicate. 
    * **Mitigation:** use the `message_id` as an idempotency key — if a `message_start` for this `message_id` already exists, return the existing seq instead of writing again.
* **Concurrent writers:**
    * Two agents streaming simultaneously to the same conversation: their records interleave on the S2 stream. This is correct behavior. S2 assigns sequence numbers, guaranteeing total ordering. The interleaving is the ordering — it represents the temporal reality of concurrent composition.
    * Readers demultiplex by `message_id`. Each reader maintains a map of `message_id → accumulated_content` and assembles complete messages from interleaved chunks.
* **Orphaned messages (crash during streaming):**
    * With NDJSON streaming POST, the connection lifecycle IS the message lifecycle. If the agent crashes or disconnects, the server detects it immediately via `r.Context().Done()` or EOF on `r.Body.Read()`.
    * On detection: write `message_abort` event to the S2 stream. The HTTP handler returns (no response sent, since the connection is dead). Clean, deterministic — no background goroutine needed for timeout-based orphan detection.
    * On conversation leave: cancel the request context for any in-progress streaming write from this agent, triggering the abort path above.
    * **Edge case — server crash mid-stream:** The S2 stream has `message_start` + some `message_append` records but no `message_end` or `message_abort`. On server restart, a recovery sweep reads from the `in_progress_messages` PostgreSQL table (which tracks all active streaming writes), appends `message_abort` events to S2 for each row, and deletes the rows. No S2 stream scanning needed — the Postgres table is the index of unterminated messages.
* **Validation on write:**
    * Agent must be a member of the conversation (checked once when the streaming POST request arrives, before reading the body).
    * Content must be plaintext (spec says "purely plaintext communication").
    * Empty content lines are allowed (LLMs sometimes emit empty tokens).
    * For complete message POST: content must not be empty (no point sending an empty message).

**Files:** `internal/api/messages.go`, `internal/store/s2.go`, `internal/model/events.go`

---

### 1.4 Message Streaming — Read Path

**SSE (primary read transport):**

```http
GET /conversations/:cid/stream
Header: X-Agent-ID: <agent_id>
Query: ?from=<seq_num>   (optional, resume from specific position)

Response: 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive

id: 42
event: message_start
data: {"message_id":"m1","sender_id":"a1","timestamp":"2026-04-14T10:00:00Z"}

id: 43
event: message_append
data: {"message_id":"m1","content":"Hello, "}

id: 44
event: message_append
data: {"message_id":"m1","content":"how are you?"}

id: 45
event: message_end
data: {"message_id":"m1"}

id: 46
event: agent_joined
data: {"agent_id":"a2","timestamp":"2026-04-14T10:01:00Z"}
```

**Why SSE maps perfectly to S2 tailing:**
* SSE `id` field = S2 sequence number. Auto-resume via `Last-Event-ID` header on reconnect.
* SSE `event` field = record type (`message_start`, `message_append`, etc.)
* SSE `data` field = record body JSON
* SSE is unidirectional (server → client), matching the read-only nature of tailing.
* SSE auto-reconnects in browsers/clients with `Last-Event-ID`, giving us free resume semantics.

**Read flow:**
1.  Client sends `GET /conversations/:cid/stream` with optional `?from=seq` or `Last-Event-ID`.
2.  Server checks agent membership.
3.  Server determines starting position:
    * If `from` query param: use that sequence number.
    * If `Last-Event-ID` header: use that + 1.
    * If neither: look up agent's stored cursor from in-memory cache (falling back to PostgreSQL).
    * If no stored cursor: start from sequence 0 (beginning of conversation).
4.  Server opens S2 read session from the starting position.
5.  S2 returns historical records (replay/catchup phase).
6.  Server translates each S2 record to an SSE event and writes to the HTTP response.
7.  When S2 reaches the end of the stream, it transitions to real-time tailing.
8.  New records appended to the S2 stream are immediately forwarded as SSE events.
9.  Server periodically flushes the agent's read cursor from in-memory cache to PostgreSQL (batched every 5 seconds).

**Connection lifecycle:**
* Register connection in the connection registry: `activeConnections[(agent_id, cid)] = cancelFunc`
* On client disconnect: detected via `r.Context().Done()`, clean up, update cursor.
* On leave: server calls `cancelFunc`, which closes the response, client sees connection close.
* On server shutdown: graceful drain — send final event, close all connections.

**Cursor management:**
* Store in PostgreSQL (warm tier): `cursors(agent_id UUID, conversation_id UUID, seq_num BIGINT, updated_at TIMESTAMPTZ)`. Hot tier: in-memory `sync.RWMutex` map updated on every event delivery.
* Update strategy: batch updates — every 10 events or every 5 seconds, whichever comes first.
* On disconnect: flush final cursor position.
* On reconnect: resume from stored cursor.
* Result: at-least-once delivery. Agent may re-receive a few events after crash. Events have sequence numbers and message IDs for client-side deduplication.

**History endpoint (non-streaming):**
```http
GET /conversations/:cid/messages
Header: X-Agent-ID: <agent_id>
Query: ?limit=50&before=<seq_num>

Response: 200 OK
{
  "messages": [
    {"message_id":"m1","sender_id":"a1","content":"Hello, how are you?","seq_start":42,"seq_end":45,"complete":true},
    {"message_id":"m2","sender_id":"a2","content":"I'm well, thanks","seq_start":46,"seq_end":49,"complete":true}
  ]
}
```
*(This reconstructs complete messages from the raw event stream. Reads from S2, groups events by message_id, assembles content, returns structured messages. Useful for agents that want to see conversation history without processing raw events.)*

**Edge cases:**
* Agent connects to conversation they're not a member of: 403.
* Agent connects to nonexistent conversation: 404.
* Agent connects while another SSE connection for the same (agent, conversation) is active: close the old connection, use the new one. Only one active reader per agent per conversation.
* `from` seq_num is beyond the stream's current tail: start tailing immediately, wait for new events.
* `from` seq_num is before the stream's trim point (28-day retention on free tier): silently start from the stream's earliest available record (head). Log a warning server-side for observability. The agent catches up on whatever history remains.
* Slow reader: S2 handles backpressure. If the client can't keep up, the HTTP response buffer fills, Go's `http.Flusher` blocks, and the S2 read session slows down. No unbounded memory growth.

**Files:** `internal/api/sse.go`, `internal/api/history.go`

---

### 1.5 S2 Stream Storage Layer

**Full design details:** See `s2-architecture-plan.md` for the complete S2 architecture — basin configuration, append strategy analysis, read session management, error handling, recovery sweep, scaling roadmap, and Go interface design.

**Stream topology:** One S2 stream per conversation. Stream name: `conversations/{conversation_id}` (hierarchical, prefix-listable).

**Why one stream per conversation:**
* S2 provides total ordering within a stream → total ordering within a conversation (exactly what chat needs).
* Multiple concurrent readers tailing the same stream → all members see the same events in the same order.
* Replay from any position → offline agents catch up by reading from their cursor.
* No fan-out needed. No message duplication. No cross-stream ordering concerns.
* Unlimited streams per basin → unlimited conversations.

**Why NOT one stream per agent (inbox model):**
* Would require writing each message to N streams (one per participant). N writes vs 1 write.
* Cross-stream ordering is undefined — can't guarantee message A appears before message B across different agent streams.
* More storage, more writes, more complexity, worse ordering guarantees. Strictly inferior for this use case.

**Basin configuration:**
```go
s2.CreateBasinArgs{
    Basin: "agentmail",
    Scope: s2.Ptr(s2.BasinScopeAwsUsEast1),
    Config: &s2.BasinConfig{
        CreateStreamOnAppend: s2.Bool(true),   // auto-create streams on first append
        CreateStreamOnRead:   s2.Bool(false),  // no phantom streams on read
        DefaultStreamConfig: &s2.StreamConfig{
            StorageClass: s2.Ptr(s2.StorageClassExpress),  // 40ms ack (vs 400ms Standard)
            RetentionPolicy: &s2.RetentionPolicy{
                Age: s2.Int64(28 * 24 * 3600),  // 28 days (free tier max)
            },
            Timestamping: &s2.TimestampingConfig{
                Mode: s2.Ptr(s2.TimestampingModeArrival),  // S2 stamps on receipt
            },
        },
    },
}
```

**Account:** Free tier with $10 welcome credits. Express storage class — 40ms append ack is non-negotiable for real-time token streaming (Standard's 400ms makes readers lag ~12 tokens behind at 30 tokens/sec). Cost delta: $0.125/million messages. Negligible.

**S2 operations used:**

| Operation | When | S2 SDK Method | Pattern |
| :--- | :--- | :--- | :--- |
| **Append events (unary)** | System events, complete messages | `stream.Append()` | Single batch, atomic |
| **Append session (pipelined)** | Streaming token writes | `stream.AppendSession()` | Per streaming POST, non-blocking submits |
| **Read session (tailing)** | SSE handlers | `stream.ReadSession()` | Catch-up → real-time, indefinite tailing |
| **Read range (bounded)** | History endpoint, recovery | `stream.Read()` | Bounded batch, no tailing |
| **Check tail** | Health checks, cursor validation | `stream.CheckTail()` | Point query |

**S2 client wrapper (`internal/store/s2.go`):**
```go
type S2Store interface {
    AppendEvents(ctx context.Context, convID uuid.UUID, events []Event) (AppendResult, error)
    OpenAppendSession(ctx context.Context, convID uuid.UUID) (*AppendSessionWrapper, error)
    OpenReadSession(ctx context.Context, convID uuid.UUID, fromSeq uint64) (*ReadSessionWrapper, error)
    ReadRange(ctx context.Context, convID uuid.UUID, fromSeq uint64, limit int) ([]SequencedEvent, error)
    CheckTail(ctx context.Context, convID uuid.UUID) (uint64, error)
}
```
* Initialize with S2 auth token (from `S2_ACCESS_TOKEN` env var)
* Basin handle: single basin `agentmail` for the whole service
* `OpenReadSession` returns an iterator — the caller calls `Next()` in a loop. When the stream transitions from catch-up to real-time tailing, the iterator keeps yielding records seamlessly. When the caller cancels the context, the session closes.

**Append strategy (why two patterns):**
* **Unary** for system events + complete messages: Rare, simple, one atomic batch. Single round-trip.
* **AppendSession** for streaming tokens: Unary at 40ms ack = max 25 appends/sec. LLMs emit 30-100 tokens/sec. AppendSession pipelines appends — submit token N while token N-1's ack is still in-flight. No throughput bottleneck.
* **Producer (auto-batching) was evaluated and rejected:** 5ms linger delay before records flush adds latency for no benefit — tokens arrive serially from one NDJSON body, so there's nothing to batch.

**Record encoding:**
* S2 record headers: `type` → event type string (enables dispatch without JSON-parsing the body)
* S2 record body: JSON-encoded event payload
* Timestamps: S2-assigned (arrival mode) — included in `SequencedEvent` on the read path
* Why JSON over protobuf: the entire API is JSON. Protobuf adds a compilation step, client codegen dependency, and cognitive overhead for AI agents reading the code. JSON is self-describing, debuggable, and curl-friendly. The record bodies are small (message chunks), so serialization performance is irrelevant.

**Concurrency control:**
* No fencing tokens needed. Multiple agents write concurrently — this is desired behavior.
* No optimistic sequence numbers needed. We don't care about write ordering between agents; S2's sequencing IS the ordering.
* S2 atomic batch append for complete messages (3 records in one Unary call). Streaming tokens are pipelined via AppendSession — one submit per NDJSON line from the request body.

**Concurrent message interleaving:** When two agents stream simultaneously, their records interleave on the S2 stream in whatever order S2 receives them. This is correct — the interleaving IS the total order. Readers demultiplex by `message_id` to reconstruct each agent's message independently. The history endpoint assembles complete messages and orders them by `message_start` sequence number (who spoke first).

**Error handling and retry:** S2 SDK retry config with `AppendRetryPolicyAll` (3 attempts, exponential backoff). Duplicate records from retries are harmless — `message_id` on every record serves as a natural deduplication key. ReadSession failures cause SSE disconnection; client reconnects with `Last-Event-ID` for seamless resume.

---

### 1.6 Metadata Storage (PostgreSQL / Neon)

#### 1.6.1 Why PostgreSQL, Not SQLite

| SQLite Constraint | Impact at scale |
|---|---|
| **Single-writer lock** | ALL writes serialize — cursor flushes, invites, leaves, agent registration queue behind each other. At 10K cursor writes/sec, this is a hard wall. |
| **Database-level locks, not row-level** | Two agents leaving DIFFERENT conversations block each other. PostgreSQL's `FOR UPDATE` locks only the rows in the relevant conversation. |
| **No concurrent write connections** | WAL mode allows concurrent readers, but only one writer at a time. Every write-path goroutine contends for the same lock. |
| **No `LISTEN/NOTIFY`** | No path to multi-instance cache invalidation without adding another dependency. |
| **No native UUID type** | UUIDs stored as 36-byte TEXT, not 16-byte binary. 2.25x storage overhead on every UUID column and index. |
| **No `unnest()` batch operations** | Cursor flush requires N individual INSERT statements or brittle multi-value INSERT strings. |
| **Embedded = single process** | If you need a second server instance (horizontal scaling), you need a separate database entirely. |

**What PostgreSQL provides:**
- **Row-level locking** (`FOR UPDATE`) — concurrent leave operations on different conversations don't block each other
- **Connection pooling** with true concurrent writers — pgxpool manages 15 connections handling 30K+ queries/sec
- **`unnest()` batch upserts** — flush 10K cursor updates in a single 5-20ms statement
- **Native UUID type** — 16-byte binary storage, 55% smaller than TEXT representation
- **`LISTEN/NOTIFY`** — multi-instance cache invalidation (scaling path)
- **Mature ecosystem** — pgx, sqlc, monitoring, backups, replicas, partitioning — all battle-tested at scale

**What we lose:** Zero-config embedded deployment. PostgreSQL is an external dependency. Single-binary simplicity. The Go binary now requires a database connection string. **These costs are already paid** by the decision to deploy on Fly.io + Neon.

#### 1.6.2 Hosting: Neon Serverless PostgreSQL

Fly.io's own documentation is titled **"This Is Not Managed Postgres."** Their unmanaged Postgres is a VM running a Postgres Docker image — you handle backups, failover, monitoring, and recovery. Their truly managed offering starts at $38/month with no free tier.

| Dimension | Neon | Fly.io Postgres (unmanaged) | Fly.io Managed Postgres |
|---|---|---|---|
| **Management** | Fully managed | Self-managed | Fully managed |
| **Cost** | Free tier (0.5 GB, 100 CU-hours/mo) | ~$0 (VM cost only) | $38/mo minimum |
| **Backups** | Automatic, point-in-time recovery | Manual | Automatic |
| **Failover** | Automatic | Manual | Automatic |
| **PostgreSQL version** | 16, 17 | Whatever you install | 16 |
| **Connection pooling** | Built-in PgBouncer | DIY | Built-in |

**Architecture:**
```text
┌─────────────────────┐         TCP (direct)        ┌──────────────────────┐
│  Go Server          │ ──────────────────────────→  │  Neon PostgreSQL     │
│  (Fly.io, iad)      │         ~1-5ms latency       │  (AWS us-east-1)     │
│                     │ ←──────────────────────────   │  PostgreSQL 17       │
│  pgxpool (15 conns) │                              │                      │
└─────────────────────┘                              └──────────────────────┘
```

**Our choice: Direct connection.** We manage our own pool via pgxpool. pgx's built-in statement caching (`QueryExecModeCacheStatement`) gives us the performance benefit of prepared statements without PgBouncer's limitations.

**Neon-specific considerations:**
- **Scale-to-zero:** Neon suspends compute after 5 minutes of inactivity. Cold start is <500ms. For the live service evaluation, disable scale-to-zero to eliminate cold starts.
- **Storage:** Free tier is 0.5 GB. At our actual take-home scale (hundreds of agents), well under 0.5 GB. At 1M agents the footprint is ~700 MB — upgrade to Neon Launch ($19/mo, 10 GB).
- **Compute hours:** 100 CU-hours/month at 0.25 CU = 400 hours of runtime = ~16 days continuous. Sufficient for evaluation.

#### 1.6.3 Driver Stack: pgx v5 + sqlc

**pgx v5 native interface (not `database/sql` wrapper):**
- ~50% faster than `database/sql` due to binary wire protocol and statement caching
- Native PostgreSQL type support (UUID, JSONB, arrays, composite types)
- Batch operations (`pgx.Batch`, `pgx.CopyFrom`) not available through `database/sql`
- pgxpool is purpose-built for pgx, not a generic pool

**Key packages:**
- `github.com/jackc/pgx/v5` — driver
- `github.com/jackc/pgx/v5/pgxpool` — connection pool
- `github.com/jackc/pgx/v5/pgtype` — type system for UUID, arrays, etc.

**sqlc code generation:** Write SQL queries in `.sql` files. sqlc parses them against the schema at build time and generates type-safe Go structs + functions that call pgx. Compile-time SQL validation, no manual row scanning, zero runtime overhead. Where sqlc doesn't apply: `unnest()` batch upserts for cursor flush — use raw pgx.

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

#### 1.6.4 Primary Key Strategy: UUIDv7

UUIDv4 is random — every INSERT scatters across the B-tree, causing random page splits and cache thrashing. UUIDv7 is time-ordered (embeds a millisecond timestamp, RFC 9562), so inserts append to the rightmost leaf page.

| Metric | UUIDv4 | UUIDv7 | Improvement |
|---|---|---|---|
| Bulk insert throughput | Baseline | +49% | Sequential page writes vs random |
| Index size (1M rows) | Baseline | -25% | Fewer page splits → less fragmentation |
| Buffer cache hit ratio | Lower | Higher | Hot rightmost pages stay cached |

**Go side:** Generate with `uuid.NewV7()` from `github.com/google/uuid` (v1.6.0+, RFC 9562 compliant).
**PostgreSQL side:** Store as `UUID` type (16 bytes binary), NOT `TEXT` (36 bytes).
**No `DEFAULT gen_random_uuid()`** — we always generate in Go to ensure UUIDv7. If a row is inserted without an explicit ID, it should fail loudly.

**UUIDv7 and opacity:** The spec says "opaque identifier." UUIDv7 embeds a timestamp, but "opaque" means the client shouldn't depend on internal structure for functionality. The embedded timestamp is an implementation detail useful for debugging. No security implication (no auth to protect).

#### 1.6.5 Schema Design

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

-- Cursors: server-managed read positions for at-least-once delivery
-- No foreign keys — validated at API layer, and cursors may outlive membership
-- (preserved on leave for potential re-invite resume)
CREATE TABLE cursors (
    agent_id         UUID NOT NULL,
    conversation_id  UUID NOT NULL,
    seq_num          BIGINT NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, conversation_id)
);

-- In-progress streaming writes: tracks active streaming messages for crash recovery.
-- On server crash, recovery sweep writes message_abort for each row, then deletes.
-- Postgres-first insert (before S2 message_start) ensures crash safety.
CREATE TABLE in_progress_messages (
    message_id       UUID PRIMARY KEY,
    conversation_id  UUID NOT NULL,
    agent_id         UUID NOT NULL,
    s2_stream_name   TEXT NOT NULL,
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**Table rationale:**
* **`agents`:** No indexes beyond PK. No metadata columns (spec says "no metadata"). Agents are permanent (no soft-delete).
* **`conversations`:** `s2_stream_name` decouples conversation ID from S2 stream name (format: `conversations/{conversation_id}`). UNIQUE index prevents two conversations pointing to the same stream.
* **`members`:** PK order `(conversation_id, agent_id)` optimized for the hottest queries: membership check (`WHERE conversation_id = $1 AND agent_id = $2`) and member list (`WHERE conversation_id = $1`). Separate index on `(agent_id)` for "list conversations for agent." Foreign keys with no CASCADE — agents and conversations are permanent. **Hard delete on leave:** simpler queries (no `AND left_at IS NULL`), S2 stream has full membership event history, re-invite is a fresh INSERT.
* **`cursors`:** No foreign keys (validated at API layer, cursors may outlive membership). **Cursors survive leave** — on re-invite, agent resumes from where they left off, catching up on missed messages. `BIGINT` for `seq_num` (S2 sequence numbers are 64-bit). PK order `(agent_id, conversation_id)` optimized for disconnect cleanup.

**Why exactly five tables:** No `messages` table (messages live in S2). No `sessions`/`connections` table (tracked in-memory). No `events`/`audit` table (S2 streams ARE the audit log). The fifth table (`in_progress_messages`) is the minimal addition needed for crash recovery — it tracks active streaming writes so the server can emit `message_abort` events for unterminated messages on restart. Rows exist only for the duration of active streaming POST requests (seconds to minutes), so the table is tiny.

#### 1.6.6 Connection Pooling

```go
poolConfig, _ := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))

poolConfig.MaxConns = 15                              // Optimal for 4-core SSD: (cores * 2) + headroom
poolConfig.MinConns = 5                               // Pre-warm 5 connections on startup
poolConfig.MaxConnLifetime = 30 * time.Minute         // Recycle connections to prevent stale state
poolConfig.MaxConnLifetimeJitter = 5 * time.Minute    // Stagger recycling to prevent thundering herd
poolConfig.MaxConnIdleTime = 5 * time.Minute          // Release idle connections
poolConfig.HealthCheckPeriod = 30 * time.Second       // Detect dead connections
```

**Why 15 connections handles thousands of goroutines:** Membership checks are indexed point lookups (~100-500µs execution). Throughput: `15 conns × (1,000,000µs / 300µs) = 50,000 queries/sec`. Goroutines waiting for a connection are queued by pgxpool — wait is negligible.

**Critical rule: Never hold a connection during SSE.** SSE handlers touch the database exactly twice (cursor read on connect, cursor flush on disconnect), both sub-millisecond. The pool stays healthy.

#### 1.6.7 Membership Caching

Every API call validates membership — the #1 query by volume. In-process LRU cache with TTL reduces Postgres load.

```go
type MembershipCache struct {
    cache *lru.Cache[membershipKey, membershipEntry]
    ttl   time.Duration  // 60 seconds
    pool  *pgxpool.Pool
}
```

**Configuration:** Max 100,000 entries (~4.8 MB). TTL 60 seconds. LRU eviction.

**Read path:** Check cache → if hit and not expired, return → if miss, query Postgres, populate cache.
**Invite:** Insert into Postgres + set cache to true.
**Leave:** Delete from Postgres + delete from cache (don't set false — let next check query Postgres fresh).

**Why in-process, not Redis:** In-process is 10,000x faster (~50ns vs ~500µs network hop), zero operational cost, no additional dependency. Redis is only needed at 2+ server instances with shared cache.

**Multi-instance scaling path (documented, not implemented):** Use `LISTEN/NOTIFY` — on invite/leave, `NOTIFY membership_changed, 'conversation_id'`. All instances listen, invalidate local cache. TTL remains as safety net.

#### 1.6.8 Concurrency & Locking

**Race Condition 1: Last Member Cannot Leave**

Two agents in a conversation both call leave simultaneously. Both check count, both see 2, both proceed → 0 members.

**Solution:** `SELECT ... FOR UPDATE` serializes concurrent leave operations per conversation:
```sql
BEGIN;
SELECT agent_id FROM members WHERE conversation_id = $1 FOR UPDATE;
-- Application checks: (1) Is leaving agent in result set? (2) Is count == 1?
DELETE FROM members WHERE conversation_id = $1 AND agent_id = $2;
COMMIT;
```

No deadlock: both transactions lock the same rows in the same B-tree order. Second transaction blocks until first commits.

**Race Condition 2: Invite Idempotency**

`INSERT ... ON CONFLICT DO NOTHING RETURNING conversation_id`. If RETURNING returns a row → newly added, write `agent_joined` to S2. If no row → already a member, skip.

**Race Condition 3: Invite vs. Leave on Same Agent**

PostgreSQL serializes them. Leave-first: row deleted, then invite inserts fresh row. Invite-first: `ON CONFLICT DO NOTHING`, then leave deletes. Both outcomes consistent.

**Race Condition 4: Last Member Leave vs. Invite**

Leave uses `FOR UPDATE` (locks existing rows), invite uses `INSERT` (new row). Leave-first with count=1: rejects, invite proceeds. Invite-first: count becomes 2, leave proceeds. Both outcomes safe.

**Race Condition 5: Concurrent Agent Registration**

No race. UUIDv7s are unique. Independent rows. No locking needed.

#### 1.6.9 Connection Registry (Active Stream Termination)

The spec requires: "Active streaming connections are terminated on leave."

The ConnRegistry tracks **both** SSE read connections and streaming write connections per `(agent_id, conversation_id)`:

```go
type ConnRegistry struct {
    mu    sync.Mutex
    conns map[connKey]*connEntry
}

type connKey struct {
    AgentID        uuid.UUID
    ConversationID uuid.UUID
    ConnType       string  // "sse" or "write"
}

type connEntry struct {
    Cancel context.CancelFunc   // cancels the SSE/stream goroutine
    Done   chan struct{}         // closed when the goroutine exits
}
```

**Leave handler flow:**
1. Begin Postgres transaction → `SELECT ... FOR UPDATE` → check count > 1 → DELETE → commit
2. Invalidate membership cache
3. Look up `(agent_id, conversation_id, "write")` in ConnRegistry
4. If found:
   a. Call `entry.Cancel()` → write handler detects context cancellation
   b. `<-entry.Done` (5s timeout) → wait for write handler to exit
   c. If write handler failed to append `message_abort` → leave handler appends it as fallback (belt-and-suspenders)
5. Look up `(agent_id, conversation_id, "sse")` in ConnRegistry
6. If found: call `entry.Cancel()`, then `<-entry.Done` (5s timeout)
7. Write `agent_left` event to S2 stream (AFTER all connection goroutines have exited, ensuring abort precedes leave on the stream)
8. Return 200

**Timeout on wait:** 5-second timeout on each `<-entry.Done` to prevent a stuck goroutine from blocking the leave response indefinitely. If timeout fires, log a warning and proceed.

**SSE handler registration flow:**
```
1. Create context with cancel: ctx, cancel := context.WithCancel(r.Context())
2. Create done channel: done := make(chan struct{})
3. Register: registry.Register(agentID, convID, "sse", cancel, done)
4. defer:
   a. close(done)                   -- signals that goroutine has exited
   b. registry.Deregister(agentID, convID, "sse")
   c. cursor.FlushOne(agentID, convID)  -- flush final cursor position
5. Run SSE loop until ctx.Done()
```

**Streaming write handler registration flow:**
```
1. Create context with cancel: ctx, cancel := context.WithCancel(r.Context())
2. Create done channel: done := make(chan struct{})
3. Register: registry.Register(agentID, convID, "write", cancel, done)
4. defer:
   a. close(done)                   -- signals that goroutine has exited
   b. registry.Deregister(agentID, convID, "write")
5. Run streaming write loop until ctx.Done() or EOF
```

**One SSE connection per (agent, conversation):** If agent opens a second SSE connection, cancel the old one first. Prevents resource leaks.
**One streaming write per (agent, conversation):** If agent starts a second streaming write to the same conversation while one is in progress, reject with 409 Conflict.

#### 1.6.10 Store Interface: Go Design

```go
// Top-level
type Store interface {
    Agents() AgentStore
    Conversations() ConversationStore
    Members() MembershipService
    Cursors() CursorStore
    ConnRegistry() *ConnRegistry
    Close() error
}

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
    GetCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error)
    UpdateCursor(agentID, convID uuid.UUID, seqNum uint64)  // memory-only, no error
    FlushOne(ctx context.Context, agentID, convID uuid.UUID) error
    FlushAll(ctx context.Context) error
    Start(ctx context.Context)
}
```

**Domain types:**
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

#### 1.6.11 SQL Queries (sqlc Source)

**agents.sql:**
```sql
-- name: CreateAgent :one
INSERT INTO agents (id, created_at) VALUES ($1, now()) RETURNING id, created_at;
-- name: AgentExists :one
SELECT EXISTS(SELECT 1 FROM agents WHERE id = $1);
```

**conversations.sql:**
```sql
-- name: CreateConversation :exec
INSERT INTO conversations (id, s2_stream_name, created_at) VALUES ($1, $2, now());
-- name: GetConversation :one
SELECT id, s2_stream_name, created_at FROM conversations WHERE id = $1;
-- name: ConversationExists :one
SELECT EXISTS(SELECT 1 FROM conversations WHERE id = $1);
```

**members.sql:**
```sql
-- name: AddMember :one
INSERT INTO members (conversation_id, agent_id, joined_at) VALUES ($1, $2, now())
ON CONFLICT (conversation_id, agent_id) DO NOTHING RETURNING conversation_id;
-- name: RemoveMember :exec
DELETE FROM members WHERE conversation_id = $1 AND agent_id = $2;
-- name: IsMember :one
SELECT EXISTS(SELECT 1 FROM members WHERE conversation_id = $1 AND agent_id = $2);
-- name: LockMembersForUpdate :many
SELECT agent_id FROM members WHERE conversation_id = $1 FOR UPDATE;
-- name: ListConversationsForAgent :many
SELECT m.conversation_id, c.created_at FROM members m
JOIN conversations c ON c.id = m.conversation_id WHERE m.agent_id = $1 ORDER BY c.created_at DESC;
-- name: ListMembersForConversation :many
SELECT agent_id FROM members WHERE conversation_id = $1;
```

**cursors.sql:**
```sql
-- name: GetCursor :one
SELECT seq_num FROM cursors WHERE agent_id = $1 AND conversation_id = $2;
-- name: UpsertCursor :exec
INSERT INTO cursors (agent_id, conversation_id, seq_num, updated_at) VALUES ($1, $2, $3, now())
ON CONFLICT (agent_id, conversation_id) DO UPDATE SET seq_num = $3, updated_at = now()
WHERE cursors.seq_num < $3;
```

**in_progress.sql:**
```sql
-- name: InsertInProgress :exec
INSERT INTO in_progress_messages (message_id, conversation_id, agent_id, s2_stream_name, started_at)
VALUES ($1, $2, $3, $4, now());
-- name: DeleteInProgress :exec
DELETE FROM in_progress_messages WHERE message_id = $1;
-- name: ListInProgress :many
SELECT message_id, conversation_id, agent_id, s2_stream_name, started_at FROM in_progress_messages;
```

Batch cursor flush uses raw pgx with `unnest()` arrays (not sqlc).

#### 1.6.12 Migration Strategy

**For take-home:** Embedded SQL with `CREATE TABLE IF NOT EXISTS` run on startup. No migration tool needed for single-version schema.
**For production:** golang-migrate (`github.com/golang-migrate/migrate`). Numbered migration files, `schema_migrations` tracking table, backward-incompatible changes require multi-step deploys.

#### 1.6.13 Scaling Roadmap (Millions → Billions)

**Phase 1 — Current Design (Millions):** Single Fly.io instance + Neon Postgres. In-process membership cache (LRU, 100K entries, 60s TTL). In-memory cursor hot tier + batched Postgres flush. pgxpool with 15 connections. Handles ~1M agents, ~5M conversations, ~50K concurrent SSE connections.

**Phase 2 — Read Replicas (Tens of Millions):** Add Neon read replicas. Route reads to replicas, keep writes on primary. Membership cache reduces replica load by 90%+.

**Phase 3 — Partitioning (Hundreds of Millions):** Hash-partition `members` by `conversation_id` (64-128 partitions). Hash-partition `cursors` by `agent_id`. Tradeoff: `ListConversationsForAgent` scans all partitions — mitigate with denormalized table or accept cross-partition scan.

**Phase 4 — Dedicated Instances (Billions):** Replace Neon with dedicated PostgreSQL (AWS RDS). Shard by tenant/region. Redis between hot tier and Postgres. LISTEN/NOTIFY for cross-instance invalidation. PgBouncer for connection pooling across instances.

#### 1.6.14 Edge Cases & Failure Modes

* **Server crash during leave transaction:** PostgreSQL rolls back. Member row restored. Leave didn't happen. Correct.
* **Server crash during invite (after Postgres commit, before S2 event):** Agent is a member but no `agent_joined` event on S2 stream. Functional (agent can read/write), cosmetic gap in history. Production mitigation: reconciliation sweep on startup.
* **Neon cold start during evaluation:** Disable scale-to-zero, or use Fly.io health checks to keep Neon compute warm.
* **pgxpool exhaustion:** pgxpool queues goroutines. Sub-millisecond queries mean wait is negligible. Set `context.WithTimeout(ctx, 5*time.Second)` on all DB ops for fail-fast.
* **Membership cache poisoning (direct Postgres manipulation):** 60-second TTL self-corrects. Don't bypass the API.

**Files:** `internal/store/postgres.go`, `internal/store/cursor_cache.go`, `internal/store/membership_cache.go`, `internal/store/conn_registry.go`, `internal/store/schema/schema.sql`, `internal/store/queries/*.sql`, `internal/store/db/` (sqlc generated), `sqlc.yaml`

---

### 1.7 Read Cursor Management — Two-Tier Architecture

**Design:** Server-managed cursors with a two-tier architecture: in-memory hot tier for zero-cost updates, batched PostgreSQL warm tier for durability.

**Why server-managed (not client-managed):**
* The spec recommends it: "We recommend that the server track each agent's read position."
* AI agents are stateless between sessions. They can't reliably persist their own cursors.
* Server-side cursors mean reconnection is seamless — no state to pass from client.

**Architecture:**
```text
                    In-Memory (Hot Tier)                     PostgreSQL (Warm Tier)
                    ────────────────────                     ──────────────────────
SSE event delivered
        │
        ▼
┌─────────────────────┐                              ┌──────────────────────────┐
│   CursorCache       │     every 5 seconds          │   cursors table          │
│   sync.RWMutex      │  ─────────────────────────→  │   (agent_id, conv_id,    │
│   map[key]entry     │     batch UPSERT via         │    seq_num, updated_at)  │
│   dirty set         │     unnest() arrays          │                          │
└─────────────────────┘                              └──────────────────────────┘
        │                                                       ↑
        │  on disconnect / leave                                │
        └───────────────────────────────────────────────────────┘
                    immediate single flush
```

**Hot tier (in-memory):** `sync.RWMutex` + `map[cursorKey]cursorEntry` + dirty set. Updated on every event delivery. Zero I/O cost. Why `sync.RWMutex`, not `sync.Map`: cursor updates are write-heavy from many goroutines on overlapping keys — neither `sync.Map` optimization pattern applies. At extreme scale (>100K concurrent SSE connections): shard into 64 buckets by `hash(agent_id) % 64`.

**Warm tier (PostgreSQL):** Background goroutine flushes all dirty cursors every 5 seconds via `unnest()` batch upsert:
```sql
INSERT INTO cursors (agent_id, conversation_id, seq_num, updated_at)
SELECT * FROM unnest($1::uuid[], $2::uuid[], $3::bigint[], $4::timestamptz[])
ON CONFLICT (agent_id, conversation_id)
DO UPDATE SET seq_num = EXCLUDED.seq_num, updated_at = EXCLUDED.updated_at
WHERE cursors.seq_num < EXCLUDED.seq_num;
```
The `WHERE cursors.seq_num < EXCLUDED.seq_num` clause prevents stale flushes from regressing a cursor.

**Flush triggers:**

| Trigger | What happens | Why |
|---|---|---|
| **Periodic (every 5 sec)** | Background goroutine flushes ALL dirty cursors | Bounds data loss on crash to 5 seconds |
| **Clean disconnect** | SSE handler flushes THIS agent's cursor immediately | Zero data loss on graceful close |
| **Server shutdown** | Graceful shutdown flushes ALL dirty cursors before exit | Zero data loss on planned restart |
| **Agent leave** | Flush this agent's cursor for this conversation | Preserve accurate position for potential re-invite |

**Crash recovery:** Server crashes → in-memory cursors lost. Agent reconnects → server reads cursor from PostgreSQL (at most 5 seconds stale) → S2 read session starts from that position → agent re-receives up to ~150 events (5 sec × ~30 events/sec) → events carry sequence numbers for client-side deduplication. Textbook **at-least-once delivery**.

**Cursor behavior on leave and re-invite:**
* On leave: flush cursor to PostgreSQL, remove from in-memory cache, do NOT delete from PostgreSQL.
* On re-invite + SSE connect: cache miss → fall back to PostgreSQL → cursor exists from before leave → resume from that position → agent catches up on messages sent while gone.
* This is better than deleting cursors (which would force re-reading the entire conversation from sequence 0).
* The spec's "access to full conversation history" is satisfied by the S2 stream — agent CAN read from 0 via `?from=0`.

**Delivery semantics:** At-least-once.
* If server crashes between delivering an event and flushing the cursor, the event will be re-delivered on reconnect.
* Events carry sequence numbers and message IDs. Clients can deduplicate if needed.
* Why not exactly-once: requires client acknowledgments, adds protocol complexity, and at-least-once with idempotent processing is the industry standard for streaming systems.

---

### 1.8 Claude-Powered Agent

**The spec requires:** "at least one Claude-powered agent running on it that we can converse with."

> **Full design:** [`claude-agent-plan.md`](claude-agent-plan.md) — exhaustive design document covering identity, discoverability, conversation lifecycle, message handling, Claude API integration, rate limiting, error handling, and recovery. What follows is the executive summary.

**Architecture:** Internal goroutine within the Go server process, calling store/S2 layers directly — not via HTTP self-calls. The resident agent is a first-class client of the same interfaces the API handlers use, demonstrating the system working end-to-end.

```text
┌─────────────────────────────────────────────────────────┐
│ Go Server Process                                       │
│                                                         │
│  ┌─────────────┐  invite channel  ┌──────────────────┐  │
│  │ API Handlers │ ──────────────→ │ Resident Agent   │  │
│  │ (HTTP)       │                 │                  │  │
│  └──────┬───────┘                 │  ┌────────────┐  │  │
│         │                         │  │ Listener   │  │  │
│         │ same store interfaces   │  │ goroutine  │  │  │
│         ▼                         │  │ (per conv) │  │  │
│  ┌──────────────┐                 │  └─────┬──────┘  │  │
│  │ Store Layer  │ ←───────────────│────────┘         │  │
│  │ (Postgres)   │                 │                  │  │
│  └──────────────┘                 │  ┌────────────┐  │  │
│  ┌──────────────┐   direct R/W   │  │ Response   │  │  │
│  │ S2 Client    │ ←──────────────│──│ goroutine  │  │  │
│  │ (streams)    │                 │  └─────┬──────┘  │  │
│  └──────────────┘                 │        │         │  │
│                                   │        ▼         │  │
│                                   │  ┌────────────┐  │  │
│                                   │  │ Claude API │  │  │
│                                   │  │ (stream)   │  │  │
│                                   │  └────────────┘  │  │
│                                   └──────────────────┘  │
└─────────────────────────────────────────────────────────┘
```

**Core decisions:**

| Decision | Choice | Rationale |
|---|---|---|
| Identity | Stable UUID via `RESIDENT_AGENT_ID` env var, idempotent `EnsureExists` on startup | Survives restarts without orphaning conversations |
| Discoverability | `GET /agents/resident` — unauthenticated, returns array | AI-agent-friendly, no chicken-and-egg auth problem |
| Conversation discovery | Go channel for real-time invites + one-time startup reconciliation | Event-driven, no polling; catches conversations from prior server lifetimes |
| Reads | Direct S2 `ReadSession` per conversation (one listener goroutine each) | Same durable stream the API handlers read from; no HTTP overhead |
| Writes | Direct S2 `AppendSession` for streaming responses | Token-by-token flow from Claude to S2 with no intermediate HTTP layer |
| Response triggering | Always respond to every `message_end` from another agent | Resident agent is human-out-of-loop; `[NO_RESPONSE]` protocol rejected as unnecessary complexity |
| History | Lazy seeding from S2 on first external message; bounded sliding window (last 50 messages) | No wasted I/O for conversations that never receive messages |
| Claude integration | `anthropic-sdk-go` streaming → S2 `AppendSession` | `message_start` → N × `message_append` (token chunks) → `message_end` (or `message_abort` on failure) |
| Concurrency | Per-conversation channel semaphore (capacity 1) + global Claude semaphore (capacity 5) | Sequential responses per conversation; bounded global API load |
| Error handling | 429 → exponential backoff with `Retry-After`; transient errors → 1 retry; persistent failures → `sendErrorMessage()` visible in conversation | Graceful degradation, never silent failure |
| Crash recovery | Sweep `in_progress_messages` on startup → write `message_abort` for orphaned messages | Clean state after unclean shutdown |

**Key implementation files** (see `claude-agent-plan.md` § Files for full breakdown):

| File | Responsibility |
|---|---|
| `internal/agent/agent.go` | `Agent` struct, `Start()`/`Shutdown()`, conversation state map, semaphores |
| `internal/agent/listener.go` | Per-conversation listener goroutine, event dispatch, `onEvent()` |
| `internal/agent/respond.go` | `triggerResponse()`, `respond()`, `callClaudeStreaming()`, `sendErrorMessage()` |
| `internal/agent/history.go` | `seedHistory()`, `buildClaudeMessages()`, sliding window management |
| `internal/agent/discovery.go` | Invite channel consumer, startup reconciliation loop |

**Relationship to external agents:** The resident agent uses direct store/S2 access because it lives in-process. External agents (the primary users of the service) use the HTTP API: NDJSON streaming POST for writes, SSE for reads. These are distinct, appropriate mechanisms — no conflict. The `claude-agent-plan.md` documents patterns for external human-in-the-loop agents in its `CLIENT.md` / `FUTURE.md` recommendations.

---

## Phase 2: Assumption Interrogation & Radical Alternatives

### Assumptions in the Spec (Explicit and Implicit)

* **#: 1**
    * **Assumption:** S2 is the right storage primitive
    * **Type:** Recommended
    * **My Position:** Agree. S2 is purpose-built for this exact problem. Using anything else means rebuilding what S2 already provides.
* **#: 2**
    * **Assumption:** Conversations need total ordering
    * **Type:** Implicit
    * **My Position:** Agree for single-conversation scope. Within one conversation, total ordering is essential — participants need a shared reality. Cross-conversation ordering is irrelevant.
* **#: 3**
    * **Assumption:** The server should mediate all access
    * **Type:** Explicit
    * **My Position:** Agree. Direct S2 access would leak implementation details and bypass membership checks.
* **#: 4**
    * **Assumption:** No authentication is needed
    * **Type:** Explicit
    * **My Position:** Agree for scope. Agent IDs as bearer tokens is sufficient. The spec is testing messaging design, not auth.
* **#: 5**
    * **Assumption:** Messages are plaintext only
    * **Type:** Explicit
    * **My Position:** Accept. Don't build rich formatting. Don't build file uploads. Stay focused.
* **#: 6**
    * **Assumption:** AI coding agents are the primary clients
    * **Type:** Explicit
    * **My Position:** This is the most important assumption. It means: prefer HTTP-native transports. Prefer curl-friendly, pipeable APIs. Prefer self-describing JSON. Avoid protocols that require specialized client libraries or complex connection management (WebSocket, gRPC). The streaming write must be something an AI coding agent can build scaffolding for from documentation alone.
* **#: 7**
    * **Assumption:** Real-time streaming matters
    * **Type:** Explicit
    * **My Position:** Agree. LLMs generate tokens incrementally. Showing tokens as they arrive is the whole point.
* **#: 8**
    * **Assumption:** Conversations have unbounded participants
    * **Type:** Implicit
    * **My Position:** Agree for the API. No limit on members. But practically, >100 members in a conversation would cause S2 write contention and reader fan-out issues. Not a concern for the take-home.
* **#: 9**
    * **Assumption:** Messages are small
    * **Type:** Implicit
    * **My Position:** Agree. LLM outputs are text. Individual chunks are a few tokens. Complete messages are a few KB at most. Well within S2's 1 MiB record limit.
* **#: 10**
    * **Assumption:** The server is a single instance
    * **Type:** Implicit
    * **My Position:** Accept for now. The take-home doesn't require horizontal scaling. But the design should not preclude it — stateless request handling + external storage (S2 + PostgreSQL (Neon)) is naturally scalable.

### Challenges to Spec Assumptions

**Challenge 1: "Use S2" — is there a simpler alternative?**
Could we skip S2 and use PostgreSQL LISTEN/NOTIFY + a messages table? Yes, technically. But:
* You'd rebuild ordering, durability, and replay that S2 provides natively.
* LISTEN/NOTIFY is fire-and-forget — missed notifications are gone. You'd need polling as fallback.
* PostgreSQL isn't designed for high-throughput, low-latency stream tailing.
* The spec recommends S2 because the evaluators built S2. Using it shows you can learn and apply new infrastructure idiomatically.
* **Verdict:** Use S2. It's the right tool and the evaluators want to see you use it well.

**Challenge 2: "One stream per conversation" — what about one stream per agent?**
Per-agent streams (inbox model) would mean: when agent A sends a message to conversation C with members `[A, B, C]`, the server writes to B's inbox stream and C's inbox stream. Each agent tails only their own stream.
* **Problems:**
    * Write amplification: 1 message → N writes (one per member). Gets worse with large groups.
    * Ordering: messages from different senders arrive in different order in different inboxes. No shared reality.
    * Conversation reconstruction: to show conversation history, you'd need to merge-sort multiple agent streams. Brutal.
    * Invitation: when agent D joins, you'd need to copy all historical messages to D's inbox. Or maintain a separate history stream anyway — which is just the per-conversation model.
* **Verdict:** Per-conversation streams. Strictly superior for this use case.

**Challenge 3: "What transport for streaming writes?" — NDJSON streaming POST vs WebSocket vs per-token HTTP POST**

Three candidates were evaluated for the streaming write path. One was chosen. The other two were rejected with clear rationale.

**Candidate A: Per-token HTTP POST (REJECTED)**

The original design: `POST /messages/stream` to start, `POST /messages/:mid/append` per chunk, `POST /messages/:mid/end` to close. A separate HTTP request-response cycle for every token.

* At 30–100 tokens/sec, this is 30–100 full HTTP round-trips per second. Each carries headers, TCP overhead, and server-side request parsing.
* The agent must make API calls from inside its generation loop — or the scaffolding must fire individual HTTP requests per token. Neither is practical.
* The AgentMail founder explicitly rejected this pattern: *"Certainly don't use tool calls [per-token API calls]. Set up scaffolding around the agent to stream its output."*
* Server-side: must track in-progress messages across requests in a `map[message_id]`, run a background goroutine for timeout-based orphan detection, and handle the race conditions of per-request validation.
* **Verdict:** Rejected. Not scalable, not robust, not how agents work.

**Candidate B: WebSocket (REJECTED)**

WebSocket provides full-duplex communication on a persistent connection. Tokens flow as WebSocket messages, server can send acks/errors mid-stream.

* **Stateful fragility.** WebSockets maintain a persistent TCP connection. Load balancers (AWS ALB, nginx) aggressively timeout idle connections. If an agent pauses token generation for 30 seconds, the load balancer may silently sever the connection. Mitigation requires custom ping/pong heartbeat logic in both server and client.
* **Agent friction.** AI coding agents are the primary clients. Asking an LLM to write a Python script that makes an HTTP POST is trivial. Asking it to write an async Python script that manages a WebSocket connection, handles binary framing, manages heartbeat intervals, and reconnects on failure is asking for hallucination and brittle code. The `websockets` library requires `asyncio`; the sync alternatives are less maintained.
* **Coupled failure domains.** If WebSocket carries both reads and writes on one connection, a network hiccup kills both simultaneously. With SSE (reads) + NDJSON POST (writes), each has an independent failure domain. One dropping doesn't affect the other.
* **Server complexity.** WebSocket handlers require: upgrade negotiation, read goroutine + write goroutine + select loop, connection registry, heartbeat ticker, graceful shutdown, custom message framing protocol. Standard HTTP middleware (auth, logging, rate limiting) doesn't apply — you must reimplement it.
* **Horizontal scaling burden.** WebSocket connections are stateful — load balancers need sticky sessions or connection-aware routing. NDJSON POST is stateless HTTP — any load balancer works out of the box.
* **The bidirectionality advantage is hollow for this use case.** What would the server send mid-stream? "You're not a member anymore" (extremely rare, agent learns from response or SSE). "Per-chunk ack" (unnecessary — TCP guarantees delivery). "Backpressure" (HTTP handles natively via TCP flow control). None justify the complexity cost.
* **Verdict:** Rejected. WebSocket solves a problem we don't have (bidirectional mid-stream communication) while introducing problems we don't want (statefulness, heartbeats, async client code, coupled failure domains, horizontal scaling friction).

**Candidate C: NDJSON streaming POST (CHOSEN)**

A single HTTP POST request with a streaming NDJSON body. The client writes content lines to the request body as tokens are generated. The server reads line-by-line using `bufio.NewScanner(r.Body)`, appending each to S2 in real-time. When the client closes the body, the server responds.

* **HTTP-native.** `Transfer-Encoding: chunked` is built into HTTP/1.1. Every proxy, firewall, and load balancer on earth understands it. HTTP/2 streams it natively via DATA frames.
* **Agent and developer ergonomics.** No specialized client library needed. Python: `requests.post(data=generator())`. Shell: `pipe | curl --data-binary @-`. An AI coding agent can build the scaffolding from documentation alone — 12 lines of sync Python.
* **Separation of concerns.** SSE for reads, NDJSON POST for writes — decoupled network paths. If the write drops, the read continues. Retry is just a new POST.
* **Server simplicity.** The handler is a sequential `for scanner.Scan()` loop. Standard HTTP middleware works unchanged. Disconnect detection is automatic via `r.Context().Done()`. No goroutines, no connection registry, no heartbeat logic.
* **Stateless for horizontal scaling.** Each request is a standard HTTP request. No sticky sessions, no connection affinity. Resources are proportional to concurrent streaming messages (seconds to ~1 minute each), not total connected agents.
* **Industry precedent.** Elasticsearch Bulk API, ClickHouse HTTP interface, OpenObserve — all use NDJSON over HTTP POST for streaming writes in production at scale.

**Acknowledged tradeoffs:**
* **Proxy buffering.** Some reverse proxies (nginx default, Kubernetes ingress-nginx) buffer chunked POST bodies. Mitigation: we control deployment infra (Fly.io supports streaming); document `proxy_request_buffering off` for self-hosted. AI agents typically don't run behind buffering corporate proxies. Note: WebSocket has a symmetric problem — many corporate firewalls block or interfere with WebSocket upgrade.
* **No mid-stream server-to-client communication.** The response comes only after the body completes. Mitigation: the SSE read connection provides real-time feedback; mid-stream write errors are extremely rare edge cases.
* **Read-once body breaks automatic retries.** If connection drops at 90%, the client can't resend the same stream. Mitigation: for LLM output, you regenerate. `message_id` serves as idempotency key for the start event.

**What would change this decision:** If we needed true bidirectional mid-stream negotiation (e.g., server-side content moderation that halts generation, or collaborative editing where two agents interleave tokens on the same message), WebSocket would be necessary. For one-directional token streaming from agent to server, NDJSON POST is strictly superior on every dimension that matters for this system.

**Challenge 4: "SSE for reads" — why not just polling?**
Polling: `GET /conversations/:cid/messages?after=seq_num` every N seconds.
* **Why SSE is better:**
    * Latency: SSE delivers events in <50ms (S2 tailing latency). Polling at 1-second intervals adds up to 1 second of latency.
    * Efficiency: SSE is one long-lived connection. Polling is a new HTTP request every interval.
    * Simplicity for the client: SSE auto-reconnects with `Last-Event-ID`. Polling requires the client to manage the cursor.
* **Why polling might be acceptable:**
    * Some environments don't support long-lived connections.
    * Polling is dead simple to implement and test.
* **Position:** SSE as primary. Add the history GET endpoint for polling-style access as a fallback. Both read from the same S2 stream.

**Challenge 5: "SQLite for metadata" — should we use PostgreSQL?**
* **Position: PostgreSQL chosen.** SQLite fails at scale due to single-writer lock, database-level locking, no concurrent write connections, no `LISTEN/NOTIFY`, no native UUID type, no `unnest()` batch operations, and embedded single-process constraint. PostgreSQL provides row-level locking, concurrent writers via pgxpool, batch upserts, native UUID, and a clear scaling path. Neon serverless PostgreSQL: fully managed, free tier, zero ops. See Section 1.6 for the complete design.

---

### Radical Alternative: The "Conversation as a Stream Machine" Model

Instead of treating S2 as a dumb append-only log, treat each conversation stream as a state machine. Every record is a state transition. The current state of the conversation is the result of replaying all transitions from the beginning.

**State includes:**
* Current members
* In-progress messages (started but not ended)
* Complete message count
* Per-agent metadata (last active time, message count)

This means: the S2 stream is the single source of truth for everything. No separate membership table in PostgreSQL needed — membership is derived from `agent_joined` and `agent_left` events on the stream. Cursors could be stored as special events on a per-agent metadata stream.

**Why this is better:** Single source of truth. No split-brain between PostgreSQL and S2. Replay the stream → reconstruct all state. Perfect for disaster recovery and debugging.

**Why I'm not doing it:** It makes every membership check require reading the stream (or maintaining an in-memory materialized view). PostgreSQL is a fine materialized view of conversation metadata. The hybrid approach (S2 for messages + PostgreSQL for metadata) is simpler to implement and reason about. Document this alternative in `FUTURE.md` as the "event sourcing" evolution.

---

## Phase 3: The "Stand-Out" Strategy

### The Wow Factor

Implement a `/conversations/:cid/replay` endpoint that returns the full conversation as a human-readable (and agent-readable) transcript, reconstructing complete messages from the raw event stream. This demonstrates:
* Deep understanding of the event model
* Ability to transform low-level stream events into high-level domain objects
* Practical utility for debugging and observability

Additionally, deploy two Claude-powered agents with different personalities that can converse with each other AND with external agents. Example: a "teacher" agent and a "student" agent. When the evaluator connects their own agent, they can observe or join a multi-party conversation. This demonstrates the group conversation feature working end-to-end with real AI agents.

### Testing Strategy

**Unit tests (`*_test.go` alongside each package):**
* Event serialization/deserialization (record → JSON → record roundtrip)
* Membership validation logic (invite checks, leave checks, last-member rejection)
* Cursor update batching logic
* Message assembly from interleaved events (demultiplexing test)

**Integration tests (`tests/integration_test.go`):**
* Full conversation lifecycle: register agents → create conversation → invite → send messages → read history → leave
* Streaming write + streaming read: start message → append chunks → end → verify SSE receives all chunks in real-time
* Concurrent writers: two agents stream messages simultaneously → verify interleaved events → verify both messages fully reconstructable
* Disconnect/reconnect: send messages → disconnect reader → send more messages → reconnect → verify reader catches up from cursor
* Leave terminates connection: agent is reading SSE → another member (or self) calls leave → SSE connection closes
* Abort on crash: agent starts streaming message → never sends end → verify `message_abort` appears after timeout
* Invite gives history access: create conversation → send messages → invite new agent → new agent reads full history

**Edge case tests:**
* Last member cannot leave
* Invite nonexistent agent returns error
* Invite existing member is no-op
* Write to conversation after leaving returns 403
* Read from conversation after leaving returns 403
* Empty conversation (no messages) SSE stream: connects, waits, receives events when messages arrive
* Very long message (close to 1 MiB): verify it works within S2 limits

**Load tests (optional, documented in `DESIGN.md`):**
* 100 agents, 50 conversations, concurrent streaming writes and reads
* Measure: latency (write ack, read delivery), throughput (messages/sec), memory usage

**Observability**
* Structured logging (zerolog or slog): every API request, every S2 operation, every connection lifecycle event
* Metrics: active SSE connections, messages/sec, S2 latency, API response times
* Health endpoint: `GET /health` → checks PostgreSQL and S2 connectivity

---

### Three-Layer Synthesis

**Layer 1: Tried & True (The Boring Way)**
PostgreSQL for everything. Messages table with `conversation_id`, `sender_id`, `content`, `created_at`. REST API. Polling for new messages. WebSocket bolted on for "real-time." This is how most chat apps start.
* *Problem:* No streaming. Polling introduces latency. WebSocket for real-time means maintaining connection state, handling reconnections, and duplicating the message delivery path. Messages are stored as complete units — no way to stream tokens as they're generated.

**Layer 2: New & Popular (The Trendy Way)**
Kafka/Redis Streams for pub/sub. gRPC for streaming. Kubernetes for deployment. This is the "cloud-native" approach.
* *Critique:* Massively over-engineered for this problem. Kafka requires cluster management, topic configuration, consumer group coordination. gRPC requires protobuf compilation, client codegen, and is hostile to AI coding agents that want to use curl. Kubernetes is a deployment distraction. You'd spend more time on infrastructure than on the actual messaging logic.

**Layer 3: First Principles (The Right Way)**
The fundamental truth: A conversation is a durable, ordered sequence of events with real-time tailing. That's a stream. S2 is a managed stream service. The server's only job is protocol translation and access control.
* This means:
    * No message broker between the API and storage. S2 IS both.
    * No separate "real-time" layer. S2 tailing IS real-time delivery.
    * No complex ordering algorithms. S2 sequencing IS the total order.
    * No fan-out mechanism. S2 concurrent readers IS fan-out.

The result: a Go server with ~1500 lines of code that does exactly what the spec asks, nothing more. The complexity budget goes into the messaging semantics (event model, streaming writes, cursor management, crash recovery), not into infrastructure wrangling.

**What would make this 10x better for 2x the effort:** Add a message-level API on top of the event-level API. The event stream (SSE) gives you raw events. A message-level API reconstructs complete messages, supports pagination, search, and filtering. This transforms the system from "a stream you tail" into "a conversation you interact with." The `/replay` and `/messages` endpoints in this plan are the beginning of this.

---

## Implementation Roadmap

### Build Order (Critical Path)

* **Step 1: Project scaffolding (30 min)**
    * `go mod init`, directory structure, `main.go` with chi router
    * PostgreSQL (Neon) connection setup, schema migration via embedded SQL with `CREATE TABLE IF NOT EXISTS`
    * Health endpoint
    * Makefile with build/run/test targets
* **Step 2: S2 client wrapper (1 hr)**
    * Provision S2 account (free tier, $10 credits), get auth token
    * Create basin `agentmail` with Express storage class, `CreateStreamOnAppend: true`, arrival timestamping, 28-day retention
    * Implement `AppendEvents` (unary), `OpenAppendSession` (pipelined), `OpenReadSession` (tailing), `ReadRange` (bounded), `CheckTail`
    * Test against real S2 (integration test)
* **Step 3: Agent registry (30 min)**
    * `POST /agents` endpoint
    * PostgreSQL CRUD via sqlc-generated code
    * Middleware: extract `X-Agent-ID` header, validate agent exists
* **Step 4: Conversation management (1.5 hr)**
    * Create, list, invite, leave endpoints
    * Membership checks
    * S2 auto-creates stream on first `agent_joined` append via `CreateStreamOnAppend: true`
    * System events (`agent_joined`, `agent_left`) written to S2
* **Step 5: Complete message send (1 hr)**
    * `POST /conversations/:cid/messages`
    * Batch write to S2 (`message_start` + `message_append` + `message_end`)
    * Return `message_id` and `seq`
* **Step 6: SSE read stream (2 hr)**
    * `GET /conversations/:cid/stream`
    * S2 read session → SSE event translation
    * Cursor lookup on connect, cursor update on delivery
    * Connection registry for leave termination
    * Auto-tail when caught up
* **Step 7: Streaming write — NDJSON POST (1.5 hr)**
    * `POST /conversations/:cid/messages/stream` with streaming NDJSON body
    * Server-side: `bufio.NewScanner(r.Body)` loop, per-line S2 append
    * `message_abort` on disconnect (EOF / context cancellation)
    * Recovery sweep on startup for unterminated messages
* **Step 8: Claude agent (2 hr)**
    * Register agent on startup
    * SSE listener per conversation
    * Claude API streaming integration
    * Token forwarding via streaming write API
    * Multi-conversation support
* **Step 9: Testing (2 hr)**
    * Integration test suite covering all scenarios listed above
    * Concurrent writer test
    * Disconnect/reconnect test
* **Step 10: Documentation (1.5 hr)**
    * `DESIGN.md`: every design decision with rationale
    * `FUTURE.md`: production-grade redesign
    * `CLIENT.md`: self-contained agent onboarding doc (including scaffolding examples)
* **Step 11: Deployment (1 hr)**
    * Dockerfile (multi-stage build)
    * `fly.toml` configuration
    * Deploy to Fly.io
    * Verify Claude agent is running and reachable

### Files to Create

```text
agentmail-take-home/
├── cmd/server/main.go              # Entry point, config, startup
├── internal/
│   ├── api/
│   │   ├── router.go               # Chi router, middleware
│   │   ├── agents.go               # POST /agents
│   │   ├── conversations.go        # Conversation CRUD
│   │   ├── messages.go             # Message send (complete POST + NDJSON streaming POST)
│   │   ├── sse.go                  # SSE streaming read
│   │   ├── history.go              # GET /messages (reconstructed)
│   │   └── middleware.go           # Agent ID extraction/validation
│   ├── store/
│   │   ├── schema/
│   │   │   └── schema.sql          # Complete DDL
│   │   ├── queries/
│   │   │   ├── agents.sql          # sqlc queries for agents
│   │   │   ├── conversations.sql   # sqlc queries for conversations
│   │   │   ├── members.sql         # sqlc queries for members
│   │   │   ├── cursors.sql         # sqlc queries for cursors
│   │   │   └── in_progress.sql     # sqlc queries for in-progress message tracking
│   │   ├── db/                     # sqlc generated code (git-committed)
│   │   │   ├── db.go
│   │   │   ├── models.go
│   │   │   └── *.sql.go
│   │   ├── postgres.go             # Store implementation (composes sub-stores)
│   │   ├── cursor_cache.go         # In-memory cursor hot tier
│   │   ├── membership_cache.go     # In-memory membership LRU cache
│   │   ├── conn_registry.go        # Active connection tracking
│   │   ├── s2.go                   # S2 stream operations
│   │   └── s2_test.go              # S2 integration test
│   ├── model/
│   │   ├── events.go               # Event types, serialization
│   │   └── types.go                # Agent, Conversation, Member
│   ├── agent/
│   │   ├── claude.go               # Claude API integration
│   │   └── runner.go               # Agent lifecycle management
│   └── service/
│       ├── conversations.go        # Business logic layer
│       └── messages.go             # Message orchestration
├── tests/
│   ├── integration_test.go         # Full lifecycle tests
│   ├── streaming_test.go           # SSE + streaming write tests
│   └── concurrent_test.go          # Concurrent writer tests
├── docs/
│   ├── DESIGN.md                   # Design decisions
│   ├── FUTURE.md                   # No-restrictions redesign
│   └── CLIENT.md                   # Agent client documentation
├── go.mod
├── go.sum
├── Dockerfile
├── fly.toml
├── Makefile
├── sqlc.yaml                       # sqlc code generation config
└── README.md                       # (already exists)
```

### Key Dependencies

* `github.com/go-chi/chi/v5` — HTTP router
* `github.com/s2-streamstore/s2-sdk-go` — S2 client
* `github.com/anthropics/anthropic-sdk-go` — Claude API
* `github.com/google/uuid` — UUIDv7 generation (v1.6.0+, RFC 9562)
* `github.com/jackc/pgx/v5` — PostgreSQL driver (native interface + pgxpool)
* sqlc (build-time CLI) — Type-safe SQL code generation
* `github.com/rs/zerolog` — Structured logging

---

## Blast Radius & Reversibility

| Decision | Worst Case if Wrong | Reversibility |
| :--- | :--- | :--- |
| **Go as language** | Slower to prototype than Python | Low — full rewrite. But Go is the right call for this domain. |
| **S2 as stream storage** | S2 has an outage or API breaking change | Medium — swap S2 client for direct S3 + custom sequencing. The S2 wrapper (`internal/store/s2.go`) isolates the blast radius. |
| **PostgreSQL (Neon) for metadata** | Neon outage or cold-start latency | Medium — fallback to Fly.io managed Postgres or self-hosted. Schema and queries are standard PostgreSQL. |
| **SSE for reads** | Client doesn't support SSE | High — history polling endpoint exists as alternative. |
| **NDJSON streaming POST for writes** | Proxy buffers the body, losing real-time streaming | High — complete message POST works universally as fallback. Proxy config (`proxy_request_buffering off`) resolves for self-hosted. |
| **One stream per conversation** | Hot conversation with many writers | Low — but S2 handles 100 MiBps per stream. LLM token rates won't come close. |
| **Event-based record model** | Complexity in demultiplexing | Medium — could simplify to complete-message-only model, but lose streaming. The event model is the correct design. |

---

## Verification Plan

1.  **Local smoke test:** Start server, register two agents, create conversation, send messages, verify SSE delivery.
2.  **Streaming test:** Start streaming write from agent A, verify agent B's SSE receives chunks in real-time.
3.  **Concurrent test:** Two agents stream messages simultaneously, verify both messages reconstructable from interleaved events.
4.  **Reconnect test:** Disconnect agent B's SSE, send messages from A, reconnect B, verify B catches up from cursor.
5.  **Leave test:** Agent B leaves, verify B's SSE connection closes, verify B can't read/write.
6.  **Claude agent test:** Invite Claude agent to conversation, send message, verify Claude responds with streaming tokens.
7.  **Integration test suite:** `go test ./tests/... -v` — all scenarios pass.
8.  **Deployed test:** Hit the live Fly.io URL, register agent, create conversation with Claude agent, have a conversation.