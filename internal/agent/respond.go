package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"agentmail/internal/model"
	"agentmail/internal/store"
)

// Retry policy for Claude API calls. 429 responses honor the server-provided
// Retry-After header when present, otherwise fall back to exponential backoff.
const (
	claudeMaxAttempts      = 5
	claudeBaseBackoff      = 500 * time.Millisecond
	claudeMaxBackoff       = 30 * time.Second
	responseAbortDeadline  = 5 * time.Second
	errorMessageDefaultMsg = "I ran into an error while generating a response. Please try again."
)

// triggerResponse tries to launch a new response goroutine for the given
// conversation. Non-blocking, called from onEvent under state.mu.
//
// Concurrency contract:
//   - The per-conversation semaphore (state.responseSem, cap 1) guarantees at
//     most one response goroutine active per conversation; we try the
//     non-blocking send here, and if the slot is full we simply skip — the
//     currently-running response will re-check the history after message_end
//     and naturally pick up the newer message.
//   - The goroutine is tracked via state.responseWg so the listener's
//     deferred cleanup can wait for the Claude write to finish before
//     flushing the delivery cursor.
//
// Caller must hold state.mu.
func (a *Agent) triggerResponse(ctx context.Context, convID uuid.UUID, state *convState) {
	select {
	case state.responseSem <- struct{}{}:
	default:
		// A response is already running for this conversation. When it
		// finishes and re-reads state.history, the new message will be
		// part of the prompt for the NEXT trigger — or it'll just be
		// part of this in-flight response's prompt if we appended it
		// before the goroutine built its messages slice.
		log.Debug().
			Str("conv_id", convID.String()).
			Msg("agent: response already in-flight, skipping trigger")
		return
	}

	state.responseWg.Add(1)
	go func() {
		defer state.responseWg.Done()
		defer func() { <-state.responseSem }()
		a.respond(ctx, convID, state)
	}()
}

// respond is the per-response lifecycle. Acquires the global Claude
// concurrency slot, claims the in-progress row, opens an S2 AppendSession,
// streams Claude tokens as message_append events, and finalizes with
// message_end + dedup.InsertComplete. On any mid-stream failure it calls
// abortAndRecord with an appropriate reason so the conversation's stream
// is left in a consistent state.
func (a *Agent) respond(ctx context.Context, convID uuid.UUID, state *convState) {
	// Acquire global Claude slot.
	select {
	case a.claudeSem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-a.claudeSem }()

	if ctx.Err() != nil {
		return
	}

	// Snapshot history under the state lock so Claude sees a coherent view.
	state.mu.Lock()
	history := append([]convMessage(nil), state.history...)
	state.mu.Unlock()

	if len(history) == 0 {
		return
	}

	msgs := buildClaudeMessages(history, a.id)
	if len(msgs) == 0 {
		return
	}

	msgID := uuid.Must(uuid.NewV7())
	streamName := fmt.Sprintf("conversations/%s", convID)

	claimed, err := a.meta.ClaimInProgressMessage(ctx, msgID, convID, a.id, streamName)
	if err != nil {
		log.Warn().
			Err(err).
			Str("conv_id", convID.String()).
			Str("msg_id", msgID.String()).
			Msg("agent: claim in-progress failed")
		return
	}
	if !claimed {
		// UUIDv7 collision is a programmer bug; log and bail.
		log.Error().
			Str("conv_id", convID.String()).
			Str("msg_id", msgID.String()).
			Msg("agent: ClaimInProgressMessage returned !claimed for a fresh UUIDv7")
		return
	}

	// Start seq bracketing: snapshot tail immediately before message_start.
	// On CheckTail error we continue — dedup startSeq is observability, not
	// correctness-critical for a writer that never retries msgIDs.
	preTail, _ := a.s2.CheckTail(ctx, convID)

	session, err := a.s2.OpenAppendSession(ctx, convID)
	if err != nil {
		log.Warn().
			Err(err).
			Str("conv_id", convID.String()).
			Str("msg_id", msgID.String()).
			Msg("agent: open append session failed")
		a.abortAndRecord(convID, msgID, nil, model.AbortReasonSlowWriter)
		return
	}
	defer session.Close()

	if err := session.Submit(ctx, model.NewMessageStartEvent(msgID, a.id)); err != nil {
		log.Warn().Err(err).Msg("agent: submit message_start failed")
		a.abortAndRecord(convID, msgID, nil, abortReasonFromErr(err, ctx))
		return
	}

	// Stream Claude tokens → message_append. callClaudeStreaming handles
	// retries internally for 429 / transient errors. It returns the assembled
	// text we sent (for appending into state.history) and the final error.
	assembled, streamErr := a.streamToSession(ctx, session, msgID, msgs)
	if streamErr != nil {
		startSeq := preTail + 1
		a.abortAndRecord(convID, msgID, &startSeq, abortReasonFromErr(streamErr, ctx))
		return
	}

	if err := session.Submit(ctx, model.NewMessageEndEvent(msgID)); err != nil {
		log.Warn().Err(err).Msg("agent: submit message_end failed")
		startSeq := preTail + 1
		a.abortAndRecord(convID, msgID, &startSeq, abortReasonFromErr(err, ctx))
		return
	}

	// Close the session to flush acks before reading the post-tail.
	if err := session.Close(); err != nil {
		log.Warn().Err(err).Msg("agent: append session close returned error (ignored)")
	}

	// End seq bracketing: snapshot tail after the session has drained.
	postTail, _ := a.s2.CheckTail(context.Background(), convID)
	startSeq := preTail + 1
	endSeq := postTail
	if endSeq < startSeq {
		// Defensive: if CheckTail failed or lagged, collapse the range.
		endSeq = startSeq
	}

	// Record dedup complete + delete in-progress on a fresh background
	// context — ctx may already be canceled by shutdown/leave but we need
	// these two writes to land so Postgres is consistent. Bounded deadline.
	finalCtx, cancel := context.WithTimeout(context.Background(), responseAbortDeadline)
	defer cancel()

	if err := a.meta.InsertMessageDedupComplete(finalCtx, convID, msgID, startSeq, endSeq); err != nil {
		log.Warn().Err(err).Str("msg_id", msgID.String()).Msg("agent: insert dedup complete failed")
	}
	if err := a.meta.DeleteInProgressMessage(finalCtx, msgID); err != nil {
		log.Warn().Err(err).Str("msg_id", msgID.String()).Msg("agent: delete in-progress failed")
	}

	// Fold the assembled response into state.history so subsequent
	// triggers see it in the prompt. maxHistory trimming happens in
	// appendMessage.
	if assembled != "" {
		state.mu.Lock()
		appendMessage(state, convMessage{Sender: a.id, Content: assembled})
		state.mu.Unlock()
	}
}

// streamToSession runs one Claude streaming call with retry, forwarding each
// text delta as a message_append event on the open AppendSession. Returns the
// assembled text (what the agent actually said) and any terminal error.
//
// Retry policy:
//   - 429: honor Retry-After / Retry-After-Ms when present, otherwise
//     exponential backoff.
//   - 5xx: exponential backoff.
//   - Client-side / context errors: no retry.
//
// Once the first text delta has been forwarded, we stop retrying — the S2
// stream has observable partial output and the right move on failure is to
// abort on-stream rather than start a second (out-of-order) response. The
// retry loop only kicks in for errors that happen before any text delta is
// emitted.
func (a *Agent) streamToSession(
	ctx context.Context,
	session store.AppendSession,
	msgID uuid.UUID,
	msgs []anthropic.MessageParam,
) (string, error) {
	var (
		assembled strings.Builder
		anyText   bool
	)

	for attempt := 0; attempt < claudeMaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return assembled.String(), ctx.Err()
		}

		stream := a.claude.Stream(ctx, msgs)
		err := forwardStream(ctx, stream, session, msgID, &assembled, &anyText)
		closeErr := stream.Close()
		if err == nil && closeErr != nil {
			err = closeErr
		}

		if err == nil {
			return assembled.String(), nil
		}
		if anyText {
			// Partial output already on stream; escalate.
			return assembled.String(), err
		}
		if !retryableClaudeErr(err) {
			return assembled.String(), err
		}

		backoff := claudeBackoff(err, attempt)
		log.Warn().
			Err(err).
			Int("attempt", attempt+1).
			Dur("backoff", backoff).
			Msg("agent: claude stream error, retrying")

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return assembled.String(), ctx.Err()
		}
	}
	return assembled.String(), fmt.Errorf("agent: claude stream failed after %d attempts", claudeMaxAttempts)
}

// forwardStream pumps a single Claude streaming call onto the S2 session.
// Sets *anyText = true as soon as the first non-empty text delta is
// forwarded.
func forwardStream(
	ctx context.Context,
	stream claudeStream,
	session store.AppendSession,
	msgID uuid.UUID,
	assembled *strings.Builder,
	anyText *bool,
) error {
	for stream.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		ev := stream.Current()
		// Extract text delta via the public AsAny() switch. The stream
		// carries many event types (message_start, content_block_start,
		// content_block_delta, content_block_stop, message_delta,
		// message_stop, etc.); we only care about text deltas.
		variant, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent)
		if !ok {
			continue
		}
		text := variant.Delta.Text
		if text == "" {
			continue
		}
		if err := session.Submit(ctx, model.NewMessageAppendEvent(msgID, text)); err != nil {
			return err
		}
		assembled.WriteString(text)
		*anyText = true
	}
	return stream.Err()
}

// retryableClaudeErr reports whether we should retry on this error. 429 and
// 5xx are retryable; everything else (including context errors) is not.
func retryableClaudeErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var aerr *anthropic.Error
	if errors.As(err, &aerr) {
		if aerr.StatusCode == http.StatusTooManyRequests {
			return true
		}
		if aerr.StatusCode >= 500 && aerr.StatusCode < 600 {
			return true
		}
		return false
	}
	// Unknown errors (network glitches, EOF mid-stream) — treat as retryable
	// up to claudeMaxAttempts.
	return true
}

// claudeBackoff returns the delay to wait before the next attempt. Honors
// Retry-After / Retry-After-Ms headers on 429 responses; otherwise returns
// exponential backoff clamped to claudeMaxBackoff.
func claudeBackoff(err error, attempt int) time.Duration {
	var aerr *anthropic.Error
	if errors.As(err, &aerr) && aerr.Response != nil {
		if h := aerr.Response.Header.Get("Retry-After-Ms"); h != "" {
			if ms, perr := strconv.Atoi(h); perr == nil && ms > 0 {
				return clampBackoff(time.Duration(ms) * time.Millisecond)
			}
		}
		if h := aerr.Response.Header.Get("Retry-After"); h != "" {
			if secs, perr := strconv.ParseFloat(h, 64); perr == nil && secs > 0 {
				return clampBackoff(time.Duration(secs * float64(time.Second)))
			}
		}
	}
	d := time.Duration(float64(claudeBaseBackoff) * math.Pow(2, float64(attempt)))
	return clampBackoff(d)
}

func clampBackoff(d time.Duration) time.Duration {
	if d < claudeBaseBackoff {
		return claudeBaseBackoff
	}
	if d > claudeMaxBackoff {
		return claudeMaxBackoff
	}
	return d
}

// abortAndRecord writes a message_abort event to S2 (unary, on a fresh
// background context so leave/shutdown doesn't block the abort) and records
// the terminal dedup row + deletes the in-progress claim.
func (a *Agent) abortAndRecord(convID, msgID uuid.UUID, startSeq *uint64, reason string) {
	bgCtx, cancel := context.WithTimeout(context.Background(), responseAbortDeadline)
	defer cancel()

	if _, err := a.s2.AppendEvents(bgCtx, convID, []model.Event{
		model.NewMessageAbortEvent(msgID, reason),
	}); err != nil {
		log.Warn().
			Err(err).
			Str("conv_id", convID.String()).
			Str("msg_id", msgID.String()).
			Str("reason", reason).
			Msg("agent: append message_abort failed")
		// Continue — we still want to close out the in-progress row so
		// the recovery sweep doesn't re-abort on next startup.
	}

	if err := a.meta.InsertMessageDedupAborted(bgCtx, convID, msgID, startSeq); err != nil {
		log.Warn().Err(err).Str("msg_id", msgID.String()).Msg("agent: insert dedup aborted failed")
	}
	if err := a.meta.DeleteInProgressMessage(bgCtx, msgID); err != nil {
		log.Warn().Err(err).Str("msg_id", msgID.String()).Msg("agent: delete in-progress failed (abort path)")
	}
}

// abortReasonFromErr maps an error to an abort reason. Uses context state to
// distinguish leave (agent_left) from shutdown / claude-error cases.
func abortReasonFromErr(err error, ctx context.Context) string {
	if errors.Is(err, store.ErrSlowWriter) {
		return model.AbortReasonSlowWriter
	}
	if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
		return model.AbortReasonAgentLeft
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return model.AbortReasonIdleTimeout
	}
	var aerr *anthropic.Error
	if errors.As(err, &aerr) {
		return model.AbortReasonClaudeError
	}
	return model.AbortReasonClaudeError
}

// buildClaudeMessages converts the agent's conversation history into the
// Anthropic SDK's MessageParam shape. Rules:
//   - A message authored by a.id is an assistant turn; everything else is a
//     user turn.
//   - Anthropic's API requires alternating user/assistant turns starting with
//     user. We enforce this by merging consecutive same-role messages into
//     one, and by prepending a placeholder-attributed user turn if the first
//     message is from the assistant (shouldn't happen normally but guards
//     the edge case after a seed).
//   - Non-self turns are prefixed with `[<sender>]: ` so the assistant sees
//     who said what in group conversations.
func buildClaudeMessages(history []convMessage, selfID uuid.UUID) []anthropic.MessageParam {
	if len(history) == 0 {
		return nil
	}

	type turn struct {
		role    anthropic.MessageParamRole
		content strings.Builder
	}
	var turns []*turn

	for _, m := range history {
		var role anthropic.MessageParamRole
		var line string
		if m.Sender == selfID {
			role = anthropic.MessageParamRoleAssistant
			line = m.Content
		} else {
			role = anthropic.MessageParamRoleUser
			line = fmt.Sprintf("[%s]: %s", m.Sender.String(), m.Content)
		}

		if len(turns) > 0 && turns[len(turns)-1].role == role {
			turns[len(turns)-1].content.WriteString("\n")
			turns[len(turns)-1].content.WriteString(line)
			continue
		}
		t := &turn{role: role}
		t.content.WriteString(line)
		turns = append(turns, t)
	}

	// Anthropic requires the first message to be user. If history starts
	// with an assistant message (rare — would require the seed to pull our
	// own completions but no other-agent messages), drop it.
	for len(turns) > 0 && turns[0].role != anthropic.MessageParamRoleUser {
		turns = turns[1:]
	}
	if len(turns) == 0 {
		return nil
	}

	msgs := make([]anthropic.MessageParam, 0, len(turns))
	for _, t := range turns {
		block := anthropic.NewTextBlock(t.content.String())
		if t.role == anthropic.MessageParamRoleAssistant {
			msgs = append(msgs, anthropic.NewAssistantMessage(block))
		} else {
			msgs = append(msgs, anthropic.NewUserMessage(block))
		}
	}
	return msgs
}

// sendErrorMessage is a diagnostic unary message — used when Claude fails so
// completely that no in-progress row should be left behind. Bypasses the
// dedup tracking (fresh msgID each time) and is a single 3-record unary
// batch. Currently unused (callers prefer abortAndRecord so the original
// msgID's terminal state is recorded); kept here for the plan's API
// completeness and future reuse.
func (a *Agent) sendErrorMessage(ctx context.Context, convID uuid.UUID, detail string) {
	if detail == "" {
		detail = errorMessageDefaultMsg
	}
	msgID := uuid.Must(uuid.NewV7())
	if _, err := a.s2.AppendEvents(ctx, convID, model.NewCompleteMessageEvents(msgID, a.id, detail)); err != nil {
		log.Warn().
			Err(err).
			Str("conv_id", convID.String()).
			Msg("agent: sendErrorMessage append failed")
	}
}
