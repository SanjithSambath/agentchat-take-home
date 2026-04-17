package store

import (
	"sync"

	"github.com/google/uuid"
)

// agentCache is a sync.Map-backed existence cache for agent IDs. It's
// populated at startup (ListAllAgentIDs) and on every successful CreateAgent,
// then read on every authenticated request to avoid a Postgres round-trip.
//
// Negative results are NOT cached — a Postgres miss leaves the map untouched
// so a subsequent CreateAgent (or external insert) is immediately visible.
// Since agents are permanent (no deletion), the map only ever grows.
type agentCache struct {
	m sync.Map // uuid.UUID -> struct{}
}

func newAgentCache() *agentCache { return &agentCache{} }

func (c *agentCache) Has(id uuid.UUID) bool {
	_, ok := c.m.Load(id)
	return ok
}

func (c *agentCache) Add(id uuid.UUID) {
	c.m.Store(id, struct{}{})
}
