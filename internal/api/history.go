package api

import (
	"net/http"
	"sort"

	"agentmail/internal/model"

	"github.com/rs/zerolog/log"
)

// GetHistory handles GET /conversations/{cid}/messages.
//
// Two mutually exclusive cursor modes:
//   - ?from=<seq>   : ASC catch-up (ack-aligned). Oldest unread first.
//   - ?before=<seq> : DESC pagination (default). Newest first.
//
// Providing both is a 400 mutually_exclusive_cursors. Neither is equivalent
// to `before=<tail>` — most-recent page from the conversation head.
//
// The handler oversamples the raw event range by a factor of 8 then runs
// model.AssembleMessages to group events by message_id. This trades a
// bounded amount of extra read bandwidth for simple one-shot pagination —
// assembling in the handler avoids inventing a message-indexed secondary
// index in the store layer.
func (h *Handler) GetHistory(w http.ResponseWriter, r *http.Request) {
	_, convID, ok := h.requireMembership(w, r)
	if !ok {
		return
	}

	limit, ok := parseLimitQuery(w, r, 50, 100)
	if !ok {
		return
	}
	fromVal, fromPresent, ok := parseUint64Query(w, r, "from", CodeInvalidFrom)
	if !ok {
		return
	}
	beforeVal, beforePresent, ok := parseUint64Query(w, r, "before", CodeInvalidBefore)
	if !ok {
		return
	}
	if fromPresent && beforePresent {
		WriteError(w, r, http.StatusBadRequest, CodeMutuallyExclusiveCursors,
			"?from and ?before are mutually exclusive")
		return
	}

	oversample := limit * 8

	if fromPresent {
		// ASC catch-up: events from fromVal forward.
		events, err := h.s2.ReadRange(r.Context(), convID, fromVal, oversample)
		if err != nil {
			log.Ctx(r.Context()).Error().Err(err).Msg("history read-range failed (from)")
			WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
			return
		}
		msgs := model.AssembleMessages(events)
		sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].SeqStart < msgs[j].SeqStart })

		hasMore := len(msgs) > limit
		if hasMore {
			msgs = msgs[:limit]
		}
		writeHistory(w, r, msgs, hasMore)
		return
	}

	// DESC pagination. Resolve `before`: if omitted, use head+1 so seq_start
	// < before matches every message up to and including the current tail.
	upper := beforeVal
	if !beforePresent {
		head, err := h.meta.GetConversationHeadSeq(r.Context(), convID)
		if err != nil {
			log.Ctx(r.Context()).Error().Err(err).Msg("history head_seq read failed")
			WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
			return
		}
		upper = head + 1
	}

	// Walk backward in chunks until we have at least `limit` complete
	// messages strictly below `upper`, or we've exhausted the stream.
	const maxPages = 4
	var collected []model.HistoryMessage
	cursor := upper
	pages := 0
	for pages < maxPages {
		pages++
		windowStart := uint64(0)
		if cursor > uint64(oversample) {
			windowStart = cursor - uint64(oversample)
		}
		events, err := h.s2.ReadRange(r.Context(), convID, windowStart, oversample)
		if err != nil {
			log.Ctx(r.Context()).Error().Err(err).Msg("history read-range failed (before)")
			WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
			return
		}
		msgs := model.AssembleMessages(events)
		for _, m := range msgs {
			if m.SeqStart < upper {
				collected = append(collected, m)
			}
		}
		if windowStart == 0 || len(collected) > limit {
			break
		}
		cursor = windowStart
	}

	sort.SliceStable(collected, func(i, j int) bool {
		return collected[i].SeqStart > collected[j].SeqStart
	})
	hasMore := len(collected) > limit
	if hasMore {
		collected = collected[:limit]
	}
	writeHistory(w, r, collected, hasMore)
}

func writeHistory(w http.ResponseWriter, r *http.Request, msgs []model.HistoryMessage, hasMore bool) {
	out := make([]HistoryMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, HistoryMessage{
			MessageID: m.MessageID,
			SenderID:  m.SenderID,
			Content:   m.Content,
			SeqStart:  m.SeqStart,
			SeqEnd:    m.SeqEnd,
			Status:    m.Status,
		})
	}
	WriteJSON(w, r, http.StatusOK, HistoryResponse{Messages: out, HasMore: hasMore})
}
