package api

import (
	"net/http"
	"time"

	"agentmail/internal/store"

	"github.com/go-chi/chi/v5"
)

// NewRouter wires every Phase 1 route on top of the frozen middleware chain
// and returns a ready-to-serve chi.Router.
//
// Middleware order (outermost → innermost):
//
//	Recovery → RequestID → Logger → AgentAuth → Timeout → Handler
//
// Three route groups:
//
//  1. Unauthenticated, short timeout (5s): /health, POST /agents,
//     GET /agents/resident.
//  2. Authenticated, CRUD timeout (30s): everything except the two streaming
//     endpoints.
//  3. Authenticated, no timeout: streaming writes and SSE — they manage
//     their own idle detection and absolute caps.
//
// The 404/405 handlers write the standard error envelope so clients don't
// have to special-case chi's default `text/plain` error pages.
func NewRouter(s2 store.S2Store, meta store.MetadataStore, resident ResidentInfo, conns *ConnRegistry) chi.Router {
	if conns == nil {
		conns = NewConnRegistry()
	}
	h := NewHandler(s2, meta, resident, conns)

	r := chi.NewRouter()
	r.Use(Recovery)
	r.Use(RequestID)
	r.Use(Logger)

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		WriteError(w, req, http.StatusNotFound, CodeNotFound, "not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		WriteError(w, req, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	})

	// Unauthenticated group.
	r.Group(func(r chi.Router) {
		r.Use(Timeout(5 * time.Second))
		r.Get("/health", h.Health)
		r.Post("/agents", h.CreateAgent)
		r.Get("/agents/resident", h.GetResidentAgent)
		r.Get("/client/run_agent.py", h.GetClientRunAgent)
	})

	// Observer group — omniscient read-only view for the UI. No AgentAuth,
	// no membership gate. SSE bypasses the Timeout middleware like the
	// member stream.
	r.Group(func(r chi.Router) {
		r.Use(Timeout(30 * time.Second))
		r.Get("/observer/conversations", h.ObserverListConversations)
		r.Get("/observer/conversations/{cid}/messages", h.ObserverGetHistory)
	})
	r.Group(func(r chi.Router) {
		r.Get("/observer/conversations/{cid}/stream", h.ObserverSSEStream)
	})

	// Authenticated, non-streaming group.
	r.Group(func(r chi.Router) {
		r.Use(AgentAuth(meta))
		r.Use(Timeout(30 * time.Second))

		r.Get("/conversations", h.ListConversations)
		r.Post("/conversations", h.CreateConversation)
		r.Get("/conversations/{cid}", h.GetConversation)

		r.Post("/conversations/{cid}/invite", h.InviteAgent)
		r.Post("/conversations/{cid}/leave", h.LeaveConversation)
		r.Post("/conversations/{cid}/messages", h.SendMessage)
		r.Get("/conversations/{cid}/messages", h.GetHistory)
		r.Post("/conversations/{cid}/ack", h.AckCursor)

		r.Get("/agents/me/unread", h.ListUnread)
	})

	// Authenticated, streaming group — no Timeout middleware; handlers
	// manage their own idle and absolute deadlines.
	r.Group(func(r chi.Router) {
		r.Use(AgentAuth(meta))

		r.Post("/conversations/{cid}/messages/stream", h.StreamMessage)
		r.Get("/conversations/{cid}/stream", h.SSEStream)
	})

	return r
}
