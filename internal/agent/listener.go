package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"agentmail/internal/model"
	"agentmail/internal/store"
)

// convMessage is the agent's internal representation of a complete, assembled
// message. No Role field — role is derived at Claude-call-time from whether
// Sender == a.id (assistant) or not (user). This avoids baking in a single
// perspective for group conversations.
type convMessage struct {
	Sender  uuid.UUID
	Content string
}

// pendingMessage tracks a message that's currently streaming from another
// agent. Assembled events are folded into content; the entry is removed on
// message_end or message_abort, or swept after stalePendingTTL of silence.
type pendingMessage struct {
	sender     uuid.UUID
	content    strings.Builder
	lastAppend time.Time
}

// convState holds all per-conversation state the agent maintains: the in-
// progress message assembly, the bounded history window, lazy-seed flag, and
// response-goroutine coordination.
//
// Accessed under mu. responseSem coordinates sequential responses within a
// conversation (capacity 1). responseWg tracks in-flight response goroutines
// so the listener's deferred cleanup can wait for them before flushing
// cursors and signaling done (the leave-handler ordering contract).
type convState struct {
	mu            sync.Mutex
	pending       map[uuid.UUID]*pendingMessage
	history       []convMessage
	seeded        bool
	lastSeededSeq uint64
	responseSem   chan struct{}
	responseWg    sync.WaitGroup
}

func (a *Agent) getOrCreateConvState(convID uuid.UUID) *convState {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.convStates[convID]
	if !ok {
		s = &convState{
			pending:     make(map[uuid.UUID]*pendingMessage),
			responseSem: make(chan struct{}, 1),
		}
		a.convStates[convID] = s
	}
	return s
}

// startListening spawns exactly one listener goroutine per conversation.
// Idempotent — a second call for the same convID while the first listener is
// still running returns without doing anything. The goroutine runs listen()
// inside a retry loop; on any S2 error it backs off and retries. On context
// cancel (shutdown or NotifyLeave) the goroutine exits, waits for any in-
// flight response to finish, flushes the delivery cursor, and closes the
// listener's done channel so the leave handler can proceed.
func (a *Agent) startListening(convID uuid.UUID) {
	a.mu.Lock()
	if _, exists := a.listeners[convID]; exists {
		a.mu.Unlock()
		return
	}
	listenerCtx, cancel := context.WithCancel(a.rootCtx)
	handle := &listenerHandle{cancel: cancel, done: make(chan struct{})}
	a.listeners[convID] = handle
	a.mu.Unlock()

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			// Ordering is load-bearing for the leave-handler contract:
			//   1. Wait for any in-flight Claude response to finish (may
			//      write message_abort).
			//   2. Flush the delivery cursor on a fresh background context
			//      (listenerCtx is canceled).
			//   3. close(done) — unblocks the leave handler so it can
			//      write agent_left.
			//   4. Remove the listener entry so a re-invite creates a
			//      fresh goroutine.
			state := a.getOrCreateConvState(convID)
			state.responseWg.Wait()

			flushCtx, cancelFlush := context.WithTimeout(context.Background(), leaveDrainDeadline)
			if err := a.meta.FlushDeliveryCursorOne(flushCtx, a.id, convID); err != nil {
				log.Warn().
					Err(err).
					Str("conv_id", convID.String()).
					Msg("agent: cursor flush on listener exit failed")
			}
			cancelFlush()

			close(handle.done)

			a.mu.Lock()
			delete(a.listeners, convID)
			a.mu.Unlock()
		}()

		backoff := time.Second
		for {
			err := a.listen(listenerCtx, convID)
			if listenerCtx.Err() != nil {
				return
			}
			log.Warn().
				Err(err).
				Str("conv_id", convID.String()).
				Dur("backoff", backoff).
				Msg("agent: listener error, retrying")
			select {
			case <-time.After(backoff):
				backoff *= 2
				if backoff > listenerMaxBackoff {
					backoff = listenerMaxBackoff
				}
			case <-listenerCtx.Done():
				return
			}
		}
	}()
}

// listen runs one ReadSession lifecycle. Returns on ctx cancel or any S2
// error; the caller's retry loop decides whether to reconnect.
//
// Starting seq: on first listen (no cursor persisted) we tail from 0 —
// GetDeliveryCursor returns 0 in that case, and ReadSession(from=0) means
// "from the beginning". On resume, we advance past the last delivered event
// (cursor is inclusive of the last-processed seq).
func (a *Agent) listen(ctx context.Context, convID uuid.UUID) error {
	cursor, err := a.meta.GetDeliveryCursor(ctx, a.id, convID)
	if err != nil && !errors.Is(err, store.ErrAgentNotFound) && !errors.Is(err, store.ErrConversationNotFound) {
		// Transient cursor fetch error — surface for retry.
		return err
	}
	var fromSeq uint64
	if cursor > 0 {
		fromSeq = cursor + 1
	}

	rs, err := a.s2.OpenReadSession(ctx, convID, fromSeq)
	if err != nil {
		return err
	}
	defer rs.Close()

	for rs.Next() {
		ev := rs.Event()
		a.meta.UpdateDeliveryCursor(a.id, convID, ev.SeqNum)
		a.onEvent(ctx, convID, ev)
	}
	return rs.Err()
}

// onEvent dispatches a single SequencedEvent for the per-conversation listener.
// Responsibilities:
//  1. Lazy-seed history on the first response-worthy event (first message_end
//     authored by another agent).
//  2. Assemble partial messages in state.pending so the agent sees complete
//     messages, not raw tokens, when building Claude context.
//  3. On message_end/message_abort, fold the assembled message into
//     state.history (capped at maxHistory) — except for self-authored messages
//     (already in history via appendMessage during our own respond path).
//  4. On message_end authored by another agent, trigger a response via
//     triggerResponse.
//  5. Skip events already folded during history seeding (SeqNum <=
//     lastSeededSeq) so we don't double-count on resume.
//  6. Sweep stale pending entries (older than stalePendingTTL) to avoid
//     leaking memory if a writer dies mid-stream without message_abort.
//
// Membership events (agent_joined, agent_left) are informational for the
// Claude agent — logged only. A future improvement could inject them as
// system-visible context lines.
func (a *Agent) onEvent(ctx context.Context, convID uuid.UUID, ev model.SequencedEvent) {
	state := a.getOrCreateConvState(convID)

	// Lazy seed on the first response-worthy event. seedHistory acquires
	// state.mu internally; call it before we take the lock here.
	if !state.seeded && ev.Type == model.EventTypeMessageEnd {
		a.seedHistory(ctx, convID, state, ev.SeqNum)
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	// Drop events already folded by seedHistory.
	if ev.SeqNum <= state.lastSeededSeq {
		return
	}

	a.sweepStalePending(state)

	switch ev.Type {
	case model.EventTypeMessageStart:
		p, err := model.ParsePayload[model.MessageStartPayload](ev)
		if err != nil {
			log.Warn().Err(err).Uint64("seq", ev.SeqNum).Msg("agent: parse message_start")
			return
		}
		state.pending[p.MessageID] = &pendingMessage{
			sender:     p.SenderID,
			lastAppend: time.Now(),
		}

	case model.EventTypeMessageAppend:
		p, err := model.ParsePayload[model.MessageAppendPayload](ev)
		if err != nil {
			log.Warn().Err(err).Uint64("seq", ev.SeqNum).Msg("agent: parse message_append")
			return
		}
		pm, ok := state.pending[p.MessageID]
		if !ok {
			return
		}
		pm.content.WriteString(p.Content)
		pm.lastAppend = time.Now()

	case model.EventTypeMessageEnd:
		p, err := model.ParsePayload[model.MessageEndPayload](ev)
		if err != nil {
			log.Warn().Err(err).Uint64("seq", ev.SeqNum).Msg("agent: parse message_end")
			return
		}
		pm, ok := state.pending[p.MessageID]
		if !ok {
			return
		}
		sender := pm.sender
		content := pm.content.String()
		delete(state.pending, p.MessageID)

		// Self-authored messages are already folded into history by our
		// respond path (appendMessage after message_end). Skip to avoid
		// double-counting — our own stream-tail will re-surface them here.
		if sender == a.id {
			return
		}

		appendMessage(state, convMessage{Sender: sender, Content: content})

		// Trigger a response on completed messages from other agents.
		// triggerResponse is non-blocking — it launches a goroutine that
		// acquires semaphores, streams Claude, and writes back to S2.
		a.triggerResponse(ctx, convID, state)

	case model.EventTypeMessageAbort:
		p, err := model.ParsePayload[model.MessageAbortPayload](ev)
		if err != nil {
			log.Warn().Err(err).Uint64("seq", ev.SeqNum).Msg("agent: parse message_abort")
			return
		}
		delete(state.pending, p.MessageID)

	case model.EventTypeAgentJoined, model.EventTypeAgentLeft:
		// Informational. No state change — membership is authoritative in
		// Postgres, not on-stream.

	default:
		log.Debug().
			Str("type", string(ev.Type)).
			Uint64("seq", ev.SeqNum).
			Msg("agent: unknown event type")
	}
}

// appendMessage appends a complete message to state.history, trimming to
// maxHistory when the window overflows. Caller must hold state.mu.
func appendMessage(state *convState, m convMessage) {
	state.history = append(state.history, m)
	if n := len(state.history); n > maxHistory {
		state.history = append([]convMessage(nil), state.history[n-maxHistory:]...)
	}
}

// sweepStalePending drops partial messages whose writer has gone silent past
// stalePendingTTL. Prevents the pending map from growing unboundedly if a
// remote writer dies without message_abort. Caller must hold state.mu.
func (a *Agent) sweepStalePending(state *convState) {
	cutoff := time.Now().Add(-stalePendingTTL)
	for id, pm := range state.pending {
		if pm.lastAppend.Before(cutoff) {
			delete(state.pending, id)
		}
	}
}

