# AgentMail: Server Lifecycle — Complete Design Plan

## Executive Summary

This document specifies the complete design for `cmd/server/main.go` — the entry point, configuration, startup sequence, graceful shutdown, and health check for the AgentMail server. This is the skeleton everything hangs on: every other component (S2, Postgres, API handlers, Claude agent) is initialized, wired, and torn down here.

### Why This Matters

A server without a designed lifecycle makes ad-hoc decisions during implementation: what order to initialize, what to do when Postgres is unreachable, how to drain SSE connections, what signals to catch. These decisions compound into a fragile startup that fails mysteriously and a shutdown that leaks resources or loses data. The server lifecycle is the frame — get it wrong and the painting falls off the wall.

### Core Decisions

- **Configuration:** Environment variables only. No config files, no flags. 6 required, 4 optional with sensible defaults. Fail-fast on missing required vars — don't discover a missing `DATABASE_URL` ten seconds into startup after you've already connected to S2.
- **Startup sequence:** Strict dependency order. Postgres first (migration + cache warming), S2 second (client + recovery sweep), cursor flush third, Claude agent fourth, HTTP server last. Each step has explicit failure handling: fail-fast for unrecoverable errors, log-and-continue for degraded-but-functional states.
- **Graceful shutdown:** SIGINT/SIGTERM → stop accepting connections → drain in-flight requests → shut down Claude agent → cancel all SSE/streaming connections → flush cursors → close S2 → close Postgres → exit. Hard deadline: 30 seconds. Anything still running after 30 seconds is force-killed.
- **Health endpoint:** Deep check (Postgres + S2 connectivity), structured JSON response, 5-second timeout. Bypasses agent auth. Probed by an external uptime monitor (or Cloudflare Worker cron) since the Cloudflare tunnel itself does not probe upstream health.

---

## 1. Configuration — Environment Variables

### The Complete Set

Every environment variable the server reads, with its type, requirement level, default value, and validation rules:

```text
┌──────────────────────────┬──────────┬──────────┬─────────────────┬───────────────────────────┐
│ Variable                 │ Type     │ Required │ Default         │ Validation                │
├──────────────────────────┼──────────┼──────────┼─────────────────┼───────────────────────────┤
│ DATABASE_URL             │ string   │ Yes      │ —               │ Valid Postgres DSN        │
│ S2_AUTH_TOKEN            │ string   │ Yes      │ —               │ Non-empty                 │
│ PORT                     │ uint16   │ No       │ 8080            │ 1–65535                   │
│ LOG_LEVEL                │ string   │ No       │ info            │ debug|info|warn|error     │
│ SHUTDOWN_TIMEOUT_SECONDS │ uint     │ No       │ 30              │ 5–300                     │
│ RESIDENT_AGENT_ID        │ UUID     │ No       │ (empty)         │ Valid UUIDv7 if present   │
│ ANTHROPIC_API_KEY        │ string   │ Cond.    │ —               │ Required if RESIDENT_     │
│                          │          │          │                 │ AGENT_ID is set           │
│ S2_BASIN                 │ string   │ No       │ agentmail       │ Non-empty, valid S2 name  │
│ CURSOR_FLUSH_INTERVAL_S  │ uint     │ No       │ 5               │ 1–60                      │
│ HEALTH_CHECK_TIMEOUT_S   │ uint     │ No       │ 5               │ 1–30                      │
└──────────────────────────┴──────────┴──────────┴─────────────────┴───────────────────────────┘
```

### Variable-by-Variable Rationale

**`DATABASE_URL` (required)**

The PostgreSQL connection string. Format: `postgresql://user:password@host:port/dbname?sslmode=require`.

Why required with no default: A default like `localhost:5432` would silently connect to a local Postgres that has nothing to do with the production system. Fail-fast on missing connection string prevents the "it worked on my machine" class of deployment bugs.

Validation: Attempt `pgxpool.ParseConfig(url)` — the pgx library validates the DSN format before any network call. If the DSN is malformed (wrong scheme, missing host, invalid options), it fails immediately with a descriptive error. This catches typos in the process environment before the server spends 30 seconds timing out on a bad connection.

**`S2_AUTH_TOKEN` (required)**

The S2 bearer token. Required because the server cannot function without S2 — it's the entire message storage layer.

Validation: non-empty string. The S2 SDK validates the token on first API call. We don't pre-validate the token format because S2 doesn't document it — it's opaque.

**`PORT` (optional, default 8080)**

The TCP port the HTTP server listens on.

Why 8080, not 80: Cloudflare's tunnel forwards `https://<hostname>` (TLS-terminated at Cloudflare's edge) to whatever loopback port we tell it to (`cloudflared tunnel --url http://localhost:8080`). The internal port doesn't matter to external clients. 8080 is the convention for non-privileged HTTP servers. Using port 80 would require root privileges in some environments.

Why `$PORT` is configurable: Some environments set `PORT` for us (certain PaaS runtimes, local dev tooling). Our default is 8080; anyone can override via the environment without rebuilding.

**`LOG_LEVEL` (optional, default "info")**

Controls zerolog's global log level.

Levels:
- `debug`: Every Postgres query, every S2 append, every SSE event. Extremely verbose. For local development only.
- `info`: Startup/shutdown events, request logging, recovery sweep results, agent bootstrap. The default production level.
- `warn`: Transient errors (S2 retry, stale cursor), dropped notifications, cache misses. Signals attention-worthy but non-critical conditions.
- `error`: Unrecoverable failures, panics, unresponsive dependencies. Signals investigation-required conditions.

Why not `trace`: zerolog supports it, but trace-level logging in a streaming server produces megabytes per second. Debug is sufficient.

**`SHUTDOWN_TIMEOUT_SECONDS` (optional, default 30)**

The hard deadline for graceful shutdown. After this many seconds, the process force-exits regardless of in-flight work.

Why 30 seconds: The worst-case shutdown path (Section 3) takes ~15 seconds (10s for streaming write abort + 5s for cursor flush + overhead). 30 seconds provides 2x headroom. Whichever process supervisor we use (systemd, launchd, Docker, or a foreground Ctrl-C) must be configured to wait at least 35 seconds between SIGTERM and SIGKILL — e.g. systemd `TimeoutStopSec=35s`, Docker `--stop-timeout 35`. That gives our 30-second shutdown time to complete plus a 5-second buffer.

Range 5–300: Below 5 seconds, some components can't drain cleanly. Above 300 seconds (5 minutes), a stuck shutdown blocks deployments unacceptably.

**`RESIDENT_AGENT_ID` (optional)**

The UUIDv7 of the resident Claude agent. If empty, the server runs without a resident agent — all API endpoints work, but `GET /agents/resident` returns 503.

Validation: If present, must be a valid UUID (parsed via `uuid.Parse()`). Doesn't have to be UUIDv7 specifically — we can't validate the version without inspecting the version nibble, and there's no functional reason to reject a UUIDv4 here.

**`ANTHROPIC_API_KEY` (conditionally required)**

The Claude API key. Required if and only if `RESIDENT_AGENT_ID` is set.

Why conditional: If there's no resident agent, the Anthropic key is unused. Requiring it unconditionally would make the server harder to run for API-only testing (no Claude features).

Validation: Non-empty when required. We don't pre-validate the key format — the Anthropic SDK validates on first API call. A bad key fails when the agent tries to respond to its first message, not at startup. This is acceptable because:
- The agent is a degraded-but-functional component — it sends error messages to conversations on Claude API failure
- Pre-validating would require a test API call at startup, adding latency and a network dependency to the critical boot path

Wait — actually, let me reconsider. If the evaluator deploys with a typo in the API key, the agent silently fails on every Claude call and sends "I encountered an error" messages. That's a terrible first impression. A pre-validation test is worth the cost.

**Decision: Pre-validate the Anthropic key at startup** by making a minimal API call (e.g., list models, or a trivial non-streaming message with 1 max_token). If it fails, log a WARNING (not a fatal error) and continue startup. The agent starts but will fail on Claude calls — which is the same behavior as runtime Claude downtime. The warning makes the misconfiguration visible in logs.

```go
func validateAnthropicKey(ctx context.Context, client *anthropic.Client) error {
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()
    _, err := client.Messages.New(ctx, anthropic.MessageNewParams{
        Model:     anthropic.ModelClaudeSonnet4_20250514,
        MaxTokens: 1,
        Messages: []anthropic.MessageParam{{
            Role:    anthropic.MessageParamRoleUser,
            Content: []anthropic.ContentBlockParam{anthropic.NewTextBlock("ping")},
        }},
    })
    return err
}
```

Cost: ~0.001 cents per startup. Negligible.

**`S2_BASIN` (optional, default "agentmail")**

The S2 basin name. Extracted as a config variable (not hardcoded) because:
- Local development might use a different basin to avoid colliding with production data
- Testing might use an ephemeral basin per test run
- Multi-environment deployments (staging, production) use different basins

**`CURSOR_FLUSH_INTERVAL_S` (optional, default 5)**

Interval in seconds between batched cursor flushes from in-memory to Postgres. Designed in `sql-metadata-plan.md` §7. Extracted as config because:
- Higher values reduce Postgres write load but increase data loss on crash
- Lower values increase Postgres write load but reduce data loss
- Default 5 seconds balances both — at most 150 events re-delivered on crash (5s × ~30 events/sec)

**`HEALTH_CHECK_TIMEOUT_S` (optional, default 5)**

Timeout for individual health checks (Postgres ping, S2 check) within the `GET /health` endpoint.

### Config Struct

```go
type Config struct {
    DatabaseURL          string
    S2AuthToken          string
    S2Basin              string
    Port                 int
    LogLevel             string
    ShutdownTimeout      time.Duration
    ResidentAgentID      *uuid.UUID  // nil if not configured
    AnthropicAPIKey      string      // empty if no resident agent
    CursorFlushInterval  time.Duration
    HealthCheckTimeout   time.Duration
}
```

**Why `*uuid.UUID` for ResidentAgentID:** Distinguishes "not configured" (`nil`) from "configured" (non-nil). A zero-value UUID (`uuid.Nil`) could theoretically be a valid ID, and checking `agentID == uuid.Nil` is semantically wrong — it's checking for a specific UUID value, not the absence of a configuration. Nil pointer is unambiguous.

### Loading and Validation

```go
func LoadConfig() (Config, error) {
    var errs []string

    cfg := Config{
        S2Basin:             envOrDefault("S2_BASIN", "agentmail"),
        Port:                envIntOrDefault("PORT", 8080),
        LogLevel:            envOrDefault("LOG_LEVEL", "info"),
        ShutdownTimeout:     time.Duration(envIntOrDefault("SHUTDOWN_TIMEOUT_SECONDS", 30)) * time.Second,
        CursorFlushInterval: time.Duration(envIntOrDefault("CURSOR_FLUSH_INTERVAL_S", 5)) * time.Second,
        HealthCheckTimeout:  time.Duration(envIntOrDefault("HEALTH_CHECK_TIMEOUT_S", 5)) * time.Second,
    }

    // Required variables
    cfg.DatabaseURL = os.Getenv("DATABASE_URL")
    if cfg.DatabaseURL == "" {
        errs = append(errs, "DATABASE_URL is required")
    }

    cfg.S2AuthToken = os.Getenv("S2_AUTH_TOKEN")
    if cfg.S2AuthToken == "" {
        errs = append(errs, "S2_AUTH_TOKEN is required")
    }

    // Optional resident agent
    if raw := os.Getenv("RESIDENT_AGENT_ID"); raw != "" {
        id, err := uuid.Parse(raw)
        if err != nil {
            errs = append(errs, fmt.Sprintf("RESIDENT_AGENT_ID is not a valid UUID: %s", raw))
        } else {
            cfg.ResidentAgentID = &id
        }
    }

    if cfg.ResidentAgentID != nil {
        cfg.AnthropicAPIKey = os.Getenv("ANTHROPIC_API_KEY")
        if cfg.AnthropicAPIKey == "" {
            errs = append(errs, "ANTHROPIC_API_KEY is required when RESIDENT_AGENT_ID is set")
        }
    }

    // Validate ranges
    if cfg.Port < 1 || cfg.Port > 65535 {
        errs = append(errs, fmt.Sprintf("PORT must be 1-65535, got %d", cfg.Port))
    }
    validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
    if !validLevels[cfg.LogLevel] {
        errs = append(errs, fmt.Sprintf("LOG_LEVEL must be debug|info|warn|error, got %s", cfg.LogLevel))
    }

    if len(errs) > 0 {
        return Config{}, fmt.Errorf("configuration errors:\n  %s", strings.Join(errs, "\n  "))
    }

    return cfg, nil
}
```

**Key design choice: collect ALL errors, don't fail on first.** If `DATABASE_URL` and `S2_AUTH_TOKEN` are both missing, report both. Don't make the operator fix one, restart, discover the other, fix, restart again. One pass, all errors, one fix.

### DSN Pre-Validation (Not Connection)

```go
// Validate the DSN can be parsed BEFORE attempting a network connection.
// This catches malformed connection strings immediately.
_, err := pgxpool.ParseConfig(cfg.DatabaseURL)
if err != nil {
    return Config{}, fmt.Errorf("DATABASE_URL is malformed: %w", err)
}
```

This runs during config loading, before any network I/O. A typo like `postgresq://...` (missing the `l`) is caught in milliseconds, not after a 30-second connection timeout.

### Environment Variable Source

**Local development:** `.env` file sourced into the shell (`set -o allexport && source .env && set +o allexport`) before `go run`. Not committed to git.

**Production (Cloudflare Tunnel deployment):** The process supervisor feeds the binary its entire environment — there is no two-tier "secrets vs non-secrets" split. Typical wirings:
- **systemd:** `EnvironmentFile=/etc/agentmail/env` (file mode 0600, owned by the service user).
- **launchd:** `<key>EnvironmentVariables</key>` plist, or a wrapper shell script that sources `.env` and `exec`s the binary.
- **Cloud secret manager** (AWS SSM / GCP Secret Manager / Vault): fetch-at-launch shim that exports the values and `exec`s the binary.

All required vars (`DATABASE_URL`, `S2_AUTH_TOKEN`, `ANTHROPIC_API_KEY`, `RESIDENT_AGENT_ID`) and optional vars (`PORT`, `LOG_LEVEL`, `S2_BASIN`) ride through the same `os.Getenv` path — our code doesn't know or care which mechanism populated the environment.

### Why No Config File (YAML/TOML/JSON)

| Approach | Pro | Con |
|---|---|---|
| **Env vars (chosen)** | 12-factor standard. Works with systemd, launchd, Docker, cloud secret managers, CI. No file to mount. | No nesting, no complex types. |
| **YAML/TOML config file** | Hierarchical, readable. | Must be placed on the host or mounted into a container. Different path in dev vs prod. Easy to accidentally commit secrets. |
| **CLI flags** | Type-safe, auto-generated `--help`. | Flags in `ExecStart=` / `entrypoint` are clunky. No standard way to pass flags from a cloud secret manager. |
| **Combined (flags + env + file)** | Maximum flexibility. | Maximum complexity. Config precedence bugs. |

For a Go server with <10 configuration values, env vars are the sweet spot. No files to mount, no precedence rules, no YAML parsing library.

### Why No `godotenv` in the Binary

`godotenv` loads `.env` files at runtime. Including it in the production binary means the server looks for a `.env` file on the filesystem — a file that shouldn't exist on a production host. If it accidentally exists (stale deployment artifact, a forgotten dev scratchpad), it silently overrides the values set by systemd / secret manager / launchd.

**Decision:** Use `godotenv` for local development only (in a `//go:build dev` file or via `make dev`). The production binary reads only from `os.Getenv()`.

---

## 2. Startup Sequence

### Dependency Graph

The startup sequence follows the dependency graph between components:

```text
                    ┌─────────────┐
                    │ Parse Config │
                    └──────┬──────┘
                           │
              ┌────────────┼────────────┐
              ▼            ▼            ▼
       ┌──────────┐ ┌──────────┐ ┌───────────┐
       │ Init     │ │ Connect  │ │ Configure │
       │ Logging  │ │ Postgres │ │ S2 Client │
       └──────────┘ └────┬─────┘ └─────┬─────┘
                         │             │
                    ┌────┴────┐        │
                    ▼         ▼        │
              ┌──────────┐ ┌───────┐   │
              │ Run      │ │ Warm  │   │
              │ Migration│ │ Agent │   │
              └────┬─────┘ │ Cache │   │
                   │       └───┬───┘   │
                   │           │       │
                   └─────┬─────┘       │
                         │             │
                         ▼             ▼
              ┌──────────────────────────────┐
              │ Recovery Sweep               │
              │ (needs Postgres + S2)        │
              └──────────────┬───────────────┘
                             │
                    ┌────────┴────────┐
                    ▼                 ▼
              ┌───────────────┐ ┌──────────────────┐
              │ Start Cursor  │ │ Bootstrap Claude  │
              │ Flush Ticker  │ │ Agent (if config) │
              └───────┬───────┘ └────────┬─────────┘
                      │                  │
                      └────────┬─────────┘
                               ▼
                    ┌──────────────────────┐
                    │ Build HTTP Router    │
                    │ + Middleware         │
                    └──────────┬───────────┘
                               │
                               ▼
                    ┌──────────────────────┐
                    │ Start HTTP Server    │
                    │ (begin serving)      │
                    └──────────────────────┘
```

### Complete Startup Code

```go
func main() {
    // ─── Step 0: Parse and validate configuration ───────────────────────
    cfg, err := LoadConfig()
    if err != nil {
        fmt.Fprintf(os.Stderr, "FATAL: %s\n", err)
        os.Exit(1)
    }

    // ─── Step 1: Initialize structured logging ──────────────────────────
    initLogging(cfg.LogLevel)
    log.Info().
        Int("port", cfg.Port).
        Str("s2_basin", cfg.S2Basin).
        Bool("resident_agent", cfg.ResidentAgentID != nil).
        Msg("starting agentmail server")

    // ─── Step 2: Create root context (canceled on shutdown signal) ──────
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    // ─── Step 3: Connect to PostgreSQL ──────────────────────────────────
    pool, err := connectPostgres(ctx, cfg.DatabaseURL)
    if err != nil {
        log.Fatal().Err(err).Msg("failed to connect to postgres")
    }
    defer pool.Close()
    log.Info().Int32("pool_size", pool.Stat().TotalConns()).Msg("postgres connected")

    // ─── Step 4: Run schema migration ───────────────────────────────────
    if err := runMigration(ctx, pool); err != nil {
        log.Fatal().Err(err).Msg("schema migration failed")
    }
    log.Info().Msg("schema migration complete")

    // ─── Step 5: Warm agent existence cache ─────────────────────────────
    agentCache := store.NewAgentCache()
    if err := agentCache.WarmFromDB(ctx, pool); err != nil {
        log.Fatal().Err(err).Msg("failed to warm agent cache")
    }
    log.Info().Int("agents", agentCache.Len()).Msg("agent cache warmed")

    // ─── Step 6: Initialize S2 client ───────────────────────────────────
    s2Client := store.NewS2Store(cfg.S2AuthToken, cfg.S2Basin)
    log.Info().Str("basin", cfg.S2Basin).Msg("s2 client initialized")

    // ─── Step 7: Build metadata store (composes sub-stores) ─────────────
    metaStore := store.New(pool, agentCache)

    // ─── Step 8: Recovery sweep — in-progress messages ──────────────────
    recovered, err := recoverInProgressMessages(ctx, metaStore, s2Client)
    if err != nil {
        log.Warn().Err(err).Msg("recovery sweep incomplete — will retry on next restart")
    } else {
        log.Info().Int("recovered", recovered).Msg("in-progress message recovery complete")
    }

    // ─── Step 9: Start cursor flush ticker ──────────────────────────────
    cursorCtx, cursorCancel := context.WithCancel(ctx)
    go metaStore.Cursors().Start(cursorCtx, cfg.CursorFlushInterval)
    log.Info().Dur("interval", cfg.CursorFlushInterval).Msg("cursor flush ticker started")

    // ─── Step 10: Bootstrap resident agent (if configured) ──────────────
    var agent *resident.Agent
    var notifier api.AgentNotifier = &api.NoopNotifier{}

    if cfg.ResidentAgentID != nil {
        agent, notifier, err = bootstrapResidentAgent(ctx, *cfg.ResidentAgentID,
            cfg.AnthropicAPIKey, metaStore, s2Client)
        if err != nil {
            log.Error().Err(err).Msg("resident agent bootstrap failed — running without agent")
            agent = nil
            notifier = &api.NoopNotifier{}
        } else {
            log.Info().Str("agent_id", cfg.ResidentAgentID.String()).Msg("resident agent active")
        }
    } else {
        log.Info().Msg("no resident agent configured — running API only")
    }

    // ─── Step 11: Build HTTP router ─────────────────────────────────────
    handler := api.NewHandler(metaStore, s2Client, notifier, cfg.ResidentAgentID,
        cfg.HealthCheckTimeout)
    router := api.NewRouter(handler)
    log.Info().Msg("http router built")

    // ─── Step 12: Start HTTP server ─────────────────────────────────────
    srv := &http.Server{
        Addr:              fmt.Sprintf(":%d", cfg.Port),
        Handler:           router,
        ReadHeaderTimeout: 10 * time.Second,
        IdleTimeout:       120 * time.Second,
    }

    // Server goroutine
    srvErr := make(chan error, 1)
    go func() {
        log.Info().Int("port", cfg.Port).Msg("http server listening")
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            srvErr <- err
        }
        close(srvErr)
    }()

    // ─── Wait for shutdown signal or server error ───────────────────────
    select {
    case <-ctx.Done():
        log.Info().Msg("shutdown signal received")
    case err := <-srvErr:
        log.Fatal().Err(err).Msg("http server failed")
    }

    // ─── Graceful shutdown (Section 3) ──────────────────────────────────
    shutdown(srv, agent, cursorCancel, metaStore, pool, cfg.ShutdownTimeout)
}
```

### Step-by-Step Rationale

**Step 0: Config parsing before anything else.**

No logging, no connections, no goroutines until config is validated. A missing `DATABASE_URL` should produce a clear error on stderr and exit code 1 — not a structured log that may or may not appear depending on the logging config that also failed to load.

Why `fmt.Fprintf(os.Stderr, ...)` and not `log.Fatal(...)`: The logger isn't initialized yet. Using it would require a default logger that writes to stderr, which is what `fmt.Fprintf` already does. Don't introduce a dependency on the logger before the logger exists.

Why `os.Exit(1)`: Config errors are unrecoverable. There's nothing to clean up (nothing has been initialized). Exit immediately.

**Step 1: Logging initialization is step 1, not step 0.**

Logging depends on config (`LOG_LEVEL`). Config parsing must succeed first. After step 1, every subsequent step can use structured logging.

```go
func initLogging(level string) {
    lvl, _ := zerolog.ParseLevel(level)
    zerolog.SetGlobalLevel(lvl)
    log.Logger = zerolog.New(os.Stdout).With().
        Timestamp().
        Caller().
        Logger()
}
```

Why stdout, not stderr: Most log collectors (systemd journal, Docker log drivers, cloud log agents, `tail -f` in a shell) capture stdout by default. Structured JSON logs to stdout is the 12-factor convention. Error output to stderr is reserved for unstructured fatal errors (step 0).

Why caller info: Enables clicking through to source in IDE. Negligible overhead for the debugging value.

**Step 2: Root context with signal notification.**

`signal.NotifyContext` creates a context that's canceled when SIGINT or SIGTERM arrives. Every component receives this context (directly or derived) and stops cleanly when it's canceled.

Why both SIGINT and SIGTERM:
- SIGINT: Ctrl+C during local development
- SIGTERM: systemd graceful stop, launchd stop, `docker stop`, `cloudflared` tearing down its child, `kill` default signal

Why a single root context: All components share a single cancellation tree. Canceling the root propagates to every goroutine — no orphaned goroutines, no forgotten cancellations. Components that need independent cancellation (e.g., cursor flush) derive child contexts.

**Step 3: Postgres connection — FAIL FAST.**

```go
func connectPostgres(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
    config, err := pgxpool.ParseConfig(dsn)
    if err != nil {
        return nil, fmt.Errorf("invalid DATABASE_URL: %w", err)
    }

    config.MaxConns = 15
    config.MinConns = 5
    config.MaxConnLifetime = 30 * time.Minute
    config.MaxConnLifetimeJitter = 5 * time.Minute
    config.MaxConnIdleTime = 5 * time.Minute
    config.HealthCheckPeriod = 30 * time.Second

    // 10-second connection timeout — fail fast if Postgres is unreachable
    connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()

    pool, err := pgxpool.NewWithConfig(connectCtx, config)
    if err != nil {
        return nil, fmt.Errorf("postgres connection failed: %w", err)
    }

    // Verify connectivity immediately (don't trust lazy connection)
    if err := pool.Ping(connectCtx); err != nil {
        pool.Close()
        return nil, fmt.Errorf("postgres ping failed: %w", err)
    }

    return pool, nil
}
```

Why fail-fast (Fatal) on Postgres failure: The server cannot function without Postgres. Every API call needs membership checks, agent validation, or cursor operations. Starting without Postgres produces a server that accepts HTTP connections and returns 500 on every request — worse than not starting at all.

Why 10-second timeout: Neon cold start is <500ms. Direct Postgres is <100ms. If Postgres doesn't respond in 10 seconds, it's unreachable (wrong hostname, firewall, credentials). Don't waste 30 seconds discovering this.

Why `pool.Ping()` after `NewWithConfig()`: pgxpool creates connections lazily by default. `MinConns = 5` triggers background connection creation, but `NewWithConfig()` may return before those connections are established. `Ping()` forces an immediate round-trip, confirming Postgres is reachable and credentials are valid.

**Race condition: What if Postgres becomes unreachable between Ping and the first real query?**

Not a startup concern — this is a runtime concern handled by pgxpool's health checks (every 30 seconds) and request-level timeouts (30 seconds for CRUD). The Ping during startup confirms "Postgres was reachable at boot time" — sufficient for a sane startup. Runtime resilience is a different concern.

**Step 4: Schema migration — FAIL FAST.**

```go
func runMigration(ctx context.Context, pool *pgxpool.Pool) error {
    //go:embed schema/schema.sql
    var schemaSQL string

    ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()

    _, err := pool.Exec(ctx, schemaSQL)
    return err
}
```

The schema uses `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS` — idempotent. First run creates everything. Subsequent runs are no-ops.

Why fail-fast on migration failure: If the schema can't be created, every query will fail. Same reasoning as Postgres connectivity — a server without tables is a server that 500s everything.

Why embedded SQL (not golang-migrate): The take-home has one schema version. A migration framework adds a dependency, a `schema_migrations` table, and migration file management for zero benefit. Document golang-migrate as the production migration strategy.

**Edge case: Migration timeout.** Creating 4 tables + 3 indexes on an empty Neon database takes <100ms. If it takes >10 seconds, something is catastrophically wrong (Postgres is accepting connections but not processing queries — possible connection pooler issue). Fail fast.

**Edge case: Schema already exists but is WRONG (columns missing, wrong types).** `CREATE TABLE IF NOT EXISTS` doesn't check column definitions — it only checks table existence. If the schema was manually altered (column dropped, type changed), queries will fail at runtime with descriptive errors (column not found, type mismatch). We don't add schema validation at startup because:
- Manual schema alteration is a deployment error, not a runtime concern
- Schema validation adds a query per table, per startup — cost that grows with schema complexity
- The first real query failure is self-diagnosing (error message includes the column/type mismatch)

For production: golang-migrate with versioned migrations and a `schema_migrations` table catches schema drift.

**Step 5: Agent cache warming — FAIL FAST.**

`SELECT id FROM agents` — sequential scan of a UUID-only table. At 1M agents: ~500ms. At take-home scale (dozens): <1ms.

Why fail-fast: Cache warming is a `SELECT` query on the same Postgres we just pinged. If it fails, Postgres died between steps 3 and 5 — a transient failure that warrants a restart, not degraded operation.

**Step 6: S2 client initialization — NO NETWORK CALL.**

```go
func NewS2Store(token, basin string) *S2Store {
    client := s2.New(token, &s2.ClientOptions{
        Compression: s2.CompressionZstd,
        RetryConfig: &s2.RetryConfig{
            MaxAttempts:       3,
            MinBaseDelay:      100 * time.Millisecond,
            MaxBaseDelay:      1 * time.Second,
            AppendRetryPolicy: s2.AppendRetryPolicyAll,
        },
    })
    return &S2Store{
        client: client,
        basin:  client.Basin(basin),
    }
}
```

The S2 SDK is lazy — `s2.New()` and `client.Basin()` create client structs without making network calls. The first network call happens during the recovery sweep (step 8) or the first API request.

Why not pre-validate S2 connectivity: Unlike Postgres, S2 is not needed for all operations. Agent registration, conversation creation, listing, and membership operations work with Postgres alone. S2 is needed for message writes and reads. Starting without S2 is degraded but partially functional.

That said — the recovery sweep (step 8) and the Claude agent (step 10) both need S2. If S2 is unreachable:
- Recovery sweep: logs a warning, leaves in-progress rows for next restart (self-healing)
- Claude agent: listeners fail to open ReadSessions, retry with exponential backoff (self-healing)
- Message writes: return 503 to clients (client retries)
- SSE reads: fail to open, client reconnects (client retries)

The system self-heals when S2 comes back. No admin intervention needed. This is correct behavior for a system with two external dependencies (Postgres + S2) where one can be temporarily unavailable.

**However:** For the take-home evaluation, we want confidence that S2 is working. Add a non-fatal connectivity check:

```go
// Best-effort S2 connectivity check (non-fatal)
if err := checkS2Connectivity(ctx, s2Client); err != nil {
    log.Warn().Err(err).Msg("S2 connectivity check failed — message operations will fail until S2 is reachable")
} else {
    log.Info().Msg("s2 connectivity verified")
}
```

```go
func checkS2Connectivity(ctx context.Context, s2 *S2Store) error {
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()
    // CheckTail on a non-existent stream returns a "stream not found" error,
    // which is a successful round-trip (proves S2 is reachable and auth is valid).
    // A network error or auth error is the real failure.
    _, err := s2.CheckTail(ctx, uuid.Nil)
    if err != nil {
        if isStreamNotFound(err) {
            return nil // S2 is reachable, stream just doesn't exist — expected
        }
        return err
    }
    return nil
}
```

**Step 7: Build metadata store.**

Composes the pgxpool, agent cache, membership cache, cursor cache, and connection registry into a single `Store` interface. No I/O, no failure path. This is pure construction.

**Step 8: Recovery sweep — WARN ON FAILURE, DON'T FAIL.**

The recovery sweep reads the `in_progress_messages` table and, for every unterminated message from the previous server run: (1) writes `message_abort` to S2, (2) inserts a `messages_dedup` row with `status='aborted'` (`ON CONFLICT DO NOTHING`), and (3) deletes the `in_progress_messages` row. Step 2 is what makes a client retry with the same `message_id` after a crash return `409 already_aborted` — without it, the dedup table would have no record of the aborted attempt and the retry would proceed silently with duplicate semantics. See [s2-architecture-plan.md](s2-architecture-plan.md) §10 for the function body.

Why warn-and-continue instead of fail-fast:
- If Postgres is up but S2 is down, the recovery sweep can't write abort events. The rows stay in the table and are retried on next restart. This is self-healing by design (s2-architecture-plan.md §10).
- If Postgres is down — impossible, we just successfully ran a migration in step 4.
- Failing the entire server startup because of leftover in-progress rows punishes restarts. The server should start serving new requests even if cleanup from the last run is incomplete.

**Step 9: Cursor flush ticker.**

Starts a background goroutine that flushes dirty cursors from in-memory to Postgres every N seconds. Uses a derived context (`cursorCtx`) so it can be canceled independently during shutdown (we need the cursor flush to complete AFTER SSE connections close, but BEFORE Postgres closes).

Why `go metaStore.Cursors().Start(cursorCtx)` and not a method on the server struct: The cursor store owns its flush lifecycle. The server controls when it starts and stops via context cancellation. Clean separation.

**Step 10: Claude agent bootstrap — WARN ON FAILURE, DON'T FAIL.**

```go
func bootstrapResidentAgent(
    ctx context.Context,
    agentID uuid.UUID,
    anthropicKey string,
    store store.Store,
    s2 *store.S2Store,
) (*resident.Agent, api.AgentNotifier, error) {
    // 1. Ensure agent identity exists in Postgres
    if err := store.Agents().EnsureExists(ctx, agentID); err != nil {
        return nil, nil, fmt.Errorf("agent identity: %w", err)
    }

    // 2. Validate Anthropic key (best-effort, non-fatal warning)
    claudeClient := anthropic.NewClient(option.WithAPIKey(anthropicKey))
    if err := validateAnthropicKey(ctx, claudeClient); err != nil {
        log.Warn().Err(err).Msg("anthropic API key validation failed — agent will retry on first response")
    }

    // 3. Create agent, start discovery loop and listeners
    agent := resident.NewAgent(agentID, store, s2, claudeClient)
    if err := agent.Start(ctx); err != nil {
        return nil, nil, fmt.Errorf("agent start: %w", err)
    }

    return agent, agent.Notifier(), nil
}
```

Why warn-and-continue instead of fail-fast: The API is the primary deliverable. The Claude agent is an important but secondary feature. If the agent fails to start (Postgres error during reconciliation, S2 unreachable for all listeners), the API should still serve external agents. The evaluator can connect their own agents and use the service — the resident agent being down doesn't break anything.

**Ordering guarantee: Agent bootstrap BEFORE HTTP server starts.**

The agent's discovery goroutine must be active before the HTTP server accepts traffic. If the server starts first, an evaluator could invite the agent before the invite channel is connected — the notification would be lost.

Sequence:
1. Agent bootstrap: `EnsureExists()`, `reconcileConversations()`, `discoveryLoop()` (goroutine started, channel active)
2. HTTP server starts: invite requests now flow through the invite handler → `notifier.OnInvite()` → agent's invite channel

No gap between "channel connected" and "server accepting invites" because the channel is connected (step 1) before the server listens (step 12).

**Step 11: Build HTTP router.**

Pure construction. Wires middleware, handlers, and routes per `http-api-layer-plan.md` §2. No I/O.

**Step 12: Start HTTP server.**

The server starts in a goroutine. `ListenAndServe()` blocks until the server is shut down or fails. The main goroutine selects between the shutdown signal and a server error.

Why `ReadHeaderTimeout: 10 * time.Second`: Prevents slowloris attacks where a client sends headers very slowly, holding a connection open. 10 seconds is generous for any legitimate client.

Why `IdleTimeout: 120 * time.Second`: Keep-alive connections that are idle for 2 minutes are closed. This frees resources from clients that opened a connection and forgot about it. SSE connections are NOT idle — they send heartbeats every 30 seconds.

Why no `ReadTimeout` or `WriteTimeout`: These apply to the ENTIRE request/response cycle, including SSE streams and streaming writes. Setting them would kill long-lived connections. Per-endpoint timeouts (30s CRUD, none for streaming) are handled by middleware and handler-level context deadlines.

### Startup Failure Matrix

Every external dependency, what happens when it fails at startup, and whether the server starts:

```text
┌──────────────────┬─────────────────────────────────────────┬──────────────┐
│ Dependency       │ Failure at Startup                      │ Server Starts│
├──────────────────┼─────────────────────────────────────────┼──────────────┤
│ Config (env vars)│ Missing required var or invalid value   │ NO — exit(1) │
│ Postgres connect │ Unreachable, bad credentials, timeout   │ NO — Fatal   │
│ Postgres migrate │ Schema creation fails                   │ NO — Fatal   │
│ Agent cache warm │ SELECT query fails                      │ NO — Fatal   │
│ S2 connectivity  │ Unreachable, bad token                  │ YES — warn   │
│ Recovery sweep   │ S2 down (can't write message_abort)     │ YES — warn   │
│ Cursor ticker    │ (goroutine start, can't fail)           │ YES          │
│ Claude agent     │ Postgres or S2 issue during bootstrap   │ YES — warn   │
│ Anthropic key    │ Invalid key                             │ YES — warn   │
│ HTTP listen      │ Port in use, permission denied          │ NO — Fatal   │
└──────────────────┴─────────────────────────────────────────┴──────────────┘
```

**The principle: Postgres is fatal, everything else degrades.** Postgres is the source of truth for identity, membership, and cursors. Without it, the server can't validate a single request. S2, the Claude agent, and Anthropic are all degradable — the server functions (partially) without them and self-heals when they come back.

### Startup Time Budget

| Step | Expected Latency | Notes |
|---|---|---|
| Config parsing | <1ms | String parsing, no I/O |
| Logging init | <1ms | |
| Postgres connect | ~50–500ms | Neon cold start is <500ms; warm is <50ms |
| Migration | <100ms | 4 tables, 3 indexes, IF NOT EXISTS |
| Agent cache warm | <100ms | At take-home scale; ~500ms at 1M agents |
| S2 connectivity | <100ms | Single round-trip to us-east-1 |
| Recovery sweep | <500ms | One Postgres query + N unary S2 appends |
| Cursor ticker start | <1ms | Goroutine launch |
| Agent bootstrap | <1s | EnsureExists + reconcile + N ReadSession opens |
| Router build | <1ms | Pure construction |
| **Total** | **~1–3 seconds** | |

At take-home scale, the server is ready to serve in under 3 seconds. The dominant cost is Neon cold start (if applicable) and agent bootstrap (sequential ReadSession opens for N conversations).

### Edge Cases

**Server starts while Neon is scale-to-zero'd:**

Neon suspends compute after 5 minutes of inactivity. Cold start is <500ms. The `connectPostgres()` timeout of 10 seconds covers this with 20x headroom. If Neon's cold start ever exceeds 10 seconds (extremely unlikely), increase the timeout.

Mitigation for evaluation: Disable scale-to-zero via Neon dashboard during the evaluation period.

**Server starts while S2 is in a maintenance window:**

S2 connectivity check warns. Recovery sweep warns and leaves rows for next restart. Agent listeners fail and retry with backoff. HTTP endpoints that hit S2 return 503. Health endpoint reports `s2: unreachable`. The server is degraded but alive. When S2 recovers, everything self-heals.

**Server starts immediately after a crash (OOM, SIGKILL):**

The `in_progress_messages` table has rows from unterminated streaming writes. Recovery sweep, per row: (1) writes `message_abort` to S2; (2) inserts a `messages_dedup` row with `status='aborted'` (`ON CONFLICT DO NOTHING`) so any client that retries with the same `message_id` receives `409 already_aborted` rather than silently starting a new write; (3) deletes the `in_progress_messages` row. Cursor hot tier is empty — agents reconnect with cursors from Postgres (at most 5 seconds stale). Agent listeners re-open ReadSessions from their last flushed cursor. Total recovery: automatic, no manual intervention. See [s2-architecture-plan.md](s2-architecture-plan.md) §10 for the sweep algorithm body.

**Two server instances start simultaneously (horizontal scaling or deployment overlap):**

Both run migrations — idempotent (`IF NOT EXISTS`). Both warm agent caches — read-only, no conflict. Both run recovery sweeps — race condition: both try to write `message_abort` for the same in-progress message. S2 append succeeds for both — duplicate `message_abort` on the stream. Readers see two aborts for the same `message_id` — they ignore the second (already removed from pending map). Both attempt `INSERT INTO messages_dedup … status='aborted' ON CONFLICT DO NOTHING` — first writer wins, second is a silent no-op (the `(conversation_id, message_id)` PK conflict is the expected case, not an error). Both delete the Postgres `in_progress_messages` row — second delete is a no-op (row already gone). Both register the resident agent — `ON CONFLICT DO NOTHING`. Both reconcile conversations — both start listeners for the same conversations.

**The problem with two instances listening to the same conversations:** Both agents respond to every message. The evaluator sees double responses. This is a fundamental single-instance assumption. Multi-instance requires leader election for the resident agent — document in FUTURE.md, don't build for take-home.

**Port already in use:**

`ListenAndServe()` returns `bind: address already in use`. The server goroutine sends the error on `srvErr`. Main goroutine receives it, logs Fatal, exits. Clean and immediate.

**SIGTERM arrives during startup (before HTTP server):**

`ctx.Done()` fires. Any in-progress step (Postgres connect, migration, agent bootstrap) sees context cancellation and returns an error. The main function exits without reaching the HTTP server. No shutdown sequence needed — nothing has been started.

Wait — this needs more careful handling. If Postgres is connected (step 3) but the HTTP server hasn't started (step 12), we need to close the pool:

```go
// In main(), after pool creation:
defer pool.Close()
```

The `defer pool.Close()` handles this. Even if startup aborts mid-sequence, all deferred cleanup runs. Steps that create closeable resources must have immediate defers:
- `defer pool.Close()` after step 3
- `defer cursorCancel()` after step 9
- `defer agent.Shutdown()` after step 10

**Agent cache warming takes too long (millions of agents):**

At 1M agents, cache warming scans ~16 MB of UUID data. On Neon with network latency, this might take 500ms–1s. At 10M agents, ~1–2s. At 100M agents, the `sync.Map` approach itself is the bottleneck (Section 3 of `http-api-layer-plan.md` documents the scaling path to Bloom filters). For take-home scale, not a concern.

---

## 3. Graceful Shutdown

### The Problem

The server has multiple long-lived resources at shutdown time:
- Active SSE connections (goroutines reading from S2, writing to HTTP response)
- Active streaming writes (goroutines reading from HTTP body, writing to S2)
- The resident Claude agent (goroutines listening to conversations, goroutines calling Claude API)
- The cursor flush ticker (goroutine periodically flushing to Postgres)
- S2 sessions (open gRPC/HTTP2 connections)
- Postgres connections (pgxpool)

Tearing these down in the wrong order loses data:
- Closing Postgres before flushing cursors → cursor data lost (up to 5 seconds of position data)
- Closing S2 before the Claude agent writes `message_abort` → unterminated messages on the stream
- Stopping the HTTP server before draining SSE → abrupt disconnects without cursor flush

### Shutdown Sequence

```text
SIGINT or SIGTERM received
         │
         ▼
┌───────────────────────────────────────────────────────────┐
│ 1. Stop accepting new HTTP connections                     │
│    srv.Shutdown(shutdownCtx)                               │
│    — Existing connections continue until handlers return    │
│    — No new connections accepted                           │
│    — Sets a hard deadline (30 seconds) on all handlers     │
└───────────────────┬───────────────────────────────────────┘
                    │
                    ▼
┌───────────────────────────────────────────────────────────┐
│ 2. Shut down resident agent                                │
│    agent.Shutdown(shutdownCtx)                             │
│    — Cancels all listener contexts                         │
│    — Waits for in-flight Claude responses (write abort)    │
│    — Each listener: flush cursor → close(done) → deregister│
│    — Timeout: 10 seconds                                   │
└───────────────────┬───────────────────────────────────────┘
                    │
                    ▼
┌───────────────────────────────────────────────────────────┐
│ 3. Cancel all remaining SSE and streaming write connections │
│    metaStore.ConnRegistry().CancelAll()                     │
│    — Each SSE handler: cursor flush → close response       │
│    — Each streaming write: message_abort → close session   │
│    — Wait for all Done channels (5s timeout per conn)      │
└───────────────────┬───────────────────────────────────────┘
                    │
                    ▼
┌───────────────────────────────────────────────────────────┐
│ 4. Final cursor flush                                      │
│    metaStore.Cursors().FlushAll(shutdownCtx)                │
│    — Flushes ALL dirty cursors to Postgres                 │
│    — Catches any cursors that weren't flushed in step 2/3  │
│    — Single batch UPSERT via unnest()                      │
└───────────────────┬───────────────────────────────────────┘
                    │
                    ▼
┌───────────────────────────────────────────────────────────┐
│ 5. Stop cursor flush ticker                                │
│    cursorCancel()                                          │
│    — Stops the periodic flush goroutine                    │
│    — No more flushes after this point                      │
└───────────────────┬───────────────────────────────────────┘
                    │
                    ▼
┌───────────────────────────────────────────────────────────┐
│ 6. Close Postgres pool                                     │
│    pool.Close()                                            │
│    — Waits for in-use connections to be released           │
│    — Then closes all connections                           │
│    — After this, no database operations are possible       │
└───────────────────┬───────────────────────────────────────┘
                    │
                    ▼
┌───────────────────────────────────────────────────────────┐
│ 7. Log and exit                                            │
│    log.Info().Dur("shutdown_duration", elapsed).            │
│        Msg("server shut down cleanly")                     │
│    os.Exit(0)                                              │
└───────────────────────────────────────────────────────────┘
```

### Implementation

```go
func shutdown(
    srv *http.Server,
    agent *resident.Agent,
    cursorCancel context.CancelFunc,
    store store.Store,
    pool *pgxpool.Pool,
    timeout time.Duration,
) {
    start := time.Now()
    shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()

    // Step 1: Stop accepting new connections, drain in-flight requests
    log.Info().Msg("stopping http server...")
    if err := srv.Shutdown(shutdownCtx); err != nil {
        log.Warn().Err(err).Msg("http server shutdown error (forcing close)")
        srv.Close() // force-close if graceful shutdown times out
    }
    log.Info().Dur("elapsed", time.Since(start)).Msg("http server stopped")

    // Step 2: Shut down resident agent
    if agent != nil {
        log.Info().Msg("shutting down resident agent...")
        agentCtx, agentCancel := context.WithTimeout(shutdownCtx, 10*time.Second)
        agent.Shutdown(agentCtx)
        agentCancel()
        log.Info().Dur("elapsed", time.Since(start)).Msg("resident agent stopped")
    }

    // Step 3: Cancel remaining SSE/streaming connections
    log.Info().Msg("canceling remaining connections...")
    remaining := store.ConnRegistry().CancelAll(shutdownCtx)
    log.Info().Int("canceled", remaining).Dur("elapsed", time.Since(start)).
        Msg("connections canceled")

    // Step 4: Final cursor flush
    log.Info().Msg("flushing cursors...")
    if err := store.Cursors().FlushAll(shutdownCtx); err != nil {
        log.Warn().Err(err).Msg("final cursor flush failed — cursors may be up to 5s stale on restart")
    }
    log.Info().Dur("elapsed", time.Since(start)).Msg("cursors flushed")

    // Step 5: Stop cursor ticker
    cursorCancel()

    // Step 6: Close Postgres (deferred in main, but explicit log here)
    log.Info().Dur("total", time.Since(start)).Msg("server shut down cleanly")
}
```

### Why This Ordering

**Step 1 before Step 2:**

The HTTP server must stop accepting new requests BEFORE the agent shuts down. If the agent shuts down first, new invite requests for the agent arrive, trigger `notifier.OnInvite()`, and the notification goes to a closed channel — panic or dropped. By stopping the HTTP server first, no new requests arrive, so no new notifications can be generated.

Wait — `srv.Shutdown()` doesn't immediately stop all handlers. It stops new connections but waits for in-flight handlers to complete. An in-flight invite handler could call `notifier.OnInvite()` while the agent is shutting down in step 2.

**Resolution:** The agent's invite channel has capacity 100 and uses non-blocking send (`select/default`). If the agent is shutting down and the channel is being drained, the notification either lands (agent picks it up and discards because context is canceled) or is dropped (non-blocking send, buffer full — logged). No panic, no hang.

Additionally, `srv.Shutdown()` with a deadline ensures in-flight handlers complete before step 2 starts (or are force-killed at the deadline). The typical case: all CRUD handlers complete in <1 second, SSE/streaming handlers continue until step 3 cancels them.

**Step 2 before Step 3:**

The agent's listeners are registered in ConnRegistry. `agent.Shutdown()` cancels them via context cancellation (the same cancel func stored in ConnRegistry). Step 3's `CancelAll()` is a safety net for any connections the agent didn't clean up AND external client connections. Doing agent shutdown first is cleaner because the agent waits for in-flight Claude responses — `CancelAll()` would cancel those abruptly.

**Step 3 before Step 4:**

SSE handlers flush their individual cursors on disconnect. Step 3 triggers those disconnects. Step 4 catches any cursors that weren't flushed (e.g., handler panicked before cursor flush, or timeout fired before handler completed cleanup).

**Step 4 before Step 5:**

The final `FlushAll()` might overlap with the cursor ticker's periodic flush (step 5 hasn't stopped it yet). This is safe — `FlushAll()` acquires the cursor cache lock, collects dirty entries, and executes the batch UPSERT. If the ticker fires simultaneously, it also acquires the lock, finds no dirty entries (they were just flushed), and does nothing. No conflict.

But it's cleaner to stop the ticker after the final flush — ensures no concurrent flush operations.

**Step 5 before Step 6:**

The ticker must stop before Postgres closes. If Postgres closes while the ticker's flush goroutine is mid-query, the query fails with a pool-closed error. Stopping the ticker first eliminates this race.

### The `CancelAll()` Method

The ConnRegistry needs a method that cancels every active connection and waits for all of them to exit:

```go
func (r *ConnRegistry) CancelAll(ctx context.Context) int {
    r.mu.Lock()
    entries := make([]*connEntry, 0, len(r.conns))
    for _, entry := range r.conns {
        entry.Cancel()
        entries = append(entries, entry)
    }
    r.mu.Unlock()

    canceled := 0
    for _, entry := range entries {
        select {
        case <-entry.Done:
            canceled++
        case <-ctx.Done():
            return canceled
        }
    }
    return canceled
}
```

Why unlock before waiting: Holding the lock while waiting for `Done` channels would deadlock if a handler's cleanup tries to call `Deregister()` (which needs the lock).

Why wait for `Done` (not just cancel and move on): Handlers perform cleanup in their deferred functions (cursor flush, S2 session close, `message_abort` write). If we don't wait, the cleanup may be mid-execution when we close Postgres in step 6. Waiting ensures cleanup completes before resource teardown.

### Hard Deadline Behavior

If the 30-second shutdown timeout fires:

1. `shutdownCtx.Done()` fires
2. `srv.Shutdown(shutdownCtx)` returns immediately (in-flight handlers may still be running)
3. `agent.Shutdown(agentCtx)` returns immediately (listeners may still be running)
4. `CancelAll(shutdownCtx)` returns immediately (some `Done` channels not received)
5. `FlushAll(shutdownCtx)` returns immediately or with error (Postgres query canceled)
6. Postgres pool closes — forcefully kills any in-progress queries
7. Process exits

**What data is lost:**
- Cursors that weren't flushed: at most 5 seconds of cursor position data per agent. Agents re-receive those events on reconnect (at-least-once delivery). No message data is lost.
- In-progress streaming writes that weren't aborted: `in_progress_messages` rows remain in Postgres. Recovery sweep on next startup writes `message_abort`. No data loss — just delayed cleanup.
- In-progress Claude responses that weren't aborted: same as above — `in_progress_messages` catches them.

**The only true data loss on hard shutdown:** In-memory cursor positions that haven't been flushed to Postgres. This is bounded by the flush interval (5 seconds) and results in at-least-once redelivery, not data loss.

### Process Supervisor Integration

The binary is shutdown-signal-driven; configuration lives in whichever supervisor launches it. Two representative examples:

**systemd unit (`/etc/systemd/system/agentmail.service`):**

```ini
[Unit]
Description=AgentMail server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=agentmail
EnvironmentFile=/etc/agentmail/env
ExecStart=/usr/local/bin/agentmail
KillSignal=SIGTERM
TimeoutStopSec=35s
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

**Docker run (if containerized, though cloudflared doesn't require it):**

```bash
docker run --rm \
  --env-file /etc/agentmail/env \
  --stop-signal SIGTERM \
  --stop-timeout 35 \
  -p 8080:8080 \
  ghcr.io/you/agentmail:latest
```

**`TimeoutStopSec=35s` / `--stop-timeout 35`:** Supervisor sends SIGTERM, then SIGKILL after this many seconds. Our `SHUTDOWN_TIMEOUT_SECONDS = 30` + 5 seconds buffer = 35. The server has 30 seconds to shut down gracefully, then 5 seconds of buffer before the supervisor force-kills it.

**No in-supervisor health check:** Cloudflare Tunnel doesn't probe upstream health; neither does systemd by default. Health monitoring is wired externally (uptime monitor or Cloudflare Worker cron hitting `/health`) — see deployment-plan.md §2 and health-check section below.

**Restart-in-place deploys:** The running process receives SIGTERM (via `systemctl restart agentmail` or `pkill -TERM`) and begins draining. The new process starts in its place and binds port 8080. During the ~30 s overlap:
- Clients connected to the old process stay on it until their connection closes
- New connections during the ~1 s listener gap get 502 from cloudflared → client retry → new process picks up
- SSE clients are disconnected during shutdown and reconnect to the new process via `Last-Event-ID`

### Race Conditions

**Race: Shutdown signal arrives while the cursor flush ticker is mid-flush.**

The ticker's `FlushAll()` is executing a Postgres batch UPSERT. Simultaneously, step 4's `FlushAll()` also wants to flush. Both acquire the cursor cache lock — one blocks until the other releases.

Resolution: The `dirty` set is cleared atomically under the lock. Whichever `FlushAll()` runs first collects and clears the dirty set. The second `FlushAll()` finds an empty dirty set and does nothing. No double-flush, no missed cursors.

**Race: SIGTERM during step 1 (`srv.Shutdown()`) — second SIGTERM arrives.**

`signal.NotifyContext` handles the first signal. If a second signal arrives (user presses Ctrl+C twice), the default Go signal handler kills the process. This is the expected behavior — double-signal means "I really want to stop, now."

To be safer, register a second signal handler that calls `os.Exit(1)`:

```go
// In main(), after the shutdown function:
go func() {
    // Second signal = force exit
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
    <-sigCh
    log.Warn().Msg("second signal received — forcing exit")
    os.Exit(1)
}()
```

**Race: New SSE connection opens between step 1 and step 3.**

`srv.Shutdown()` stops accepting new connections. But an in-flight request that was ACCEPTED before step 1 might not have opened its SSE connection yet (it's in the middleware pipeline). By the time it reaches the SSE handler and registers in ConnRegistry, step 3 has already called `CancelAll()`.

Resolution: The SSE handler checks `ctx.Err()` before entering the event loop. The shutdown context propagates through `r.Context()` (because `srv.Shutdown()` cancels active request contexts after the deadline). The handler sees the cancellation and returns immediately — no leaked connection.

Actually — `srv.Shutdown()` does NOT cancel active request contexts. It closes the listener (no new connections) and waits for active handlers to return. The request's context (`r.Context()`) is only canceled if the CLIENT disconnects. Our step 3 (`CancelAll()`) cancels the SSE handler's derived context (via ConnRegistry's cancel func). So the flow is:

1. Step 1: `srv.Shutdown()` — stops new connections, waits for handlers
2. Step 3: `CancelAll()` — cancels all SSE/streaming contexts
3. Handlers see cancellation, cleanup, return
4. `srv.Shutdown()` returns (all handlers done)

Wait — step 1 and step 3 are sequential. `srv.Shutdown()` blocks until all handlers return. But handlers are blocked on SSE (reading from S2) — they won't return until someone cancels them. That someone is step 3. But step 3 can't run until step 1 returns. Deadlock.

**This is a real problem.** Let me redesign.

**Fix: Don't rely on `srv.Shutdown()` to drain SSE handlers.**

```go
// Step 1: Stop accepting new connections (with a SHORT deadline for CRUD handlers)
crudTimeout := 5 * time.Second
crudCtx, crudCancel := context.WithTimeout(context.Background(), crudTimeout)
defer crudCancel()

// This will return after CRUD handlers complete, but SSE/streaming handlers will still
// be running (they don't respect the shutdown context — they respect their own cancel funcs)
go func() {
    if err := srv.Shutdown(crudCtx); err != nil {
        log.Warn().Err(err).Msg("http server graceful shutdown timeout — forcing close")
        srv.Close()
    }
}()

// Step 2: Shut down agent (cancels agent's SSE listeners)
// Step 3: Cancel remaining connections (cancels external SSE/streaming handlers)
// ... these proceed immediately, don't wait for srv.Shutdown()
```

Actually, the cleaner solution: `srv.Shutdown()` runs concurrently with agent shutdown and connection cancellation. It returns when all handlers have returned. Since steps 2 and 3 cancel the handlers, `srv.Shutdown()` returns shortly after.

**Revised shutdown:**

```go
func shutdown(
    srv *http.Server,
    agent *resident.Agent,
    cursorCancel context.CancelFunc,
    store store.Store,
    pool *pgxpool.Pool,
    timeout time.Duration,
) {
    start := time.Now()
    shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()

    // Step 1: Stop accepting new connections (non-blocking start)
    srvDone := make(chan struct{})
    go func() {
        srv.Shutdown(shutdownCtx) // waits for handlers to return
        close(srvDone)
    }()

    // Step 2: Shut down resident agent (cancels agent listeners)
    if agent != nil {
        log.Info().Msg("shutting down resident agent...")
        agentCtx, agentCancel := context.WithTimeout(shutdownCtx, 10*time.Second)
        agent.Shutdown(agentCtx)
        agentCancel()
        log.Info().Dur("elapsed", time.Since(start)).Msg("resident agent stopped")
    }

    // Step 3: Cancel all remaining connections
    log.Info().Msg("canceling remaining connections...")
    remaining := store.ConnRegistry().CancelAll(shutdownCtx)
    log.Info().Int("canceled", remaining).Msg("connections canceled")

    // Step 4: Wait for HTTP server to finish (handlers should be done now)
    select {
    case <-srvDone:
        log.Info().Dur("elapsed", time.Since(start)).Msg("http server stopped")
    case <-shutdownCtx.Done():
        log.Warn().Msg("http server shutdown timed out — forcing close")
        srv.Close()
    }

    // Step 5: Final cursor flush
    log.Info().Msg("flushing cursors...")
    if err := store.Cursors().FlushAll(shutdownCtx); err != nil {
        log.Warn().Err(err).Msg("final cursor flush failed")
    }

    // Step 6: Stop cursor ticker
    cursorCancel()

    // Step 7: pool.Close() happens via defer in main()
    log.Info().Dur("total", time.Since(start)).Msg("server shut down cleanly")
}
```

**The fix:** `srv.Shutdown()` runs concurrently in a goroutine. Steps 2-3 proceed immediately to cancel handlers. `srv.Shutdown()` sees handlers returning (because their contexts are canceled) and completes. Step 4 waits for `srv.Shutdown()` to confirm all handlers are done. No deadlock.

**Race: Handler opens S2 AppendSession after step 3 cancels its context.**

A streaming write handler is between "validation complete" and "open AppendSession." Step 3 cancels its context. The `OpenAppendSession()` call receives a canceled context and returns an error immediately. The handler writes `message_abort` (best-effort, background context) and returns. Clean.

---

## 4. Health Endpoint

### Design

Already partially specified in `http-api-layer-plan.md` §5. This section completes the design with full implementation detail.

### Decision: Deep Check (Postgres + S2)

```text
┌────────────┐
│ GET /health │
├────────────┤
│             │
│  Postgres ──┤─→ SELECT 1 (3s timeout)
│             │
│  S2 ────────┤─→ CheckTail (3s timeout)
│             │
│  Status: ───┤─→ "ok" if both pass
│             │   "degraded" if any fail
│             │
│  HTTP: ─────┤─→ 200 if ok
│             │   503 if degraded
└─────────────┘
```

### Why Deep, Not Shallow

| Approach | Checks | Pro | Con |
|---|---|---|---|
| **Shallow** (process alive) | Process responds | Near-instant, never false-negative | Server is "healthy" even if Postgres and S2 are dead — upstream monitors don't alert on a useless server |
| **Deep** (chosen) | Postgres + S2 | Accurate — only reports healthy when the server can actually serve requests | Transient blip on Postgres causes 503 → uptime monitor may page / restart the process |

**Why deep wins for us:** External probes (uptime monitor, Cloudflare Worker cron) use `/health` to decide whether to page an operator or trigger an automated restart. A shallow check that always returns 200 means the server is "healthy" even when every request will fail with 503. A deep check returning 503 tells the monitor the truth — someone (or something) needs to act.

**Mitigating false negatives from transient blips:** Configure the uptime monitor with a threshold (e.g. 3 consecutive failed 1-minute probes = page). A single Postgres blip produces one 503, which doesn't trigger the alert. Three consecutive 503s (3 minutes of Postgres downtime) triggers the alert or auto-restart hook — which is the correct response.

### Implementation

```go
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), h.healthTimeout)
    defer cancel()

    checks := make(map[string]string, 2)
    allOK := true

    // Check Postgres (parallel with S2)
    var pgErr, s2Err error
    var wg sync.WaitGroup
    wg.Add(2)

    go func() {
        defer wg.Done()
        pgErr = h.pool.Ping(ctx)
    }()

    go func() {
        defer wg.Done()
        _, s2Err = h.s2.CheckTail(ctx, uuid.Nil)
        if s2Err != nil && isStreamNotFound(s2Err) {
            s2Err = nil // stream-not-found = S2 is reachable (expected error)
        }
    }()

    wg.Wait()

    if pgErr != nil {
        checks["postgres"] = fmt.Sprintf("unreachable: %s", pgErr)
        allOK = false
    } else {
        checks["postgres"] = "ok"
    }

    if s2Err != nil {
        checks["s2"] = fmt.Sprintf("unreachable: %s", s2Err)
        allOK = false
    } else {
        checks["s2"] = "ok"
    }

    status := "ok"
    statusCode := 200
    if !allOK {
        status = "degraded"
        statusCode = 503
    }

    writeJSON(w, statusCode, HealthResponse{
        Status: status,
        Checks: checks,
    })
}
```

### Design Choices

**Parallel checks:** Postgres and S2 are checked concurrently. If both have 3-second timeouts, the worst case is 3 seconds (not 6). The health endpoint must respond within the external probe's timeout (typically 5 seconds for uptime monitors; Cloudflare's per-request edge timeout is 100 s so that's not the constraint).

**`CheckTail` on `uuid.Nil` for S2:** `uuid.Nil` is `00000000-0000-0000-0000-000000000000`. Stream name: `conversations/00000000-0000-0000-0000-000000000000`. This stream doesn't exist, so S2 returns "stream not found" — which proves S2 is reachable and the auth token is valid. We treat stream-not-found as success.

Why not a dedicated health-check stream: Creating and maintaining a dedicated stream adds unnecessary state. The stream-not-found error is a perfectly valid round-trip test.

**No agent auth on health endpoint:** Health checks run from external monitoring infrastructure (uptime monitor, Cloudflare Worker cron, operator `curl`), not from registered agents. Requiring `X-Agent-ID` would break infrastructure monitoring.

**No caching:** Health status is checked fresh on every call. Caching would defeat the purpose — if Postgres goes down, a cached "ok" response continues for the cache TTL.

**No Claude API check:** The Claude API is not on the critical path for the API. External agents can use the service without the resident agent. Including it in health checks would cause false negatives when Anthropic has an outage — marking our service as degraded when it's fully functional for its core purpose.

If we wanted to monitor Claude API health separately: add an optional `?include=claude` query parameter that adds the Anthropic check. Default health check stays Postgres + S2 only.

### Response Format

```json
// Healthy
{
  "status": "ok",
  "checks": {
    "postgres": "ok",
    "s2": "ok"
  }
}

// Degraded
{
  "status": "degraded",
  "checks": {
    "postgres": "ok",
    "s2": "unreachable: connection refused"
  }
}
```

Why `map[string]string` for checks: Extensible without type changes. Adding a new check (Redis, Claude, disk space) is one line in the handler. Values are human-readable — `"ok"` or a short error description.

Why no uptime, version, or commit hash: The health endpoint answers "can this server serve requests?" Adding metadata conflates health checking with service discovery. If needed, add a separate `GET /info` endpoint.

### External Health Check Configuration

The probe lives outside the binary. Two typical setups:

**UptimeRobot / Pingdom / BetterStack HTTP monitor:**

| Parameter | Value | Rationale |
|---|---|---|
| URL | `https://agentmail.example.com/health` | Goes through Cloudflare edge → tunnel → origin |
| Method | GET | Matches our handler |
| Expected status | 200 | 503 = degraded = page |
| Interval | 60 s (free tier) or 30 s (paid) | Frequent enough to catch real outages |
| Timeout | 5 s | > our internal 4 s check budget |
| Failure threshold | 3 consecutive | Ignores transient blips |
| Alert | Email / Slack / PagerDuty | On 3rd consecutive failure |

**Cloudflare Worker cron (alternative):** A Worker scheduled every minute that `fetch()`es `/health`, inspects the body's `checks` map, and posts to Slack/PagerDuty on degraded status. Costs $5/mo for Workers Paid.

### Edge Cases

**Health check during startup (before HTTP server is ready):**

Before the HTTP listener binds port 8080, an external probe gets "connection refused" from cloudflared (no origin listening). This is the same 502 the probe would see during restart. The monitor's failure threshold (3 consecutive) tolerates the ~3 s startup window without alerting.

**Health check during shutdown:**

`srv.Shutdown()` stops accepting new connections. Probes from the uptime monitor are new connections — they're refused. The monitor sees connection-refused, counts it as a failure. During a graceful restart, the new process binds within ~1 s — well below the 3-consecutive-failure threshold.

**Postgres and S2 both unreachable simultaneously:**

Both checks fail within their 3-second timeouts. Total response time: 3 seconds (parallel). Response: 503 with both components showing unreachable. The monitor alerts after 3 consecutive probes. If the condition persists, whatever auto-remediation we've wired (e.g. a Worker that calls `systemctl restart agentmail`) fires. On restart, the server fails to connect to Postgres (step 3) and exits with Fatal. The supervisor's `Restart=on-failure` starts a new process. The cycle repeats until dependencies recover.

**Health check timeout fires before both checks complete:**

If Postgres takes 4 seconds and S2 takes 1 second, the 5-second overall timeout fires before the health handler returns the response. The probe sees a timeout, treats it as unhealthy. This is correct — if the health check itself can't complete in time, the server is degraded.

To prevent this: the health check's internal timeout should be slightly LESS than the external probe's timeout — say 4 seconds when the probe allows 5 — to ensure the response is sent before the probe gives up:

```go
// Use 80% of the configured health check timeout for individual checks
checkTimeout := time.Duration(float64(h.healthTimeout) * 0.8)
ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
```

---

## 5. Complete `main.go` Structure

### Pseudocode Overview

```go
package main

import (
    // stdlib
    "context"
    "fmt"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    // internal
    "agentmail/internal/api"
    "agentmail/internal/agent"
    "agentmail/internal/store"

    // external
    "github.com/rs/zerolog"
    "github.com/rs/zerolog/log"
)

func main() {
    // Parse config (exit on error)
    // Init logging
    // Create root context (signal-aware)
    // Connect Postgres (fatal on error)
    // Run migration (fatal on error)
    // Warm agent cache (fatal on error)
    // Init S2 client (lazy, no network call)
    // Check S2 connectivity (warn on error)
    // Build store
    // Recovery sweep (warn on error)
    // Start cursor flush ticker
    // Bootstrap agent if configured (warn on error)
    // Build router
    // Start HTTP server
    // Wait for signal or error
    // Graceful shutdown
}
```

### File Organization

```text
cmd/
└── server/
    └── main.go          # Entry point — config, startup, shutdown, main()

internal/
├── config/
│   └── config.go        # Config struct, LoadConfig(), env helpers
```

Why `internal/config/` separate from `cmd/server/main.go`: The Config struct is used by tests (to construct a server with custom config). Keeping it in `cmd/` would make it inaccessible to `internal/` packages. Putting it in `internal/config/` makes it importable from tests and any internal package.

Why not put startup/shutdown logic in `internal/server/`: For the take-home, `main.go` is sufficient. The startup sequence is ~80 lines. Extracting it into a `Server` struct with `Start()` and `Stop()` methods adds a layer of indirection for zero benefit at this scale. Document the extraction as a production enhancement.

---

## 6. Scaling Considerations

### Single Instance (Take-Home)

Everything in this document works as-is. One host, one Go process behind one Cloudflare Tunnel. Startup in <3 seconds. Shutdown in <15 seconds. Health checks (probed externally) verify both dependencies.

### Multi-Instance (Horizontal Scaling)

When running multiple instances behind a load balancer:

**What works unchanged:**
- Config loading (each instance reads same env vars)
- Postgres connection (each instance gets its own pool)
- Schema migration (idempotent — all instances can run it)
- Agent cache warming (read-only, no conflict)
- S2 client (stateless, each instance gets its own)
- HTTP router and middleware (stateless)
- Health checks (each instance checks independently)
- Graceful shutdown (each instance shuts down independently)

**What needs changes:**
- **Recovery sweep:** Multiple instances racing to abort the same in-progress messages. Harmless (duplicate `message_abort` is ignored by readers), but wasteful. Fix: add a distributed lock (Postgres advisory lock) or a "claimed_by" column on `in_progress_messages`.
- **Resident agent:** Multiple instances each run their own agent, all listening to the same conversations, all responding to every message. The evaluator sees double/triple responses. Fix: leader election for agent ownership. Document in FUTURE.md.
- **Cursor flush ticker:** Multiple instances flushing `delivery_seq` for different SSE connections. No conflict — cursors are per-(agent, conversation), and each SSE connection is on one instance. The `WHERE cursors.delivery_seq < EXCLUDED.delivery_seq` regression guard on the batched upsert prevents out-of-order flushes from rewinding the cursor.
- **ConnRegistry:** Instance-local. A leave request hitting Instance A can only cancel connections on Instance A. If the agent's SSE connection is on Instance B, Instance A's leave handler can't reach it. Fix: broadcast leave events via Postgres `LISTEN/NOTIFY` or Redis pub/sub.

### Billions of Agents

At extreme scale, the startup sequence changes:
- Agent cache warming is replaced by Bloom filter loading (or no pre-warming — LRU cache fills lazily)
- Recovery sweep may need pagination (millions of in-progress rows after a large-scale crash)
- Health checks add more dimensions (Redis, message queue, read replicas)
- Configuration management moves from env vars to a config service (Consul, etcd)
- Shutdown may need to coordinate across instances (drain region before maintenance)

Document, don't build.

---

## 7. Edge Cases & Failure Modes — Comprehensive Matrix

### Startup Edge Cases

| Scenario | Behavior | Recovery |
|---|---|---|
| **Postgres DNS doesn't resolve** | `pgxpool.NewWithConfig()` fails within 10s timeout | Fatal exit. Fix DNS. |
| **Postgres accepts connection but auth fails** | `pool.Ping()` returns auth error | Fatal exit. Fix credentials. |
| **Postgres accepts connection but database doesn't exist** | Migration fails with "database does not exist" | Fatal exit. Create database. |
| **Migration partially applied (crash during CREATE TABLE)** | DDL is transactional in Postgres — rolls back on crash | `IF NOT EXISTS` handles partial state cleanly. |
| **Schema exists with wrong column types** | `IF NOT EXISTS` succeeds (table exists). First query fails with type mismatch. | Runtime error with descriptive message. Fix schema manually or drop and recreate. |
| **S2 token is expired** | S2 connectivity check warns (auth error). | Server starts. Message operations fail. Fix token. |
| **Two instances start simultaneously with empty database** | Both run migration simultaneously. Postgres serializes DDL — one succeeds, other is no-op. | No conflict. |
| **RESIDENT_AGENT_ID points to an agent that doesn't exist in Postgres** | `EnsureExists()` inserts it. | Self-healing. |
| **RESIDENT_AGENT_ID points to an agent that exists and is in 1000 conversations** | `reconcileConversations()` starts 1000 listeners sequentially. Startup takes ~10s. | Acceptable. Parallelize for production (document in FUTURE.md). |
| **Port in `cloudflared --url` doesn't match PORT env var** | Server listens on PORT. cloudflared tries to forward to the URL's port and gets "connection refused." All tunneled requests return 502. | Fix the `cloudflared tunnel --url http://localhost:<PORT>` invocation or the `PORT` env var to match. |

### Shutdown Edge Cases

| Scenario | Behavior | Data Impact |
|---|---|---|
| **No active connections at shutdown** | Steps 2-3 are no-ops. Shutdown completes in <1s. | None. |
| **1000 active SSE connections at shutdown** | `CancelAll()` cancels all 1000 contexts. Handlers flush cursors, close S2 sessions. ~5s. | Cursors flushed. Clean. |
| **Claude API call in-flight during shutdown** | Response goroutine detects context cancellation, writes `message_abort`, exits. | Partial Claude response discarded. `message_abort` on stream. Evaluator sees aborted message. |
| **Streaming write in-flight during shutdown** | Context canceled. Handler writes `message_abort`. `in_progress_messages` row deleted. | Partial message on stream, properly aborted. |
| **Postgres unreachable during final cursor flush** | `FlushAll()` returns error. Warning logged. | Cursors up to 5s stale. At-least-once redelivery on reconnect. |
| **Shutdown timeout fires (30s exceeded)** | `srv.Close()` force-closes all connections. Defers run (pool.Close()). Process exits. | Same as crash — in-memory cursors lost, recovery sweep on next start. |
| **SIGKILL during shutdown (supervisor timeout exceeded)** | Process killed immediately. No cleanup. | Same as crash. Recovery sweep handles everything on next start. |
| **Shutdown called twice (second SIGTERM)** | First shutdown is in progress. Second signal handler calls `os.Exit(1)`. | Same as SIGKILL. |

### Runtime Edge Cases (Handled by Lifecycle Design)

| Scenario | Component | Behavior |
|---|---|---|
| **Postgres connection pool exhaustion** | pgxpool | Goroutines queue. 30s CRUD timeout fires. Request returns 504. |
| **Postgres goes down during operation** | All handlers | Queries fail with context error. Return 500. Health reports degraded. |
| **S2 goes down during operation** | Message handlers | Writes return 503. SSE connections lose their S2 ReadSession, client reconnects. |
| **Server OOM killed** | OS | Same as crash. Recovery sweep on restart. |
| **Go runtime panic (nil pointer, etc.)** | Recovery middleware | Panic caught, 500 returned, stack trace logged. Server continues for other requests. |
| **File descriptor exhaustion** | HTTP server | `Accept()` fails. New connections rejected. Existing connections unaffected. |
| **DNS resolution fails for Neon** | pgxpool health check | Stale connections continue working. New connections fail. Pool shrinks. Eventually all connections die if DNS stays broken. |

---

## 8. Files

```text
cmd/
└── server/
    └── main.go          # Entry point: main(), shutdown(), connectPostgres(),
                         # runMigration(), bootstrapResidentAgent(),
                         # recoverInProgressMessages(), initLogging()

internal/
└── config/
    └── config.go        # Config struct, LoadConfig(), envOrDefault(),
                         # envIntOrDefault(), validation
```

### Environment Variables Summary (for `.env` / `EnvironmentFile=`)

```bash
# Required
DATABASE_URL=postgresql://user:pass@host:5432/agentmail?sslmode=require
S2_AUTH_TOKEN=s2_token_here

# Required if resident agent is enabled
RESIDENT_AGENT_ID=01906e5b-0000-7000-8000-000000000001
ANTHROPIC_API_KEY=sk-ant-...

# Optional (with defaults)
PORT=8080
LOG_LEVEL=info
S2_BASIN=agentmail
SHUTDOWN_TIMEOUT_SECONDS=30
CURSOR_FLUSH_INTERVAL_S=5
HEALTH_CHECK_TIMEOUT_S=5
```
