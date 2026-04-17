package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentmail/internal/store"

	"github.com/rs/zerolog/log"
)

// Per-dependency ping budget. The spec (server-lifecycle-plan.md §Health
// Checks) allots 5 s for the whole endpoint; running both checks in parallel
// with a 3 s sub-budget each keeps us comfortably under that ceiling.
const healthPingTimeout = 3 * time.Second

// Status strings reported in the response body.
const (
	healthStatusOK      = "ok"
	healthStatusFail    = "unhealthy"
	healthStatusDegrade = "degraded"
)

// HealthResponse is the JSON envelope of GET /health. Status is the overall
// verdict; Checks carries the per-dependency result so operators can see
// which leg is down at a glance.
type HealthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// Health runs Postgres and S2 pings in parallel, each under healthPingTimeout,
// and returns 200 when both succeed or 503 when either fails. The body carries
// per-dependency status for debuggability.
//
// S2's CheckTail on the nil UUID is expected to return a stream-not-found
// error on a healthy basin — that proves the auth / transport round-tripped.
// Any other error (network, auth, basin misconfig) is treated as failure.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	var wg sync.WaitGroup
	var pgStatus, s2Status string

	wg.Add(2)
	go func() {
		defer wg.Done()
		pgStatus = pingPostgres(r.Context(), h.meta)
	}()
	go func() {
		defer wg.Done()
		s2Status = pingS2(r.Context(), h.s2)
	}()
	wg.Wait()

	code := http.StatusOK
	overall := healthStatusOK
	if pgStatus != healthStatusOK || s2Status != healthStatusOK {
		code = http.StatusServiceUnavailable
		overall = healthStatusFail
	}

	WriteJSON(w, r, code, HealthResponse{
		Status: overall,
		Checks: map[string]string{
			"postgres": pgStatus,
			"s2":       s2Status,
		},
	})
}

func pingPostgres(parent context.Context, meta store.MetadataStore) string {
	ctx, cancel := context.WithTimeout(parent, healthPingTimeout)
	defer cancel()
	if err := meta.Ping(ctx); err != nil {
		log.Ctx(parent).Warn().Err(err).Msg("health: postgres ping failed")
		return healthStatusFail
	}
	return healthStatusOK
}

func pingS2(parent context.Context, s2 store.S2Store) string {
	ctx, cancel := context.WithTimeout(parent, healthPingTimeout)
	defer cancel()
	// A stream-not-found on the nil UUID still proves HTTP + auth + basin
	// routing all worked; treat it as success.
	if _, err := s2.CheckTail(ctx, uuid.Nil); err != nil {
		if errors.Is(err, store.ErrRangeNotSatisfiable) || isS2NotFound(err) {
			return healthStatusOK
		}
		log.Ctx(parent).Warn().Err(err).Msg("health: s2 probe failed")
		return healthStatusFail
	}
	return healthStatusOK
}

// isS2NotFound returns true when err looks like an S2 "stream not found"
// response, which on a nil-UUID probe is the happy path. Matched on error
// text because the SDK's typed error isn't exported uniformly across
// versions; store.ErrRangeNotSatisfiable covers the primary case, this is
// a belt-and-suspenders fallback.
func isS2NotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "stream not found") ||
		strings.Contains(msg, "NotFound") ||
		strings.Contains(msg, "not_found")
}
