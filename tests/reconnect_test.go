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

// TestReconnectWithLastEventID: B opens SSE, reads a few events, cancels;
// A writes more; B reopens with Last-Event-ID=<last seq> and sees only
// post-disconnect events.
func TestReconnectWithLastEventID(t *testing.T) {
	ts := startTestServer(t, false)
	defer ts.Close()

	a := ts.createAgent(t)
	b := ts.createAgent(t)
	conv := ts.createConversation(t, a)
	ts.invite(t, a, conv, b)

	sse := ts.openSSE(t, b, conv, "")
	_ = sse.Collect(2, 5*time.Second) // drain join frames

	first := ts.sendMessage(t, a, conv, "first")
	events := sse.Collect(3, 10*time.Second)
	if len(events) != 3 {
		t.Fatalf("expected 3 events on first session, got %d", len(events))
	}
	lastSeq := events[len(events)-1].ID
	if lastSeq != first.SeqEnd {
		t.Fatalf("last seq = %d, want %d", lastSeq, first.SeqEnd)
	}
	sse.Close()

	second := ts.sendMessage(t, a, conv, "second")

	// Reopen with Last-Event-ID pointing at the final frame we already saw.
	sse2 := ts.openSSE(t, b, conv, strconv.FormatUint(lastSeq, 10))
	defer sse2.Close()

	fresh := sse2.Collect(3, 10*time.Second)
	if len(fresh) != 3 {
		t.Fatalf("expected 3 fresh events after reconnect, got %d: %+v", len(fresh), fresh)
	}
	if fresh[0].ID != lastSeq+1 {
		t.Fatalf("fresh[0].ID = %d, want %d", fresh[0].ID, lastSeq+1)
	}
	if fresh[len(fresh)-1].ID != second.SeqEnd {
		t.Fatalf("fresh last = %d, want %d", fresh[len(fresh)-1].ID, second.SeqEnd)
	}
}

// TestHistoryEndpointReturnsCompletedMessages validates that the history
// endpoint surfaces completed messages in send order with strictly
// increasing seq_start.
func TestHistoryEndpointReturnsCompletedMessages(t *testing.T) {
	ts := startTestServer(t, false)
	defer ts.Close()

	a := ts.createAgent(t)
	conv := ts.createConversation(t, a)

	for i := 1; i <= 3; i++ {
		ts.sendMessage(t, a, conv, fmt.Sprintf("msg %d", i))
	}

	var hist api.HistoryResponse
	code, raw := ts.doJSON(t, http.MethodGet,
		fmt.Sprintf("/conversations/%s/messages", conv),
		a, nil, &hist)
	if code != http.StatusOK {
		t.Fatalf("history: status=%d body=%s", code, raw)
	}
	if len(hist.Messages) < 3 {
		t.Fatalf("expected >=3 messages, got %d", len(hist.Messages))
	}
	var lastStart uint64
	for _, m := range hist.Messages {
		if m.SeqStart <= lastStart {
			t.Fatalf("history seq_start non-monotone: %d <= %d", m.SeqStart, lastStart)
		}
		lastStart = m.SeqStart
	}
}
