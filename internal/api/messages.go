package api

import (
	"context"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"agentmail/internal/model"
	"agentmail/internal/store"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// SendMessage handles POST /conversations/{cid}/messages — the complete-send
// write path. The dedup gate is mandatory and ordered:
//
//  1. GetMessageDedup — if complete, replay cached response (200). If
//     aborted, 409 already_aborted.
//  2. ClaimInProgressMessage — enforces the cross-process UNIQUE constraint
//     on (conv_id, message_id). False ⇒ 409 in_progress_conflict.
//  3. Unary append of the 3-record batch [message_start, message_append,
//     message_end] — single S2 RPC, atomically sequenced.
//  4. On success: InsertMessageDedupComplete → UpdateConversationHeadSeq →
//     DeleteInProgressMessage. Return 201.
//  5. On S2 failure: best-effort abort (unary message_abort append,
//     InsertMessageDedupAborted, DeleteInProgressMessage). Detach from the
//     request context so a slow abort doesn't block on client disconnect.
func (h *Handler) SendMessage(w http.ResponseWriter, r *http.Request) {
	agentID, convID, ok := h.requireMembership(w, r)
	if !ok {
		return
	}
	if !validateContentType(w, r, "application/json") {
		return
	}
	body, ok := decodeJSON[SendMessageRequest](w, r, 1<<20)
	if !ok {
		return
	}

	if body.MessageID == uuid.Nil {
		WriteError(w, r, http.StatusBadRequest, CodeMissingMessageID, "message_id is required")
		return
	}
	if !isUUIDv7(body.MessageID) {
		WriteError(w, r, http.StatusBadRequest, CodeInvalidMessageID, "message_id must be a UUIDv7")
		return
	}
	if body.Content == "" {
		WriteError(w, r, http.StatusBadRequest, CodeEmptyContent, "content must not be empty")
		return
	}
	if !utf8.ValidString(body.Content) {
		WriteError(w, r, http.StatusBadRequest, CodeInvalidUTF8, "content is not valid UTF-8")
		return
	}

	// Dedup gate.
	if row, found, err := h.meta.GetMessageDedup(r.Context(), convID, body.MessageID); err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("dedup lookup failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	} else if found {
		switch row.Status {
		case model.StatusComplete:
			var start, end uint64
			if row.StartSeq != nil {
				start = *row.StartSeq
			}
			if row.EndSeq != nil {
				end = *row.EndSeq
			}
			WriteJSON(w, r, http.StatusOK, SendMessageResponse{
				MessageID:        body.MessageID,
				SeqStart:         start,
				SeqEnd:           end,
				AlreadyProcessed: true,
			})
			return
		case model.StatusAborted:
			WriteError(w, r, http.StatusConflict, CodeAlreadyAborted,
				"a prior attempt with this message_id was aborted")
			return
		}
	}

	claimed, err := h.meta.ClaimInProgressMessage(r.Context(), body.MessageID, convID, agentID,
		store.StreamNameForConversation(convID))
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("claim in-progress failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	if !claimed {
		WriteError(w, r, http.StatusConflict, CodeInProgressConflict,
			"another write is already in-flight for this message_id")
		return
	}

	events := model.NewCompleteMessageEvents(body.MessageID, agentID, body.Content)
	res, err := h.s2.AppendEvents(r.Context(), convID, events)
	if err != nil {
		h.finalizeCompleteAbort(r.Context(), convID, body.MessageID, err)
		if errors.Is(err, store.ErrSlowWriter) {
			WriteError(w, r, http.StatusServiceUnavailable, CodeSlowWriter,
				"append timed out; try again later")
			return
		}
		log.Ctx(r.Context()).Error().Err(err).Msg("append complete-message failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}

	// seq_start = first record seq; seq_end = last record seq. AppendResult
	// is half-open: [StartSeq, EndSeq).
	seqStart := res.StartSeq
	seqEnd := res.EndSeq
	if seqEnd > 0 {
		seqEnd--
	}

	if err := h.meta.InsertMessageDedupComplete(r.Context(), convID, body.MessageID, seqStart, seqEnd); err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("dedup insert-complete failed (write already landed)")
	}
	if err := h.meta.UpdateConversationHeadSeq(r.Context(), convID, seqEnd); err != nil {
		log.Ctx(r.Context()).Warn().Err(err).Msg("head_seq advance failed (non-fatal)")
	}
	if err := h.meta.DeleteInProgressMessage(r.Context(), body.MessageID); err != nil {
		log.Ctx(r.Context()).Warn().Err(err).Msg("in-progress cleanup failed (non-fatal)")
	}

	WriteJSON(w, r, http.StatusCreated, SendMessageResponse{
		MessageID:        body.MessageID,
		SeqStart:         seqStart,
		SeqEnd:           seqEnd,
		AlreadyProcessed: false,
	})
}

// finalizeCompleteAbort is the best-effort cleanup after a failed complete
// send. Detached from the request context so a client disconnect doesn't
// skip the dedup-aborted write — a retry with the same message_id would then
// silently re-enter the write path.
func (h *Handler) finalizeCompleteAbort(reqCtx context.Context, convID, msgID uuid.UUID, origErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reason := model.AbortReasonServerCrash
	if errors.Is(origErr, store.ErrSlowWriter) {
		reason = model.AbortReasonSlowWriter
	}
	if _, err := h.s2.AppendEvents(ctx, convID, []model.Event{model.NewMessageAbortEvent(msgID, reason)}); err != nil {
		log.Ctx(reqCtx).Warn().Err(err).Msg("best-effort abort append failed")
	}
	if err := h.meta.InsertMessageDedupAborted(ctx, convID, msgID, nil); err != nil {
		log.Ctx(reqCtx).Warn().Err(err).Msg("dedup insert-aborted failed")
	}
	if err := h.meta.DeleteInProgressMessage(ctx, msgID); err != nil {
		log.Ctx(reqCtx).Warn().Err(err).Msg("in-progress cleanup (abort path) failed")
	}
}
