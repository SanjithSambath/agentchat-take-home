// Package agent implements the Claude-powered resident agent that lives
// inside the AgentMail server process. The agent registers itself on startup,
// opens one S2 ReadSession per conversation it is a member of, streams Claude
// responses back through S2 AppendSessions on every message_end from another
// agent, and shuts down cleanly on process termination.
//
// Design: ALL_DESIGN_IMPLEMENTATION/claude-agent-plan.md (§§1, 3-7).
// Integration: §10 of high-level-overview.md. The agent consumes the frozen
// store interfaces directly; it never round-trips through HTTP to talk to
// itself.
package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"agentmail/internal/config"
	"agentmail/internal/store"
)

// Tunables. Picked per claude-agent-plan.md; keep named and in one place.
const (
	inviteChanBuffer    = 100
	maxHistory          = 50
	globalClaudeConc    = 5
	stalePendingTTL     = 5 * time.Minute
	listenerMaxBackoff  = 30 * time.Second
	reconcileMaxRetries = 3
	leaveDrainDeadline  = 5 * time.Second

	modelID   = anthropic.ModelClaudeOpus4_7
	maxTokens = 4096

	systemPrompt = "You are a helpful AI assistant on the AgentMail platform. " +
		"You are participating in a conversation with one or more AI agents. " +
		"Respond naturally and concisely."
)

// ResidentInfo is the surface Agent 3 (HTTP) uses to mount GET /agents/resident.
// Declared here so the agent package compiles standalone; Agent 5 can re-export
// (or consume via structural typing) at integration time.
type ResidentInfo interface {
	ID() uuid.UUID
	Available() bool
}

// Agent is the in-process Claude-powered resident agent. One Agent per
// RESIDENT_AGENT_ID; the server constructs it once at startup.
type Agent struct {
	id     uuid.UUID
	s2     store.S2Store
	meta   store.MetadataStore
	claude claudeStreamer

	inviteCh  chan uuid.UUID
	claudeSem chan struct{}

	mu         sync.Mutex
	listeners  map[uuid.UUID]*listenerHandle
	convStates map[uuid.UUID]*convState
	wg         sync.WaitGroup

	rootCtx    context.Context
	rootCancel context.CancelFunc

	started   atomic.Bool
	available atomic.Bool
}

// listenerHandle is the per-conversation control block. cancel stops the
// listener's context; done closes after the goroutine exits fully (cursor
// flushed, state cleaned up).
type listenerHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// claudeStreamer is the minimal surface respond.go needs from the Claude API.
// Kept as an interface so tests can inject a fake stream without the Anthropic
// SDK. The real implementation is anthropicStreamer (see below).
type claudeStreamer interface {
	Stream(ctx context.Context, msgs []anthropic.MessageParam) claudeStream
}

// claudeStream mirrors *ssestream.Stream[MessageStreamEventUnion] — the
// concrete return type of anthropic.MessageService.NewStreaming. The pointer
// type satisfies this interface directly, so no wrapper is needed on the hot
// path.
type claudeStream interface {
	Next() bool
	Current() anthropic.MessageStreamEventUnion
	Err() error
	Close() error
}

// anthropicStreamer is the production claudeStreamer. It forwards to the
// Anthropic Go SDK's streaming messages API with this agent's fixed params.
type anthropicStreamer struct {
	client *anthropic.Client
}

func (a *anthropicStreamer) Stream(ctx context.Context, msgs []anthropic.MessageParam) claudeStream {
	return a.client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     modelID,
		MaxTokens: maxTokens,
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Messages:  msgs,
	})
}

// NewAgent constructs a resident agent. The caller is responsible for also
// constructing and passing in the Anthropic client; the agent does not touch
// the environment directly. Returns an error if the config disables the
// resident agent or if any dependency is nil.
func NewAgent(cfg config.Config, s2 store.S2Store, meta store.MetadataStore, claude *anthropic.Client) (*Agent, error) {
	if !cfg.HasResidentAgent() {
		return nil, errors.New("agent: no resident agent configured")
	}
	if s2 == nil || meta == nil {
		return nil, errors.New("agent: s2 and meta stores must be non-nil")
	}
	if claude == nil {
		return nil, errors.New("agent: anthropic client must be non-nil")
	}
	return newAgentWithStreamer(cfg.ResidentAgentID, s2, meta, &anthropicStreamer{client: claude})
}

// newAgentWithStreamer is the test-friendly constructor. Not exported.
func newAgentWithStreamer(id uuid.UUID, s2 store.S2Store, meta store.MetadataStore, streamer claudeStreamer) (*Agent, error) {
	if id == uuid.Nil {
		return nil, errors.New("agent: resident agent id must not be nil")
	}
	return &Agent{
		id:         id,
		s2:         s2,
		meta:       meta,
		claude:     streamer,
		inviteCh:   make(chan uuid.UUID, inviteChanBuffer),
		claudeSem:  make(chan struct{}, globalClaudeConc),
		listeners:  make(map[uuid.UUID]*listenerHandle),
		convStates: make(map[uuid.UUID]*convState),
	}, nil
}

// Start registers the agent's identity, reconciles existing conversations,
// and activates the invite channel. Must be called once before any route
// that may deliver invites is serving traffic.
func (a *Agent) Start(ctx context.Context) error {
	if !a.started.CompareAndSwap(false, true) {
		return errors.New("agent: Start called twice")
	}
	a.rootCtx, a.rootCancel = context.WithCancel(context.Background())

	if err := a.registerIdentity(ctx); err != nil {
		a.rootCancel()
		return err
	}

	a.reconcileConversations(ctx)

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.discoveryLoop()
	}()

	a.available.Store(true)
	log.Info().Str("agent_id", a.id.String()).Msg("agent: resident agent started")
	return nil
}

// Shutdown cancels all listeners, waits for in-flight Claude responses to
// drain, flushes cursors, and returns. If shutdownCtx fires first, shutdown
// returns with some goroutines still running; those may lose up to
// CursorFlushInterval of cursor drift (at-least-once semantics).
func (a *Agent) Shutdown(shutdownCtx context.Context) {
	if !a.started.Load() {
		return
	}
	a.available.Store(false)

	a.rootCancel()
	a.mu.Lock()
	for _, l := range a.listeners {
		l.cancel()
	}
	a.mu.Unlock()

	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Info().Msg("agent: shutdown complete")
	case <-shutdownCtx.Done():
		log.Warn().Msg("agent: shutdown deadline exceeded; some listeners may not have flushed")
	}
}

// ID returns the agent's stable identity. Safe to call concurrently.
func (a *Agent) ID() uuid.UUID { return a.id }

// Available reports whether the agent is currently serving. False before
// Start returns and after Shutdown begins.
func (a *Agent) Available() bool { return a.available.Load() }

// NotifyInvite is the integration seam for the HTTP invite handler. It must
// be called after the agent row has been inserted into members (regardless
// of whether the agent_joined S2 append succeeded — the listener retry loop
// is self-healing on stream-not-found). Non-blocking: drops with a warning
// if the buffer is full (100-slot buffer; at take-home scale never occurs).
func (a *Agent) NotifyInvite(convID uuid.UUID) {
	if !a.available.Load() {
		return
	}
	select {
	case a.inviteCh <- convID:
	default:
		log.Warn().
			Str("conv_id", convID.String()).
			Msg("agent: invite channel full, dropping notification")
	}
}

// NotifyLeave is the integration seam for the HTTP leave handler. It cancels
// the per-conversation listener and returns a channel that closes when the
// listener goroutine has fully exited (cursor flushed, in-flight response
// aborted if any). The leave handler should wait on this channel (with a
// ~5 s deadline) before writing agent_left to S2, so the on-stream order is
// message_abort (if any) → agent_left.
//
// If the agent isn't listening to the conversation, returns an already-closed
// channel.
func (a *Agent) NotifyLeave(convID uuid.UUID) <-chan struct{} {
	a.mu.Lock()
	l, ok := a.listeners[convID]
	a.mu.Unlock()
	if !ok {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	l.cancel()
	return l.done
}
