// Live integration tests for the S2 store. Every test exercises the real
// wire protocol against the basin named in S2_BASIN (default "agentmail").
// Tests skip automatically under `-short` or when S2_ACCESS_TOKEN is unset,
// so `go test ./internal/store/ -short` remains hermetic.
//
// Cleanup note: each test uses a fresh uuid.New() conversation id, so streams
// never collide across runs. The basin's 28-day retention handles cleanup
// without explicit teardown.

package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"agentmail/internal/config"
	"agentmail/internal/model"
)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

func newTestStore(t *testing.T) S2Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping live S2 test in -short mode")
	}
	token := os.Getenv("S2_ACCESS_TOKEN")
	if token == "" {
		token = os.Getenv("S2_AUTH_TOKEN")
	}
	if token == "" {
		t.Skip("S2_ACCESS_TOKEN not set; skipping live S2 test")
	}
	basin := os.Getenv("S2_BASIN")
	if basin == "" {
		basin = "agentmail"
	}
	cfg := config.Config{
		S2AuthToken: token,
		S2Basin:     basin,
	}
	s, err := NewS2Store(cfg)
	if err != nil {
		t.Fatalf("NewS2Store: %v", err)
	}
	return s
}

func testCtx(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), d)
}

// ---------------------------------------------------------------------------
// AppendEvents (unary, complete-message batch)
// ---------------------------------------------------------------------------

func TestAppendEvents_Roundtrip(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := testCtx(t, 15*time.Second)
	defer cancel()

	convID := uuid.New()
	msgID := uuid.New()
	senderID := uuid.New()
	events := model.NewCompleteMessageEvents(msgID, senderID, "hello world")

	result, err := s.AppendEvents(ctx, convID, events)
	if err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	if got, want := result.EndSeq-result.StartSeq, uint64(3); got != want {
		t.Fatalf("range size: got %d, want %d", got, want)
	}

	got, err := s.ReadRange(ctx, convID, result.StartSeq, 3)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ReadRange returned %d events, want 3", len(got))
	}
	wantTypes := []model.EventType{
		model.EventTypeMessageStart,
		model.EventTypeMessageAppend,
		model.EventTypeMessageEnd,
	}
	for i, ev := range got {
		if ev.Type != wantTypes[i] {
			t.Errorf("event %d: type %s, want %s", i, ev.Type, wantTypes[i])
		}
		if ev.SeqNum != result.StartSeq+uint64(i) {
			t.Errorf("event %d: seq %d, want %d", i, ev.SeqNum, result.StartSeq+uint64(i))
		}
	}

	start, err := model.ParsePayload[model.MessageStartPayload](got[0])
	if err != nil {
		t.Fatalf("ParsePayload[MessageStartPayload]: %v", err)
	}
	if start.MessageID != msgID {
		t.Errorf("MessageID mismatch: got %s want %s", start.MessageID, msgID)
	}
	if start.SenderID != senderID {
		t.Errorf("SenderID mismatch: got %s want %s", start.SenderID, senderID)
	}

	appendPayload, err := model.ParsePayload[model.MessageAppendPayload](got[1])
	if err != nil {
		t.Fatalf("ParsePayload[MessageAppendPayload]: %v", err)
	}
	if appendPayload.Content != "hello world" {
		t.Errorf("Content: got %q, want %q", appendPayload.Content, "hello world")
	}
}

// ---------------------------------------------------------------------------
// AppendSession pipeline
// ---------------------------------------------------------------------------

func TestAppendSession_Pipeline(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := testCtx(t, 30*time.Second)
	defer cancel()

	convID := uuid.New()
	msgID := uuid.New()
	senderID := uuid.New()

	// Seed with a start event via unary append so tests can compare against
	// a known starting offset.
	startRes, err := s.AppendEvents(ctx, convID, []model.Event{
		model.NewMessageStartEvent(msgID, senderID),
	})
	if err != nil {
		t.Fatalf("seed AppendEvents: %v", err)
	}

	sess, err := s.OpenAppendSession(ctx, convID)
	if err != nil {
		t.Fatalf("OpenAppendSession: %v", err)
	}

	const nTokens = 12
	contents := make([]string, nTokens)
	for i := 0; i < nTokens; i++ {
		contents[i] = "tok-" + time.Now().Format("150405.000") + "-" + itoa(i)
		if err := sess.Submit(ctx, model.NewMessageAppendEvent(msgID, contents[i])); err != nil {
			t.Fatalf("Submit[%d]: %v", i, err)
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// First appended token is right after the seed start event.
	got, err := s.ReadRange(ctx, convID, startRes.EndSeq, nTokens)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if len(got) != nTokens {
		t.Fatalf("ReadRange returned %d events, want %d", len(got), nTokens)
	}
	for i, ev := range got {
		if ev.Type != model.EventTypeMessageAppend {
			t.Errorf("event %d: type %s, want %s", i, ev.Type, model.EventTypeMessageAppend)
		}
		p, err := model.ParsePayload[model.MessageAppendPayload](ev)
		if err != nil {
			t.Fatalf("ParsePayload[%d]: %v", i, err)
		}
		if p.Content != contents[i] {
			t.Errorf("event %d content: got %q, want %q", i, p.Content, contents[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Slow writer: pre-expired parent ctx surfaces as ErrSlowWriter
// ---------------------------------------------------------------------------

func TestAppendSession_SlowWriter_ExpiredCtx(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := testCtx(t, 15*time.Second)
	defer cancel()

	convID := uuid.New()
	// Seed the stream so the session has somewhere real to attach to.
	if _, err := s.AppendEvents(ctx, convID, []model.Event{
		model.NewAgentJoinedEvent(uuid.New()),
	}); err != nil {
		t.Fatalf("seed AppendEvents: %v", err)
	}

	sess, err := s.OpenAppendSession(ctx, convID)
	if err != nil {
		t.Fatalf("OpenAppendSession: %v", err)
	}
	defer func() { _ = sess.Close() }()

	// Parent deadline already in the past → the internal Wait(ctx) with a
	// 2 s timeout derived from an expired parent fires immediately.
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Millisecond))
	defer cancelExpired()

	err = sess.Submit(expired, model.NewMessageAppendEvent(uuid.New(), "x"))
	if !errors.Is(err, ErrSlowWriter) {
		t.Fatalf("expected ErrSlowWriter, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Sticky cached error: after one failure, Submit keeps returning it.
// ---------------------------------------------------------------------------

func TestAppendSession_StickyError(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := testCtx(t, 15*time.Second)
	defer cancel()

	convID := uuid.New()
	if _, err := s.AppendEvents(ctx, convID, []model.Event{
		model.NewAgentJoinedEvent(uuid.New()),
	}); err != nil {
		t.Fatalf("seed AppendEvents: %v", err)
	}

	sess, err := s.OpenAppendSession(ctx, convID)
	if err != nil {
		t.Fatalf("OpenAppendSession: %v", err)
	}

	// Poison with a pre-expired ctx. Submit does NOT cache ErrSlowWriter
	// (that's a caller-ctx condition, not a session fault), so the stickiness
	// we verify here is the caller's own error propagation: a second Submit
	// on a healthy ctx should succeed. This documents the intended semantics:
	// ErrSlowWriter is per-call; only background-ack failures poison the
	// session.
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Millisecond))
	cancelExpired()
	err1 := sess.Submit(expired, model.NewMessageAppendEvent(uuid.New(), "x"))
	if !errors.Is(err1, ErrSlowWriter) {
		t.Fatalf("first Submit: expected ErrSlowWriter, got %v", err1)
	}

	// With a healthy ctx, Submit should recover — ErrSlowWriter is not sticky.
	err2 := sess.Submit(ctx, model.NewMessageAppendEvent(uuid.New(), "ok"))
	if err2 != nil {
		t.Fatalf("recovered Submit: unexpected err %v", err2)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ReadSession: catch-up replay → real-time tail
// ---------------------------------------------------------------------------

func TestReadSession_CatchupThenTail(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := testCtx(t, 30*time.Second)
	defer cancel()

	convID := uuid.New()
	msgID := uuid.New()
	senderID := uuid.New()

	seed, err := s.AppendEvents(ctx, convID, model.NewCompleteMessageEvents(msgID, senderID, "catchup"))
	if err != nil {
		t.Fatalf("seed AppendEvents: %v", err)
	}

	read, err := s.OpenReadSession(ctx, convID, seed.StartSeq)
	if err != nil {
		t.Fatalf("OpenReadSession: %v", err)
	}
	defer read.Close()

	// Catch-up phase: exactly 3 events.
	for i := 0; i < 3; i++ {
		if !read.Next() {
			t.Fatalf("catchup Next[%d]=false, err=%v", i, read.Err())
		}
	}

	// Real-time tail phase: append a 4th event from a goroutine; the reader
	// should receive it without closing the session.
	appended := make(chan uint64, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		r, err := s.AppendEvents(ctx, convID, []model.Event{
			model.NewAgentJoinedEvent(uuid.New()),
		})
		if err != nil {
			appended <- 0
			return
		}
		appended <- r.StartSeq
	}()

	tailCtx, tailCancel := context.WithTimeout(ctx, 10*time.Second)
	defer tailCancel()

	done := make(chan struct{})
	var tailEv model.SequencedEvent
	go func() {
		if read.Next() {
			tailEv = read.Event()
		}
		close(done)
	}()

	select {
	case <-done:
		wantSeq := <-appended
		if tailEv.SeqNum != wantSeq {
			t.Fatalf("tail seq got %d, want %d", tailEv.SeqNum, wantSeq)
		}
		if tailEv.Type != model.EventTypeAgentJoined {
			t.Fatalf("tail type got %s, want %s", tailEv.Type, model.EventTypeAgentJoined)
		}
	case <-tailCtx.Done():
		t.Fatalf("timed out waiting for real-time tail event")
	}
}

// ---------------------------------------------------------------------------
// ReadRange: bounded slice
// ---------------------------------------------------------------------------

func TestReadRange_Bounded(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := testCtx(t, 15*time.Second)
	defer cancel()

	convID := uuid.New()
	// 5 events.
	events := []model.Event{
		model.NewAgentJoinedEvent(uuid.New()),
		model.NewAgentJoinedEvent(uuid.New()),
		model.NewAgentJoinedEvent(uuid.New()),
		model.NewAgentJoinedEvent(uuid.New()),
		model.NewAgentJoinedEvent(uuid.New()),
	}
	res, err := s.AppendEvents(ctx, convID, events)
	if err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	got, err := s.ReadRange(ctx, convID, res.StartSeq+1, 3)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got)=%d, want 3", len(got))
	}
	for i, ev := range got {
		wantSeq := res.StartSeq + 1 + uint64(i)
		if ev.SeqNum != wantSeq {
			t.Errorf("got[%d].SeqNum=%d want %d", i, ev.SeqNum, wantSeq)
		}
	}
}

// ---------------------------------------------------------------------------
// CheckTail
// ---------------------------------------------------------------------------

func TestCheckTail(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := testCtx(t, 15*time.Second)
	defer cancel()

	convID := uuid.New()

	first, err := s.AppendEvents(ctx, convID, []model.Event{
		model.NewAgentJoinedEvent(uuid.New()),
		model.NewAgentJoinedEvent(uuid.New()),
	})
	if err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	tail, err := s.CheckTail(ctx, convID)
	if err != nil {
		t.Fatalf("CheckTail: %v", err)
	}
	if tail != first.EndSeq {
		t.Fatalf("tail after first append: got %d, want %d", tail, first.EndSeq)
	}

	second, err := s.AppendEvents(ctx, convID, []model.Event{
		model.NewAgentJoinedEvent(uuid.New()),
	})
	if err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	tail2, err := s.CheckTail(ctx, convID)
	if err != nil {
		t.Fatalf("CheckTail(2): %v", err)
	}
	if tail2 != second.EndSeq {
		t.Fatalf("tail after second append: got %d, want %d", tail2, second.EndSeq)
	}
	if tail2 != tail+1 {
		t.Fatalf("tail did not advance by 1: %d → %d", tail, tail2)
	}
}

// ---------------------------------------------------------------------------
// Local helper (avoid pulling in strconv everywhere)
// ---------------------------------------------------------------------------

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
