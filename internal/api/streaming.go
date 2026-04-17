package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
	"unicode/utf8"

	"agentmail/internal/model"
	"agentmail/internal/store"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Streaming-send limits. The scanner buffer is sized for the largest
// well-formed NDJSON line we allow: the chunk payload cap (1 MiB) plus
// slack for JSON framing and the `content` key (1 KiB is generous).
const (
	streamScannerMax    = (1 << 20) + 1024
	streamIdleTimeout   = 5 * time.Minute
	streamMaxDuration   = 1 * time.Hour
	streamHandshakeRead = 5 * time.Second
)

// StreamMessage handles POST /conversations/{cid}/messages/stream — the
// NDJSON streaming write path. See http-api-layer-plan.md §5.3.
//
// Protocol:
//  1. First line: {"message_id": "<uuidv7>"}. Parsed BEFORE the dedup gate
//     so replays / conflicts short-circuit without reading the body.
//  2. Dedup: complete → 200 replay; aborted → 409 already_aborted.
//  3. Claim: false → 409 in_progress_conflict.
//  4. Open AppendSession; submit message_start; per body line:
//     parse → utf8 check → submit message_append.
//  5. Clean EOF: submit message_end → session.Close → CheckTail for seqEnd
//     → dedup insert-complete → head advance → delete in-progress → 200.
//  6. On any failure: abort path (detached ctx unary append + dedup aborted +
//     delete in-progress). HTTP response is either an error envelope (if the
//     response hasn't committed) or simply closed.
func (h *Handler) StreamMessage(w http.ResponseWriter, r *http.Request) {
	agentID, convID, ok := h.requireMembership(w, r)
	if !ok {
		return
	}
	if !validateNDJSONContentType(w, r) {
		return
	}

	// Derive an absolute-cap context — closed after 1 h no matter what.
	ctx, cancel := context.WithTimeout(r.Context(), streamMaxDuration)
	defer cancel()

	rc := http.NewResponseController(w)
	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, 64<<10), streamScannerMax)

	// Handshake: read exactly one line with a short deadline. The handshake
	// is low-volume (a 60-byte JSON object); 5 s is plenty and bounds the
	// case where the client dribbles the header.
	_ = rc.SetReadDeadline(time.Now().Add(streamHandshakeRead))
	if !scanner.Scan() {
		writeScanErr(w, r, scanner.Err(), CodeMissingMessageID, "first line (message_id) missing")
		return
	}
	var hdr StreamHeader
	if err := json.Unmarshal(scanner.Bytes(), &hdr); err != nil {
		WriteError(w, r, http.StatusBadRequest, CodeInvalidMessageID,
			"first line must be {\"message_id\":\"<uuidv7>\"}")
		return
	}
	if hdr.MessageID == uuid.Nil {
		WriteError(w, r, http.StatusBadRequest, CodeMissingMessageID, "message_id is required")
		return
	}
	if !isUUIDv7(hdr.MessageID) {
		WriteError(w, r, http.StatusBadRequest, CodeInvalidMessageID, "message_id must be a UUIDv7")
		return
	}

	// Dedup gate.
	if row, found, err := h.meta.GetMessageDedup(ctx, convID, hdr.MessageID); err != nil {
		log.Ctx(ctx).Error().Err(err).Msg("dedup lookup failed")
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
			// Replay: respond immediately; the server-side Go HTTP layer
			// will discard any unread body bytes the client retransmits.
			WriteJSON(w, r, http.StatusOK, StreamMessageResponse{
				MessageID:        hdr.MessageID,
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

	claimed, err := h.meta.ClaimInProgressMessage(ctx, hdr.MessageID, convID, agentID,
		store.StreamNameForConversation(convID))
	if err != nil {
		log.Ctx(ctx).Error().Err(err).Msg("claim in-progress failed")
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	if !claimed {
		WriteError(w, r, http.StatusConflict, CodeInProgressConflict,
			"another write is already in-flight for this message_id")
		return
	}

	// From this point, any failure MUST route through the abort path.
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	h.conns.RegisterWrite(agentID, convID, hdr.MessageID, streamCancel)
	defer h.conns.DeregisterWrite(agentID, convID, hdr.MessageID)

	// Capture seqStart bound: tail+1 at this moment. Concurrent distinct
	// message_ids on the same conversation may interleave, so this is an
	// estimate; Agent 1's S2 implementation can tighten by returning exact
	// seq from AppendSession — until then this is the best the frozen
	// interface exposes. See plan open question.
	tailBefore, _ := h.s2.CheckTail(streamCtx, convID)

	session, err := h.s2.OpenAppendSession(streamCtx, convID)
	if err != nil {
		h.finalizeStreamAbort(ctx, convID, hdr.MessageID, err)
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "could not open stream session")
		return
	}

	abortAndReply := func(code, reason, message string, status int) {
		_ = session.Close()
		h.finalizeStreamAbortWithReason(ctx, convID, hdr.MessageID, reason)
		WriteError(w, r, status, code, message)
	}

	if err := session.Submit(streamCtx, model.NewMessageStartEvent(hdr.MessageID, agentID)); err != nil {
		if errors.Is(err, store.ErrSlowWriter) {
			abortAndReply(CodeSlowWriter, model.AbortReasonSlowWriter, "append timed out", http.StatusServiceUnavailable)
			return
		}
		abortAndReply(CodeInternalError, model.AbortReasonServerCrash, "internal server error", http.StatusInternalServerError)
		return
	}

	// Body loop. Reset the idle deadline before each Scan.
	for {
		_ = rc.SetReadDeadline(time.Now().Add(streamIdleTimeout))
		if !scanner.Scan() {
			break
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var chunk StreamChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			abortAndReply(CodeInvalidJSON, model.AbortReasonDisconnect,
				fmt.Sprintf("malformed NDJSON chunk: %s", err.Error()), http.StatusBadRequest)
			return
		}
		if !utf8.ValidString(chunk.Content) {
			abortAndReply(CodeInvalidUTF8, model.AbortReasonInvalidUTF8,
				"chunk content is not valid UTF-8", http.StatusBadRequest)
			return
		}
		if chunk.Content == "" {
			continue
		}
		if err := session.Submit(streamCtx, model.NewMessageAppendEvent(hdr.MessageID, chunk.Content)); err != nil {
			if errors.Is(err, store.ErrSlowWriter) {
				abortAndReply(CodeSlowWriter, model.AbortReasonSlowWriter, "append timed out", http.StatusServiceUnavailable)
				return
			}
			abortAndReply(CodeInternalError, model.AbortReasonServerCrash, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	// Scanner exited; classify the termination reason.
	if err := scanner.Err(); err != nil {
		switch {
		case errors.Is(err, bufio.ErrTooLong):
			abortAndReply(CodeLineTooLarge, model.AbortReasonLineTooLarge,
				fmt.Sprintf("NDJSON line exceeds %d bytes", streamScannerMax),
				http.StatusRequestEntityTooLarge)
			return
		case errors.Is(err, os.ErrDeadlineExceeded):
			abortAndReply(CodeIdleTimeout, model.AbortReasonIdleTimeout,
				"idle timeout waiting for next NDJSON line", http.StatusRequestTimeout)
			return
		case errors.Is(err, context.DeadlineExceeded):
			abortAndReply(CodeMaxDuration, model.AbortReasonMaxDuration,
				"maximum streaming duration exceeded", http.StatusRequestTimeout)
			return
		case errors.Is(err, context.Canceled), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
			_ = session.Close()
			h.finalizeStreamAbortWithReason(ctx, convID, hdr.MessageID, model.AbortReasonDisconnect)
			// Client disconnect — no response body to write.
			return
		default:
			abortAndReply(CodeInternalError, model.AbortReasonServerCrash,
				fmt.Sprintf("body read failed: %s", err.Error()), http.StatusInternalServerError)
			return
		}
	}

	// Clean EOF path.
	if err := session.Submit(streamCtx, model.NewMessageEndEvent(hdr.MessageID)); err != nil {
		_ = session.Close()
		if errors.Is(err, store.ErrSlowWriter) {
			h.finalizeStreamAbortWithReason(ctx, convID, hdr.MessageID, model.AbortReasonSlowWriter)
			WriteError(w, r, http.StatusServiceUnavailable, CodeSlowWriter, "append timed out")
			return
		}
		h.finalizeStreamAbortWithReason(ctx, convID, hdr.MessageID, model.AbortReasonServerCrash)
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return
	}
	if err := session.Close(); err != nil {
		h.finalizeStreamAbortWithReason(ctx, convID, hdr.MessageID, model.AbortReasonServerCrash)
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "session close failed")
		return
	}

	tailAfter, err := h.s2.CheckTail(ctx, convID)
	if err != nil {
		log.Ctx(ctx).Warn().Err(err).Msg("check-tail post-stream failed (seqEnd best-effort)")
	}
	seqStart := tailBefore + 1
	seqEnd := tailAfter
	if seqEnd < seqStart {
		// Pathological — treat both as unknown rather than lie.
		seqStart, seqEnd = 0, 0
	}

	if err := h.meta.InsertMessageDedupComplete(ctx, convID, hdr.MessageID, seqStart, seqEnd); err != nil {
		log.Ctx(ctx).Warn().Err(err).Msg("dedup insert-complete (stream) failed")
	}
	if seqEnd > 0 {
		if err := h.meta.UpdateConversationHeadSeq(ctx, convID, seqEnd); err != nil {
			log.Ctx(ctx).Warn().Err(err).Msg("head_seq advance (stream) failed")
		}
	}
	if err := h.meta.DeleteInProgressMessage(ctx, hdr.MessageID); err != nil {
		log.Ctx(ctx).Warn().Err(err).Msg("in-progress cleanup (stream) failed")
	}

	WriteJSON(w, r, http.StatusOK, StreamMessageResponse{
		MessageID:        hdr.MessageID,
		SeqStart:         seqStart,
		SeqEnd:           seqEnd,
		AlreadyProcessed: false,
	})
}

// writeScanErr maps the Scan()==false failure on the handshake read into a
// reasonable HTTP envelope. The handshake hasn't claimed anything yet, so
// no abort path is needed.
func writeScanErr(w http.ResponseWriter, r *http.Request, err error, fallbackCode, fallbackMsg string) {
	switch {
	case err == nil:
		WriteError(w, r, http.StatusBadRequest, fallbackCode, fallbackMsg)
	case errors.Is(err, os.ErrDeadlineExceeded):
		WriteError(w, r, http.StatusRequestTimeout, CodeIdleTimeout,
			"idle timeout waiting for first NDJSON line")
	case errors.Is(err, bufio.ErrTooLong):
		WriteError(w, r, http.StatusRequestEntityTooLarge, CodeLineTooLarge,
			fmt.Sprintf("handshake line exceeds %d bytes", streamScannerMax))
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		WriteError(w, r, http.StatusBadRequest, CodeEmptyBody, "request body is empty")
	default:
		WriteError(w, r, http.StatusBadRequest, CodeInvalidJSON,
			fmt.Sprintf("failed to read first line: %s", err.Error()))
	}
}

// finalizeStreamAbort runs the shared cleanup: unary message_abort append
// (best-effort), InsertMessageDedupAborted, DeleteInProgressMessage. Runs
// on a detached, time-bounded context so a slow S2 call doesn't block
// response writing or swallow a retry's dedup hit.
func (h *Handler) finalizeStreamAbort(reqCtx context.Context, convID, msgID uuid.UUID, origErr error) {
	reason := model.AbortReasonServerCrash
	if errors.Is(origErr, store.ErrSlowWriter) {
		reason = model.AbortReasonSlowWriter
	}
	h.finalizeStreamAbortWithReason(reqCtx, convID, msgID, reason)
}

func (h *Handler) finalizeStreamAbortWithReason(reqCtx context.Context, convID, msgID uuid.UUID, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.s2.AppendEvents(ctx, convID, []model.Event{model.NewMessageAbortEvent(msgID, reason)}); err != nil {
		log.Ctx(reqCtx).Warn().Err(err).Msg("stream abort append failed")
	}
	if err := h.meta.InsertMessageDedupAborted(ctx, convID, msgID, nil); err != nil {
		log.Ctx(reqCtx).Warn().Err(err).Msg("stream dedup insert-aborted failed")
	}
	if err := h.meta.DeleteInProgressMessage(ctx, msgID); err != nil {
		log.Ctx(reqCtx).Warn().Err(err).Msg("stream in-progress cleanup failed")
	}
}
