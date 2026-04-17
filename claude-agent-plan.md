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
- **Response triggering:** Always respond to every `message_end` from another agent. Self-messages hardcode-skipped (never sent to Claude). Sequential queuing per conversation via channel semaphore. `[NO_RESPONSE]` protocol dropped — unnecessary complexity for a resident agent. **This "always respond" policy is resident-only**; external agents adopt their own consumption policy within the active/passive/wake-up model defined in [`spec.md`](../spec.md) §1.4, and the service never forces an LLM invocation on any agent.
- **History seeding:** Lazy — history is empty on connect, seeded from S2 on first `message_end` from another agent. Bounded sliding window (last 50 messages).
- **Claude API integration:** Streaming responses via Anthropic Go SDK (`client.Messages.NewStreaming`), piped directly to S2 AppendSession (no HTTP self-call for writes). Response goroutine is a child of the listener goroutine, tracked via per-conversation WaitGroup.
- **Context management:** Sliding window over message history, system prompt + last N messages. Consecutive same-role messages merged with sender attribution for Anthropic API compliance.
- **Multi-conversation:** One listener goroutine per conversation, shared Claude API client with global concurrency semaphore (capacity 5). Per-conversation sequential queuing via channel semaphore.
- **Error handling:** Graceful degradation — Claude API failures produce a visible error message in the conversation. Mid-stream failures write `message_abort`. In-progress tracking via Postgres for crash recovery. 429 rate limits handled with exponential backoff and `Retry-After` respect.

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
   Body: {"message_id": "<fresh_uuidv7>", "content": "Hello!"}
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

**The cursor management concern is a non-issue.** The agent calls `cursors.GetDeliveryCursor()` and `cursors.UpdateDeliveryCursor()` — the same store interface the SSE handler uses. It's not reimplementation. The only "duplicated" logic is the `for readSession.Next()` loop and cursor update call — ~20 lines of straightforward code. (The resident agent never acks via the client-facing `Ack(...)` path — it's an in-process server component, not an external client.)

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
    startSeq, err := a.store.Cursors().GetDeliveryCursor(ctx, a.id, convID)
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

        // Update delivery_seq in memory (batched flush to Postgres every 5s by CursorStore)
        a.store.Cursors().UpdateDeliveryCursor(a.id, convID, event.SeqNum)

        // Dispatch to message accumulation and response system (Section 5)
        a.onEvent(convID, event)
    }

    // 4. Session ended — return the error for the caller to handle
    return readSession.Err()
}
```

**Properties of `listen()`:**
- **~20 lines.** No lifecycle management, no retry logic, no registration. Just the pure read loop.
- **Cursor tracking uses the existing CursorStore delivery path.** `GetDeliveryCursor()` reads from in-memory cache (falling back to Postgres). `UpdateDeliveryCursor()` writes to in-memory cache only (batched flush handles durability). Same code path as external SSE clients. The resident agent does not touch `ack_seq` — that cursor is reserved for external clients that explicitly call `POST /conversations/:cid/ack`.
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
The in-memory `delivery_seq` was updated on every event delivery but may not have been flushed to Postgres. On restart, `GetDeliveryCursor()` falls back to Postgres — at most 5 seconds stale. The agent re-processes up to ~150 events (5 sec × ~30 events/sec). Idempotent event processing in Section 5 handles this.

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

        // Trigger response (copies history under lock, spawns async goroutine — see Section 6)
        a.triggerResponse(ctx, convID, state)

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

**Self-message skip is hardcoded, not sent to Claude.** The agent's own responses appear on the same S2 stream. Without this check, every response triggers another Claude API call — wasting money and latency, and risking infinite echo loops. "Should I respond to myself?" is never a judgment call. Hardcode it.

**`message_append` with no matching `message_start`:** Silently dropped. This happens when the agent's cursor resumes mid-message after a crash. The partial message is unrecoverable — wait for the next complete message. Not an error.

**`message_abort`:** Delete the pending entry, no response. The sender intentionally abandoned the message.

---

### Lazy History Seeding

History is seeded on the first `message_end` from another agent, not on connect. This avoids paying the ReadRange cost for dormant conversations that never receive new messages.

```go
func (a *Agent) triggerResponse(ctx context.Context, convID uuid.UUID, state *convState) {
    // Lazy seed: first time we need to respond, load history
    if !state.seeded {
        a.seedHistory(convID, state)
        state.seeded = true
    }

    // Copy history under lock (state.mu is held by caller via onEvent's defer)
    snapshot := make([]Message, len(state.history))
    copy(snapshot, state.history)

    // Spawn response goroutine — see Section 6 for full respond() implementation
    state.responseWg.Add(1)
    go func() {
        defer state.responseWg.Done()
        a.respond(ctx, convID, snapshot)
    }()
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

**Always respond: the resident agent responds to every `message_end` from another agent.**

Self-messages are hardcode-skipped in `onEvent()` (`if msg.Sender == a.id { return }`). Every message from a different agent triggers `triggerResponse()`, which spawns an async response goroutine (see Section 6). Sequential queuing via channel semaphore ensures at most one Claude call per conversation at a time.

**Why always-respond, not [NO_RESPONSE] protocol:**

The `[NO_RESPONSE]` protocol (Claude decides whether to respond via sentinel text) was evaluated and rejected for the resident agent:

| Approach | Problem |
|---|---|
| `[NO_RESPONSE]` sentinel + buffer detection | Adds complexity to the streaming pipeline (must buffer initial tokens, string-match). Fragile. |
| Tool-call-based decision | Incompatible with streaming — tool call arguments arrive as JSON fragments, not streamable text. |
| Two-phase (decide, then stream) | Doubles API latency and cost for every incoming message. |
| **Always respond** | **Zero overhead. The resident agent's job IS to respond.** Self-echo skip prevents infinite loops. |

The resident agent has no reason to stay silent. It exists to demonstrate the platform by conversing. The self-echo hardcode skip is the only guard needed — it's trivially deterministic and prevents infinite loops.

**This "always respond" policy is resident-only.** External agents are first-class clients of the consumption model in [`spec.md`](../spec.md) §1.4 — active (open SSE tail and process every event), passive (member with no tail, messages accumulate on S2 without interrupting the agent's work), or wake-up (periodic `GET /agents/me/unread` + explicit `POST /conversations/{cid}/ack`). The service never invokes an LLM on an external agent's behalf and never forces an agent out of passive mode.

**Production enhancement (FUTURE.md):** For production agent frameworks with multiple AI agents in group conversations, intelligent response gating (mode-toggle, tool-based decisions, lightweight pre-filters) belongs in the client-side agent architecture. The AgentMail API supports this via the natural patterns of SSE connect/disconnect and NDJSON POST start/stop. Document in CLIENT.md as guidance for external agent developers.

---

### At-Least-Once Delivery — Dedup After Crash Recovery

When the agent restarts and `listen()` resumes from the Postgres `delivery_seq`, it may re-process events that were already handled before the crash. The cursor is at most 5 seconds stale (batched flush interval).

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

## 6. Claude API Integration & Token Piping

### The Problem

The agent has accumulated a conversation history (Section 5), decided to respond (every `message_end` from another agent), and needs to:
1. Call the Claude API with the conversation context
2. Stream Claude's response tokens in real-time to the S2 conversation stream
3. Handle failures at every stage — Claude API errors, S2 write failures, context cancellation from leave/shutdown
4. Ensure clean event ordering on the S2 stream (`message_abort` before `agent_left`)

This is the mechanical core: the bridge between Claude's streaming output and S2's AppendSession.

---

### Decision: Direct S2 AppendSession (Not HTTP Self-Call)

The agent writes directly to S2 via `s2Store.OpenAppendSession()` — the same SDK call the HTTP streaming write handler uses internally. Consistent with the read path decision (Section 4 chose direct S2 ReadSession over HTTP SSE to self).

**Why not HTTP self-call:** The original `spec.md` §1.8 sketch proposed piping Claude tokens through an NDJSON POST to the agent's own `/messages/stream` endpoint via `io.Pipe`. This was replaced because:
- HTTP serialization + loopback + middleware adds latency for zero benefit
- The agent is an internal component — going through HTTP to talk to yourself is a round-trip to nowhere
- Bootstrap ordering: the agent starts before the HTTP server, so the endpoint may not be serving yet
- The `in_progress_messages` tracking and abort handling are straightforward to add directly

---

### Decision: Always Respond (No [NO_RESPONSE] Protocol)

The resident agent responds to every `message_end` from another agent. The `[NO_RESPONSE]` protocol designed in Section 5 is **dropped** for the resident agent.

**Why dropped:**
- The resident agent is a demonstration bot. Its job is to respond to messages. There is no scenario where silence is the correct behavior.
- `[NO_RESPONSE]` was designed for group conversations with multiple AI agents where some messages aren't directed at you. At take-home scale (evaluator testing), this edge case doesn't justify the complexity.
- `[NO_RESPONSE]` detection with streaming requires either: (a) buffering initial tokens and string-matching a sentinel (fragile, adds latency), (b) tool-call-based decision (incompatible with streaming — tool call arguments arrive as JSON fragments, not streamable text), or (c) two-phase API calls (doubles latency and cost). All three are worse than not doing it.
- Self-messages are already hardcode-skipped in `onEvent()` (Section 5). Infinite echo loops are impossible.

**Production enhancement (FUTURE.md):** For a production agent framework supporting external agents with human-in-the-loop workflows, intelligent response gating belongs in the client-side agent architecture (mode-toggle between communication and reasoning), not in the server-side resident agent. Document this as a CLIENT.md guidance pattern.

---

### Response Goroutine Lifecycle

The response goroutine is a **child of the listener goroutine**, tracked via a per-conversation `sync.WaitGroup`. This ensures:
- The listener goroutine doesn't block waiting for Claude (continues reading events)
- The deferred cleanup in `startListening()` waits for in-progress responses before signaling `close(done)`
- The leave handler's event ordering guarantee (`message_abort` → `agent_left`) is maintained

**`convState` additions:**

```go
type convState struct {
    mu          sync.Mutex
    pending     map[string]*pendingMessage
    history     []Message
    seeded      bool
    lastSeededSeq uint64

    responseSem chan struct{}   // capacity 1 — at most one in-flight Claude call
    responseWg  sync.WaitGroup // tracks in-progress response goroutine
}
```

**Initialization (in `getOrCreateConvState()`):**

```go
state = &convState{
    pending:     make(map[string]*pendingMessage),
    responseSem: make(chan struct{}, 1),
}
```

**Updated deferred cleanup in `startListening()`:**

```go
defer func() {
    // 0. Wait for any in-progress response to finish
    //    (writes message_abort if needed before exiting)
    state := a.getOrCreateConvState(convID)
    state.responseWg.Wait()

    // 1. Flush cursor
    a.store.Cursors().FlushOne(context.Background(), a.id, convID)
    // 2. Signal completion — unblocks leave handler
    close(done)
    // 3-4. ConnRegistry deregister, listeners map cleanup (unchanged)
}()
```

**Why `responseWg.Wait()` before cursor flush:** The response goroutine may write events to S2 (message_abort). The cursor should reflect the latest state after those writes. Ordering: wait for response → flush cursor → signal done.

---

### Sequential Queuing — Channel Semaphore

At most one Claude response is in-flight per conversation at any time. This prevents:
- Two concurrent Claude calls with overlapping history (incoherent, wasteful)
- Two concurrent AppendSessions writing interleaved response tokens from the same agent

**Mechanism:** `responseSem chan struct{}` with capacity 1. The response goroutine acquires the semaphore before starting, releases when done. The second response waits (context-aware) for the first to complete.

```go
select {
case state.responseSem <- struct{}{}:
    // acquired — proceed with Claude call
    defer func() { <-state.responseSem }()
case <-ctx.Done():
    // leave or shutdown — don't wait, exit immediately
    return
}
```

**Why channel semaphore, not `sync.Mutex`:** Context cancellation doesn't interrupt `sync.Mutex.Lock()`. A goroutine blocked on mutex acquisition during a leave would hang until the first response finishes. The channel `select` respects `ctx.Done()` — the blocked goroutine exits immediately on leave/shutdown.

**Coalescing (FUTURE.md):** If multiple messages arrive while a response is in-flight, they all queue independently. Each triggers a separate Claude call when it acquires the semaphore. At production scale, a "latest wins" flag could coalesce rapid-fire messages into fewer Claude calls. Not needed at take-home scale.

---

### The `triggerResponse()` Function — Complete Implementation

`triggerResponse()` is called from `onEvent()` under `state.mu`. It copies the history snapshot under the lock, then spawns an async goroutine for the Claude call.

```go
func (a *Agent) triggerResponse(ctx context.Context, convID uuid.UUID, state *convState) {
    // Lazy seed: first time we need to respond, load history
    if !state.seeded {
        a.seedHistory(convID, state)
        state.seeded = true
    }

    // Copy history under lock (state.mu is held by caller)
    snapshot := make([]Message, len(state.history))
    copy(snapshot, state.history)

    // Spawn response goroutine (child of listener)
    state.responseWg.Add(1)
    go func() {
        defer state.responseWg.Done()
        a.respond(ctx, convID, snapshot)
    }()
}
```

**Why copy history under lock:** After `triggerResponse()` returns, `state.mu` is released. The listener goroutine continues processing events, modifying `state.history` via `appendMessage()`. The response goroutine must work with a stable snapshot — not a slice that's being mutated concurrently.

**Why pass `ctx` (the `listenerCtx`):** The response goroutine must respect the same cancellation as the listener. On leave/shutdown, `listenerCtx` cancellation propagates to both the Claude API call and the S2 AppendSession.

---

### The `respond()` Function — The Token Pipeline

```go
func (a *Agent) respond(ctx context.Context, convID uuid.UUID, history []Message) {
    // 1. Acquire per-conversation semaphore (context-aware)
    select {
    case a.getOrCreateConvState(convID).responseSem <- struct{}{}:
        defer func() { <-a.getOrCreateConvState(convID).responseSem }()
    case <-ctx.Done():
        return
    }

    // 2. Build Claude messages array
    claudeMsgs := a.buildClaudeMessages(history)

    // 3. Call Claude streaming API (with one retry on transient failure)
    var stream *anthropic.MessageStream
    var err error
    for attempt := 0; attempt < 2; attempt++ {
        stream = a.claude.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
            Model:     anthropic.ModelClaudeSonnet4_20250514,
            MaxTokens: 4096,
            System:    []anthropic.TextBlockParam{
                anthropic.NewTextBlockParam("You are a helpful AI assistant on the AgentMail platform. You are participating in a conversation with one or more AI agents. Respond naturally and concisely."),
            },
            Messages: claudeMsgs,
        })
        if stream != nil {
            break
        }
        if attempt == 0 && ctx.Err() == nil {
            time.Sleep(500 * time.Millisecond)
        }
    }
    if stream == nil || ctx.Err() != nil {
        a.sendErrorMessage(ctx, convID, "I encountered an error connecting to my language model. Please try again.")
        return
    }
    defer stream.Close()

    // 4. Generate the message_id ONCE for this response, then flow through the
    //    same in_progress + dedup gates as the HTTP streaming write handler.
    //    This is what makes the resident agent's writes indistinguishable from
    //    an external agent's writes at the storage layer — there is no special
    //    path for in-process writers. A server crash mid-response produces the
    //    same recovery-sweep behavior; a (hypothetical) restart-and-retry of
    //    the same msgID would hit the dedup table and be rejected with the same
    //    semantics. One write path, one correctness argument.
    msgID := uuid.Must(uuid.NewV7())
    claimed, err := a.store.InProgress().Claim(ctx, msgID, convID, a.id, fmt.Sprintf("conversations/%s", convID))
    if err != nil || !claimed {
        // Extremely unlikely (agent never reuses a msgID), but if it ever fires
        // the response is effectively a no-op — another writer owns this msgID.
        a.sendErrorMessage(ctx, convID, "I encountered an internal error preparing my response.")
        return
    }

    // 5. Open S2 AppendSession.
    session, err := a.s2.OpenAppendSession(ctx, convID)
    if err != nil {
        // Clean up the claim before bailing.
        a.abortAndRecord(ctx, convID, msgID, "s2_unavailable")
        a.sendErrorMessage(ctx, convID, "I encountered an error preparing my response. Please try again.")
        return
    }
    defer session.Close()

    // 6. Write message_start — wrapped with the same 2 s Submit timeout as the
    //    HTTP streaming write handler. See s2-architecture-plan.md §5 for the
    //    AppendSessionWrapper contract.
    if err := session.Submit(ctx, Event{Type: "message_start", Body: MessageStartPayload{
        MessageID: msgID, SenderID: a.id,
    }}); err != nil {
        a.abortAndRecord(ctx, convID, msgID, "slow_writer")
        return
    }

    // 7. Stream tokens: Claude → S2 (every Submit carries the 2 s timeout).
    aborted := false
    abortReason := ""
    for stream.Next() {
        if ctx.Err() != nil {
            aborted = true
            abortReason = "agent_left"
            break
        }

        event := stream.Current()
        switch delta := event.AsAny().(type) {
        case anthropic.ContentBlockDeltaEvent:
            switch textDelta := delta.Delta.AsAny().(type) {
            case anthropic.TextDelta:
                if textDelta.Text != "" {
                    if err := session.Submit(ctx, Event{Type: "message_append", Body: MessageAppendPayload{
                        MessageID: msgID, Content: textDelta.Text,
                    }}); err != nil {
                        aborted = true
                        abortReason = "slow_writer"
                    }
                }
            }
        }
        if aborted {
            break
        }
    }

    // 8. Error / abort path — unified with the HTTP handler's abort helper.
    if err := stream.Err(); err != nil {
        aborted = true
        if abortReason == "" {
            abortReason = "claude_error"
        }
    }
    if aborted {
        a.abortAndRecord(context.Background(), convID, msgID, abortReason)
        return
    }

    // 9. Write message_end, capture seq_end, record terminal outcome.
    endSeq, err := session.SubmitAndWait(ctx, Event{Type: "message_end", Body: MessageEndPayload{
        MessageID: msgID,
    }})
    if err != nil {
        a.abortAndRecord(context.Background(), convID, msgID, "slow_writer")
        return
    }
    a.store.Tx(context.Background(), func(tx store.Tx) error {
        if err := a.store.Dedup().InsertComplete(tx, convID, msgID, startSeq, endSeq); err != nil {
            return err
        }
        return a.store.InProgress().Delete(tx, msgID)
    })
}

// abortAndRecord: shared terminal-abort helper. Identical semantics to the HTTP
// streaming-write handler's abort path. Writes message_abort via Unary append
// (bypassing the AppendSession which may be in an error state), then records
// messages_dedup 'aborted' (ON CONFLICT DO NOTHING) and deletes in_progress_messages
// in one tx. See spec.md "Shared abort helper" and sql-metadata-plan.md §12.
func (a *Agent) abortAndRecord(ctx context.Context, convID, msgID uuid.UUID, reason string) {
    a.s2.AppendEvents(ctx, convID, []Event{{
        Type: "message_abort",
        Body: MessageAbortPayload{MessageID: msgID, Reason: reason},
    }})
    a.store.Tx(ctx, func(tx store.Tx) error {
        _ = a.store.Dedup().InsertAborted(tx, convID, msgID)
        return a.store.InProgress().Delete(tx, msgID)
    })
}
```

### Token Flow Diagram

```text
┌─────────────┐     stream.Next()      ┌─────────────────┐    Submit()     ┌─────────┐
│  Claude API  │ ─────────────────────→ │  respond()       │ ─────────────→ │   S2    │
│  (streaming) │  ContentBlockDelta     │  goroutine       │  AppendSession │ (stream │
│              │  TextDelta.Text        │                  │                │per conv)│
└─────────────┘                         └─────────────────┘                └─────────┘
                                               │
                                               │ on error / ctx cancel
                                               ▼
                                        message_abort
                                        (unary append)
```

**Properties of the token pipeline:**
- **Zero buffering.** Each `TextDelta` goes directly to `session.Submit()`. The reader sees tokens in real-time (~40ms S2 ack latency after each submit).
- **Context-aware at every step.** `ctx.Err()` check inside the loop catches leave/shutdown between tokens. The Claude API call itself respects `ctx` cancellation.
- **Abort via unary, not session.** If the AppendSession is in an error state (S2 failure), `message_abort` goes through a fresh unary `AppendEvents()` call with `context.Background()` (best-effort — must try even if the listener's context is canceled).
- **In-progress tracking wraps the lifecycle.** Postgres insert before `message_start`, delete after `message_end` or `message_abort`. If the server crashes mid-response, the recovery sweep on startup cleans up.

---

### Message History Formatting for Claude API

The Anthropic API requires messages to alternate between `user` and `assistant` roles, starting with `user`. In a group conversation, multiple agents' messages all map to `user` from the resident agent's perspective, creating consecutive same-role messages that the API rejects.

**Solution:** Merge consecutive same-role messages with sender attribution.

```go
func (a *Agent) buildClaudeMessages(history []Message) []anthropic.MessageParam {
    if len(history) == 0 {
        return nil
    }

    var msgs []anthropic.MessageParam
    var currentRole anthropic.MessageParamRole
    var currentContent strings.Builder

    for _, msg := range history {
        role := anthropic.MessageParamRoleUser
        if msg.Sender == a.id {
            role = anthropic.MessageParamRoleAssistant
        }

        if role == currentRole && currentContent.Len() > 0 {
            // Same role as previous — merge with sender attribution
            currentContent.WriteString("\n\n")
            if role == anthropic.MessageParamRoleUser {
                currentContent.WriteString(fmt.Sprintf("[%s]: ", msg.Sender))
            }
            currentContent.WriteString(msg.Content)
        } else {
            // Role changed — flush previous message
            if currentContent.Len() > 0 {
                msgs = append(msgs, anthropic.MessageParam{
                    Role:    currentRole,
                    Content: []anthropic.ContentBlockParam{anthropic.NewTextBlock(currentContent.String())},
                })
            }
            currentRole = role
            currentContent.Reset()
            if role == anthropic.MessageParamRoleUser && len(history) > 1 {
                currentContent.WriteString(fmt.Sprintf("[%s]: ", msg.Sender))
            }
            currentContent.WriteString(msg.Content)
        }
    }

    // Flush last message
    if currentContent.Len() > 0 {
        msgs = append(msgs, anthropic.MessageParam{
            Role:    currentRole,
            Content: []anthropic.ContentBlockParam{anthropic.NewTextBlock(currentContent.String())},
        })
    }

    // Anthropic API requires first message to be "user"
    // If history starts with our own message (assistant), prepend a synthetic user message
    if len(msgs) > 0 && msgs[0].Role == anthropic.MessageParamRoleAssistant {
        msgs = append([]anthropic.MessageParam{{
            Role:    anthropic.MessageParamRoleUser,
            Content: []anthropic.ContentBlockParam{anthropic.NewTextBlock("[conversation history begins with your previous response]")},
        }}, msgs...)
    }

    return msgs
}
```

**Why sender attribution (`[agent-id]: content`):** In a group conversation with agents B and C, the resident agent sees both as "user." Without attribution, Claude can't distinguish who said what. The `[agent-id]` prefix gives Claude enough context to respond coherently to multi-party conversations.

**Why only attribute user messages, not assistant messages:** The assistant messages are always from the resident agent itself. There's only one assistant. No ambiguity.

---

### Error Message Helper

When Claude API or S2 fails before any streaming has started, the agent sends a visible error message to the conversation so the evaluator knows the agent is alive but encountered an issue.

```go
func (a *Agent) sendErrorMessage(ctx context.Context, convID uuid.UUID, content string) {
    msgID := uuid.NewV7()
    a.s2.AppendEvents(ctx, convID, []Event{
        {Type: "message_start", Body: MessageStartPayload{MessageID: msgID, SenderID: a.id}},
        {Type: "message_append", Body: MessageAppendPayload{MessageID: msgID, Content: content}},
        {Type: "message_end", Body: MessageEndPayload{MessageID: msgID}},
    })
}
```

**Unary append (atomic batch):** The error message is a complete message — three records in one batch. Either all three land or none do. No in-progress tracking needed (no streaming, no crash window).

**Why this path does NOT go through the `messages_dedup` gate:** Idempotency is about protecting against duplicate *client retries* of the same logical write. `sendErrorMessage` is an in-process, non-retryable diagnostic that generates a fresh UUIDv7 on every call and is invoked at most once per failure. There is no retry loop and no second actor with the same `msgID`. Adding the dedup bookkeeping would be ceremony without a correctness return. The one-line invariant: dedup is applied on every path that an external caller could trigger twice with the same `message_id`; `sendErrorMessage` is not such a path.

**Best-effort:** If S2 is also down, the error message fails silently. The evaluator sees no response. This is acceptable — if both Claude and S2 are unreachable simultaneously, there's nothing the agent can do. The next message from the evaluator will trigger another attempt.

---

### Claude API Parameters

| Parameter | Value | Rationale |
|---|---|---|
| **Model** | `claude-sonnet-4-20250514` | Fast time-to-first-token (~500ms), good quality for conversation, cost-effective |
| **max_tokens** | 4096 | Generous for conversational responses. Not so large that a runaway response costs a fortune. |
| **temperature** | Default (1.0) | No reason to override for general conversation |
| **System prompt** | See below | Concise, establishes role, works in 1:1 and group |

**System prompt:**
```
You are a helpful AI assistant on the AgentMail platform. You are participating in a conversation with one or more AI agents. Respond naturally and concisely.
```

Short. No personality gimmicks. No constraints that would confuse the evaluator. The evaluator wants to verify the agent works — not that it has a clever persona.

---

### Edge Cases

**Response goroutine outlives the listener (leave during Claude generation):**
`listenerCtx` is canceled → `ctx.Err()` check inside the token loop fires → abort path writes `message_abort` via unary append with `context.Background()` → response goroutine exits → `responseWg.Done()` → `startListening()` deferred cleanup proceeds → `close(done)` → leave handler writes `agent_left`. Ordering on the stream: `message_abort` → `agent_left`. Correct.

**Claude API returns empty response (no content blocks):**
`stream.Next()` returns false immediately after start. No `TextDelta` events. The agent has written `message_start` but no `message_append`. `stream.Err()` is nil (clean completion with empty output). Write `message_end` — the result is an empty message on the stream. Harmless.

**S2 AppendSession fails after message_start but before any message_append:**
The `session.Submit()` error propagates (detected via the session's internal error state). The loop exits. Abort path writes `message_abort` via unary. The stream shows: `message_start` → `message_abort`. Readers see a started-then-aborted message. Correct.

**Two message_end events arrive while Claude is responding to the first:**
Second `triggerResponse()` spawns a second goroutine. Second goroutine blocks on `responseSem` (channel semaphore). When first response completes, second goroutine acquires the semaphore and runs with the now-updated history (which includes all messages that arrived during the first response). Context-aware: if leave happens while waiting, the goroutine exits immediately without acquiring the semaphore.

**Server crash mid-response:**
`in_progress_messages` table has the message ID. Recovery sweep on next startup writes `message_abort` for the unterminated message. Same mechanism as the HTTP streaming write handler. No special-casing for the resident agent.

**Claude API rate limited (429):**
See Section 7 for 429-specific handling with exponential backoff and `Retry-After` header respect.

---

### Files

```
internal/agent/
├── agent.go       # Agent struct (updated with convStates, claudeSem), Shutdown()
├── listen.go      # startListening(), listen(), onEvent()
├── respond.go     # triggerResponse(), respond(), callClaudeStreaming(), buildClaudeMessages(), sendErrorMessage()
├── state.go       # convState (updated with responseSem, responseWg), Message, pendingMessage
├── history.go     # seedHistory(), assembleMessages()
├── notifier.go    # AgentNotifier interface, agentNotifier, noopNotifier
└── discovery.go   # discoveryLoop(), reconcileConversations()
```

---

## 7. Rate Limiting, Error Handling & Recovery

### The Problem

The agent may be in multiple conversations simultaneously. Each incoming `message_end` triggers a Claude API call. Without global throttling, a burst of messages across many conversations produces a burst of concurrent Claude API calls — hitting rate limits (429), wasting money on rejected requests, and degrading the experience for all conversations.

Additionally, Section 6 designed error handling for the happy-path failures (Claude down, S2 down, context canceled). This section addresses the operational concerns: rate limit management, retry strategy differentiation, and server restart recovery.

---

### Global Claude API Concurrency Semaphore

**The problem:** Per-conversation semaphores (Section 6) ensure at most one Claude call per conversation. But across N conversations, up to N concurrent calls can hit the Claude API simultaneously. Anthropic rate limits (RPM/TPM) apply globally to the API key.

**Decision:** Global semaphore with capacity 5.

```go
type Agent struct {
    // ... existing fields ...
    claudeSem chan struct{} // capacity 5 — global Claude API concurrency limit
}
```

**Initialization:**

```go
agent := &Agent{
    claudeSem: make(chan struct{}, 5),
    // ... other fields ...
}
```

**Acquisition in `respond()` — after per-conversation semaphore, before Claude call:**

```go
// Per-conversation semaphore (at most one response per conversation)
select {
case state.responseSem <- struct{}{}:
    defer func() { <-state.responseSem }()
case <-ctx.Done():
    return
}

// Global Claude API semaphore (at most 5 concurrent Claude calls total)
select {
case a.claudeSem <- struct{}{}:
    defer func() { <-a.claudeSem }()
case <-ctx.Done():
    return
}
```

**Why capacity 5:** Anthropic's standard tier allows ~50 RPM for Sonnet. At 5 concurrent calls with average response time of ~5 seconds, that's ~60 calls/minute — close to the limit but within it. The semaphore prevents bursts, and the 429 retry logic (below) handles momentary overages. At take-home scale (1-5 active conversations), the semaphore never blocks.

**Why two semaphores (per-conversation + global):** Different concerns. The per-conversation semaphore prevents incoherent interleaved responses within a conversation (correctness). The global semaphore prevents API rate limit exhaustion across conversations (operational). Both are context-aware — leave/shutdown exits immediately from either wait.

**Ordering (per-conversation THEN global):** A goroutine holds the per-conversation semaphore while waiting for the global semaphore. This means the conversation is "reserved" — no other response for that conversation can start. If we reversed the order (global then per-conversation), a goroutine could hold a global slot while waiting for its conversation to become free — wasting global capacity.

---

### 429-Specific Retry with Exponential Backoff

Section 6's `respond()` does a flat "one retry after 500ms" for all errors. This is insufficient for 429 (rate limited) — retrying immediately makes it worse. The Anthropic API returns a `Retry-After` header on 429 responses.

**Decision:** Differentiated retry strategy.

```go
func (a *Agent) callClaudeStreaming(ctx context.Context, msgs []anthropic.MessageParam) (*anthropic.MessageStream, error) {
    var lastErr error

    for attempt := 0; attempt < 3; attempt++ {
        if attempt > 0 && ctx.Err() != nil {
            return nil, ctx.Err()
        }

        stream := a.claude.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
            Model:     anthropic.ModelClaudeSonnet4_20250514,
            MaxTokens: 4096,
            System: []anthropic.TextBlockParam{
                anthropic.NewTextBlockParam("You are a helpful AI assistant on the AgentMail platform. You are participating in a conversation with one or more AI agents. Respond naturally and concisely."),
            },
            Messages: msgs,
        })

        // Success
        if stream != nil && stream.Err() == nil {
            return stream, nil
        }

        lastErr = stream.Err()

        // Determine backoff based on error type
        var backoff time.Duration
        if isRateLimited(lastErr) {
            // 429: respect Retry-After or default to exponential backoff
            backoff = retryAfterOrDefault(lastErr, time.Duration(attempt+1)*2*time.Second)
        } else {
            // Transient error (500, network): short backoff
            backoff = time.Duration(attempt+1) * 500 * time.Millisecond
        }

        select {
        case <-time.After(backoff):
            continue
        case <-ctx.Done():
            return nil, ctx.Err()
        }
    }

    return nil, fmt.Errorf("claude API failed after 3 attempts: %w", lastErr)
}
```

**`isRateLimited()` checks for HTTP 429 in the error chain.** The Anthropic Go SDK wraps HTTP errors — inspect the error for status code.

**`retryAfterOrDefault()` parses the `Retry-After` header** from the error response if available. If the header isn't accessible through the SDK's error type, defaults to exponential backoff: 2s, 4s, 6s.

**Why 3 attempts, not 2:** Rate limits are bursty. The first retry may still hit the limit. Three attempts with increasing backoff gives the rate limit window time to reset. At 2s + 4s + 6s = 12 seconds total worst-case wait — acceptable for a conversational response.

**Updated `respond()` uses `callClaudeStreaming()`:**

The stream creation in Section 6's `respond()` is replaced with:

```go
// 3. Call Claude streaming API (with retry and rate limit handling)
stream, err := a.callClaudeStreaming(ctx, claudeMsgs)
if err != nil || ctx.Err() != nil {
    a.sendErrorMessage(ctx, convID, "I encountered an error connecting to my language model. Please try again.")
    return
}
defer stream.Close()
```

---

### Server Restart Recovery — Complete Agent Bootstrap

On server restart, the agent must recover to a fully functional state. The recovery is composed of mechanisms already designed in previous sections, listed here for completeness:

**Bootstrap sequence (in order):**

```
1. Parse RESIDENT_AGENT_ID from environment
   → If missing: skip agent bootstrap, server runs without resident agent (Section 1)

2. EnsureExists in Postgres (idempotent insert)
   → Agent identity is stable across restarts (Section 1)

3. Recovery sweep: in_progress_messages table
   → Write message_abort for any unterminated messages from previous run
   → Delete recovered rows
   → Same mechanism as HTTP handler recovery (s2-architecture-plan.md §10)

4. Reconcile conversations: ListConversations(agentID)
   → Start a listener for each conversation the agent is a member of
   → Retry up to 3 times if Postgres is transiently unavailable (Section 3)

5. Each listener resolves its cursor from Postgres (at most 5s stale)
   → Opens S2 ReadSession from cursor position
   → At-least-once delivery — may re-process events since last flush (Section 4)
   → lastSeededSeq dedup prevents duplicate history entries (Section 5)

6. Activate invite notification channel
   → New invites after this point flow through Go channel (Section 3)

7. HTTP server starts accepting traffic
   → No invites can arrive before step 6 — no gap (Section 3)
```

**No new mechanisms needed.** Every recovery step was designed in its respective section. This sequence documents the order and confirms there are no gaps between sections.

**Recovery time estimate:** Steps 1-2: <100ms. Step 3: <500ms (one Postgres query + N unary S2 appends). Step 4: <1s (one Postgres query + N S2 ReadSession opens, sequential). Steps 5-7: immediate. **Total: <2 seconds** from process start to fully functional agent.

---

### Edge Cases

**All conversations fire simultaneously after a period of inactivity:**
Global semaphore (capacity 5) queues excess Claude calls. Conversations beyond the first 5 wait (context-aware). Each waiter acquires the semaphore as earlier calls complete. The experience for waiting conversations is delayed response (seconds, not minutes). At take-home scale, this scenario requires 6+ active conversations messaging simultaneously — unlikely.

**Claude API is down for an extended period (minutes):**
Each conversation's response attempt retries 3 times (up to 12 seconds), then sends an error message. Subsequent messages from the evaluator trigger fresh attempts. The agent doesn't enter a "broken" state — each incoming message is an independent attempt. Recovery is automatic when Claude comes back.

**Server crashes during agent bootstrap (between steps 3 and 4):**
Recovery sweep (step 3) may have written some `message_abort` events but not all. On next restart, the recovery sweep runs again. Rows that were already cleaned up are gone. Rows that weren't are retried. `message_abort` for a message that already has one is harmless — readers ignore duplicate aborts for the same `message_id`. Idempotent.

**Agent is in a conversation but the S2 stream was trimmed (28-day retention):**
The listener's `listen()` function opens a ReadSession from `delivery_seq`. If the cursor points to trimmed data, S2 returns an error. The retry loop in `startListening()` catches this. On the next attempt, `GetDeliveryCursor()` returns the stale value, but the S2 SDK's `ReadSession` with a sequence number beyond the trim point should start from the stream head (earliest available record). The agent catches up on whatever history remains. Log a warning for observability.

---

### Summary of All Error Handling Across the Agent

| Failure | Where Handled | Behavior |
|---|---|---|
| **Claude API transient error (500, network)** | Section 6 `respond()` via `callClaudeStreaming()` | 3 retries, 500ms/1s/1.5s backoff, then `sendErrorMessage()` |
| **Claude API rate limited (429)** | Section 7 `callClaudeStreaming()` | 3 retries, respect `Retry-After` or 2s/4s/6s backoff, then `sendErrorMessage()` |
| **Claude stream fails mid-generation** | Section 6 `respond()` | `message_abort` via unary append, reason `"claude_error"` |
| **S2 AppendSession fails mid-response** | Section 6 `respond()` | Cancel Claude stream, `message_abort` via unary (best-effort) |
| **Claude emits a token >1 MiB (S2 record cap)** | Section 6 `respond()` | Same path as any S2 AppendSession failure: cancel Claude stream, `message_abort` via unary with reason `"content_too_large"` per the registry in [event-model-plan.md](event-model-plan.md) §3. See [http-api-layer-plan.md](http-api-layer-plan.md) §6.4 for the broader error mapping. In practice Claude tokens are 1–4 bytes; this is a defensive case, not a hot path. |
| **S2 unary append fails (error message, abort)** | Section 6 `sendErrorMessage()`, abort path | Silent failure — if S2 is fully down, nothing can be written. `in_progress_messages` catches on restart. |
| **Context canceled (leave)** | Section 6 `respond()` loop | `message_abort` with reason `"agent_left"`, then goroutine exits |
| **Context canceled (shutdown)** | Section 4 `Shutdown()` | Cancel all listeners → `responseWg.Wait()` → cursor flush → exit |
| **Server crash mid-response** | Section 7 bootstrap step 3 | Recovery sweep writes `message_abort` from `in_progress_messages` table |
| **Postgres unavailable on startup** | Section 3 `reconcileConversations()` | 3 retries with 1s/2s/3s backoff, then start with no listeners |
| **S2 stream not found (new conversation)** | Section 3 `startListening()` | Retry with exponential backoff up to 30s. Self-healing when first S2 write creates stream. |
| **S2 ReadSession fails mid-stream** | Section 4 `startListening()` retry loop | Retry with exponential backoff. Cursor preserves position. At-least-once delivery. |
| **Stale cursor (28-day retention trim)** | Section 7 edge cases | Start from stream head. Log warning. |

---
