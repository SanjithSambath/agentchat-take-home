package api

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// ConnRegistry tracks active SSE connections and in-flight streaming writes
// per (agent, conversation) so that a leave or shutdown can terminate them
// cleanly. It does two distinct jobs:
//
//  1. SSE — one active tail per (agent, conv). Registering a new one cancels
//     any prior one (the last connection wins, per the spec).
//  2. Streaming writes — multiple in flight allowed, keyed by message_id.
//     Uniqueness across message_ids is enforced by the in_progress_messages
//     UNIQUE constraint in the store layer, not here.
//
// LookupAndCancel cancels both buckets for a given (agent, conv) and returns a
// channel that closes once every handler goroutine has deregistered itself.
// Callers should bound their wait with a timeout.
type ConnRegistry struct {
	mu       sync.Mutex
	sse      map[connKey]*sseEntry
	write    map[connKey]map[uuid.UUID]*writeEntry
	nextSSE  atomic.Uint64
}

type connKey struct {
	Agent uuid.UUID
	Conv  uuid.UUID
}

type sseEntry struct {
	id     uint64
	cancel context.CancelFunc
	done   chan struct{}
}

type writeEntry struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewConnRegistry returns a fresh, empty registry.
func NewConnRegistry() *ConnRegistry {
	return &ConnRegistry{
		sse:   make(map[connKey]*sseEntry),
		write: make(map[connKey]map[uuid.UUID]*writeEntry),
	}
}

// SSEHandle is returned from RegisterSSE. Pass it to DeregisterSSE so we don't
// accidentally deregister an entry that replaced us. `done` is a direct
// reference to our entry's done channel so Deregister can signal it even
// after a replacer has overwritten the map entry.
type SSEHandle struct {
	key  connKey
	id   uint64
	done chan struct{}
}

// RegisterSSE installs cancel as the terminator for an SSE tail on
// (agent, conv). Any previously-registered SSE tail for the same pair is
// cancelled first (last writer wins). The returned prior channel (non-nil
// only if a prior tail existed) closes when the prior handler deregisters —
// callers that need to wait can select on it.
func (r *ConnRegistry) RegisterSSE(agent, conv uuid.UUID, cancel context.CancelFunc) (SSEHandle, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := connKey{agent, conv}
	id := r.nextSSE.Add(1)
	entry := &sseEntry{id: id, cancel: cancel, done: make(chan struct{})}

	var prior <-chan struct{}
	if existing, ok := r.sse[key]; ok {
		existing.cancel()
		prior = existing.done
	}
	r.sse[key] = entry
	return SSEHandle{key: key, id: id, done: entry.done}, prior
}

// DeregisterSSE removes the SSE entry iff it's the same one identified by h
// (a newer connection may have replaced us; leave that entry alone). Our own
// done channel — carried on the handle itself — is closed unconditionally so
// anyone waiting on it unblocks, whether we were current or already replaced.
func (r *ConnRegistry) DeregisterSSE(h SSEHandle) {
	r.mu.Lock()
	if entry, ok := r.sse[h.key]; ok && entry.id == h.id {
		delete(r.sse, h.key)
	}
	r.mu.Unlock()

	if h.done != nil {
		select {
		case <-h.done:
		default:
			close(h.done)
		}
	}
}

// closePriorSSE is used by a newly-registered SSE handler when the prior
// handler may have already exited without calling DeregisterSSE (rare, but
// possible if the prior panicked). It closes the prior's done channel so the
// replacer's prior-wait doesn't block forever. In practice, handlers always
// defer DeregisterSSE; this is a safety valve.
//
// Not currently exported — left as a documentation anchor.

// RegisterWrite installs cancel as the terminator for an in-flight streaming
// write keyed by (agent, conv, messageID). Concurrent distinct message_ids
// are allowed.
func (r *ConnRegistry) RegisterWrite(agent, conv, messageID uuid.UUID, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := connKey{agent, conv}
	m, ok := r.write[key]
	if !ok {
		m = make(map[uuid.UUID]*writeEntry)
		r.write[key] = m
	}
	m[messageID] = &writeEntry{cancel: cancel, done: make(chan struct{})}
}

// DeregisterWrite removes the streaming-write entry for (agent, conv, msg)
// and closes its done channel so leave-waiters unblock.
func (r *ConnRegistry) DeregisterWrite(agent, conv, messageID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := connKey{agent, conv}
	m, ok := r.write[key]
	if !ok {
		return
	}
	entry, ok := m[messageID]
	if !ok {
		return
	}
	delete(m, messageID)
	if len(m) == 0 {
		delete(r.write, key)
	}
	select {
	case <-entry.done:
	default:
		close(entry.done)
	}
}

// LookupAndCancel cancels every SSE tail and streaming write registered for
// (agent, conv). It returns a <-chan struct{} that closes once every cancelled
// goroutine has called Deregister*. Callers should wait on the channel with a
// timeout (leave uses 5 s).
func (r *ConnRegistry) LookupAndCancel(agent, conv uuid.UUID) <-chan struct{} {
	r.mu.Lock()
	key := connKey{agent, conv}

	waits := make([]chan struct{}, 0)

	if entry, ok := r.sse[key]; ok {
		entry.cancel()
		waits = append(waits, entry.done)
	}
	if m, ok := r.write[key]; ok {
		for _, entry := range m {
			entry.cancel()
			waits = append(waits, entry.done)
		}
	}
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		for _, w := range waits {
			<-w
		}
		close(done)
	}()
	return done
}

// WaitFor waits on done with the given timeout. Returns true if done closed,
// false on timeout.
func WaitFor(done <-chan struct{}, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}
