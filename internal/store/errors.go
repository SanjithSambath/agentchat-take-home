package store

import "errors"

// Canonical sentinel errors returned by the two store interfaces. Handlers
// branch on these with errors.Is.
var (
	// ErrAgentNotFound is returned when an agent lookup misses.
	ErrAgentNotFound = errors.New("store: agent not found")

	// ErrConversationNotFound is returned when a conversation lookup misses.
	ErrConversationNotFound = errors.New("store: conversation not found")

	// ErrNotMember is returned by RemoveMember when the row doesn't exist.
	ErrNotMember = errors.New("store: agent not a member of conversation")

	// ErrLastMember is returned by RemoveMember when removal would drop the
	// conversation to zero members. Surfaces as HTTP 409 last_member.
	ErrLastMember = errors.New("store: cannot remove last member of conversation")

	// ErrAlreadyAborted is returned by the dedup gate when a prior attempt
	// with the same message_id was aborted. Surfaces as HTTP 409 already_aborted.
	ErrAlreadyAborted = errors.New("store: message_id already aborted")

	// ErrSlowWriter is returned by AppendSession.Submit when the 2 s timeout
	// fires. Surfaces as HTTP 503 slow_writer on the streaming write path.
	ErrSlowWriter = errors.New("store: slow writer")

	// ErrRangeNotSatisfiable is returned by ReadRange / OpenReadSession when
	// the caller's fromSeq is behind the stream's trim point.
	ErrRangeNotSatisfiable = errors.New("store: sequence range not satisfiable")
)
