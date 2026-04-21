package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"agentmail/internal/model"

	"github.com/rs/zerolog/log"
)

// SSE parameters. The channel cap (64) is the handoff buffer between the
// tailer goroutine and the writer goroutine. 500 ms is the slow-consumer
// guard: if the socket writer can't accept a record within that window, we
// assume TCP backpressure is starving the connection and tear it down rather
// than accumulate memory.
const (
	sseChanCap          = 64
	sseSlowWriterGrace  = 500 * time.Millisecond
	sseHeartbeatPeriod  = 30 * time.Second
	sseAbsoluteDuration = 24 * time.Hour
)

// SSEStream handles GET /conversations/{cid}/stream.
//
// Layout:
//   - Pre-stream: membership → cursor → register-sse (replace-on-register).
//   - Open the read session and commit 200 + SSE headers with a first Flush.
//   - Tailer goroutine: loops session.Next(), pushes SequencedEvents into a
//     bounded channel with a 500 ms send deadline.
//   - Writer loop (this goroutine): selects on context, heartbeat ticker,
//     and the channel. On each event: emit id/event/data, Flush, advance
//     delivery cursor.
//   - On exit (any path): flush delivery cursor, close session, deregister.
func (h *Handler) SSEStream(w http.ResponseWriter, r *http.Request) {
	agentID, convID, ok := h.requireMembership(w, r)
	if !ok {
		return
	}

	fromSeq, fromPresent, ok := parseUint64Query(w, r, "from", CodeInvalidFrom)
	if !ok {
		return
	}

	// Last-Event-ID overrides the stored cursor (per SSE spec) iff ?from is
	// absent. If both are present, ?from wins — explicit query params beat
	// headers.
	//
	// IMPORTANT: Last-Event-ID is the SSE-spec-defined client confirmation
	// that the client received everything up to N. It's our only authoritative
	// signal of actual delivery — rc.Flush() only tells us bytes hit the
	// kernel send buffer, not that the client read them (buffering proxies
	// can swallow chunks). So we advance the durable delivery_seq cursor HERE,
	// on confirmed resume, rather than in the hot per-event loop below.
	if !fromPresent {
		if lei := r.Header.Get("Last-Event-ID"); lei != "" {
			if v, err := strconv.ParseUint(lei, 10, 64); err == nil {
				fromSeq = v + 1
				fromPresent = true
				// Advance the stored cursor. Client confirmed receipt up
				// through seq v, so the next thing to deliver is v+1.
				// The stored cursor's convention is "next seq to deliver"
				// (it's used directly as fromSeq on a subsequent connect
				// without headers), so store v+1, not v. Storing v would
				// cause the next header-less reconnect to replay seq v.
				h.meta.UpdateDeliveryCursor(agentID, convID, v+1)
			}
		}
	}
	if !fromPresent {
		stored, err := h.meta.GetDeliveryCursor(r.Context(), agentID, convID)
		if err != nil {
			log.Ctx(r.Context()).Error().Err(err).Msg("sse: read delivery cursor failed")
			WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
			return
		}
		fromSeq = stored
	}

	// Per-connection context bounded by the 24 h absolute cap.
	ctx, cancel := context.WithTimeout(r.Context(), sseAbsoluteDuration)
	defer cancel()

	handle, prior := h.conns.RegisterSSE(agentID, convID, cancel)
	defer h.conns.DeregisterSSE(handle)
	if prior != nil {
		// Give the prior handler up to 2 s to finish its teardown. We don't
		// strictly have to wait — both handlers share the flush path — but
		// waiting avoids a micro-window where the prior's final cursor flush
		// could race with ours.
		WaitFor(prior, 2*time.Second)
	}

	session, err := h.s2.OpenReadSession(ctx, convID, fromSeq)
	if err != nil {
		log.Ctx(r.Context()).Error().Err(err).Msg("sse: open read session failed")
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
		log.Ctx(r.Context()).Debug().Err(err).Msg("sse: initial flush failed")
		return
	}

	// Drive the tailer off a separate goroutine. The tailer's only job is
	// to move events from session.Next() into eventCh — if the writer is
	// slow, the send times out and the tailer exits, which cancels ctx and
	// unblocks Next.
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

	defer func() {
		// Flush the delivery cursor synchronously on exit. We use
		// r.Context() rather than ctx here: ctx may have been cancelled,
		// but r.Context() is at least alive until the handler returns.
		if err := h.meta.FlushDeliveryCursorOne(r.Context(), agentID, convID); err != nil {
			log.Ctx(r.Context()).Warn().Err(err).Msg("sse: cursor flush on exit failed")
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case <-tailerDone:
			// Tailer exited of its own accord — either ctx was cancelled
			// (handled above) or session.Err() is set. Try to surface it
			// via an SSE `error` event; if the write fails the connection
			// is already gone and we just exit.
			if err := session.Err(); err != nil {
				emitSSEError(w, rc, CodeS2ReadError, err.Error())
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
				// Malformed payload — skip but keep the stream alive.
				log.Ctx(r.Context()).Warn().Err(err).Uint64("seq", ev.SeqNum).Msg("sse: enrich failed")
				continue
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.SeqNum, ev.Type, data); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
			// Deliberately NOT advancing delivery_seq here. rc.Flush()
			// succeeded means bytes reached the kernel send buffer, not that
			// the client read them — through a buffering proxy (Cloudflare
			// quick tunnel, nginx with default proxy_buffering, etc.) these
			// are not the same. The cursor is advanced on Last-Event-ID
			// reconnect (above) or POST /ack (see ack.go). This is the
			// at-least-once semantics promised by CLIENT.md §3.
		}
	}
}

// emitSSEError writes an SSE `error` frame and flushes. Failures are
// swallowed — if the client's already gone there's nothing we can do.
func emitSSEError(w http.ResponseWriter, rc *http.ResponseController, code, msg string) {
	body, err := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: msg})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", body)
	_ = rc.Flush()
}
