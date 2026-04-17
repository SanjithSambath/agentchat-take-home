//go:build integration

package tests

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"agentmail/internal/api"
)

// TestLastMemberLeaveRejected: a lone member cannot leave; expect 409
// last_member.
func TestLastMemberLeaveRejected(t *testing.T) {
	ts := startTestServer(t, false)
	defer ts.Close()

	a := ts.createAgent(t)
	conv := ts.createConversation(t, a)

	code, raw := ts.doJSON(t, http.MethodPost,
		fmt.Sprintf("/conversations/%s/leave", conv),
		a, nil, nil)
	if code != http.StatusConflict {
		t.Fatalf("solo leave: status=%d body=%s", code, raw)
	}
}

// TestLeaveAppendsAgentLeft: after a two-member conversation has one party
// leave, the SSE observer sees the agent_left event and the leaver can no
// longer list the conversation.
func TestLeaveAppendsAgentLeft(t *testing.T) {
	ts := startTestServer(t, false)
	defer ts.Close()

	a := ts.createAgent(t)
	b := ts.createAgent(t)
	conv := ts.createConversation(t, a)
	ts.invite(t, a, conv, b)

	// A watches.
	sse := ts.openSSE(t, a, conv, "")
	defer sse.Close()
	_ = sse.Collect(2, 5*time.Second) // two agent_joined frames

	// B leaves.
	code, raw := ts.doJSON(t, http.MethodPost,
		fmt.Sprintf("/conversations/%s/leave", conv),
		b, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("B leave: status=%d body=%s", code, raw)
	}

	ev, ok := sse.Next(10 * time.Second)
	if !ok {
		t.Fatalf("expected agent_left frame, got none")
	}
	if ev.Type != "agent_left" {
		t.Fatalf("frame type = %q, want agent_left", ev.Type)
	}
	body := asMap(t, ev.Data)
	if body["agent_id"] != b.String() {
		t.Fatalf("agent_left.agent_id = %v, want %s", body["agent_id"], b)
	}

	// B's list should no longer carry this conv.
	var list api.ListConversationsResponse
	ts.doJSON(t, http.MethodGet, "/conversations", b, nil, &list)
	for _, c := range list.Conversations {
		if c.ConversationID == conv {
			t.Fatalf("B still sees conv %s after leaving", conv)
		}
	}
}

// TestReinviteResumesFromDeliveryCursor: B leaves after partially reading,
// A sends more, B is re-invited — B's next SSE stream must resume ahead of
// previously-acked events (we advance ack to prove the server honors it).
func TestReinviteResumesFromDeliveryCursor(t *testing.T) {
	ts := startTestServer(t, false)
	defer ts.Close()

	a := ts.createAgent(t)
	b := ts.createAgent(t)
	conv := ts.createConversation(t, a)
	ts.invite(t, a, conv, b)

	// B reads the first message, acks, then leaves.
	sse := ts.openSSE(t, b, conv, "")
	_ = sse.Collect(2, 5*time.Second) // join frames
	first := ts.sendMessage(t, a, conv, "pre-leave")
	firstFrames := sse.Collect(3, 10*time.Second)
	if len(firstFrames) != 3 {
		t.Fatalf("expected 3 frames before leave, got %d", len(firstFrames))
	}
	code, raw := ts.doJSON(t, http.MethodPost,
		fmt.Sprintf("/conversations/%s/ack", conv),
		b, api.AckRequest{Seq: first.SeqEnd}, nil)
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("ack: status=%d body=%s", code, raw)
	}
	sse.Close()

	code, raw = ts.doJSON(t, http.MethodPost,
		fmt.Sprintf("/conversations/%s/leave", conv),
		b, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("leave: status=%d body=%s", code, raw)
	}

	// A sends while B is gone. B is re-invited and reopens SSE.
	second := ts.sendMessage(t, a, conv, "post-leave")
	ts.invite(t, a, conv, b)

	sse2 := ts.openSSE(t, b, conv, strconv.FormatUint(first.SeqEnd, 10))
	defer sse2.Close()

	events := sse2.Collect(5, 15*time.Second)
	if len(events) == 0 {
		t.Fatalf("expected replay events after reinvite, got none")
	}
	for _, ev := range events {
		if ev.ID <= first.SeqEnd {
			t.Fatalf("event seq %d <= last seen %d (cursor not honored)", ev.ID, first.SeqEnd)
		}
	}
	var sawPostLeave bool
	for _, ev := range events {
		if ev.ID == second.SeqEnd && ev.Type == "message_end" {
			sawPostLeave = true
		}
	}
	if !sawPostLeave {
		t.Fatalf("missing message_end for post-leave message (seq %d) in %+v", second.SeqEnd, events)
	}
}
