package agent

import (
	"context"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"agentmail/internal/model"
)

// seedHistory populates state.history from the last ~500 events in the
// conversation's S2 stream. Called lazily on the first message_end event
// a listener observes from another agent — this gives the agent context
// from any messages that landed before we started tailing (e.g., after a
// resume, or on a conversation where other agents had already been chatting).
//
// Design notes:
//   - Bounded read: seedWindow events is enough to capture the last
//     maxHistory complete messages in all realistic cases. We then trim
//     down to maxHistory before merging into state.history.
//   - model.AssembleMessages does the message-by-message assembly from
//     the event stream; we then drop any self-authored messages (agent
//     echo) and build convMessage rows.
//   - lastSeededSeq is set to endSeq of the window we read, so onEvent
//     can skip events already folded in (prevents double-counting the
//     triggering message_end that we pass in as triggerSeq).
//   - Acquires state.mu internally; caller must not hold it.
//   - Errors are logged and swallowed — seeding is best-effort; the agent
//     can still respond using only the current message if the read fails.
const seedWindow = 500

func (a *Agent) seedHistory(ctx context.Context, convID uuid.UUID, state *convState, triggerSeq uint64) {
	// Compute a from-seq so we pull the seedWindow events preceding the
	// triggering event. CheckTail gives us the most up-to-date end; using
	// triggerSeq as a proxy is equivalent for a live listener (triggerSeq
	// <= tail by construction).
	var fromSeq uint64
	if triggerSeq > seedWindow {
		fromSeq = triggerSeq - seedWindow
	}

	events, err := a.s2.ReadRange(ctx, convID, fromSeq, seedWindow)
	if err != nil {
		log.Warn().
			Err(err).
			Str("conv_id", convID.String()).
			Msg("agent: history seed read failed; proceeding without seed")
		// Mark seeded anyway so we don't retry on every subsequent
		// message_end — a persistent S2 read error will just leave us
		// with an empty window for the life of this listener.
		state.mu.Lock()
		state.seeded = true
		state.mu.Unlock()
		return
	}

	msgs := model.AssembleMessages(events)

	// Convert to convMessage and drop:
	//   1. Non-complete messages (will be re-assembled live).
	//   2. Messages whose message_end is at or after triggerSeq. These
	//      will be (or have just been) delivered to onEvent via the live
	//      path; folding them here would double-count.
	//   3. Self-authored messages — our respond path already appends to
	//      history after its own message_end.
	seeded := make([]convMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Status != model.StatusComplete {
			continue
		}
		if m.SeqEnd >= triggerSeq {
			continue
		}
		if m.SenderID == a.id {
			continue
		}
		seeded = append(seeded, convMessage{Sender: m.SenderID, Content: m.Content})
	}

	// Trim to at most maxHistory. The stream is in seq order, so take
	// the tail.
	if n := len(seeded); n > maxHistory {
		seeded = seeded[n-maxHistory:]
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	// Prepend seeded messages only if history is still empty — if another
	// event raced ahead of us and already appended, just mark seeded.
	if len(state.history) == 0 {
		state.history = seeded
	}
	state.seeded = true
	// Mark every event strictly before triggerSeq as already folded so
	// onEvent skips them on replay. The triggering event (and anything
	// after) must still flow through the live path — so cap at
	// triggerSeq - 1.
	if triggerSeq > 0 {
		cap := triggerSeq - 1
		if cap > state.lastSeededSeq {
			state.lastSeededSeq = cap
		}
	}

	log.Debug().
		Str("conv_id", convID.String()).
		Int("seeded_messages", len(seeded)).
		Uint64("last_seeded_seq", state.lastSeededSeq).
		Msg("agent: history seeded")
}
