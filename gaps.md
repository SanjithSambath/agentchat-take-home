Audit: Exhaustively Designed vs. Sketched

  Exhaustively Designed (plan-document depth, ready to code from)

  ┌──────────────────────────────────────────────────────┬──────────────────────────────┬─────────┐
  │                      Component                       │            Where             │ Verdict │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ S2 stream topology, basin config, retention          │ s2-architecture-plan.md §1-3 │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Record format & event types                          │ s2-architecture-plan.md §4   │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Append strategy (Unary vs AppendSession vs Producer) │ s2-architecture-plan.md §5   │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ ReadSession management                               │ s2-architecture-plan.md §6   │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Stream creation (auto-create)                        │ s2-architecture-plan.md §7   │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Concurrent message interleaving                      │ s2-architecture-plan.md §8   │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Leave-mid-write flow                                 │ s2-architecture-plan.md §9   │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ In-progress message tracking & recovery              │ s2-architecture-plan.md §10  │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ S2 client wrapper interface                          │ s2-architecture-plan.md §11  │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ S2 error handling & retry                            │ s2-architecture-plan.md §12  │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ PostgreSQL schema (4 tables)                         │ sql-metadata-plan.md §5      │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Connection pooling (pgxpool config)                  │ sql-metadata-plan.md §6      │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Cursor two-tier architecture                         │ sql-metadata-plan.md §7      │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Membership caching (LRU)                             │ sql-metadata-plan.md §8      │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Concurrency & locking (5 race conditions)            │ sql-metadata-plan.md §9      │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Connection registry                                  │ sql-metadata-plan.md §10     │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Store interface (Go)                                 │ sql-metadata-plan.md §11     │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ sqlc queries                                         │ sql-metadata-plan.md §12     │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Write path — NDJSON streaming POST                   │ spec.md §1.3                 │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Write path — complete message POST                   │ spec.md §1.3                 │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Read path — SSE                                      │ spec.md §1.4                 │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Read path — history GET                              │ spec.md §1.4                 │ Done    │
  ├──────────────────────────────────────────────────────┼──────────────────────────────┼─────────┤
  │ Transport decision (NDJSON vs WS vs per-token)       │ spec.md Phase 2              │ Done    │
  └──────────────────────────────────────────────────────┴──────────────────────────────┴─────────┘

  NOT Exhaustively Designed (sketched, bullet-pointed, or missing entirely)

  Here's what's either thin or absent. These are the remaining design hills:

  ---
  Gap 1: Claude-Powered Agent — spec.md §1.8

  This is a half-page sketch where everything else got 10+ pages. For a component the evaluators will directly interact with, it's
  dangerously under-designed. What's missing:

  Agent discovery & bootstrapping:
  - How does the evaluator find your agent? The spec mentions a "lobby" — is that a well-known conversation ID? An agent ID printed at
   startup? A discovery endpoint?
  - If the evaluator invites your agent to a NEW conversation, how does the agent discover it was invited? It can't poll GET
  /conversations forever. Does it need a meta-listener?

  SSE listener architecture:
  - One goroutine per conversation — but how does the agent learn about new conversations? The ListConversations endpoint only returns
   current memberships. Does the agent poll it? At what interval?
  - What happens to the SSE goroutine when the agent is removed from a conversation?

  Message accumulation & response triggering:
  - The agent listens to raw SSE events. It sees interleaved message_start, message_append, message_end events. It needs to
  demultiplex, accumulate complete messages, and decide when to respond.
  - When does the agent respond? On every message_end? Only when the message is from someone else? What if three messages arrive in
  rapid succession — does it respond to each, or batch them?
  - What about its OWN messages echoing back on the SSE stream? It needs to ignore messages where sender_id == self.

  Claude API integration mechanics:
  - Exact system prompt.
  - How conversation history is formatted for the Claude API (messages array structure).
  - Context window management — when history exceeds the limit, what's the truncation strategy? Keep system prompt + last N messages?
  Summarize?
  - Model choice and parameters (temperature, max_tokens).

  Token piping — the io.Pipe bridge:
  - The spec mentions "tokens flow from Claude through to S2 with no buffering" via io.Pipe. The actual Go code for connecting
  anthropic.StreamMessage() output to an NDJSON POST request body is non-trivial and not designed.
  - How does the agent handle Claude API streaming errors mid-generation?

  Rate limiting & queuing:
  - Multiple conversations are active. Claude API has rate limits. What's the queuing strategy? Per-conversation response queue?
  Global semaphore?
  - What if the agent is in 50 conversations and all get a message at once?

  Error behavior:
  - Claude API returns 429 (rate limited) — retry with backoff? Send error message to conversation?
  - Claude API returns 500 — how does the agent communicate failure to the conversation?
  - The streaming write to AgentMail fails mid-response — what happens?

---- 
GAP 1 SUBSTEPS START


 The README requires:

  ▎ "at least one Claude-powered agent running on it that we can converse with. We will also evaluate the service by connecting our
  ▎ own agents as clients — both to converse with your agents and to converse with each other through your service."

  And from the "Stand-Out Strategy" in your spec:

  ▎ "deploy two Claude-powered agents with different personalities that can converse with each other AND with external agents"

  Here's how I'd approach the design for the Claude agent. This is a complex component with several distinct sub-problems. I'll lay
  them out, and you tell me which order you want to grill through, or if you want to restructure.

  Sub-Problems Within the Claude Agent Design

  A. Agent Discovery & Bootstrapping

  How does the evaluator find your agent? How does the agent register and make itself available? This is the front door — if this is
  unclear, the evaluator is stuck.

  Options: well-known agent ID persisted across restarts, a /agent discovery endpoint, agent ID printed to logs, etc.

  B. Conversation Discovery (the polling problem)

  The agent is in N conversations. Someone invites it to conversation N+1. How does the agent learn about this? There's no push
  notification in your API — SSE is per-conversation, not per-agent. This is a real architectural gap.

  C. SSE Listener Architecture

  One goroutine per conversation — lifecycle management, cleanup, error handling, reconnection. The internal machinery of how the
  agent maintains N concurrent SSE streams.

  D. Message Demultiplexing & Response Triggering

  Raw SSE events arrive interleaved. The agent needs to accumulate complete messages, ignore its own echoes, decide when to respond,
  and handle rapid-fire messages.

  E. Claude API Integration & Token Piping

  System prompt, message history formatting, context window management, the io.Pipe bridge from Claude streaming output to NDJSON
  POST. The mechanical core.

  F. Rate Limiting, Queuing & Multi-Conversation Orchestration

  The agent is in many conversations. Claude API has rate limits. How does the agent prioritize, queue, and throttle?

  G. Error Handling & Edge Cases

  Claude API failures, streaming write failures, agent gets removed mid-response, server restarts and the agent needs to recover.

  H. The Two-Agent "Stand-Out" (optional, but spec mentions it)

  Teacher + student agents, personality differentiation, self-conversation. Is this still in scope or are you cutting to one agent?


GAP 1 SUBSTEPS END
---- 


  ---
  Gap 2: HTTP API Layer — router, middleware, error format, request/response contracts ✅ RESOLVED
  → See http-api-layer-plan.md (full design) and spec.md §1.9 (executive summary)

  The spec lists endpoints and shows example request/response pairs. But the API layer as a system component is not designed:

  Error response format:
  - Is there a standard error envelope? {"error": {"code": "not_found", "message": "..."}}?
  - What HTTP status codes map to what conditions? (You've mentioned some — 403, 404 — but there's no comprehensive mapping.)

  Middleware chain:
  - What's the exact order? Logging → request ID → timeout → agent ID extraction → handler?
  - Agent validation middleware: does it run on ALL routes? Or only on routes that need it (everything except POST /agents and GET
  /health)?

  Request/response types (Go structs):
  - Every endpoint needs request and response types. These are mentioned implicitly in the API examples but never collected as Go
  types.
  - Example: POST /conversations/:cid/invite — what's the request body? {"agent_id": "uuid"}? The spec says "adds another agent by its
   identifier" but doesn't specify the wire format.

  Content-Type handling:
  - All endpoints accept/return JSON — except the streaming write (NDJSON) and SSE (text/event-stream). Is this validated?

  Request timeouts:
  - What's the timeout for each endpoint type? Standard CRUD: 30s? Streaming write: no timeout (connection-bound)? SSE: no timeout?

  ---
  Gap 3: Server Lifecycle — startup, shutdown, configuration

  cmd/server/main.go is listed in the file tree but never designed.

  Startup sequence:
  - What's the exact order? Parse env vars → connect Postgres → run migration → connect S2 (create basin if needed?) → run recovery
  sweep → start cursor flush goroutine → start Claude agent → start HTTP server.
  - What happens if Postgres is unreachable at startup? Retry? Fail fast?
  - What happens if S2 is unreachable? Can the server start in degraded mode?

  Environment variables:
  - Complete list: DATABASE_URL, S2_ACCESS_TOKEN, S2_BASIN, ANTHROPIC_API_KEY, PORT, others?
  - Which are required vs. optional with defaults?

  Graceful shutdown:
  - Signal handling (SIGINT, SIGTERM).
  - Drain sequence: stop accepting new requests → close all SSE connections (triggering cursor flushes) → flush remaining cursors →
  close S2 sessions → close Postgres pool → exit.
  - Timeout on graceful shutdown (e.g., 30 seconds before force-kill).

  Health endpoint:
  - GET /health — what does it check? Postgres connectivity + S2 connectivity? Or just "server is running"?
  - Does it return structured JSON or just 200?

  ---
  Gap 4: Event Model (Go Types) — internal/model/events.go

  The S2 plan specifies the wire format (S2 headers + JSON body) and the six event types. But the Go type system for working with
  these events is not designed:

  - Is there a single Event interface with concrete types per event? Or a struct with a Type string and json.RawMessage body?
  - Constructor functions: NewMessageStartEvent(msgID, senderID) → what exactly do they return?
  - How does serialization work? Event → S2 AppendRecord and S2 Record → SequencedEvent?
  - The SequencedEvent type in the S2 plan is a start, but the domain types (MessageStartPayload, MessageAppendPayload, etc.) aren't
  specified.

  ---
  Gap 5: Service/Orchestration Layer ✅ RESOLVED
  → Decision: No service layer. API handlers ARE the orchestration. See spec.md "Files to Create" section.

  The file tree shows internal/service/conversations.go and internal/service/messages.go. But spec.md has API handlers calling the
  store directly (e.g., the StreamMessage handler in §1.3 directly calls h.s2.OpenAppendSession and h.store.InProgress().Insert).

  The question: Is there a service layer or not? If yes, what goes there vs. the API handlers vs. the store? If no, remove it from the
   file tree. This ambiguity will bite you during implementation.

  Decision: No service layer. The API handlers ARE the orchestration. The store handles data access, the S2 client handles stream
  operations, and handlers compose them. An intermediate service layer would add indirection for zero benefit. File tree updated
  in spec.md to remove internal/service/. This is an explicit design decision, not a gap.

  ---
  Gap 6: Deployment Configuration

  Dockerfile and fly.toml are listed but not specified:

  - Multi-stage Docker build: what base image? What Go version? Static binary?
  - fly.toml: machine size, region, internal port, health check config, env var secrets?
  - How are secrets (DATABASE_URL, S2_ACCESS_TOKEN, ANTHROPIC_API_KEY) managed? fly secrets set?

  ---
  Priority Order for Remaining Design Work

  Based on impact and dependency:

  1. Claude Agent (Gap 1) — The evaluators interact with this directly. Under-designing it means fumbling during implementation on the
   most visible component.
  2. HTTP API Layer (Gap 2) — Every endpoint's exact contract. Without this, you'll make ad-hoc decisions during coding that may be
  inconsistent.
  3. Server Lifecycle (Gap 3) — Startup, shutdown, configuration. The skeleton everything hangs on.
  4. Event Model Go Types (Gap 4) — The bridge between your S2 wire format and your Go code. Small but important to nail before
  coding.
  5. Service Layer Decision (Gap 5) — Quick decision: yes or no. Eliminate the ambiguity.
  6. Deployment (Gap 6) — Mechanical, but needs to be specified so it's one-shot during build.
