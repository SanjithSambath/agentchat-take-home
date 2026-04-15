Got it — the file upload handoff is failing on your side.

Here’s the formatted spec directly in chat so you can copy it:

````md
# AgentMail: Exhaustive Spec Analysis & Implementation Plan

## Context

What: A stream-based agent-to-agent messaging service with durable history, real-time token streaming, and group conversations.

Why this matters: The spec is testing three things simultaneously: (1) systems design taste — can you model conversations as streams without overengineering, (2) infrastructure judgment — do you reach for the right primitives instead of gluing together a Rube Goldberg machine, (3) agent empathy — is the resulting API something an AI coding agent can use without a human holding its hand.

The fundamental insight: Conversations ARE streams. S2 gives you durable, ordered, replayable streams with real-time tailing. The server is a thin protocol translation layer between HTTP clients and S2 streams. Do not build a message broker. Do not build a queue. Do not add Kafka or Redis. The entire message storage and delivery layer is S2. The server handles identity, membership, routing, and protocol adaptation — nothing more.

---

## Architecture Overview

```text
┌─────────────┐     HTTP POST      ┌──────────────┐     append
┌─────────┐
│  Agent (LLM)│ ──────────────────→│  Go Server   │ ──────────────→│   S2
  │
│             │     SSE stream     │              │     tail        │
(stream │
│             │ ←──────────────────│              │ ←──────────────│per
conv)│
└─────────────┘                    └──────┬───────┘
└─────────┘
                                          │
                                          │ metadata
                                          ▼
                                    ┌──────────┐
                                    │  SQLite   │
                                    │ (agents,  │
                                    │  convos,  │
                                    │  cursors) │
                                    └──────────┘
````

**Two storage layers, cleanly separated:**

* S2: All message content. One stream per conversation. Records are events (message_start, message_append, message_end, system events). S2 handles ordering, durability, replay, real-time tailing.
* SQLite: All metadata. Agent registry, conversation membership, read cursors. Embedded, zero-config, transactional.

**Transport:**

* HTTP POST for all writes (register, create, invite, leave, send messages, stream chunks)
* SSE for streaming reads (tail a conversation in real-time, replay from cursor)
* WebSocket as optional upgrade for bidirectional real-time sessions

Language: Go. First-class S2 SDK (v1 HTTP API). Goroutines handle thousands of concurrent SSE/WebSocket connections. Single binary deployment. The problem domain (concurrent streaming, connection management, protocol translation) is Go's sweet spot.

---

## Phase 1: Exhaustive Component Breakdown

### 1.1 Agent Registry

What the spec says: Register provisions a new agent, returns an opaque ID. No metadata. No authentication. Each call creates a new identity. The ID scopes all access.

**Hidden complexities:**

* The agent ID is the sole credential. Leaking it means impersonation. The spec says no auth, so we accept this — but the ID should be a cryptographically random UUID, not a sequential integer, to prevent enumeration.
* "No metadata" means no name, no description, nothing. This is intentional — the spec wants you to avoid building user management.
* The spec doesn't mention agent deletion. Agents are permanent. An agent that's a member of conversations cannot be garbage-collected without orphaning messages.

**Implementation:**

* `POST /agents` → generates UUIDv4, inserts into agents table, returns `{ "agent_id": "uuid" }`
* SQLite table: `agents(id TEXT PRIMARY KEY, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)`
* No request body needed. No idempotency concerns (each call creates a new agent).
* Validate agent existence on every subsequent API call via middleware.

**Edge cases:**

* Concurrent registration: no conflicts (UUIDs are unique)
* Invalid agent ID in subsequent requests: return 404 with clear error message
* Empty database on fresh deploy: first agent registration works with no special handling

Files: `internal/api/agents.go`, `internal/store/sqlite.go`

---

### 1.2 Conversation Management

What the spec says: Create, list, invite, leave. Server-assigned IDs. Creator is auto-member. Multiple conversations can have identical participant sets. Any member can invite. No roles. Invite is idempotent for existing members, error for nonexistent agents. Leave by last member is rejected. Prior messages remain after leave. Active streams terminated on leave. Re-invite is allowed.

**Hidden complexities:**

**Create:**

* Must atomically: (a) create conversation in SQLite, (b) add creator as member, (c) create S2 stream for the conversation.
* If S2 stream creation fails after SQLite insert, we have an orphaned conversation. Handle this: create S2 stream first, then insert into SQLite. If SQLite fails, we have an empty S2 stream (harmless).
* The S2 stream name should encode the conversation ID for easy lookup (e.g., `conv-{conversation_id}`).

**List:**

* Returns conversations where the agent is currently a member.
* Each entry needs conversation ID and current member list.
* This is a join query: conversations JOIN members WHERE agent_id = ?, then for each conversation, fetch all members.
* For efficiency: single query with `GROUP_CONCAT` or similar.
* Consider: should this include conversations the agent has left? The spec says "all conversations the agent is a member of" — current members only.

**Invite:**

* Idempotent for existing members (return 200, not error).
* Error for nonexistent agents (return 404).
* Error for nonexistent conversations (return 404).
* Only members can invite. Check membership first.
* Write an `agent_joined` system event to the S2 stream so it appears in conversation history.
* The invitee gets access to full history — this is automatic since they'll read from the S2 stream which contains everything.

**Leave:**

* Reject if the agent is the last member (return 400 or 409).
* Write an `agent_left` system event to the S2 stream.
* Critical: terminate active SSE/WebSocket connections for this agent on this conversation.

  * Requires a connection registry: `map[(agent_id, conversation_id)] → cancel_func`
  * On leave, look up and call cancel, which closes the SSE response.
* Critical: abort any in-progress streaming message from this agent.

  * Check if there's an open `message_start` without a corresponding `message_end`.
  * If so, write a `message_abort` event before the `agent_left` event.
* After leave, reject all read/write requests from this agent for this conversation.
* The agent's read cursor can be preserved (for potential re-invite) or deleted (simpler).

**Edge cases:**

* Race condition: agent A invites agent B while agent B leaves simultaneously. Resolve by checking membership under a transaction/lock.
* Agent invites themselves: already a member, idempotent no-op.
* Leave + immediate re-invite: should work. The agent gets a fresh start but full history is visible.
* Create conversation with no one to talk to: valid. An agent can create a conversation with itself. The spec explicitly says "A single agent can create a conversation with itself."

Files: `internal/api/conversations.go`, `internal/store/sqlite.go`

SQLite tables:

```sql
conversations(id TEXT PRIMARY KEY, created_at TIMESTAMP)
members(conversation_id TEXT, agent_id TEXT, joined_at TIMESTAMP, PRIMARY KEY(conversation_id, agent_id))
```

---

### 1.3 Message Streaming — Write Path

This is the hardest part of the spec. The spec requires streaming writes (token by token) AND the ability to send complete messages. Two distinct write patterns, both writing to the same S2 stream.

The unit of communication: An S2 record is the atomic unit on the stream. A "message" is a logical unit composed of one or more records. This distinction is the core design decision.

**Record types (events on the S2 stream):**

**Event Type:** `message_start`
**Purpose:** Opens a new message
**Headers:** `type=message_start`
**Body (JSON):** `{"message_id":"uuid","sender_id":"uuid"}`

**Event Type:** `message_append`
**Purpose:** Content chunk
**Headers:** `type=message_append`
**Body (JSON):** `{"message_id":"uuid","content":"tokens"}`

**Event Type:** `message_end`
**Purpose:** Closes a message
**Headers:** `type=message_end`
**Body (JSON):** `{"message_id":"uuid"}`

**Event Type:** `message_abort`
**Purpose:** Marks abandoned message
**Headers:** `type=message_abort`
**Body (JSON):** `{"message_id":"uuid","reason":"disconnect"}`

**Event Type:** `agent_joined`
**Purpose:** System event: invite
**Headers:** `type=agent_joined`
**Body (JSON):** `{"agent_id":"uuid"}`

**Event Type:** `agent_left`
**Purpose:** System event: leave
**Headers:** `type=agent_left`
**Body (JSON):** `{"agent_id":"uuid"}`

**Why this event model:**

* Streaming: Each `message_append` is immediately durable and visible to readers. A reader tailing the stream sees tokens in real-time.
* Message boundaries: `message_start`/`message_end` let readers reconstruct complete messages from the event stream.
* Crash safety: If an agent crashes mid-message (sends `message_start` + some `message_append` but no `message_end`), the server can detect this and emit `message_abort`. Readers know the message is incomplete.
* Concurrent writes: Two agents streaming messages simultaneously produce interleaved records on the stream. The `message_id` field on every record lets readers demultiplex — group events by `message_id` to reconstruct each agent's message independently.
* Atomic complete messages: For a non-streaming send, the server writes `[message_start, message_append, message_end]` as a single S2 batch append — atomic, all-or-nothing.

**API for complete message send:**

```http
POST /conversations/:cid/messages
Header: X-Agent-ID: <agent_id>
Body: { "content": "Hello, how are you?" }
```

**Response:**

```json
{ "message_id": "uuid", "seq": 42 }
```

Server writes three records to S2 in one batch: `message_start`, `message_append` (full content), `message_end`. Atomic.

**API for streaming message send:**

```http
# Step 1: Start message
POST /conversations/:cid/messages/stream
Header: X-Agent-ID: <agent_id>
Body: {}

Response: 201 Created
{ "message_id": "uuid" }

# Step 2: Append chunks (repeat)
POST /conversations/:cid/messages/:mid/append
Header: X-Agent-ID: <agent_id>
Body: { "content": "Hello, " }

Response: 200 OK
{ "seq": 43 }

# Step 3: End message
POST /conversations/:cid/messages/:mid/end
Header: X-Agent-ID: <agent_id>
Body: {}

Response: 200 OK
{ "seq": 45 }
```

**Hidden complexities:**

**When is a write "successful"?**

* S2 acknowledges after regional durability (data is in S3). Our server returns success to the client only after S2 acknowledges. This means: a successful write is durable. If the server crashes after S2 ack but before sending the HTTP response, the write is durable but the client thinks it failed. The client retries, creating a duplicate. Mitigation: use the `message_id` as an idempotency key — if a `message_start` for this `message_id` already exists, return the existing `seq` instead of writing again.

**Concurrent writers:**

* Two agents streaming simultaneously to the same conversation: their records interleave on the S2 stream. This is correct behavior. S2 assigns sequence numbers, guaranteeing total ordering. The interleaving is the ordering — it represents the temporal reality of concurrent composition.
* Readers demultiplex by `message_id`. Each reader maintains a map of `message_id → accumulated_content` and assembles complete messages from interleaved chunks.

**Orphaned messages (crash during composition):**

* Server tracks in-progress messages: `map[message_id] → {agent_id, conversation_id, last_activity_time}`
* Background goroutine checks for stale in-progress messages (no append/end for >30 seconds).
* On detection: write `message_abort` event to the stream, clean up tracking state.
* On agent disconnect: immediately check for in-progress messages from that agent and abort them.
* On conversation leave: same — abort in-progress messages first.

**Validation on write:**

* Agent must be a member of the conversation (check SQLite).
* For append/end: the `message_id` must exist and belong to this agent (check in-memory tracker).
* For append/end: the message must not already be ended or aborted.
* Content must not be empty for append (or allow empty? Spec doesn't say — allow it, LLMs sometimes emit empty tokens).
* Content must be plaintext (spec says "purely plaintext communication").

Files: `internal/api/messages.go`, `internal/store/s2.go`, `internal/model/events.go`

---

### 1.4 Message Streaming — Read Path

**SSE (primary read transport):**

```http
GET /conversations/:cid/stream
Header: X-Agent-ID: <agent_id>
Query: ?from=<seq_num>
```

**Response:**

```http
200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive

id: 42
event: message_start
data:
{"message_id":"m1","sender_id":"a1","timestamp":"2026-04-14T10:00:00Z"}

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

1. Client sends `GET /conversations/:cid/stream` with optional `?from=seq` or `Last-Event-ID`.
2. Server checks agent membership.
3. Server determines starting position:

   * If `from` query param: use that sequence number.
   * If `Last-Event-ID` header: use that + 1.
   * If neither: look up agent's stored cursor from SQLite.
   * If no stored cursor: start from sequence 0 (beginning of conversation).
4. Server opens S2 read session from the starting position.
5. S2 returns historical records (replay/catchup phase).
6. Server translates each S2 record to an SSE event and writes to the HTTP response.
7. When S2 reaches the end of the stream, it transitions to real-time tailing.
8. New records appended to the S2 stream are immediately forwarded as SSE events.
9. Server periodically updates the agent's read cursor in SQLite.

**Connection lifecycle:**

* Register connection in the connection registry: `activeConnections[(agent_id, cid)] = cancelFunc`
* On client disconnect: detected via `r.Context().Done()`, clean up, update cursor.
* On leave: server calls `cancelFunc`, which closes the response, client sees connection close.
* On server shutdown: graceful drain — send final event, close all connections.

**Cursor management:**

* Store in SQLite: `cursors(agent_id TEXT, conversation_id TEXT, seq_num INTEGER, updated_at TIMESTAMP)`
* Update strategy: batch updates — every 10 events or every 5 seconds, whichever comes first.
* On disconnect: flush final cursor position.
* On reconnect: resume from stored cursor.
* Result: at-least-once delivery. Agent may re-receive a few events after crash. Events have sequence numbers and message IDs for client-side deduplication.

**WebSocket (optional bidirectional transport):**

```http
GET /conversations/:cid/ws → WebSocket upgrade
Header: X-Agent-ID: <agent_id>
Query: ?from=<seq_num>
```

**Server → Client:**

```json
{"seq":42,"event":"message_start","data":{"message_id":"m1","sender_id":"a1"}}
```

**Client → Server:**

```json
{"action":"send","content":"Hello!"}
{"action":"stream_start"}
{"action":"stream_append","message_id":"m1","content":"tok"}
{"action":"stream_end","message_id":"m1"}
```

WebSocket combines read and write on one connection. Useful for agents that want full-duplex real-time interaction without managing separate HTTP requests.

**History endpoint (non-streaming):**

```http
GET /conversations/:cid/messages
Header: X-Agent-ID: <agent_id>
Query: ?limit=50&before=<seq_num>
```

**Response:**

```json
{
  "messages": [
    {"message_id":"m1","sender_id":"a1","content":"Hello, how are you?","seq_start":42,"seq_end":45,"complete":true},
    {"message_id":"m2","sender_id":"a2","content":"I'm well, thanks","seq_start":46,"seq_end":49,"complete":true}
  ]
}
```

This reconstructs complete messages from the raw event stream. Reads from S2, groups events by `message_id`, assembles content, returns structured messages. Useful for agents that want to see conversation history without processing raw events.

**Edge cases:**

* Agent connects to conversation they're not a member of: 403.
* Agent connects to nonexistent conversation: 404.
* Agent connects while another SSE connection for the same `(agent, conversation)` is active: close the old connection, use the new one. Only one active reader per agent per conversation.
* `from seq_num` is beyond the stream's current tail: start tailing immediately, wait for new events.
* `from seq_num` is before the stream's trim point (if trimming is implemented): return error or start from earliest available.
* Slow reader: S2 handles backpressure. If the client can't keep up, the HTTP response buffer fills, Go's `http.Flusher` blocks, and the S2 read session slows down. No unbounded memory growth.

Files: `internal/api/sse.go`, `internal/api/websocket.go`, `internal/api/history.go`

---

### 1.5 S2 Stream Storage Layer

Stream topology: One S2 stream per conversation. Stream name: `conv-{conversation_id}`.

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

**S2 operations used:**

| Operation      | When                             | S2 API                                             |
| -------------- | -------------------------------- | -------------------------------------------------- |
| Create stream  | On conversation create           | `POST /streams`                                    |
| Append records | On message send/chunk            | `POST /streams/{id}/records` with batch of records |
| Read records   | On SSE connect (catchup)         | `GET /streams/{id}/records?seq_num=N`              |
| Tail stream    | On SSE connect (real-time)       | `GET /streams/{id}/records` (no count → auto-tail) |
| Check tail     | Health checks, cursor validation | `GET /streams/{id}/records/tail`                   |

**S2 client wrapper (`internal/store/s2.go`):**

* Initialize with S2 auth token (from env var)
* Basin handle (one basin for the whole service)
* Methods: `CreateStream(convID)`, `AppendEvents(convID, []Event)`, `ReadFrom(convID, seqNum) → channel`, `TailStream(convID) → channel`
* `ReadFrom` returns a Go channel that emits records. The caller ranges over the channel. When the stream transitions to tailing, the channel keeps emitting as new records arrive. When the caller cancels the context, the channel closes and the S2 session ends.

**Record encoding:**

* S2 record headers: `type → event type string` (for filtering without parsing body)
* S2 record body: JSON-encoded event payload
* Why JSON over protobuf: the entire API is JSON. Protobuf adds a compilation step, client codegen dependency, and cognitive overhead for AI agents reading the code. JSON is self-describing, debuggable, and curl-friendly. The record bodies are small (message chunks), so serialization performance is irrelevant.

**Concurrency control:**

* No fencing tokens needed. Multiple agents write concurrently — this is desired behavior.
* No optimistic sequence numbers needed. We don't care about write ordering between agents; S2's sequencing IS the ordering.
* S2 atomic batch append for complete messages (3 records in one call). Individual chunks are single-record appends.

---

### 1.6 Metadata Storage (SQLite)

**Why SQLite:**

* Zero external dependencies. Embedded in the Go binary.
* Sufficient for this scale (the spec isn't testing database expertise).
* Transactional. ACID. WAL mode for concurrent readers.
* Single file, easy to back up, easy to deploy.
* Keeps the focus on the core messaging problem, not database administration.

**Schema:**

```sql
CREATE TABLE agents (
    id TEXT PRIMARY KEY,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE conversations (
    id TEXT PRIMARY KEY,
    s2_stream_name TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE members (
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    agent_id TEXT NOT NULL REFERENCES agents(id),
    joined_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (conversation_id, agent_id)
);

CREATE TABLE cursors (
    agent_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    seq_num INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (agent_id, conversation_id)
);
```

**Indexes:**

* `members(agent_id)` — for listing conversations by agent
* `cursors(agent_id, conversation_id)` — already the primary key

Connection management: Single `*sql.DB` with WAL mode enabled. `PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`

Files: `internal/store/sqlite.go`

---

### 1.7 Read Cursor Management

Design: Server-managed cursors. The server tracks where each agent has read to in each conversation.

**Why server-managed (not client-managed):**

* The spec recommends it: "We recommend that the server track each agent's read position."
* AI agents are stateless between sessions. They can't reliably persist their own cursors.
* Server-side cursors mean reconnection is seamless — no state to pass from client.

**Cursor update strategy:** Batch updates to reduce SQLite write pressure.

* In-memory: track the latest delivered `seq_num` per `(agent, conversation)`.
* Flush to SQLite: every 10 events delivered OR every 5 seconds, whichever first.
* On disconnect: immediate flush of final position.
* On reconnect: read cursor from SQLite, resume from there.

**Delivery semantics:** At-least-once.

* If server crashes between delivering an event and flushing the cursor, the event will be re-delivered on reconnect.
* Events carry sequence numbers and message IDs. Clients can deduplicate if needed.
* Why not exactly-once: requires client acknowledgments, adds protocol complexity, and at-least-once with idempotent processing is the industry standard for streaming systems.

---

### 1.8 Claude-Powered Agent

The spec requires: "at least one Claude-powered agent running on it that we can converse with."

**Implementation:** A standalone goroutine (or separate process) that:

1. Registers itself as an agent on startup.
2. Creates a "lobby" conversation (or joins existing ones when invited).
3. Listens to all its conversations via SSE.
4. When a complete message arrives (`message_end` event), sends the accumulated conversation history to Claude API.
5. Streams Claude's response tokens back to the conversation via the streaming write API.
6. Maintains conversation context (message history) in memory.

**Architecture:**

```text
┌──────────────────────────────────────────┐
│ Claude Agent Process                     │
│                                          │
│  ┌──────────┐    SSE     ┌────────────┐  │
│  │ Listener │ ←───────── │ Our Server │  │
│  └────┬─────┘            └──────┬─────┘  │
│       │ message received        ↑        │
│       ▼                         │        │
│  ┌──────────┐             POST /append   │
│  │ Claude   │  stream     ┌─────┴─────┐  │
│  │ API      │ ──────────→ │ Responder │  │
│  └──────────┘  tokens     └───────────┘  │
└──────────────────────────────────────────┘
```

**Claude API integration:**

* Use `github.com/anthropics/anthropic-sdk-go` for the Anthropic SDK.
* Streaming mode: receive tokens from Claude, forward each chunk via `POST` to `/messages/:mid/append`.
* System prompt: "You are a helpful AI assistant participating in a group conversation. Be concise and direct."
* Model: `claude-sonnet-4-20250514` (good balance of speed and quality for real-time chat).

**Multi-conversation handling:**

* One goroutine per active conversation SSE connection.
* Shared Claude API client with rate limiting.
* Message history stored per conversation in memory (`map[conversation_id] → []Message`).
* Truncate history if it exceeds context window (keep system prompt + last N messages).

**Edge cases:**

* Claude API rate limit: queue responses, retry with backoff.
* Claude API error: send an error message to the conversation ("Sorry, I encountered an error. Please try again.").
* Very long conversations: sliding window over message history.
* Multiple messages arrive before Claude finishes responding: queue them, respond sequentially.
* Agent is invited to a new conversation: start listening via new SSE connection.

Files: `internal/agent/claude.go`, `internal/agent/listener.go`

---

## Phase 2: Assumption Interrogation & Radical Alternatives

### Assumptions in the Spec (Explicit and Implicit)

**1. Assumption:** S2 is the right storage primitive
**Type:** Recommended
**My Position:** Agree. S2 is purpose-built for this exact problem. Using anything else means rebuilding what S2 already provides.

**2. Assumption:** Conversations need total ordering
**Type:** Implicit
**My Position:** Agree for single-conversation scope. Within one conversation, total ordering is essential — participants need a shared reality. Cross-conversation ordering is irrelevant.

**3. Assumption:** The server should mediate all access
**Type:** Explicit
**My Position:** Agree. Direct S2 access would leak implementation details and bypass membership checks.

**4. Assumption:** No authentication is needed
**Type:** Explicit
**My Position:** Agree for scope. Agent IDs as bearer tokens is sufficient. The spec is testing messaging design, not auth.

**5. Assumption:** Messages are plaintext only
**Type:** Explicit
**My Position:** Accept. Don't build rich formatting. Don't build file uploads. Stay focused.

**6. Assumption:** AI coding agents are the primary clients
**Type:** Explicit
**My Position:** This is the most important assumption. It means: prefer HTTP over WebSocket for writes. Prefer curl-friendly APIs. Prefer self-describing JSON. Avoid protocols that need client libraries.

**7. Assumption:** Real-time streaming matters
**Type:** Explicit
**My Position:** Agree. LLMs generate tokens incrementally. Showing tokens as they arrive is the whole point.

**8. Assumption:** Conversations have unbounded participants
**Type:** Implicit
**My Position:** Agree for the API. No limit on members. But practically, >100 members in a conversation would cause S2 write contention and reader fan-out issues. Not a concern for the take-home.

**9. Assumption:** Messages are small
**Type:** Implicit
**My Position:** Agree. LLM outputs are text. Individual chunks are a few tokens. Complete messages are a few KB at most. Well within S2's 1 MiB record limit.

**10. Assumption:** The server is a single instance
**Type:** Implicit
**My Position:** Accept for now. The take-home doesn't require horizontal scaling. But the design should not preclude it — stateless request handling + external storage (S2 + SQLite/Postgres) is naturally scalable.

### Challenges to Spec Assumptions

#### Challenge 1: "Use S2" — is there a simpler alternative?

Could we skip S2 and use PostgreSQL `LISTEN/NOTIFY` + a messages table? Yes, technically. But:

* You'd rebuild ordering, durability, and replay that S2 provides natively.
* `LISTEN/NOTIFY` is fire-and-forget — missed notifications are gone. You'd need polling as fallback.
* PostgreSQL isn't designed for high-throughput, low-latency stream tailing.
* The spec recommends S2 because the evaluators built S2. Using it shows you can learn and apply new infrastructure idiomatically.

**Verdict:** Use S2. It's the right tool and the evaluators want to see you use it well.

#### Challenge 2: "One stream per conversation" — what about one stream per agent?

Per-agent streams (inbox model) would mean: when agent A sends a message to conversation C with members [A, B, C], the server writes to B's inbox stream and C's inbox stream. Each agent tails only their own stream.

**Problems:**

* Write amplification: 1 message → N writes (one per member). Gets worse with large groups.
* Ordering: messages from different senders arrive in different order in different inboxes. No shared reality.
* Conversation reconstruction: to show conversation history, you'd need to merge-sort multiple agent streams. Brutal.
* Invitation: when agent D joins, you'd need to copy all historical messages to D's inbox. Or maintain a separate history stream anyway — which is just the per-conversation model.

**Verdict:** Per-conversation streams. Strictly superior for this use case.

#### Challenge 3: "HTTP POST for streaming writes" — is this the right transport?

Alternative: WebSocket for writes too. Agent opens a WebSocket, sends chunks as WebSocket messages.

**Tradeoffs:**

* WebSocket: lower per-chunk overhead (no HTTP headers per chunk), but requires WebSocket client library. Claude Code and curl both support WebSocket, but it's more complex.
* HTTP POST per chunk: higher overhead (full HTTP request per chunk), but trivially scriptable. Each chunk is an independent request. Failure of one chunk doesn't kill the whole message — the agent can retry just that chunk.
* Streaming HTTP POST (chunked transfer encoding): single HTTP request with streaming body. Efficient, but hard to implement correctly in most HTTP clients. If the connection drops mid-stream, you lose the whole request.

**Position:** Offer both HTTP POST (per-chunk) and WebSocket. HTTP POST is the default for simplicity. WebSocket is the upgrade path for performance. Most agents will use HTTP POST. The overhead of per-chunk HTTP requests is negligible for LLM token rates (~30–100 tokens/sec, so ~30–100 requests/sec, well within any server's capacity).

What would change my mind: If latency measurements show per-chunk HTTP adds >50ms overhead vs WebSocket, prioritize WebSocket. But for LLM token rates, this won't happen.

#### Challenge 4: "SSE for reads" — why not just polling?

Polling: `GET /conversations/:cid/messages?after=seq_num` every N seconds.

**Why SSE is better:**

* Latency: SSE delivers events in <50ms (S2 tailing latency). Polling at 1-second intervals adds up to 1 second of latency.
* Efficiency: SSE is one long-lived connection. Polling is a new HTTP request every interval.
* Simplicity for the client: SSE auto-reconnects with `Last-Event-ID`. Polling requires the client to manage the cursor.

**Why polling might be acceptable:**

* Some environments don't support long-lived connections.
* Polling is dead simple to implement and test.

**Position:** SSE as primary. Add the history GET endpoint for polling-style access as a fallback. Both read from the same S2 stream.

#### Challenge 5: "SQLite for metadata" — should we use PostgreSQL?

For the take-home: SQLite is the right choice. Zero ops, embedded, sufficient.

For production: PostgreSQL. WAL replication, connection pooling, better concurrent write handling, `LISTEN/NOTIFY` for cache invalidation.

**Position:** SQLite now. The abstraction layer (`internal/store/`) makes swapping trivial. Document PostgreSQL as the production upgrade in `FUTURE.md`.

---

### Radical Alternative: The "Conversation as a Stream Machine" Model

Instead of treating S2 as a dumb append-only log, treat each conversation stream as a state machine. Every record is a state transition. The current state of the conversation is the result of replaying all transitions from the beginning.

**State includes:**

* Current members
* In-progress messages (started but not ended)
* Complete message count
* Per-agent metadata (last active time, message count)

This means: the S2 stream is the single source of truth for everything. No separate membership table in SQLite needed — membership is derived from `agent_joined` and `agent_left` events on the stream. Cursors could be stored as special events on a per-agent metadata stream.

**Why this is better:** Single source of truth. No split-brain between SQLite and S2. Replay the stream → reconstruct all state. Perfect for disaster recovery and debugging.

**Why I'm not doing it:** It makes every membership check require reading the stream (or maintaining an in-memory materialized view). SQLite is a fine materialized view of conversation metadata. The hybrid approach (S2 for messages + SQLite for metadata) is simpler to implement and reason about. Document this alternative in `FUTURE.md` as the "event sourcing" evolution.

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
* Streaming write + streaming read: start message → append chunks → end → verify SSE receives all chunks in order
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

### Observability

* Structured logging (`zerolog` or `slog`): every API request, every S2 operation, every connection lifecycle event
* Metrics: active SSE connections, messages/sec, S2 latency, API response times
* Health endpoint: `GET /health` → checks SQLite and S2 connectivity

---

## Three-Layer Synthesis

### Layer 1: Tried & True (The Boring Way)

PostgreSQL for everything. Messages table with `conversation_id`, `sender_id`, `content`, `created_at`. REST API. Polling for new messages. WebSocket bolted on for "real-time." This is how most chat apps start.

**Problem:** No streaming. Polling introduces latency. WebSocket for real-time means maintaining connection state, handling reconnections, and duplicating the message delivery path. Messages are stored as complete units — no way to stream tokens as they're generated.

### Layer 2: New & Popular (The Trendy Way)

Kafka/Redis Streams for pub/sub. gRPC for streaming. Kubernetes for deployment. This is the "cloud-native" approach.

**Critique:** Massively over-engineered for this problem. Kafka requires cluster management, topic configuration, consumer group coordination. gRPC requires protobuf compilation, client codegen, and is hostile to AI coding agents that want to use curl. Kubernetes is a deployment distraction. You'd spend more time on infrastructure than on the actual messaging logic.

### Layer 3: First Principles (The Right Way)

The fundamental truth: A conversation is a durable, ordered sequence of events with real-time tailing. That's a stream. S2 is a managed stream service. The server's only job is protocol translation and access control.

This means:

* No message broker between the API and storage. S2 IS both.
* No separate "real-time" layer. S2 tailing IS real-time delivery.
* No complex ordering algorithms. S2 sequencing IS the total order.
* No fan-out mechanism. S2 concurrent readers IS fan-out.

The result: a Go server with ~1500 lines of code that does exactly what the spec asks, nothing more. The complexity budget goes into the messaging semantics (event model, streaming writes, cursor management, crash recovery), not into infrastructure wrangling.

What would make this 10x better for 2x the effort: Add a message-level API on top of the event-level API. The event stream (SSE) gives you raw events. A message-level API reconstructs complete messages, supports pagination, search, and filtering. This transforms the system from "a stream you tail" into "a conversation you interact with." The `/replay` and `/messages` endpoints in this plan are the beginning of this.

---

## Implementation Roadmap

### Build Order (Critical Path)

**Step 1: Project scaffolding (30 min)**

* `go mod init`, directory structure, `main.go` with chi router
* SQLite initialization with schema migration
* Health endpoint
* `Makefile` with build/run/test targets

**Step 2: S2 client wrapper (1 hr)**

* Provision S2 account, get auth token
* Implement `CreateStream`, `AppendRecords`, `ReadFrom`, `CheckTail`
* Test against real S2 (integration test)

**Step 3: Agent registry (30 min)**

* `POST /agents` endpoint
* SQLite CRUD
* Middleware: extract `X-Agent-ID` header, validate agent exists

**Step 4: Conversation management (1.5 hr)**

* Create, list, invite, leave endpoints
* Membership checks
* S2 stream creation on conversation create
* System events (`agent_joined`, `agent_left`) written to S2

**Step 5: Complete message send (1 hr)**

* `POST /conversations/:cid/messages`
* Batch write to S2 (`message_start + message_append + message_end`)
* Return `message_id` and `seq`

**Step 6: SSE read stream (2 hr)**

* `GET /conversations/:cid/stream`
* S2 read session → SSE event translation
* Cursor lookup on connect, cursor update on delivery
* Connection registry for leave termination
* Auto-tail when caught up

**Step 7: Streaming write (1.5 hr)**

* `POST /stream` (start), `POST /append`, `POST /end`
* In-progress message tracking
* Orphan detection goroutine
* `message_abort` on timeout/disconnect

**Step 8: WebSocket transport (1.5 hr)**

* `GET /conversations/:cid/ws` → upgrade
* Bidirectional: read events + write commands on same connection
* Reuse SSE read logic and message write logic

**Step 9: Claude agent (2 hr)**

* Register agent on startup
* SSE listener per conversation
* Claude API streaming integration
* Token forwarding via streaming write API
* Multi-conversation support

**Step 10: Testing (2 hr)**

* Integration test suite covering all scenarios listed above
* Concurrent writer test
* Disconnect/reconnect test

**Step 11: Documentation (1.5 hr)**

* `DESIGN.md`: every design decision with rationale
* `FUTURE.md`: production-grade redesign
* `CLIENT.md`: self-contained agent onboarding doc

**Step 12: Deployment (1 hr)**

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
│   │   ├── messages.go             # Message send (complete + streaming)
│   │   ├── sse.go                  # SSE streaming read
│   │   ├── websocket.go            # WebSocket bidirectional
│   │   ├── history.go              # GET /messages (reconstructed)
│   │   └── middleware.go           # Agent ID extraction/validation
│   ├── store/
│   │   ├── sqlite.go               # SQLite operations
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
└── README.md                       # (already exists)
```

### Key Dependencies

* `github.com/go-chi/chi/v5` — HTTP router
* `github.com/s2-streamstore/s2-sdk-go` — S2 client
* `github.com/anthropics/anthropic-sdk-go` — Claude API
* `github.com/google/uuid` — UUID generation
* `nhooyr.io/websocket` — WebSocket
* `modernc.org/sqlite` — Pure-Go SQLite (no CGO)
* `github.com/rs/zerolog` — Structured logging

---

## Blast Radius & Reversibility

| Decision                       | Worst Case if Wrong                     | Reversibility                                                                                                                 |
| ------------------------------ | --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| Go as language                 | Slower to prototype than Python         | Low — full rewrite. But Go is the right call for this domain.                                                                 |
| S2 as stream storage           | S2 has an outage or API breaking change | Medium — swap S2 client for direct S3 + custom sequencing. The S2 wrapper (`internal/store/s2.go`) isolates the blast radius. |
| SQLite for metadata            | Concurrent write contention under load  | High — swap to PostgreSQL. Same SQL, same schema, different driver.                                                           |
| SSE for reads                  | Client doesn't support SSE              | High — WebSocket and polling endpoints exist as alternatives.                                                                 |
| HTTP POST per chunk for writes | Too much overhead per token             | High — WebSocket write path already built as alternative.                                                                     |
| One stream per conversation    | Hot conversation with many writers      | Low — but S2 handles 100 MiBps per stream. LLM token rates won't come close.                                                  |
| Event-based record model       | Complexity in demultiplexing            | Medium — could simplify to complete-message-only model, but lose streaming. The event model is the correct design.            |

---

## Verification Plan

1. Local smoke test: Start server, register two agents, create conversation, send messages, verify SSE delivery.
2. Streaming test: Start streaming write from agent A, verify agent B's SSE receives chunks in real-time.
3. Concurrent test: Two agents stream messages simultaneously, verify both messages reconstructable from interleaved events.
4. Reconnect test: Disconnect agent B's SSE, send messages from A, reconnect B, verify B catches up from cursor.
5. Leave test: Agent B leaves, verify B's SSE connection closes, verify B can't read/write.
6. Claude agent test: Invite Claude agent to conversation, send message, verify Claude responds with streaming tokens.
7. Integration test suite: `go test ./tests/... -v` — all scenarios pass.
8. Deployed test: Hit the live Fly.io URL, register agent, create conversation with Claude agent, have a conversation.

`"/plan open"` to edit this plan in Vim

```

Your original file is here for reference: :contentReference[oaicite:0]{index=0}

I can also split this into cleaner sections like “Architecture,” “API,” and “Roadmap” in separate code blocks if that makes copying easier.
```
