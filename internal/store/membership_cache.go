package store

import (
	"time"

	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
)

// membershipCache is the in-process LRU for (agent, conversation) membership
// lookups. See sql-metadata-plan.md §8. 100K entries × ~48 bytes ≈ 4.8 MB.
// 60-second TTL bounds staleness as a safety net under any missed invalidation.
const (
	membershipCacheCapacity = 100_000
	membershipCacheTTL      = 60 * time.Second
)

type membershipKey struct {
	AgentID        uuid.UUID
	ConversationID uuid.UUID
}

type membershipEntry struct {
	IsMember bool
	CachedAt time.Time
}

type membershipCache struct {
	cache *lru.Cache[membershipKey, membershipEntry]
	ttl   time.Duration
}

func newMembershipCache() *membershipCache {
	c, _ := lru.New[membershipKey, membershipEntry](membershipCacheCapacity)
	return &membershipCache{cache: c, ttl: membershipCacheTTL}
}

// Get reports the cached membership decision along with a hit flag. A hit
// with expired TTL returns (_, false) so the caller re-queries Postgres.
func (m *membershipCache) Get(agentID, convID uuid.UUID) (bool, bool) {
	entry, ok := m.cache.Get(membershipKey{AgentID: agentID, ConversationID: convID})
	if !ok {
		return false, false
	}
	if time.Since(entry.CachedAt) > m.ttl {
		return false, false
	}
	return entry.IsMember, true
}

// Set records a freshly-computed membership decision. Both polarities are
// cached so a non-member repeatedly failing auth doesn't re-hit Postgres.
func (m *membershipCache) Set(agentID, convID uuid.UUID, isMember bool) {
	m.cache.Add(membershipKey{AgentID: agentID, ConversationID: convID}, membershipEntry{
		IsMember: isMember,
		CachedAt: time.Now(),
	})
}

// Invalidate drops the entry for a single (agent, conversation) pair. Called
// on leave; the next IsMember call falls through to Postgres.
func (m *membershipCache) Invalidate(agentID, convID uuid.UUID) {
	m.cache.Remove(membershipKey{AgentID: agentID, ConversationID: convID})
}
