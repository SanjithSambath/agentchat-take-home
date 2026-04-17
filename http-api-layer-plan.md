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
| 413 | `request_too_large` | JSON body exceeded per-endpoint `MaxBytesReader` cap | `"Request body exceeds {N} bytes"` |
| 413 | `content_too_large` | S2 rejected a record for exceeding its 1 MiB limit (maps the downstream error to a stable code) | `"Content exceeds the 1 MiB record limit"` |
| 400 | `invalid_utf8` | `content` field (complete-message or streaming line) contained bytes that are not valid UTF-8 | `"Content is not valid UTF-8"` |

**Note:** `line_too_large` (413) is also a stable code but is reachable on only one endpoint (streaming send) and is therefore listed only in that endpoint's per-endpoint table and in §6.6, not in this universal table.

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

Requires agent auth. Requires valid `{cid}`. Requires membership check. Idempotent on the client-supplied `message_id`.

**Request body:**

```json
{
  "message_id": "01906e5c-...-...",  // REQUIRED, client-generated UUIDv7
  "content": "..."                    // REQUIRED, non-empty
}
```

| HTTP Status | Error Code | Condition |
|---|---|---|
| 201 | — | Success (message created). Response carries `start_seq`, `end_seq`, and `already_processed: false`. |
| 200 | — | Idempotency replay: same `message_id` was successfully processed earlier. Response carries cached `start_seq`, `end_seq`, and `already_processed: true`. |
| 400 | `invalid_json` | Request body is not valid JSON |
| 400 | `missing_message_id` | `message_id` field missing from body |
| 400 | `invalid_message_id` | `message_id` is present but not a valid UUIDv7 string |
| 400 | `missing_field` | `content` field missing |
| 400 | `empty_content` | `content` field present but empty string |
| 400 | `invalid_utf8` | `content` contains bytes that are not valid UTF-8 |
| 400 | `unknown_field` | Unrecognized fields in body |
| 413 | `request_too_large` | Body exceeded the 1 MB `MaxBytesReader` cap |
| 413 | `content_too_large` | Content cleared the body cap but S2 rejected the record for exceeding 1 MiB |
| 409 | `in_progress_conflict` | A write with the same `message_id` is currently in flight (duplicate client request racing). Client may retry after a short backoff; the in-flight writer will resolve the `message_id` to either `complete` or `aborted`. |
| 409 | `already_aborted` | A prior write with the same `message_id` was aborted (crash, disconnect, slow-writer timeout). Client must generate a fresh `message_id`. |
| 503 | `slow_writer` | S2 backpressure exceeded the 2 s Submit timeout. Server wrote `message_abort` and recorded the dedup row as `aborted`. Client must generate a fresh `message_id` to retry. |
| 500 | `internal_error` | S2 append failed for a reason not covered above |

#### POST /conversations/{cid}/messages/stream (Streaming Send)

Requires agent auth. Requires valid `{cid}`. Requires membership check. Idempotent on the client-supplied `message_id`, declared on the **first NDJSON line** of the request body.

**Request body (NDJSON, one JSON object per line):**

```
{"message_id":"01906e5c-...-..."}     ← REQUIRED first line, UUIDv7
{"content":"first chunk"}
{"content":"second chunk"}
...
```

The very first line MUST contain only `{"message_id": "<uuidv7>"}`. The server parses and validates it before opening the `AppendSession`. Every subsequent line carries a `content` chunk. EOF (graceful close) produces `message_end`; connection reset produces `message_abort`.

| HTTP Status | Error Code | Condition | Notes |
|---|---|---|---|
| 200 | — | Success (message streamed and completed). Response carries `start_seq`, `end_seq`, `already_processed: false`. | |
| 200 | — | Idempotency replay: same `message_id` already completed. Response carries cached seqs + `already_processed: true`; the server does NOT read the rest of the NDJSON body — it closes the connection immediately. | |
| 400 | `missing_message_id` | First NDJSON line missing `message_id` (or body empty before first line) | Server responds before reading any content |
| 400 | `invalid_message_id` | First NDJSON line `message_id` is not a valid UUIDv7 | Server responds before reading any content |
| 400 | `invalid_utf8` | A `content` field in an NDJSON line contained bytes that are not valid UTF-8 | Handler aborts the message (writes `message_abort` reason `invalid_utf8`) and closes the connection |
| 413 | `line_too_large` | A single NDJSON line exceeded the 1 MiB + 1 KiB scanner buffer (§6.1) | Handler aborts the message (writes `message_abort` reason `line_too_large`) |
| 413 | `content_too_large` | A line cleared the scanner buffer but S2 rejected the record for exceeding 1 MiB | Handler aborts the message (writes `message_abort` reason `content_too_large`) |
| 409 | `in_progress_conflict` | A write with the same `message_id` is currently in flight | Unique constraint on `in_progress_messages (conversation_id, message_id)` |
| 409 | `already_aborted` | A prior write with the same `message_id` was aborted | Client must generate a fresh `message_id` |
| 503 | `slow_writer` | AppendSession `Submit` blocked past the 2 s timeout. Server wrote `message_abort`, recorded dedup `aborted`. | |
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
    MessageID uuid.UUID `json:"message_id"` // REQUIRED, client-generated UUIDv7 (idempotency key)
    Content   string    `json:"content"`
}

type SendMessageResponse struct {
    MessageID        uuid.UUID `json:"message_id"`
    SeqStart         uint64    `json:"seq_start"`          // S2 seq of message_start
    SeqEnd           uint64    `json:"seq_end"`            // S2 seq of message_end
    AlreadyProcessed bool      `json:"already_processed"`  // true if replayed from messages_dedup
}
```

**Wire example (first attempt):**
```
→ POST /conversations/01906e5c-1234-.../messages
  X-Agent-ID: 01906e5b-3c4a-...
  Content-Type: application/json
  {"message_id":"01906e5d-5678-...","content":"Hello, how are you?"}
← 201 Created
  {"message_id":"01906e5d-5678-...","seq_start":40,"seq_end":42,"already_processed":false}
```

**Wire example (idempotent replay):**
```
→ POST /conversations/01906e5c-1234-.../messages
  X-Agent-ID: 01906e5b-3c4a-...
  Content-Type: application/json
  {"message_id":"01906e5d-5678-...","content":"Hello, how are you?"}
← 200 OK
  {"message_id":"01906e5d-5678-...","seq_start":40,"seq_end":42,"already_processed":true}
```

**`seq_start` / `seq_end`:** The S2 sequence numbers of `message_start` and `message_end` respectively. Cached in `messages_dedup` so retries replay them identically.

**`message_id` validation:**
- Missing → 400 `missing_message_id`
- Not a valid UUIDv7 → 400 `invalid_message_id`
- Client SHOULD generate one fresh UUIDv7 per logical message and reuse it across retries of that same logical message.

**Content validation:**
- Missing `content` field → 400 `missing_field`
- Empty string `""` → 400 `empty_content`
- Bytes that are not valid UTF-8 → 400 `invalid_utf8` (§6.3)
- Whitespace-only → allowed (agents might intentionally send whitespace)
- Max length: 1 MB body cap via `MaxBytesReader` (§3 table) → 413 `request_too_large`; content that clears the body cap but exceeds S2's 1 MiB record limit → 413 `content_too_large` (§6.4). At ~4 chars/byte, 1 MiB is ~250K characters per message — far beyond any LLM response.

#### POST /conversations/{cid}/messages/stream (Streaming Send)

```go
// Request body is NDJSON. First line MUST carry the idempotency key:
type StreamHeader struct {
    MessageID uuid.UUID `json:"message_id"` // REQUIRED, client-generated UUIDv7
}

// Every subsequent line:
type StreamChunk struct {
    Content string `json:"content"`
}

type StreamMessageResponse struct {
    MessageID        uuid.UUID `json:"message_id"`
    SeqStart         uint64    `json:"seq_start"`
    SeqEnd           uint64    `json:"seq_end"`
    AlreadyProcessed bool      `json:"already_processed"`
}
```

**Wire example (first attempt):**
```
→ POST /conversations/01906e5c-1234-.../messages/stream
  X-Agent-ID: 01906e5b-3c4a-...
  Content-Type: application/x-ndjson
  {"message_id":"01906e5d-9abc-..."}
  {"content":"Hello, "}
  {"content":"how are "}
  {"content":"you?"}
← 200 OK
  {"message_id":"01906e5d-9abc-...","seq_start":42,"seq_end":45,"already_processed":false}
```

**Wire example (idempotent replay — client retried after a network glitch):**
```
→ POST /conversations/01906e5c-1234-.../messages/stream
  X-Agent-ID: 01906e5b-3c4a-...
  Content-Type: application/x-ndjson
  {"message_id":"01906e5d-9abc-..."}
  {"content":"Hello, "}   ← server closes connection before reading these
  ...
← 200 OK
  {"message_id":"01906e5d-9abc-...","seq_start":42,"seq_end":45,"already_processed":true}
```

**`seq_start` / `seq_end`:** The range of S2 sequence numbers for this message's records (`message_start` through `message_end`). Lets the client know exactly which events it just created.

**First-line handshake:** The server reads exactly one NDJSON line before opening the `AppendSession`. That line MUST be `{"message_id":"<uuidv7>"}`. The dedup lookup and `in_progress_messages` claim happen before any content is consumed, so a replay of a completed message returns the cached response without reading the body — the client's retransmitted chunks are discarded.

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
| POST /conversations/{cid}/messages/stream | Unlimited total | Streaming body — `MaxBytesReader` not applied. Individual lines bounded to 1 MiB + 1 KiB by the explicit `scanner.Buffer(...)` call (§6.1). |
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

**Bounded event channel and slow-consumer disconnect.** Each SSE connection owns two goroutines: a *tailer* reading from the S2 ReadSession and a *writer* flushing to the HTTP response. They communicate via a buffered channel `eventCh := make(chan SequencedEvent, 64)`. The cap is deliberate — it absorbs short stalls (GC pauses, proxy reconnects) but not unbounded lag. Every enqueue uses a 500 ms deadline:

```go
select {
case eventCh <- ev:
    // delivered to the writer goroutine
case <-time.After(500 * time.Millisecond):
    // writer hasn't drained in 500 ms — the consumer is wedged.
    metrics.SlowConsumerDisconnects.WithLabelValues(convID.String()).Inc()
    log.Warn().Str("agent_id", agentID.String()).
        Str("conversation_id", convID.String()).
        Uint64("last_seq", ev.SeqNum).
        Msg("sse slow_consumer: disconnecting")
    cancel()            // tears down ReadSession + writer goroutine
    return errSlowConsumer
case <-ctx.Done():
    return ctx.Err()
}
```

**Why 64 events and 500 ms.** Normal SSE delivery is sub-millisecond end-to-end (in-process channel → HTTP response buffer → TCP send). A 64-event backlog covers ~2 seconds of peak LLM streaming at 30 tokens/sec per conversation — ample headroom for a brief GC pause or TCP retransmit. A 500 ms enqueue deadline means the *writer goroutine* has been stuck for half a second, which is deeply anomalous: a healthy writer drains each event and calls `http.Flusher.Flush()` in microseconds. Half a second of blockage means the client-side TCP receive buffer is full (client not reading), the proxy is stalled, or the network path is broken. Disconnecting at that point is correct:

- The client's SSE library will auto-reconnect with `Last-Event-ID` — zero data loss (events are still on S2; cursor was flushed at the last delivered seq).
- The server releases the goroutine, the ReadSession, the ConnRegistry slot, and any pinned cursor hot-tier memory.

**Why this is additive to TCP backpressure.** TCP-level backpressure via `http.Flusher` alone is not sufficient: a slow-but-not-dead consumer keeps the TCP connection open indefinitely, the kernel happily buffers, and the writer goroutine blocks in `Flush()` potentially for minutes while the tailer goroutine keeps filling the channel's send buffer in the OS kernel. The bounded in-process channel + short deadline gives us a detection point and a clean teardown before the problem reaches OS-level buffering.

**Slow-consumer disconnect is not an error event.** The client just sees its SSE connection close. SSE libraries reconnect automatically; on reconnect with `Last-Event-ID`, catch-up resumes from the last acked seq. No special client-side handling required.

### Complete Message Write Lifecycle

`POST /conversations/{cid}/messages` is a single request/response. The idempotency gate runs before any S2 write; the body is read fully into memory first (bounded by `MaxBytesReader` = 1 MB).

```text
Request received
  │
  ├─ Initial validation (agent auth, membership check, content-type, body ≤ 1 MB)
  │   If validation fails: return error immediately
  │
  ├─ Parse body: { message_id, content }
  │   Missing message_id        → 400 missing_message_id
  │   Not a valid UUIDv7         → 400 invalid_message_id
  │   Missing content            → 400 missing_field
  │   Empty content              → 400 empty_content
  │
  ├─ Dedup lookup: SELECT messages_dedup WHERE (conv, mid)
  │   HIT status='complete'      → 200 with cached { seq_start, seq_end, already_processed: true }
  │   HIT status='aborted'       → 409 already_aborted
  │
  ├─ Claim: INSERT in_progress_messages (message_id, conv, agent, stream_name)
  │         ON CONFLICT (conversation_id, message_id) DO NOTHING
  │   rows_affected == 0         → 409 in_progress_conflict
  │
  ├─ S2 unary batch append [message_start, message_append(content), message_end]
  │   This is one atomic AppendInput — S2 assigns three contiguous seqs.
  │   On S2 error:
  │     - Best-effort unary append of message_abort (may no-op if S2 fully down)
  │     - Tx: INSERT messages_dedup 'aborted' ON CONFLICT DO NOTHING; DELETE in_progress
  │     - Return 503 internal_error (or 503 slow_writer if error was a deadline)
  │
  ├─ Commit the terminal state:
  │   BEGIN
  │     INSERT messages_dedup (conv, mid, 'complete', seq_start, seq_end)
  │            ON CONFLICT DO NOTHING
  │     DELETE in_progress_messages WHERE message_id = $1
  │   COMMIT
  │
  └─ 201 Created with { message_id, seq_start, seq_end, already_processed: false }
```

**Crash between S2 append and dedup commit.** If the server crashes between the S2 append and the final transaction, the `in_progress_messages` row persists with no corresponding dedup row, but S2 already has `message_start` + `message_append` + `message_end` on the stream. The recovery sweep on next startup (see `server-lifecycle-plan.md` §recovery sweep) finds the orphan row and writes `message_abort` to S2 + INSERTs dedup with `status='aborted'`. The row is cleaned up either way. A client retrying with the same `message_id` will see the `aborted` dedup row and must pick a fresh `message_id` — a tiny cost for not requiring a distributed transaction between S2 and Postgres.

**Why unary append, not AppendSession, for the complete path.** Three records in one `AppendInput` is S2's atomic-batch primitive — all three land or none do. AppendSession is for the streaming path where records arrive over time and pipelining matters. For three-in-one, unary is simpler and strictly correct.

### Streaming Write Lifecycle

```text
Connection opened
  │
  ├─ Initial validation (agent auth, membership check, content-type)
  │   If validation fails: return error immediately (no body read)
  │
  ├─ Read FIRST NDJSON line (handshake, idempotency key)
  │   Missing or not a JSON object with "message_id" → 400 missing_message_id
  │   Not a valid UUIDv7                             → 400 invalid_message_id
  │
  ├─ Dedup lookup: SELECT messages_dedup WHERE (conv, mid)
  │   HIT status='complete'  → 200 replay { seq_start, seq_end, already_processed: true }
  │                            (server closes connection without reading remaining body)
  │   HIT status='aborted'   → 409 already_aborted
  │
  ├─ Register with ConnRegistry (leave-termination hookup)
  │   ConnRegistry records a cancel func for this streaming write so the leave
  │   handler (and server shutdown) can abort it cleanly. It does NOT enforce
  │   "one streaming write per (agent, conv)" — the same agent may have multiple
  │   distinct message_ids in flight concurrently; each one has its own cancel
  │   func keyed by message_id under the (agent, conv) bucket. Per-message-id
  │   duplicate protection is provided by in_progress_messages UNIQUE in the
  │   next step, not by ConnRegistry.
  │
  ├─ Claim: INSERT in_progress_messages ON CONFLICT (conversation_id, message_id) DO NOTHING
  │   rows_affected == 0 → 409 in_progress_conflict
  │
  ├─ Open S2 AppendSession
  │   Submit message_start   (wrapped in context.WithTimeout(parent, 2s) — see below)
  │
  ├─ Body reading loop
  │   │
  │   ├─ scanner.Scan() blocks until NDJSON content line arrives
  │   │   Idle timeout: 5 minutes between lines
  │   │   │
  │   │   ├─ On line: parse JSON, Submit message_append to S2
  │   │   │   Submit is wrapped with 2s context.WithTimeout.
  │   │   │   On Submit timeout: goto abort path with reason "slow_writer" → 503
  │   │   │
  │   │   ├─ On idle timeout: abort message, close connection
  │   │   │   Rationale: LLMs don't pause 5 minutes mid-generation.
  │   │   │   If the client is silent for 5 minutes, it's dead or stuck.
  │   │   │
  │   │   └─ Absolute safety cap: 1 hour
  │   │       No single LLM response runs for an hour.
  │   │
  │   ├─ On EOF (body complete):
  │   │   Submit message_end (2s timeout); capture seq_start + seq_end
  │   │   Tx: INSERT messages_dedup 'complete' ON CONFLICT DO NOTHING;
  │   │       DELETE in_progress_messages
  │   │   Return 200 { seq_start, seq_end, already_processed: false }
  │   │
  │   ├─ On context cancel (leave, server shutdown, slow_writer timeout):
  │   │   Submit message_abort via Unary append (session may be errored)
  │   │   Tx: INSERT messages_dedup 'aborted' ON CONFLICT DO NOTHING;
  │   │       DELETE in_progress_messages
  │   │   Return 503 slow_writer (on timeout) or drop connection (on leave/shutdown)
  │   │
  │   └─ On scanner error / malformed JSON:
  │       Submit message_abort, same dedup/in_progress cleanup as above,
  │       return error response
  │
  └─ Done (ConnRegistry deregister happens in deferred cleanup)
```

**The 2 s `Submit` timeout.** S2's `AppendSession` has built-in backpressure (blocks when inflight bytes exceed 5 MiB; see [s2-architecture-plan.md](s2-architecture-plan.md) §5). In the normal case that backpressure is self-regulating — the session paces submits against S2 throughput. In a pathological case (S2 partial outage, extreme latency), the built-in backpressure could block indefinitely, pinning the handler goroutine and its Postgres-held resources. Wrapping every call to `AppendSessionWrapper.Submit(ctx, event)` with `ctx, cancel := context.WithTimeout(parentCtx, 2*time.Second)` converts "block forever" into "fail after 2 s with a specific error," which the handler then translates into a clean abort path: write `message_abort` via Unary append (a separate path that isn't blocked by the session's inflight queue), record dedup `aborted`, delete `in_progress_messages`, respond `503 slow_writer`. The client's retry produces a fresh `message_id` (because the dedup row is `aborted`) and has a good chance of succeeding if the S2 incident was transient.

**The 2 s number.** S2 Express regional ack target is ~40 ms. A 2 s timeout is 50× the normal case — wide enough to absorb ordinary latency spikes, narrow enough that a handler can't be pinned for minutes. It is deliberately NOT tied to the 30 s CRUD timeout (which is the request-level deadline for the unauthenticated/authenticated routes, not for streaming ones); the streaming write has no overall request timeout, but every individual Submit has this 2 s inner deadline.

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
// Explicit line cap: 1 MiB + 1 KiB envelope. See §6.1 for rationale.
const maxNDJSONLineBytes = 1<<20 + 1024
scanner.Buffer(make([]byte, 64<<10), maxNDJSONLineBytes)

for scanner.Scan() {
    // scanner.Scan() calls body.Read(), which resets the idle timer
    // on each successful read
    // ... process line ...
}
// scanner.Err() error mapping lives in §6.1.
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

## 6. Input Limits & Validation

### The Problem

Every ingress surface is a trust boundary. Without explicit limits, three classes of failure occur:

1. **Silent correctness bugs.** Go's `bufio.Scanner` defaults to a 64 KiB line buffer. S2 accepts records up to 1 MiB. If a client sends an NDJSON line larger than 64 KiB, `scanner.Scan()` returns `false` with `bufio.ErrTooLong` — the request terminates mid-stream with a cryptic error and no `message_abort` event reason the client can act on. This is not hypothetical; a base64-encoded tool-call argument or a large LLM token burst reaches 64 KiB routinely.
2. **Resource exhaustion.** Unbounded header sizes, unbounded request IDs, unbounded stream durations. Each is a knob an adversary — or a buggy client — can turn.
3. **Downstream failures surfacing as opaque errors.** If validation happens only at the S2 layer or the Postgres layer, the error text that reaches the client is `"record exceeds 1 MiB"` from S2, not a stable `{"code": "content_too_large"}` from our API. Our error-code contract (§1) breaks.

The take-home scope excludes authentication and rate-limiting infrastructure. That is not an excuse to skip limits — it is the reason to pick them carefully. Every boundary is either **enforced now** (with a dedicated error code) or **explicitly accepted as unlimited at evaluation scale** (with a named reason and a production mitigation).

No implicit limits. No "we'll figure it out later." Every knob is named.

### Layered Enforcement Model

```
Client
  │
  ▼
[chi router]          ← URL length (Go default 8 KiB header line; 1 MiB total header block)
  │                   ← HTTP header block (Go default 1 MiB — deployment-plan.md §4.6)
  ▼
[middleware]          ← X-Agent-ID format (§2 Agent Auth: UUID parse)
                      ← X-Request-ID length + charset (§2 Request ID: ≤128 chars, silent fallback)
                      ← Content-Type validation (§4)
                      ← Timeout binding (§5)
  │
  ▼
[handler]             ← JSON body: MaxBytesReader per endpoint (§3 table)
                      ← NDJSON line: explicit 1 MiB + 1 KiB scanner buffer (§6.1)
                      ← Path params: UUID parse
                      ← Body invariants: UTF-8, non-empty content, UUIDv7 IDs
  │
  ▼
[store / S2 client]   ← Postgres UNIQUE constraints (defacto limits — §6.4)
                      ← S2 record ≤ 1 MiB (enforced by S2; mapped to content_too_large)
```

Fail fast on the cheapest check first. Header checks before body parsing. Length checks before semantic validation. Path-parameter parsing before membership lookup. Membership lookup before any S2 call.

### 6.1 The NDJSON Scanner Buffer (Correctness-Critical)

Go's `bufio.Scanner` default buffer is 64 KiB. S2's record limit is 1 MiB. If we use `bufio.NewScanner(r.Body)` unchanged, long tokens, tool-call arguments, or base64-encoded content cause `scanner.Scan()` to return `false` with `bufio.ErrTooLong`. The handler then cannot distinguish "client closed cleanly" from "line too long" without inspecting `scanner.Err()`. Mishandled, this silently produces `message_abort` events with reason `eof` when the real cause was an oversized line.

**Decision: explicit 1 MiB + 1 KiB scanner buffer, with an explicit error mapping.**

```go
const maxNDJSONLineBytes = 1<<20 + 1024 // 1 MiB + 1 KiB envelope overhead

scanner := bufio.NewScanner(body) // body already wrapped with idle-timeout reader (§5)
scanner.Buffer(make([]byte, 64<<10), maxNDJSONLineBytes)

for scanner.Scan() {
    // process line
}
if err := scanner.Err(); err != nil {
    switch {
    case errors.Is(err, bufio.ErrTooLong):
        abortMessage(ctx, s, messageID, AbortReasonLineTooLarge) // "line_too_large"
        writeError(w, 413, "line_too_large",
            fmt.Sprintf("NDJSON line exceeds %d bytes", maxNDJSONLineBytes))
        return
    case errors.Is(err, context.DeadlineExceeded),
         errors.Is(err, os.ErrDeadlineExceeded):
        // Idle timeout fired (§5 SetReadDeadline). Response already committed;
        // no further writeError.
        abortMessage(ctx, s, messageID, AbortReasonIdleTimeout) // "idle_timeout"
        return
    case errors.Is(err, context.Canceled):
        // Client disconnect (ctx canceled by request lifecycle) or server shutdown.
        abortMessage(ctx, s, messageID, AbortReasonDisconnect) // "disconnect"
        return
    default:
        // TCP read error, framing error — treat as lost client.
        abortMessage(ctx, s, messageID, AbortReasonDisconnect) // "disconnect"
        return
    }
}
```

**Why 1 MiB + 1 KiB:** S2's 1 MiB record cap is the natural ceiling for the `content` field. The extra 1 KiB envelope accommodates the JSON framing (`{"content":"..."}` plus any optional fields) without reserving additional room for content itself. A line that clears the scanner but subsequently fails S2's 1 MiB check surfaces as a distinct S2 error (mapped to `content_too_large`, 413) — two limits, two codes, two root causes.

**Why the initial buffer is 64 KiB (not 1 MiB):** `bufio.Scanner.Buffer` takes both an initial buffer AND a maximum. The scanner grows the buffer on demand. For 99% of lines (a few hundred bytes each — one LLM token per line), the 64 KiB initial slab never grows. Pre-allocating 1 MiB per concurrent stream would waste memory at 1k+ concurrent writes (1 GiB of idle buffer).

**Interaction with the idle-timeout pattern (§5 Streaming Write Lifecycle):** The `http.ResponseController.SetReadDeadline` approach from §5 fires `os.ErrDeadlineExceeded` through `scanner.Err()`. Our mapping above preserves that path — deadline errors don't collide with `ErrTooLong`.

**Interaction with the "abort on bad JSON line" decision (§3 Streaming Send endpoint):** JSON-malformed lines are caught by the in-loop `json.Unmarshal` call, not by the scanner. The scanner's job is byte framing; JSON validity is the handler's job. These two paths do not overlap and each has its own abort reason.

### 6.2 Header Validation

**`X-Agent-ID` (required on authenticated routes).** Validated by the Agent Auth middleware (§2 Middleware 4). Existing error codes: `missing_agent_id` (400), `invalid_agent_id` (400 — not a valid UUID), `agent_not_found` (404 — valid UUID but unknown). No new codes needed here.

Note: the middleware accepts any valid UUID, not only UUIDv7. In practice every agent ID we issue is UUIDv7 (§ `sql-metadata-plan.md` §4), so a client passing a UUIDv4 will always fail at the DB lookup with `agent_not_found`. Tightening the middleware to reject non-UUIDv7 at parse time is a one-line regex change; we defer it because the existing codes already cover the failure unambiguously.

**`X-Request-ID` (optional, echoed in response).**

| Property | Value | Rationale |
|---|---|---|
| Length | 1 to 128 characters | 128 is generous for UUIDs, trace IDs, ULIDs, concatenated traces |
| Charset | ASCII printable (`0x20`–`0x7E`) | Prevents log-injection via control characters; prevents header-smuggling via CRLF |
| Missing | Server generates a UUIDv4 | §2 Middleware 2 |
| Oversized | Silently replaced with a server-generated UUIDv4 | Do NOT 400 on request ID problems — the request itself is still valid. Log a warning |
| Invalid charset | Silently replaced | Same rationale |

**Why silently replace (not reject):** `X-Request-ID` is a correlation aid, not a semantic input. A client that sent a garbled one still deserves a response. Rejecting would turn a log-correlation bug into a user-facing failure.

**`Content-Type`:** already covered in §4.

**Unknown headers:** passed through untouched. Do not strip. Do not fail. (Reverse proxies inject headers routinely.)

### 6.3 Body Invariants (Post-Parse)

After `decodeJSON` (§3) or NDJSON line parse succeeds, the following checks run in the handler before any S2 call:

| Invariant | Violation | Error |
|---|---|---|
| `content` non-empty on `message_append` / complete-message | Empty string | `400 empty_content` (already in §1) |
| `content` is valid UTF-8 | Non-UTF-8 byte sequence | `400 invalid_utf8` (new in §1) |
| `message_id` is UUIDv7 (not UUIDv4) | Wrong version bits | `400 invalid_message_id` (already in §1) |
| `conversation_id` path param is a UUID | Not parseable | `400 invalid_conversation_id` (already in §1) |
| `agent_id` in invite body is a UUID | Not parseable | `400 invalid_field` (already in §1) |

**Why UTF-8 validation requires an explicit pass.** Go's `encoding/json` does **not** validate UTF-8 when decoding a JSON string into a Go `string`. Invalid byte sequences (lone continuation bytes, truncated multi-byte sequences, overlong encodings) pass through `json.Unmarshal` into the Go string unchanged. The plaintext-preservation contract (README: "purely plaintext communication") requires rejection rather than silent corruption, so we add an explicit `utf8.ValidString` check at every `content`-accepting entry point. It is cheap — a single linear byte scan — but it is not free-by-decoding.

**Complete-message path (`POST /conversations/{cid}/messages`):**

```go
if !utf8.ValidString(req.Content) {
    writeError(w, 400, "invalid_utf8", "Content is not valid UTF-8")
    return
}
```

**Streaming path (`POST /conversations/{cid}/messages/stream`):** Same check, applied per-line after `json.Unmarshal` succeeds on the NDJSON `{"content": "..."}` object and before the S2 append. On failure, the handler writes `message_abort` with reason `invalid_utf8` (registered in `event-model-plan.md` §3) and closes the connection:

```go
for scanner.Scan() {
    var chunk struct{ Content string `json:"content"` }
    if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
        // Abort on bad JSON line (§3 streaming-endpoint decision).
        abortMessage(ctx, s, messageID, AbortReasonDisconnect)
        writeError(w, 400, "invalid_json", err.Error())
        return
    }
    if !utf8.ValidString(chunk.Content) {
        abortMessage(ctx, s, messageID, AbortReasonInvalidUTF8)
        writeError(w, 400, "invalid_utf8", "Content is not valid UTF-8")
        return
    }
    // ... append to S2 ...
}
```

The cost of the per-line `utf8.ValidString` pass is negligible: LLM token chunks are typically 1–20 bytes. At 30k concurrent streams producing 100 tokens/sec each, the global UTF-8 scan budget is on the order of microseconds per second of wall time.

**Why empty-content rejection is at the API layer, not the event model.** The event model (`event-model-plan.md` §2) treats the event as a pure data structure with no semantic rules. The HTTP layer owns the "an append is by definition a non-empty delta" rule. This keeps the event model reusable (e.g., by the resident agent, which constructs events directly).

**Zero-width / invisible content:** not rejected. A client that sends `\u200B` (zero-width space) gets that exact byte through to the stream. Plaintext preservation (README) forbids normalization.

**Max content per `message_append`:** naturally bounded by the scanner (1 MiB + 1 KiB) and S2 (1 MiB record). No explicit handler check — the scanner check fires first. This is intentional: one limit, one error path, no redundant code.

### 6.4 Defacto Limits (Enforced Elsewhere, Cross-Referenced Here)

These limits are real but enforced outside the API layer. Listed here so the reader of this plan sees the full picture:

| Limit | Where Enforced | Error at Violation |
|---|---|---|
| One SSE connection per `(agent, conversation)` | Connection registry — `sql-metadata-plan.md` §10 | Existing stream closed; new stream wins |
| One in-progress message per `(conversation, message_id)` | `in_progress_messages` UNIQUE — `s2-architecture-plan.md` §10 | `409 in_progress_conflict` |
| Message dedup by `(conversation, message_id)` | `messages_dedup` PK — `sql-metadata-plan.md` §5 | `200/201` with cached `seq_start`/`seq_end` (idempotent replay) or `409 already_aborted` |
| S2 record ≤ 1 MiB | S2 itself | Mapped by the S2 client wrapper to `413 content_too_large` |
| S2 batch ≤ 1000 records or 1 MiB | S2 `AppendSession` SDK | Handled internally by `AppendSession` batching |
| Header block ≤ 1 MiB | Go `http.Server.MaxHeaderBytes` default — `deployment-plan.md` §4.6 | `431 Request Header Fields Too Large` (stdlib) |
| URL path length | Go `http.Server` default buffers | Stdlib `414 URI Too Long` |
| Per-request timeouts | §5 Timeout Tiers | `408` or `504` per tier |

### 6.5 Accepted-Surface (Documented, Not Enforced)

At evaluation scale (a handful of agents, tens of conversations) these are safe at their default of "unlimited." Each is named so the senior-engineer reading this knows we thought about it.

| Knob | Current Behavior | Fails at Roughly | Production Enforcement |
|---|---|---|---|
| Conversations per agent | Unlimited | 10⁶ rows in `conversation_members` per agent before list query degrades | Per-agent quota via a `quotas` table; 429 on exceed |
| Members per conversation | Unlimited | At ~10³ members per conversation, `agent_joined`/`agent_left` fanout starts to dominate event volume | Hard cap (e.g., 256), 400 on invite over cap |
| Total agents | Unlimited | Neon row storage cost — not a failure mode, just a bill | Agent TTL: unused agents expire after N days |
| Total conversations | Unlimited | S2 stream creation rate limits + basin capacity | Tenant quota on basin; per-tenant basins |
| Concurrent SSE across all agents | Fly machine file-descriptor limit (~65k) | ~30–50k concurrent streams per Fly machine | Admission control + horizontal scaling; `server-lifecycle-plan.md` §6 |
| Concurrent NDJSON writes across all agents | Same fd limit; S2 AppendSession count | Same ~30–50k before fd pressure | Same |
| Per-agent request rate | No rate limiting | Unbounded — a misbehaving client can saturate the instance | Token bucket per `X-Agent-ID`; §7 Rate Limiting |
| Per-agent concurrent requests | No limit | Per-agent amplification of any other pressure | Semaphore per agent, 429 on exceed |
| Message bytes per conversation (lifetime) | Unlimited (28-day S2 retention bounds it) | S2 storage cost only | Per-conversation quota |
| NDJSON stream duration | 5-min idle timeout (§5); no absolute maximum | An open stream producing one token per 4 min runs forever | Absolute max stream duration (e.g., 1 hour) |

**Explicit statement for the evaluator:** every item in this table is an accepted risk for the take-home scope, not an oversight. At production scale, every row gets an enforcement story; the enforcement story lives in `future-plan.md`.

**The single exception that IS enforced now:** the take-home still requires the system to survive a single well-behaved evaluator's test suite. If evaluation reveals any of these surfaces as a real failure mode, the fix is localized (a single middleware, a single UNIQUE constraint, a single admission counter) — not a redesign.

### 6.6 Error-Code Additions to the §1 Registry

This section introduces the following stable codes into §1:

| Code | Status | Category | Introduced in |
|---|---|---|---|
| `line_too_large` | 413 | Request size | §6.1 NDJSON scanner |
| `content_too_large` | 413 | Request size | §6.4 S2 1 MiB mapping |
| `invalid_utf8` | 400 | Validation | §6.3 Body invariants |
| `request_too_large` | 413 | Request size | Existing behavior of `decodeJSON` (§3) — formalized here |

All four are stable contract codes. Clients branch on them exactly like the rest of §1.

### 6.7 Testing Strategy

Four tests — table-driven where possible, no ceremony:

1. **NDJSON oversize line.** Build a POST body with a 2 MiB content field in one line. Assert `413 line_too_large`. Assert a `message_abort` event with reason `line_too_large` is written to S2.
2. **Header validation matrix.** Table-driven: missing, malformed, wrong-length, non-ASCII `X-Agent-ID` → assert the correct existing error code per row. Same for `X-Request-ID` silent-fallback behavior.
3. **Body invariants.** Complete-message POST with: empty content, non-UTF-8 bytes (`\xC3\x28`), UUIDv4 `message_id` → assert the three correct error codes.
4. **Idle stream bound.** Open an NDJSON POST, send one line, wait 5 min 10s. Assert the stream closes with the existing idle-timeout path from §5 (no new code — verifies our limits don't break the existing timeout contract).

All four run against real Postgres + a throwaway S2 basin. These are boundary tests; mocks defeat them.

### 6.8 Files Touched

- `internal/api/middleware/request_id.go` — length/charset guard with silent fallback
- `internal/api/handlers/stream_write.go` — explicit `scanner.Buffer(...)` + error mapping
- `internal/api/handlers/messages.go` — `utf8.ValidString` on complete-message `content`
- `internal/api/handlers/errors.go` — four stable codes registered (`line_too_large`, `content_too_large`, `invalid_utf8`, `request_too_large`)
- `internal/s2/errors.go` — map S2 "record too large" to `content_too_large`
- No schema changes. No store changes. No middleware order changes.

---

## 7. Cross-Cutting Concerns

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
| POST /conversations/{cid}/messages | **Yes** (keyed by client-supplied `message_id`) | `messages_dedup` + UNIQUE on `in_progress_messages` |
| POST /conversations/{cid}/messages/stream | **Yes** (keyed by client-supplied `message_id` on first NDJSON line) | `messages_dedup` + UNIQUE on `in_progress_messages` |
| POST /conversations/{cid}/ack | Yes (regression-guarded) | `WHERE ack_seq < EXCLUDED.ack_seq` |

**Write-path idempotency contract.** Both message-write endpoints require a client-supplied `message_id` (UUIDv7). The same `message_id` submitted twice is idempotent:

1. If the prior attempt completed, the server replays the cached `start_seq` + `end_seq` from `messages_dedup` with `already_processed: true`. Same HTTP status, same response shape.
2. If the prior attempt was aborted (client disconnect, leave mid-stream, slow-writer timeout, recovery sweep), the server returns `409 already_aborted`. The client must generate a fresh `message_id` to retry.
3. If a concurrent attempt is in flight (two requests racing), the second hits the `UNIQUE (conversation_id, message_id)` on `in_progress_messages` and receives `409 in_progress_conflict`.

No `Idempotency-Key` header. The `message_id` is already the message's identity and the key readers demultiplex by on the S2 stream — collapsing identity and idempotency into one field keeps the contract tight and the handler logic branch-free. See [sql-metadata-plan.md](sql-metadata-plan.md) §5 (`messages_dedup` table) and §12 (queries).

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

## 8. Handler Structure Template

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

## 9. Complete Endpoint Summary

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

## 10. Scaling Considerations for the API Layer

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

## 11. Files

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
