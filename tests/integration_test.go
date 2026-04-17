//go:build integration

package tests

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"agentmail/internal/api"
)

// TestRegisterAndCreateConversation exercises the README acceptance steps:
// POST /agents twice → POST /conversations → GET /conversations.
func TestRegisterAndCreateConversation(t *testing.T) {
	ts := startTestServer(t, false)
	defer ts.Close()
	ensureLoopback(t, ts.BaseURL)

	a := ts.createAgent(t)
	b := ts.createAgent(t)
	if a == uuid.Nil || b == uuid.Nil || a == b {
		t.Fatalf("expected two distinct agent ids, got a=%s b=%s", a, b)
	}

	conv := ts.createConversation(t, a)

	var list api.ListConversationsResponse
	code, raw := ts.doJSON(t, http.MethodGet, "/conversations", a, nil, &list)
	if code != http.StatusOK {
		t.Fatalf("list conversations: status=%d body=%s", code, raw)
	}
	found := false
	for _, c := range list.Conversations {
		if c.ConversationID != conv {
			continue
		}
		found = true
		if len(c.Members) != 1 || c.Members[0] != a {
			t.Fatalf("conversation members = %v, want [%s]", c.Members, a)
		}
	}
	if !found {
		t.Fatalf("created conversation %s not returned in list; got %+v", conv, list)
	}
}

// TestInviteReflectsInMembership covers:
//   - A creates conv, A invites B.
//   - B now sees the conversation in GET /conversations.
//   - Inviting B again is a no-op (already_member=true).
//   - Inviting a bogus UUID returns 404 invitee_not_found.
func TestInviteReflectsInMembership(t *testing.T) {
	ts := startTestServer(t, false)
	defer ts.Close()

	a := ts.createAgent(t)
	b := ts.createAgent(t)
	conv := ts.createConversation(t, a)

	inv := ts.invite(t, a, conv, b)
	if inv.AlreadyMember {
		t.Fatalf("first invite reported already_member=true")
	}

	var blist api.ListConversationsResponse
	code, raw := ts.doJSON(t, http.MethodGet, "/conversations", b, nil, &blist)
	if code != http.StatusOK {
		t.Fatalf("list as B: status=%d body=%s", code, raw)
	}
	seen := false
	for _, c := range blist.Conversations {
		if c.ConversationID == conv {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("B did not see conv %s in its conversations list", conv)
	}

	inv2 := ts.invite(t, a, conv, b)
	if !inv2.AlreadyMember {
		t.Fatalf("re-invite expected already_member=true, got %+v", inv2)
	}

	bogus := uuid.New()
	code, raw = ts.doJSON(t, http.MethodPost,
		fmt.Sprintf("/conversations/%s/invite", conv),
		a, api.InviteRequest{AgentID: bogus}, nil)
	if code != http.StatusNotFound {
		t.Fatalf("bogus invitee: status=%d body=%s", code, raw)
	}
}

// TestCompleteMessageDelivery walks a full write+read round-trip:
// A + B join, B opens SSE, A POSTs a complete message, B receives the 3
// matching frames in monotone seq order.
func TestCompleteMessageDelivery(t *testing.T) {
	ts := startTestServer(t, false)
	defer ts.Close()

	a := ts.createAgent(t)
	b := ts.createAgent(t)
	conv := ts.createConversation(t, a)
	ts.invite(t, a, conv, b)

	sse := ts.openSSE(t, b, conv, "")
	defer sse.Close()

	// Drain the pre-existing join events so the test asserts only on the
	// message_* frames produced by the send. Two joins (A, B) are expected.
	pre := sse.Collect(2, 5*time.Second)
	if len(pre) < 2 {
		t.Fatalf("expected at least 2 agent_joined frames, got %d", len(pre))
	}

	sent := ts.sendMessage(t, a, conv, "hello from A")
	if sent.SeqStart == 0 || sent.SeqEnd < sent.SeqStart+2 {
		t.Fatalf("send response malformed: %+v", sent)
	}

	events := sse.Collect(3, 10*time.Second)
	if len(events) != 3 {
		t.Fatalf("expected 3 message frames, got %d: %+v", len(events), events)
	}
	wantTypes := []string{"message_start", "message_append", "message_end"}
	var lastSeq uint64
	for i, ev := range events {
		if ev.Type != wantTypes[i] {
			t.Fatalf("event[%d].type = %q, want %q", i, ev.Type, wantTypes[i])
		}
		if ev.ID <= lastSeq {
			t.Fatalf("seq non-monotone at %d: %d <= %d", i, ev.ID, lastSeq)
		}
		lastSeq = ev.ID
	}
	body := asMap(t, events[1].Data)
	if body["content"] != "hello from A" {
		t.Fatalf("append payload content=%v, want \"hello from A\"", body["content"])
	}
}

// TestHistoryReplayOnLateJoin: A sends 3 messages, C is invited after,
// C's SSE stream starts from seq 0 and replays all 3 before going live.
func TestHistoryReplayOnLateJoin(t *testing.T) {
	ts := startTestServer(t, false)
	defer ts.Close()

	a := ts.createAgent(t)
	c := ts.createAgent(t)
	conv := ts.createConversation(t, a)

	for i := 1; i <= 3; i++ {
		if _, err := uuid.NewV7(); err != nil {
			t.Fatal(err)
		}
		_ = ts.sendMessage(t, a, conv, fmt.Sprintf("m%d", i))
	}

	ts.invite(t, a, conv, c)

	sse := ts.openSSE(t, c, conv, "")
	defer sse.Close()

	// Expect: agent_joined(A), 3 × (start+append+end) = 10 events, then
	// agent_joined(C). Total 11 on SSE open (the last one is from C's own
	// invite append that already happened before we opened the stream).
	events := sse.Collect(11, 15*time.Second)
	if len(events) < 10 {
		t.Fatalf("expected at least 10 replay events, got %d", len(events))
	}

	starts, appends, ends := 0, 0, 0
	for _, ev := range events {
		switch ev.Type {
		case "message_start":
			starts++
		case "message_append":
			appends++
		case "message_end":
			ends++
		}
	}
	if starts != 3 || appends != 3 || ends != 3 {
		t.Fatalf("replay counts: start=%d append=%d end=%d, want 3/3/3", starts, appends, ends)
	}
}
