// Command server is the AgentMail HTTP entry point.
//
// Responsibilities:
//
//  1. Load + validate config, wire zerolog.
//  2. Bring up the Postgres metadata store (pgx pool + idempotent migration +
//     agent-existence cache warm).
//  3. Construct the S2 store and probe connectivity (best-effort warn).
//  4. Run the recovery sweep: finalize any in_progress_messages rows left by a
//     prior crash by appending message_abort to their S2 streams and moving
//     them to messages_dedup as aborted.
//  5. Start the delivery cursor flusher.
//  6. If configured, construct the Anthropic client and start the resident
//     Claude agent.
//  7. Build the HTTP router and serve.
//  8. On SIGINT/SIGTERM (e.g. a `cloudflared tunnel` restart or local Ctrl-C),
//     drain in this order: HTTP server (graceful, then hard), resident agent,
//     per-pair connection registry (via per-agent leaves that already happened;
//     otherwise BaseContext cancellation), final cursor flush, Postgres close.
//     See ALL_DESIGN_IMPLEMENTATION/server-lifecycle-plan.md §Shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"agentmail/internal/agent"
	"agentmail/internal/api"
	"agentmail/internal/config"
	"agentmail/internal/model"
	"agentmail/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	// 1. Config — fail fast on missing required env vars.
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// 2. Logging.
	initLogging(cfg.LogLevel)
	log.Info().
		Uint16("port", cfg.Port).
		Str("s2_basin", cfg.S2Basin).
		Bool("resident_agent", cfg.HasResidentAgent()).
		Msg("startup: config loaded")

	// 3. Root context wired to SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 4. Postgres metadata store.
	meta, err := store.NewMetadataStoreWithOptions(ctx, cfg.DatabaseURL, store.MetadataStoreOptions{
		CursorFlushInterval: cfg.CursorFlushInterval,
	})
	if err != nil {
		return fmt.Errorf("startup: metadata store: %w", err)
	}
	defer func() {
		if err := meta.Close(); err != nil {
			log.Warn().Err(err).Msg("shutdown: metadata store close error")
		}
	}()

	// 5. S2 store (no network I/O here; the SDK is lazy).
	s2, err := store.NewS2Store(cfg)
	if err != nil {
		return fmt.Errorf("startup: s2 store: %w", err)
	}

	// 6. S2 connectivity probe — best-effort. A "stream not found" error on a
	// nil UUID is a successful auth/round-trip; anything else is a warn.
	probeS2(ctx, s2)

	// 7. Recovery sweep — finalize crashed in-flight writes from the prior
	// incarnation. Per-row failures are warned and skipped; the sweep is
	// self-healing on the next restart.
	recoverInProgressMessages(ctx, meta, s2)

	// 8. Start the delivery cursor flusher. Idempotent; safe to call once.
	meta.StartDeliveryCursorFlusher(ctx)

	// 9. Resident Claude agent (conditional).
	var resident api.ResidentInfo = api.DisabledResident{}
	var agentInst *agent.Agent
	if cfg.HasResidentAgent() {
		claude := anthropic.NewClient(option.WithAPIKey(cfg.AnthropicAPIKey))
		a, err := agent.NewAgent(cfg, s2, meta, &claude)
		if err != nil {
			log.Warn().Err(err).Msg("startup: resident agent construction failed; continuing without it")
		} else {
			startCtx, startCancel := context.WithTimeout(ctx, 30*time.Second)
			if err := a.Start(startCtx); err != nil {
				log.Warn().Err(err).Msg("startup: resident agent Start failed; continuing without it")
			} else {
				agentInst = a
				resident = a
				log.Info().Str("agent_id", a.ID().String()).Msg("startup: resident agent ready")
			}
			startCancel()
		}
	}

	// 10. HTTP router + server.
	conns := api.NewConnRegistry()
	router := api.NewRouter(s2, meta, resident, conns)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 11. Serve.
	errCh := make(chan error, 1)
	go func() {
		log.Info().Str("addr", srv.Addr).Msg("http: listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	// 12. Wait for signal or serve error.
	select {
	case <-ctx.Done():
		log.Info().Msg("shutdown: signal received")
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http: serve: %w", err)
		}
	}

	// 13. Graceful shutdown — reverse order of bring-up.
	return shutdown(srv, agentInst, meta, cfg.ShutdownTimeout)
}

// probeS2 calls CheckTail on a nil UUID just to exercise the HTTP/auth path.
// The SDK returns a stream-not-found error for an unknown stream; we treat
// that as success. Any other error is a warn, never fatal.
func probeS2(ctx context.Context, s2 store.S2Store) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s2.CheckTail(probeCtx, uuid.Nil); err != nil {
		// "stream not found" is expected; anything else is a warn.
		log.Info().Err(err).Msg("startup: s2 probe completed (not-found expected)")
		return
	}
	log.Info().Msg("startup: s2 probe ok")
}

// recoverInProgressMessages iterates every row in in_progress_messages,
// writes a message_abort(server_crash) to its S2 stream, records the dedup
// row as aborted, and deletes the claim. The abort append is the only
// irreversible step; the dedup insert and in-progress delete are retried
// with a short backoff so a transient Postgres blip doesn't orphan a row
// that will otherwise fire a duplicate abort on the next restart.
func recoverInProgressMessages(ctx context.Context, meta store.MetadataStore, s2 store.S2Store) {
	rows, err := meta.ListInProgressMessages(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("startup: recovery sweep list failed; skipping")
		return
	}
	if len(rows) == 0 {
		log.Info().Msg("startup: recovery sweep: no in-flight messages")
		return
	}
	recovered, orphaned := 0, 0
	for _, row := range rows {
		abort := model.NewMessageAbortEvent(row.MessageID, model.AbortReasonServerCrash)
		if _, err := s2.AppendEvents(ctx, row.ConversationID, []model.Event{abort}); err != nil {
			log.Warn().
				Err(err).
				Str("message_id", row.MessageID.String()).
				Str("conversation_id", row.ConversationID.String()).
				Msg("startup: recovery: append abort failed")
			continue
		}
		if err := retryMeta(ctx, func() error {
			return meta.InsertMessageDedupAborted(ctx, row.ConversationID, row.MessageID, nil)
		}); err != nil {
			log.Error().
				Err(err).
				Str("message_id", row.MessageID.String()).
				Msg("startup: recovery: dedup insert failed after retries")
		}
		if err := retryMeta(ctx, func() error {
			return meta.DeleteInProgressMessage(ctx, row.MessageID)
		}); err != nil {
			log.Error().
				Err(err).
				Str("message_id", row.MessageID.String()).
				Msg("startup: recovery: delete in-progress row failed after retries — will retry on next restart")
			orphaned++
			continue
		}
		recovered++
	}
	log.Info().
		Int("recovered", recovered).
		Int("orphaned", orphaned).
		Int("total", len(rows)).
		Msg("startup: recovery sweep complete")
}

// retryMeta runs fn with short exponential backoff (3 attempts, ~1.5s total)
// so a transient Postgres blip during the recovery sweep doesn't force the
// row into orphan state.
func retryMeta(ctx context.Context, fn func() error) error {
	var err error
	backoff := 100 * time.Millisecond
	for attempt := 0; attempt < 3; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return err
		}
		backoff *= 3
	}
	return err
}

// shutdown runs the reverse-of-startup drain with a single overall budget.
// Order: HTTP graceful → resident agent → final cursor flush → metadata Close
// (deferred in run()). The HTTP server's own graceful Shutdown cancels live
// request contexts, which in turn unblocks any SSE/streaming handler that
// ties its lifetime to r.Context().
func shutdown(srv *http.Server, a *agent.Agent, meta store.MetadataStore, budget time.Duration) error {
	started := time.Now()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn().Err(err).Msg("shutdown: http graceful failed; closing hard")
		_ = srv.Close()
	}

	if a != nil {
		// Derive the agent budget from the shared shutdownCtx so the outer
		// deadline dominates: if HTTP drain already consumed most of the
		// budget, the agent gets what's left rather than its own fresh 10s.
		agentBudget := 10 * time.Second
		if d, ok := shutdownCtx.Deadline(); ok {
			if remaining := time.Until(d); remaining < agentBudget {
				agentBudget = remaining
			}
		}
		agentCtx, agentCancel := context.WithTimeout(shutdownCtx, agentBudget)
		a.Shutdown(agentCtx)
		agentCancel()
	}

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := meta.FlushDeliveryCursorAll(flushCtx); err != nil {
		log.Warn().Err(err).Msg("shutdown: final cursor flush error")
	}
	flushCancel()

	log.Info().Dur("elapsed", time.Since(started)).Msg("shutdown: complete")
	return nil
}

// initLogging configures the process-global zerolog logger.
func initLogging(levelStr string) {
	level, err := zerolog.ParseLevel(levelStr)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	log.Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()
}
