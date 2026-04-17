package api

import (
	"context"
	"net/http"
	"runtime/debug"
	"time"

	"agentmail/internal/store"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// contextKey is the private type for context keys so collisions with other
// packages are impossible.
type contextKey string

const (
	ctxRequestID contextKey = "request_id"
	ctxAgentID   contextKey = "agent_id"
)

// RequestID middleware accepts an incoming X-Request-ID (≤128 chars) or
// generates a fresh UUIDv4, and attaches it to the request context plus
// the response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" || len(rid) > 128 {
			rid = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", rid)
		ctx := context.WithValue(r.Context(), ctxRequestID, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext extracts the request id, returning empty string when
// the middleware hasn't run (tests / panics before RequestID).
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

// AgentIDFromContext extracts the authenticated agent id. Returns uuid.Nil
// if AgentAuth didn't run.
func AgentIDFromContext(ctx context.Context) uuid.UUID {
	if v, ok := ctx.Value(ctxAgentID).(uuid.UUID); ok {
		return v
	}
	return uuid.Nil
}

// WithAgentID returns a derived context carrying agentID. Used by AgentAuth
// (Phase 1) and tests.
func WithAgentID(ctx context.Context, agentID uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxAgentID, agentID)
}

// Logger middleware attaches a per-request zerolog.Logger (with request_id
// baked in) to the context and emits a single structured log line on
// completion with method, path, status, duration.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := RequestIDFromContext(r.Context())
		rlog := log.With().Str("request_id", rid).Logger()
		ctx := rlog.WithContext(r.Context())

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r.WithContext(ctx))

		evt := rlog.Info()
		if sw.status >= 500 {
			evt = rlog.Error()
		} else if sw.status >= 400 {
			evt = rlog.Warn()
		}
		evt.
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", sw.status).
			Dur("duration", time.Since(start)).
			Msg("request")
	})
}

// Recovery middleware is the outermost layer. It catches any panic, logs
// the stack, and returns a 500 internal_error envelope.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Ctx(r.Context()).
					Error().
					Interface("panic", rec).
					Bytes("stack", debug.Stack()).
					Msg("panic recovered")
				WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Timeout wraps the handler chain with a per-request deadline. Streaming
// routes bypass this middleware and manage their own idle detection.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// statusWriter captures the status code written by the inner handler so
// the Logger middleware can log it.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.wroteHeader {
		sw.status = code
		sw.wroteHeader = true
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.wroteHeader = true
	}
	return sw.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying ResponseWriter so http.NewResponseController
// can reach Flusher/Hijacker/SetReadDeadline on the concrete writer beneath
// this middleware.
func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

// AgentAuth validates the X-Agent-ID header: present, well-formed UUID, and
// matching a real agent row per the store's cached existence check. On
// success the resolved uuid.UUID is attached to the context via WithAgentID
// so handlers can retrieve it with AgentIDFromContext. Mounted on the
// authenticated route group in NewRouter; the unauthenticated group
// (POST /agents, GET /agents/resident, GET /health) skips it.
func AgentAuth(meta store.MetadataStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := r.Header.Get("X-Agent-ID")
			if raw == "" {
				WriteError(w, r, http.StatusBadRequest, CodeMissingAgentID,
					"X-Agent-ID header is required")
				return
			}
			id, err := uuid.Parse(raw)
			if err != nil {
				WriteError(w, r, http.StatusBadRequest, CodeInvalidAgentID,
					"X-Agent-ID must be a valid UUID")
				return
			}
			exists, err := meta.AgentExists(r.Context(), id)
			if err != nil {
				log.Ctx(r.Context()).Error().Err(err).Msg("agent auth: store check failed")
				WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
				return
			}
			if !exists {
				WriteError(w, r, http.StatusNotFound, CodeAgentNotFound,
					"agent not found; POST /agents to register")
				return
			}
			ctx := WithAgentID(r.Context(), id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Ensure zerolog package is referenced so unused-import lint stays silent
// if future edits drop the With()-chain above.
var _ = zerolog.LevelInfoValue
