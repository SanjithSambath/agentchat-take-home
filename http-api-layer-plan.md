# AgentMail: HTTP API Layer — Complete Design Plan

## Executive Summary

This document specifies the complete design for AgentMail's HTTP API layer: the machinery that sits between "HTTP request arrives" and "handler logic runs." This is the skeleton every endpoint hangs on — error format, middleware chain, request/response contracts, content-type handling, timeouts, and all the cross-cutting concerns that must be consistent across every route.

### Why This Matters

Without a designed API layer, each handler makes ad-hoc decisions during coding. One endpoint returns `{"error":"..."}`, another returns `{"message":"..."}`, a third returns a bare string. One handler validates Content-Type, another doesn't. One checks agent existence, another forgets. These inconsistencies compound into an API that AI agents can't reliably parse and evaluators notice immediately.

### Core Decisions

- **Error format:** Standard JSON envelope `{"error":{"code":"...","message":"..."}}` on every error response. Machine-readable code for programmatic branching, human-readable message for debugging. No exceptions.
- **Middleware chain:** Recovery → Request ID → Structured Logging → (Agent Auth on protected routes) → (Timeout on CRUD routes) → Handler. Three route groups: unauthenticated, authenticated+timeout, authenticated+streaming.
- **Request/response types:** Complete Go structs for every endpoint. `snake_case` JSON. `DisallowUnknownFields()` enabled. Typed UUID and time.Time serialization.
- **Content-Type:** Validated on all POST requests. JSON endpoints reject non-JSON. NDJSON endpoint rejects non-NDJSON with helpful redirect message.
- **Timeouts:** 30s for CRUD, none for SSE/streaming (connection-bound with idle detection and absolute safety caps).
- **Agent validation:** `sync.Map` in-process existence cache (write-once, read-many). Zero-contention reads. Direct Postgres fallback on miss.

---

## 1. Error Response Format

### The Problem

Every endpoint can fail. Without a standard error shape, consumers must special-case each endpoint's error format. AI coding agents — our primary clients — need a single, predictable structure they can branch on programmatically. They can't pattern-match against inconsistent error shapes across 11 endpoints.

### Decision: Nested Error Object

```json
{
  "error": {
    "code": "not_member",
    "message": "Agent a1b2c3d4-... is not a member of conversation e5f6g7h8-..."
  }
}
```

**Two fields, both required on every error:**

| Field | Purpose | Consumer |
|---|---|---|
| `code` | Machine-readable, stable string. Agents branch on this. | AI agents, test harnesses |
| `message` | Human-readable, may change between versions. For logs and debugging. | Developers, evaluators |

### Why This Shape

**Single top-level key (`error`) makes error detection trivial:**

```python
response = requests.post(...)
data = response.json()
if "error" in data:
    handle_error(data["error"]["code"])
else:
    handle_success(data)
```

No status code parsing needed for the simplest case. Status codes are still correct and meaningful (see below), but the body is self-describing.

**Alternatives considered and rejected:**

| Shape | Why Rejected |
|---|---|
| `{"error": "message string"}` | No machine-readable code. Agents must parse English to branch. Brittle. |
| `{"error": "code", "message": "..."}` | Top-level `error` is a string — ambiguous whether it's a code or message. Two top-level keys means success responses can't also have a `message` field without collision. |
| `{"error": {"code": "...", "message": "...", "details": [...]}}` | Over-engineered. Our request bodies have 0-1 fields. Field-level validation error arrays are unnecessary. If we ever need `details`, adding it is backward-compatible. |
| Bare HTTP status code, no body | AI agents need context. A bare 404 doesn't tell you whether the conversation or the agent wasn't found. |
| Google-style `{"error": {"code": 404, "status": "NOT_FOUND", "message": "..."}}` | Numeric code duplicates HTTP status. String status duplicates our `code`. Two redundant fields. |

### Go Types

```go
type APIError struct {
    Error ErrorBody `json:"error"`
}

type ErrorBody struct {
    Code    string `json:"code"`
    Message string `json:"message"`
}
```

### Comprehensive Error Code Registry

Every error condition across every endpoint, with its HTTP status code and error code:

#### Universal Errors (Any Endpoint)

| HTTP Status | Error Code | Condition | Message Template |
|---|---|---|---|
| 500 | `internal_error` | Unexpected server error (DB failure, S2 failure, panic recovery) | `"Internal server error"` |
| 405 | `method_not_allowed` | Wrong HTTP method for this route | `"Method {method} not allowed on {path}"` |
| 415 | `unsupported_media_type` | Wrong Content-Type on POST | `"Expected Content-Type {expected}, got {actual}"` |

#### Agent Auth Middleware Errors

| HTTP Status | Error Code | Condition | Message Template |
|---|---|---|---|
| 400 | `missing_agent_id` | `X-Agent-ID` header absent | `"X-Agent-ID header is required"` |
| 400 | `invalid_agent_id` | `X-Agent-ID` header present but not a valid UUID | `"X-Agent-ID must be a valid UUID"` |
| 404 | `agent_not_found` | `X-Agent-ID` is a valid UUID but no agent with this ID exists | `"Agent {id} not found"` |

#### Conversation Path Parameter Errors

| HTTP Status | Error Code | Condition | Message Template |
|---|---|---|---|
| 400 | `invalid_conversation_id` | `{cid}` path parameter is not a valid UUID | `"Conversation ID must be a valid UUID"` |
| 404 | `conversation_not_found` | Conversation with this ID doesn't exist | `"Conversation {id} not found"` |
| 403 | `not_member` | Agent exists, conversation exists, but agent is not a member | `"Agent {agent_id} is not a member of conversation {conv_id}"` |

#### POST /agents

No request body. No path parameters. No agent auth.

| HTTP Status | Error Code | Condition |
|---|---|---|
| 201 | — | Success (new agent created) |
| 500 | `internal_error` | Postgres insert failed |

#### GET /agents/resident

No request body. No agent auth.

| HTTP Status | Error Code | Condition |
|---|---|---|
| 200 | — | Success |
| 503 | `resident_agent_unavailable` | `RESIDENT_AGENT_ID` not configured or agent not registered |

#### POST /conversations

No request body. Requires agent auth.

| HTTP Status | Error Code | Condition |
|---|---|---|
| 201 | — | Success (conversation created, creator is member) |
| 500 | `internal_error` | Postgres transaction or S2 append failed |

#### GET /conversations

No request body. Requires agent auth.

| HTTP Status | Error Code | Condition |
|---|---|---|
| 200 | — | Success (may return empty list) |
| 500 | `internal_error` | Postgres query failed |

#### POST /conversations/{cid}/invite

Requires agent auth. Requires valid `{cid}`. Requires membership check (inviter must be a member).

| HTTP Status | Error Code | Condition | Notes |
|---|---|---|---|
| 200 | — | Success (newly invited OR already a member — idempotent) | |
| 400 | `invalid_json` | Request body is not valid JSON | |
| 400 | `missing_field` | `agent_id` field missing from request body | Message: `"Field 'agent_id' is required"` |
| 400 | `invalid_field` | `agent_id` field present but not a valid UUID | Message: `"Field 'agent_id' must be a valid UUID"` |
| 400 | `unknown_field` | Request body contains unrecognized fields | Message: `"Unknown field: '{name}'"` |
| 404 | `invitee_not_found` | The agent being invited doesn't exist | Distinct from `agent_not_found` (which is the inviter) |
| 500 | `internal_error` | Postgres or S2 failure | |

**Why `invitee_not_found` is distinct from `agent_not_found`:** Two different agents are involved — the inviter (authenticated via `X-Agent-ID`, validated by middleware, error is `agent_not_found`) and the invitee (from request body, validated by handler, error is `invitee_not_found`). AI agents need to distinguish: "my identity is wrong" vs "the agent I'm trying to invite doesn't exist."

#### POST /conversations/{cid}/leave

No request body. Requires agent auth. Requires valid `{cid}`. Requires membership check.

| HTTP Status | Error Code | Condition | Notes |
|---|---|---|---|
| 200 | — | Success (agent removed) | |
| 409 | `last_member` | Agent is the only remaining member | Spec: "A leave by the last remaining member is rejected" |
| 500 | `internal_error` | Postgres or S2 failure | |

**Why 409 Conflict, not 400 Bad Request for last-member:** The request is syntactically valid. The conflict is with the current state of the resource — there's only one member. 409 signals "retry after the state changes" (e.g., after inviting someone else). 400 signals "your request is malformed" — misleading.

#### POST /conversations/{cid}/messages (Complete Send)

Requires agent auth. Requires valid `{cid}`. Requires membership check.

| HTTP Status | Error Code | Condition |
|---|---|---|
| 201 | — | Success (message created) |
| 400 | `invalid_json` | Request body is not valid JSON |
| 400 | `missing_field` | `content` field missing |
| 400 | `empty_content` | `content` field present but empty string |
| 400 | `unknown_field` | Unrecognized fields in body |
| 500 | `internal_error` | S2 append failed |

#### POST /conversations/{cid}/messages/stream (Streaming Send)

Requires agent auth. Requires valid `{cid}`. Requires membership check.

| HTTP Status | Error Code | Condition | Notes |
|---|---|---|---|
| 200 | — | Success (message streamed and completed) | |
| 409 | `concurrent_write` | Agent already has an active streaming write to this conversation | Only one active stream per (agent, conversation) |
| 500 | `internal_error` | S2 append session failed | |

**Mid-stream errors** (after body reading has started): The server can't change the status code once it's committed to reading the body. If S2 fails mid-stream, the server appends `message_abort` and closes the connection. The client receives either a truncated response or a connection reset. The abort event on the S2 stream is the source of truth.

**Malformed NDJSON lines mid-stream:** A line that isn't valid JSON is skipped with a warning log. The server does NOT abort the entire message for one bad line — LLMs occasionally emit malformed output, and dropping one token is better than aborting 500 tokens of valid content. The `message` field in the final response includes a `warnings` count if any lines were skipped.

Actually — let me reconsider this. Two approaches:

| Approach | Behavior | Pro | Con |
|---|---|---|---|
| **Skip bad lines** | Log warning, continue, include warning count in response | Resilient to LLM quirks | Silent data loss — client doesn't know a token was dropped |
| **Abort on bad line** | Write `message_abort`, close connection, return error | Strict correctness | One bad token kills the whole message |

**Decision: Abort on bad line.** Rationale: NDJSON lines are produced by the client's scaffolding code, not raw LLM output. The scaffolding wraps tokens in `{"content":"..."}` — if that serialization is broken, something is fundamentally wrong. Aborting immediately with a clear error is better than silently continuing with corrupted content. The `message_abort` event on S2 makes the failure visible to all readers.

#### GET /conversations/{cid}/stream (SSE)

Requires agent auth. Requires valid `{cid}`. Requires membership check.

| HTTP Status | Error Code | Condition | Notes |
|---|---|---|---|
| 200 | — | Success (SSE stream opened) | |
| 400 | `invalid_from` | `from` query parameter present but not a valid uint64 | |

Errors after the SSE stream is established (200 already sent) are delivered as SSE error events:

```
event: error
data: {"code":"s2_read_error","message":"Stream read failed, please reconnect"}
```

Then the connection closes. The client reconnects with `Last-Event-ID`.

#### GET /conversations/{cid}/messages (History)

Requires agent auth. Requires valid `{cid}`. Requires membership check.

| HTTP Status | Error Code | Condition |
|---|---|---|
| 200 | — | Success (may return empty messages array) |
| 400 | `invalid_limit` | `limit` query parameter present but not a valid positive integer, or exceeds max (100) |
| 400 | `invalid_before` | `before` query parameter present but not a valid uint64 |
| 400 | `invalid_from` | `from` query parameter present but not a valid uint64 |
| 400 | `mutually_exclusive_cursors` | Both `before` and `from` provided in the same request |
| 500 | `internal_error` | S2 read failed |

#### POST /conversations/{cid}/ack

Requires agent auth. Requires valid `{cid}`. Requires membership check. Canonical client ack for the passive-catch-up / unread model (see `spec.md` §1.4 and §1.7).

| HTTP Status | Error Code | Condition |
|---|---|---|
| 204 | — | Success (ack accepted; regression is a silent no-op also returning 204) |
| 400 | `invalid_json` | Request body not valid JSON |
| 400 | `ack_invalid_seq` | `seq` field missing, not a non-negative integer, or > 2^63-1 |
| 400 | `ack_beyond_head` | `seq > conversations.head_seq` (client attempted to ack past the tail) |
| 500 | `internal_error` | Postgres write failed |

#### GET /agents/me/unread

Requires agent auth. No path parameters. No membership check (scoped to the calling agent's own memberships).

| HTTP Status | Error Code | Condition |
|---|---|---|
| 200 | — | Success (may return empty `conversations` array) |
| 400 | `invalid_limit` | `limit` query parameter present but not a valid positive integer, or exceeds max (500) |
| 500 | `internal_error` | Postgres query failed |

#### GET /health

No auth. No path parameters.

| HTTP Status | Error Code | Condition |
|---|---|---|
| 200 | — | All systems healthy |
| 503 | `unhealthy` | Postgres or S2 unreachable |

### 403 vs 404: The Enumeration Question

Classic security concern: if 403 means "exists but you can't access it" and 404 means "doesn't exist," an attacker can enumerate which conversation IDs exist by observing the difference.

**Why we use both (not just 404 for everything):**

1. **UUIDv7 space is 2^122 bits of randomness.** Brute-force enumeration is computationally infeasible. An attacker checking 1 billion IDs/sec would need 10^27 years to find a valid conversation.
2. **No authentication means no secrets to protect.** The spec explicitly says "no authentication." Conversation IDs are the access mechanism for members. Non-members already know conversations exist (they were invited, then left, etc.). Hiding existence provides zero security value.
3. **Distinct error codes help AI agents debug faster.** A 404 when the agent expected to be a member means "wrong conversation ID." A 403 means "right conversation, but you left or were never invited." The debugging path is completely different. Conflating them wastes the evaluator's time.
4. **The spec implies distinct behavior.** "Only members can read from or write to a conversation" (permission check, 403) is a different sentence from "inviting a nonexistent agent is an error" (existence check, 404).

**Decision: 403 for not-a-member, 404 for doesn't-exist.** UUIDv7 makes enumeration impractical, no-auth means nothing to protect, and clear errors are more valuable than security theater.

### Error Response Content-Type

**Always `application/json`.** Even on endpoints that normally return `text/event-stream` or `application/x-ndjson`. Errors are returned during the validation phase, before the streaming content type is set. The response writer hasn't committed to a streaming content type yet.

**Edge case: error after SSE headers are sent.** If the SSE connection is established (200 + `text/event-stream` already sent) and then an error occurs (S2 failure, agent removed), the server cannot change Content-Type. Instead, send an SSE-formatted error event:

```
event: error
data: {"code":"stream_error","message":"..."}

```

Then close the connection. The client detects the close and reconnects.

### Error Serialization Safety

**What if `json.Marshal` fails on the error response itself?** (e.g., out of memory, broken `MarshalJSON` method). Use a pre-allocated fallback:

```go
var fallbackErrorBody = []byte(`{"error":{"code":"internal_error","message":"Internal server error"}}`)

func writeError(w http.ResponseWriter, status int, code, message string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    body, err := json.Marshal(APIError{Error: ErrorBody{Code: code, Message: message}})
    if err != nil {
        w.Write(fallbackErrorBody)
        return
    }
    w.Write(body)
}
```

The `fallbackErrorBody` is allocated once at startup as a `[]byte` literal. No allocation, no marshaling, no failure path. This is the error handler's error handler.

### Error Codes Are a Stable Contract

Error codes (`not_member`, `agent_not_found`, etc.) are part of the API contract. They MUST NOT change between versions. Messages CAN change (they're for humans). AI agents that branch on `code == "not_member"` must not break when we improve a message string.

Document this explicitly in CLIENT.md: "Branch on the `code` field, not the `message` field."

---

## 2. Middleware Chain

### The Problem

Middleware runs on every request. The order determines what context is available, what errors look like, and what wraps what. Getting this wrong means inconsistent logging, missing request IDs, unhandled panics, or agent validation running on routes that don't need it.

### Decision: Three Route Groups

```text
Global Middleware (all routes):
  ┌─ Recovery (outermost — catches panics)
  ├─ Request ID (generates + propagates)
  └─ Structured Logging (method, path, status, duration, request_id)

Route Group 1: Unauthenticated
  POST /agents
  GET  /agents/resident
  GET  /health

Route Group 2: Authenticated + Timeout (30s)
  GET  /conversations
  POST /conversations
  POST /conversations/{cid}/invite
  POST /conversations/{cid}/leave
  POST /conversations/{cid}/messages
  GET  /conversations/{cid}/messages

Route Group 3: Authenticated + Streaming (no timeout)
  POST /conversations/{cid}/messages/stream
  GET  /conversations/{cid}/stream
```

### Middleware Execution Order

For a request to `POST /conversations/{cid}/invite`:

```text
Request arrives
  │
  ▼
┌──────────────────────────────────────────────┐
│ 1. Recovery                                  │
│    Catches panics, returns 500 error envelope │
│    ┌────────────────────────────────────────┐ │
│    │ 2. Request ID                         │ │
│    │    Generates UUIDv4, sets header,     │ │
│    │    adds to context                    │ │
│    │    ┌────────────────────────────────┐  │ │
│    │    │ 3. Structured Logging         │  │ │
│    │    │    Captures start time,       │  │ │
│    │    │    wraps ResponseWriter,      │  │ │
│    │    │    logs on completion          │  │ │
│    │    │    ┌────────────────────────┐  │  │ │
│    │    │    │ 4. Agent Auth         │  │  │ │
│    │    │    │    Reads X-Agent-ID,  │  │  │ │
│    │    │    │    validates UUID,    │  │  │ │
│    │    │    │    checks existence,  │  │  │ │
│    │    │    │    adds to context    │  │  │ │
│    │    │    │    ┌────────────────┐ │  │  │ │
│    │    │    │    │ 5. Timeout    │ │  │  │ │
│    │    │    │    │    30s ctx    │ │  │  │ │
│    │    │    │    │    deadline   │ │  │  │ │
│    │    │    │    │    ┌────────┐ │ │  │  │ │
│    │    │    │    │    │Handler │ │ │  │  │ │
│    │    │    │    │    └────────┘ │ │  │  │ │
│    │    │    │    └────────────────┘ │  │  │ │
│    │    │    └────────────────────────┘  │  │ │
│    │    └────────────────────────────────┘  │ │
│    └────────────────────────────────────────┘ │
└──────────────────────────────────────────────┘
```

### Middleware 1: Recovery

**What:** Catches panics from any downstream handler or middleware. Prevents a single buggy request from crashing the entire server process.

**Why outermost:** A panic in the logging middleware or request ID middleware would bypass a recovery that's nested inside them. Recovery must be the outermost layer.

**Behavior:**
1. `defer func() { if r := recover(); r != nil { ... } }()`
2. Log the panic value and stack trace at ERROR level (includes request ID if available)
3. Return standard error envelope: `{"error":{"code":"internal_error","message":"Internal server error"}}` with status 500
4. Do NOT include the panic message or stack trace in the response body — leaks internal implementation details

**Why not chi's built-in `middleware.Recoverer`:** It returns a plain text stack trace to the client. That violates our error envelope contract and leaks internals. Custom middleware, ~15 lines.

```go
func Recovery(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        defer func() {
            if rec := recover(); rec != nil {
                reqID, _ := r.Context().Value(ctxKeyRequestID).(string)
                log.Error().
                    Str("request_id", reqID).
                    Str("panic", fmt.Sprintf("%v", rec)).
                    Str("stack", string(debug.Stack())).
                    Msg("panic recovered")
                writeError(w, 500, "internal_error", "Internal server error")
            }
        }()
        next.ServeHTTP(w, r)
    })
}
```

### Middleware 2: Request ID

**What:** Generates a unique ID for each request. Propagated via context, response header, and every log line.

**Why UUIDv4 (not UUIDv7):** No ordering needed. UUIDv4 is marginally faster (no timestamp computation). Request IDs are for correlation, not sorting.

**Behavior:**
1. Check if `X-Request-ID` header exists on the incoming request (client-provided). If yes, validate it's a sane length (≤128 chars) and use it. If no, generate UUIDv4.
2. Set `X-Request-ID` on the response header.
3. Add to `context.WithValue` for downstream access.

**Why accept client-provided request IDs:** The evaluator's test harness may inject request IDs for end-to-end tracing. Respecting them makes debugging easier for both sides. Length-limit prevents abuse.

```go
func RequestID(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        id := r.Header.Get("X-Request-ID")
        if id == "" || len(id) > 128 {
            id = uuid.New().String()
        }
        w.Header().Set("X-Request-ID", id)
        ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}
```

### Middleware 3: Structured Logging

**What:** Logs every completed request with method, path, status code, duration, request ID, and agent ID (if present).

**Why structured (not printf):** Structured logs (JSON via zerolog) are machine-parseable. `fly logs` output becomes greppable. Alert systems can filter on `status >= 500`.

**Behavior:**
1. Capture start time.
2. Wrap `http.ResponseWriter` with a status-capturing wrapper (records the status code passed to `WriteHeader`).
3. Call `next.ServeHTTP`.
4. On return: log method, path, status, duration, request_id, agent_id (from context, may be empty).

**Log levels:**
- 2xx: INFO
- 4xx: WARN
- 5xx: ERROR

**What NOT to log:** Request/response bodies. They may be large (streaming) and contain agent-generated content. Log only metadata.

**SSE and streaming connections:** The log fires when the handler returns, which for SSE could be minutes or hours later. This is correct — we want to know the total connection duration. For long-lived connections, add periodic "heartbeat" log lines (every 60 seconds) showing the connection is still active.

```go
func Logger(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        ww := &statusWriter{ResponseWriter: w, status: 200}
        next.ServeHTTP(ww, r)
        
        duration := time.Since(start)
        reqID, _ := r.Context().Value(ctxKeyRequestID).(string)
        agentID, _ := r.Context().Value(ctxKeyAgentID).(string)
        
        event := log.Info()
        if ww.status >= 500 {
            event = log.Error()
        } else if ww.status >= 400 {
            event = log.Warn()
        }
        
        event.
            Str("method", r.Method).
            Str("path", r.URL.Path).
            Int("status", ww.status).
            Dur("duration", duration).
            Str("request_id", reqID).
            Str("agent_id", agentID).
            Msg("request completed")
    })
}
```

### Middleware 4: Agent Auth

**What:** Extracts the `X-Agent-ID` header, validates it's a real UUID, confirms the agent exists, and adds the agent ID to the request context.

**Applied to:** Route groups 2 and 3 only. NOT applied to `POST /agents` (no agent exists yet), `GET /health` (infrastructure check), or `GET /agents/resident` (discovery endpoint).

**Behavior:**
1. Read `X-Agent-ID` header. If missing → 400 `missing_agent_id`.
2. Parse as UUID. If invalid → 400 `invalid_agent_id`.
3. Check agent existence (see caching strategy below). If not found → 404 `agent_not_found`.
4. Add `agentID` to context via `context.WithValue`.

**Agent Existence Caching:**

Every authenticated request checks "does this agent exist?" — the highest-volume query in the entire system. Agents are permanent (never deleted). This is a textbook write-once, read-forever pattern.

**`sync.Map` as the in-process existence cache:**

| Property | Value | Why |
|---|---|---|
| Data structure | `sync.Map[uuid.UUID, struct{}]` | Zero-size values. Existence-only. |
| Write frequency | Once per agent creation | `POST /agents` stores after Postgres insert |
| Read frequency | Every authenticated request | 100% hit rate after first request per agent |
| Eviction | None | Agents are permanent. No staleness concern. |
| Memory at 1M agents | ~16 MB (16 bytes × 1M) | UUID is 16 bytes. `struct{}` is 0 bytes. `sync.Map` overhead ~2x. |
| Concurrency | Lock-free reads (`sync.Map` optimized for read-heavy) | Perfect for this access pattern |
| Cache miss | Query Postgres (`SELECT EXISTS(...)`), populate cache | First request for any agent, or after server restart |

**Startup cache warming:**

On server boot, the cache is empty. Two options:

| Approach | Pro | Con |
|---|---|---|
| **Cold start (no warming)** | Simpler. First request per agent hits Postgres, then cached forever. | First request after restart has ~200µs extra latency. Burst of DB queries if many agents connect simultaneously. |
| **Warm on startup** | `SELECT id FROM agents` → populate cache. Zero cold-start latency. | Startup time increases. At 1M agents: ~500ms to scan and insert. |

**Decision: Warm on startup.** The `agents` table is small (UUID only, no metadata). Scanning it takes milliseconds. Eliminates cold-start DB pressure entirely. The query is `SELECT id FROM agents` — sequential scan of a UUID-only PK index, streamed into the `sync.Map`.

```go
func (c *AgentCache) WarmFromDB(ctx context.Context, pool *pgxpool.Pool) error {
    rows, err := pool.Query(ctx, "SELECT id FROM agents")
    if err != nil {
        return err
    }
    defer rows.Close()
    count := 0
    for rows.Next() {
        var id uuid.UUID
        rows.Scan(&id)
        c.m.Store(id, struct{}{})
        count++
    }
    log.Info().Int("agents_loaded", count).Msg("agent cache warmed")
    return nil
}
```

**Scaling path (billions of agents):**

At 1B agents, `sync.Map` consumes ~32 GB (16 bytes × 1B × 2x overhead). Not viable for a single process.

Scaling options:

| Scale | Solution | Memory | Lookup Cost |
|---|---|---|---|
| Thousands (take-home) | `sync.Map` | KB | ~50ns |
| Millions | `sync.Map` | ~32 MB | ~50ns |
| Tens of millions | LRU cache (bounded, TTL) | Configurable cap | ~100ns + occasional DB miss |
| Hundreds of millions | Bloom filter (negative check) + LRU (positive cache) | ~180 MB bloom + bounded LRU | ~200ns bloom check, DB on positive |
| Billions | Bloom filter + distributed cache (Redis) | ~1.4 GB bloom | ~200ns bloom + ~500µs Redis on positive |

**Bloom filter math at 1B agents:** At 1% false positive rate, a Bloom filter needs 1.44 bytes per element = 1.44 GB. A negative result (agent doesn't exist) is definitive — no DB hit. A positive result (might exist) requires confirmation from cache or DB. Since the vast majority of requests are from existing agents, the bloom filter catches the rare invalid-ID case without touching the DB.

**Document, don't build.** For the take-home, `sync.Map` handles the scale. Document the Bloom filter path in FUTURE.md.

### Middleware 5: Timeout

**What:** Sets a deadline on the request context. If the handler doesn't complete within the deadline, the context is cancelled, and a 504 Gateway Timeout is returned.

**Applied to:** Route group 2 only (CRUD endpoints). NOT applied to SSE or streaming write (they're connection-bound).

**Why not `http.TimeoutHandler`:** It panics if the handler writes after timeout, which is unsafe for handlers that do cleanup work. Instead, use `context.WithTimeout` and let handlers check `ctx.Err()`:

```go
func Timeout(d time.Duration) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            ctx, cancel := context.WithTimeout(r.Context(), d)
            defer cancel()
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}
```

**Why 30 seconds:** CRUD operations hit Postgres (sub-millisecond) and S2 (40ms append). Even with retries (3 attempts, exponential backoff), any CRUD operation should complete in <5 seconds. 30 seconds is a generous safety net. If a handler takes 30 seconds, something is catastrophically wrong (DB connection pool exhaustion, S2 outage).

**What happens when timeout fires:**
- `ctx.Done()` closes
- Postgres queries cancel automatically (pgx respects context)
- S2 operations cancel (SDK respects context)
- Handler returns, logging middleware captures the duration
- If the handler hasn't written a response yet: the deferred `cancel()` fires but no 504 is automatically written — the handler must check `ctx.Err()` and return

**Better approach — write 504 on timeout:**

Actually, the `context.WithTimeout` approach alone doesn't automatically send a 504. It just cancels the context. If the handler is stuck in a blocking call that doesn't check context (unlikely with pgx and S2 SDK, but possible), the client waits forever.

**Solution: chi's `middleware.Timeout` with custom error body:**

```go
func TimeoutWithError(d time.Duration) func(http.Handler) http.Handler {
    body, _ := json.Marshal(APIError{Error: ErrorBody{
        Code:    "timeout",
        Message: "Request timed out",
    }})
    return middleware.Timeout(d,
        middleware.WithTimeoutBody(string(body)),
        middleware.WithTimeoutContentType("application/json"),
    )
}
```

Wait — chi's `middleware.Timeout` doesn't have those options. It writes a plain text "Timeout." Let me use a custom implementation:

```go
func Timeout(d time.Duration) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            ctx, cancel := context.WithTimeout(r.Context(), d)
            defer cancel()
            
            done := make(chan struct{})
            tw := &timeoutWriter{ResponseWriter: w}
            
            go func() {
                next.ServeHTTP(tw, r.WithContext(ctx))
                close(done)
            }()
            
            select {
            case <-done:
                // Handler completed normally
            case <-ctx.Done():
                // Timeout fired
                tw.mu.Lock()
                defer tw.mu.Unlock()
                if !tw.written {
                    writeError(w, 504, "timeout", "Request timed out")
                }
            }
        })
    }
}
```

This ensures the client gets a proper 504 error envelope even if the handler is stuck. The `timeoutWriter` wrapper prevents double-writes (handler writing after timeout).

### Route Registration (Complete chi Setup)

```go
func NewRouter(h *Handler, agentAuth func(http.Handler) http.Handler) *chi.Mux {
    r := chi.NewRouter()

    // Global middleware (all routes)
    r.Use(Recovery)
    r.Use(RequestID)
    r.Use(Logger)

    // Unauthenticated routes
    r.Post("/agents", h.CreateAgent)
    r.Get("/agents/resident", h.GetResidentAgent)
    r.Get("/health", h.Health)

    // Authenticated routes
    r.Group(func(r chi.Router) {
        r.Use(agentAuth)

        // CRUD routes (with timeout)
        r.Group(func(r chi.Router) {
            r.Use(Timeout(30 * time.Second))

            r.Get("/conversations", h.ListConversations)
            r.Post("/conversations", h.CreateConversation)
            r.Post("/conversations/{cid}/invite", h.InviteAgent)
            r.Post("/conversations/{cid}/leave", h.LeaveConversation)
            r.Post("/conversations/{cid}/messages", h.SendMessage)
            r.Get("/conversations/{cid}/messages", h.GetHistory)
            r.Post("/conversations/{cid}/ack", h.AckCursor)
            r.Get("/agents/me/unread", h.ListUnread)
        })

        // Streaming routes (no timeout — connection-bound)
        r.Post("/conversations/{cid}/messages/stream", h.StreamMessage)
        r.Get("/conversations/{cid}/stream", h.SSEStream)
    })

    return r
}
```

### Route Matching Edge Cases

**Trailing slashes:** chi strips trailing slashes by default (`chi.WithRedirectTrailingSlash`). `/conversations/` redirects to `/conversations`. Consistent.

**Unknown routes:** chi returns 404 automatically. But the default 404 is a plain text "404 page not found." Override with our error envelope:

```go
r.NotFound(func(w http.ResponseWriter, r *http.Request) {
    writeError(w, 404, "not_found", fmt.Sprintf("No route matches %s %s", r.Method, r.URL.Path))
})

r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
    writeError(w, 405, "method_not_allowed",
        fmt.Sprintf("Method %s not allowed on %s", r.Method, r.URL.Path))
})
```

This ensures even routing-level errors use the standard error envelope. An AI agent parsing any response from any URL always gets consistent JSON.

---

## 3. Request/Response Contracts (Go Types)

### Design Principles

1. **`snake_case` JSON field names.** Consistent with spec examples, Anthropic API, S2 API, and Go convention (`json:"field_name"`).
2. **`DisallowUnknownFields()` on all JSON decoders.** Catches AI agent typos (`agnet_id` vs `agent_id`) immediately instead of silently ignoring them. Strict validation > silent forgiveness for API-first consumers.
3. **Native `uuid.UUID` in Go types.** The `google/uuid` package provides `MarshalJSON`/`UnmarshalJSON` that produces `"550e8400-e29b-41d4-a716-446655440000"` format automatically.
4. **`time.Time` serialization is RFC 3339.** Go's `encoding/json` does this by default. `"2026-04-14T10:00:00Z"`.
5. **Empty slices marshal as `[]`, not `null`.** Enabled via sqlc's `emit_empty_slices: true` and explicit `make([]T, 0)` in handlers. An AI agent checking `len(response.conversations)` should not crash on `null`.
6. **No response envelope wrapping success responses.** Success responses return the bare domain object. Only errors use the `{"error":{...}}` envelope. This keeps success paths clean and reduces nesting.

### Complete Type Catalog

#### Error Types

```go
// APIError is the standard error envelope for all error responses.
type APIError struct {
    Error ErrorBody `json:"error"`
}

type ErrorBody struct {
    Code    string `json:"code"`
    Message string `json:"message"`
}
```

#### POST /agents

```go
// No request type (no body)

type CreateAgentResponse struct {
    AgentID uuid.UUID `json:"agent_id"`
}
```

**Wire example:**
```
→ POST /agents
← 201 Created
  {"agent_id":"01906e5b-3c4a-7f1e-8b9d-2a4c6e8f0123"}
```

#### GET /agents/resident

```go
// No request type

type GetResidentAgentResponse struct {
    AgentID uuid.UUID `json:"agent_id"`
}
```

**Wire example:**
```
→ GET /agents/resident
← 200 OK
  {"agent_id":"01906e5b-0000-7000-8000-000000000001"}
```

#### POST /conversations

```go
// No request type (creator is X-Agent-ID)

type CreateConversationResponse struct {
    ConversationID uuid.UUID   `json:"conversation_id"`
    Members        []uuid.UUID `json:"members"`
    CreatedAt      time.Time   `json:"created_at"`
}
```

**Wire example:**
```
→ POST /conversations
  X-Agent-ID: 01906e5b-3c4a-7f1e-8b9d-2a4c6e8f0123
← 201 Created
  {"conversation_id":"01906e5c-1234-7f1e-8b9d-aabbccddeeff","members":["01906e5b-3c4a-7f1e-8b9d-2a4c6e8f0123"],"created_at":"2026-04-14T10:00:00Z"}
```

**Why include `members` in create response:** The creator is auto-added as a member. Returning the member list confirms this. The caller doesn't have to make a separate GET to verify.

**Why include `created_at`:** Useful for sorting, debugging, and display. Zero-cost to include (server has it from the Postgres INSERT RETURNING).

#### GET /conversations

```go
// No request type

type ListConversationsResponse struct {
    Conversations []ConversationSummary `json:"conversations"`
}

type ConversationSummary struct {
    ConversationID uuid.UUID   `json:"conversation_id"`
    Members        []uuid.UUID `json:"members"`
    CreatedAt      time.Time   `json:"created_at"`
}
```

**Wire example:**
```
→ GET /conversations
  X-Agent-ID: 01906e5b-3c4a-...
← 200 OK
  {"conversations":[{"conversation_id":"01906e5c-1234-...","members":["01906e5b-3c4a-...","01906e5b-9f8e-..."],"created_at":"2026-04-14T10:00:00Z"}]}
```

**Empty list:** Returns `{"conversations":[]}`, not `{"conversations":null}`. The `conversations` key is always present and always an array.

**Pagination:** Not implemented for list conversations. At the scale of the take-home (hundreds of conversations per agent), returning the full list is fine. For billions: add `?limit=N&after=cursor` pagination.

**Why wrap in `{"conversations":[...]}` instead of bare array:** Top-level JSON arrays are a security concern in older browsers (JSON hijacking) and make the response non-extensible. Wrapping allows adding `has_more`, `total_count`, etc. later without breaking clients.

#### POST /conversations/{cid}/invite

```go
type InviteRequest struct {
    AgentID uuid.UUID `json:"agent_id"`
}

type InviteResponse struct {
    ConversationID uuid.UUID `json:"conversation_id"`
    AgentID        uuid.UUID `json:"agent_id"`
    AlreadyMember  bool      `json:"already_member"`
}
```

**Wire example (new invite):**
```
→ POST /conversations/01906e5c-1234-.../invite
  X-Agent-ID: 01906e5b-3c4a-...
  Content-Type: application/json
  {"agent_id":"01906e5b-9f8e-7d6c-5b4a-3928170e0f00"}
← 200 OK
  {"conversation_id":"01906e5c-1234-...","agent_id":"01906e5b-9f8e-...","already_member":false}
```

**Wire example (idempotent re-invite):**
```
← 200 OK
  {"conversation_id":"01906e5c-1234-...","agent_id":"01906e5b-9f8e-...","already_member":true}
```

**Why `already_member` field:** The spec says "Inviting an agent that is already a member is a no-op." Both cases return 200. The `already_member` boolean lets the caller know whether the invite actually changed anything. Useful for logging and debugging. Not required for correctness.

**Why 200 for both (not 201 for new, 200 for existing):** The spec says "no-op" for existing members, suggesting identical behavior. Using 200 uniformly avoids clients branching on status codes for idempotent operations. The `already_member` field provides the distinction if needed.

#### POST /conversations/{cid}/leave

```go
// No request type (agent is X-Agent-ID)

type LeaveResponse struct {
    ConversationID uuid.UUID `json:"conversation_id"`
    AgentID        uuid.UUID `json:"agent_id"`
}
```

**Wire example:**
```
→ POST /conversations/01906e5c-1234-.../leave
  X-Agent-ID: 01906e5b-3c4a-...
← 200 OK
  {"conversation_id":"01906e5c-1234-...","agent_id":"01906e5b-3c4a-..."}
```

#### POST /conversations/{cid}/messages (Complete Send)

```go
type SendMessageRequest struct {
    Content string `json:"content"`
}

type SendMessageResponse struct {
    MessageID uuid.UUID `json:"message_id"`
    Seq       uint64    `json:"seq"`
}
```

**Wire example:**
```
→ POST /conversations/01906e5c-1234-.../messages
  X-Agent-ID: 01906e5b-3c4a-...
  Content-Type: application/json
  {"content":"Hello, how are you?"}
← 201 Created
  {"message_id":"01906e5d-5678-...","seq":42}
```

**`seq` field:** The S2 sequence number of the `message_end` record. Useful for cursor positioning — a client that receives this knows it has read up to at least seq 42.

**Content validation:**
- Missing `content` field → 400 `missing_field`
- Empty string `""` → 400 `empty_content`
- Whitespace-only → allowed (agents might intentionally send whitespace)
- No max length enforced at API layer (S2's 1 MiB record limit is the natural cap; at ~4 chars/byte, that's ~250K characters per message — far beyond any LLM response)

#### POST /conversations/{cid}/messages/stream (Streaming Send)

```go
// Request body is NDJSON stream, read line-by-line:
type StreamChunk struct {
    Content string `json:"content"`
}

type StreamMessageResponse struct {
    MessageID uuid.UUID `json:"message_id"`
    SeqStart  uint64    `json:"seq_start"`
    SeqEnd    uint64    `json:"seq_end"`
}
```

**Wire example:**
```
→ POST /conversations/01906e5c-1234-.../messages/stream
  X-Agent-ID: 01906e5b-3c4a-...
  Content-Type: application/x-ndjson
  {"content":"Hello, "}
  {"content":"how are "}
  {"content":"you?"}
← 200 OK
  {"message_id":"01906e5d-9abc-...","seq_start":42,"seq_end":45}
```

**`seq_start` / `seq_end`:** The range of S2 sequence numbers for this message's records (`message_start` through `message_end`). Lets the client know exactly which events it just created.

**Why 200 not 201:** The response is sent after the body is fully read and all S2 appends are acknowledged. By that point, the message is "created." 201 would also be correct, but 200 is more natural for "operation completed" on a streaming request that's more of an action than a resource creation.

#### GET /conversations/{cid}/messages (History — Canonical Passive Catch-Up)

This is the canonical passive-catch-up primitive for the consumption model defined in `spec.md` §1.4. It returns **assembled messages only** — there is intentionally no raw-events alternative, no granularity knob. Active readers that want raw events use SSE.

Two mutually exclusive cursor modes:
- **`before` (pagination, newest-first).** `GET ...?limit=50&before=<seq_num>` — return up to `limit` messages whose `seq_start < before`, ordered by `seq_start` DESC. Used for "show me recent history." `before` omitted = start from the current tail.
- **`from` (catch-up, oldest-first, ack-aligned).** `GET ...?limit=50&from=<seq_num>` — return up to `limit` messages whose `seq_start >= from`, ordered by `seq_start` ASC. Used by passive agents re-engaging after tuning out: `from=<their ack_seq>` returns every complete message that landed after they last acknowledged.

Providing both is a 400 (`mutually_exclusive_cursors`). Providing neither is equivalent to `before=<tail>` (newest-first page from the tail).

```go
// Query parameters:
//   ?limit=50&before=<seq_num>   (pagination, DESC — the historical default)
//   ?limit=50&from=<seq_num>     (catch-up, ASC — ack-cursor-aligned)

type HistoryResponse struct {
    Messages []HistoryMessage `json:"messages"`
    HasMore  bool             `json:"has_more"`
}

type HistoryMessage struct {
    MessageID uuid.UUID `json:"message_id"`
    SenderID  uuid.UUID `json:"sender_id"`
    Content   string    `json:"content"`
    SeqStart  uint64    `json:"seq_start"`
    SeqEnd    uint64    `json:"seq_end"`
    Status    string    `json:"status"`
}
```

**Wire example:**
```
→ GET /conversations/01906e5c-1234-.../messages?limit=50
  X-Agent-ID: 01906e5b-3c4a-...
← 200 OK
  {"messages":[{"message_id":"01906e5d-5678-...","sender_id":"01906e5b-3c4a-...","content":"Hello, how are you?","seq_start":42,"seq_end":45,"status":"complete"},{"message_id":"01906e5d-9abc-...","sender_id":"01906e5b-9f8e-...","content":"I'm doing well, thanks for asking!","seq_start":46,"seq_end":52,"status":"complete"}],"has_more":false}
```

**`status` field (replaces `complete` boolean):**

| Value | Meaning |
|---|---|
| `"complete"` | `message_end` received. Full content available. |
| `"in_progress"` | `message_start` received but no `message_end` or `message_abort`. Currently being streamed. |
| `"aborted"` | `message_abort` received. Partial content — agent disconnected or was removed. |

Why a string enum instead of `bool complete`: three states can't be represented by a boolean. An aborted message is not "complete" but it's also not "in progress." The `status` field is unambiguous.

**Query parameter defaults and limits:**

| Parameter | Default | Min | Max | Notes |
|---|---|---|---|---|
| `limit` | 50 | 1 | 100 | Number of reconstructed messages to return |
| `before` | (stream tail) | 0 | (stream tail) | Exclusive upper bound — return messages whose `seq_start` < `before`. Pagination mode (DESC). Mutually exclusive with `from`. |
| `from` | — | 0 | (stream tail) | Inclusive lower bound — return messages whose `seq_start` >= `from`. Catch-up mode (ASC). Mutually exclusive with `before`. |

**Pagination mechanics (DESC mode):**
- First page: `GET /conversations/{cid}/messages?limit=50` (most recent 50 messages)
- Next page: `GET /conversations/{cid}/messages?limit=50&before={seq_start of last message in previous response}`
- `has_more: true` means there are older messages beyond the returned set

**Catch-up mechanics (ASC mode):**
- First batch: `GET /conversations/{cid}/messages?from=<ack_seq>&limit=50` (oldest unread first)
- Next batch: `GET /conversations/{cid}/messages?from={seq_end + 1 of last message in previous response}&limit=50`
- `has_more: true` means there are newer messages beyond the returned set
- After processing, client acks with `POST /conversations/{cid}/ack` passing the highest processed `seq_end`

**Message ordering:** Messages are ordered by `seq_start` descending (newest first). This is the natural order for chat — you want to see the most recent messages without paging through history.

Wait — actually, let me reconsider ordering. For AI agents catching up, chronological order (oldest first) might be more natural for feeding into an LLM's context window. But for pagination, newest-first is standard (you want page 1 to be the latest messages).

**Decision: Newest first (descending `seq_start`).** This matches:
- Chat convention (most recent at the top of page 1)
- The `before` pagination pattern (go backward in time)
- What an agent needs for "what happened recently?"

For chronological history (feeding into LLM context), the agent fetches all pages and reverses. Or uses the SSE stream which IS chronological.

#### GET /conversations/{cid}/stream (SSE)

No request/response Go types — SSE uses `text/event-stream` format, not JSON. The wire format is already fully specified in spec.md §1.4.

**Query parameters:**

| Parameter | Type | Default | Notes |
|---|---|---|---|
| `from` | uint64 | Agent's stored `delivery_seq` (or 0) | Start reading from this sequence number. Advances the delivery cursor as events are shipped. Does NOT advance `ack_seq` — use `POST /conversations/{cid}/ack` for that. |

#### POST /conversations/{cid}/ack

Canonical client ack for the consumption model. Advances `ack_seq` for `(agent, conversation)` synchronously. Idempotent. See `spec.md` §1.4 and §1.7; see `sql-metadata-plan.md` §7 for the write-through mechanics.

```go
type AckRequest struct {
    Seq uint64 `json:"seq"`
}
// Response: 204 No Content, empty body. No Go type.
```

**Wire example:**
```
→ POST /conversations/01906e5c-1234-.../ack
  X-Agent-ID: 01906e5b-3c4a-...
  Content-Type: application/json
  {"seq":127}
← 204 No Content
```

**Semantics:**
- Validates `0 <= seq <= conversations.head_seq`. `seq > head_seq` → 400 `ack_beyond_head`.
- Regression is silent: `seq < current_ack_seq` leaves the row unchanged and returns 204. The `WHERE cursors.ack_seq < EXCLUDED.ack_seq` guard in the UPSERT handles this at the SQL layer.
- On return, the ack is durable. The next `GET /agents/me/unread` reflects it.
- Non-member: 403 `not_member`. Unknown conversation: 404 `conversation_not_found`.

#### GET /agents/me/unread

Lists every conversation the calling agent is a member of where `head_seq > ack_seq`. Single indexed SQL join — no denormalized counters, no per-agent push streams. See `spec.md` §1.4 for the rationale and `sql-metadata-plan.md` §12 `ListUnreadForAgent` for the query.

```go
// Query parameters: ?limit=100

type UnreadResponse struct {
    Conversations []UnreadEntry `json:"conversations"`
}

type UnreadEntry struct {
    ConversationID uuid.UUID `json:"conversation_id"`
    HeadSeq        uint64    `json:"head_seq"`
    AckSeq         uint64    `json:"ack_seq"`
    EventDelta     uint64    `json:"event_delta"`
}
```

**Wire example:**
```
→ GET /agents/me/unread?limit=100
  X-Agent-ID: 01906e5b-3c4a-...
← 200 OK
  {"conversations":[
    {"conversation_id":"01906e5c-1234-...","head_seq":302,"ack_seq":247,"event_delta":55},
    {"conversation_id":"01906e60-5678-...","head_seq":19,"ack_seq":0,"event_delta":19}
  ]}
```

**Query parameter defaults and limits:**

| Parameter | Default | Min | Max | Notes |
|---|---|---|---|---|
| `limit` | 100 | 1 | 500 | Max number of unread conversations returned, ordered by `head_seq` DESC |

**Semantics:**
- `event_delta` is in **events**, not messages. It is a size signal (zero / some / a lot). To see content, call `GET /conversations/{cid}/messages?from=<ack_seq>`.
- Empty result (`conversations: []`) when the agent has no memberships, or has acked every conversation up to its tail.
- No authentication-related membership check (scoped to the caller's own memberships). Returns 200 with empty list if the agent has no memberships.
- Computed on demand. The server never runs background work on behalf of an agent that never calls this endpoint.

#### GET /health

```go
type HealthResponse struct {
    Status   string            `json:"status"`
    Checks   map[string]string `json:"checks"`
}
```

**Wire example (healthy):**
```
→ GET /health
← 200 OK
  {"status":"ok","checks":{"postgres":"ok","s2":"ok"}}
```

**Wire example (degraded):**
```
→ GET /health
← 503 Service Unavailable
  {"status":"degraded","checks":{"postgres":"ok","s2":"unreachable"}}
```

**Why `map[string]string` for checks:** Extensible. Adding a new dependency check (Redis, Claude API, etc.) doesn't change the type. Values are `"ok"` or a short error description.

**Health check behavior:**
- Postgres: `SELECT 1` with 3-second timeout
- S2: `CheckTail` on a known stream (or basin-level operation) with 3-second timeout
- Status is `"ok"` only if ALL checks pass. Otherwise `"degraded"`.

### JSON Parsing Helper

All JSON-body endpoints use a shared parsing function:

```go
func decodeJSON[T any](w http.ResponseWriter, r *http.Request, maxBytes int64) (T, bool) {
    var v T
    r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
    
    dec := json.NewDecoder(r.Body)
    dec.DisallowUnknownFields()
    
    if err := dec.Decode(&v); err != nil {
        var syntaxErr *json.SyntaxError
        var typeErr *json.UnmarshalTypeError
        var maxBytesErr *http.MaxBytesError
        
        switch {
        case errors.As(err, &syntaxErr):
            writeError(w, 400, "invalid_json",
                fmt.Sprintf("Malformed JSON at position %d", syntaxErr.Offset))
        case errors.As(err, &typeErr):
            writeError(w, 400, "invalid_field",
                fmt.Sprintf("Field '%s' has wrong type: expected %s", typeErr.Field, typeErr.Type))
        case errors.As(err, &maxBytesErr):
            writeError(w, 413, "request_too_large",
                fmt.Sprintf("Request body exceeds %d bytes", maxBytes))
        case strings.HasPrefix(err.Error(), "json: unknown field"):
            field := strings.TrimPrefix(err.Error(), "json: unknown field ")
            writeError(w, 400, "unknown_field",
                fmt.Sprintf("Unknown field: %s", field))
        case errors.Is(err, io.EOF):
            writeError(w, 400, "empty_body", "Request body is empty")
        default:
            writeError(w, 400, "invalid_json",
                fmt.Sprintf("Invalid JSON: %s", err.Error()))
        }
        return v, false
    }
    
    // Reject trailing content (multiple JSON objects in one body)
    if dec.More() {
        writeError(w, 400, "invalid_json", "Unexpected content after JSON object")
        return v, false
    }
    
    return v, true
}
```

**Max body sizes per endpoint:**

| Endpoint | Max Body | Rationale |
|---|---|---|
| POST /conversations/{cid}/invite | 1 KB | Single UUID field. 1 KB is 10x generous. |
| POST /conversations/{cid}/messages | 1 MB | Complete message content. S2's 1 MiB record limit is the natural cap. |
| POST /conversations/{cid}/messages/stream | Unlimited | Streaming body — `MaxBytesReader` not applied. Individual lines bounded by scanner buffer. |
| All other POSTs | 1 KB | Safety net. |

### Path Parameter Parsing Helper

```go
func parseConversationID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
    cidStr := chi.URLParam(r, "cid")
    cid, err := uuid.Parse(cidStr)
    if err != nil {
        writeError(w, 400, "invalid_conversation_id", "Conversation ID must be a valid UUID")
        return uuid.Nil, false
    }
    return cid, true
}
```

### Membership Check Helper

Many endpoints need: parse conversation ID → check conversation exists → check agent is a member. Shared helper:

```go
func (h *Handler) requireMembership(
    w http.ResponseWriter,
    r *http.Request,
) (agentID uuid.UUID, convID uuid.UUID, ok bool) {
    agentID = agentIDFromContext(r.Context())
    
    convID, ok = parseConversationID(w, r)
    if !ok {
        return
    }
    
    exists, err := h.store.Conversations().Exists(r.Context(), convID)
    if err != nil {
        writeError(w, 500, "internal_error", "Internal server error")
        return uuid.Nil, uuid.Nil, false
    }
    if !exists {
        writeError(w, 404, "conversation_not_found",
            fmt.Sprintf("Conversation %s not found", convID))
        return uuid.Nil, uuid.Nil, false
    }
    
    isMember, err := h.store.Members().IsMember(r.Context(), agentID, convID)
    if err != nil {
        writeError(w, 500, "internal_error", "Internal server error")
        return uuid.Nil, uuid.Nil, false
    }
    if !isMember {
        writeError(w, 403, "not_member",
            fmt.Sprintf("Agent %s is not a member of conversation %s", agentID, convID))
        return uuid.Nil, uuid.Nil, false
    }
    
    return agentID, convID, true
}
```

**Note on ordering:** Conversation existence is checked before membership. This produces the correct error: if the conversation doesn't exist, you get 404 (not 403). If it exists but you're not a member, you get 403. The information leak concern is addressed in Section 1 (UUIDv7 makes enumeration impractical).

### Race Condition: Membership Check vs. Concurrent Leave

A subtle race: handler calls `requireMembership` → returns true → another request processes a leave → handler proceeds to write to a conversation the agent just left.

**Impact:** The write succeeds on S2 (S2 doesn't know about membership). A message from a non-member appears on the stream. Readers see a message from an agent that left.

**Severity:** Low. The window is microseconds (between membership check and S2 append). The message is valid — it was sent before the leave was processed. It's temporally correct even if the membership state changed.

**Mitigation (if needed):** Use optimistic locking — after the S2 append, re-check membership. If the agent left during the write, append a `message_abort` retroactively. **Not implemented for take-home** — the race window is negligible and the impact is cosmetic.

**Production mitigation:** The leave handler waits for in-progress writes to complete before deleting membership (spec.md §1.2 already specifies this via ConnRegistry). So this race only affects complete message sends (not streaming, which are tracked in ConnRegistry). For complete sends, the window is the time between membership check and S2 append — single-digit milliseconds. Acceptable.

---

## 4. Content-Type Handling

### Ingress Validation

**JSON endpoints (POST with JSON body):**

Validated in the `decodeJSON` helper (called before any body parsing):

```go
func validateContentType(r *http.Request, expected string) error {
    ct := r.Header.Get("Content-Type")
    if ct == "" {
        return fmt.Errorf("Content-Type header is required")
    }
    mediaType, _, err := mime.ParseMediaType(ct)
    if err != nil {
        return fmt.Errorf("malformed Content-Type header")
    }
    if mediaType != expected {
        return fmt.Errorf("expected %s, got %s", expected, mediaType)
    }
    return nil
}
```

**Lenient parsing via `mime.ParseMediaType`:** Accepts `application/json`, `application/json; charset=utf-8`, `application/json;charset=UTF-8`, etc. Strips parameters before comparison.

**NDJSON streaming endpoint:**

Accepts both `application/x-ndjson` and `application/ndjson` (the `x-` prefix is the IANA-registered form, but many tools omit it):

```go
func validateNDJSONContentType(r *http.Request) error {
    ct := r.Header.Get("Content-Type")
    mediaType, _, _ := mime.ParseMediaType(ct)
    if mediaType != "application/x-ndjson" && mediaType != "application/ndjson" {
        return fmt.Errorf("expected application/x-ndjson, got %s. " +
            "For complete messages, use POST /conversations/{cid}/messages with application/json", mediaType)
    }
    return nil
}
```

**The helpful redirect message:** If someone sends `application/json` to the streaming endpoint, the error message tells them to use the complete message endpoint instead. This is a common mistake an AI agent might make — guiding it to the right endpoint is better than a bare "wrong content type" error.

**GET endpoints:** No Content-Type validation. GET requests don't have bodies. If a Content-Type header is present on a GET, ignore it.

### Egress Content-Type

| Endpoint | Success Content-Type | Error Content-Type |
|---|---|---|
| All JSON endpoints | `application/json` | `application/json` |
| SSE stream | `text/event-stream` | `application/json` (pre-stream errors) |
| Streaming write response | `application/json` | `application/json` |
| Health | `application/json` | `application/json` |

**Key insight:** Error responses are ALWAYS `application/json`, regardless of the endpoint's normal content type. Errors happen during validation (before the streaming content type is set), so the response writer hasn't committed yet.

### Edge Case: Missing Content-Type on POST

Some HTTP clients (notably `curl` without `-H`) send POST requests without a Content-Type header, or with `application/x-www-form-urlencoded` as the default.

**Decision: Require Content-Type on all POSTs with a body.** Return 415 with a clear error message if missing or wrong. This catches misconfigured clients early instead of producing confusing parse errors downstream.

**Exception: POST /agents and POST /conversations/{cid}/leave** — these have no request body. Content-Type is irrelevant and not validated.

### Edge Case: Request Body on GET

Some clients send bodies on GET requests (technically allowed by HTTP spec but discouraged). Our handlers ignore GET request bodies. No validation, no error — just ignore.

---

## 5. Request Timeouts & Connection Lifecycle

### Timeout Tiers

```text
┌───────────────────────────────────────────────────────────────────┐
│                    Timeout Architecture                           │
├─────────────────────┬──────────────┬──────────────────────────────┤
│ Tier                │ Timeout      │ Endpoints                    │
├─────────────────────┼──────────────┼──────────────────────────────┤
│ CRUD (bounded ops)  │ 30 seconds   │ POST /agents                 │
│                     │              │ POST /conversations          │
│                     │              │ GET  /conversations          │
│                     │              │ POST /.../invite             │
│                     │              │ POST /.../leave              │
│                     │              │ POST /.../messages           │
│                     │              │ GET  /.../messages           │
├─────────────────────┼──────────────┼──────────────────────────────┤
│ Health              │ 5 seconds    │ GET /health                  │
├─────────────────────┼──────────────┼──────────────────────────────┤
│ SSE stream          │ None*        │ GET /.../stream              │
│ (connection-bound)  │              │                              │
├─────────────────────┼──────────────┼──────────────────────────────┤
│ Streaming write     │ None*        │ POST /.../messages/stream    │
│ (body-bound)        │              │                              │
└─────────────────────┴──────────────┴──────────────────────────────┘

* "None" means no middleware-level timeout. Handlers manage their own
  lifecycle via idle detection and absolute safety caps.
```

### CRUD Timeout: 30 Seconds

**Why 30 seconds:** Worst-case CRUD path is `POST /conversations/{cid}/leave`:
1. Postgres `SELECT ... FOR UPDATE` (~200µs)
2. Postgres `DELETE` (~100µs)
3. ConnRegistry lookup + cancel + wait (up to 5s per connection, max 2 connections = 10s)
4. S2 `message_abort` append if needed (~40ms with retry)
5. S2 `agent_left` append (~40ms with retry)
6. Membership cache invalidation (~50ns)

Total worst case: ~10.5 seconds. 30 seconds provides 3x headroom for S2 retries and Postgres contention.

**What happens at 30 seconds:**
1. Context cancelled → all in-flight Postgres queries abort → all in-flight S2 operations abort
2. Timeout middleware sends 504 `timeout` error response
3. If the handler was mid-transaction, Postgres rolls back (correct — incomplete operation should not persist)

### SSE Stream Lifecycle

```text
Connection opened
  │
  ├─ Initial validation (agent auth, membership check, cursor lookup)
  │   Timeout: inherited from request context (no explicit cap)
  │
  ├─ S2 ReadSession opened
  │   │
  │   ├─ Catch-up phase (replay historical events)
  │   │   No timeout — replay as fast as S2 delivers
  │   │
  │   ├─ Real-time tailing phase
  │   │   │
  │   │   ├─ Every 30 seconds: send heartbeat comment `: heartbeat\n\n`
  │   │   │   Heartbeat serves two purposes:
  │   │   │   1. Keeps TCP connection alive (prevents NAT/proxy timeout)
  │   │   │   2. Detects dead clients (write fails → connection closed)
  │   │   │
  │   │   ├─ On new event: write SSE event, update cursor, flush response
  │   │   │
  │   │   └─ Absolute safety cap: 24 hours
  │   │       Prevents resource leaks from forgotten connections.
  │   │       Client reconnects with Last-Event-ID.
  │   │
  │   ├─ Context cancelled (leave, server shutdown, safety cap)
  │   │   → Flush cursor, close S2 session, close HTTP response
  │   │
  │   └─ Client disconnect detected (r.Context().Done())
  │       → Flush cursor, close S2 session, deregister from ConnRegistry
  │
  └─ Error during S2 read
      → Send SSE error event, close connection
```

**Heartbeat mechanics:**

```go
heartbeat := time.NewTicker(30 * time.Second)
defer heartbeat.Stop()

for {
    select {
    case <-ctx.Done():
        return
    case <-heartbeat.C:
        _, err := fmt.Fprintf(w, ": heartbeat\n\n")
        if err != nil {
            return // client disconnected
        }
        flusher.Flush()
    case event, ok := <-eventCh:
        if !ok {
            return // channel closed
        }
        writeSSEEvent(w, event)
        flusher.Flush()
    }
}
```

**Why 30-second heartbeats:** Most load balancers (AWS ALB, Fly.io proxy, nginx default) timeout idle connections at 60 seconds. 30-second heartbeats keep the connection active with 2x safety margin. SSE comments (lines starting with `:`) are ignored by SSE clients — they don't trigger event handlers.

**Why 24-hour absolute cap:** Fly.io machines can restart for maintenance, deployments, or scaling events. A 24-hour cap ensures connections are periodically refreshed. The client auto-reconnects with `Last-Event-ID`, so there's no data loss. Without a cap, a leaked goroutine (client that disconnected but the server didn't detect it) runs forever.

### Streaming Write Lifecycle

```text
Connection opened
  │
  ├─ Initial validation (agent auth, membership check, content-type)
  │   If validation fails: return error immediately (no body read)
  │
  ├─ Concurrent write check (ConnRegistry)
  │   If already streaming: return 409 concurrent_write
  │
  ├─ Register in ConnRegistry
  │   Insert in_progress_messages row in Postgres
  │   Open S2 AppendSession
  │   Submit message_start
  │
  ├─ Body reading loop
  │   │
  │   ├─ scanner.Scan() blocks until NDJSON line arrives
  │   │   Idle timeout: 5 minutes between lines
  │   │   │
  │   │   ├─ On line: parse JSON, submit message_append to S2
  │   │   │
  │   │   ├─ On idle timeout: abort message, close connection
  │   │   │   Rationale: LLMs don't pause 5 minutes mid-generation.
  │   │   │   If the client is silent for 5 minutes, it's dead or stuck.
  │   │   │
  │   │   └─ Absolute safety cap: 1 hour
  │   │       No single LLM response runs for an hour.
  │   │
  │   ├─ On EOF (body complete): submit message_end, return success response
  │   │
  │   ├─ On context cancel (leave, server shutdown):
  │   │   Submit message_abort via Unary append (session may be errored)
  │   │   Return (no response — connection is dead)
  │   │
  │   └─ On scanner error / malformed JSON:
  │       Submit message_abort, return error response
  │
  ├─ Deregister from ConnRegistry
  │   Delete in_progress_messages row
  │
  └─ Done
```

**Idle timeout implementation:**

```go
idleTimeout := 5 * time.Minute
absoluteDeadline := time.Now().Add(1 * time.Hour)
timer := time.NewTimer(idleTimeout)
defer timer.Stop()

for {
    select {
    case <-ctx.Done():
        // Context cancelled (leave, shutdown)
        abortMessage(...)
        return
    case <-timer.C:
        // Idle timeout — no NDJSON line for 5 minutes
        abortMessage(...)
        writeError(w, 408, "idle_timeout", "No content received for 5 minutes")
        return
    default:
    }
    
    if time.Now().After(absoluteDeadline) {
        abortMessage(...)
        writeError(w, 408, "max_duration", "Streaming write exceeded 1 hour maximum")
        return
    }
    
    if !scanner.Scan() {
        break // EOF or error
    }
    timer.Reset(idleTimeout)
    // process line...
}
```

Actually, the `select` with `default` plus blocking `scanner.Scan()` doesn't work well together. The scanner blocks the goroutine, so the idle timeout and context cancellation need to run in a separate goroutine or use a different pattern.

**Better approach — deadline on the body reader:**

```go
type deadlineReader struct {
    r       io.Reader
    timeout time.Duration
    timer   *time.Timer
}

func (d *deadlineReader) Read(p []byte) (int, error) {
    d.timer.Reset(d.timeout)
    ch := make(chan readResult, 1)
    go func() {
        n, err := d.r.Read(p)
        ch <- readResult{n, err}
    }()
    select {
    case res := <-ch:
        return res.n, res.err
    case <-d.timer.C:
        return 0, errIdleTimeout
    }
}
```

Wait — this spawns a goroutine per `Read` call, which is expensive. Better approach: use the request context's deadline. Set a 5-minute rolling deadline that resets after each line:

Actually, the cleanest Go pattern for this is to wrap `r.Body` in a reader that respects both context cancellation AND idle timeout. The standard approach:

```go
// Wrap the body with idle timeout
body := newIdleTimeoutReader(r.Body, 5*time.Minute)
scanner := bufio.NewScanner(body)

for scanner.Scan() {
    // scanner.Scan() calls body.Read(), which resets the idle timer
    // on each successful read
    // ... process line ...
}
```

The idle timeout reader uses `SetReadDeadline` on the underlying TCP connection (accessible via `http.ResponseController` in Go 1.20+):

```go
rc := http.NewResponseController(w)

for scanner.Scan() {
    // Reset read deadline before each scan
    rc.SetReadDeadline(time.Now().Add(5 * time.Minute))
    // ... process line ...
}
```

`http.ResponseController.SetReadDeadline` sets a deadline on the underlying network connection's read side. When the deadline fires, `scanner.Scan()` returns with an `os.ErrDeadlineExceeded` error. Clean, no goroutine leaks, no channels.

**This is the recommended approach.** `http.ResponseController` is available in Go 1.20+ (our minimum Go version is 1.22 for generics and slices package).

### Health Endpoint Timeout: 5 Seconds

The health endpoint is called by load balancers and monitoring systems every 10-30 seconds. It must be fast. Individual checks have 3-second timeouts:

```go
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
    defer cancel()
    
    checks := make(map[string]string)
    
    // Postgres check
    if err := h.pool.Ping(ctx); err != nil {
        checks["postgres"] = "unreachable: " + err.Error()
    } else {
        checks["postgres"] = "ok"
    }
    
    // S2 check
    if _, err := h.s2.CheckTail(ctx, "health-check-stream"); err != nil {
        checks["s2"] = "unreachable: " + err.Error()
    } else {
        checks["s2"] = "ok"
    }
    
    status := "ok"
    statusCode := 200
    for _, v := range checks {
        if v != "ok" {
            status = "degraded"
            statusCode = 503
            break
        }
    }
    
    writeJSON(w, statusCode, HealthResponse{Status: status, Checks: checks})
}
```

### Timeout Behavior Under Load

**Connection pool exhaustion:** If all 15 pgxpool connections are busy, a new request waits for a connection. The 30-second CRUD timeout covers this wait — if a connection isn't available in 30 seconds, the request times out. This is correct behavior: the client retries, and the server isn't accumulating unbounded goroutines.

**S2 slowness:** S2 Express ack is normally ~40ms. If S2 becomes slow (degraded mode, network issues), appends may take 1-5 seconds. The retry policy (3 attempts, exponential backoff) has a total timeout of ~10 seconds. Well within the 30-second CRUD timeout.

**Thundering herd after restart:** On server restart, all SSE clients reconnect simultaneously. Each re-validates membership (Postgres hit, but membership cache is warmed on startup) and opens an S2 read session. The Postgres pool handles the burst (15 connections × 50K queries/sec capacity). S2 sessions are TCP connections — the Go runtime handles thousands concurrently.

---

## 6. Cross-Cutting Concerns

### Response Writing Helper

```go
func writeJSON(w http.ResponseWriter, status int, v any) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    if err := json.NewEncoder(w).Encode(v); err != nil {
        log.Error().Err(err).Msg("failed to write response")
    }
}
```

**`json.NewEncoder(w).Encode(v)` vs `json.Marshal(v)` + `w.Write()`:** Encoder writes directly to the response writer, avoiding an intermediate `[]byte` allocation. For small responses (our case), the difference is negligible. But Encoder also appends a trailing newline, which makes `curl` output more readable.

### Context Keys

```go
type contextKey int

const (
    ctxKeyRequestID contextKey = iota
    ctxKeyAgentID
)

func requestIDFromContext(ctx context.Context) string {
    id, _ := ctx.Value(ctxKeyRequestID).(string)
    return id
}

func agentIDFromContext(ctx context.Context) uuid.UUID {
    id, _ := ctx.Value(ctxKeyAgentID).(uuid.UUID)
    return id
}
```

**Why custom type, not `string` key:** The `context.WithValue` docs explicitly recommend against string keys to avoid collisions between packages. Custom `contextKey` type is unexported, so no external package can accidentally read or overwrite our values.

### CORS

**Not implemented.** The spec says "do not implement authentication, authorization, user management, or any other peripheral concerns." CORS is a browser security mechanism. AI coding agents are HTTP clients, not browsers. They don't send CORS preflight requests.

If the evaluator tests from a browser-based tool (Postman, Insomnia, or a custom web UI), CORS would block requests. Mitigation: document in CLIENT.md that the API is designed for programmatic clients, not browsers. If needed, adding `Access-Control-Allow-Origin: *` is a one-line middleware addition — zero impact on the rest of the design.

### Rate Limiting

**Not implemented.** The spec says to focus on the core messaging problem. Rate limiting is a peripheral concern.

**The risk:** Without rate limiting, one agent can:
- Open 10,000 SSE connections (goroutine/memory exhaustion)
- Send 100,000 messages/sec (S2 cost)
- Create 1,000,000 agents (Postgres storage)

**For the take-home:** Acceptable. The evaluator won't attack their own test service.

**Production mitigation (documented, not built):**
- Per-agent rate limits: 10 SSE connections, 100 messages/min, 10 conversations/min
- Global rate limits: 10,000 SSE connections total, 10,000 messages/sec
- Implementation: `golang.org/x/time/rate` token bucket per agent ID
- Enforcement: middleware between agent auth and handler

### Idempotency

| Endpoint | Idempotent? | Mechanism |
|---|---|---|
| POST /agents | No (each call creates new agent) | By design |
| POST /conversations | No (each call creates new conversation) | By design |
| POST /conversations/{cid}/invite | Yes | `INSERT ... ON CONFLICT DO NOTHING` |
| POST /conversations/{cid}/leave | No (second leave returns 403) | Membership check |
| POST /conversations/{cid}/messages | No (each call creates new message) | By design |
| POST /conversations/{cid}/messages/stream | No (each call creates new message) | `message_id` as recovery key |

No explicit idempotency key header needed. The natural semantics are sufficient.

### Request Logging Correlation

Every log line includes:
- `request_id`: from Request ID middleware
- `agent_id`: from Agent Auth middleware (empty for unauthenticated routes)
- `conversation_id`: from handler (when applicable)
- `message_id`: from handler (when applicable)

This enables end-to-end tracing: given a request ID, find all related log entries across middleware and handler.

### Graceful Shutdown Interaction

When the server receives SIGTERM:
1. Stop accepting new connections (HTTP server `Shutdown()`)
2. In-flight CRUD requests complete (up to 30-second timeout)
3. SSE connections receive a final heartbeat and close
4. Streaming writes abort (context cancelled, `message_abort` appended)
5. All cursor flushes complete
6. ConnRegistry drains
7. Postgres pool closes
8. S2 sessions close
9. Process exits

The 30-second CRUD timeout ensures in-flight requests don't block shutdown indefinitely. The `http.Server.Shutdown(ctx)` call uses a context with a 30-second deadline — if handlers don't finish by then, they're force-killed.

---

## 7. Handler Structure Template

Every handler follows the same structure:

```go
func (h *Handler) ExampleEndpoint(w http.ResponseWriter, r *http.Request) {
    // 1. Extract and validate parameters
    agentID, convID, ok := h.requireMembership(w, r)
    if !ok {
        return
    }
    
    // 2. Parse request body (if applicable)
    req, ok := decodeJSON[ExampleRequest](w, r, 1024)
    if !ok {
        return
    }
    
    // 3. Validate business rules
    if req.Content == "" {
        writeError(w, 400, "empty_content", "Content must not be empty")
        return
    }
    
    // 4. Execute operation (store/S2)
    result, err := h.doSomething(r.Context(), agentID, convID, req)
    if err != nil {
        // Map domain errors to HTTP errors
        switch {
        case errors.Is(err, store.ErrLastMember):
            writeError(w, 409, "last_member",
                "Cannot leave: you are the last member of this conversation")
        default:
            log.Error().Err(err).
                Str("request_id", requestIDFromContext(r.Context())).
                Msg("operation failed")
            writeError(w, 500, "internal_error", "Internal server error")
        }
        return
    }
    
    // 5. Write success response
    writeJSON(w, 200, result)
}
```

**Key principles:**
- **Early return on error.** No nested if/else chains. Each validation step either succeeds or writes an error and returns.
- **Domain errors mapped to HTTP errors.** The store layer returns domain errors (`ErrLastMember`, `ErrNotFound`). The handler maps them to HTTP status codes and error codes. The store never knows about HTTP.
- **Internal errors logged, not exposed.** The client gets `"Internal server error"`. The log gets the full error with stack context and request ID.
- **No business logic in handlers.** Handlers orchestrate: validate → execute → respond. Business logic lives in the store layer.

---

## 8. Complete Endpoint Summary

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

---

## 9. Scaling Considerations for the API Layer

### At Thousands (Take-Home)

Everything in this document works as-is. Single Fly.io instance, `sync.Map` agent cache, no rate limiting, no CORS.

### At Millions

- **Agent cache:** `sync.Map` still viable (~32 MB at 2M agents). Monitor memory.
- **Rate limiting:** Add per-agent token bucket middleware.
- **Request logging:** Switch from synchronous zerolog to async buffered logger (or ship to external log service).
- **Connection limits:** Add per-agent SSE connection limit (e.g., max 100 concurrent SSE connections per agent).
- **Horizontal scaling:** Stateless HTTP handlers + external storage (Postgres + S2) means multiple instances behind a load balancer work out of the box. SSE connections are instance-local — client reconnects go to any instance (cursor in Postgres handles resume).

### At Billions

- **Agent cache:** Bloom filter + LRU. Bloom filter sized for total agent population. LRU sized for working set (agents active in last hour).
- **Rate limiting:** Distributed rate limiting via Redis or a dedicated rate limit service. Per-agent limits enforced globally across all instances.
- **Request routing:** API gateway (e.g., Kong, Envoy) in front of application servers. Gateway handles TLS termination, rate limiting, request routing, CORS.
- **Response compression:** Add gzip middleware for JSON responses >1 KB. SSE is not compressed (breaks streaming semantics in most proxies).
- **Request queueing:** Under extreme load, queue requests and shed excess load with 503 (not 504). Use a bounded work queue per endpoint type.
- **Multi-region:** API layer is stateless — deploy in multiple regions. Postgres (or Postgres equivalent) needs read replicas per region. S2 handles cross-region natively (basin scope).

---

## 10. Files

All HTTP API layer code lives in `internal/api/`:

| File | Contents |
|---|---|
| `internal/api/router.go` | Chi router setup, route registration, route groups |
| `internal/api/middleware.go` | Recovery, Request ID, Logger, Agent Auth, Timeout |
| `internal/api/errors.go` | `APIError` type, `writeError`, `writeJSON`, `fallbackErrorBody` |
| `internal/api/helpers.go` | `decodeJSON`, `parseConversationID`, `requireMembership`, `validateContentType` |
| `internal/api/types.go` | All request/response Go structs |
| `internal/api/agents.go` | `CreateAgent`, `GetResidentAgent` handlers |
| `internal/api/conversations.go` | `CreateConversation`, `ListConversations`, `InviteAgent`, `LeaveConversation` handlers |
| `internal/api/messages.go` | `SendMessage`, `StreamMessage` handlers |
| `internal/api/sse.go` | `SSEStream` handler |
| `internal/api/history.go` | `GetHistory` handler |
| `internal/api/health.go` | `Health` handler |
| `internal/store/agent_cache.go` | `sync.Map`-based agent existence cache, startup warming |
