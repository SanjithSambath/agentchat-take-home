package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"agentmail/internal/model"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Observer* handlers power the omniscient UI. They skip AgentAuth and the
// membership gate so the UI can see every conversation in the system without
// needing to be invited. Read-only by construction — the observer routes are
// GET-only and write no state to S2 or Postgres.

// ObserverListConversations handles GET /observer/conversations.
func (h *Handler) ObserverListConversations(w http.ResponseWriter, r *http.Request) {
	convs, err := h.meta.ListAllConversations(r.Context())
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("observer: list all conversations failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	ids := make([]uuid.UUID, 0, len(convs))
	for _, c := range convs {
		ids = append(ids, c.ID)
	}
	memberMap, err := h.meta.ListMembersForConversations(r.Context(), ids)
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("observer: bulk list members failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	out := make([]ConversationSummary, 0, len(convs))
	for _, c := range convs {
		members := memberMap[c.ID]
		if members == nil {
			members = []uuid.UUID{}
		}
		out = append(out, ConversationSummary{
			ConversationID: c.ID,
			Members:        members,
			CreatedAt:      c.CreatedAt.UTC(),
			HeadSeq:        c.HeadSeq,
		})
	}
	WriteJSON(w, r, http.StatusOK, ListConversationsResponse{Conversations: out})
}

// ObserverGetHistory handles GET /observer/conversations/{cid}/messages.
// Same assembly logic as GetHistory but with no membership check.
func (h *Handler) ObserverGetHistory(w http.ResponseWriter, r *http.Request) {
	convID, ok := parseConversationID(w, r)
	if !ok {
		return
	}
	exists, err := h.meta.ConversationExists(r.Context(), convID)
	if err != nil {
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	if !exists {
		WriteError(w, r, http.StatusNotFound, CodeConversationNotFound,
			fmt.Sprintf("conversation %s not found", convID))
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
		events, err := h.s2.ReadRange(r.Context(), convID, fromVal, oversample)
		if err != nil {
			log.Ctx(r.Context()).Error().Err(err).Msg("observer history read-range failed (from)")
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

	upper := beforeVal
	if !beforePresent {
		head, err := h.meta.GetConversationHeadSeq(r.Context(), convID)
		if err != nil {
			log.Ctx(r.Context()).Error().Err(err).Msg("observer history head_seq read failed")
			WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
			return
		}
		upper = head + 1
	}

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
			log.Ctx(r.Context()).Error().Err(err).Msg("observer history read-range failed (before)")
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

// ObserverSSEStream handles GET /observer/conversations/{cid}/stream.
// Attaches to the S2 read session with no membership check and no delivery
// cursor persistence — the observer is a pure tail, not an agent.
func (h *Handler) ObserverSSEStream(w http.ResponseWriter, r *http.Request) {
	convID, ok := parseConversationID(w, r)
	if !ok {
		return
	}
	exists, err := h.meta.ConversationExists(r.Context(), convID)
	if err != nil {
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	if !exists {
		WriteError(w, r, http.StatusNotFound, CodeConversationNotFound,
			fmt.Sprintf("conversation %s not found", convID))
		return
	}

	fromSeq, fromPresent, ok := parseUint64Query(w, r, "from", CodeInvalidFrom)
	if !ok {
		return
	}
	if !fromPresent {
		if lei := r.Header.Get("Last-Event-ID"); lei != "" {
			if v, err := strconv.ParseUint(lei, 10, 64); err == nil {
				fromSeq = v + 1
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), sseAbsoluteDuration)
	defer cancel()

	session, err := h.s2.OpenReadSession(ctx, convID, fromSeq)
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("observer sse: open read session failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "could not open read session")
		return
	}
	defer session.Close()

	rc := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(":ok\n\n"))
	if err := rc.Flush(); err != nil {
		return
	}

	eventCh := make(chan model.SequencedEvent, sseChanCap)
	tailerDone := make(chan struct{})
	go func() {
		defer close(tailerDone)
		for session.Next() {
			ev := session.Event()
			select {
			case eventCh <- ev:
			case <-ctx.Done():
				return
			case <-time.After(sseSlowWriterGrace):
				cancel()
				return
			}
		}
	}()

	hb := time.NewTicker(sseHeartbeatPeriod)
	defer hb.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tailerDone:
			if err := session.Err(); err != nil {
				body, _ := json.Marshal(struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				}{Code: CodeS2ReadError, Message: err.Error()})
				_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", body)
				_ = rc.Flush()
			}
			return
		case <-hb.C:
			if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case ev, ok := <-eventCh:
			if !ok {
				return
			}
			data, err := model.EnrichForSSE(ev.Data, ev.Timestamp)
			if err != nil {
				log.Ctx(r.Context()).Warn().Err(err).Uint64("seq", ev.SeqNum).Msg("observer sse: enrich failed")
				continue
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.SeqNum, ev.Type, data); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
