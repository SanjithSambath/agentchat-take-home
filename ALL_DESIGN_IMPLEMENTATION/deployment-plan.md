# AgentMail: Deployment Configuration — Complete Design Plan

## Executive Summary

This document specifies the complete deployment configuration for AgentMail: static Go binary (`go build ./cmd/server`), Cloudflare Tunnel as public ingress (`cloudflared`), secrets via process environment, `cmd/server/main.go` (server lifecycle), health checks, and the full startup/shutdown sequence. This is Gap 6 from the design audit — the last mechanical piece that must be specified so deployment is one-shot during build.

### Why This Matters

A take-home with a broken deployment is a zero. The evaluators will:
1. Hit the live URL. If it's down, everything else is irrelevant.
2. Connect their own agents as clients. The service must be running, healthy, reachable, with the Claude agent active.
3. Potentially redeploy if they fork the repo. `go build` and `cloudflared tunnel --url http://localhost:8080` must work first-try.

Deployment is "mechanical" but not trivial. A wrong decision here — wrong base image, missing env var, unhealthy health check, broken graceful shutdown — means the evaluator's first impression is "it doesn't work."

### Core Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Build strategy | `go build ./cmd/server` — single static binary | No container registry, no image tags, no multi-stage build. Ship the file, run the file. |
| Binary linking | Static (`CGO_ENABLED=0`) | No glibc dependency. Runs on any Linux/macOS. Trivially copyable across hosts. |
| Host | Any x86-64 or arm64 Linux/macOS box with outbound HTTPS | No inbound ports, no public IP required — the tunnel is outbound-initiated. Colocate in `us-east` to stay near Neon's `us-east-1` primary. |
| Public ingress | `cloudflared tunnel` (quick or named) | Cloudflare's edge terminates TLS and forwards HTTP/2 to `http://localhost:8080`. Free tier. No certs to manage, no reverse proxy to configure. |
| Scaling | Single instance, no auto-scale | Take-home scope. Auto-scaling SSE is complex (sticky sessions, connection draining). Document the multi-instance path via Cloudflare Load Balancer, don't build it. |
| Secrets | Process environment (`.env` locally, `EnvironmentFile` or platform secret manager in production) — never in git | The server reads `os.Getenv`. No file format dependency, no platform lock-in. |
| Health check | HTTP `GET /health` → Postgres ping + S2 connectivity | Probed externally by an uptime monitor or Cloudflare Worker cron — the tunnel itself doesn't probe upstream. Must check real dependencies, not just "process is alive." |
| Internal port | 8080 | Standard non-privileged port. Tunneled via `--url http://localhost:8080`; Cloudflare's edge exposes it as HTTPS. |
| Graceful shutdown | SIGTERM → 30s drain → force kill | Process managers (systemd, Ctrl-C, `cloudflared` killing its child) send SIGTERM. Our shutdown sequence must complete within that window. |

---

## 1. Build — `go build` Static Binary

### The Problem

A container image is overhead for this service. Cloudflare Tunnel terminates at the host's process, not at a container runtime — there is no image registry to push to, no orchestrator pulling layers, no multi-tenant host to isolate the binary from. A Dockerfile would add: a build-context tax, a registry push on each deploy, a tag strategy, and operational machinery (image scanning, layer caching, garbage collection) the take-home doesn't use. Ship the binary directly.

### Decision: Plain `go build`, CGO disabled, stripped and trimmed

```bash
CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
    -trimpath \
    -o agentmail \
    ./cmd/server
```

Output: one `./agentmail` binary (~12-15 MB on linux/amd64, similar on arm64/darwin). Copy it to the host, make it executable, run it.

### Flag-by-Flag Rationale

**`CGO_ENABLED=0`**
- Produces a statically-linked binary. No dependency on glibc or musl. Runs on any Linux kernel unchanged.
- **Why not CGO?** We don't use any CGO-dependent libraries. `pgx` is pure Go. `uuid` is pure Go. `chi` is pure Go. `zerolog` is pure Go. The S2 SDK is pure Go. The Anthropic SDK is pure Go. There is zero reason to enable CGO.

**Target OS / architecture**
- `GOOS=linux GOARCH=amd64` if building on one host and running on another (the common case). If building on the same host you'll run on, omit both — `go build` uses the host's defaults.
- All our dependencies are architecture-agnostic pure Go, so arm64 works identically (`GOARCH=arm64`); pick whatever matches your host.

**`-ldflags="-s -w -X main.version=..."`**
- `-s`: Strip symbol table. Saves ~2 MB.
- `-w`: Strip DWARF debug info. Saves ~3 MB.
- `-X main.version=...`: Inject build version at compile time. Logged on startup and returned in health check. `git describe --tags --always --dirty` gives `v1.0.0-3-g1234567-dirty` or `dev` if no git info available (e.g., building from a tarball).
- **Combined savings:** A typical Go binary drops from ~20 MB to ~12-15 MB with these flags.

**`-trimpath`**
- Removes local filesystem paths from the binary. Without this, stack traces contain `/Users/sanjith/Desktop/agentmail-take-home/internal/api/messages.go`. With it, paths are relative: `internal/api/messages.go`. Cleaner, more portable, no information leakage.

**`-o agentmail`**
- Output path. Plain filename — copy it wherever you want (`/usr/local/bin/agentmail`, `~/bin/agentmail`, or leave it in the repo root).

**`./cmd/server`**
- Build target. The `cmd/server/main.go` entry point.

### 1.1 What the binary needs at runtime

The static binary has no shared-library dependencies, but the host still needs:

- **TLS root CAs** — for outbound HTTPS to S2 (`s2.dev`), Neon (`neon.tech`), Anthropic (`api.anthropic.com`). Any reasonable Linux/macOS install has these in `/etc/ssl/certs/ca-certificates.crt` or equivalent; no extra work required.
- **Timezone data** — Go's `time` package uses the system's zoneinfo (or falls back to `time/tzdata` if imported). We don't import `time/tzdata` explicitly because every mainstream host provides it.
- **`cloudflared` binary** — the public-ingress companion process. See §2.
- **A process supervisor** (systemd, launchd, `tmux`, or `nohup` for the demo case) — to restart the binary if it exits and to feed it the process environment.

No container runtime, no shell on the path, no `curl` dependency inside a container. The binary is invoked directly by the supervisor.

### 1.2 Final artifact analysis

| Piece | Size | Contents |
|---|---|---|
| `agentmail` binary | ~12-15 MB | Statically linked Go binary, stripped, trimpath'd |
| `cloudflared` binary (on host) | ~36 MB (prebuilt darwin/arm64; linux builds similar) | Cloudflare's tunnel client; one-time install, not rebuilt per deploy |
| **Deploy artifact** | **~12-15 MB** | Just the `agentmail` binary — `cloudflared` is a one-time host setup |

For comparison: a two-stage Docker image (Alpine + the same binary) would be ~21-24 MB and require a registry. We save the registry round-trip and the image layer tax on every deploy.

### 1.3 Build reproducibility

**Deterministic builds require:**
1. `go.sum` checked into git (already done via `go mod tidy`).
2. Go module proxy (`GOPROXY=https://proxy.golang.org,direct`) — the default. Modules are immutable and content-addressed.
3. Pinned Go version via `go.mod`'s `go` directive (and ideally matched on the build host).
4. `-trimpath` strips local paths.

**What breaks reproducibility:**
- `git describe` in ldflags — different commits produce different binaries. Acceptable: we want the version embedded.
- The build host's Go toolchain version — upgrading from `go1.22` to `go1.23` can change binary output deterministically but differently. Pin via `go.mod` or a CI `setup-go` action with an exact version to control this.

---

## 2. Cloudflared Tunnel — Public Ingress Configuration

### The Problem

`cloudflared` is the companion process that brokers public traffic to our `localhost:8080` listener. Wrong settings here mean: tunnel auth fails, the tunnel flaps on deploy, SSE connections get killed prematurely by the edge's idle timeout, or streaming POST bodies get buffered (destroying real-time token streaming).

### Two modes: quick vs named

**Quick tunnel** — zero-config, ephemeral URL, but **does not support SSE reliably**.

```bash
cloudflared tunnel --url http://localhost:8080
```

Prints a `https://<random>.trycloudflare.com` URL on startup. No login, no DNS, no persisted config. The URL dies when the process exits.

**Known defect for our workload.** Quick tunnels do **not** honor the `disableChunkedEncoding: false` flag that named tunnels respect, and Cloudflare's edge coalesces small chunks unpredictably on these ephemeral URLs. SSE event frames (~100 bytes each) and `: heartbeat` comments (~14 bytes) get buffered until some internal threshold is met. Measured: local SSE delivers the `:ok` handshake + first event in ~500 ms; the same endpoint through a quick tunnel delivered **0 bytes over 35 s**. Every `run_agent.py` daemon that connects through a quick tunnel stalls on its SSE tail and never sees peer messages.

Consequence: **do not use `make tunnel` for the take-home evaluation window or any flow where an external agent connects.** The Makefile target is retained for offline dev only and prints a WARNING on invocation.

**ngrok free tier** — recommended dev/demo transport.

```bash
# One-time
brew install ngrok
ngrok config add-authtoken <token>

# Per session
make ngrok    # prints https://<random>.ngrok-free.app
```

ngrok's free-tier HTTP tunnels pass chunked/streaming responses through without coalescing, so SSE works reliably. The URL is ephemeral (random per session) and free-tier rate limits (~40 inbound req/min) don't bite SSE (one long GET) or NDJSON writes (one POST per voice turn). Full setup instructions in `deploy/ngrok.md`.

**Named tunnel** — persistent, your own domain.

**Named tunnel** — persistent, your own domain.

```bash
cloudflared tunnel login                         # one-time browser auth
cloudflared tunnel create agentmail              # creates a tunnel + credentials file
cloudflared tunnel route dns agentmail agentmail.<your-domain>.com
cloudflared tunnel run --config ~/.cloudflared/agentmail.yml agentmail
```

With a config file:

```yaml
# ~/.cloudflared/agentmail.yml
tunnel: agentmail
credentials-file: /home/agentmail/.cloudflared/<tunnel-uuid>.json

ingress:
  - hostname: agentmail.example.com
    service: http://localhost:8080
    originRequest:
      noTLSVerify: false
      connectTimeout: 5s
      tlsTimeout: 5s
      tcpKeepAlive: 30s
      keepAliveConnections: 100
      keepAliveTimeout: 90s
      httpHostHeader: agentmail.example.com
      disableChunkedEncoding: false
  - service: http_status:404
```

### Line-by-Line Rationale

**`tunnel: agentmail`**
- Named tunnel identifier. Cloudflare-account-scoped, not globally unique (unlike a Fly app name). Multiple tunnels can coexist on the same account — create one per environment (`agentmail-prod`, `agentmail-staging`) if needed.

**Region choice — colocate with Neon, not the tunnel**
- There is no `primary_region` directive: Cloudflare's edge is a global anycast network, not a region choice. What matters is where the **origin** (our Go binary) lives, because that's where Postgres round-trips happen.
- Colocate the origin host in `us-east-1` (AWS Virginia) — same as Neon's default region. Expected origin-to-Neon latency: **~1-5 ms**. Moving the origin to `us-west-2` makes that ~60-80 ms — every database query 10× slower.
- S2's endpoint (`api.s2.dev`) is anycast; latency depends on the origin host's geography too. US-East keeps both paths fast.
- Cloudflare's edge is already close to every evaluator regardless of where they are — the public URL they hit terminates at the nearest PoP, and Cloudflare's backbone carries the request to our origin.

**Signal handling — SIGTERM for graceful shutdown**
- `cloudflared` is a Cloudflare-maintained binary; we don't configure its signal handling. What *we* configure is how our Go server reacts when the orchestrator (systemd, launchd, `tmux` shell, `docker stop`, or a foreground Ctrl-C) sends SIGTERM.
- SIGTERM triggers the graceful shutdown sequence in §4. SIGKILL (untrappable) follows if the process manager decides we've exceeded its kill timeout.

**Origin timeout and connection pooling**
- `connectTimeout: 5s`: How long `cloudflared` waits to open a TCP connection to `http://localhost:8080`. Our binary listens on the same host — the connection is loopback, so this is effectively "is the Go server up?" 5 s is generous for loopback; 1 s would work.
- `tlsTimeout: 5s`: Only relevant if the origin URL is `https://`. Ours is `http://` (loopback), so this is unused. Leaving the default documents intent.
- `tcpKeepAlive: 30s`: Send TCP keepalives on the tunnel↔origin socket every 30 s. Keeps NAT/firewall state warm and detects dead origins. Matches our SSE heartbeat cadence (see http-api-layer-plan.md § SSE heartbeats).
- `keepAliveConnections: 100`: Cap on idle pooled connections cloudflared keeps open to the origin. At 5K concurrent SSE clients we'll have 5K live connections regardless; the pool is only for idle CRUD connections. 100 is plenty.
- `keepAliveTimeout: 90s`: Close idle pooled origin connections after 90 s. Slightly longer than Cloudflare's default 100 s client-side idle timeout so our side isn't the first to tear down.

**Chunked transfer and streaming**
- `disableChunkedEncoding: false`: **Critical.** Chunked transfer encoding is how our NDJSON POST and SSE GET work over HTTP/1.1. Leaving this false lets the body flow through in real time. If we ever set it to `true`, streaming breaks: cloudflared would need a Content-Length, which we don't know up front.
- HTTP/2 (the negotiated transport between cloudflared and our Go server) uses DATA frames, which don't have the chunking concept — but the setting still gates whether cloudflared will buffer to add Content-Length. Leave it false.
- `httpHostHeader: agentmail.example.com`: Rewrites the `Host:` header on the origin request. Our server doesn't vhost on `Host`, but it does log it; setting it explicitly gives consistent logs.

**Health checks — external, not tunnel-internal**

Cloudflare Tunnel does not probe upstream health itself (unlike Fly's `[[http_service.checks]]` or an AWS target group). If our origin is down, the tunnel returns 502 Bad Gateway to clients. We wire health monitoring externally:

- **Option A — uptime monitor.** UptimeRobot or Pingdom on a 1-minute cadence hitting `https://agentmail.example.com/health`. Free tier handles it.
- **Option B — Cloudflare Worker cron.** A Worker that runs every minute, fetches `/health`, and pages via PagerDuty/Slack on failure. Costs $5/mo for the Workers Paid plan.

Our `GET /health` endpoint (http-api-layer-plan.md §health) checks Postgres connectivity and S2 connectivity with 3-second per-check timeouts, returning 200 on both-ok and 503 otherwise.

### Idle timeout and SSE heartbeats

Cloudflare's edge idle timeout for HTTP connections is **100 s** by default. SSE connections that don't send a byte for 100 s will be closed by Cloudflare. Our heartbeat cadence (30 s, see http-api-layer-plan.md § SSE) keeps every active SSE well under that ceiling.

For named tunnels, Cloudflare documents a **524 Origin Time-out** if the tunnel origin doesn't respond within 100 s. That's a *request-level* timeout, not an idle-connection timeout — for streaming bodies, as long as the origin keeps emitting bytes, the 524 doesn't fire.

### HTTP/2 backend

`cloudflared` speaks HTTP/2 to the origin over a TLS loopback connection internally (even though our origin URL is `http://`; cloudflared can negotiate h2c). HTTP/2 matters for:

- **SSE multiplexing:** Multiple SSE connections from the same client multiplex over a single TCP connection at the edge↔cloudflared hop. Our local cloudflared↔Go server hop is just loopback TCP — H2 there is a nice-to-have, not a requirement.
- **Streaming POST:** HTTP/2 DATA frames are the natural transport for NDJSON streaming bodies. No chunked transfer encoding ambiguity.
- **Go's `net/http` supports HTTP/2 natively.** No code changes needed. `http.ListenAndServe` handles H1 by default; cloudflared connects over H1 unless we explicitly enable h2c on the Go listener, which is unnecessary for loopback.

### Host sizing

There's no `[[vm]]` stanza — the host is whatever machine we chose to run on. Our target:

| Component | Estimate |
|---|---|
| Go runtime + `agentmail` binary | ~30 MB |
| Agent existence cache (sync.Map, 10K agents) | ~1 MB |
| Membership LRU cache (100K entries) | ~10 MB |
| Cursor hot tier (in-memory map) | ~5 MB |
| Per-SSE connection (bufio + state) | ~20 KB × 5K = 100 MB |
| Per-streaming-write (bufio + scanner) | ~10 KB × 100 = 1 MB |
| Claude agent (history buffers, semaphore state) | ~20 MB |
| Goroutine stacks (10K goroutines × 4 KB) | ~40 MB |
| `cloudflared` resident footprint | ~40 MB |
| **Headroom** | **~260 MB** |
| **Total** | **~507 MB** |

Any host with 512 MB+ RAM and an x86-64 or arm64 CPU fits comfortably. A small EC2 `t4g.nano` (arm64, 0.5 GB) or equivalent is a good cloud target; a laptop or spare VM works for the demo.

**Why not 256 MB?** Fine for light evaluation (10 agents, 5 conversations). But 100+ agents with concurrent streaming gets tight — the `cloudflared` footprint alone is ~40 MB before our workload. 512 MB is the smallest comfortable size.

**Why not 1 GB?** Unnecessary for the take-home. We're not running 50K concurrent SSE connections. If we needed to scale, we'd add instances behind a Cloudflare Load Balancer before adding RAM to one host.

---

## 3. Secrets Management

### The Problem

The application needs four secrets to function. If any are missing, the server cannot start. If any leak (in build flags, committed `.env` files, git history, log lines), the consequences range from data breach to account compromise.

### Complete Environment Variable Inventory

| Variable | Required | Source | Example | Used By |
|---|---|---|---|---|
| `DATABASE_URL` | Yes | process env (`.env` → systemd `EnvironmentFile=` → cloud secret manager) | `postgresql://user:pass@ep-xxx.us-east-1.aws.neon.tech/agentmail?sslmode=require` | pgxpool — Neon PostgreSQL connection |
| `S2_ACCESS_TOKEN` | Yes | process env | `s2_tok_...` | S2 SDK — stream storage authentication |
| `ANTHROPIC_API_KEY` | Yes | process env | `sk-ant-...` | Anthropic SDK — Claude API for resident agent |
| `RESIDENT_AGENT_ID` | Yes | process env | `01904d3a-7e40-7f1e-...` (UUIDv7) | Stable identity for the in-process Claude agent |
| `PORT` | No (default: `8080`) | process env | `8080` | HTTP listen address |
| `S2_BASIN` | No (default: `agentmail`) | process env | `agentmail` | S2 basin name |
| `LOG_LEVEL` | No (default: `info`) | process env | `debug` | zerolog level |

### Secret Lifecycle

**Setting secrets (one-time, before the process starts):**

```bash
# Option A — local .env file (gitignored), sourced before `go run` or the binary.
cat > .env <<'EOF'
DATABASE_URL=postgresql://agentmail:PASSWORD@ep-xxx.us-east-1.aws.neon.tech/agentmail?sslmode=require
S2_ACCESS_TOKEN=s2_tok_xxxxxxxxxxxxxxxxxxxx
ANTHROPIC_API_KEY=sk-ant-api03-xxxxxxxxxxxxxxxx
RESIDENT_AGENT_ID=01904d3a-7e40-7f1e-8000-000000000001
EOF
set -o allexport && source .env && set +o allexport
./agentmail

# Option B — systemd service with EnvironmentFile, file permissions 0600, owner = service user.
#   /etc/systemd/system/agentmail.service
#   [Service]
#   EnvironmentFile=/etc/agentmail/env
#   ExecStart=/usr/local/bin/agentmail

# Option C — cloud secret manager (AWS SSM Parameter Store, GCP Secret Manager, HashiCorp Vault).
# Fetch at launch, export to env, exec the binary. No secrets on disk.
```

**How the process reads secrets:**
1. Exit the process: `cfg := LoadConfig()` calls `os.Getenv(...)` for each variable.
2. There is no secret store embedded in the binary — it's entirely driven by the parent process's environment.
3. Secrets are never logged (only key *names* are logged during startup, see "Log config at startup (redacted)" below).
4. Changing a secret means updating the environment and restarting the process (`systemctl restart agentmail`, `kill -TERM <pid>` then relaunch, etc.). Graceful shutdown drains in-flight work first (§4.3).

### Validation on Startup

```go
type Config struct {
    DatabaseURL    string
    S2AccessToken  string
    AnthropicKey   string
    ResidentAgentID uuid.UUID
    Port           string
    S2Basin        string
    LogLevel       string
}

func LoadConfig() (Config, error) {
    cfg := Config{
        Port:     envOrDefault("PORT", "8080"),
        S2Basin:  envOrDefault("S2_BASIN", "agentmail"),
        LogLevel: envOrDefault("LOG_LEVEL", "info"),
    }

    var missing []string

    cfg.DatabaseURL = os.Getenv("DATABASE_URL")
    if cfg.DatabaseURL == "" {
        missing = append(missing, "DATABASE_URL")
    }

    cfg.S2AccessToken = os.Getenv("S2_ACCESS_TOKEN")
    if cfg.S2AccessToken == "" {
        missing = append(missing, "S2_ACCESS_TOKEN")
    }

    cfg.AnthropicKey = os.Getenv("ANTHROPIC_API_KEY")
    if cfg.AnthropicKey == "" {
        missing = append(missing, "ANTHROPIC_API_KEY")
    }

    raw := os.Getenv("RESIDENT_AGENT_ID")
    if raw == "" {
        missing = append(missing, "RESIDENT_AGENT_ID")
    } else {
        id, err := uuid.Parse(raw)
        if err != nil {
            return cfg, fmt.Errorf("RESIDENT_AGENT_ID is not a valid UUID: %w", err)
        }
        cfg.ResidentAgentID = id
    }

    if len(missing) > 0 {
        return cfg, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
    }

    return cfg, nil
}
```

**Key design decisions:**

1. **Fail fast with ALL missing vars listed.** Don't fail on the first missing var, restart, fail on the second, repeat. Collect all missing vars and report them in one error. The operator fixes everything in one edit of the `.env` or secret-manager entry.

2. **Validate format where possible.** `RESIDENT_AGENT_ID` must be a valid UUID. `DATABASE_URL` must parse as a connection string (pgxpool.ParseConfig does this). `PORT` must be a valid number. Catch config errors at startup, not at first request.

3. **Log config at startup (redacted).** Log the presence/absence of each var and the non-secret config values. Never log secret values.

```
INF config loaded database_url=set s2_access_token=set anthropic_api_key=set resident_agent_id=01904d3a-... port=8080 s2_basin=agentmail log_level=info
```

### Security Considerations

**Threat: Secret baked into the binary.**
If a `-ldflags="-X main.dbURL=$DATABASE_URL"` appears in the build script, the secret lands in the compiled binary's read-only data section — extractable with `strings ./agentmail | grep postgres`.

**Mitigation:** Our `go build` invocation (§1) only sets `main.version`. Secrets are read at startup via `os.Getenv()`, never at build time.

**Threat: Secret in git history.**
A `.env` file committed to git, even if later deleted, lives in git history forever.

**Mitigation:**
1. `.gitignore` includes `.env`, `.env.*`, `.env.local`.
2. No secret values in any committed file.
3. The `LoadConfig()` function reads from `os.Getenv()`, not from files. The binary itself never opens `.env`.
4. If deploying via systemd, the `EnvironmentFile=` path lives outside the git-tracked repo (`/etc/agentmail/env`).

**Threat: Secret in build logs.**
`go build -ldflags="-X main.dbURL=$DATABASE_URL"` would log the secret in the build output.

**Mitigation:** We never embed secrets in ldflags. Only `main.version` (the git hash) is injected at build time.

**Threat: Secret exposure via /health or error messages.**
A health check that returns `{"error":"failed to connect to postgresql://user:pass@host/db"}` leaks the connection string.

**Mitigation:** Health check errors return generic status (`"postgres": "unhealthy"`) without connection details. Internal logs include the error but logs are not exposed via the API.

### Local Development

For local development, use a `.env` file (gitignored) with a tool like `direnv` or manual export:

```bash
export DATABASE_URL="postgresql://localhost:5432/agentmail?sslmode=disable"
export S2_ACCESS_TOKEN="s2_tok_dev_..."
export ANTHROPIC_API_KEY="sk-ant-..."
export RESIDENT_AGENT_ID="01904d3a-7e40-7f1e-8000-000000000001"
export PORT="8080"
```

The `LoadConfig()` function doesn't care whether env vars come from a `.env` file, a systemd `EnvironmentFile=`, a cloud secret manager, or manual export. Same code path everywhere.

---

## 4. Server Lifecycle — `cmd/server/main.go`

### The Problem

This is the entry point. It's responsible for:
1. Parsing configuration.
2. Connecting to all external dependencies (Postgres, S2).
3. Running one-time setup (schema migration, cache warming, crash recovery).
4. Starting the HTTP server and the Claude agent.
5. Handling graceful shutdown on SIGTERM.

If startup order is wrong, the server crashes on the first request. If shutdown is wrong, connections get severed, cursors are lost, and in-progress messages are orphaned.

### 4.1 Startup Sequence

```
┌─────────────────────────────────────────────────────────────┐
│                    STARTUP SEQUENCE                          │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  1. Load config (env vars)                                  │
│     ├─ Validate all required vars present                   │
│     ├─ Parse RESIDENT_AGENT_ID as UUID                      │
│     └─ FAIL FAST if anything missing/invalid                │
│                                                             │
│  2. Initialize logger                                       │
│     ├─ Set level from LOG_LEVEL                             │
│     └─ Log startup banner (version, region, port)           │
│                                                             │
│  3. Connect to PostgreSQL (Neon)                            │
│     ├─ pgxpool.ParseConfig(DATABASE_URL)                    │
│     ├─ Set pool config (MaxConns=15, MinConns=5, etc.)      │
│     ├─ pgxpool.NewWithConfig(ctx, poolConfig)               │
│     ├─ pool.Ping(ctx) — verify connectivity                 │
│     └─ FAIL FAST if unreachable (with 10s timeout)          │
│                                                             │
│  4. Run schema migration                                    │
│     ├─ Embedded SQL: CREATE TABLE IF NOT EXISTS × 5 tables  │
│     ├─ Idempotent: safe to run on every startup             │
│     └─ FAIL FAST if migration fails (schema incompatible)   │
│                                                             │
│  5. Warm caches                                             │
│     ├─ Agent existence cache: SELECT id FROM agents → sync.Map │
│     ├─ At 10K agents: ~50ms. At 1M agents: ~500ms.         │
│     └─ LOG count: "warmed agent cache: 1234 agents"         │
│                                                             │
│  6. Initialize S2 client                                    │
│     ├─ s2.NewClient(S2_ACCESS_TOKEN)                        │
│     ├─ Verify basin exists: ListBasins or CheckTail on a    │
│     │  known stream                                         │
│     └─ FAIL FAST if S2 unreachable (with 10s timeout)       │
│                                                             │
│  7. Recovery sweep                                          │
│     ├─ SELECT * FROM in_progress_messages                   │
│     ├─ For each row:                                        │
│     │   ├─ Append message_abort to S2 stream                │
│     │   ├─ INSERT messages_dedup (conv, mid, 'aborted')     │
│     │   │  ON CONFLICT DO NOTHING  — so retries see 409     │
│     │   │  already_aborted (spec.md §1.3, s2 §10)           │
│     │   └─ DELETE FROM in_progress_messages WHERE …         │
│     └─ LOG count: "recovered N orphaned messages"           │
│                                                             │
│  8. Initialize stores and caches                            │
│     ├─ Cursor cache (in-memory hot tier)                    │
│     ├─ Membership cache (LRU, 100K entries, 60s TTL)        │
│     └─ Connection registry (empty at startup)               │
│                                                             │
│  9. Build HTTP router                                       │
│     ├─ Chi router with middleware chain                     │
│     ├─ Register all route groups                            │
│     └─ NotFound / MethodNotAllowed handlers                 │
│                                                             │
│  10. Ensure resident agent exists                           │
│      ├─ INSERT INTO agents ON CONFLICT DO NOTHING           │
│      │  (idempotent: same ID across restarts)               │
│      └─ Add to agent existence cache                        │
│                                                             │
│  11. Start Claude agent                                     │
│      ├─ Startup reconciliation: list conversations agent    │
│      │  is a member of → start SSE listener per convo       │
│      ├─ Start invite channel consumer                       │
│      └─ LOG: "Claude agent started, listening on N convos"  │
│                                                             │
│  12. Start cursor flush goroutine                           │
│      ├─ Ticker: every 5 seconds, flush dirty cursors to PG  │
│      └─ Runs until shutdown signal                          │
│                                                             │
│  13. Start HTTP server                                      │
│      ├─ http.Server{Addr: ":"+PORT, Handler: router}       │
│      ├─ ListenAndServe in a goroutine                       │
│      └─ LOG: "server listening on :8080"                    │
│                                                             │
│  14. Block on shutdown signal                               │
│      ├─ signal.Notify(sigCh, SIGINT, SIGTERM)               │
│      └─ <-sigCh → begin graceful shutdown                   │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

### 4.2 Startup Timing Budget

| Step | Expected Duration | Failure Mode |
|---|---|---|
| Load config | <1ms | Missing env var → exit 1 |
| Init logger | <1ms | Never fails |
| Connect Postgres | 1-5ms (warm) / 500ms (Neon cold start) | Unreachable → exit 1 after 10s |
| Schema migration | <50ms | Incompatible schema → exit 1 |
| Warm agent cache | 50ms (10K agents) / 500ms (1M agents) | Postgres error → exit 1 |
| Init S2 client | 50-200ms (TCP + TLS handshake) | Unreachable → exit 1 after 10s |
| Recovery sweep | 1-50ms (typically 0 rows) | S2 unreachable → exit 1 |
| Init caches | <1ms | Never fails (empty initialization) |
| Build router | <1ms | Never fails |
| Ensure resident agent | 1-5ms | Postgres error → exit 1 |
| Start Claude agent | 50-500ms (SSE connections to S2) | Non-fatal (agent starts degraded, retries) |
| Start flush goroutine | <1ms | Never fails |
| Start HTTP server | <1ms | Port in use → exit 1 |
| **Total** | **~200ms (warm) / ~2s (cold)** | |

**Why fail fast on external dependency failure:** An external uptime monitor probes `/health` every minute. If Postgres or S2 is unreachable, it's better to exit immediately and let the process supervisor (systemd `Restart=on-failure`, launchd `KeepAlive`, or a plain shell loop) restart the binary than to start in a degraded state where every request fails. The restart loop will converge once the dependency is available.

**Exception: Claude agent.** If S2 is reachable but the Claude agent can't start listeners for some conversations (transient S2 error on specific streams), the agent starts in degraded mode — it listens to the conversations it can reach and retries the rest. The HTTP server is still fully functional for external agents. The Claude agent's degradation doesn't affect the core messaging service.

### 4.3 Graceful Shutdown Sequence

```
┌─────────────────────────────────────────────────────────────┐
│                   SHUTDOWN SEQUENCE                          │
│                                                             │
│  Trigger: SIGTERM received (or SIGINT for local dev)        │
│  Budget: 30 seconds (kill_timeout=35s minus 5s buffer)      │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  1. LOG: "shutdown signal received, draining..."            │
│     └─ Set global "shutting down" flag                      │
│                                                             │
│  2. Stop accepting new connections                          │
│     ├─ http.Server.Shutdown(ctx) — 30s deadline             │
│     ├─ Stops accepting new TCP connections immediately      │
│     ├─ Existing CRUD requests: waits for them to complete   │
│     │  (up to 30s timeout — they already have 30s timeouts) │
│     └─ Note: does NOT close hijacked connections (SSE) or   │
│        streaming POST bodies — those need explicit handling  │
│                                                             │
│  3. Close all SSE connections                               │
│     ├─ ConnRegistry.CloseAllReads()                         │
│     ├─ For each active SSE: cancel context → goroutine      │
│     │  exits → triggers cursor flush for that connection     │
│     ├─ Wait for all SSE goroutines to exit (5s timeout)     │
│     └─ LOG: "closed N SSE connections"                      │
│                                                             │
│  4. Abort all in-progress streaming writes                  │
│     ├─ ConnRegistry.CloseAllWrites()                        │
│     ├─ For each active streaming write: cancel context →    │
│     │  handler writes message_abort to S2 → handler exits   │
│     ├─ Wait for all write goroutines to exit (5s timeout)   │
│     └─ LOG: "aborted N streaming writes"                    │
│                                                             │
│  5. Stop Claude agent                                       │
│     ├─ agent.Shutdown()                                     │
│     ├─ Cancel all listener goroutines                       │
│     ├─ Wait for any in-flight Claude responses to complete  │
│     │  or abort (5s timeout)                                │
│     └─ LOG: "Claude agent stopped"                          │
│                                                             │
│  6. Stop cursor flush goroutine                             │
│     ├─ Cancel the ticker context                            │
│     └─ Wait for the goroutine to exit                       │
│                                                             │
│  7. Final cursor flush                                      │
│     ├─ CursorCache.FlushAll() — write ALL dirty cursors     │
│     │  to Postgres in a single batch                        │
│     ├─ This catches cursors dirtied during step 3           │
│     └─ LOG: "flushed N cursors"                             │
│                                                             │
│  8. Close S2 client                                         │
│     ├─ Close any remaining S2 sessions                      │
│     └─ LOG: "S2 client closed"                              │
│                                                             │
│  9. Close Postgres pool                                     │
│     ├─ pool.Close() — waits for in-use connections to       │
│     │  return, then closes all                              │
│     └─ LOG: "Postgres pool closed"                          │
│                                                             │
│  10. LOG: "shutdown complete"                               │
│      └─ os.Exit(0)                                          │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

### 4.4 Shutdown Edge Cases and Race Conditions

**Race: New request arrives during shutdown step 2.**
`http.Server.Shutdown()` stops the listener immediately. No new TCP connections are accepted. Any request that was mid-flight (headers received, body not yet read) continues processing. This is correct.

**Race: SSE connection starts just before shutdown.**
Between step 1 (signal received) and step 3 (close all SSE), a new SSE connection could establish. The connection registry tracks it, and step 3's `CloseAllReads()` catches it. No leak.

**Race: Streaming write starts just before shutdown.**
Same as above — ConnRegistry catches it in step 4.

**Race: Cursor flush in step 7 fails (Postgres already closed).**
Step 7 happens BEFORE step 9 (close Postgres pool). The pool is still available. This ordering is critical.

**Race: Claude agent is mid-response during shutdown.**
Step 5 gives the agent 5 seconds to finish any in-flight Claude API call. If the Claude response is still streaming after 5 seconds, the agent cancels the context (which triggers `message_abort` on S2 via the streaming write handler's abort path). The message is properly aborted, not orphaned.

**Race: Cloudflared forwards a new request to the stopped listener.**
After `http.Server.Shutdown()` returns, the TCP listener is closed. `cloudflared` detects the closed loopback connection and returns 502 Bad Gateway to the Cloudflare edge, which surfaces to the client. The client retries; the next instance (or the same one after restart) picks it up.

**Edge case: Shutdown takes longer than the supervisor's kill timeout.**
If our 30-second shutdown exceeds the supervisor's kill timeout (systemd default: 90 s — plenty; Docker default: 10 s — too tight, override with `--stop-timeout 35`), the supervisor sends SIGKILL. The process dies immediately. Consequences:
- Dirty cursors NOT flushed → agents re-receive a few events on reconnect (at-least-once delivery, which is our documented guarantee).
- In-progress streaming writes NOT aborted → recovery sweep on next startup handles this.
- S2 sessions NOT cleanly closed → S2 server-side timeout closes them (~30s).
- Postgres connections NOT cleanly closed → pgxpool connections time out server-side (~30s), or Neon's connection killer handles it.

**None of these are data-loss scenarios.** The system self-heals on restart. This is by design — the recovery sweep, cursor persistence, and at-least-once delivery guarantee work together to make unclean shutdown safe.

### 4.5 Main Function Structure

```go
func main() {
    // 1. Config
    cfg, err := LoadConfig()
    if err != nil {
        fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
        os.Exit(1)
    }

    // 2. Logger
    log := initLogger(cfg.LogLevel)
    log.Info().
        Str("version", version).
        Str("port", cfg.Port).
        Str("hostname", os.Getenv("HOSTNAME")).
        Msg("starting agentmail")

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    // 3. Postgres
    pool, err := connectPostgres(ctx, cfg.DatabaseURL)
    if err != nil {
        log.Fatal().Err(err).Msg("failed to connect to postgres")
    }
    defer pool.Close()

    // 4. Migration
    if err := runMigration(ctx, pool); err != nil {
        log.Fatal().Err(err).Msg("failed to run migration")
    }

    // 5. Warm caches
    store := store.New(pool)
    agentCount, err := store.WarmAgentCache(ctx)
    if err != nil {
        log.Fatal().Err(err).Msg("failed to warm agent cache")
    }
    log.Info().Int("agents", agentCount).Msg("warmed agent cache")

    // 6. S2 client
    s2Client, err := connectS2(cfg.S2AccessToken, cfg.S2Basin)
    if err != nil {
        log.Fatal().Err(err).Msg("failed to connect to s2")
    }

    // 7. Recovery sweep
    recovered, err := recoverOrphanedMessages(ctx, store, s2Client)
    if err != nil {
        log.Fatal().Err(err).Msg("failed to recover orphaned messages")
    }
    if recovered > 0 {
        log.Warn().Int("count", recovered).Msg("recovered orphaned messages")
    }

    // 8-9. Router
    handler := api.NewHandler(store, s2Client, log)
    router := api.NewRouter(handler)

    // 10. Resident agent
    if err := store.EnsureAgentExists(ctx, cfg.ResidentAgentID); err != nil {
        log.Fatal().Err(err).Msg("failed to ensure resident agent")
    }

    // 11. Claude agent
    agent := agent.New(cfg.ResidentAgentID, cfg.AnthropicKey, store, s2Client, handler.InviteCh(), log)
    if err := agent.Start(ctx); err != nil {
        log.Error().Err(err).Msg("Claude agent start failed (non-fatal, will retry)")
    }

    // 12. Cursor flush
    go store.CursorCache().RunFlushLoop(ctx, 5*time.Second)

    // 13. HTTP server
    srv := &http.Server{
        Addr:              ":" + cfg.Port,
        Handler:           router,
        ReadHeaderTimeout: 10 * time.Second,
        IdleTimeout:       120 * time.Second,
    }

    go func() {
        log.Info().Str("addr", srv.Addr).Msg("server listening")
        if err := srv.ListenAndServe(); err != http.ErrServerClosed {
            log.Fatal().Err(err).Msg("server error")
        }
    }()

    // 14. Wait for shutdown signal
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
    sig := <-sigCh

    log.Info().Str("signal", sig.String()).Msg("shutdown signal received")

    shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer shutdownCancel()

    // Shutdown sequence
    if err := srv.Shutdown(shutdownCtx); err != nil {
        log.Error().Err(err).Msg("http server shutdown error")
    }

    handler.ConnRegistry().CloseAllReads()
    handler.ConnRegistry().CloseAllWrites()

    agent.Shutdown()

    cancel() // stops cursor flush loop

    if err := store.CursorCache().FlushAll(shutdownCtx); err != nil {
        log.Error().Err(err).Msg("final cursor flush failed")
    }

    pool.Close()
    log.Info().Msg("shutdown complete")
}
```

### 4.6 HTTP Server Configuration Edge Cases

**`ReadHeaderTimeout: 10 * time.Second`**
Without this, a client that opens a TCP connection and sends nothing (slowloris attack) holds the connection open indefinitely, consuming a goroutine and a file descriptor. 10 seconds is generous for legitimate clients.

**`IdleTimeout: 120 * time.Second`**
How long to keep an idle keep-alive connection open. 120 seconds is higher than the default (no idle timeout in Go's http.Server) but still bounds resource usage. For SSE connections, idle timeout doesn't apply — they're not idle, they're streaming.

**No `WriteTimeout` or `ReadTimeout` set at server level.**
We deliberately do NOT set these. SSE connections and streaming POST bodies are long-lived — a global read/write timeout would kill them. Instead, per-route timeouts are handled by the middleware chain (30s for CRUD, handler-managed for streaming).

**`MaxHeaderBytes` not explicitly set (default: 1 MB).**
Go's default is 1 MB for headers. Sufficient. We don't receive large headers — the biggest is `X-Agent-ID` (a UUID, 36 bytes) and `Last-Event-ID` (an int64, up to 20 digits).

---

## 5. Deploy Procedure

### 5.1 First-Time Setup

```bash
# 1. Install cloudflared (macOS: Homebrew; Linux: direct download)
brew install cloudflared                         # macOS
# OR (linux amd64):
#   curl -L -o cloudflared https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64
#   chmod +x cloudflared && sudo mv cloudflared /usr/local/bin/

# 2. Provision Neon database (external — via Neon dashboard)
#    - Create project in us-east-1 (Virginia)
#    - Create database "agentmail"
#    - Get connection string from Neon dashboard
#    - Disable scale-to-zero for evaluation period

# 3. Provision S2 (external — via s2.dev)
#    - Create account, get auth token
#    - Create basin "agentmail" with Express storage class
#    - Basin config: CreateStreamOnAppend=true, TimestampAppendOnArrival=true, retention=28d

# 4. Generate resident agent ID (one-time, save it)
#    Use any UUIDv7 generator:
#    python3 -c "import uuid; print(uuid.uuid7())"
#    → 01904d3a-7e40-7f1e-8000-xxxxxxxxxxxx

# 5. Populate .env (gitignored)
cat > .env <<'EOF'
DATABASE_URL=postgresql://...
S2_ACCESS_TOKEN=s2_tok_...
ANTHROPIC_API_KEY=sk-ant-...
RESIDENT_AGENT_ID=01904d3a-...
EOF

# 6. Build the binary
CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(git describe --always)" -trimpath -o agentmail ./cmd/server

# 7. Run the server (export env first)
set -o allexport && source .env && set +o allexport
./agentmail &

# 8. Start the tunnel (quick mode for the demo)
cloudflared tunnel --url http://localhost:8080
# Prints: https://<random>.trycloudflare.com  — share this URL with evaluators.
```

### 5.2 Subsequent Deploys

```bash
# Code change → rebuild → restart
git pull
CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(git describe --always)" -trimpath -o agentmail ./cmd/server

# Graceful restart: SIGTERM the running process, then relaunch.
pkill -TERM -f '^./agentmail$'   # or: systemctl restart agentmail
./agentmail &

# The cloudflared tunnel stays up throughout — it just starts returning 502
# for the ~1 s between the old process closing the listener and the new one
# opening it, then resumes. No DNS changes, no URL rotation.
```

### 5.3 Deploy Strategy: Restart-in-Place (Default)

For a single-instance deployment with cloudflared, the default deploy is simply "restart the binary":

1. Build the new binary next to the old one (atomic via rename).
2. Send SIGTERM to the running process — graceful shutdown begins (§4.3).
3. Wait for the process to exit (up to 30 s).
4. Launch the new binary; it listens on the same port; cloudflared reconnects to the origin and resumes forwarding.

**During the transition (~30 seconds):**
- CRUD requests in-flight complete on the old process (HTTP server graceful shutdown waits for them).
- New CRUD requests during the window: cloudflared → 502 → client retry → new process picks up.
- SSE connections on the old process close when step 3 of shutdown fires. Clients reconnect, resume from `Last-Event-ID` against the new process.
- Streaming writes: step 4 of shutdown aborts them on S2. The agent's scaffolding sees the abort and retries.

**Brief interruption for all transports.** Clients auto-recover. For zero-downtime, see the horizontal-scaling path in FUTURE.md §1 — run N instances behind Cloudflare Load Balancer and roll one at a time.

### 5.4 Monitoring the Deploy

```bash
# Tail the server's own logs (zerolog writes structured JSON to stderr)
journalctl -u agentmail -f              # if installed as a systemd unit
# OR, for a plain background process:
tail -f agentmail.log

# Tail the tunnel's logs
# (cloudflared runs in the foreground by default — watch its stderr)

# Verify the public URL responds
curl https://<your-tunnel>.trycloudflare.com/health

# Verify directly on the origin (skips Cloudflare)
curl http://localhost:8080/health
```

---

## 6. Cloudflare-Specific Considerations

### 6.1 Proxy Behavior and Streaming

Cloudflare's edge (plus the `cloudflared` tunnel client) sits between the internet and our origin. Critical behaviors:

**Request body streaming (NDJSON POST):**
Cloudflare does NOT buffer request bodies when the `Content-Type` is streaming-compatible and the body uses chunked transfer encoding or HTTP/2 DATA frames. Our NDJSON streaming POST works without additional configuration provided `disableChunkedEncoding: false` in the tunnel config (§2). (Unlike nginx, which defaults to `proxy_request_buffering on`.)

**Response body streaming (SSE):**
Cloudflare does NOT buffer `text/event-stream` responses. SSE events flow through immediately. The `Content-Type: text/event-stream` header is what signals this to Cloudflare's edge; no Worker-level tweak needed.

**Connection timeout:**
Cloudflare's edge has a **100-second idle timeout** for HTTP connections (documented as the 524 Origin Time-out). For CRUD requests (sub-second), this is irrelevant. For SSE connections, we send heartbeat comments every 30 seconds:
```
: heartbeat
```
This keeps the connection alive through the edge. Without heartbeats, a quiet conversation would see the SSE connection killed after 100 seconds of no events.

For streaming writes, the 100 s applies to origin response time — as long as the server keeps writing bytes of the acknowledgment response or the request body keeps arriving, the timer resets. **Mitigation:** Document in CLIENT.md that streaming writes should send content within 100 seconds or the connection may be closed by the infrastructure. In practice, LLM token generation is continuous — gaps of 100+ seconds don't happen during normal generation.

### 6.2 TLS Termination

Cloudflare terminates TLS at the edge. `cloudflared` forwards plaintext HTTP to our loopback origin on port 8080 (or negotiates h2c). We do NOT configure TLS in the Go server — no certificates, no key files, no `ListenAndServeTLS`. Cloudflare handles edge certificate provisioning, renewal, and HTTPS enforcement automatically for any hostname proxied through a named tunnel; quick tunnels use Cloudflare's wildcard `*.trycloudflare.com` cert.

The app is accessible at `https://<random>.trycloudflare.com` (quick mode) or `https://agentmail.<your-domain>.com` (named mode). Custom domains are configured via `cloudflared tunnel route dns` once the DNS zone is on Cloudflare.

### 6.3 IPv6 and Anycast

Cloudflare uses a massive anycast network — every `*.trycloudflare.com` or named-tunnel hostname resolves to addresses that route to the nearest Cloudflare PoP. Because our origin is behind a tunnel, the origin has **no public IP at all**: traffic terminates at Cloudflare's edge and travels over the outbound-initiated tunnel to our host.

**For the evaluators:** The URL `https://<tunnel>.trycloudflare.com` (or the named-tunnel hostname) just works. No IP address management, no DNS configuration on our side beyond `cloudflared tunnel route dns`, no load balancer setup.

### 6.4 Persistent Storage

We do NOT use any host-attached persistent volumes. Our server is stateless — all persistent state is in Neon (metadata) and S2 (messages). The host can be destroyed and recreated without data loss. This is a deliberate design choice that enables:
- Near-zero-downtime deploys (new process in place of old one; cloudflared auto-reconnects).
- Auto-restart on crash (supervisor spawns a fresh process; the tunnel auto-reconnects).
- Host migration (move the binary + `.env` to a different VM in a different region; update `cloudflared` credentials, point DNS — no in-place "region migration" primitive to configure).

### 6.5 Process Restarts

The binary may restart for:
- **Deploy:** New build. Graceful (supervisor sends SIGTERM → shutdown sequence).
- **Uptime-monitor alert-driven restart:** An external monitor sees `/health` fail N times in a row and an operator (or an auto-remediation hook) sends SIGTERM. Graceful.
- **Host maintenance:** Cloud-provider host reboot, kernel upgrade, etc. Usually graceful (SIGTERM from the init system) but occasionally abrupt (power cycle).
- **OOM kill:** Process exceeds host memory limit. Ungraceful (SIGKILL, no shutdown sequence). Handled by recovery sweep on next startup.

In all cases, our startup sequence (§4.1) restores full functionality. The recovery sweep handles any orphaned state from ungraceful kills.

---

## 7. Makefile

```makefile
.PHONY: build run test deploy

build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o bin/agentmail ./cmd/server

run: build
	./bin/agentmail

test:
	go test ./... -v -race -count=1

test-integration:
	go test ./tests/... -v -race -count=1 -tags=integration

lint:
	golangci-lint run ./...

sqlc:
	sqlc generate

deploy: build
	# Restart-in-place: send SIGTERM to the running binary, then relaunch.
	-pkill -TERM -f '^./bin/agentmail$$'
	./bin/agentmail &

tunnel:
	cloudflared tunnel --url http://localhost:8080

logs:
	# Replace with your actual log source (systemd journal, file, Docker, etc.).
	tail -f agentmail.log

status:
	curl -fsS http://localhost:8080/health | jq .
```

**`-race` in tests:** Go's race detector catches concurrent access bugs. Essential for a system with this many goroutines. Adds ~10x slowdown to tests — acceptable for correctness.

**`-count=1`:** Disables test caching. Integration tests hit real external services; cached results are meaningless.

**`-tags=integration`:** Integration tests (hitting real S2, real Neon) are gated behind a build tag so `go test ./...` doesn't accidentally run them in CI without credentials.

---

## 8. Scaling Considerations (Documented, Not Built)

### 8.1 Single Instance Limits

A single host running `agentmail` + `cloudflared` with 512 MB RAM and a shared vCPU can handle:

| Metric | Capacity | Bottleneck |
|---|---|---|
| Concurrent SSE connections | ~5,000-10,000 | Memory (20 KB per connection) |
| Concurrent streaming writes | ~500-1,000 | Memory + S2 session overhead |
| CRUD requests/sec | ~10,000+ | CPU (JSON marshal/unmarshal) |
| Postgres queries/sec | ~30,000+ | pgxpool 15 connections, sub-ms queries |
| Active conversations | Unlimited | S2 handles stream storage, we just route |

**For the take-home evaluation:** The evaluators will test with maybe 10-50 agents and 10-20 conversations. We're at <1% capacity. The system will feel instantaneous.

### 8.2 Multi-Instance Path (Documented)

When a single instance isn't enough:

1. **Scale to 2-3 instances behind Cloudflare Load Balancer.** Run the binary on N hosts, each with its own `cloudflared` tunnel (or a shared Cloudflare Load Balancer pool routing to N origin servers). CRUD requests load-balance automatically. Problem: SSE connections are stateful (each instance has its own connection registry). An agent's SSE might connect to instance A, but a write to the same conversation arrives at instance B. The write goes to S2 (shared), so instance A's SSE (tailing S2) sees it immediately. **This works without sticky sessions** because S2 is the shared state, not the Go process.

2. **Scale beyond 3 instances:** Membership cache invalidation becomes the challenge. Each instance has a local membership LRU cache. When agent A leaves a conversation on instance 1, instance 2's cache still says A is a member for up to 60 seconds. Solution: `LISTEN/NOTIFY` on Postgres (already designed in sql-metadata-plan.md §10) or Redis pub/sub for cache invalidation.

3. **Scale to dozens of instances:** Need distributed cursor management. Two instances flushing cursors for the same agent can conflict. Solution: Redis as cursor hot tier (single source of truth across instances), Postgres as durable tier. Designed but not built (sql-metadata-plan.md §14).

### 8.3 Cost Analysis

| Component | Monthly Cost | Notes |
|---|---|---|
| Origin host (e.g. AWS `t4g.nano` arm64, 0.5 GB, `us-east-1`) | ~$3.00 | 24/7; equivalent on any cloud. Free on a spare VM / laptop. |
| Cloudflare Tunnel (free tier) | $0 | Unlimited bandwidth, unlimited tunnels; 100k-req/mo rate-limiting rule free |
| Outbound bandwidth to Cloudflare | $0 | Cloudflare doesn't charge for egress from origin through the tunnel |
| Neon PostgreSQL (free tier) | $0 | 0.5 GB storage, 190 compute hours/month |
| S2 (free tier credits) | $0 | $10 free credits; Express tier at ~$0.001/MB stored |
| Anthropic API (provided key) | $0 | Provided by evaluators |
| **Total** | **~$3.00/month** | Or $0 if the origin host is a spare machine / existing VM |

---

## 9. Edge Cases, Failure Modes, and Recovery

### 9.1 Neon Cold Start During Deploy

**Scenario:** We deploy. The new machine starts. It tries to connect to Neon. Neon's compute has been idle for 5+ minutes and is suspended.

**Impact:** Postgres connection takes ~500ms instead of ~5ms. Total startup time: ~2-3 seconds instead of ~200ms. Well within a normal uptime-monitor probe cadence.

**Mitigation already in place:**
- The process is supposed to run 24/7, so Neon gets periodic `/health` queries from the external uptime monitor every minute. Neon never goes idle.
- If the old process is shutting down while the new one is starting, there's a brief gap. Neon's 500ms cold start is short compared to the shutdown window.

**Belt-and-suspenders:** Disable Neon's scale-to-zero for the evaluation period via Neon dashboard settings.

### 9.2 S2 Unreachable During Deploy

**Scenario:** New process starts. S2 is temporarily unreachable (API outage, DNS hiccup).

**Impact:** Server exits with fatal error. The supervisor sees the process exited non-zero and restarts it (systemd `Restart=on-failure`, `KeepAlive true` in launchd, or a shell `while true; do ./agentmail; done` loop). Restart loop until S2 comes back. External health monitor alerts after N failed probes.

**Why fail fast is correct here:** Without S2, the server cannot deliver messages, cannot stream, cannot do its core job. Starting in degraded mode would mean every request returns 503 anyway. Better to fail fast, let the supervisor retry, and converge once S2 is back.

**Alternative considered:** Start without S2, return 503 on message operations, serve metadata operations from Postgres. Rejected — adds complexity for a scenario that should last seconds (S2 has 99.99% availability per their SLA). The evaluators would see 503s and think the service is broken.

### 9.3 OOM Kill

**Scenario:** Memory usage exceeds the host's RAM ceiling. Kernel sends SIGKILL (untrappable). Process dies immediately. No graceful shutdown.

**Impact:**
- Dirty cursors lost → agents re-receive up to 5 seconds of events (the flush interval). At-least-once delivery guarantee holds.
- In-progress streaming writes orphaned → recovery sweep on next startup writes `message_abort` to S2.
- Postgres connections leaked → Neon's connection killer closes them after ~60s.
- S2 sessions leaked → S2 server-side timeout closes them after ~30s.

**Mitigation:**
- Monitor memory via the process's own zerolog output (Go runtime reports heap stats via `runtime.ReadMemStats` if we emit them) or host-level telemetry (`free -h`, `top`, Prometheus `node_exporter`).
- If approaching limit, resize the host (bigger VM, swap up) and restart.
- Profile with `pprof` if the cause is a leak — the binary exposes `/debug/pprof` when built with `-tags=pprof`.

### 9.4 Deploy During Active Streaming Write

**Scenario:** Agent is mid-streaming-write (NDJSON POST, 60% of tokens sent). Deploy triggers SIGTERM on the old process.

**Sequence:**
1. SIGTERM received.
2. `http.Server.Shutdown()` — waits for in-flight requests (including the streaming write) to complete.
3. Step 4 of shutdown: `CloseAllWrites()` cancels the streaming write's context.
4. The streaming write handler detects context cancellation, appends `message_abort` to S2, returns.
5. The agent's client sees the connection close.
6. The agent can retry the message against the new process (start a new streaming write with new content).

**The message is NOT silently lost.** It's explicitly aborted on the S2 stream. Any reader sees `message_abort` and knows the message was incomplete.

### 9.5 Deploy During Active SSE Connection

**Scenario:** 100 agents have SSE connections to the old process. Deploy triggers SIGTERM.

**Sequence:**
1. SIGTERM received.
2. Step 3 of shutdown: `CloseAllReads()` cancels all SSE contexts.
3. Each SSE handler detects cancellation, flushes the cursor for that connection, returns.
4. The agent's SSE client sees the connection close.
5. The agent reconnects (SSE auto-reconnect or manual retry).
6. The new process receives the reconnection.
7. The agent's `Last-Event-ID` or stored cursor positions the new SSE stream from where the old one left off.

**Zero message loss.** At-least-once delivery. The agent may re-receive a few events (between last cursor flush and disconnection). Events have sequence numbers for deduplication.

### 9.6 Concurrent Deploy and Invite

**Scenario:** Evaluator invites the Claude agent to a conversation at the exact moment a deploy is happening. Old process is shutting down, new process is starting up.

**Sequence:**
1. The invite request hits whichever process currently owns the listener. In a single-instance deploy there's a ~1 s gap where the old listener has closed and the new one hasn't bound — the client gets a 502 from cloudflared and retries.
2. On retry, the new process is listening: invite succeeds (Postgres write). Claude agent's invite channel receives the notification and starts listening immediately.
3. If the invite happened *before* the old process closed its listener, the write landed in Postgres. On the new process, the Claude agent's startup reconciliation discovers the new conversation membership and starts listening.

**No missed invites.** The Claude agent's startup reconciliation scans all current memberships — it catches everything, regardless of when the invite happened.

### 9.7 Cloudflare API / Tunnel Control-Plane Unavailability

**Scenario:** Cloudflare's tunnel control plane has a transient outage. `cloudflared` loses its connection to Cloudflare's edge.

**Impact:** The tunnel is down — the public hostname returns 502 (no available origin). The running `agentmail` process is unaffected; requests directly to `localhost:8080` (if you're on the host) still work.

**Mitigation:** `cloudflared` auto-reconnects with exponential backoff. Cloudflare's tunnel control plane has a 99.99% SLA on their enterprise plan; the free tier has best-effort availability but in practice sees the same uptime. No action needed — the tunnel comes back on its own.

### 9.8 DNS Resolution Failure

**Scenario:** The Go binary can't resolve `api.s2.dev` or `ep-xxx.neon.tech` at runtime.

**Impact:** All S2 and Postgres operations fail. Health check returns 503. External uptime monitor alerts after N failed probes.

**Mitigation:** The host's system resolver is the authoritative DNS path. Use a reliable resolver (`systemd-resolved`, `1.1.1.1`, or whatever the cloud provider supplies) and configure DNS caching (`nscd` or the systemd cache) so short outages don't cascade. External DNS failures (S2 or Neon DNS) are extremely rare but possible. The supervisor's restart loop converges once DNS resolves.

**Belt-and-suspenders for Neon:** Use the IP address instead of hostname in `DATABASE_URL`. Not recommended (Neon may change IPs), but available as a last resort.

---

## 10. Local Development vs. Production Parity

| Aspect | Local | Production (Cloudflare Tunnel) |
|---|---|---|
| Binary | `go run ./cmd/server` or `make run` | Static `./agentmail` binary, supervised by systemd / launchd / shell loop |
| Postgres | Local Postgres or Neon dev branch | Neon production branch |
| S2 | Real S2 (no local emulator) | Real S2 |
| Anthropic | Real API (or mock for unit tests) | Real API |
| TLS | None (HTTP) | Cloudflare edge handles TLS; tunnel forwards plaintext HTTP to origin |
| Port | 8080 (or env override) | 8080 on loopback; public via HTTPS on the Cloudflare hostname |
| Secrets | `.env` file or shell export | `.env` / `EnvironmentFile=` / cloud secret manager (same `os.Getenv` code path) |
| Health checks | Manual `curl localhost:8080/health` | External uptime monitor or Cloudflare Worker cron, 1-minute cadence |
| Logging | Console (pretty-printed) | JSON (machine-parseable) |

**The same binary runs everywhere.** No `#ifdef PRODUCTION`. No separate configs. The only difference is where environment variables come from and whether TLS is terminated externally. `LoadConfig()` reads `os.Getenv()` — it doesn't know or care about the environment.

---

## 11. Pre-Deploy Checklist

Before the first public launch:

- [ ] Neon database created in `us-east-1` with database `agentmail`
- [ ] Neon scale-to-zero disabled for evaluation period
- [ ] S2 account created, auth token obtained
- [ ] S2 basin `agentmail` created with Express tier, `CreateStreamOnAppend=true`, arrival timestamping, 28-day retention
- [ ] Anthropic API key available
- [ ] Resident agent UUID generated and saved
- [ ] `cloudflared` installed on the origin host (`brew install cloudflared` on macOS, or direct download on Linux)
- [ ] `.env` populated with all four required secrets (`DATABASE_URL`, `S2_ACCESS_TOKEN`, `ANTHROPIC_API_KEY`, `RESIDENT_AGENT_ID`)
- [ ] `.gitignore` includes `.env`, `.env.*`, `bin/`, `agentmail` (the built binary)
- [ ] `go mod tidy` run (go.sum up to date)
- [ ] `go test ./... -race` passes locally
- [ ] `CGO_ENABLED=0 go build -o agentmail ./cmd/server` produces a runnable binary
- [ ] Process starts: `./agentmail` logs `starting agentmail` without errors
- [ ] `curl http://localhost:8080/health` returns `{"status":"healthy","checks":{"postgres":"ok","s2":"ok"}}`
- [ ] `cloudflared tunnel --url http://localhost:8080` prints a `trycloudflare.com` URL (or a named-tunnel hostname resolves)
- [ ] `curl https://<tunnel-url>/health` returns the same healthy response through the tunnel
- [ ] `curl https://<tunnel-url>/agents/resident` returns the resident agent IDs
- [ ] Send a message to the Claude agent → verify it responds

---

## 12. Files to Create

```
agentmail-take-home/
├── cmd/server/main.go          # Entry point (this document §4)
├── .env.example                # Env var template (secrets omitted)
├── Makefile                    # Build/test/tunnel targets (this document §7)
```

All three files are fully specified in this document — no design decisions remain for implementation.

`cloudflared` config lives on the origin host (e.g. `~/.cloudflared/agentmail.yml` for named tunnels) and is not checked into this repo; the quick-tunnel path needs no config file at all.
