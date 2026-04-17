package agent

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"

	"agentmail/internal/store"
)

// registerIdentity verifies the resident agent's row exists in Postgres. The
// frozen MetadataStore.CreateAgent generates a fresh UUIDv7 — it cannot be
// used to idempotently insert the operator-supplied RESIDENT_AGENT_ID. So the
// agent calls AgentExists and logs a loud warning if the row is missing. Once
// the row is present (seeded by operator, or added via a future EnsureExists),
// AgentAuth middleware will succeed for the agent's writes.
//
// This is the "degraded fallback" documented in the approved plan.
func (a *Agent) registerIdentity(ctx context.Context) error {
	exists, err := a.meta.AgentExists(ctx, a.id)
	if err != nil {
		return err
	}
	if !exists {
		log.Warn().
			Str("agent_id", a.id.String()).
			Msg("agent: resident agent row absent in Postgres — AgentAuth will fail until the row is created; " +
				"operator must seed the row, or MetadataStore must expose EnsureExists")
	}
	return nil
}

// reconcileConversations lists every conversation the agent is a member of
// and starts a listener for each. Retries transient Postgres errors up to
// reconcileMaxRetries with 1s/2s/3s backoff (plan §3).
func (a *Agent) reconcileConversations(ctx context.Context) {
	var convs []store.Conversation
	var err error
	for attempt := 0; attempt < reconcileMaxRetries; attempt++ {
		convs, err = a.meta.ListConversationsForAgent(ctx, a.id)
		if err == nil {
			break
		}
		log.Warn().
			Err(err).
			Int("attempt", attempt+1).
			Msg("agent: reconcile retry")
		select {
		case <-time.After(time.Duration(attempt+1) * time.Second):
		case <-ctx.Done():
			return
		}
	}
	if err != nil {
		log.Error().
			Err(err).
			Msg("agent: reconcile failed after all retries — starting with no listeners")
		return
	}

	log.Info().
		Int("conversations", len(convs)).
		Msg("agent: reconciling existing conversations")
	for _, c := range convs {
		a.startListening(c.ID)
	}
}

// discoveryLoop consumes the invite channel. Exits when rootCtx is canceled
// (Shutdown path).
func (a *Agent) discoveryLoop() {
	for {
		select {
		case convID, ok := <-a.inviteCh:
			if !ok {
				return
			}
			a.startListening(convID)
		case <-a.rootCtx.Done():
			return
		}
	}
}
