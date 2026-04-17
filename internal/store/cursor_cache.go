package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cursorCache is the hot tier for the two-tier delivery_seq cursor. See
// sql-metadata-plan.md §7. Updates from the SSE delivery path go in-memory
// only; a background goroutine batches dirty entries into a single unnest()
// upsert every CursorFlushInterval. Regression-guarded in SQL.
//
// Scope: delivery_seq only. ack_seq is synchronous write-through (no hot tier).
type cursorKey struct {
	AgentID        uuid.UUID
	ConversationID uuid.UUID
}

type cursorEntry struct {
	DeliverySeq uint64
	UpdatedAt   time.Time
}

type cursorCache struct {
	pool *pgxpool.Pool

	mu      sync.RWMutex
	cursors map[cursorKey]cursorEntry
	dirty   map[cursorKey]struct{}

	flushInterval time.Duration
}

func newCursorCache(pool *pgxpool.Pool, interval time.Duration) *cursorCache {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &cursorCache{
		pool:          pool,
		cursors:       make(map[cursorKey]cursorEntry),
		dirty:         make(map[cursorKey]struct{}),
		flushInterval: interval,
	}
}

// Update advances delivery_seq in memory. Monotonic — a smaller seq is ignored.
// Marks the key dirty for the next periodic flush.
func (c *cursorCache) Update(agentID, convID uuid.UUID, seq uint64) {
	key := cursorKey{AgentID: agentID, ConversationID: convID}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.cursors[key]; ok && cur.DeliverySeq >= seq {
		return
	}
	c.cursors[key] = cursorEntry{DeliverySeq: seq, UpdatedAt: now}
	c.dirty[key] = struct{}{}
}

// Get returns the current in-memory delivery_seq and whether the entry was
// present. Callers fall back to Postgres on a miss.
func (c *cursorCache) Get(agentID, convID uuid.UUID) (uint64, bool) {
	key := cursorKey{AgentID: agentID, ConversationID: convID}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.cursors[key]
	return e.DeliverySeq, ok
}

// snapshotDirty drains the dirty set and returns parallel slices for the
// unnest() upsert. Under the lock so writes during snapshot either land in
// this flush or the next one.
func (c *cursorCache) snapshotDirty() (keys []cursorKey, agents []uuid.UUID, convs []uuid.UUID, seqs []int64, times []time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.dirty) == 0 {
		return nil, nil, nil, nil, nil
	}
	keys = make([]cursorKey, 0, len(c.dirty))
	agents = make([]uuid.UUID, 0, len(c.dirty))
	convs = make([]uuid.UUID, 0, len(c.dirty))
	seqs = make([]int64, 0, len(c.dirty))
	times = make([]time.Time, 0, len(c.dirty))
	for k := range c.dirty {
		e, ok := c.cursors[k]
		if !ok {
			continue
		}
		keys = append(keys, k)
		agents = append(agents, k.AgentID)
		convs = append(convs, k.ConversationID)
		seqs = append(seqs, int64(e.DeliverySeq))
		times = append(times, e.UpdatedAt)
	}
	// Drain — any concurrent Update between snapshot and flush-commit will
	// re-add the key with a newer value, to be caught by the next tick.
	for _, k := range keys {
		delete(c.dirty, k)
	}
	return
}

// restoreDirty re-marks keys on flush failure so the next tick retries.
func (c *cursorCache) restoreDirty(keys []cursorKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range keys {
		c.dirty[k] = struct{}{}
	}
}

const batchUpsertDeliveryCursor = `
INSERT INTO cursors (agent_id, conversation_id, delivery_seq, updated_at)
SELECT * FROM unnest($1::uuid[], $2::uuid[], $3::bigint[], $4::timestamptz[])
ON CONFLICT (agent_id, conversation_id)
DO UPDATE SET delivery_seq = EXCLUDED.delivery_seq, updated_at = EXCLUDED.updated_at
WHERE cursors.delivery_seq < EXCLUDED.delivery_seq;
`

// FlushAll writes every dirty delivery_seq to Postgres in a single unnest()
// upsert. Regression guard lives in the SQL. Safe to call concurrently with
// Update; either the snapshotted value or the newer one wins per the guard.
func (c *cursorCache) FlushAll(ctx context.Context) error {
	keys, agents, convs, seqs, times := c.snapshotDirty()
	if len(keys) == 0 {
		return nil
	}
	_, err := c.pool.Exec(ctx, batchUpsertDeliveryCursor, agents, convs, seqs, times)
	if err != nil {
		c.restoreDirty(keys)
		return err
	}
	return nil
}

// FlushOne writes a single cursor immediately. Used on SSE disconnect, leave,
// and shutdown per sql-metadata-plan.md §7.
func (c *cursorCache) FlushOne(ctx context.Context, agentID, convID uuid.UUID) error {
	key := cursorKey{AgentID: agentID, ConversationID: convID}
	c.mu.Lock()
	entry, present := c.cursors[key]
	if !present {
		c.mu.Unlock()
		return nil
	}
	// Remove from dirty optimistically; restore if the write fails.
	_, wasDirty := c.dirty[key]
	delete(c.dirty, key)
	c.mu.Unlock()

	_, err := c.pool.Exec(ctx, `
INSERT INTO cursors (agent_id, conversation_id, delivery_seq, updated_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (agent_id, conversation_id)
DO UPDATE SET delivery_seq = EXCLUDED.delivery_seq, updated_at = EXCLUDED.updated_at
WHERE cursors.delivery_seq < EXCLUDED.delivery_seq;
`, agentID, convID, int64(entry.DeliverySeq), entry.UpdatedAt)
	if err != nil {
		if wasDirty {
			c.restoreDirty([]cursorKey{key})
		}
		return err
	}
	return nil
}

// GetFromPostgres is a fallback read used by the store when the hot tier misses.
// Returns 0 when no row exists (fresh tail).
func (c *cursorCache) GetFromPostgres(ctx context.Context, agentID, convID uuid.UUID) (uint64, error) {
	var seq int64
	err := c.pool.QueryRow(ctx,
		`SELECT delivery_seq FROM cursors WHERE agent_id = $1 AND conversation_id = $2`,
		agentID, convID).Scan(&seq)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return uint64(seq), nil
}

// Run starts the periodic flush loop. Blocks until ctx is cancelled, then
// performs a final best-effort flush so graceful shutdown doesn't drop the
// last 5 s of delivery state.
func (c *cursorCache) Run(ctx context.Context) {
	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Best-effort final flush with a fresh short context.
			final, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = c.FlushAll(final)
			cancel()
			return
		case <-ticker.C:
			if err := c.FlushAll(ctx); err != nil && !errors.Is(err, context.Canceled) {
				// Intentionally not logged here — the store wires zerolog in a
				// wrapper. Errors during shutdown are covered by the final flush.
				_ = err
			}
		}
	}
}
