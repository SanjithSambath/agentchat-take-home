package api

import (
	"net/http"

	"github.com/rs/zerolog/log"
)

// AckCursor handles POST /conversations/{cid}/ack. Advances the ack_seq for
// (agent, conv) synchronously. Idempotent: regressions (seq < current) are
// silent no-ops per the UPSERT guard in the store layer.
func (h *Handler) AckCursor(w http.ResponseWriter, r *http.Request) {
	agentID, convID, ok := h.requireMembership(w, r)
	if !ok {
		return
	}
	if !validateContentType(w, r, "application/json") {
		return
	}
	body, ok := decodeJSON[AckRequest](w, r, 1<<10)
	if !ok {
		return
	}

	headSeq, err := h.meta.GetConversationHeadSeq(r.Context(), convID)
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("head_seq read failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	if body.Seq > headSeq {
		WriteError(w, r, http.StatusBadRequest, CodeAckBeyondHead,
			"ack seq is beyond the conversation head")
		return
	}

	if err := h.meta.Ack(r.Context(), agentID, convID, body.Seq); err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("ack write failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListUnread handles GET /agents/me/unread. Returns every conversation where
// head_seq > ack_seq for the calling agent, up to ?limit=N (default 100,
// max 500).
func (h *Handler) ListUnread(w http.ResponseWriter, r *http.Request) {
	agentID := AgentIDFromContext(r.Context())

	limit, ok := parseLimitQuery(w, r, 100, 500)
	if !ok {
		return
	}

	entries, err := h.meta.ListUnreadForAgent(r.Context(), agentID, int32(limit))
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("list unread failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}

	out := make([]UnreadEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, UnreadEntry{
			ConversationID: e.ConversationID,
			HeadSeq:        e.HeadSeq,
			AckSeq:         e.AckSeq,
			EventDelta:     e.EventDelta,
		})
	}
	WriteJSON(w, r, http.StatusOK, UnreadResponse{Conversations: out})
}
