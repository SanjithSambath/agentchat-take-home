package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// Router returns a chi.Router with the Phase 0 middleware chain mounted
// and only /health wired. Phase 1 agents register their route groups by
// mounting onto this router in main.go.
//
// Middleware order (outermost → innermost) per http-api-layer-plan.md §2:
//
//	Recovery → RequestID → Logger → (AgentAuth) → (Timeout) → Handler
//
// Recovery wraps everything so panics become envelopes. RequestID comes
// before Logger so every log line carries request_id. AgentAuth and
// Timeout are applied per route group (Phase 1).
func Router() chi.Router {
	r := chi.NewRouter()
	r.Use(Recovery)
	r.Use(RequestID)
	r.Use(Logger)

	// Unauthenticated routes with a short timeout.
	r.Group(func(r chi.Router) {
		r.Use(Timeout(5 * time.Second))
		r.Get("/health", Health)
	})

	// Phase 1 agents mount authenticated route groups here. Stub route
	// group left intentionally empty so main.go compiles.
	_ = http.StatusOK
	return r
}
