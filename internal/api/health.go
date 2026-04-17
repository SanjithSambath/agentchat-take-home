package api

import "net/http"

// HealthResponse is the body of GET /health.
// Phase 0 is a bare liveness check — returns 200 as long as the process is
// running. Phase 1 will extend this with a Postgres ping and an S2
// connectivity probe (see server-lifecycle-plan.md §Health Checks).
type HealthResponse struct {
	OK bool `json:"ok"`
}

// Health handles GET /health.
func Health(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, r, http.StatusOK, HealthResponse{OK: true})
}
