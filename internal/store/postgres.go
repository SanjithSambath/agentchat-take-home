// Package store — Postgres metadata store implementation.
//
// postgres.go is the only concrete MetadataStore. It composes the
// sqlc-generated db.Queries against a pgxpool.Pool with three in-process
// caches (agent existence, membership LRU, delivery_seq hot tier). Every
// design choice is derived from ALL_DESIGN_IMPLEMENTATION/sql-metadata-plan.md.
package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "embed"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"agentmail/internal/store/db"
)

// schemaSQL is the frozen DDL executed on NewMetadataStore. Every CREATE is
// IF NOT EXISTS so running it repeatedly is a no-op.
//
//go:embed schema/schema.sql
var schemaSQL string

// MetadataStoreOptions lets callers tune the flush interval. The server
// wires in config.Config.CursorFlushInterval here.
type MetadataStoreOptions struct {
	CursorFlushInterval time.Duration // default 5s
}

// postgresMetadataStore satisfies store.MetadataStore.
type postgresMetadataStore struct {
	pool *pgxpool.Pool
	q    *db.Queries

	agents      *agentCache
	members     *membershipCache
	cursors     *cursorCache

	// flushCtx/cancel drive the background delivery_seq flusher. Started once
	// by StartDeliveryCursorFlusher and torn down on Close.
	flushMu     sync.Mutex
	flushCtx    context.Context
	flushCancel context.CancelFunc
	flushDone   chan struct{}
}

// NewMetadataStore opens the pgx pool, runs the idempotent schema migration,
// warms the agent existence cache, and returns a ready-to-use store. Does
// NOT start the delivery cursor flusher — callers invoke
// StartDeliveryCursorFlusher once they're ready to accept traffic.
func NewMetadataStore(ctx context.Context, dsn string) (MetadataStore, error) {
	return NewMetadataStoreWithOptions(ctx, dsn, MetadataStoreOptions{})
}

// NewMetadataStoreWithOptions is the variant that accepts a custom cursor
// flush interval. Primarily used by the server wiring layer and tests.
func NewMetadataStoreWithOptions(ctx context.Context, dsn string, opts MetadataStoreOptions) (MetadataStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	cfg.MaxConns = 15
	cfg.MinConns = 5
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnLifetimeJitter = 5 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	// Neon's -pooler endpoint routes through PgBouncer in transaction mode,
	// which forbids session-level prepared statements. CacheDescribe caches
	// the parsed statement description without issuing a server-side PREPARE,
	// giving us sqlc's ergonomics without tripping PgBouncer.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}

	// Idempotent migration. Safe to run on every startup.
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}

	s := &postgresMetadataStore{
		pool:    pool,
		q:       db.New(pool),
		agents:  newAgentCache(),
		members: newMembershipCache(),
		cursors: newCursorCache(pool, opts.CursorFlushInterval),
	}

	if err := s.warmAgentCache(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: warm agent cache: %w", err)
	}

	return s, nil
}

func (s *postgresMetadataStore) warmAgentCache(ctx context.Context) error {
	ids, err := s.q.ListAllAgentIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		s.agents.Add(id)
	}
	return nil
}

// ============================================================
// Agents
// ============================================================

func (s *postgresMetadataStore) CreateAgent(ctx context.Context) (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: mint agent uuid: %w", err)
	}
	row, err := s.q.CreateAgent(ctx, id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: create agent: %w", err)
	}
	s.agents.Add(row.ID)
	return row.ID, nil
}

func (s *postgresMetadataStore) AgentExists(ctx context.Context, id uuid.UUID) (bool, error) {
	if s.agents.Has(id) {
		return true, nil
	}
	ok, err := s.q.AgentExists(ctx, id)
	if err != nil {
		return false, fmt.Errorf("store: agent exists: %w", err)
	}
	if ok {
		s.agents.Add(id)
	}
	return ok, nil
}

// ============================================================
// Conversations
// ============================================================

func (s *postgresMetadataStore) CreateConversation(ctx context.Context, id uuid.UUID, s2StreamName string) error {
	if err := s.q.CreateConversation(ctx, db.CreateConversationParams{
		ID:           id,
		S2StreamName: s2StreamName,
	}); err != nil {
		return fmt.Errorf("store: create conversation: %w", err)
	}
	return nil
}

func (s *postgresMetadataStore) GetConversation(ctx context.Context, id uuid.UUID) (*Conversation, error) {
	row, err := s.q.GetConversation(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrConversationNotFound
		}
		return nil, fmt.Errorf("store: get conversation: %w", err)
	}
	return &Conversation{
		ID:           row.ID,
		S2StreamName: row.S2StreamName,
		HeadSeq:      uint64(row.HeadSeq),
		CreatedAt:    row.CreatedAt.Time,
	}, nil
}

func (s *postgresMetadataStore) ConversationExists(ctx context.Context, id uuid.UUID) (bool, error) {
	ok, err := s.q.ConversationExists(ctx, id)
	if err != nil {
		return false, fmt.Errorf("store: conversation exists: %w", err)
	}
	return ok, nil
}

func (s *postgresMetadataStore) GetConversationHeadSeq(ctx context.Context, id uuid.UUID) (uint64, error) {
	seq, err := s.q.GetConversationHeadSeq(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrConversationNotFound
		}
		return 0, fmt.Errorf("store: get head seq: %w", err)
	}
	return uint64(seq), nil
}

func (s *postgresMetadataStore) UpdateConversationHeadSeq(ctx context.Context, id uuid.UUID, headSeq uint64) error {
	if err := s.q.UpdateConversationHeadSeq(ctx, db.UpdateConversationHeadSeqParams{
		ID:      id,
		HeadSeq: int64(headSeq),
	}); err != nil {
		return fmt.Errorf("store: update head seq: %w", err)
	}
	return nil
}

// ============================================================
// Membership
// ============================================================

func (s *postgresMetadataStore) AddMember(ctx context.Context, convID, agentID uuid.UUID) (bool, error) {
	_, err := s.q.AddMember(ctx, db.AddMemberParams{
		ConversationID: convID,
		AgentID:        agentID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT DO NOTHING — agent was already a member. Cache the
			// positive so a follow-up IsMember hits.
			s.members.Set(agentID, convID, true)
			return false, nil
		}
		return false, fmt.Errorf("store: add member: %w", err)
	}
	s.members.Set(agentID, convID, true)
	return true, nil
}

// RemoveMember serializes concurrent leaves via SELECT ... FOR UPDATE inside
// a single transaction (see sql-metadata-plan.md §9 Race Condition 1). The
// caller of this method does not manage the transaction — this function owns
// it end-to-end so the lock protects the last-member check.
func (s *postgresMetadataStore) RemoveMember(ctx context.Context, convID, agentID uuid.UUID) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: remove member begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := s.q.WithTx(tx)

	members, err := qtx.LockMembersForUpdate(ctx, convID)
	if err != nil {
		return fmt.Errorf("store: remove member lock: %w", err)
	}

	present := false
	for _, m := range members {
		if m == agentID {
			present = true
			break
		}
	}
	if !present {
		return ErrNotMember
	}
	if len(members) == 1 {
		return ErrLastMember
	}

	if err := qtx.RemoveMember(ctx, db.RemoveMemberParams{
		ConversationID: convID,
		AgentID:        agentID,
	}); err != nil {
		return fmt.Errorf("store: remove member delete: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: remove member commit: %w", err)
	}

	// Invalidate rather than Set(false): per plan §8, a re-invite shortly
	// after a leave should hit Postgres for the fresh truth.
	s.members.Invalidate(agentID, convID)
	return nil
}

func (s *postgresMetadataStore) IsMember(ctx context.Context, agentID, convID uuid.UUID) (bool, error) {
	if v, hit := s.members.Get(agentID, convID); hit {
		return v, nil
	}
	ok, err := s.q.IsMember(ctx, db.IsMemberParams{
		ConversationID: convID,
		AgentID:        agentID,
	})
	if err != nil {
		return false, fmt.Errorf("store: is member: %w", err)
	}
	s.members.Set(agentID, convID, ok)
	return ok, nil
}

func (s *postgresMetadataStore) ListMembers(ctx context.Context, convID uuid.UUID) ([]uuid.UUID, error) {
	members, err := s.q.ListMembers(ctx, convID)
	if err != nil {
		return nil, fmt.Errorf("store: list members: %w", err)
	}
	return members, nil
}

func (s *postgresMetadataStore) ListConversationsForAgent(ctx context.Context, agentID uuid.UUID) ([]Conversation, error) {
	rows, err := s.q.ListConversationsForAgent(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: list conversations: %w", err)
	}
	out := make([]Conversation, 0, len(rows))
	for _, r := range rows {
		out = append(out, Conversation{
			ID:           r.ID,
			S2StreamName: r.S2StreamName,
			HeadSeq:      uint64(r.HeadSeq),
			CreatedAt:    r.CreatedAt.Time,
		})
	}
	return out, nil
}

func (s *postgresMetadataStore) ListAllConversations(ctx context.Context) ([]Conversation, error) {
	rows, err := s.q.ListAllConversations(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list all conversations: %w", err)
	}
	out := make([]Conversation, 0, len(rows))
	for _, r := range rows {
		out = append(out, Conversation{
			ID:           r.ID,
			S2StreamName: r.S2StreamName,
			HeadSeq:      uint64(r.HeadSeq),
			CreatedAt:    r.CreatedAt.Time,
		})
	}
	return out, nil
}

// LockMembersForUpdate returns the current member set under a row-level lock.
// The lock only holds for the lifetime of this call (short-lived transaction) —
// real serialization of concurrent leaves lives inside RemoveMember.
func (s *postgresMetadataStore) LockMembersForUpdate(ctx context.Context, convID uuid.UUID) ([]uuid.UUID, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("store: lock members begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)
	members, err := qtx.LockMembersForUpdate(ctx, convID)
	if err != nil {
		return nil, fmt.Errorf("store: lock members: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: lock members commit: %w", err)
	}
	return members, nil
}

// ============================================================
// Delivery cursor (two-tier)
// ============================================================

func (s *postgresMetadataStore) GetDeliveryCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error) {
	if seq, ok := s.cursors.Get(agentID, convID); ok {
		return seq, nil
	}
	return s.cursors.GetFromPostgres(ctx, agentID, convID)
}

func (s *postgresMetadataStore) UpdateDeliveryCursor(agentID, convID uuid.UUID, seqNum uint64) {
	s.cursors.Update(agentID, convID, seqNum)
}

func (s *postgresMetadataStore) FlushDeliveryCursorOne(ctx context.Context, agentID, convID uuid.UUID) error {
	return s.cursors.FlushOne(ctx, agentID, convID)
}

func (s *postgresMetadataStore) FlushDeliveryCursorAll(ctx context.Context) error {
	return s.cursors.FlushAll(ctx)
}

// StartDeliveryCursorFlusher starts the background flush goroutine exactly
// once. Subsequent calls are no-ops so the server can call it from multiple
// init paths without guarding itself. Ownership of shutdown belongs to Close.
func (s *postgresMetadataStore) StartDeliveryCursorFlusher(ctx context.Context) {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if s.flushCancel != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.flushCtx = runCtx
	s.flushCancel = cancel
	s.flushDone = done
	go func() {
		defer close(done)
		s.cursors.Run(runCtx)
	}()
}

// ============================================================
// Ack cursor (single-tier, synchronous)
// ============================================================

func (s *postgresMetadataStore) Ack(ctx context.Context, agentID, convID uuid.UUID, seqNum uint64) error {
	if err := s.q.AckCursor(ctx, db.AckCursorParams{
		AgentID:        agentID,
		ConversationID: convID,
		AckSeq:         int64(seqNum),
	}); err != nil {
		return fmt.Errorf("store: ack: %w", err)
	}
	return nil
}

func (s *postgresMetadataStore) GetAckCursor(ctx context.Context, agentID, convID uuid.UUID) (uint64, error) {
	seq, err := s.q.GetAckCursor(ctx, db.GetAckCursorParams{
		AgentID:        agentID,
		ConversationID: convID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: get ack: %w", err)
	}
	return uint64(seq), nil
}

func (s *postgresMetadataStore) ListUnreadForAgent(ctx context.Context, agentID uuid.UUID, limit int32) ([]UnreadEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.q.ListUnreadForAgent(ctx, db.ListUnreadForAgentParams{
		AgentID: agentID,
		Limit:   limit,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list unread: %w", err)
	}
	out := make([]UnreadEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, UnreadEntry{
			ConversationID: r.ConversationID,
			HeadSeq:        uint64(r.HeadSeq),
			AckSeq:         uint64(r.AckSeq),
			EventDelta:     uint64(r.EventDelta),
		})
	}
	return out, nil
}

// ============================================================
// In-progress messages
// ============================================================

func (s *postgresMetadataStore) ClaimInProgressMessage(ctx context.Context, messageID, convID, agentID uuid.UUID, s2StreamName string) (bool, error) {
	n, err := s.q.ClaimInProgressMessage(ctx, db.ClaimInProgressMessageParams{
		MessageID:      messageID,
		ConversationID: convID,
		AgentID:        agentID,
		S2StreamName:   s2StreamName,
	})
	if err != nil {
		return false, fmt.Errorf("store: claim in-progress: %w", err)
	}
	return n == 1, nil
}

func (s *postgresMetadataStore) DeleteInProgressMessage(ctx context.Context, messageID uuid.UUID) error {
	if err := s.q.DeleteInProgressMessage(ctx, messageID); err != nil {
		return fmt.Errorf("store: delete in-progress: %w", err)
	}
	return nil
}

func (s *postgresMetadataStore) ListInProgressMessages(ctx context.Context) ([]InProgressRow, error) {
	rows, err := s.q.ListInProgressMessages(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list in-progress: %w", err)
	}
	out := make([]InProgressRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, InProgressRow{
			MessageID:      r.MessageID,
			ConversationID: r.ConversationID,
			AgentID:        r.AgentID,
			S2StreamName:   r.S2StreamName,
			StartedAt:      r.StartedAt.Time,
		})
	}
	return out, nil
}

// ============================================================
// Message dedup (terminal outcomes)
// ============================================================

func (s *postgresMetadataStore) GetMessageDedup(ctx context.Context, convID, messageID uuid.UUID) (*DedupRow, bool, error) {
	row, err := s.q.GetMessageDedup(ctx, db.GetMessageDedupParams{
		ConversationID: convID,
		MessageID:      messageID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("store: get dedup: %w", err)
	}
	out := &DedupRow{Status: row.Status}
	if row.StartSeq != nil {
		v := uint64(*row.StartSeq)
		out.StartSeq = &v
	}
	if row.EndSeq != nil {
		v := uint64(*row.EndSeq)
		out.EndSeq = &v
	}
	return out, true, nil
}

func (s *postgresMetadataStore) InsertMessageDedupComplete(ctx context.Context, convID, messageID uuid.UUID, startSeq, endSeq uint64) error {
	start := int64(startSeq)
	end := int64(endSeq)
	if err := s.q.InsertMessageDedupComplete(ctx, db.InsertMessageDedupCompleteParams{
		ConversationID: convID,
		MessageID:      messageID,
		StartSeq:       &start,
		EndSeq:         &end,
	}); err != nil {
		return fmt.Errorf("store: insert dedup complete: %w", err)
	}
	return nil
}

// InsertMessageDedupAborted is implemented via raw pgx because sqlc cannot
// cleanly express a nullable positional bigint parameter for start_seq.
// end_seq is always NULL for aborted rows.
const insertMessageDedupAbortedSQL = `
INSERT INTO messages_dedup (conversation_id, message_id, status, start_seq, end_seq)
VALUES ($1, $2, 'aborted', $3, NULL)
ON CONFLICT (conversation_id, message_id) DO NOTHING;
`

func (s *postgresMetadataStore) InsertMessageDedupAborted(ctx context.Context, convID, messageID uuid.UUID, startSeq *uint64) error {
	var startArg any
	if startSeq != nil {
		// pgtype.Int8 survives PgBouncer transaction-mode; plain int64 also works,
		// but using pgtype keeps null handling explicit for readers.
		startArg = pgtype.Int8{Int64: int64(*startSeq), Valid: true}
	} else {
		startArg = pgtype.Int8{Valid: false}
	}
	if _, err := s.pool.Exec(ctx, insertMessageDedupAbortedSQL, convID, messageID, startArg); err != nil {
		return fmt.Errorf("store: insert dedup aborted: %w", err)
	}
	return nil
}

// ============================================================
// Lifecycle
// ============================================================

func (s *postgresMetadataStore) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

func (s *postgresMetadataStore) Close() error {
	s.flushMu.Lock()
	cancel := s.flushCancel
	done := s.flushDone
	s.flushCancel = nil
	s.flushDone = nil
	s.flushMu.Unlock()

	if cancel != nil {
		cancel()
		if done != nil {
			<-done
		}
	} else {
		// Never started — still try one last flush so tests that don't start
		// the flusher don't lose state on Close.
		ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		_ = s.cursors.FlushAll(ctx)
		c()
	}

	s.pool.Close()
	return nil
}

// Compile-time interface assertion.
var _ MetadataStore = (*postgresMetadataStore)(nil)
