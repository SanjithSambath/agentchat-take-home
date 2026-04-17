package api

import (
	"net/http"

	"github.com/rs/zerolog/log"
)

// CreateAgent handles POST /agents. No body, no auth. Returns the new
// agent id assigned by the store (which also populates its existence cache).
func (h *Handler) CreateAgent(w http.ResponseWriter, r *http.Request) {
	id, err := h.meta.CreateAgent(r.Context())
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("create agent failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	WriteJSON(w, r, http.StatusCreated, CreateAgentResponse{AgentID: id})
}

// GetResidentAgent handles GET /agents/resident. When the resident agent is
// disabled (DisabledResident or the feature flag is off), return 503 with
// the dedicated resident_agent_unavailable code so callers can distinguish
// "not configured" from a transient failure.
func (h *Handler) GetResidentAgent(w http.ResponseWriter, r *http.Request) {
	if !h.resident.Available() {
		WriteError(w, r, http.StatusServiceUnavailable, CodeResidentUnavailable,
			"resident agent is not configured")
		return
	}
	WriteJSON(w, r, http.StatusOK, GetResidentAgentResponse{AgentID: h.resident.ID()})
}
