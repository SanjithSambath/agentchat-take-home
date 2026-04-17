package store_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agentmail/internal/store"
)

// These tests run against the live Neon database referenced by DATABASE_URL.
// They create fresh UUIDv7 ids per test so multiple runs don't collide.
// Skipped entirely when DATABASE_URL is unset (e.g., plain `go test`).

func newStore(t *testing.T) store.MetadataStore {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Short flush interval so cursor tests don't hang waiting for the default 5s.
	s, err := store.NewMetadataStoreWithOptions(ctx, dsn, store.MetadataStoreOptions{
		CursorFlushInterval: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustUUIDv7(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuidv7: %v", err)
	}
	return id
}

func TestAgentCreateAndExists(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// Create → id populated in cache.
	id, err := s.CreateAgent(ctx)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if id == uuid.Nil {
		t.Fatalf("CreateAgent returned nil uuid")
	}

	ok, err := s.AgentExists(ctx, id)
	if err != nil || !ok {
		t.Fatalf("AgentExists(created) = %v, %v", ok, err)
	}

	// Miss on a random fresh uuid.
	missing := mustUUIDv7(t)
	ok, err = s.AgentExists(ctx, missing)
	if err != nil {
		t.Fatalf("AgentExists(missing) err: %v", err)
	}
	if ok {
		t.Fatalf("AgentExists(missing) = true, want false")
	}
}

func TestAgentCacheWarmOnNewStore(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	first := newStore(t)
	id, err := first.CreateAgent(ctx)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Fresh store instance should warm the sync.Map from Postgres and hit
	// the cache on the AgentExists call below without a DB miss.
	second, err := store.NewMetadataStoreWithOptions(ctx, dsn, store.MetadataStoreOptions{
		CursorFlushInterval: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("second NewMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	ok, err := second.AgentExists(ctx, id)
	if err != nil || !ok {
		t.Fatalf("second.AgentExists = %v, %v", ok, err)
	}
}

func TestConversationCreateAndHeadSeq(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	convID := mustUUIDv7(t)
	if err := s.CreateConversation(ctx, convID, "conversations/"+convID.String()); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	got, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.ID != convID {
		t.Fatalf("GetConversation id = %s, want %s", got.ID, convID)
	}
	if got.HeadSeq != 0 {
		t.Fatalf("initial head_seq = %d, want 0", got.HeadSeq)
	}

	// Monotonic advance.
	if err := s.UpdateConversationHeadSeq(ctx, convID, 10); err != nil {
		t.Fatalf("UpdateHead 10: %v", err)
	}
	seq, err := s.GetConversationHeadSeq(ctx, convID)
	if err != nil || seq != 10 {
		t.Fatalf("head_seq after 10 = %d, err %v", seq, err)
	}

	// Regression guard — smaller value is a no-op.
	if err := s.UpdateConversationHeadSeq(ctx, convID, 5); err != nil {
		t.Fatalf("UpdateHead 5: %v", err)
	}
	seq, err = s.GetConversationHeadSeq(ctx, convID)
	if err != nil || seq != 10 {
		t.Fatalf("head_seq regression-guarded = %d, want 10 (err %v)", seq, err)
	}

	// Higher still works.
	if err := s.UpdateConversationHeadSeq(ctx, convID, 20); err != nil {
		t.Fatalf("UpdateHead 20: %v", err)
	}
	seq, err = s.GetConversationHeadSeq(ctx, convID)
	if err != nil || seq != 20 {
		t.Fatalf("head_seq after 20 = %d, want 20 (err %v)", seq, err)
	}
}

func TestGetConversationNotFound(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	_, err := s.GetConversation(ctx, mustUUIDv7(t))
	if !errors.Is(err, store.ErrConversationNotFound) {
		t.Fatalf("expected ErrConversationNotFound, got %v", err)
	}
}

func TestMembershipFlow(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	a1, err := s.CreateAgent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.CreateAgent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conv := mustUUIDv7(t)
	if err := s.CreateConversation(ctx, conv, "conversations/"+conv.String()); err != nil {
		t.Fatal(err)
	}

	// AddMember returns true for newly added.
	added, err := s.AddMember(ctx, conv, a1)
	if err != nil || !added {
		t.Fatalf("AddMember a1 newly: added=%v err=%v", added, err)
	}

	// Idempotent — second call returns false.
	added, err = s.AddMember(ctx, conv, a1)
	if err != nil || added {
		t.Fatalf("AddMember a1 idempotent: added=%v err=%v", added, err)
	}

	// IsMember cache hit.
	ok, err := s.IsMember(ctx, a1, conv)
	if err != nil || !ok {
		t.Fatalf("IsMember a1: %v, %v", ok, err)
	}

	// Not-member returns false.
	ok, err = s.IsMember(ctx, a2, conv)
	if err != nil || ok {
		t.Fatalf("IsMember a2 before add: %v, %v", ok, err)
	}

	// Remove last member → ErrLastMember.
	if err := s.RemoveMember(ctx, conv, a1); !errors.Is(err, store.ErrLastMember) {
		t.Fatalf("RemoveMember last = %v, want ErrLastMember", err)
	}

	// Add a2, then we can remove a1 successfully.
	if _, err := s.AddMember(ctx, conv, a2); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, conv, a1); err != nil {
		t.Fatalf("RemoveMember a1 (not last): %v", err)
	}

	// a1 is now not a member.
	ok, err = s.IsMember(ctx, a1, conv)
	if err != nil || ok {
		t.Fatalf("IsMember a1 post-remove: %v, %v", ok, err)
	}

	// RemoveMember on non-member → ErrNotMember.
	if err := s.RemoveMember(ctx, conv, a1); !errors.Is(err, store.ErrNotMember) {
		t.Fatalf("RemoveMember non-member = %v, want ErrNotMember", err)
	}

	// ListMembers returns [a2].
	members, err := s.ListMembers(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0] != a2 {
		t.Fatalf("ListMembers = %v, want [%s]", members, a2)
	}

	// ListConversationsForAgent returns conv for a2.
	convs, err := s.ListConversationsForAgent(ctx, a2)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range convs {
		if c.ID == conv {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListConversationsForAgent a2 missing conv; got %v", convs)
	}
}

// Two goroutines attempt to leave a 2-member conversation simultaneously.
// The FOR UPDATE lock must serialize them: exactly one succeeds and the
// other sees ErrLastMember. The count never drops below 1.
func TestConcurrentLeaveSerializes(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	a1, _ := s.CreateAgent(ctx)
	a2, _ := s.CreateAgent(ctx)
	conv := mustUUIDv7(t)
	if err := s.CreateConversation(ctx, conv, "conversations/"+conv.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember(ctx, conv, a1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember(ctx, conv, a2); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = s.RemoveMember(ctx, conv, a1) }()
	go func() { defer wg.Done(); errs[1] = s.RemoveMember(ctx, conv, a2) }()
	wg.Wait()

	successes := 0
	lastMember := 0
	for _, e := range errs {
		switch {
		case e == nil:
			successes++
		case errors.Is(e, store.ErrLastMember):
			lastMember++
		default:
			t.Fatalf("unexpected leave error: %v", e)
		}
	}
	if successes != 1 || lastMember != 1 {
		t.Fatalf("concurrent leave: successes=%d lastMember=%d (want 1/1)", successes, lastMember)
	}

	// Exactly one member remains.
	members, err := s.ListMembers(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Fatalf("after concurrent leave: members=%v, want exactly one", members)
	}
}

func TestDeliveryCursorTwoTier(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	agent, _ := s.CreateAgent(ctx)
	conv := mustUUIDv7(t)
	if err := s.CreateConversation(ctx, conv, "conversations/"+conv.String()); err != nil {
		t.Fatal(err)
	}

	// Hot-tier update (no I/O).
	s.UpdateDeliveryCursor(agent, conv, 42)

	// Read hits hot tier.
	seq, err := s.GetDeliveryCursor(ctx, agent, conv)
	if err != nil || seq != 42 {
		t.Fatalf("GetDeliveryCursor hot = %d err %v", seq, err)
	}

	// Flush all → value lands in Postgres.
	if err := s.FlushDeliveryCursorAll(ctx); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	// Regression guard: flush a smaller value via FlushOne after setting memory
	// to a smaller seq should be impossible (Update ignores regressions), so
	// instead verify directly: second FlushAll with no dirty keys is a no-op.
	if err := s.FlushDeliveryCursorAll(ctx); err != nil {
		t.Fatalf("FlushAll (clean): %v", err)
	}

	// Advance and flush-one.
	s.UpdateDeliveryCursor(agent, conv, 100)
	if err := s.FlushDeliveryCursorOne(ctx, agent, conv); err != nil {
		t.Fatalf("FlushOne: %v", err)
	}

	// Read through a new store instance (hot tier empty → Postgres fallback).
	dsn := os.Getenv("DATABASE_URL")
	fresh, err := store.NewMetadataStoreWithOptions(ctx, dsn, store.MetadataStoreOptions{
		CursorFlushInterval: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("fresh store: %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })

	seq, err = fresh.GetDeliveryCursor(ctx, agent, conv)
	if err != nil || seq != 100 {
		t.Fatalf("fresh GetDeliveryCursor = %d err %v (want 100)", seq, err)
	}
}

func TestDeliveryCursorRegressionGuard(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	agent, _ := s.CreateAgent(ctx)
	conv := mustUUIDv7(t)
	if err := s.CreateConversation(ctx, conv, "conversations/"+conv.String()); err != nil {
		t.Fatal(err)
	}

	s.UpdateDeliveryCursor(agent, conv, 500)
	if err := s.FlushDeliveryCursorOne(ctx, agent, conv); err != nil {
		t.Fatal(err)
	}

	// Force a direct Postgres read by checking value through a fresh store.
	dsn := os.Getenv("DATABASE_URL")
	fresh, err := store.NewMetadataStoreWithOptions(ctx, dsn, store.MetadataStoreOptions{
		CursorFlushInterval: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })

	// Set smaller seq via a *third* fresh store with its own empty hot tier,
	// then flush. SQL regression guard means Postgres value stays 500.
	third, err := store.NewMetadataStoreWithOptions(ctx, dsn, store.MetadataStoreOptions{
		CursorFlushInterval: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = third.Close() })
	third.UpdateDeliveryCursor(agent, conv, 100)
	if err := third.FlushDeliveryCursorOne(ctx, agent, conv); err != nil {
		t.Fatal(err)
	}

	seq, err := fresh.GetDeliveryCursor(ctx, agent, conv)
	if err != nil || seq != 500 {
		t.Fatalf("after stale flush seq=%d err=%v (want 500)", seq, err)
	}
}

func TestAckCursorAndUnread(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	agent, _ := s.CreateAgent(ctx)
	conv := mustUUIDv7(t)
	if err := s.CreateConversation(ctx, conv, "conversations/"+conv.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember(ctx, conv, agent); err != nil {
		t.Fatal(err)
	}

	// Advance head_seq so unread math has something to work with.
	if err := s.UpdateConversationHeadSeq(ctx, conv, 50); err != nil {
		t.Fatal(err)
	}

	if err := s.Ack(ctx, agent, conv, 20); err != nil {
		t.Fatalf("Ack 20: %v", err)
	}
	seq, err := s.GetAckCursor(ctx, agent, conv)
	if err != nil || seq != 20 {
		t.Fatalf("GetAck after 20 = %d err %v", seq, err)
	}

	// Regression-guarded — smaller ack is a no-op.
	if err := s.Ack(ctx, agent, conv, 5); err != nil {
		t.Fatalf("Ack 5: %v", err)
	}
	seq, _ = s.GetAckCursor(ctx, agent, conv)
	if seq != 20 {
		t.Fatalf("Ack regression-guarded = %d, want 20", seq)
	}

	// Unread should show 50 - 20 = 30.
	entries, err := s.ListUnreadForAgent(ctx, agent, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.ConversationID == conv {
			found = true
			if e.HeadSeq != 50 || e.AckSeq != 20 || e.EventDelta != 30 {
				t.Fatalf("unread = %+v, want head=50 ack=20 delta=30", e)
			}
		}
	}
	if !found {
		t.Fatalf("conv missing from unread list")
	}

	// After acking up to head, conversation drops off the unread list.
	if err := s.Ack(ctx, agent, conv, 50); err != nil {
		t.Fatal(err)
	}
	entries, _ = s.ListUnreadForAgent(ctx, agent, 100)
	for _, e := range entries {
		if e.ConversationID == conv {
			t.Fatalf("conv still in unread after full ack: %+v", e)
		}
	}
}

func TestInProgressIdempotency(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	agent, _ := s.CreateAgent(ctx)
	conv := mustUUIDv7(t)
	if err := s.CreateConversation(ctx, conv, "conversations/"+conv.String()); err != nil {
		t.Fatal(err)
	}

	msgID := mustUUIDv7(t)
	streamName := "conversations/" + conv.String()

	claimed, err := s.ClaimInProgressMessage(ctx, msgID, conv, agent, streamName)
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}

	// Duplicate claim → false.
	claimed, err = s.ClaimInProgressMessage(ctx, msgID, conv, agent, streamName)
	if err != nil || claimed {
		t.Fatalf("duplicate claim: claimed=%v err=%v (want false)", claimed, err)
	}

	// ListInProgressMessages contains our row.
	rows, err := s.ListInProgressMessages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.MessageID == msgID {
			found = true
			if r.ConversationID != conv || r.AgentID != agent || r.S2StreamName != streamName {
				t.Fatalf("in-progress row mismatch: %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("claimed message not in list")
	}

	// Delete clears the row and re-enables claiming.
	if err := s.DeleteInProgressMessage(ctx, msgID); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimInProgressMessage(ctx, msgID, conv, agent, streamName)
	if err != nil || !claimed {
		t.Fatalf("post-delete re-claim: claimed=%v err=%v", claimed, err)
	}
	_ = s.DeleteInProgressMessage(ctx, msgID)
}

func TestDedupCompleteAndAborted(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	conv := mustUUIDv7(t)
	if err := s.CreateConversation(ctx, conv, "conversations/"+conv.String()); err != nil {
		t.Fatal(err)
	}

	completeID := mustUUIDv7(t)
	abortedIDWithStart := mustUUIDv7(t)
	abortedIDNoStart := mustUUIDv7(t)

	// Complete insert.
	if err := s.InsertMessageDedupComplete(ctx, conv, completeID, 10, 15); err != nil {
		t.Fatalf("InsertComplete: %v", err)
	}
	row, ok, err := s.GetMessageDedup(ctx, conv, completeID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || row.Status != "complete" || row.StartSeq == nil || *row.StartSeq != 10 || row.EndSeq == nil || *row.EndSeq != 15 {
		t.Fatalf("complete row = %+v (ok=%v)", row, ok)
	}

	// Aborted with start_seq.
	start := uint64(7)
	if err := s.InsertMessageDedupAborted(ctx, conv, abortedIDWithStart, &start); err != nil {
		t.Fatalf("InsertAborted+start: %v", err)
	}
	row, ok, err = s.GetMessageDedup(ctx, conv, abortedIDWithStart)
	if err != nil || !ok {
		t.Fatalf("GetDedup aborted+start: ok=%v err=%v", ok, err)
	}
	if row.Status != "aborted" || row.StartSeq == nil || *row.StartSeq != 7 || row.EndSeq != nil {
		t.Fatalf("aborted+start row = %+v", row)
	}

	// Aborted with nil start_seq.
	if err := s.InsertMessageDedupAborted(ctx, conv, abortedIDNoStart, nil); err != nil {
		t.Fatalf("InsertAborted nil: %v", err)
	}
	row, ok, err = s.GetMessageDedup(ctx, conv, abortedIDNoStart)
	if err != nil || !ok {
		t.Fatalf("GetDedup aborted nil: %v / %v", ok, err)
	}
	if row.Status != "aborted" || row.StartSeq != nil || row.EndSeq != nil {
		t.Fatalf("aborted nil row = %+v", row)
	}

	// Miss returns (nil, false, nil).
	row, ok, err = s.GetMessageDedup(ctx, conv, mustUUIDv7(t))
	if err != nil || ok || row != nil {
		t.Fatalf("miss dedup: row=%+v ok=%v err=%v", row, ok, err)
	}
}

func TestCursorFlusherRunAndShutdown(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	agent, _ := s.CreateAgent(ctx)
	conv := mustUUIDv7(t)
	if err := s.CreateConversation(ctx, conv, "conversations/"+conv.String()); err != nil {
		t.Fatal(err)
	}

	flushCtx, cancel := context.WithCancel(context.Background())
	s.StartDeliveryCursorFlusher(flushCtx)

	s.UpdateDeliveryCursor(agent, conv, 77)

	// Wait one flush tick (500 ms) plus slack.
	time.Sleep(1200 * time.Millisecond)

	// Read from a fresh store to bypass the hot tier.
	dsn := os.Getenv("DATABASE_URL")
	fresh, err := store.NewMetadataStoreWithOptions(ctx, dsn, store.MetadataStoreOptions{
		CursorFlushInterval: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	seq, err := fresh.GetDeliveryCursor(ctx, agent, conv)
	if err != nil || seq != 77 {
		t.Fatalf("periodic-flushed seq = %d err %v", seq, err)
	}

	cancel()
}
