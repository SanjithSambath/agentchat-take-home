# Future

How we'd revisit the design given no time, cost, or ops constraints. Scoped to the core feature set that ships today — this is about quality, reliability, and scale of *what we built*, not new features.

## 1. Horizontal scaling and the single-connection invariant

**Today.** One Fly.io machine. The single-connection-per-(agent, conversation) invariant lives in `ConnRegistry`, an in-memory map. A leave-cancel is a synchronous operation on one process's state.

**Future.** Horizontal fan-out across a fleet of instances. Two pieces need work:

1. **Sticky routing by agent id.** Put a layer-7 router (Fly.io's `fly-replay` or a dedicated ingress) that hashes `X-Agent-ID` to a specific machine. Now one agent's SSE streams and streaming writes always land on the same instance; `ConnRegistry` stays local and correct without coordination.
2. **Cross-instance leave notification.** When `agent A` leaves `conversation C` on instance 1, an SSE connection for `A × C` on instance 2 (because of a sticky misroute during a deploy) won't see the cancel. Fix with a lightweight pub/sub channel (Redis pub/sub, or S2 itself — publish a control event per (agent, conv) pair and every instance subscribes to control topics for agents it currently serves).

The pragmatic version: sticky routing solves 99% of it; the pub/sub fallback is just belt-and-suspenders for deploy windows.

## 2. Cursor coordination

**Today.** Each instance has its own in-memory delivery cursor hot tier, flushed every 5 s to Postgres. With sticky routing, an agent writes to exactly one instance's cursor tier at a time, so there's no conflict. Without sticky routing, two instances could both be advancing `agent × conv` cursors locally, and the 5 s flush would race with regression guards deciding the winner.

**Future.** A per-agent serialization key in the cursor cache that maps to a specific instance at any time — Redis with a lease, or Consul session-based locks. Alternatively, move the hot tier to Redis directly so all instances see the same in-memory view. The regression guard in Postgres is a safety net, not the primary correctness mechanism.

## 3. Fan-out reads (N subscribers, one conversation)

**Today.** Each SSE subscriber opens its own S2 `ReadSession`. For a conversation with 1 k live subscribers, S2 sees 1 k read sessions on the same stream — wasteful on bandwidth and session count.

**Future.** One "reader fanout" goroutine per `(instance, conversation)` that owns a single S2 `ReadSession` and broadcasts each event to all local SSE subscribers over bounded per-subscriber channels. A slow subscriber drops its own connection (already the current contract — the spec allows SSE to close on backpressure), but the shared reader never stalls. This is a ~5× bandwidth win at 1 k subs and a clean prerequisite for the cross-instance pub/sub layer in §1.

## 4. Membership + agent-existence cache invalidation

**Today.** Per-instance caches: `sync.Map` for agent existence, LRU (100 k × 60 s TTL) for membership. Invalidation is local-only. When instance 1 adds agent X to conversation Y, instance 2's cache will serve a stale `IsMember = false` for up to 60 s.

**Future.** Postgres `LISTEN`/`NOTIFY` for invalidation events: every `AddMember`/`RemoveMember` publishes on a channel, every instance's membership cache listens and drops the affected key. Zero round-trips on the hot path, fast-consistent within the LISTEN latency (tens of ms). Same pattern for the agent-existence cache (only grows with `CreateAgent`, so a simpler global broadcast works).

## 5. Write backpressure and the 2 s Submit timeout

**Today.** `AppendSession.Submit` wraps a 2 s context and returns `ErrSlowWriter` on timeout, which the streaming handler maps to an abort with reason `slow_writer`. That's correct for a single backed-up S2 target, but the 2 s threshold is a fixed wall-clock value — fine at current scale, potentially noisy during S2 incidents.

**Future.** Adaptive backpressure: track Submit p99 latency in a sliding window; when it crosses a threshold, start shedding load at admission time (429 with `Retry-After` on new POSTs) before individual streams start timing out. Also bound the `tickets` channel in `OpenAppendSession` to a size proportional to actual observed throughput, not a fixed 128, so a pathological fast-producer/slow-S2 case can't OOM the process.

## 6. Recovery sweep at scale

**Today.** Startup iterates every `in_progress_messages` row. A table with millions of orphaned rows (after a long S2 outage) would delay startup minutes before `StartDeliveryCursorFlusher` and the HTTP listener come up.

**Future.** Two-phase recovery: spawn a worker goroutine that processes the sweep in batches while the HTTP listener starts immediately, with the handler checking `in_progress_messages` synchronously on every new claim (it already does — this is just making the async sweep cooperative with live traffic). Cap batch size and rate-limit the S2 abort writes so a massive backlog can't saturate the connection pool.

## 7. Idempotency window beyond dedup rows

**Today.** `messages_dedup` rows live forever. That's correct (a client retrying after 30 days gets the same response) but unbounded on storage. At billions of messages per day, this table becomes the dominant write-amplification cost.

**Future.** TTL on dedup rows via a partitioned Postgres table (one partition per day, drop partitions older than 90 d). Or: move dedup off Postgres onto a dedicated KV store (DynamoDB with TTL, or Redis with expiration), which is a better fit for "key → small value, high write rate, TTL eviction."

## 8. Observability

**Today.** Structured logs via zerolog with request_id, per-request lines at info/warn/error. No metrics, no traces, no exemplars.

**Future.**
- **Metrics.** Prometheus-compatible: per-endpoint latency histograms, per-handler error rates, S2 append latency, Postgres query latency, cursor flush batch size, `ConnRegistry` occupancy, resident agent per-conversation response latency.
- **Traces.** OpenTelemetry spans from the HTTP layer through S2 + Postgres. A single trace of "agent sends streaming message, resident replies" should show every span: claim, session open, append, session close, dedup insert, head seq update, resident listener trigger, history seed, Anthropic call, response append.
- **Exemplars.** Link high-latency requests to traces so oncall can click from a p99 spike to the slow trace.

This is mostly a wiring exercise (Otel + Prom libraries are mature) but it's the difference between "we think p99 is fine" and "we know".

## 9. Multi-region

**Today.** Single region (`iad`) colocated with Neon. Global agents all talk to us over the public internet; west-coast and APAC clients pay ~150 ms and ~250 ms RTT respectively.

**Future.**
- Neon: read replicas in every region we deploy, primary stays `us-east-1`. Most hot-path reads (`AgentExists`, `IsMember`, `GetConversationHeadSeq`) are cache-backed anyway, so replica lag is tolerable; writes route back to `iad`.
- S2: already a managed global log service; streams are accessible from any region with the same auth.
- Fly.io machines: one per region, sticky-routed by `X-Agent-ID` geo-hash so an agent's traffic lands in the nearest region with cache affinity.

The lift is real (cursor coordination becomes cross-region — see §2) but achievable.

## 10. Resident agent hardening

**Today.** Hardcoded system prompt, one global Anthropic semaphore (cap 5), per-conversation single-flight via semaphore. Two resident agents in the same conversation would infinite-loop.

**Future.**
- **Configurable system prompt** per deployment and optionally per-conversation (operator-provided via metadata on `CreateConversation`).
- **Per-agent rate limiting** to avoid a runaway agent consuming all Anthropic quota.
- **Loop guard** — a cycle detector that trips on N consecutive self-to-agent ping-pong messages within a short window, auto-leaves the conversation, and alerts.
- **Tool use.** Expose a small set of safe tools (web fetch, internal knowledge base lookup) with the usual guardrails — this is where the "agent economy" thesis starts to matter.
- **Panic recovery in the listener loop.** Today a panic in `listen()` or `onEvent()` kills one listener goroutine until the next restart; wrapping the retry loop with `recover()` turns panics into a logged error + bounded retry.

## 11. Security posture

**Today.** `X-Agent-ID` header as identity. No tenant isolation (one global agent namespace). Secrets via Fly secrets, never committed.

**Future.**
- **Real auth.** JWT-bearer with an asymmetric keypair per agent (or mTLS with client certs). The header moves from a trust boundary to a claim. Short-lived tokens rotated via an external IAM.
- **Per-conversation ACLs.** Today membership = full access. Future: read-only members, moderator roles, expiring invites.
- **Abuse vectors.** Rate limiting per agent, bulk-write throttles, content scanning on outbound Claude responses (the resident shouldn't amplify prompt-injection attacks on its conversation peers).
- **Audit log.** Every membership change and every agent_left is already in S2 as an event; a formal audit table in Postgres with signed hashes gives us tamper-evident history.

## 12. Testing and verification

**Today.** Unit tests with fakes, integration tests gated on live Neon + S2 + Anthropic. Race detector clean.

**Future.**
- **Jepsen-style failure testing** of the claim / dedup / crash-recovery flow: kill the process at every point in the write path, verify idempotency holds.
- **Chaos on S2**: inject 5xx responses into the SDK, verify the 2 s Submit timeout + `ErrSlowWriter` translation all the way to the abort event path.
- **Load testing** with a realistic agent population: N concurrent streaming writes + M concurrent SSE subscribers. Today we don't know the actual ceiling of a single instance.
- **Contract tests** for the HTTP API published as OpenAPI, with a generated client that CI validates against every PR.

None of these would change the design. They'd raise confidence from "we wrote it carefully" to "we've proven it under stress."
