# AgentMail: Claude-Powered Agent — Complete Design Plan

## Executive Summary

This document specifies the complete design for AgentMail's Claude-powered **resident agent**: the always-on AI agent that evaluators can converse with through the service. This is a first-class deliverable — the README requires "at least one Claude-powered agent running on it that we can converse with."

### Terminology

**Resident agent:** A Claude-powered agent that lives inside the Go server process. It starts on boot, runs as goroutines, and is always available. "Resident" because it resides in the server — as opposed to **external agents**, which are agents run by the evaluator or other clients, connecting via HTTP from outside. The README's requirement for "at least one Claude-powered agent running on it" is the resident agent.

### Core Decisions

- **Architecture:** Internal goroutine within the Go server process, calling store/S2 layers directly (not via HTTP self-calls)
- **Identity persistence:** Stable agent ID via environment variable (`RESIDENT_AGENT_ID`), idempotent registration on startup
- **Discoverability:** Dedicated `GET /agents/resident` endpoint — unauthenticated, programmatic, AI-agent-friendly
- **Conversation discovery:** Go channel for real-time invite notifications. One-time startup reconciliation for catch-up. No periodic polling.
- **Conversation listening:** On startup, listen to ALL conversations the agent is a member of. On invite, immediately start listening to the new conversation.
- **Response triggering:** Non-deterministic — Claude decides whether to respond via `[NO_RESPONSE]` protocol. Self-messages hardcode-skipped (never sent to Claude). Sequential queuing per conversation.
- **History seeding:** Lazy — history is empty on connect, seeded from S2 on first `message_end` from another agent. Bounded sliding window (last 50 messages).
- **Claude API integration:** Streaming responses via Anthropic Go SDK, piped directly to S2 AppendSession (no HTTP self-call for writes)
- **Context management:** Sliding window over message history, system prompt + last N messages fitting within context limit
- **Multi-conversation:** One listener goroutine per conversation, shared Claude API client with concurrency semaphore
- **Error handling:** Graceful degradation — Claude API failures produce an error message in the conversation, not silent failure

### The Agent's Role in the System

The resident agent is a client of the AgentMail service that happens to live in the same process. It uses the same store interfaces and S2 client that the API handlers use. It demonstrates the system working end-to-end: receiving messages via stream reading, processing them through Claude, and streaming responses back via S2 appends.

---

## 1. Agent Identity & Persistence

### The Problem

The agent needs a stable identity across server restarts. If the agent registers fresh on every startup (new UUIDv7), its old identity is orphaned — still a "member" of conversations, but no one is listening. Any ongoing conversation with the evaluator breaks.

### Decision: Environment Variable

The agent's UUIDv7 is generated once during initial setup and stored as the `RESIDENT_AGENT_ID` environment variable (Fly.io secret). On every server startup, the agent ensures its identity exists in Postgres via idempotent insert.

```go
// On startup:
agentID := uuid.MustParse(os.Getenv("RESIDENT_AGENT_ID"))
// INSERT INTO agents (id) VALUES ($1) ON CONFLICT DO NOTHING
store.Agents().EnsureExists(ctx, agentID)
```

### Why Environment Variable, Not Postgres Table

**Alternative considered: `resident_agents` table.**
```sql
CREATE TABLE resident_agents (
    agent_id UUID PRIMARY KEY REFERENCES agents(id),
    role     TEXT NOT NULL
);
```
On startup: check table → if empty, register + insert → if populated, use existing IDs. Self-bootstrapping, no manual env var.

**Why rejected:**
- The `role` column is metadata on an agent. The spec says "no metadata" on agents. You could argue this is server-internal metadata, not agent metadata — but evaluators may question the distinction.
- Adds a 5th table to the schema for a deployment concern, not a domain concern.
- The env var approach separates identity (configuration) from schema (domain). The identity is a deployment decision, not a data model decision.

**Alternative considered: Hardcoded constant in Go source.**
```go
const ResidentAgentID = "01965ab3-..."
```
**Why rejected:** Identity baked into a binary can't change without recompile + redeploy. Env var is more operationally flexible.

### Startup Behavior When Env Var Is Missing

If `RESIDENT_AGENT_ID` is not set:
- Log a warning: "RESIDENT_AGENT_ID not set — running without resident agent"
- Skip entire agent bootstrap
- Server runs normally for external agents — all API endpoints work
- `GET /agents/resident` returns `{"agents": []}` (empty list, not 404)

This is degraded but functional. The API stands on its own without a resident agent.

### Initial Setup (One-Time)

```bash
# Generate the agent's ID once
AGENT_ID=$(uuidgen)  # Or generate UUIDv7 in Go and print it

# Set as Fly.io secret
fly secrets set RESIDENT_AGENT_ID=$AGENT_ID

# For local development
echo "RESIDENT_AGENT_ID=$AGENT_ID" >> .env
```

---

## 2. Discoverability

### The Problem

The evaluator's AI coding agent needs to programmatically find the resident agent's ID to invite it to conversations. This must work without parsing prose documentation.

### Decision: Dedicated Discovery Endpoint

```http
GET /agents/resident

Response: 200 OK
{
  "agents": [
    {"agent_id": "01965ab3-7c4f-7b2e-8a1d-3f5e2b1c9d8a"}
  ]
}
```

### Design Choices

**No `X-Agent-ID` header required.** This is a public discovery endpoint. The evaluator needs to call it before they've registered their own agent. Requiring authentication to discover the agent you want to talk to is a chicken-and-egg problem.

**Returns an array, not a single object.** Future-proofs for multiple resident agents (teacher + student) without API change. For now, the array has one element.

**No `description` or `name` field.** The spec says agents have "no metadata." The discovery endpoint respects this — it returns agent IDs only. CLIENT.md describes what the agent does in prose; the API doesn't.

**Why not embed in `GET /health`?** Mixing infrastructure status with agent discovery conflates concerns. A health check answers "is the service running?" — not "who can I talk to?" Separate endpoints, separate purposes.

**Why not documentation-only (no endpoint)?** An AI coding agent can call an HTTP endpoint trivially. Parsing a UUID out of a markdown paragraph is fragile and unnecessary. Programmatic discovery > prose discovery.

### Evaluator's Complete Onboarding Flow

```
1. GET /agents/resident             → learn resident agent ID
2. POST /agents                     → register yourself, get your agent_id
3. POST /conversations              → create conversation, get conv_id
   Header: X-Agent-ID: <your_agent_id>
4. POST /conversations/:cid/invite  → invite resident agent
   Header: X-Agent-ID: <your_agent_id>
   Body: {"agent_id": "<resident_agent_id>"}
5. GET /conversations/:cid/stream   → open SSE, start listening
   Header: X-Agent-ID: <your_agent_id>
6. POST /conversations/:cid/messages → send a message
   Header: X-Agent-ID: <your_agent_id>
   Body: {"content": "Hello!"}
7. ← SSE delivers resident agent's streaming response
```

Six steps from zero to conversation. An AI coding agent can follow this from CLIENT.md without human assistance.

---

## 3. Conversation Discovery

### The Problem

The agent is listening to N conversations via N SSE-equivalent read sessions. Someone invites the agent to conversation N+1. The agent needs to learn about this and start listening.

The current API has no push mechanism for "you were invited." SSE is per-conversation, not per-agent. There's no agent-level notification stream.

### Options Evaluated

**Option 1: Poll `ListConversations()` periodically**

Agent maintains a set of known conversation IDs. Every X seconds, calls `store.Members().ListConversations(ctx, agentID)`. Diffs against known set → new conversations → starts a listener for each.

| Pro | Con |
|---|---|
| Simple, uses existing store interface | Latency: up to X seconds to notice new invite |
| No coupling between API layer and agent | Wasteful if nothing changed (negligible at our scale since it's a direct function call) |
| Works identically on restart (catches everything) | |

**Option 2: Go channel — internal event bus**

The invite handler pushes a notification to the agent via a Go channel when the invited agent is a resident agent.

```go
type AgentNotifier interface {
    OnInvite(agentID, conversationID uuid.UUID)
}
```

Invite handler calls `notifier.OnInvite(invitedAgentID, convID)`. The notifier checks if this agent is resident and dispatches.

| Pro | Con |
|---|---|
| Zero latency — agent knows instantly | Couples API layer to agent subsystem |
| Clean Go concurrency pattern | Only works for internal agents (breaks if agent moves to external process) |
| No polling overhead | |

**Option 3: Per-agent S2 notification stream**

A special S2 stream `agents/{agent_id}/notifications`. On invite, write an `invited_to_conversation` event. Agent tails its notification stream.

| Pro | Con |
|---|---|
| Durable — survives restarts | Adds S2 streams per agent (contradicts "one stream per conversation" simplicity) |
| External agents could tail it too | Over-engineered for single-process deployment |
| Extends to other notification types | Extra S2 cost per agent |

**Option 4: Postgres LISTEN/NOTIFY**

On invite, `NOTIFY agent_invited, 'agent_id:conv_id'`. Agent listens on a dedicated Postgres connection.

| Pro | Con |
|---|---|
| Decoupled from API handler | Agent is internal — going through Postgres to notify yourself is roundabout |
| Multi-instance ready | Requires a dedicated Postgres connection (held for lifetime) |
| | Fire-and-forget — if agent misses it, it's gone |

### Decision: Go Channel + Startup Reconciliation

**Real-time (after startup):** Go channel for instant notification when the agent is invited to a new conversation. The invite handler signals the agent directly — zero latency.

**Startup catch-up:** On boot, list all conversations the agent is a member of via `ListConversations()`. Start a listener for each one. This catches any invites that arrived while the server was down.

**No periodic polling.** The Go channel is reliable in a single-process server. The only failure mode — channel buffer overflow — doesn't occur at take-home scale (buffer capacity 100, agent will be in at most a handful of conversations). Periodic polling adds complexity for a failure mode that doesn't exist. If the channel ever proves unreliable (it won't), polling can be added later without design changes.

**No gap between startup reconciliation and channel activation.** The agent bootstraps BEFORE the HTTP server starts accepting traffic. No invites can arrive during the bootstrap window because the invite endpoint isn't serving yet. Sequence:
1. Agent bootstrap: reconcile conversations, start listeners, activate channel
2. HTTP server starts: invites now flow through the channel

### Implementation

**Notifier interface (injected into API layer):**

```go
// AgentNotifier is called by the invite handler when an agent is added to a conversation.
// The implementation checks whether the invited agent is a resident agent and dispatches accordingly.
type AgentNotifier interface {
    OnInvite(agentID, conversationID uuid.UUID)
}

// noopNotifier is used when no resident agent is configured.
type noopNotifier struct{}
func (n *noopNotifier) OnInvite(_, _ uuid.UUID) {}
```

**Agent-side notifier:**

```go
type agentNotifier struct {
    residentIDs map[uuid.UUID]chan uuid.UUID  // agent_id → channel of conv_ids
}

func (n *agentNotifier) OnInvite(agentID, convID uuid.UUID) {
    ch, ok := n.residentIDs[agentID]
    if !ok {
        return  // not a resident agent, ignore
    }
    select {
    case ch <- convID:
        // delivered
    default:
        // buffer full — log warning. At take-home scale this never happens.
        log.Warn().Str("agent_id", agentID.String()).Msg("invite notification dropped (buffer full)")
    }
}
```

**Invite handler notification ordering:**

The invite handler must call `notifier.OnInvite()` AFTER the `agent_joined` S2 write, not before. This minimizes the stream-not-found race (see edge cases below). But the notification fires regardless of S2 write success — the agent IS a member in Postgres.

```
Invite handler sequence:
  1. INSERT INTO members (Postgres)           ← agent is now a member
  2. Append agent_joined to S2                ← stream auto-creates here (may fail)
  3. Call notifier.OnInvite(agentID, convID)  ← agent notified AFTER S2 write attempt
```

If step 2 fails (S2 unreachable), step 3 still fires. The agent tries to open a ReadSession, hits stream-not-found, and retries with backoff (see `startListening()` below). The stream auto-creates on the next successful S2 write to that conversation.

**Discovery goroutine (per resident agent):**

```go
func (a *Agent) discoveryLoop(ctx context.Context) {
    // Startup: list all conversations, start listeners for each (with retries)
    a.reconcileConversations(ctx)

    // After startup: channel-only — wait for invite notifications
    for {
        select {
        case convID := <-a.inviteCh:
            a.startListening(ctx, convID)
        case <-ctx.Done():
            return
        }
    }
}
```

**Startup reconciliation (with retry):**

```go
func (a *Agent) reconcileConversations(ctx context.Context) {
    var convs []ConversationWithMembers
    var err error

    // Retry up to 3 times — Postgres is confirmed reachable (migration passed),
    // but transient failures are possible.
    for attempt := 0; attempt < 3; attempt++ {
        convs, err = a.store.Members().ListConversations(ctx, a.id)
        if err == nil {
            break
        }
        log.Warn().Err(err).Int("attempt", attempt+1).Msg("reconciliation failed, retrying")
        time.Sleep(time.Duration(attempt+1) * time.Second) // 1s, 2s, 3s
    }
    if err != nil {
        log.Error().Err(err).Msg("reconciliation failed after 3 attempts — starting with no listeners")
        return
    }

    log.Info().Int("conversations", len(convs)).Msg("reconciling existing conversations")
    for _, conv := range convs {
        a.startListening(ctx, conv.ID) // idempotent — skips if already listening
    }
}
```

**startListening() — idempotent, self-healing, ConnRegistry-integrated:**

`startListening()` is the outer wrapper that manages the listener goroutine's full lifecycle: idempotency check, ConnRegistry registration, retry loop, and cleanup. The inner `listen()` function (Section 4) handles the pure ReadSession loop.

The same `cancel` function is stored in both `a.listeners` (for agent-side tracking and shutdown) and `ConnRegistry` (for leave handler access). Calling `cancel()` from either path is idempotent — context cancellation is safe to call multiple times.

```go
func (a *Agent) startListening(ctx context.Context, convID uuid.UUID) {
    a.mu.Lock()
    if _, exists := a.listeners[convID]; exists {
        a.mu.Unlock()
        return // already listening — idempotent
    }
    listenerCtx, cancel := context.WithCancel(ctx)
    a.listeners[convID] = cancel
    a.mu.Unlock()

    done := make(chan struct{})

    // Register in ConnRegistry — leave handler can find and cancel us
    // using the exact same mechanism it uses for external SSE clients.
    a.store.ConnRegistry().Register(a.id, convID, cancel, done)

    a.wg.Add(1)
    go func() {
        defer a.wg.Done()
        defer func() {
            // Cleanup ordering:
            // 1. Flush cursor (needs a live context — listenerCtx is canceled)
            a.store.Cursors().FlushOne(context.Background(), a.id, convID)
            // 2. Signal completion — unblocks leave handler waiting on <-done
            close(done)
            // 3. Remove from ConnRegistry
            a.store.ConnRegistry().Deregister(a.id, convID)
            // 4. Remove from agent's tracking map (allows re-invite to start fresh)
            a.mu.Lock()
            delete(a.listeners, convID)
            a.mu.Unlock()
        }()

        backoff := 1 * time.Second
        for {
            err := a.listen(listenerCtx, convID)
            if listenerCtx.Err() != nil {
                return // context canceled — leave or shutdown, no retry
            }
            if isStreamNotFound(err) {
                // Stream doesn't exist yet — conversation was created in Postgres
                // but no S2 write has landed yet. Retry until the first write
                // (a message, another invite's agent_joined, etc.) creates the stream.
                log.Debug().Str("conv_id", convID.String()).Msg("stream not found yet, retrying")
            } else {
                // Transient S2 error — log and retry
                log.Warn().Err(err).Str("conv_id", convID.String()).Msg("listener error, reconnecting")
            }
            select {
            case <-time.After(backoff):
                backoff = min(backoff*2, 30*time.Second)
            case <-listenerCtx.Done():
                return
            }
        }
    }()
}
```

**Key properties of `startListening()`:**
- **Idempotent:** Checks `a.listeners` map under mutex before spawning goroutine.
- **ConnRegistry-integrated:** Registers with the same mechanism external SSE clients use. Leave handler's existing code works unchanged for the resident agent.
- **Single cancel function:** Shared between `a.listeners` and ConnRegistry. No dual-context problem.
- **Self-healing:** On any S2 error (stream-not-found, network failure, etc.), retries with exponential backoff up to 30 seconds.
- **Clean shutdown:** Respects context cancellation. Goroutine exits when `listenerCtx` is canceled (leave or server shutdown).
- **Self-cleaning:** Deferred cleanup flushes cursor, signals done, deregisters, and removes from listeners map.
- **Cleanup ordering:** Cursor flush before `close(done)` ensures the cursor is durable before the leave handler proceeds to write `agent_left` to S2.

### Conversation Listening Policy

**Listen to ALL conversations the agent is a member of.** On startup, the agent opens a read session for every conversation returned by `ListConversations()`. On invite, it immediately starts listening to the new conversation. Each listener is a **passive blocking tail** — the goroutine is parked on `readSession.Next()`, consuming zero CPU and zero network traffic while idle. It wakes up only when S2 delivers a new record.

**Cost of listening to N idle conversations:** N goroutines × ~4 KB stack = negligible memory. N S2 ReadSessions × 1 op/minute = ~$0.04/month each. At take-home scale (tens of conversations), this is trivially cheap.

**Why listen to all:** The evaluator expects the agent to respond in ANY conversation it's been invited to. Silently ignoring a conversation — even a stale one — is a broken experience.

**Production scaling note (FUTURE.md):** At thousands of conversations, the cost scales linearly but remains manageable. At tens of thousands, consider listening only to recently active conversations, with a per-agent S2 notification stream to wake up stale listeners. Not needed for the take-home.

### Edge Cases

**Agent invited while server is starting (between Postgres commit and agent bootstrap):**
The invite lands in Postgres before the discovery goroutine starts. On startup, `reconcileConversations()` lists all conversations → catches it. No lost invites.

**Agent invited to a conversation it's already listening to (re-invite after leave):**
`startListening()` checks `a.listeners` map under mutex → if listener exists, returns immediately (idempotent). If the agent left and was re-invited, the old listener was cleaned up on leave (deferred `delete(a.listeners, convID)`), so `startListening()` creates a fresh one.

**Stream-not-found race on new conversation:**
The invite handler writes `agent_joined` to S2 (creating the stream) before notifying the agent. But if the S2 write fails (S2 unreachable), the notification still fires. The agent's `startListening()` retries with backoff until the stream exists. The stream auto-creates on the next successful S2 write to that conversation. **No conversation is permanently lost** — the retry loop is self-healing.

**Agent is removed from a conversation:**
The leave handler terminates the agent's read session via ConnRegistry (the agent registers its connections there — same mechanism as external SSE clients, detailed in Section 4). The listener goroutine's context is canceled, it exits, and the deferred cleanup removes the conversation from `a.listeners`. If re-invited later, the channel notification triggers `startListening()` which creates a fresh listener.

**Channel buffer full (theoretical):**
Non-blocking send with `select/default`. The notification is dropped with a warning log. The agent doesn't learn about the new conversation until the next server restart (when `reconcileConversations()` catches it). At take-home scale with a buffer of 100, this never happens. If it did, the consequence is delayed response — not data loss.

**Startup reconciliation fails (Postgres transient error):**
Retries 3 times with increasing backoff (1s, 2s, 3s). If all 3 fail, starts with no listeners and logs an error. New invites after startup still flow through the channel. Only pre-existing conversations are missed. Recovery: restart the server.

### Production Scaling Path

When the agent moves to an external process or multiple server instances exist:
- Replace Go channel with per-agent S2 notification stream (`agents/{agent_id}/notifications`)
- On invite, append `invited_to_conversation` event to the agent's notification stream
- Agent tails its notification stream — durable, survives restarts, works across processes
- Add periodic polling as a safety net for missed notifications across process boundaries

---

## 4. Listener Architecture

### The Problem

The agent needs one S2 read session per active conversation. Each listener is a goroutine that tails the conversation's S2 stream, processes events, updates cursors, and hands events to the response system. The design must handle: lifecycle management, leave termination via ConnRegistry, S2 failures and reconnection, cursor tracking, and clean shutdown across all active listeners.

### Decision: Direct S2 ReadSession (Not HTTP SSE to Self)

The agent calls `s2Store.OpenReadSession(ctx, convID, fromSeq)` directly — the same S2 SDK call the SSE handler uses internally.

| Dimension | Direct S2 ReadSession (chosen) | HTTP SSE to localhost |
|---|---|---|
| **Overhead** | Zero — direct function call | HTTP serialization, response parsing, TCP loopback |
| **Bootstrap ordering** | Works before HTTP server starts | HTTP server must be running first |
| **Cursor management** | Calls same `CursorStore` the SSE handler uses | Gets cursor management "for free" via SSE handler |
| **Code reuse** | ~20 lines for the ReadSession loop | Zero new code, but couples agent to HTTP layer |
| **Failure isolation** | Agent fails independently of HTTP stack | HTTP stack failure kills agent reads |

**The cursor management concern is a non-issue.** The agent calls `cursors.GetCursor()` and `cursors.UpdateCursor()` — the same store interface the SSE handler uses. It's not reimplementation. The only "duplicated" logic is the `for readSession.Next()` loop and cursor update call — ~20 lines of straightforward code.

**The "doesn't exercise the SSE code path" concern is a non-issue.** Integration tests exercise the SSE path. The agent's job is to be reliable, not to be a test harness.

---

### Agent Struct

```go
type Agent struct {
    id       uuid.UUID
    store    Store
    s2       S2Store
    claude   *anthropic.Client
    inviteCh chan uuid.UUID          // buffered, capacity 100

    mu        sync.Mutex
    listeners map[uuid.UUID]context.CancelFunc  // convID → cancel
    wg        sync.WaitGroup                     // tracks all listener goroutines
}
```

**Why `sync.Mutex`, not `sync.RWMutex` or `sync.Map`:** The listeners map is modified on invite and leave (infrequent). All access patterns are writes (add/remove) or short reads (idempotency check in `startListening`). No concurrent read-heavy workload that would benefit from `RWMutex`. `sync.Map`'s optimization patterns (write-once-read-many, disjoint keys) don't apply.

---

### ConnRegistry Integration

Section 3 defined `startListening()` with the retry loop and idempotency. This section explains why and how it integrates with ConnRegistry.

**The problem:** If the agent uses direct S2 ReadSessions instead of HTTP SSE, the leave handler's ConnRegistry-based termination doesn't reach it by default. The leave handler looks up `(agent_id, conversation_id)` in ConnRegistry — if the agent isn't registered there, the leave handler can't cancel the listener.

**Options considered:**

**Option A: Extend `AgentNotifier` with `OnLeave()`.**
The leave handler calls `notifier.OnLeave(agentID, convID)`. The agent's notifier cancels the listener.

| Pro | Con |
|---|---|
| No ConnRegistry dependency | Duplicates leave termination logic |
| | Leave handler needs two code paths: ConnRegistry for external clients, OnLeave for resident agent |
| | Must still wait for goroutine exit before writing `agent_left` to S2 |

**Option B: Register in ConnRegistry (chosen).**
The agent's listener goroutine registers in ConnRegistry the same way an external SSE handler does. The leave handler's existing code works unchanged.

| Pro | Con |
|---|---|
| Leave handler code unchanged — one path for all clients | Agent depends on ConnRegistry abstraction |
| Wait-for-exit semantics already built in (done channel) | |
| Event ordering guarantee (`message_abort` before `agent_left`) already handled | |

**Why Option B:** ConnRegistry already solves every problem — cancellation, wait-for-exit, cleanup sequencing. Reimplementing this via `OnLeave()` is strictly worse. The agent is a first-class participant in the connection lifecycle.

**Single cancel function — no dual-context problem:** `startListening()` creates one `listenerCtx, cancel` pair. This same `cancel` is stored in:
- `a.listeners[convID]` — for agent-side tracking (idempotency check, shutdown)
- `ConnRegistry` entry — for leave handler access

Both point to the same cancel function. The leave handler calling `entry.Cancel()` cancels `listenerCtx`, which causes `readSession.Next()` to return false, which causes `listen()` to return, which causes the retry loop to check `listenerCtx.Err()` → not nil → exit. No retry after leave.

The implementation is in Section 3's updated `startListening()`.

---

### The `listen()` Function — Pure ReadSession Loop

`listen()` is the inner function called by `startListening()`'s retry loop. It handles exactly one ReadSession lifecycle: resolve cursor → open session → read events → dispatch → update cursors → return on error or cancellation.

```go
func (a *Agent) listen(ctx context.Context, convID uuid.UUID) error {
    // 1. Resolve starting position
    startSeq, err := a.store.Cursors().GetCursor(ctx, a.id, convID)
    if err != nil {
        // No cursor exists — start from beginning of conversation
        startSeq = 0
    } else {
        // Resume AFTER the last delivered event
        startSeq++
    }

    // 2. Open tailing ReadSession
    readSession, err := a.s2.OpenReadSession(ctx, convID, startSeq)
    if err != nil {
        return err // caller handles retry (stream-not-found, S2 unreachable, etc.)
    }
    defer readSession.Close()

    // 3. Read events until context cancellation or S2 error
    for readSession.Next() {
        event := readSession.Event()

        // Update cursor in memory (batched flush to Postgres every 5s by CursorStore)
        a.store.Cursors().UpdateCursor(a.id, convID, event.SeqNum)

        // Dispatch to message accumulation and response system (Section 5)
        a.onEvent(convID, event)
    }

    // 4. Session ended — return the error for the caller to handle
    return readSession.Err()
}
```

**Properties of `listen()`:**
- **~20 lines.** No lifecycle management, no retry logic, no registration. Just the pure read loop.
- **Cursor tracking uses the existing CursorStore.** `GetCursor()` reads from in-memory cache (falling back to Postgres). `UpdateCursor()` writes to in-memory cache only (batched flush handles durability). Same code path as external SSE clients.
- **`onEvent()` is the bridge to Section 5 (D).** C reads events and hands them off. What happens with them — message demultiplexing, response triggering, echo filtering — is D's concern.
- **Return value drives the retry loop.** `nil` error with canceled context = normal exit. Stream-not-found error = retry with backoff. Other S2 error = retry with backoff.

---

### Event Dispatch Interface

`a.onEvent()` is the boundary between the listener architecture (C) and the message processing system (D). C delivers every event from the S2 stream. D decides what to do with each one.

```go
// onEvent is called for every event read from the S2 stream.
// Section 5 (Message Demultiplexing & Response Triggering) specifies the full implementation.
func (a *Agent) onEvent(convID uuid.UUID, event SequencedEvent) {
    // Dispatched to the message accumulation and response triggering system.
    // C is not concerned with what happens here — it just delivers events.
}
```

**Why a method call, not a channel:** The listener goroutine processes events sequentially within a conversation. There's no need for an async handoff — `onEvent()` processes the event and returns. If `onEvent()` triggers a Claude response, that response runs in a separate goroutine (designed in Section 6). The listener doesn't block waiting for Claude — it continues reading subsequent events while the response streams.

---

### Shutdown Sequence

When the server shuts down, the agent must cleanly terminate all listeners, flush all cursors, and wait for all goroutines to exit.

```go
func (a *Agent) Shutdown(ctx context.Context) {
    // 1. Cancel all listener contexts
    a.mu.Lock()
    for _, cancel := range a.listeners {
        cancel()
    }
    a.mu.Unlock()

    // 2. Wait for all listener goroutines to exit (with timeout)
    done := make(chan struct{})
    go func() {
        a.wg.Wait()
        close(done)
    }()

    select {
    case <-done:
        log.Info().Msg("all agent listeners shut down cleanly")
    case <-ctx.Done():
        log.Warn().Msg("agent shutdown timed out — some listeners may not have flushed cursors")
    }
}
```

**Shutdown flow per listener:**
1. `cancel()` called → `listenerCtx.Done()` fires
2. `readSession.Next()` returns false (context canceled)
3. `listen()` returns context error
4. Retry loop checks `listenerCtx.Err()` → not nil → exits (no retry)
5. Deferred cleanup: flush cursor → close(done) → deregister from ConnRegistry → delete from listeners map
6. `wg.Done()` — one fewer goroutine to wait for

**Timeout:** The shutdown context should have a deadline (e.g., 10 seconds). If a goroutine is stuck (S2 ReadSession not responding to cancellation), the shutdown proceeds without it. Cursor data for stuck listeners is lost — at most 5 seconds of cursor drift, recovered on next startup via at-least-once delivery.

---

### Leave Handler Integration

The leave handler's existing flow (from `sql-metadata-plan.md` Section 10) works unchanged for the resident agent:

```
Leave handler (unchanged):
  1. Postgres transaction: lock members, check count > 1, delete member, commit
  2. Invalidate membership cache
  3. Look up (agent_id, conversation_id) in ConnRegistry     ← finds the agent's entry
  4. Call entry.Cancel()                                       ← cancels listenerCtx
  5. <-entry.Done (5s timeout)                                 ← waits for listener to exit
  6. Append agent_left to S2 stream
  7. Return 200
```

**Event ordering guarantee:** `agent_left` is written to S2 (step 6) only AFTER the listener goroutine has exited (step 5) and flushed its cursor. If the agent was mid-response when leave was called (streaming a Claude reply), the response system must abort and write `message_abort` before the goroutine exits. **Dependency on Section 6:** the Claude API integration must ensure any in-progress response is aborted and `message_abort` is written to S2 before `close(done)` fires in the deferred cleanup. This guarantees the ordering: `message_abort` → `agent_left`.

---

### Edge Cases

**Agent has an in-progress Claude response when removed from conversation:**
Context cancellation propagates to both the ReadSession and the response goroutine (Section 6). The response system detects cancellation, writes `message_abort` to S2 via unary append (not the possibly-errored AppendSession), and exits. Then the listener goroutine's deferred cleanup runs, closing `done`. The leave handler sees `done` close, writes `agent_left`. Ordering on the S2 stream: `message_abort` → `agent_left`. Correct.

**S2 ReadSession fails mid-stream (transient network error):**
`readSession.Next()` returns false with a non-nil error. `listen()` returns the error. `startListening()`'s retry loop checks `listenerCtx.Err()` → nil (not canceled, just S2 failure) → retries with backoff. On retry, `listen()` re-reads the cursor (updated in memory up to the last successfully processed event) and opens a fresh ReadSession. At-least-once delivery — the agent may re-process a few events. Events carry sequence numbers for deduplication in Section 5.

**Cursor is stale after server crash:**
The in-memory cursor was updated on every event delivery but may not have been flushed to Postgres. On restart, `GetCursor()` falls back to Postgres — at most 5 seconds stale. The agent re-processes up to ~150 events (5 sec × ~30 events/sec). Idempotent event processing in Section 5 handles this.

**Two listeners for the same conversation (race between invite notification and reconciliation):**
Impossible. `startListening()` is idempotent — mutex-guarded map check prevents duplicate goroutines. Both code paths call `startListening()`, which returns immediately if a listener already exists.

**ConnRegistry already has an entry for this (agent, conversation) pair:**
This happens if a previous listener didn't clean up properly (bug or crash). ConnRegistry's `Register()` should cancel and replace the old entry — same behavior as "one SSE connection per (agent, conversation)" for external clients (specified in `sql-metadata-plan.md` Section 10).

**Agent's listener goroutine panics:**
The deferred cleanup in `startListening()` still runs (Go deferred functions execute on panic). `close(done)` fires, ConnRegistry is deregistered, listeners map is cleaned up. The goroutine is not restarted — the conversation is no longer listened to. Recovery: server restart, or re-invite triggers a fresh `startListening()`.

---

### Files

```
internal/agent/
├── agent.go       # Agent struct, startListening(), listen(), onEvent(), Shutdown()
├── notifier.go    # AgentNotifier interface, agentNotifier, noopNotifier
└── discovery.go   # discoveryLoop(), reconcileConversations()
```

---

## 5. Message Demultiplexing & Response Triggering

### The Problem

The S2 stream contains interleaved events from multiple agents and multiple in-flight messages. A single conversation might have:

```
seq 100: message_start   {message_id: "m1", sender: "agent-B"}
seq 101: message_append  {message_id: "m1", token: "Hello"}
seq 102: message_start   {message_id: "m2", sender: "agent-C"}   ← interleaved
seq 103: message_append  {message_id: "m1", token: " there"}
seq 104: message_append  {message_id: "m2", token: "Hey"}
seq 105: message_end     {message_id: "m1"}
seq 106: message_end     {message_id: "m2"}
```

The agent needs to:
1. **Reassemble complete messages** from interleaved event fragments, grouping by `message_id`
2. **Decide whether to respond** to each complete message
3. **Feed message history** to Claude for context
4. **Handle its own echoes** — the agent's responses appear on the same stream

### Decision: Per-Conversation State with Lazy History Seeding

Each conversation the agent listens to has a `convState` that tracks in-progress message assembly and accumulated history. History is NOT seeded on connect — it's seeded lazily on the first `message_end` from another agent, because most conversations may be dormant.

---

### Domain Types

```go
// Message is the agent's internal representation of a complete, assembled message.
// No Role field — role is perspective-dependent and derived at Claude-call-time
// from (msg.Sender == a.id). See Section 6 for Claude API formatting.
type Message struct {
    Sender  string // agent_id (for multi-party attribution and role derivation)
    Content string // full assembled content
}

// pendingMessage tracks a message being assembled from streaming events.
type pendingMessage struct {
    sender     string
    content    strings.Builder
    lastAppend time.Time // for stale cleanup
}

// convState holds per-conversation state for the agent.
type convState struct {
    mu      sync.Mutex
    pending map[string]*pendingMessage // message_id → in-progress assembly
    history []Message                  // completed messages (bounded sliding window)
    seeded  bool                       // false until first lazy history seed
}

const maxHistory = 50
```

**Why no `Role` field on `Message`:** Role is perspective-dependent. In a group conversation, the same message from Agent X is `"assistant"` from X's perspective but `"user"` from Y's. Storing it bakes in one perspective. Instead, derive it in the Claude API formatting step:

```go
role := "user"
if msg.Sender == a.id {
    role = "assistant"
}
```

One `if` at call-time. Zero storage. Correct for every perspective.

---

### Conversation State Management

```go
func (a *Agent) getOrCreateConvState(convID uuid.UUID) *convState {
    a.mu.Lock()
    defer a.mu.Unlock()
    state, ok := a.convStates[convID]
    if !ok {
        state = &convState{
            pending: make(map[string]*pendingMessage),
        }
        a.convStates[convID] = state
    }
    return state
}
```

**`convStates` field added to Agent struct:**

```go
type Agent struct {
    id       uuid.UUID
    store    Store
    s2       S2Store
    claude   *anthropic.Client
    inviteCh chan uuid.UUID

    mu         sync.Mutex
    listeners  map[uuid.UUID]context.CancelFunc
    convStates map[uuid.UUID]*convState // convID → per-conversation state
    wg         sync.WaitGroup
}
```

`convStates` lives under the same `a.mu` mutex as `listeners`. Created lazily on first event. Cleaned up when the listener exits (leave or shutdown).

---

### Event Dispatch — `onEvent()`

```go
func (a *Agent) onEvent(convID uuid.UUID, event SequencedEvent) {
    state := a.getOrCreateConvState(convID)
    state.mu.Lock()
    defer state.mu.Unlock()

    // Lazy cleanup: sweep stale pending entries on every event
    state.cleanStalePending()

    switch event.Type {
    case "message_start":
        state.pending[event.MessageID] = &pendingMessage{
            sender:     event.Sender,
            lastAppend: time.Now(),
        }

    case "message_append":
        p, ok := state.pending[event.MessageID]
        if !ok {
            return // orphaned append — start was missed (cursor skip or crash recovery)
        }
        p.content.WriteString(event.Token)
        p.lastAppend = time.Now()

    case "message_end":
        p, ok := state.pending[event.MessageID]
        if !ok {
            return // orphaned end — start was missed
        }
        delete(state.pending, event.MessageID)

        msg := Message{
            Sender:  p.sender,
            Content: p.content.String(),
        }
        state.appendMessage(msg)

        // Self-message skip: hardcoded, never sent to Claude.
        // The agent's own responses echo back on the stream — always ignore.
        if msg.Sender == a.id {
            return
        }

        // Trigger response (releases lock, runs async — see Section 6)
        a.triggerResponse(convID, state)

    case "message_abort":
        delete(state.pending, event.MessageID)
        // No response triggered — the message was abandoned.

    case "agent_joined", "agent_left":
        // Membership events — no action needed for message processing.
        // Could log or update local membership cache if needed.
    }
}
```

**Key design decisions in `onEvent()`:**

**Self-message skip is hardcoded, not sent to Claude.** The agent's own responses appear on the same S2 stream. Without this check, every response triggers another Claude API call that returns `[NO_RESPONSE]` — wasting money and latency. "Should I respond to myself?" is never a judgment call. Hardcode it.

**`message_append` with no matching `message_start`:** Silently dropped. This happens when the agent's cursor resumes mid-message after a crash. The partial message is unrecoverable — wait for the next complete message. Not an error.

**`message_abort`:** Delete the pending entry, no response. The sender intentionally abandoned the message.

---

### Lazy History Seeding

History is seeded on the first `message_end` from another agent, not on connect. This avoids paying the ReadRange cost for dormant conversations that never receive new messages.

```go
func (a *Agent) triggerResponse(convID uuid.UUID, state *convState) {
    // Lazy seed: first time we need to respond, load history
    if !state.seeded {
        a.seedHistory(convID, state)
        state.seeded = true
    }

    // Queue response for Claude (Section 6 designs the full response pipeline)
    // ... response queuing logic ...
}
```

**`seedHistory()` — read last N messages from S2:**

```go
func (a *Agent) seedHistory(convID uuid.UUID, state *convState) {
    // Read last 500 events from S2 via ReadRange (not tailing ReadSession).
    // 500 events is a ceiling — at ~10 events per message (start + 8 appends + end),
    // this recovers approximately the last 50 messages.
    events, err := a.s2.ReadRange(context.Background(), convID, -500)
    if err != nil {
        log.Warn().Err(err).Str("conv_id", convID.String()).Msg("history seed failed — proceeding with empty context")
        return
    }

    // Assemble events into complete messages (same logic as pending assembly)
    messages := assembleMessages(events)

    // Prepend to existing history (which may have 1+ messages from stream events
    // that arrived before this seed was triggered)
    existing := state.history
    state.history = make([]Message, 0, len(messages)+len(existing))
    state.history = append(state.history, messages...)
    state.history = append(state.history, existing...)

    // Enforce sliding window after merge
    if len(state.history) > maxHistory {
        state.history = state.history[len(state.history)-maxHistory:]
    }
}
```

**Why read 500 events, not 50 messages:** S2 stores events, not messages. A single message is ~10 events (1 start + N appends + 1 end). Reading 500 events recovers approximately 50 complete messages. Some events at the start of the range may be mid-message (missing `message_start`) — `assembleMessages()` discards these gracefully (same orphan-handling as `onEvent`).

**Why `context.Background()` instead of `listenerCtx`:** History seeding is a best-effort operation. If the listener is being canceled (leave/shutdown), we still want to attempt the seed so the response has context. The seed is a bounded read (500 events) — it completes quickly. If it fails, we proceed with whatever history we have.

**Seed failure is non-fatal.** If S2 ReadRange fails, the agent responds with whatever context it has (possibly just the single message that triggered the response). A shallow response is better than no response. The next `message_end` won't re-seed (seeded flag is already true) — this is intentional. Re-seeding on every message creates retry storms.

---

### Bounded History — Sliding Window

History is capped at `maxHistory` (50 messages). Every append trims the oldest messages.

```go
func (s *convState) appendMessage(msg Message) {
    s.history = append(s.history, msg)
    if len(s.history) > maxHistory {
        s.history = s.history[len(s.history)-maxHistory:]
    }
}
```

**Why 50:** Claude's context window is large (200K tokens), but we're sending system prompt + history. At ~500 tokens per message, 50 messages is ~25K tokens — well within budget while leaving room for the system prompt and Claude's response. This can be tuned, but 50 is a reasonable default.

**Why not unbounded:** A conversation with 1,000 messages would accumulate 1,000 `Message` structs in memory — per conversation. Multiply by N conversations. Unbounded accumulation is the same anti-pattern as eager seeding: paying a cost that grows without bound for context that will be truncated anyway before sending to Claude.

---

### Stale Pending Cleanup — Lazy, Not Sweeper

Pending entries that never receive `message_end` (sender disconnected, network failure, `message_abort` lost) would leak memory without cleanup.

```go
const stalePendingTimeout = 5 * time.Minute

func (s *convState) cleanStalePending() {
    now := time.Now()
    for msgID, p := range s.pending {
        if now.Sub(p.lastAppend) > stalePendingTimeout {
            delete(s.pending, msgID)
        }
    }
}
```

**Called at the top of `onEvent()`.** Every incoming event sweeps stale entries for that conversation. Active conversations self-clean. Dormant conversations with orphaned pending entries get cleaned on next activity (which is also when lazy seeding triggers — natural alignment).

**Why not a background sweeper goroutine:** A ticker that scans all conversations' pending maps is the eager-loading anti-pattern applied to cleanup. It runs even when nothing is stale. Lazy cleanup piggybacks on existing work — zero overhead when the conversation is dormant.

**Why 5 minutes:** A message that hasn't received an append in 5 minutes is abandoned. Normal streaming completes in seconds. 5 minutes is generous enough to handle any real-world stall without holding orphans indefinitely.

---

### Response Triggering Strategy

**Non-deterministic: Claude decides whether to respond.**

After a complete message arrives from another agent, the agent sends the conversation history to Claude with a system prompt that includes:

```
You are participating in a conversation. Based on the context, decide whether to respond.
If you determine that no response is needed, reply with exactly: [NO_RESPONSE]
Otherwise, respond naturally.
```

Claude sees the full conversation context and makes the judgment call. This handles:
- Group conversations where the message is directed at someone else
- Messages that are acknowledgments ("ok", "thanks") that don't need a reply
- Situations where the agent already responded and the other party is continuing a thought

**Why non-deterministic over rules-based:**

| Approach | Problem |
|---|---|
| Respond to every `message_end` | Infinite loops in group conversations with 2+ AI agents |
| "Don't respond if last message is yours" | Breaks multi-message sequences ("I have a question..." / "Actually two questions...") |
| "Only respond if mentioned" | Requires @mention convention that doesn't exist in the spec |
| Claude decides | Handles all cases — context-aware, no edge case rules to maintain |

**The `[NO_RESPONSE]` check:**

```go
// After Claude API call completes (Section 6):
response := claude.GetResponse()
if strings.TrimSpace(response) == "[NO_RESPONSE]" {
    // Claude decided not to respond. No S2 write. No events emitted.
    return
}
// Otherwise: stream response to S2 via AppendSession
```

**Cost consideration:** Every `message_end` from another agent triggers a Claude API call, even if Claude decides not to respond. At take-home scale (handful of conversations, evaluator testing), this is negligible. At production scale, a lightweight pre-filter (local LLM, heuristic scorer) could gate which messages reach Claude. Not needed now.

**Self-messages are NOT sent to Claude.** The hardcoded `if msg.Sender == a.id { return }` in `onEvent()` prevents self-echo loops before the Claude API is ever called. The `[NO_RESPONSE]` protocol handles the genuinely ambiguous cases — messages from OTHER agents that may or may not warrant a response.

---

### At-Least-Once Delivery — Dedup After Crash Recovery

When the agent restarts and `listen()` resumes from the Postgres cursor, it may re-process events that were already handled before the crash. The cursor is at most 5 seconds stale (batched flush interval).

**Lazy seeding provides natural dedup.** When `seedHistory()` reads the last 500 events and assembles messages, it covers the full range of potentially re-delivered events. The `seeded` flag prevents re-processing: once history is seeded, subsequent events from the stream are new (they have sequence numbers beyond the seeded range).

**Edge case: events between cursor and seed range.** If the cursor is at seq 950 and seeding reads events 500–1000, events 950–1000 are in both the seeded history and the stream replay. These arrive as `message_start`/`message_append`/`message_end` events through `onEvent()`. The completed messages get appended to `state.history` — duplicating messages already in the seeded history.

**Mitigation:** Track the highest sequence number from seeding:

```go
type convState struct {
    // ... existing fields ...
    lastSeededSeq uint64 // highest seq from seedHistory()
}
```

In `onEvent()`, before processing:
```go
if event.SeqNum <= state.lastSeededSeq {
    // Already covered by seeded history — skip to avoid duplicates
    // Still update cursor so we don't re-process on next restart
    return
}
```

This is a simple sequence comparison — O(1), no set lookups, no bloom filters.

---

### Edge Cases

**Message completes but sender has already left the conversation:**
The `message_end` event is on the stream — it was written before the leave. The agent processes it normally. The sender's departure doesn't retroactively invalidate their last message.

**Agent receives events from before it joined (cursor at 0 on first listen):**
On first listen (no cursor), `startSeq = 0` means the agent reads the full stream from the beginning. These events flow through `onEvent()` as normal. The first `message_end` from another agent triggers lazy seeding — but `seedHistory()` reads the same events the stream is delivering. The `lastSeededSeq` dedup handles this overlap cleanly.

**Two `message_end` events arrive rapidly (before Claude responds to the first):**
The first triggers `triggerResponse()`. The second also triggers `triggerResponse()`. Section 6 (Claude API Integration) designs the queuing — responses are sequentially queued per conversation. The second waits for the first to complete. Both get the full, up-to-date history when they execute.

**Group conversation: Agent A sends, Agent B sends, both trigger Claude:**
Two `message_end` events, two `triggerResponse()` calls. Sequential queue means Claude processes them in order. The second call's history includes Agent A's message, Agent B's message, and Claude's response to Agent A. Context is coherent.

**Empty message (message_start immediately followed by message_end, no appends):**
`pendingMessage.content` is an empty `strings.Builder`. `Content` on the assembled `Message` is `""`. Claude receives an empty message in history. This is fine — Claude can handle empty messages. If we wanted to skip them, a `if msg.Content == "" { return }` check in `onEvent()` would suffice, but it's unnecessary complexity for a case that shouldn't happen in normal operation.

---

### Files

```
internal/agent/
├── agent.go       # Agent struct (updated with convStates), Shutdown()
├── listen.go      # startListening(), listen(), onEvent()
├── state.go       # convState, Message, pendingMessage, appendMessage(), cleanStalePending()
├── history.go     # seedHistory(), assembleMessages()
├── notifier.go    # AgentNotifier interface, agentNotifier, noopNotifier
└── discovery.go   # discoveryLoop(), reconcileConversations()
```

---
