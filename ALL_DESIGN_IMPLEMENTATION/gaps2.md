Audit Report: Gaps Remaining After Filling gaps.md
I read every plan document, cross-referenced against README.md deliverables/evaluation criteria, and checked for scattered-mention-but-undesigned artifacts. Here are my findings.

What's genuinely closed
The original 6 gaps from gaps.md have dedicated plan documents that match the rigor of s2-architecture-plan.md and sql-metadata-plan.md:

Original Gap	Document	Depth
Gap 1 (Claude Agent)
claude-agent-plan.md (86KB)
Exhaustive
Gap 2 (HTTP API Layer)
http-api-layer-plan.md (85KB)
Exhaustive
Gap 3 (Server Lifecycle)
server-lifecycle-plan.md (83KB)
Exhaustive
Gap 4 (Event Model)
event-model-plan.md (61KB)
Exhaustive
Gap 5 (Service Layer)
Decision: no service layer
Resolved
Gap 6 (Deployment)
deployment-plan.md (64KB)
Exhaustive
The depth is real. These documents enumerate race conditions, include line-by-line rationale, specify Go structs, and have scaling sections.

Gaps That Emerged After Filling
These are the new "design hills" — things that either (a) README.md explicitly calls out as deliverables but have no plan, (b) are referenced throughout the plans as FUTURE.md / CLIENT.md / DESIGN.md but never designed, or (c) are cross-cutting concerns no single plan owns.

I've ranked them by impact on the evaluation criteria.

Gap 7 — CLIENT.md content design (REQUIRED deliverable, no plan)
Severity: Critical. This is a named deliverable in README.md.

README Deliverable #4: "A single markdown file that, when passed to an AI coding agent (e.g., Claude Code), gives the agent everything it needs to use your messaging service without any human assistance." Evaluation criterion: "Developer and Agent Experience — The client documentation markdown file is self-contained and sufficient for an AI agent to use the service without human help."

Current state: CLIENT.md is referenced 14+ times across plan documents ("document in CLIENT.md", "CLIENT.md guidance", "self-contained agent onboarding doc including scaffolding examples"). It is never designed. There is no client-documentation-plan.md.

Why this matters more than the other gaps: This is the most visible artifact to evaluators. The code they read is the server. The doc they read to use the server is CLIENT.md. If it's written by improvisation at the end of the sprint, it will be weaker than the system it documents. Every other plan explicitly defers content to it.

What needs to be designed:

Canonical structure (Quick Start → Concepts → API Reference → Event Model → Streaming Patterns → Error Handling → Multi-conversation orchestration → Worked example)
Concrete curl/JSON examples for every endpoint, complete enough to copy-paste
NDJSON write format with a full token-streaming example (the non-trivial part)
SSE read format with interleaved-events-for-concurrent-writers example
Complete error code enumeration + retry semantics per code
Reference reconnection loop with cursor handoff
Reference pattern for an external agent in at least one language (Python or Go), since these are the languages evaluators will actually use
Idempotency model (what retries are safe)
Explicit statement of delivery guarantees from the client's perspective
Anti-patterns (don't poll GET /conversations on a hot loop, etc.)
The resident agent's discovery flow so the evaluator knows how to find it
Recommend: A client-documentation-plan.md at the same depth as http-api-layer-plan.md, specifying structure, every worked example, and the Go/Python reference snippets. The actual CLIENT.md file is then a straightforward write-up from that plan.

Gap 8 — Testing strategy & infrastructure (REQUIRED deliverable, sketched not designed)
Severity: Critical. Named deliverable + dedicated evaluation dimension.

README Deliverable #1: "A complete, working implementation of the messaging service with tests." Evaluation criterion: "Testing — Meaningful test coverage that demonstrates correctness of the core messaging behavior. Tests should inspire confidence in the system's reliability."

Current state: spec.md §1565-1557 has a ~20-line bullet list of unit/integration/edge case/load test scenarios. Every other gap got a dedicated 60-90KB plan. Testing got a bullet list.

What's missing at plan-document depth:

Test pyramid strategy: ratio of unit to integration to e2e, where boundaries are drawn
Mocking strategy for S2: interface-based fake in-memory stream? real S2 with a throwaway basin per test run? This is non-trivial and affects every test in the repo
Mocking strategy for Postgres: testcontainers-go with real Postgres? pgx test utilities? transaction-rollback-per-test? This affects test isolation and speed
Mocking strategy for Anthropic: record/replay fixtures? stub server? environment flag?
Test DB lifecycle (schema-per-test, transactions, truncation, and the race conditions each introduces)
Concurrency tests: t.Parallel interactions with shared stores, race detector configuration, how to deterministically trigger each of the 5 race conditions enumerated in sql-metadata-plan.md §9
Property-based / fuzz tests for the event demux (the most correctness-sensitive code in the system)
Deterministic clock/UUID injection for test determinism (cursor batching, UUIDv7 ordering)
Snapshot/golden file strategy for NDJSON and SSE wire format regression
CI pipeline: GitHub Actions yaml, what's gated behind build tags, how credentials flow
Flaky test policy: eventual consistency in integration tests, polling vs notifications
Explicit scenarios for the correctness claims made elsewhere (e.g., "interleaved concurrent writes produce reconstructable messages" is asserted in s2-plan §8 but not translated into a concrete test specification)
How to test graceful shutdown (subprocess + signal + verify behavior)
Recommend: A testing-plan.md at full plan-document depth, covering fake vs real dependency strategy, test isolation, a concrete scenario catalogue mapped to spec sections, and a CI config. Given that "Testing" is an explicit evaluation dimension, sketched bullets are under-investment.

Gap 9 — FUTURE.md / "no-restrictions" design document (REQUIRED deliverable, no consolidated plan)
Severity: Critical. Named deliverable.

README Deliverable #2: "Documentation covering… How you would revisit those design decisions given no restrictions on time and resources. This is not about adding new features — it is about infrastructure, reliability, scale, and the overall design quality of the core feature set you chose to implement." Evaluation criterion: "Documentation — … Thoughtfulness in the 'no restrictions' design discussion, particularly around infrastructure, reliability, and scale."

Current state: Every plan has a local "Scaling Considerations" section (e.g., http-api-layer-plan.md §9, s2-architecture-plan.md §13, sql-metadata-plan.md §14). These are component-local. FUTURE.md is referenced 8+ times across plans ("document in FUTURE.md") as a future artifact. There is no unifying design for what FUTURE.md actually says as a document, how it's structured, or how the per-plan scaling bullets cohere into a single narrative.

The risk: Scattered "document in FUTURE.md" notes will collapse into an incoherent dump unless a plan explicitly designs the document. This deliverable is how evaluators assess senior-level thinking about the system at scale — it needs the same production-grade rigor as the rest of the planning.

What a future-plan.md needs to spec out:

Document structure: per-component "unconstrained redesign" chapters mapped 1:1 to the current components
Each chapter: current design → constraints we accepted → what would change under no-restrictions → second-order consequences
Cross-cutting chapters not owned by any current plan:
Multi-region active-active (replication, read-locality, write-quorum, conflict resolution since S2 is regional)
Stricter delivery guarantees (what exactly-once would require end-to-end, and why we chose at-least-once + dedup)
Custom stream layer vs. S2 (build-vs-buy revisited)
Replacing NDJSON with bidi gRPC / WebSocket for agents that want both directions on one connection
End-to-end ordering at scale (when S2 partitioning becomes necessary per conversation — the current "one stream per conversation" bet)
Membership materialization as a consistent cache (Redis/Spanner-backed) once Postgres becomes the bottleneck
Resident-agent multi-instance leader election (referenced as "document in FUTURE.md" in server-lifecycle-plan.md but never specified)
Abuse/rate-limiting infrastructure at billions scale
Schema evolution of the event wire format across versions
Cost model at 1M / 100M / 1B agents with concrete $/month projections
Compliance surface (SOC 2, HIPAA, GDPR deletion) — even if dismissed as "not this product"
Recommend: A future-plan.md that specifies structure, claims, and evidence for the unconstrained redesign. This is the deliverable that most rewards depth — a weak FUTURE.md signals weak senior-engineer thinking no matter how strong the implementation is.

Gap 10 — Observability & diagnostics (cross-cutting, no owner)
Severity: High. Production-grade thinking is a CLAUDE.md hard rule; "billions of agents" is the scale target.

Current state: Scattered mentions totaling roughly two paragraphs across all plans:

http-api-layer-plan.md has request ID and structured logging
server-lifecycle-plan.md has /health
spec.md §1558 has three bullets (structured logging, metrics list, health endpoint)
"Log a warning for observability" appears 4-5 times with no specification of how or where those logs go
No plan owns: metrics surface, tracing model, log schema, log aggregation path, alerting thresholds, runbook design, debug endpoints, performance profiling access, dashboard design.

Why this is a real gap, not a future-only concern: The streaming system has non-trivial failure modes that are invisible without observability (slow readers, cursor lag, S2 backpressure, Claude rate-limit blocks). During evaluation, if something subtly breaks, the evaluator can't tell why without logs/metrics. And the CLAUDE.md rule says production-grade thinking from day one.

What needs designing:

Structured log schema: mandatory fields (request_id, agent_id, conversation_id, seq, message_id, component, level), JSON format
Log levels policy (what's INFO vs DEBUG vs WARN)
Log aggregation path for the Cloudflare-tunneled host (stdout → journalctl / systemd-cat or container log driver) and where that goes next (Axiom/Betterstack/Loki) with cost
Metrics: concrete list of counters / gauges / histograms worth emitting, not "metrics: active SSE connections, messages/sec" (that's naming not designing)
Metrics transport: Prometheus scrape endpoint? OTLP push? Does the take-home emit them even if no scraper exists?
Distributed tracing: OpenTelemetry spans on each handler, each S2 operation, each DB query — or deferred to FUTURE.md with explicit rationale
/debug/pprof exposure policy (on? behind flag? off?)
Correlating a single NDJSON streaming write across: handler → AppendSession → ack loop → downstream SSE delivery (request_id propagates through all of these)
SLIs worth defining even if no SLO enforcement: p99 first-token latency, SSE connection drop rate, cursor flush lag, S2 append p99
Recommend: An observability-plan.md that commits to a specific surface. Right now the mentions are performative ("log a warning for observability"), not designed.

Gap 11 — Backpressure & flow control (cross-cutting, sketched)
Severity: High. Core streaming correctness concern.

Current state: Mentioned in passing:

s2-architecture-plan.md §5: "AppendSession pipelines…backpressure automatically (blocks Submit when inflight bytes exceed 5 MiB default)"
spec.md §446: "Slow reader: S2 handles backpressure. If the client can't keep up, the HTTP response buffer fills, Go's http.Flusher blocks, and the S2 read session slows down. No unbounded memory growth."
These assertions are made but not designed. A streaming system's reliability claim rests on the flow-control story being end-to-end and correct.

Unanswered questions:

Claude → S2 path: Claude's streaming output fills a buffered Go channel. S2 AppendSession.Submit blocks when its 5 MiB inflight cap is reached. Does the channel between Claude and S2 have a bounded buffer that propagates backpressure back to the Anthropic SDK? Or does it buffer unboundedly and OOM on a sustained S2 slowdown? The claude-agent plan references io.Pipe which is synchronous — good — but this claim needs to be tested and documented explicitly.
NDJSON client → S2 path: If S2 is slow, AppendSession blocks, the handler's Scanner loop blocks reading r.Body. TCP window fills, client is backpressured. This works for a well-behaved client. What about a client that sends NDJSON lines far faster than S2 can absorb? Is there a server-side rate limit before backpressure, or do we trust TCP end-to-end?
S2 → SSE client path: spec.md §446 asserts the flow works but doesn't specify the Go http.Flusher semantics under load. Does the ReadSession actually stop pulling, or do records queue in the SDK's internal buffer?
Cursor flush path: Under sustained load, the cursor flush batch may grow unbounded if Postgres is slow. sql-metadata-plan.md §7 mentions batching and flush triggers but not a bounded-queue drop-or-block policy.
Connection registry growth: Under a burst of 100k concurrent SSE opens, when does the registry refuse new connections vs. accept and fail gracefully?
Recommend: A backpressure-plan.md (or a §in server-lifecycle-plan.md) that walks each of the four streaming pipelines above, specifies the bounded buffer at every stage, names who blocks and who drops, and specifies the observable symptom when the system is saturated.

Gap 12 — Input validation, size limits & defensive programming — RESOLVED
Severity: Medium-High. Correctness and abuse surface.
→ See http-api-layer-plan.md §6 "Input Limits & Validation" (new section).

Resolution:
- §6.1 NDJSON scanner buffer: explicit 1 MiB + 1 KiB cap via `scanner.Buffer(...)` with error mapping for `bufio.ErrTooLong` → `413 line_too_large` and a `message_abort` reason of the same name.
- §6.2 Header validation: `X-Agent-ID` validated by existing Agent Auth middleware; `X-Request-ID` given explicit 128-char + ASCII-printable charset guard with silent fallback.
- §6.3 Body invariants: UTF-8 validation on complete-message `content` via `utf8.ValidString` → `400 invalid_utf8`; empty-content rejection already on the endpoint table.
- §6.4 Defacto limits: consolidated table pointing to ConnRegistry (`sql-metadata-plan.md` §10), `in_progress_messages` UNIQUE (`s2-architecture-plan.md` §10), and S2's 1 MiB cap (mapped to `413 content_too_large`).
- §6.5 Accepted-surface table: every unlimited-at-evaluation-scale knob named with current behavior, failure point, and production enforcement — including conversations-per-agent, members-per-conversation, per-agent rate limits, stream duration.
- §6.6 Four new stable error codes registered in §1: `line_too_large`, `content_too_large`, `invalid_utf8`, `request_too_large`.
- §6.7 Testing strategy: 4 boundary tests — oversize line, header matrix, body invariants, idle bound.

Cascaded changes:
- event-model-plan.md §3: abort-reason registry extended with `line_too_large`, `content_too_large`, `invalid_utf8` constants.
- event-model-plan.md §2: "Very Large Content" cross-reference now points to http-api-layer-plan.md §6.1/§6.4.
- s2-architecture-plan.md §5: streaming handler pseudocode updated to include the explicit `scanner.Buffer(...)` call with cross-reference to §6.1.
- sql-metadata-plan.md §10: ConnRegistry subsection now explicitly cross-references §6.4's defacto-limit table and documents the two distinct concurrent-write limits it enforces.
- claude-agent-plan.md §7: resident-agent error-handling table includes a row for "Claude emits a token >1 MiB" routing to the existing `message_abort` path with reason `content_too_large`.
- high-level-overview.md: streaming write description now cites the explicit scanner cap with cross-reference to §6.1.

spec.md §1.3 scanner snippet is pending a surgical update (awaiting CLAUDE.md Rule 1 confirmation from the user).

Gap 13 — Data lifecycle & retention (no owner, partial coverage)
Severity: Medium. The "billions of agents" framing makes this real.

Current state:

S2: 28-day retention documented in s2-architecture-plan.md
Trimmed-read behavior: specified in spec.md §445 ("silently start from stream's earliest available record")
Postgres: no retention story; agents, conversations, members, cursors grow unbounded
What's missing:

Orphan cleanup: what happens when all members leave a conversation? The README explicitly says "A leave by the last remaining member is rejected," so the conversation cannot become empty. But the last member can stay forever in a dead conversation. At billions scale, this is unbounded growth.
Orphaned agent cleanup: agents registered and never used. No TTL.
S2 stream lifecycle: if a conversation is abandoned, the stream is never deleted. S2 bills per storage. Plan needs to name this.
Postgres vacuum policy for cursors on abandoned conversations
Backup/recovery strategy for Neon (Neon has PITR; specify retention window and recovery RPO)
Disaster recovery for S2 (S2 has no user-visible backup — what's the recovery plan on accidental basin deletion or a major S2 incident?)
GDPR-style agent deletion (out of scope per README, but "document why we don't support it" is the senior-engineer answer)
Recommend: A short data-lifecycle-plan.md or absorb into future-plan.md. The immediate-term answer is "accept unbounded growth for take-home"; the plan should name that explicitly and say what breaks when.

Gap 14 — Schema / API / wire-format versioning (mentioned, not designed)
Severity: Medium.

Current state:

event-model-plan.md §1 mentions forward compatibility (typed string constants, unknown event handling)
sql-metadata-plan.md §13 has "Schema Evolution Path" with a sentence or two
No versioning for HTTP API (/v1/? implicit?)
No versioning policy for S2 record format when adding a 7th event type
What's missing:

HTTP API versioning policy (URL vs header vs none, with rationale)
Event schema evolution: how do readers handle new event types appearing mid-stream (documented as "skip and warn" but no broader policy — e.g., what about renamed fields? removed fields?)
SQL migration story beyond v001 (tooling? golang-migrate? embedded?)
Client contract: error codes are stable (good, spec.md §1286); is the wire-format stable? What's the deprecation policy?
Recommend: A short versioning section, either consolidated into http-api-layer-plan.md or its own mini-plan. This is the kind of thing that seems minor until the first production breaking change.

Gap 15 — Cross-component race condition audit
Severity: Medium. Each plan covers its own races; interactions between components are not audited.

Individual plans are strong:

sql-metadata-plan.md §9 enumerates 5 Postgres-level races
s2-architecture-plan.md §9 covers the leave-mid-write race
claude-agent-plan.md §7 covers agent recovery races
http-api-layer-plan.md §3 covers membership-check vs leave
What's not audited: races that span components. Example scenarios:

Resident agent startup reconcileConversations() runs in parallel with an invite for that agent — does it see the new conversation via its go-channel, or miss it because reconciliation started first?
SSE ReadSession delivering seq=N, cursor flush pending, server SIGTERM: does the cursor flush on shutdown capture seq=N, or does the agent re-see seq=N on reconnect?
Concurrent invite(X) and leave(X) where X is mid-connection: what's the observable order on the stream? Does SSE close immediately, or does the client see the agent_left event before close?
Concurrent leave by last member (rejected) racing with an invite that succeeds: does the last-member-leave really win the reject?
Claude agent crashed mid-response → server restart → recovery sweep emits message_abort → old agent reconnects and sees abort it didn't write → behavior?
Two concurrent invite(X) calls from two members — Postgres UPSERT handles idempotency; does the stream see one agent_joined or two?
Each plan answers a subset. No document stitches them together. At billions scale, cross-component interleavings are where real bugs live.

Recommend: A race-condition-audit.md that enumerates interesting N≥2-component interactions and either points to the authoritative plan section or raises a new concern. This is pure analysis, not new design — but it's the work senior engineers do before declaring a system "complete."

Gap 16 — Resident-agent multi-instance / horizontal scale policy
Severity: Medium. Explicitly deferred but the deferral isn't sufficient.

server-lifecycle-plan.md §6 acknowledges: "Multiple instances each run their own agent, all listening to the same conversations, all responding to every message. Fix: leader election for agent ownership. Document in FUTURE.md."

That's three sentences. "Leader election" is hand-wavy — is it a single-instance invariant enforced by the process supervisor, a Postgres advisory lock, Redis SETNX, Raft quorum? Each has different failure modes. Since FUTURE.md doesn't exist yet, this promise is currently a rounding error.

Recommend: Either a concrete resident-agent-HA section inside future-plan.md (once that exists) or an acknowledgment that the take-home deploys single-instance with a documented failure mode (evaluator sees downtime during deploy rolling restarts), and that's the accepted trade-off.

Summary: Recommended Priority Order
Based on README.md deliverables + evaluation criteria + production-grade thinking:

#	Gap	Why	Document to create
1
CLIENT.md content
Required deliverable. Most visible to evaluators. 14+ plans defer to it.
client-documentation-plan.md
2
Testing plan
Required deliverable. Dedicated evaluation criterion. Currently a bullet list.
testing-plan.md
3
FUTURE.md
Required deliverable. Scattered "document in FUTURE.md" references with no coherent plan.
future-plan.md
4
Observability
Production-grade from day one (CLAUDE.md rule). Current coverage is performative.
observability-plan.md
5
Backpressure
Core streaming correctness; asserted but not designed end-to-end.
Section in server-lifecycle-plan.md or dedicated
6
Input limits & validation
Abuse surface / correctness. Partial coverage.
Section in http-api-layer-plan.md
7
Data lifecycle
Scale-target realism.
Section in future-plan.md or dedicated
8
Versioning
Evolution story. Mentioned, not designed.
Section in existing plan
9
Cross-component race audit
Analysis, not design. Senior-engineer-level work.
race-condition-audit.md
10
Resident-agent HA
Acknowledged, three sentences deep.
Section in future-plan.md
Recommendation
The three named README deliverables (CLIENT.md, FUTURE.md, testing) are the most under-designed relative to their importance. They each deserve the same plan-document-depth rigor as s2-architecture-plan.md — not bullet lists at the end of spec.md.

The cross-cutting concerns (observability, backpressure, lifecycle, versioning, race audit) are less severe but are the kinds of things evaluators probe to distinguish senior-level from mid-level system design work.

Following your established pattern in gaps.md → dedicated plan documents, I'd recommend prioritizing:

client-documentation-plan.md — highest visibility
testing-plan.md — explicit evaluation dimension
future-plan.md — explicit evaluation dimension
observability-plan.md — cross-cutting, currently performative
Merge the remaining smaller gaps as new sections into the most relevant existing plans, keeping surgical-edit discipline.
Shall I proceed with any of these? Per CLAUDE.md Rule 1, I will not modify spec.md or create any new plan documents without your explicit confirmation on which gap and which document to tackle first.