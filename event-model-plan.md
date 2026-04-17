# AgentMail: Event Model (Go Types) — Complete Design Plan

## Executive Summary

This document specifies the complete Go type system for AgentMail's event model: the types, constructors, serializers, and deserializers that bridge the S2 wire format (headers + JSON body) with every Go consumer in the system. This is `internal/model/events.go` — a single file, ~250 lines, touched by every handler on both the write and read paths.

### Why This Matters

The event model is the spine of the system. Every write handler creates events. Every read handler consumes them. The Claude agent processes them. The history endpoint reconstructs messages from them. Without precise Go types, each consumer makes ad-hoc decisions: string literals for event types (typos silently break dispatch), untyped JSON parsing (runtime panics on wrong field names), inconsistent UUID handling (string vs `[16]byte`). These inconsistencies compound into bugs that only surface under interleaving — exactly the scenario the evaluators will test.

### Core Decisions

- **Event type constants:** Six `string` constants, typed as `EventType` for compile-time safety. No raw string literals anywhere in the codebase.
- **Payload structs:** Six structs with `uuid.UUID` fields and `json:"snake_case"` tags. Timestamps deliberately excluded from payloads (S2 arrival-mode timestamps are metadata, not event data).
- **Constructor functions:** Six functions returning `Event`, one per event type. These are the only sanctioned way to create `Event` values — prevents type/body mismatches.
- **`Event` struct:** Thin — `Type EventType` + `Body any`. Serialized to `s2.AppendRecord` inside the S2 store wrapper. Callers never touch S2 SDK types.
- **`SequencedEvent` struct:** Thin — carries `SeqNum`, `Timestamp`, `Type`, and `Data json.RawMessage`. Raw JSON body is NOT pre-parsed. SSE handler passes it through; history handler and agent parse on demand.
- **Deserialization:** Generic `ParsePayload[T]()` function. Zero-allocation on the SSE hot path (raw pass-through). Parse only when typed access is needed (history, agent).
- **SSE enrichment:** Timestamp injected into raw JSON at output time via `EnrichForSSE()`. Cheap parse-modify-marshal on tiny payloads (~50 bytes).
- **Abort reasons:** String constants, not an enum type. Known set of values, extensible without breaking consumers.

### Consumers & Producers — Complete Map

| Component | File | Produces Events | Consumes SequencedEvents |
|---|---|---|---|
| Complete message handler | `internal/api/messages.go` | `message_start`, `message_append`, `message_end` (one batch) | — |
| Streaming write handler | `internal/api/messages.go` | `message_start`, N × `message_append`, `message_end` or `message_abort` | — |
| Invite handler | `internal/api/conversations.go` | `agent_joined` | — |
| Leave handler | `internal/api/conversations.go` | `agent_left`, possibly `message_abort` | — |
| Recovery sweep | `cmd/server/main.go` | `message_abort` | — |
| SSE handler | `internal/api/sse.go` | — | All types (pass-through to SSE wire format) |
| History handler | `internal/api/history.go` | — | All message types (reconstruct complete messages) |
| Resident agent listener | `internal/agent/listen.go` | — | All types (dispatch to onEvent) |
| Resident agent onEvent | `internal/agent/state.go` | — | `message_start`, `message_append`, `message_end`, `message_abort` (assemble messages) |
| Resident agent respond | `internal/agent/respond.go` | `message_start`, N × `message_append`, `message_end` or `message_abort` | — |
| Resident agent seedHistory | `internal/agent/history.go` | — | All message types (reconstruct history) |

**Every write path produces `Event` values. Every read path consumes `SequencedEvent` values.** These are the two fundamental types in the event model.

---

## 1. Event Type Constants

### The Problem

Every handler, every switch statement, every S2 header comparison uses event type strings. Without constants, a typo like `"mesage_start"` silently produces broken events that no reader can dispatch. At billions of agents, a single typo in one code path can corrupt an entire conversation's stream with undispatchable records that persist in S2 for 28 days.

### Decision: Typed String Constants

```go
type EventType string

const (
    EventTypeMessageStart  EventType = "message_start"
    EventTypeMessageAppend EventType = "message_append"
    EventTypeMessageEnd    EventType = "message_end"
    EventTypeMessageAbort  EventType = "message_abort"
    EventTypeAgentJoined   EventType = "agent_joined"
    EventTypeAgentLeft     EventType = "agent_left"
)
```

### Why `type EventType string`, Not Bare `string`

A custom type means:
1. **Switch exhaustiveness:** `switch event.Type` over `EventType` values. An IDE/linter can warn about unhandled cases if we add a seventh event type later.
2. **Function signatures:** `func NewEvent(t EventType, body any)` prevents passing arbitrary strings. `NewEvent("typo", payload)` is a compile error if `"typo"` isn't assignable to `EventType` — wait, actually Go allows untyped string constants to be assigned to named string types. So `NewEvent("typo", payload)` compiles. The safety is limited to *typed* constants. But `NewEvent(EventTypeMessageStart, payload)` is self-documenting and greppable, which is the real value.
3. **Grep-ability:** `rg "EventTypeMessage"` finds every use. `rg '"message_start"'` misses uses through the constant. Single source of truth.

### Why Not `iota` Integer Enum

Integer enums (`const MessageStart = iota`) require a string-mapping table for S2 headers and JSON output. The event type IS a string on the wire. Making it a string in Go eliminates conversion overhead and debugging friction. When you inspect a `SequencedEvent` in a debugger, you see `Type: "message_start"`, not `Type: 0`.

### Forward Compatibility: Unknown Event Types

If S2 somehow contains a record with an unknown `type` header (schema evolution, manual injection, bug), the system must handle it gracefully:

- **SSE handler:** Pass through unchanged. The SSE client receives an event with an unrecognized `event:` field. Well-behaved SSE clients ignore unknown event types. This is a feature of SSE — forward-compatible by design.
- **History handler:** Skip unknown events during message reconstruction. They're not message events, so they don't contribute to message assembly. Log a warning for observability.
- **Agent onEvent:** Default case in switch — log and skip. No crash, no panic.

```go
func IsMessageEvent(t EventType) bool {
    switch t {
    case EventTypeMessageStart, EventTypeMessageAppend,
         EventTypeMessageEnd, EventTypeMessageAbort:
        return true
    }
    return false
}

func IsSystemEvent(t EventType) bool {
    switch t {
    case EventTypeAgentJoined, EventTypeAgentLeft:
        return true
    }
    return false
}
```

These helpers allow callers to filter without exhaustive switch statements when they only care about event categories.

---

## 2. Payload Structs

### The Six Payloads

```go
type MessageStartPayload struct {
    MessageID uuid.UUID `json:"message_id"`
    SenderID  uuid.UUID `json:"sender_id"`
}

type MessageAppendPayload struct {
    MessageID uuid.UUID `json:"message_id"`
    Content   string    `json:"content"`
}

type MessageEndPayload struct {
    MessageID uuid.UUID `json:"message_id"`
}

type MessageAbortPayload struct {
    MessageID uuid.UUID `json:"message_id"`
    Reason    string    `json:"reason"`
}

type AgentJoinedPayload struct {
    AgentID uuid.UUID `json:"agent_id"`
}

type AgentLeftPayload struct {
    AgentID uuid.UUID `json:"agent_id"`
}
```

### Design Decisions — Field by Field

**`uuid.UUID` not `string` for IDs:**

`google/uuid.UUID` is `[16]byte` — value type, comparable, hashable, zero-allocation copies. JSON marshaling produces `"01906e5b-3c4a-7f1e-8b9d-2a4c6e8f0123"` (standard hyphenated format). JSON unmarshaling accepts the same. Using `string` would mean:
- 36 bytes per ID (vs 16 bytes) — 2.25× memory overhead per field
- Heap allocation per ID (strings are pointer + length + data)
- No compile-time distinction between "any string" and "a valid UUID"
- Manual validation on every parse

At billions of events processed, the per-event memory savings compound. Every `message_append` event carries a `MessageID` — at 30–100 tokens/sec across thousands of conversations, `uuid.UUID` eliminates millions of string allocations per hour.

**`json:"snake_case"` tags:**

Consistent with the entire API surface (http-api-layer-plan.md §3: "snake_case JSON field names"). The S2 record bodies use the same casing. The SSE `data` field uses the same casing. One format everywhere.

**`Content` is `string`, not `[]byte`:**

JSON text is UTF-8. Go strings are UTF-8. `json.Unmarshal` into a `string` field performs UTF-8 validation. `[]byte` would skip validation but require base64 encoding in JSON (Go's default `json.Marshal` behavior for `[]byte`), which breaks the plain-JSON contract.

**`Reason` in `MessageAbortPayload` is `string`, not a typed enum:**

See Section 3 below. The abort reason is descriptive metadata for debugging, not a programmatic branch point for consumers.

### Why No Timestamp in Payloads

The S2 architecture plan (§2) explicitly chose **arrival-mode timestamping**: S2 stamps each record on receipt. The server does NOT set a client-side timestamp. Reasons (from the S2 plan):
- Simpler — one less field for the server to manage
- More accurate — no clock skew between the Go server and S2
- Honest — arrival time represents when S2 received the record, the only time that matters for ordering

Including a `timestamp` field in payload structs would mean either:
1. **Duplicating S2's timestamp** — redundant data, different values (server clock vs S2 clock), confusing for readers
2. **Setting it to the server's wall clock** — potentially inconsistent with S2's ordering, since the server's clock and S2's receipt timestamp may differ by milliseconds

Instead, the timestamp is carried by `SequencedEvent.Timestamp` (S2 metadata) and injected into SSE output at render time (Section 8). The payload structs remain pure domain data.

**The spec.md SSE example shows:**
```
data: {"message_id":"m1","sender_id":"a1","timestamp":"2026-04-14T10:00:00Z"}
```

That `timestamp` in the `data` field is NOT from the record body. It's the S2 record timestamp, injected by the SSE handler at output time. This is protocol translation — exactly what the server does.

### Why No `SeqNum` in Payloads

Same reasoning as timestamp. Sequence numbers are S2 metadata, not event data. They're carried by `SequencedEvent.SeqNum` and rendered as the SSE `id` field. Embedding them in the payload would duplicate data that's already in the record metadata.

### Edge Case: Zero-Value UUIDs

`uuid.Nil` (all zeros) is technically a valid UUID value. A zero-value `MessageStartPayload{}` has `MessageID: uuid.Nil` and `SenderID: uuid.Nil`. JSON: `{"message_id":"00000000-0000-0000-0000-000000000000","sender_id":"00000000-..."}`.

**Should constructors reject zero UUIDs?** Yes. A message with a nil message_id or sender_id is always a bug — it means the caller forgot to generate or pass the UUID. Constructor functions validate and return an error (or panic — see Section 4).

**Provenance of `MessageID`.** The event-model code does NOT generate `message_id`. For messages written by external agents it is supplied by the client on the request body (the complete-message POST includes it as a required JSON field; the streaming POST declares it on the mandatory first NDJSON line). For messages written by the in-process resident Claude agent it is generated once by the agent in `respond.go` via `uuid.Must(uuid.NewV7())` before the response begins, so the agent and the HTTP handler flow through the same `in_progress_messages` + `messages_dedup` gates. Either way, by the time a `MessageStartPayload` is constructed, `MessageID` is already chosen — the event model's job is to serialize it onto the stream, not to invent it. See [http-api-layer-plan.md](http-api-layer-plan.md) §3 (wire types) and [sql-metadata-plan.md](sql-metadata-plan.md) §5 (`messages_dedup` rationale).

### Edge Case: Empty Content in `MessageAppendPayload`

The spec says: "Empty content lines are allowed (LLMs sometimes emit empty tokens)." So `Content: ""` is valid for `message_append`. The constructor does NOT reject empty content. The payload struct accepts it. Consumers handle it (the agent's `strings.Builder.WriteString("")` is a no-op — correct behavior).

### Edge Case: Very Large Content

S2's maximum record size is 1 MiB. A single `message_append` with a 1 MiB content string would hit this limit. In practice, LLM tokens are 1–20 bytes. The size limits are owned by the HTTP layer — see [http-api-layer-plan.md](http-api-layer-plan.md) §6.1 (explicit 1 MiB + 1 KiB `scanner.Buffer(...)` cap producing `413 line_too_large`) and §6.4 (S2's 1 MiB rejection mapped to `413 content_too_large`). The event model itself does not enforce size limits — that's the transport layer's job. Constructors accept arbitrary-length content; serialization into an `s2.AppendRecord` at write time surfaces any S2 rejection to the HTTP handler.

### Edge Case: Unicode and Special Characters

JSON handles Unicode natively. `json.Marshal` escapes control characters and produces valid UTF-8. `json.Unmarshal` validates UTF-8 input. No normalization (NFC/NFD) is applied — what goes in comes out. This is correct: the spec says "purely plaintext communication," and plaintext preserves exact byte content.

---

## 3. Abort Reason Constants

### Known Values

```go
const (
    AbortReasonDisconnect    = "disconnect"       // client disconnected mid-stream
    AbortReasonServerCrash   = "server_crash"     // recovery sweep found unterminated message
    AbortReasonAgentLeft     = "agent_left"       // agent removed from conversation mid-write
    AbortReasonClaudeError   = "claude_error"     // Claude API failure during resident agent response
    AbortReasonIdleTimeout   = "idle_timeout"     // streaming write idle for >5 minutes
    AbortReasonMaxDuration   = "max_duration"     // streaming write exceeded 1 hour
    AbortReasonLineTooLarge  = "line_too_large"   // NDJSON line exceeded 1 MiB + 1 KiB (http-api-layer-plan.md §6.1)
    AbortReasonContentTooLarge = "content_too_large" // S2 rejected a record for exceeding 1 MiB (http-api-layer-plan.md §6.4)
    AbortReasonInvalidUTF8   = "invalid_utf8"     // content bytes were not valid UTF-8 (http-api-layer-plan.md §6.3)
)
```

### Why String Constants, Not a Typed Enum

The `reason` field is **descriptive metadata for debugging**. No consumer branches on the reason value to change behavior. Readers process `message_abort` identically regardless of reason — delete pending message state, mark as aborted in history. The reason is for observability and logs.

A typed enum (`type AbortReason string`) would add:
- No safety: the JSON field is still a string, and external producers could write any value
- Additional conversion boilerplate at every use site
- False confidence: the enum suggests completeness, but the set can grow (new abort reasons for new failure modes) without bumping a version

String constants give grep-ability without ceremony. The `MessageAbortPayload.Reason` field accepts any string. Known constants are documented above. Unknown reason strings in existing data are handled gracefully (display as-is).

---

## 4. Constructor Functions (Write Path)

### The Functions

```go
func NewMessageStartEvent(messageID, senderID uuid.UUID) Event {
    if messageID == uuid.Nil {
        panic("events: NewMessageStartEvent called with nil messageID")
    }
    if senderID == uuid.Nil {
        panic("events: NewMessageStartEvent called with nil senderID")
    }
    return Event{
        Type: EventTypeMessageStart,
        Body: MessageStartPayload{MessageID: messageID, SenderID: senderID},
    }
}

func NewMessageAppendEvent(messageID uuid.UUID, content string) Event {
    if messageID == uuid.Nil {
        panic("events: NewMessageAppendEvent called with nil messageID")
    }
    return Event{
        Type: EventTypeMessageAppend,
        Body: MessageAppendPayload{MessageID: messageID, Content: content},
    }
}

func NewMessageEndEvent(messageID uuid.UUID) Event {
    if messageID == uuid.Nil {
        panic("events: NewMessageEndEvent called with nil messageID")
    }
    return Event{
        Type: EventTypeMessageEnd,
        Body: MessageEndPayload{MessageID: messageID},
    }
}

func NewMessageAbortEvent(messageID uuid.UUID, reason string) Event {
    if messageID == uuid.Nil {
        panic("events: NewMessageAbortEvent called with nil messageID")
    }
    return Event{
        Type: EventTypeMessageAbort,
        Body: MessageAbortPayload{MessageID: messageID, Reason: reason},
    }
}

func NewAgentJoinedEvent(agentID uuid.UUID) Event {
    if agentID == uuid.Nil {
        panic("events: NewAgentJoinedEvent called with nil agentID")
    }
    return Event{
        Type: EventTypeAgentJoined,
        Body: AgentJoinedPayload{AgentID: agentID},
    }
}

func NewAgentLeftEvent(agentID uuid.UUID) Event {
    if agentID == uuid.Nil {
        panic("events: NewAgentLeftEvent called with nil agentID")
    }
    return Event{
        Type: EventTypeAgentLeft,
        Body: AgentLeftPayload{AgentID: agentID},
    }
}
```

### Why Panic on Nil UUIDs, Not Return Error

A nil UUID in a constructor is always a programmer error — the caller forgot to generate or pass the ID. It's never a runtime condition the caller can recover from. Panicking:
1. Fails immediately and loudly during development/testing
2. Produces a stack trace pointing to the exact call site
3. Avoids error-return boilerplate that callers would invariably ignore (`event, _ := NewMessageStartEvent(...)`)

This follows Go convention: `regexp.MustCompile` panics on invalid patterns, `template.Must` panics on parse errors. Construction of well-known types with programmer-controlled inputs should panic on invalid input.

**If this were user-controlled input** (e.g., parsing directly from an HTTP body into this struct), returning an error would be correct. But by the time a constructor is called, the HTTP layer has already validated the `message_id` — see §1 "Provenance of `MessageID`" — and resolved bad input into a `400 missing_message_id` / `400 invalid_message_id` before any event is constructed. At the constructor boundary the UUID is always either a validated client-supplied value (for external writes) or an agent-generated one (for the resident agent). A nil UUID reaching a constructor therefore means a server-side bug, and panic is the correct response.

### Why Constructors Return `Event`, Not `s2.AppendRecord`

The `Event` type is a domain type in the model package. The `s2.AppendRecord` type is an SDK type in the S2 client library. If constructors returned `s2.AppendRecord`:
- The model package would depend on the S2 SDK — incorrect dependency direction
- Unit tests of event creation would need to import the S2 SDK
- Switching from S2 to another backend would require changing every constructor

Instead, the S2 store wrapper (`internal/store/s2.go`) owns the `Event → s2.AppendRecord` conversion. The model package is dependency-free except for `google/uuid` and `encoding/json`.

### The `Event` Struct

```go
type Event struct {
    Type EventType
    Body any // must be one of the six payload types; json.Marshal'd for S2 record body
}
```

**Why `any` not an interface:**

We could define a sealed `EventPayload` interface:
```go
type EventPayload interface {
    eventType() EventType
}
func (p MessageStartPayload) eventType() EventType { return EventTypeMessageStart }
// ... etc
```

This would guarantee at compile time that `Event.Body` is always a valid payload type. But:
1. Go doesn't have sealed interfaces — any package can implement `eventType()`
2. The `eventType()` method duplicates information already in `Event.Type`
3. The constructor functions already guarantee consistency — you can't create a mismatched event through a constructor
4. The sealed interface adds 6 method implementations for zero practical safety gain beyond what constructors provide

The constructors ARE the type-safety layer. `Event.Body any` keeps the struct simple. Direct struct initialization (`Event{Type: "...", Body: ...}`) is possible but discouraged by convention — constructors are the sanctioned creation path.

### Complete Message Helper

The complete message send handler creates three events in one atomic batch. A helper makes this ergonomic:

```go
func NewCompleteMessageEvents(messageID, senderID uuid.UUID, content string) []Event {
    if content == "" {
        panic("events: NewCompleteMessageEvents called with empty content")
    }
    return []Event{
        NewMessageStartEvent(messageID, senderID),
        NewMessageAppendEvent(messageID, content),
        NewMessageEndEvent(messageID),
    }
}
```

**Why panic on empty content:** The spec says "For complete message POST: content must not be empty" (spec.md §1.3). The API handler validates this before calling the constructor. An empty content here is a programming error (handler forgot to validate).

This helper is used by the `SendMessage` handler. Note that `msgID` is `req.MessageID` — the client-supplied UUIDv7 from the request body, already validated by the HTTP layer and passed through the `messages_dedup` + `in_progress_messages` gates before this call (see [http-api-layer-plan.md](http-api-layer-plan.md) §5 "Complete Message Write Lifecycle"):
```go
msgID := req.MessageID  // client-supplied, already validated
events := model.NewCompleteMessageEvents(msgID, agentID, req.Content)
result, err := h.s2.AppendEvents(ctx, convID, events)
```

---

## 5. Serialization: Event → s2.AppendRecord

### Ownership

The S2 store wrapper (`internal/store/s2.go`) owns this conversion. The model package does not import the S2 SDK. The store's `AppendEvents` and `AppendSessionWrapper.Submit` methods accept `Event` values and produce `s2.AppendRecord` values internally.

### The Conversion

```go
func eventToRecord(e Event) (s2.AppendRecord, error) {
    body, err := json.Marshal(e.Body)
    if err != nil {
        return s2.AppendRecord{}, fmt.Errorf("marshal event body: %w", err)
    }
    return s2.AppendRecord{
        Headers: []s2.Header{s2.NewHeader("type", string(e.Type))},
        Body:    body,
    }, nil
}
```

**Called in `AppendEvents`:**

```go
func (s *s2Store) AppendEvents(ctx context.Context, convID uuid.UUID, events []Event) (AppendResult, error) {
    records := make([]s2.AppendRecord, len(events))
    for i, e := range events {
        rec, err := eventToRecord(e)
        if err != nil {
            return AppendResult{}, fmt.Errorf("event %d: %w", i, err)
        }
        records[i] = rec
    }
    // ... append to S2
}
```

**Called in `AppendSessionWrapper.Submit`:**

```go
func (a *AppendSessionWrapper) Submit(ctx context.Context, e Event) (s2.SubmitFuture, error) {
    rec, err := eventToRecord(e)
    if err != nil {
        return nil, fmt.Errorf("event to record: %w", err)
    }
    return a.session.Submit(&s2.AppendInput{Records: []s2.AppendRecord{rec}})
}
```

### Error Analysis

**Can `json.Marshal` fail?** For our payload structs: effectively no. `uuid.UUID` always marshals successfully. `string` always marshals successfully. The only failure mode is a `Body` that contains an unmarshallable type (e.g., a channel, a function). Since constructors only accept payload structs, this can't happen in normal use. The error path exists for safety — if someone bypasses constructors and sets `Body` to something weird, the error is caught cleanly instead of producing a panic in the S2 SDK.

### Wire Format Verification

For `NewMessageStartEvent(msgID, senderID)` where `msgID = "019565a3-7c4f-7b2e-8a1d-3f5e2b1c9d8a"` and `senderID = "01906e5b-3c4a-7f1e-8b9d-2a4c6e8f0123"`:

```
S2 Record:
  Headers: [("type", "message_start")]
  Body:    {"message_id":"019565a3-7c4f-7b2e-8a1d-3f5e2b1c9d8a","sender_id":"01906e5b-3c4a-7f1e-8b9d-2a4c6e8f0123"}
```

Matches the wire format specified in `s2-architecture-plan.md` §4. Verified.

---

## 6. Deserialization: s2.Record → SequencedEvent

### The `SequencedEvent` Struct

```go
type SequencedEvent struct {
    SeqNum    uint64          // S2-assigned, monotonically increasing
    Timestamp time.Time       // S2-assigned (arrival mode), converted from milliseconds
    Type      EventType       // from S2 record header "type"
    Data      json.RawMessage // raw JSON body, parsed lazily by consumers
}
```

**Refinement from the S2 plan:** `Timestamp` is `time.Time` instead of `uint64`. The S2 store converts during deserialization:

```go
ts := time.UnixMilli(int64(s2Record.Timestamp))
```

**Why `time.Time`:** Downstream code (SSE enrichment, history endpoint) needs formatted timestamps. `time.Time` provides `.Format(time.RFC3339)`, `.Before()`, `.After()`, timezone handling. Storing `uint64` milliseconds would require conversion at every use site — wasteful repetition.

**Why `Data json.RawMessage` (not pre-parsed):**

This is the critical design decision. Three options were evaluated:

| Option | Description | SSE Handler Impact | History/Agent Impact | Memory |
|---|---|---|---|---|
| **A: Raw JSON (chosen)** | `Data json.RawMessage` — parse on demand | Zero-cost pass-through | Parse when needed | Minimal — just raw bytes |
| B: Pre-parsed payload | `Payload any` (typed interface) | Must re-serialize to JSON for SSE `data:` field | Direct typed access | Higher — parsed struct + original bytes |
| C: Both raw + parsed | `Data json.RawMessage` + `Payload any`, lazy | Best of both | Best of both | Highest — both representations |

**Why Option A wins:**

The SSE handler is the **highest-throughput consumer**. At 30–100 tokens/sec per active conversation across thousands of conversations, the SSE handler processes tens of thousands of events per second. For each event, it:
1. Writes `id: {seqnum}\n`
2. Writes `event: {type}\n`
3. Writes `data: {json}\n\n`

Step 3 needs the raw JSON bytes. With Option A, `SequencedEvent.Data` is already the bytes — zero parsing, zero allocation, zero copy. The SSE handler just writes them to the response (with timestamp enrichment, see Section 8).

With Option B, the SSE handler would need to `json.Marshal(event.Payload)` — re-serializing what was just deserialized. At 50,000 events/sec, that's 50,000 unnecessary marshal operations. At ~500ns per marshal, that's 25ms/sec of pure waste.

The history handler and agent parse events less frequently (history is a polled endpoint, agent processes one message at a time). The parse cost there is negligible. Option A optimizes for the hot path.

### The Conversion (s2.Record → SequencedEvent)

This lives in the S2 store wrapper:

```go
func recordToSequencedEvent(rec *s2.Record) (SequencedEvent, error) {
    // Extract event type from headers
    eventType := ""
    for _, h := range rec.Headers {
        if h.Name == "type" {
            eventType = string(h.Value)
            break
        }
    }
    if eventType == "" {
        return SequencedEvent{}, fmt.Errorf("record seq %d: missing 'type' header", rec.SeqNum)
    }

    return SequencedEvent{
        SeqNum:    rec.SeqNum,
        Timestamp: time.UnixMilli(int64(rec.Timestamp)),
        Type:      EventType(eventType),
        Data:      json.RawMessage(rec.Body),
    }, nil
}
```

**Used in `ReadSessionWrapper.Event()`:**

```go
func (r *ReadSessionWrapper) Event() (SequencedEvent, error) {
    rec := r.session.Record()
    return recordToSequencedEvent(rec)
}
```

**Used in `ReadRange`:**

```go
func (s *s2Store) ReadRange(ctx context.Context, convID uuid.UUID, fromSeq uint64, limit int) ([]SequencedEvent, error) {
    // ... S2 read call ...
    events := make([]SequencedEvent, 0, len(records))
    for _, rec := range records {
        se, err := recordToSequencedEvent(&rec)
        if err != nil {
            log.Warn().Err(err).Uint64("seq", rec.SeqNum).Msg("skipping malformed record")
            continue // skip malformed records, don't fail the entire read
        }
        events = append(events, se)
    }
    return events, nil
}
```

### Error Handling: Malformed Records

**Missing `type` header:** The record is undispatchable. `recordToSequencedEvent` returns an error. In `ReadRange`, the record is skipped with a warning. In the SSE handler, the record could either be skipped (drop the event) or forwarded as a generic event. **Decision: skip with warning.** A record without a type header is a bug — either in the writer (failed to set the header) or in S2 (data corruption). Logging the warning enables investigation.

**Malformed JSON body:** Not caught at the `SequencedEvent` level — `Data` is raw bytes, no JSON validation. Caught when a consumer calls `ParsePayload[T]()` and `json.Unmarshal` fails. This is correct: the SSE handler doesn't need to validate JSON (it just passes bytes through), and the consumers that do parse handle their own errors.

**Empty body:** Valid for `message_end` (body is `{"message_id":"..."}`, which is never empty, but a zero-length body would be malformed JSON). Caught at parse time, same as malformed JSON.

---

## 7. Typed Payload Parsing (Read Path)

### The Generic Parser

```go
func ParsePayload[T any](se SequencedEvent) (T, error) {
    var p T
    if err := json.Unmarshal(se.Data, &p); err != nil {
        return p, fmt.Errorf("parse %s payload (seq %d): %w", se.Type, se.SeqNum, err)
    }
    return p, nil
}
```

**Usage in the history handler:**

```go
case EventTypeMessageStart:
    payload, err := model.ParsePayload[model.MessageStartPayload](event)
    if err != nil {
        log.Warn().Err(err).Msg("skipping malformed message_start")
        continue
    }
    // use payload.MessageID, payload.SenderID

case EventTypeMessageAppend:
    payload, err := model.ParsePayload[model.MessageAppendPayload](event)
    if err != nil {
        log.Warn().Err(err).Msg("skipping malformed message_append")
        continue
    }
    // use payload.Content
```

**Usage in the agent's `onEvent()`:**

```go
case model.EventTypeMessageStart:
    payload, err := model.ParsePayload[model.MessageStartPayload](event)
    if err != nil {
        return // skip malformed events
    }
    state.pending[payload.MessageID] = &pendingMessage{
        sender:     payload.SenderID,
        lastAppend: time.Now(),
    }
```

### Why Generic Function, Not Per-Type Methods

Alternative: methods on `SequencedEvent`:
```go
func (se SequencedEvent) AsMessageStart() (MessageStartPayload, error) { ... }
func (se SequencedEvent) AsMessageAppend() (MessageAppendPayload, error) { ... }
// ... 4 more
```

This adds 6 methods to `SequencedEvent`, each doing the same thing with a different type parameter. The generic function achieves the same result with one implementation. Less code, same safety, same clarity.

**Why not a `ParsePayload` that returns `any`:** Returning `any` requires a type assertion at the call site. The generic version returns the concrete type — no assertion needed, compile-time type safety.

### Performance: Parse Cost

`json.Unmarshal` on a ~50-byte payload: ~300–500ns. At the history handler's scale (50 messages × ~10 events = 500 parses per request), that's ~250µs. Negligible compared to S2 read latency (~40ms).

At the agent's scale (processing events from one conversation at a time), parse cost is invisible.

The only hot path is SSE, which doesn't parse — it uses raw `Data` bytes. Correct by design.

### Error Handling in Consumers

Malformed payloads are **skipped, not fatal**:
- **History handler:** Skip the event, log warning. A malformed `message_start` means one message is missing from history. Better than failing the entire endpoint.
- **Agent `onEvent()`:** Skip the event. A malformed event can't be processed. The agent continues with the next event.
- **SSE handler:** Doesn't parse — passes raw bytes. The client receives the malformed JSON as-is. Client-side handling is the client's responsibility.

### Edge Case: Extra JSON Fields (Forward Compatibility)

If a future version adds a field to a payload (e.g., `message_start` gains a `priority` field), `json.Unmarshal` into the current `MessageStartPayload` struct silently ignores the extra field. This is correct — the current code doesn't need the new field. Forward-compatible by default.

If we used `DisallowUnknownFields()` on the decoder, extra fields would cause parse failures. **Do NOT use `DisallowUnknownFields()` for event payload parsing.** That's for HTTP request validation (catch typos from clients). Event payloads are internal — unknown fields should be tolerated.

### Edge Case: Missing JSON Fields (Backward Compatibility)

If an old record is missing a field that the current struct expects (e.g., `message_abort` before `reason` was added), `json.Unmarshal` sets the Go field to its zero value:
- `uuid.UUID` → `uuid.Nil` (all zeros)
- `string` → `""`
- `int` → `0`

Consumers must handle zero values gracefully. For `MessageAbortPayload`, an empty `Reason` means "unknown abort reason" — display as `"unknown"` or just `"aborted"`. Not a crash, not an error.

---

## 8. SSE Output Enrichment

### The Problem

The SSE wire format (spec.md §1.4) includes timestamps in the `data` field:

```
id: 42
event: message_start
data: {"message_id":"m1","sender_id":"a1","timestamp":"2026-04-14T10:00:00Z"}
```

But `SequencedEvent.Data` contains only the S2 record body — no timestamp. The S2 timestamp lives in `SequencedEvent.Timestamp`. The SSE handler must inject the timestamp into the JSON before writing it.

### Decision: Parse-Modify-Marshal

```go
func EnrichForSSE(data json.RawMessage, timestamp time.Time) (json.RawMessage, error) {
    var m map[string]json.RawMessage
    if err := json.Unmarshal(data, &m); err != nil {
        return data, err // return original data on parse failure (best-effort)
    }

    tsJSON, err := json.Marshal(timestamp.UTC().Format(time.RFC3339))
    if err != nil {
        return data, err
    }
    m["timestamp"] = tsJSON

    return json.Marshal(m)
}
```

**Used in the SSE handler:**

```go
for readSession.Next() {
    event, err := readSession.Event()
    if err != nil {
        log.Warn().Err(err).Msg("skipping malformed S2 record")
        continue
    }

    enriched, err := model.EnrichForSSE(event.Data, event.Timestamp)
    if err != nil {
        enriched = event.Data // fallback to raw data without timestamp
    }

    fmt.Fprintf(w, "id: %d\n", event.SeqNum)
    fmt.Fprintf(w, "event: %s\n", event.Type)
    fmt.Fprintf(w, "data: %s\n\n", enriched)
    flusher.Flush()

    h.cursors.UpdateDeliveryCursor(agentID, convID, event.SeqNum)
}
```

### Why Parse-Modify-Marshal, Not Raw Byte Splice

Alternative: insert `"timestamp":"..."` by splicing bytes:

```go
func enrichForSSE(data json.RawMessage, ts time.Time) json.RawMessage {
    formatted := ts.UTC().Format(time.RFC3339)
    // Remove trailing '}', append timestamp field, re-add '}'
    return json.RawMessage(
        fmt.Sprintf(`%s,"timestamp":"%s"}`, data[:len(data)-1], formatted),
    )
}
```

Faster (no parse/marshal), but fragile:
- Assumes `data` has no trailing whitespace — `json.Marshal` never produces any, but what if someone hand-writes a test record?
- Assumes `data` is a JSON object (starts with `{`, ends with `}`) — true for all our payloads, but unchecked
- Produces double-timestamps if `data` already has a `"timestamp"` field (can't happen now, but dangerous if payloads evolve)

Parse-modify-marshal is ~1µs for a 50-byte payload. At 50,000 events/sec, that's 50ms/sec — acceptable. Correctness over micro-optimization.

### Performance Budget

| Step | Cost per event | At 50K events/sec |
|---|---|---|
| `json.Unmarshal` into `map[string]json.RawMessage` | ~400ns | 20ms/sec |
| Insert timestamp key | ~50ns | 2.5ms/sec |
| `json.Marshal` the map | ~400ns | 20ms/sec |
| **Total enrichment** | **~850ns** | **42.5ms/sec** |

At 50K events/sec (far beyond take-home scale), enrichment consumes ~4% of one CPU core. Acceptable.

**Optimization path (FUTURE.md):** At 1M+ events/sec, switch to the byte-splice approach with defensive checks (`bytes.HasSuffix(data, []byte("}"))` guard). Or pre-compute the timestamp JSON string and use `bytes.Join`. Not needed now.

### Edge Case: EnrichForSSE Failure

If `json.Unmarshal` fails (malformed JSON in the record body), `EnrichForSSE` returns the original data unchanged. The SSE handler sends the un-enriched data. The client receives the event without a timestamp — degraded but functional. Better than dropping the event entirely.

---

## 9. History Endpoint: Message Assembly

### The Problem

The history endpoint (`GET /conversations/{cid}/messages`) returns reconstructed complete messages. It reads raw events from S2 and assembles them into `HistoryMessage` structs (defined in http-api-layer-plan.md §3):

```go
type HistoryMessage struct {
    MessageID uuid.UUID `json:"message_id"`
    SenderID  uuid.UUID `json:"sender_id"`
    Content   string    `json:"content"`
    SeqStart  uint64    `json:"seq_start"`
    SeqEnd    uint64    `json:"seq_end"`
    Status    string    `json:"status"` // "complete", "in_progress", "aborted"
}
```

This assembly logic is also used by the resident agent's `seedHistory()` (claude-agent-plan.md §5).

### The `AssembleMessages` Function

```go
type pendingAssembly struct {
    messageID uuid.UUID
    senderID  uuid.UUID
    content   strings.Builder
    seqStart  uint64
}

func AssembleMessages(events []SequencedEvent) []HistoryMessage {
    pending := make(map[uuid.UUID]*pendingAssembly) // messageID → in-progress
    var messages []HistoryMessage

    for _, event := range events {
        switch event.Type {
        case EventTypeMessageStart:
            payload, err := ParsePayload[MessageStartPayload](event)
            if err != nil {
                continue // skip malformed
            }
            pending[payload.MessageID] = &pendingAssembly{
                messageID: payload.MessageID,
                senderID:  payload.SenderID,
                seqStart:  event.SeqNum,
            }

        case EventTypeMessageAppend:
            payload, err := ParsePayload[MessageAppendPayload](event)
            if err != nil {
                continue
            }
            p, ok := pending[payload.MessageID]
            if !ok {
                continue // orphaned append — start was missed (cursor skip)
            }
            p.content.WriteString(payload.Content)

        case EventTypeMessageEnd:
            payload, err := ParsePayload[MessageEndPayload](event)
            if err != nil {
                continue
            }
            p, ok := pending[payload.MessageID]
            if !ok {
                continue // orphaned end
            }
            delete(pending, payload.MessageID)
            messages = append(messages, HistoryMessage{
                MessageID: p.messageID,
                SenderID:  p.senderID,
                Content:   p.content.String(),
                SeqStart:  p.seqStart,
                SeqEnd:    event.SeqNum,
                Status:    "complete",
            })

        case EventTypeMessageAbort:
            payload, err := ParsePayload[MessageAbortPayload](event)
            if err != nil {
                continue
            }
            p, ok := pending[payload.MessageID]
            if !ok {
                continue
            }
            delete(pending, payload.MessageID)
            messages = append(messages, HistoryMessage{
                MessageID: p.messageID,
                SenderID:  p.senderID,
                Content:   p.content.String(),
                SeqStart:  p.seqStart,
                SeqEnd:    event.SeqNum,
                Status:    "aborted",
            })

        case EventTypeAgentJoined, EventTypeAgentLeft:
            // System events don't contribute to message assembly. Skip.
        }
    }

    // Remaining pending entries are in-progress messages
    for _, p := range pending {
        messages = append(messages, HistoryMessage{
            MessageID: p.messageID,
            SenderID:  p.senderID,
            Content:   p.content.String(),
            SeqStart:  p.seqStart,
            SeqEnd:    0, // no end sequence — still in progress
            Status:    "in_progress",
        })
    }

    return messages
}
```

### Why This Lives in the Model Package

`AssembleMessages` is used by both the history handler (`internal/api/history.go`) and the resident agent's `seedHistory` (`internal/agent/history.go`). Placing it in the model package avoids duplication and keeps the logic in one place. Both consumers import `model.AssembleMessages(events)`.

### Edge Cases in Assembly

**Interleaved messages from concurrent writers:**
Two agents streaming simultaneously produce:
```
seq 100: message_start {m1, agentA}
seq 101: message_append {m1, "Hello"}
seq 102: message_start {m2, agentB}    ← interleaved
seq 103: message_append {m1, " world"}
seq 104: message_append {m2, "Hey"}
seq 105: message_end {m1}
seq 106: message_end {m2}
```

`AssembleMessages` handles this correctly because it demultiplexes by `message_id`. The `pending` map tracks both m1 and m2 independently. Result:
- m1: "Hello world", complete, seqStart=100, seqEnd=105
- m2: "Hey", complete, seqStart=102, seqEnd=106

**Orphaned appends at the start of a range:**
If the read range starts mid-message (e.g., `from=103` in the example above), the first events are `message_append` for m1 with no preceding `message_start`. The `pending[m1]` lookup fails → event is skipped. m1 is silently dropped. This is correct — a partial message at the boundary of a read range cannot be reconstructed.

**Duplicate events from S2 retries:**
S2 `AppendRetryPolicyAll` can produce duplicate records (s2-architecture-plan.md §12). Possible duplicates:
- Duplicate `message_start`: `pending[msgID]` is overwritten. The second start replaces the first. Content accumulated before the duplicate is lost, but in practice, a duplicate start arrives immediately after the first (retry latency is milliseconds), so no content has accumulated. Harmless.
- Duplicate `message_append`: Content is appended twice. The reader sees repeated tokens. Cosmetic issue, not a correctness problem.
- Duplicate `message_end`: First `message_end` completes the message and removes it from `pending`. Second `message_end` hits the `!ok` branch → skipped. Harmless.

**Why event-level dedup remains unnecessary even after the handler-level idempotency work:** The HTTP-level idempotency gate (`messages_dedup` + UNIQUE on `in_progress_messages`, see [sql-metadata-plan.md](sql-metadata-plan.md) §5) closes the outer loop — a whole-request client retry never produces a second logical message. The remaining duplicate source is the S2 SDK's own retry-on-jitter, whose duplicates carry the *same* `message_id` and the *same* content bytes. Readers' natural `message_id` demultiplexing (described above — the three "Duplicate" bullets) handles those transparently. Adding event-level `(message_id, seq_num)` tracking would be the belt on top of the belt-and-suspenders we already have; it only becomes interesting if a future design allows multiple *different* logical messages to legitimately share a `message_id`, which this system explicitly forbids. Kept as a pure enhancement note: not built, not needed.

---

## 10. Scalability Analysis

### Memory Per Event

| Type | Field Sizes | JSON Size | Go Struct Size |
|---|---|---|---|
| `MessageStartPayload` | 2 × `uuid.UUID` (16B each) | ~80 bytes | 32 bytes |
| `MessageAppendPayload` | `uuid.UUID` (16B) + `string` (variable) | ~50 + content len | 32 bytes + content |
| `MessageEndPayload` | `uuid.UUID` (16B) | ~45 bytes | 16 bytes |
| `MessageAbortPayload` | `uuid.UUID` (16B) + `string` (~20B) | ~65 bytes | 40 bytes |
| `AgentJoinedPayload` | `uuid.UUID` (16B) | ~45 bytes | 16 bytes |
| `AgentLeftPayload` | `uuid.UUID` (16B) | ~45 bytes | 16 bytes |

`SequencedEvent` overhead: `uint64` (8B) + `time.Time` (24B) + `EventType string` (~20B) + `json.RawMessage` header (24B, slice header) + body bytes = ~76 bytes + body. At 50 bytes average body, ~126 bytes per `SequencedEvent`.

### Allocation Pressure

**Write path:** Each constructor allocates one `Event` struct (stack-allocated if it doesn't escape) and one payload struct (same). `json.Marshal` in the S2 store allocates a `[]byte` for the body. Total: 2–3 allocations per event. At 100 tokens/sec: ~300 allocs/sec. GC-invisible.

**Read path (SSE):** `recordToSequencedEvent` copies `rec.Body` into `json.RawMessage` (one allocation). `EnrichForSSE` does one unmarshal + one marshal (2–3 allocations). Total: ~4 allocations per event. At 50K events/sec: ~200K allocs/sec. Go's GC handles this without pause spikes (allocs are short-lived, generation-0 collected).

**Read path (history):** `AssembleMessages` parses each event (one alloc per parse), accumulates content in `strings.Builder` (amortized allocations). For a page of 50 messages × ~10 events = 500 parses: ~500 allocations per request. Sub-millisecond GC impact.

### JSON Parse Cost at Scale

| Scale | Events/sec | Parse cost/sec | CPU fraction |
|---|---|---|---|
| Take-home (100 agents) | ~1,000 | ~500µs | 0.05% |
| 1K agents | ~10,000 | ~5ms | 0.5% |
| 100K agents | ~1,000,000 | ~500ms | 50% of one core |
| 1M agents | ~10,000,000 | ~5s | **Bottleneck** |

At 1M+ agents with SSE enrichment parsing, the JSON parse-modify-marshal becomes a CPU bottleneck. Mitigation paths:
1. Switch to byte-splice enrichment (eliminates parse/marshal)
2. Use `jsoniter` or `sonic` (2–5× faster JSON processing)
3. Pre-compute SSE output bytes during S2 read (amortize across multiple readers)
4. Cache enriched output per record (if multiple SSE readers tail the same position)

**For the take-home:** Standard `encoding/json` handles the scale with room to spare. Document optimization paths in FUTURE.md.

---

## 11. Race Conditions

### Are There Any?

The event model types are **value types** (structs) or **immutable after creation** (`json.RawMessage` is a `[]byte` that's never mutated after construction). No shared mutable state exists in the model package.

**`Event` values** are created by constructors and consumed by the S2 store. They're passed by value to `AppendEvents` and `Submit`. No goroutine shares an `Event` value.

**`SequencedEvent` values** are created by `recordToSequencedEvent` and consumed by handlers. The SSE handler reads events sequentially (one goroutine per SSE connection). The history handler processes events sequentially within a single request. The agent's `onEvent` is called sequentially by the listener goroutine.

**`AssembleMessages`** is a pure function — takes a slice of events, returns messages. No shared state. The `pending` map is local to the function invocation.

**Verdict: No race conditions in the event model itself.** All concurrency concerns are in the layers above (handlers, agent, store) — already designed in their respective plans.

### The Only Cross-Goroutine Concern

The `EnrichForSSE` function is called from SSE handler goroutines (one per connection). Each call gets its own copy of `event.Data` (the `json.RawMessage` was created fresh by `recordToSequencedEvent`). No shared bytes. No race.

---

## 12. Integration Points — Code Updates Required

The event model types replace ad-hoc string literals and anonymous structs throughout the codebase. These are the changes needed in each plan document's code:

### spec.md §1.3 (Streaming Write Handler)

**Before (current code):**
```go
session.Submit(ctx, messageStartEvent(msgID, agentID))
session.Submit(ctx, messageAppendEvent(msgID, chunk.Content))
session.Submit(ctx, messageEndEvent(msgID))
h.s2.AppendEvents(ctx, convID, []Event{messageAbortEvent(msgID, "disconnect")})
```

**After (with event model):**
```go
session.Submit(ctx, model.NewMessageStartEvent(msgID, agentID))
session.Submit(ctx, model.NewMessageAppendEvent(msgID, chunk.Content))
session.Submit(ctx, model.NewMessageEndEvent(msgID))
h.s2.AppendEvents(ctx, convID, []model.Event{model.NewMessageAbortEvent(msgID, model.AbortReasonDisconnect)})
```

### claude-agent-plan.md §5 (onEvent)

**Before (current code):**
```go
switch event.Type {
case "message_start":
    state.pending[event.MessageID] = &pendingMessage{
        sender: event.Sender, // ← these fields don't exist on SequencedEvent
    }
case "message_append":
    p.content.WriteString(event.Token) // ← event.Token doesn't exist
```

**After (with event model):**
```go
switch event.Type {
case model.EventTypeMessageStart:
    payload, err := model.ParsePayload[model.MessageStartPayload](event)
    if err != nil { return }
    state.pending[payload.MessageID] = &pendingMessage{
        sender:     payload.SenderID,
        lastAppend: time.Now(),
    }
case model.EventTypeMessageAppend:
    payload, err := model.ParsePayload[model.MessageAppendPayload](event)
    if err != nil { return }
    p, ok := state.pending[payload.MessageID]
    if !ok { return }
    p.content.WriteString(payload.Content)
```

**Note:** The claude-agent-plan's `pending` map changes from `map[string]*pendingMessage` to `map[uuid.UUID]*pendingMessage`. The `sender` field in `pendingMessage` changes from `string` to `uuid.UUID`.

### claude-agent-plan.md §6 (respond — Token Pipeline)

**Before (current code):**
```go
session.Submit(ctx, Event{Type: "message_start", Body: MessageStartPayload{...}})
session.Submit(ctx, Event{Type: "message_append", Body: MessageAppendPayload{...}})
```

**After (with event model):**
```go
session.Submit(ctx, model.NewMessageStartEvent(msgID, a.id))
session.Submit(ctx, model.NewMessageAppendEvent(msgID, textDelta.Text))
session.Submit(ctx, model.NewMessageEndEvent(msgID))
```

### s2-architecture-plan.md §5 (Handler Pseudocode)

The handler pseudocode references `messageStartRecord()`, `messageAppendRecord()`, `messageEndRecord()`, `messageAbortRecord()`. These are replaced by the model constructors. The S2 store's `Submit` and `AppendEvents` accept `model.Event`, not raw records.

### SSE Handler (s2-architecture-plan.md §6)

**Before (current code):**
```go
event := recordToSSEEvent(rec)
fmt.Fprintf(w, "data: %s\n\n", event.Data)
```

**After (with event model):**
```go
event, err := readSession.Event() // returns model.SequencedEvent
enriched, _ := model.EnrichForSSE(event.Data, event.Timestamp)
fmt.Fprintf(w, "id: %d\n", event.SeqNum)
fmt.Fprintf(w, "event: %s\n", event.Type)
fmt.Fprintf(w, "data: %s\n\n", enriched)
```

---

## 13. Complete Code — internal/model/events.go

```go
package model

import (
    "encoding/json"
    "fmt"
    "strings"
    "time"

    "github.com/google/uuid"
)

// --- Event Type Constants ---

type EventType string

const (
    EventTypeMessageStart  EventType = "message_start"
    EventTypeMessageAppend EventType = "message_append"
    EventTypeMessageEnd    EventType = "message_end"
    EventTypeMessageAbort  EventType = "message_abort"
    EventTypeAgentJoined   EventType = "agent_joined"
    EventTypeAgentLeft     EventType = "agent_left"
)

func IsMessageEvent(t EventType) bool {
    switch t {
    case EventTypeMessageStart, EventTypeMessageAppend,
        EventTypeMessageEnd, EventTypeMessageAbort:
        return true
    }
    return false
}

func IsSystemEvent(t EventType) bool {
    switch t {
    case EventTypeAgentJoined, EventTypeAgentLeft:
        return true
    }
    return false
}

// --- Abort Reason Constants ---

const (
    AbortReasonDisconnect      = "disconnect"
    AbortReasonServerCrash     = "server_crash"
    AbortReasonAgentLeft       = "agent_left"
    AbortReasonClaudeError     = "claude_error"
    AbortReasonIdleTimeout     = "idle_timeout"
    AbortReasonMaxDuration     = "max_duration"
    AbortReasonLineTooLarge    = "line_too_large"
    AbortReasonContentTooLarge = "content_too_large"
    AbortReasonInvalidUTF8     = "invalid_utf8"
)

// --- Payload Structs ---

type MessageStartPayload struct {
    MessageID uuid.UUID `json:"message_id"`
    SenderID  uuid.UUID `json:"sender_id"`
}

type MessageAppendPayload struct {
    MessageID uuid.UUID `json:"message_id"`
    Content   string    `json:"content"`
}

type MessageEndPayload struct {
    MessageID uuid.UUID `json:"message_id"`
}

type MessageAbortPayload struct {
    MessageID uuid.UUID `json:"message_id"`
    Reason    string    `json:"reason"`
}

type AgentJoinedPayload struct {
    AgentID uuid.UUID `json:"agent_id"`
}

type AgentLeftPayload struct {
    AgentID uuid.UUID `json:"agent_id"`
}

// --- Event (Write Path) ---

type Event struct {
    Type EventType
    Body any
}

// --- Constructor Functions ---

func NewMessageStartEvent(messageID, senderID uuid.UUID) Event {
    if messageID == uuid.Nil {
        panic("events: NewMessageStartEvent called with nil messageID")
    }
    if senderID == uuid.Nil {
        panic("events: NewMessageStartEvent called with nil senderID")
    }
    return Event{
        Type: EventTypeMessageStart,
        Body: MessageStartPayload{MessageID: messageID, SenderID: senderID},
    }
}

func NewMessageAppendEvent(messageID uuid.UUID, content string) Event {
    if messageID == uuid.Nil {
        panic("events: NewMessageAppendEvent called with nil messageID")
    }
    return Event{
        Type: EventTypeMessageAppend,
        Body: MessageAppendPayload{MessageID: messageID, Content: content},
    }
}

func NewMessageEndEvent(messageID uuid.UUID) Event {
    if messageID == uuid.Nil {
        panic("events: NewMessageEndEvent called with nil messageID")
    }
    return Event{
        Type: EventTypeMessageEnd,
        Body: MessageEndPayload{MessageID: messageID},
    }
}

func NewMessageAbortEvent(messageID uuid.UUID, reason string) Event {
    if messageID == uuid.Nil {
        panic("events: NewMessageAbortEvent called with nil messageID")
    }
    return Event{
        Type: EventTypeMessageAbort,
        Body: MessageAbortPayload{MessageID: messageID, Reason: reason},
    }
}

func NewAgentJoinedEvent(agentID uuid.UUID) Event {
    if agentID == uuid.Nil {
        panic("events: NewAgentJoinedEvent called with nil agentID")
    }
    return Event{
        Type: EventTypeAgentJoined,
        Body: AgentJoinedPayload{AgentID: agentID},
    }
}

func NewAgentLeftEvent(agentID uuid.UUID) Event {
    if agentID == uuid.Nil {
        panic("events: NewAgentLeftEvent called with nil agentID")
    }
    return Event{
        Type: EventTypeAgentLeft,
        Body: AgentLeftPayload{AgentID: agentID},
    }
}

func NewCompleteMessageEvents(messageID, senderID uuid.UUID, content string) []Event {
    if content == "" {
        panic("events: NewCompleteMessageEvents called with empty content")
    }
    return []Event{
        NewMessageStartEvent(messageID, senderID),
        NewMessageAppendEvent(messageID, content),
        NewMessageEndEvent(messageID),
    }
}

// --- SequencedEvent (Read Path) ---

type SequencedEvent struct {
    SeqNum    uint64
    Timestamp time.Time
    Type      EventType
    Data      json.RawMessage
}

// --- Typed Payload Parsing ---

func ParsePayload[T any](se SequencedEvent) (T, error) {
    var p T
    if err := json.Unmarshal(se.Data, &p); err != nil {
        return p, fmt.Errorf("parse %s payload (seq %d): %w", se.Type, se.SeqNum, err)
    }
    return p, nil
}

// --- SSE Output Enrichment ---

func EnrichForSSE(data json.RawMessage, timestamp time.Time) (json.RawMessage, error) {
    var m map[string]json.RawMessage
    if err := json.Unmarshal(data, &m); err != nil {
        return data, err
    }
    tsJSON, err := json.Marshal(timestamp.UTC().Format(time.RFC3339))
    if err != nil {
        return data, err
    }
    m["timestamp"] = tsJSON
    return json.Marshal(m)
}

// --- Message Assembly (History Endpoint + Agent History Seeding) ---

type HistoryMessage struct {
    MessageID uuid.UUID `json:"message_id"`
    SenderID  uuid.UUID `json:"sender_id"`
    Content   string    `json:"content"`
    SeqStart  uint64    `json:"seq_start"`
    SeqEnd    uint64    `json:"seq_end"`
    Status    string    `json:"status"`
}

type pendingAssembly struct {
    messageID uuid.UUID
    senderID  uuid.UUID
    content   strings.Builder
    seqStart  uint64
}

func AssembleMessages(events []SequencedEvent) []HistoryMessage {
    pending := make(map[uuid.UUID]*pendingAssembly)
    var messages []HistoryMessage

    for _, event := range events {
        switch event.Type {
        case EventTypeMessageStart:
            payload, err := ParsePayload[MessageStartPayload](event)
            if err != nil {
                continue
            }
            pending[payload.MessageID] = &pendingAssembly{
                messageID: payload.MessageID,
                senderID:  payload.SenderID,
                seqStart:  event.SeqNum,
            }

        case EventTypeMessageAppend:
            payload, err := ParsePayload[MessageAppendPayload](event)
            if err != nil {
                continue
            }
            p, ok := pending[payload.MessageID]
            if !ok {
                continue
            }
            p.content.WriteString(payload.Content)

        case EventTypeMessageEnd:
            payload, err := ParsePayload[MessageEndPayload](event)
            if err != nil {
                continue
            }
            p, ok := pending[payload.MessageID]
            if !ok {
                continue
            }
            delete(pending, payload.MessageID)
            messages = append(messages, HistoryMessage{
                MessageID: p.messageID,
                SenderID:  p.senderID,
                Content:   p.content.String(),
                SeqStart:  p.seqStart,
                SeqEnd:    event.SeqNum,
                Status:    "complete",
            })

        case EventTypeMessageAbort:
            payload, err := ParsePayload[MessageAbortPayload](event)
            if err != nil {
                continue
            }
            p, ok := pending[payload.MessageID]
            if !ok {
                continue
            }
            delete(pending, payload.MessageID)
            messages = append(messages, HistoryMessage{
                MessageID: p.messageID,
                SenderID:  p.senderID,
                Content:   p.content.String(),
                SeqStart:  p.seqStart,
                SeqEnd:    event.SeqNum,
                Status:    "aborted",
            })
        }
    }

    for _, p := range pending {
        messages = append(messages, HistoryMessage{
            MessageID: p.messageID,
            SenderID:  p.senderID,
            Content:   p.content.String(),
            SeqStart:  p.seqStart,
            SeqEnd:    0,
            Status:    "in_progress",
        })
    }

    return messages
}
```

---

## 14. Dependency Graph

```text
internal/model/events.go
    imports: encoding/json, fmt, strings, time, github.com/google/uuid
    imported by:
        internal/store/s2.go        (Event → s2.AppendRecord, s2.Record → SequencedEvent)
        internal/api/messages.go    (creates Event values via constructors)
        internal/api/conversations.go (creates agent_joined/agent_left events)
        internal/api/sse.go         (consumes SequencedEvent, calls EnrichForSSE)
        internal/api/history.go     (calls AssembleMessages, uses HistoryMessage)
        internal/agent/listen.go    (consumes SequencedEvent in onEvent)
        internal/agent/state.go     (calls ParsePayload in onEvent dispatch)
        internal/agent/respond.go   (creates Event values via constructors)
        internal/agent/history.go   (calls AssembleMessages for history seeding)
        cmd/server/main.go          (creates message_abort events in recovery sweep)
```

No circular dependencies. The model package depends only on standard library + `google/uuid`. Every other package imports model — clean DAG.

---

## 15. Files

```
internal/model/
├── events.go       # Everything above: types, constants, constructors,
│                   # ParsePayload, EnrichForSSE, AssembleMessages
└── events_test.go  # Table-driven tests for serialization round-trip,
                    # constructor panics, enrichment, assembly edge cases
```

**Why one file, not split:** The entire event model is ~250 lines of Go. Splitting into `types.go`, `constructors.go`, `parsing.go`, `assembly.go` would create 4 files of ~60 lines each — more navigation overhead than value. One file, one `grep`, one mental model.

**Test coverage priorities:**
1. Serialization round-trip: `NewMessageStartEvent → eventToRecord → recordToSequencedEvent → ParsePayload[MessageStartPayload]` — verify data survives the wire format
2. Constructor panics: nil UUIDs, empty content in `NewCompleteMessageEvents`
3. `EnrichForSSE`: timestamp injection, malformed JSON fallback
4. `AssembleMessages`: interleaved messages, orphaned appends, duplicates, in-progress messages, aborted messages, empty input

---

## 16. Open Questions (Decisions Needed)

### Q1: `HistoryMessage` — Model Package or API Package?

`HistoryMessage` is currently defined in http-api-layer-plan.md §3 as an API response type. But `AssembleMessages` returns it, and the agent's `seedHistory` uses it too. Two options:

**Option A (chosen above):** `HistoryMessage` lives in model package. Both API and agent import it.
- Pro: Single definition, no duplication
- Con: API response type lives outside the API package (unusual in Go projects)

**Option B:** `HistoryMessage` lives in the API package. The agent defines its own `Message` type (already exists in claude-agent-plan.md). `AssembleMessages` returns a model-level type that both convert from.
- Pro: Clean package boundaries
- Con: Extra conversion step, duplication of fields

**Recommendation:** Option A. The `HistoryMessage` struct is a domain type (represents a reconstructed message), not a transport type. The API package renders it to JSON; the agent converts it to its `Message` type (which only needs `Sender` and `Content`). One definition in model, consumers transform as needed.

### Q2: Should `EnrichForSSE` Handle Timestamp-Only-On-Start Events?

Currently, `EnrichForSSE` adds a timestamp to every event type. But do `message_append` and `message_end` need timestamps in SSE output?

**Arguments for (current design):** Every event gets a timestamp. Consistent. The reader always knows when each event was written. Useful for debugging latency (measure time between `message_start` and `message_end`).

**Arguments against:** Timestamps on `message_append` are noise — at 30 tokens/sec, they add ~30 bytes × 30 events/sec = 900 bytes/sec per connection of timestamp data that clients don't need. The S2 sequence number already provides ordering.

**Recommendation:** Include timestamps on all events. The bandwidth cost is negligible (900 bytes/sec at LLM rates). The debugging value is high (latency measurement, ordering verification). If bandwidth becomes a concern at scale, make it configurable via a query parameter (`?timestamps=false`).

### Q3: Should `Event.Body` Be `any` or `EventPayload` Interface?

Discussed in Section 4. **Decision: `any`.** Constructors provide the safety layer. A sealed interface adds ceremony without practical benefit.

---

## 17. Changes to Existing Documents

If this plan is accepted, the following documents need minor updates to reference the model types instead of ad-hoc strings/structs:

| Document | Section | Change |
|---|---|---|
| spec.md §1.3 | Handler pseudocode | Replace `messageStartEvent()` → `model.NewMessageStartEvent()` |
| spec.md §1.4 | SSE handler pseudocode | Add `EnrichForSSE` call |
| s2-architecture-plan.md §11 | Supporting Types | Move `Event`, `SequencedEvent` to model package; keep `AppendResult`, wrappers in store |
| claude-agent-plan.md §5 | `onEvent()` | Replace `event.MessageID` → `ParsePayload[T]()` pattern |
| claude-agent-plan.md §5 | `pendingMessage` | Change `sender string` → `sender uuid.UUID` |
| claude-agent-plan.md §6 | `respond()` | Replace `Event{Type: "...", Body: ...}` → `model.NewXxxEvent()` |
| http-api-layer-plan.md §3 | `HistoryMessage` | Reference model package definition, remove duplicate |

These are documentation-only changes — the semantic meaning is identical. The code just uses proper types instead of raw strings.
