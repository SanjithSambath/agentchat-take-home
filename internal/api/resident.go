package api

import "github.com/google/uuid"

// ResidentInfo is the seam the HTTP layer uses to expose the in-process
// resident agent, if one is configured. Agent 4 (Claude resident agent)
// implements this; Agent 5 wires an instance (or DisabledResident) into
// NewRouter.
//
// ID/Available back GET /agents/resident. NotifyInvite and NotifyLeave are
// the invite/leave-path seams the conversations handler calls when the
// membership change targets the resident agent itself; the agent's
// in-process discovery loop consumes them to open (or close) its
// per-conversation S2 read session.
type ResidentInfo interface {
	ID() uuid.UUID
	Available() bool
	// NotifyInvite is called after the membership row is inserted for the
	// resident agent. Non-blocking on the handler's side — the
	// implementation must not stall the HTTP request.
	NotifyInvite(convID uuid.UUID)
	// NotifyLeave is called before the leave handler writes agent_left to
	// S2. The returned channel closes once the agent's per-conversation
	// listener has fully drained; the handler waits on it with a bounded
	// deadline so the on-stream order is message_abort (if any) →
	// agent_left. Implementations that aren't listening should return an
	// already-closed channel.
	NotifyLeave(convID uuid.UUID) <-chan struct{}
}

// DisabledResident is the zero implementation used when
// RESIDENT_AGENT_ID is not configured. GET /agents/resident returns
// 503 resident_agent_unavailable for this value; NotifyInvite is a no-op
// and NotifyLeave returns an already-closed channel.
type DisabledResident struct{}

func (DisabledResident) ID() uuid.UUID             { return uuid.Nil }
func (DisabledResident) Available() bool           { return false }
func (DisabledResident) NotifyInvite(uuid.UUID)    {}
func (DisabledResident) NotifyLeave(uuid.UUID) <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
