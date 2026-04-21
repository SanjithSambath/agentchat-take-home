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
* **PostgreSQL (Neon):** All metadata. Agent registry, conversation membership, read cursors. Managed serverless PostgreSQL via Neon, accessed over direct TCP from the Go server host (typically colocated in `us-east-1`).

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
    * Reject if the agent is the last member (return 409 `last_member` — the request is syntactically valid; the conflict is with current resource state).
    * Write an `agent_left` system event to the S2 stream.
    * **Critical:** terminate active SSE connections and in-progress streaming writes for this agent on this conversation.
        * The ConnRegistry tracks **both** SSE read connections and streaming write connections per `(agent_id, conversation_id)`.
        * On leave, cancel both connection types and wait for their goroutines to exit (5s timeout each).
    * **Critical:** abort any in-progress streaming message from this agent.
        * The streaming write handler's context cancellation triggers its abort path — it appends `message_abort` to S2 before exiting.
        * **Belt-and-suspenders:** If the write handler fails to append `message_abort` (S2 temporarily unreachable), the leave handler appends it as a fallback.
        * The `message_abort` event MUST precede the `agent_left` event on the S2 stream. This ordering is guaranteed because the leave handler waits for the write handler to exit before writing `agent_left`.
    * After leave, reject all read/write requests from this agent for this conversation.
    * Both of the agent's cursors (`delivery_seq` + `ack_seq`) are preserved in PostgreSQL for potential re-invite resume. See §1.7.

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
Body: { "message_id": "01906e5d-5678-...", "content": "Hello, how are you?" }

Response: 201 Created
{ "message_id": "01906e5d-5678-...", "seq_start": 40, "seq_end": 42, "already_processed": false }

# On retry with the same message_id (idempotent replay):
Response: 200 OK
{ "message_id": "01906e5d-5678-...", "seq_start": 40, "seq_end": 42, "already_processed": true }
```
*(Server writes three records to S2 in one batch: message_start, message_append (full content), message_end. Atomic. The client supplies `message_id` as a UUIDv7 — it is both the message's identity and the idempotency key. See `http-api-layer-plan.md` §5 "Complete Message Write Lifecycle" for the full handler flow including the dedup gate.)*

**API for streaming message send (NDJSON streaming POST):**

The agent opens a **single HTTP POST request** with a streaming NDJSON body. The server reads the body line-by-line as chunks arrive, appending each to S2 in real-time. The entire message lifecycle — start, all chunks, end — happens within one persistent HTTP connection. No per-token round-trips.

```http
POST /conversations/:cid/messages/stream
Header: X-Agent-ID: <agent_id>
Header: Content-Type: application/x-ndjson

# Request body: FIRST line is the idempotency handshake, then content chunks.
{"message_id":"01906e5d-9abc-..."}
{"content":"Hello, "}
{"content":"how are "}
{"content":"you?"}

# Response (sent after body completes):
200 OK
{ "message_id": "01906e5d-9abc-...", "seq_start": 42, "seq_end": 45, "already_processed": false }
```

**Server-side mechanics (Go) — AppendSession for pipelined writes:**
```go
func (h *Handler) StreamMessage(w http.ResponseWriter, r *http.Request) {
    // 1. Read FIRST NDJSON line — the idempotency handshake.
    scanner := bufio.NewScanner(r.Body)
    if !scanner.Scan() { return 400 missing_message_id }
    var hdr struct{ MessageID uuid.UUID `json:"message_id"` }
    if err := json.Unmarshal(scanner.Bytes(), &hdr); err != nil || isZero(hdr.MessageID) || !isUUIDv7(hdr.MessageID) {
        return 400 invalid_message_id
    }
    msgID := hdr.MessageID

    // 2. Dedup lookup.
    if row, ok := h.store.Dedup().Get(ctx, convID, msgID); ok {
        if row.Status == "complete" {
            return 200 { msgID, row.StartSeq, row.EndSeq, already_processed: true }
        }
        return 409 already_aborted
    }

    // 3. Claim the message_id — UNIQUE (conversation_id, message_id) on in_progress_messages
    //    turns concurrent duplicates into a clean 409.
    claimed, _ := h.store.InProgress().Claim(ctx, msgID, convID, agentID, streamName)
    if !claimed { return 409 in_progress_conflict }

    // 4. Open pipelined append session.
    session, err := h.s2.OpenAppendSession(ctx, convID)
    defer session.Close()
    errCh := make(chan error, 1)
    go func() { /* drain futures, send first error to errCh */ }()

    // 5. Submit message_start with a 2s timeout to bound AppendSession backpressure.
    if err := submitWithTimeout(ctx, session, messageStartEvent(msgID, agentID), 2*time.Second); err != nil {
        abort(ctx, h, convID, msgID, "slow_writer")
        return 503 slow_writer
    }

    // 6. Stream loop.
    for scanner.Scan() {
        if ctx.Err() != nil { break }  // leave / shutdown
        var chunk struct{ Content string `json:"content"` }
        json.Unmarshal(scanner.Bytes(), &chunk)
        if err := submitWithTimeout(ctx, session, messageAppendEvent(msgID, chunk.Content), 2*time.Second); err != nil {
            abort(ctx, h, convID, msgID, "slow_writer")
            return 503 slow_writer
        }
    }

    // 7. Abort path (context canceled or scanner error).
    if ctx.Err() != nil || scanner.Err() != nil {
        abort(ctx, h, convID, msgID, "disconnect")
        return
    }

    // 8. Normal close: emit message_end and record the terminal outcome.
    seqEnd, _ := submitAndWait(ctx, session, messageEndEvent(msgID), 2*time.Second)
    h.store.Tx(ctx, func(tx) {
        h.store.Dedup().InsertComplete(tx, convID, msgID, seqStart, seqEnd)
        h.store.InProgress().Delete(tx, msgID)
    })
    json.NewEncoder(w).Encode(StreamMessageResponse{msgID, seqStart, seqEnd, false})
}

// Shared abort helper (used by the streaming write handler and the recovery sweep).
func abort(ctx, h, convID, msgID uuid.UUID, reason string) {
    // Unary append — bypasses the possibly-errored AppendSession.
    h.s2.AppendEvents(ctx, convID, []Event{messageAbortEvent(msgID, reason)})
    h.store.Tx(ctx, func(tx) {
        h.store.Dedup().InsertAborted(tx, convID, msgID)  // ON CONFLICT DO NOTHING
        h.store.InProgress().Delete(tx, msgID)
    })
}
```

**Why AppendSession, not Unary per token:** Unary at 40ms ack (Express) = max 25 sequential appends/sec. LLMs emit 30-100 tokens/sec. AppendSession pipelines the appends — submit token N while token N-1's ack is still in-flight. No bottleneck at any token rate S2 can handle.

**The 2 s Submit timeout (bounded backpressure).** `AppendSession` has built-in backpressure: `Submit` blocks when inflight bytes exceed S2's default 5 MiB. In the steady state that is self-regulating — the session paces submits against S2 throughput. In a pathological state (S2 partial outage, extreme latency), the built-in backpressure could block indefinitely, pinning the handler goroutine (plus its `in_progress_messages` row and AppendSession resources) for minutes. The `submitWithTimeout(ctx, session, ev, 2*time.Second)` wrapper converts "block forever" into "fail in 2 s with a specific error." On timeout the handler enters the shared `abort()` path: Unary-append a `message_abort` (which bypasses the errored session's inflight queue), write the `messages_dedup` aborted row, delete the `in_progress_messages` row, return `503 slow_writer`. 2 s is ~50× S2 Express's normal ~40 ms ack — wide for benign latency spikes, narrow enough to catch real wedges. See [s2-architecture-plan.md](s2-architecture-plan.md) §5 for the AppendSession wrapper implementation and [http-api-layer-plan.md](http-api-layer-plan.md) §5 for the full abort-path contract.

**`head_seq` update.** On every successful S2 append, the S2 store wrapper (`internal/store/s2.go`) calls `UpdateConversationHeadSeq(convID, LastSeqNum)` against Postgres — a regression-guarded `UPDATE conversations SET head_seq = $2 WHERE id = $1 AND head_seq < $2`. This keeps the cached tail-position in Postgres in lockstep with S2 (drift ≤ a few ms) and makes `GET /agents/me/unread` a single indexed join with no S2 round-trip. The handler above does not invoke this directly — it is a side-effect of the store wrapper's append primitives, applied uniformly to both Unary `AppendEvents` and streaming `AppendSession` paths. See `sql-metadata-plan.md` §5 and §12 for the schema and query.

`bufio.NewScanner(r.Body)` blocks at `scanner.Scan()` until a complete NDJSON line arrives from the client, then returns it. When the client closes the request body, `Scan()` returns `false` (EOF). If the connection drops mid-stream, `r.Context().Done()` fires and/or `r.Body.Read()` returns an error. This works identically on HTTP/1.1 chunked encoding and HTTP/2 DATA frames.

**Client-side scaffolding — the two-line pattern:**

The streaming write is designed for the "scaffolding wraps the agent" model. The agent doesn't know about AgentMail. Scaffolding captures its output and pipes it through a single POST.

*Python (12 lines, sync, stdlib-compatible):*
```python
import requests, json, uuid

def stream_message(base_url, agent_id, conv_id, token_iterator):
    message_id = str(uuid.uuid4())  # prefer uuid7; uuid4 works for take-home scope
    def chunks():
        yield json.dumps({"message_id": message_id}) + "\n"   # required handshake
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
MID=$(uuidgen | tr 'A-Z' 'a-z')
{
  printf '{"message_id":"%s"}\n' "$MID"                      # required handshake
  agent_command | while IFS= read -r token; do
    printf '{"content":"%s"}\n' "$token"
  done
} | curl -s -X POST \
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

The NDJSON body has a minimal two-stage protocol:
* **First line (handshake)** = `{"message_id":"<uuidv7>"}` — the client-supplied idempotency key. REQUIRED; missing/invalid → 400.
* **Subsequent lines (content chunks)** = `{"content":"token"}`, not `{"type":"append","content":"token"}`.

And the message lifecycle maps onto the HTTP request lifecycle:
* **Message start** = server receives the first NDJSON line, validates `message_id`, passes dedup/in-progress gates, then appends `message_start` to S2
* **Content chunks** = each subsequent NDJSON line in the body
* **Message end** = client closes the request body (EOF)
* **Message abort** = connection drops before body completes, leave mid-stream, or `slow_writer` timeout

This keeps the client protocol dead simple — emit one handshake line, then `{"content":"..."}` lines, then close. The server handles all lifecycle events internally.

**Acknowledged tradeoffs:**

* **No mid-stream server feedback.** If the agent is removed from the conversation while streaming, they won't know until the response (or via their SSE read connection). Mitigation: extremely rare edge case.
* **Proxy buffering risk.** Some reverse proxies (nginx default config, Kubernetes ingress) buffer chunked POST request bodies. Mitigation: we control our deployment infrastructure (Cloudflare Tunnel supports streaming out of the box with `disableChunkedEncoding: false`) and document the `proxy_request_buffering off` directive for self-hosted deployments. AI agents typically don't run behind buffering corporate proxies.
* **Read-once body means no automatic retries of the same HTTP request.** If the connection drops at 90% completion, the client can't resume the same body on the server side. What the client can (and MUST) do is re-send the entire streaming POST with the **same `message_id`**. The idempotency gate (see "When is a write successful" below) makes that retry correctness-safe: either the prior attempt completed (server replays the cached seqs) or it was aborted (server returns 409 and the client generates a fresh `message_id`).

**Hidden complexities:**

* **When is a write "successful"?**
    * S2 acknowledges after regional durability (data is in S3). Our server returns success to the client only after S2 acknowledges. This means: a successful write is durable. If the server crashes after S2 ack but before sending the HTTP response, the write is durable but the client thinks it failed and retries — creating a duplicate *unless* the server has a dedup key to recognize the retry by.
    * **Mitigation (concrete mechanism, not a principle).** The client supplies `message_id` (UUIDv7) in every message-write request — as a required JSON field on the complete-message POST, and as the mandatory first NDJSON line on the streaming POST. The server keeps two pieces of state per `(conversation_id, message_id)`:
        1. `in_progress_messages` with `UNIQUE (conversation_id, message_id)` — the in-flight claim. Any concurrent duplicate hits the unique constraint → `409 in_progress_conflict`.
        2. `messages_dedup (conversation_id, message_id, status, start_seq, end_seq)` — the terminal outcome. On retry after a successful write, the handler replays cached seqs with `already_processed: true`. On retry after an aborted write, the handler returns `409 already_aborted` and the client MUST pick a fresh `message_id`.
    * See [sql-metadata-plan.md](sql-metadata-plan.md) §5 for the schema and §12 for the exact queries. See [http-api-layer-plan.md](http-api-layer-plan.md) §5 "Complete Message Write Lifecycle" and "Streaming Write Lifecycle" for the handler flow.
* **Concurrent writers:**
    * Two agents streaming simultaneously to the same conversation: their records interleave on the S2 stream. This is correct behavior. S2 assigns sequence numbers, guaranteeing total ordering. The interleaving is the ordering — it represents the temporal reality of concurrent composition.
    * Readers demultiplex by `message_id`. Each reader maintains a map of `message_id → accumulated_content` and assembles complete messages from interleaved chunks.
* **Orphaned messages (crash during streaming):**
    * With NDJSON streaming POST, the connection lifecycle IS the message lifecycle. If the agent crashes or disconnects, the server detects it immediately via `r.Context().Done()` or EOF on `r.Body.Read()`.
    * On detection: write `message_abort` event to the S2 stream. The HTTP handler returns (no response sent, since the connection is dead). Clean, deterministic — no background goroutine needed for timeout-based orphan detection.
    * On conversation leave: cancel the request context for any in-progress streaming write from this agent, triggering the abort path above.
    * **Edge case — server crash mid-stream:** The S2 stream has `message_start` + some `message_append` records but no `message_end` or `message_abort`. On server restart, a recovery sweep reads from the `in_progress_messages` PostgreSQL table (which tracks all active streaming writes) and, for each row, (1) appends `message_abort` to S2, (2) inserts a `messages_dedup` row with `status='aborted'` (`ON CONFLICT DO NOTHING`, safe against concurrent sweepers or a delayed ack from the original handler), and (3) deletes the `in_progress_messages` row. All three steps are idempotent; duplicate `message_abort` records are harmless (readers already removed the `message_id` from their pending map). No S2 stream scanning needed. The sweep's dedup insertion is what makes a client retry with the same `message_id` return `409 already_aborted` instead of silently replaying a stale partial message — the client generates a fresh `message_id` and proceeds. See [server-lifecycle-plan.md](server-lifecycle-plan.md) for the full sweep algorithm.
* **Validation on write:**
    * Agent must be a member of the conversation (checked once when the streaming POST request arrives, before reading the body).
    * Content must be plaintext (spec says "purely plaintext communication").
    * Empty content lines are allowed (LLMs sometimes emit empty tokens).
    * For complete message POST: content must not be empty (no point sending an empty message).

**Files:** `internal/api/messages.go`, `internal/store/s2.go`, `internal/model/events.go`

---

### 1.4 Message Streaming — Read Path

**Consumption Model: Active, Passive, Wake-Up.**

External agents consume a conversation in one of three modes. The mode is never declared to the server — the transport the client is using *is* the mode. No server-side per-agent consumption-mode state exists.

* **Active.** The agent holds an open SSE tail on `GET /conversations/:cid/stream`. Events flow live. The `delivery_seq` cursor auto-advances as events are shipped. The agent's own framework decides what to do with each event (feed its LLM, dispatch a tool call, ignore). The service never invokes anything on the agent's behalf.
* **Passive.** The agent remains a member of the conversation but holds no tail. Messages continue to be appended to the S2 stream by other members. The agent consumes nothing in real time and is not interrupted. Leave is destructive and is NOT the way to tune out — staying a member with no open tail IS passive mode.
* **Wake-Up.** A passive agent learns that it has unread material by calling `GET /agents/me/unread` on its own schedule (client-initiated pull, not server push). The response enumerates every conversation where `head_seq > ack_seq`. The agent then decides whether to engage (open a tail, or pull assembled messages via `GET /conversations/:cid/messages`) or continue ignoring.

The "always respond on `message_end`" policy used by the Claude-powered resident agent (§1.8) is a **resident-specific** choice. External agents adopt any consumption policy they want within this model.

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
9.  Server periodically flushes the agent's `delivery_seq` from in-memory cache to PostgreSQL (batched every 5 seconds). `ack_seq` is not involved here — it is only advanced by explicit `POST /conversations/:cid/ack`.

**Connection lifecycle:**
* Register connection in the connection registry: `activeConnections[(agent_id, cid)] = cancelFunc`
* On client disconnect: detected via `r.Context().Done()`, clean up, update cursor.
* On leave: server calls `cancelFunc`, which closes the response, client sees connection close.
* On server shutdown: graceful drain — send final event, close all connections.

**Cursor management — two cursors per (agent, conversation):**

* **`delivery_seq`** — server-advanced. Tracks the highest sequence number delivered to the agent over an active tail (SSE). Stored in PostgreSQL (warm tier) under `cursors(agent_id, conversation_id, delivery_seq, ack_seq, updated_at)`. Hot tier: in-memory `sync.RWMutex` map updated on every event delivery. Flushed in batches every 5 seconds (or 10 events, whichever comes first); immediately on disconnect, leave, or shutdown. Used to resume a reconnecting tail so the agent does not redundantly re-stream what the wire already delivered.
* **`ack_seq`** — client-advanced. Advanced only by an explicit `POST /conversations/:cid/ack` from the agent. Synchronous write-through upsert — no hot tier, no batching. Represents what the agent has actually *processed* (as opposed to what was merely delivered to the wire). `ack_seq` powers the unread computation on `GET /agents/me/unread`. Independent of `delivery_seq` — `ack_seq` can be ahead of `delivery_seq` (passive-catch-up reader that pulled assembled messages via the history endpoint) or behind (active tailer that has received events but not yet finished processing them).
* **Invariants.** `0 ≤ ack_seq ≤ head_seq` and `0 ≤ delivery_seq ≤ head_seq`. No invariant is enforced between `ack_seq` and `delivery_seq`.
* **Both cursors survive leave.** On re-invite, the agent resumes from its prior `delivery_seq` for tails and its prior `ack_seq` for unread computation. See §1.7.
* **Delivery semantics.** At-least-once for tails (agent may re-receive up to ~5 seconds of events after a crash, deduplicated client-side by sequence number). Exactly-what-you-acked for `ack_seq` (synchronous, regression-guarded).

**History endpoint (canonical passive catch-up):**
```http
GET /conversations/:cid/messages
Header: X-Agent-ID: <agent_id>
Query: ?limit=50&before=<seq_num>   (pagination, newest-first)
       ?limit=50&from=<seq_num>     (catch-up, ack-cursor-aligned, oldest-first)

Response: 200 OK
{
  "messages": [
    {"message_id":"m1","sender_id":"a1","content":"Hello, how are you?","seq_start":42,"seq_end":45,"status":"complete"},
    {"message_id":"m2","sender_id":"a2","content":"I'm well, thanks","seq_start":46,"seq_end":49,"status":"complete"}
  ],
  "has_more": false
}
```
*(This reconstructs complete messages from the raw event stream. Reads from S2, groups events by `message_id`, assembles content, returns structured messages.)*

**This endpoint is the canonical passive-catch-up primitive.** It returns **assembled messages only** — there is intentionally no raw-events alternative, no granularity knob. Passive consumers (polymarket-style agents that tuned out and want to re-engage) call this with `?from=<their ack_seq>` to receive every complete message that landed after what they last acknowledged. `?from` and `?before` are mutually exclusive. Active tailers that want raw events use SSE above.

**`status` field:** Three-valued string enum replacing the original `complete` boolean, because aborted messages are neither complete nor in-progress:

| Value | Meaning |
|---|---|
| `"complete"` | `message_end` received. Full content available. |
| `"in_progress"` | `message_start` received but no `message_end` or `message_abort` yet. Currently being streamed. |
| `"aborted"` | `message_abort` received. Partial content — agent disconnected or was removed. |

**Pagination:** Default limit 50, max 100. `before` is an exclusive upper bound (return messages whose `seq_start` < `before`). Response includes `has_more` boolean for pagination. Messages ordered newest-first (descending `seq_start`). See `http-api-layer-plan.md` §3 for complete query parameter specification.

**Edge cases:**
* Agent connects to conversation they're not a member of: 403.
* Agent connects to nonexistent conversation: 404.
* Agent connects while another SSE connection for the same (agent, conversation) is active: close the old connection, use the new one. Only one active reader per agent per conversation.
* `from` seq_num is beyond the stream's current tail: start tailing immediately, wait for new events.
* `from` seq_num is before the stream's trim point (28-day retention on free tier): silently start from the stream's earliest available record (head). Log a warning server-side for observability. The agent catches up on whatever history remains.
* Slow reader: handled by a two-layer backpressure model. (1) The SSE handler owns a bounded per-connection event channel `eventCh := make(chan SequencedEvent, 64)` between the tail goroutine (S2 ReadSession consumer) and the writer goroutine (HTTP `Flusher`). Every enqueue uses a 500 ms deadline via `select { case eventCh <- ev: ... case <-time.After(500*time.Millisecond): ... }`; if the writer hasn't drained in 500 ms the connection is disconnected as `slow_consumer` (the client reconnects with `Last-Event-ID` and resumes from its cursor — zero data loss because events stay on S2 and `delivery_seq` was flushed at the last acked event). (2) For stalls that the channel deadline doesn't reach (e.g., the writer is blocked inside `Flush()` while the client-side TCP receive buffer is full), S2 + `http.Flusher` still provide the secondary TCP-level backpressure that slows the S2 read session. The bounded channel + short deadline is what makes detection *explicit* rather than waiting for OS-level buffer exhaustion. See [http-api-layer-plan.md](http-api-layer-plan.md) §5 "SSE Stream Lifecycle" for the implementation.

**Ack endpoint (advance the ack cursor):**
```http
POST /conversations/:cid/ack
Header: X-Agent-ID: <agent_id>
Content-Type: application/json

{"seq": 127}

Response: 204 No Content
```
*(Idempotent. Advances `ack_seq` for `(agent, conversation)` to `max(current_ack_seq, seq)`. Synchronous write-through to Postgres — no hot tier, no batching. The client has durability on return.)*

**Semantics and errors:**
* `seq` MUST satisfy `0 ≤ seq ≤ conversations.head_seq`. `seq > head_seq` is rejected with 400 `ack_beyond_head`. `seq < 0` or non-integer is rejected with 400 `ack_invalid_seq`.
* **Regression is silently ignored.** If the caller sends `seq < current_ack_seq`, the row is unchanged and 204 is returned. The upsert uses `WHERE cursors.ack_seq < EXCLUDED.ack_seq`. This prevents out-of-order retries from un-acking material.
* Membership is required. Non-member: 403. Unknown conversation: 404.
* **No body on success** — 204 No Content. The cursor state is queryable via `GET /agents/me/unread`.

**Unread endpoint (list all conversations with unacked events):**
```http
GET /agents/me/unread?limit=100
Header: X-Agent-ID: <agent_id>

Response: 200 OK
{
  "conversations": [
    {"conversation_id":"01906e5c-...","head_seq":302,"ack_seq":247,"event_delta":55},
    {"conversation_id":"01906e60-...","head_seq":19,"ack_seq":0,"event_delta":19}
  ]
}
```
*(One indexed SQL join. Returns only conversations where `head_seq > ack_seq`, ordered by `head_seq` descending. Default `limit` 100, max 500.)*

**Why on-demand, not push:**

The service does NOT maintain denormalized per-agent unread counters, does NOT maintain per-agent activity streams, and does NOT run any server-side cron on the agent's behalf. The unread view is computed from two already-present facts — `conversations.head_seq` (cached inline on every successful S2 append) and `cursors.ack_seq` (advanced by the agent's own acks). A single SQL join answers the query:

```sql
SELECT c.id AS conversation_id,
       c.head_seq,
       COALESCE(k.ack_seq, 0) AS ack_seq,
       c.head_seq - COALESCE(k.ack_seq, 0) AS event_delta
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

**`event_delta` is in events, not messages.** Converting to a message count would require scanning the range. The delta is a size signal (zero / some / a lot) — it tells the agent *whether* to engage, not *what* the unread content is. To see the content, the agent calls `GET /conversations/:cid/messages?from=<ack_seq>` (assembled messages).

**Semantics and errors:**
* Passive agents that never call this endpoint cost the server zero. There is no background work per agent.
* Agents with no memberships: `conversations: []`.
* Agents with memberships but everything acked: `conversations: []`.
* Race with concurrent writes: `head_seq` is cached inline with S2 appends; a call landing mid-write may report `head_seq` lagging the true S2 tail by up to a few milliseconds. Acceptable — the agent sees it on the next call.

See `sql-metadata-plan.md` §5 for the schema and §7 for the cursor store interface. See `http-api-layer-plan.md` §3 for Go request/response types.

**Files:** `internal/api/sse.go`, `internal/api/history.go`, `internal/api/ack.go`, `internal/api/unread.go`

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

**What we lose:** Zero-config embedded deployment. PostgreSQL is an external dependency. Single-binary simplicity. The Go binary now requires a database connection string. **These costs are already paid** by the decision to host on Neon (see §1.6.2).

#### 1.6.2 Hosting: Neon Serverless PostgreSQL

Running our own Postgres — on a VM, in a container, or as a self-managed cluster — means owning backups, failover, monitoring, and version upgrades. Neon is a fully managed serverless Postgres with a free tier that covers our take-home scale and a clear upgrade path.

| Dimension | Neon | Self-managed Postgres (VM/container) | Managed Postgres (RDS/Cloud SQL) |
|---|---|---|---|
| **Management** | Fully managed | Self-managed | Fully managed |
| **Cost** | Free tier (0.5 GB, 100 CU-hours/mo) | ~$0 (host cost only) | $15-40/mo minimum |
| **Backups** | Automatic, point-in-time recovery | Manual | Automatic |
| **Failover** | Automatic | Manual | Automatic |
| **PostgreSQL version** | 16, 17 | Whatever you install | 15, 16 |
| **Connection pooling** | Built-in PgBouncer | DIY | Add-on or DIY |

**Architecture:**
```text
┌─────────────────────┐         TCP (direct)        ┌──────────────────────┐
│  Go Server          │ ──────────────────────────→  │  Neon PostgreSQL     │
│  (us-east-1 host)   │         ~1-5ms latency       │  (AWS us-east-1)     │
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

-- Conversations: each row maps to one S2 stream.
-- head_seq caches the latest S2 sequence number for the conversation; updated
-- inline with every successful S2 append. Powers GET /agents/me/unread.
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

-- Cursors: server-managed read positions.
-- Two cursors per (agent, conversation):
--   delivery_seq — server-advanced as events are shipped over a tail (SSE).
--                  Batched flush every 5s from an in-memory hot tier. Powers tail resume.
--   ack_seq      — client-advanced only by POST /conversations/:cid/ack.
--                  Synchronous write-through (no hot tier). Powers GET /agents/me/unread.
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

-- In-progress streaming writes: tracks active streaming messages for crash recovery.
-- On server crash, recovery sweep writes message_abort for each row, then deletes.
-- Postgres-first insert (before S2 message_start) ensures crash safety.
--
-- UNIQUE (conversation_id, message_id) is the in-flight idempotency gate: a
-- duplicate client request with the same message_id fails the INSERT and
-- receives 409 in_progress_conflict (see http-api-layer-plan.md §1, §5).
CREATE TABLE in_progress_messages (
    message_id       UUID PRIMARY KEY,
    conversation_id  UUID NOT NULL,
    agent_id         UUID NOT NULL,
    s2_stream_name   TEXT NOT NULL,
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, message_id)
);

-- Messages dedup: terminal-outcome record per (conversation, client-supplied message_id).
-- Written once when a message reaches a terminal state ('complete' or 'aborted').
-- On client retry with the same message_id, the server replays from this table:
--   'complete' → 200 with cached seq_start/seq_end, already_processed:true
--   'aborted'  → 409 already_aborted; client must generate a fresh message_id
-- Written by: successful write handlers (complete) and abort/sweep paths (aborted).
-- See sql-metadata-plan.md §5 for full rationale.
CREATE TABLE messages_dedup (
    conversation_id  UUID NOT NULL,
    message_id       UUID NOT NULL,
    status           TEXT NOT NULL CHECK (status IN ('complete', 'aborted')),
    start_seq        BIGINT,
    end_seq          BIGINT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_id, message_id)
);
```

**Table rationale:**
* **`agents`:** No indexes beyond PK. No metadata columns (spec says "no metadata"). Agents are permanent (no soft-delete).
* **`conversations`:** `s2_stream_name` decouples conversation ID from S2 stream name (format: `conversations/{conversation_id}`). UNIQUE index prevents two conversations pointing to the same stream. `head_seq` is the cached latest S2 sequence number, updated inline on every successful S2 append from `AppendResult.LastSeqNum()`; it powers `GET /agents/me/unread` without paying an S2 round-trip per unread query.
* **`members`:** PK order `(conversation_id, agent_id)` optimized for the hottest queries: membership check (`WHERE conversation_id = $1 AND agent_id = $2`) and member list (`WHERE conversation_id = $1`). Separate index on `(agent_id)` for "list conversations for agent." Foreign keys with no CASCADE — agents and conversations are permanent. **Hard delete on leave:** simpler queries (no `AND left_at IS NULL`), S2 stream has full membership event history, re-invite is a fresh INSERT.
* **`cursors`:** No foreign keys (validated at API layer, cursors may outlive membership). **Two independent cursors in one row.** `delivery_seq` is written by the batched-flush hot tier on tail delivery; `ack_seq` is written synchronously by the `POST /conversations/:cid/ack` handler. Both are regression-guarded (out-of-order writes are silent no-ops). **Both cursors survive leave** — on re-invite, agent resumes tailing from `delivery_seq` and gets an accurate unread count from `ack_seq`. `BIGINT` on both columns (S2 sequence numbers are 64-bit). PK order `(agent_id, conversation_id)` optimized for disconnect cleanup and for the `GET /agents/me/unread` join.
* **`in_progress_messages`:** UUID PK on `message_id` (server-local lookups during sweep); UNIQUE `(conversation_id, message_id)` is the *in-flight idempotency gate* — any concurrent duplicate write with the same client-supplied `message_id` fails the INSERT and the handler returns `409 in_progress_conflict`. No foreign keys (performance-critical write path). Rows exist for the lifetime of an active streaming POST (seconds to minutes) — the table is always tiny.
* **`messages_dedup`:** PK `(conversation_id, message_id)` is the *terminal-outcome idempotency key*. A second request with a `message_id` that already has a dedup row replays the cached outcome (`complete` → 200 with cached seqs; `aborted` → 409 so the client regenerates). `start_seq` / `end_seq` nullable because aborted rows may have been aborted before `message_start` was ever appended. No TTL enforced at schema level — retention is bounded in practice by write rate × the natural ceiling on how far back a client would ever retry; production scale would add a 24–48 h retention job (FUTURE.md).

**Why exactly six tables:** No `messages` table (messages live in S2). No `sessions`/`connections` table (tracked in-memory). No `events`/`audit` table (S2 streams ARE the audit log). The two additions beyond the core four (`agents`, `conversations`, `members`, `cursors`) are both tightly scoped to crash-safety and idempotency of the write path:
  * `in_progress_messages` — the minimal index of *currently active* streaming writes, so the recovery sweep can emit `message_abort` for unterminated messages on restart. Rows are ephemeral (seconds to minutes).
  * `messages_dedup` — the minimal record of *terminal outcomes* keyed by client-supplied `message_id`, so client retries after a crash/network drop are correctness-safe. Rows are small and purely metadata — no message content.

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
    InProgress() InProgressStore   // in-flight idempotency claim + crash-recovery index
    Dedup() DedupStore              // terminal-outcome record per (conv, message_id)
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
    // Delivery cursor (two-tier, batched). See §1.7 / sql-metadata-plan.md §7.
    GetDeliveryCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error)
    UpdateDeliveryCursor(agentID, convID uuid.UUID, seqNum uint64)  // memory-only, no error
    FlushOne(ctx context.Context, agentID, convID uuid.UUID) error
    FlushAll(ctx context.Context) error
    Start(ctx context.Context)

    // Ack cursor (single-tier, synchronous). See §1.7 / sql-metadata-plan.md §7.
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

type InProgressStore interface {
    // Claim: atomic idempotency gate. INSERT … ON CONFLICT (conversation_id,
    // message_id) DO NOTHING. Returns (true, nil) on a successful claim,
    // (false, nil) if the (conv, mid) is already in-flight or a stale row
    // persists from a crashed prior attempt.
    Claim(ctx context.Context, messageID, convID, agentID uuid.UUID, s2StreamName string) (bool, error)
    Delete(ctx context.Context, messageID uuid.UUID) error
    List(ctx context.Context) ([]InProgressRow, error)  // recovery sweep
}

type DedupStore interface {
    // Get: dedup lookup at the start of every write handler.
    Get(ctx context.Context, convID, messageID uuid.UUID) (*DedupRow, bool, error)
    InsertComplete(ctx context.Context, convID, messageID uuid.UUID, startSeq, endSeq uint64) error
    InsertAborted(ctx context.Context, convID, messageID uuid.UUID) error
}

type InProgressRow struct {
    MessageID      uuid.UUID
    ConversationID uuid.UUID
    AgentID        uuid.UUID
    S2StreamName   string
    StartedAt      time.Time
}

type DedupRow struct {
    Status    string     // 'complete' | 'aborted'
    StartSeq  *uint64    // populated when Status == 'complete'
    EndSeq    *uint64    // populated when Status == 'complete'
}
```

**Domain types:**
```go
type Conversation struct {
    ID            uuid.UUID
    S2StreamName  string
    HeadSeq       uint64    // cached S2 tail; updated inline on every successful append
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
INSERT INTO conversations (id, s2_stream_name, head_seq, created_at) VALUES ($1, $2, 0, now());
-- name: GetConversation :one
SELECT id, s2_stream_name, head_seq, created_at FROM conversations WHERE id = $1;
-- name: ConversationExists :one
SELECT EXISTS(SELECT 1 FROM conversations WHERE id = $1);
-- name: GetConversationHeadSeq :one
SELECT head_seq FROM conversations WHERE id = $1;
-- name: UpdateConversationHeadSeq :exec
-- Called inline with every successful S2 append. Regression-guarded.
UPDATE conversations SET head_seq = $2 WHERE id = $1 AND head_seq < $2;
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
-- name: GetDeliveryCursor :one
SELECT delivery_seq FROM cursors WHERE agent_id = $1 AND conversation_id = $2;
-- name: GetAckCursor :one
SELECT ack_seq FROM cursors WHERE agent_id = $1 AND conversation_id = $2;
-- name: UpsertDeliveryCursor :exec
-- Single-row delivery_seq UPSERT for disconnect/leave/shutdown. Does NOT touch ack_seq.
INSERT INTO cursors (agent_id, conversation_id, delivery_seq, updated_at) VALUES ($1, $2, $3, now())
ON CONFLICT (agent_id, conversation_id) DO UPDATE SET delivery_seq = EXCLUDED.delivery_seq, updated_at = EXCLUDED.updated_at
WHERE cursors.delivery_seq < EXCLUDED.delivery_seq;
-- name: AckCursor :exec
-- Synchronous write-through for POST /conversations/:cid/ack. Regression-guarded.
INSERT INTO cursors (agent_id, conversation_id, ack_seq, updated_at) VALUES ($1, $2, $3, now())
ON CONFLICT (agent_id, conversation_id) DO UPDATE SET ack_seq = EXCLUDED.ack_seq, updated_at = EXCLUDED.updated_at
WHERE cursors.ack_seq < EXCLUDED.ack_seq;
-- name: ListUnreadForAgent :many
SELECT c.id AS conversation_id, c.head_seq,
       COALESCE(k.ack_seq, 0) AS ack_seq,
       c.head_seq - COALESCE(k.ack_seq, 0) AS event_delta
FROM members m
JOIN conversations c ON c.id = m.conversation_id
LEFT JOIN cursors k ON k.agent_id = m.agent_id AND k.conversation_id = m.conversation_id
WHERE m.agent_id = $1 AND c.head_seq > COALESCE(k.ack_seq, 0)
ORDER BY c.head_seq DESC LIMIT $2;
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

**Phase 1 — Current Design (Millions):** Single Go server host behind a Cloudflare Tunnel + Neon Postgres. In-process membership cache (LRU, 100K entries, 60s TTL). In-memory cursor hot tier + batched Postgres flush. pgxpool with 15 connections. Handles ~1M agents, ~5M conversations, ~50K concurrent SSE connections.

**Phase 2 — Read Replicas (Tens of Millions):** Add Neon read replicas. Route reads to replicas, keep writes on primary. Membership cache reduces replica load by 90%+.

**Phase 3 — Partitioning (Hundreds of Millions):** Hash-partition `members` by `conversation_id` (64-128 partitions). Hash-partition `cursors` by `agent_id`. Tradeoff: `ListConversationsForAgent` scans all partitions — mitigate with denormalized table or accept cross-partition scan.

**Phase 4 — Dedicated Instances (Billions):** Replace Neon with dedicated PostgreSQL (AWS RDS). Shard by tenant/region. Redis between hot tier and Postgres. LISTEN/NOTIFY for cross-instance invalidation. PgBouncer for connection pooling across instances.

#### 1.6.14 Edge Cases & Failure Modes

* **Server crash during leave transaction:** PostgreSQL rolls back. Member row restored. Leave didn't happen. Correct.
* **Server crash during invite (after Postgres commit, before S2 event):** Agent is a member but no `agent_joined` event on S2 stream. Functional (agent can read/write), cosmetic gap in history. Production mitigation: reconciliation sweep on startup.
* **Neon cold start during evaluation:** Disable scale-to-zero, or use an external uptime check (UptimeRobot / Cloudflare Worker cron) against `/health` to keep Neon compute warm.
* **pgxpool exhaustion:** pgxpool queues goroutines. Sub-millisecond queries mean wait is negligible. Set `context.WithTimeout(ctx, 5*time.Second)` on all DB ops for fail-fast.
* **Membership cache poisoning (direct Postgres manipulation):** 60-second TTL self-corrects. Don't bypass the API.

**Files:** `internal/store/postgres.go`, `internal/store/cursor_cache.go`, `internal/store/membership_cache.go`, `internal/store/conn_registry.go`, `internal/store/schema/schema.sql`, `internal/store/queries/*.sql`, `internal/store/db/` (sqlc generated), `sqlc.yaml`

---

### 1.7 Read Cursor Management — Two-Tier Architecture

**Design:** Two independent server-managed cursors per `(agent, conversation)` — `delivery_seq` (auto-advanced on tail delivery, two-tier: in-memory hot tier for zero-cost updates, batched PostgreSQL warm tier for durability) and `ack_seq` (advanced only by explicit client `POST /conversations/:cid/ack`, single-tier: synchronous write-through to Postgres, no hot tier).

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
┌─────────────────────┐                              ┌──────────────────────────────┐
│   CursorCache       │    every 5 seconds           │   cursors table              │
│   sync.RWMutex      │ ───────────────────────────→ │   (agent_id, conv_id,        │
│   map[key]entry     │    batch UPSERT delivery_seq │    delivery_seq, ack_seq,    │
│   dirty set         │    via unnest() arrays       │    updated_at)               │
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

**Hot tier (in-memory):** `sync.RWMutex` + `map[cursorKey]cursorEntry` + dirty set. Updated on every event delivery. Zero I/O cost. Why `sync.RWMutex`, not `sync.Map`: cursor updates are write-heavy from many goroutines on overlapping keys — neither `sync.Map` optimization pattern applies. At extreme scale (>100K concurrent SSE connections): shard into 64 buckets by `hash(agent_id) % 64`.

**Warm tier (PostgreSQL):** Background goroutine flushes all dirty `delivery_seq` values every 5 seconds via `unnest()` batch upsert (does NOT touch `ack_seq`):
```sql
INSERT INTO cursors (agent_id, conversation_id, delivery_seq, updated_at)
SELECT * FROM unnest($1::uuid[], $2::uuid[], $3::bigint[], $4::timestamptz[])
ON CONFLICT (agent_id, conversation_id)
DO UPDATE SET delivery_seq = EXCLUDED.delivery_seq, updated_at = EXCLUDED.updated_at
WHERE cursors.delivery_seq < EXCLUDED.delivery_seq;
```
The `WHERE cursors.delivery_seq < EXCLUDED.delivery_seq` clause prevents stale flushes from regressing the cursor.

**Flush triggers (`delivery_seq` only — `ack_seq` is synchronous, never batched):**

| Trigger | What happens | Why |
|---|---|---|
| **Periodic (every 5 sec)** | Background goroutine flushes ALL dirty `delivery_seq` values | Bounds data loss on crash to 5 seconds |
| **Clean disconnect** | SSE handler flushes THIS agent's `delivery_seq` immediately | Zero data loss on graceful close |
| **Server shutdown** | Graceful shutdown flushes ALL dirty `delivery_seq` values before exit | Zero data loss on planned restart |
| **Agent leave** | Flush this agent's `delivery_seq` for this conversation | Preserve accurate position for potential re-invite |
| **`POST .../ack` received** | Synchronous upsert of `ack_seq` to Postgres, bypasses hot tier | Client has durability on return; ack must be immediately visible to the next `GET /agents/me/unread` |

**Crash recovery:** Server crashes → in-memory `delivery_seq` values lost (`ack_seq` is always durable, never cached). Agent reconnects → server reads `delivery_seq` from PostgreSQL (at most 5 seconds stale) → S2 read session starts from that position → agent re-receives up to ~150 events (5 sec × ~30 events/sec) → events carry sequence numbers for client-side deduplication. Textbook **at-least-once delivery**.

**Cursor behavior on leave and re-invite (both cursors preserved):**
* On leave: flush `delivery_seq` to PostgreSQL, remove from in-memory cache, do NOT delete the `cursors` row from PostgreSQL. `ack_seq` is already durable (never cached).
* On re-invite + SSE connect: cache miss → fall back to PostgreSQL → `delivery_seq` exists from before leave → resume from that position → agent catches up on messages sent while gone.
* On re-invite + `GET /agents/me/unread`: `ack_seq` is still what the agent last explicitly acknowledged. Any message that landed while the agent was gone is counted as unread.
* This is better than deleting cursors (which would force re-reading the entire conversation from sequence 0).
* The spec's "access to full conversation history" is satisfied by the S2 stream — agent CAN read from 0 via `?from=0`.

**Ack Cursor — Client-Controlled, Synchronous Write-Through:**

The `ack_seq` column exists so the client has an authoritative cursor that reflects what it has actually *processed* (as opposed to what was merely delivered to the wire — that is `delivery_seq`'s job). It is advanced only by `POST /conversations/:cid/ack` with a `{"seq": N}` body.

* **No hot tier.** The ack path does a direct UPSERT to Postgres with a regression-guard `WHERE cursors.ack_seq < EXCLUDED.ack_seq`. No in-memory cache, no batched flush.
* **Why no hot tier.** Ack volume is orders of magnitude lower than event-delivery volume — a tail delivers 30–100 events/sec per conversation; a client typically acks once per message (or once per batch of passive-catch-up messages). A 5-second delay on ack durability would make `GET /agents/me/unread` lie — an agent that just acked seq 127 could get back a stale unread count including events ≤ 127. Correctness demands write-through.
* **Regression is a silent no-op.** Out-of-order retry of the same ack, or a replay of a lower seq, leaves the row unchanged and returns 204. The server never reduces `ack_seq`.
* **`ack_seq` and `delivery_seq` are independent.** `ack_seq` can be greater than `delivery_seq` (passive-catch-up reader that pulled assembled messages via `GET /conversations/:cid/messages` and acked the range without ever opening a tail). `ack_seq` can be less than `delivery_seq` (active tailer that has received events but not yet finished processing them). No invariant is enforced between them.

**Delivery semantics:**
* **For `delivery_seq` / tail delivery:** At-least-once. If server crashes between delivering an event and flushing `delivery_seq`, the event will be re-delivered on reconnect. Events carry sequence numbers and message IDs. Clients can deduplicate.
* **For `ack_seq` / unread computation:** Exactly-what-you-acked. Synchronous write-through plus regression guard means the `ack_seq` visible to `GET /agents/me/unread` is always `max` of everything the client has ever durably acked.
* Why not exactly-once on tail delivery: requires client acknowledgments coupled with delivery — adds protocol complexity for marginal gain. The split above gives clients exact control via `ack_seq` where it matters (unread tracking) without forcing ack-coupling on every tailed event.

---

### 1.8 Claude-Powered Agent

**Scope note:** The resident agent's "always respond on `message_end` from another agent" policy described below is **resident-specific**. It is the policy of one particular in-process Claude-powered bot, not a service primitive. External agents adopt any consumption policy they want within the active/passive/wake-up model defined in §1.4 — nothing in the service forces an LLM invocation on any agent.

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

### 1.8.1 External Claude Code Persona Agents

> **Cross-references:** wire pattern and embedded reference scripts → [`CLIENT.md §2`](CLIENT.md); topology placement → [`high-level-overview.md §10b`](high-level-overview.md).

**Decision:** AgentMail supports a **two-pattern agent topology**. The §1.8 resident is the server-bundled default responder; a second, client-side pattern — the Claude Code persona agent — is first-class, documented, and tested. Both produce indistinguishable events on the S2 stream; peers cannot tell them apart.

**Motivation.** Early naive-agent test runs revealed that a Claude Code session told "you are persona X" defaults to three failure modes: block `POST /messages` instead of the streaming endpoint; replay-streaming (decode the whole reply, then chunk it into NDJSON with `time.sleep`); and polling SSE with `sleep && tail` instead of reacting to events. All three satisfy the letter of the spec while violating the spirit of "stream tokens as they are generated." The fix is a structural one, not a prompt-engineering one: a single long-running Python daemon (`run_agent.py`, served from the Go server at `GET /client/run_agent.py`) owns the SSE tail, opens a fresh `anthropic.messages.stream(...)` per turn with a bound `leave_conversation` tool, and pipes text deltas straight into the NDJSON body. Claude Code's role is reduced to **orchestrator**: it curls the runtime once, runs one command with `--target` + `--brief`, and blocks until the daemon exits (via the tool call, a peer-left cascade, SIGINT, or a safety cap). Claude Code never decodes persona text in its own turn.

**Transport reuse — no new server surface.** This pattern adds **zero** new endpoints, event types, or error codes. It is a disciplined client of the existing HTTP/SSE surface: `POST /agents`, `POST /conversations`, `POST /conversations/{cid}/invite`, `POST /conversations/{cid}/messages/stream` (NDJSON write), `GET /conversations/{cid}/stream` (SSE read), `GET /conversations/{cid}/messages` (history seed for each speak turn). Idempotency, dedup, recovery, and `Last-Event-ID` resume all work unchanged.

**Contrast with §1.8 resident:**

| | **§1.8 resident** | **§1.8.1 Claude Code persona** |
|---|---|---|
| Process | in-server goroutine | external Claude Code session |
| Anthropic call site | `internal/agent/respond.go` → S2 `AppendSession` direct | `run_agent.py` daemon → `POST /.../messages/stream` (NDJSON) |
| Inbound | direct S2 `ReadSession` | SSE via `GET /.../stream`, tailed in-process by the daemon |
| Termination | HTTP `POST /leave` by the operator only (resident never leaves voluntarily) | `leave_conversation` tool call by the voice stream, plus backstops (peer-left cascade, signal, safety cap) |
| Availability | always-on, bundled with every deployment | started by an operator on demand |
| When to prefer | default responder, fixed voice | domain-specific voices, demos, adversarial testing |

**Self-contained client doc.** Per `README.md` L82 + L96, `CLIENT.md` must be the *only* file a naive Claude Code agent needs. `§0 Claude Code: one-command bootstrap` gives the exact shell block; `§7 The run_agent.py runtime` is the behavioral contract for the daemon. The Python source itself is served from the Go server at `GET /client/run_agent.py` (embedded via `go:embed`), so the markdown is small and the runtime version is pinned to the deployed server. The three inputs the human operator owes the agent are: a name (`--name`), a target (`--target resident|peer:<id>|join:<cid>`), and the ask (`--brief`).

**Out of scope for this design decision:**
- Server enforcement of the orchestrator/voice split. It is a client-side discipline, reinforced by documentation and anti-pattern enumeration (`CLIENT.md §9`).
- Per-token tool calls from Claude Code. Incompatible with Anthropic's decode model and explicitly discouraged by founder guidance; the fresh-stream-per-message approach is the endorsed workaround. A single `leave_conversation` tool evaluated once per message is not in this class — it is a boundary-of-message decision, not a hot-path call.
- TypeScript parity for the `run_agent.py` daemon. `CLIENT.md §8.2` and `§8.4` retain TypeScript equivalents of the wire kernels; a full TS operator variant is a future enhancement.

---

### 1.9 HTTP API Layer — Router, Middleware, Error Format, Contracts

> **Full design:** [`http-api-layer-plan.md`](http-api-layer-plan.md) — exhaustive design document covering error format, middleware chain, request/response Go types with wire examples, Content-Type handling, timeout architecture, agent existence caching, and scaling roadmap from thousands to billions. What follows is the executive summary.

The HTTP API layer is the skeleton every endpoint hangs on. Without a consistent, pre-designed API layer, each handler makes ad-hoc decisions during coding — inconsistent error shapes, forgotten validations, mismatched Content-Types. This section specifies the cross-cutting machinery.

**Core decisions:**

| Decision | Choice | Rationale |
|---|---|---|
| Error format | `{"error":{"code":"...","message":"..."}}` envelope on every error | Machine-readable `code` for AI agent branching; human-readable `message` for debugging. Single top-level `error` key makes error detection trivial: `if "error" in data`. |
| Middleware chain | Recovery → Request ID → Logger → (Agent Auth) → (Timeout) → Handler | Three route groups: unauthenticated, authenticated+30s timeout, authenticated+streaming (no timeout) |
| Request/response types | Complete Go structs per endpoint; `snake_case` JSON; `DisallowUnknownFields()` | Catches AI agent typos (`agnet_id`) immediately. Empty slices marshal as `[]`, not `null`. |
| Content-Type validation | Required on all POSTs with a body; NDJSON endpoint accepts `application/x-ndjson` and `application/ndjson` | Wrong Content-Type on streaming endpoint returns helpful redirect to the complete-message endpoint |
| Timeouts | 30s CRUD, 5s health, none for SSE/streaming (handler-managed idle detection + safety caps) | SSE: 30s heartbeat + 24h absolute cap. Streaming write: 5-minute idle timeout + 1h absolute cap via `http.ResponseController.SetReadDeadline` |
| Agent validation caching | `sync.Map` existence cache (write-once, read-many), warmed from Postgres on startup | Zero-contention reads (~50ns). Agents are permanent → no eviction needed. Scaling path: Bloom filter + LRU at billions. |
| 403 vs 404 | 403 for not-a-member; 404 for doesn't-exist | UUIDv7 makes enumeration impractical; no-auth means nothing to protect; distinct errors help AI agents debug faster |

**Error code registry (comprehensive):**

Every error condition across every endpoint is mapped to a specific HTTP status code and machine-readable error code. Key codes:

| HTTP Status | Error Code | When |
|---|---|---|
| 400 | `missing_agent_id` | `X-Agent-ID` header absent |
| 400 | `invalid_agent_id` | `X-Agent-ID` not a valid UUID |
| 400 | `invalid_json` | Malformed JSON body |
| 400 | `unknown_field` | Unrecognized field in request body (typo protection) |
| 400 | `empty_content` | Message content is empty string |
| 403 | `not_member` | Agent is not a member of the conversation |
| 404 | `agent_not_found` | Agent ID doesn't exist |
| 404 | `conversation_not_found` | Conversation ID doesn't exist |
| 404 | `invitee_not_found` | Agent being invited doesn't exist |
| 409 | `last_member` | Cannot leave — last member in conversation |
| 409 | `in_progress_conflict` | Concurrent duplicate write with the same `message_id` is in flight (UNIQUE on `in_progress_messages` caught it) |
| 409 | `already_aborted` | A prior write with the same `message_id` was aborted (crash, disconnect, slow-writer timeout); client must generate a fresh `message_id` |
| 400 | `missing_message_id` | `message_id` missing from body (complete) or first NDJSON line (streaming) |
| 400 | `invalid_message_id` | `message_id` present but not a valid UUIDv7 |
| 415 | `unsupported_media_type` | Wrong Content-Type on POST |
| 500 | `internal_error` | Server error (DB/S2 failure) |
| 503 | `unhealthy` | Health check: Postgres or S2 unreachable |
| 503 | `slow_writer` | AppendSession `Submit` blocked past the 2 s timeout; server wrote `message_abort` + dedup `aborted` |
| 504 | `timeout` | CRUD request exceeded 30s |

Error codes are a **stable API contract** — they never change between versions. Messages may change. CLIENT.md documents: "Branch on `code`, not `message`."

**Route architecture:**

```text
┌───────┬──────────────────────────────────────────┬──────────┬────────┬───────────┬─────────┐
│ Method│ Path                                     │ Auth     │Timeout │ Body In   │ Body Out│
├───────┼──────────────────────────────────────────┼──────────┼────────┼───────────┼─────────┤
│ POST  │ /agents                                  │ No       │ 30s    │ (none)    │ JSON    │
│ GET   │ /agents/resident                         │ No       │ 30s    │ (none)    │ JSON    │
│ GET   │ /health                                  │ No       │ 5s     │ (none)    │ JSON    │
│ POST  │ /conversations                           │ Yes      │ 30s    │ (none)    │ JSON    │
│ GET   │ /conversations                           │ Yes      │ 30s    │ (none)    │ JSON    │
│ POST  │ /conversations/{cid}/invite              │ Yes      │ 30s    │ JSON      │ JSON    │
│ POST  │ /conversations/{cid}/leave               │ Yes      │ 30s    │ (none)    │ JSON    │
│ POST  │ /conversations/{cid}/messages            │ Yes      │ 30s    │ JSON      │ JSON    │
│ GET   │ /conversations/{cid}/messages            │ Yes      │ 30s    │ (none)    │ JSON    │
│ POST  │ /conversations/{cid}/messages/stream     │ Yes      │ (none) │ NDJSON    │ JSON    │
│ GET   │ /conversations/{cid}/stream              │ Yes      │ (none) │ (none)    │ SSE     │
└───────┴──────────────────────────────────────────┴──────────┴────────┴───────────┴─────────┘
```

**Middleware details** (see `http-api-layer-plan.md` §2):
- **Recovery:** Custom panic handler returns error envelope (not chi's plain-text stack trace). Outermost layer.
- **Request ID:** UUIDv4, respects client-provided `X-Request-ID` (≤128 chars). Propagated via context and response header.
- **Logger:** Structured JSON (zerolog). Method, path, status, duration, request_id, agent_id. 2xx=INFO, 4xx=WARN, 5xx=ERROR.
- **Agent Auth:** Reads `X-Agent-ID`, validates UUID, checks `sync.Map` cache → Postgres fallback. Exemptions: `POST /agents`, `GET /health`, `GET /agents/resident`.
- **Timeout:** `context.WithTimeout` with custom 504 error envelope on expiry. Applied to CRUD routes only.

**Request/response contracts** (see `http-api-layer-plan.md` §3):

Complete Go structs and wire examples for every endpoint. Key types:

```go
type CreateAgentResponse struct {
    AgentID uuid.UUID `json:"agent_id"`
}

type CreateConversationResponse struct {
    ConversationID uuid.UUID   `json:"conversation_id"`
    Members        []uuid.UUID `json:"members"`
    CreatedAt      time.Time   `json:"created_at"`
}

type InviteRequest struct {
    AgentID uuid.UUID `json:"agent_id"`
}

type SendMessageRequest struct {
    MessageID uuid.UUID `json:"message_id"` // REQUIRED, client-generated UUIDv7 (idempotency key)
    Content   string    `json:"content"`
}

type HistoryResponse struct {
    Messages []HistoryMessage `json:"messages"`
    HasMore  bool             `json:"has_more"`
}

type HealthResponse struct {
    Status string            `json:"status"`
    Checks map[string]string `json:"checks"`
}
```

**Shared handler helpers** (see `http-api-layer-plan.md` §3):
- `decodeJSON[T]` — generic JSON parser with `DisallowUnknownFields()`, `MaxBytesReader`, granular error classification (syntax, type, unknown field, body too large, EOF)
- `requireMembership` — parse conversation ID → check existence → check membership. Returns proper 400/404/403 errors.
- `writeError` / `writeJSON` — consistent response writers with pre-allocated fallback for error serialization failures

**Handler structure template** — every handler follows: validate params → parse body → check business rules → execute → respond. Early return on error. Domain errors (`ErrLastMember`) mapped to HTTP errors at the handler layer. Store layer never knows about HTTP.

**Cross-cutting concerns** (see `http-api-layer-plan.md` §6):
- CORS: not implemented (AI agents are HTTP clients, not browsers)
- Rate limiting: not implemented (focus on core messaging per spec); documented production path with `golang.org/x/time/rate`
- Idempotency: invite is idempotent via `ON CONFLICT DO NOTHING`; all others follow natural semantics
- Graceful shutdown: SIGTERM → stop accepting → drain CRUD (30s) → close SSE → abort streaming writes → flush cursors → close pools → exit

**Files** (see `http-api-layer-plan.md` §10):

| File | Contents |
|---|---|
| `internal/api/router.go` | Chi router, route groups, NotFound/MethodNotAllowed handlers |
| `internal/api/middleware.go` | Recovery, Request ID, Logger, Agent Auth, Timeout |
| `internal/api/errors.go` | `APIError` type, `writeError`, `writeJSON`, fallback body |
| `internal/api/helpers.go` | `decodeJSON[T]`, `parseConversationID`, `requireMembership`, `validateContentType` |
| `internal/api/types.go` | All request/response Go structs |
| `internal/store/agent_cache.go` | `sync.Map`-based agent existence cache, startup warming |

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
* **Proxy buffering.** Some reverse proxies (nginx default, Kubernetes ingress-nginx) buffer chunked POST bodies. Mitigation: we control deployment infra (Cloudflare Tunnel streams bodies with `disableChunkedEncoding: false`); document `proxy_request_buffering off` for self-hosted. AI agents typically don't run behind buffering corporate proxies. Note: WebSocket has a symmetric problem — many corporate firewalls block or interfere with WebSocket upgrade.
* **No mid-stream server-to-client communication.** The response comes only after the body completes. Mitigation: the SSE read connection provides real-time feedback; mid-stream write errors are extremely rare edge cases.
* **Read-once body breaks automatic retries of the same HTTP request.** The client cannot re-stream the same body against the same request. It CAN re-issue the entire streaming POST with the same client-supplied `message_id` — the server's dedup gate (`messages_dedup` + UNIQUE on `in_progress_messages`, see [sql-metadata-plan.md](sql-metadata-plan.md) §5) either replays the cached terminal outcome or returns `409 already_aborted` so the client generates a fresh `message_id`. For LLM output specifically, regenerating with a new `message_id` is often cheaper than resuming anyway.

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
    * Build static Go binary (`CGO_ENABLED=0 go build -trimpath ./cmd/server`)
    * Install and authenticate `cloudflared` on the host
    * Start the tunnel (`cloudflared tunnel --url http://localhost:8080` for quick mode, or a named tunnel with `ingress:` config for a stable hostname)
    * Verify Claude agent is running and reachable over the tunnel URL

### Files to Create

**No service layer.** For this system's complexity, API handlers ARE the orchestration. The store handles data access, the S2 client handles stream operations, and handlers compose them. An intermediate service layer would add indirection for zero benefit. This is an explicit design decision, not a gap.

```text
agentmail-take-home/
├── cmd/server/main.go              # Entry point, config, startup, graceful shutdown
├── internal/
│   ├── api/
│   │   ├── router.go               # Chi router, route groups, NotFound/MethodNotAllowed
│   │   ├── middleware.go           # Recovery, Request ID, Logger, Agent Auth, Timeout
│   │   ├── errors.go              # APIError type, writeError, writeJSON, fallback body
│   │   ├── helpers.go             # decodeJSON[T], parseConversationID, requireMembership
│   │   ├── types.go               # All request/response Go structs
│   │   ├── agents.go               # POST /agents, GET /agents/resident
│   │   ├── conversations.go        # Create, list, invite, leave handlers
│   │   ├── messages.go             # Complete send + NDJSON streaming send
│   │   ├── sse.go                  # SSE streaming read
│   │   ├── history.go              # GET /messages (reconstructed)
│   │   └── health.go               # GET /health
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
│   │   ├── agent_cache.go          # sync.Map agent existence cache, startup warming
│   │   ├── cursor_cache.go         # In-memory cursor hot tier
│   │   ├── membership_cache.go     # In-memory membership LRU cache
│   │   ├── conn_registry.go        # Active connection tracking
│   │   ├── s2.go                   # S2 stream operations
│   │   └── s2_test.go              # S2 integration test
│   ├── model/
│   │   ├── events.go               # Event types, serialization
│   │   └── types.go                # Agent, Conversation, Member
│   └── agent/
│       ├── agent.go                # Agent struct, Start()/Shutdown(), state map, semaphores
│       ├── listener.go             # Per-conversation listener goroutine, event dispatch
│       ├── respond.go              # triggerResponse(), callClaudeStreaming(), sendErrorMessage()
│       ├── history.go              # seedHistory(), buildClaudeMessages(), sliding window
│       └── discovery.go            # Invite channel consumer, startup reconciliation
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
├── cloudflared.yml                # Cloudflare Tunnel ingress config (named tunnel)
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
| **PostgreSQL (Neon) for metadata** | Neon outage or cold-start latency | Medium — fallback to any managed Postgres (AWS RDS, Google Cloud SQL) or self-hosted. Schema and queries are standard PostgreSQL. |
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
8.  **Deployed test:** Hit the live Cloudflare Tunnel URL, register agent, create conversation with Claude agent, have a conversation.