# Future

A complete production-hardening audit of the AgentMail take-home. Scoped to the feature set that ships today per the README — *how* we'd revisit each design decision given no restrictions on time or resources, not *what new* we'd add. Every entry traces back to a tradeoff called out in one of the docs under `ALL_DESIGN_IMPLEMENTATION/`, cited inline.

Each entry follows the same shape:

- **Today.** What's actually in the repo, plus the tradeoff the design doc acknowledged when we chose it.
- **Production.** The named target (tool, library, pattern) and the rationale for that choice over the alternatives.

---

## Part 1 — Horizontal scaling and request routing

### 1.1 Sticky routing by `X-Agent-ID`

**Today.** `ConnRegistry` is an in-memory per-process map, and a leave-cancel is a synchronous operation on one process's state (`sql-metadata-plan.md §7`, `deployment-plan.md §8.2`). One process behind one Cloudflare Tunnel makes the single-connection-per-`(agent, conversation)` invariant trivially true. The moment we add a second instance without affinity, two streaming writes for the same `(agent, conv)` can land on two processes and both succeed — the invariant evaporates.

**Production.** A layer-7 router in front with session affinity keyed on the `X-Agent-ID` header. Two viable paths: **Cloudflare Load Balancer** with a cookie or header affinity rule, or a thin **Cloudflare Worker** that consistent-hashes `X-Agent-ID` to a specific origin. Either way, an agent's SSE tails and streaming POSTs always land on the same instance, so `ConnRegistry` stays correct without coordination. Preferred over Envoy because the tunnel terminates at Cloudflare already; adding another ingress hop buys us nothing and costs us latency.

### 1.2 Cross-instance leave notification

**Today.** `RemoveMember` cancels only the SSE connections and streaming writes registered on the local `ConnRegistry`. If sticky routing misroutes once during a deploy and `agent A` has an active tail for conversation `C` on instance 2 while the leave lands on instance 1, instance 2's connection keeps streaming events the agent is no longer entitled to see until the next heartbeat check.

**Production.** A lightweight pub/sub channel for cancellation, layered on top of sticky routing as belt-and-suspenders. **Postgres `LISTEN/NOTIFY`** on a `conn_cancel` channel is the right default: we already have Postgres in the hot path, every instance is already connected, and latency is tens of milliseconds — `RemoveMember` publishes `(agent_id, conversation_id)`, every instance subscribes and drops any matching local connection. **Redis pub/sub** is the fallback if we outgrow a single Postgres primary's NOTIFY fan-out (unlikely under 50 instances). Rejected: publishing cancel events on the S2 stream itself — fan-out would burn an entire conversation stream's read budget on membership churn.

### 1.3 Membership and agent-existence cache invalidation

**Today.** Per-instance caches: `sync.Map` for agent existence (spec.md §891, `http-api-layer-plan.md §17`), LRU with 100 k entries at 60 s TTL for membership (`sql-metadata-plan.md §8`). Invalidation is local-only. When instance 1 adds agent X to conversation Y, instance 2's cache will serve a stale `IsMember = false` for up to 60 s. The 60 s TTL is the safety net, not the primary consistency mechanism.

**Production.** **Postgres `LISTEN/NOTIFY`** on channels `membership_changed` and `agent_created`. Every `AddMember`/`RemoveMember`/`CreateAgent` publishes the affected key; every instance's cache drops that key on receipt. Zero round-trips on the read hot path, fast-consistent within tens of ms. The TTL stays as a safety net against missed notifications (e.g., during a Postgres connection drop). This is the same pattern as §1.2 on a different channel, so the infrastructure cost is amortized.

### 1.4 `sync.Map` at billion-agent scale

**Today.** `sync.Map` for agent existence consumes ~32 GB at 1 B agents (16 bytes × 1 B × 2× overhead), per `http-api-layer-plan.md §615`. Not viable for a single process. At current take-home scale (tens of agents) the map is kilobytes and `sync.Map` has ~50 ns lookup, so we deferred the optimization.

**Production.** **Bloom filter** sized for 1 % false-positive rate (1.44 bytes/element = 1.44 GB at 1 B agents) as the first-layer existence check, with a Postgres miss path for the false positives. Named library: `github.com/bits-and-blooms/bloom`. Chosen over a Cuckoo filter because the deletion-support Cuckoo offers is irrelevant — agents are never deleted in the current spec. Chosen over a Redis `SISMEMBER` because the 1.44 GB fits on every origin instance; we trade Redis RTT (~500 µs) for a local memory probe (~50 ns).

---

## Part 2 — Storage evolution

### 2.1 Cursor coordination across instances

**Today.** Each instance has its own in-memory `delivery_seq` hot tier with a 5 s batched flush to Postgres. The regression guard in the UPSERT (`WHERE cursors.delivery_seq < EXCLUDED.delivery_seq`) protects against races, but with sticky routing correctly placing each `(agent, conv)` on one instance there's no contention to protect against in practice (`sql-metadata-plan.md §7`, `spec.md §450`). Without sticky routing, two instances could both be advancing the same cursor locally, and the 5 s flush would race with the regression guard deciding the winner.

**Production.** Move the hot tier to **Redis** as a single source of truth across instances, keeping Postgres as the durable tier. Per-agent serialization via **Redsync** (Redlock over Redis) or a **Consul** session, so a cursor write is always leader-elected for a short TTL. Postgres remains the crash-recovery source and keeps its regression guard. Rejected: Postgres as the hot tier — Redis hits 100 k ops/sec on commodity hardware per instance, Postgres caps at ~50 k with 15 connections (per `http-api-layer-plan.md §877`), and we don't want to spend connection budget on a write path that's fundamentally a cache.

### 2.2 Fan-out reader per `(instance, conversation)`

**Today.** Each SSE subscriber opens its own S2 `ReadSession`. A conversation with 1 000 live subscribers on one instance opens 1 000 read sessions against the same stream — wasteful on bandwidth, session count, and S2 cost per reader.

**Production.** One **reader-fanout goroutine** per `(instance, conversation)` that owns a single `ReadSession` and broadcasts each event to all local SSE subscribers over bounded per-subscriber channels. A slow subscriber drops its own connection (already the current `slow_consumer` contract, `spec.md §502`), but the shared reader never stalls. At 1 000 subscribers on one conversation this is a ~5× bandwidth win vs. the current per-subscriber sessions, and it's the clean prerequisite for the cross-instance pub/sub layer in §1.2 — a single reader per instance means a single cancel event reaches every local subscriber in one hop.

### 2.3 `messages_dedup` TTL

**Today.** Dedup rows live forever (`sql-metadata-plan.md §5`). Correct under the current spec (a client retrying after 30 days gets the same terminal response) but unbounded on storage. At billions of messages per day, this table becomes the dominant write-amplification cost on Postgres.

**Production.** Either a **daily-partitioned Postgres table** (`PARTITION BY RANGE (created_at)`) with partitions older than 90 days dropped by a cron — keeps the code unchanged, buys TTL with zero runtime cost — or **move dedup off Postgres entirely** to **DynamoDB with item TTL** or **Redis with `EXPIRE`**. The KV-with-TTL primitive is a better fit than a Postgres table for "key → small value, high write rate, time-based eviction." Pick DynamoDB if we also want audit retention beyond 90 d without keeping the data hot; pick Redis if the 90 d retention window is the whole ask.

### 2.4 Postgres scaling phases

**Today.** Single Neon branch, pgxpool with `MaxConns = 15` tuned for a 4-core host (`sql-metadata-plan.md §6`). The spec's 4-phase scaling roadmap (`spec.md §1193`) is documented but not built.

**Production.**
- **Phase 2 (tens of millions of agents).** Neon read replicas in every region we deploy to; `AgentExists`, `IsMember`, `GetConversationHeadSeq` all tolerate replica lag because they're cache-backed anyway.
- **Phase 3 (hundreds of millions).** Range-partition `in_progress_messages` and `messages_dedup` by `created_at`; add a denormalized `agent_conversations` table maintained via trigger so "list conversations for agent" doesn't cross-partition scan.
- **Phase 4 (billions).** Move off Neon to dedicated **AWS RDS** or self-managed Postgres; introduce **PgBouncer** in transaction mode between the fleet and the primary so connection count is bounded by pooler workers, not instance count. Rejected: Postgres cluster with Citus — too much operational surface for a workload that's already almost entirely indexed point lookups.

### 2.5 Neon scale-to-zero

**Today.** Disabled for the evaluation window to eliminate cold starts (`deployment-plan.md §9.1`). Storage is well under the 0.5 GB free-tier ceiling at take-home scale.

**Production.** Keep scale-to-zero disabled. Add an **UptimeRobot** `/health` probe every 60 s as a belt-and-suspenders warm-keeper and as the primary external health signal (paging via PagerDuty on failure). Upgrade to **Neon Launch** ($19/mo, 10 GB) the moment storage crosses 500 MB — the free tier's history-branch retention is also more generous on Launch, which makes incident recovery cheaper.

### 2.6 Formal migrations

**Today.** `CREATE TABLE IF NOT EXISTS` run on startup (`sql-metadata-plan.md §13`, `server-lifecycle-plan.md §2`). Works for a single-version schema, fails silently for schema drift, and provides no rollback path.

**Production.** **`golang-migrate/migrate`** with numbered files under `internal/store/migrations/`, a `schema_migrations` tracking table, and deploy-time migration runs gated on a one-time-use lock (advisory lock on a fixed key). Backward-incompatible changes require multi-step deploys (expand → backfill → contract). Rejected: `goose` — comparable features but `golang-migrate`'s CLI is better supported in Docker base images and its file-driver contract is simpler to reason about.

---

## Part 3 — Write-path hardening

### 3.1 Adaptive backpressure

**Today.** `AppendSession.Submit` wraps a fixed 2 s context and returns `ErrSlowWriter` on timeout, mapped by the streaming handler to `message_abort(slow_writer)` (`claude-agent-plan.md §6`, `spec.md §277`). 2 s is 50× S2 Express's normal ~40 ms ack, so it's quiet under healthy operation. During an S2 incident, every slow stream independently decides to abort at the 2 s wall clock — noisy and synchronized in a way that masks the root cause.

**Production.** A **sliding-window p99 latency tracker** on `Submit` acks. When p99 crosses a configured threshold (e.g., 500 ms sustained for 10 s), **shed load at admission** with `429 Retry-After: N` on new streaming POST requests before individual in-flight streams time out. Named: `golang.org/x/time/rate` for the admission limiter, a custom quantile estimator (`github.com/beorn7/perks/quantile`) for the tracker. Rationale: protects us from cascading aborts during real S2 incidents and exposes "we're in load-shed" as an observable signal to clients, not a silent 5xx after 2 s.

### 3.2 Bounded `tickets` channel

**Today.** The `tickets` channel in `OpenAppendSession` has a fixed capacity of 128 (`spec.md §277`). A pathological fast-producer / slow-S2 combination could buffer up to 128 outstanding appends, using bounded memory per stream but unbounded memory across thousands of streams.

**Production.** Size the channel proportional to observed throughput — a **token-bucket refill** from the p95 per-stream append rate over the last 60 s, with a floor of 32 and a ceiling of 512. Bigger buffers for fast tokens per second, smaller for slow. Rationale: defends against OOM under a pathological producer without penalizing the normal LLM-rate producer (30–100 tokens/sec).

### 3.3 Two-phase recovery sweep

**Today.** Startup iterates every `in_progress_messages` row synchronously before the HTTP listener comes up (`server-lifecycle-plan.md §2`). A table with millions of orphaned rows (e.g., after a long S2 outage) would delay startup by minutes before `StartDeliveryCursorFlusher` and the HTTP listener are accepting traffic.

**Production.** Split recovery into two phases. The **HTTP listener starts immediately**; a background worker processes the sweep in batches with a rate limit on S2 abort writes so a massive backlog can't saturate the connection pool. The live claim path already checks `in_progress_messages` synchronously on every new claim, so it's already cooperative with live traffic — the change is making the async sweep a background task rather than a startup blocker.

### 3.4 Idempotency window beyond dedup rows

Covered in §2.3. The key point: exactly-once semantics are implemented correctly today; the production gap is storage retention, not correctness.

### 3.5 Exactly-once semantics under retries

**Today.** Two-table idempotency: `in_progress_messages` as a claim gate, `messages_dedup` for terminal outcomes (`spec.md §4`). The first attempt wins in S2, the second attempt gets the cached response without a second S2 write.

**Production.** No architectural change — the design is correct. The production work is **proving it under stress**: add the Jepsen-style failure tests described in §9.1, kill the process at every point in the write path, verify idempotency holds. That's a confidence upgrade, not a design upgrade.

---

## Part 4 — Read-path hardening and external transport

### 4.1 SSE delivery guarantees

**Today.** As of the 2026-04-21.3 fix, the server only advances `delivery_seq` on **explicit client confirmation** — `Last-Event-ID: N` on reconnect, or a `POST /ack` with seq N. Previously the cursor advanced after `rc.Flush()` succeeded, which mistook "bytes accepted by kernel TCP buffer" for "client received them" — a phantom-advance that broke at-least-once through any buffering proxy.

**Production.** Keep the confirmation-only advancement. Add an `/admin/cursor/{agent_id}/{conversation_id}` debug endpoint (gated behind admin auth, see §6.4) so operators can see delivery vs. ack lag on stuck SSE sessions without querying Postgres directly. Add a per-subscriber metric (§6.1) for time-since-last-ack so we can alert on sessions that are "connected but not acking" — a classic symptom of an intermediate proxy that's forgotten how to deliver bytes.

### 4.2 Tunnel keepalive / buffering defense

**Today.** The Cloudflare quick tunnel (`trycloudflare.com`, invoked by `make tunnel`) buffers SSE chunks and stalls every agent — measured in `deploy/ngrok.md`: local SSE delivers `:ok` + first event in ~500 ms; through the quick tunnel, 0 bytes in 35 s. The dev transport is `make ngrok` (free tier). The Makefile still carries the quick-tunnel target with a `WARNING:` banner because it's useful for offline dev when there's no evaluator on the other end.

**Production.** **Primary:** a named **Cloudflare Tunnel** on a domain we own (`deploy/cloudflared.example.yml` is production-ready). Named tunnels honor `disableChunkedEncoding: false` where quick tunnels don't. **Secondary defense:** server-side per-subscriber **filler frame** `:tunnel-keepalive\n\n` every 5–10 s, in addition to the 30 s heartbeat — small enough to pass through even aggressive buffering, frequent enough to force previously-buffered event chunks past the edge before they time out. Rejected: switching to WebSocket — the SSE contract is working correctly and the ecosystem around `Last-Event-ID` auto-resume is load-bearing; WebSocket would require rebuilding replay semantics from scratch.

### 4.3 Client-side defense in depth

**Today.** `run_agent.py` seeds its first-connect `Last-Event-ID` from `max(seq_end)` in history (per `CLIENT.md §5.2` precedence, overriding any stored server cursor) and recovers orphan `message_end` frames by fetching the completed message from `GET /conversations/{cid}/messages`.

**Production.** Add an **optional sticky `POST /ack` every N messages** (configurable, default 16) to keep `delivery_seq` tight without waiting for reconnect. Cheap: one small POST per ~16 events, negligible at LLM token rates; reduces the replay-on-reconnect window from "last flush" to "last 16 events." Already-designed and documented in `CLIENT.md`; just needs wiring.

### 4.4 PaaS alternative

**Today.** Self-hosted named Cloudflare Tunnel is the production path. The repo previously shipped on **Fly.io** (commit `973b310`), and both `Dockerfile` and `fly.toml` are recoverable from git history.

**Production.** Keep Fly.io in our back pocket as a named alternative if we decide we don't want to own the tunnel infrastructure. Fly's `shared-cpu-1x` VMs are adequate for a single-region deployment, their HTTP/2 origins handle SSE correctly out of the box, and their `fly.toml` deploy model is simpler than a systemd + cloudflared combo. Rejected as primary only because we already have the Cloudflare setup working and it's cheaper at steady state.

---

## Part 5 — Resident agent productization

### 5.1 Configurable system prompt

**Today.** Hardcoded system prompt, one deployment (`claude-agent-plan.md §6`). Fine for a single reference agent; stops scaling the moment we want two residents with different personas.

**Production.** Operator-provided `RESIDENT_SYSTEM_PROMPT` env var as the deployment-wide default, with a **per-conversation override** stored in conversation metadata (added via a `metadata` field on `CreateConversation`). The resident reads `conversation.metadata.system_prompt` if present, falls back to the env var otherwise. Rationale: the hardcoded-prompt assumption collapses as soon as "personas" matter; moving it to metadata keeps the knob where it belongs (per-conversation) rather than adding a fifth Postgres table for a deployment concern.

### 5.2 Loop guard

**Today.** None. Two resident agents in the same conversation infinite-loop on each other until one of them gets pushed out of the Anthropic concurrency semaphore (`claude-agent-plan.md §0`). We explicitly chose not to give the resident agency to leave — blast radius is too large — so the design assumes a human operator will notice and `RemoveMember` one of them.

**Production.** A **cycle detector** in the resident: if it sees N consecutive self↔peer ping-pong exchanges within a short window (default 5 messages in 60 s), it **emits a `message_abort(loop_detected)` on any in-progress reply, auto-`Leave`s the conversation, and alerts** via the observability stack. The leave is safe because this is an operator-observable failure mode, not a model-deciding-to-leave scenario. Tunable per conversation via metadata (some legitimate workflows have tight back-and-forth).

### 5.3 Panic recovery in listener loop

**Today.** A panic in `listen()` or `onEvent()` kills one listener goroutine until the next process restart (`server-lifecycle-plan.md §7`). The agent silently stops responding to that one conversation while the rest of the service is fine.

**Production.** Wrap the retry loop with `defer recover()` — converts panics into a logged error + bounded-backoff retry of the listener, same mechanism we already use for transient S2 errors. Rationale: a panic should degrade one listener's latency, not silently kill the listener's availability. Rejected: panic-then-crash strategy (the "let it die" pattern) — only useful when a supervisor restarts the whole process, and our per-conversation listeners are cheap enough that restarting the one listener is strictly better than restarting the whole process.

### 5.4 Per-agent rate limit

**Today.** One global Anthropic semaphore with capacity 5 (`claude-agent-plan.md §7`). A runaway agent (e.g., one stuck in a fast loop with a chatty peer) can consume the whole semaphore and starve other conversations.

**Production.** **`golang.org/x/time/rate`** per-agent limiter keyed on `(agent_id, conversation_id)` — default 10 requests per minute per pair, configurable. Keep the global cap 5 as the outer bound on Anthropic concurrency. Rationale: the global cap protects Anthropic quota; the per-pair limit protects fairness across conversations under adversarial or buggy peers.

### 5.5 Multi-instance leader election

**Today.** Listener startup is synchronous and single-instance (`server-lifecycle-plan.md §6`). Two instances would both open listeners for every conversation the resident is a member of, resulting in duplicate responses.

**Production.** **Postgres advisory lock** leader election per `(resident_agent_id, conversation_id)` — `pg_try_advisory_lock(hashtext(agent_id || conversation_id))`. Only the leader runs the listener; followers hold the lock attempt open and take over if the leader's Postgres connection drops. Rejected: Consul session-based locks — adds a dependency for a feature Postgres already does well; advisory locks are transactionally consistent with the `agent_joined` row that makes this agent a member of this conversation, which is the exact invariant we want to serialize on.

### 5.6 Tool use

**Today.** None. The resident is a pure chat agent. The "`[NO_RESPONSE]` protocol dropped" decision in `claude-agent-plan.md §0` made this deliberate — the resident always responds — and fresh-stream-per-message is the endorsed Anthropic pattern for streaming writes.

**Production.** Minimal safe toolbelt — **web fetch** (behind a domain allowlist), **KB lookup** against a vector store, **conversation history search** over the local S2 replay. Each tool gated by per-agent ACLs in Postgres (new `agent_tools` table, agent_id → tool_name → policy). Rationale: this is the agent-economy inflection point. Before tool use, the resident is a demo; after, it's a product. But adding it earlier is premature because tool-use-with-streaming requires Anthropic-API-level tool orchestration (the user's founder-guidance memory captures why we avoided this for take-home).

---

## Part 6 — Observability

### 6.1 Metrics

**Today.** Structured logs via **zerolog** with `request_id`, per-request lines at info/warn/error (`http-api-layer-plan.md §17`). No numeric metrics, no aggregation, no alerting surface beyond log-search.

**Production.** **Prometheus** exporter at `/metrics` (named: `github.com/prometheus/client_golang`). Named counters and histograms:
- `agentmail_http_request_duration_seconds{endpoint, status}` — per-endpoint latency.
- `agentmail_s2_append_duration_seconds{stream}` — S2 ack latency, the single most important number for write-path health.
- `agentmail_pg_query_duration_seconds{query}` — Postgres hot-path latency.
- `agentmail_cursor_flush_batch_size` — cursor flusher backlog.
- `agentmail_conn_registry_active{kind}` — SSE and streaming-write counts per instance.
- `agentmail_resident_response_duration_seconds` — Anthropic call + append latency end-to-end.
- `agentmail_tickets_channel_depth` — write-path backpressure signal.

Rationale: Prometheus is the default in the Go ecosystem, pairs naturally with Grafana for dashboards, and its scrape-pull model is cheap on the origin (we're not pushing metrics to a collector per request).

### 6.2 Traces

**Today.** None.

**Production.** **OpenTelemetry** spans from the HTTP handler through S2 SDK + pgx. Named: `go.opentelemetry.io/otel` with an OTLP exporter to **Grafana Tempo** or **Honeycomb**. A single trace of "agent sends streaming message, resident replies" should show every span: claim, session open, each append, session close, dedup insert, head-seq update, resident listener trigger, history seed (if any), Anthropic call, response append, response dedup insert. Rationale: this is the clearest debugging surface for the hot path. Log correlation via `request_id` gets us 60% of this information, but traces make the causality graph explicit and let us click from a p99 spike straight to the slow span.

### 6.3 Exemplars

**Production.** Link Prometheus histogram buckets to OTel trace IDs so oncall can click from a p99 latency spike straight to the exemplar trace that caused it. This is a ~100-line wiring exercise once metrics + traces are both live, and it's the difference between "we know p99 is elevated" and "we know why."

### 6.4 pprof

**Today.** Optional build flag `-tags=pprof` adds `/debug/pprof` (`deployment-plan.md §9.3`). Off by default — one more step during an incident.

**Production.** **Always on, gated behind admin auth** (mTLS client cert or a static shared token in the env var `ADMIN_TOKEN`). Cheap — pprof endpoints return data only when scraped — huge return during incidents. Rationale: an off-by-default profiler means you discover it's off during the incident, which is the worst possible moment.

### 6.5 Request logging

**Today.** No request/response middleware on the CRUD hot path. Logging happens on errors and lifecycle events.

**Production.** A structured middleware that logs `status`, `latency_ms`, `agent_id`, `request_id`, and `endpoint` on every request. **Sampled at 1 % on 2xx, 100 % on 5xx** — keeps steady-state log volume bounded while preserving every failure for postmortem. Rationale: the first thing an investigation wants is "was this slow, and for whom." Without the middleware, we reconstruct this from S2 + Postgres queries; with it, it's one log query.

---

## Part 7 — Security posture

### 7.1 Auth

**Today.** `X-Agent-ID` is both identity and credential (`spec.md §53`). The UUIDv7 namespace gives us 2^122 bits of entropy, so enumeration is not a practical attack, but any party who sees an agent ID (logs, backups, network captures) has full authority to speak as that agent.

**Production.** **JWT-bearer** with an asymmetric keypair per agent — RS256 or EdDSA. The `X-Agent-ID` header moves from a trust boundary to a claim inside the signed token. Short-lived tokens (5 min) rotated via an external IAM (Auth0, Cloudflare Access, or a thin in-house token server). **mTLS** with client certs is the stronger alternative if we own both ends and want per-connection authentication; JWT is the right default when evaluators bring their own Claude Code environments and we can't mandate certs.

### 7.2 Per-conversation ACLs

**Today.** Membership equals full access (`spec.md §46`). Any member can invite, any member can leave, any member can send and read.

**Production.** Extend the `members` table with a `role` column: `owner`, `moderator`, `writer`, `reader`. Moderator can invite/remove; reader is read-only on the SSE tail and history endpoints but blocked on `POST /messages`. **Expiring invites** via a `valid_until` column on `members`, enforced at `IsMember` check time. Rationale: the current flat model works for 2-agent conversations and small trusted groups; it breaks the moment a conversation has more than a handful of members, because the first disruptive member removes everyone else. Roles are the minimal ACL primitive that makes large conversations safe.

### 7.3 Rate limits and abuse

**Today.** None (`spec.md §1509`, `http-api-layer-plan.md §1509`). A misbehaving client can open unlimited streaming writes and exhaust the `tickets` channel capacity across the fleet.

**Production.** **`golang.org/x/time/rate`** per-agent + per-conversation limiters (e.g., 100 req/min per agent, 10 streaming writes/min per `(agent, conversation)`). **Content scanning** on outbound resident responses — lightweight prompt-injection heuristics — to prevent the resident from amplifying attacks received from a peer onto the next peer it talks to. Rejected: IP-based rate limiting — agents are behind Cloudflare so the source IP is the tunnel edge, not the client.

### 7.4 Audit log

**Today.** Every membership change is already an S2 event (`agent_joined`, `agent_left`). Informal but sufficient audit trail — the events are durable, ordered, and replayable.

**Production.** A formal **Postgres audit table** with signed hashes chained Merkle-style: each row includes the hash of the previous row plus a signature from the service's audit key. Named: standard `crypto/ed25519` over a canonical JSON encoding of the row. Required for compliance readiness (SOC 2 Type II, HIPAA) where "tamper-evident" is a checklist item. S2 remains the source of truth for event history; the audit table is a cryptographic commitment on top of membership changes specifically, where tamper-evidence matters most.

---

## Part 8 — Deployment and operations

### 8.1 Single host → HA

**Today.** One binary, systemd `Restart=on-failure`, restart-in-place deploys via `make build && pkill -TERM && relaunch` with ~30 s downtime per deploy (`deployment-plan.md §5.3, §7`). Acceptable for a take-home; a 30 s brownout at every release is unacceptable for a production service.

**Production.** **2–3 instances** behind a **Cloudflare Load Balancer pool**, all sharing S2 and Postgres as external state. **Blue-green rollout** via tunnel weight shift — new version comes up on 0 % traffic, health check passes, weight shifts to 100 %. No per-instance durable state to drain because S2 is the shared log and Postgres is the shared metadata. Rationale: zero-downtime deploys are cheap once §1.1 (sticky routing) is in place, and the LB is the same infrastructure we're already using for the tunnel.

### 8.2 CI/CD

**Today.** `make build && pkill -TERM && relaunch`. No pipeline, no test gate between local and prod (`deployment-plan.md §7`).

**Production.** **GitHub Actions** pipeline:
1. Unit tests (`go test -race -count=1 ./...`).
2. Integration tests against a **throwaway Neon branch** + **ephemeral S2 basin** — Neon's branch-per-PR is a first-class feature, and S2 basins cost cents per hour; we can afford a fresh one per pipeline run.
3. Build the binary, sign with Sigstore Cosign.
4. Upload signed binary as a GitHub release artifact.
5. Automated **blue-green** deploy to the fleet via the same tunnel-weight-shift mechanism as §8.1.

Rationale: the integration-tests-against-real-infrastructure piece is the key insight — mocking S2/Postgres in CI replicates the take-home's mock mistake; real Neon branches and S2 basins are cheap enough to run on every PR.

### 8.3 IaC

**Today.** Neon branch, S2 basin, and `cloudflared` tunnel all set up via dashboards and CLI commands (`deployment-plan.md §5.1`). No versioned infrastructure definition; re-creating the stack from scratch requires manual steps from a runbook.

**Production.** **Terraform** with **Neon** and **Cloudflare** providers for the parts that have native TF support; **Pulumi** (Go SDK) for the **S2 basin** because S2 doesn't have a mature TF provider and Pulumi can call the S2 control-plane API directly. **1Password Connect** or **HashiCorp Vault** for secrets pulled into the TF state at plan time via `data.vault_generic_secret`. Rationale: keeping infra-as-code in the same language as the service (via Pulumi Go) makes the boundary between "app code" and "infra code" thin — the same type system covers both.

### 8.4 Secret management

**Today.** `.env` file locally, `EnvironmentFile=` via systemd in production (`deployment-plan.md §3`). No rotation, no key derivation, no secret-manager integration.

**Production.** **HashiCorp Vault AppRole** authentication with 1 h token TTL, secrets fetched at process start and on SIGHUP. Anthropic key rotation becomes: update the secret in Vault, send SIGHUP to each instance, done — no service restart needed. Rationale: the ops value isn't protecting the secret (our threat model is small today), it's making rotation **possible** without a deploy. Rejected: AWS Secrets Manager — same outcome, more vendor lock-in; Vault is the default for multi-cloud and on-prem hybrid.

### 8.5 Multi-region

**Today.** Single process in `us-east-1`, colocated with the Neon primary. West-coast and APAC clients pay the origin's RTT (`spec.md §1193`).

**Production.**
- **Neon read replicas** in every region we deploy to; primary stays `us-east-1`. Most hot-path reads (`AgentExists`, `IsMember`, `GetConversationHeadSeq`) tolerate replica lag because they're cache-backed; writes route back to the primary.
- **S2** is already a globally accessible managed log; streams work from any region with the same auth.
- **Origin instances per region**, each behind its own cloudflared tunnel or a shared CF Load Balancer pool. **Cloudflare Argo Smart Routing** or a **Cloudflare Worker geo-hash on `X-Agent-ID`** lands an agent's traffic on the nearest region with cache affinity.

The real lift is cursor coordination becoming cross-region (see §2.1). Achievable — Redis Enterprise has Active-Active geo-replication with conflict-free last-write-wins, which is what we want for `delivery_seq` semantics.

### 8.6 SLOs and runbooks

**Today.** None.

**Production.** SLOs:
- SSE **time-to-first-byte** p99 < 500 ms.
- Streaming write **acceptance latency** (first byte ack) p99 < 200 ms.
- History read p99 < 100 ms.
- `message_end` **cross-agent delivery latency** p99 < 1 s (write on A → SSE receipt on B).

Runbooks in `deploy/runbooks/` for the four known failure modes: **S2 incident** (ErrSlowWriter cascades, shed load), **Neon failover** (5 s primary switchover, cursor flusher blocks), **tunnel cert expiry** (rotate via `cloudflared service install`), **resident quota exhaustion** (reset Anthropic counter, temporarily lower per-agent rate). Rationale: SLOs let us hold ourselves accountable; runbooks let the on-call resolve incidents without paging the original author.

### 8.7 Config management

**Today.** Env vars with a ~10-variable surface (`server-lifecycle-plan.md §1`). Simple, correct, hard to manage across a fleet.

**Production.** **Consul** or **etcd** with a typed schema, read at startup and on SIGHUP. Keeps env-var simplicity for local dev (fallback path), centralized management for the fleet. Only worth the operational cost past ~10 instances; for 2–3 instances, env vars via Ansible-managed `.env` files are strictly better than a config service's failure modes.

---

## Part 9 — Testing and verification

### 9.1 Jepsen-style failure testing

**Today.** Unit tests with fakes, integration tests gated on live Neon + S2 + Anthropic. Race detector clean.

**Production.** **Jepsen-style failure injection** on the write path: kill the process at every point (after claim, before append, mid-stream, after append, before dedup insert) and verify idempotency holds across the retry. Named library: `github.com/jepsen-io/jepsen` is Clojure; the Go equivalent is a purpose-built harness (`chaos-mesh` for process-kill chaos, custom assertion code). The assertion is simple: after any crash + retry with the same `message_id`, S2 contains exactly one committed message or exactly zero messages (never a duplicate, never a partial).

### 9.2 S2 chaos

**Production.** Inject 5xx responses into the S2 SDK transport (`httptest.ResponseRecorder`-backed fake transport) and verify the 2 s `Submit` timeout → `ErrSlowWriter` → `message_abort(slow_writer)` path all the way to what an SSE subscriber observes. Rationale: the `ErrSlowWriter` code path is load-bearing under real S2 incidents but rarely exercised in unit tests because 2 s waits are test-unfriendly. A fake transport with sub-second injected delays makes this fast.

### 9.3 Load testing

**Today.** We don't know the actual ceiling of a single instance. Documented estimates (5 k–10 k concurrent SSE, 500–1 k streaming writes, 10 k+ CRUD req/sec, per `deployment-plan.md §8.1`) are back-of-envelope.

**Production.** **`k6`** with the **`grafana/xk6-sse`** extension for the SSE side, a custom `k6` script for NDJSON streaming writes. Named scenario: N concurrent streaming writers + M concurrent SSE subscribers, ramped to find the instance's knee. Publish the ceiling + the scaling factor per added core so capacity planning is concrete. Rationale: the estimates are useful for sizing; measurements are what's actually required for SLOs (§8.6).

### 9.4 Contract tests

**Production.** Publish the HTTP API as **OpenAPI 3.1**; CI-generated client (via **`oapi-codegen`**) runs against every PR. Schema validation in handler tests via **`kin-openapi`**. Rationale: the client documentation (`docs/CLIENT.md`) is already the de-facto contract for external agents; an OpenAPI spec makes it machine-checkable so a schema-incompatible change can't ship silently.

---

None of Part 9 would change the design. It raises confidence from "we wrote it carefully" to "we've proven it under stress."
