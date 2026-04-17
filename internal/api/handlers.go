package api

import (
	"agentmail/internal/store"
)

// Handler is the single struct that hangs every HTTP method off. All concrete
// dependencies are injected at construction; handlers pull from the same Go
// struct so tests can swap `meta` and `s2` for in-memory fakes cleanly.
//
// Why one struct instead of one per handler file: the handlers are small and
// share the same five-tuple of dependencies. Splitting into agent/conv/etc
// handler structs produces identical constructor boilerplate five times over
// without gaining any coupling benefit — handler files still logically group
// methods, they just hang off one receiver.
type Handler struct {
	s2       store.S2Store
	meta     store.MetadataStore
	resident ResidentInfo
	conns    *ConnRegistry
}

// NewHandler wires the Handler. A nil resident is rejected — callers pass
// DisabledResident{} when the feature is off.
func NewHandler(s2 store.S2Store, meta store.MetadataStore, resident ResidentInfo, conns *ConnRegistry) *Handler {
	if resident == nil {
		resident = DisabledResident{}
	}
	if conns == nil {
		conns = NewConnRegistry()
	}
	return &Handler{s2: s2, meta: meta, resident: resident, conns: conns}
}
