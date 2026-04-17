package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestConnRegistry_SSEReplaceOnRegister(t *testing.T) {
	reg := NewConnRegistry()
	agent, conv := uuid.New(), uuid.New()

	var firstCancelled atomic.Bool
	firstCtx, firstCancel := context.WithCancel(context.Background())
	go func() {
		<-firstCtx.Done()
		firstCancelled.Store(true)
	}()
	h1, prior1 := reg.RegisterSSE(agent, conv, firstCancel)
	if prior1 != nil {
		t.Fatal("expected no prior on first register")
	}

	// Register a second one.
	_, prior2 := reg.RegisterSSE(agent, conv, func() {})
	if prior2 == nil {
		t.Fatal("expected prior channel on second register")
	}

	// First should be cancelled.
	select {
	case <-firstCtx.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first SSE never cancelled")
	}
	reg.DeregisterSSE(h1)

	// prior2's done channel should close now (first deregistered).
	select {
	case <-prior2:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("prior channel never closed after deregister")
	}
}

func TestConnRegistry_LookupAndCancelMulti(t *testing.T) {
	reg := NewConnRegistry()
	agent, conv := uuid.New(), uuid.New()

	sseCtx, sseCancel := context.WithCancel(context.Background())
	h, _ := reg.RegisterSSE(agent, conv, sseCancel)

	m1, m2 := uuid.New(), uuid.New()
	w1Ctx, w1Cancel := context.WithCancel(context.Background())
	reg.RegisterWrite(agent, conv, m1, w1Cancel)
	w2Ctx, w2Cancel := context.WithCancel(context.Background())
	reg.RegisterWrite(agent, conv, m2, w2Cancel)

	done := reg.LookupAndCancel(agent, conv)

	// Everything should be cancelled.
	for _, c := range []context.Context{sseCtx, w1Ctx, w2Ctx} {
		select {
		case <-c.Done():
		case <-time.After(500 * time.Millisecond):
			t.Fatal("cancel not observed")
		}
	}

	// Simulate handlers deregistering.
	reg.DeregisterSSE(h)
	reg.DeregisterWrite(agent, conv, m1)
	reg.DeregisterWrite(agent, conv, m2)

	if !WaitFor(done, 500*time.Millisecond) {
		t.Fatal("done channel did not close after all deregs")
	}
}
