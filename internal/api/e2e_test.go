package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestEndToEnd drives the full take-home curl transcript through NewRouter
// against the in-memory fakes: two agents register, one creates and invites,
// one opens SSE, the other sends, the receiver acks, then both leave.
//
// This test is the executable form of the curl transcript promised in the
// take-home report. Success here proves the wiring is end-to-end correct
// without needing real Postgres / S2.
func TestEndToEnd(t *testing.T) {
	meta := newFakeMeta()
	s2 := newFakeS2()
	router := NewRouter(s2, meta, DisabledResident{}, nil)
	srv := httptest.NewServer(router)
	defer srv.Close()

	do := func(t *testing.T, req *http.Request) *http.Response {
		t.Helper()
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
		}
		return res
	}

	// 1. Register A and B.
	var aID, bID uuid.UUID
	for _, target := range []*uuid.UUID{&aID, &bID} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/agents", nil)
		res := do(t, req)
		if res.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			t.Fatalf("register: status=%d body=%s", res.StatusCode, body)
		}
		var body CreateAgentResponse
		_ = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		*target = body.AgentID
	}
	t.Logf("agents: A=%s B=%s", aID, bID)

	// 2. A creates a conversation.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/conversations", nil)
	req.Header.Set("X-Agent-ID", aID.String())
	res := do(t, req)
	if res.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("create conv: status=%d body=%s", res.StatusCode, body)
	}
	var conv CreateConversationResponse
	_ = json.NewDecoder(res.Body).Decode(&conv)
	res.Body.Close()
	t.Logf("conversation: %s", conv.ConversationID)

	// 3. A invites B.
	inv, _ := json.Marshal(InviteRequest{AgentID: bID})
	req, _ = http.NewRequest(http.MethodPost,
		srv.URL+"/conversations/"+conv.ConversationID.String()+"/invite",
		bytes.NewReader(inv))
	req.Header.Set("X-Agent-ID", aID.String())
	req.Header.Set("Content-Type", "application/json")
	res = do(t, req)
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("invite: status=%d body=%s", res.StatusCode, body)
	}
	res.Body.Close()

	// 4. B opens SSE.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	defer sseCancel()
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet,
		srv.URL+"/conversations/"+conv.ConversationID.String()+"/stream?from=1", nil)
	sseReq.Header.Set("X-Agent-ID", bID.String())
	sseRes, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	defer sseRes.Body.Close()
	if sseRes.StatusCode != http.StatusOK {
		t.Fatalf("sse status=%d", sseRes.StatusCode)
	}

	// 5. A sends a complete message.
	msgID := uuid.Must(uuid.NewV7())
	sendBody, _ := json.Marshal(SendMessageRequest{MessageID: msgID, Content: "hello from A"})
	req, _ = http.NewRequest(http.MethodPost,
		srv.URL+"/conversations/"+conv.ConversationID.String()+"/messages",
		bytes.NewReader(sendBody))
	req.Header.Set("X-Agent-ID", aID.String())
	req.Header.Set("Content-Type", "application/json")
	res = do(t, req)
	if res.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("send: status=%d body=%s", res.StatusCode, body)
	}
	var send SendMessageResponse
	_ = json.NewDecoder(res.Body).Decode(&send)
	res.Body.Close()

	// 6. B reads SSE and confirms 3 message events arrived in order.
	reader := bufio.NewReader(sseRes.Body)
	events := readSSEEvents(t, reader, 5, 3*time.Second) // 2 agent_joined + 3 message events
	kinds := map[string]int{}
	var maxID uint64
	for _, e := range events {
		kinds[e.event]++
		var cur uint64
		_, _ = fmt.Sscanf(e.id, "%d", &cur)
		if cur < maxID {
			t.Fatalf("sse ids not monotonic: %v", events)
		}
		maxID = cur
	}
	if kinds["message_start"] == 0 || kinds["message_append"] == 0 || kinds["message_end"] == 0 {
		t.Fatalf("missing expected events; got %v", kinds)
	}

	// 7. B calls unread.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/agents/me/unread", nil)
	req.Header.Set("X-Agent-ID", bID.String())
	res = do(t, req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unread status=%d", res.StatusCode)
	}
	res.Body.Close()

	// 8. B acks up to the last message seq.
	ackBody, _ := json.Marshal(AckRequest{Seq: send.SeqEnd})
	req, _ = http.NewRequest(http.MethodPost,
		srv.URL+"/conversations/"+conv.ConversationID.String()+"/ack",
		bytes.NewReader(ackBody))
	req.Header.Set("X-Agent-ID", bID.String())
	req.Header.Set("Content-Type", "application/json")
	res = do(t, req)
	if res.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("ack status=%d body=%s", res.StatusCode, body)
	}
	res.Body.Close()

	// 9. B's history contains the message.
	req, _ = http.NewRequest(http.MethodGet,
		srv.URL+"/conversations/"+conv.ConversationID.String()+"/messages?limit=50", nil)
	req.Header.Set("X-Agent-ID", bID.String())
	res = do(t, req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("history status=%d", res.StatusCode)
	}
	var hist HistoryResponse
	_ = json.NewDecoder(res.Body).Decode(&hist)
	res.Body.Close()
	found := false
	for _, m := range hist.Messages {
		if m.MessageID == msgID && strings.Contains(m.Content, "hello from A") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("message not in history: %+v", hist)
	}

	// 10. A leaves. Cancel SSE first so LeaveConversation doesn't wait 5s.
	sseCancel()
	req, _ = http.NewRequest(http.MethodPost,
		srv.URL+"/conversations/"+conv.ConversationID.String()+"/leave", nil)
	req.Header.Set("X-Agent-ID", aID.String())
	res = do(t, req)
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("leave status=%d body=%s", res.StatusCode, body)
	}
	res.Body.Close()
}
