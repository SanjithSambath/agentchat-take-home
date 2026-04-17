// Command server is the AgentMail HTTP entry point.
//
// Phase 0 responsibility: load config, build the chi router with middleware,
// serve /health. Phase 1 will extend this with store/S2 construction,
// recovery sweep, cursor flusher, and resident agent bootstrap per
// ALL_DESIGN_IMPLEMENTATION/server-lifecycle-plan.md §Startup Sequence.
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

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"agentmail/internal/api"
	"agentmail/internal/config"
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

	// 2. Logging — zerolog wired to stderr at the configured level.
	initLogging(cfg.LogLevel)
	log.Info().
		Uint16("port", cfg.Port).
		Str("s2_basin", cfg.S2Basin).
		Bool("resident_agent", cfg.HasResidentAgent()).
		Msg("startup: config loaded")

	// 3. Root context wired to SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Phase 1 startup steps land between here and the HTTP serve call:
	//   - Postgres connection + migration + cache warming
	//   - S2 client construction + recovery sweep
	//   - Cursor flusher goroutine
	//   - Resident agent bootstrap
	// See server-lifecycle-plan.md §Startup Sequence.

	// 4. HTTP router + server.
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           api.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 5. Serve in a goroutine so we can select on shutdown signals.
	errCh := make(chan error, 1)
	go func() {
		log.Info().Str("addr", srv.Addr).Msg("http: listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	// 6. Wait for signal or serve error.
	select {
	case <-ctx.Done():
		log.Info().Msg("shutdown: signal received")
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http: serve: %w", err)
		}
	}

	// 7. Graceful shutdown with hard deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn().Err(err).Msg("http: shutdown error")
	}
	log.Info().Msg("shutdown: complete")
	return nil
}

// initLogging configures the process-global zerolog logger. Phase 0 uses
// the default JSON writer; Phase 1 may swap in a ConsoleWriter for local
// dev based on a config flag.
func initLogging(levelStr string) {
	level, err := zerolog.ParseLevel(levelStr)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	log.Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()
}
