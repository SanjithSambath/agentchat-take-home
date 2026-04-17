package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"agentmail/internal/model"
	"agentmail/internal/store"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// CreateConversation handles POST /conversations. The X-Agent-ID caller is
// auto-added as the first member and an agent_joined event is appended to
// the new S2 stream.
//
// Ordering rationale: create the Postgres rows (conversation + membership)
// before the S2 append. A successful S2 append that can't be followed by a
// successful membership write leaves a stream that no one owns; reversing
// the order avoids that. If the S2 append fails afterwards we delete nothing
// — the conversation row is fine, it just has no agent_joined event yet,
// and the recovery sweep does not need to do anything special about it.
func (h *Handler) CreateConversation(w http.ResponseWriter, r *http.Request) {
	agentID := AgentIDFromContext(r.Context())

	convID, err := uuid.NewV7()
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("uuid v7 generation failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	streamName := store.StreamNameForConversation(convID)

	if err := h.meta.CreateConversation(r.Context(), convID, streamName); err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("create conversation failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	if _, err := h.meta.AddMember(r.Context(), convID, agentID); err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("add creator membership failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}

	res, err := h.s2.AppendEvents(r.Context(), convID, []model.Event{model.NewAgentJoinedEvent(agentID)})
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("append agent_joined failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	if res.EndSeq > 0 {
		if err := h.meta.UpdateConversationHeadSeq(r.Context(), convID, res.EndSeq-1); err != nil {
			log.Ctx(r.Context()).Warn().Err(err).Msg("head_seq advance failed (non-fatal)")
		}
	}

	conv, err := h.meta.GetConversation(r.Context(), convID)
	if err != nil || conv == nil {
		// Fall back to synthesizing created_at — the row definitely exists,
		// we just couldn't read it back. Clients still get a usable response.
		WriteJSON(w, r, http.StatusCreated, CreateConversationResponse{
			ConversationID: convID,
			Members:        []uuid.UUID{agentID},
			CreatedAt:      time.Now().UTC(),
		})
		return
	}
	WriteJSON(w, r, http.StatusCreated, CreateConversationResponse{
		ConversationID: convID,
		Members:        []uuid.UUID{agentID},
		CreatedAt:      conv.CreatedAt.UTC(),
	})
}

// ListConversations handles GET /conversations. Returns every conversation
// the caller is currently a member of. Members list is populated per-row
// via ListMembers so clients don't have to make N extra calls.
func (h *Handler) ListConversations(w http.ResponseWriter, r *http.Request) {
	agentID := AgentIDFromContext(r.Context())

	convs, err := h.meta.ListConversationsForAgent(r.Context(), agentID)
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("list conversations failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}

	out := make([]ConversationSummary, 0, len(convs))
	for _, c := range convs {
		members, err := h.meta.ListMembers(r.Context(), c.ID)
		if err != nil {
			log.Ctx(r.Context()).Error().Err(err).Msg("list members failed")
			WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
			return
		}
		out = append(out, ConversationSummary{
			ConversationID: c.ID,
			Members:        members,
			CreatedAt:      c.CreatedAt.UTC(),
		})
	}
	WriteJSON(w, r, http.StatusOK, ListConversationsResponse{Conversations: out})
}

// InviteAgent handles POST /conversations/{cid}/invite. Idempotent: re-inviting
// a current member returns 200 with already_member=true and skips the S2
// append (otherwise the stream would accrue no-op agent_joined events).
func (h *Handler) InviteAgent(w http.ResponseWriter, r *http.Request) {
	_, convID, ok := h.requireMembership(w, r)
	if !ok {
		return
	}

	if !validateContentType(w, r, "application/json") {
		return
	}
	body, ok := decodeJSON[InviteRequest](w, r, 1<<10)
	if !ok {
		return
	}
	if body.AgentID == uuid.Nil {
		WriteError(w, r, http.StatusBadRequest, CodeMissingField, "agent_id is required")
		return
	}

	exists, err := h.meta.AgentExists(r.Context(), body.AgentID)
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("invitee existence check failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	if !exists {
		WriteError(w, r, http.StatusNotFound, CodeInviteeNotFound,
			fmt.Sprintf("agent %s not found", body.AgentID))
		return
	}

	wasNew, err := h.meta.AddMember(r.Context(), convID, body.AgentID)
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("add member failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}

	if wasNew {
		res, err := h.s2.AppendEvents(r.Context(), convID, []model.Event{model.NewAgentJoinedEvent(body.AgentID)})
		if err != nil {
			log.Ctx(r.Context()).Error().Err(err).Msg("append agent_joined (invite) failed")
			WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
			return
		}
		if res.EndSeq > 0 {
			if err := h.meta.UpdateConversationHeadSeq(r.Context(), convID, res.EndSeq-1); err != nil {
				log.Ctx(r.Context()).Warn().Err(err).Msg("head_seq advance failed (non-fatal)")
			}
		}
		// If the invitee is the resident agent, poke its discovery loop so
		// it opens a read session without waiting for a restart.
		if h.resident.Available() && body.AgentID == h.resident.ID() {
			h.resident.NotifyInvite(convID)
		}
	}

	WriteJSON(w, r, http.StatusOK, InviteResponse{
		ConversationID: convID,
		AgentID:        body.AgentID,
		AlreadyMember:  !wasNew,
	})
}

// LeaveConversation handles POST /conversations/{cid}/leave.
//
// Flow (per sql-metadata-plan §9 and http-api-layer-plan):
//  1. Cancel active SSE + streaming writes for this (agent, conv) via the
//     connection registry, then wait up to 5 s for the goroutines to reach
//     their abort code paths. Otherwise a streaming write could still be
//     submitting records after the row is removed.
//  2. LockMembersForUpdate + RemoveMember — Agent 1's MetadataStore is
//     responsible for running these in one pgx transaction. The interface
//     exposes them as two calls but their implementation must share a tx.
//  3. Append agent_left to the S2 stream.
func (h *Handler) LeaveConversation(w http.ResponseWriter, r *http.Request) {
	agentID, convID, ok := h.requireMembership(w, r)
	if !ok {
		return
	}

	done := h.conns.LookupAndCancel(agentID, convID)
	if !WaitFor(done, 5*time.Second) {
		log.Ctx(r.Context()).Warn().
			Str("agent_id", agentID.String()).
			Str("conversation_id", convID.String()).
			Msg("leave: timed out waiting for connection deregistration; proceeding anyway")
	}

	// If the resident agent is the one leaving, drain its per-conversation
	// listener first so any in-flight message_abort lands before agent_left.
	if h.resident.Available() && agentID == h.resident.ID() {
		if !WaitFor(h.resident.NotifyLeave(convID), 5*time.Second) {
			log.Ctx(r.Context()).Warn().
				Str("conversation_id", convID.String()).
				Msg("leave: timed out waiting for resident listener drain; proceeding anyway")
		}
	}

	if _, err := h.meta.LockMembersForUpdate(r.Context(), convID); err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("lock members for update failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}

	if err := h.meta.RemoveMember(r.Context(), convID, agentID); err != nil {
		switch {
		case errors.Is(err, store.ErrLastMember):
			WriteError(w, r, http.StatusConflict, CodeLastMember,
				"cannot leave: you are the last member of this conversation")
		case errors.Is(err, store.ErrNotMember):
			// Race: someone (or another attempt) already removed us while
			// we were cancelling connections. Treat as success.
			WriteError(w, r, http.StatusForbidden, CodeNotMember,
				"agent is not a member of this conversation")
		default:
			log.Ctx(r.Context()).Error().Err(err).Msg("remove member failed")
			WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		}
		return
	}

	// Detach from the request context so a client disconnect doesn't prevent
	// the agent_left append from landing — we've already removed the row.
	detached, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := h.s2.AppendEvents(detached, convID, []model.Event{model.NewAgentLeftEvent(agentID)})
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("append agent_left failed")
		// The row is gone — still return 200. The stream is eventually
		// consistent via the recovery sweep / next writer.
	}
	if err == nil && res.EndSeq > 0 {
		if err := h.meta.UpdateConversationHeadSeq(detached, convID, res.EndSeq-1); err != nil {
			log.Ctx(r.Context()).Warn().Err(err).Msg("head_seq advance failed (non-fatal)")
		}
	}

	WriteJSON(w, r, http.StatusOK, LeaveResponse{
		ConversationID: convID,
		AgentID:        agentID,
	})
}
