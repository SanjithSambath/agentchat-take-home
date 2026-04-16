# AgentMail: Deployment Configuration — Complete Design Plan

## Executive Summary

This document specifies the complete deployment configuration for AgentMail: Dockerfile (multi-stage build), `fly.toml` (Fly.io app config), secrets management, `cmd/server/main.go` (server lifecycle), health checks, and the full startup/shutdown sequence. This is Gap 6 from the design audit — the last mechanical piece that must be specified so deployment is one-shot during build.

### Why This Matters

A take-home with a broken deployment is a zero. The evaluators will:
1. Hit the live URL. If it's down, everything else is irrelevant.
2. Connect their own agents as clients. The service must be running, healthy, reachable, with the Claude agent active.
3. Potentially redeploy if they fork the repo. The Dockerfile and fly.toml must work first-try.

Deployment is "mechanical" but not trivial. A wrong decision here — wrong base image, missing env var, unhealthy health check, broken graceful shutdown — means the evaluator's first impression is "it doesn't work."

### Core Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Build strategy | Multi-stage Docker (builder + runtime) | Minimal attack surface, smallest image (~15 MB), no build tools in production |
| Base image | `golang:1.23-alpine` → `alpine:3.20` | Alpine is 5 MB. Scratch would be smaller but lacks shell, CA certs, timezone data, and `dig`/`curl` for debugging. Alpine gives us all of that at 5 MB cost. |
| Binary linking | Static (`CGO_ENABLED=0`) | No glibc dependency. Runs on any Linux. Required for alpine runtime. |
| Fly.io machine | `shared-cpu-1x`, 512 MB RAM, `iad` region | Colocated with Neon (us-east-1). Shared CPU is sufficient — our bottleneck is I/O (network to S2 and Neon), not compute. 512 MB handles ~50K concurrent goroutines. |
| Scaling | Single instance, no auto-scale | Take-home scope. Auto-scaling SSE is complex (sticky sessions, connection draining). Document the multi-instance path, don't build it. |
| Secrets | `fly secrets set` — never in Dockerfile, fly.toml, or git | Fly.io injects as environment variables at runtime. Secrets are encrypted at rest, never logged, never in image layers. |
| Health check | HTTP `GET /health` → Postgres ping + S2 connectivity | Fly.io restarts the machine if health checks fail. Must check real dependencies, not just "process is alive." |
| Internal port | 8080 | Standard non-privileged port. Fly.io's Anycast proxy handles TLS termination and routes port 443 → internal 8080. |
| Graceful shutdown | SIGTERM → 30s drain → force kill | Fly.io sends SIGTERM, waits `kill_timeout` seconds, then SIGKILL. Our shutdown sequence must complete within that window. |

---

## 1. Dockerfile — Multi-Stage Build

### The Problem

A naive `FROM golang:1.23` produces a 1.2 GB image containing the entire Go toolchain, all source code, all build caches, and the compiled binary. This image:
- Takes 60+ seconds to push to Fly.io's registry on each deploy.
- Contains the Go compiler, git, and other tools an attacker could exploit.
- Exposes source code in image layers.
- Wastes disk on the Fly.io machine (billed per GB/month on larger plans).

### Decision: Two-Stage Build

**Stage 1 (builder):** Full Go toolchain. Downloads dependencies. Compiles a static binary.
**Stage 2 (runtime):** Minimal Alpine image. Copies only the binary. Nothing else.

```dockerfile
# ============================================================
# Stage 1: Build
# ============================================================
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
    -trimpath \
    -o /bin/agentmail \
    ./cmd/server

# ============================================================
# Stage 2: Runtime
# ============================================================
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata curl

COPY --from=builder /bin/agentmail /bin/agentmail

EXPOSE 8080

ENTRYPOINT ["/bin/agentmail"]
```

### Line-by-Line Rationale

**`FROM golang:1.23-alpine AS builder`**
- Alpine-based Go image for the builder stage. Smaller download than `golang:1.23` (Debian-based, 800 MB vs 300 MB). Doesn't matter for final image size (we discard this stage), but speeds up CI builds and layer caching.
- Go 1.23 is the current stable release. Pinned to minor version, not `latest`, for reproducible builds.

**`RUN apk add --no-cache ca-certificates tzdata`**
- `ca-certificates`: Needed at build time if `go mod download` fetches from HTTPS module proxies (it does). Also copied to runtime for HTTPS calls to S2, Neon, and Anthropic.
- `tzdata`: Go's `time` package needs timezone data. We embed it in the builder so the runtime image has it via the binary's embedded tzdata (Go 1.15+ can embed timezone data with `import _ "time/tzdata"`, but having it in the filesystem is belt-and-suspenders).
- `--no-cache`: Don't store the apk index in the image layer. Saves ~5 MB.

**`COPY go.mod go.sum ./` then `RUN go mod download`**
- Layer caching optimization. `go.mod` and `go.sum` change less frequently than source code. By copying them first and running `go mod download` in a separate layer, Docker caches the dependency download. Subsequent builds that only change source code skip the download entirely.
- Without this: every source code change triggers a full dependency download (minutes, depending on network).

**`COPY . .`**
- Copies all source code. Relies on `.dockerignore` to exclude `.git`, `*.md`, `docs/`, and other non-build files (see §1.2).

**`CGO_ENABLED=0`**
- Produces a statically-linked binary. No dependency on glibc or musl. Runs on any Linux kernel. Required for running on Alpine (musl libc) and would also work on `scratch`.
- **Why not CGO?** We don't use any CGO-dependent libraries. `pgx` is pure Go. `uuid` is pure Go. `chi` is pure Go. `zerolog` is pure Go. The S2 SDK is pure Go. The Anthropic SDK is pure Go. There is zero reason to enable CGO.

**`GOOS=linux GOARCH=amd64`**
- Explicit target. Fly.io machines are Linux/amd64. If someone builds on an M-series Mac (arm64), this cross-compiles correctly.
- **Why not arm64?** Fly.io supports arm64 machines and they're cheaper. However, the S2 SDK and all our dependencies are architecture-agnostic Go. We could switch to arm64 by changing this line and the `fly.toml` machine config. For the take-home, amd64 is the default and safe choice.

**`-ldflags="-s -w -X main.version=..."`**
- `-s`: Strip symbol table. Saves ~2 MB.
- `-w`: Strip DWARF debug info. Saves ~3 MB.
- `-X main.version=...`: Inject build version at compile time. Logged on startup and returned in health check. `git describe --tags --always --dirty` gives `v1.0.0-3-g1234567-dirty` or `dev` if no git info available (e.g., building from a tarball).
- **Combined savings:** A typical Go binary drops from ~20 MB to ~12-15 MB with these flags.

**`-trimpath`**
- Removes local filesystem paths from the binary. Without this, stack traces contain `/Users/sanjith/Desktop/agentmail-take-home/internal/api/messages.go`. With it, paths are relative: `internal/api/messages.go`. Cleaner, more portable, no information leakage.

**`-o /bin/agentmail`**
- Output path in the builder stage. We copy this single file to the runtime stage.

**`./cmd/server`**
- Build target. The `cmd/server/main.go` entry point.

**`FROM alpine:3.20`**
- Minimal runtime. 5 MB base. Contains a shell (`/bin/sh`), package manager, and standard Unix utilities.
- **Why not `scratch`?** Scratch is 0 bytes — no shell, no CA certificates, no timezone data, no `curl`, no `sh`. This makes debugging impossible. You can't `fly ssh console` into a scratch container and run commands. You can't `curl` the health endpoint from inside the container. You can't inspect logs or the filesystem. For 5 MB, Alpine gives us all of that.
- **Why not `distroless`?** Google's distroless images are ~2 MB and include CA certs and timezone data but no shell. Better than scratch, but still no debugging tools. The 3 MB difference from Alpine isn't worth losing `sh` and `curl`.

**`RUN apk add --no-cache ca-certificates tzdata curl`**
- `ca-certificates`: For outbound HTTPS to S2 (s2.dev), Neon (neon.tech), Anthropic (api.anthropic.com).
- `tzdata`: Timezone data for Go's `time` package.
- `curl`: For debugging. `fly ssh console` → `curl localhost:8080/health`. Invaluable during evaluation if something breaks.

**`EXPOSE 8080`**
- Documentation only. Doesn't actually publish the port. Tells the reader (and Fly.io's auto-detection) which port the app listens on.

**`ENTRYPOINT ["/bin/agentmail"]`**
- Exec form (JSON array), not shell form. The binary receives signals directly (SIGTERM from Fly.io). With shell form (`ENTRYPOINT /bin/agentmail`), signals go to `/bin/sh`, which may not forward them, breaking graceful shutdown.

### 1.1 .dockerignore

```
.git
.gitignore
*.md
docs/
tests/
old/
.env
.env.*
fly.toml
Makefile
LICENSE
```

**Why:** Keeps the Docker build context small. Excludes:
- `.git`: Can be 100+ MB with history. Irrelevant to the build.
- `*.md`, `docs/`: Documentation. Not compiled.
- `tests/`: Test files. Not included in production binary (Go's build system excludes `_test.go` files automatically, but this keeps them out of the build context entirely).
- `.env`, `.env.*`: Local development secrets. Must never end up in an image layer.
- `fly.toml`: Fly.io config. Not needed inside the container.

### 1.2 Final Image Analysis

| Layer | Size | Contents |
|---|---|---|
| Alpine base | ~5 MB | musl libc, busybox, apk |
| ca-certificates + tzdata + curl | ~4 MB | TLS root CAs, timezone database, curl binary |
| agentmail binary | ~12-15 MB | Statically linked Go binary |
| **Total** | **~21-24 MB** | |

Compare: `golang:1.23` base image alone is 800 MB. Our full runtime image is 3% of that.

### 1.3 Build Reproducibility

**Deterministic builds require:**
1. `go.sum` checked into git (already done via `go mod tidy`).
2. Go module proxy (`GOPROXY=https://proxy.golang.org,direct`) — the default. Modules are immutable and content-addressed.
3. Pinned base image tags (`golang:1.23-alpine`, `alpine:3.20`, not `latest`).
4. `-trimpath` strips local paths.

**What breaks reproducibility:**
- `git describe` in ldflags — different commits produce different binaries. Acceptable: we want the version embedded.
- `apk add` in the runtime stage — Alpine packages update over time. For full reproducibility, pin package versions: `apk add ca-certificates=20240226-r0`. For the take-home, unpinned is fine.

---

## 2. fly.toml — Fly.io Application Configuration

### The Problem

`fly.toml` controls how Fly.io deploys, routes, scales, and monitors the application. Wrong settings here mean: requests don't route correctly, health checks fail (triggering infinite restart loops), SSE connections get killed prematurely, or streaming POST bodies get buffered (destroying real-time token streaming).

### Configuration

```toml
app = "agentmail"
primary_region = "iad"
kill_signal = "SIGTERM"
kill_timeout = 35

[build]

[env]
  PORT = "8080"

[http_service]
  internal_port = 8080
  force_https = true
  auto_stop_machines = "stop"
  auto_start_machines = true
  min_machines_running = 1

  [http_service.concurrency]
    type = "connections"
    hard_limit = 10000
    soft_limit = 8000

[[http_service.checks]]
  grace_period = "15s"
  interval = "30s"
  method = "GET"
  path = "/health"
  timeout = "5s"
  unhealthy_threshold = 3
  healthy_threshold = 1

[[services.http_checks]]

[http_service.http_options]
  h2_backend = true

[[vm]]
  size = "shared-cpu-1x"
  memory = "512mb"
```

### Line-by-Line Rationale

**`app = "agentmail"`**
- Fly.io app name. Globally unique across all Fly.io users. If `agentmail` is taken, use `agentmail-<username>` or similar.

**`primary_region = "iad"`**
- Washington, DC (us-east-1 equivalent). Colocated with Neon's default region (us-east-1, also Virginia). Network latency between our Fly.io machine and Neon: **~1-5ms**. If we picked `lax` (Los Angeles), that becomes ~60-80ms — every database query 10x slower.
- S2's endpoint (`api.s2.dev`) is anycast, but their infrastructure is likely also US East. Even if not, S2 latency is already budgeted at 10-40ms for Express tier appends.
- The evaluators are likely in the US. `iad` gives sub-50ms latency to most of the East Coast and sub-100ms to the West Coast.

**`kill_signal = "SIGTERM"`**
- The signal Fly.io sends when it wants to stop the machine (deploy, scale down, maintenance). Our Go server traps this and starts graceful shutdown. SIGTERM is the Unix standard for "please shut down gracefully." SIGKILL (untrappable) follows after `kill_timeout`.

**`kill_timeout = 35`**
- Seconds between SIGTERM and SIGKILL. Our graceful shutdown sequence (§4) needs up to 30 seconds to drain. We add 5 seconds of buffer. If shutdown hasn't completed in 35 seconds, Fly.io kills the process — better than hanging forever.
- **Why 35 and not 30?** The Go `http.Server.Shutdown()` gets a 30-second context. But there's work before and after that call (flush cursors, close pools). 35 seconds gives the full sequence room to breathe.

**`[build]`**
- Empty `[build]` section tells Fly.io to build using the Dockerfile in the repo root. No custom build args, no buildpacks.

**`[env]`**
- `PORT = "8080"`: The only non-secret environment variable. Our server reads `os.Getenv("PORT")` to determine the listen address.
- **Why not hardcode 8080 in the binary?** Because local development may use a different port, and it's a one-line env var read vs a rebuild.
- **CRITICAL: Secrets are NOT in `[env]`.** Secrets go via `fly secrets set` (§3). The `[env]` section is committed to git and visible in the Fly.io dashboard. Putting `DATABASE_URL` here would expose the Neon connection string (including password) in the repository.

**`[http_service]`**

`internal_port = 8080`: The port our Go server listens on inside the container. Fly.io's Anycast proxy terminates TLS and forwards traffic to this port.

`force_https = true`: Redirect HTTP → HTTPS. All our clients (AI agents) should use HTTPS. No reason to allow plaintext.

`auto_stop_machines = "stop"`: If no traffic for the configured idle period, Fly.io stops the machine (not destroys — it restarts on the next request). This saves money during periods of no evaluation.

`auto_start_machines = true`: When a request arrives and no machine is running, Fly.io starts one automatically. Cold start: ~2-5 seconds (pull image from registry + boot Alpine + start Go binary + health check passes).

`min_machines_running = 1`: Keep at least one machine running at all times. **Critical for the evaluation.** Without this, the evaluator's first request triggers a cold start. With it, the machine is always warm. Cost: ~$3-5/month for a shared-cpu-1x running 24/7.

**Concurrency limits:**

```toml
[http_service.concurrency]
  type = "connections"
  hard_limit = 10000
  soft_limit = 8000
```

`type = "connections"`: Concurrency is measured by active connections, not requests-per-second. This is correct for our workload: SSE connections are long-lived (minutes to hours), and each one counts as one connection.

`hard_limit = 10000`: Maximum concurrent connections before Fly.io starts rejecting new ones. Our machine can handle this — a single Go process easily manages 10K+ goroutines. The bottleneck is memory: each goroutine uses ~4 KB stack, so 10K goroutines = 40 MB. Plus per-connection state (bufio buffers, S2 session). At ~20 KB per SSE connection (conservative), 10K connections = 200 MB. Well within our 512 MB.

`soft_limit = 8000`: When connections exceed this, Fly.io would route new connections to another machine (if multi-instance). Since we're single-instance, this is informational. We set it below hard_limit as a warning threshold.

**Health checks:**

```toml
[[http_service.checks]]
  grace_period = "15s"
  interval = "30s"
  method = "GET"
  path = "/health"
  timeout = "5s"
  unhealthy_threshold = 3
  healthy_threshold = 1
```

`grace_period = "15s"`: After a machine starts, wait 15 seconds before health checking. Our startup sequence (§4.2) takes ~5-10 seconds (connect Postgres, warm caches, connect S2, start Claude agent). 15 seconds gives comfortable headroom. Without this, the health check fires immediately, the server isn't ready, it fails, Fly.io restarts the machine, repeat forever.

`interval = "30s"`: Check health every 30 seconds. Frequent enough to detect failures within a minute. Infrequent enough to not waste resources. Each health check hits Postgres and S2, so we don't want it every 5 seconds.

`method = "GET"`, `path = "/health"`: Standard HTTP health check. Our health endpoint (see http-api-layer-plan.md) checks Postgres connectivity and S2 connectivity with 3-second per-check timeouts.

`timeout = "5s"`: If the health check doesn't respond in 5 seconds, consider it failed. Our health endpoint should respond in <100ms normally (Postgres ping + S2 check). 5 seconds accommodates Neon cold starts (up to ~500ms) and S2 transient latency.

`unhealthy_threshold = 3`: Three consecutive failed checks before Fly.io considers the machine unhealthy and restarts it. This prevents a single transient failure (Neon hiccup, S2 blip) from triggering a restart. Three checks at 30-second intervals = 90 seconds of sustained failure before restart.

`healthy_threshold = 1`: One successful check after being unhealthy = machine is healthy again. No need to wait for multiple successes — if the health check passes, the dependencies are reachable.

**HTTP/2 backend:**

```toml
[http_service.http_options]
  h2_backend = true
```

`h2_backend = true`: Fly.io's proxy speaks HTTP/2 to our Go server (inside the container). This is important for:
- **SSE multiplexing:** Multiple SSE connections from the same client multiplex over a single TCP connection. Without H2, each SSE connection is a separate TCP connection from the proxy to our server.
- **Streaming POST:** HTTP/2 DATA frames are the natural transport for NDJSON streaming bodies. No chunked transfer encoding ambiguity.
- **Go's `net/http` supports HTTP/2 natively.** No code changes needed. `http.ListenAndServe` auto-negotiates H2 when TLS is not involved (Fly.io's proxy handles TLS, so the internal connection is plaintext H2 via h2c, or we let Fly manage the upgrade).

**VM sizing:**

```toml
[[vm]]
  size = "shared-cpu-1x"
  memory = "512mb"
```

`shared-cpu-1x`: Shared vCPU. Our workload is I/O-bound (waiting on S2 appends, Neon queries, Claude API). CPU usage is minimal — JSON serialization, event routing, goroutine scheduling. A dedicated CPU would be wasted money.

`memory = "512mb"`: Memory breakdown:
| Component | Estimate |
|---|---|
| Go runtime + binary | ~30 MB |
| Agent existence cache (sync.Map, 10K agents) | ~1 MB |
| Membership LRU cache (100K entries) | ~10 MB |
| Cursor hot tier (in-memory map) | ~5 MB |
| Per-SSE connection (bufio + state) | ~20 KB × 5K = 100 MB |
| Per-streaming-write (bufio + scanner) | ~10 KB × 100 = 1 MB |
| Claude agent (history buffers, semaphore state) | ~20 MB |
| Goroutine stacks (10K goroutines × 4 KB) | ~40 MB |
| **Headroom** | **~300 MB** |
| **Total** | **~507 MB** |

At 5K concurrent SSE connections (generous for a take-home), we're within budget. If we approach the limit, the first signal is goroutine scheduling latency, not OOM — Go's GC will work harder as heap pressure increases.

**Why not 256 MB?** We'd be fine for light evaluation (10 agents, 5 conversations). But if the evaluators spin up 100+ agents with concurrent streaming, 256 MB gets tight. 512 MB is the smallest comfortable size. Cost difference: ~$1.50/month.

**Why not 1 GB?** Unnecessary for the take-home. We're not running 50K concurrent SSE connections. If we needed to scale, we'd go to a dedicated CPU machine before increasing memory.

---

## 3. Secrets Management

### The Problem

The application needs four secrets to function. If any are missing, the server cannot start. If any leak (in Dockerfile, fly.toml, git history, image layers), the consequences range from data breach to account compromise.

### Complete Environment Variable Inventory

| Variable | Required | Source | Example | Used By |
|---|---|---|---|---|
| `DATABASE_URL` | Yes | `fly secrets set` | `postgresql://user:pass@ep-xxx.us-east-1.aws.neon.tech/agentmail?sslmode=require` | pgxpool — Neon PostgreSQL connection |
| `S2_ACCESS_TOKEN` | Yes | `fly secrets set` | `s2_tok_...` | S2 SDK — stream storage authentication |
| `ANTHROPIC_API_KEY` | Yes | `fly secrets set` | `sk-ant-...` | Anthropic SDK — Claude API for resident agent |
| `RESIDENT_AGENT_ID` | Yes | `fly secrets set` | `01904d3a-7e40-7f1e-...` (UUIDv7) | Stable identity for the in-process Claude agent |
| `PORT` | No (default: `8080`) | `fly.toml [env]` | `8080` | HTTP listen address |
| `S2_BASIN` | No (default: `agentmail`) | `fly.toml [env]` or `fly secrets set` | `agentmail` | S2 basin name |
| `LOG_LEVEL` | No (default: `info`) | `fly.toml [env]` | `debug` | zerolog level |

### Secret Lifecycle

**Setting secrets (one-time, before first deploy):**

```bash
fly secrets set \
  DATABASE_URL="postgresql://agentmail:PASSWORD@ep-xxx.us-east-1.aws.neon.tech/agentmail?sslmode=require" \
  S2_ACCESS_TOKEN="s2_tok_xxxxxxxxxxxxxxxxxxxx" \
  ANTHROPIC_API_KEY="sk-ant-api03-xxxxxxxxxxxxxxxx" \
  RESIDENT_AGENT_ID="01904d3a-7e40-7f1e-8000-000000000001"
```

**How Fly.io handles secrets:**
1. Secrets are encrypted at rest in Fly.io's secret store.
2. On machine start, Fly.io injects them as environment variables in the process.
3. Secrets are NOT visible in `fly.toml`, NOT in the Docker image, NOT in the build log.
4. `fly secrets list` shows secret names but NOT values. Values are write-only from the CLI.
5. Changing a secret triggers an automatic redeployment (new machine with updated env vars).

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

1. **Fail fast with ALL missing vars listed.** Don't fail on the first missing var, restart, fail on the second, repeat. Collect all missing vars and report them in one error. The operator fixes everything in one `fly secrets set` call.

2. **Validate format where possible.** `RESIDENT_AGENT_ID` must be a valid UUID. `DATABASE_URL` must parse as a connection string (pgxpool.ParseConfig does this). `PORT` must be a valid number. Catch config errors at startup, not at first request.

3. **Log config at startup (redacted).** Log the presence/absence of each var and the non-secret config values. Never log secret values.

```
INF config loaded database_url=set s2_access_token=set anthropic_api_key=set resident_agent_id=01904d3a-... port=8080 s2_basin=agentmail log_level=info
```

### Security Considerations

**Threat: Secret in Docker image layers.**
If a `RUN echo $SECRET` or `ENV DATABASE_URL=...` appears in the Dockerfile, the secret is baked into an image layer permanently — even if a subsequent layer "removes" it. Image layers are immutable and inspectable via `docker history`.

**Mitigation:** Our Dockerfile has zero references to secrets. Secrets arrive only at runtime via Fly.io's env var injection.

**Threat: Secret in git history.**
A `.env` file committed to git, even if later deleted, lives in git history forever.

**Mitigation:**
1. `.gitignore` includes `.env`, `.env.*`, `.env.local`.
2. `.dockerignore` includes `.env`, `.env.*`.
3. No secret values in any committed file.
4. The `LoadConfig()` function reads from `os.Getenv()`, not from files.

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

The `LoadConfig()` function doesn't care whether env vars come from Fly.io secrets, a `.env` file, or manual export. Same code path everywhere.

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
│     ├─ For each: append message_abort to S2 stream          │
│     ├─ Delete row from in_progress_messages                 │
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

**Why fail fast on external dependency failure:** The Fly.io health check grace period is 15 seconds. If Postgres or S2 is unreachable, it's better to exit immediately and let Fly.io restart the machine than to start in a degraded state where every request fails. The restart loop will converge once the dependency is available.

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

**Race: Fly.io sends a new request to the stopped listener.**
After `http.Server.Shutdown()` returns, the TCP listener is closed. Fly.io's proxy detects the closed connection and returns 502 to the client. The client retries (Fly.io's proxy has automatic retry for connection errors). Since we're mid-deploy, the new machine will be ready soon.

**Edge case: Shutdown takes longer than kill_timeout.**
If our 30-second shutdown exceeds the 35-second kill_timeout, Fly.io sends SIGKILL. The process dies immediately. Consequences:
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
        Str("region", os.Getenv("FLY_REGION")).
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
# 1. Install flyctl
curl -L https://fly.io/install.sh | sh
fly auth login

# 2. Create the app (one-time)
fly apps create agentmail --region iad

# 3. Provision Neon database (external — via Neon dashboard)
#    - Create project in us-east-1 (Virginia)
#    - Create database "agentmail"
#    - Get connection string from Neon dashboard
#    - Disable scale-to-zero for evaluation period

# 4. Provision S2 (external — via s2.dev)
#    - Create account, get auth token
#    - Create basin "agentmail" with Express storage class
#    - Basin config: CreateStreamOnAppend=true, TimestampAppendOnArrival=true, retention=28d

# 5. Generate resident agent ID (one-time, save it)
#    Use any UUIDv7 generator:
#    python3 -c "import uuid; print(uuid.uuid7())"
#    → 01904d3a-7e40-7f1e-8000-xxxxxxxxxxxx

# 6. Set secrets
fly secrets set \
  DATABASE_URL="postgresql://..." \
  S2_ACCESS_TOKEN="s2_tok_..." \
  ANTHROPIC_API_KEY="sk-ant-..." \
  RESIDENT_AGENT_ID="01904d3a-..."

# 7. Deploy
fly deploy
```

### 5.2 Subsequent Deploys

```bash
# Code change → deploy
fly deploy

# That's it. Fly.io:
# 1. Builds the Docker image (multi-stage)
# 2. Pushes to Fly.io registry
# 3. Starts new machine with the new image
# 4. Runs health check (GET /health)
# 5. Routes traffic to new machine
# 6. Sends SIGTERM to old machine
# 7. Old machine shuts down gracefully
```

### 5.3 Deploy Strategy: Rolling (Default)

Fly.io's default deploy strategy:
1. Start new machine with new image.
2. Wait for health check to pass.
3. Route traffic to new machine.
4. Send SIGTERM to old machine.
5. Old machine drains and shuts down.

**During the transition (~15-30 seconds):**
- CRUD requests route to either old or new machine (stateless, both are correct).
- SSE connections on the old machine stay open until the old machine shuts down. When it sends SIGTERM, step 3 of our shutdown closes them. Clients reconnect, hit the new machine, resume from `Last-Event-ID`.
- Streaming writes on the old machine: step 4 of our shutdown aborts them. The agent's scaffolding sees the connection close and can retry on the new machine.

**Zero-downtime for CRUD. Brief interruption for SSE and streaming writes.** Clients auto-recover. This is acceptable for the take-home. Production would use blue-green with connection draining.

### 5.4 Monitoring the Deploy

```bash
# Watch deploy progress
fly deploy --verbose

# Check machine status
fly status

# Check logs in real-time
fly logs

# SSH into the running container (debugging)
fly ssh console

# Inside the container:
curl localhost:8080/health
```

---

## 6. Fly.io-Specific Considerations

### 6.1 Proxy Behavior and Streaming

Fly.io's Anycast proxy sits between the internet and our machine. Critical behaviors:

**Request body streaming (NDJSON POST):**
Fly.io's proxy does NOT buffer request bodies by default. Chunked transfer encoding and HTTP/2 DATA frames pass through to the backend in real-time. Our NDJSON streaming POST works without configuration. (Unlike nginx, which defaults to `proxy_request_buffering on`.)

**Response body streaming (SSE):**
Fly.io's proxy does NOT buffer response bodies. SSE events flow through immediately. No special configuration needed. `Content-Type: text/event-stream` disables any implicit buffering.

**Connection timeout:**
Fly.io's proxy has a 60-second idle timeout for HTTP connections. For CRUD requests (sub-second), this is irrelevant. For SSE connections, we send heartbeat comments every 30 seconds:
```
: heartbeat
```
This keeps the connection alive through the proxy. Without heartbeats, a quiet conversation would see the SSE connection killed after 60 seconds of no events.

For streaming writes, the proxy's idle timeout applies to the request body. If the client stops sending NDJSON lines for 60 seconds, the proxy may close the connection. Our server-side 5-minute idle timeout is more generous, but the proxy timeout wins. **Mitigation:** Document in CLIENT.md that streaming writes should send content within 60 seconds or the connection may be closed by the infrastructure. In practice, LLM token generation is continuous — gaps of 60+ seconds don't happen during normal generation.

### 6.2 TLS Termination

Fly.io terminates TLS at the proxy. Our Go server receives plaintext HTTP on port 8080. We do NOT configure TLS in the Go server — no certificates, no key files, no `ListenAndServeTLS`. Fly.io handles certificate provisioning (Let's Encrypt), renewal, and HTTPS enforcement.

The app is accessible at `https://agentmail.fly.dev`. Custom domains can be added via `fly certs create`.

### 6.3 IPv6 and Anycast

Fly.io uses Anycast routing. The app gets a shared IPv4 address and a dedicated IPv6 address. Requests from anywhere in the world route to the nearest Fly.io edge, then to the `iad` region where our machine runs.

**For the evaluators:** The URL `https://agentmail.fly.dev` just works. No IP address management, no DNS configuration, no load balancer setup.

### 6.4 Persistent Storage

We do NOT use Fly.io volumes. Our server is stateless — all persistent state is in Neon (metadata) and S2 (messages). The machine can be destroyed and recreated without data loss. This is a deliberate design choice that enables:
- Zero-downtime deploys (new machine, kill old one).
- Auto-restart on crash (Fly.io starts a fresh machine).
- Region migration (move to a different region by changing `primary_region`).

### 6.5 Machine Restarts

Fly.io may restart machines for:
- **Deploy:** New image version. Graceful (SIGTERM → shutdown sequence).
- **Health check failure:** Three consecutive failures. Graceful (SIGTERM → shutdown sequence).
- **Host maintenance:** Fly.io moves the machine to different hardware. Graceful.
- **OOM kill:** Machine exceeds memory limit. Ungraceful (SIGKILL, no shutdown sequence). Handled by recovery sweep on next startup.

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

deploy:
	fly deploy

logs:
	fly logs

status:
	fly status

ssh:
	fly ssh console
```

**`-race` in tests:** Go's race detector catches concurrent access bugs. Essential for a system with this many goroutines. Adds ~10x slowdown to tests — acceptable for correctness.

**`-count=1`:** Disables test caching. Integration tests hit real external services; cached results are meaningless.

**`-tags=integration`:** Integration tests (hitting real S2, real Neon) are gated behind a build tag so `go test ./...` doesn't accidentally run them in CI without credentials.

---

## 8. Scaling Considerations (Documented, Not Built)

### 8.1 Single Instance Limits

Our single Fly.io shared-cpu-1x, 512 MB machine can handle:

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

1. **Scale to 2-3 instances:** `fly scale count 3`. CRUD requests load-balance automatically. Problem: SSE connections are stateful (each instance has its own connection registry). An agent's SSE might connect to instance A, but a write to the same conversation arrives at instance B. The write goes to S2 (shared), so instance A's SSE (tailing S2) sees it immediately. **This works without sticky sessions** because S2 is the shared state, not the Go process.

2. **Scale beyond 3 instances:** Membership cache invalidation becomes the challenge. Each instance has a local membership LRU cache. When agent A leaves a conversation on instance 1, instance 2's cache still says A is a member for up to 60 seconds. Solution: `LISTEN/NOTIFY` on Postgres (already designed in sql-metadata-plan.md §10) or Redis pub/sub for cache invalidation.

3. **Scale to dozens of instances:** Need distributed cursor management. Two instances flushing cursors for the same agent can conflict. Solution: Redis as cursor hot tier (single source of truth across instances), Postgres as durable tier. Designed but not built (sql-metadata-plan.md §14).

### 8.3 Cost Analysis

| Component | Monthly Cost | Notes |
|---|---|---|
| Fly.io shared-cpu-1x, 512 MB | ~$3.50 | 24/7 with min_machines_running=1 |
| Fly.io outbound bandwidth | ~$0 | 100 GB/month free tier; text messaging is tiny |
| Neon PostgreSQL (free tier) | $0 | 0.5 GB storage, 190 compute hours/month |
| S2 (free tier credits) | $0 | $10 free credits; Express tier at ~$0.001/MB stored |
| Anthropic API (provided key) | $0 | Provided by evaluators |
| **Total** | **~$3.50/month** | |

---

## 9. Edge Cases, Failure Modes, and Recovery

### 9.1 Neon Cold Start During Deploy

**Scenario:** We deploy. The new machine starts. It tries to connect to Neon. Neon's compute has been idle for 5+ minutes and is suspended.

**Impact:** Postgres connection takes ~500ms instead of ~5ms. Total startup time: ~2-3 seconds instead of ~200ms. Well within the 15-second health check grace period.

**Mitigation already in place:**
- `min_machines_running = 1` keeps the server running, so Neon gets periodic health check queries every 30 seconds. Neon never goes idle.
- If the old machine is shutting down while the new one is starting, there's a brief gap. Neon's 500ms cold start is still within the grace period.

**Belt-and-suspenders:** Disable Neon's scale-to-zero for the evaluation period via Neon dashboard settings.

### 9.2 S2 Unreachable During Deploy

**Scenario:** New machine starts. S2 is temporarily unreachable (API outage, DNS hiccup).

**Impact:** Server exits with fatal error. Fly.io sees the machine as failed, restarts it. Restart loop until S2 comes back.

**Why fail fast is correct here:** Without S2, the server cannot deliver messages, cannot stream, cannot do its core job. Starting in degraded mode would mean every request returns 503 anyway. Better to fail fast, let Fly.io retry, and converge once S2 is back.

**Alternative considered:** Start without S2, return 503 on message operations, serve metadata operations from Postgres. Rejected — adds complexity for a scenario that should last seconds (S2 has 99.99% availability per their SLA). The evaluators would see 503s and think the service is broken.

### 9.3 OOM Kill

**Scenario:** Memory usage exceeds 512 MB. Kernel sends SIGKILL (untrappable). Process dies immediately. No graceful shutdown.

**Impact:**
- Dirty cursors lost → agents re-receive up to 5 seconds of events (the flush interval). At-least-once delivery guarantee holds.
- In-progress streaming writes orphaned → recovery sweep on next startup writes `message_abort` to S2.
- Postgres connections leaked → Neon's connection killer closes them after ~60s.
- S2 sessions leaked → S2 server-side timeout closes them after ~30s.

**Mitigation:**
- Monitor memory via `fly logs` (Go runtime reports heap stats).
- If approaching limit, increase to 1 GB (`fly scale memory 1024`).
- Profile with `pprof` if the cause is a leak.

### 9.4 Deploy During Active Streaming Write

**Scenario:** Agent is mid-streaming-write (NDJSON POST, 60% of tokens sent). Deploy triggers SIGTERM on old machine.

**Sequence:**
1. SIGTERM received.
2. `http.Server.Shutdown()` — waits for in-flight requests (including the streaming write) to complete.
3. Step 4 of shutdown: `CloseAllWrites()` cancels the streaming write's context.
4. The streaming write handler detects context cancellation, appends `message_abort` to S2, returns.
5. The agent's client sees the connection close.
6. The agent can retry the message on the new machine (start a new streaming write with new content).

**The message is NOT silently lost.** It's explicitly aborted on the S2 stream. Any reader sees `message_abort` and knows the message was incomplete.

### 9.5 Deploy During Active SSE Connection

**Scenario:** 100 agents have SSE connections to the old machine. Deploy triggers SIGTERM.

**Sequence:**
1. SIGTERM received.
2. Step 3 of shutdown: `CloseAllReads()` cancels all SSE contexts.
3. Each SSE handler detects cancellation, flushes the cursor for that connection, returns.
4. The agent's SSE client sees the connection close.
5. The agent reconnects (SSE auto-reconnect or manual retry).
6. The new machine receives the reconnection.
7. The agent's `Last-Event-ID` or stored cursor positions the new SSE stream from where the old one left off.

**Zero message loss.** At-least-once delivery. The agent may re-receive a few events (between last cursor flush and disconnection). Events have sequence numbers for deduplication.

### 9.6 Concurrent Deploy and Invite

**Scenario:** Evaluator invites the Claude agent to a conversation at the exact moment a deploy is happening. Old machine is shutting down, new machine is starting up.

**Sequence:**
1. The invite request hits either the old or new machine (Fly.io routes to the healthy one).
2. If old machine: invite succeeds (Postgres write), then old machine shuts down. On the new machine, the Claude agent's startup reconciliation discovers the new conversation membership and starts listening.
3. If new machine: invite succeeds. Claude agent's invite channel receives the notification and starts listening immediately.

**No missed invites.** The Claude agent's startup reconciliation scans all current memberships — it catches everything, regardless of when the invite happened.

### 9.7 Image Registry Unavailability

**Scenario:** Fly.io's Docker registry is down. `fly deploy` fails.

**Impact:** Deploy fails. The running machine is unaffected. The service continues operating on the current version.

**Mitigation:** Retry `fly deploy` later. This is a Fly.io infrastructure issue, not ours.

### 9.8 DNS Resolution Failure

**Scenario:** The Go binary can't resolve `api.s2.dev` or `ep-xxx.neon.tech` at runtime.

**Impact:** All S2 and Postgres operations fail. Health check returns 503. After 3 failed health checks (90 seconds), Fly.io restarts the machine.

**Mitigation:** Fly.io machines use Fly.io's internal DNS resolver, which is highly available. External DNS failures (S2 or Neon DNS) are extremely rare but possible. The restart loop converges once DNS resolves.

**Belt-and-suspenders for Neon:** Use the IP address instead of hostname in `DATABASE_URL`. Not recommended (Neon may change IPs), but available as a last resort.

---

## 10. Local Development vs. Production Parity

| Aspect | Local | Production (Fly.io) |
|---|---|---|
| Binary | `go run ./cmd/server` or `make run` | Static binary in Alpine container |
| Postgres | Local Postgres or Neon dev branch | Neon production branch |
| S2 | Real S2 (no local emulator) | Real S2 |
| Anthropic | Real API (or mock for unit tests) | Real API |
| TLS | None (HTTP) | Fly.io proxy handles TLS |
| Port | 8080 (or env override) | 8080 internal, 443 external |
| Secrets | `.env` file or shell export | `fly secrets set` |
| Health checks | Manual `curl localhost:8080/health` | Fly.io automated every 30s |
| Logging | Console (pretty-printed) | JSON (machine-parseable) |

**The same binary runs everywhere.** No `#ifdef PRODUCTION`. No separate configs. The only difference is where environment variables come from and whether TLS is terminated externally. `LoadConfig()` reads `os.Getenv()` — it doesn't know or care about the environment.

---

## 11. Pre-Deploy Checklist

Before the first `fly deploy`:

- [ ] Neon database created in `us-east-1` with database `agentmail`
- [ ] Neon scale-to-zero disabled for evaluation period
- [ ] S2 account created, auth token obtained
- [ ] S2 basin `agentmail` created with Express tier, `CreateStreamOnAppend=true`, arrival timestamping, 28-day retention
- [ ] Anthropic API key available
- [ ] Resident agent UUID generated and saved
- [ ] `fly apps create agentmail --region iad` completed
- [ ] All four secrets set via `fly secrets set`
- [ ] `.dockerignore` includes `.env`, `.git`, `*.md`, `docs/`, `tests/`
- [ ] `.gitignore` includes `.env`, `.env.*`, `bin/`
- [ ] `go mod tidy` run (go.sum up to date)
- [ ] `go test ./... -race` passes locally
- [ ] `fly deploy` succeeds
- [ ] `curl https://agentmail.fly.dev/health` returns `{"status":"healthy","checks":{"postgres":"ok","s2":"ok"}}`
- [ ] `curl https://agentmail.fly.dev/agents/resident` returns the resident agent IDs
- [ ] Send a message to the Claude agent → verify it responds

---

## 12. Files to Create

```
agentmail-take-home/
├── cmd/server/main.go          # Entry point (this document §4)
├── Dockerfile                   # Multi-stage build (this document §1)
├── .dockerignore                # Build context exclusions (this document §1.1)
├── fly.toml                     # Fly.io app config (this document §2)
├── Makefile                     # Build/test/deploy targets (this document §7)
```

All five files are fully specified in this document — no design decisions remain for implementation.
