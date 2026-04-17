// Package store defines the frozen interfaces for durable storage.
//
// Two interfaces live here: S2Store (event stream) and MetadataStore
// (Postgres). Every other package depends only on these interfaces. Concrete
// implementations live in sibling files (s2.go, postgres.go) and are produced
// by Phase 1 agents.
//
// See:
//   - ALL_DESIGN_IMPLEMENTATION/s2-architecture-plan.md
//   - ALL_DESIGN_IMPLEMENTATION/sql-metadata-plan.md
package store

import (
	"context"
	"time"

	"github.com/google/uuid"

	"agentmail/internal/model"
)

// ============================================================
// S2 Event Stream Store
// ============================================================

// S2Store is the wrapper around the S2 SDK. One stream per conversation,
// named `conversations/<conversation_id>`. Streams auto-create on first
// append (CreateStreamOnAppend = true).
type S2Store interface {
	// AppendEvents writes a batch of events atomically as a single unary append.
	// Blocks until S2 acknowledges (~40 ms on Express storage). Used for system
	// events and complete messages ([start, append, end] as one 3-record batch).
	AppendEvents(ctx context.Context, convID uuid.UUID, events []model.Event) (AppendResult, error)

	// OpenAppendSession opens a pipelined append session for token streaming.
	// Submit calls are non-blocking; ack drain happens in the background.
	// Session lifetime = one streaming POST request. Each Submit wraps a 2 s
	// context to avoid a slow S2 path pinning the handler goroutine.
	OpenAppendSession(ctx context.Context, convID uuid.UUID) (AppendSession, error)

	// OpenReadSession starts a tailing read session from fromSeq. Catch-up
	// replay transitions seamlessly to real-time tail. The returned session
	// blocks in Next() until a record arrives or ctx is cancelled.
	OpenReadSession(ctx context.Context, convID uuid.UUID, fromSeq uint64) (ReadSession, error)

	// ReadRange reads a bounded, non-tailing range. Used by the history
	// endpoint and the recovery sweep.
	ReadRange(ctx context.Context, convID uuid.UUID, fromSeq uint64, limit int) ([]model.SequencedEvent, error)

	// CheckTail returns the current tail sequence number of a conversation.
	// Used for health checks and cursor validation.
	CheckTail(ctx context.Context, convID uuid.UUID) (uint64, error)
}

// AppendResult reports the S2-assigned sequence range of a successful append.
// StartSeq is inclusive, EndSeq is one past the last record.
type AppendResult struct {
	StartSeq uint64
	EndSeq   uint64
}

// AppendSession is the interface a pipelined append session exposes to callers.
type AppendSession interface {
	// Submit queues one event. Non-blocking; acks drained in the background.
	// Wraps a 2 s timeout internally — returns ErrSlowWriter on timeout.
	Submit(ctx context.Context, event model.Event) error

	// Close flushes remaining acks and releases resources.
	Close() error
}

// ReadSession is the interface a tailing read session exposes to callers.
// Standard iterator pattern.
type ReadSession interface {
	// Next blocks until a record arrives (or ctx is cancelled or stream ends).
	// Returns false on cancellation / error; caller should check Err().
	Next() bool

	// Event returns the current record. Only valid after Next returned true.
	Event() model.SequencedEvent

	// Err returns any error encountered. Checked after Next returns false.
	Err() error

	// Close releases the session. Idempotent.
	Close() error
}

// ============================================================
// Postgres Metadata Store
// ============================================================

// MetadataStore is the hand-written facade over the sqlc-generated queries
// plus in-memory caches. Every Postgres operation goes through this interface.
// Implementations: internal/store/postgres.go (real), test doubles in tests.
type MetadataStore interface {
	// --- Agents ---

	// CreateAgent inserts a fresh agent row with a new UUIDv7 and returns the id.
	// Also populates the agent-existence cache.
	CreateAgent(ctx context.Context) (uuid.UUID, error)

	// AgentExists is the hot-path check behind the agent-auth middleware.
	// Cache-first (sync.Map, ~50 ns on hit), Postgres fallback on miss.
	AgentExists(ctx context.Context, id uuid.UUID) (bool, error)

	// --- Conversations ---

	// CreateConversation inserts a conversation row with the given id and
	// S2 stream name. Called inside the POST /conversations handler.
	CreateConversation(ctx context.Context, id uuid.UUID, s2StreamName string) error

	// GetConversation returns the full row or nil if not found.
	GetConversation(ctx context.Context, id uuid.UUID) (*Conversation, error)

	// ConversationExists is a lightweight existence check.
	ConversationExists(ctx context.Context, id uuid.UUID) (bool, error)

	// GetConversationHeadSeq returns the cached head_seq (server-side tail cursor).
	GetConversationHeadSeq(ctx context.Context, id uuid.UUID) (uint64, error)

	// UpdateConversationHeadSeq advances the cached head_seq. Called inline
	// after every successful S2 append so GET /agents/me/unread is cheap.
	UpdateConversationHeadSeq(ctx context.Context, id uuid.UUID, headSeq uint64) error

	// --- Membership ---

	// AddMember inserts a (convID, agentID) row idempotently. Returns true
	// if the row was newly added (caller writes an agent_joined event) or
	// false if the agent was already a member (no event).
	AddMember(ctx context.Context, convID, agentID uuid.UUID) (bool, error)

	// RemoveMember hard-deletes the (convID, agentID) row. Errors with
	// ErrLastMember if this would drop the member count to zero and with
	// ErrNotMember if no such row exists. Must be called inside a transaction
	// that also holds LockMembersForUpdate to serialize concurrent leaves.
	RemoveMember(ctx context.Context, convID, agentID uuid.UUID) error

	// IsMember is the hot-path membership check. LRU-cached (60 s TTL).
	IsMember(ctx context.Context, agentID, convID uuid.UUID) (bool, error)

	// ListMembers returns all current member agent ids for a conversation.
	ListMembers(ctx context.Context, convID uuid.UUID) ([]uuid.UUID, error)

	// ListConversationsForAgent returns all conversations the agent is a
	// current member of. Powers GET /conversations.
	ListConversationsForAgent(ctx context.Context, agentID uuid.UUID) ([]Conversation, error)

	// LockMembersForUpdate takes a SELECT ... FOR UPDATE lock on all member
	// rows for the conversation. Called inside a transaction before RemoveMember
	// to serialize concurrent leaves (prevents two last-members leaving at once).
	LockMembersForUpdate(ctx context.Context, convID uuid.UUID) ([]uuid.UUID, error)

	// --- Cursors: delivery (two-tier) ---

	// GetDeliveryCursor returns the current delivery_seq (hot tier if present,
	// Postgres otherwise). Used when opening a new SSE connection.
	GetDeliveryCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error)

	// UpdateDeliveryCursor advances the in-memory hot tier. Non-blocking,
	// no I/O, no error. Flushed to Postgres by the background goroutine
	// every CursorFlushInterval (default 5 s).
	UpdateDeliveryCursor(agentID, convID uuid.UUID, seqNum uint64)

	// FlushDeliveryCursorOne writes a single (agent, conversation) cursor
	// immediately. Called on SSE disconnect, leave, and shutdown.
	FlushDeliveryCursorOne(ctx context.Context, agentID, convID uuid.UUID) error

	// FlushDeliveryCursorAll writes every dirty cursor. Called by the
	// background ticker and on shutdown.
	FlushDeliveryCursorAll(ctx context.Context) error

	// StartDeliveryCursorFlusher runs the periodic flush loop until ctx is
	// cancelled. Call once at server start.
	StartDeliveryCursorFlusher(ctx context.Context)

	// --- Cursors: ack (single-tier, synchronous) ---

	// Ack advances ack_seq for the agent-conversation pair. Synchronous
	// write-through (no hot tier). Regression-guarded: a lower seqNum is a no-op.
	Ack(ctx context.Context, agentID, convID uuid.UUID, seqNum uint64) error

	// GetAckCursor returns the last acked sequence number.
	GetAckCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error)

	// ListUnreadForAgent returns unread entries (head_seq - ack_seq) for every
	// conversation the agent is a member of. Powers GET /agents/me/unread.
	ListUnreadForAgent(ctx context.Context, agentID uuid.UUID, limit int32) ([]UnreadEntry, error)

	// --- In-progress messages (crash recovery) ---

	// ClaimInProgressMessage inserts a row atomically. Returns true if the
	// claim was accepted or false if (convID, messageID) already existed —
	// the latter means the client is retrying an in-flight write.
	ClaimInProgressMessage(ctx context.Context, messageID, convID, agentID uuid.UUID, s2StreamName string) (bool, error)

	// DeleteInProgressMessage removes the claim on successful terminal state.
	DeleteInProgressMessage(ctx context.Context, messageID uuid.UUID) error

	// ListInProgressMessages returns every outstanding claim at startup so
	// the recovery sweep can abort them.
	ListInProgressMessages(ctx context.Context) ([]InProgressRow, error)

	// --- Message dedup (terminal outcomes) ---

	// GetMessageDedup returns the terminal record for (convID, messageID), if any.
	// Second return value is true if a row was found.
	GetMessageDedup(ctx context.Context, convID, messageID uuid.UUID) (*DedupRow, bool, error)

	// InsertMessageDedupComplete records a successful complete-message outcome.
	// Carries start_seq and end_seq so retries replay the same sequence range.
	InsertMessageDedupComplete(ctx context.Context, convID, messageID uuid.UUID, startSeq, endSeq uint64) error

	// InsertMessageDedupAborted records an aborted-message outcome so retries
	// with the same message_id get 409 already_aborted.
	InsertMessageDedupAborted(ctx context.Context, convID, messageID uuid.UUID, startSeq *uint64) error

	// --- Lifecycle ---

	// Ping verifies that the underlying Postgres pool can service a round-trip.
	// Used by the /health endpoint; caller supplies a short timeout.
	Ping(ctx context.Context) error

	// Close releases the underlying connection pool.
	Close() error
}

// ============================================================
// Auxiliary types (shared across implementations)
// ============================================================

// Conversation is the row shape returned by GetConversation and
// ListConversationsForAgent. head_seq is the server-cached S2 tail cursor.
type Conversation struct {
	ID           uuid.UUID `json:"id"`
	S2StreamName string    `json:"s2_stream_name"`
	HeadSeq      uint64    `json:"head_seq"`
	CreatedAt    time.Time `json:"created_at"`
}

// UnreadEntry is the shape returned by ListUnreadForAgent. EventDelta is
// head_seq - ack_seq.
type UnreadEntry struct {
	ConversationID uuid.UUID `json:"conversation_id"`
	HeadSeq        uint64    `json:"head_seq"`
	AckSeq         uint64    `json:"ack_seq"`
	EventDelta     uint64    `json:"event_delta"`
}

// InProgressRow is the shape returned by ListInProgressMessages. Used by
// the recovery sweep at startup.
type InProgressRow struct {
	MessageID      uuid.UUID
	ConversationID uuid.UUID
	AgentID        uuid.UUID
	S2StreamName   string
	StartedAt      time.Time
}

// DedupRow is the terminal outcome of a message write. StartSeq and EndSeq
// are non-nil only for Status = "complete"; aborted rows may carry StartSeq
// if the abort happened after message_start was appended.
type DedupRow struct {
	Status   string
	StartSeq *uint64
	EndSeq   *uint64
}
